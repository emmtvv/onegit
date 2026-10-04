package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"onegit/internal/config"
	"onegit/internal/runner"
	"onegit/internal/server"
	"onegit/internal/store"
	"onegit/internal/testutil"
)

var ctx = context.Background()

func TestHealth(t *testing.T) {
	t.Parallel()
	h := start(t, nil)
	b := h.browser()
	if p := b.get("/healthz"); p.status != http.StatusOK || p.body != "ok" {
		t.Errorf("/healthz: %d %q", p.status, p.body)
	}
	if p := b.get("/readyz"); p.status != http.StatusOK {
		t.Errorf("/readyz: %d %q", p.status, p.body)
	}
}

func TestBootstrapAndAccounts(t *testing.T) {
	t.Parallel()
	h := start(t, nil)
	anon := h.browser()
	if p := anon.get("/"); p.status != http.StatusSeeOther || p.location != "/login?next=%2F" {
		t.Errorf("anonymous /: %d %q", p.status, p.location)
	}
	anon.ok("/login", "Sign in")
	if p := anon.post("/login", "username", "admin", "password", "wrong-password"); p.status != http.StatusUnauthorized ||
		!strings.Contains(p.body, "Incorrect username or password") {
		t.Errorf("wrong password: %d", p.status)
	}

	// The initial password only opens the change-password page.
	b := h.browser()
	if p := b.post("/login", "username", "admin", "password", adminInitial, "next", "/pulls"); p.location != "/pulls" {
		t.Fatalf("login redirect = %q", p.location)
	}
	if p := b.get("/pulls"); p.status != http.StatusSeeOther || !strings.HasPrefix(p.location, "/password/change?next=") {
		t.Errorf("before the password change: %d %q", p.status, p.location)
	}
	b.ok("/password/change")
	for name, form := range map[string][]string{
		"wrong current": {"current", "nope-nope-nope", "new", "new-password-1", "confirm", "new-password-1"},
		"mismatch":      {"current", adminInitial, "new", "new-password-1", "confirm", "new-password-2"},
		"unchanged":     {"current", adminInitial, "new", adminInitial, "confirm", adminInitial},
		"too short":     {"current", adminInitial, "new", "short", "confirm", "short"},
	} {
		if p := b.post("/password/change", form...); p.status != http.StatusBadRequest {
			t.Errorf("%s: %d", name, p.status)
		}
	}
	// Git refuses the initial password too.
	w := testutil.NewWork(t)
	if out, err := git(t, w.Dir, "ls-remote", h.remote("admin", adminInitial)); err == nil || !strings.Contains(out, "403") {
		t.Errorf("git with the initial password: %v\n%s", err, out)
	}
	if p := b.post("/password/change", "current", adminInitial, "new", "admin-password-1", "confirm", "admin-password-1", "next", "/pulls"); p.location != "/pulls" {
		t.Fatalf("password change: %d %q", p.status, p.location)
	}
	b.ok("/", "git clone") // empty repository page
	if _, err := git(t, w.Dir, "ls-remote", h.remote("admin", "admin-password-1")); err != nil {
		t.Errorf("git with the new password: %v", err)
	}

	// Users, roles and admin-only pages.
	dev := h.createUser(b, "dev", "write")
	b.ok("/admin/users", "dev", "admin")
	if body := b.ok("/admin/users?q=DE", "dev", "1 found"); strings.Contains(body, `href="/admin/users/1"`) {
		t.Errorf("user search shows admin:\n%.500s", body)
	}
	if p := b.post("/admin/users", "username", "dev", "role", "read"); !strings.Contains(p.header.Get("Set-Cookie"), "taken") {
		t.Errorf("duplicate user: %v", p.header)
	}
	if p := b.post("/admin/users", "username", "bad name!"); p.status != http.StatusSeeOther {
		t.Errorf("invalid name: %d", p.status)
	}
	if p := dev.get("/admin/users"); p.status != http.StatusForbidden {
		t.Errorf("admin page as a writer: %d", p.status)
	}
	dev.ok("/settings", "dev@example.com")

	// Access tokens are shown once and work for git.
	body := dev.post("/settings/tokens", "name", "laptop").body
	tok := accessTokenRe.FindString(body)
	if tok == "" {
		t.Fatalf("token not shown:\n%.500s", body)
	}
	if _, err := git(t, w.Dir, "ls-remote", h.remote("whatever", tok)); err != nil {
		t.Errorf("git with a token: %v", err)
	}
	if strings.Contains(dev.ok("/settings/tokens", "laptop"), tok) {
		t.Error("token shown again")
	}

	// Deactivating a user ends their sessions.
	users, _ := h.srv.Store.ListUsers(ctx)
	var devID int64
	for _, u := range users {
		if u.Username == "dev" {
			devID = u.ID
		}
	}
	if p := b.post(fmt.Sprintf("/admin/users/%d", devID), "email", "dev@example.com", "role", "write"); p.status != http.StatusSeeOther {
		t.Errorf("deactivate: %d", p.status)
	}
	if p := dev.get("/settings"); p.status != http.StatusSeeOther {
		t.Errorf("session of a deactivated user: %d", p.status)
	}

	// Logout.
	b.post("/logout")
	if p := b.get("/admin/users"); p.status != http.StatusSeeOther {
		t.Errorf("after logout: %d", p.status)
	}
	// Cross-origin form posts are refused (CSRF).
	req, _ := http.NewRequest("POST", h.url+"/login", strings.NewReader("username=admin"))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if p := anon.do(req); p.status != http.StatusForbidden {
		t.Errorf("cross-site POST: %d", p.status)
	}
}

func TestBootstrapNeedsPassword(t *testing.T) {
	t.Parallel()
	cfg := testutil.Config(t)
	cfg.Admin.InitialPassword = ""
	if _, err := server.New(ctx, cfg, testutil.Logger()); err == nil || !strings.Contains(err.Error(), "ONEGIT_ADMIN_PASSWORD") {
		t.Errorf("empty database without an admin password: %v", err)
	}
}

func TestLoginRateLimit(t *testing.T) {
	t.Parallel()
	h := start(t, func(c *config.Config) { c.HTTP.TrustProxy = true })
	b := h.browser()
	post := func(ip, pw string) page {
		req, _ := http.NewRequest("POST", h.url+"/login", strings.NewReader("username=admin&password="+pw))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Real-IP", ip)
		return b.do(req)
	}
	for i := 0; i < 10; i++ {
		post("10.0.0.1", "wrong-password")
	}
	if p := post("10.0.0.1", adminInitial); p.status != http.StatusUnauthorized || !strings.Contains(p.body, "Too many failed attempts") {
		t.Errorf("locked out: %d", p.status)
	}
	if p := post("10.0.0.2", adminInitial); p.status != http.StatusSeeOther {
		t.Errorf("another IP: %d", p.status)
	}
}

func TestGitPushPolicyAndPullRequests(t *testing.T) {
	t.Parallel()
	h := start(t, nil)
	admin := h.admin()
	dev := h.createUser(admin, "dev", "write")
	rev := h.createUser(admin, "rev", "write")
	h.createUser(admin, "viewer", "read")
	devRemote := h.remote("dev", testutil.Password("dev"))

	w := testutil.NewWork(t)
	w.Commit("init", map[string]string{"README.md": "# Demo\n\nHello **world**\n", "src/main.go": "package main\n", "docs/a.md": "a"})
	mustGit(t, w.Dir, "push", devRemote, "main")
	w.Git("tag", "-a", "v1.0", "-m", "v1")
	mustGit(t, w.Dir, "push", devRemote, "v1.0")

	// Clone works and HEAD points at main.
	clone := t.TempDir()
	mustGit(t, clone, "clone", "-q", devRemote, "c")
	if got := mustGit(t, filepath.Join(clone, "c"), "rev-parse", "--abbrev-ref", "HEAD"); strings.TrimSpace(got) != "main" {
		t.Errorf("cloned HEAD = %q", got)
	}

	// Push policy.
	if out, err := git(t, w.Dir, "push", h.remote("viewer", testutil.Password("viewer")), "main:refs/heads/x"); err == nil || !strings.Contains(out, "403") {
		t.Errorf("reader push: %v\n%s", err, out)
	}
	if out, err := git(t, w.Dir, "push", devRemote, "main:refs/pull/1/head"); err == nil || !strings.Contains(out, "only branches and tags") {
		t.Errorf("push to refs/pull: %v\n%s", err, out)
	}
	if out, err := git(t, w.Dir, "push", devRemote, ":main"); err == nil || !strings.Contains(out, "cannot be deleted") {
		t.Errorf("delete the default branch: %v\n%s", err, out)
	}

	// A new branch gets a "create a pull request" hint.
	w.Git("checkout", "-q", "-b", "feature/login")
	w.Commit("Add login", map[string]string{"src/login.go": "package main\n\nfunc login() {}\n"})
	out := mustGit(t, w.Dir, "push", devRemote, "feature/login")
	if !strings.Contains(out, "Create a pull request for \"feature/login\"") || !strings.Contains(out, h.url+"/pulls/new?head=feature%2Flogin") {
		t.Errorf("push hint:\n%s", out)
	}

	// Browsing.
	sha := strings.TrimSpace(w.Git("rev-parse", "main"))
	dev.ok("/", "Hello <strong>world</strong>", "README.md")
	dev.ok("/tree/main/src", "main.go")
	dev.ok("/tree/feature/login/src", "login.go")
	dev.ok("/blob/main/src/main.go", "package")
	dev.ok("/commits/main", "init")
	dev.ok("/commits/main/docs", "init")
	dev.ok("/commit/"+sha, "README.md")
	dev.ok("/branches", "feature/login")
	dev.ok("/branches?q=LOGIN", "feature/login")
	if body := dev.ok("/branches?q=nomatch"); strings.Contains(body, "feature/login") || !strings.Contains(body, "No branches match") {
		t.Errorf("filtered branches:\n%.500s", body)
	}
	dev.ok("/tags", "v1.0")
	dev.ok("/refs?view=commits&path=src&q=log", `"name":"feature/login"`, `"href":"/commits/feature/login/src"`)
	if body := dev.ok("/refs?q=v1"); !strings.Contains(body, `"tags":[{"name":"v1.0","href":"/tree/v1.0"}]`) || !strings.Contains(body, `"branches":[]`) {
		t.Errorf("refs: %s", body)
	}
	raw := dev.get("/raw/main/README.md")
	if raw.status != http.StatusOK || !strings.HasPrefix(raw.header.Get("Content-Type"), "text/plain") ||
		!strings.Contains(raw.header.Get("Content-Security-Policy"), "sandbox") || !strings.Contains(raw.body, "Hello") {
		t.Errorf("raw: %d %v", raw.status, raw.header)
	}
	for _, path := range []string{"/blob/main/missing.txt", "/tree/nope", "/commit/0123456789abcdef", "/nothing-here"} {
		if p := dev.get(path); p.status != http.StatusNotFound {
			t.Errorf("GET %s: %d", path, p.status)
		}
	}

	// Pull request through the UI.
	dev.ok("/pulls/new?head=feature/login", "Add login")
	p := dev.post("/pulls", "head", "feature/login", "base", "main", "title", "Add login", "body", "Please review")
	if p.status != http.StatusSeeOther || p.location != "/pulls/1" {
		t.Fatalf("create PR: %d %q\n%.500s", p.status, p.location, p.body)
	}
	dev.ok("/pulls", "Add login")
	dev.ok("/pulls/1", "Please review")
	dev.ok("/pulls/1/files", "login.go")
	dev.ok("/pulls/1/commits", "Add login")
	w.Commit("Fix login", map[string]string{"src/login.go": "package main\n\nfunc login() bool { return true }\n"})
	if out := mustGit(t, w.Dir, "push", devRemote, "feature/login"); !strings.Contains(out, "View pull request #1") {
		t.Errorf("push hint for an open PR:\n%s", out)
	}
	head := strings.TrimSpace(w.Git("rev-parse", "HEAD"))
	if p := dev.post("/pulls/1/comments", "body", "general remark"); p.status != http.StatusSeeOther {
		t.Errorf("comment: %d", p.status)
	}
	if p := rev.post("/pulls/1/comments", "body", "why true?", "path", "src/login.go", "side", "new", "line", "3", "commit", head); p.status != http.StatusSeeOther {
		t.Errorf("line comment: %d", p.status)
	}
	dev.ok("/pulls/1", "general remark", "why true?")

	// Protect main: pushes must go through PRs with one approval.
	if p := admin.post("/admin/branches", "pattern", "main", "require_pull_request", "on", "required_approvals", "1"); p.status != http.StatusSeeOther {
		t.Fatalf("protect main: %d", p.status)
	}
	admin.ok("/admin/branches", "main")
	w.Git("checkout", "-q", "main")
	w.Commit("direct", map[string]string{"x.txt": "x"})
	if out, err := git(t, w.Dir, "push", devRemote, "main"); err == nil || !strings.Contains(out, "through a pull request") {
		t.Errorf("direct push to protected main: %v\n%s", err, out)
	}
	w.Git("reset", "-q", "--hard", "HEAD~1")

	if p := dev.post("/pulls/1/merge", "style", "squash"); p.status != http.StatusSeeOther {
		t.Errorf("merge without approval: %d", p.status)
	}
	if pr, _ := h.srv.Store.PullByID(ctx, 1); pr.IsMerged() {
		t.Fatal("merged without the required approval")
	}
	if p := dev.post("/pulls/1/reviews", "state", "approved", "body", "self"); p.status != http.StatusSeeOther {
		t.Errorf("self review: %d", p.status)
	}
	if p := rev.post("/pulls/1/reviews", "state", "approved", "body", "LGTM"); p.status != http.StatusSeeOther {
		t.Errorf("review: %d", p.status)
	}
	if p := dev.post("/pulls/1/merge", "style", "squash", "delete_branch", "on"); p.status != http.StatusSeeOther {
		t.Fatalf("merge: %d", p.status)
	}
	pr, _ := h.srv.Store.PullByID(ctx, 1)
	if !pr.IsMerged() || *pr.MergeStyle != "squash" {
		t.Fatalf("PR = %+v", pr)
	}
	mustGit(t, w.Dir, "fetch", "-q", "--prune", devRemote, "+refs/heads/*:refs/remotes/o/*")
	if got := strings.TrimSpace(w.Git("log", "-1", "--format=%s", "o/main")); got != "Add login (#1)" {
		t.Errorf("main subject = %q", got)
	}
	if _, err := w.TryGit("rev-parse", "--verify", "o/feature/login"); err == nil {
		t.Error("head branch not deleted after merge")
	}
	dev.ok("/pulls?state=closed", "Add login")
}

func TestPublicRead(t *testing.T) {
	t.Parallel()
	h := start(t, func(c *config.Config) { c.Repo.PublicRead = true })
	admin := h.admin()
	w := testutil.NewWork(t)
	w.Commit("init", map[string]string{"README.md": "public"})
	mustGit(t, w.Dir, "push", h.remote("admin", "admin-password-1"), "main")
	anon := h.browser()
	anon.ok("/", "public")
	anon.ok("/pulls")
	if p := anon.get("/pulls/new"); p.status != http.StatusSeeOther {
		t.Errorf("anonymous PR form: %d", p.status)
	}
	if p := anon.get("/admin/users"); p.status != http.StatusSeeOther {
		t.Errorf("anonymous admin: %d", p.status)
	}
	mustGit(t, t.TempDir(), "clone", "-q", h.cfg.HTTPCloneURL(), "c")
	admin.ok("/admin/users")
}

func TestRegistryAndPackagesUI(t *testing.T) {
	t.Parallel()
	h := start(t, nil)
	admin := h.admin()
	req, _ := http.NewRequest("GET", h.url+"/v2/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), h.url+"/v2/token") {
		t.Errorf("/v2/: %d %v", resp.StatusCode, resp.Header)
	}
	do := func(method, path string, body []byte, ctype string) *http.Response {
		req, _ := http.NewRequest(method, h.url+path, bytes.NewReader(body))
		req.SetBasicAuth("admin", "admin-password-1")
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	digest := func(b []byte) string {
		s := sha256.Sum256(b)
		return "sha256:" + hex.EncodeToString(s[:])
	}
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	layer := []byte("layer")
	for _, b := range [][]byte{config, layer} {
		if r := do("POST", "/v2/team/app/blobs/uploads/?digest="+digest(b), b, ""); r.StatusCode != http.StatusCreated {
			t.Fatalf("blob upload: %d", r.StatusCode)
		}
	}
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},`+
		`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"%s","size":%d}]}`,
		digest(config), len(config), digest(layer), len(layer))
	if r := do("PUT", "/v2/team/app/manifests/1.0", []byte(manifest), "application/vnd.oci.image.manifest.v1+json"); r.StatusCode != http.StatusCreated {
		t.Fatalf("manifest: %d", r.StatusCode)
	}
	admin.ok("/packages", "team/app")
	admin.ok("/packages?q=APP", "team/app", "<td>1</td>", "42 B")
	if body := admin.ok("/packages?q=nope"); strings.Contains(body, "team/app") || !strings.Contains(body, "No images match") {
		t.Errorf("filtered packages:\n%.300s", body)
	}
	admin.ok("/packages/team/app", "1.0")
	admin.ok("/packages/team/app/-/1.0", "linux/amd64", "team/app@"+digest([]byte(manifest)))
	admin.ok("/admin/packages")
	if p := admin.post("/admin/packages", "enabled", "on", "keep_count", "5", "remove_pattern", "pr-.*"); p.status != http.StatusSeeOther {
		t.Errorf("save cleanup rule: %d", p.status)
	}
	if p := admin.post("/admin/packages", "keep_pattern", "("); p.status == http.StatusSeeOther && !strings.Contains(p.header.Get("Set-Cookie"), "pattern") {
		t.Errorf("invalid cleanup pattern accepted: %d", p.status)
	}
	if p := admin.post("/packages/-/delete", "name", "team/app", "version", "1.0"); p.status != http.StatusSeeOther {
		t.Errorf("delete version: %d", p.status)
	}
	if r := do("GET", "/v2/team/app/manifests/1.0", nil, ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("deleted version still served: %d", r.StatusCode)
	}
}

func TestCIAndDeploymentsEndToEnd(t *testing.T) {
	t.Parallel()
	h := start(t, nil)
	admin := h.admin()
	dev := h.createUser(admin, "dev", "write")

	// Runners are created in the admin UI; the token is shown once.
	ciToken := runnerTokenRe.FindString(admin.post("/admin/runners", "name", "builder", "kind", "ci", "labels", "linux").body)
	if p := admin.post("/admin/deploy/dimensions", "name", "environment", "source", "list", "values", "dev, prod", "position", "1"); p.status != http.StatusSeeOther {
		t.Fatalf("dimension: %d", p.status)
	}
	deployToken := runnerTokenRe.FindString(admin.post("/admin/runners", "name", "deployer", "kind", "deploy", "labels", "deploy",
		"targets", "environment=dev").body)
	if ciToken == "" || deployToken == "" {
		t.Fatal("runner tokens not shown")
	}
	for _, form := range [][]string{
		{"kind", "grant", "description", "writers to dev", "selector", "environment=dev", "principals", "role:write"},
		{"kind", "require", "description", "prod is frozen", "selector", "environment=prod", "freeze", "on", "approvals", "0"},
	} {
		if p := admin.post("/admin/deploy/rules", form...); p.status != http.StatusSeeOther {
			t.Fatalf("rule %v: %d", form, p.status)
		}
	}
	if p := admin.post("/admin/secrets", "name", "API_KEY", "selector", "environment=dev", "value", "dev-api-key-123"); p.status != http.StatusSeeOther {
		t.Fatalf("secret: %d", p.status)
	}
	if strings.Contains(admin.ok("/admin/secrets", "API_KEY"), "dev-api-key-123") {
		t.Error("secret value shown in the UI")
	}

	w := testutil.NewWork(t)
	w.Commit("init", map[string]string{
		"README.md": "demo",
		".onegit/pipelines/ci.yml": `
on: {push: {branches: [main]}, manual: }
jobs:
  test:
    runs-on: [linux]
    steps:
      - run: test -f README.md && echo "tests passed on $ONEGIT_REF_NAME"
      - run: echo "secret is ${API_KEY:-absent}"
`,
		".onegit/deploy/app.yml": `
targets: {environment: [dev, prod]}
runs-on: [deploy]
steps:
  - run: echo "deploying to $ENVIRONMENT with $API_KEY"
`,
	})
	mustGit(t, w.Dir, "push", h.remote("dev", testutil.Password("dev")), "main")
	sha := strings.TrimSpace(w.Git("rev-parse", "HEAD"))

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	for _, tok := range []string{ciToken, deployToken} {
		r, err := runner.New(runner.Config{URL: h.url, Token: tok, WorkDir: t.TempDir(), Version: "e2e", Log: testutil.Logger()})
		testutil.Must(t, err)
		go r.Run(runCtx)
	}

	testutil.Eventually(t, 60*time.Second, "pipeline to pass", func() bool {
		st, _ := h.srv.Store.CommitStatuses(ctx, sha)
		return len(st) == 1 && st[0].State == "success"
	})
	dev.ok("/actions", "ci")
	dev.ok("/actions/runs/1", "test")
	dev.ok("/actions/jobs/1", "test")
	raw := dev.ok("/actions/jobs/1/raw", "tests passed on main")
	if !strings.Contains(raw, "secret is absent") {
		t.Errorf("pipeline jobs must not see deploy secrets:\n%s", raw)
	}
	var logResp struct {
		Chunks []store.LogChunk `json:"chunks"`
		Done   bool             `json:"done"`
		Status string           `json:"status"`
	}
	json.Unmarshal([]byte(dev.ok("/actions/jobs/1/log?after=-1")), &logResp)
	if !logResp.Done || logResp.Status != "success" || len(logResp.Chunks) == 0 {
		t.Errorf("log JSON = %+v", logResp)
	}

	// Manual run and rerun from the UI.
	if p := dev.post("/actions/run", "file", ".onegit/pipelines/ci.yml", "branch", "main"); p.status != http.StatusSeeOther {
		t.Errorf("manual run: %d", p.status)
	}
	if p := dev.post("/actions/runs/1/rerun"); p.status != http.StatusSeeOther {
		t.Errorf("rerun: %d", p.status)
	}

	// Deploy to dev: allowed at once, runs on the deploy runner with the secret masked.
	dev.ok("/deploy/new?ref=main&environment=dev&check=1", "Allowed by writers to dev")
	p := dev.post("/deploy", "ref", "main", "environment", "dev", "comment", "first deploy")
	if p.status != http.StatusSeeOther || p.location != "/deploy/1" {
		t.Fatalf("deploy: %d %q\n%.800s", p.status, p.location, p.body)
	}
	testutil.Eventually(t, 60*time.Second, "deployment to finish", func() bool {
		d, _ := h.srv.Store.DeploymentByID(ctx, 1)
		return d.Status == "success"
	})
	d, _ := h.srv.Store.DeploymentByID(ctx, 1)
	jobs, _ := h.srv.Store.JobsForRun(ctx, *d.RunID)
	log := dev.ok(fmt.Sprintf("/actions/jobs/%d/raw", jobs[0].ID), "deploying to dev with ***")
	if strings.Contains(log, "dev-api-key-123") {
		t.Error("secret leaked into the deploy log")
	}
	dev.ok("/deploy/1", "first deploy")
	dev.ok("/deploy", "dev")
	dev.ok("/deploy/target?key=environment%3Ddev", "dev")

	// Prod is frozen; writers have no grant there anyway.
	if p := dev.post("/deploy", "ref", "main", "environment", "prod"); p.status != http.StatusUnprocessableEntity ||
		!strings.Contains(p.body, "frozen") {
		t.Errorf("frozen prod: %d", p.status)
	}

	for _, path := range []string{"/admin/runners", "/admin/teams", "/admin/deploy"} {
		admin.ok(path)
	}
	admin.ok("/admin/audit", "deploy.request", "secret.save", "runner.create")
	stop()
}
