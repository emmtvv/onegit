package server_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"onegit/internal/runner"
	"onegit/internal/store"
	"onegit/internal/testutil"
)

const monorepoPipeline = `
on: {push: {branches: [main]}, pull_request: }
jobs:
  build:
    runs-on: [linux]
    matrix: {variant: [a, b]}
    artifacts: {paths: [out/]}
    cache: {key: deps, key-files: [services/api/main.go], paths: [.deps]}
    steps:
      - run: |
          mkdir -p out && echo "built $MATRIX_VARIANT" > out/$MATRIX_VARIANT.txt
          if [ -f .deps/stamp ]; then echo "cache hit: $(cat .deps/stamp)"; else echo "cache miss"; fi
          mkdir -p .deps && echo "from-run-$ONEGIT_RUN_ID" > .deps/stamp
  test:
    needs: build
    runs-on: [linux]
    steps:
      - run: cat out/a.txt out/b.txt
`

// jobLog returns the raw log of the run's job with the given name.
func jobLog(t *testing.T, h *harness, b *browser, runID int64, name string) string {
	t.Helper()
	jobs, err := h.srv.Store.JobsForRun(ctx, runID)
	testutil.Must(t, err)
	for _, j := range jobs {
		if j.Name == name {
			return b.ok("/actions/jobs/" + itoa(j.ID) + "/raw")
		}
	}
	t.Fatalf("run %d has no job %q", runID, name)
	return ""
}

func waitRun(t *testing.T, h *harness, sha, event string) *store.Run {
	t.Helper()
	var run *store.Run
	testutil.Eventually(t, 90*time.Second, event+" run of "+sha[:7]+" to finish", func() bool {
		runs, _ := h.srv.Store.ListRuns(ctx, store.RunFilter{SHA: sha, Limit: 10})
		for _, r := range runs {
			if r.Event == event && r.FinishedAt != nil {
				run = r
				return true
			}
		}
		return false
	})
	return run
}

func TestMonorepoFeaturesEndToEnd(t *testing.T) {
	t.Parallel()
	h := start(t, nil)
	admin := h.admin()
	dev := h.createUser(admin, "dev", "write")
	ciToken := runnerTokenRe.FindString(admin.post("/admin/runners", "name", "builder", "kind", "ci", "labels", "linux").body)
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	r, err := runner.New(runner.Config{URL: h.url, Token: ciToken, WorkDir: t.TempDir(), Capacity: 2, Version: "e2e", Log: testutil.Logger()})
	testutil.Must(t, err)
	go r.Run(runCtx)

	w := testutil.NewWork(t)
	w.Commit("init monorepo", map[string]string{
		"README.md":                "monorepo",
		"CODEOWNERS":               "/services/api/ @dev\n",
		"services/api/main.go":     "package main\n\nfunc NewServer() {}\n",
		"services/api/README.md":   "# API service\n",
		"services/web/app.js":      "console.log('NewServer is elsewhere')\n",
		".onegit/pipelines/ci.yml": monorepoPipeline,
	})
	remote := h.remote("dev", testutil.Password("dev"))
	mustGit(t, w.Dir, "push", remote, "main")
	first := strings.TrimSpace(w.Git("rev-parse", "HEAD"))

	// Matrix, artifacts between jobs and the cache.
	run := waitRun(t, h, first, "push")
	if run.Status != store.JobSuccess {
		t.Fatalf("first run: %s\n%s", run.Status, jobLog(t, h, dev, run.ID, "test"))
	}
	if log := jobLog(t, h, dev, run.ID, "test"); !strings.Contains(log, "built a") || !strings.Contains(log, "built b") {
		t.Errorf("test job did not get the build artifacts:\n%s", log)
	}
	if log := jobLog(t, h, dev, run.ID, "build (a)"); !strings.Contains(log, "cache miss") || !strings.Contains(log, "Uploading 1 file(s) as artifacts") {
		t.Errorf("build (a):\n%s", log)
	}
	dev.ok("/actions/runs/"+itoa(run.ID), "build (a)", "build (b)", "Artifacts")
	arts, _ := h.srv.Store.ArtifactsForRun(ctx, run.ID)
	if len(arts) != 2 {
		t.Fatalf("artifacts = %d", len(arts))
	}
	if p := dev.get("/actions/artifacts/" + itoa(arts[0].ID)); p.status != http.StatusOK || p.header.Get("Content-Type") != "application/gzip" ||
		!strings.Contains(p.header.Get("Content-Disposition"), "artifacts.tar.gz") {
		t.Errorf("artifact download: %d %v", p.status, p.header)
	}
	if n, _, _ := h.srv.Store.CacheUsage(ctx); n != 1 {
		t.Errorf("caches = %d", n)
	}

	// The next run restores the cache (same key files).
	w.Commit("docs", map[string]string{"README.md": "monorepo, again"})
	mustGit(t, w.Dir, "push", remote, "main")
	second := strings.TrimSpace(w.Git("rev-parse", "HEAD"))
	run2 := waitRun(t, h, second, "push")
	if log := jobLog(t, h, dev, run2.ID, "build (a)"); !strings.Contains(log, "cache hit: from-run-"+itoa(run.ID)) ||
		!strings.Contains(log, "restored as is") {
		t.Errorf("second build (a) did not use the cache:\n%s", log)
	}

	// Blame and search.
	dev.ok("/blame/main/services/api/main.go", "init monorepo", "NewServer", "Blame")
	dev.ok("/blob/main/services/api/main.go", "Owned by", "@dev", "/blame/main/services/api/main.go")
	dev.ok("/search?q=NewServer", "services/api/main.go", "services/web/app.js", "<mark>NewServer</mark>")
	if body := dev.ok("/search?q=NewServer+path:services/api"); strings.Contains(body, "app.js") {
		t.Error("path filter ignored")
	}
	dev.ok("/search?q=new.*r&regex=1&case=1", "No results")
	dev.ok("/search?q=app&type=files", "services/web/app.js")
	dev.ok("/search?q=x&ref=nope", "There is no branch")

	// Projects.
	dev.ok("/projects", "not set up")
	if p := admin.post("/admin/projects", "pattern", "services/*", "dimension", ""); p.status != http.StatusSeeOther {
		t.Fatalf("project settings: %d", p.status)
	}
	if p := admin.post("/admin/projects", "pattern", "services/app-*"); p.status != http.StatusSeeOther {
		t.Fatalf("bad pattern: %d", p.status)
	}
	admin.ok("/admin/projects", "services/*", "api", "web")
	dev.ok("/projects", "api", "web", "@dev", "services/api")
	dev.ok("/projects/api", "API service", "@dev", "Recent CI runs")
	if p := dev.get("/projects/nope"); p.status != http.StatusNotFound {
		t.Errorf("unknown project: %d", p.status)
	}
	dev.ok("/tree/main/services/api", "Owned by", "/projects/api")
	dev.ok("/actions?project=api", "All projects", "api")

	// Merge queue: a PR is tested on top of main and lands on its own.
	if p := admin.post("/admin/branches", "pattern", "main", "required_approvals", "0", "require_merge_queue", "on",
		"merge_queue_depth", "3"); p.status != http.StatusSeeOther {
		t.Fatalf("protection: %d", p.status)
	}
	mustGit(t, w.Dir, "checkout", "-q", "-b", "feature")
	w.Commit("api change", map[string]string{"services/api/handler.go": "package main\n"})
	mustGit(t, w.Dir, "push", remote, "feature")
	if p := dev.post("/pulls", "head", "feature", "base", "main", "title", "API change"); p.status != http.StatusSeeOther {
		t.Fatalf("open PR: %d", p.status)
	}
	testutil.Eventually(t, 30*time.Second, "changed files of the PR", func() bool {
		return strings.Contains(dev.ok("/pulls?project=api"), "API change") && !strings.Contains(dev.ok("/pulls?project=web"), "API change")
	})
	if p := dev.post("/pulls/1/merge", "style", "squash"); p.status != http.StatusSeeOther {
		t.Fatalf("merge: %d", p.status)
	}
	dev.ok("/pulls/1", "takes changes only through the merge queue")
	if p := dev.post("/pulls/1/queue", "style", "squash", "delete_branch", "on"); p.status != http.StatusSeeOther {
		t.Fatalf("queue: %d", p.status)
	}
	dev.ok("/pulls/1", "In the merge queue")
	testutil.Eventually(t, 120*time.Second, "the queued PR to land", func() bool {
		p, _ := h.srv.Store.PullByID(ctx, 1)
		return p.IsMerged()
	})
	p, _ := h.srv.Store.PullByID(ctx, 1)
	runs, _ := h.srv.Store.ListRuns(ctx, store.RunFilter{SHA: *p.MergeSHA, Limit: 10})
	var events []string
	for _, r := range runs {
		events = append(events, r.Event)
	}
	if !strings.Contains(strings.Join(events, ","), "merge_queue") {
		t.Errorf("runs of the merged commit: %v", events)
	}
	dev.ok("/pulls/queue", "Recently", "API change", "merged")
	dev.ok("/pulls/1", "via the merge queue")
	// Landing starts the push pipelines of main.
	waitRun(t, h, *p.MergeSHA, "push")
	if out, err := git(t, w.Dir, "ls-remote", remote, "refs/heads/feature"); err != nil || strings.TrimSpace(out) != "" {
		t.Errorf("feature branch not deleted: %q %v", out, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
