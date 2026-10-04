package store_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"onegit/internal/store"
	"onegit/internal/testutil"
)

func anyJob(*store.Job) bool { return true }

func newRun(t *testing.T, st *store.Store, kind string, jobs ...*store.Job) *store.Run {
	t.Helper()
	r := &store.Run{Kind: kind, Name: "p", File: "p.yml", Event: "push", Ref: "refs/heads/main",
		SHA: strings.Repeat("a", 40), Status: store.JobQueued}
	for _, j := range jobs {
		if j.Spec == nil {
			j.Spec = []byte("{}")
		}
		if j.Kind == "" {
			j.Kind = "ci"
		}
		if j.Status == "" {
			j.Status = store.JobQueued
		}
	}
	testutil.Must(t, st.CreateRun(ctx, r, jobs))
	return r
}

func TestRunners(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	r := &store.Runner{Name: "builder", Kind: "ci", Labels: []string{"linux", "docker"}}
	testutil.Must(t, st.CreateRunner(ctx, r, "h1"))
	if !r.Enabled || r.Targets == nil {
		t.Errorf("new runner = %+v", r)
	}
	if err := st.CreateRunner(ctx, &store.Runner{Name: "builder", Kind: "ci", Labels: []string{}}, "h2"); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("duplicate runner name: %v", err)
	}
	got, err := st.RunnerByTokenHash(ctx, "h1")
	if err != nil || got.ID != r.ID {
		t.Fatalf("RunnerByTokenHash = %v, %v", got, err)
	}
	testutil.Must(t, st.SetRunnerToken(ctx, r.ID, "h3"))
	if _, err := st.RunnerByTokenHash(ctx, "h1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("old token still works: %v", err)
	}
	r.Enabled, r.Labels, r.Targets = false, []string{"linux"}, map[string][]string{"env": {"dev"}}
	testutil.Must(t, st.UpdateRunner(ctx, r))
	testutil.Must(t, st.TouchRunner(ctx, r.ID, "v1.2.3"))
	got, _ = st.RunnerByID(ctx, r.ID)
	if got.Enabled || got.Version != "v1.2.3" || got.LastSeenAt == nil || got.Targets["env"][0] != "dev" || len(got.Labels) != 1 {
		t.Errorf("updated runner = %+v", got)
	}
	if list, _ := st.ListRunners(ctx); len(list) != 1 {
		t.Errorf("ListRunners = %d", len(list))
	}
	testutil.Must(t, st.DeleteRunner(ctx, r.ID))
	if _, err := st.RunnerByID(ctx, r.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted runner: %v", err)
	}
	if !store.HasLabels([]string{"a", "b"}, []string{"b"}) || store.HasLabels([]string{"a"}, []string{"a", "c"}) || !store.HasLabels(nil, nil) {
		t.Error("HasLabels")
	}
}

func TestClaimJob(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	linux := &store.Runner{Name: "linux", Kind: "ci", Labels: []string{"linux"}}
	testutil.Must(t, st.CreateRunner(ctx, linux, "h-linux"))
	both := &store.Runner{Name: "both", Kind: "ci", Labels: []string{"linux", "docker"}}
	testutil.Must(t, st.CreateRunner(ctx, both, "h-both"))
	dep := &store.Runner{Name: "dep", Kind: "deploy", Labels: []string{"linux", "docker"}}
	testutil.Must(t, st.CreateRunner(ctx, dep, "h-dep"))

	dockerJob := &store.Job{Name: "docker", RunsOn: []string{"linux", "docker"}}
	anyJ := &store.Job{Name: "any"}
	run := newRun(t, st, "pipeline", dockerJob, anyJ)

	// The linux-only runner cannot take the docker job; it gets the next one.
	j, err := st.ClaimJob(ctx, linux, "t1", anyJob)
	if err != nil || j == nil || j.ID != anyJ.ID || j.Status != store.JobRunning || j.RunnerName != "linux" || j.StartedAt == nil {
		t.Fatalf("linux runner claimed %+v, %v", j, err)
	}
	// Deploy runners never take CI jobs.
	if j, _ := st.ClaimJob(ctx, dep, "t2", anyJob); j != nil {
		t.Errorf("deploy runner claimed CI job %s", j.Name)
	}
	// accept can refuse a candidate.
	if j, _ := st.ClaimJob(ctx, both, "t2", func(*store.Job) bool { return false }); j != nil {
		t.Error("claimed a job accept refused")
	}
	j, _ = st.ClaimJob(ctx, both, "t2", anyJob)
	if j == nil || j.ID != dockerJob.ID {
		t.Fatalf("docker runner claimed %+v", j)
	}
	if j, _ := st.ClaimJob(ctx, both, "t3", anyJob); j != nil {
		t.Error("claimed a job twice")
	}
	r, _ := st.RunByID(ctx, run.ID)
	if r.Status != store.JobRunning || r.StartedAt == nil {
		t.Errorf("run = %+v", r)
	}
	if got, err := st.RunningJobByTokenHash(ctx, "t2"); err != nil || got.ID != dockerJob.ID {
		t.Errorf("RunningJobByTokenHash = %v, %v", got, err)
	}
}

func TestClaimJobConcurrently(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	var jobs []*store.Job
	for i := 0; i < 10; i++ {
		jobs = append(jobs, &store.Job{Name: "j" + string(rune('a'+i))})
	}
	newRun(t, st, "pipeline", jobs...)
	var runners []*store.Runner
	for i := 0; i < 5; i++ {
		r := &store.Runner{Name: "r" + string(rune('a'+i)), Kind: "ci", Labels: []string{}}
		testutil.Must(t, st.CreateRunner(ctx, r, "h"+r.Name))
		runners = append(runners, r)
	}
	var mu sync.Mutex
	seen := map[int64]bool{}
	var wg sync.WaitGroup
	for _, r := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := st.ClaimJob(ctx, r, "tok-"+r.Name+time.Now().String(), anyJob)
				if err != nil {
					t.Error(err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				if seen[j.ID] {
					t.Errorf("job %d claimed twice", j.ID)
				}
				seen[j.ID] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != 10 {
		t.Errorf("%d jobs claimed, want 10", len(seen))
	}
}

func TestDeployTargetsAreExclusive(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	key := "environment=prod"
	target := map[string]string{"environment": "prod"}
	r1 := &store.Runner{Name: "d1", Kind: "deploy", Labels: []string{}}
	r2 := &store.Runner{Name: "d2", Kind: "deploy", Labels: []string{}}
	testutil.Must(t, st.CreateRunner(ctx, r1, "hd1"))
	testutil.Must(t, st.CreateRunner(ctx, r2, "hd2"))
	a := &store.Job{Name: "prod", Kind: "deploy", Target: target, TargetKey: &key}
	newRun(t, st, "deploy", a)
	b := &store.Job{Name: "prod", Kind: "deploy", Target: target, TargetKey: &key}
	newRun(t, st, "deploy", b)
	otherKey := "environment=dev"
	c := &store.Job{Name: "dev", Kind: "deploy", Target: map[string]string{"environment": "dev"}, TargetKey: &otherKey}
	newRun(t, st, "deploy", c)

	j1, _ := st.ClaimJob(ctx, r1, "x1", anyJob)
	if j1 == nil || j1.ID != a.ID {
		t.Fatalf("first claim = %+v", j1)
	}
	// The second prod deploy waits; the dev one may run in parallel.
	j2, _ := st.ClaimJob(ctx, r2, "x2", anyJob)
	if j2 == nil || j2.ID != c.ID {
		t.Fatalf("second claim = %+v, want the dev job", j2)
	}
	if j, _ := st.ClaimJob(ctx, r2, "x3", anyJob); j != nil {
		t.Fatalf("claimed %s while prod is busy", j.Name)
	}
	if _, _, err := st.FinishJob(ctx, a.ID, store.JobSuccess, ""); err != nil {
		t.Fatal(err)
	}
	if j, _ := st.ClaimJob(ctx, r2, "x4", anyJob); j == nil || j.ID != b.ID {
		t.Fatalf("after prod finished: %+v", j)
	}
	testutil.Must(t, st.SetJobSteps(ctx, b.ID, []store.StepState{{Name: "deploy", Status: "running"}}))
	if _, _, err := st.FinishJob(ctx, b.ID, store.JobFailure, "boom"); err != nil {
		t.Fatal(err)
	}
	latest, _ := st.LatestDeploys(ctx)
	if len(latest) != 1 || latest[0].JobID != a.ID || latest[0].TargetKey != key {
		t.Errorf("LatestDeploys = %+v (only successful deploys count)", latest)
	}
	if _, _, err := st.FinishJob(ctx, c.ID, store.JobSuccess, ""); err != nil {
		t.Fatal(err)
	}
	latest, _ = st.LatestDeploys(ctx)
	got := map[string]int64{}
	for _, d := range latest {
		got[d.TargetKey] = d.JobID
	}
	if len(latest) != 2 || got[key] != a.ID || got[otherKey] != c.ID {
		t.Errorf("LatestDeploys with two targets = %v", got)
	}
	hist, _ := st.TargetHistory(ctx, key, 10)
	if len(hist) != 2 || hist[0].JobID != b.ID || hist[0].Status != store.JobFailure {
		t.Errorf("TargetHistory = %+v", hist)
	}
}

func TestRunProgression(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	runner := &store.Runner{Name: "r", Kind: "ci", Labels: []string{}}
	testutil.Must(t, st.CreateRunner(ctx, runner, "hr"))

	// test → build → deploy, and lint independent.
	test := &store.Job{Name: "test"}
	lint := &store.Job{Name: "lint"}
	build := &store.Job{Name: "build", Needs: []string{"test"}, Status: store.JobWaiting}
	pub := &store.Job{Name: "publish", Needs: []string{"build", "lint"}, Status: store.JobWaiting}
	run := newRun(t, st, "pipeline", test, lint, build, pub)

	claim := func(want string) *store.Job {
		t.Helper()
		j, err := st.ClaimJob(ctx, runner, "tok-"+want, anyJob)
		if err != nil || j == nil || j.Name != want {
			t.Fatalf("claim: got %+v, %v; want %s", j, err, want)
		}
		return j
	}
	claim("test")
	claim("lint")
	if j, _ := st.ClaimJob(ctx, runner, "t", anyJob); j != nil {
		t.Fatalf("waiting job %s was claimable", j.Name)
	}
	r, changed, err := st.FinishJob(ctx, test.ID, store.JobSuccess, "")
	if err != nil || len(changed) != 2 || changed[0].ID != test.ID || changed[1].Name != "build" || changed[1].Status != store.JobQueued {
		t.Fatalf("finish test: changed %+v, %v", changed, err)
	}
	if r.FinishedAt != nil {
		t.Error("run finished early")
	}
	// A retried finish is a no-op.
	if _, changed, _ := st.FinishJob(ctx, test.ID, store.JobFailure, ""); len(changed) != 0 {
		t.Errorf("second finish changed %+v", changed)
	}
	claim("build")
	st.FinishJob(ctx, build.ID, store.JobSuccess, "")
	// publish still waits for lint.
	if j, _ := st.JobByID(ctx, pub.ID); j.Status != store.JobWaiting {
		t.Errorf("publish = %s", j.Status)
	}
	r, changed, _ = st.FinishJob(ctx, lint.ID, store.JobFailure, "lint errors")
	if len(changed) != 2 || changed[1].Name != "publish" || changed[1].Status != store.JobSkipped {
		t.Errorf("lint failure must skip publish: %+v", changed)
	}
	if r.Status != store.JobFailure || r.FinishedAt == nil {
		t.Errorf("run = %+v", r)
	}
	jobs, _ := st.JobsForRun(ctx, run.ID)
	if jobs[1].Message != "lint errors" || jobs[3].Message == "" {
		t.Errorf("messages = %q, %q", jobs[1].Message, jobs[3].Message)
	}
	if _, _, err := st.FinishJob(ctx, 999999, store.JobSuccess, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("finish of a missing job: %v", err)
	}
}

func TestAdvanceRunWithSkippedJobs(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	// A job skipped up front (events filter) skips its dependents; the run
	// then succeeds, as skipped counts as done.
	a := &store.Job{Name: "a", Status: store.JobSkipped}
	b := &store.Job{Name: "b", Needs: []string{"a"}, Status: store.JobWaiting}
	run := newRun(t, st, "pipeline", a, b)
	r, changed, err := st.AdvanceRun(ctx, run.ID)
	if err != nil || len(changed) != 1 || changed[0].Status != store.JobSkipped || r.Status != store.JobSuccess || r.FinishedAt == nil {
		t.Errorf("AdvanceRun = %+v, %+v, %v", r, changed, err)
	}
}

func TestCancelRun(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	runner := &store.Runner{Name: "r", Kind: "ci", Labels: []string{}}
	testutil.Must(t, st.CreateRunner(ctx, runner, "hr"))
	running := &store.Job{Name: "running"}
	queued := &store.Job{Name: "queued"}
	waiting := &store.Job{Name: "waiting", Needs: []string{"running"}, Status: store.JobWaiting}
	run := newRun(t, st, "pipeline", running, queued, waiting)
	st.ClaimJob(ctx, runner, "tok", anyJob)

	r, changed, err := st.CancelRun(ctx, run.ID)
	if err != nil || len(changed) != 2 || r.FinishedAt != nil {
		t.Fatalf("CancelRun = %+v, %d changed, %v", r, len(changed), err)
	}
	// The running job is asked to stop through its heartbeat.
	if cancel, err := st.Heartbeat(ctx, running.ID); !cancel || err != nil {
		t.Errorf("Heartbeat = %v, %v", cancel, err)
	}
	r, _, _ = st.FinishJob(ctx, running.ID, store.JobCancelled, "cancelled")
	if r.Status != store.JobCancelled || r.FinishedAt == nil {
		t.Errorf("run = %+v", r)
	}
	// A finished job's heartbeat says stop.
	if cancel, _ := st.Heartbeat(ctx, running.ID); !cancel {
		t.Error("heartbeat of a finished job must cancel")
	}
}

func TestPruneLogs(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	job := &store.Job{Name: "j"}
	newRun(t, st, "pipeline", job)
	testutil.Must(t, st.AppendLog(ctx, job.ID, 0, "old\n"))
	if n, err := st.PruneLogs(ctx, time.Now().Add(time.Hour)); n != 1 || err != nil {
		t.Errorf("PruneLogs(every job) = %d, %v", n, err)
	}
	if left, _ := st.LogPage(ctx, job.ID, -1, 10); len(left) != 0 {
		t.Errorf("left after pruning: %v", left)
	}
}

func TestLogsHeartbeatsAndStaleJobs(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	runner := &store.Runner{Name: "r", Kind: "ci", Labels: []string{}}
	testutil.Must(t, st.CreateRunner(ctx, runner, "hr"))
	job := &store.Job{Name: "j"}
	newRun(t, st, "pipeline", job)
	st.ClaimJob(ctx, runner, "tok", anyJob)

	testutil.Must(t, st.AppendLog(ctx, job.ID, 0, "line 1\n"))
	testutil.Must(t, st.AppendLog(ctx, job.ID, 1, "line 2\n"))
	testutil.Must(t, st.AppendLog(ctx, job.ID, 1, "retry ignored\n"))
	testutil.Must(t, st.AppendLog(ctx, job.ID, 2, "line 3\n"))
	chunks, _ := st.LogPage(ctx, job.ID, -1, 10)
	if len(chunks) != 3 || chunks[1].Data != "line 2\n" {
		t.Errorf("chunks = %+v", chunks)
	}
	if after, _ := st.LogPage(ctx, job.ID, 0, 1); len(after) != 1 || after[0].Seq != 1 {
		t.Errorf("one chunk after 0 = %+v", after)
	}
	if tail, cut, _ := st.LogTail(ctx, job.ID, 10); !cut || len(tail) != 2 || tail[0].Seq != 1 || tail[1].Seq != 2 {
		t.Errorf("LogTail(10 bytes) = %+v, cut %v", tail, cut)
	}
	if tail, cut, _ := st.LogTail(ctx, job.ID, 1<<20); cut || len(tail) != 3 {
		t.Errorf("LogTail(1MiB) = %+v, cut %v", tail, cut)
	}
	var all strings.Builder
	testutil.Must(t, st.StreamLog(ctx, job.ID, func(d string) error { all.WriteString(d); return nil }))
	if all.String() != "line 1\nline 2\nline 3\n" {
		t.Errorf("StreamLog = %q", all.String())
	}
	if n, err := st.PruneLogs(ctx, time.Now().Add(-time.Hour)); n != 0 || err != nil {
		t.Errorf("PruneLogs(an hour ago) = %d, %v", n, err)
	}
	if cancel, err := st.Heartbeat(ctx, job.ID); cancel || err != nil {
		t.Errorf("Heartbeat = %v, %v", cancel, err)
	}
	if ids, _ := st.StaleJobs(ctx, time.Minute); len(ids) != 0 {
		t.Errorf("fresh job reported stale: %v", ids)
	}
	time.Sleep(20 * time.Millisecond)
	if ids, _ := st.StaleJobs(ctx, 10*time.Millisecond); len(ids) != 1 || ids[0] != job.ID {
		t.Errorf("StaleJobs = %v", ids)
	}
	steps := []store.StepState{{Name: "a", Status: "success"}}
	testutil.Must(t, st.SetJobSteps(ctx, job.ID, steps))
	j, _ := st.JobByID(ctx, job.ID)
	if len(j.Steps) != 1 || j.Steps[0].Status != "success" || j.Duration() < 0 {
		t.Errorf("job = %+v", j)
	}
	if (&store.Job{}).Duration() != 0 {
		t.Error("duration of an unstarted job")
	}
}

func TestCommitStatusesAndRunList(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	sha := strings.Repeat("f", 40)
	testutil.Must(t, st.SetCommitStatus(ctx, &store.CommitStatus{SHA: sha, Context: "ci / test", State: "pending"}, nil))
	testutil.Must(t, st.SetCommitStatus(ctx, &store.CommitStatus{SHA: sha, Context: "ci / test", State: "success", Description: "ok"}, nil))
	testutil.Must(t, st.SetCommitStatus(ctx, &store.CommitStatus{SHA: sha, Context: "ci / lint", State: "skipped"}, nil))
	list, _ := st.CommitStatuses(ctx, sha)
	if len(list) != 2 || list[1].Context != "ci / test" || list[1].State != "success" || list[1].Description != "ok" {
		t.Errorf("statuses = %+v", list)
	}
	for state, ok := range map[string]bool{"success": true, "skipped": true, "failure": false, "pending": false, "error": false} {
		if (&store.CommitStatus{State: state}).OK() != ok {
			t.Errorf("OK(%s) != %v", state, ok)
		}
	}
	for _, s := range []string{store.JobSuccess, store.JobFailure, store.JobCancelled, store.JobSkipped} {
		if !store.JobTerminal(s) {
			t.Errorf("%s not terminal", s)
		}
	}
	if store.JobTerminal(store.JobRunning) || store.JobTerminal(store.JobWaiting) {
		t.Error("running/waiting terminal")
	}

	newRun(t, st, "pipeline", &store.Job{Name: "a"})
	newRun(t, st, "deploy", &store.Job{Name: "b", Kind: "deploy"})
	if runs, _ := st.ListRuns(ctx, store.RunFilter{Kind: "deploy", Limit: 10}); len(runs) != 1 || runs[0].Kind != "deploy" {
		t.Errorf("ListRuns(deploy) = %+v", runs)
	}
	if runs, _ := st.ListRuns(ctx, store.RunFilter{SHA: strings.Repeat("a", 40), Limit: 10}); len(runs) != 2 {
		t.Errorf("ListRuns(sha) = %d", len(runs))
	}
	if runs, _ := st.ListRuns(ctx, store.RunFilter{Limit: 1, Offset: 1}); len(runs) != 1 || runs[0].Kind != "pipeline" {
		t.Errorf("ListRuns paging = %+v", runs)
	}
}
