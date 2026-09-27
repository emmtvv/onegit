package web

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"onegit/internal/deploy"
	"onegit/internal/store"
)

// deployCell is one target in the overview.
type deployCell struct {
	Target deploy.Target
	Key    string
	Last   *store.TargetDeploy
}

func (w *Web) deployOverview(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dims, err := w.Store.ListDeployDimensions(ctx)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	latest, err := w.Store.LatestDeploys(ctx)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	byKey := map[string]*store.TargetDeploy{}
	for _, d := range latest {
		byKey[d.TargetKey] = d
	}
	data := map[string]any{"Dims": dims}

	// Two dimensions (e.g. project × environment): a matrix. Otherwise a list
	// of targets that have been deployed.
	if len(dims) == 2 {
		values := w.Deploy.DimensionValues(ctx, dims, w.recipeSHA(r))
		rows, cols := values[dims[0].Name], values[dims[1].Name]
		type row struct {
			Value string
			Cells []deployCell
		}
		var matrix []row
		for _, rv := range rows {
			rr := row{Value: rv}
			for _, cv := range cols {
				t := deploy.Target{dims[0].Name: rv, dims[1].Name: cv}
				rr.Cells = append(rr.Cells, deployCell{Target: t, Key: t.Key(), Last: byKey[t.Key()]})
			}
			matrix = append(matrix, rr)
		}
		data["Matrix"], data["Cols"] = matrix, cols
	} else {
		var cells []deployCell
		for _, d := range latest {
			cells = append(cells, deployCell{Target: d.Target, Key: d.TargetKey, Last: d})
		}
		sort.Slice(cells, func(i, j int) bool { return cells[i].Target.Label(dims) < cells[j].Target.Label(dims) })
		data["Cells"] = cells
	}
	pending, err := w.Store.ListDeployments(ctx, "pending", 50, 0)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	recent, err := w.Store.ListDeployments(ctx, "", 30, 0)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	data["Pending"], data["Recent"] = w.deploymentViews(dims, pending), w.deploymentViews(dims, recent)
	w.render(rw, r, http.StatusOK, "deploy", &Page{Title: "Deployments", Tab: "deploy", Data: data})
}

type deploymentView struct {
	*store.Deployment
	Labels []string
}

func (w *Web) deploymentViews(dims []*store.DeployDimension, list []*store.Deployment) []deploymentView {
	out := make([]deploymentView, len(list))
	for i, d := range list {
		out[i].Deployment = d
		for _, t := range d.Targets {
			out[i].Labels = append(out[i].Labels, deploy.Target(t).Label(dims))
		}
	}
	return out
}

func (w *Web) recipeSHA(r *http.Request) string {
	sha, _ := w.Repo.ResolveCommit(r.Context(), "refs/heads/"+w.defaultBranch(r.Context()))
	return sha
}

// targetsFromForm builds the cartesian product of the values chosen for
// every dimension.
func targetsFromForm(dims []*store.DeployDimension, form url.Values) ([]deploy.Target, error) {
	targets := []deploy.Target{{}}
	for _, d := range dims {
		vals := form[d.Name]
		if len(vals) == 0 {
			return nil, errors.New("choose at least one " + d.Name)
		}
		var next []deploy.Target
		for _, t := range targets {
			for _, v := range vals {
				n := deploy.Target{}
				for k, x := range t {
					n[k] = x
				}
				n[d.Name] = v
				next = append(next, n)
			}
		}
		targets = next
	}
	if len(targets) > 200 {
		return nil, errors.New("too many targets at once (max 200)")
	}
	return targets, nil
}

func (w *Web) deployNew(rw http.ResponseWriter, r *http.Request) {
	w.renderDeployForm(rw, r, r.URL.Query(), r.URL.Query().Get("check") != "", "")
}

func (w *Web) renderDeployForm(rw http.ResponseWriter, r *http.Request, form url.Values, check bool, formErr string) {
	ctx := r.Context()
	dims, err := w.Store.ListDeployDimensions(ctx)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	ref := strings.TrimSpace(form.Get("ref"))
	if ref == "" {
		ref = w.defaultBranch(ctx)
	}
	data := map[string]any{
		"Dims": dims, "Values": w.Deploy.DimensionValues(ctx, dims, w.recipeSHA(r)),
		"Selected": form, "Ref": ref, "Comment": form.Get("comment"),
	}
	if check {
		targets, err := targetsFromForm(dims, form)
		if err == nil {
			var ev *deploy.Evaluation
			ev, err = w.Deploy.Plan(ctx, currentUser(r), ref, targets)
			data["Eval"] = ev
		}
		if err != nil && formErr == "" {
			formErr = err.Error()
		}
	}
	status := http.StatusOK
	if formErr != "" {
		status = http.StatusUnprocessableEntity
	}
	w.render(rw, r, status, "deploy_new", &Page{Title: "New deployment", Tab: "deploy", Error: formErr, Data: data})
}

func (w *Web) deployCreate(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		w.errorPage(rw, r, http.StatusBadRequest, "Invalid form.")
		return
	}
	dims, err := w.Store.ListDeployDimensions(ctx)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	targets, err := targetsFromForm(dims, r.PostForm)
	if err != nil {
		w.renderDeployForm(rw, r, r.PostForm, false, err.Error())
		return
	}
	d, _, err := w.Deploy.Request(ctx, currentUser(r), r.PostForm.Get("ref"), targets, r.PostForm.Get("comment"))
	var ue *deploy.UserError
	if errors.As(err, &ue) {
		// Show the evaluation so the user sees exactly what is missing.
		w.renderDeployForm(rw, r, r.PostForm, true, "The deployment was not requested: fix the failed checks below.")
		return
	}
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	msg := "Deployment requested; it is waiting for approval."
	if d.Status == "running" {
		msg = "Deployment started."
	}
	w.redirectFlash(rw, r, deployURL(d.ID), msg)
}

func deployURL(id int64) string { return "/deploy/" + strconv.FormatInt(id, 10) }

func (w *Web) deployView(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	d, err := w.Store.DeploymentByID(ctx, id)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	ev, err := w.Deploy.Current(ctx, d)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	reviews, err := w.Store.DeploymentApprovals(ctx, id)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	var jobs []*store.Job
	if d.RunID != nil {
		jobs, _ = w.Store.JobsForRun(ctx, *d.RunID)
	}
	u := currentUser(r)
	commit, _ := w.Repo.GetCommit(ctx, d.SHA)
	w.render(rw, r, http.StatusOK, "deploy_view", &Page{Title: "Deployment #" + strconv.FormatInt(id, 10), Tab: "deploy", Data: map[string]any{
		"D": d, "Eval": ev, "Reviews": reviews, "Jobs": jobs, "Commit": commit,
		"CanApprove": w.Deploy.CanApprove(ctx, u, d, ev),
		"CanCancel":  u != nil && (u.IsAdmin() || (d.RequestedBy != nil && *d.RequestedBy == u.ID)) && (d.Status == "pending" || d.Status == "running"),
		"Redeploy":   redeployURL(d),
	}})
}

// redeployURL pre-fills the form with the same commit and targets (rollback
// = redeploying an older deployment).
func redeployURL(d *store.Deployment) string {
	q := url.Values{"ref": {d.SHA}, "check": {"1"}}
	seen := map[string]bool{}
	for _, t := range d.Targets {
		for k, v := range t {
			if !seen[k+"="+v] {
				q.Add(k, v)
				seen[k+"="+v] = true
			}
		}
	}
	return "/deploy/new?" + q.Encode()
}

func (w *Web) deployReview(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	approve := r.FormValue("verdict") == "approve"
	d, err := w.Deploy.Review(r.Context(), currentUser(r), id, approve, r.FormValue("comment"))
	var ue *deploy.UserError
	switch {
	case errors.As(err, &ue):
		w.redirectFlash(rw, r, deployURL(id), ue.Msg)
		return
	case err != nil:
		w.fail(rw, r, err)
		return
	}
	msg := "Approved."
	switch {
	case !approve:
		msg = "Rejected."
	case d.Status == "running":
		msg = "Approved; the deployment has started."
	}
	w.redirectFlash(rw, r, deployURL(id), msg)
}

func (w *Web) deployCancel(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	err := w.Deploy.Cancel(r.Context(), currentUser(r), id)
	var ue *deploy.UserError
	switch {
	case errors.As(err, &ue):
		w.redirectFlash(rw, r, deployURL(id), ue.Msg)
		return
	case err != nil:
		w.fail(rw, r, err)
		return
	}
	w.redirectFlash(rw, r, deployURL(id), "Cancelled.")
}

func (w *Web) deployTarget(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	key := r.URL.Query().Get("key")
	dims, _ := w.Store.ListDeployDimensions(ctx)
	history, err := w.Store.TargetHistory(ctx, key, 100)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	t := deploy.ParseTargetKey(key)
	q := url.Values{}
	for k, v := range t {
		q.Set(k, v)
	}
	w.render(rw, r, http.StatusOK, "deploy_target", &Page{Title: t.Label(dims) + " · Deployments", Tab: "deploy", Data: map[string]any{
		"Label": t.Label(dims), "Target": t, "History": history, "Query": q.Encode(),
	}})
}
