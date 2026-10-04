package server

import (
	"context"
	"time"
)

const (
	repoMaintenanceInterval = time.Hour
	repoMaintenanceLockID   = 0x6f6e656769742d6d // pg advisory lock: one replica maintains the shared repository
)

// repoMaintenanceLoop runs git housekeeping in the background, on whichever
// replica gets the lock.
func (s *Server) repoMaintenanceLoop(ctx context.Context) {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		start := time.Now()
		ran, err := s.Store.TryAdvisoryLock(ctx, repoMaintenanceLockID, func() error {
			if s.Repo.IsEmpty(ctx) {
				return nil
			}
			return s.Repo.Maintain(ctx)
		})
		switch {
		case err != nil && ctx.Err() == nil:
			s.Log.Error("repository maintenance failed", "err", err)
		case ran:
			s.Log.Info("repository maintenance", "took", time.Since(start).Round(time.Millisecond))
		}
		t.Reset(repoMaintenanceInterval)
	}
}
