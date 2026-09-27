package ci_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"onegit/internal/auth"
	"onegit/internal/ci"
	"onegit/internal/git"
	"onegit/internal/hooks"
	"onegit/internal/store"
	"onegit/internal/testutil"
)

var ctx = context.Background()

const pipeline = `
name: build
on:
  push:
    branches: [main, "feature/*"]
    paths: ["app/**", ".onegit/**"]
  pull_request:
  manual:
env: {GLOBAL: g}
jobs:
  test:
    runs-on: linux
    env: {LOCAL: l}
    steps:
      - run: make test
  image:
    needs: test
    events: [push]
    runs-on: [linux, docker]
    timeout: 5m
    steps:
      - run: make image
`

type env struct {
	t    *testing.T
	st   *store.Store
	repo *git.Repo
	work *testutil.Work
	svc  *ci.Service
	user *store.User
}

func setup(t *testing.T, files map[string]string) *env {
	t.Helper()
	cfg := testutil.Config(t)
	cfg.HTTP.BaseURL = "https://git.example.com"
	e := &env{t: t, st: testutil.Store(t, cfg), repo: testutil.Bare(t), work: testutil.NewWork(t)}
	e.svc = &ci.Service{Store: e.st, Repo: e.repo, Cfg: cfg, Log: testutil.Logger()}
	e.user = testutil.User(t, e.st, "dev", store.RoleWrite)
	if files == nil {
		files = map[string]string{}
	}
	if _, ok := files[".onegit/pipelines/build.yml"]; !ok {
		files[".onegit/pipelines/build.yml"] = pipeline
	}
	files["app/main.go"] = "package main\n"
	e.work.Commit("init", files)
	e.work.Push(e.repo, "main")
	return e
}

// pushed pushes branch and runs the push trigger, returning the new tip.
func (e *env) pushed(branch string) string {
	e.t.Helper()
	old, err := e.repo.ResolveCommit(ctx, "refs/heads/"+branch)
	if err != nil {
		old = git.ZeroSHA
	}
	e.work.Push(e.repo, branch)
	sha, _ := e.repo.ResolveCommit(ctx, "refs/heads/"+branch)
	e.svc.OnPush(ctx, e.user, []hooks.RefUpdate{{OldSHA: old, NewSHA: sha, Ref: "refs/heads/" + branch}})
	return sha
}

func (e *env) runs(sha string) []*store.Run {
	e.t.Helper()
	runs, err := e.st.ListRuns(ctx, store.RunFilter{SHA: sha, Limit: 100})
	testutil.Must(e.t, err)
	return runs
}

func (e *env) jobs(run *store.Run) map[string]*store.Job {
	e.t.Helper()
	list, err := e.st.JobsForRun(ctx, run.ID)
	testutil.Must(e.t, err)
	out := map[string]*store.Job{}
	for _, j := range list {
		out[j.Name] = j
	}
	return out
}

func (e *env) statuses(sha string) map[string]*store.CommitStatus {
	list, _ := e.st.CommitStatuses(ctx, sha)
	out := map[string]*store.CommitStatus{}
	for _, s := range list {
		out[s.Context] = s
	}
	return out
}

func TestLoadPipelinesAndRecipes(t *testing.T) {
	t.Parallel()
	e := setup(t, map[string]string{
		".onegit/pipelines/broken.yml": "jobs: {}",
		".onegit/pipelines/notes.txt":  "ignored",
		".onegit/deploy/b.yaml":        "targets: {env: [prod]}\nsteps: [{run: b}]\n",
		".onegit/deploy/a.yml":         "targets: {env: [dev]}\nsteps: [{run: a}]\n",
	})
	sha, _ := e.repo.ResolveCommit(ctx, "main")
	pipes, errs := e.svc.LoadPipelines(ctx, sha)
	if len(pipes) != 1 || pipes[".onegit/pipelines/build.yml"] == nil || len(errs) != 1 || !strings.Contains(errs[0].Error(), "broken.yml") {
		t.Errorf("pipelines = %v, errors = %v", pipes, errs)
	}
	recipes, errs := e.svc.LoadRecipes(ctx, sha)
	if len(recipes) != 2 || recipes[0].File != ".onegit/deploy/a.yml" || len(errs) != 0 {
		t.Errorf("recipes = %v, errors = %v", recipes, errs)
	}
	// No .onegit directory at all.
	w := testutil.NewWork(t)
	empty := w.Commit("empty", map[string]string{"x": "y"})
	w.Push(e.repo, "HEAD:refs/heads/empty")
	if pipes, errs := e.svc.LoadPipelines(ctx, empty); len(pipes) != 0 || len(errs) != 0 {
		t.Errorf("no pipelines dir: %v, %v", pipes, errs)
	}
}

func TestOnPush(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	// A path outside the filter starts nothing.
	e.work.Commit("docs only", map[string]string{"README.md": "docs"})
	docs := e.pushed("main")
	if runs := e.runs(docs); len(runs) != 0 {
		t.Fatalf("docs-only push started %d runs", len(runs))
	}
	sha := e.work.Commit("app change", map[string]string{"app/main.go": "package main // v2\n"})
	e.pushed("main")
	runs := e.runs(sha)
	if len(runs) != 1 {
		t.Fatalf("%d runs", len(runs))
	}
	run := runs[0]
	if run.Event != "push" || run.Ref != "refs/heads/main" || run.BeforeSHA != docs || run.Name != "build" ||
		!slices.Equal(run.ChangedFiles, []string{"app/main.go"}) || *run.TriggeredBy != e.user.ID {
		t.Errorf("run = %+v", run)
	}
	jobs := e.jobs(run)
	if jobs["test"].Status != store.JobQueued || jobs["image"].Status != store.JobWaiting ||
		!slices.Equal(jobs["image"].RunsOn, []string{"linux", "docker"}) {
		t.Errorf("jobs = test %s, image %s", jobs["test"].Status, jobs["image"].Status)
	}
	var payload ci.JobPayload
	json.Unmarshal(jobs["test"].Spec, &payload)
	if payload.Env["GLOBAL"] != "g" || payload.Env["LOCAL"] != "l" || payload.TimeoutSec != 3600 || payload.Context != "build / test" {
		t.Errorf("payload = %+v", payload)
	}
	json.Unmarshal(jobs["image"].Spec, &payload)
	if payload.TimeoutSec != 300 {
		t.Errorf("image timeout = %d", payload.TimeoutSec)
	}
	st := e.statuses(sha)
	if st["build / test"] == nil || st["build / test"].State != "pending" ||
		st["build / test"].TargetURL != "https://git.example.com"+ci.JobURL(jobs["test"].ID) {
		t.Errorf("statuses = %+v", st)
	}

	// Results propagate to statuses and dependent jobs.
	testutil.Must(t, e.svc.FinishJob(ctx, jobs["test"].ID, store.JobFailure, "tests failed"))
	st = e.statuses(sha)
	if st["build / test"].State != "failure" || st["build / test"].Description != "tests failed" || st["build / image"].State != "skipped" {
		t.Errorf("statuses after failure = test %+v, image %+v", st["build / test"], st["build / image"])
	}
	r, _ := e.st.RunByID(ctx, run.ID)
	if r.Status != store.JobFailure {
		t.Errorf("run status = %s", r.Status)
	}

	// Rerun reads the pipeline from the same commit.
	re, err := e.svc.Rerun(ctx, r, &e.user.ID)
	if err != nil || re.ID == run.ID || re.SHA != sha || re.Event != "push" {
		t.Errorf("rerun = %+v, %v", re, err)
	}
	if e.statuses(sha)["build / test"].State != "pending" {
		t.Error("rerun did not reset the status")
	}
	gone := *r
	gone.File = ".onegit/pipelines/gone.yml"
	if _, err := e.svc.Rerun(ctx, &gone, nil); err == nil {
		t.Error("rerun of a missing pipeline")
	}

	// Branches not in the filter start nothing; deletions are ignored.
	e.work.Git("checkout", "-q", "-b", "other")
	o := e.work.Commit("other", map[string]string{"app/x.go": "x"})
	e.pushed("other")
	if runs := e.runs(o); len(runs) != 0 {
		t.Errorf("push to an unfiltered branch started %d runs", len(runs))
	}
	e.svc.OnPush(ctx, nil, []hooks.RefUpdate{{OldSHA: o, NewSHA: git.ZeroSHA, Ref: "refs/heads/other"}})
}

func TestOnPushNewBranchUsesForkPoint(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	e.work.Git("checkout", "-q", "-b", "feature/a")
	e.work.Commit("docs", map[string]string{"docs.md": "d"})
	sha := e.pushed("feature/a")
	// Only docs changed since the fork point, so the path filter excludes it.
	if runs := e.runs(sha); len(runs) != 0 {
		t.Errorf("new branch without app changes started %d runs", len(runs))
	}
	e.work.Git("checkout", "-q", "-b", "feature/b", "main")
	e.work.Commit("app", map[string]string{"app/b.go": "b"})
	sha = e.pushed("feature/b")
	if runs := e.runs(sha); len(runs) != 1 || !slices.Equal(runs[0].ChangedFiles, []string{"app/b.go"}) {
		t.Errorf("runs = %+v", runs)
	}
}

func TestPullRequestAndManualRuns(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	base, _ := e.repo.ResolveCommit(ctx, "main")
	e.work.Git("checkout", "-q", "-b", "fix")
	head := e.work.Commit("fix", map[string]string{"app/fix.go": "f"})
	e.work.Push(e.repo, "fix")
	p := &store.Pull{Title: "Fix", AuthorID: &e.user.ID, HeadBranch: "fix", BaseBranch: "main", HeadSHA: head, MergeBase: base}
	testutil.Must(t, e.st.CreatePull(ctx, p))
	e.svc.OnPull(ctx, p, &e.user.ID)
	runs := e.runs(head)
	if len(runs) != 1 || runs[0].Event != "pull_request" || *runs[0].PullID != p.ID || runs[0].Ref != "refs/pull/1/head" {
		t.Fatalf("PR runs = %+v", runs)
	}
	// The image job only runs for pushes.
	if j := e.jobs(runs[0])["image"]; j.Status != store.JobSkipped || !strings.Contains(j.Message, "pull_request") {
		t.Errorf("image job = %s %q", j.Status, j.Message)
	}
	// Pushing to the PR's head runs pull_request pipelines again (plus push ones).
	next := e.work.Commit("more", map[string]string{"app/fix.go": "f2"})
	e.work.Push(e.repo, "fix")
	p.HeadSHA = next
	testutil.Must(t, e.svc.Store.AddPullEvent(ctx, p.ID, nil, "pushed", nil))
	_, err := e.st.UpdatePullLocked(ctx, p.ID, func(pp *store.Pull) error { pp.HeadSHA = next; return nil })
	testutil.Must(t, err)
	e.svc.OnPush(ctx, e.user, []hooks.RefUpdate{{OldSHA: head, NewSHA: next, Ref: "refs/heads/fix"}})
	if runs := e.runs(next); len(runs) != 1 || runs[0].Event != "pull_request" {
		t.Errorf("runs after PR push = %+v", runs)
	}
	// Closed PRs don't run.
	p.State = store.PullClosed
	e.svc.OnPull(ctx, p, nil)

	run, err := e.svc.RunManual(ctx, e.user, ".onegit/pipelines/build.yml", "main")
	if err != nil || run.Event != "manual" || run.Ref != "refs/heads/main" || run.SHA != base {
		t.Errorf("manual run = %+v, %v", run, err)
	}
	for name, c := range map[string][2]string{
		"missing branch": {".onegit/pipelines/build.yml", "nope"},
		"missing file":   {".onegit/pipelines/nope.yml", "main"},
	} {
		if _, err := e.svc.RunManual(ctx, e.user, c[0], c[1]); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestManualTriggerRequired(t *testing.T) {
	t.Parallel()
	e := setup(t, map[string]string{".onegit/pipelines/build.yml": "on: push\njobs:\n  a: {steps: [{run: x}]}\n"})
	if _, err := e.svc.RunManual(ctx, e.user, ".onegit/pipelines/build.yml", "main"); err == nil ||
		!strings.Contains(err.Error(), "manual") {
		t.Errorf("RunManual without a manual trigger: %v", err)
	}
}

func TestInvalidPipelineSetsErrorStatus(t *testing.T) {
	t.Parallel()
	e := setup(t, map[string]string{".onegit/pipelines/build.yml": "on: push\njobs:\n  a: {needs: b, steps: [{run: x}]}\n"})
	e.work.Commit("change", map[string]string{"app/y.go": "y"})
	sha := e.pushed("main")
	st := e.statuses(sha)["onegit / pipelines"]
	if st == nil || st.State != "error" || !strings.Contains(st.Description, "unknown job") {
		t.Errorf("status = %+v", st)
	}
}

func TestCancel(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	run, err := e.svc.RunManual(ctx, e.user, ".onegit/pipelines/build.yml", "main")
	testutil.Must(t, err)
	var finished *store.Run
	e.svc.OnRunFinished = func(_ context.Context, r *store.Run) { finished = r }
	testutil.Must(t, e.svc.Cancel(ctx, run.ID))
	if finished == nil || finished.Status != store.JobCancelled {
		t.Errorf("OnRunFinished got %+v", finished)
	}
	if st := e.statuses(run.SHA)["build / test"]; st.State != "error" || st.Description != "Cancelled" {
		t.Errorf("status = %+v", st)
	}
}

// ---- runner protocol ----

type api struct {
	t   *testing.T
	srv *httptest.Server
}

func (a *api) post(path, token string, body any) (*http.Response, []byte) {
	a.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", a.srv.URL+path, bytes.NewReader(b))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

func newRunner(t *testing.T, st *store.Store, name, kind string, labels []string, targets map[string][]string) string {
	t.Helper()
	tok := ci.RunnerTokenPrefix + auth.RandomString(20)
	testutil.Must(t, st.CreateRunner(ctx, &store.Runner{Name: name, Kind: kind, Labels: labels, Targets: targets}, auth.HashToken(tok)))
	return tok
}

func TestRunnerAPI(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	a := &api{t: t, srv: httptest.NewServer(e.svc.RunnerAPI())}
	defer a.srv.Close()
	run, err := e.svc.RunManual(ctx, e.user, ".onegit/pipelines/build.yml", "main")
	testutil.Must(t, err)
	tok := newRunner(t, e.st, "linux-1", "ci", []string{"linux"}, nil)

	if resp, _ := a.post("/api/runner/v1/fetch", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("fetch without a token: %d", resp.StatusCode)
	}
	if resp, _ := a.post("/api/runner/v1/fetch", ci.RunnerTokenPrefix+"forged", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("fetch with a forged token: %d", resp.StatusCode)
	}
	resp, body := a.post("/api/runner/v1/fetch", tok, ci.FetchRequest{Version: "test-1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetch: %d %s", resp.StatusCode, body)
	}
	var as ci.Assignment
	json.Unmarshal(body, &as)
	if as.Name != "test" || as.SHA != run.SHA || as.RepoURL != "https://git.example.com/monorepo.git" ||
		!strings.HasPrefix(as.Token, auth.JobTokenPrefix) || len(as.Steps) != 1 || as.Steps[0].Run != "make test" {
		t.Errorf("assignment = %+v", as)
	}
	for k, v := range map[string]string{"CI": "true", "ONEGIT_EVENT": "manual", "ONEGIT_REF_NAME": "main", "ONEGIT_SHA": run.SHA,
		"ONEGIT_JOB": "test", "ONEGIT_PIPELINE": "build", "ONEGIT_ACTOR": "dev", "ONEGIT_REGISTRY": "git.example.com",
		"GLOBAL": "g", "LOCAL": "l"} {
		if as.Env[k] != v {
			t.Errorf("env %s = %q, want %q", k, as.Env[k], v)
		}
	}
	runners, _ := e.st.ListRunners(ctx)
	if runners[0].Version != "test-1" || runners[0].LastSeenAt == nil {
		t.Errorf("runner not touched: %+v", runners[0])
	}
	if st := e.statuses(run.SHA)["build / test"]; !strings.Contains(st.Description, "linux-1") {
		t.Errorf("running status = %+v", st)
	}

	jobPath := func(suffix string) string { return "/api/runner/v1/jobs/" + itoa(as.ID) + "/" + suffix }
	if resp, _ := a.post(jobPath("log"), tok, ci.LogRequest{}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("runner token on a job endpoint: %d", resp.StatusCode)
	}
	resp, body = a.post(jobPath("log"), as.Token, ci.LogRequest{Seq: 0, Data: "hello\n"})
	var lr ci.LogResponse
	json.Unmarshal(body, &lr)
	if resp.StatusCode != http.StatusOK || lr.Cancel {
		t.Errorf("log: %d %s", resp.StatusCode, body)
	}
	a.post(jobPath("log"), as.Token, ci.LogRequest{Seq: -1}) // heartbeat only
	if chunks, _ := e.st.LogChunks(ctx, as.ID, -1); len(chunks) != 1 || chunks[0].Data != "hello\n" {
		t.Errorf("chunks = %+v", chunks)
	}
	if resp, _ := a.post(jobPath("steps"), as.Token, []store.StepState{{Name: "Run make test", Status: "running"}}); resp.StatusCode != http.StatusNoContent {
		t.Errorf("steps: %d", resp.StatusCode)
	}
	if resp, _ := a.post("/api/runner/v1/jobs/"+itoa(as.ID+1)+"/log", as.Token, ci.LogRequest{}); resp.StatusCode != http.StatusGone {
		t.Errorf("token of another job: %d", resp.StatusCode)
	}
	if resp, _ := a.post(jobPath("finish"), as.Token, ci.FinishRequest{Status: "weird"}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad finish status: %d", resp.StatusCode)
	}
	if resp, _ := a.post(jobPath("finish"), as.Token, ci.FinishRequest{Status: "success"}); resp.StatusCode != http.StatusNoContent {
		t.Errorf("finish: %d", resp.StatusCode)
	}
	// The token dies with the job, and the runner is told to stop.
	resp, body = a.post(jobPath("log"), as.Token, ci.LogRequest{Seq: 1, Data: "late"})
	json.Unmarshal(body, &lr)
	if resp.StatusCode != http.StatusGone || !lr.Cancel {
		t.Errorf("log after finish: %d %s", resp.StatusCode, body)
	}
	// The image job only runs on push, so the manual run is complete.
	if r, _ := e.st.RunByID(ctx, run.ID); r.Status != store.JobSuccess || r.FinishedAt == nil {
		t.Errorf("run = %s", r.Status)
	}

	// A disabled runner is refused.
	r, _ := e.st.ListRunners(ctx)
	r[0].Enabled = false
	testutil.Must(t, e.st.UpdateRunner(ctx, r[0]))
	if resp, _ := a.post("/api/runner/v1/fetch", tok, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("disabled runner: %d", resp.StatusCode)
	}
}

func TestFetchLongPollEndsWithoutJobs(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	srv := httptest.NewServer(e.svc.RunnerAPI())
	defer srv.Close()
	tok := newRunner(t, e.st, "idle", "ci", []string{}, nil)
	c, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(c, "POST", srv.URL+"/api/runner/v1/fetch", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("fetch returned %d without jobs; it should long-poll", resp.StatusCode)
	}
	if time.Since(start) < time.Second {
		t.Error("fetch returned too early")
	}
}

func TestDeployAssignment(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	srv := httptest.NewServer(e.svc.RunnerAPI())
	defer srv.Close()
	a := &api{t: t, srv: srv}
	e.svc.SecretsFor = func(_ context.Context, target map[string]string) (map[string]string, error) {
		return map[string]string{"API_KEY": "secret-for-" + target["environment"], "SHORT": "abc"}, nil
	}
	sha, _ := e.repo.ResolveCommit(ctx, "main")
	d := &store.Deployment{SHA: sha, Ref: "main", RecipeSHA: sha, Targets: []map[string]string{}, RequestedBy: &e.user.ID, Status: "running"}
	testutil.Must(t, e.st.CreateDeployment(ctx, d))
	recipe, err := ci.ParseRecipe(".onegit/deploy/app.yml", []byte("runs-on: [deploy]\ntooling: true\nenv: {API_KEY: overridden}\nsteps: [{run: ./deploy}]\n"))
	testutil.Must(t, err)
	_, err = e.svc.StartDeployment(ctx, d, []ci.DeployJob{
		{Target: map[string]string{"environment": "prod"}, Key: "environment=prod", Label: "prod", Recipe: recipe, Artifact: "reg/app@sha256:x"},
		{Target: map[string]string{"environment": "dev"}, Key: "environment=dev", Label: "dev", Recipe: recipe},
	})
	testutil.Must(t, err)

	// A CI runner never gets deploy jobs; a deploy runner only its targets.
	ciTok := newRunner(t, e.st, "ci", "ci", []string{"deploy"}, nil)
	c, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(c, "POST", srv.URL+"/api/runner/v1/fetch", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+ciTok)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Errorf("CI runner got a deploy job: %d", resp.StatusCode)
	}
	devTok := newRunner(t, e.st, "dev-deployer", "deploy", []string{"deploy"}, map[string][]string{"environment": {"dev"}})
	resp, body := a.post("/api/runner/v1/fetch", devTok, nil)
	var as ci.Assignment
	json.Unmarshal(body, &as)
	if resp.StatusCode != http.StatusOK || as.Name != "dev" {
		t.Fatalf("dev runner got %d %+v", resp.StatusCode, as)
	}
	if as.Env["ENVIRONMENT"] != "dev" || as.Env["ONEGIT_TARGET_ENVIRONMENT"] != "dev" || as.Env["API_KEY"] != "secret-for-dev" ||
		as.Env["ONEGIT_RECIPE"] != ".onegit/deploy/app.yml" || as.Env["ONEGIT_DEPLOYMENT_ID"] != itoa(d.ID) || as.ToolingSHA != sha ||
		as.Env["ONEGIT_ARTIFACT"] != "" {
		t.Errorf("deploy env = %+v, tooling %q", as.Env, as.ToolingSHA)
	}
	// Short secrets are not masked (they'd mangle logs); long ones are.
	if !slices.Contains(as.Masks, "secret-for-dev") || slices.Contains(as.Masks, "abc") {
		t.Errorf("masks = %v", as.Masks)
	}
	prodTok := newRunner(t, e.st, "prod-deployer", "deploy", []string{"deploy"}, map[string][]string{"environment": {"prod*"}})
	_, body = a.post("/api/runner/v1/fetch", prodTok, nil)
	json.Unmarshal(body, &as)
	if as.Name != "prod" || as.Env["ONEGIT_ARTIFACT"] != "reg/app@sha256:x" {
		t.Errorf("prod assignment = %+v", as)
	}
	// Deploy jobs don't set commit statuses.
	if st := e.statuses(sha); len(st) != 0 {
		t.Errorf("deploy set statuses: %v", st)
	}
}

func TestSecretsFailureFailsJob(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	srv := httptest.NewServer(e.svc.RunnerAPI())
	defer srv.Close()
	e.svc.SecretsFor = func(context.Context, map[string]string) (map[string]string, error) {
		return nil, context.DeadlineExceeded
	}
	sha, _ := e.repo.ResolveCommit(ctx, "main")
	d := &store.Deployment{SHA: sha, Ref: "main", RecipeSHA: sha, Targets: []map[string]string{}, Status: "running"}
	testutil.Must(t, e.st.CreateDeployment(ctx, d))
	recipe, _ := ci.ParseRecipe("r.yml", []byte("steps: [{run: x}]"))
	run, err := e.svc.StartDeployment(ctx, d, []ci.DeployJob{{Target: map[string]string{"e": "x"}, Key: "e=x", Label: "x", Recipe: recipe}})
	testutil.Must(t, err)
	tok := newRunner(t, e.st, "d", "deploy", []string{}, nil)
	c, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(c, "POST", srv.URL+"/api/runner/v1/fetch", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	jobs, _ := e.st.JobsForRun(ctx, run.ID)
	if jobs[0].Status != store.JobFailure || !strings.Contains(jobs[0].Message, "could not prepare") {
		t.Errorf("job = %s %q", jobs[0].Status, jobs[0].Message)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
