// Package web implements the server-rendered UI.
package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"onegit/internal/auth"
	"onegit/internal/ci"
	"onegit/internal/config"
	"onegit/internal/deploy"
	"onegit/internal/git"
	"onegit/internal/kv"
	"onegit/internal/projects"
	"onegit/internal/pulls"
	"onegit/internal/registry"
	"onegit/internal/store"
)

//go:embed templates static
var assets embed.FS

type Deps struct {
	Cfg   *config.Config
	Store *store.Store
	KV    *kv.KV
	Repo  *git.Repo
	Auth  *auth.Service
	OIDC  *auth.OIDC // nil when disabled
	Pulls *pulls.Service
	// Registry is nil when the container registry is disabled.
	Registry *registry.Service
	CI       *ci.Service
	Deploy   *deploy.Service
	Projects *projects.Service
	Log      *slog.Logger
}

type Web struct {
	Deps
	tmpl    map[string]*templateSet
	handler http.Handler
}

func New(d Deps) (*Web, error) {
	w := &Web{Deps: d}
	var err error
	if w.tmpl, err = loadTemplates(); err != nil {
		return nil, err
	}
	w.handler = w.routes()
	return w, nil
}

func (w *Web) ServeHTTP(rw http.ResponseWriter, r *http.Request) { w.handler.ServeHTTP(rw, r) }

func (w *Web) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler(static)))
	mux.HandleFunc("GET /static/chroma.css", serveChromaCSS)

	// Auth
	mux.HandleFunc("GET /login", w.loginPage)
	mux.HandleFunc("POST /login", w.loginSubmit)
	mux.HandleFunc("POST /logout", w.logout)
	mux.HandleFunc("GET /login/oidc", w.oidcStart)
	mux.HandleFunc("GET /login/oidc/callback", w.oidcCallback)
	mux.Handle("GET /password/change", w.requireLogin(w.changePasswordPage))
	mux.Handle("POST /password/change", w.requireLogin(w.changePasswordSubmit))

	// Code browsing
	read := w.requireRead
	mux.Handle("GET /{$}", read(w.home))
	mux.Handle("GET /tree/{rest...}", read(w.tree))
	mux.Handle("GET /blob/{rest...}", read(w.blob))
	mux.Handle("GET /raw/{rest...}", read(w.raw))
	mux.Handle("GET /blame/{rest...}", read(w.blame))
	mux.Handle("GET /commits/{rest...}", read(w.commits))
	mux.Handle("GET /commit/{sha}", read(w.commit))
	mux.Handle("GET /branches", read(w.branches))
	mux.Handle("GET /tags", read(w.tags))
	mux.Handle("GET /search", read(w.search))

	// Pull requests
	login := w.requireLogin
	mux.Handle("GET /pulls", read(w.pullList))
	mux.Handle("GET /pulls/new", login(w.pullNew))
	mux.Handle("GET /pulls/queue", read(w.mergeQueue))
	mux.Handle("POST /pulls", login(w.pullCreate))
	mux.Handle("GET /pulls/{id}", read(w.pullView))
	mux.Handle("GET /pulls/{id}/commits", read(w.pullView))
	mux.Handle("GET /pulls/{id}/files", read(w.pullView))
	mux.Handle("POST /pulls/{id}/comments", login(w.pullComment))
	mux.Handle("POST /pulls/{id}/comments/{cid}/delete", login(w.pullDeleteComment))
	mux.Handle("POST /pulls/{id}/reviews", login(w.pullReview))
	mux.Handle("POST /pulls/{id}/merge", login(w.pullMerge))
	mux.Handle("POST /pulls/{id}/auto-merge", login(w.pullAutoMerge))
	mux.Handle("POST /pulls/{id}/queue", login(w.pullQueue))
	mux.Handle("POST /pulls/{id}/edit", login(w.pullEdit))
	mux.Handle("POST /pulls/{id}/delete-branch", login(w.pullDeleteBranch))

	// Projects
	mux.Handle("GET /projects", read(w.projectList))
	mux.Handle("GET /projects/{name}", read(w.projectView))

	// Container registry
	mux.Handle("GET /packages", read(w.packageList))
	mux.Handle("GET /packages/{rest...}", read(w.packageView))
	mux.Handle("POST /packages/-/delete", login(w.packageDeleteVersion))
	mux.Handle("POST /packages/-/delete-package", login(w.packageDeleteImage))

	// Actions
	mux.Handle("GET /actions", read(w.actions))
	mux.Handle("POST /actions/run", login(w.runManual))
	mux.Handle("GET /actions/runs/{id}", read(w.runPage))
	mux.Handle("POST /actions/runs/{id}/cancel", login(w.cancelRun))
	mux.Handle("POST /actions/runs/{id}/rerun", login(w.rerunPipeline))
	mux.Handle("GET /actions/jobs/{id}", read(w.jobPage))
	mux.Handle("GET /actions/jobs/{id}/log", read(w.jobLog))
	mux.Handle("GET /actions/jobs/{id}/raw", read(w.jobRawLog))
	mux.Handle("GET /actions/artifacts/{id}", read(w.artifactDownload))

	// Deployments
	mux.Handle("GET /deploy", read(w.deployOverview))
	mux.Handle("GET /deploy/new", login(w.deployNew))
	mux.Handle("POST /deploy", login(w.deployCreate))
	mux.Handle("GET /deploy/target", read(w.deployTarget))
	mux.Handle("GET /deploy/{id}", read(w.deployView))
	mux.Handle("POST /deploy/{id}/review", login(w.deployReview))
	mux.Handle("POST /deploy/{id}/cancel", login(w.deployCancel))

	// User settings
	mux.Handle("GET /settings", login(w.settingsProfile))
	mux.Handle("POST /settings/password", login(w.settingsPassword))
	mux.Handle("GET /settings/tokens", login(w.settingsTokens))
	mux.Handle("POST /settings/tokens", login(w.addToken))
	mux.Handle("POST /settings/tokens/{id}/delete", login(w.deleteToken))

	// Admin
	admin := w.requireAdmin
	mux.Handle("GET /admin/users", admin(w.adminUsers))
	mux.Handle("POST /admin/users", admin(w.adminCreateUser))
	mux.Handle("GET /admin/users/{id}", admin(w.adminEditUser))
	mux.Handle("POST /admin/users/{id}", admin(w.adminUpdateUser))
	mux.Handle("POST /admin/users/{id}/delete", admin(w.adminDeleteUser))
	mux.Handle("GET /admin/branches", admin(w.adminBranches))
	mux.Handle("POST /admin/branches", admin(w.adminSaveBranch))
	mux.Handle("POST /admin/branches/{id}", admin(w.adminSaveBranch))
	mux.Handle("POST /admin/branches/{id}/delete", admin(w.adminDeleteBranch))
	mux.Handle("GET /admin/runners", admin(w.adminRunners))
	mux.Handle("POST /admin/runners", admin(w.adminCreateRunner))
	mux.Handle("POST /admin/runners/{id}", admin(w.adminUpdateRunner))
	mux.Handle("POST /admin/runners/{id}/token", admin(w.adminRunnerToken))
	mux.Handle("POST /admin/runners/{id}/delete", admin(w.adminDeleteRunner))
	mux.Handle("GET /admin/teams", admin(w.adminTeams))
	mux.Handle("POST /admin/teams", admin(w.adminSaveTeam))
	mux.Handle("POST /admin/teams/{id}", admin(w.adminSaveTeam))
	mux.Handle("POST /admin/teams/{id}/members", admin(w.adminTeamMember))
	mux.Handle("POST /admin/teams/{id}/delete", admin(w.adminDeleteTeam))
	mux.Handle("GET /admin/deploy", admin(w.adminDeploy))
	mux.Handle("POST /admin/deploy/dimensions", admin(w.adminSaveDimension))
	mux.Handle("POST /admin/deploy/dimensions/{name}/delete", admin(w.adminDeleteDimension))
	mux.Handle("POST /admin/deploy/rules", admin(w.adminSaveRule))
	mux.Handle("POST /admin/deploy/rules/{id}", admin(w.adminSaveRule))
	mux.Handle("POST /admin/deploy/rules/{id}/delete", admin(w.adminDeleteRule))
	mux.Handle("GET /admin/secrets", admin(w.adminSecrets))
	mux.Handle("POST /admin/secrets", admin(w.adminSaveSecret))
	mux.Handle("POST /admin/secrets/{id}", admin(w.adminSaveSecret))
	mux.Handle("POST /admin/secrets/{id}/delete", admin(w.adminDeleteSecret))
	mux.Handle("GET /admin/audit", admin(w.adminAudit))
	mux.Handle("GET /admin/packages", admin(w.adminPackages))
	mux.Handle("POST /admin/packages", admin(w.adminSavePackages))
	mux.Handle("POST /admin/packages/run", admin(w.adminRunCleanup))
	mux.Handle("GET /admin/projects", admin(w.adminProjects))
	mux.Handle("POST /admin/projects", admin(w.adminSaveProjects))

	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) { w.notFound(rw, r) })

	cop := http.NewCrossOriginProtection()
	return w.withUser(w.enforcePasswordChange(cop.Handler(securityHeaders(mux))))
}

func staticHandler(fsys fs.FS) http.Handler {
	// Alpine has no /etc/mime.types, and Go only knows a few types itself.
	mime.AddExtensionType(".woff2", "font/woff2")
	h := http.FileServerFS(fsys)
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(rw, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		h := rw.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(rw, r)
	})
}

// ---- current user ----

type ctxKey int

const userKey ctxKey = 0

func (w *Web) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(auth.SessionCookie); err == nil {
			if u, err := w.Auth.SessionUser(r.Context(), c.Value); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), userKey, u))
			}
		}
		next.ServeHTTP(rw, r)
	})
}

// enforcePasswordChange confines users with a temporary password to the
// change-password page until they pick a new one.
func (w *Web) enforcePasswordChange(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		if u == nil || !u.MustChangePassword {
			next.ServeHTTP(rw, r)
			return
		}
		switch p := r.URL.Path; {
		case p == "/password/change", p == "/logout", strings.HasPrefix(p, "/static/"):
			next.ServeHTTP(rw, r)
		default:
			http.Redirect(rw, r, "/password/change?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		}
	})
}

func currentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

type handlerFunc func(http.ResponseWriter, *http.Request)

func (w *Web) requireRead(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !w.Cfg.Repo.PublicRead && currentUser(r) == nil {
			w.redirectToLogin(rw, r)
			return
		}
		h(rw, r)
	})
}

func (w *Web) requireLogin(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if currentUser(r) == nil {
			w.redirectToLogin(rw, r)
			return
		}
		h(rw, r)
	})
}

func (w *Web) requireAdmin(h handlerFunc) http.Handler {
	return w.requireLogin(func(rw http.ResponseWriter, r *http.Request) {
		if !currentUser(r).IsAdmin() {
			w.errorPage(rw, r, http.StatusForbidden, "Only administrators can access this page.")
			return
		}
		h(rw, r)
	})
}

func (w *Web) redirectToLogin(rw http.ResponseWriter, r *http.Request) {
	http.Redirect(rw, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
}

// ---- rendering ----

type Page struct {
	Title  string
	Tab    string
	User   *store.User
	Cfg    *config.Config
	Flash  string
	Error  string
	Data   any
	Branch string // default branch, for nav links
	// OpenPulls is the count shown on the nav tab.
	OpenPulls int
	// Projects: the Projects tab is shown.
	Projects bool
}

func (w *Web) render(rw http.ResponseWriter, r *http.Request, status int, name string, p *Page) {
	p.User = currentUser(r)
	p.Cfg = w.Cfg
	if p.Branch == "" {
		p.Branch = w.defaultBranch(r.Context())
	}
	if p.Flash == "" {
		p.Flash = popFlash(rw, r)
	}
	if p.Tab != "" {
		p.OpenPulls, _, _ = w.Store.CountPulls(r.Context())
		_, _, p.Projects, _ = w.Projects.Settings(r.Context())
	}
	ts, ok := w.tmpl[name]
	if !ok {
		w.Log.Error("unknown template", "name", name)
		http.Error(rw, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := ts.Execute(&buf, p); err != nil {
		w.Log.Error("render", "template", name, "err", err)
		http.Error(rw, "internal error", http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(status)
	buf.WriteTo(rw)
}

func (w *Web) errorPage(rw http.ResponseWriter, r *http.Request, status int, msg string) {
	w.render(rw, r, status, "error", &Page{Title: http.StatusText(status), Data: map[string]any{
		"Status": status, "Message": msg,
	}})
}

func (w *Web) notFound(rw http.ResponseWriter, r *http.Request) {
	w.errorPage(rw, r, http.StatusNotFound, "The page you are looking for does not exist.")
}

func (w *Web) serverError(rw http.ResponseWriter, r *http.Request, err error) {
	w.Log.Error("request failed", "path", r.URL.Path, "err", err)
	w.errorPage(rw, r, http.StatusInternalServerError, "Something went wrong.")
}

func (w *Web) fail(rw http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, git.ErrNotExist) || errors.Is(err, store.ErrNotFound) {
		w.notFound(rw, r)
		return
	}
	w.serverError(rw, r, err)
}

// Flash messages survive one redirect via a short-lived cookie.
const flashCookie = "onegit_flash"

func setFlash(rw http.ResponseWriter, msg string) {
	http.SetCookie(rw, &http.Cookie{Name: flashCookie, Value: url.QueryEscape(msg), Path: "/", MaxAge: 60, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func popFlash(rw http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return ""
	}
	http.SetCookie(rw, &http.Cookie{Name: flashCookie, Path: "/", MaxAge: -1})
	v, _ := url.QueryUnescape(c.Value)
	return v
}

func (w *Web) redirectFlash(rw http.ResponseWriter, r *http.Request, to, msg string) {
	if msg != "" {
		setFlash(rw, msg)
	}
	http.Redirect(rw, r, to, http.StatusSeeOther)
}

func (w *Web) defaultBranch(ctx context.Context) string {
	if b := w.Repo.HeadBranch(ctx); b != "" {
		return b
	}
	return w.Cfg.Repo.DefaultBranch
}

func (w *Web) clientIP(r *http.Request) string { return auth.RequestIP(r, w.Cfg.HTTP.TrustProxy) }

func (w *Web) secureCookies() bool { return strings.HasPrefix(w.Cfg.HTTP.BaseURL, "https://") }
