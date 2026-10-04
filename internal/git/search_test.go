package git_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"onegit/internal/git"
	"onegit/internal/testutil"
)

func TestBlame(t *testing.T) {
	t.Parallel()
	repo, w := testutil.Bare(t), testutil.NewWork(t)
	c1 := w.Commit("first", map[string]string{"dir/f.txt": "one\ntwo\nthree\n"})
	c2 := w.Commit("second", map[string]string{"dir/f.txt": "one\nTWO\nthree\nfour\n"})
	w.Push(repo, "main")

	hunks, err := repo.Blame(ctx, c2, "dir/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		sha   string
		start int
		lines []string
	}
	exp := []want{{c1, 1, []string{"one"}}, {c2, 2, []string{"TWO"}}, {c1, 3, []string{"three"}}, {c2, 4, []string{"four"}}}
	if len(hunks) != len(exp) {
		t.Fatalf("got %d hunks, want %d", len(hunks), len(exp))
	}
	for i, h := range hunks {
		e := exp[i]
		if h.Commit.SHA != e.sha || h.StartLine != e.start || len(h.Lines) != len(e.lines) || h.Lines[0] != e.lines[0] {
			t.Errorf("hunk %d = %s@%d %q, want %s@%d %q", i, h.Commit.SHA[:7], h.StartLine, h.Lines, e.sha[:7], e.start, e.lines)
		}
		if h.Commit.Subject == "" || h.Commit.Author.Name != "Alice" || h.Commit.Author.When.IsZero() {
			t.Errorf("hunk %d: commit metadata missing: %+v", i, h.Commit)
		}
	}
	// The second commit changed existing lines: blame points before it.
	if hunks[1].PrevSHA != c1 || hunks[1].PrevPath != "dir/f.txt" {
		t.Errorf("previous = %q %q, want %s dir/f.txt", hunks[1].PrevSHA, hunks[1].PrevPath, c1)
	}
	if hunks[0].PrevSHA != "" {
		t.Errorf("lines added by the root commit have previous %q", hunks[0].PrevSHA)
	}
	// Both hunks of a commit share one Commit value.
	if hunks[1].Commit != hunks[3].Commit {
		t.Error("commit metadata is not shared between hunks")
	}
	if _, err := repo.Blame(ctx, c2, "missing.txt"); err == nil {
		t.Error("blame of a missing file succeeded")
	}
}

func TestGrepAndFindFiles(t *testing.T) {
	t.Parallel()
	repo, w := testutil.Bare(t), testutil.NewWork(t)
	sha := w.Commit("files", map[string]string{
		"services/api/main.go": "package main\n// Hello API\nfunc hello() {}\n",
		"services/web/app.js":  "console.log('hello web')\n",
		"docs/readme.md":       "nothing to see\n",
		"bin/blob":             "hello\x00binary",
	})
	w.Push(repo, "main")

	m, trunc, err := repo.Grep(ctx, sha, git.GrepOptions{Pattern: "hello", IgnoreCase: true})
	if err != nil || trunc {
		t.Fatal(err, trunc)
	}
	var got []string
	for _, x := range m {
		got = append(got, x.Path+":"+strconv.Itoa(x.Line))
	}
	if want := []string{"services/api/main.go:2", "services/api/main.go:3", "services/web/app.js:1"}; !slices.Equal(got, want) {
		t.Errorf("matches = %v, want %v", got, want)
	}
	if m[0].Text != "// Hello API" {
		t.Errorf("text = %q", m[0].Text)
	}
	m, _, _ = repo.Grep(ctx, sha, git.GrepOptions{Pattern: "Hello"})
	if len(m) != 1 {
		t.Errorf("case-sensitive: %v", m)
	}
	m, _, _ = repo.Grep(ctx, sha, git.GrepOptions{Pattern: "hel+o", Regex: true, Paths: []string{":(glob)**/*.js"}})
	if len(m) != 1 || m[0].Path != "services/web/app.js" {
		t.Errorf("regex with glob path: %v", m)
	}
	m, _, _ = repo.Grep(ctx, sha, git.GrepOptions{Pattern: "hello", IgnoreCase: true, Paths: []string{"services", ":(exclude)services/web"}})
	if len(m) != 2 {
		t.Errorf("exclude: %v", m)
	}
	m, trunc, _ = repo.Grep(ctx, sha, git.GrepOptions{Pattern: "e", MaxMatches: 2})
	if len(m) != 2 || !trunc {
		t.Errorf("limit: %d %v", len(m), trunc)
	}
	if m, _, err := repo.Grep(ctx, sha, git.GrepOptions{Pattern: "zzz-nothing"}); err != nil || len(m) != 0 {
		t.Errorf("no match: %v %v", m, err)
	}
	if _, _, err := repo.Grep(ctx, sha, git.GrepOptions{Pattern: "a(", Regex: true}); !errors.Is(err, git.ErrBadPattern) {
		t.Errorf("bad regex: %v", err)
	}

	files, trunc, err := repo.FindFiles(ctx, sha, 10, func(p string) bool { return strings.Contains(p, "services/") })
	if err != nil || trunc || !slices.Equal(files, []string{"services/api/main.go", "services/web/app.js"}) {
		t.Errorf("FindFiles = %v %v %v", files, trunc, err)
	}
	files, trunc, _ = repo.FindFiles(ctx, sha, 1, func(string) bool { return true })
	if len(files) != 1 || !trunc {
		t.Errorf("FindFiles limit = %v %v", files, trunc)
	}
	if _, _, err := repo.FindFiles(ctx, "0123456789012345678901234567890123456789", 10, func(string) bool { return true }); err == nil {
		t.Error("FindFiles on a missing commit succeeded")
	}
}
