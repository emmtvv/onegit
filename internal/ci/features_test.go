package ci_test

import (
	"encoding/json"
	"testing"
	"time"

	"onegit/internal/ci"
	"onegit/internal/store"
	"onegit/internal/testutil"
)

const matrixPipeline = `
name: matrix
on:
  push:
  schedule:
    - cron: "*/5 * * * *"
jobs:
  test:
    runs-on: ["${matrix.os}"]
    matrix:
      go: ["1.22", "1.23"]
      os: [linux]
    env: {GOV: "go${matrix.go}"}
    steps:
      - name: test on ${matrix.go}
        run: go${matrix.go} test
  report:
    needs: test
    steps: [{run: report}]
  nightly:
    events: [schedule]
    steps: [{run: long}]
`

func TestMatrixRun(t *testing.T) {
	t.Parallel()
	e := setup(t, map[string]string{".onegit/pipelines/build.yml": matrixPipeline})
	sha := e.pushed("main")
	runs := e.runs(sha)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	jobs := e.jobs(runs[0])
	j := jobs["test (1.22, linux)"]
	if j == nil || jobs["test (1.23, linux)"] == nil || len(jobs) != 4 {
		t.Fatalf("jobs = %v", keys(jobs))
	}
	var p ci.JobPayload
	testutil.Must(t, json.Unmarshal(j.Spec, &p))
	if j.BaseName != "test" || j.RunsOn[0] != "linux" || p.Env["GOV"] != "go1.22" || p.Env["MATRIX_GO"] != "1.22" ||
		p.Steps[0].Name != "test on 1.22" || p.Steps[0].Run != "go1.22 test" || p.Context != "matrix / test (1.22, linux)" {
		t.Errorf("matrix job = %+v %+v", j, p)
	}
	if jobs["nightly"].Status != store.JobSkipped {
		t.Errorf("schedule-only job on push: %s", jobs["nightly"].Status)
	}
	if st := e.statuses(sha)["matrix / test (1.23, linux)"]; st == nil || st.State != "pending" {
		t.Errorf("matrix status = %+v", st)
	}
	// report waits for every job of the matrix.
	testutil.Must(t, e.svc.FinishJob(ctx, j.ID, store.JobSuccess, ""))
	if got := e.jobs(runs[0])["report"]; got.Status != store.JobWaiting {
		t.Errorf("report after one matrix job: %s", got.Status)
	}
	testutil.Must(t, e.svc.FinishJob(ctx, jobs["test (1.23, linux)"].ID, store.JobFailure, "boom"))
	if got := e.jobs(runs[0])["report"]; got.Status != store.JobSkipped {
		t.Errorf("report after a failed matrix job: %s", got.Status)
	}
}

func TestSchedules(t *testing.T) {
	t.Parallel()
	e := setup(t, map[string]string{".onegit/pipelines/build.yml": matrixPipeline})
	t0 := time.Date(2026, 10, 4, 10, 1, 0, 0, time.UTC)
	// First sight: remembered, not run.
	testutil.Must(t, e.svc.RunSchedules(ctx, t0))
	scheduled := func() []*store.Run {
		var out []*store.Run
		runs, _ := e.st.ListRuns(ctx, store.RunFilter{Kind: "pipeline", Limit: 100})
		for _, r := range runs {
			if r.Event == "schedule" {
				out = append(out, r)
			}
		}
		return out
	}
	if n := len(scheduled()); n != 0 {
		t.Fatalf("ran on first sight: %d", n)
	}
	testutil.Must(t, e.svc.RunSchedules(ctx, t0.Add(3*time.Minute))) // 10:04: not due (10:05)
	if n := len(scheduled()); n != 0 {
		t.Fatalf("ran early: %d", n)
	}
	testutil.Must(t, e.svc.RunSchedules(ctx, t0.Add(4*time.Minute))) // 10:05
	runs := scheduled()
	if len(runs) != 1 || runs[0].Ref != "refs/heads/main" {
		t.Fatalf("scheduled runs = %+v", runs)
	}
	if jobs := e.jobs(runs[0]); jobs["nightly"].Status != store.JobQueued {
		t.Errorf("schedule-only job: %s", jobs["nightly"].Status)
	}
	// Hours of downtime collapse into one run.
	testutil.Must(t, e.svc.RunSchedules(ctx, t0.Add(4*time.Hour)))
	testutil.Must(t, e.svc.RunSchedules(ctx, t0.Add(4*time.Hour+time.Second)))
	if n := len(scheduled()); n != 2 {
		t.Errorf("after downtime: %d runs, want 2", n)
	}
	list := e.svc.Schedules(ctx)
	if len(list) != 1 || list[0].Cron != "*/5 * * * *" || list[0].Next.IsZero() {
		t.Errorf("Schedules = %+v", list)
	}
}

func TestMergeQueueRuns(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	base, _ := e.repo.ResolveCommit(ctx, "refs/heads/main")
	cand := e.work.Commit("candidate", map[string]string{"app/x.go": "package main\n"})
	e.work.Push(e.repo, "HEAD:refs/merge-queue/1")
	pr := &store.Pull{Title: "t", HeadBranch: "feat", BaseBranch: "main", HeadSHA: cand, MergeBase: base}
	testutil.Must(t, e.st.CreatePull(ctx, pr))
	e.svc.RunMergeQueue(ctx, cand, base, "refs/merge-queue/1", "main", pr.ID, &e.user.ID)
	runs := e.runs(cand)
	if len(runs) != 1 || runs[0].Event != "merge_queue" || *runs[0].PullID != pr.ID || runs[0].ChangedFiles[0] != "app/x.go" {
		t.Fatalf("runs = %+v", runs)
	}
	// Jobs limited to push events are skipped; the rest run.
	if jobs := e.jobs(runs[0]); jobs["image"].Status != store.JobSkipped || jobs["test"].Status != store.JobQueued {
		t.Errorf("jobs: image %s, test %s", jobs["image"].Status, jobs["test"].Status)
	}
	var done []string
	e.svc.OnCheckDone = func(sha string) { done = append(done, sha) }
	e.svc.CancelMergeQueue(ctx, cand)
	if r, _ := e.st.RunByID(ctx, runs[0].ID); r.FinishedAt == nil || r.Status != store.JobCancelled {
		t.Errorf("after cancel: %+v", r)
	}
	if len(done) == 0 || done[0] != cand {
		t.Errorf("OnCheckDone = %v", done)
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
