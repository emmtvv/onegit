package web

import (
	"bytes"
	"html/template"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
)

var formatter = chromahtml.New(
	chromahtml.WithClasses(true),
	chromahtml.WithLineNumbers(true),
	chromahtml.WithLinkableLineNumbers(true, "L"),
	chromahtml.LineNumbersInTable(true),
	chromahtml.TabWidth(4),
)

// highlight renders source with line numbers; falls back to plain text.
func highlight(filename string, src []byte) template.HTML {
	lexer := lexers.Match(filename)
	if lexer == nil {
		lexer = lexers.Analyse(string(src))
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)
	it, err := lexer.Tokenise(nil, string(src))
	if err != nil {
		return template.HTML("<pre>" + template.HTMLEscapeString(string(src)) + "</pre>")
	}
	var buf bytes.Buffer
	if err := formatter.Format(&buf, styles.Get("github"), it); err != nil {
		return template.HTML("<pre>" + template.HTMLEscapeString(string(src)) + "</pre>")
	}
	return template.HTML(buf.String())
}

// highlightLines highlights source and returns one HTML fragment per line,
// for views that lay lines out themselves (blame). Wrap them in an element
// with class "chroma" to get the token colours.
func highlightLines(filename string, src []byte) []template.HTML {
	text := strings.TrimSuffix(string(src), "\n")
	plain := func() []template.HTML {
		lines := strings.Split(text, "\n")
		out := make([]template.HTML, len(lines))
		for i, l := range lines {
			out[i] = template.HTML(template.HTMLEscapeString(l))
		}
		return out
	}
	lexer := lexers.Match(filename)
	if lexer == nil {
		lexer = lexers.Analyse(text)
	}
	if lexer == nil {
		return plain()
	}
	it, err := chroma.Coalesce(lexer).Tokenise(nil, text)
	if err != nil {
		return plain()
	}
	var out []template.HTML
	for _, line := range chroma.SplitTokensIntoLines(it.Tokens()) {
		var b strings.Builder
		for _, tok := range line {
			v := template.HTMLEscapeString(strings.TrimSuffix(tok.Value, "\n"))
			if v == "" {
				continue
			}
			if cls := tokenClass(tok.Type); cls != "" {
				b.WriteString(`<span class="` + cls + `">` + v + `</span>`)
			} else {
				b.WriteString(v)
			}
		}
		out = append(out, template.HTML(b.String()))
	}
	return out
}

// tokenClass is chroma's short CSS class for a token type, falling back to
// its broader categories.
func tokenClass(t chroma.TokenType) string {
	for _, tt := range []chroma.TokenType{t, t.SubCategory(), t.Category()} {
		if cls, ok := chroma.StandardTypes[tt]; ok {
			return cls
		}
	}
	return ""
}

// chromaCSS is the token colours for each UI theme, served as separate
// files so the layout can apply one of them by media query (see Page.LightMedia).
// Keeping them apart means light colours never leak into dark mode for token
// classes the dark style leaves undefined.
var chromaCSS = sync.OnceValue(func() map[string][]byte {
	out := map[string][]byte{}
	for theme, style := range map[string]string{"light": "github", "dark": "github-dark"} {
		var buf bytes.Buffer
		_ = formatter.WriteCSS(&buf, styles.Get(style)) // writes to memory
		// Let our own CSS control backgrounds so the code blends with the page.
		out[theme] = bytes.ReplaceAll(buf.Bytes(), []byte("background-color: #"), []byte("--chroma-bg: #"))
	}
	return out
})

func serveChromaCSS(theme string) http.HandlerFunc {
	css := chromaCSS()[theme]
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write(css)
	}
}

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	// Raw HTML is not rendered (goldmark's default), which keeps READMEs safe.
)

func renderMarkdown(src []byte) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert(src, &buf); err != nil {
		return template.HTML("<pre>" + template.HTMLEscapeString(string(src)) + "</pre>")
	}
	return template.HTML(buf.String())
}

func isMarkdown(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown", ".mdown":
		return true
	}
	return false
}

func isImage(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".bmp", ".avif":
		return true
	}
	return false
}

func isReadme(name string) bool {
	base := strings.ToLower(strings.TrimSuffix(name, path.Ext(name)))
	return base == "readme"
}
