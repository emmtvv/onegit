package ci

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"onegit/internal/store"
)

// DeployJob is one target of an approved deployment.
type DeployJob struct {
	Target   map[string]string
	Key      string
	Label    string
	Recipe   *Recipe
	Artifact string // image@digest pinned at approval time, if the recipe has one
}

// StartDeployment queues one deploy job per target. Jobs of the same target
// never run concurrently (see store.ClaimJob); different targets run in
// parallel on whichever deploy runners accept them.
func (s *Service) StartDeployment(ctx context.Context, d *store.Deployment, targets []DeployJob) (*store.Run, error) {
	run := &store.Run{Kind: "deploy", Name: "Deployment #" + strconv.FormatInt(d.ID, 10), Event: "deploy",
		Ref: d.Ref, SHA: d.SHA, DeploymentID: &d.ID, TriggeredBy: d.RequestedBy, Status: store.JobQueued}
	var jobs []*store.Job
	for _, t := range targets {
		r := t.Recipe
		timeout := time.Duration(r.Timeout)
		if timeout <= 0 {
			timeout = defaultTimeout
		}
		env := merge(r.Env)
		if t.Artifact != "" {
			env["ONEGIT_ARTIFACT"] = t.Artifact
		}
		spec, err := json.Marshal(JobPayload{Steps: r.Steps, Env: env, TimeoutSec: int(timeout.Seconds()),
			RecipeFile: r.File, RecipeSHA: d.RecipeSHA, Tooling: r.Tooling})
		if err != nil {
			return nil, err
		}
		key := t.Key
		jobs = append(jobs, &store.Job{Name: t.Label, Kind: "deploy", RunsOn: r.RunsOn, Spec: spec,
			Target: t.Target, TargetKey: &key, Status: store.JobQueued})
	}
	if err := s.Store.CreateRun(ctx, run, jobs); err != nil {
		return nil, err
	}
	return run, nil
}
