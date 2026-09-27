package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"onegit/internal/ci"
	"onegit/internal/store"
	"onegit/internal/testutil"
)

// fakeServer implements the runner protocol for a fixed list of jobs.
type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	queue    []*ci.Assignment
	logs     map[int64]*strings.Builder
	steps    map[int64][]store.StepState
	finished map[int64]ci.FinishRequest
	cancel   func(jobID int64, log string) bool
	fetches  int
	done     chan int64
}

func newFakeServer(t *testing.T, jobs ...*ci.Assignment) *fakeServer {
	f := &fakeServer{t: t, queue: jobs, logs: map[int64]*strings.Builder{}, steps: map[int64][]store.StepState{},
		finished: map[int64]ci.FinishRequest{}, done: make(chan int64, 10)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/runner/v1/fetch", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.fetches++
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer ogrun_test" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		var a *ci.Assignment
		if len(f.queue) > 0 {
			a, f.queue = f.queue[0], f.queue[1:]
		}
		f.mu.Unlock()
		if a == nil {
			time.Sleep(50 * time.Millisecond)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		json.NewEncoder(w).Encode(a)
	})
	job := func(r *http.Request) int64 {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if r.Header.Get("Authorization") != "Bearer ogj_"+r.PathValue("id") {
			t.Errorf("job %d: wrong token %q", id, r.Header.Get("Authorization"))
		}
		return id
	}
	mux.HandleFunc("POST /api/runner/v1/jobs/{id}/log", func(w http.ResponseWriter, r *http.Request) {
		id := job(r)
		var req ci.LogRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		if f.logs[id] == nil {
			f.logs[id] = &strings.Builder{}
		}
		f.logs[id].WriteString(req.Data)
		cancel := f.cancel != nil && f.cancel(id, f.logs[id].String())
		f.mu.Unlock()
		json.NewEncoder(w).Encode(ci.LogResponse{Cancel: cancel})
	})
	mux.HandleFunc("POST /api/runner/v1/jobs/{id}/steps", func(w http.ResponseWriter, r *http.Request) {
		id := job(r)
		var steps []store.StepState
		json.NewDecoder(r.Body).Decode(&steps)
		f.mu.Lock()
		f.steps[id] = steps
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/runner/v1/jobs/{id}/finish", func(w http.ResponseWriter, r *http.Request) {
		id := job(r)
		var req ci.FinishRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.finished[id] = req
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		f.done <- id
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) log(id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logs[id] == nil {
		return ""
	}
	return f.logs[id].String()
}

// runJobs runs a runner until every job has finished.
func runJobs(t *testing.T, f *fakeServer, workdir string, capacity int) {
	t.Helper()
	r, err := New(Config{URL: f.srv.URL + "/", Token: "ogrun_test", WorkDir: workdir, Capacity: capacity,
		Version: "test", Log: testutil.Logger()})
	if err != nil {
		t.Fatal(err)
	}
	// Count the jobs before the runner starts taking them off the queue.
	f.mu.Lock()
	n := len(f.queue)
	f.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(stopped)
	}()
	for i := 0; i < n; i++ {
		select {
		case <-f.done:
		case <-time.After(60 * time.Second):
			t.Fatal("jobs did not finish")
		}
	}
	cancel()
	<-stopped
}

// repo returns a bare repository with one commit (and its SHA).
func repo(t *testing.T, files map[string]string) (string, string) {
	bare := testutil.Bare(t)
	w := testutil.NewWork(t)
	sha := w.Commit("init", files)
	w.Push(bare, "main")
	return bare.Path, sha
}

func assignment(id int64, repoURL, sha string, steps ...ci.Step) *ci.Assignment {
	return &ci.Assignment{ID: id, RunID: 1, Name: "job", Kind: "ci", Token: "ogj_" + strconv.FormatInt(id, 10),
		RepoURL: repoURL, SHA: sha, Ref: "refs/heads/main", Steps: steps, TimeoutSec: 60}
}

func TestRunJob(t *testing.T) {
	t.Setenv("ONEGIT_RUNNER_TOKEN", "must-not-leak")
	url, sha := repo(t, map[string]string{"README.md": "hi", "sub/file.txt": "x"})
	a := assignment(1, url, sha,
		ci.Step{Name: "greet", Run: `echo "$GREETING from $ONEGIT_SHA"; echo "key=$API_KEY"; echo "runner=${ONEGIT_RUNNER_TOKEN:-unset}"`},
		ci.Step{Run: "test -f README.md && git rev-parse HEAD && git remote get-url origin"},
		ci.Step{Run: "pwd; echo $STEP_VAR", WorkingDir: "sub", Env: map[string]string{"STEP_VAR": "from-step"}},
		ci.Step{Run: `cat "$ONEGIT_CHANGED_FILES"; echo; cat "$DOCKER_CONFIG/config.json"; echo; test -n "$ONEGIT_TOKEN"`},
	)
	a.Env = map[string]string{"GREETING": "hello", "API_KEY": "super-secret-value", "ONEGIT_SHA": sha}
	a.Masks = []string{"super-secret-value"}
	a.ChangedFiles = []string{"a.go", "b.go"}
	f := newFakeServer(t, a)
	work := t.TempDir()
	os.MkdirAll(filepath.Join(work, "job-99"), 0o755) // a crashed runner's leftovers
	runJobs(t, f, work, 1)

	res := f.finished[1]
	if res.Status != "success" {
		t.Fatalf("result = %+v\n%s", res, f.log(1))
	}
	log := f.log(1)
	for _, want := range []string{"hello from " + sha, "key=***", "runner=unset", "##[step] greet", "##[step] Run test -f README.md",
		sha + "\n" + url, filepath.Join("sub") + "\nfrom-step", "a.go\nb.go", `{"auths":{}}`} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "super-secret-value") {
		t.Error("secret leaked into the log")
	}
	steps := f.steps[1]
	if len(steps) != 5 || steps[0].Name != "Set up job" || steps[1].Name != "greet" {
		t.Fatalf("steps = %+v", steps)
	}
	for _, s := range steps {
		if s.Status != "success" || s.StartedAt == nil || s.FinishedAt == nil {
			t.Errorf("step %+v", s)
		}
	}
	// Workspaces are removed; the cache repository stays.
	left, _ := filepath.Glob(filepath.Join(work, "job-*"))
	if len(left) != 0 {
		t.Errorf("leftover workspaces: %v", left)
	}
	if _, err := os.Stat(filepath.Join(work, ".cache", "repo.git", "HEAD")); err != nil {
		t.Errorf("cache repository missing: %v", err)
	}
}

func TestFailingSteps(t *testing.T) {
	url, sha := repo(t, map[string]string{"x": "y"})
	fail := assignment(1, url, sha,
		ci.Step{Run: "exit 7", ContinueOnError: true},
		ci.Step{Run: "false | true"}, // pipefail
		ci.Step{Run: "echo never"},
	)
	bad := assignment(2, url, strings.Repeat("0", 40), ci.Step{Run: "echo never"})
	f := newFakeServer(t, fail, bad)
	runJobs(t, f, t.TempDir(), 2)

	res := f.finished[1]
	if res.Status != "failure" || !strings.Contains(res.Message, `step "Run false | true" failed: exit code 1`) {
		t.Errorf("result = %+v", res)
	}
	var states []string
	for _, s := range f.steps[1] {
		states = append(states, s.Status)
	}
	if strings.Join(states, ",") != "success,failure,failure,skipped" {
		t.Errorf("step states = %v", states)
	}
	if !strings.Contains(f.log(1), "Step failed (exit code 7); continuing") || strings.Contains(f.log(1), "never") {
		t.Errorf("log:\n%s", f.log(1))
	}
	if res := f.finished[2]; res.Status != "failure" || !strings.Contains(res.Message, "set up failed") {
		t.Errorf("bad checkout = %+v", res)
	}
}

func TestCancelAndTimeout(t *testing.T) {
	url, sha := repo(t, map[string]string{"x": "y"})
	// fin""ished: the step name shows the command, the output must not.
	cancelled := assignment(1, url, sha, ci.Step{Run: `echo started; sleep 30; echo fin""ished`}, ci.Step{Run: `echo ne""xt`})
	slow := assignment(2, url, sha, ci.Step{Run: "sleep 30"})
	slow.TimeoutSec = 1
	f := newFakeServer(t, cancelled, slow)
	f.cancel = func(id int64, log string) bool { return id == 1 && strings.Contains(log, "started") }
	start := time.Now()
	runJobs(t, f, t.TempDir(), 2)
	if time.Since(start) > 20*time.Second {
		t.Errorf("jobs took %s: the process group was not stopped", time.Since(start))
	}
	if res := f.finished[1]; res.Status != "cancelled" {
		t.Errorf("cancelled job = %+v", res)
	}
	if strings.Contains(f.log(1), "finished") || strings.Contains(f.log(1), "\nnext") {
		t.Errorf("cancelled job kept running:\n%s", f.log(1))
	}
	if res := f.finished[2]; res.Status != "failure" || !strings.Contains(res.Message, "timed out after 1s") {
		t.Errorf("timed-out job = %+v", res)
	}
}

func TestToolingCheckout(t *testing.T) {
	bare := testutil.Bare(t)
	w := testutil.NewWork(t)
	recipe := w.Commit("recipe", map[string]string{"deploy.sh": "echo tooling"})
	target := w.Commit("target", map[string]string{"deploy.sh": "echo target", "app": "v2"})
	w.Push(bare, "main")
	a := assignment(1, bare.Path, target, ci.Step{Run: `sh ./deploy.sh; sh "$ONEGIT_TOOLING_DIR/deploy.sh"; cat app`})
	a.Kind, a.ToolingSHA = "deploy", recipe
	f := newFakeServer(t, a)
	runJobs(t, f, t.TempDir(), 1)
	if res := f.finished[1]; res.Status != "success" || !strings.Contains(f.log(1), "echo target\n") && !strings.Contains(f.log(1), "target\ntooling\nv2") {
		t.Errorf("result = %+v\n%s", res, f.log(1))
	}
}

func TestNewValidates(t *testing.T) {
	for _, c := range []Config{
		{Token: "ogrun_x"},
		{URL: "http://x"},
		{URL: "http://x", Token: "og_personal"},
	} {
		c.WorkDir = t.TempDir()
		if _, err := New(c); err == nil {
			t.Errorf("New(%+v) accepted", c)
		}
	}
	r, err := New(Config{URL: "http://x/", Token: "ogrun_x", WorkDir: t.TempDir(), Log: testutil.Logger()})
	if err != nil || r.cfg.Capacity != 1 || r.cfg.URL != "http://x" {
		t.Errorf("defaults: %+v, %v", r, err)
	}
}

func TestRunnerRetriesAfterErrors(t *testing.T) {
	f := newFakeServer(t)
	r, _ := New(Config{URL: f.srv.URL, Token: "ogrun_wrong", WorkDir: t.TempDir(), Log: testutil.Logger()})
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Errorf("Run = %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Backoff: 1s after the first failure, so at most two fetches.
	if f.fetches < 1 || f.fetches > 2 {
		t.Errorf("%d fetches", f.fetches)
	}
}

// ---- log stream ----

func TestLogStreamMasksAndChunks(t *testing.T) {
	var mu sync.Mutex
	var chunks []string
	var seqs []int
	fail := true
	l := newLogStream([]string{"s3cr3t"}, func(seq int, data string) bool {
		mu.Lock()
		defer mu.Unlock()
		if data != "" && fail {
			fail = false
			return false // first send fails; the data must be retried
		}
		chunks = append(chunks, data)
		seqs = append(seqs, seq)
		return true
	})
	l.Write([]byte("password=s3cr"))
	l.flush(false) // no complete line yet: nothing is sent
	l.Write([]byte("3t\npartial"))
	l.flush(false) // fails
	l.flush(false) // retried
	l.flush(true)
	mu.Lock()
	defer mu.Unlock()
	if len(chunks) != 2 || chunks[0] != "password=***\n" || chunks[1] != "partial" || !slices.Equal(seqs, []int{0, 1}) {
		t.Errorf("chunks = %q, seqs %v", chunks, seqs)
	}
}

func TestLogStreamHeartbeatAndLimits(t *testing.T) {
	var got []int
	l := newLogStream(nil, func(seq int, data string) bool {
		got = append(got, seq)
		return true
	})
	l.lastSend = time.Now().Add(-time.Minute)
	l.flush(false)
	if !slices.Equal(got, []int{-1}) {
		t.Errorf("heartbeat seqs = %v", got)
	}
	// A very long line without newline is sent once it's big enough.
	l.Write([]byte(strings.Repeat("x", partialLineWait)))
	l.flush(false)
	if len(got) != 2 || got[1] != 0 {
		t.Errorf("long partial line not sent: %v", got)
	}
	// Output beyond the cap is dropped with a note.
	l.sent = maxLogBytes + 1
	l.Write([]byte("more\n"))
	l.Write([]byte("and more\n"))
	if string(l.buf) != "\n[log truncated: the job printed more than 32 MiB]\n" {
		t.Errorf("buffer = %q", l.buf)
	}
}

func TestHelpers(t *testing.T) {
	for st, want := range map[string]string{
		"make test":             "Run make test",
		"  first line\nsecond":  "Run first line",
		strings.Repeat("a", 80): "Run " + strings.Repeat("a", 57) + "...",
	} {
		if got := stepName(ci.Step{Run: st}); got != want {
			t.Errorf("stepName(%q) = %q", st, got)
		}
	}
	if stepName(ci.Step{Name: "Named", Run: "x"}) != "Named" {
		t.Error("explicit step name ignored")
	}
	env := withoutKey([]string{"A=1", "DOCKER_CONFIG=/x", "DOCKER_CONFIGX=2"}, "DOCKER_CONFIG")
	if !slices.Equal(env, []string{"A=1", "DOCKER_CONFIGX=2"}) {
		t.Errorf("withoutKey = %v", env)
	}
	if lastLine("a\nb\nc") != "c" || lastLine("single") != "single" {
		t.Error("lastLine")
	}
	t.Setenv("ONEGIT_RUNNER_URL", "x")
	for _, kv := range baseEnv() {
		if strings.HasPrefix(kv, "ONEGIT_RUNNER_") {
			t.Errorf("baseEnv leaks %s", kv)
		}
	}
}

func TestIsolatedDockerConfig(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "cli-plugins"), 0o755)
	os.WriteFile(filepath.Join(src, "config.json"), []byte(`{"auths":{"registry":{"auth":"secret"}}}`), 0o600)
	t.Setenv("DOCKER_CONFIG", src)
	dir, err := isolatedDockerConfig(filepath.Join(t.TempDir(), "job.docker"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if string(b) != `{"auths":{}}` {
		t.Errorf("config.json = %s", b)
	}
	if target, err := os.Readlink(filepath.Join(dir, "cli-plugins")); err != nil || target != filepath.Join(src, "cli-plugins") {
		t.Errorf("cli-plugins link = %q, %v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "contexts")); err == nil {
		t.Error("linked a directory that does not exist")
	}
}
