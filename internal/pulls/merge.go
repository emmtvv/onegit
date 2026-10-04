package pulls

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"onegit/internal/git"
	"onegit/internal/hooks"
	"onegit/internal/store"
)

type MergeStyle string

const (
	StyleSquash MergeStyle = "squash"
	StyleMerge  MergeStyle = "merge"
	StyleRebase MergeStyle = "rebase"
)

var MergeStyles = []MergeStyle{StyleSquash, StyleMerge, StyleRebase}

func (m MergeStyle) Valid() bool { return m == StyleSquash || m == StyleMerge || m == StyleRebase }

type MergeOptions struct {
	Style   MergeStyle
	Title   string // commit subject (squash / merge)
	Message string // commit body (squash / merge)
}

// DefaultMessage is the suggested commit message for a merge style.
func (s *Service) DefaultMessage(ctx context.Context, p *store.Pull, style MergeStyle) (title, body string) {
	num := strconv.FormatInt(p.ID, 10)
	switch style {
	case StyleMerge:
		return "Merge pull request #" + num + " from " + p.HeadBranch, p.Title
	case StyleSquash:
		var b strings.Builder
		if cs, err := s.Repo.Log(ctx, p.MergeBase+".."+p.HeadSHA, git.LogOptions{Limit: 100}); err == nil && len(cs) > 1 {
			for i := len(cs) - 1; i >= 0; i-- {
				fmt.Fprintf(&b, "* %s\n", cs[i].Subject)
			}
		}
		return p.Title + " (#" + num + ")", strings.TrimSpace(b.String())
	}
	return "", ""
}

// Merge merges an open PR into its base branch if every check passes.
func (s *Service) Merge(ctx context.Context, id int64, u *store.User, o MergeOptions) (*store.Pull, error) {
	if !u.CanWrite() {
		return nil, userErr("You do not have permission to merge.")
	}
	if !o.Style.Valid() {
		o.Style = StyleSquash
	}
	var baseSHA, newTip string
	p, err := s.Store.UpdatePullLocked(ctx, id, func(p *store.Pull) error {
		if !p.IsOpen() {
			return userErr("This pull request is not open.")
		}
		reviews, err := s.Store.ListPullReviews(ctx, p.ID)
		if err != nil {
			return err
		}
		st, err := s.Status(ctx, p, reviews)
		if err != nil {
			return err
		}
		if st.QueueRequired {
			return userErr("%s takes changes only through the merge queue.", p.BaseBranch)
		}
		if !st.Mergeable() {
			return userErr("Cannot merge: %s", strings.Join(st.Blockers, " "))
		}
		if tip, _ := s.Repo.ResolveCommit(ctx, "refs/heads/"+p.HeadBranch); tip != p.HeadSHA {
			return userErr("The head branch was just updated. Reload the page and try again.")
		}
		baseSHA = st.BaseSHA
		if newTip, err = s.buildMerge(ctx, p, baseSHA, u, o); err != nil {
			return err
		}
		if err := s.Repo.UpdateRef(ctx, "refs/heads/"+p.BaseBranch, newTip, baseSHA); err != nil {
			if errors.Is(err, git.ErrRefChanged) {
				return userErr("%s was updated while merging. Try again.", p.BaseBranch)
			}
			return err
		}
		now := time.Now()
		style := string(o.Style)
		p.State, p.MergedAt, p.MergedBy, p.MergeSHA, p.MergeStyle = store.PullMerged, &now, &u.ID, &newTip, &style
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := s.Store.AddPullEvent(ctx, id, &u.ID, "merged", map[string]any{"sha": newTip, "style": string(o.Style)}); err != nil {
		s.Log.Error("pull event", "err", err)
	}
	s.Log.Info("pull merged", "pull", id, "user", u.Username, "style", o.Style, "base", p.BaseBranch, "sha", newTip)
	// Server-side ref updates don't run hooks: sync other PRs and start
	// push pipelines ourselves.
	s.afterBaseUpdate(ctx, u, []hooks.RefUpdate{{OldSHA: baseSHA, NewSHA: newTip, Ref: "refs/heads/" + p.BaseBranch}})
	return s.Store.PullByID(ctx, id)
}

func (s *Service) afterBaseUpdate(ctx context.Context, u *store.User, updates []hooks.RefUpdate) {
	s.SyncPush(ctx, u, updates)
	if s.BaseUpdated != nil {
		s.BaseUpdated(ctx, u, updates)
	}
}

func (s *Service) buildMerge(ctx context.Context, p *store.Pull, baseSHA string, u *store.User, o MergeOptions) (string, error) {
	committer := signature(u, s.host())
	if o.Style == StyleRebase {
		if ok, _ := s.Repo.IsAncestor(ctx, baseSHA, p.HeadSHA); ok {
			return p.HeadSHA, nil // fast-forward keeps the original commits
		}
		commits, err := s.Repo.CommitsToReplay(ctx, baseSHA, p.HeadSHA)
		if err != nil {
			return "", err
		}
		cur := baseSHA
		for _, c := range commits {
			mr, err := s.Repo.MergeTree(ctx, c.Parent, cur, c.SHA)
			if err != nil {
				return "", err
			}
			if len(mr.Conflicts) > 0 {
				return "", userErr("Commit %s does not apply cleanly on %s; rebase is not possible. Use squash or merge.", c.SHA[:10], p.BaseBranch)
			}
			if cur, err = s.Repo.CommitTree(ctx, mr.Tree, []string{cur}, c.Message, c.Author, committer); err != nil {
				return "", err
			}
		}
		return cur, nil
	}

	mr, err := s.Repo.MergeTree(ctx, "", baseSHA, p.HeadSHA)
	if err != nil {
		return "", err
	}
	if len(mr.Conflicts) > 0 {
		return "", userErr("This branch has conflicts that must be resolved.")
	}
	defTitle, defBody := s.DefaultMessage(ctx, p, o.Style)
	title, body := strings.TrimSpace(o.Title), strings.TrimSpace(o.Message)
	if title == "" {
		title, body = defTitle, defBody
	}
	msg := title + "\n"
	if body != "" {
		msg += "\n" + body + "\n"
	}
	if o.Style == StyleMerge {
		return s.Repo.CommitTree(ctx, mr.Tree, []string{baseSHA, p.HeadSHA}, msg, committer, committer)
	}
	// Squash: the PR author authors the commit, the merger commits it.
	author := committer
	if p.AuthorID != nil {
		if a, err := s.Store.UserByID(ctx, *p.AuthorID); err == nil {
			author = signature(a, s.host())
		}
	}
	return s.Repo.CommitTree(ctx, mr.Tree, []string{baseSHA}, msg, author, committer)
}

// DeleteHeadBranch removes a closed or merged PR's head branch, provided it
// still points at the PR's last head (refs/pull/N/head keeps the commits).
func (s *Service) DeleteHeadBranch(ctx context.Context, p *store.Pull, u *store.User) error {
	if !u.CanWrite() {
		return userErr("You do not have permission to delete branches.")
	}
	if p.IsOpen() {
		return userErr("Close the pull request before deleting its branch.")
	}
	branch := p.HeadBranch
	if branch == s.Repo.HeadBranch(ctx) {
		return userErr("The default branch cannot be deleted.")
	}
	prot, err := s.Store.BranchProtectionFor(ctx, branch)
	if err != nil {
		return err
	}
	if prot != nil && !prot.AllowDeletion {
		return userErr("Branch %s is protected and cannot be deleted.", branch)
	}
	others, err := s.Store.OpenPullsForBranch(ctx, branch)
	if err != nil {
		return err
	}
	for _, o := range others {
		if o.BaseBranch == branch {
			return userErr("Branch %s is the base of open pull request #%d.", branch, o.ID)
		}
	}
	tip, err := s.Repo.ResolveCommit(ctx, "refs/heads/"+branch)
	if err != nil {
		return userErr("Branch %s no longer exists.", branch)
	}
	if tip != p.HeadSHA {
		return userErr("Branch %s has new commits; it was not deleted.", branch)
	}
	if err := s.Repo.UpdateRef(ctx, "refs/heads/"+branch, git.ZeroSHA, tip); err != nil {
		if errors.Is(err, git.ErrRefChanged) {
			return userErr("Branch %s has new commits; it was not deleted.", branch)
		}
		return err
	}
	s.Store.AddPullEvent(ctx, p.ID, &u.ID, "branch_deleted", map[string]any{"branch": branch, "sha": tip})
	s.SyncPush(ctx, u, []hooks.RefUpdate{{OldSHA: tip, NewSHA: git.ZeroSHA, Ref: "refs/heads/" + branch}})
	return nil
}

// HeadBranchExists reports whether the PR's head branch still exists.
func (s *Service) HeadBranchExists(ctx context.Context, p *store.Pull) bool {
	_, err := s.Repo.ResolveCommit(ctx, "refs/heads/"+p.HeadBranch)
	return err == nil
}

// CheckPush enforces branch protection for a push (pre-receive). env holds
// the quarantine variables so new objects are visible. It returns a
// rejection message, or "" to allow.
func (s *Service) CheckPush(ctx context.Context, updates []hooks.RefUpdate, env []string) (string, error) {
	rules, err := s.Store.ListBranchProtections(ctx)
	if err != nil || len(rules) == 0 {
		return "", err
	}
	repo := s.Repo.WithEnv(env)
	for _, up := range updates {
		branch, ok := strings.CutPrefix(up.Ref, "refs/heads/")
		if !ok {
			continue
		}
		prot := store.MatchBranchProtection(rules, branch)
		switch {
		case prot == nil, up.OldSHA == git.ZeroSHA:
			// Unprotected, or creating the branch (e.g. the very first push).
		case up.NewSHA == git.ZeroSHA:
			if !prot.AllowDeletion {
				return fmt.Sprintf("branch %q is protected and cannot be deleted", branch), nil
			}
		case prot.RequirePullRequest:
			return fmt.Sprintf("branch %q is protected: changes must be made through a pull request", branch), nil
		case !prot.AllowForcePush:
			if ok, err := repo.IsAncestor(ctx, up.OldSHA, up.NewSHA); err != nil {
				return "", err
			} else if !ok {
				return fmt.Sprintf("branch %q is protected: force-push is not allowed", branch), nil
			}
		}
	}
	return "", nil
}
