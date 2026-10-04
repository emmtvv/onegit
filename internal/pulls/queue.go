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

// The background work of pull requests: changed-file lists for project
// filters, auto-merge and the merge queue. One replica at a time does it.
const backgroundLockID = 0x6f6770756c6c

const backgroundInterval = 15 * time.Second

// maxPullFiles caps the changed files stored per PR.
const maxPullFiles = 20000

func QueueRef(entryID int64) string { return "refs/merge-queue/" + strconv.FormatInt(entryID, 10) }

func (s *Service) kicks() chan struct{} {
	s.kickOnce.Do(func() { s.kick = make(chan struct{}, 1) })
	return s.kick
}

// Kick asks the background loop to run soon (a check finished, a review
// arrived, a PR was queued...).
func (s *Service) Kick() {
	select {
	case s.kicks() <- struct{}{}:
	default:
	}
}

// Background runs Process every few seconds and whenever kicked, until ctx
// is cancelled.
func (s *Service) Background(ctx context.Context) {
	t := time.NewTicker(backgroundInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kicks():
		}
		ran, err := s.Process(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			s.Log.Error("pull request background work", "err", err)
		case !ran:
			// Another replica is at it; look again shortly in case it read
			// the state before the change that kicked us.
			time.AfterFunc(2*time.Second, s.Kick)
		}
	}
}

// Process does one pass of the background work if no other replica is; it
// reports whether it ran.
func (s *Service) Process(ctx context.Context) (bool, error) {
	return s.Store.TryAdvisoryLock(ctx, backgroundLockID, func() error {
		var errs []error
		errs = append(errs, s.backfillFiles(ctx))
		errs = append(errs, s.processAutoMerge(ctx))
		errs = append(errs, s.processQueues(ctx))
		return errors.Join(errs...)
	})
}

// backfillFiles computes changed files for PRs whose head moved.
func (s *Service) backfillFiles(ctx context.Context) error {
	for range 10 {
		list, err := s.Store.PullsNeedingFiles(ctx, 50)
		if err != nil || len(list) == 0 {
			return err
		}
		for _, p := range list {
			files, err := s.Repo.ChangedFiles(ctx, p.MergeBase, p.HeadSHA)
			if err != nil {
				s.Log.Warn("changed files of pull request", "pull", p.ID, "err", err)
				files = nil // record the attempt; objects may be gone
			}
			if len(files) > maxPullFiles {
				files = files[:maxPullFiles]
			}
			if err := s.Store.SetPullFiles(ctx, p.ID, files, p.HeadSHA); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- auto-merge ----

// EnableAutoMerge merges the PR (or puts it into the merge queue) as u once
// every requirement is met.
func (s *Service) EnableAutoMerge(ctx context.Context, id int64, u *store.User, o MergeOptions, deleteBranch bool) error {
	if !u.CanWrite() {
		return userErr("You do not have permission to merge.")
	}
	if !o.Style.Valid() {
		o.Style = StyleSquash
	}
	_, err := s.Store.UpdatePullLocked(ctx, id, func(p *store.Pull) error {
		if !p.IsOpen() {
			return userErr("This pull request is not open.")
		}
		p.AutoMergeBy, p.AutoMergeStyle = &u.ID, string(o.Style)
		p.AutoMergeTitle, p.AutoMergeMessage = strings.TrimSpace(o.Title), strings.TrimSpace(o.Message)
		p.AutoMergeDeleteBranch = deleteBranch
		return nil
	})
	if err != nil {
		return err
	}
	s.Store.AddPullEvent(ctx, id, &u.ID, "auto_merge_enabled", map[string]any{"style": string(o.Style)})
	s.Kick()
	return nil
}

// DisableAutoMerge turns auto-merge off.
func (s *Service) DisableAutoMerge(ctx context.Context, id int64, u *store.User) error {
	if err := s.clearAutoMerge(ctx, id); err != nil {
		return err
	}
	var actor *int64
	if u != nil {
		actor = &u.ID
	}
	return s.Store.AddPullEvent(ctx, id, actor, "auto_merge_disabled", nil)
}

func (s *Service) clearAutoMerge(ctx context.Context, id int64) error {
	_, err := s.Store.UpdatePullLocked(ctx, id, func(p *store.Pull) error {
		if p.AutoMergeBy == nil {
			return errNoChange
		}
		p.AutoMergeBy = nil
		return nil
	})
	if errors.Is(err, errNoChange) {
		return nil
	}
	return err
}

func (s *Service) processAutoMerge(ctx context.Context) error {
	list, err := s.Store.AutoMergePulls(ctx)
	if err != nil {
		return err
	}
	for _, p := range list {
		u, err := s.Store.UserByID(ctx, *p.AutoMergeBy)
		if err != nil || !u.CanWrite() {
			if err := s.clearAutoMerge(ctx, p.ID); err != nil {
				return err
			}
			s.Store.AddPullEvent(ctx, p.ID, nil, "auto_merge_disabled",
				map[string]any{"reason": "the user who enabled it can no longer merge"})
			continue
		}
		reviews, err := s.Store.ListPullReviews(ctx, p.ID)
		if err != nil {
			return err
		}
		st, err := s.Status(ctx, p, reviews)
		if err != nil {
			s.Log.Warn("auto-merge status", "pull", p.ID, "err", err)
			continue
		}
		if !st.Mergeable() {
			continue
		}
		o := MergeOptions{Style: MergeStyle(p.AutoMergeStyle), Title: p.AutoMergeTitle, Message: p.AutoMergeMessage}
		if st.QueueRequired {
			err := s.Enqueue(ctx, p.ID, u, o, p.AutoMergeDeleteBranch)
			var ue *UserError
			switch {
			case errors.As(err, &ue):
				s.Log.Info("auto-merge could not queue", "pull", p.ID, "reason", ue.Msg)
			case err != nil:
				return err
			default:
				if err := s.clearAutoMerge(ctx, p.ID); err != nil {
					return err
				}
			}
			continue
		}
		merged, err := s.Merge(ctx, p.ID, u, o)
		var ue *UserError
		switch {
		case errors.As(err, &ue):
			s.Log.Info("auto-merge waits", "pull", p.ID, "reason", ue.Msg)
			continue
		case err != nil:
			return err
		}
		s.Log.Info("auto-merged", "pull", p.ID, "user", u.Username)
		if p.AutoMergeDeleteBranch {
			if err := s.DeleteHeadBranch(ctx, merged, u); err != nil && !errors.As(err, &ue) {
				return err
			}
		}
	}
	return nil
}

// ---- merge queue ----

// Enqueue puts an open, otherwise mergeable PR into its base branch's merge
// queue.
func (s *Service) Enqueue(ctx context.Context, id int64, u *store.User, o MergeOptions, deleteBranch bool) error {
	if !u.CanWrite() {
		return userErr("You do not have permission to merge.")
	}
	if !o.Style.Valid() {
		o.Style = StyleSquash
	}
	p, err := s.Store.PullByID(ctx, id)
	if err != nil {
		return err
	}
	if !p.IsOpen() {
		return userErr("This pull request is not open.")
	}
	reviews, err := s.Store.ListPullReviews(ctx, id)
	if err != nil {
		return err
	}
	st, err := s.Status(ctx, p, reviews)
	if err != nil {
		return err
	}
	if !st.QueueRequired {
		return userErr("%s does not use a merge queue; merge the pull request directly.", p.BaseBranch)
	}
	if !st.Mergeable() {
		return userErr("Cannot queue: %s", strings.Join(st.Blockers, " "))
	}
	e := &store.QueueEntry{PullID: id, BaseBranch: p.BaseBranch, Style: string(o.Style), Title: strings.TrimSpace(o.Title),
		Message: strings.TrimSpace(o.Message), DeleteBranch: deleteBranch, EnqueuedBy: &u.ID, HeadSHA: p.HeadSHA}
	if err := s.Store.Enqueue(ctx, e); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			return userErr("This pull request is already in the merge queue.")
		}
		return err
	}
	s.Store.AddPullEvent(ctx, id, &u.ID, "queued", map[string]any{"style": string(o.Style)})
	s.Log.Info("pull queued", "pull", id, "user", u.Username, "base", p.BaseBranch)
	s.Kick()
	return nil
}

// Dequeue takes a PR out of the merge queue.
func (s *Service) Dequeue(ctx context.Context, id int64, u *store.User) error {
	p, err := s.Store.PullByID(ctx, id)
	if err != nil {
		return err
	}
	if !u.CanWrite() && !p.IsAuthor(u) {
		return userErr("You cannot remove this pull request from the merge queue.")
	}
	if !s.dequeue(ctx, id, &u.ID, store.QueueRemoved, "removed by "+u.Username) {
		return userErr("This pull request is not in the merge queue.")
	}
	return nil
}

// dequeue ends the PR's active queue entry, if any; the queue behind it is
// rebuilt on the next pass.
func (s *Service) dequeue(ctx context.Context, pullID int64, actor *int64, state, reason string) bool {
	e, err := s.Store.ActiveQueueEntry(ctx, pullID)
	if err != nil {
		return false
	}
	if ok, err := s.Store.FinishQueueEntry(ctx, e.ID, state, reason); err != nil || !ok {
		return false
	}
	s.dropCandidate(ctx, e)
	s.Store.AddPullEvent(ctx, pullID, actor, "dequeued", map[string]any{"reason": reason})
	s.Kick()
	return true
}

func (s *Service) failEntry(ctx context.Context, e *store.QueueEntry, reason string) {
	if ok, err := s.Store.FinishQueueEntry(ctx, e.ID, store.QueueFailed, reason); err != nil || !ok {
		return
	}
	s.dropCandidate(ctx, e)
	s.Store.AddPullEvent(ctx, e.PullID, nil, "queue_failed", map[string]any{"reason": reason})
	s.Log.Info("merge queue: entry failed", "pull", e.PullID, "reason", reason)
}

// dropCandidate stops the checks of an entry's candidate and releases it.
func (s *Service) dropCandidate(ctx context.Context, e *store.QueueEntry) {
	if e.TestSHA == "" {
		return
	}
	if s.CancelChecks != nil {
		s.CancelChecks(ctx, e.TestSHA)
	}
	if err := s.Repo.UpdateRef(ctx, QueueRef(e.ID), git.ZeroSHA, ""); err != nil {
		s.Log.Warn("delete merge queue ref", "entry", e.ID, "err", err)
	}
}

func (s *Service) processQueues(ctx context.Context) error {
	branches, err := s.Store.QueueBranches(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, b := range branches {
		// A few rounds: a failure or a landing changes what the entries
		// behind it are built on.
		for range 5 {
			changed, err := s.queueRound(ctx, b)
			if err != nil {
				errs = append(errs, fmt.Errorf("merge queue %s: %w", b, err))
			}
			if err != nil || !changed {
				break
			}
		}
	}
	return errors.Join(errs...)
}

type queued struct {
	e *store.QueueEntry
	p *store.Pull
}

// queueRound advances one branch's queue: it drops entries whose PR changed,
// (re)builds candidates for the first depth entries, each on top of the one
// before it, fails entries whose checks failed, and lands the longest
// prefix whose last candidate passed (it contains the ones before it). It
// reports whether anything changed.
func (s *Service) queueRound(ctx context.Context, base string) (bool, error) {
	entries, err := s.Store.ActiveQueue(ctx, base)
	if err != nil || len(entries) == 0 {
		return false, err
	}
	prot, err := s.Store.BranchProtectionFor(ctx, base)
	if err != nil {
		return false, err
	}
	depth := 5
	if prot != nil && prot.MergeQueueDepth > 0 {
		depth = prot.MergeQueueDepth
	}
	tip, err := s.Repo.ResolveCommit(ctx, "refs/heads/"+base)
	if err != nil {
		for _, e := range entries {
			s.failEntry(ctx, e, "the base branch "+base+" no longer exists")
		}
		return true, nil
	}

	changed := false
	var list []queued
	for _, e := range entries {
		p, err := s.Store.PullByID(ctx, e.PullID)
		switch {
		case err != nil:
			return changed, err
		case !p.IsOpen():
			s.dequeue(ctx, p.ID, nil, store.QueueRemoved, "the pull request is no longer open")
			changed = true
		case p.HeadSHA != e.HeadSHA:
			s.dequeue(ctx, p.ID, nil, store.QueueRemoved, "new commits were pushed")
			changed = true
		default:
			list = append(list, queued{e, p})
		}
	}

	// Build the chain of candidates.
	prev := tip
	for i, q := range list {
		e := q.e
		if i >= depth {
			if e.State == store.QueueTesting {
				s.dropCandidate(ctx, e)
				if err := s.Store.ResetQueueEntry(ctx, e.ID); err != nil {
					return changed, err
				}
				e.State, e.TestSHA = store.QueueQueued, ""
			}
			continue
		}
		if e.State == store.QueueTesting && e.BaseSHA == prev && e.TestSHA != "" {
			prev = e.TestSHA
			continue
		}
		s.dropCandidate(ctx, e) // built on a base that is gone
		enqueuer, err := s.queueUser(ctx, e)
		if err != nil {
			s.failEntry(ctx, e, err.Error())
			return true, nil
		}
		o := MergeOptions{Style: MergeStyle(e.Style), Title: e.Title, Message: e.Message}
		cand, err := s.buildMerge(ctx, q.p, prev, enqueuer, o)
		if err != nil {
			var ue *UserError
			if errors.As(err, &ue) {
				s.failEntry(ctx, e, "it conflicts with "+base+" and the pull requests ahead of it in the queue")
				return true, nil
			}
			return changed, err
		}
		if err := s.Repo.UpdateRef(ctx, QueueRef(e.ID), cand, ""); err != nil {
			return changed, err
		}
		if err := s.Store.StartQueueTest(ctx, e.ID, prev, cand); err != nil {
			return changed, err
		}
		e.State, e.BaseSHA, e.TestSHA = store.QueueTesting, prev, cand
		s.Log.Info("merge queue: testing", "pull", e.PullID, "candidate", cand, "on", prev)
		if s.StartChecks != nil {
			s.StartChecks(ctx, cand, prev, QueueRef(e.ID), base, e.PullID, e.EnqueuedBy)
		}
		changed = true
		prev = cand
	}

	// Results, in queue order.
	land := -1
	for i, q := range list {
		if i >= depth || q.e.State != store.QueueTesting {
			break
		}
		res, reason, err := s.candidateResult(ctx, q.e, prot)
		if err != nil {
			return changed, err
		}
		if res == resultFail {
			s.failEntry(ctx, q.e, reason)
			return true, nil
		}
		if res == resultPass {
			land = i
		}
	}
	if land < 0 {
		return changed, nil
	}
	// Reviews may have changed while the checks ran.
	for i := 0; i <= land; i++ {
		reviews, err := s.Store.ListPullReviews(ctx, list[i].p.ID)
		if err != nil {
			return changed, err
		}
		st, err := s.status(ctx, list[i].p, reviews, true)
		if err != nil {
			return changed, err
		}
		if !st.Mergeable() {
			s.failEntry(ctx, list[i].e, strings.Join(st.Blockers, " "))
			return true, nil
		}
	}
	return true, s.land(ctx, base, tip, list[:land+1])
}

const (
	resultPending = iota
	resultPass
	resultFail
)

// candidateResult judges a candidate by the branch's required checks, or
// by every check on it when none are required.
func (s *Service) candidateResult(ctx context.Context, e *store.QueueEntry, prot *store.BranchProtection) (int, string, error) {
	statuses, err := s.Store.CommitStatuses(ctx, e.TestSHA)
	if err != nil {
		return resultPending, "", err
	}
	if prot != nil && len(prot.RequiredChecks) > 0 {
		pending := false
		for _, name := range prot.RequiredChecks {
			var found *store.CommitStatus
			for _, c := range statuses {
				if c.Context == name {
					found = c
				}
			}
			switch {
			case found == nil:
				// Checks start synchronously with the candidate: a required
				// check that is not there never will be.
				return resultFail, fmt.Sprintf("required check %q did not run on the merge candidate", name), nil
			case found.State == "pending":
				pending = true
			case !found.OK():
				return resultFail, fmt.Sprintf("required check %q failed on the merge candidate", name), nil
			}
		}
		if pending {
			return resultPending, "", nil
		}
		return resultPass, "", nil
	}
	pending := false
	for _, c := range statuses {
		switch {
		case c.State == "pending":
			pending = true
		case !c.OK():
			return resultFail, fmt.Sprintf("check %q failed on the merge candidate", c.Context), nil
		}
	}
	if pending {
		return resultPending, "", nil
	}
	return resultPass, "", nil
}

// land fast-forwards base from tip to the last candidate and records every
// PR in it as merged.
func (s *Service) land(ctx context.Context, base, tip string, list []queued) error {
	last := list[len(list)-1].e
	if err := s.Repo.UpdateRef(ctx, "refs/heads/"+base, last.TestSHA, tip); err != nil {
		if errors.Is(err, git.ErrRefChanged) {
			return nil // someone pushed: the next round rebuilds on the new tip
		}
		return err
	}
	var by *store.User
	for _, q := range list {
		e := q.e
		u, _ := s.queueUser(ctx, e)
		if u != nil {
			by = u
		}
		_, err := s.Store.UpdatePullLocked(ctx, q.p.ID, func(p *store.Pull) error {
			if !p.IsOpen() {
				return errNoChange
			}
			now := time.Now()
			sha, style := e.TestSHA, e.Style
			p.State, p.MergedAt, p.MergedBy, p.MergeSHA, p.MergeStyle = store.PullMerged, &now, e.EnqueuedBy, &sha, &style
			return nil
		})
		if err != nil && !errors.Is(err, errNoChange) {
			return err
		}
		if _, err := s.Store.FinishQueueEntry(ctx, e.ID, store.QueueMerged, ""); err != nil {
			return err
		}
		if err := s.Repo.UpdateRef(ctx, QueueRef(e.ID), git.ZeroSHA, ""); err != nil {
			s.Log.Warn("delete merge queue ref", "entry", e.ID, "err", err)
		}
		s.Store.AddPullEvent(ctx, q.p.ID, e.EnqueuedBy, "merged", map[string]any{"sha": e.TestSHA, "style": e.Style, "queue": true})
		s.Log.Info("merge queue: merged", "pull", q.p.ID, "base", base, "sha", e.TestSHA)
	}
	s.afterBaseUpdate(ctx, by, []hooks.RefUpdate{{OldSHA: tip, NewSHA: last.TestSHA, Ref: "refs/heads/" + base}})
	for _, q := range list {
		if !q.e.DeleteBranch {
			continue
		}
		u, err := s.queueUser(ctx, q.e)
		if err != nil {
			continue
		}
		p, err := s.Store.PullByID(ctx, q.p.ID)
		if err != nil {
			return err
		}
		var ue *UserError
		if err := s.DeleteHeadBranch(ctx, p, u); err != nil && !errors.As(err, &ue) {
			return err
		}
	}
	return nil
}

func (s *Service) queueUser(ctx context.Context, e *store.QueueEntry) (*store.User, error) {
	if e.EnqueuedBy == nil {
		return nil, errors.New("the user who queued it no longer exists")
	}
	u, err := s.Store.UserByID(ctx, *e.EnqueuedBy)
	if err != nil {
		return nil, errors.New("the user who queued it no longer exists")
	}
	if !u.CanWrite() {
		return nil, errors.New(u.Username + " who queued it can no longer merge")
	}
	return u, nil
}
