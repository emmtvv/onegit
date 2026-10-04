package pulls

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strings"

	"onegit/internal/store"
)

// Owner keys name who an owner in CODEOWNERS stands for, so open PRs can
// be looked up by owner (store.PullOwner): "@dana" is the user dana or the
// team dana, "@org/sre" the team sre, an email address the user with it.

// OwnerKeys returns the keys an owner, as written in CODEOWNERS, matches.
func OwnerKeys(owner string) []string {
	if n, ok := strings.CutPrefix(owner, "@"); ok {
		if strings.Contains(n, "/") {
			return []string{"team:" + teamName(n)}
		}
		return []string{"user:" + strings.ToLower(n), "team:" + teamName(n)}
	}
	if strings.Contains(owner, "@") {
		return []string{"email:" + strings.ToLower(owner)}
	}
	return nil
}

// UserKeys returns the keys that match u, a member of teams (lower-cased).
func UserKeys(u *store.User, teams []string) []string {
	keys := []string{"user:" + strings.ToLower(u.Username)}
	if u.Email != "" {
		keys = append(keys, "email:"+strings.ToLower(u.Email))
	}
	for _, t := range teams {
		keys = append(keys, "team:"+t)
	}
	return keys
}

// baseRules are the owner rules of one base branch: its CODEOWNERS file and
// the branch protection's owners.
type baseRules struct {
	file, server *CodeOwners
	prot         *store.BranchProtection
	rev          string // identifies the rules, see store.PullsNeedingOwners
}

func (s *Service) baseRules(ctx context.Context, branch string) (*baseRules, error) {
	br := &baseRules{}
	var err error
	if br.prot, err = s.Store.BranchProtectionFor(ctx, branch); err != nil {
		return nil, err
	}
	h := sha256.New()
	if br.prot != nil && strings.TrimSpace(br.prot.Owners) != "" {
		br.server = ParseCodeOwners(br.prot.Owners)
		h.Write([]byte(br.prot.Owners))
	}
	h.Write([]byte{0})
	if sha, err := s.Repo.ResolveCommit(ctx, "refs/heads/"+branch); err == nil {
		for _, path := range CodeOwnersPaths {
			if b, err := s.Repo.ReadBlob(ctx, sha, path, 1<<20); err == nil {
				br.file = ParseCodeOwners(string(b))
				h.Write(b)
				break
			}
		}
	}
	br.rev = hex.EncodeToString(h.Sum(nil))[:16]
	return br, nil
}

// owners lists who owns which of files.
func (br *baseRules) owners(files []string) []store.PullOwner {
	var out []store.PullOwner
	seen := map[store.PullOwner]bool{}
	for _, co := range []*CodeOwners{br.file, br.server} {
		if co == nil {
			continue
		}
		for _, f := range files {
			owners, pattern := co.Owners(f)
			for _, o := range owners {
				for _, k := range OwnerKeys(o) {
					po := store.PullOwner{Pattern: pattern, Key: k}
					if !seen[po] {
						seen[po] = true
						out = append(out, po)
					}
				}
			}
		}
	}
	return out
}

// backfillOwners records the code owners of open PRs whose changed files or
// owner rules changed since. ownersRev remembers, per base branch, the
// rules every open PR was last brought up to date with: while they stay
// the same only new PRs need a look.
func (s *Service) backfillOwners(ctx context.Context) error {
	bases, err := s.Store.OpenPullBases(ctx)
	if err != nil {
		return err
	}
	for _, base := range bases {
		br, err := s.baseRules(ctx, base)
		if err != nil {
			return err
		}
		s.mu.Lock()
		all := s.ownersRev[base] != br.rev
		s.mu.Unlock()
		done := false
		for range 10 {
			list, err := s.Store.PullsNeedingOwners(ctx, base, br.rev, all, 100)
			if err != nil {
				return err
			}
			for _, p := range list {
				if err := s.Store.SetPullOwners(ctx, p.ID, br.rev, br.owners(p.Files)); err != nil {
					return err
				}
			}
			if len(list) < 100 {
				done = true
				break
			}
		}
		if done && all {
			s.mu.Lock()
			if s.ownersRev == nil {
				s.ownersRev = map[string]string{}
			}
			s.ownersRev[base] = br.rev
			s.mu.Unlock()
		}
	}
	return nil
}

// ReviewWait is an open PR that waits on one user's review.
type ReviewWait struct {
	Pull *store.Pull
	// Reason says why it is the user's turn.
	Reason string
}

// ReviewInbox lists, newest first, up to limit open PRs that wait on u's
// review. It is u's turn when u is not the author and either
//   - owns changed files (CODEOWNERS on the base branch, or the branch
//     protection's owners) that no owner has approved on the current head,
//     and has given no verdict yet, or
//   - requested changes on an older head, or approved one while the
//     branch dismisses stale approvals.
//
// teams are u's lower-cased team names. Owners come from pull_owners, which
// the background loop keeps, so a brand-new PR shows up a moment later.
func (s *Service) ReviewInbox(ctx context.Context, u *store.User, teams []string, limit int) ([]ReviewWait, error) {
	keys := UserKeys(u, teams)
	owned, err := s.Store.OwnedPullsAwaiting(ctx, u.ID, keys, limit)
	if err != nil {
		return nil, err
	}
	out, err := s.unapproved(ctx, owned, keys)
	if err != nil {
		return nil, err
	}

	changed, err := s.Store.ReviewedPullsChanged(ctx, u.ID, limit)
	if err != nil {
		return nil, err
	}
	dismiss := map[string]bool{}
	for _, r := range changed {
		base := r.Pull.BaseBranch
		if _, ok := dismiss[base]; !ok {
			prot, err := s.Store.BranchProtectionFor(ctx, base)
			if err != nil {
				return nil, err
			}
			dismiss[base] = prot != nil && prot.DismissStaleApprovals
		}
		switch {
		case r.State == store.ReviewChangesRequested:
			out = append(out, ReviewWait{Pull: r.Pull, Reason: "new commits since you requested changes"})
		case dismiss[base]:
			out = append(out, ReviewWait{Pull: r.Pull, Reason: "new commits since your approval"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pull.ID > out[j].Pull.ID })
	return out[:min(len(out), limit)], nil
}

// unapproved keeps the owned PRs with a group of files that keys own and
// that no owner has approved on the current head yet.
func (s *Service) unapproved(ctx context.Context, owned []store.OwnedPull, keys []string) ([]ReviewWait, error) {
	if len(owned) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(owned))
	for i, o := range owned {
		ids[i] = o.Pull.ID
	}
	groups, err := s.Store.PullOwnerKeys(ctx, ids)
	if err != nil {
		return nil, err
	}
	reviews, err := s.Store.ReviewsForPulls(ctx, ids)
	if err != nil {
		return nil, err
	}
	// The latest verdict of each reviewer, by PR.
	approvedBy := map[int64][]int64{}
	var approvers []int64
	for _, o := range owned {
		p, latest := o.Pull, map[int64]*store.PullReview{}
		for _, r := range reviews[p.ID] {
			if r.ReviewerID != nil && r.State != store.ReviewCommented {
				latest[*r.ReviewerID] = r
			}
		}
		for id, r := range latest {
			if r.State == store.ReviewApproved && r.CommitSHA == p.HeadSHA && (p.AuthorID == nil || *p.AuthorID != id) {
				approvedBy[p.ID] = append(approvedBy[p.ID], id)
				approvers = append(approvers, id)
			}
		}
	}
	approverKeys, err := s.Store.WriterOwnerKeys(ctx, approvers)
	if err != nil {
		return nil, err
	}
	var out []ReviewWait
	for _, o := range owned {
		patterns := make([]string, 0, len(groups[o.Pull.ID]))
		for pattern := range groups[o.Pull.ID] {
			patterns = append(patterns, pattern)
		}
		sort.Strings(patterns)
		for _, pattern := range patterns {
			group := groups[o.Pull.ID][pattern]
			if !overlaps(group, keys) {
				continue
			}
			approved := false
			for _, id := range approvedBy[o.Pull.ID] {
				approved = approved || overlaps(group, approverKeys[id])
			}
			if !approved {
				out = append(out, ReviewWait{Pull: o.Pull, Reason: "code owner of " + pattern})
				break
			}
		}
	}
	return out, nil
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}
