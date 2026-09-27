package auth

import (
	"slices"
	"testing"

	"onegit/internal/config"
	"onegit/internal/store"
)

func TestClaimStrings(t *testing.T) {
	if got := claimStrings([]any{"a", 1, "b"}); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("list = %v", got)
	}
	if got := claimStrings("single"); !slices.Equal(got, []string{"single"}) {
		t.Errorf("string = %v", got)
	}
	if got := claimStrings(42); got != nil {
		t.Errorf("number = %v", got)
	}
}

func TestSanitizeUsername(t *testing.T) {
	for in, want := range map[string]string{
		"john.doe": "john.doe", "John Doe": "John-Doe", "--x--": "x", "!!!": "user", ".hidden.": "hidden", "иван": "user",
	} {
		if got := sanitizeUsername(in); got != want {
			t.Errorf("sanitizeUsername(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRoleFor(t *testing.T) {
	cfg := config.Default()
	o := &OIDC{cfg: cfg}
	if got := o.roleFor([]string{"x"}); got != "" {
		t.Errorf("no mapping configured: %q", got)
	}
	cfg.OIDC.AdminGroup, cfg.OIDC.WriteGroup, cfg.OIDC.DefaultRole = "adm", "dev", "read"
	for groups, want := range map[string]store.Role{"adm": store.RoleAdmin, "dev": store.RoleWrite, "other": store.RoleRead} {
		if got := o.roleFor([]string{groups}); got != want {
			t.Errorf("roleFor(%s) = %q, want %q", groups, got, want)
		}
	}
	if got := o.roleFor([]string{"dev", "adm"}); got != store.RoleAdmin {
		t.Errorf("admin must win: %q", got)
	}
}
