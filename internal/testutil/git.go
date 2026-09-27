package testutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"onegit/internal/git"
)

// Tests must not depend on the developer's git configuration (signing,
// hooks, default branch, ...). This also covers git commands run by the code
// under test, which inherit the process environment.
func init() {
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
}

// GitEnv isolates git from the developer's configuration and fixes the
// identity, so tests behave the same everywhere.
func GitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C",
		"GIT_AUTHOR_NAME=Alice", "GIT_AUTHOR_EMAIL=alice@example.com",
		"GIT_COMMITTER_NAME=Alice", "GIT_COMMITTER_EMAIL=alice@example.com",
	)
}

// Work is a non-bare repository to create commits in.
type Work struct {
	t   testing.TB
	Dir string
	// Env is appended to every git command (e.g. a different author).
	Env []string
	// clock makes commit times strictly increasing.
	clock time.Time
}

// NewWork creates an empty repository on branch main.
func NewWork(t testing.TB) *Work {
	t.Helper()
	w := &Work{t: t, Dir: t.TempDir(), clock: time.Unix(1700000000, 0)}
	w.Git("init", "-q", "-b", "main")
	return w
}

// Git runs git in the work tree and returns trimmed stdout.
func (w *Work) Git(args ...string) string {
	w.t.Helper()
	out, err := w.TryGit(args...)
	if err != nil {
		w.t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// TryGit runs git and returns stdout, or an error including stderr.
func (w *Work) TryGit(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = w.Dir
	cmd.Env = append(GitEnv(), w.Env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", &gitError{err: err, stderr: stderr.String()}
	}
	return strings.TrimSpace(string(out)), nil
}

type gitError struct {
	err    error
	stderr string
}

func (e *gitError) Error() string { return e.err.Error() + ": " + strings.TrimSpace(e.stderr) }

// Write creates or replaces files (paths relative to the work tree).
func (w *Work) Write(files map[string]string) {
	w.t.Helper()
	for name, content := range files {
		p := filepath.Join(w.Dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			w.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			w.t.Fatal(err)
		}
	}
}

// Remove deletes files from the work tree.
func (w *Work) Remove(names ...string) {
	w.t.Helper()
	for _, n := range names {
		if err := os.RemoveAll(filepath.Join(w.Dir, n)); err != nil {
			w.t.Fatal(err)
		}
	}
}

// Commit writes files, stages everything and commits; it returns the SHA.
func (w *Work) Commit(msg string, files map[string]string) string {
	w.t.Helper()
	w.Write(files)
	w.Git("add", "-A")
	w.clock = w.clock.Add(time.Minute)
	date := w.clock.Format(time.RFC3339)
	cmd := []string{"-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", msg}
	env := w.Env
	w.Env = append(append([]string{}, env...), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	w.Git(cmd...)
	w.Env = env
	return w.Git("rev-parse", "HEAD")
}

// Bare creates a bare repository through git.Open. Its hooks run `true`, so
// local pushes into it succeed without a server.
func Bare(t testing.TB) *git.Repo {
	t.Helper()
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	r, err := git.Open(context.Background(), filepath.Join(t.TempDir(), "repo.git"), "main", truePath)
	if err != nil {
		t.Fatalf("open bare repo: %v", err)
	}
	return r
}

// Push pushes refspecs from the work tree into a bare repository.
func (w *Work) Push(bare *git.Repo, refspecs ...string) {
	w.t.Helper()
	w.Git(append([]string{"push", "-q", "--force", bare.Path}, refspecs...)...)
}
