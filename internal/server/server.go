// Package server wires all onegit components together.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"onegit/internal/auth"
	"onegit/internal/blob"
	"onegit/internal/ci"
	"onegit/internal/config"
	"onegit/internal/deploy"
	"onegit/internal/git"
	"onegit/internal/hooks"
	"onegit/internal/kv"
	"onegit/internal/projects"
	"onegit/internal/pulls"
	"onegit/internal/registry"
	"onegit/internal/store"
	"onegit/internal/transport"
	"onegit/internal/web"
)

type Server struct {
	Cfg   *config.Config
	Log   *slog.Logger
	Store *store.Store
	KV    *kv.KV
	Blob  *blob.Store
	Repo  *git.Repo
	Auth  *auth.Service
	OIDC  *auth.OIDC
	Pulls *pulls.Service
	// Registry is nil when the container registry is disabled.
	Registry *registry.Service
	CI       *ci.Service
	Deploy   *deploy.Service

	// internalToken authenticates hook callbacks. It is per process: git
	// hooks always call back into the replica that spawned them.
	internalToken string
	internalURL   string
	gitHTTP       *transport.HTTP
	web           *web.Web
}

func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Server, error) {
	st, err := store.Open(ctx, cfg.DatabaseURL(), cfg.Database.MaxConns)
	if err != nil {
		return nil, err
	}
	kvs, err := kv.Open(ctx, cfg.Redis.URL, cfg.Redis.KeyPrefix)
	if err != nil {
		return nil, err
	}
	bs, err := blob.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, err
	}
	repo, err := git.Open(ctx, cfg.RepoPath(), cfg.Repo.DefaultBranch, exe)
	if err != nil {
		return nil, fmt.Errorf("open repository: %w", err)
	}
	s := &Server{
		Cfg: cfg, Log: log, Store: st, KV: kvs, Blob: bs, Repo: repo,
		Auth:          &auth.Service{Store: st, KV: kvs},
		internalToken: auth.RandomString(32),
	}
	s.Pulls = &pulls.Service{Store: st, Repo: repo, KV: kvs, Log: log, BaseURL: cfg.HTTP.BaseURL}
	if err := s.bootstrapAdmin(ctx); err != nil {
		return nil, err
	}
	s.CI = &ci.Service{Store: st, Repo: repo, Cfg: cfg, Log: log, Blob: bs}
	s.CI.OnCheckDone = func(string) { s.Pulls.Kick() }
	s.Pulls.StartChecks = s.CI.RunMergeQueue
	s.Pulls.CancelChecks = s.CI.CancelMergeQueue
	s.Pulls.BaseUpdated = func(ctx context.Context, by *store.User, updates []hooks.RefUpdate) {
		// Like a push: pipelines start in the background.
		go s.CI.OnPush(context.WithoutCancel(ctx), by, updates)
	}
	if s.Deploy, err = deploy.New(ctx, st, repo, s.CI, cfg, log); err != nil {
		return nil, err
	}
	if cfg.Registry.Enabled {
		if s.Registry, err = registry.New(ctx, st, bs, kvs, s.Auth, cfg, log); err != nil {
			return nil, err
		}
	}
	if cfg.OIDC.Enabled {
		if s.OIDC, err = auth.NewOIDC(ctx, cfg, st); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// bootstrapAdmin creates the initial admin from ONEGIT_ADMIN_PASSWORD when
// the database has no users. The password must be changed at first login.
func (s *Server) bootstrapAdmin(ctx context.Context) error {
	users, err := s.Store.ListUsers(ctx)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return nil
	}
	pw := s.Cfg.Admin.InitialPassword
	if pw == "" {
		return errors.New("no users exist yet: set ONEGIT_ADMIN_PASSWORD to create the initial admin")
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	created, err := s.Store.CreateInitialAdmin(ctx, &store.User{
		Username: s.Cfg.Admin.Username, Email: s.Cfg.Admin.Email, PasswordHash: hash,
	})
	if err != nil {
		return fmt.Errorf("create initial admin: %w", err)
	}
	if created {
		s.Log.Info("created initial admin; the password must be changed at first login", "username", s.Cfg.Admin.Username)
	}
	return nil
}

// GitEnv is the environment for git processes run on behalf of a user; it
// lets our hooks call back into the server and identify the pusher.
func (s *Server) GitEnv(u *store.User) []string {
	return []string{
		hooks.EnvInternalURL + "=" + s.internalURL,
		hooks.EnvInternalToken + "=" + s.internalToken,
		hooks.EnvPusherID + "=" + strconv.FormatInt(u.ID, 10),
		hooks.EnvPusherName + "=" + u.Username,
	}
}

func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.Cfg.HTTP.Addr)
	if err != nil {
		s.close()
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve handles requests on ln until ctx is cancelled, then shuts down and
// closes the backing connections.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	defer s.close()
	port := ln.Addr().(*net.TCPAddr).Port
	s.internalURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	var err error
	deps := transport.Deps{
		RepoName: s.Cfg.Repo.Name, RepoPath: s.Cfg.RepoPath(),
		PublicRead: s.Cfg.Repo.PublicRead, TrustProxy: s.Cfg.HTTP.TrustProxy,
		Auth: s.Auth, GitEnv: s.GitEnv, Log: s.Log,
	}
	if s.gitHTTP, err = transport.NewHTTP(deps); err != nil {
		return err
	}
	s.web, err = web.New(web.Deps{Cfg: s.Cfg, Store: s.Store, KV: s.KV, Repo: s.Repo, Auth: s.Auth, OIDC: s.OIDC, Pulls: s.Pulls,
		Registry: s.Registry, CI: s.CI, Deploy: s.Deploy, Log: s.Log,
		Projects: &projects.Service{Store: s.Store, Repo: s.Repo, Cfg: s.Cfg}})
	if err != nil {
		return err
	}

	if s.Registry != nil {
		go s.Registry.GCLoop(ctx)
	}
	go s.CI.Janitor(ctx)
	go s.CI.Maintenance(ctx)
	go s.Pulls.Background(ctx)
	go s.repoMaintenanceLoop(ctx)
	go func() {
		// Warm the sorted ref lists: the first listing reads every ref.
		for _, k := range []git.RefKind{git.KindBranch, git.KindTag} {
			_, _, _ = s.Repo.ListRefs(ctx, k, git.RefQuery{Limit: 1})
		}
	}()

	httpSrv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 30 * time.Second}
	errc := make(chan error, 1)
	go func() {
		s.Log.Info("http listening", "addr", ln.Addr().String(), "url", s.Cfg.HTTP.BaseURL)
		errc <- httpSrv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	s.Log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		s.Log.Warn("shutdown", "err", err)
	}
	return nil
}

func (s *Server) close() {
	s.Store.Close()
	s.KV.Close()
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/hook/{name}", s.handleHook)
	mux.Handle("/api/runner/", s.CI.RunnerAPI())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("/", s.web)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.gitHTTP.Match(r); ok {
			s.gitHTTP.ServeHTTP(w, r)
			return
		}
		if s.Registry != nil && registry.Match(r) {
			s.Registry.ServeHTTP(w, r)
			return
		}
		if s.Registry != nil && registry.MatchAPI(r) {
			s.Registry.ServeAPI(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// readyz checks every backing service; load balancers should route only to
// ready replicas.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	checks := map[string]func(context.Context) error{
		"postgres": s.Store.Ping,
		"redis":    s.KV.Ping,
		"s3":       s.Blob.Ping,
		"repo": func(ctx context.Context) error {
			_, err := os.Stat(filepath.Join(s.Cfg.RepoPath(), "HEAD"))
			return err
		},
	}
	var failures []string
	for name, check := range checks {
		if err := check(ctx); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if len(failures) > 0 {
		http.Error(w, strings.Join(failures, "\n"), http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok\n"))
}
