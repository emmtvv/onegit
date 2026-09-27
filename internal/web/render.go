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

var chromaCSS = sync.OnceValue(func() []byte {
	// Each theme lives in its own media block so light colours never leak
	// into dark mode for token classes the dark style leaves undefined.
	var buf bytes.Buffer
	buf.WriteString("@media (prefers-color-scheme: light) {\n")
	_ = formatter.WriteCSS(&buf, styles.Get("github")) // writes to memory
	buf.WriteString("}\n@media (prefers-color-scheme: dark) {\n")
	_ = formatter.WriteCSS(&buf, styles.Get("github-dark"))
	buf.WriteString("}\n")
	// Let our own CSS control backgrounds so the code blends with the page.
	return bytes.ReplaceAll(buf.Bytes(), []byte("background-color: #"), []byte("--chroma-bg: #"))
})

func serveChromaCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(chromaCSS())
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
