package registry

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"onegit/internal/blob"
	"onegit/internal/store"
)

// Upload parts and assembled blobs are stored under objectsPrefix/<upload id>/.
// A single-part upload's only part becomes the blob's object as is.
const objectsPrefix = "registry/objects/"

func uploadPrefix(id string) string { return objectsPrefix + id + "/" }

func newObjectKey(uploadID, kind string) string {
	return uploadPrefix(uploadID) + kind + "-" + strings.ToLower(rand.Text())
}

func (s *Service) getBlob(w http.ResponseWriter, r *http.Request, digest string) {
	if !validDigest(digest) {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "unsupported digest", map[string]string{"digest": digest})
		return
	}
	b, err := s.Store.RegistryBlob(r.Context(), digest)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry", map[string]string{"digest": digest})
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Docker-Content-Digest", digest)
	h.Set("ETag", `"`+digest+`"`)
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "max-age=31536000, immutable")
	if r.Method == http.MethodHead {
		// Clients check existence before pushing a layer and skip the upload
		// when it exists; keep the blob safe from garbage collection.
		s.Store.TouchRegistryBlob(r.Context(), digest)
		h.Set("Content-Length", strconv.FormatInt(b.Size, 10))
		return
	}
	obj, err := s.Blob.Open(r.Context(), b.S3Key)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer obj.Close()
	http.ServeContent(w, r, "", time.Time{}, obj) // handles Range requests
}

// ---- uploads ----

func uploadLocation(name, id string) string { return "/v2/" + name + "/blobs/uploads/" + id }

func writeUploadState(w http.ResponseWriter, name string, u *store.RegistryUpload, status int) {
	h := w.Header()
	h.Set("Location", uploadLocation(name, u.ID))
	h.Set("Docker-Upload-UUID", u.ID)
	h.Set("Range", "0-"+strconv.FormatInt(max(u.Size-1, 0), 10))
	h.Set("Content-Length", "0")
	w.WriteHeader(status)
}

func writeBlobCreated(w http.ResponseWriter, name, digest string) {
	h := w.Header()
	h.Set("Location", "/v2/"+name+"/blobs/"+digest)
	h.Set("Docker-Content-Digest", digest)
	h.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

// startUpload handles POST /blobs/uploads/: cross-repository mounts (blobs
// are shared, so any known digest mounts), monolithic uploads (?digest=)
// and the start of a chunked upload.
func (s *Service) startUpload(w http.ResponseWriter, r *http.Request, u *store.User, name string) {
	ctx := r.Context()
	q := r.URL.Query()
	if mount := q.Get("mount"); validDigest(mount) {
		if _, err := s.Store.RegistryBlob(ctx, mount); err == nil {
			s.Store.TouchRegistryBlob(ctx, mount)
			writeBlobCreated(w, name, mount)
			return
		}
	}
	if !s.checkQuota(w, r, 0) {
		return
	}
	state, _ := sha256.New().(encoding.BinaryMarshaler).MarshalBinary()
	up := &store.RegistryUpload{ID: strings.ToLower(rand.Text()), Repo: name, HashState: state, UserID: userRef(u)}
	if err := s.Store.CreateRegistryUpload(ctx, up); err != nil {
		s.internalError(w, r, err)
		return
	}
	if digest := q.Get("digest"); digest != "" {
		s.complete(w, r, name, up, digest)
		return
	}
	writeUploadState(w, name, up, http.StatusAccepted)
}

func (s *Service) loadUpload(w http.ResponseWriter, r *http.Request, name, id string) *store.RegistryUpload {
	up, err := s.Store.RegistryUpload(r.Context(), id)
	if err == nil && up.Repo == name {
		return up
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, err)
		return nil
	}
	writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "blob upload unknown to registry", nil)
	return nil
}

func (s *Service) uploadStatus(w http.ResponseWriter, r *http.Request, name, id string) {
	if up := s.loadUpload(w, r, name, id); up != nil {
		writeUploadState(w, name, up, http.StatusNoContent)
	}
}

func (s *Service) patchUpload(w http.ResponseWriter, r *http.Request, name, id string) {
	up := s.loadUpload(w, r, name, id)
	if up == nil {
		return
	}
	if !s.appendBody(w, r, up) {
		return
	}
	writeUploadState(w, name, up, http.StatusAccepted)
}

// finishUpload handles PUT ?digest=, optionally with a final chunk.
func (s *Service) finishUpload(w http.ResponseWriter, r *http.Request, name, id string) {
	if up := s.loadUpload(w, r, name, id); up != nil {
		s.complete(w, r, name, up, r.URL.Query().Get("digest"))
	}
}

func (s *Service) cancelUpload(w http.ResponseWriter, r *http.Request, name, id string) {
	up := s.loadUpload(w, r, name, id)
	if up == nil {
		return
	}
	if ok, err := s.Store.DeleteRegistryUpload(r.Context(), up.ID); err != nil {
		s.internalError(w, r, err)
		return
	} else if ok {
		s.deleteObjects(uploadPrefix(up.ID))
	}
	w.WriteHeader(http.StatusNoContent)
}

var (
	errUploadMoved = errors.New("upload changed concurrently")
	errTooLarge    = errors.New("blob exceeds the registry size limit")
)

// appendBody stores the request body as the next part of the upload. It
// honours Content-Range (the chunk must start at the current offset). On
// failure the response has been written.
func (s *Service) appendBody(w http.ResponseWriter, r *http.Request, up *store.RegistryUpload) bool {
	if cr := r.Header.Get("Content-Range"); cr != "" {
		startStr, _, _ := strings.Cut(strings.TrimPrefix(cr, "bytes="), "-")
		if start, err := strconv.ParseInt(startStr, 10, 64); err != nil || start != up.Size {
			writeUploadRangeError(w, up)
			return false
		}
	}
	err := s.appendPart(r.Context(), up, r.Body, r.ContentLength)
	switch {
	case errors.Is(err, errTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", err.Error(),
			map[string]int64{"limit": s.Cfg.Registry.MaxBlobBytes})
		return false
	case errors.Is(err, errUploadMoved):
		writeUploadRangeError(w, up)
		return false
	case err != nil:
		s.internalError(w, r, err)
		return false
	}
	return true
}

func writeUploadRangeError(w http.ResponseWriter, up *store.RegistryUpload) {
	w.Header().Set("Range", "0-"+strconv.FormatInt(max(up.Size-1, 0), 10))
	writeError(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", "chunk out of order", nil)
}

func restoreHash(state []byte) (hash.Hash, error) {
	h := sha256.New()
	return h, h.(encoding.BinaryUnmarshaler).UnmarshalBinary(state)
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

// appendPart streams body into a new S3 object while updating the running
// hash, then records it with a compare-and-swap on the upload size.
func (s *Service) appendPart(ctx context.Context, up *store.RegistryUpload, body io.Reader, length int64) error {
	if length == 0 {
		return nil
	}
	limit := s.Cfg.Registry.MaxBlobBytes
	if limit > 0 {
		if length > 0 && up.Size+length > limit {
			return errTooLarge
		}
		// Stop reading one byte past the limit; the check below rejects it.
		body = io.LimitReader(body, limit-up.Size+1)
	}
	h, err := restoreHash(up.HashState)
	if err != nil {
		return err
	}
	if length < 0 {
		// Peek so an empty chunked body does not create an object.
		var first [1]byte
		n, err := io.ReadFull(body, first[:])
		if n == 0 {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		body = io.MultiReader(bytes.NewReader(first[:n]), body)
	}
	cr := &countingReader{r: io.TeeReader(body, h)}
	key := newObjectKey(up.ID, "part")
	if err := s.Blob.Put(ctx, key, cr, length, "application/octet-stream"); err != nil {
		s.deleteObjects(key)
		return err
	}
	if limit > 0 && up.Size+cr.n > limit {
		s.deleteObjects(key)
		return errTooLarge
	}
	state, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return err
	}
	ok, err := s.Store.AdvanceRegistryUpload(ctx, up, key, up.Size+cr.n, state)
	if err != nil || !ok {
		s.deleteObjects(key)
		if err == nil {
			err = errUploadMoved
		}
		return err
	}
	return nil
}

// complete appends the request body (if any), verifies the digest and turns
// the upload into a blob.
func (s *Service) complete(w http.ResponseWriter, r *http.Request, name string, up *store.RegistryUpload, digest string) {
	ctx := r.Context()
	if !validDigest(digest) {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "a sha256 digest is required", map[string]string{"digest": digest})
		return
	}
	if !s.appendBody(w, r, up) {
		return
	}
	h, err := restoreHash(up.HashState)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != digest {
		if ok, _ := s.Store.DeleteRegistryUpload(ctx, up.ID); ok {
			s.deleteObjects(uploadPrefix(up.ID))
		}
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "content does not match digest",
			map[string]string{"digest": digest, "actual": got})
		return
	}

	if _, err := s.Store.RegistryBlob(ctx, digest); errors.Is(err, store.ErrNotFound) && !s.checkQuota(w, r, up.Size) {
		if ok, _ := s.Store.DeleteRegistryUpload(ctx, up.ID); ok {
			s.deleteObjects(uploadPrefix(up.ID))
		}
		return
	}

	var key string
	switch len(up.PartKeys) {
	case 1:
		key = up.PartKeys[0]
	default:
		// No parts (an empty blob) or several: write one object.
		key = newObjectKey(up.ID, "blob")
		if err := s.concat(ctx, key, up); err != nil {
			s.deleteObjects(key)
			s.internalError(w, r, err)
			return
		}
	}
	used, err := s.Store.FinishRegistryUpload(ctx, up, &store.RegistryBlob{Digest: digest, Size: up.Size, S3Key: key})
	if errors.Is(err, store.ErrNotFound) {
		if len(up.PartKeys) != 1 {
			s.deleteObjects(key)
		}
		writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload was cancelled or changed concurrently", nil)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	// Drop the parts that did not become the blob (and our copy when the
	// blob already existed).
	var garbage []string
	for _, k := range append(up.PartKeys, key) {
		if k != used {
			garbage = append(garbage, k)
		}
	}
	s.deleteObjects(garbage...)
	writeBlobCreated(w, name, digest)
}

// checkQuota rejects the request when adding size bytes would exceed the
// total registry size limit.
func (s *Service) checkQuota(w http.ResponseWriter, r *http.Request, size int64) bool {
	limit := s.Cfg.Registry.MaxTotalBytes
	if limit <= 0 {
		return true
	}
	total, err := s.Store.RegistryTotalSize(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return false
	}
	if total+size > limit || (size == 0 && total >= limit) {
		writeError(w, http.StatusRequestEntityTooLarge, "DENIED", "registry storage quota exceeded",
			map[string]int64{"limit": limit, "used": total})
		return false
	}
	return true
}

func (s *Service) concat(ctx context.Context, key string, up *store.RegistryUpload) error {
	pr, pw := io.Pipe()
	go func() {
		for _, k := range up.PartKeys {
			rc, _, err := s.Blob.Get(ctx, k)
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			_, err = io.Copy(pw, rc)
			rc.Close()
			if err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.Close()
	}()
	err := s.Blob.Put(ctx, key, pr, up.Size, "application/octet-stream")
	pr.CloseWithError(err)
	return err
}

// deleteObjects removes S3 objects (keys ending in "/" are prefixes) in the
// background: cleanup must not fail or delay the request, and orphans left
// behind by a crash are collected with their stale upload.
func (s *Service) deleteObjects(keys ...string) {
	if len(keys) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		for _, k := range keys {
			var err error
			if strings.HasSuffix(k, "/") {
				err = s.Blob.DeletePrefix(ctx, k)
			} else {
				err = s.Blob.Delete(ctx, k)
			}
			if err != nil && !errors.Is(err, blob.ErrNotFound) {
				s.Log.Warn("registry: delete object", "key", k, "err", err)
			}
		}
	}()
}
