// Package pulls implements pull requests: creation, syncing with pushes,
// merge checks (conflicts, reviews, branch protection, CODEOWNERS) and
// server-side merging without a worktree.
package pulls

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"onegit/internal/git"
	"onegit/internal/hooks"
	"onegit/internal/kv"
	"onegit/internal/store"
)

type Service struct {
	Store   *store.Store
	Repo    *git.Repo
	KV      *kv.KV
	Log     *slog.Logger
	BaseURL string
}

// UserError is a validation failure whose message is safe to show.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func userErr(format string, args ...any) error { return &UserError{Msg: fmt.Sprintf(format, args...)} }

func PullRef(id int64) string { return "refs/pull/" + strconv.FormatInt(id, 10) + "/head" }

// Create opens a PR from head into base.
func (s *Service) Create(ctx context.Context, author *store.User, head, base, title, body string) (*store.Pull, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, userErr("Title is required.")
	}
	if head == base {
		return nil, userErr("Choose two different branches.")
	}
	headSHA, err := s.Repo.ResolveCommit(ctx, "refs/heads/"+head)
	if err != nil {
		return nil, userErr("Branch %q does not exist.", head)
	}
	baseSHA, err := s.Repo.ResolveCommit(ctx, "refs/heads/"+base)
	if err != nil {
		return nil, userErr("Branch %q does not exist.", base)
	}
	if ok, _ := s.Repo.IsAncestor(ctx, headSHA, baseSHA); ok {
		return nil, userErr("%s has no commits that are not already in %s.", head, base)
	}
	mb, err := s.Repo.MergeBase(ctx, baseSHA, headSHA)
	if err != nil {
		return nil, userErr("%s and %s have no common history.", head, base)
	}
	// Checked up front so failed inserts don't burn PR numbers.
	if _, err := s.Store.OpenPullFor(ctx, head, base); err == nil {
		return nil, userErr("A pull request for %s → %s is already open.", head, base)
	}
	p := &store.Pull{
		Title: title, Body: strings.TrimSpace(body), AuthorID: &author.ID,
		HeadBranch: head, BaseBranch: base, HeadSHA: headSHA, MergeBase: mb,
	}
	if err := s.Store.CreatePull(ctx, p); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			return nil, userErr("A pull request for %s → %s is already open.", head, base)
		}
		return nil, err
	}
	if err := s.Repo.UpdateRef(ctx, PullRef(p.ID), headSHA, ""); err != nil {
		s.Log.Warn("update pull ref", "pull", p.ID, "err", err)
	}
	return s.Store.PullByID(ctx, p.ID)
}

// SyncPush updates open PRs after branches changed: new head commits, base
// moves, deleted branches, and PRs merged by a plain push to their base.
// pusher may be nil for server-side changes.
func (s *Service) SyncPush(ctx context.Context, pusher *store.User, updates []hooks.RefUpdate) {
	var actor *int64
	if pusher != nil {
		actor = &pusher.ID
	}
	for _, up := range updates {
		branch, ok := strings.CutPrefix(up.Ref, "refs/heads/")
		if !ok {
			continue
		}
		pulls, err := s.Store.OpenPullsForBranch(ctx, branch)
		if err != nil {
			s.Log.Error("sync pulls", "branch", branch, "err", err)
			continue
		}
		for _, p := range pulls {
			if err := s.syncOne(ctx, p.ID, branch, up, actor); err != nil {
				s.Log.Error("sync pull", "pull", p.ID, "err", err)
			}
		}
	}
}

func (s *Service) syncOne(ctx context.Context, id int64, branch string, up hooks.RefUpdate, actor *int64) error {
	var event string
	var data map[string]any
	_, err := s.Store.UpdatePullLocked(ctx, id, func(p *store.Pull) error {
		event, data = "", nil
		if !p.IsOpen() {
			return errNoChange
		}
		deleted := up.NewSHA == git.ZeroSHA
		switch {
		case p.HeadBranch == branch && deleted:
			event = "head_deleted"
			return errNoChange
		case p.HeadBranch == branch:
			if p.HeadSHA == up.NewSHA {
				return errNoChange
			}
			forced := false
			if ok, _ := s.Repo.IsAncestor(ctx, p.HeadSHA, up.NewSHA); !ok {
				forced = true
			}
			count := 0
			if !forced {
				if cs, err := s.Repo.Log(ctx, p.HeadSHA+".."+up.NewSHA, git.LogOptions{Limit: 1000}); err == nil {
					count = len(cs)
				}
			}
			event, data = "pushed", map[string]any{"from": p.HeadSHA, "to": up.NewSHA, "forced": forced, "count": count}
			p.HeadSHA = up.NewSHA
			if err := s.Repo.UpdateRef(ctx, PullRef(p.ID), p.HeadSHA, ""); err != nil {
				s.Log.Warn("update pull ref", "pull", p.ID, "err", err)
			}
		case p.BaseBranch == branch && deleted:
			return errNoChange
		default: // base moved
			if ok, _ := s.Repo.IsAncestor(ctx, p.HeadSHA, up.NewSHA); ok {
				// Head is now contained in base: merged outside onegit.
				now := time.Now()
				p.State, p.MergedAt, p.MergedBy = store.PullMerged, &now, actor
				sha, style := up.NewSHA, "manual"
				p.MergeSHA, p.MergeStyle = &sha, &style
				event, data = "merged", map[string]any{"sha": sha, "style": style}
			}
		}
		if mb, err := s.Repo.MergeBase(ctx, "refs/heads/"+p.BaseBranch, p.HeadSHA); err == nil && p.IsOpen() {
			p.MergeBase = mb
		}
		return nil
	})
	if err != nil && !errors.Is(err, errNoChange) {
		return err
	}
	if event != "" {
		return s.Store.AddPullEvent(ctx, id, actor, event, data)
	}
	return nil
}

var errNoChange = errors.New("no change")

// SetState closes or reopens a PR.
func (s *Service) SetState(ctx context.Context, id int64, u *store.User, open bool) error {
	_, err := s.Store.UpdatePullLocked(ctx, id, func(p *store.Pull) error {
		switch {
		case p.IsMerged():
			return userErr("This pull request is already merged.")
		case open && p.IsOpen(), !open && !p.IsOpen():
			return errNoChange
		}
		if open {
			head, err := s.Repo.ResolveCommit(ctx, "refs/heads/"+p.HeadBranch)
			if err != nil {
				return userErr("Branch %s no longer exists.", p.HeadBranch)
			}
			if _, err := s.Repo.ResolveCommit(ctx, "refs/heads/"+p.BaseBranch); err != nil {
				return userErr("Branch %s no longer exists.", p.BaseBranch)
			}
			p.HeadSHA = head
			if mb, err := s.Repo.MergeBase(ctx, "refs/heads/"+p.BaseBranch, head); err == nil {
				p.MergeBase = mb
			}
			p.State, p.ClosedAt = store.PullOpen, nil
			if err := s.Repo.UpdateRef(ctx, PullRef(p.ID), head, ""); err != nil {
				s.Log.Warn("update pull ref", "pull", p.ID, "err", err)
			}
		} else {
			now := time.Now()
			p.State, p.ClosedAt = store.PullClosed, &now
		}
		return nil
	})
	switch {
	case errors.Is(err, errNoChange):
		return nil
	case errors.Is(err, store.ErrDuplicate):
		return userErr("Another pull request for the same branches is already open.")
	case err != nil:
		return err
	}
	kind := "closed"
	if open {
		kind = "reopened"
	}
	return s.Store.AddPullEvent(ctx, id, &u.ID, kind, nil)
}

// CreateURL is the "open a pull request" link printed after a push.
func (s *Service) CreateURL(branch string) string {
	return s.BaseURL + "/pulls/new?head=" + url.QueryEscape(branch)
}

func (s *Service) PullURL(id int64) string {
	return s.BaseURL + "/pulls/" + strconv.FormatInt(id, 10)
}

func signature(u *store.User, host string) git.Signature {
	email := u.Email
	if email == "" {
		email = u.Username + "@users.noreply." + host
	}
	return git.Signature{Name: u.DisplayName(), Email: email, When: time.Now()}
}

func (s *Service) host() string {
	if u, err := url.Parse(s.BaseURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return "onegit.local"
}
