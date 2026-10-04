package pulls

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"onegit/internal/git"
	"onegit/internal/store"
)

// Verdict is a reviewer's current position on a PR.
type Verdict struct {
	UserID int64
	Name   string
	Email  string
	State  store.ReviewState
	When   time.Time
	// Stale: approved an older head while the branch dismisses stale approvals.
	Stale bool
	// Counts: the approval counts towards required approvals.
	Counts bool
}

// OwnerGroup is a set of changed files that share the same code owners.
type OwnerGroup struct {
	Pattern    string
	Owners     []string // as written in CODEOWNERS
	Files      []string
	Resolvable bool     // at least one owner is a known user or team member
	ApprovedBy []string // owners with a counting approval
	// Server: from the branch protection's owners (always enforced), not
	// from the CODEOWNERS file.
	Server bool
}

func (g *OwnerGroup) Satisfied() bool { return !g.Resolvable || len(g.ApprovedBy) > 0 }

// Status is everything needed to decide whether a PR can be merged.
type Status struct {
	HeadExists bool
	BaseExists bool
	BaseSHA    string
	Conflicts  []string

	Protection     *store.BranchProtection
	Verdicts       []*Verdict
	Approvals      int
	Required       int
	CodeOwnersFile string
	OwnerGroups    []*OwnerGroup
	Checks         []*store.CommitStatus
	RequiredChecks []string
	// QueueRequired: the base branch takes PRs only through the merge queue.
	QueueRequired bool

	Blockers []string
}

func (st *Status) Mergeable() bool { return len(st.Blockers) == 0 }

// Status evaluates an open PR.
func (s *Service) Status(ctx context.Context, p *store.Pull, reviews []*store.PullReview) (*Status, error) {
	return s.status(ctx, p, reviews, false)
}

// status evaluates an open PR. For the merge queue (inQueue), conflicts and
// checks are judged on the queue's candidate commit instead, so they do not
// block here.
func (s *Service) status(ctx context.Context, p *store.Pull, reviews []*store.PullReview, inQueue bool) (*Status, error) {
	st := &Status{}
	_, err := s.Repo.ResolveCommit(ctx, "refs/heads/"+p.HeadBranch)
	st.HeadExists = err == nil
	st.BaseSHA, err = s.Repo.ResolveCommit(ctx, "refs/heads/"+p.BaseBranch)
	st.BaseExists = err == nil
	if !st.HeadExists {
		st.Blockers = append(st.Blockers, fmt.Sprintf("The head branch %s was deleted.", p.HeadBranch))
	}
	if !st.BaseExists {
		st.Blockers = append(st.Blockers, fmt.Sprintf("The base branch %s does not exist.", p.BaseBranch))
		return st, nil
	}

	mr, err := s.MergeCheck(ctx, st.BaseSHA, p.HeadSHA)
	if err != nil {
		return nil, err
	}
	st.Conflicts = mr.Conflicts
	if len(st.Conflicts) > 0 && !inQueue {
		st.Blockers = append(st.Blockers, "This branch has conflicts that must be resolved.")
	}

	st.Protection, err = s.Store.BranchProtectionFor(ctx, p.BaseBranch)
	if err != nil {
		return nil, err
	}
	if err := s.evalReviews(ctx, p, reviews, st); err != nil {
		return nil, err
	}
	if err := s.evalCodeOwners(ctx, p, st); err != nil {
		return nil, err
	}
	if st.Checks, err = s.Store.CommitStatuses(ctx, p.HeadSHA); err != nil {
		return nil, err
	}

	if prot := st.Protection; prot != nil {
		st.QueueRequired = prot.RequireMergeQueue
		st.Required = prot.RequiredApprovals
		if st.Approvals < st.Required {
			n := st.Required - st.Approvals
			st.Blockers = append(st.Blockers, fmt.Sprintf("%d more approving review%s required.", n, plural(n)))
		}
		if prot.BlockOnChangesRequested {
			var names []string
			for _, v := range st.Verdicts {
				if v.State == store.ReviewChangesRequested {
					names = append(names, v.Name)
				}
			}
			if len(names) > 0 {
				st.Blockers = append(st.Blockers, "Changes requested by "+strings.Join(names, ", ")+".")
			}
		}
		for _, g := range st.OwnerGroups {
			if (prot.RequireCodeOwnerReview || g.Server) && !g.Satisfied() {
				st.Blockers = append(st.Blockers, fmt.Sprintf("Code owner review required for %s (%s).", g.Pattern, strings.Join(g.Owners, ", ")))
			}
		}
		st.RequiredChecks = prot.RequiredChecks
		for _, name := range prot.RequiredChecks {
			if inQueue {
				break
			}
			var found *store.CommitStatus
			for _, c := range st.Checks {
				if c.Context == name {
					found = c
				}
			}
			switch {
			case found == nil:
				st.Blockers = append(st.Blockers, fmt.Sprintf("Required check %q has not run on the latest commit.", name))
			case found.State == "pending":
				st.Blockers = append(st.Blockers, fmt.Sprintf("Required check %q is still running.", name))
			case !found.OK():
				st.Blockers = append(st.Blockers, fmt.Sprintf("Required check %q failed.", name))
			}
		}
	}
	return st, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// MergeCheck is cached per (base, head): the result never changes.
func (s *Service) MergeCheck(ctx context.Context, base, head string) (*git.MergeResult, error) {
	key := "pullmerge:" + base + ":" + head
	var mr git.MergeResult
	if ok, err := s.KV.GetJSON(ctx, key, &mr); ok && err == nil {
		return &mr, nil
	}
	res, err := s.Repo.MergeTree(ctx, "", base, head)
	if err != nil {
		return nil, err
	}
	if err := s.KV.SetJSON(ctx, key, res, 7*24*time.Hour); err != nil {
		s.Log.Warn("cache merge check", "err", err)
	}
	return res, nil
}

// evalReviews reduces reviews to one verdict per reviewer: the latest
// approve/request-changes wins; plain comments don't change a verdict.
func (s *Service) evalReviews(ctx context.Context, p *store.Pull, reviews []*store.PullReview, st *Status) error {
	byUser := map[int64]*Verdict{}
	var order []int64
	for _, r := range reviews {
		if r.ReviewerID == nil || r.State == store.ReviewCommented {
			continue
		}
		v, ok := byUser[*r.ReviewerID]
		if !ok {
			v = &Verdict{UserID: *r.ReviewerID}
			byUser[*r.ReviewerID] = v
			order = append(order, *r.ReviewerID)
		}
		v.Name, v.Email, v.State, v.When = r.ReviewerName, r.ReviewerEmail, r.State, r.CreatedAt
		v.Stale = r.State == store.ReviewApproved && r.CommitSHA != p.HeadSHA &&
			st.Protection != nil && st.Protection.DismissStaleApprovals
	}
	for _, id := range order {
		v := byUser[id]
		if v.State == store.ReviewApproved && !v.Stale && (p.AuthorID == nil || *p.AuthorID != id) {
			// Only current writers count; reviewers may have lost access since.
			if u, err := s.Store.UserByID(ctx, id); err == nil && u.CanWrite() {
				v.Counts = true
				st.Approvals++
			}
		}
		st.Verdicts = append(st.Verdicts, v)
	}
	return nil
}

// evalCodeOwners groups changed files by their owners. CODEOWNERS is read
// from the base branch, so a PR cannot change its own reviewers; the branch
// protection's owners come from the server and are always enforced.
func (s *Service) evalCodeOwners(ctx context.Context, p *store.Pull, st *Status) error {
	var fileOwners *CodeOwners
	for _, path := range CodeOwnersPaths {
		b, err := s.Repo.ReadBlob(ctx, st.BaseSHA, path, 1<<20)
		if err == nil {
			fileOwners, st.CodeOwnersFile = ParseCodeOwners(string(b)), path
			break
		}
	}
	var serverOwners *CodeOwners
	if st.Protection != nil && strings.TrimSpace(st.Protection.Owners) != "" {
		serverOwners = ParseCodeOwners(st.Protection.Owners)
	}
	if fileOwners == nil && serverOwners == nil {
		return nil
	}
	files, err := s.Repo.ChangedFiles(ctx, p.MergeBase, p.HeadSHA)
	if err != nil {
		return err
	}
	for _, src := range []struct {
		co     *CodeOwners
		server bool
	}{{fileOwners, false}, {serverOwners, true}} {
		if src.co == nil {
			continue
		}
		groups := map[string]*OwnerGroup{}
		for _, f := range files {
			owners, pattern := src.co.Owners(f)
			if len(owners) == 0 {
				continue
			}
			key := pattern + "\x00" + strings.Join(owners, " ")
			g, ok := groups[key]
			if !ok {
				g = &OwnerGroup{Pattern: pattern, Owners: owners, Server: src.server}
				groups[key] = g
				st.OwnerGroups = append(st.OwnerGroups, g)
			}
			g.Files = append(g.Files, f)
		}
	}
	if len(st.OwnerGroups) == 0 {
		return nil
	}
	approved := map[int64]bool{}
	for _, v := range st.Verdicts {
		approved[v.UserID] = v.Counts
	}
	res, err := s.resolveOwners(ctx, st.OwnerGroups)
	if err != nil {
		return err
	}
	for _, g := range st.OwnerGroups {
		for _, o := range g.Owners {
			for _, u := range res[o] {
				g.Resolvable = true
				if approved[u.ID] && !contains(g.ApprovedBy, u.Username) {
					g.ApprovedBy = append(g.ApprovedBy, u.Username)
				}
			}
		}
	}
	sort.SliceStable(st.OwnerGroups, func(i, j int) bool {
		if st.OwnerGroups[i].Server != st.OwnerGroups[j].Server {
			return !st.OwnerGroups[i].Server
		}
		return st.OwnerGroups[i].Pattern < st.OwnerGroups[j].Pattern
	})
	return nil
}

// resolveOwners maps each owner as written ("@user", "@team", "@org/team",
// "email") to the active users it stands for.
func (s *Service) resolveOwners(ctx context.Context, groups []*OwnerGroup) (map[string][]*store.User, error) {
	var names, emails, teams []string
	for _, g := range groups {
		for _, o := range g.Owners {
			if n, ok := strings.CutPrefix(o, "@"); ok {
				names = append(names, strings.ToLower(n))
				teams = append(teams, teamName(n))
			} else if strings.Contains(o, "@") {
				emails = append(emails, strings.ToLower(o))
			}
		}
	}
	users, err := s.Store.UsersByLogin(ctx, names, emails)
	if err != nil {
		return nil, err
	}
	members, err := s.Store.TeamMemberIDs(ctx, teams)
	if err != nil {
		return nil, err
	}
	out := map[string][]*store.User{}
	for _, g := range groups {
		for _, o := range g.Owners {
			if _, done := out[o]; done {
				continue
			}
			var list []*store.User
			for _, u := range users {
				if u.Active && ownerMatches(o, u) {
					list = append(list, u)
				}
			}
			if n, ok := strings.CutPrefix(o, "@"); ok {
				for _, id := range members[teamName(n)] {
					if u, err := s.Store.UserByID(ctx, id); err == nil && u.Active {
						list = append(list, u)
					}
				}
			}
			out[o] = list
		}
	}
	return out, nil
}

// teamName strips a GitHub-style organisation ("org/team" → "team").
func teamName(n string) string {
	return strings.ToLower(n[strings.LastIndex(n, "/")+1:])
}

// OwnerListIncludes reports whether the owners (as written in CODEOWNERS)
// include u directly or through one of teams (lower-cased names).
func OwnerListIncludes(owners []string, u *store.User, teams []string) bool {
	for _, o := range owners {
		if ownerMatches(o, u) {
			return true
		}
		if n, ok := strings.CutPrefix(o, "@"); ok && contains(teams, teamName(n)) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func ownerMatches(owner string, u *store.User) bool {
	if n, ok := strings.CutPrefix(owner, "@"); ok {
		return strings.EqualFold(n, u.Username)
	}
	return u.Email != "" && strings.EqualFold(owner, u.Email)
}
