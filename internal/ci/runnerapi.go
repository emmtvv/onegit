package ci

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"onegit/internal/auth"
	"onegit/internal/store"
)

// Runner protocol (JSON over HTTP, all POST):
//
//	/api/runner/v1/fetch              runner token; long-polls for a job
//	/api/runner/v1/jobs/{id}/log      job token; appends output, returns {cancel}
//	/api/runner/v1/jobs/{id}/steps    job token; step states
//	/api/runner/v1/jobs/{id}/finish   job token; final status
//
// Runner tokens start with RunnerTokenPrefix, job tokens with
// auth.JobTokenPrefix; the job token also authenticates git fetches and
// registry pushes while the job runs.
const (
	RunnerTokenPrefix = "ogrun_"
	fetchWait         = 25 * time.Second
	maxLogChunk       = 1 << 20
)

type FetchRequest struct {
	Version string `json:"version"`
}

// Assignment is a job handed to a runner.
type Assignment struct {
	ID           int64             `json:"id"`
	RunID        int64             `json:"run_id"`
	Name         string            `json:"name"`
	Kind         string            `json:"kind"`
	Token        string            `json:"token"`
	RepoURL      string            `json:"repo_url"`
	SHA          string            `json:"sha"`
	Ref          string            `json:"ref"`
	ToolingSHA   string            `json:"tooling_sha,omitempty"`
	Env          map[string]string `json:"env"`
	Masks        []string          `json:"masks,omitempty"`
	Steps        []Step            `json:"steps"`
	TimeoutSec   int               `json:"timeout"`
	ChangedFiles []string          `json:"changed_files,omitempty"`
}

type LogRequest struct {
	Seq  int    `json:"seq"` // -1: heartbeat only
	Data string `json:"data"`
}

type LogResponse struct {
	Cancel bool `json:"cancel"`
}

type FinishRequest struct {
	Status  string `json:"status"` // success, failure, cancelled
	Message string `json:"message"`
}

func (s *Service) RunnerAPI() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/runner/v1/fetch", s.fetch)
	mux.HandleFunc("POST /api/runner/v1/jobs/{id}/log", s.withJob(s.jobLog))
	mux.HandleFunc("POST /api/runner/v1/jobs/{id}/steps", s.withJob(s.jobSteps))
	mux.HandleFunc("POST /api/runner/v1/jobs/{id}/finish", s.withJob(s.jobFinish))
	return mux
}

func bearer(r *http.Request) string {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return tok
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Service) fetch(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r)
	if !strings.HasPrefix(tok, RunnerTokenPrefix) {
		http.Error(w, "runner token required", http.StatusUnauthorized)
		return
	}
	runner, err := s.Store.RunnerByTokenHash(r.Context(), auth.HashToken(tok))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "unknown runner token", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !runner.Enabled {
		http.Error(w, "runner is disabled", http.StatusForbidden)
		return
	}
	var req FetchRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // the body is optional
	s.Store.TouchRunner(r.Context(), runner.ID, req.Version)

	accept := func(j *store.Job) bool {
		return runner.Kind != "deploy" || SelectorMatches(runner.Targets, j.Target)
	}
	deadline := time.Now().Add(fetchWait)
	for {
		jobToken := auth.JobTokenPrefix + auth.RandomString(30)
		j, err := s.Store.ClaimJob(r.Context(), runner, auth.HashToken(jobToken), accept)
		if err != nil {
			if r.Context().Err() == nil {
				s.Log.Error("claim job", "runner", runner.Name, "err", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
			return
		}
		if j != nil {
			a, err := s.assignment(r.Context(), j, jobToken)
			if err != nil {
				s.Log.Error("prepare job", "job", j.ID, "err", err)
				if err := s.FinishJob(context.Background(), j.ID, store.JobFailure, "could not prepare the job: "+err.Error()); err != nil {
					s.Log.Error("fail job", "job", j.ID, "err", err)
				}
				continue
			}
			s.JobStarted(r.Context(), j)
			writeJSON(w, http.StatusOK, a)
			return
		}
		if time.Now().After(deadline) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *Service) assignment(ctx context.Context, j *store.Job, token string) (*Assignment, error) {
	run, err := s.Store.RunByID(ctx, j.RunID)
	if err != nil {
		return nil, err
	}
	var p JobPayload
	if err := json.Unmarshal(j.Spec, &p); err != nil {
		return nil, err
	}
	env := map[string]string{
		"CI":                "true",
		"ONEGIT":            "true",
		"ONEGIT_SERVER_URL": s.Cfg.HTTP.BaseURL,
		"ONEGIT_REPOSITORY": s.Cfg.Repo.Name,
		"ONEGIT_REGISTRY":   s.Cfg.RegistryHost(),
		"ONEGIT_EVENT":      run.Event,
		"ONEGIT_REF":        run.Ref,
		"ONEGIT_REF_NAME":   refName(run.Ref),
		"ONEGIT_SHA":        run.SHA,
		"ONEGIT_BEFORE_SHA": run.BeforeSHA,
		"ONEGIT_RUN_ID":     strconv.FormatInt(run.ID, 10),
		"ONEGIT_JOB_ID":     strconv.FormatInt(j.ID, 10),
		"ONEGIT_JOB":        j.Name,
		"ONEGIT_PIPELINE":   run.Name,
		"ONEGIT_ACTOR":      run.TriggeredByName,
	}
	if run.PullID != nil {
		env["ONEGIT_PULL_REQUEST"] = strconv.FormatInt(*run.PullID, 10)
	}
	a := &Assignment{
		ID: j.ID, RunID: run.ID, Name: j.Name, Kind: j.Kind, Token: token,
		RepoURL: s.Cfg.HTTPCloneURL(), SHA: run.SHA, Ref: run.Ref,
		Steps: p.Steps, TimeoutSec: p.TimeoutSec, ChangedFiles: run.ChangedFiles,
	}
	if j.Kind == "deploy" {
		if run.DeploymentID != nil {
			env["ONEGIT_DEPLOYMENT_ID"] = strconv.FormatInt(*run.DeploymentID, 10)
		}
		env["ONEGIT_RECIPE"] = p.RecipeFile
		env["ONEGIT_RECIPE_SHA"] = p.RecipeSHA
		for dim, v := range j.Target {
			env[strings.ToUpper(dim)] = v
			env["ONEGIT_TARGET_"+strings.ToUpper(dim)] = v
		}
		if p.Tooling {
			a.ToolingSHA = p.RecipeSHA
		}
	}
	for k, v := range p.Env {
		env[k] = v
	}
	if j.Kind == "deploy" && s.SecretsFor != nil {
		secrets, err := s.SecretsFor(ctx, j.Target)
		if err != nil {
			return nil, err
		}
		for k, v := range secrets {
			env[k] = v // secrets win over recipe env
			if len(v) >= 4 {
				a.Masks = append(a.Masks, v)
			}
		}
	}
	a.Env = env
	return a, nil
}

func refName(ref string) string {
	for _, p := range []string{"refs/heads/", "refs/tags/"} {
		if n, ok := strings.CutPrefix(ref, p); ok {
			return n
		}
	}
	return ref
}

// withJob authenticates a job endpoint with the job's own token.
func (s *Service) withJob(h func(http.ResponseWriter, *http.Request, *store.Job)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		tok := bearer(r)
		if !strings.HasPrefix(tok, auth.JobTokenPrefix) {
			http.Error(w, "job token required", http.StatusUnauthorized)
			return
		}
		j, err := s.Store.RunningJobByTokenHash(r.Context(), auth.HashToken(tok))
		if errors.Is(err, store.ErrNotFound) || (err == nil && j.ID != id) {
			// Finished or cancelled jobs lose their token: tell the runner to stop.
			writeJSON(w, http.StatusGone, LogResponse{Cancel: true})
			return
		}
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		h(w, r, j)
	}
}

func (s *Service) jobLog(w http.ResponseWriter, r *http.Request, j *store.Job) {
	var req LogRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLogChunk+4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Seq >= 0 && req.Data != "" {
		if err := s.Store.AppendLog(r.Context(), j.ID, req.Seq, req.Data); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	cancel, err := s.Store.Heartbeat(r.Context(), j.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, LogResponse{Cancel: cancel})
}

func (s *Service) jobSteps(w http.ResponseWriter, r *http.Request, j *store.Job) {
	var steps []store.StepState
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&steps); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.Store.SetJobSteps(r.Context(), j.ID, steps); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) jobFinish(w http.ResponseWriter, r *http.Request, j *store.Job) {
	var req FinishRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	switch req.Status {
	case store.JobSuccess, store.JobFailure, store.JobCancelled:
	default:
		http.Error(w, "bad status", http.StatusBadRequest)
		return
	}
	if err := s.FinishJob(r.Context(), j.ID, req.Status, truncate(req.Message, 500)); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SelectorMatches reports whether a target satisfies a selector: every
// dimension in the selector must match one of its globs.
func SelectorMatches(sel map[string][]string, target map[string]string) bool {
	for dim, pats := range sel {
		if len(pats) == 0 {
			continue
		}
		v, ok := target[dim]
		if !ok || !MatchAny(pats, v) {
			return false
		}
	}
	return true
}
