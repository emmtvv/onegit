package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"onegit/internal/ci"
	"onegit/internal/store"
)

// ResolveRef turns a branch, tag or commit into a commit id.
func (s *Service) ResolveRef(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	for _, cand := range []string{"refs/heads/" + ref, "refs/tags/" + ref, ref} {
		if sha, err := s.Repo.ResolveCommit(ctx, cand); err == nil {
			return sha, nil
		}
	}
	return "", userErr("%q is not a branch, tag or commit", ref)
}

// Plan evaluates a prospective request without saving it.
func (s *Service) Plan(ctx context.Context, u *store.User, ref string, targets []Target) (*Evaluation, error) {
	if len(targets) == 0 {
		return nil, userErr("Choose at least one target.")
	}
	sha, err := s.ResolveRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	recipeSHA, err := s.Repo.ResolveCommit(ctx, "refs/heads/"+s.DefaultBranch(ctx))
	if err != nil {
		return nil, userErr("The default branch does not exist yet.")
	}
	return s.Evaluate(ctx, u, sha, recipeSHA, targets, nil)
}

// Request creates a deployment. It fails when a hard check fails; otherwise
// it waits for approvals or starts at once.
func (s *Service) Request(ctx context.Context, u *store.User, ref string, targets []Target, comment string) (*store.Deployment, *Evaluation, error) {
	ev, err := s.Plan(ctx, u, ref, targets)
	if err != nil {
		return nil, nil, err
	}
	if ev.Blocked() {
		return nil, ev, userErr("This deployment is not allowed: %s", strings.Join(ev.Problems(), "; "))
	}
	d := &store.Deployment{SHA: ev.SHA, Ref: strings.TrimSpace(ref), RecipeSHA: ev.RecipeSHA, RequestedBy: &u.ID,
		Comment: strings.TrimSpace(comment), Status: "pending", Evaluation: toMap(ev)}
	for _, t := range targets {
		d.Targets = append(d.Targets, t)
	}
	if err := s.Store.CreateDeployment(ctx, d); err != nil {
		return nil, nil, err
	}
	s.Store.Audit(ctx, &u.ID, "deploy.request", "deployment #"+strconv.FormatInt(d.ID, 10),
		map[string]any{"sha": d.SHA, "targets": d.Targets})
	if !ev.ApprovalsPending() {
		d, err = s.start(ctx, d.ID, u)
		return d, ev, err
	}
	return d, ev, nil
}

func targetsOf(d *store.Deployment) []Target {
	out := make([]Target, len(d.Targets))
	for i, t := range d.Targets {
		out[i] = t
	}
	return out
}

// Current re-evaluates a deployment with the approvals given so far.
func (s *Service) Current(ctx context.Context, d *store.Deployment) (*Evaluation, error) {
	requester := &store.User{}
	if d.RequestedBy != nil {
		u, err := s.Store.UserByID(ctx, *d.RequestedBy)
		if err != nil {
			return nil, err
		}
		requester = u
	}
	approvals, err := s.Store.DeploymentApprovals(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	return s.Evaluate(ctx, requester, d.SHA, d.RecipeSHA, targetsOf(d), approvals)
}

// CanApprove reports whether u may give a verdict on a pending deployment.
func (s *Service) CanApprove(ctx context.Context, u *store.User, d *store.Deployment, ev *Evaluation) bool {
	if u == nil || d.Status != "pending" || (d.RequestedBy != nil && *d.RequestedBy == u.ID) {
		return false
	}
	ec, err := s.newEvalContext(ctx, u, d.SHA, d.RecipeSHA)
	if err != nil {
		return false
	}
	for _, n := range ev.Approvals {
		if !n.allowAuthor && ec.authorEmail != "" && strings.EqualFold(u.Email, ec.authorEmail) {
			continue
		}
		if s.eligible(ec, u, ec.userTeams, n.principals, n.targets) {
			return true
		}
	}
	return false
}

// Review records an approval or rejection and starts the deployment once
// every requirement is met.
func (s *Service) Review(ctx context.Context, u *store.User, id int64, approve bool, comment string) (*store.Deployment, error) {
	d, err := s.Store.DeploymentByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.Status != "pending" {
		return nil, userErr("This deployment is no longer waiting for approval.")
	}
	if d.RequestedBy != nil && *d.RequestedBy == u.ID {
		return nil, userErr("You cannot approve your own deployment.")
	}
	ev, err := s.Current(ctx, d)
	if err != nil {
		return nil, err
	}
	if !s.CanApprove(ctx, u, d, ev) {
		return nil, userErr("You are not an approver for this deployment.")
	}
	verdict := "approve"
	if !approve {
		verdict = "reject"
	}
	if err := s.Store.SetDeploymentApproval(ctx, id, u.ID, verdict, strings.TrimSpace(comment)); err != nil {
		return nil, err
	}
	s.Store.Audit(ctx, &u.ID, "deploy."+verdict, "deployment #"+strconv.FormatInt(id, 10), nil)
	if !approve {
		return s.Store.UpdateDeploymentLocked(ctx, id, func(d *store.Deployment) error {
			if d.Status == "pending" {
				d.Status = "rejected"
			}
			return nil
		})
	}
	return s.start(ctx, id, u)
}

// start re-checks everything under the row lock and queues the jobs; it
// leaves the deployment pending while approvals are missing.
func (s *Service) start(ctx context.Context, id int64, actor *store.User) (*store.Deployment, error) {
	var jobs []ci.DeployJob
	var runErr error
	d, err := s.Store.UpdateDeploymentLocked(ctx, id, func(d *store.Deployment) error {
		if d.Status != "pending" {
			return nil
		}
		ev, err := s.Current(ctx, d)
		if err != nil {
			return err
		}
		d.Evaluation = toMap(ev)
		switch {
		case len(ev.RejectedBy) > 0:
			d.Status = "rejected"
			return nil
		case ev.Blocked(), ev.ApprovalsPending():
			return nil
		}
		jobs = jobs[:0]
		for _, t := range ev.Targets {
			jobs = append(jobs, ci.DeployJob{Target: t.Target, Key: t.Key, Label: t.Label, Recipe: t.recipe, Artifact: t.Artifact})
		}
		run, err := s.CI.StartDeployment(ctx, d, jobs)
		if err != nil {
			runErr = err
			return nil
		}
		d.Status, d.RunID = "running", &run.ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	if runErr != nil {
		return d, runErr
	}
	if d.Status == "running" {
		s.Store.Audit(ctx, &actor.ID, "deploy.start", "deployment #"+strconv.FormatInt(id, 10),
			map[string]any{"sha": d.SHA, "run": d.RunID})
	}
	return d, nil
}

// Cancel withdraws a pending deployment or stops a running one.
func (s *Service) Cancel(ctx context.Context, u *store.User, id int64) error {
	d, err := s.Store.DeploymentByID(ctx, id)
	if err != nil {
		return err
	}
	if !(u.IsAdmin() || (d.RequestedBy != nil && *d.RequestedBy == u.ID)) {
		return userErr("Only the requester or an administrator can cancel a deployment.")
	}
	switch d.Status {
	case "pending":
		_, err = s.Store.UpdateDeploymentLocked(ctx, id, func(d *store.Deployment) error {
			if d.Status == "pending" {
				d.Status = "cancelled"
			}
			return nil
		})
	case "running":
		if d.RunID != nil {
			err = s.CI.Cancel(ctx, *d.RunID)
		}
	default:
		return userErr("This deployment has already finished.")
	}
	if err == nil {
		s.Store.Audit(ctx, &u.ID, "deploy.cancel", "deployment #"+strconv.FormatInt(id, 10), nil)
	}
	return err
}

func (s *Service) onRunFinished(ctx context.Context, run *store.Run) {
	if run.DeploymentID == nil {
		return
	}
	_, err := s.Store.UpdateDeploymentLocked(ctx, *run.DeploymentID, func(d *store.Deployment) error {
		if d.Status == "running" {
			d.Status = run.Status
		}
		return nil
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.Log.Error("finish deployment", "deployment", *run.DeploymentID, "err", err)
	}
}

func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}
