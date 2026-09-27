// Package registry implements a container registry (the OCI distribution
// API, /v2/). Blob contents live in S3, metadata and manifests in Postgres,
// so any replica can serve any request — including the chunks of one upload.
//
// Access follows the global roles, as Gitea's owner-level package access:
// readers pull, writers push and delete. Clients authenticate with a password or personal access token,
// either as Basic auth on every request or — what docker does — exchanged
// once at /v2/token for a short-lived bearer token.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"onegit/internal/auth"
	"onegit/internal/blob"
	"onegit/internal/config"
	"onegit/internal/kv"
	"onegit/internal/store"
)

type Service struct {
	Store *store.Store
	Blob  *blob.Store
	Auth  *auth.Service
	KV    *kv.KV // caches parsed image configs
	Cfg   *config.Config
	Log   *slog.Logger

	tokenKey []byte
}

func New(ctx context.Context, st *store.Store, bs *blob.Store, kvs *kv.KV, as *auth.Service, cfg *config.Config, log *slog.Logger) (*Service, error) {
	key, err := st.Secret(ctx, "registry_token_key", func() ([]byte, error) {
		return []byte(auth.RandomString(32)), nil
	})
	if err != nil {
		return nil, fmt.Errorf("registry token key: %w", err)
	}
	return &Service{Store: st, Blob: bs, KV: kvs, Auth: as, Cfg: cfg, Log: log, tokenKey: key}, nil
}

var (
	// Repository names per the OCI distribution spec.
	nameRe   = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)
	tagRe    = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

	uploadsPath  = regexp.MustCompile(`^/v2/(.+)/blobs/uploads(?:/([^/]*))?$`)
	blobPath     = regexp.MustCompile(`^/v2/(.+)/blobs/([^/]+)$`)
	manifestPath = regexp.MustCompile(`^/v2/(.+)/manifests/([^/]+)$`)
	tagsPath     = regexp.MustCompile(`^/v2/(.+)/tags/list$`)
)

func validDigest(d string) bool { return digestRe.MatchString(d) }

// Match reports whether the request belongs to the registry API.
func Match(r *http.Request) bool {
	return r.URL.Path == "/v2" || strings.HasPrefix(r.URL.Path, "/v2/")
}

type access int

const (
	pull access = iota
	push
	remove
)

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	p := r.URL.Path
	switch p {
	case "/v2", "/v2/":
		s.base(w, r)
		return
	case "/v2/token":
		s.token(w, r)
		return
	case "/v2/_catalog":
		if _, ok := s.authorize(w, r, "", pull); ok {
			s.catalog(w, r)
		}
		return
	}

	var name string
	var route func(u *store.User)
	var need access
	if m := uploadsPath.FindStringSubmatch(p); m != nil {
		name = m[1]
		id := m[2]
		need = push
		switch {
		case r.Method == http.MethodPost && id == "":
			route = func(u *store.User) { s.startUpload(w, r, u, name) }
		case r.Method == http.MethodGet && id != "":
			route = func(*store.User) { s.uploadStatus(w, r, name, id) }
		case r.Method == http.MethodPatch && id != "":
			route = func(*store.User) { s.patchUpload(w, r, name, id) }
		case r.Method == http.MethodPut && id != "":
			route = func(*store.User) { s.finishUpload(w, r, name, id) }
		case r.Method == http.MethodDelete && id != "":
			route = func(*store.User) { s.cancelUpload(w, r, name, id) }
		}
	} else if m := blobPath.FindStringSubmatch(p); m != nil {
		name = m[1]
		digest := m[2]
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			route = func(*store.User) { s.getBlob(w, r, digest) }
		case http.MethodDelete:
			// Blobs are removed by garbage collection once unreferenced.
			need, route = remove, func(*store.User) {
				writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "blobs are garbage-collected; delete the manifest instead", nil)
			}
		}
	} else if m := manifestPath.FindStringSubmatch(p); m != nil {
		name = m[1]
		ref := m[2]
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			route = func(*store.User) { s.getManifest(w, r, name, ref) }
		case http.MethodPut:
			need, route = push, func(u *store.User) { s.putManifest(w, r, u, name, ref) }
		case http.MethodDelete:
			need, route = remove, func(*store.User) { s.deleteManifest(w, r, name, ref) }
		}
	} else if m := tagsPath.FindStringSubmatch(p); m != nil {
		name = m[1]
		if r.Method == http.MethodGet {
			route = func(*store.User) { s.listTags(w, r, name) }
		}
	} else {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown registry endpoint", nil)
		return
	}

	if route == nil {
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed", nil)
		return
	}
	if len(name) > 255 || !nameRe.MatchString(name) {
		writeError(w, http.StatusBadRequest, "NAME_INVALID", "invalid repository name", map[string]string{"name": name})
		return
	}
	if u, ok := s.authorize(w, r, name, need); ok {
		route(u)
	}
}

// base answers the API version check. It always challenges anonymous
// clients: `docker login` treats a 200 here as "no auth needed".
func (s *Service) base(w http.ResponseWriter, r *http.Request) {
	u, err := s.identify(r)
	if err != nil || u == nil {
		s.challenge(w, "", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("{}"))
}

// authorize checks access to repository name; on failure it has already
// written the response. The user is nil for anonymous pulls.
func (s *Service) authorize(w http.ResponseWriter, r *http.Request, name string, need access) (*store.User, bool) {
	u, err := s.identify(r)
	if err != nil && !errors.Is(err, errNoCredentials) {
		s.challenge(w, scope(name, need), err)
		return nil, false
	}
	if u == nil {
		if need == pull && s.Cfg.Repo.PublicRead {
			return nil, true
		}
		s.challenge(w, scope(name, need), nil)
		return nil, false
	}
	switch {
	case need == push && !u.CanWrite() && !(u.Job != nil && u.Job.Kind == "ci"):
		writeError(w, http.StatusForbidden, "DENIED", "you do not have write access to the registry", nil)
		return nil, false
	case need == remove && !u.CanWrite():
		writeError(w, http.StatusForbidden, "DENIED", "you do not have write access to the registry", nil)
		return nil, false
	}
	return u, true
}

var errNoCredentials = errors.New("no credentials")

// userRef is the user to store as uploader: CI jobs act for whoever
// triggered the run (possibly nobody).
func userRef(u *store.User) *int64 {
	if u.Job != nil {
		return u.Job.UserID
	}
	return &u.ID
}

// identify resolves the caller: Basic (password or access token), Bearer
// access token, or Bearer registry token. A registry token issued to an
// anonymous client yields (nil, nil).
func (s *Service) identify(r *http.Request) (*store.User, error) {
	ctx := r.Context()
	if user, pass, ok := r.BasicAuth(); ok {
		return s.Auth.CheckBasic(ctx, user, pass, auth.RequestIP(r, s.Cfg.HTTP.TrustProxy))
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil, errNoCredentials
	}
	if strings.HasPrefix(tok, auth.TokenPrefix) {
		return s.Auth.CheckToken(ctx, tok)
	}
	sub, ok := s.verifyToken(tok)
	if !ok {
		return nil, auth.ErrInvalidCredentials
	}
	if sub.jobID != 0 {
		return s.Auth.JobUser(ctx, sub.jobID)
	}
	if sub.userID == 0 {
		return nil, nil
	}
	u, err := s.Store.UserByID(ctx, sub.userID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !u.Active) {
		return nil, auth.ErrInvalidCredentials
	}
	return u, err
}

func scope(name string, need access) string {
	if name == "" {
		return "registry:catalog:*"
	}
	switch need {
	case push:
		return "repository:" + name + ":pull,push"
	case remove:
		return "repository:" + name + ":delete"
	}
	return "repository:" + name + ":pull"
}

func (s *Service) challenge(w http.ResponseWriter, scope string, err error) {
	c := fmt.Sprintf(`Bearer realm="%s/v2/token",service="%s"`, s.Cfg.HTTP.BaseURL, s.Cfg.RegistryHost())
	if scope != "" {
		c += fmt.Sprintf(`,scope="%s"`, scope)
	}
	w.Header().Set("WWW-Authenticate", c)
	msg := "authentication required"
	if err != nil && !errors.Is(err, errNoCredentials) {
		msg = err.Error()
	}
	writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", msg, nil)
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, msg string, detail any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string][]apiError{"errors": {{Code: code, Message: msg, Detail: detail}}})
}

func (s *Service) internalError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // client went away
	}
	s.Log.Error("registry request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "UNKNOWN", "internal error", nil)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
