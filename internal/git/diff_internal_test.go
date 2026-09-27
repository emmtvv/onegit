package git

import (
	"bufio"
	"strings"
	"testing"
)

func parse(t *testing.T, src string, o DiffOptions) *Diff {
	t.Helper()
	if o.MaxFiles == 0 {
		o.MaxFiles = 500
	}
	if o.MaxLines == 0 {
		o.MaxLines = 20000
	}
	if o.MaxFileLines == 0 {
		o.MaxFileLines = 3000
	}
	return parseDiff(bufio.NewReader(strings.NewReader(src)), o)
}

func TestParseDiffHeaders(t *testing.T) {
	t.Parallel()
	d := parse(t, `diff --git "a/sp ace" "b/sp ace"
index 1111111..2222222 100644
--- "a/sp ace"
+++ "b/sp ace"
@@ -1 +1 @@
-old
+new
diff --git a/mode.sh b/mode.sh
old mode 100644
new mode 100755
diff --git a/x.txt b/x.txt
index 3333333..4444444 100644
--- a/x.txt
+++ b/x.txt
@@ -1,2 +1,2 @@ func header()
 same
--- this line starts with dashes
+++ and this with pluses
\ No newline at end of file
diff --git a/old.go b/new.go
similarity index 90%
copy from old.go
copy to new.go
`, DiffOptions{})
	if len(d.Files) != 4 {
		t.Fatalf("%d files", len(d.Files))
	}
	if f := d.Files[0]; f.OldPath != "sp ace" || f.NewPath != "sp ace" || f.Additions != 1 || f.Deletions != 1 {
		t.Errorf("quoted paths: %+v", f)
	}
	if f := d.Files[1]; f.OldMode != "100644" || f.NewMode != "100755" || len(f.Hunks) != 0 {
		t.Errorf("mode change: %+v", f)
	}
	f := d.Files[2]
	if f.Deletions != 1 || f.Additions != 1 || f.Hunks[0].Header != "func header()" {
		t.Errorf("--- and +++ inside a hunk must be content: %+v", f)
	}
	lines := f.Hunks[0].Lines
	if lines[1].Content != "-- this line starts with dashes" || lines[2].Content != "++ and this with pluses" || !lines[2].NoEOL {
		t.Errorf("lines = %+v", lines)
	}
	if f := d.Files[3]; f.Status != StatusCopied || f.OldPath != "old.go" || f.NewPath != "new.go" {
		t.Errorf("copy: %+v", f)
	}
}

func TestParseDiffLimits(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for i := 0; i < 5; i++ {
		b.WriteString("diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1,0 +1,3 @@\n+1\n+2\n+3\n")
	}
	if d := parse(t, b.String(), DiffOptions{MaxLines: 6}); len(d.Files) != 2 || !d.Truncated {
		t.Errorf("MaxLines: %d files, truncated=%v", len(d.Files), d.Truncated)
	}
	d := parse(t, b.String(), DiffOptions{MaxFileLines: 2})
	if !d.Truncated || !d.Files[0].Truncated || d.Files[0].Hunks != nil || d.Files[0].Additions != 3 {
		t.Errorf("MaxFileLines: %+v", d.Files[0])
	}
}

func TestParseHunkHeader(t *testing.T) {
	t.Parallel()
	for in, want := range map[string][4]int{
		"@@ -1 +1 @@":            {1, 1, 1, 1},
		"@@ -0,0 +1,5 @@":        {0, 0, 1, 5},
		"@@ -10,3 +12,7 @@ fn x": {10, 3, 12, 7},
	} {
		h := parseHunkHeader(in)
		if got := [4]int{h.OldStart, h.OldLines, h.NewStart, h.NewLines}; got != want {
			t.Errorf("%q = %v, want %v", in, got, want)
		}
	}
}

func TestSplitGitHeader(t *testing.T) {
	t.Parallel()
	for in, want := range map[string][2]string{
		"a/x b/x":         {"x", "x"},
		"a/dir/x b/dir/y": {"dir/x", "dir/y"},
		`"a/q" "b/q"`:     {"", ""},
		"weird":           {"weird", "weird"},
	} {
		a, b := splitGitHeader(in)
		if [2]string{a, b} != want {
			t.Errorf("splitGitHeader(%q) = %q, %q", in, a, b)
		}
	}
	if got := unquote(`"\320\264.md"`); got != "д.md" {
		t.Errorf("unquote octal = %q", got)
	}
	if got := unquote("plain\t"); got != "plain" {
		t.Errorf("unquote trailing tab = %q", got)
	}
}

func TestParseCommits(t *testing.T) {
	t.Parallel()
	out := "sha1\x00p1 p2\x00Ann\x00ann@x\x00100\x00Bob\x00bob@x\x00200\x00subj\x00body\n\x1e\n" +
		"broken record\x1e"
	cs := parseCommits(out)
	if len(cs) != 1 {
		t.Fatalf("%d commits", len(cs))
	}
	c := cs[0]
	if c.SHA != "sha1" || len(c.Parents) != 2 || c.Author.When.Unix() != 100 || c.Committer.Name != "Bob" || c.Body != "body" {
		t.Errorf("commit = %+v", c)
	}
}

func TestIsHex(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{"abc123": true, "": false, "ABC": false, "xyz": false} {
		if isHex(s) != want {
			t.Errorf("isHex(%q) != %v", s, want)
		}
	}
}
