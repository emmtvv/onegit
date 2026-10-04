// Package deploy decides who may deploy what where. Deployment rules live in
// the database and are edited in the UI; the repository only says how to
// deploy (recipes, read from the default branch). A deployment request is
// checked against grants (who) and requirements (branch, checks, CI-built
// artifact, approvals, freeze) before and again right before it runs.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"onegit/internal/ci"
	"onegit/internal/config"
	"onegit/internal/git"
	"onegit/internal/pulls"
	"onegit/internal/store"
)

type Service struct {
	Store *store.Store
	Repo  *git.Repo
	CI    *ci.Service
	Cfg   *config.Config
	Log   *slog.Logger

	key []byte
}

func New(ctx context.Context, st *store.Store, repo *git.Repo, c *ci.Service, cfg *config.Config, log *slog.Logger) (*Service, error) {
	s := &Service{Store: st, Repo: repo, CI: c, Cfg: cfg, Log: log}
	if err := s.initKey(ctx); err != nil {
		return nil, err
	}
	c.SecretsFor = s.SecretsFor
	c.OnRunFinished = s.onRunFinished
	return s, nil
}

// UserError is safe to show to the user.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func userErr(format string, args ...any) error { return &UserError{Msg: fmt.Sprintf(format, args...)} }

type Target map[string]string

// Key is the canonical "dim=value,dim=value" form (dimensions sorted).
func (t Target) Key() string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + t[k]
	}
	return strings.Join(parts, ",")
}

// Label is the target for humans: values in dimension order.
func (t Target) Label(dims []*store.DeployDimension) string {
	var parts []string
	for _, d := range dims {
		if v, ok := t[d.Name]; ok {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " / ")
}

func ParseTargetKey(key string) Target {
	t := Target{}
	for _, p := range strings.Split(key, ",") {
		if k, v, ok := strings.Cut(p, "="); ok {
			t[k] = v
		}
	}
	return t
}

func selectorMatches(sel store.Selector, target map[string]string) bool {
	return ci.SelectorMatches(sel, target)
}

func (s *Service) DefaultBranch(ctx context.Context) string {
	if b := s.Repo.HeadBranch(ctx); b != "" {
		return b
	}
	return s.Cfg.Repo.DefaultBranch
}

// ---- dimensions ----

// DimensionValues lists the possible values of each dimension; "paths"
// dimensions are discovered in the default branch at sha.
func (s *Service) DimensionValues(ctx context.Context, dims []*store.DeployDimension, sha string) map[string][]string {
	out := map[string][]string{}
	for _, d := range dims {
		switch d.Source {
		case "list":
			out[d.Name] = d.Values
		case "paths":
			if sha != "" {
				out[d.Name] = s.Repo.MatchDirs(ctx, sha, d.PathPattern)
			}
		}
	}
	return out
}

// targetPaths returns repository directories that belong to a target (from
// its "paths" dimensions), for code-owner principals.
func targetPaths(dims []*store.DeployDimension, t Target) []string {
	var out []string
	for _, d := range dims {
		if d.Source != "paths" {
			continue
		}
		if prefix, _, ok := strings.Cut(d.PathPattern, "*"); ok && t[d.Name] != "" {
			out = append(out, prefix+t[d.Name]+"/")
		}
	}
	return out
}

// ---- evaluation ----

type Check struct {
	Kind   string `json:"kind"` // recipe, grant, freeze, branch, checks, artifact
	RuleID int64  `json:"rule_id,omitempty"`
	Title  string `json:"title"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type TargetEval struct {
	Target   Target  `json:"target"`
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Recipe   string  `json:"recipe,omitempty"`
	Artifact string  `json:"artifact,omitempty"`
	Checks   []Check `json:"checks"`
	recipe   *ci.Recipe
}

func (t *TargetEval) OK() bool {
	for _, c := range t.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

type ApprovalNeed struct {
	RuleID      int64    `json:"rule_id"`
	Description string   `json:"description"`
	Required    int      `json:"required"`
	Approvers   string   `json:"approvers"`
	ApprovedBy  []string `json:"approved_by"`
	principals  []store.Principal
	allowAuthor bool
	targets     []Target
}

func (a *ApprovalNeed) Satisfied() bool { return len(a.ApprovedBy) >= a.Required }

type Evaluation struct {
	SHA        string          `json:"sha"`
	RecipeSHA  string          `json:"recipe_sha"`
	Targets    []*TargetEval   `json:"targets"`
	Approvals  []*ApprovalNeed `json:"approvals"`
	RejectedBy []string        `json:"rejected_by,omitempty"`
}

// Blocked: some target fails a hard check (it cannot become OK by waiting).
func (e *Evaluation) Blocked() bool {
	for _, t := range e.Targets {
		if !t.OK() {
			return true
		}
	}
	return false
}

func (e *Evaluation) ApprovalsPending() bool {
	for _, a := range e.Approvals {
		if !a.Satisfied() {
			return true
		}
	}
	return false
}

func (e *Evaluation) Problems() []string {
	var out []string
	for _, t := range e.Targets {
		for _, c := range t.Checks {
			if !c.OK {
				out = append(out, t.Label+": "+c.Title+strings.TrimSuffix(" — "+c.Detail, " — "))
			}
		}
	}
	return out
}

// evalContext caches what every target needs.
type evalContext struct {
	user        *store.User
	userTeams   []string
	sha         string
	authorEmail string
	recipeSHA   string
	dims        []*store.DeployDimension
	rules       []*store.DeployRule
	recipes     []*ci.Recipe
	recipeErrs  []error
	owners      *pulls.CodeOwners
	statuses    []*store.CommitStatus
	branches    []git.Ref
	onBranch    map[string]bool
}

// Evaluate checks a request. approvals are the verdicts given so far.
func (s *Service) Evaluate(ctx context.Context, u *store.User, sha, recipeSHA string, targets []Target,
	approvals []*store.DeploymentApproval) (*Evaluation, error) {
	ec, err := s.newEvalContext(ctx, u, sha, recipeSHA)
	if err != nil {
		return nil, err
	}
	ev := &Evaluation{SHA: sha, RecipeSHA: recipeSHA}
	needs := map[int64]*ApprovalNeed{}
	for _, t := range targets {
		te := &TargetEval{Target: t, Key: t.Key(), Label: t.Label(ec.dims)}
		s.checkRecipe(ec, te)
		s.checkGrant(ctx, ec, te)
		for _, r := range ec.rules {
			if r.Kind != "require" || !selectorMatches(r.Selector, t) {
				continue
			}
			if len(r.ExceptBranches) > 0 {
				if b := s.onAnyBranch(ctx, ec, r.ExceptBranches); b != "" {
					te.Checks = append(te.Checks, Check{Kind: "exempt", RuleID: r.ID, OK: true,
						Title: ruleTitle(r) + " does not apply", Detail: "the commit is on " + b})
					continue
				}
			}
			s.checkRequirement(ctx, ec, te, r)
			if r.Approvals > 0 {
				n := needs[r.ID]
				if n == nil {
					n = &ApprovalNeed{RuleID: r.ID, Description: ruleTitle(r), Required: r.Approvals,
						Approvers:  describePrincipals(r.Approvers, "anyone with write access"),
						principals: r.Approvers, allowAuthor: r.AllowAuthorApproval}
					needs[r.ID] = n
					ev.Approvals = append(ev.Approvals, n)
				}
				n.targets = append(n.targets, t)
			}
		}
		if !hasCheck(te, "artifact") {
			s.resolveArtifact(ctx, ec, te, false)
		}
		ev.Targets = append(ev.Targets, te)
	}
	// Count approvals: never the requester; not the commit author unless
	// allowed; only principals the rule names (for every target it covers).
	for _, a := range approvals {
		approver, err := s.Store.UserByID(ctx, a.UserID)
		if err != nil || !approver.Active || approver.ID == u.ID {
			continue
		}
		teams, _ := s.Store.UserTeams(ctx, approver.ID)
		for _, n := range ev.Approvals {
			if !n.allowAuthor && ec.authorEmail != "" && strings.EqualFold(approver.Email, ec.authorEmail) {
				continue
			}
			if !s.eligible(ec, approver, teams, n.principals, n.targets) {
				continue
			}
			if a.Verdict == "reject" {
				if !contains(ev.RejectedBy, approver.Username) {
					ev.RejectedBy = append(ev.RejectedBy, approver.Username)
				}
				continue
			}
			n.ApprovedBy = append(n.ApprovedBy, approver.Username)
		}
	}
	return ev, nil
}

func hasCheck(te *TargetEval, kind string) bool {
	for _, c := range te.Checks {
		if c.Kind == kind {
			return true
		}
	}
	return false
}

func (s *Service) newEvalContext(ctx context.Context, u *store.User, sha, recipeSHA string) (*evalContext, error) {
	ec := &evalContext{user: u, sha: sha, recipeSHA: recipeSHA, onBranch: map[string]bool{}}
	var err error
	if ec.userTeams, err = s.Store.UserTeams(ctx, u.ID); err != nil {
		return nil, err
	}
	if c, err := s.Repo.GetCommit(ctx, sha); err == nil {
		ec.authorEmail = c.Author.Email
	}
	if ec.dims, err = s.Store.ListDeployDimensions(ctx); err != nil {
		return nil, err
	}
	if ec.rules, err = s.Store.ListDeployRules(ctx); err != nil {
		return nil, err
	}
	ec.recipes, ec.recipeErrs = s.CI.LoadRecipes(ctx, recipeSHA)
	ec.owners = s.codeOwners(ctx, recipeSHA)
	if ec.statuses, err = s.Store.CommitStatuses(ctx, sha); err != nil {
		return nil, err
	}
	if ec.branches, err = s.Repo.Branches(ctx); err != nil {
		return nil, err
	}
	return ec, nil
}

// codeOwners merges the CODEOWNERS file and the server-side owners of the
// default branch's protection rule, both taken from the default branch.
func (s *Service) codeOwners(ctx context.Context, sha string) *pulls.CodeOwners {
	var src strings.Builder
	for _, p := range pulls.CodeOwnersPaths {
		if b, err := s.Repo.ReadBlob(ctx, sha, p, 1<<20); err == nil {
			src.Write(b)
			src.WriteString("\n")
			break
		}
	}
	if prot, err := s.Store.BranchProtectionFor(ctx, s.DefaultBranch(ctx)); err == nil && prot != nil {
		src.WriteString(prot.Owners)
	}
	return pulls.ParseCodeOwners(src.String())
}

func (s *Service) checkRecipe(ec *evalContext, te *TargetEval) {
	var matched []*ci.Recipe
	for _, r := range ec.recipes {
		if r.Serves(te.Target) {
			matched = append(matched, r)
		}
	}
	switch len(matched) {
	case 0:
		detail := "add one to " + ci.RecipesDir + "/ on the default branch"
		if len(ec.recipeErrs) > 0 {
			detail = ec.recipeErrs[0].Error()
		}
		te.Checks = append(te.Checks, Check{Kind: "recipe", Title: "No deploy recipe for this target", Detail: detail})
	case 1:
		te.Recipe, te.recipe = matched[0].File, matched[0]
		te.Checks = append(te.Checks, Check{Kind: "recipe", Title: "Recipe " + matched[0].File, OK: true})
	default:
		var files []string
		for _, r := range matched {
			files = append(files, r.File)
		}
		te.Checks = append(te.Checks, Check{Kind: "recipe", Title: "Several recipes declare this target",
			Detail: strings.Join(files, ", ")})
	}
}

func (s *Service) checkGrant(ctx context.Context, ec *evalContext, te *TargetEval) {
	for _, r := range ec.rules {
		if r.Kind != "grant" || !selectorMatches(r.Selector, te.Target) {
			continue
		}
		if s.eligible(ec, ec.user, ec.userTeams, r.Principals, []Target{te.Target}) {
			te.Checks = append(te.Checks, Check{Kind: "grant", RuleID: r.ID, Title: "Allowed by " + ruleTitle(r), OK: true})
			return
		}
	}
	te.Checks = append(te.Checks, Check{Kind: "grant", Title: "You are not allowed to deploy here",
		Detail: "no grant rule covers you for this target"})
}

func (s *Service) checkRequirement(ctx context.Context, ec *evalContext, te *TargetEval, r *store.DeployRule) {
	if r.Freeze {
		te.Checks = append(te.Checks, Check{Kind: "freeze", RuleID: r.ID, Title: "Deploys are frozen",
			Detail: firstNonEmpty(r.FreezeMessage, ruleTitle(r))})
	}
	if len(r.Branches) > 0 {
		c := Check{Kind: "branch", RuleID: r.ID, Title: "Commit must be on " + strings.Join(r.Branches, ", ")}
		if b := s.onAnyBranch(ctx, ec, r.Branches); b != "" {
			c.OK, c.Detail = true, "on "+b
		}
		te.Checks = append(te.Checks, c)
	}
	if r.RequireChecks {
		c := Check{Kind: "checks", RuleID: r.ID, Title: "All checks must pass"}
		var bad []string
		for _, st := range ec.statuses {
			if !st.OK() {
				bad = append(bad, st.Context+" ("+st.State+")")
			}
		}
		switch {
		case len(ec.statuses) == 0:
			c.Detail = "the commit has no checks"
		case len(bad) > 0:
			c.Detail = strings.Join(bad, ", ")
		default:
			c.OK = true
		}
		te.Checks = append(te.Checks, c)
	}
	if r.RequireCIArtifact && !hasCheck(te, "artifact") {
		s.resolveArtifact(ctx, ec, te, true)
	}
}

// onAnyBranch returns a branch matching patterns that contains the commit.
func (s *Service) onAnyBranch(ctx context.Context, ec *evalContext, patterns []string) string {
	for _, b := range ec.branches {
		if !ci.MatchAny(patterns, b.Name) {
			continue
		}
		on, cached := ec.onBranch[b.Name]
		if !cached {
			on, _ = s.Repo.IsAncestor(ctx, ec.sha, b.SHA)
			ec.onBranch[b.Name] = on
		}
		if on {
			return b.Name
		}
	}
	return ""
}

// resolveArtifact looks up the recipe's image for this commit. With require,
// it must exist (unless optional) and have been built by CI from the commit.
// The digest is pinned for the deploy job either way.
func (s *Service) resolveArtifact(ctx context.Context, ec *evalContext, te *TargetEval, require bool) {
	add := func(ok bool, title, detail string) {
		if require {
			te.Checks = append(te.Checks, Check{Kind: "artifact", Title: title, OK: ok, Detail: detail})
		}
	}
	if te.recipe == nil {
		return
	}
	spec := te.recipe.Artifact
	if spec == nil || spec.Image == "" {
		add(false, "Image must be built by CI", "the recipe declares no artifact")
		return
	}
	vars := map[string]string{"sha": ec.sha, "SHA": ec.sha}
	for k, v := range te.Target {
		vars[k] = v
	}
	expand := func(s string) string { return os.Expand(s, func(k string) string { return vars[k] }) }
	image, tag := expand(spec.Image), expand(firstNonEmpty(spec.Tag, "${sha}"))
	m, err := s.Store.RegistryManifestByTag(ctx, image, tag)
	switch {
	case errors.Is(err, store.ErrNotFound) && spec.Optional:
		add(true, "No image "+image+":"+tag, "optional: nothing to verify")
		return
	case errors.Is(err, store.ErrNotFound):
		add(false, "Image must be built by CI", image+":"+tag+" does not exist")
		return
	case err != nil:
		add(false, "Image must be built by CI", err.Error())
		return
	}
	te.Artifact = s.Cfg.RegistryHost() + "/" + image + "@" + m.Digest
	if m.BuildSHA != ec.sha {
		detail := "pushed by hand"
		if m.BuildSHA != "" {
			detail = "built by CI from " + m.BuildSHA[:min(10, len(m.BuildSHA))]
		}
		add(false, "Image must be built by CI from this commit", image+":"+tag+" was "+detail)
		return
	}
	add(true, "Image built by CI", image+":"+tag)
}

// eligible reports whether u matches any principal for every target (for
// code owners, the owners of each target's paths).
func (s *Service) eligible(ec *evalContext, u *store.User, teams []string, principals []store.Principal, targets []Target) bool {
	if len(principals) == 0 {
		return u.CanWrite()
	}
	for _, p := range principals {
		switch p.Type {
		case "user":
			if strings.EqualFold(p.Value, u.Username) {
				return true
			}
		case "team":
			if contains(teams, strings.ToLower(p.Value)) {
				return true
			}
		case "role":
			if (p.Value == "read" && u.Active) || (p.Value == "write" && u.CanWrite()) || (p.Value == "admin" && u.IsAdmin()) {
				return true
			}
		case "codeowners":
			if s.ownsAll(ec, u, teams, targets) {
				return true
			}
		}
	}
	return false
}

func (s *Service) ownsAll(ec *evalContext, u *store.User, teams []string, targets []Target) bool {
	for _, t := range targets {
		paths := targetPaths(ec.dims, t)
		if len(paths) == 0 {
			return false
		}
		for _, p := range paths {
			owners, _ := ec.owners.Owners(p)
			if !pulls.OwnerListIncludes(owners, u, teams) {
				return false
			}
		}
	}
	return true
}

func ruleTitle(r *store.DeployRule) string {
	if r.Description != "" {
		return r.Description
	}
	return fmt.Sprintf("rule #%d", r.ID)
}

func describePrincipals(ps []store.Principal, empty string) string {
	if len(ps) == 0 {
		return empty
	}
	var out []string
	for _, p := range ps {
		switch p.Type {
		case "codeowners":
			out = append(out, "code owners")
		case "role":
			out = append(out, "role "+p.Value)
		default:
			out = append(out, p.Type+" "+p.Value)
		}
	}
	return strings.Join(out, ", ")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
