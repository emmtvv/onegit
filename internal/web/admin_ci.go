package web

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"onegit/internal/auth"
	"onegit/internal/ci"
	"onegit/internal/store"
)

// ---- selector and principal text forms ----

// parseSelector reads "project=api,web; environment=production" (";" or
// newlines between dimensions).
func parseSelector(s string, dims []*store.DeployDimension) (store.Selector, error) {
	sel := store.Selector{}
	known := map[string]bool{}
	for _, d := range dims {
		known[d.Name] = true
	}
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '\n' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		dim, vals, ok := strings.Cut(part, "=")
		dim = strings.TrimSpace(dim)
		if !ok || dim == "" {
			return nil, fmt.Errorf("%q: use dimension=value,value", part)
		}
		if !known[dim] {
			return nil, fmt.Errorf("unknown dimension %q", dim)
		}
		for _, v := range strings.Split(vals, ",") {
			if v = strings.TrimSpace(v); v != "" {
				sel[dim] = append(sel[dim], v)
			}
		}
	}
	return sel, nil
}

func formatSelector(sel store.Selector) string {
	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+strings.Join(sel[k], ","))
	}
	return strings.Join(parts, "; ")
}

// parsePrincipals reads "team:sre, user:alice, role:admin, codeowners".
func parsePrincipals(s string) ([]store.Principal, error) {
	var out []store.Principal
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' }) {
		typ, val, _ := strings.Cut(strings.TrimSpace(f), ":")
		switch typ {
		case "user", "team":
			if val == "" {
				return nil, fmt.Errorf("%q needs a name", f)
			}
		case "role":
			if val != "read" && val != "write" && val != "admin" {
				return nil, fmt.Errorf("%q: role must be read, write or admin", f)
			}
		case "codeowners":
			val = ""
		default:
			return nil, fmt.Errorf("%q: use user:NAME, team:NAME, role:ROLE or codeowners", f)
		}
		out = append(out, store.Principal{Type: typ, Value: val})
	}
	return out, nil
}

func formatPrincipals(ps []store.Principal) string {
	var out []string
	for _, p := range ps {
		if p.Value == "" {
			out = append(out, p.Type)
		} else {
			out = append(out, p.Type+":"+p.Value)
		}
	}
	return strings.Join(out, ", ")
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func (w *Web) audit(r *http.Request, action, subject string, data any) {
	u := currentUser(r)
	if err := w.Store.Audit(r.Context(), &u.ID, action, subject, data); err != nil {
		w.Log.Warn("audit", "err", err)
	}
}

// ---- runners ----

func (w *Web) adminRunners(rw http.ResponseWriter, r *http.Request) {
	w.renderRunners(rw, r, "", "")
}

func (w *Web) renderRunners(rw http.ResponseWriter, r *http.Request, token, tokenFor string) {
	runners, err := w.Store.ListRunners(r.Context())
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.render(rw, r, http.StatusOK, "admin_runners", &Page{Title: "Runners · Admin", Tab: "admin", Data: map[string]any{
		"Runners": runners, "Token": token, "TokenFor": tokenFor, "URL": w.Cfg.HTTP.BaseURL,
	}})
}

var runnerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func (w *Web) adminCreateRunner(rw http.ResponseWriter, r *http.Request) {
	dims, _ := w.Store.ListDeployDimensions(r.Context())
	run := &store.Runner{Name: strings.TrimSpace(r.FormValue("name")), Kind: r.FormValue("kind"), Labels: splitList(r.FormValue("labels"))}
	if !runnerNameRe.MatchString(run.Name) {
		w.redirectFlash(rw, r, "/admin/runners", "Runner names use letters, digits, '.', '_' and '-'.")
		return
	}
	if run.Kind != "ci" && run.Kind != "deploy" {
		w.redirectFlash(rw, r, "/admin/runners", "Choose a runner kind.")
		return
	}
	sel, err := parseSelector(r.FormValue("targets"), dims)
	if err != nil {
		w.redirectFlash(rw, r, "/admin/runners", "Targets: "+err.Error())
		return
	}
	run.Targets = sel
	token := ci.RunnerTokenPrefix + auth.RandomString(30)
	if err := w.Store.CreateRunner(r.Context(), run, auth.HashToken(token)); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			w.redirectFlash(rw, r, "/admin/runners", "A runner named "+run.Name+" already exists.")
			return
		}
		w.serverError(rw, r, err)
		return
	}
	w.audit(r, "runner.create", run.Name, map[string]any{"kind": run.Kind, "labels": run.Labels, "targets": run.Targets})
	w.renderRunners(rw, r, token, run.Name)
}

func (w *Web) adminUpdateRunner(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run, err := w.Store.RunnerByID(r.Context(), id)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	dims, _ := w.Store.ListDeployDimensions(r.Context())
	sel, err := parseSelector(r.FormValue("targets"), dims)
	if err != nil {
		w.redirectFlash(rw, r, "/admin/runners", "Targets: "+err.Error())
		return
	}
	run.Labels, run.Targets, run.Enabled = splitList(r.FormValue("labels")), sel, r.FormValue("enabled") == "on"
	if err := w.Store.UpdateRunner(r.Context(), run); err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.audit(r, "runner.update", run.Name, map[string]any{"labels": run.Labels, "targets": run.Targets, "enabled": run.Enabled})
	w.redirectFlash(rw, r, "/admin/runners", "Runner "+run.Name+" saved.")
}

func (w *Web) adminRunnerToken(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run, err := w.Store.RunnerByID(r.Context(), id)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	token := ci.RunnerTokenPrefix + auth.RandomString(30)
	if err := w.Store.SetRunnerToken(r.Context(), id, auth.HashToken(token)); err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.audit(r, "runner.token", run.Name, nil)
	w.renderRunners(rw, r, token, run.Name)
}

func (w *Web) adminDeleteRunner(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run, err := w.Store.RunnerByID(r.Context(), id)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	if err := w.Store.DeleteRunner(r.Context(), id); err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.audit(r, "runner.delete", run.Name, nil)
	w.redirectFlash(rw, r, "/admin/runners", "Runner "+run.Name+" deleted.")
}

// ---- teams ----

func (w *Web) adminTeams(rw http.ResponseWriter, r *http.Request) {
	teams, err := w.Store.ListTeams(r.Context())
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.render(rw, r, http.StatusOK, "admin_teams", &Page{Title: "Teams · Admin", Tab: "admin", Data: map[string]any{
		"Teams": teams, "OIDC": w.OIDC != nil,
	}})
}

func (w *Web) adminSaveTeam(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	t := &store.Team{ID: id, Name: strings.TrimSpace(r.FormValue("name")),
		Description: strings.TrimSpace(r.FormValue("description")), OIDCGroup: strings.TrimSpace(r.FormValue("oidc_group"))}
	err := w.Store.SaveTeam(r.Context(), t)
	switch {
	case errors.Is(err, store.ErrDuplicate):
		w.redirectFlash(rw, r, "/admin/teams", "A team named "+t.Name+" already exists.")
		return
	case err != nil:
		w.redirectFlash(rw, r, "/admin/teams", "Team names use letters, digits, '.', '_' and '-'.")
		return
	}
	w.audit(r, "team.save", t.Name, map[string]any{"oidc_group": t.OIDCGroup})
	w.redirectFlash(rw, r, "/admin/teams#team-"+strconv.FormatInt(t.ID, 10), "Team "+t.Name+" saved.")
}

func (w *Web) adminDeleteTeam(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	t, err := w.Store.TeamByID(r.Context(), id)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	if err := w.Store.DeleteTeam(r.Context(), id); err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.audit(r, "team.delete", t.Name, nil)
	w.redirectFlash(rw, r, "/admin/teams", "Team "+t.Name+" deleted.")
}

func (w *Web) adminTeamMember(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	t, err := w.Store.TeamByID(r.Context(), id)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	u, err := w.Store.UserByUsername(r.Context(), strings.TrimSpace(r.FormValue("username")))
	if err != nil {
		w.redirectFlash(rw, r, "/admin/teams#team-"+strconv.FormatInt(id, 10), "No such user.")
		return
	}
	if r.FormValue("remove") != "" {
		err = w.Store.RemoveTeamMember(r.Context(), id, u.ID)
	} else {
		err = w.Store.AddTeamMember(r.Context(), id, u.ID)
	}
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	action := "team.add"
	if r.FormValue("remove") != "" {
		action = "team.remove"
	}
	w.audit(r, action, t.Name, map[string]any{"user": u.Username})
	w.redirectFlash(rw, r, "/admin/teams#team-"+strconv.FormatInt(id, 10), "")
}

// ---- deploy rules and dimensions ----

type ruleForm struct {
	*store.DeployRule
	SelectorText   string
	PrincipalsText string
	ApproversText  string
}

func (w *Web) adminDeploy(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dims, err := w.Store.ListDeployDimensions(ctx)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	rules, err := w.Store.ListDeployRules(ctx)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	var grants, reqs []ruleForm
	for _, rl := range rules {
		f := ruleForm{DeployRule: rl, SelectorText: formatSelector(rl.Selector),
			PrincipalsText: formatPrincipals(rl.Principals), ApproversText: formatPrincipals(rl.Approvers)}
		if rl.Kind == "grant" {
			grants = append(grants, f)
		} else {
			reqs = append(reqs, f)
		}
	}
	w.render(rw, r, http.StatusOK, "admin_deploy", &Page{Title: "Deploy rules · Admin", Tab: "admin", Data: map[string]any{
		"Dims": dims, "Values": w.Deploy.DimensionValues(ctx, dims, w.recipeSHA(r)), "Grants": grants, "Reqs": reqs,
	}})
}

var dimNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func (w *Web) adminSaveDimension(rw http.ResponseWriter, r *http.Request) {
	d := &store.DeployDimension{Name: strings.TrimSpace(r.FormValue("name")), Source: r.FormValue("source"),
		PathPattern: strings.Trim(strings.TrimSpace(r.FormValue("path_pattern")), "/")}
	d.Position, _ = strconv.Atoi(r.FormValue("position"))
	if !dimNameRe.MatchString(d.Name) {
		w.redirectFlash(rw, r, "/admin/deploy", "Dimension names are lowercase letters, digits and '_' (they become env variables).")
		return
	}
	switch d.Source {
	case "list":
		d.Values = splitList(r.FormValue("values"))
		if len(d.Values) == 0 {
			w.redirectFlash(rw, r, "/admin/deploy", "List at least one value.")
			return
		}
	case "paths":
		if strings.Count(d.PathPattern, "*") != 1 || strings.Contains(d.PathPattern, "**") {
			w.redirectFlash(rw, r, "/admin/deploy", "The path pattern needs exactly one '*' segment, e.g. projects/*/project.yaml.")
			return
		}
	default:
		w.redirectFlash(rw, r, "/admin/deploy", "Choose where the values come from.")
		return
	}
	if err := w.Store.SaveDeployDimension(r.Context(), d); err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.audit(r, "deploy.dimension", d.Name, map[string]any{"source": d.Source, "values": d.Values, "path": d.PathPattern})
	w.redirectFlash(rw, r, "/admin/deploy", "Dimension "+d.Name+" saved.")
}

func (w *Web) adminDeleteDimension(rw http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := w.Store.DeleteDeployDimension(r.Context(), name); err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.audit(r, "deploy.dimension.delete", name, nil)
	w.redirectFlash(rw, r, "/admin/deploy", "Dimension "+name+" deleted.")
}

func (w *Web) adminSaveRule(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dims, _ := w.Store.ListDeployDimensions(ctx)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rule := &store.DeployRule{ID: id, Kind: r.FormValue("kind"), Description: strings.TrimSpace(r.FormValue("description"))}
	if id != 0 {
		old, err := w.Store.DeployRuleByID(ctx, id)
		if err != nil {
			w.fail(rw, r, err)
			return
		}
		rule.Kind = old.Kind
	}
	fail := func(msg string) { w.redirectFlash(rw, r, "/admin/deploy#"+rule.Kind+"s", msg) }
	var err error
	if rule.Selector, err = parseSelector(r.FormValue("selector"), dims); err != nil {
		fail("Targets: " + err.Error())
		return
	}
	switch rule.Kind {
	case "grant":
		if rule.Principals, err = parsePrincipals(r.FormValue("principals")); err != nil {
			fail("Who: " + err.Error())
			return
		}
		if len(rule.Principals) == 0 {
			fail("A grant needs at least one user, team, role or codeowners.")
			return
		}
	case "require":
		rule.Branches = splitList(r.FormValue("branches"))
		rule.ExceptBranches = splitList(r.FormValue("except_branches"))
		rule.RequireChecks = r.FormValue("require_checks") == "on"
		rule.RequireCIArtifact = r.FormValue("require_ci_artifact") == "on"
		rule.AllowAuthorApproval = r.FormValue("allow_author_approval") == "on"
		rule.Freeze = r.FormValue("freeze") == "on"
		rule.FreezeMessage = strings.TrimSpace(r.FormValue("freeze_message"))
		if rule.Approvals, err = strconv.Atoi(r.FormValue("approvals")); err != nil || rule.Approvals < 0 || rule.Approvals > 10 {
			fail("Approvals must be a number between 0 and 10.")
			return
		}
		if rule.Approvers, err = parsePrincipals(r.FormValue("approvers")); err != nil {
			fail("Approvers: " + err.Error())
			return
		}
	default:
		fail("Unknown rule kind.")
		return
	}
	if err := w.Store.SaveDeployRule(ctx, rule); err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.audit(r, "deploy.rule", "rule #"+strconv.FormatInt(rule.ID, 10), rule)
	w.redirectFlash(rw, r, "/admin/deploy#"+rule.Kind+"s", "Rule saved.")
}

func (w *Web) adminDeleteRule(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rule, err := w.Store.DeployRuleByID(r.Context(), id)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	if err := w.Store.DeleteDeployRule(r.Context(), id); err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.audit(r, "deploy.rule.delete", "rule #"+strconv.FormatInt(id, 10), rule)
	w.redirectFlash(rw, r, "/admin/deploy#"+rule.Kind+"s", "Rule deleted.")
}

// ---- secrets ----

func (w *Web) adminSecrets(rw http.ResponseWriter, r *http.Request) {
	secrets, err := w.Store.ListDeploySecrets(r.Context())
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	type row struct {
		*store.DeploySecret
		SelectorText string
	}
	rows := make([]row, len(secrets))
	for i, s := range secrets {
		rows[i] = row{s, formatSelector(s.Selector)}
	}
	w.render(rw, r, http.StatusOK, "admin_secrets", &Page{Title: "Secrets · Admin", Tab: "admin", Data: map[string]any{
		"Secrets": rows, "KeyFromEnv": w.Deploy.KeyFromEnv(),
	}})
}

var secretNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (w *Web) adminSaveSecret(rw http.ResponseWriter, r *http.Request) {
	dims, _ := w.Store.ListDeployDimensions(r.Context())
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	sec := &store.DeploySecret{ID: id, Name: strings.TrimSpace(r.FormValue("name"))}
	if !secretNameRe.MatchString(sec.Name) || strings.HasPrefix(sec.Name, "ONEGIT_") {
		w.redirectFlash(rw, r, "/admin/secrets", "Secret names are environment variable names (not starting with ONEGIT_).")
		return
	}
	var err error
	if sec.Selector, err = parseSelector(r.FormValue("selector"), dims); err != nil {
		w.redirectFlash(rw, r, "/admin/secrets", "Targets: "+err.Error())
		return
	}
	if err := w.Deploy.SaveSecret(r.Context(), currentUser(r), sec, r.FormValue("value")); err != nil {
		w.redirectFlash(rw, r, "/admin/secrets", "Could not save: "+err.Error())
		return
	}
	w.redirectFlash(rw, r, "/admin/secrets", "Secret "+sec.Name+" saved.")
}

func (w *Web) adminDeleteSecret(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := w.Store.DeleteDeploySecret(r.Context(), id); err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.audit(r, "secret.delete", "secret #"+strconv.FormatInt(id, 10), nil)
	w.redirectFlash(rw, r, "/admin/secrets", "Secret deleted.")
}

// ---- audit ----

func (w *Web) adminAudit(rw http.ResponseWriter, r *http.Request) {
	entries, err := w.Store.ListAudit(r.Context(), 300)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.render(rw, r, http.StatusOK, "admin_audit", &Page{Title: "Audit log · Admin", Tab: "admin", Data: entries})
}
