package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"onegit/internal/config"
	"onegit/internal/git"
	"onegit/internal/hooks"
	"onegit/internal/store"
)

// maxChangedFiles caps the change set kept per run.
const maxChangedFiles = 20000

const defaultTimeout = time.Hour

type Service struct {
	Store *store.Store
	Repo  *git.Repo
	Cfg   *config.Config
	Log   *slog.Logger

	// SecretsFor returns the decrypted secrets for a deploy target.
	SecretsFor func(ctx context.Context, target map[string]string) (map[string]string, error)
	// OnRunFinished is called once a run reaches its final status.
	OnRunFinished func(ctx context.Context, run *store.Run)
}

// JobPayload is stored in ci_jobs.spec.
type JobPayload struct {
	Steps      []Step            `json:"steps"`
	Env        map[string]string `json:"env"`
	TimeoutSec int               `json:"timeout"`
	Context    string            `json:"context,omitempty"` // commit status context (pipelines)
	RecipeFile string            `json:"recipe_file,omitempty"`
	RecipeSHA  string            `json:"recipe_sha,omitempty"`
	Tooling    bool              `json:"tooling,omitempty"`
}

func (s *Service) defaultBranch(ctx context.Context) string {
	if b := s.Repo.HeadBranch(ctx); b != "" {
		return b
	}
	return s.Cfg.Repo.DefaultBranch
}

// LoadPipelines reads .onegit/pipelines/*.yml at a commit. Broken files are
// returned as errors next to the valid pipelines.
func (s *Service) LoadPipelines(ctx context.Context, sha string) (map[string]*Pipeline, []error) {
	return loadDir(ctx, s.Repo, sha, PipelinesDir, ParsePipeline)
}

// LoadRecipes reads .onegit/deploy/*.yml at a commit (the default branch).
func (s *Service) LoadRecipes(ctx context.Context, sha string) ([]*Recipe, []error) {
	m, errs := loadDir(ctx, s.Repo, sha, RecipesDir, ParseRecipe)
	var out []*Recipe
	for _, r := range m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, errs
}

func loadDir[T any](ctx context.Context, repo *git.Repo, sha, dir string, parse func(string, []byte) (T, error)) (map[string]T, []error) {
	entries, err := repo.ListTree(ctx, sha, dir)
	if err != nil {
		return nil, nil // no such directory
	}
	out := map[string]T{}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !(strings.HasSuffix(e.Name, ".yml") || strings.HasSuffix(e.Name, ".yaml")) {
			continue
		}
		file := path.Join(dir, e.Name)
		src, err := repo.ReadBlob(ctx, sha, file, 1<<20)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", file, err))
			continue
		}
		v, err := parse(file, src)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out[file] = v
	}
	return out, errs
}

// ---- triggers ----

// OnPush starts pipelines for pushed branches and tags, and pull_request
// pipelines for open PRs whose head moved.
func (s *Service) OnPush(ctx context.Context, pusher *store.User, updates []hooks.RefUpdate) {
	var by *int64
	if pusher != nil {
		by = &pusher.ID
	}
	def := s.defaultBranch(ctx)
	for _, up := range updates {
		if up.NewSHA == git.ZeroSHA {
			continue
		}
		ev := Event{Name: "push", Ref: up.Ref}
		before := ""
		switch {
		case up.OldSHA != git.ZeroSHA:
			before = up.OldSHA
			ev.Changed, ev.ChangedKnown = s.changed(ctx, up.OldSHA, up.NewSHA)
		case strings.HasPrefix(up.Ref, "refs/heads/") && up.Ref != "refs/heads/"+def:
			// New branch: compare with where it forked from the default branch.
			if mb, err := s.Repo.MergeBase(ctx, "refs/heads/"+def, up.NewSHA); err == nil {
				ev.Changed, ev.ChangedKnown = s.changed(ctx, mb, up.NewSHA)
			}
		}
		s.startMatching(ctx, up.NewSHA, before, ev, nil, by)

		if branch, ok := strings.CutPrefix(up.Ref, "refs/heads/"); ok {
			pulls, _ := s.Store.OpenPullsForBranch(ctx, branch)
			for _, p := range pulls {
				if p.HeadBranch == branch {
					if fresh, err := s.Store.PullByID(ctx, p.ID); err == nil {
						s.OnPull(ctx, fresh, by)
					}
				}
			}
		}
	}
}

// OnPull runs pull_request pipelines for a PR's current head. Pipelines come
// from the head commit: they get no secrets, so this is safe.
func (s *Service) OnPull(ctx context.Context, p *store.Pull, by *int64) {
	if !p.IsOpen() {
		return
	}
	ev := Event{Name: "pull_request", Ref: "refs/pull/" + strconv.FormatInt(p.ID, 10) + "/head", BaseRef: p.BaseBranch}
	ev.Changed, ev.ChangedKnown = s.changed(ctx, p.MergeBase, p.HeadSHA)
	id := p.ID
	s.startMatching(ctx, p.HeadSHA, p.MergeBase, ev, &id, by)
}

// RunManual starts a pipeline with a "manual" trigger on a branch.
func (s *Service) RunManual(ctx context.Context, u *store.User, file, branch string) (*store.Run, error) {
	sha, err := s.Repo.ResolveCommit(ctx, "refs/heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("branch %q does not exist", branch)
	}
	pipes, _ := s.LoadPipelines(ctx, sha)
	p, ok := pipes[file]
	if !ok {
		return nil, fmt.Errorf("%s does not exist on %s", file, branch)
	}
	if !p.On.Manual {
		return nil, fmt.Errorf("%s has no manual trigger", file)
	}
	return s.startPipeline(ctx, p, file, Event{Name: "manual", Ref: "refs/heads/" + branch}, sha, "", nil, &u.ID)
}

func (s *Service) changed(ctx context.Context, from, to string) ([]string, bool) {
	files, err := s.Repo.ChangedFiles(ctx, from, to)
	if err != nil {
		return nil, false
	}
	if len(files) > maxChangedFiles {
		return files[:maxChangedFiles], false
	}
	return files, true
}

func (s *Service) startMatching(ctx context.Context, sha, before string, ev Event, pullID, by *int64) {
	pipes, errs := s.LoadPipelines(ctx, sha)
	for _, err := range errs {
		s.Log.Warn("invalid pipeline", "sha", sha, "err", err)
		if err := s.Store.SetCommitStatus(ctx, &store.CommitStatus{SHA: sha, Context: "onegit / pipelines", State: "error",
			Description: truncate(err.Error(), 200)}, nil); err != nil {
			s.Log.Warn("set commit status", "err", err)
		}
	}
	files := make([]string, 0, len(pipes))
	for f := range pipes {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		p := pipes[f]
		if !p.Matches(ev) {
			continue
		}
		if _, err := s.startPipeline(ctx, p, f, ev, sha, before, pullID, by); err != nil {
			s.Log.Error("start pipeline", "file", f, "sha", sha, "err", err)
		}
	}
}

func (s *Service) startPipeline(ctx context.Context, p *Pipeline, file string, ev Event, sha, before string, pullID, by *int64) (*store.Run, error) {
	run := &store.Run{Kind: "pipeline", Name: p.Name, File: file, Event: ev.Name, Ref: ev.Ref, SHA: sha, BeforeSHA: before,
		ChangedFiles: ev.Changed, PullID: pullID, TriggeredBy: by, Status: store.JobQueued}
	names := make([]string, 0, len(p.Jobs))
	for n := range p.Jobs {
		names = append(names, n)
	}
	sort.Strings(names)
	var jobs []*store.Job
	for _, n := range names {
		js := p.Jobs[n]
		timeout := time.Duration(js.Timeout)
		if timeout <= 0 {
			timeout = defaultTimeout
		}
		payload := JobPayload{Steps: js.Steps, Env: merge(p.Env, js.Env), TimeoutSec: int(timeout.Seconds()),
			Context: p.Name + " / " + n}
		spec, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		j := &store.Job{Name: n, Kind: "ci", Needs: js.Needs, RunsOn: js.RunsOn, Spec: spec, Status: store.JobQueued}
		switch {
		case len(js.Events) > 0 && !contains(js.Events, ev.Name):
			j.Status, j.Message = store.JobSkipped, "not run for "+ev.Name+" events"
		case len(js.Needs) > 0:
			j.Status = store.JobWaiting
		}
		jobs = append(jobs, j)
	}
	if err := s.Store.CreateRun(ctx, run, jobs); err != nil {
		return nil, err
	}
	for _, j := range jobs {
		s.setStatus(ctx, run, j)
	}
	// Jobs skipped up front may skip or unblock others.
	if r, changed, err := s.Store.AdvanceRun(ctx, run.ID); err == nil {
		s.afterChange(ctx, r, changed)
		run = r
	}
	return run, nil
}

// ---- results ----

// FinishJob records a job result and propagates it.
func (s *Service) FinishJob(ctx context.Context, jobID int64, status, message string) error {
	run, changed, err := s.Store.FinishJob(ctx, jobID, status, message)
	if err != nil {
		return err
	}
	s.afterChange(ctx, run, changed)
	return nil
}

// Cancel stops a run.
func (s *Service) Cancel(ctx context.Context, runID int64) error {
	run, changed, err := s.Store.CancelRun(ctx, runID)
	if err != nil {
		return err
	}
	s.afterChange(ctx, run, changed)
	return nil
}

// JobStarted is called when a runner picks a job up.
func (s *Service) JobStarted(ctx context.Context, j *store.Job) {
	if run, err := s.Store.RunByID(ctx, j.RunID); err == nil {
		s.setStatus(ctx, run, j)
	}
}

func (s *Service) afterChange(ctx context.Context, run *store.Run, changed []*store.Job) {
	if run == nil {
		return
	}
	for _, j := range changed {
		s.setStatus(ctx, run, j)
	}
	if run.FinishedAt != nil && s.OnRunFinished != nil && len(changed) > 0 {
		s.OnRunFinished(ctx, run)
	}
}

// setStatus mirrors a pipeline job onto the commit status of its context.
func (s *Service) setStatus(ctx context.Context, run *store.Run, j *store.Job) {
	if j.Kind != "ci" {
		return
	}
	var p JobPayload
	if json.Unmarshal(j.Spec, &p) != nil || p.Context == "" {
		return
	}
	st := &store.CommitStatus{SHA: run.SHA, Context: p.Context, TargetURL: s.Cfg.HTTP.BaseURL + JobURL(j.ID)}
	switch j.Status {
	case store.JobWaiting, store.JobQueued:
		st.State, st.Description = "pending", "Waiting to run"
	case store.JobRunning:
		st.State, st.Description = "pending", "Running on "+j.RunnerName
	case store.JobSuccess:
		st.State, st.Description = "success", "Succeeded in "+j.Duration().String()
	case store.JobSkipped:
		st.State, st.Description = "skipped", j.Message
	case store.JobCancelled:
		st.State, st.Description = "error", "Cancelled"
	default:
		st.State, st.Description = "failure", firstNonEmpty(j.Message, "Failed")
	}
	st.Description = truncate(st.Description, 140)
	id := j.ID
	if err := s.Store.SetCommitStatus(ctx, st, &id); err != nil {
		s.Log.Warn("set commit status", "err", err)
	}
}

// ---- janitor ----

// runnerSilence is how long a running job may go without a heartbeat before
// it is failed (runners report at least every 10 seconds).
const runnerSilence = 2 * time.Minute

// Janitor fails jobs whose runner disappeared.
func (s *Service) Janitor(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ids, err := s.Store.StaleJobs(ctx, runnerSilence)
		if err != nil {
			continue
		}
		for _, id := range ids {
			if err := s.FinishJob(ctx, id, store.JobFailure, "the runner stopped responding"); err != nil && !errors.Is(err, store.ErrNotFound) {
				s.Log.Warn("fail stale job", "job", id, "err", err)
			}
		}
	}
}

// ---- helpers ----

func JobURL(id int64) string { return "/actions/jobs/" + strconv.FormatInt(id, 10) }
func RunURL(id int64) string { return "/actions/runs/" + strconv.FormatInt(id, 10) }

func merge(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// Rerun starts a pipeline run again: same file, commit and event, with the
// pipeline definition read from that commit.
func (s *Service) Rerun(ctx context.Context, run *store.Run, by *int64) (*store.Run, error) {
	pipes, errs := s.LoadPipelines(ctx, run.SHA)
	p, ok := pipes[run.File]
	if !ok {
		if len(errs) > 0 {
			return nil, errs[0]
		}
		return nil, fmt.Errorf("%s no longer exists at %s", run.File, run.SHA[:10])
	}
	ev := Event{Name: run.Event, Ref: run.Ref, Changed: run.ChangedFiles, ChangedKnown: true}
	return s.startPipeline(ctx, p, run.File, ev, run.SHA, run.BeforeSHA, run.PullID, by)
}
