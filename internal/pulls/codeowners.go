package pulls

import (
	"bufio"
	"regexp"
	"strings"
)

// CodeOwnersPaths are searched in order on the base branch.
var CodeOwnersPaths = []string{".github/CODEOWNERS", ".gitea/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}

type ownerRule struct {
	Pattern string
	re      *regexp.Regexp
	Owners  []string // "@user" or "email@host" as written
}

// CodeOwners is a parsed CODEOWNERS file (GitHub syntax).
type CodeOwners struct {
	rules []ownerRule
}

// ParseCodeOwners parses the file; invalid lines are skipped.
func ParseCodeOwners(src string) *CodeOwners {
	co := &CodeOwners{}
	sc := bufio.NewScanner(strings.NewReader(src))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, " #"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		re, err := compileOwnerPattern(f[0])
		if err != nil {
			continue
		}
		var owners []string
		for _, o := range f[1:] {
			if strings.HasPrefix(o, "#") {
				break
			}
			owners = append(owners, o)
		}
		co.rules = append(co.rules, ownerRule{Pattern: f[0], re: re, Owners: owners})
	}
	return co
}

// Owners returns the owners of a path and the pattern that matched; the
// last matching rule wins. A matching rule without owners means "no owner".
func (co *CodeOwners) Owners(p string) (owners []string, pattern string) {
	for i := len(co.rules) - 1; i >= 0; i-- {
		if co.rules[i].re.MatchString(p) {
			return co.rules[i].Owners, co.rules[i].Pattern
		}
	}
	return nil, ""
}

// compileOwnerPattern translates a gitignore-style CODEOWNERS pattern:
//   - a pattern with a leading or inner "/" is anchored at the repo root,
//     otherwise it matches at any depth;
//   - a trailing "/" matches everything inside that directory;
//   - "*" and "?" do not cross "/", "**" does;
//   - a plain name ("docs") matches a file or a whole directory, but a last
//     segment with a wildcard ("docs/*") matches only direct children.
func compileOwnerPattern(p string) (*regexp.Regexp, error) {
	if p == "*" || p == "**" || p == "/**" {
		return regexp.Compile(`^.*$`)
	}
	dirOnly := strings.HasSuffix(p, "/")
	p = strings.TrimSuffix(p, "/")
	anchored := strings.Contains(p, "/")
	p = strings.TrimPrefix(p, "/")
	last := p[strings.LastIndex(p, "/")+1:]

	var b strings.Builder
	if anchored {
		b.WriteString("^")
	} else {
		b.WriteString("^(?:.*/)?")
	}
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				if i+2 < len(p) && p[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
				} else {
					b.WriteString(".*")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	switch {
	case dirOnly:
		b.WriteString("/.*$")
	case strings.Contains(last, "*") && !strings.Contains(last, "**"):
		b.WriteString("$")
	default:
		b.WriteString("(?:/.*)?$")
	}
	return regexp.Compile(b.String())
}
