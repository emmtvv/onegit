package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"onegit/internal/avatars"
	"onegit/internal/store"
)

// avatarTTL bounds how long another replica's avatar change takes to show.
const avatarTTL = 30 * time.Second

// avatarIndex maps users and commit emails to stored avatars so templates
// can render them without a query per avatar. Only users with an avatar
// are in it.
type avatarIndex struct {
	store *store.Store

	mu      sync.Mutex
	loaded  time.Time
	byID    map[int64]store.AvatarRef
	byEmail map[string]store.AvatarRef
}

func (ix *avatarIndex) snapshot() (map[int64]store.AvatarRef, map[string]store.AvatarRef) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if time.Since(ix.loaded) > avatarTTL {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		refs, err := ix.store.AvatarRefs(ctx)
		cancel()
		// On error keep the old maps; loaded is bumped either way so a
		// database hiccup is not retried on every avatar.
		ix.loaded = time.Now()
		if err == nil {
			ix.byID = make(map[int64]store.AvatarRef, len(refs))
			ix.byEmail = make(map[string]store.AvatarRef, len(refs))
			for _, r := range refs {
				ix.byID[r.UserID] = r
				if e := normEmail(r.Email); e != "" {
					ix.byEmail[e] = r
				}
			}
		}
	}
	return ix.byID, ix.byEmail
}

func (ix *avatarIndex) invalidate() {
	ix.mu.Lock()
	ix.loaded = time.Time{}
	ix.mu.Unlock()
}

func normEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func avatarImg(ref store.AvatarRef, name string, size []string) template.HTML {
	cls := "avatar"
	if len(size) > 0 {
		cls += " avatar-" + size[0]
	}
	return template.HTML(fmt.Sprintf(`<img class="%s" src="/avatars/%d?v=%d" alt="" title="%s" loading="lazy">`,
		cls, ref.UserID, ref.UpdatedAt.Unix(), template.HTMLEscapeString(name)))
}

// avatarFuncs are the template funcs that know about stored avatars; the
// package-level ones only draw initials.
func (w *Web) avatarFuncs() template.FuncMap {
	return template.FuncMap{
		// avatar is for git identities: matched to a user by email.
		"avatar": func(name, email string, size ...string) template.HTML {
			if _, byEmail := w.avatars.snapshot(); byEmail != nil {
				if ref, ok := byEmail[normEmail(email)]; ok && email != "" {
					return avatarImg(ref, name, size)
				}
			}
			return avatar(name, email, size...)
		},
		"useravatar": func(u *store.User, size ...string) template.HTML {
			if byID, _ := w.avatars.snapshot(); byID != nil {
				if ref, ok := byID[u.ID]; ok {
					return avatarImg(ref, u.Username, size)
				}
			}
			return avatar(u.Username, u.Email, size...)
		},
	}
}

func (w *Web) serveAvatar(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, obj, err := w.Avatars.Open(r.Context(), id)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	defer obj.Close()
	h := rw.Header()
	h.Set("Content-Type", a.ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if r.URL.Query().Get("v") != "" {
		// Versioned URLs change whenever the image does.
		h.Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "private, no-cache")
	}
	http.ServeContent(rw, r, "", a.UpdatedAt, obj)
}

func (w *Web) settingsAvatar(rw http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if cur, err := w.Store.Avatar(r.Context(), u.ID); err == nil && cur.Source == store.AvatarOIDC {
		w.redirectFlash(rw, r, "/settings", "Your avatar comes from "+w.Cfg.OIDC.DisplayName+" and cannot be changed here.")
		return
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		w.serverError(rw, r, err)
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, avatars.MaxSize+64<<10)
	f, _, err := r.FormFile("avatar")
	if err != nil {
		w.redirectFlash(rw, r, "/settings", "Choose an image of up to 1 MB.")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, avatars.MaxSize+1))
	if err != nil {
		w.serverError(rw, r, err)
		return
	}
	switch err := w.Avatars.Set(r.Context(), u.ID, store.AvatarManual, data); {
	case errors.Is(err, avatars.ErrTooLarge), errors.Is(err, avatars.ErrUnsupported):
		w.redirectFlash(rw, r, "/settings", "Upload a PNG, JPEG, GIF or WebP image of up to 1 MB.")
		return
	case err != nil:
		w.serverError(rw, r, err)
		return
	}
	w.avatars.invalidate()
	w.redirectFlash(rw, r, "/settings", "Avatar updated.")
}

func (w *Web) deleteAvatar(rw http.ResponseWriter, r *http.Request) {
	// Only a manual avatar can be removed; an IdP one comes back anyway.
	if err := w.Avatars.Delete(r.Context(), currentUser(r).ID, store.AvatarManual); err != nil {
		w.serverError(rw, r, err)
		return
	}
	w.avatars.invalidate()
	w.redirectFlash(rw, r, "/settings", "Avatar removed.")
}
