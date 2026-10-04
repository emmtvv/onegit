package web

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"onegit/internal/ci"
	"onegit/internal/store"
)

const runsPerPage = 30

type runView struct {
	*store.Run
	Jobs []*store.Job
}

func (w *Web) actions(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	kind := r.URL.Query().Get("kind")
	if kind != "deploy" {
		kind = "pipeline"
	}
	project, dir := w.projectDir(r)
	runs, err := w.Store.ListRuns(ctx, store.RunFilter{Kind: kind, Dir: dir, Limit: runsPerPage + 1, Offset: (page - 1) * runsPerPage})
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	hasNext := len(runs) > runsPerPage
	runs = runs[:min(len(runs), runsPerPage)]
	views := make([]runView, len(runs))
	for i, run := range runs {
		views[i].Run = run
		views[i].Jobs, _ = w.Store.JobsForRun(ctx, run.ID)
	}
	// Pipelines that can be started by hand, from the default branch.
	var manual []string
	branch := w.defaultBranch(ctx)
	if sha, err := w.Repo.ResolveCommit(ctx, "refs/heads/"+branch); err == nil {
		pipes, _ := w.CI.LoadPipelines(ctx, sha)
		for f, p := range pipes {
			if p.On.Manual {
				manual = append(manual, f)
			}
		}
		sort.Strings(manual)
	}
	var schedules []ci.Schedule
	if kind == "pipeline" && page == 1 && project == "" {
		schedules = w.CI.Schedules(ctx)
	}
	projectList, _, _ := w.Projects.List(ctx)
	w.render(rw, r, http.StatusOK, "actions", &Page{Title: "Actions", Tab: "actions", Data: map[string]any{
		"Runs": views, "Page": page, "HasNext": hasNext, "Kind": kind, "Manual": manual, "Branch": branch,
		"CanWrite": currentUser(r).CanWrite(), "Schedules": schedules, "Project": project, "Projects": projectList,
	}})
}

func (w *Web) runPage(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run, err := w.Store.RunByID(r.Context(), id)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	jobs, err := w.Store.JobsForRun(r.Context(), id)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	artifacts, err := w.Store.ArtifactsForRun(r.Context(), id)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.render(rw, r, http.StatusOK, "run", &Page{Title: run.Name + " #" + strconv.FormatInt(run.ID, 10), Tab: "actions", Data: map[string]any{
		"Run": run, "Jobs": jobs, "CanWrite": currentUser(r).CanWrite(), "Artifacts": artifacts,
	}})
}

func (w *Web) jobPage(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	job, err := w.Store.JobByID(r.Context(), id)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	run, err := w.Store.RunByID(r.Context(), job.RunID)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	jobs, _ := w.Store.JobsForRun(r.Context(), run.ID)
	var spec ci.JobPayload
	_ = json.Unmarshal(job.Spec, &spec) // an unreadable spec just shows no steps
	var artifact *store.Artifact
	if arts, err := w.Store.ArtifactsForRun(r.Context(), run.ID); err == nil {
		for _, a := range arts {
			if a.JobID == job.ID {
				artifact = a
			}
		}
	}
	w.render(rw, r, http.StatusOK, "job", &Page{Title: job.Name + " · " + run.Name, Tab: "actions", Data: map[string]any{
		"Run": run, "Job": job, "Jobs": jobs, "Spec": spec, "CanWrite": currentUser(r).CanWrite(), "Artifact": artifact,
	}})
}

// jobLog returns log chunks after ?after= plus the job state, for the live
// log view.
func (w *Web) jobLog(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	after, err := strconv.Atoi(r.URL.Query().Get("after"))
	if err != nil {
		after = -1
	}
	job, err := w.Store.JobByID(r.Context(), id)
	if err != nil {
		http.Error(rw, "not found", http.StatusNotFound)
		return
	}
	chunks, err := w.Store.LogChunks(r.Context(), id, after)
	if err != nil {
		http.Error(rw, "internal error", http.StatusInternalServerError)
		return
	}
	if chunks == nil {
		chunks = []store.LogChunk{}
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(rw).Encode(map[string]any{
		"chunks": chunks, "status": job.Status, "steps": job.Steps, "message": job.Message,
		"done": store.JobTerminal(job.Status), "duration": job.Duration().String(),
	})
}

// jobRawLog serves the whole log as text.
func (w *Web) jobRawLog(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	chunks, err := w.Store.LogChunks(r.Context(), id, -1)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.Header().Set("Content-Security-Policy", "sandbox")
	for _, c := range chunks {
		rw.Write([]byte(c.Data))
	}
}

func (w *Web) requireCIWriter(rw http.ResponseWriter, r *http.Request) bool {
	if !currentUser(r).CanWrite() {
		w.errorPage(rw, r, http.StatusForbidden, "You need write access to do this.")
		return false
	}
	return true
}

func (w *Web) cancelRun(rw http.ResponseWriter, r *http.Request) {
	if !w.requireCIWriter(rw, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run, err := w.Store.RunByID(r.Context(), id)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	if run.DeploymentID != nil {
		// Deployments are cancelled through their own page (requester/admin only).
		http.Redirect(rw, r, "/deploy/"+strconv.FormatInt(*run.DeploymentID, 10), http.StatusSeeOther)
		return
	}
	if err := w.CI.Cancel(r.Context(), id); err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.redirectFlash(rw, r, ci.RunURL(id), "Cancelling the run.")
}

// rerunPipeline starts the same pipeline again for the same commit and event.
func (w *Web) rerunPipeline(rw http.ResponseWriter, r *http.Request) {
	if !w.requireCIWriter(rw, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run, err := w.Store.RunByID(r.Context(), id)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	if run.Kind != "pipeline" {
		w.errorPage(rw, r, http.StatusBadRequest, "Deployments are re-run by requesting a new deployment.")
		return
	}
	u := currentUser(r)
	next, err := w.CI.Rerun(r.Context(), run, &u.ID)
	if err != nil {
		w.redirectFlash(rw, r, ci.RunURL(id), "Could not re-run: "+err.Error())
		return
	}
	http.Redirect(rw, r, ci.RunURL(next.ID), http.StatusSeeOther)
}

func (w *Web) runManual(rw http.ResponseWriter, r *http.Request) {
	if !w.requireCIWriter(rw, r) {
		return
	}
	file, branch := r.FormValue("file"), strings.TrimSpace(r.FormValue("branch"))
	run, err := w.CI.RunManual(r.Context(), currentUser(r), file, branch)
	if err != nil {
		w.redirectFlash(rw, r, "/actions", err.Error())
		return
	}
	http.Redirect(rw, r, ci.RunURL(run.ID), http.StatusSeeOther)
}

// artifactDownload serves a job's artifacts as tar.gz.
func (w *Web) artifactDownload(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, err := w.Store.ArtifactByID(r.Context(), id)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	if a.Expired() || w.CI.Blob == nil {
		w.errorPage(rw, r, http.StatusGone, "These artifacts have expired.")
		return
	}
	rc, size, err := w.CI.Blob.Get(r.Context(), a.BlobKey)
	if err != nil {
		w.errorPage(rw, r, http.StatusGone, "These artifacts are no longer stored.")
		return
	}
	defer rc.Close()
	name := strings.Map(func(c rune) rune {
		if c == '/' || c == '\\' || c == '"' || c < ' ' {
			return '_'
		}
		return c
	}, a.JobName)
	h := rw.Header()
	h.Set("Content-Type", "application/gzip")
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + "-artifacts.tar.gz"}))
	if _, err := io.Copy(rw, rc); err != nil && r.Context().Err() == nil {
		w.Log.Warn("send artifacts", "id", id, "err", err)
	}
}
