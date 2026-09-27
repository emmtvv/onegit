// Package runner is the job executor started with `onegit runner`. It polls
// the server for jobs, checks the commit out from a local cache, runs the
// steps with bash on the host and streams output back.
package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"onegit/internal/ci"
	"onegit/internal/store"
)

type Config struct {
	URL       string // onegit base URL
	Token     string // runner token
	WorkDir   string
	Capacity  int
	KeepWork  bool
	Version   string
	Log       *slog.Logger
	shellPath string
}

type Runner struct {
	cfg     Config
	client  *http.Client
	cacheMu sync.Mutex // serialises fetches into the shared cache repository
}

func New(cfg Config) (*Runner, error) {
	if cfg.URL == "" || cfg.Token == "" {
		return nil, errors.New("runner: -url and -token are required")
	}
	if !strings.HasPrefix(cfg.Token, ci.RunnerTokenPrefix) {
		return nil, errors.New("runner: the token must be a runner token (" + ci.RunnerTokenPrefix + "…)")
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	if cfg.Capacity <= 0 {
		cfg.Capacity = 1
	}
	abs, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return nil, err
	}
	cfg.WorkDir = abs
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return nil, err
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil, errors.New("runner: git is required")
	}
	if p, err := exec.LookPath("bash"); err == nil {
		cfg.shellPath = p
	} else if p, err := exec.LookPath("sh"); err == nil {
		cfg.shellPath = p
	} else {
		return nil, errors.New("runner: bash or sh is required")
	}
	return &Runner{cfg: cfg, client: &http.Client{}}, nil
}

// Run polls for jobs until ctx is cancelled; running jobs are then stopped
// and reported as failed.
func (r *Runner) Run(ctx context.Context) error {
	r.cfg.Log.Info("runner started", "server", r.cfg.URL, "capacity", r.cfg.Capacity, "workdir", r.cfg.WorkDir)
	// Workspaces left behind by a crashed runner (nothing runs yet).
	if stale, _ := filepath.Glob(filepath.Join(r.cfg.WorkDir, "job-*")); len(stale) > 0 {
		for _, p := range stale {
			os.RemoveAll(p)
		}
		r.cfg.Log.Info("removed stale workspaces", "count", len(stale))
	}
	slots := make(chan struct{}, r.cfg.Capacity)
	var wg sync.WaitGroup
	defer wg.Wait()
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return nil
		case slots <- struct{}{}:
		}
		job, err := r.fetch(ctx)
		if err != nil {
			<-slots
			if ctx.Err() != nil {
				return nil
			}
			r.cfg.Log.Warn("fetch job", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
		if job == nil {
			<-slots
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			r.execute(ctx, job)
		}()
	}
}

func (r *Runner) post(ctx context.Context, path, token string, body, out any, timeout time.Duration) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.URL+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusGone {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusGone) {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && resp.StatusCode == http.StatusOK {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func (r *Runner) fetch(ctx context.Context) (*ci.Assignment, error) {
	var a ci.Assignment
	code, err := r.post(ctx, "/api/runner/v1/fetch", r.cfg.Token, ci.FetchRequest{Version: r.cfg.Version}, &a, 60*time.Second)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNoContent {
		return nil, nil
	}
	return &a, nil
}

// ---- job execution ----

type jobRun struct {
	r      *Runner
	a      *ci.Assignment
	log    *logStream
	cancel context.CancelCauseFunc
	steps  []store.StepState
}

var errCancelled = errors.New("cancelled on the server")

func (r *Runner) execute(parent context.Context, a *ci.Assignment) {
	logger := r.cfg.Log.With("job", a.ID, "name", a.Name)
	logger.Info("job started")
	timeout := time.Duration(a.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = time.Hour
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	ctx, cancelTimeout := context.WithTimeoutCause(ctx, timeout, fmt.Errorf("timed out after %s", timeout))
	defer cancelTimeout()

	jr := &jobRun{r: r, a: a, cancel: cancel}
	jr.log = newLogStream(a.Masks, func(seq int, data string) bool {
		var resp ci.LogResponse
		_, err := r.post(context.Background(), fmt.Sprintf("/api/runner/v1/jobs/%d/log", a.ID), a.Token,
			ci.LogRequest{Seq: seq, Data: data}, &resp, 30*time.Second)
		if err != nil {
			logger.Warn("send log", "err", err)
			return false
		}
		if resp.Cancel {
			cancel(errCancelled)
		}
		return true
	})
	go jr.log.pump(ctx)

	status, message := jr.run(ctx)
	jr.log.close()
	if !r.cfg.KeepWork {
		r.cleanup(a)
	}
	req := ci.FinishRequest{Status: status, Message: message}
	for i := 0; i < 5; i++ {
		if _, err := r.post(context.Background(), fmt.Sprintf("/api/runner/v1/jobs/%d/finish", a.ID), a.Token, req, nil, 30*time.Second); err == nil {
			break
		} else {
			logger.Warn("report result", "err", err)
			time.Sleep(time.Duration(i+1) * 2 * time.Second)
		}
	}
	logger.Info("job finished", "status", status, "message", message)
}

func (r *Runner) workspace(a *ci.Assignment) string {
	return filepath.Join(r.cfg.WorkDir, "job-"+strconv.FormatInt(a.ID, 10))
}

func (jr *jobRun) run(ctx context.Context) (status, message string) {
	a := jr.a
	ws := jr.r.workspace(a)
	for _, st := range a.Steps {
		jr.steps = append(jr.steps, store.StepState{Name: stepName(st), Status: "pending"})
	}
	jr.steps = append([]store.StepState{{Name: "Set up job", Status: "pending"}}, jr.steps...)

	now := time.Now()
	jr.steps[0].Status, jr.steps[0].StartedAt = "running", &now
	jr.reportSteps()
	jr.log.header("Set up job")
	fmt.Fprintf(jr.log, "Runner: %s (onegit %s)\nCommit: %s (%s)\n", hostname(), jr.r.cfg.Version, a.SHA, a.Ref)
	env, err := jr.prepare(ctx, ws)
	done := time.Now()
	jr.steps[0].FinishedAt = &done
	if err != nil {
		jr.steps[0].Status = "failure"
		fmt.Fprintf(jr.log, "Error: %v\n", err)
		jr.skipRest(1)
		return jr.outcome(ctx, "failure", "set up failed: "+err.Error())
	}
	jr.steps[0].Status = "success"

	for i, st := range a.Steps {
		idx := i + 1
		if ctx.Err() != nil {
			jr.skipRest(idx)
			return jr.outcome(ctx, "failure", "")
		}
		now := time.Now()
		jr.steps[idx].Status, jr.steps[idx].StartedAt = "running", &now
		jr.reportSteps()
		jr.log.header(stepName(st))
		err := jr.runStep(ctx, ws, env, st)
		done := time.Now()
		jr.steps[idx].FinishedAt = &done
		switch {
		case err == nil:
			jr.steps[idx].Status = "success"
		case st.ContinueOnError && ctx.Err() == nil:
			jr.steps[idx].Status = "failure"
			fmt.Fprintf(jr.log, "Step failed (%v); continuing\n", err)
		default:
			jr.steps[idx].Status = "failure"
			fmt.Fprintf(jr.log, "Error: %v\n", err)
			jr.skipRest(idx + 1)
			return jr.outcome(ctx, "failure", fmt.Sprintf("step %q failed: %v", stepName(st), err))
		}
	}
	jr.reportSteps()
	return "success", ""
}

// outcome distinguishes cancellation and timeouts from plain failures.
func (jr *jobRun) outcome(ctx context.Context, status, message string) (string, string) {
	defer jr.reportSteps()
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, errCancelled):
		return "cancelled", "cancelled"
	case cause != nil && !errors.Is(cause, context.Canceled):
		return "failure", cause.Error()
	case cause != nil:
		return "failure", "the runner is shutting down"
	}
	return status, message
}

func (jr *jobRun) skipRest(from int) {
	for i := from; i < len(jr.steps); i++ {
		if jr.steps[i].Status == "pending" {
			jr.steps[i].Status = "skipped"
		}
	}
}

func (jr *jobRun) reportSteps() {
	_, err := jr.r.post(context.Background(), fmt.Sprintf("/api/runner/v1/jobs/%d/steps", jr.a.ID), jr.a.Token, jr.steps, nil, 30*time.Second)
	if err != nil {
		jr.r.cfg.Log.Warn("report steps", "job", jr.a.ID, "err", err)
	}
}

func stepName(st ci.Step) string {
	if st.Name != "" {
		return st.Name
	}
	line, _, _ := strings.Cut(strings.TrimSpace(st.Run), "\n")
	if len(line) > 60 {
		line = line[:57] + "..."
	}
	return "Run " + line
}

// prepare checks out the commit (and the recipe commit for tooling) and
// builds the job environment.
func (jr *jobRun) prepare(ctx context.Context, ws string) ([]string, error) {
	a := jr.a
	os.RemoveAll(ws)
	if err := jr.r.checkout(ctx, a, a.SHA, ws, jr.log); err != nil {
		return nil, err
	}
	env := withoutKey(baseEnv(), "DOCKER_CONFIG")
	for k, v := range a.Env {
		env = append(env, k+"="+v)
	}
	env = append(env, "ONEGIT_TOKEN="+a.Token, "ONEGIT_WORKSPACE="+ws)
	if a.ToolingSHA != "" {
		tooling := ws + "-tooling"
		if err := jr.r.checkout(ctx, a, a.ToolingSHA, tooling, jr.log); err != nil {
			return nil, fmt.Errorf("tooling checkout: %w", err)
		}
		env = append(env, "ONEGIT_TOOLING_DIR="+tooling)
	}
	dockerDir, err := isolatedDockerConfig(ws + ".docker")
	if err != nil {
		return nil, fmt.Errorf("docker config: %w", err)
	}
	env = append(env, "DOCKER_CONFIG="+dockerDir)
	changed := ws + ".changed"
	if err := os.WriteFile(changed, []byte(strings.Join(a.ChangedFiles, "\n")), 0o644); err != nil {
		return nil, err
	}
	env = append(env, "ONEGIT_CHANGED_FILES="+changed)
	return env, nil
}

// isolatedDockerConfig gives the job its own docker client config: a
// `docker login` inside the job stays in the job (never in the host's
// keychain or config) and the job cannot use the host's registry
// credentials. Plugins (buildx) and contexts are shared by symlink.
func isolatedDockerConfig(dir string) (string, error) {
	src := os.Getenv("DOCKER_CONFIG")
	if src == "" {
		home, _ := os.UserHomeDir()
		src = filepath.Join(home, ".docker")
	}
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auths":{}}`), 0o600); err != nil {
		return "", err
	}
	for _, sub := range []string{"cli-plugins", "contexts", "buildx"} {
		if _, err := os.Stat(filepath.Join(src, sub)); err == nil {
			_ = os.Symlink(filepath.Join(src, sub), filepath.Join(dir, sub)) // best effort: plugins are optional
		}
	}
	return dir, nil
}

// baseEnv is the runner's environment minus its own settings.
func baseEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ONEGIT_RUNNER_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (r *Runner) cacheDir() string { return filepath.Join(r.cfg.WorkDir, ".cache", "repo.git") }

// checkout fetches sha into the runner's cache repository (repeated jobs
// only transfer new objects) and clones it locally into dir. A local clone
// hardlinks the objects, so it is cheap, and unlike a worktree its .git is
// self-contained: git keeps working inside `docker run -v $PWD:/src`.
func (r *Runner) checkout(ctx context.Context, a *ci.Assignment, sha, dir string, out io.Writer) error {
	cache := r.cacheDir()
	os.RemoveAll(dir)
	auth := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("ci:"+a.Token))
	r.cacheMu.Lock()
	err := func() error {
		if _, err := os.Stat(filepath.Join(cache, "HEAD")); err != nil {
			if err := r.git(ctx, "", out, "init", "-q", "--bare", cache); err != nil {
				return err
			}
		}
		if r.git(ctx, cache, io.Discard, "cat-file", "-e", sha+"^{commit}") == nil {
			return nil
		}
		fmt.Fprintf(out, "Fetching %s\n", sha)
		return r.git(ctx, cache, out, "-c", "http.extraHeader="+auth, "fetch", "-q", "--no-tags", "--no-write-fetch-head",
			a.RepoURL, "+"+sha+":refs/onegit/jobs/"+strconv.FormatInt(a.ID, 10))
	}()
	if err == nil {
		// The cache has no branches, only refs/onegit/jobs/*: git warns about
		// an "empty repository", which is noise in the job log (errors still
		// come back through the returned error).
		err = r.git(ctx, "", io.Discard, "clone", "-q", "--local", "--no-checkout", cache, dir)
	}
	r.cacheMu.Unlock()
	if err != nil {
		return err
	}
	work := filepath.Join(dir, ".git")
	if err := r.git(ctx, work, out, "remote", "set-url", "origin", a.RepoURL); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "checkout", "-q", "--detach", sha)
	cmd.Dir, cmd.Env = dir, append(baseEnv(), "GIT_TERMINAL_PROMPT=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout: %s", lastLine(strings.TrimSpace(string(b))))
	}
	return nil
}

func (r *Runner) cleanup(a *ci.Assignment) {
	ws := r.workspace(a)
	ctx := context.Background()
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	for _, dir := range []string{ws, ws + "-tooling"} {
		os.RemoveAll(dir)
	}
	os.Remove(ws + ".changed")
	os.RemoveAll(ws + ".docker")
	_ = r.git(ctx, r.cacheDir(), io.Discard, "update-ref", "-d", "refs/onegit/jobs/"+strconv.FormatInt(a.ID, 10))
}

func (r *Runner) git(ctx context.Context, dir string, out io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(baseEnv(), "GIT_TERMINAL_PROMPT=0")
	if dir != "" {
		cmd.Env = append(cmd.Env, "GIT_DIR="+dir)
	}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, io.MultiWriter(out, &stderr)
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("git %s: %s", args[0], lastLine(msg))
		}
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}

func (jr *jobRun) runStep(ctx context.Context, ws string, env []string, st ci.Step) error {
	shell := jr.r.cfg.shellPath
	args := []string{"-e", "-c", st.Run}
	if filepath.Base(shell) == "bash" {
		args = []string{"--noprofile", "--norc", "-eo", "pipefail", "-c", st.Run}
	}
	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.Dir = ws
	if st.WorkingDir != "" {
		cmd.Dir = filepath.Join(ws, filepath.Clean("/"+st.WorkingDir))
	}
	cmd.Env = append([]string{}, env...)
	for k, v := range st.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = jr.log, jr.log
	setProcessGroup(cmd)
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return context.Cause(ctx)
	case errors.As(err, &exit):
		return fmt.Errorf("exit code %d", exit.ExitCode())
	}
	return err
}

func withoutKey(env []string, key string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
