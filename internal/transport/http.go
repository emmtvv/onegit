// Package transport serves git over smart HTTP.
package transport

import (
	"compress/gzip"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"onegit/internal/auth"
	"onegit/internal/store"
)

type Deps struct {
	RepoName   string
	RepoPath   string
	PublicRead bool
	TrustProxy bool
	Auth       *auth.Service
	// GitEnv returns extra environment for git processes run on behalf of u.
	GitEnv func(u *store.User) []string
	Log    *slog.Logger
}

type HTTP struct {
	Deps
	gitBin string
}

func NewHTTP(d Deps) (*HTTP, error) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	return &HTTP{Deps: d, gitBin: bin}, nil
}

// Match reports whether the request is a git smart-HTTP request and returns
// the path suffix after the repository name (e.g. "/info/refs").
func (h *HTTP) Match(r *http.Request) (string, bool) {
	p := r.URL.Path
	for _, prefix := range []string{"/" + h.RepoName + ".git", "/" + h.RepoName} {
		if rest, ok := strings.CutPrefix(p, prefix); ok {
			switch rest {
			case "/info/refs", "/git-upload-pack", "/git-receive-pack":
				return rest, true
			}
		}
	}
	return "", false
}

func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	suffix, ok := h.Match(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	service := r.URL.Query().Get("service")
	if suffix != "/info/refs" {
		service = strings.TrimPrefix(suffix, "/")
	}
	var needWrite bool
	switch service {
	case "git-upload-pack":
	case "git-receive-pack":
		needWrite = true
	default:
		http.Error(w, "dumb http protocol is not supported", http.StatusForbidden)
		return
	}

	user, err := h.authenticate(r)
	switch {
	case errors.Is(err, auth.ErrPasswordChangeRequired), errors.Is(err, auth.ErrTooManyAttempts):
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	case err != nil && !errors.Is(err, errNoCredentials):
		h.challenge(w)
		return
	}
	switch {
	case user == nil && (needWrite || !h.PublicRead):
		h.challenge(w)
		return
	case needWrite && !user.CanWrite():
		http.Error(w, "you do not have write access to this repository", http.StatusForbidden)
		return
	}

	if suffix != "/info/refs" {
		h.serveRPC(w, r, service, user)
		return
	}

	env := []string{
		"GIT_PROJECT_ROOT=" + filepath.Dir(h.RepoPath),
		"GIT_HTTP_EXPORT_ALL=1",
	}
	if user != nil {
		env = append(env, "REMOTE_USER="+user.Username)
		env = append(env, h.GitEnv(user)...)
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + filepath.Base(h.RepoPath) + suffix
	handler := &cgi.Handler{
		Path:       h.gitBin,
		Args:       []string{"http-backend"},
		Env:        env,
		InheritEnv: []string{"PATH", "HOME", "LANG", "TMPDIR"},
		Logger:     slog.NewLogLogger(h.Log.Handler(), slog.LevelWarn),
	}
	handler.ServeHTTP(w, r2)
}

// serveRPC runs "git <service> --stateless-rpc" directly instead of going
// through http-backend: net/http/cgi rejects chunked request bodies, which
// git uses for every pack larger than http.postBuffer (1 MiB by default).
func (h *HTTP) serveRPC(w http.ResponseWriter, r *http.Request, service string, user *store.User) {
	if r.Header.Get("Content-Type") != "application/x-"+service+"-request" {
		http.Error(w, "unexpected content type", http.StatusBadRequest)
		return
	}
	var body io.Reader = r.Body
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip", "x-gzip":
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "bad gzip body", http.StatusBadRequest)
			return
		}
		defer gz.Close()
		body = gz
	default:
		http.Error(w, "unsupported content encoding", http.StatusUnsupportedMediaType)
		return
	}

	cmd := exec.CommandContext(r.Context(), h.gitBin, strings.TrimPrefix(service, "git-"), "--stateless-rpc", h.RepoPath)
	cmd.Env = os.Environ()
	if user != nil {
		cmd.Env = append(cmd.Env, h.GitEnv(user)...)
	}
	if proto := r.Header.Get("Git-Protocol"); proto != "" {
		cmd.Env = append(cmd.Env, "GIT_PROTOCOL="+proto)
	}
	var stderr strings.Builder
	cmd.Stdin, cmd.Stderr = body, &stderr
	cmd.Stdout = flushWriter{w, http.NewResponseController(w)}

	w.Header().Set("Content-Type", "application/x-"+service+"-result")
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	if err := cmd.Run(); err != nil && r.Context().Err() == nil {
		h.Log.Warn("git rpc failed", "service", service, "err", err, "stderr", stderr.String())
	}
}

// flushWriter flushes after every write so sideband progress reaches the
// client immediately.
type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		f.rc.Flush()
	}
	return n, err
}

var errNoCredentials = errors.New("no credentials")

func (h *HTTP) authenticate(r *http.Request) (*store.User, error) {
	if user, pass, ok := r.BasicAuth(); ok {
		return h.Auth.CheckBasic(r.Context(), user, pass, auth.RequestIP(r, h.TrustProxy))
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return h.Auth.CheckToken(r.Context(), tok)
	}
	return nil, errNoCredentials
}

func (h *HTTP) challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="onegit"`)
	http.Error(w, "authentication required", http.StatusUnauthorized)
}
