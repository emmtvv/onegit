package web

import (
	"regexp"
	"slices"
	"testing"
)

func TestParseSearchQuery(t *testing.T) {
	sq := parseSearchQuery("  NewServer  ctx path:services/api -path:**/*_test.go path:/docs/ path: ")
	if sq.Pattern != "NewServer ctx path:" {
		t.Errorf("pattern = %q", sq.Pattern)
	}
	want := []string{":(literal)services/api", ":(exclude,glob)**/*_test.go", ":(literal)docs/"}
	if !slices.Equal(sq.Paths, want) {
		t.Errorf("paths = %q, want %q", sq.Paths, want)
	}
}

func TestSearchHighlighter(t *testing.T) {
	for _, c := range []struct {
		pattern      string
		regex, icase bool
		line, want   string
	}{
		{"a<b", false, false, "x a<b y a<b", "x <mark>a&lt;b</mark> y <mark>a&lt;b</mark>"},
		{"hello", false, true, "Hello hello", "<mark>Hello</mark> <mark>hello</mark>"},
		{"hel+o", true, false, "helllo", "<mark>helllo</mark>"},
		{"[[:alpha:]", true, false, "a<b", "a&lt;b"}, // Go cannot compile it: no marks
		{"x*", true, false, "abc", "abc"},            // empty matches are not marked
	} {
		if got := string(searchHighlighter(c.pattern, c.regex, c.icase)(c.line)); got != c.want {
			t.Errorf("%q in %q = %q, want %q", c.pattern, c.line, got, c.want)
		}
	}
}

func TestGlobToRegexp(t *testing.T) {
	for _, c := range []struct {
		glob, path string
		match      bool
	}{
		{"**/*.go", "a/b/c.go", true},
		{"**/*.go", "c.go", true},
		{"*.go", "a/c.go", false},
		{"services/*/main.go", "services/api/main.go", true},
		{"a.b", "axb", false},
	} {
		re := regexpMust(globToRegexp(c.glob))
		if re.MatchString(c.path) != c.match {
			t.Errorf("%s ~ %s: want %v", c.glob, c.path, c.match)
		}
	}
}

func regexpMust(s string) interface{ MatchString(string) bool } { return regexp.MustCompile(s) }
