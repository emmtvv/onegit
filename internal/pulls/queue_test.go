package pulls_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"onegit/internal/hooks"
	"onegit/internal/pulls"
	"onegit/internal/store"
	"onegit/internal/testutil"
)

// checks fakes CI for the merge queue: candidates get a pending required
// check that the test resolves.
type checks struct {
	e         *env
	mu        sync.Mutex
	started   [][2]string // candidate, built on
	cancelled []string
	updates   []hooks.RefUpdate
	noCheck   bool // start no check at all
}

func (c *checks) wire() {
	c.e.svc.StartChecks = func(_ context.Context, sha, before, ref, base string, pullID int64, by *int64) {
		c.mu.Lock()
		c.started = append(c.started, [2]string{sha, before})
		c.mu.Unlock()
		if !strings.HasPrefix(ref, "refs/merge-queue/") || base != "main" {
			c.e.t.Errorf("StartChecks ref %q base %q", ref, base)
		}
		if !c.noCheck {
			c.set(sha, "pending")
		}
	}
	c.e.svc.CancelChecks = func(_ context.Context, sha string) {
		c.mu.Lock()
		c.cancelled = append(c.cancelled, sha)
		c.mu.Unlock()
	}
	c.e.svc.BaseUpdated = func(_ context.Context, _ *store.User, ups []hooks.RefUpdate) {
		c.mu.Lock()
		c.updates = append(c.updates, ups...)
		c.mu.Unlock()
	}
}

func (c *checks) set(sha, state string) {
	c.e.t.Helper()
	testutil.Must(c.e.t, c.e.st.SetCommitStatus(ctx, &store.CommitStatus{SHA: sha, Context: "ci / test", State: state}, nil))
}

func (e *env) process() {
	e.t.Helper()
	if ran, err := e.svc.Process(ctx); err != nil || !ran {
		e.t.Fatalf("Process: ran=%v err=%v", ran, err)
	}
}

func (e *env) pull(id int64) *store.Pull {
	e.t.Helper()
	p, err := e.st.PullByID(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) events(id int64) []string {
	e.t.Helper()
	evs, err := e.st.ListPullEvents(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Kind)
	}
	return out
}

func TestAutoMerge(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := &checks{e: e}
	c.wire()
	testutil.Must(t, e.st.SaveBranchProtection(ctx, &store.BranchProtection{Pattern: "main", RequiredApprovals: 1}))
	e.branch("feat", map[string]string{"feat.txt": "x"})
	p := e.open("feat")

	if err := e.svc.EnableAutoMerge(ctx, p.ID, e.reader, pulls.MergeOptions{}, false); !userErr(err) {
		t.Errorf("reader enabled auto-merge: %v", err)
	}
	testutil.Must(t, e.svc.EnableAutoMerge(ctx, p.ID, e.author, pulls.MergeOptions{Style: pulls.StyleMerge}, true))
	e.process()
	if got := e.pull(p.ID); !got.IsOpen() || !got.AutoMerge() || got.AutoMergeStyle != "merge" || got.AutoMergeByName != "author" {
		t.Fatalf("before approval: %+v", got)
	}

	e.review(p, e.rev, store.ReviewApproved, p.HeadSHA)
	e.process()
	got := e.pull(p.ID)
	if !got.IsMerged() || got.AutoMerge() || *got.MergeStyle != "merge" || got.MergedByName != "author" {
		t.Fatalf("after approval: %+v", got)
	}
	if len(c.updates) != 1 || c.updates[0].Ref != "refs/heads/main" || c.updates[0].NewSHA != *got.MergeSHA {
		t.Errorf("push pipelines for main: %+v", c.updates)
	}
	if _, err := e.repo.ResolveCommit(ctx, "refs/heads/feat"); err == nil {
		t.Error("the head branch was not deleted")
	}
	evs := e.events(p.ID)
	if !slices.Contains(evs, "auto_merge_enabled") || !slices.Contains(evs, "merged") {
		t.Errorf("events = %v", evs)
	}

	// Disabling stops it.
	e.branch("other", map[string]string{"other.txt": "x"})
	p2 := e.open("other")
	testutil.Must(t, e.svc.EnableAutoMerge(ctx, p2.ID, e.author, pulls.MergeOptions{}, false))
	testutil.Must(t, e.svc.DisableAutoMerge(ctx, p2.ID, e.author))
	e.review(p2, e.rev, store.ReviewApproved, p2.HeadSHA)
	e.process()
	if got := e.pull(p2.ID); !got.IsOpen() || got.AutoMerge() {
		t.Errorf("disabled auto-merge merged anyway: %+v", got)
	}

	// Closing a PR ends auto-merge.
	testutil.Must(t, e.svc.EnableAutoMerge(ctx, p2.ID, e.author, pulls.MergeOptions{}, false))
	testutil.Must(t, e.svc.SetState(ctx, p2.ID, e.author, false))
	if e.pull(p2.ID).AutoMerge() {
		t.Error("closed PR keeps auto-merge")
	}
}

func TestMergeQueue(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := &checks{e: e}
	c.wire()
	testutil.Must(t, e.st.SaveBranchProtection(ctx, &store.BranchProtection{Pattern: "main", RequireMergeQueue: true,
		RequiredChecks: []string{"ci / test"}, MergeQueueDepth: 5}))
	var prs []*store.Pull
	for _, n := range []string{"a", "b", "c"} {
		head := e.branch(n, map[string]string{n + ".txt": n})
		c.set(head, "success") // the PR's own check, required to queue
		prs = append(prs, e.open(n))
	}
	if _, err := e.svc.Merge(ctx, prs[0].ID, e.author, pulls.MergeOptions{}); !userErr(err) || !strings.Contains(err.Error(), "merge queue") {
		t.Fatalf("direct merge: %v", err)
	}
	if !e.status(prs[0]).QueueRequired {
		t.Error("status does not say the queue is required")
	}
	for _, p := range prs {
		testutil.Must(t, e.svc.Enqueue(ctx, p.ID, e.author, pulls.MergeOptions{Style: pulls.StyleSquash}, false))
	}
	if err := e.svc.Enqueue(ctx, prs[0].ID, e.author, pulls.MergeOptions{}, false); !userErr(err) {
		t.Errorf("queued twice: %v", err)
	}

	// Three stacked candidates: each on top of the one before.
	e.process()
	if len(c.started) != 3 || c.started[0][1] != e.base || c.started[1][1] != c.started[0][0] || c.started[2][1] != c.started[1][0] {
		t.Fatalf("candidates = %v (base %s)", c.started, e.base)
	}
	cand := []string{c.started[0][0], c.started[1][0], c.started[2][0]}
	for i, sha := range cand {
		if ref, _ := e.repo.ResolveCommit(ctx, pulls.QueueRef(int64(i+1))); ref != sha {
			t.Errorf("queue ref %d = %s, want %s", i+1, ref, sha)
		}
	}
	// Nothing lands while checks run.
	e.process()
	if tip, _ := e.repo.ResolveCommit(ctx, "refs/heads/main"); tip != e.base || len(c.started) != 3 {
		t.Fatalf("landed or rebuilt early: tip %s, started %d", tip, len(c.started))
	}

	// b's candidate passes first: it contains a, so both land.
	c.set(cand[1], "success")
	e.process()
	if tip, _ := e.repo.ResolveCommit(ctx, "refs/heads/main"); tip != cand[1] {
		t.Fatalf("main = %s, want %s", tip, cand[1])
	}
	for i, p := range prs[:2] {
		got := e.pull(p.ID)
		if !got.IsMerged() || *got.MergeSHA != cand[i] || *got.MergeStyle != "squash" || got.MergedByName != "author" {
			t.Errorf("PR %s = %+v", p.HeadBranch, got)
		}
	}
	if len(c.updates) != 1 || c.updates[0].OldSHA != e.base || c.updates[0].NewSHA != cand[1] {
		t.Errorf("base updates = %+v", c.updates)
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := e.repo.ReadBlob(ctx, "refs/heads/main", f, 0); err != nil {
			t.Errorf("%s not on main: %v", f, err)
		}
	}
	if _, err := e.repo.ResolveCommit(ctx, pulls.QueueRef(1)); err == nil {
		t.Error("queue ref of a merged entry remains")
	}
	// c was built on b's candidate, which is main now: it keeps testing.
	if len(c.started) != 3 {
		t.Errorf("c was rebuilt: %v", c.started)
	}

	// c fails: it leaves the queue.
	c.set(cand[2], "failure")
	e.process()
	got := e.pull(prs[2].ID)
	if !got.IsOpen() {
		t.Fatalf("failed PR = %+v", got)
	}
	if _, err := e.st.ActiveQueueEntry(ctx, got.ID); err == nil {
		t.Error("failed PR still queued")
	}
	if !slices.Contains(e.events(got.ID), "queue_failed") || !slices.Contains(c.cancelled, cand[2]) {
		t.Errorf("events %v, cancelled %v", e.events(got.ID), c.cancelled)
	}
	recent, _ := e.st.RecentQueueEntries(ctx, 10)
	if len(recent) != 3 || recent[0].State != store.QueueFailed || !strings.Contains(recent[0].Reason, `"ci / test" failed`) {
		t.Errorf("recent = %+v", recent[0])
	}
}

func TestMergeQueueRebuildsAndRemoves(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := &checks{e: e, noCheck: true}
	c.wire()
	// No required checks: a candidate passes once every check on it passed,
	// at once when there are none.
	testutil.Must(t, e.st.SaveBranchProtection(ctx, &store.BranchProtection{Pattern: "main", RequireMergeQueue: true, MergeQueueDepth: 1}))
	e.branch("x", map[string]string{"same.txt": "x\n"})
	e.branch("y", map[string]string{"same.txt": "y\n"})
	e.branch("z", map[string]string{"z.txt": "z"})
	px, py, pz := e.open("x"), e.open("y"), e.open("z")
	for _, p := range []*store.Pull{px, py, pz} {
		testutil.Must(t, e.svc.Enqueue(ctx, p.ID, e.author, pulls.MergeOptions{Style: pulls.StyleRebase}, true))
	}
	// A push to a queued PR takes it out.
	e.work.Git("checkout", "-q", "z")
	e.work.Commit("more", map[string]string{"z2.txt": "z"})
	e.work.Git("checkout", "-q", "main")
	e.push("z")
	if _, err := e.st.ActiveQueueEntry(ctx, pz.ID); err == nil {
		t.Fatal("pushed PR still queued")
	}

	// Depth 1: x is tested alone and lands; y then conflicts with it.
	e.process()
	if !e.pull(px.ID).IsMerged() {
		t.Fatalf("x = %+v", e.pull(px.ID))
	}
	if _, err := e.repo.ResolveCommit(ctx, "refs/heads/x"); err == nil {
		t.Error("x's branch was not deleted")
	}
	got := e.pull(py.ID)
	if !got.IsOpen() {
		t.Fatalf("y = %+v", got)
	}
	recent, _ := e.st.RecentQueueEntries(ctx, 10)
	var reasons []string
	for _, r := range recent {
		reasons = append(reasons, r.State+": "+r.Reason)
	}
	if !strings.Contains(strings.Join(reasons, "\n"), "failed: it conflicts with main") {
		t.Errorf("queue history = %q", reasons)
	}
	if err := e.svc.Dequeue(ctx, py.ID, e.author); !userErr(err) {
		t.Errorf("dequeue of a PR not in the queue: %v", err)
	}
}
