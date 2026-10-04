package ci

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"onegit/internal/auth"
	"onegit/internal/blob"
	"onegit/internal/store"
)

// Artifacts and caches travel as tar.gz archives between runners and S3:
//
//	PUT /api/runner/v1/jobs/{id}/artifacts         the job's artifacts
//	GET /api/runner/v1/jobs/{id}/artifacts/{aid}   an artifact of the same run
//	GET /api/runner/v1/jobs/{id}/cache?key=&prefix= find a cache (JSON) or 204
//	GET /api/runner/v1/jobs/{id}/cache/{cid}       a cache the job may read
//	PUT /api/runner/v1/jobs/{id}/cache?key=        save a cache in the job's scope

// ArtifactRef is an artifact a job downloads before its steps.
type ArtifactRef struct {
	ID   int64  `json:"id"`
	Job  string `json:"job"`
	Size int64  `json:"size"`
}

// CacheHit answers a cache lookup.
type CacheHit struct {
	ID   int64  `json:"id"`
	Key  string `json:"key"`
	Size int64  `json:"size"`
}

const archiveType = "application/gzip"

// downloadsFor lists the artifacts of the successful jobs that j needs.
func (s *Service) downloadsFor(ctx context.Context, j *store.Job) ([]ArtifactRef, error) {
	if len(j.Needs) == 0 {
		return nil, nil
	}
	arts, err := s.Store.ArtifactsForRun(ctx, j.RunID)
	if err != nil || len(arts) == 0 {
		return nil, err
	}
	jobs, err := s.Store.JobsForRun(ctx, j.RunID)
	if err != nil {
		return nil, err
	}
	needed := map[int64]bool{}
	for _, o := range jobs {
		if o.Status == store.JobSuccess && contains(j.Needs, o.Base()) {
			needed[o.ID] = true
		}
	}
	var out []ArtifactRef
	for _, a := range arts {
		if needed[a.JobID] && !a.Expired() {
			out = append(out, ArtifactRef{ID: a.ID, Job: a.JobName, Size: a.Size})
		}
	}
	return out, nil
}

// cacheScopes are the scopes a job reads caches from: its own ref first,
// then the default branch.
func (s *Service) cacheScopes(ctx context.Context, run *store.Run) []string {
	def := "refs/heads/" + s.defaultBranch(ctx)
	if run.Ref == def {
		return []string{def}
	}
	return []string{run.Ref, def}
}

func (s *Service) noBlob(w http.ResponseWriter) bool {
	if s.Blob == nil {
		http.Error(w, "artifact storage is not configured", http.StatusServiceUnavailable)
		return true
	}
	return false
}

// receive streams the request body into S3, enforcing max (0 = unlimited).
func (s *Service) receive(w http.ResponseWriter, r *http.Request, key string, max int64) (int64, bool) {
	body := r.Body
	if max > 0 {
		if r.ContentLength > max {
			http.Error(w, "archive is larger than "+strconv.FormatInt(max, 10)+" bytes", http.StatusRequestEntityTooLarge)
			return 0, false
		}
		body = http.MaxBytesReader(w, r.Body, max)
	}
	cr := &countingReader{r: body}
	size := r.ContentLength
	if size <= 0 {
		size = -1
	}
	if err := s.Blob.Put(r.Context(), key, cr, size, archiveType); err != nil {
		_ = s.Blob.Delete(context.WithoutCancel(r.Context()), key)
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "archive is larger than "+strconv.FormatInt(max, 10)+" bytes", http.StatusRequestEntityTooLarge)
			return 0, false
		}
		s.Log.Error("store archive", "key", key, "err", err)
		http.Error(w, "could not store the archive", http.StatusInternalServerError)
		return 0, false
	}
	return cr.n, true
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (s *Service) send(w http.ResponseWriter, r *http.Request, key string) {
	rc, size, err := s.Blob.Get(r.Context(), key)
	if errors.Is(err, blob.ErrNotFound) {
		http.Error(w, "archive is gone", http.StatusGone)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", archiveType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	io.Copy(w, rc)
}

func (s *Service) uploadArtifacts(w http.ResponseWriter, r *http.Request, j *store.Job) {
	if s.noBlob(w) {
		return
	}
	var p JobPayload
	if json.Unmarshal(j.Spec, &p) != nil || p.Artifacts == nil {
		http.Error(w, "this job declares no artifacts", http.StatusBadRequest)
		return
	}
	key := "ci/artifacts/" + strconv.FormatInt(j.RunID, 10) + "/" + strconv.FormatInt(j.ID, 10) + "-" + auth.RandomString(8) + ".tar.gz"
	size, ok := s.receive(w, r, key, s.Cfg.CI.MaxArtifactBytes)
	if !ok {
		return
	}
	keep := time.Duration(p.Artifacts.ExpireIn)
	if keep <= 0 {
		keep = time.Duration(s.Cfg.CI.ArtifactRetentionDays) * 24 * time.Hour
	}
	a := &store.Artifact{JobID: j.ID, RunID: j.RunID, Size: size, BlobKey: key, ExpiresAt: time.Now().Add(keep)}
	old, err := s.Store.SaveArtifact(r.Context(), a)
	if err != nil {
		_ = s.Blob.Delete(context.WithoutCancel(r.Context()), key)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if old != "" {
		_ = s.Blob.Delete(r.Context(), old)
	}
	writeJSON(w, http.StatusOK, ArtifactRef{ID: a.ID, Job: j.Name, Size: size})
}

func (s *Service) downloadArtifact(w http.ResponseWriter, r *http.Request, j *store.Job) {
	if s.noBlob(w) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("aid"), 10, 64)
	a, err := s.Store.ArtifactByID(r.Context(), id)
	if err != nil || a.RunID != j.RunID {
		http.Error(w, "no such artifact in this run", http.StatusNotFound)
		return
	}
	s.send(w, r, a.BlobKey)
}

func (s *Service) findCache(w http.ResponseWriter, r *http.Request, j *store.Job) {
	run, err := s.Store.RunByID(r.Context(), j.RunID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	c, err := s.Store.FindCache(r.Context(), s.cacheScopes(r.Context(), run), q.Get("key"), q.Get("prefix"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		w.WriteHeader(http.StatusNoContent)
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, CacheHit{ID: c.ID, Key: c.Key, Size: c.Size})
	}
}

func (s *Service) downloadCache(w http.ResponseWriter, r *http.Request, j *store.Job) {
	if s.noBlob(w) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	c, err := s.Store.CacheByID(r.Context(), id)
	run, rerr := s.Store.RunByID(r.Context(), j.RunID)
	if err != nil || rerr != nil || !contains(s.cacheScopes(r.Context(), run), c.Scope) {
		http.Error(w, "no such cache for this job", http.StatusNotFound)
		return
	}
	s.send(w, r, c.BlobKey)
}

func (s *Service) uploadCache(w http.ResponseWriter, r *http.Request, j *store.Job) {
	if s.noBlob(w) {
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" || len(key) > 512 {
		http.Error(w, "a cache key is required", http.StatusBadRequest)
		return
	}
	run, err := s.Store.RunByID(r.Context(), j.RunID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	blobKey := "ci/cache/" + auth.RandomString(16) + ".tar.gz"
	size, ok := s.receive(w, r, blobKey, s.Cfg.CI.MaxCacheBytes)
	if !ok {
		return
	}
	c := &store.Cache{Scope: run.Ref, Key: key, Size: size, BlobKey: blobKey}
	switch err := s.Store.SaveCache(r.Context(), c); {
	case errors.Is(err, store.ErrDuplicate):
		_ = s.Blob.Delete(r.Context(), blobKey)
		w.WriteHeader(http.StatusConflict)
	case err != nil:
		_ = s.Blob.Delete(context.WithoutCancel(r.Context()), blobKey)
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, CacheHit{ID: c.ID, Key: c.Key, Size: c.Size})
	}
}

// cleanupFiles deletes expired artifacts and evicts caches.
func (s *Service) cleanupFiles(ctx context.Context) error {
	if s.Blob == nil {
		return nil
	}
	for {
		keys, err := s.Store.ExpiredArtifacts(ctx, 500)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := s.Blob.Delete(ctx, k); err != nil {
				s.Log.Warn("delete artifact", "key", k, "err", err)
			}
		}
		if len(keys) < 500 {
			break
		}
	}
	keys, err := s.Store.EvictCaches(ctx, time.Duration(s.Cfg.CI.CacheRetentionDays)*24*time.Hour, s.Cfg.CI.MaxCacheBytes)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := s.Blob.Delete(ctx, k); err != nil {
			s.Log.Warn("delete cache", "key", k, "err", err)
		}
	}
	return nil
}
