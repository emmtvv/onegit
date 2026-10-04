package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"onegit/internal/ci"
)

const postStepName = "Upload artifacts and cache"

// transferTimeout bounds one archive upload or download.
const transferTimeout = 30 * time.Minute

// hasPost reports whether the job saves anything after its steps.
func (jr *jobRun) hasPost() bool {
	return jr.a.Artifacts != nil || jr.a.Cache != nil && jr.a.Cache.Policy != "pull"
}

// postJob uploads artifacts (as their "when" says) and saves the cache
// (after success only). A failed artifact upload fails the job; a failed
// cache save only warns.
func (jr *jobRun) postJob(ctx context.Context, ws, status, message string) (string, string) {
	idx := len(jr.steps) - 1
	if ctx.Err() != nil || (status != "success" && status != "failure") {
		jr.steps[idx].Status = "skipped"
		return status, message
	}
	now := time.Now()
	jr.steps[idx].Status, jr.steps[idx].StartedAt = "running", &now
	jr.reportSteps()
	jr.log.header(postStepName)
	result := "success"
	if a := jr.a.Artifacts; a != nil {
		want := a.When == "always" || (a.When == "on_failure") == (status == "failure")
		if want {
			if err := jr.uploadArtifacts(ctx, ws); err != nil {
				fmt.Fprintf(jr.log, "Error: uploading artifacts: %v\n", err)
				result = "failure"
				if status == "success" {
					status, message = "failure", "uploading artifacts failed: "+err.Error()
				}
			}
		} else {
			fmt.Fprintf(jr.log, "Artifacts are not kept for a job that ends with %s\n", status)
		}
	}
	if c := jr.a.Cache; c != nil && c.Policy != "pull" {
		switch {
		case status != "success":
			fmt.Fprintln(jr.log, "Not saving the cache: the job did not succeed")
		case jr.cacheHit:
			fmt.Fprintf(jr.log, "Cache %s was restored as is; not saving it again\n", jr.cacheKey)
		default:
			if err := jr.saveCache(ctx, ws); err != nil {
				fmt.Fprintf(jr.log, "Warning: saving the cache: %v\n", err)
			}
		}
	}
	done := time.Now()
	jr.steps[idx].Status, jr.steps[idx].FinishedAt = result, &done
	return status, message
}

func (jr *jobRun) uploadArtifacts(ctx context.Context, ws string) error {
	f, n, err := jr.archive(ws, jr.a.Artifacts.Paths)
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer func() { _ = f.Close() }()
	if n == 0 {
		fmt.Fprintln(jr.log, "No files to upload")
		return nil
	}
	st, _ := f.Stat()
	fmt.Fprintf(jr.log, "Uploading %d file(s) as artifacts (%s)\n", n, humanSize(st.Size()))
	return jr.r.putArchive(ctx, fmt.Sprintf("/api/runner/v1/jobs/%d/artifacts", jr.a.ID), jr.a.Token, f, st.Size())
}

// computeCacheKey extends the cache key with a hash of the key files.
func (jr *jobRun) computeCacheKey(ws string) (key, prefix string) {
	c := jr.a.Cache
	if len(c.KeyFiles) == 0 {
		return c.Key, ""
	}
	var files []string
	for _, pat := range c.KeyFiles {
		m, _ := filepath.Glob(filepath.Join(ws, filepath.Clean("/"+pat)))
		files = append(files, m...)
	}
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		rel, _ := filepath.Rel(ws, f)
		b, err := os.ReadFile(f)
		if err != nil {
			continue // directories and unreadable files do not count
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(b))
		h.Write(b)
	}
	return c.Key + "-" + hex.EncodeToString(h.Sum(nil))[:16], c.Key + "-"
}

func (jr *jobRun) restoreCache(ctx context.Context, ws string) {
	key, prefix := jr.computeCacheKey(ws)
	jr.cacheKey = key
	if jr.a.Cache.Policy == "push" {
		return
	}
	q := url.Values{"key": {key}}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	var hit ci.CacheHit
	code, err := jr.r.get(ctx, fmt.Sprintf("/api/runner/v1/jobs/%d/cache?%s", jr.a.ID, q.Encode()), jr.a.Token, &hit)
	switch {
	case err != nil:
		fmt.Fprintf(jr.log, "Warning: cache lookup failed: %v\n", err)
		return
	case code == http.StatusNoContent:
		fmt.Fprintf(jr.log, "No cache for %s\n", key)
		return
	}
	fmt.Fprintf(jr.log, "Restoring cache %s (%s)\n", hit.Key, humanSize(hit.Size))
	if err := jr.r.fetchArchive(ctx, fmt.Sprintf("/api/runner/v1/jobs/%d/cache/%d", jr.a.ID, hit.ID), jr.a.Token, ws); err != nil {
		fmt.Fprintf(jr.log, "Warning: restoring the cache failed: %v\n", err)
		return
	}
	jr.cacheHit = hit.Key == key
}

func (jr *jobRun) saveCache(ctx context.Context, ws string) error {
	f, n, err := jr.archive(ws, jr.a.Cache.Paths)
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer func() { _ = f.Close() }()
	if n == 0 {
		fmt.Fprintln(jr.log, "Nothing to cache")
		return nil
	}
	st, _ := f.Stat()
	fmt.Fprintf(jr.log, "Saving cache %s (%d files, %s)\n", jr.cacheKey, n, humanSize(st.Size()))
	err = jr.r.putArchive(ctx, fmt.Sprintf("/api/runner/v1/jobs/%d/cache?key=%s", jr.a.ID, url.QueryEscape(jr.cacheKey)), jr.a.Token, f, st.Size())
	if he, ok := err.(*httpError); ok && he.code == http.StatusConflict {
		fmt.Fprintln(jr.log, "Another job saved this cache first")
		return nil
	}
	return err
}

// archive packs paths into a temporary file next to the workspace,
// rewound for reading.
func (jr *jobRun) archive(ws string, paths []string) (*os.File, int, error) {
	f, err := os.CreateTemp(filepath.Dir(ws), filepath.Base(ws)+"-*.tar.gz")
	if err != nil {
		return nil, 0, err
	}
	n, err := packPaths(ws, paths, f, jr.log)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = f.Close()
		os.Remove(f.Name())
		return nil, 0, err
	}
	return f, n, nil
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.code, http.StatusText(e.code), e.msg)
}

func (r *Runner) request(ctx context.Context, method, path, token string, body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.cfg.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/gzip")
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, &httpError{code: resp.StatusCode, msg: strings.TrimSpace(string(msg))}
	}
	return resp, nil
}

func (r *Runner) get(ctx context.Context, path, token string, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	resp, err := r.request(ctx, http.MethodGet, path, token, nil, 0)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func (r *Runner) putArchive(ctx context.Context, path, token string, f *os.File, size int64) error {
	ctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	resp, err := r.request(ctx, http.MethodPut, path, token, f, size)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// fetchArchive downloads a tar.gz and extracts it into ws.
func (r *Runner) fetchArchive(ctx context.Context, path, token, ws string) error {
	ctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	resp, err := r.request(ctx, http.MethodGet, path, token, nil, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = unpack(ws, resp.Body)
	return err
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
