package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"onegit/internal/git"
)

const (
	searchTimeout     = 15 * time.Second
	searchMaxMatches  = 1000
	searchMaxFiles    = 200
	searchLinesPerHit = 8 // lines shown per file before "N more"
)

type searchQuery struct {
	Pattern string
	Paths   []string // git pathspecs
}

// parseSearchQuery splits "foo bar path:services/api -path:vendor" into the
// pattern ("foo bar") and pathspecs. Path filters with * or ? are globs
// ("path:**/*.go").
func parseSearchQuery(q string) searchQuery {
	var sq searchQuery
	var words []string
	for _, f := range strings.Fields(q) {
		exclude := false
		v, ok := strings.CutPrefix(f, "path:")
		if !ok {
			if v, ok = strings.CutPrefix(f, "-path:"); ok {
				exclude = true
			}
		}
		if !ok || v == "" {
			words = append(words, f)
			continue
		}
		v = strings.TrimPrefix(v, "/")
		var magic []string
		if exclude {
			magic = append(magic, "exclude")
		}
		if strings.ContainsAny(v, "*?[") {
			magic = append(magic, "glob")
		} else {
			magic = append(magic, "literal")
		}
		sq.Paths = append(sq.Paths, ":("+strings.Join(magic, ",")+")"+v)
	}
	sq.Pattern = strings.Join(words, " ")
	return sq
}

type searchFile struct {
	Path    string
	Lines   []searchLine
	More    int
	MoreURL string // the same search limited to this file
}

type searchLine struct {
	No   int
	HTML template.HTML
}

func (w *Web) search(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	qv := r.URL.Query()
	q := strings.TrimSpace(qv.Get("q"))
	mode := qv.Get("type")
	if mode != "files" {
		mode = "code"
	}
	ref := strings.TrimSpace(qv.Get("ref"))
	if ref == "" {
		ref = w.defaultBranch(ctx)
	}
	data := map[string]any{
		"Q": q, "Type": mode, "Ref": ref,
		"Regex": qv.Get("regex") != "", "Case": qv.Get("case") != "",
	}
	page := &Page{Title: "Search · " + w.Cfg.Repo.Name, Tab: "code", Data: data}
	if q == "" {
		w.render(rw, r, http.StatusOK, "search", page)
		return
	}
	page.Title = q + " · Search"
	sha := w.resolveSearchRef(ctx, ref)
	if sha == "" {
		page.Error = "There is no branch, tag or commit named " + ref + "."
		w.render(rw, r, http.StatusOK, "search", page)
		return
	}
	sctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	sq := parseSearchQuery(q)
	if mode == "files" {
		files, truncated, err := w.findFiles(sctx, sha, sq)
		if err != nil && sctx.Err() == nil {
			w.serverError(rw, r, err)
			return
		}
		data["Files"], data["Truncated"], data["TimedOut"] = files, truncated, sctx.Err() != nil
		w.render(rw, r, http.StatusOK, "search", page)
		return
	}
	if sq.Pattern == "" {
		page.Error = "Enter text to search for; path: filters only narrow a search."
		w.render(rw, r, http.StatusOK, "search", page)
		return
	}
	regex, ignoreCase := data["Regex"].(bool), !data["Case"].(bool)
	matches, truncated, err := w.Repo.Grep(sctx, sha, git.GrepOptions{
		Pattern: sq.Pattern, Regex: regex, IgnoreCase: ignoreCase, Paths: sq.Paths, MaxMatches: searchMaxMatches,
	})
	switch {
	case errors.Is(err, git.ErrBadPattern):
		page.Error = "That is not a valid regular expression."
	case err != nil && sctx.Err() != nil:
		data["TimedOut"] = true
	case err != nil:
		w.serverError(rw, r, err)
		return
	}
	hl := searchHighlighter(sq.Pattern, regex, ignoreCase)
	var files []*searchFile
	for _, m := range matches {
		if len(files) == 0 || files[len(files)-1].Path != m.Path {
			if len(files) == searchMaxFiles {
				truncated = true
				break
			}
			files = append(files, &searchFile{Path: m.Path})
		}
		f := files[len(files)-1]
		if len(f.Lines) >= searchLinesPerHit {
			f.More++
			continue
		}
		f.Lines = append(f.Lines, searchLine{No: m.Line, HTML: hl(m.Text)})
	}
	for _, f := range files {
		if f.More > 0 {
			mq := url.Values{"q": {sq.Pattern + " path:" + f.Path}, "ref": {ref}}
			if regex {
				mq.Set("regex", "1")
			}
			if !ignoreCase {
				mq.Set("case", "1")
			}
			f.MoreURL = "/search?" + mq.Encode()
		}
	}
	data["Results"], data["Truncated"], data["Count"] = files, truncated, len(matches)
	w.render(rw, r, http.StatusOK, "search", page)
}

func (w *Web) resolveSearchRef(ctx context.Context, ref string) string {
	for _, cand := range []string{"refs/heads/" + ref, "refs/tags/" + ref, ref} {
		if sha, err := w.Repo.ResolveCommit(ctx, cand); err == nil {
			return sha
		}
	}
	return ""
}

// findFiles matches paths containing every word of the pattern (case
// insensitive), within the path filters. Matches in the file name rank
// before matches in directories.
func (w *Web) findFiles(ctx context.Context, sha string, sq searchQuery) ([]string, bool, error) {
	words := strings.Fields(strings.ToLower(sq.Pattern))
	type filter struct {
		re     *regexp.Regexp // glob filters
		prefix string         // literal filters: a file or directory
	}
	var include, exclude []filter
	for _, p := range sq.Paths {
		magic, v, _ := strings.Cut(strings.TrimPrefix(p, ":("), ")")
		f := filter{prefix: strings.TrimSuffix(v, "/")}
		if strings.Contains(magic, "glob") {
			f.re = regexp.MustCompile(globToRegexp(v))
		}
		if strings.Contains(magic, "exclude") {
			exclude = append(exclude, f)
		} else {
			include = append(include, f)
		}
	}
	inScope := func(p string, filters []filter) bool {
		for _, f := range filters {
			if f.re != nil && f.re.MatchString(p) || f.re == nil && (p == f.prefix || strings.HasPrefix(p, f.prefix+"/")) {
				return true
			}
		}
		return false
	}
	const collect = 2000
	files, truncated, err := w.Repo.FindFiles(ctx, sha, collect, func(p string) bool {
		if len(include) > 0 && !inScope(p, include) || len(exclude) > 0 && inScope(p, exclude) {
			return false
		}
		lp := strings.ToLower(p)
		for _, wd := range words {
			if !strings.Contains(lp, wd) {
				return false
			}
		}
		return true
	})
	score := func(p string) int {
		base := strings.ToLower(path.Base(p))
		s := 0
		for _, wd := range words {
			if strings.Contains(base, wd) {
				s--
			}
		}
		return s
	}
	sort.SliceStable(files, func(i, j int) bool {
		si, sj := score(files[i]), score(files[j])
		if si != sj {
			return si < sj
		}
		return len(files[i]) < len(files[j])
	})
	if len(files) > searchMaxFiles {
		files, truncated = files[:searchMaxFiles], true
	}
	return files, truncated, err
}

// globToRegexp converts a git glob pathspec ("**/*.go") to an anchored
// regular expression.
func globToRegexp(g string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); i++ {
		switch c := g[i]; c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				if i+2 < len(g) && g[i+2] == '/' {
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
	b.WriteString("$")
	return b.String()
}

// searchHighlighter marks the matches in a result line. Patterns Go cannot
// compile (POSIX-only syntax) are shown without marks.
func searchHighlighter(pattern string, regex, ignoreCase bool) func(string) template.HTML {
	expr := pattern
	if !regex {
		expr = regexp.QuoteMeta(pattern)
	}
	if ignoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	return func(line string) template.HTML {
		if err != nil {
			return template.HTML(template.HTMLEscapeString(line))
		}
		var b strings.Builder
		last := 0
		for _, loc := range re.FindAllStringIndex(line, 50) {
			if loc[0] == loc[1] {
				continue
			}
			b.WriteString(template.HTMLEscapeString(line[last:loc[0]]))
			b.WriteString("<mark>" + template.HTMLEscapeString(line[loc[0]:loc[1]]) + "</mark>")
			last = loc[1]
		}
		b.WriteString(template.HTMLEscapeString(line[last:]))
		return template.HTML(b.String())
	}
}
