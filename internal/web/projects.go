package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"path"
	"strconv"
	"strings"

	"onegit/internal/deploy"
	"onegit/internal/projects"
	"onegit/internal/store"
)

// projectDeploy is what runs where for one project: the latest successful
// deployment of each of its targets.
type projectDeploy struct {
	*store.TargetDeploy
	Label string // the target without the project dimension, e.g. "production"
}

type projectRow struct {
	projects.Project
	Owners    []string
	OpenPulls int
	LastRun   *store.Run
	Deploys   []projectDeploy
}

// projectDeploys groups the latest deployments by project.
func (w *Web) projectDeploys(ctx context.Context, st projects.Settings) map[string][]projectDeploy {
	out := map[string][]projectDeploy{}
	if st.Dimension == "" {
		return out
	}
	latest, err := w.Store.LatestDeploys(ctx)
	if err != nil {
		w.Log.Warn("latest deploys", "err", err)
		return out
	}
	dims, _ := w.Store.ListDeployDimensions(ctx)
	var others []*store.DeployDimension
	for _, d := range dims {
		if d.Name != st.Dimension {
			others = append(others, d)
		}
	}
	for _, d := range latest {
		name, ok := d.Target[st.Dimension]
		if !ok {
			continue
		}
		label := deploy.Target(d.Target).Label(others)
		if label == "" {
			label = d.TargetKey
		}
		out[name] = append(out[name], projectDeploy{TargetDeploy: d, Label: label})
	}
	return out
}

const projectsPerPage = 50

// projectRows fills in owners, open PRs, the default branch's last CI run
// and deployments for a page of projects, with a fixed number of queries
// however many projects there are.
func (w *Web) projectRows(ctx context.Context, list []projects.Project, sha, ref string, deploys map[string][]projectDeploy) []projectRow {
	dirs := make([]string, len(list))
	for i, p := range list {
		dirs[i] = p.Dir
	}
	var owners projects.OwnerRules
	if sha != "" {
		owners = w.Projects.OwnersAt(ctx, sha)
	}
	pulls, err := w.Store.OpenPullCountsIn(ctx, dirs)
	if err != nil {
		w.Log.Warn("open pulls per project", "err", err)
	}
	runs, err := w.Store.LastRunsIn(ctx, "pipeline", ref, dirs)
	if err != nil {
		w.Log.Warn("last runs per project", "err", err)
	}
	rows := make([]projectRow, len(list))
	for i, p := range list {
		rows[i] = projectRow{Project: p, Deploys: deploys[p.Name], Owners: owners.Of(p.Dir, true),
			OpenPulls: pulls[p.Dir], LastRun: runs[p.Dir]}
	}
	return rows
}

func (w *Web) projectList(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, st, err := w.Projects.List(ctx)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	branch := w.defaultBranch(ctx)
	sha, _ := w.Repo.ResolveCommit(ctx, "refs/heads/"+branch)
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	var found []projects.Project
	for _, p := range list {
		if q == "" || strings.Contains(strings.ToLower(p.Name), q) {
			found = append(found, p)
		}
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	from := min((page-1)*projectsPerPage, len(found))
	to := min(from+projectsPerPage, len(found))
	rows := w.projectRows(ctx, found[from:to], sha, "refs/heads/"+branch, w.projectDeploys(ctx, st))
	w.render(rw, r, http.StatusOK, "projects", &Page{Title: "Projects · " + w.Cfg.Repo.Name, Tab: "projects", Data: map[string]any{
		"Rows": rows, "Settings": st, "Configured": st.Pattern != "", "Q": q, "Total": len(list),
		"Page": page, "HasNext": to < len(found),
	}})
}

func (w *Web) projectView(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, st, err := w.Projects.Get(ctx, r.PathValue("name"))
	if errors.Is(err, projects.ErrNotFound) {
		w.notFound(rw, r)
		return
	}
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	branch := w.defaultBranch(ctx)
	ref := "refs/heads/" + branch
	sha, _ := w.Repo.ResolveCommit(ctx, ref)
	row := w.projectRows(ctx, []projects.Project{*p}, sha, ref, w.projectDeploys(ctx, st))[0]
	runs, err := w.Store.ListRuns(ctx, store.RunFilter{Kind: "pipeline", Dir: p.Dir, Limit: 15})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	views := make([]runView, len(runs))
	for i, run := range runs {
		views[i].Run = run
		views[i].Jobs, _ = w.Store.JobsForRun(ctx, run.ID)
	}
	pullList, err := w.Store.ListPullsIn(ctx, "open", p.Dir, 20, 0)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	var readme template.HTML
	if sha != "" {
		if entries, err := w.Repo.ListTree(ctx, sha, p.Dir); err == nil {
			for _, e := range entries {
				if !e.IsDir() && isReadme(e.Name) && isMarkdown(e.Name) && e.Size < maxRenderSize {
					if b, err := w.Repo.ReadBlob(ctx, sha, path.Join(p.Dir, e.Name), maxRenderSize); err == nil {
						readme = renderMarkdown(b)
					}
					break
				}
			}
		}
	}
	var deployQuery string
	if st.Dimension != "" {
		deployQuery = st.Dimension + "=" + p.Name
	}
	w.render(rw, r, http.StatusOK, "project", &Page{Title: p.Name + " · Projects", Tab: "projects", Data: map[string]any{
		"P": row, "Runs": views, "Pulls": pullList, "Readme": readme, "Branch": branch, "Settings": st,
		"DeployQuery": deployQuery,
	}})
}

// ---- admin ----

func (w *Web) adminProjects(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, explicit, ok, err := w.Projects.Settings(ctx)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	dims, err := w.Store.ListDeployDimensions(ctx)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	list, _, _ := w.Projects.List(ctx)
	w.render(rw, r, http.StatusOK, "admin_projects", &Page{Title: "Projects", Tab: "admin", Data: map[string]any{
		"Settings": st, "Explicit": explicit, "Enabled": ok, "Dims": dims, "Projects": list,
	}})
}

func (w *Web) adminSaveProjects(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var st *projects.Settings
	msg := "Projects now follow the deploy dimension."
	switch r.FormValue("action") {
	case "default":
	case "disable":
		st, msg = &projects.Settings{}, "Projects are turned off."
	default:
		st = &projects.Settings{Pattern: strings.TrimSpace(r.FormValue("pattern")), Dimension: r.FormValue("dimension")}
		msg = "Project settings saved."
		if st.Pattern == "" {
			w.redirectFlash(rw, r, "/admin/projects", "Enter a pattern such as services/*.")
			return
		}
	}
	if err := w.Projects.SaveSettings(ctx, st); err != nil {
		w.redirectFlash(rw, r, "/admin/projects", "Not saved: "+err.Error())
		return
	}
	u := currentUser(r)
	w.Store.Audit(ctx, &u.ID, "projects.settings", "", st)
	w.redirectFlash(rw, r, "/admin/projects", msg)
}

// projectDir resolves ?project= for list filters; "" when absent or unknown.
func (w *Web) projectDir(r *http.Request) (name, dir string) {
	name = r.URL.Query().Get("project")
	if name == "" {
		return "", ""
	}
	p, _, err := w.Projects.Get(r.Context(), name)
	if err != nil {
		return "", ""
	}
	return p.Name, p.Dir
}
