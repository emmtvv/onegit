package pulls

import (
	"strings"
	"testing"

	"onegit/internal/store"
)

func TestCodeOwners(t *testing.T) {
	co := ParseCodeOwners(`
# comment
*                 @lead
*.js              @js
/docs/            @docs
apps/             @apps
/build/logs/      @logs
src/*             @src-direct
**/migrations     @db
/scripts/**/run   @ops
vendor            # explicitly unowned
/README.md        @docs @lead  # inline comment
`)
	cases := map[string]string{
		"main.go":                           "@lead",
		"web/app.js":                        "@js",
		"docs/index.md":                     "@docs",
		"sub/docs/index.md":                 "@lead", // /docs/ is anchored
		"apps/web/main.go":                  "@apps",
		"services/apps/x.go":                "@apps", // apps/ is unanchored
		"build/logs/today.log":              "@logs",
		"src/main.go":                       "@src-direct",
		"src/pkg/util.go":                   "@lead", // src/* matches only direct children
		"internal/store/migrations/001.sql": "@db",
		"migrations/001.sql":                "@db",
		"scripts/run":                       "@ops",
		"scripts/a/b/run":                   "@ops",
		"vendor/lib/x.go":                   "",
		"README.md":                         "@docs @lead",
	}
	for p, want := range cases {
		owners, _ := co.Owners(p)
		if got := strings.Join(owners, " "); got != want {
			t.Errorf("%s: owners %q, want %q", p, got, want)
		}
	}
}

func TestOwnerListIncludes(t *testing.T) {
	u := &store.User{Username: "Ann", Email: "Ann@Example.com"}
	for _, c := range []struct {
		owners []string
		teams  []string
		want   bool
	}{
		{[]string{"@ann"}, nil, true},
		{[]string{"ann@example.com"}, nil, true},
		{[]string{"@bob", "@org/SRE"}, []string{"sre"}, true},
		{[]string{"@sre"}, []string{"sre"}, true},
		{[]string{"@bob"}, []string{"sre"}, false},
		{nil, []string{"sre"}, false},
	} {
		if got := OwnerListIncludes(c.owners, u, c.teams); got != c.want {
			t.Errorf("OwnerListIncludes(%v, teams %v) = %v", c.owners, c.teams, got)
		}
	}
	if ownerMatches("x@y", &store.User{}) {
		t.Error("an email owner matched a user without email")
	}
	if teamName("org/Team") != "team" || teamName("solo") != "solo" {
		t.Error("teamName")
	}
}

func TestCodeOwnersEdgeCases(t *testing.T) {
	co := ParseCodeOwners("[bad\n*.go @go\n\n   \n# only comment\n")
	if o, p := co.Owners("x.go"); len(o) != 1 || p != "*.go" {
		t.Errorf("owners = %v, %q", o, p)
	}
	if o, p := co.Owners("x.py"); o != nil || p != "" {
		t.Errorf("unowned = %v, %q", o, p)
	}
	for _, pat := range []string{"*", "**", "/**"} {
		if o, _ := ParseCodeOwners(pat + " @all").Owners("any/deep/path"); len(o) != 1 {
			t.Errorf("%q does not match everything", pat)
		}
	}
	if o, _ := ParseCodeOwners("docs/**/*.md @d").Owners("docs/a/b/c.md"); len(o) != 1 {
		t.Error("docs/**/*.md")
	}
	if o, _ := ParseCodeOwners("file?.txt @q").Owners("dir/file1.txt"); len(o) != 1 {
		t.Error("? wildcard")
	}
}
