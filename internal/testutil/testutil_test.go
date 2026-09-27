package testutil

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestHelpers(t *testing.T) {
	if Password("ann") != "password-ann" {
		t.Error("Password")
	}
	n := 0
	Eventually(t, time.Second, "counter", func() bool { n++; return n == 3 })
	if os.Getenv("GIT_CONFIG_GLOBAL") != os.DevNull {
		t.Error("git configuration not isolated")
	}
	w := NewWork(t)
	sha := w.Commit("first", map[string]string{"a/b.txt": "x"})
	if len(sha) != 40 || !strings.Contains(w.Git("log", "--format=%an %s"), "Alice first") {
		t.Errorf("commit %q", sha)
	}
	bare := Bare(t)
	w.Push(bare, "main")
	if _, err := w.TryGit("rev-parse", "nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("TryGit error = %v", err)
	}
}
