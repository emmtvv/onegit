package web

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"onegit/internal/git"
	"onegit/internal/store"
)

type templateSet struct{ t *template.Template }

func (ts *templateSet) Execute(w io.Writer, data any) error {
	return ts.t.ExecuteTemplate(w, "layout", data)
}

// loadTemplates builds one template set per page: layout + partials + page.
func loadTemplates() (map[string]*templateSet, error) {
	base, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials/*.html")
	if err != nil {
		return nil, err
	}
	pages, err := fs.Glob(assets, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}
	out := make(map[string]*templateSet, len(pages))
	for _, p := range pages {
		t, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if _, err := t.ParseFS(assets, p); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out[strings.TrimSuffix(path.Base(p), ".html")] = &templateSet{t: t}
	}
	return out, nil
}

var funcs = template.FuncMap{
	"ago":        timeAgo,
	"date":       func(t time.Time) string { return t.Format("Jan 2, 2006") },
	"datetime":   func(t time.Time) string { return t.Format("2006-01-02 15:04:05 -0700") },
	"shortsha":   func(s string) string { return s[:min(len(s), 10)] },
	"bytes":      humanBytes,
	"pathesc":    pathEscape,
	"queryesc":   url.QueryEscape,
	"crumbs":     crumbs,
	"avatar":     avatar,
	"fileicon":   fileIcon,
	"diricon":    dirIcon,
	"add":        func(a, b int) int { return a + b },
	"sub":        func(a, b int) int { return a - b },
	"linechar":   func(k git.LineKind) string { return string(rune(k)) },
	"lineclass":  lineClass,
	"hasprefix":  strings.HasPrefix,
	"markdown":   func(s string) template.HTML { return renderMarkdown([]byte(s)) },
	"threadsat":  threadsAt,
	"hunklines":  hunkLines,
	"deref":      func(p *string) string { return *p },
	"pkgurl":     packageURL,
	"deref_time": func(t *time.Time) time.Time { return *t },
	"inlist":     slices.Contains[[]string],
	"hascheck": func(list []*store.CommitStatus, ctx string) bool {
		for _, c := range list {
			if c.Context == ctx {
				return true
			}
		}
		return false
	},
	"dur": func(d time.Duration) string {
		if d <= 0 {
			return ""
		}
		return d.String()
	},
	"short":        func(s string) string { return s[:min(len(s), 7)] },
	"join":         strings.Join,
	"deint64":      func(p *int64) int64 { return *p },
	"upper":        strings.ToUpper,
	"selectortext": func(sel map[string][]string) string { return formatSelector(sel) },
	"jsontext": func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	},
	// inflight: a job, run, check or deployment state that will still change.
	"inflight": func(s string) bool {
		switch s {
		case "pending", "waiting", "queued", "running":
			return true
		}
		return false
	},
	// deploystatus maps deployment/review states onto status-icon states.
	"deploystatus": func(s string) string {
		switch s {
		case "pending":
			return "pending"
		case "approve":
			return "success"
		case "reject", "rejected":
			return "failure"
		}
		return s
	},
	"refname": func(ref string) string {
		if n, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			return n
		}
		if n, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
			return n
		}
		if n, ok := strings.CutPrefix(ref, "refs/pull/"); ok {
			return "PR #" + strings.TrimSuffix(n, "/head")
		}
		return ref
	},
	"deint": func(p *int) int { return *p },
	"parentdir": func(p string) string {
		if d := path.Dir(p); d != "." {
			return d
		}
		return ""
	},
	"dict": func(kv ...any) map[string]any {
		m := make(map[string]any, len(kv)/2)
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	case d < 365*24*time.Hour:
		return plural(int(d.Hours()/24/30), "month") + " ago"
	}
	return plural(int(d.Hours()/24/365), "year") + " ago"
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// pathEscape escapes each segment of a slash-separated path.
func pathEscape(parts ...string) string {
	var segs []string
	for _, p := range parts {
		for _, s := range strings.Split(p, "/") {
			if s != "" {
				segs = append(segs, url.PathEscape(s))
			}
		}
	}
	return strings.Join(segs, "/")
}

type crumb struct {
	Name string
	Path string
	Last bool
}

func crumbs(p string) []crumb {
	if p == "" {
		return nil
	}
	parts := strings.Split(p, "/")
	out := make([]crumb, len(parts))
	for i, part := range parts {
		out[i] = crumb{Name: part, Path: strings.Join(parts[:i+1], "/"), Last: i == len(parts)-1}
	}
	return out
}

// avatar renders a local initials badge; no third-party avatar services.
func avatar(name, email string, size ...string) template.HTML {
	key := strings.ToLower(strings.TrimSpace(email))
	if key == "" {
		key = name
	}
	h := md5.Sum([]byte(key))
	hue := int(h[0])<<8 | int(h[1])
	initial := "?"
	if r := []rune(strings.TrimSpace(name)); len(r) > 0 {
		initial = strings.ToUpper(string(r[0]))
	}
	cls := "avatar"
	if len(size) > 0 {
		cls += " avatar-" + size[0]
	}
	return template.HTML(fmt.Sprintf(`<span class="%s" style="--hue:%d" title="%s">%s</span>`,
		cls, hue%360, template.HTMLEscapeString(name), template.HTMLEscapeString(initial)))
}

func lineClass(k git.LineKind) string {
	switch k {
	case git.LineAdd:
		return "add"
	case git.LineDel:
		return "del"
	}
	return "ctx"
}

// threadsAt returns comment threads anchored at a diff line. Deleted lines
// are addressed on the old side, everything else on the new side.
func threadsAt(threads map[string][]*thread, path string, l git.DiffLine) []*thread {
	side := "new"
	if l.Kind == git.LineDel {
		side = "old"
	}
	return threads[lineKey(path, side, lineNo(l))]
}

type hunkLine struct {
	Class, Marker, Text string
}

// hunkLines renders a comment's stored diff context.
func hunkLines(s string) []hunkLine {
	var out []hunkLine
	for _, l := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		if l == "" {
			continue
		}
		out = append(out, hunkLine{Class: lineClass(git.LineKind(l[0])), Marker: l[:1], Text: l[1:]})
	}
	return out
}
