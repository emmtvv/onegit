package registry

import (
	"strings"
	"testing"
	"time"
)

func TestToken(t *testing.T) {
	s := &Service{tokenKey: []byte("key")}
	tok := s.issueToken("u42", time.Now())
	if sub, ok := s.verifyToken(tok); !ok || sub.userID != 42 || sub.jobID != 0 {
		t.Fatalf("verify(%q) = %+v, %v", tok, sub, ok)
	}
	if sub, ok := s.verifyToken(s.issueToken("u0", time.Now())); !ok || sub.userID != 0 || sub.jobID != 0 {
		t.Fatalf("anonymous token: %+v, %v", sub, ok)
	}
	if sub, ok := s.verifyToken(s.issueToken("j7", time.Now())); !ok || sub.jobID != 7 || sub.userID != 0 {
		t.Fatalf("job token: %+v, %v", sub, ok)
	}
	bad := map[string]string{
		"empty":              "",
		"access token":       "og_something",
		"forged user":        strings.Replace(tok, "ogr.u42.", "ogr.u43.", 1),
		"user made into job": strings.Replace(tok, "ogr.u42.", "ogr.j42.", 1),
		"expired":            s.issueToken("u42", time.Now().Add(-2*tokenLifetime)),
		"other key":          (&Service{tokenKey: []byte("other")}).issueToken("u42", time.Now()),
		"unknown subject":    s.issueToken("x42", time.Now()),
	}
	for name, b := range bad {
		if _, ok := s.verifyToken(b); ok {
			t.Errorf("%s: %q accepted", name, b)
		}
	}
}

func TestNames(t *testing.T) {
	for name, want := range map[string]bool{
		"app": true, "team/app": true, "a/b/c-d_e.f": true, "a__b": true,
		"App": false, "/app": false, "app/": false, "a//b": false, "-app": false, "a..b": false,
	} {
		if got := nameRe.MatchString(name); got != want {
			t.Errorf("name %q: got %v, want %v", name, got, want)
		}
	}
}
