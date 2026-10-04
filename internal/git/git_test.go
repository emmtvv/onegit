package git_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"onegit/internal/git"
	"onegit/internal/testutil"
)

var ctx = context.Background()

// fixture: main has three commits, feature/x/y (a branch with slashes) one
// more, and an annotated tag v1.0 on the second commit.
type fixture struct {
	repo                   *git.Repo
	work                   *testutil.Work
	c1, c2, c3, featureTip string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{repo: testutil.Bare(t), work: testutil.NewWork(t)}
	w := f.work
	f.c1 = w.Commit("initial", map[string]string{"README.md": "# hello\n", "src/main.go": "package main\n", "docs/a.md": "a\n"})
	f.c2 = w.Commit("add util", map[string]string{"src/util.go": "package main\n\nfunc util() {}\n"})
	w.Git("tag", "-a", "v1.0", "-m", "release 1.0")
	f.c3 = w.Commit("edit readme", map[string]string{"README.md": "# hello\n\nmore text\n"})
	w.Git("checkout", "-q", "-b", "feature/x/y")
	f.featureTip = w.Commit("feature work", map[string]string{"src/feature.go": "package main\n"})
	w.Git("checkout", "-q", "main")
	w.Push(f.repo, "main", "feature/x/y", "v1.0")
	return f
}

func TestOpenInitialisesRepository(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "repo.git")
	r, err := git.Open(ctx, dir, "trunk", "/usr/local/bin/onegit")
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsEmpty(ctx) {
		t.Error("new repository is not empty")
	}
	if got := r.HeadBranch(ctx); got != "trunk" {
		t.Errorf("HEAD = %q, want trunk", got)
	}
	for _, hook := range []string{"pre-receive", "post-receive"} {
		b, err := os.ReadFile(filepath.Join(dir, "hooks", hook))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `exec "/usr/local/bin/onegit" hook `+hook) {
			t.Errorf("%s hook = %q", hook, b)
		}
		if st, _ := os.Stat(filepath.Join(dir, "hooks", hook)); st.Mode()&0o111 == 0 {
			t.Errorf("%s hook is not executable", hook)
		}
	}
	out, err := r.Cmd(ctx, "config", "http.receivepack").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Errorf("http.receivepack = %q, %v", out, err)
	}
	// Reopening keeps the repository and rewrites hooks for a new binary.
	if _, err := git.Open(ctx, dir, "main", "/opt/onegit"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "hooks", "pre-receive"))
	if !strings.Contains(string(b), "/opt/onegit") {
		t.Errorf("hooks not reinstalled: %q", b)
	}
	if got := r.HeadBranch(ctx); got != "trunk" {
		t.Errorf("reopen changed HEAD to %q", got)
	}
}

func TestRefs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.repo
	if r.IsEmpty(ctx) {
		t.Fatal("repository is empty after push")
	}
	branches, err := r.Branches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, b := range branches {
		if b.Kind != git.KindBranch {
			t.Errorf("%s has kind %v", b.Name, b.Kind)
		}
		names[b.Name] = b.SHA
	}
	if names["main"] != f.c3 || names["feature/x/y"] != f.featureTip {
		t.Errorf("branches = %v", names)
	}
	tags, err := r.Tags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Name != "v1.0" || tags[0].SHA != f.c2 || tags[0].Kind != git.KindTag {
		t.Errorf("tags = %+v (annotated tags must be peeled to the commit)", tags)
	}

	for rev, want := range map[string]string{
		"main": f.c3, "refs/heads/main": f.c3, "v1.0": f.c2, f.c1[:8]: f.c1, "main~2": f.c1,
	} {
		got, err := r.ResolveCommit(ctx, rev)
		if err != nil || got != want {
			t.Errorf("ResolveCommit(%q) = %q, %v; want %q", rev, got, err, want)
		}
	}
	for _, rev := range []string{"", "nope", "-h", "--all", "v1.0:README.md"} {
		if _, err := r.ResolveCommit(ctx, rev); !errors.Is(err, git.ErrNotExist) {
			t.Errorf("ResolveCommit(%q) error = %v, want ErrNotExist", rev, err)
		}
	}
}

func TestSplitRefPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// A tag that is a prefix of a branch must lose to the longer name.
	f.work.Git("tag", "feature", f.c1)
	f.work.Push(f.repo, "refs/tags/feature")

	cases := []struct {
		in, ref, path string
		kind          git.RefKind
	}{
		{"main", "main", "", git.KindBranch},
		{"main/src/main.go", "main", "src/main.go", git.KindBranch},
		{"/main/src/", "main", "src", git.KindBranch},
		{"feature/x/y/src/feature.go", "feature/x/y", "src/feature.go", git.KindBranch},
		{"feature/docs", "feature", "docs", git.KindTag},
		{"v1.0/src", "v1.0", "src", git.KindTag},
		{f.c1 + "/README.md", f.c1, "README.md", git.KindCommit},
		{f.c1[:7] + "/README.md", f.c1, "README.md", git.KindCommit},
	}
	for _, c := range cases {
		ref, p, err := f.repo.SplitRefPath(ctx, c.in)
		if err != nil {
			t.Errorf("SplitRefPath(%q): %v", c.in, err)
			continue
		}
		if ref.Name != c.ref || p != c.path || ref.Kind != c.kind {
			t.Errorf("SplitRefPath(%q) = %q (%v), %q; want %q (%v), %q", c.in, ref.Name, ref.Kind, p, c.ref, c.kind, c.path)
		}
	}
	for _, in := range []string{"nope/README.md", "abc/x", "zzzzzzz", "main~1/README.md", "main^/src", "main@{1}"} {
		if _, _, err := f.repo.SplitRefPath(ctx, in); !errors.Is(err, git.ErrNotExist) {
			t.Errorf("SplitRefPath(%q) error = %v, want ErrNotExist", in, err)
		}
	}
}

func TestListRefs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.work.Git("branch", "feature/z")
	f.work.Push(f.repo, "feature/z")

	all, more, err := f.repo.ListRefs(ctx, git.KindBranch, git.RefQuery{})
	if err != nil || more || len(all) != 3 {
		t.Fatalf("ListRefs = %v, %v, %v", all, more, err)
	}
	page, more, err := f.repo.ListRefs(ctx, git.KindBranch, git.RefQuery{Query: "FEATURE", Limit: 1})
	if err != nil || !more || len(page) != 1 || !strings.HasPrefix(page[0].Name, "feature/") {
		t.Errorf("first page = %v, %v, %v", page, more, err)
	}
	next, more, err := f.repo.ListRefs(ctx, git.KindBranch, git.RefQuery{Query: "feature", Offset: 1, Limit: 1})
	if err != nil || more || len(next) != 1 || next[0].Name == page[0].Name {
		t.Errorf("second page = %v, %v, %v", next, more, err)
	}
	tags, _, err := f.repo.ListRefs(ctx, git.KindTag, git.RefQuery{Limit: 10})
	if err != nil || len(tags) != 1 || tags[0].SHA != f.c2 {
		t.Errorf("tags = %v, %v (want v1.0 peeled to %s)", tags, err, f.c2)
	}

	byName, err := f.repo.RefsByName(ctx, git.KindBranch, []string{"feature", "feature/z", "nope"})
	if err != nil || len(byName) != 1 || byName["feature/z"].SHA != f.c3 {
		t.Errorf("RefsByName = %v, %v", byName, err)
	}

	ab, err := f.repo.AheadBehindMany(ctx, f.c1, []string{"feature/x/y", "main", "nope"})
	if err != nil || ab["feature/x/y"] != [2]int{3, 0} || ab["main"] != [2]int{2, 0} || len(ab) != 2 {
		t.Errorf("AheadBehindMany = %v, %v", ab, err)
	}

	on, err := f.repo.BranchesContaining(ctx, f.featureTip)
	if err != nil || !slices.Equal(on, []string{"feature/x/y"}) {
		t.Errorf("BranchesContaining(feature tip) = %v, %v", on, err)
	}
	if on, _ := f.repo.BranchesContaining(ctx, f.c1); len(on) != 3 {
		t.Errorf("BranchesContaining(c1) = %v", on)
	}

	// The sorted list is cached: new, moved and deleted refs must show.
	f.work.Git("checkout", "-q", "-b", "fresh")
	fresh := f.work.Commit("fresh work", map[string]string{"fresh.txt": "x"})
	f.work.Git("checkout", "-q", "main")
	f.work.Push(f.repo, "fresh", "+fresh:feature/x/y", ":feature/z")
	all, _, err = f.repo.ListRefs(ctx, git.KindBranch, git.RefQuery{})
	if err != nil || len(all) != 3 || all[0].Name != "feature/x/y" && all[0].Name != "fresh" || all[0].SHA != fresh ||
		all[1].SHA != fresh || all[2].Name != "main" {
		t.Errorf("after pushes = %+v, %v", all, err)
	}
}

func TestHead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if err := f.repo.SetHead(ctx, "feature/x/y"); err != nil {
		t.Fatal(err)
	}
	if got := f.repo.HeadBranch(ctx); got != "feature/x/y" {
		t.Errorf("HeadBranch = %q", got)
	}
}

func TestTreeAndBlobs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.repo
	f.work.Commit("more files", map[string]string{
		"Zeta.txt": "z", "alpha.txt": "a", "bin.dat": "a\x00b", "lib/x.go": "x",
	})
	f.work.Git("-c", "core.symlinks=true", "update-index", "--add", "--cacheinfo", "120000,"+hashObject(t, f.work, "README.md")+",link")
	f.work.Git("-c", "commit.gpgsign=false", "commit", "-q", "-m", "symlink")
	f.work.Push(r, "main")
	head, _ := r.ResolveCommit(ctx, "main")

	entries, err := r.ListTree(ctx, head, "")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, e := range entries {
		order = append(order, e.Name)
	}
	// Directories first, then case-insensitive by name.
	want := []string{"docs", "lib", "src", "alpha.txt", "bin.dat", "link", "README.md", "Zeta.txt"}
	if !slices.Equal(order, want) {
		t.Errorf("ListTree order = %v, want %v", order, want)
	}
	for _, e := range entries {
		switch e.Name {
		case "src":
			if !e.IsDir() || e.Type != git.EntryTree || e.Path != "src" {
				t.Errorf("src entry = %+v", e)
			}
		case "link":
			if !e.IsSymlink() {
				t.Errorf("link entry = %+v", e)
			}
		case "alpha.txt":
			if e.Size != 1 || len(e.SHA) != 40 {
				t.Errorf("alpha.txt entry = %+v", e)
			}
		}
	}
	sub, err := r.ListTree(ctx, head, "src")
	if err != nil || len(sub) != 2 || sub[0].Path != "src/main.go" || sub[1].Path != "src/util.go" {
		t.Errorf("ListTree(src) = %+v, %v", sub, err)
	}
	if _, err := r.ListTree(ctx, head, "missing"); !errors.Is(err, git.ErrNotExist) {
		t.Errorf("ListTree(missing) error = %v", err)
	}

	for p, want := range map[string]string{"": "tree", "src": "tree", "README.md": "blob", "missing": ""} {
		if got := r.ObjectType(ctx, head, p); got != want {
			t.Errorf("ObjectType(%q) = %q, want %q", p, got, want)
		}
	}
	if n, err := r.BlobSize(ctx, head, "README.md"); err != nil || n != int64(len("# hello\n\nmore text\n")) {
		t.Errorf("BlobSize = %d, %v", n, err)
	}
	if _, err := r.BlobSize(ctx, head, "missing"); !errors.Is(err, git.ErrNotExist) {
		t.Errorf("BlobSize(missing) error = %v", err)
	}
	b, err := r.ReadBlob(ctx, head, "README.md", 0)
	if err != nil || string(b) != "# hello\n\nmore text\n" {
		t.Errorf("ReadBlob = %q, %v", b, err)
	}
	b, err = r.ReadBlob(ctx, head, "README.md", 4)
	if err != nil || string(b) != "# he" {
		t.Errorf("ReadBlob limited = %q, %v", b, err)
	}
	if _, err := r.ReadBlob(ctx, head, "missing", 0); err == nil {
		t.Error("ReadBlob(missing) succeeded")
	}
	var buf bytes.Buffer
	if err := r.StreamBlob(ctx, head, "bin.dat", &buf); err != nil || buf.String() != "a\x00b" {
		t.Errorf("StreamBlob = %q, %v", buf.String(), err)
	}
	if !git.IsBinary(buf.Bytes()) || git.IsBinary([]byte("plain text")) {
		t.Error("IsBinary misclassifies")
	}
	late := append(bytes.Repeat([]byte("x"), 9000), 0)
	if git.IsBinary(late) {
		t.Error("IsBinary must only look at the first 8000 bytes")
	}
}

func hashObject(t *testing.T, w *testutil.Work, path string) string {
	t.Helper()
	return w.Git("hash-object", "-w", path)
}

func TestLog(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.repo
	cs, err := r.Log(ctx, "main", git.LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 || cs[0].SHA != f.c3 || cs[2].SHA != f.c1 {
		t.Fatalf("Log = %d commits", len(cs))
	}
	c := cs[0]
	if c.Subject != "edit readme" || c.Author.Name != "Alice" || c.Author.Email != "alice@example.com" ||
		len(c.Parents) != 1 || c.Parents[0] != f.c2 || c.Short() != f.c3[:10] || c.Author.When.IsZero() {
		t.Errorf("commit = %+v", c)
	}
	if len(cs[2].Parents) != 0 {
		t.Errorf("root commit has parents %v", cs[2].Parents)
	}

	page, _ := r.Log(ctx, "main", git.LogOptions{Skip: 1, Limit: 1})
	if len(page) != 1 || page[0].SHA != f.c2 {
		t.Errorf("Log skip/limit = %v", page)
	}
	byPath, _ := r.Log(ctx, "main", git.LogOptions{Path: "src"})
	if len(byPath) != 2 || byPath[0].SHA != f.c2 {
		t.Errorf("Log path filter = %d commits", len(byPath))
	}
	rng, _ := r.Log(ctx, "main.."+f.featureTip, git.LogOptions{})
	if len(rng) != 1 || rng[0].SHA != f.featureTip {
		t.Errorf("Log range = %d commits", len(rng))
	}
	if _, err := r.Log(ctx, "nope", git.LogOptions{}); err == nil {
		t.Error("Log of a missing rev succeeded")
	}

	// Multi-line body.
	f.work.Git("-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "subject line", "-m", "body line 1\nbody line 2")
	f.work.Push(r, "main")
	c, err = r.GetCommit(ctx, "main")
	if err != nil || c.Subject != "subject line" || c.Body != "body line 1\nbody line 2" {
		t.Errorf("GetCommit = %+v, %v", c, err)
	}
	if _, err := r.GetCommit(ctx, "nope"); !errors.Is(err, git.ErrNotExist) {
		t.Errorf("GetCommit(nope) error = %v", err)
	}

	last, err := r.LastCommitFor(ctx, "main", "README.md")
	if err != nil || last.SHA != f.c3 {
		t.Errorf("LastCommitFor(README.md) = %v, %v", last, err)
	}
	if _, err := r.LastCommitFor(ctx, "main", "nope.txt"); !errors.Is(err, git.ErrNotExist) {
		t.Errorf("LastCommitFor(nope) error = %v", err)
	}
}

func TestLastCommits(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.repo
	got, err := r.LastCommits(ctx, "main", "", []string{"README.md", "src", "docs", "missing"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got["README.md"].SHA != f.c3 || got["src"].SHA != f.c2 || got["docs"].SHA != f.c1 {
		t.Errorf("LastCommits = README %v, src %v, docs %v", got["README.md"], got["src"], got["docs"])
	}
	if _, ok := got["missing"]; ok {
		t.Error("a name without commits was resolved")
	}
	sub, _ := r.LastCommits(ctx, "main", "src", []string{"main.go", "util.go"}, 100)
	if sub["main.go"].SHA != f.c1 || sub["util.go"].SHA != f.c2 {
		t.Errorf("LastCommits(src) = %v", sub)
	}
	// The scan stops after maxCommits.
	capped, _ := r.LastCommits(ctx, "main", "", []string{"README.md", "docs"}, 1)
	if _, ok := capped["docs"]; ok {
		t.Error("maxCommits not honoured")
	}
	// Non-ASCII names must not be quoted by git.
	f.work.Commit("unicode", map[string]string{"документ.md": "привет"})
	f.work.Push(r, "main")
	u, _ := r.LastCommits(ctx, "main", "", []string{"документ.md"}, 100)
	if u["документ.md"] == nil {
		t.Error("non-ASCII path not resolved")
	}
}

func TestAncestryAndCounts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.repo
	if ok, err := r.IsAncestor(ctx, f.c1, f.c3); !ok || err != nil {
		t.Errorf("IsAncestor(c1, c3) = %v, %v", ok, err)
	}
	if ok, err := r.IsAncestor(ctx, f.featureTip, f.c3); ok || err != nil {
		t.Errorf("IsAncestor(feature, main) = %v, %v", ok, err)
	}
	if _, err := r.IsAncestor(ctx, "nope", f.c3); err == nil {
		t.Error("IsAncestor with a bad rev returned no error")
	}
	if mb, err := r.MergeBase(ctx, "main", "feature/x/y"); err != nil || mb != f.c3 {
		t.Errorf("MergeBase = %q, %v", mb, err)
	}
	if ahead, behind, err := r.AheadBehind(ctx, "feature/x/y", f.c1); err != nil || ahead != 3 || behind != 0 {
		t.Errorf("AheadBehind = %d, %d, %v", ahead, behind, err)
	}
	if n, err := r.CountCommits(ctx, f.c1, "feature/x/y"); err != nil || n != 3 {
		t.Errorf("CountCommits = %d, %v", n, err)
	}
	files, err := r.ChangedFiles(ctx, f.c1, f.featureTip)
	slices.Sort(files)
	if err != nil || !slices.Equal(files, []string{"README.md", "src/feature.go", "src/util.go"}) {
		t.Errorf("ChangedFiles = %v, %v", files, err)
	}

	// A rename is two paths for ChangedFiles and one for CountChangedFiles.
	f.work.Git("mv", "docs/a.md", "docs/b.md")
	f.work.Commit("rename", nil)
	f.work.Push(r, "main")
	files, _ = r.ChangedFiles(ctx, "main~1", "main")
	slices.Sort(files)
	if !slices.Equal(files, []string{"docs/a.md", "docs/b.md"}) {
		t.Errorf("ChangedFiles(rename) = %v", files)
	}
	if n, _ := r.CountChangedFiles(ctx, "main~1", "main"); n != 1 {
		t.Errorf("CountChangedFiles(rename) = %d", n)
	}

	ids := r.BlobIDs(ctx, []string{"main:README.md", "main:missing", f.c1 + ":src/main.go"})
	if len(ids["main:README.md"]) != 40 || ids["main:missing"] != "" || len(ids[f.c1+":src/main.go"]) != 40 {
		t.Errorf("BlobIDs = %v", ids)
	}
	if len(r.BlobIDs(ctx, nil)) != 0 {
		t.Error("BlobIDs(nil) not empty")
	}
}

func TestDiff(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.repo
	w := f.work
	w.Git("mv", "docs/a.md", "docs/renamed.md")
	w.Remove("src/util.go")
	w.Commit("mixed change", map[string]string{
		"README.md":   "# hello\n\nchanged text\nnew line\n",
		"new.txt":     "one\ntwo\n",
		"image.bin":   "\x00\x01\x02",
		"with space":  "x\n",
		"src/main.go": "package main\n",
	})
	w.Push(r, "main")
	c, err := r.GetCommit(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	d, err := r.DiffCommit(ctx, c, git.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]*git.FileDiff{}
	for _, fd := range d.Files {
		byPath[fd.Path()] = fd
	}
	if fd := byPath["new.txt"]; fd == nil || fd.Status != git.StatusAdded || fd.Additions != 2 || fd.NewMode != "100644" {
		t.Errorf("new.txt diff = %+v", fd)
	}
	if fd := byPath["src/util.go"]; fd == nil || fd.Status != git.StatusDeleted || fd.Deletions != 3 || fd.Path() != "src/util.go" {
		t.Errorf("src/util.go diff = %+v", fd)
	}
	if fd := byPath["docs/renamed.md"]; fd == nil || fd.Status != git.StatusRenamed || fd.OldPath != "docs/a.md" {
		t.Errorf("rename diff = %+v", fd)
	}
	if fd := byPath["image.bin"]; fd == nil || !fd.Binary || len(fd.Hunks) != 0 {
		t.Errorf("binary diff = %+v", fd)
	}
	if fd := byPath["with space"]; fd == nil || fd.OldPath != "with space" {
		t.Errorf("path with a space = %+v", fd)
	}
	fd := byPath["README.md"]
	if fd == nil || fd.Status != git.StatusModified || fd.Additions != 2 || fd.Deletions != 1 || len(fd.Hunks) != 1 {
		t.Fatalf("README.md diff = %+v", fd)
	}
	h := fd.Hunks[0]
	if h.OldStart != 1 || h.NewStart != 1 || h.OldLines != 3 || h.NewLines != 4 {
		t.Errorf("hunk header = %+v", h)
	}
	var kinds strings.Builder
	for _, l := range h.Lines {
		kinds.WriteByte(byte(l.Kind))
	}
	if kinds.String() != "  -++" {
		t.Errorf("line kinds = %q", kinds.String())
	}
	if l := h.Lines[3]; l.NewNo != 3 || l.OldNo != 0 || l.Content != "changed text" {
		t.Errorf("added line = %+v", l)
	}
	if l := h.Lines[2]; l.OldNo != 3 || l.NewNo != 0 {
		t.Errorf("deleted line = %+v", l)
	}
	if d.Additions == 0 || d.Deletions == 0 || d.Truncated {
		t.Errorf("totals = +%d -%d truncated=%v", d.Additions, d.Deletions, d.Truncated)
	}

	// Path filter and limits.
	only, _ := r.DiffRange(ctx, c.Parents[0], c.SHA, git.DiffOptions{Paths: []string{"new.txt"}})
	if len(only.Files) != 1 {
		t.Errorf("path filter gave %d files", len(only.Files))
	}
	limited, _ := r.DiffRange(ctx, c.Parents[0], c.SHA, git.DiffOptions{MaxFiles: 2})
	if len(limited.Files) != 2 || !limited.Truncated {
		t.Errorf("MaxFiles: %d files, truncated=%v", len(limited.Files), limited.Truncated)
	}
	perFile, _ := r.DiffRange(ctx, c.Parents[0], c.SHA, git.DiffOptions{MaxFileLines: 1})
	for _, fd := range perFile.Files {
		if fd.Path() == "README.md" && (!fd.Truncated || fd.Hunks != nil || fd.Additions != 2) {
			t.Errorf("MaxFileLines: %+v", fd)
		}
	}

	// The root commit diffs against the empty tree.
	root, _ := r.GetCommit(ctx, f.c1)
	rd, err := r.DiffCommit(ctx, root, git.DiffOptions{})
	if err != nil || len(rd.Files) != 3 {
		t.Errorf("root diff = %v files, %v", len(rd.Files), err)
	}
}

func TestMergeTreeAndCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.repo
	w := f.work
	// A clean change on main and a conflicting one on "conflict".
	w.Git("checkout", "-q", "-b", "conflict", f.c3)
	conflictTip := w.Commit("conflicting", map[string]string{"README.md": "# totally different\n"})
	w.Git("checkout", "-q", "main")
	mainTip := w.Commit("main moves", map[string]string{"README.md": "# hello\n\nmain text\n", "other.txt": "o"})
	w.Push(r, "main", "conflict")

	clean, err := r.MergeTree(ctx, "", mainTip, f.featureTip)
	if err != nil || len(clean.Conflicts) != 0 || len(clean.Tree) != 40 {
		t.Fatalf("clean merge = %+v, %v", clean, err)
	}
	bad, err := r.MergeTree(ctx, "", mainTip, conflictTip)
	if err != nil || !slices.Equal(bad.Conflicts, []string{"README.md"}) {
		t.Fatalf("conflicting merge = %+v, %v", bad, err)
	}
	withBase, err := r.MergeTree(ctx, f.c3, mainTip, f.featureTip)
	if err != nil || withBase.Tree != clean.Tree {
		t.Errorf("merge with explicit base = %+v, %v", withBase, err)
	}
	if _, err := r.MergeTree(ctx, "", "nope", mainTip); err == nil {
		t.Error("MergeTree with a bad rev succeeded")
	}

	when := time.Date(2024, 5, 1, 12, 0, 0, 0, time.FixedZone("", 3*3600))
	author := git.Signature{Name: "Bob", Email: "bob@example.com", When: when}
	committer := git.Signature{Name: "Carol", Email: "carol@example.com", When: when.Add(time.Hour)}
	sha, err := r.CommitTree(ctx, clean.Tree, []string{mainTip, f.featureTip}, "Merge feature\n\nbody\n", author, committer)
	if err != nil {
		t.Fatal(err)
	}
	mc, err := r.GetCommit(ctx, sha)
	if err != nil {
		t.Fatal(err)
	}
	if mc.Subject != "Merge feature" || mc.Body != "body" || !slices.Equal(mc.Parents, []string{mainTip, f.featureTip}) ||
		mc.Author.Name != "Bob" || mc.Committer.Email != "carol@example.com" || !mc.Author.When.Equal(when) {
		t.Errorf("merge commit = %+v", mc)
	}
	raw, _ := r.Cmd(ctx, "cat-file", "commit", sha).Output()
	if !strings.Contains(string(raw), "+0300") {
		t.Errorf("author time zone lost:\n%s", raw)
	}

	// UpdateRef compare-and-swap.
	if err := r.UpdateRef(ctx, "refs/heads/main", sha, f.c1); !errors.Is(err, git.ErrRefChanged) {
		t.Errorf("UpdateRef with a stale old value: %v", err)
	}
	if err := r.UpdateRef(ctx, "refs/heads/main", sha, mainTip); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.ResolveCommit(ctx, "main"); got != sha {
		t.Errorf("main = %s after UpdateRef", got)
	}
	if err := r.UpdateRef(ctx, "refs/heads/new", sha, git.ZeroSHA); err != nil {
		t.Errorf("create ref: %v", err)
	}
	if err := r.UpdateRef(ctx, "refs/heads/new", sha, git.ZeroSHA); !errors.Is(err, git.ErrRefChanged) {
		t.Errorf("create an existing ref: %v", err)
	}
	if err := r.UpdateRef(ctx, "refs/heads/new", git.ZeroSHA, sha); err != nil {
		t.Errorf("delete ref: %v", err)
	}
	if _, err := r.ResolveCommit(ctx, "refs/heads/new"); err == nil {
		t.Error("deleted ref still resolves")
	}
	if err := r.UpdateRef(ctx, "refs/heads/x", "not-a-sha", ""); err == nil || errors.Is(err, git.ErrRefChanged) {
		t.Errorf("UpdateRef with garbage: %v", err)
	}
}

func TestCommitsToReplay(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.work
	w.Git("checkout", "-q", "feature/x/y")
	w.Env = []string{"GIT_AUTHOR_NAME=Dave", "GIT_AUTHOR_EMAIL=dave@example.com"}
	w.Git("-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "second", "-m", "details",
		"--date", "2024-01-02T03:04:05+05:30")
	w.Env = nil
	w.Push(f.repo, "feature/x/y")
	cs, err := f.repo.CommitsToReplay(ctx, "main", "feature/x/y")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].SHA != f.featureTip || cs[0].Parent != f.c3 {
		t.Fatalf("replay list = %+v", cs)
	}
	c := cs[1]
	if c.Author.Name != "Dave" || c.Message != "second\n\ndetails\n" {
		t.Errorf("replay commit = %+v", c)
	}
	if _, off := c.Author.When.Zone(); off != 5*3600+1800 {
		t.Errorf("author offset = %d, want +05:30", off)
	}
}

func TestWithEnvDoesNotLeak(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	withEnv := f.repo.WithEnv([]string{"GIT_DIR=/nonexistent"})
	if withEnv.Path != f.repo.Path {
		t.Error("WithEnv changed the path")
	}
	// The original handle is unaffected.
	if _, err := f.repo.ResolveCommit(ctx, "main"); err != nil {
		t.Errorf("original repo broken: %v", err)
	}
	cmd := withEnv.Cmd(ctx, "rev-parse", "HEAD")
	if !slices.Contains(cmd.Env, "GIT_DIR=/nonexistent") {
		t.Error("extra env not passed to git")
	}
}

func TestRunErrorMessage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := f.repo.MergeBase(ctx, "main", "does-not-exist")
	var re *git.RunError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "merge-base") || re.Stderr == "" {
		t.Errorf("error = %v", err)
	}
}

func TestMaintain(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for range 2 { // the second run skips the weekly gc and must still work
		if err := f.repo.Maintain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"objects/info/commit-graphs/commit-graph-chain", "packed-refs", "onegit-last-gc"} {
		if _, err := os.Stat(filepath.Join(f.repo.Path, p)); err != nil {
			t.Errorf("after Maintain: %v", err)
		}
	}
	if c, err := f.repo.LastCommitFor(ctx, "main", "src/util.go"); err != nil || c.SHA != f.c2 {
		t.Errorf("LastCommitFor after Maintain = %v, %v", c, err)
	}
}

func TestMatchDirs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	sha := f.work.Commit("projects", map[string]string{
		"services/api/project.yaml": "x", "services/web/main.go": "x", "services/db/project.yaml/x": "dir, not file",
		"services/file.txt": "not a dir",
	})
	f.work.Push(f.repo, "main")
	if got := f.repo.MatchDirs(ctx, sha, "services/*"); !slices.Equal(got, []string{"api", "db", "web"}) {
		t.Errorf("services/* = %v", got)
	}
	if got := f.repo.MatchDirs(ctx, sha, "services/*/project.yaml"); !slices.Equal(got, []string{"api", "db"}) {
		t.Errorf("services/*/project.yaml = %v", got)
	}
	if got := f.repo.MatchDirs(ctx, sha, "nope/*"); got != nil {
		t.Errorf("nope/* = %v", got)
	}
}
