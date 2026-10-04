package web

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"onegit/internal/auth"
	"onegit/internal/store"
)

// ---- login ----

func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

func (w *Web) loginPage(rw http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		http.Redirect(rw, r, "/", http.StatusSeeOther)
		return
	}
	w.render(rw, r, http.StatusOK, "login", &Page{Title: "Sign in", Data: map[string]any{
		"Next": safeNext(r.URL.Query().Get("next")), "OIDC": w.OIDC != nil,
	}})
}

func (w *Web) loginSubmit(rw http.ResponseWriter, r *http.Request) {
	next := safeNext(r.FormValue("next"))
	if !w.Cfg.Auth.PasswordLogin {
		w.errorPage(rw, r, http.StatusForbidden, "Password login is disabled.")
		return
	}
	u, err := w.Auth.CheckPassword(r.Context(), strings.TrimSpace(r.FormValue("username")), r.FormValue("password"), w.clientIP(r))
	if err != nil {
		msg := "Incorrect username or password."
		switch {
		case errors.Is(err, auth.ErrTooManyAttempts):
			msg = "Too many failed attempts. Try again in a few minutes."
		case !errors.Is(err, auth.ErrInvalidCredentials):
			w.serverError(rw, r, err)
			return
		}
		w.render(rw, r, http.StatusUnauthorized, "login", &Page{Title: "Sign in", Error: msg, Data: map[string]any{
			"Next": next, "OIDC": w.OIDC != nil, "Username": r.FormValue("username"),
		}})
		return
	}
	w.startSession(rw, r, u, next)
}

func (w *Web) startSession(rw http.ResponseWriter, r *http.Request, u *store.User, next string) {
	id, err := w.Auth.NewSession(r.Context(), u.ID)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	http.SetCookie(rw, &http.Cookie{
		Name: auth.SessionCookie, Value: id, Path: "/", HttpOnly: true, Secure: w.secureCookies(),
		SameSite: http.SameSiteLaxMode, Expires: time.Now().Add(auth.SessionLifetime),
	})
	http.Redirect(rw, r, next, http.StatusSeeOther)
}

func (w *Web) logout(rw http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		_ = w.Auth.EndSession(r.Context(), c.Value) // the cookie is cleared either way
	}
	http.SetCookie(rw, &http.Cookie{Name: auth.SessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(rw, r, "/login", http.StatusSeeOther)
}

const oidcCookie = "onegit_oidc"

func (w *Web) oidcStart(rw http.ResponseWriter, r *http.Request) {
	if w.OIDC == nil {
		w.notFound(rw, r)
		return
	}
	state, nonce, verifier := auth.RandomString(16), auth.RandomString(16), auth.RandomString(32)
	v := url.Values{"s": {state}, "n": {nonce}, "v": {verifier}, "next": {safeNext(r.URL.Query().Get("next"))}}
	http.SetCookie(rw, &http.Cookie{
		Name: oidcCookie, Value: v.Encode(), Path: "/login/oidc", HttpOnly: true, Secure: w.secureCookies(),
		SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})
	http.Redirect(rw, r, w.OIDC.AuthURL(state, nonce, verifier), http.StatusFound)
}

func (w *Web) oidcCallback(rw http.ResponseWriter, r *http.Request) {
	if w.OIDC == nil {
		w.notFound(rw, r)
		return
	}
	c, err := r.Cookie(oidcCookie)
	if err != nil {
		w.errorPage(rw, r, http.StatusBadRequest, "Login session expired, please try again.")
		return
	}
	http.SetCookie(rw, &http.Cookie{Name: oidcCookie, Path: "/login/oidc", MaxAge: -1})
	v, _ := url.ParseQuery(c.Value)
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		w.errorPage(rw, r, http.StatusUnauthorized, "Identity provider returned an error: "+e+" "+q.Get("error_description"))
		return
	}
	if q.Get("state") == "" || q.Get("state") != v.Get("s") {
		w.errorPage(rw, r, http.StatusBadRequest, "Invalid login state, please try again.")
		return
	}
	u, err := w.OIDC.Exchange(r.Context(), q.Get("code"), v.Get("n"), v.Get("v"))
	if err != nil {
		w.Log.Warn("oidc login failed", "err", err)
		w.errorPage(rw, r, http.StatusUnauthorized, "Login failed: "+err.Error())
		return
	}
	w.startSession(rw, r, u, safeNext(v.Get("next")))
}

// ---- settings ----

func (w *Web) settingsProfile(rw http.ResponseWriter, r *http.Request) {
	w.render(rw, r, http.StatusOK, "settings_profile", &Page{Title: "Settings", Tab: "settings"})
}

func (w *Web) settingsPassword(rw http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.PasswordHash != "" {
		if _, err := w.Auth.CheckPassword(r.Context(), u.Username, r.FormValue("current"), w.clientIP(r)); err != nil {
			w.redirectFlash(rw, r, "/settings", "Current password is incorrect.")
			return
		}
	}
	if err := w.Auth.SetPassword(r.Context(), u, r.FormValue("new")); err != nil {
		w.redirectFlash(rw, r, "/settings", passwordError(err))
		return
	}
	w.redirectFlash(rw, r, "/settings", "Password updated.")
}

// changePasswordPage is the mandatory stop for users with a temporary
// password (the initial admin, or users whose password an admin has set).
func (w *Web) changePasswordPage(rw http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if !u.MustChangePassword {
		http.Redirect(rw, r, "/settings", http.StatusSeeOther)
		return
	}
	w.render(rw, r, http.StatusOK, "change_password", &Page{Title: "Change your password", Data: map[string]any{
		"Next": safeNext(r.URL.Query().Get("next")),
	}})
}

func (w *Web) changePasswordSubmit(rw http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	next := safeNext(r.FormValue("next"))
	fail := func(msg string) {
		w.render(rw, r, http.StatusBadRequest, "change_password", &Page{Title: "Change your password", Error: msg, Data: map[string]any{"Next": next}})
	}
	if !u.MustChangePassword {
		http.Redirect(rw, r, "/settings", http.StatusSeeOther)
		return
	}
	if _, err := w.Auth.CheckPassword(r.Context(), u.Username, r.FormValue("current"), w.clientIP(r)); err != nil {
		fail("Current password is incorrect.")
		return
	}
	pw := r.FormValue("new")
	switch {
	case pw != r.FormValue("confirm"):
		fail("The new passwords do not match.")
		return
	case pw == r.FormValue("current"):
		fail("The new password must be different from the current one.")
		return
	}
	if err := w.Auth.SetPassword(r.Context(), u, pw); err != nil {
		fail(passwordError(err))
		return
	}
	w.redirectFlash(rw, r, next, "Password changed. Welcome!")
}

func passwordError(err error) string {
	msg := err.Error()
	return strings.ToUpper(msg[:1]) + msg[1:] + "."
}

func (w *Web) settingsTokens(rw http.ResponseWriter, r *http.Request) {
	w.renderTokens(rw, r, "")
}

func (w *Web) renderTokens(rw http.ResponseWriter, r *http.Request, created string) {
	toks, err := w.Store.ListTokens(r.Context(), currentUser(r).ID)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.render(rw, r, http.StatusOK, "settings_tokens", &Page{Title: "Access tokens", Tab: "settings", Data: map[string]any{
		"Tokens": toks, "Created": created,
	}})
}

func (w *Web) addToken(rw http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		w.redirectFlash(rw, r, "/settings/tokens", "Token name is required.")
		return
	}
	plain, _, err := w.Auth.NewToken(r.Context(), currentUser(r).ID, name)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	// Rendered directly (not via redirect) so the secret is shown exactly once.
	w.renderTokens(rw, r, plain)
}

func (w *Web) deleteToken(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := w.Store.DeleteToken(r.Context(), currentUser(r).ID, id); err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.redirectFlash(rw, r, "/settings/tokens", "Token revoked.")
}

// ---- admin ----

const usersPerPage = 50

var validUsername = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,38}$`)

func (w *Web) adminUsers(rw http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	users, total, err := w.Store.FindUsers(r.Context(), q, usersPerPage, (page-1)*usersPerPage)
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.render(rw, r, http.StatusOK, "admin_users", &Page{Title: "Users", Tab: "admin", Data: map[string]any{
		"Users": users, "Total": total, "Q": q, "Page": page, "HasNext": page*usersPerPage < total,
	}})
}

func (w *Web) adminCreateUser(rw http.ResponseWriter, r *http.Request) {
	u := &store.User{
		Username: strings.TrimSpace(r.FormValue("username")),
		Email:    strings.TrimSpace(r.FormValue("email")),
		FullName: strings.TrimSpace(r.FormValue("full_name")),
		Role:     store.Role(r.FormValue("role")),
		Active:   true,
	}
	if !validUsername.MatchString(u.Username) {
		w.redirectFlash(rw, r, "/admin/users", "Invalid username.")
		return
	}
	if !u.Role.Valid() {
		u.Role = store.RoleRead
	}
	if pw := r.FormValue("password"); pw != "" {
		if err := auth.ValidatePassword(pw); err != nil {
			w.redirectFlash(rw, r, "/admin/users", passwordError(err))
			return
		}
		var err error
		if u.PasswordHash, err = auth.HashPassword(pw); err != nil {
			w.serverError(rw, r, err)
			return
		}
		// An admin-chosen password is temporary by definition.
		u.MustChangePassword = true
	}
	if err := w.Store.CreateUser(r.Context(), u); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			w.redirectFlash(rw, r, "/admin/users", "That username is taken.")
			return
		}
		w.serverError(rw, r, err)
		return
	}
	w.redirectFlash(rw, r, "/admin/users", "User "+u.Username+" created.")
}

func (w *Web) adminLoadUser(rw http.ResponseWriter, r *http.Request) *store.User {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	u, err := w.Store.UserByID(r.Context(), id)
	if err != nil {
		w.fail(rw, r, err)
		return nil
	}
	return u
}

func (w *Web) adminEditUser(rw http.ResponseWriter, r *http.Request) {
	u := w.adminLoadUser(rw, r)
	if u == nil {
		return
	}
	w.render(rw, r, http.StatusOK, "admin_user", &Page{Title: u.Username, Tab: "admin", Data: map[string]any{"Target": u}})
}

func (w *Web) adminUpdateUser(rw http.ResponseWriter, r *http.Request) {
	u := w.adminLoadUser(rw, r)
	if u == nil {
		return
	}
	self := currentUser(r).ID == u.ID
	u.Email = strings.TrimSpace(r.FormValue("email"))
	u.FullName = strings.TrimSpace(r.FormValue("full_name"))
	if role := store.Role(r.FormValue("role")); role.Valid() && !self {
		u.Role = role
	}
	if !self {
		u.Active = r.FormValue("active") == "on"
	}
	if pw := r.FormValue("password"); pw != "" {
		if err := auth.ValidatePassword(pw); err != nil {
			w.redirectFlash(rw, r, "/admin/users/"+strconv.FormatInt(u.ID, 10), passwordError(err))
			return
		}
		var err error
		if u.PasswordHash, err = auth.HashPassword(pw); err != nil {
			w.serverError(rw, r, err)
			return
		}
		u.MustChangePassword = !self
	}
	if r.FormValue("unlink_sso") == "on" {
		u.OIDCSubject = nil
	}
	if err := w.Store.UpdateUser(r.Context(), u); err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.redirectFlash(rw, r, "/admin/users/"+strconv.FormatInt(u.ID, 10), "Saved.")
}

func (w *Web) adminDeleteUser(rw http.ResponseWriter, r *http.Request) {
	u := w.adminLoadUser(rw, r)
	if u == nil {
		return
	}
	if u.ID == currentUser(r).ID {
		w.redirectFlash(rw, r, "/admin/users", "You cannot delete yourself.")
		return
	}
	if err := w.Store.DeleteUser(r.Context(), u.ID); err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.redirectFlash(rw, r, "/admin/users", "User "+u.Username+" deleted.")
}
