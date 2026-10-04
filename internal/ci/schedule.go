package ci

import (
	"context"
	"sort"
	"time"

	"onegit/internal/store"
)

// Advisory lock ids: one replica at a time runs each background task.
const (
	scheduleLockID = 0x6f6763690001
	cleanupLockID  = 0x6f6763690002
)

const cleanupInterval = time.Hour

// Maintenance starts scheduled pipelines and removes expired artifacts and
// stale caches until ctx is cancelled.
func (s *Service) Maintenance(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	var lastCleanup time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.Store.TryAdvisoryLock(ctx, scheduleLockID, func() error { return s.RunSchedules(ctx, time.Now()) }); err != nil && ctx.Err() == nil {
			s.Log.Error("scheduled pipelines", "err", err)
		}
		if time.Since(lastCleanup) >= cleanupInterval {
			lastCleanup = time.Now()
			if _, err := s.Store.TryAdvisoryLock(ctx, cleanupLockID, func() error {
				if err := s.pruneLogs(ctx); err != nil {
					return err
				}
				return s.cleanupFiles(ctx)
			}); err != nil && ctx.Err() == nil {
				s.Log.Error("ci cleanup", "err", err)
			}
		}
	}
}

// Schedule is a schedule trigger of a pipeline on the default branch.
type Schedule struct {
	File, Pipeline, Cron, Timezone string
	Next                           time.Time
}

// Schedules lists the schedule triggers on the default branch with their
// next run.
func (s *Service) Schedules(ctx context.Context) []Schedule {
	sha, err := s.Repo.ResolveCommit(ctx, "refs/heads/"+s.defaultBranch(ctx))
	if err != nil {
		return nil
	}
	pipes, _ := s.LoadPipelines(ctx, sha)
	states, _ := s.Store.ScheduleStates(ctx)
	last := map[[3]string]time.Time{}
	for _, st := range states {
		last[[3]string{st.Pipeline, st.Cron, st.Timezone}] = st.LastAt
	}
	var out []Schedule
	for _, f := range sortedKeys(pipes) {
		for _, sc := range pipes[f].On.Schedule {
			cr, err := ParseCron(sc.Cron, sc.Timezone)
			if err != nil {
				continue
			}
			from := last[[3]string{f, sc.Cron, sc.Timezone}]
			if from.IsZero() || from.Before(time.Now()) {
				from = time.Now()
			}
			out = append(out, Schedule{File: f, Pipeline: pipes[f].Name, Cron: sc.Cron, Timezone: sc.Timezone, Next: cr.Next(from)})
		}
	}
	return out
}

// RunSchedules starts the pipelines whose schedule came due since it last
// fired. Missed times (the server was down) collapse into one run. A
// schedule seen for the first time starts counting from now.
func (s *Service) RunSchedules(ctx context.Context, now time.Time) error {
	def := s.defaultBranch(ctx)
	sha, err := s.Repo.ResolveCommit(ctx, "refs/heads/"+def)
	if err != nil {
		return nil // nothing pushed yet
	}
	pipes, _ := s.LoadPipelines(ctx, sha)
	states, err := s.Store.ScheduleStates(ctx)
	if err != nil {
		return err
	}
	last := map[[3]string]time.Time{}
	for _, st := range states {
		last[[3]string{st.Pipeline, st.Cron, st.Timezone}] = st.LastAt
	}
	now = now.Truncate(time.Microsecond) // Postgres precision, for the compare-and-swap
	for _, f := range sortedKeys(pipes) {
		p := pipes[f]
		for _, sc := range p.On.Schedule {
			cr, err := ParseCron(sc.Cron, sc.Timezone)
			if err != nil {
				continue
			}
			st := &store.ScheduleState{Pipeline: f, Cron: sc.Cron, Timezone: sc.Timezone}
			prev, seen := last[[3]string{f, sc.Cron, sc.Timezone}]
			if !seen {
				if _, err := s.Store.AdvanceSchedule(ctx, st, time.Time{}, now); err != nil {
					return err
				}
				continue
			}
			next := cr.Next(prev)
			if next.IsZero() || next.After(now) {
				continue
			}
			ok, err := s.Store.AdvanceSchedule(ctx, st, prev, now)
			if err != nil {
				return err
			}
			if !ok {
				continue // another replica fired it
			}
			s.Log.Info("scheduled pipeline", "file", f, "cron", sc.Cron, "sha", sha)
			if _, err := s.startPipeline(ctx, p, f, Event{Name: "schedule", Ref: "refs/heads/" + def}, sha, "", nil, nil); err != nil {
				s.Log.Error("start scheduled pipeline", "file", f, "err", err)
			}
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
