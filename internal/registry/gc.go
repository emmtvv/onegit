package registry

import (
	"context"
	"time"
)

const (
	// GCGrace protects fresh blobs and uploads: a push uploads layers before
	// the manifest that references them.
	GCGrace    = 24 * time.Hour
	gcInterval = 6 * time.Hour
	gcLockID   = 0x6f6e656769742d67 // pg advisory lock: one replica collects at a time
)

type GCStats struct {
	Uploads    int   // stale uploads removed
	Blobs      int   // unreferenced blobs removed
	BytesFreed int64 // size of removed blobs
}

// GC removes uploads idle for longer than grace and blobs that no manifest
// references and nobody touched within grace. Manifests are never collected
// automatically: deleting a tag in the UI deletes its version explicitly.
func (s *Service) GC(ctx context.Context, grace time.Duration) (GCStats, error) {
	var st GCStats
	ids, err := s.Store.DeleteStaleRegistryUploads(ctx, grace)
	if err != nil {
		return st, err
	}
	for _, id := range ids {
		if err := s.Blob.DeletePrefix(ctx, uploadPrefix(id)); err != nil {
			return st, err
		}
		st.Uploads++
	}
	digests, err := s.Store.OrphanRegistryBlobs(ctx, grace)
	if err != nil {
		return st, err
	}
	for _, d := range digests {
		b, err := s.Store.RegistryBlob(ctx, d)
		if err != nil {
			continue
		}
		key, err := s.Store.DeleteOrphanRegistryBlob(ctx, d, grace)
		if err != nil {
			return st, err
		}
		if key == "" {
			continue // referenced meanwhile
		}
		if err := s.Blob.Delete(ctx, key); err != nil {
			s.Log.Warn("registry gc: delete object", "key", key, "err", err)
		}
		st.Blobs++
		st.BytesFreed += b.Size
	}
	return st, nil
}

// GCLoop periodically applies the cleanup rule and then runs GC, on
// whichever replica gets the lock.
func (s *Service) GCLoop(ctx context.Context) {
	t := time.NewTimer(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var st GCStats
		var removed int
		ran, err := s.Store.TryAdvisoryLock(ctx, gcLockID, func() (err error) {
			if removed, err = s.RunCleanup(ctx, false); err != nil {
				return err
			}
			st, err = s.GC(ctx, GCGrace)
			return err
		})
		switch {
		case err != nil && ctx.Err() == nil:
			s.Log.Error("registry cleanup/gc failed", "err", err)
		case ran && (removed > 0 || st.Uploads > 0 || st.Blobs > 0):
			s.Log.Info("registry gc", "versions", removed, "uploads", st.Uploads, "blobs", st.Blobs, "bytes_freed", st.BytesFreed)
		}
		t.Reset(gcInterval)
	}
}
