package store_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"onegit/internal/store"
	"onegit/internal/testutil"
)

var ctx = context.Background()

func newStore(t *testing.T) *store.Store {
	t.Helper()
	return testutil.Store(t, testutil.Config(t))
}

func TestMigrationsAreIdempotent(t *testing.T) {
	t.Parallel()
	cfg := testutil.Config(t)
	// Several replicas starting at once must not race on migrations.
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := store.Open(ctx, cfg.DatabaseURL(), 2)
			if err == nil {
				st.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	st := testutil.Store(t, cfg)
	testutil.Must(t, st.Ping(ctx))
}

func TestOpenErrors(t *testing.T) {
	if _, err := store.Open(ctx, "::not a url::", 1); err == nil {
		t.Error("bad URL accepted")
	}
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := store.Open(c, "postgres://u@127.0.0.1:1/db?sslmode=disable", 1); err == nil {
		t.Error("unreachable database accepted")
	}
}

func TestSecret(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	var wg sync.WaitGroup
	results := make([][]byte, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := st.Secret(ctx, "key", func() ([]byte, error) { return []byte{byte(i)}, nil })
			if err != nil {
				t.Error(err)
			}
			results[i] = v
		}()
	}
	wg.Wait()
	for _, r := range results {
		if !bytes.Equal(r, results[0]) {
			t.Fatalf("concurrent Secret calls returned different values: %v", results)
		}
	}
	if _, err := st.Secret(ctx, "other", func() ([]byte, error) { return nil, errors.New("boom") }); err == nil {
		t.Error("generator error swallowed")
	}
}

func TestUsers(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	created, err := st.CreateInitialAdmin(ctx, &store.User{Username: "admin", PasswordHash: "h"})
	if err != nil || !created {
		t.Fatalf("CreateInitialAdmin = %v, %v", created, err)
	}
	if created, _ := st.CreateInitialAdmin(ctx, &store.User{Username: "admin2", PasswordHash: "h"}); created {
		t.Error("a second initial admin was created")
	}
	admin, err := st.UserByUsername(ctx, "ADMIN")
	if err != nil || admin.Role != store.RoleAdmin || !admin.MustChangePassword || !admin.Active || !admin.IsAdmin() {
		t.Fatalf("initial admin = %+v, %v", admin, err)
	}

	u := &store.User{Username: "Alice", Email: "alice@example.com", Role: store.RoleWrite, Active: true}
	testutil.Must(t, st.CreateUser(ctx, u))
	if u.ID == 0 || u.CreatedAt.IsZero() {
		t.Error("id/created_at not returned")
	}
	if err := st.CreateUser(ctx, &store.User{Username: "alice", Role: store.RoleRead}); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("case-insensitive duplicate: %v", err)
	}
	sub := "oidc|1"
	u.FullName, u.OIDCSubject, u.Role = "Alice A", &sub, store.RoleRead
	testutil.Must(t, st.UpdateUser(ctx, u))
	got, err := st.UserByOIDCSubject(ctx, sub)
	if err != nil || got.ID != u.ID || got.FullName != "Alice A" || got.DisplayName() != "Alice A" || !got.HasSSO() || got.CanWrite() {
		t.Errorf("updated user = %+v, %v", got, err)
	}
	u.Username = "admin"
	if err := st.UpdateUser(ctx, u); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("rename to an existing name: %v", err)
	}
	users, _ := st.ListUsers(ctx)
	if len(users) != 2 || users[0].Username != "admin" || users[1].Username != "Alice" {
		t.Errorf("ListUsers = %v", users)
	}
	if found, total, _ := st.FindUsers(ctx, "ALI", 10, 0); total != 1 || len(found) != 1 || found[0].Username != "Alice" {
		t.Errorf("FindUsers(ALI) = %v, %d", found, total)
	}
	if found, total, _ := st.FindUsers(ctx, "", 1, 1); total != 2 || len(found) != 1 || found[0].Username != "Alice" {
		t.Errorf("FindUsers second page = %v, %d", found, total)
	}
	byLogin, _ := st.UsersByLogin(ctx, []string{"admin"}, []string{"alice@example.com"})
	if len(byLogin) != 2 {
		t.Errorf("UsersByLogin = %d users", len(byLogin))
	}
	testutil.Must(t, st.DeleteUser(ctx, u.ID))
	for _, f := range []func() (*store.User, error){
		func() (*store.User, error) { return st.UserByID(ctx, u.ID) },
		func() (*store.User, error) { return st.UserByUsername(ctx, "alice") },
		func() (*store.User, error) { return st.UserByOIDCSubject(ctx, sub) },
	} {
		if _, err := f(); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("deleted user lookup: %v", err)
		}
	}

	for r, want := range map[store.Role][2]bool{store.RoleRead: {true, false}, store.RoleWrite: {true, true},
		store.RoleAdmin: {true, true}, "root": {false, false}} {
		if r.Valid() != want[0] || r.CanWrite() != want[1] {
			t.Errorf("role %q: valid=%v canWrite=%v", r, r.Valid(), r.CanWrite())
		}
	}
	var nilUser *store.User
	if nilUser.IsAdmin() || nilUser.CanWrite() {
		t.Error("nil user has rights")
	}
	inactive := &store.User{Role: store.RoleAdmin}
	if inactive.IsAdmin() || inactive.CanWrite() {
		t.Error("inactive admin has rights")
	}
}

func TestTokens(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	u := testutil.User(t, st, "tok", store.RoleRead)
	other := testutil.User(t, st, "other", store.RoleRead)
	tk := &store.Token{UserID: u.ID, Name: "ci", Prefix: "og_abc"}
	testutil.Must(t, st.AddToken(ctx, tk, "hash1"))
	got, err := st.UserByTokenHash(ctx, "hash1")
	if err != nil || got.ID != u.ID {
		t.Fatalf("UserByTokenHash = %v, %v", got, err)
	}
	if _, err := st.UserByTokenHash(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown token: %v", err)
	}
	// Another user cannot delete someone else's token.
	testutil.Must(t, st.DeleteToken(ctx, other.ID, tk.ID))
	if list, _ := st.ListTokens(ctx, u.ID); len(list) != 1 || list[0].LastUsed == nil || list[0].Name != "ci" {
		t.Errorf("tokens = %+v", list)
	}
	testutil.Must(t, st.DeleteToken(ctx, u.ID, tk.ID))
	if list, _ := st.ListTokens(ctx, u.ID); len(list) != 0 {
		t.Error("token not deleted")
	}
	// Deleting a user removes their tokens.
	testutil.Must(t, st.AddToken(ctx, &store.Token{UserID: u.ID, Name: "x", Prefix: "og_x"}, "hash2"))
	testutil.Must(t, st.DeleteUser(ctx, u.ID))
	if _, err := st.UserByTokenHash(ctx, "hash2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("token of a deleted user: %v", err)
	}
}

func TestPullsStore(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	author := testutil.User(t, st, "author", store.RoleWrite)
	p := &store.Pull{Title: "One", AuthorID: &author.ID, HeadBranch: "feat", BaseBranch: "main",
		HeadSHA: strings.Repeat("a", 40), MergeBase: strings.Repeat("b", 40)}
	testutil.Must(t, st.CreatePull(ctx, p))
	if p.ID != 1 || p.State != store.PullOpen || !p.IsOpen() || !p.IsAuthor(author) || p.IsAuthor(nil) {
		t.Fatalf("created pull = %+v", p)
	}
	dup := *p
	if err := st.CreatePull(ctx, &dup); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("second open PR for the same branches: %v", err)
	}
	if open, err := st.OpenPullFor(ctx, "feat", "main"); err != nil || open.ID != p.ID {
		t.Errorf("OpenPullFor = %v, %v", open, err)
	}
	if _, err := st.OpenPullFor(ctx, "feat", "dev"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("OpenPullFor(other base): %v", err)
	}
	for _, b := range []string{"feat", "main"} {
		if list, _ := st.OpenPullsForBranch(ctx, b); len(list) != 1 {
			t.Errorf("OpenPullsForBranch(%s) = %d", b, len(list))
		}
	}
	if byHead, _ := st.OpenPullsByHead(ctx, []string{"feat", "other"}); len(byHead["feat"]) != 1 || len(byHead) != 1 {
		t.Errorf("OpenPullsByHead = %v", byHead)
	}

	// UpdatePullLocked saves what fn changed; an error rolls back.
	_, err := st.UpdatePullLocked(ctx, p.ID, func(p *store.Pull) error {
		p.Title = "changed"
		return errors.New("abort")
	})
	if err == nil {
		t.Error("fn error not returned")
	}
	now := time.Now()
	sha, style := strings.Repeat("c", 40), "squash"
	merged, err := st.UpdatePullLocked(ctx, p.ID, func(p *store.Pull) error {
		p.State, p.MergedAt, p.MergedBy, p.MergeSHA, p.MergeStyle = store.PullMerged, &now, &author.ID, &sha, &style
		return nil
	})
	if err != nil || !merged.IsMerged() {
		t.Fatalf("merge update = %+v, %v", merged, err)
	}
	got, _ := st.PullByID(ctx, p.ID)
	if got.Title != "One" || *got.MergeSHA != sha || got.MergedByName != "author" || got.AuthorName != "author" {
		t.Errorf("pull after update = %+v", got)
	}
	if _, err := st.UpdatePullLocked(ctx, 999, func(*store.Pull) error { return nil }); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update of a missing pull: %v", err)
	}

	// A new PR for the same branches is allowed once the first is merged.
	p2 := &store.Pull{Title: "Two", HeadBranch: "feat", BaseBranch: "main", HeadSHA: sha, MergeBase: sha}
	testutil.Must(t, st.CreatePull(ctx, p2))
	if open, closed, _ := st.CountPulls(ctx); open != 1 || closed != 1 {
		t.Errorf("CountPulls = %d, %d", open, closed)
	}
	for state, want := range map[string]int{"open": 1, "closed": 1} {
		if list, _ := st.ListPulls(ctx, state, 10, 0); len(list) != want {
			t.Errorf("ListPulls(%s) = %d", state, len(list))
		}
	}

	testutil.Must(t, st.AddPullEvent(ctx, p.ID, &author.ID, "pushed", map[string]any{"count": 2}))
	testutil.Must(t, st.AddPullEvent(ctx, p.ID, nil, "closed", nil))
	events, _ := st.ListPullEvents(ctx, p.ID)
	if len(events) != 2 || events[0].Kind != "pushed" || events[0].Data["count"] != float64(2) {
		t.Errorf("events = %+v", events)
	}

	path, side, line := "a.go", "new", 3
	c := &store.PullComment{PullID: p.ID, AuthorID: &author.ID, Body: "nit", Path: &path, Side: &side, Line: &line, CommitSHA: &sha}
	testutil.Must(t, st.AddPullComment(ctx, c))
	testutil.Must(t, st.AddPullComment(ctx, &store.PullComment{PullID: p.ID, AuthorID: &author.ID, Body: "general"}))
	comments, _ := st.ListPullComments(ctx, p.ID)
	if len(comments) != 2 || !comments[0].IsLine() || comments[1].IsLine() || comments[0].AuthorName != "author" {
		t.Errorf("comments = %+v", comments)
	}
	stranger := testutil.User(t, st, "stranger", store.RoleWrite)
	admin := testutil.User(t, st, "boss", store.RoleAdmin)
	if !c.CanDelete(author) || c.CanDelete(stranger) || !c.CanDelete(admin) || c.CanDelete(nil) {
		t.Error("CanDelete rules")
	}
	if got, err := st.PullCommentByID(ctx, p.ID, c.ID); err != nil || got.Body != "nit" {
		t.Errorf("PullCommentByID = %v, %v", got, err)
	}
	if _, err := st.PullCommentByID(ctx, p2.ID, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("comment of another pull: %v", err)
	}
	testutil.Must(t, st.DeletePullComment(ctx, p.ID, c.ID))

	r := &store.PullReview{PullID: p.ID, ReviewerID: &stranger.ID, State: store.ReviewApproved, CommitSHA: sha}
	testutil.Must(t, st.AddPullReview(ctx, r))
	reviews, _ := st.ListPullReviews(ctx, p.ID)
	if len(reviews) != 1 || reviews[0].ReviewerName != "stranger" || reviews[0].State != store.ReviewApproved {
		t.Errorf("reviews = %+v", reviews)
	}
	// Deleting a user keeps their PRs, shown as "ghost".
	testutil.Must(t, st.DeleteUser(ctx, author.ID))
	got, _ = st.PullByID(ctx, p.ID)
	if got.AuthorID != nil || got.AuthorName != "ghost" {
		t.Errorf("pull of a deleted author = %+v", got)
	}
}

func TestBranchProtections(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	for _, p := range []string{"main", "release/*", "*"} {
		testutil.Must(t, st.SaveBranchProtection(ctx, &store.BranchProtection{Pattern: p, RequiredApprovals: len(p)}))
	}
	if err := st.SaveBranchProtection(ctx, &store.BranchProtection{Pattern: "main"}); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("duplicate pattern: %v", err)
	}
	// Globs follow path.Match: "*" does not cross "/".
	if got, _ := st.BranchProtectionFor(ctx, "release/a/b"); got != nil {
		t.Errorf("release/a/b matched %q", got.Pattern)
	}
	for branch, want := range map[string]string{"main": "main", "release/1.0": "release/*", "dev": "*"} {
		got, err := st.BranchProtectionFor(ctx, branch)
		if err != nil || got == nil || got.Pattern != want {
			t.Errorf("BranchProtectionFor(%s) = %+v, %v; want %s", branch, got, err, want)
		}
	}
	rules, _ := st.ListBranchProtections(ctx)
	main := rules[slices.IndexFunc(rules, func(r *store.BranchProtection) bool { return r.Pattern == "main" })]
	main.RequiredChecks, main.Owners, main.AllowForcePush = []string{"ci / test"}, "* @lead", true
	testutil.Must(t, st.SaveBranchProtection(ctx, main))
	got, _ := st.BranchProtectionFor(ctx, "main")
	if !got.AllowForcePush || got.Owners != "* @lead" || !slices.Equal(got.RequiredChecks, []string{"ci / test"}) {
		t.Errorf("updated rule = %+v", got)
	}
	testutil.Must(t, st.DeleteBranchProtection(ctx, main.ID))
	if got, _ := st.BranchProtectionFor(ctx, "main"); got == nil || got.Pattern != "*" {
		t.Errorf("after delete = %+v", got)
	}
	if store.MatchBranchProtection(nil, "main") != nil {
		t.Error("no rules must match nothing")
	}
	// Among globs the longest pattern wins, whatever the order.
	rs := []*store.BranchProtection{{Pattern: "r*"}, {Pattern: "rel*"}, {Pattern: "re*"}}
	if got := store.MatchBranchProtection(rs, "release"); got.Pattern != "rel*" {
		t.Errorf("longest glob: %s", got.Pattern)
	}
}

func TestTeamsAndAudit(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	a := testutil.User(t, st, "ann", store.RoleWrite)
	b := testutil.User(t, st, "ben", store.RoleWrite)
	ops := &store.Team{Name: "Ops", Description: "on call", OIDCGroup: "idp-ops"}
	testutil.Must(t, st.SaveTeam(ctx, ops))
	dev := &store.Team{Name: "dev"}
	testutil.Must(t, st.SaveTeam(ctx, dev))
	if err := st.SaveTeam(ctx, &store.Team{Name: "ops"}); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("duplicate team name: %v", err)
	}
	testutil.Must(t, st.AddTeamMember(ctx, dev.ID, a.ID))
	testutil.Must(t, st.AddTeamMember(ctx, dev.ID, a.ID)) // idempotent
	testutil.Must(t, st.AddTeamMember(ctx, dev.ID, b.ID))
	testutil.Must(t, st.SyncOIDCTeams(ctx, a.ID, []string{"idp-ops", "unknown"}))
	if teams, _ := st.UserTeams(ctx, a.ID); !slices.Equal(sorted(teams), []string{"dev", "ops"}) {
		t.Errorf("UserTeams = %v", teams)
	}
	// Sync never touches teams without an OIDC group.
	testutil.Must(t, st.SyncOIDCTeams(ctx, a.ID, []string{}))
	if teams, _ := st.UserTeams(ctx, a.ID); !slices.Equal(teams, []string{"dev"}) {
		t.Errorf("after sync with no groups = %v", teams)
	}
	ids, _ := st.TeamMemberIDs(ctx, []string{"DEV", "ops"})
	if len(ids["dev"]) != 2 || len(ids["ops"]) != 0 {
		t.Errorf("TeamMemberIDs = %v", ids)
	}
	b.Active = false
	testutil.Must(t, st.UpdateUser(ctx, b))
	if ids, _ := st.TeamMemberIDs(ctx, []string{"dev"}); len(ids["dev"]) != 1 {
		t.Errorf("inactive members must be ignored: %v", ids)
	}
	list, _ := st.ListTeams(ctx)
	if len(list) != 2 || list[0].Name != "dev" || !slices.Equal(list[0].Members, []string{"ann", "ben"}) {
		t.Errorf("ListTeams = %+v", list)
	}
	got, err := st.TeamByID(ctx, dev.ID)
	if err != nil || len(got.Members) != 2 {
		t.Errorf("TeamByID = %+v, %v", got, err)
	}
	ops.Name = "operations"
	testutil.Must(t, st.SaveTeam(ctx, ops))
	testutil.Must(t, st.RemoveTeamMember(ctx, dev.ID, b.ID))
	testutil.Must(t, st.DeleteTeam(ctx, dev.ID))
	if _, err := st.TeamByID(ctx, dev.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted team: %v", err)
	}

	testutil.Must(t, st.Audit(ctx, &a.ID, "deploy.request", "deployment #1", map[string]any{"sha": "abc"}))
	testutil.Must(t, st.Audit(ctx, nil, "gc", "registry", nil))
	entries, _ := st.ListAudit(ctx, 10)
	if len(entries) != 2 || entries[0].ActorName != "system" || entries[1].ActorName != "ann" || entries[1].Data["sha"] != "abc" {
		t.Errorf("audit = %+v", entries)
	}
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

func TestDeployStore(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	u := testutil.User(t, st, "deployer", store.RoleWrite)

	testutil.Must(t, st.SaveDeployDimension(ctx, &store.DeployDimension{Name: "environment", Position: 2, Source: "list",
		Values: []string{"dev", "prod"}}))
	testutil.Must(t, st.SaveDeployDimension(ctx, &store.DeployDimension{Name: "project", Position: 1, Source: "paths",
		PathPattern: "projects/*/project.yaml"}))
	dims, _ := st.ListDeployDimensions(ctx)
	if len(dims) != 2 || dims[0].Name != "project" || dims[0].Values == nil || !slices.Equal(dims[1].Values, []string{"dev", "prod"}) {
		t.Errorf("dimensions = %+v", dims)
	}
	testutil.Must(t, st.SaveDeployDimension(ctx, &store.DeployDimension{Name: "project", Position: 3, Source: "list"}))
	testutil.Must(t, st.DeleteDeployDimension(ctx, "environment"))
	if dims, _ := st.ListDeployDimensions(ctx); len(dims) != 1 || dims[0].Source != "list" {
		t.Errorf("after update/delete = %+v", dims)
	}

	r := &store.DeployRule{Kind: "require", Description: "prod needs approval",
		Selector: store.Selector{"environment": {"prod"}}, Approvals: 1,
		Approvers: []store.Principal{{Type: "team", Value: "sre"}}, ExceptBranches: []string{"main"}}
	testutil.Must(t, st.SaveDeployRule(ctx, r))
	got, err := st.DeployRuleByID(ctx, r.ID)
	if err != nil || got.Approvers[0].Value != "sre" || !slices.Equal(got.ExceptBranches, []string{"main"}) || got.Principals == nil {
		t.Fatalf("rule = %+v, %v", got, err)
	}
	// Every field must survive an update (except_branches used to be lost).
	got.ExceptBranches, got.Freeze, got.FreezeMessage, got.Branches = []string{"main", "release/*"}, true, "holidays", []string{"main"}
	testutil.Must(t, st.SaveDeployRule(ctx, got))
	again, _ := st.DeployRuleByID(ctx, r.ID)
	if !slices.Equal(again.ExceptBranches, []string{"main", "release/*"}) || !again.Freeze || again.FreezeMessage != "holidays" ||
		!slices.Equal(again.Branches, []string{"main"}) {
		t.Errorf("updated rule = %+v", again)
	}
	testutil.Must(t, st.SaveDeployRule(ctx, &store.DeployRule{Kind: "grant", Principals: []store.Principal{{Type: "role", Value: "write"}}}))
	if rules, _ := st.ListDeployRules(ctx); len(rules) != 2 || rules[0].Kind != "grant" {
		t.Errorf("rules = %+v", rules)
	}
	testutil.Must(t, st.DeleteDeployRule(ctx, r.ID))
	if _, err := st.DeployRuleByID(ctx, r.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted rule: %v", err)
	}

	sec := &store.DeploySecret{Name: "TOKEN", Value: []byte("enc1")}
	testutil.Must(t, st.SaveDeploySecret(ctx, sec, u.ID))
	sec.Value, sec.Selector = nil, store.Selector{"environment": {"prod"}}
	testutil.Must(t, st.SaveDeploySecret(ctx, sec, u.ID)) // empty value keeps the old one
	secrets, _ := st.ListDeploySecrets(ctx)
	if len(secrets) != 1 || string(secrets[0].Value) != "enc1" || secrets[0].UpdatedBy != "deployer" || secrets[0].Selector["environment"][0] != "prod" {
		t.Errorf("secrets = %+v", secrets)
	}
	testutil.Must(t, st.DeleteDeploySecret(ctx, sec.ID))

	d := &store.Deployment{SHA: strings.Repeat("d", 40), Ref: "main", RecipeSHA: strings.Repeat("e", 40),
		Targets: []map[string]string{{"environment": "prod"}}, RequestedBy: &u.ID, Status: "pending"}
	testutil.Must(t, st.CreateDeployment(ctx, d))
	testutil.Must(t, st.SetDeploymentApproval(ctx, d.ID, u.ID, "approve", "lgtm"))
	testutil.Must(t, st.SetDeploymentApproval(ctx, d.ID, u.ID, "reject", "changed my mind"))
	apps, _ := st.DeploymentApprovals(ctx, d.ID)
	if len(apps) != 1 || apps[0].Verdict != "reject" || apps[0].Username != "deployer" {
		t.Errorf("approvals = %+v", apps)
	}
	upd, err := st.UpdateDeploymentLocked(ctx, d.ID, func(d *store.Deployment) error {
		d.Status = "rejected"
		d.Evaluation = map[string]any{"ok": false}
		return nil
	})
	if err != nil || upd.Status != "rejected" {
		t.Fatalf("UpdateDeploymentLocked = %+v, %v", upd, err)
	}
	got2, _ := st.DeploymentByID(ctx, d.ID)
	if got2.FinishedAt == nil || got2.Evaluation["ok"] != false || got2.RequestedByName != "deployer" {
		t.Errorf("deployment = %+v", got2)
	}
	if list, _ := st.ListDeployments(ctx, "rejected", 10, 0); len(list) != 1 {
		t.Errorf("ListDeployments(rejected) = %d", len(list))
	}
	if list, _ := st.ListDeployments(ctx, "pending", 10, 0); len(list) != 0 {
		t.Errorf("ListDeployments(pending) = %d", len(list))
	}
	if _, err := st.DeploymentByID(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing deployment: %v", err)
	}
	if _, err := st.UpdateDeploymentLocked(ctx, 999, func(*store.Deployment) error { return nil }); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update of a missing deployment: %v", err)
	}
}
