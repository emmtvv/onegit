package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"onegit/internal/deploy"
	"onegit/internal/git"
	"onegit/internal/projects"
	"onegit/internal/pulls"
	"onegit/internal/store"
)

// projectDeploy is what runs where for one project: the latest successful
// deployment of each of its targets.
type projectDeploy struct {
	*store.TargetDeploy
	Label string // the target without the project dimension, e.g. "production"
	Drift drift
}

// drift is how far a deployment is behind the default branch for one
// project: the commits under the project's directory that it lacks.
type drift struct {
	Commits int       // up to driftLimit
	Oldest  time.Time // author date of the oldest missing commit
	Known   bool      // false when it could not be worked out
}

// driftLimit caps the commits counted: past it, the number stops mattering.
const driftLimit = 100

// Drift turns bad after this many missing commits or this long.
const (
	driftBadCommits = 10
	driftBadAge     = 7 * 24 * time.Hour
)

// Level is "" (up to date or unknown), "warn" or "bad".
func (d drift) Level() string {
	switch {
	case !d.Known || d.Commits == 0:
		return ""
	case d.Commits >= driftBadCommits || time.Since(d.Oldest) >= driftBadAge:
		return "bad"
	}
	return "warn"
}

// Count is the number of missing commits for display, "100+" past the cap.
func (d drift) Count() string {
	if d.Commits >= driftLimit {
		return strconv.Itoa(driftLimit) + "+"
	}
	return strconv.Itoa(d.Commits)
}

type projectRow struct {
	projects.Project
	Owners    []string
	OpenPulls int
	LastRun   *store.Run
	Deploys   []projectDeploy
	// Cells are Deploys by environment column, nil where nothing is deployed.
	Cells []*projectDeploy
}

// Attention: the default branch's CI fails or a deployment lags far behind.
func (r *projectRow) Attention() bool {
	if r.LastRun != nil && r.LastRun.Status == store.JobFailure {
		return true
	}
	for _, d := range r.Deploys {
		if d.Drift.Level() == "bad" {
			return true
		}
	}
	return false
}

// projectDeploys groups the latest deployments by project. others are the
// deploy dimensions besides the project one, which the labels are made of.
func (w *Web) projectDeploys(ctx context.Context, st projects.Settings) (map[string][]projectDeploy, []*store.DeployDimension) {
	out := map[string][]projectDeploy{}
	if st.Dimension == "" {
		return out, nil
	}
	latest, err := w.Store.LatestDeploys(ctx)
	if err != nil {
		w.Log.Warn("latest deploys", "err", err)
		return out, nil
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
	return out, others
}

// envColumns orders the environments of the rows' deployments: the listed
// values of the other dimensions in their order (staging before
// production), then any other labels by name.
func envColumns(rows []projectRow, others []*store.DeployDimension) []string {
	have := map[string]bool{}
	for _, r := range rows {
		for _, d := range r.Deploys {
			have[d.Label] = true
		}
	}
	labels := []string{""}
	for _, d := range others {
		if d.Source != "list" {
			labels = nil
			break
		}
		var next []string
		for _, l := range labels {
			for _, v := range d.Values {
				next = append(next, strings.TrimPrefix(l+" / "+v, " / "))
			}
		}
		labels = next
	}
	var out []string
	for _, l := range labels {
		if have[l] {
			out = append(out, l)
			delete(have, l)
		}
	}
	rest := make([]string, 0, len(have))
	for l := range have {
		rest = append(rest, l)
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// setCells lays each row's deployments out by environment column.
func setCells(rows []projectRow, envs []string) {
	for i := range rows {
		rows[i].Cells = make([]*projectDeploy, len(envs))
		for j := range rows[i].Deploys {
			if k := slices.Index(envs, rows[i].Deploys[j].Label); k >= 0 {
				rows[i].Cells[k] = &rows[i].Deploys[j]
			}
		}
	}
}

// driftWorkers bounds the git processes one page starts for drift.
const driftWorkers = 4

// fillDrift works out how far each deployment is behind head for its
// project's directory. Results are cached: a (deployed, head, dir) triple
// always has the same answer.
func (w *Web) fillDrift(ctx context.Context, head string, rows []projectRow) {
	if head == "" {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, driftWorkers)
	for i := range rows {
		for j := range rows[i].Deploys {
			d, dir := &rows[i].Deploys[j], rows[i].Dir
			if d.SHA == head {
				d.Drift = drift{Known: true}
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				d.Drift = w.driftOf(ctx, d.SHA, head, dir)
			}()
		}
	}
	wg.Wait()
}

func (w *Web) driftOf(ctx context.Context, sha, head, dir string) drift {
	key := "drift:" + sha + ":" + head + ":" + dir
	var d drift
	if ok, err := w.KV.GetJSON(ctx, key, &d); ok && err == nil {
		return d
	}
	commits, err := w.Repo.Log(ctx, sha+".."+head, git.LogOptions{Path: dir, Limit: driftLimit})
	if err != nil {
		return drift{} // e.g. the deployed commit is gone
	}
	d = drift{Commits: len(commits), Known: true}
	if len(commits) > 0 {
		d.Oldest = commits[len(commits)-1].Author.When
	}
	if err := w.KV.SetJSON(ctx, key, d, 24*time.Hour); err != nil {
		w.Log.Warn("cache drift", "err", err)
	}
	return d
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
		// A copy: fillDrift writes into the deployments.
		rows[i] = projectRow{Project: p, Deploys: slices.Clone(deploys[p.Name]), Owners: owners.Of(p.Dir, true),
			OpenPulls: pulls[p.Dir], LastRun: runs[p.Dir]}
	}
	w.fillDrift(ctx, sha, rows)
	return rows
}

// ownedBy keeps the projects whose code owners include u.
func (w *Web) ownedBy(ctx context.Context, u *store.User, list []projects.Project, sha string) []projects.Project {
	if u == nil || sha == "" {
		return nil
	}
	teams, err := w.Store.UserTeams(ctx, u.ID)
	if err != nil {
		w.Log.Warn("user teams", "err", err)
	}
	owners := w.Projects.OwnersAt(ctx, sha)
	var out []projects.Project
	for _, p := range list {
		if pulls.OwnerListIncludes(owners.Of(p.Dir, true), u, teams) {
			out = append(out, p)
		}
	}
	return out
}

// Project list filters.
const (
	filterMine      = "mine"
	filterAttention = "attention"
)

func (w *Web) projectList(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, st, err := w.Projects.List(ctx)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	branch := w.defaultBranch(ctx)
	ref := "refs/heads/" + branch
	sha, _ := w.Repo.ResolveCommit(ctx, ref)
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	filter := r.URL.Query().Get("filter")
	var found []projects.Project
	for _, p := range list {
		if q == "" || strings.Contains(strings.ToLower(p.Name), q) {
			found = append(found, p)
		}
	}
	if filter == filterMine {
		found = w.ownedBy(ctx, currentUser(r), found, sha)
	}
	deploys, others := w.projectDeploys(ctx, st)
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	var rows []projectRow
	var hasNext bool
	if filter == filterAttention {
		// Needs every row's CI and drift before it can page.
		for _, row := range w.projectRows(ctx, found, sha, ref, deploys) {
			if row.Attention() {
				rows = append(rows, row)
			}
		}
		from := min((page-1)*projectsPerPage, len(rows))
		to := min(from+projectsPerPage, len(rows))
		rows, hasNext = rows[from:to], to < len(rows)
	} else {
		from := min((page-1)*projectsPerPage, len(found))
		to := min(from+projectsPerPage, len(found))
		rows, hasNext = w.projectRows(ctx, found[from:to], sha, ref, deploys), to < len(found)
	}
	envs := envColumns(rows, others)
	setCells(rows, envs)
	var deployLink string
	if u := currentUser(r); u.CanWrite() && st.Dimension != "" {
		deployLink = "/deploy/new?" + st.Dimension + "="
	}
	w.render(rw, r, http.StatusOK, "projects", &Page{Title: "Projects · " + w.Cfg.Repo.Name, Tab: "projects", Data: map[string]any{
		"Rows": rows, "Settings": st, "Configured": st.Pattern != "", "Q": q, "Total": len(list),
		"Page": page, "HasNext": hasNext, "Envs": envs, "Filter": filter, "Branch": branch, "DeployLink": deployLink,
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
	deploys, _ := w.projectDeploys(ctx, st)
	row := w.projectRows(ctx, []projects.Project{*p}, sha, ref, deploys)[0]
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
