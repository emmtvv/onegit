package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"onegit/internal/pulls"
	"onegit/internal/store"
)

// The Home page: what waits on the signed-in user, across pull requests,
// deployments, CI and the projects they own.

const (
	homeReviews  = 30
	homeOwnPulls = 20
	homeRuns     = 6
	homeProjects = 8
)

// pullStep is one of the user's open PRs with what happens next.
type pullStep struct {
	Pull  *store.Pull
	State string // "ok" (the author can merge), "wait" (on someone else) or "bad" (on the author)
	Text  string
}

// nextStep says what a PR waits for, most pressing first.
func nextStep(p *store.Pull, st *pulls.Status, queued *store.QueueEntry) pullStep {
	s := pullStep{Pull: p}
	set := func(state, format string, args ...any) pullStep {
		s.State, s.Text = state, fmt.Sprintf(format, args...)
		return s
	}
	if queued != nil {
		if queued.State == store.QueueTesting {
			return set("wait", "Being tested in the merge queue")
		}
		return set("wait", "Waiting in the merge queue")
	}
	if !st.HeadExists || !st.BaseExists {
		return set("bad", "%s", st.Blockers[0])
	}
	if len(st.Conflicts) > 0 {
		return set("bad", "Resolve conflicts with %s", p.BaseBranch)
	}
	for _, c := range st.Checks {
		if c.State != "pending" && !c.OK() {
			return set("bad", "Check %s failed", c.Context)
		}
	}
	var asked []string
	for _, v := range st.Verdicts {
		if v.State == store.ReviewChangesRequested {
			asked = append(asked, v.Name)
		}
	}
	if len(asked) > 0 {
		return set("bad", "Changes requested by %s", strings.Join(asked, ", "))
	}
	if st.OwnersState() == "bad" {
		return set("wait", "Waiting for a code owner's review")
	}
	if st.ApprovalsState() == "bad" {
		return set("wait", "Waiting for %s", plural(st.Required-st.Approvals, "more approval"))
	}
	if st.ChecksState() == "wait" {
		return set("wait", "Checks are running")
	}
	if !st.Mergeable() {
		return set("wait", "%s", st.Blockers[0])
	}
	if p.AutoMerge() {
		return set("wait", "Merges automatically")
	}
	if st.QueueRequired {
		return set("ok", "Ready for the merge queue")
	}
	return set("ok", "Ready to merge")
}

func (w *Web) dashboard(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(r)
	teams, err := w.Store.UserTeams(ctx, u.ID)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	mine, err := w.Store.FindPulls(ctx, store.PullFilter{State: "open", AuthorID: u.ID, Page: store.Page{Limit: homeOwnPulls}})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	ids := make([]int64, len(mine))
	for i, p := range mine {
		ids[i] = p.ID
	}
	reviews, err := w.Store.ReviewsForPulls(ctx, ids)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	inbox, err := w.Pulls.ReviewInbox(ctx, u, teams, homeReviews)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	steps := make([]pullStep, 0, len(mine))
	for _, p := range mine {
		st, err := w.Pulls.Status(ctx, p, reviews[p.ID])
		if err != nil {
			w.Log.Warn("pull status for home", "pull", p.ID, "err", err)
			continue
		}
		q, err := w.Store.ActiveQueueEntry(ctx, p.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			w.Log.Warn("queue entry for home", "pull", p.ID, "err", err)
		}
		steps = append(steps, nextStep(p, st, q))
	}
	approvals := w.deploysToApprove(ctx, u)
	runs, err := w.Store.ListRuns(ctx, store.RunFilter{Kind: "pipeline", TriggeredBy: u.ID, Limit: homeRuns})
	if err != nil {
		w.fail(rw, r, err)
		return
	}

	// Projects the user owns, with the same columns as the project list.
	var owned []projectRow
	var ownedTotal int
	if list, st, err := w.Projects.List(ctx); err == nil && len(list) > 0 {
		ref := "refs/heads/" + w.defaultBranch(ctx)
		sha, _ := w.Repo.ResolveCommit(ctx, ref)
		mineP := w.ownedBy(ctx, u, list, sha)
		ownedTotal = len(mineP)
		deploys, _ := w.projectDeploys(ctx, st)
		owned = w.projectRows(ctx, mineP[:min(len(mineP), homeProjects)], sha, ref, deploys)
	}

	var fix, ready int
	for _, s := range steps {
		switch s.State {
		case "bad":
			fix++
		case "ok":
			ready++
		}
	}
	w.render(rw, r, http.StatusOK, "home", &Page{Title: "Home · " + w.Cfg.Repo.Name, Tab: "home", Data: map[string]any{
		"Inbox": inbox, "Steps": steps, "Approvals": approvals, "Runs": runs,
		"Owned": owned, "OwnedTotal": ownedTotal, "Fix": fix, "Ready": ready,
		"Idle": len(inbox) == 0 && len(approvals) == 0 && fix == 0 && ready == 0,
	}})
}

// deploysToApprove lists the pending deployments u may approve and has not
// yet given a verdict on.
func (w *Web) deploysToApprove(ctx context.Context, u *store.User) []deploymentView {
	pending, err := w.Store.ListDeployments(ctx, "pending", 50, 0)
	if err != nil {
		w.Log.Warn("pending deployments for home", "err", err)
		return nil
	}
	var out []*store.Deployment
	for _, d := range pending {
		if d.RequestedBy != nil && *d.RequestedBy == u.ID {
			continue
		}
		given, err := w.Store.DeploymentApprovals(ctx, d.ID)
		if err != nil || slices.ContainsFunc(given, func(a *store.DeploymentApproval) bool { return a.UserID == u.ID }) {
			continue
		}
		ev, err := w.Deploy.Current(ctx, d)
		if err != nil {
			w.Log.Warn("evaluate deployment for home", "deployment", d.ID, "err", err)
			continue
		}
		if w.Deploy.CanApprove(ctx, u, d, ev) {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil
	}
	dims, _ := w.Store.ListDeployDimensions(ctx)
	return w.deploymentViews(dims, out)
}
