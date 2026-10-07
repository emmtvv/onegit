package server_test

import (
	"net/http"
	"strings"
	"testing"

	"onegit/internal/store"
)

func TestTheme(t *testing.T) {
	t.Parallel()
	h := start(t, nil)
	admin := h.admin()
	dev := h.createUser(admin, "dev", "write")

	// By default the browser picks the theme.
	dev.ok("/settings", `href="/static/dark.css" media="(prefers-color-scheme: dark)"`, `value="system" checked`)

	for theme, want := range map[string]string{
		"dark":   `href="/static/dark.css" media="all"`,
		"light":  `href="/static/dark.css" media="not all"`,
		"system": `href="/static/dark.css" media="(prefers-color-scheme: dark)"`,
	} {
		if p := dev.post("/settings/theme", "theme", theme); p.status != http.StatusSeeOther {
			t.Fatalf("set %s: %d", theme, p.status)
		}
		dev.ok("/settings", want, `value="`+theme+`" checked`)
	}

	// The setting is per user.
	dev.post("/settings/theme", "theme", "dark")
	if body := admin.ok("/settings"); strings.Contains(body, `href="/static/dark.css" media="all"`) {
		t.Error("another user's theme applied")
	}

	dev.post("/settings/theme", "theme", "purple")
	if u, err := h.srv.Store.UserByUsername(ctx, "dev"); err != nil || u.Theme != store.ThemeDark {
		t.Errorf("invalid theme saved: %+v, %v", u, err)
	}

	for _, path := range []string{"/static/dark.css", "/static/chroma-light.css", "/static/chroma-dark.css"} {
		if p := dev.get(path); p.status != http.StatusOK || !strings.HasPrefix(p.header.Get("Content-Type"), "text/css") {
			t.Errorf("%s: %d %q", path, p.status, p.header.Get("Content-Type"))
		}
	}
}
