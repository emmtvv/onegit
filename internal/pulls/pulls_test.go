package pulls_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"onegit/internal/git"
	"onegit/internal/hooks"
	"onegit/internal/pulls"
	"onegit/internal/store"
	"onegit/internal/testutil"
)

var ctx = context.Background()

type env struct {
	t      *testing.T
	st     *store.Store
	repo   *git.Repo
	work   *testutil.Work
	svc    *pulls.Service
	author *store.User // write access, opens PRs
	rev    *store.User // write access, reviews
	reader *store.User
	base   string // main tip
}

func setup(t *testing.T) *env {
	t.Helper()
	cfg := testutil.Config(t)
	e := &env{t: t, st: testutil.Store(t, cfg), repo: testutil.Bare(t), work: testutil.NewWork(t)}
	e.svc = &pulls.Service{Store: e.st, Repo: e.repo, KV: testutil.KV(t, cfg), Log: testutil.Logger(), BaseURL: "https://git.example.com"}
	e.author = testutil.User(t, e.st, "author", store.RoleWrite)
	e.rev = testutil.User(t, e.st, "reviewer", store.RoleWrite)
	e.reader = testutil.User(t, e.st, "reader", store.RoleRead)
	e.base = e.work.Commit("init", map[string]string{"README.md": "hello\n", "app/main.go": "package main\n"})
	e.push("main")
	return e
}

// push pushes branches and tells the service, like post-receive does.
func (e *env) push(branches ...string) {
	e.t.Helper()
	var ups []hooks.RefUpdate
	for _, b := range branches {
		old, err := e.repo.ResolveCommit(ctx, "refs/heads/"+b)
		if err != nil {
			old = git.ZeroSHA
		}
		e.work.Push(e.repo, b)
		sha, _ := e.repo.ResolveCommit(ctx, "refs/heads/"+b)
		ups = append(ups, hooks.RefUpdate{OldSHA: old, NewSHA: sha, Ref: "refs/heads/" + b})
	}
	e.svc.SyncPush(ctx, e.author, ups)
}

// branch creates branch from main with one commit touching files.
func (e *env) branch(name string, files map[string]string) string {
	e.t.Helper()
	e.work.Git("checkout", "-q", "-B", name, "main")
	sha := e.work.Commit("work on "+name, files)
	e.work.Git("checkout", "-q", "main")
	e.push(name)
	return sha
}

func (e *env) open(head string) *store.Pull {
	e.t.Helper()
	p, err := e.svc.Create(ctx, e.author, head, "main", "Title of "+head, "body")
	if err != nil {
		e.t.Fatalf("create PR: %v", err)
	}
	return p
}

func (e *env) review(p *store.Pull, u *store.User, state store.ReviewState, sha string) {
	e.t.Helper()
	testutil.Must(e.t, e.st.AddPullReview(ctx, &store.PullReview{PullID: p.ID, ReviewerID: &u.ID, State: state, CommitSHA: sha}))
}

func (e *env) status(p *store.Pull) *pulls.Status {
	e.t.Helper()
	p, _ = e.st.PullByID(ctx, p.ID)
	reviews, _ := e.st.ListPullReviews(ctx, p.ID)
	st, err := e.svc.Status(ctx, p, reviews)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func userErr(err error) bool {
	var ue *pulls.UserError
	return errors.As(err, &ue)
}

func TestCreate(t *testing.T) {
	t.Parallel()
	e := setup(t)
	head := e.branch("feat", map[string]string{"a.txt": "a"})
	p := e.open("feat")
	if p.ID != 1 || p.HeadSHA != head || p.MergeBase != e.base || p.AuthorName != "author" || p.Body != "body" {
		t.Errorf("pull = %+v", p)
	}
	if ref, _ := e.repo.ResolveCommit(ctx, pulls.PullRef(p.ID)); ref != head {
		t.Errorf("refs/pull/1/head = %s", ref)
	}
	e.work.Git("branch", "-f", "empty", "main")
	e.push("empty")
	for name, c := range map[string][2]string{
		"no title":      {"feat", ""},
		"same branches": {"main", "x"},
		"missing head":  {"nope", "x"},
		"no commits":    {"empty", "x"},
		"duplicate":     {"feat", "again"},
	} {
		if _, err := e.svc.Create(ctx, e.author, c[0], "main", c[1], ""); !userErr(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := e.svc.Create(ctx, e.author, "feat", "nope", "x", ""); !userErr(err) {
		t.Errorf("missing base: %v", err)
	}
	// No PR number was burnt by the failed attempts.
	e.branch("feat2", map[string]string{"b.txt": "b"})
	if p2 := e.open("feat2"); p2.ID != 2 {
		t.Errorf("second PR number = %d", p2.ID)
	}
	if got := e.svc.CreateURL("feat/x y"); got != "https://git.example.com/pulls/new?head=feat%2Fx+y" {
		t.Errorf("CreateURL = %s", got)
	}
	if got := e.svc.PullURL(7); got != "https://git.example.com/pulls/7" {
		t.Errorf("PullURL = %s", got)
	}
}

func TestSyncPush(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.branch("feat", map[string]string{"a.txt": "a"})
	p := e.open("feat")

	// New commits on the head.
	e.work.Git("checkout", "-q", "feat")
	c2 := e.work.Commit("more", map[string]string{"a.txt": "a2"})
	c3 := e.work.Commit("even more", map[string]string{"a.txt": "a3"})
	e.push("feat")
	got, _ := e.st.PullByID(ctx, p.ID)
	if got.HeadSHA != c3 {
		t.Errorf("head = %s, want %s", got.HeadSHA, c3)
	}
	if ref, _ := e.repo.ResolveCommit(ctx, pulls.PullRef(p.ID)); ref != c3 {
		t.Errorf("pull ref = %s", ref)
	}
	// Force-push.
	e.work.Git("reset", "-q", "--hard", c2)
	forced := e.work.Commit("rewritten", map[string]string{"a.txt": "a4"})
	e.push("feat")
	events, _ := e.st.ListPullEvents(ctx, p.ID)
	if len(events) != 2 || events[0].Data["count"] != float64(2) || events[0].Data["forced"] != false || events[1].Data["forced"] != true {
		t.Errorf("events = %+v", events)
	}
	// The base moves: merge base follows.
	e.work.Git("checkout", "-q", "main")
	newBase := e.work.Commit("main moves", map[string]string{"other.txt": "o"})
	e.push("main")
	e.work.Git("checkout", "-q", "feat")
	e.work.Git("-c", "commit.gpgsign=false", "merge", "-q", "--no-edit", "main")
	e.push("feat")
	got, _ = e.st.PullByID(ctx, p.ID)
	if got.MergeBase != newBase {
		t.Errorf("merge base = %s, want %s (forced %s)", got.MergeBase, newBase, forced)
	}
	// Deleting the head branch leaves the PR open with an event.
	e.work.Git("checkout", "-q", "main")
	e.work.Git("push", "-q", e.repo.Path, ":feat")
	e.svc.SyncPush(ctx, e.author, []hooks.RefUpdate{{OldSHA: got.HeadSHA, NewSHA: git.ZeroSHA, Ref: "refs/heads/feat"}})
	got, _ = e.st.PullByID(ctx, p.ID)
	events, _ = e.st.ListPullEvents(ctx, p.ID)
	if !got.IsOpen() || events[len(events)-1].Kind != "head_deleted" {
		t.Errorf("after head deletion: %+v, last event %+v", got, events[len(events)-1])
	}
	if e.svc.HeadBranchExists(ctx, got) {
		t.Error("HeadBranchExists after deletion")
	}
	if st := e.status(got); st.HeadExists || st.Mergeable() {
		t.Errorf("status without head = %+v", st)
	}
}

func TestManualMergeDetected(t *testing.T) {
	t.Parallel()
	e := setup(t)
	head := e.branch("feat", map[string]string{"a.txt": "a"})
	p := e.open("feat")
	e.work.Git("merge", "-q", "--ff-only", "feat")
	e.push("main")
	got, _ := e.st.PullByID(ctx, p.ID)
	if !got.IsMerged() || *got.MergeStyle != "manual" || *got.MergeSHA != head || *got.MergedBy != e.author.ID {
		t.Errorf("pull = %+v", got)
	}
}

func TestMergeStyles(t *testing.T) {
	t.Parallel()
	e := setup(t)
	// Main moves after the branches fork, so no fast-forward is possible.
	e.branch("squash", map[string]string{"s1.txt": "1"})
	e.work.Git("checkout", "-q", "squash")
	e.work.Commit("second squash commit", map[string]string{"s2.txt": "2"})
	e.work.Git("checkout", "-q", "main")
	e.push("squash")
	e.branch("merge", map[string]string{"m.txt": "m"})
	e.branch("rebase", map[string]string{"r1.txt": "1"})
	e.work.Git("checkout", "-q", "rebase")
	e.work.Env = []string{"GIT_AUTHOR_NAME=Rita", "GIT_AUTHOR_EMAIL=rita@example.com"}
	e.work.Commit("rebase two", map[string]string{"r2.txt": "2"})
	e.work.Env = nil
	e.work.Git("checkout", "-q", "main")
	e.push("rebase")
	e.work.Commit("main moves", map[string]string{"main.txt": "x"})
	e.push("main")

	ps, pm, pr := e.open("squash"), e.open("merge"), e.open("rebase")

	title, body := e.svc.DefaultMessage(ctx, ps, pulls.StyleSquash)
	if title != "Title of squash (#1)" || body != "* work on squash\n* second squash commit" {
		t.Errorf("squash message = %q / %q", title, body)
	}
	if title, _ := e.svc.DefaultMessage(ctx, pm, pulls.StyleMerge); title != "Merge pull request #2 from merge" {
		t.Errorf("merge message = %q", title)
	}

	if _, err := e.svc.Merge(ctx, ps.ID, e.reader, pulls.MergeOptions{}); !userErr(err) {
		t.Errorf("reader merge: %v", err)
	}
	merged, err := e.svc.Merge(ctx, ps.ID, e.rev, pulls.MergeOptions{Style: "bogus"}) // defaults to squash
	if err != nil {
		t.Fatal(err)
	}
	c, _ := e.repo.GetCommit(ctx, "main")
	if !merged.IsMerged() || *merged.MergeStyle != "squash" || *merged.MergeSHA != c.SHA || len(c.Parents) != 1 ||
		c.Author.Name != "Author" || c.Committer.Name != "Reviewer" || c.Subject != "Title of squash (#1)" {
		t.Errorf("squash commit = %+v, pull %+v", c, merged)
	}
	if _, err := e.svc.Merge(ctx, ps.ID, e.rev, pulls.MergeOptions{}); !userErr(err) {
		t.Errorf("merging twice: %v", err)
	}

	_, err = e.svc.Merge(ctx, pm.ID, e.rev, pulls.MergeOptions{Style: pulls.StyleMerge, Title: "Custom", Message: "why"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ = e.repo.GetCommit(ctx, "main")
	if len(c.Parents) != 2 || c.Parents[1] != pm.HeadSHA || c.Subject != "Custom" || c.Body != "why" {
		t.Errorf("merge commit = %+v", c)
	}

	before, _ := e.repo.ResolveCommit(ctx, "main")
	if _, err := e.svc.Merge(ctx, pr.ID, e.rev, pulls.MergeOptions{Style: pulls.StyleRebase}); err != nil {
		t.Fatal(err)
	}
	log, _ := e.repo.Log(ctx, before+"..main", git.LogOptions{})
	if len(log) != 2 || log[0].Subject != "rebase two" || log[0].Author.Name != "Rita" || log[0].Committer.Name != "Reviewer" ||
		log[1].Parents[0] != before {
		t.Errorf("rebased commits = %+v", log)
	}
	// The merge event is recorded.
	events, _ := e.st.ListPullEvents(ctx, pr.ID)
	if events[len(events)-1].Kind != "merged" || events[len(events)-1].Data["style"] != "rebase" {
		t.Errorf("events = %+v", events)
	}
}

func TestRebaseFastForward(t *testing.T) {
	t.Parallel()
	e := setup(t)
	head := e.branch("ff", map[string]string{"f.txt": "f"})
	p := e.open("ff")
	if _, err := e.svc.Merge(ctx, p.ID, e.rev, pulls.MergeOptions{Style: pulls.StyleRebase}); err != nil {
		t.Fatal(err)
	}
	if tip, _ := e.repo.ResolveCommit(ctx, "main"); tip != head {
		t.Errorf("fast-forward must keep the original commit: main = %s, head %s", tip, head)
	}
}

func TestConflicts(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.branch("conflict", map[string]string{"README.md": "branch version\n"})
	p := e.open("conflict")
	e.work.Commit("main version", map[string]string{"README.md": "main version\n"})
	e.push("main")
	st := e.status(p)
	if !slices.Equal(st.Conflicts, []string{"README.md"}) || st.Mergeable() {
		t.Errorf("status = %+v", st)
	}
	for _, style := range pulls.MergeStyles {
		if _, err := e.svc.Merge(ctx, p.ID, e.rev, pulls.MergeOptions{Style: style}); !userErr(err) {
			t.Errorf("%s merge with conflicts: %v", style, err)
		}
	}
	// The result is cached and the cache is reused.
	mr1, _ := e.svc.MergeCheck(ctx, st.BaseSHA, p.HeadSHA)
	mr2, _ := e.svc.MergeCheck(ctx, st.BaseSHA, p.HeadSHA)
	if len(mr1.Conflicts) != 1 || mr1.Tree != mr2.Tree {
		t.Errorf("MergeCheck = %+v / %+v", mr1, mr2)
	}
}

func TestReviewsAndProtection(t *testing.T) {
	t.Parallel()
	e := setup(t)
	head := e.branch("feat", map[string]string{"app/x.go": "x"})
	p := e.open("feat")
	if st := e.status(p); !st.Mergeable() || st.Protection != nil {
		t.Fatalf("unprotected status = %+v", st)
	}
	testutil.Must(t, e.st.SaveBranchProtection(ctx, &store.BranchProtection{Pattern: "main", RequiredApprovals: 1,
		DismissStaleApprovals: true, BlockOnChangesRequested: true, RequiredChecks: []string{"ci / test"}}))

	st := e.status(p)
	if st.Approvals != 0 || st.Required != 1 || len(st.Blockers) != 2 {
		t.Errorf("blockers = %q", st.Blockers)
	}
	// Self-approval and reader approval don't count; plain comments don't change verdicts.
	e.review(p, e.author, store.ReviewApproved, head)
	e.review(p, e.reader, store.ReviewApproved, head)
	e.review(p, e.rev, store.ReviewChangesRequested, head)
	e.review(p, e.rev, store.ReviewCommented, head)
	st = e.status(p)
	if st.Approvals != 0 || !slices.ContainsFunc(st.Blockers, func(b string) bool { return strings.Contains(b, "Changes requested by reviewer") }) {
		t.Errorf("after change request: approvals %d, blockers %q", st.Approvals, st.Blockers)
	}
	e.review(p, e.rev, store.ReviewApproved, head)
	testutil.Must(t, e.st.SetCommitStatus(ctx, &store.CommitStatus{SHA: head, Context: "ci / test", State: "pending"}, nil))
	st = e.status(p)
	if st.Approvals != 1 || len(st.Blockers) != 1 || !strings.Contains(st.Blockers[0], "still running") {
		t.Errorf("blockers = %q", st.Blockers)
	}
	testutil.Must(t, e.st.SetCommitStatus(ctx, &store.CommitStatus{SHA: head, Context: "ci / test", State: "failure"}, nil))
	if st := e.status(p); !strings.Contains(strings.Join(st.Blockers, ""), "failed") {
		t.Errorf("failed check: %q", st.Blockers)
	}
	testutil.Must(t, e.st.SetCommitStatus(ctx, &store.CommitStatus{SHA: head, Context: "ci / test", State: "success"}, nil))
	if st := e.status(p); !st.Mergeable() {
		t.Errorf("should be mergeable: %q", st.Blockers)
	}

	// A new push makes the approval stale, and the new head has no checks.
	e.work.Git("checkout", "-q", "feat")
	e.work.Commit("more", map[string]string{"app/y.go": "y"})
	e.push("feat")
	st = e.status(p)
	if st.Approvals != 0 || !st.Verdicts[len(st.Verdicts)-1].Stale || len(st.Blockers) != 2 {
		t.Errorf("after push: approvals %d, blockers %q", st.Approvals, st.Blockers)
	}
	if _, err := e.svc.Merge(ctx, p.ID, e.rev, pulls.MergeOptions{}); !userErr(err) || !strings.Contains(err.Error(), "Cannot merge") {
		t.Errorf("merge while blocked: %v", err)
	}
	// Losing write access voids an approval.
	p, _ = e.st.PullByID(ctx, p.ID)
	e.review(p, e.rev, store.ReviewApproved, p.HeadSHA)
	testutil.Must(t, e.st.SetCommitStatus(ctx, &store.CommitStatus{SHA: p.HeadSHA, Context: "ci / test", State: "success"}, nil))
	if st := e.status(p); !st.Mergeable() {
		t.Fatalf("blockers = %q", st.Blockers)
	}
	e.rev.Role = store.RoleRead
	testutil.Must(t, e.st.UpdateUser(ctx, e.rev))
	if st := e.status(p); st.Approvals != 0 {
		t.Error("approval of a reviewer who lost write access still counts")
	}
}

func TestCodeOwnersStatus(t *testing.T) {
	t.Parallel()
	e := setup(t)
	lead := testutil.User(t, e.st, "lead", store.RoleWrite)
	sre := testutil.User(t, e.st, "sre1", store.RoleWrite)
	team := &store.Team{Name: "sre"}
	testutil.Must(t, e.st.SaveTeam(ctx, team))
	testutil.Must(t, e.st.AddTeamMember(ctx, team.ID, sre.ID))
	// CODEOWNERS on the base branch; the PR's own change to it is ignored.
	e.work.Commit("owners", map[string]string{".github/CODEOWNERS": "app/ @lead\n*.md reviewer@example.com\ndocs/ @ghost\n"})
	e.push("main")
	e.branch("feat", map[string]string{"app/x.go": "x", "README.md": "changed", "docs/d.md": "d",
		".github/CODEOWNERS": "* @author\n"})
	p := e.open("feat")
	testutil.Must(t, e.st.SaveBranchProtection(ctx, &store.BranchProtection{Pattern: "main", RequireCodeOwnerReview: true,
		Owners: "/deploy/ @org/sre\napp/ @sre\n"}))

	st := e.status(p)
	if st.CodeOwnersFile != ".github/CODEOWNERS" {
		t.Errorf("CODEOWNERS file = %q", st.CodeOwnersFile)
	}
	groups := map[string]*pulls.OwnerGroup{}
	for _, g := range st.OwnerGroups {
		key := g.Pattern
		if g.Server {
			key = "server:" + key
		}
		groups[key] = g
	}
	if g := groups["docs/"]; g == nil || g.Resolvable || !g.Satisfied() {
		t.Errorf("unknown owner must not block: %+v", g)
	}
	// /deploy/ has no changed files, so no group.
	if len(groups) != 4 || groups["app/"] == nil || groups["*.md"] == nil || groups["server:app/"] == nil {
		t.Fatalf("groups = %v", groups)
	}
	if len(st.Blockers) != 3 {
		t.Errorf("blockers = %q", st.Blockers)
	}
	e.review(p, lead, store.ReviewApproved, p.HeadSHA)
	e.review(p, e.rev, store.ReviewApproved, p.HeadSHA) // owner by email
	e.review(p, sre, store.ReviewApproved, p.HeadSHA)   // owner through the team
	st = e.status(p)
	if !st.Mergeable() {
		t.Errorf("blockers after owner approvals = %q", st.Blockers)
	}
	for _, g := range st.OwnerGroups {
		if g.Pattern == "app/" && !g.Server && !slices.Equal(g.ApprovedBy, []string{"lead"}) {
			t.Errorf("app/ approved by %v", g.ApprovedBy)
		}
	}
}

func TestCheckPush(t *testing.T) {
	t.Parallel()
	e := setup(t)
	main, _ := e.repo.ResolveCommit(ctx, "main")
	next := e.work.Commit("next", map[string]string{"n": "n"})
	e.work.Push(e.repo, "HEAD:refs/heads/tmp")
	unrelated := e.work.Git("commit-tree", "-m", "orphan", git.EmptyTreeSHA)
	e.work.Push(e.repo, unrelated+":refs/heads/orphan")
	up := func(ref, old, new string) []hooks.RefUpdate {
		return []hooks.RefUpdate{{OldSHA: old, NewSHA: new, Ref: ref}}
	}

	if msg, err := e.svc.CheckPush(ctx, up("refs/heads/main", main, unrelated), nil); msg != "" || err != nil {
		t.Errorf("no rules: %q, %v", msg, err)
	}
	testutil.Must(t, e.st.SaveBranchProtection(ctx, &store.BranchProtection{Pattern: "main"}))
	testutil.Must(t, e.st.SaveBranchProtection(ctx, &store.BranchProtection{Pattern: "release/*", RequirePullRequest: true}))
	testutil.Must(t, e.st.SaveBranchProtection(ctx, &store.BranchProtection{Pattern: "free", AllowForcePush: true, AllowDeletion: true}))
	cases := []struct {
		name    string
		updates []hooks.RefUpdate
		want    string
	}{
		{"fast-forward", up("refs/heads/main", main, next), ""},
		{"force-push", up("refs/heads/main", main, unrelated), "force-push"},
		{"delete", up("refs/heads/main", main, git.ZeroSHA), "cannot be deleted"},
		{"require PR", up("refs/heads/release/1", main, next), "pull request"},
		{"create protected", up("refs/heads/release/2", git.ZeroSHA, next), ""},
		{"allowed force", up("refs/heads/free", main, unrelated), ""},
		{"allowed delete", up("refs/heads/free", main, git.ZeroSHA), ""},
		{"unprotected", up("refs/heads/other", main, unrelated), ""},
		{"tags", up("refs/tags/v1", main, unrelated), ""},
	}
	for _, c := range cases {
		msg, err := e.svc.CheckPush(ctx, c.updates, nil)
		if err != nil || (c.want == "") != (msg == "") || !strings.Contains(msg, c.want) {
			t.Errorf("%s: %q, %v", c.name, msg, err)
		}
	}
	// A force-push check on objects git doesn't know is an error, not an allow.
	if _, err := e.svc.CheckPush(ctx, up("refs/heads/main", main, strings.Repeat("e", 40)), nil); err == nil {
		t.Error("unknown object passed the force-push check")
	}
}

func TestCloseReopenAndDeleteBranch(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.branch("feat", map[string]string{"a": "a"})
	p := e.open("feat")
	if err := e.svc.DeleteHeadBranch(ctx, p, e.author); !userErr(err) {
		t.Errorf("deleting the branch of an open PR: %v", err)
	}
	testutil.Must(t, e.svc.SetState(ctx, p.ID, e.author, false))
	testutil.Must(t, e.svc.SetState(ctx, p.ID, e.author, false)) // no-op
	p, _ = e.st.PullByID(ctx, p.ID)
	if p.State != store.PullClosed || p.ClosedAt == nil {
		t.Fatalf("closed pull = %+v", p)
	}
	// Reopening picks up the current head.
	e.work.Git("checkout", "-q", "feat")
	newHead := e.work.Commit("more", map[string]string{"b": "b"})
	e.work.Git("checkout", "-q", "main")
	e.push("feat")
	testutil.Must(t, e.svc.SetState(ctx, p.ID, e.author, true))
	p, _ = e.st.PullByID(ctx, p.ID)
	if !p.IsOpen() || p.HeadSHA != newHead || p.ClosedAt != nil {
		t.Errorf("reopened pull = %+v", p)
	}
	// Reopen fails when another open PR has the same branches.
	testutil.Must(t, e.svc.SetState(ctx, p.ID, e.author, false))
	other := e.open("feat")
	if err := e.svc.SetState(ctx, p.ID, e.author, true); !userErr(err) {
		t.Errorf("reopen with a duplicate: %v", err)
	}
	testutil.Must(t, e.svc.SetState(ctx, other.ID, e.author, false))

	if err := e.svc.DeleteHeadBranch(ctx, p, e.reader); !userErr(err) {
		t.Errorf("reader deletes a branch: %v", err)
	}
	testutil.Must(t, e.st.SaveBranchProtection(ctx, &store.BranchProtection{Pattern: "feat"}))
	if err := e.svc.DeleteHeadBranch(ctx, p, e.author); !userErr(err) {
		t.Errorf("deleting a protected branch: %v", err)
	}
	rules, _ := e.st.ListBranchProtections(ctx)
	testutil.Must(t, e.st.DeleteBranchProtection(ctx, rules[0].ID))
	p, _ = e.st.PullByID(ctx, p.ID)
	testutil.Must(t, e.svc.DeleteHeadBranch(ctx, p, e.author))
	if e.svc.HeadBranchExists(ctx, p) {
		t.Error("branch still exists")
	}
	// refs/pull keeps the commits reachable.
	if ref, _ := e.repo.ResolveCommit(ctx, pulls.PullRef(p.ID)); ref != newHead {
		t.Errorf("pull ref = %s", ref)
	}
	if err := e.svc.SetState(ctx, p.ID, e.author, true); !userErr(err) {
		t.Errorf("reopen without a head branch: %v", err)
	}
	if err := e.svc.DeleteHeadBranch(ctx, p, e.author); !userErr(err) {
		t.Errorf("deleting twice: %v", err)
	}

	// Merged PRs can't be reopened; the default branch is never deleted.
	e.branch("done", map[string]string{"d": "d"})
	done := e.open("done")
	merged, err := e.svc.Merge(ctx, done.ID, e.rev, pulls.MergeOptions{})
	testutil.Must(t, err)
	if err := e.svc.SetState(ctx, done.ID, e.author, true); !userErr(err) {
		t.Errorf("reopening a merged PR: %v", err)
	}
	e.work.Git("checkout", "-q", "done")
	e.work.Commit("after merge", map[string]string{"e": "e"})
	e.work.Git("checkout", "-q", "main")
	e.push("done")
	if err := e.svc.DeleteHeadBranch(ctx, merged, e.author); !userErr(err) || !strings.Contains(err.Error(), "new commits") {
		t.Errorf("deleting a branch with new commits: %v", err)
	}
	mainPR := *merged
	mainPR.HeadBranch = "main"
	if err := e.svc.DeleteHeadBranch(ctx, &mainPR, e.author); !userErr(err) {
		t.Errorf("deleting the default branch: %v", err)
	}
}

func TestMergeMarksContainedPullsMerged(t *testing.T) {
	t.Parallel()
	e := setup(t)
	head := e.branch("a", map[string]string{"a": "a"})
	e.work.Git("branch", "-f", "b", "a")
	e.push("b")
	pa, pb := e.open("a"), e.open("b")
	// Server-side merges run no hooks: Merge itself must sync other PRs.
	if _, err := e.svc.Merge(ctx, pa.ID, e.rev, pulls.MergeOptions{Style: pulls.StyleMerge}); err != nil {
		t.Fatal(err)
	}
	got, _ := e.st.PullByID(ctx, pb.ID)
	if !got.IsMerged() || *got.MergeStyle != "manual" || got.HeadSHA != head {
		t.Errorf("PR b = %+v", got)
	}
}

func TestMergeStyleValid(t *testing.T) {
	for _, s := range pulls.MergeStyles {
		if !s.Valid() {
			t.Errorf("%s invalid", s)
		}
	}
	if pulls.MergeStyle("octopus").Valid() {
		t.Error("octopus valid")
	}
	if (&pulls.UserError{Msg: "m"}).Error() != "m" {
		t.Error("UserError message")
	}
}

func TestReviewInbox(t *testing.T) {
	t.Parallel()
	e := setup(t)
	lead := testutil.User(t, e.st, "lead", store.RoleWrite)
	sre := testutil.User(t, e.st, "sre1", store.RoleWrite)
	dana := testutil.User(t, e.st, "dana", store.RoleWrite)
	team := &store.Team{Name: "sre"}
	testutil.Must(t, e.st.SaveTeam(ctx, team))
	testutil.Must(t, e.st.AddTeamMember(ctx, team.ID, sre.ID))
	e.work.Commit("owners", map[string]string{"CODEOWNERS": "app/ @lead\n*.md reviewer@example.com\n"})
	e.push("main")
	testutil.Must(t, e.st.SaveBranchProtection(ctx, &store.BranchProtection{Pattern: "main", DismissStaleApprovals: true,
		Owners: "app/ @org/sre\n"}))
	e.branch("feat", map[string]string{"app/x.go": "x", "README.md": "changed"})
	p := e.open("feat")
	process := func() {
		t.Helper()
		if _, err := e.svc.Process(ctx); err != nil {
			t.Fatal(err)
		}
	}
	inbox := func(u *store.User) string {
		t.Helper()
		teams, _ := e.st.UserTeams(ctx, u.ID)
		list, err := e.svc.ReviewInbox(ctx, u, teams, 10)
		testutil.Must(t, err)
		var out []string
		for _, w := range list {
			out = append(out, fmt.Sprintf("#%d %s", w.Pull.ID, w.Reason))
		}
		return strings.Join(out, "; ")
	}
	want := func(u *store.User, reason string) {
		t.Helper()
		if reason != "" {
			reason = fmt.Sprintf("#%d %s", p.ID, reason)
		}
		if got := inbox(u); got != reason {
			t.Errorf("inbox of %s = %q, want %q", u.Username, got, reason)
		}
	}
	process()
	want(lead, "code owner of app/")
	want(sre, "code owner of app/") // through the team, from the branch protection
	want(e.rev, "code owner of *.md")
	want(e.author, "")

	// New owner rules on the base branch apply to open PRs.
	e.work.Commit("owners", map[string]string{"CODEOWNERS": "app/ @lead\n*.md @dana\n"})
	e.push("main")
	process()
	want(dana, "code owner of *.md")
	want(e.rev, "")

	// One owner's approval covers the whole group.
	e.review(p, lead, store.ReviewApproved, p.HeadSHA)
	want(lead, "")
	want(sre, "")
	e.review(p, dana, store.ReviewChangesRequested, p.HeadSHA)
	want(dana, "")

	// New commits bring the PR back to who requested changes and, with
	// stale approvals dismissed, to who approved.
	e.work.Git("checkout", "-q", "feat")
	e.work.Commit("fix", map[string]string{"app/y.go": "y"})
	e.work.Git("checkout", "-q", "main")
	e.push("feat")
	process()
	want(dana, "new commits since you requested changes")
	want(lead, "new commits since your approval")
	want(sre, "code owner of app/")

	testutil.Must(t, e.svc.SetState(ctx, p.ID, e.author, false))
	want(sre, "")
	want(dana, "")
	owners, _ := e.st.PullOwnerKeys(ctx, []int64{p.ID})
	if len(owners) != 0 {
		t.Errorf("a closed PR keeps owners: %v", owners)
	}
}

func TestOwnerKeys(t *testing.T) {
	for owner, want := range map[string][]string{
		"@Dana":             {"user:dana", "team:dana"},
		"@org/SRE":          {"team:sre"},
		"Ops@Example.com":   {"email:ops@example.com"},
		"not-an-owner-name": nil,
	} {
		if got := pulls.OwnerKeys(owner); !slices.Equal(got, want) {
			t.Errorf("OwnerKeys(%q) = %v, want %v", owner, got, want)
		}
	}
	u := &store.User{Username: "Dana", Email: "D@example.com"}
	if got := pulls.UserKeys(u, []string{"sre"}); !slices.Equal(got, []string{"user:dana", "email:d@example.com", "team:sre"}) {
		t.Errorf("UserKeys = %v", got)
	}
}
