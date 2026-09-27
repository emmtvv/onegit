package web

import (
	"strings"
	"testing"
)

func TestKindOf(t *testing.T) {
	cases := map[string]string{
		"main.go":                 "badge",
		"server_test.go":          "flask",
		"test_api.py":             "flask",
		"Button.test.tsx":         "flask",
		"tool.py":                 "python",
		"Main.java":               "java",
		"App.tsx":                 "react",
		"index.js":                "corner",
		"projects/api/Dockerfile": "whale",
		"Dockerfile.prod":         "whale",
		"docker-compose.dev.yml":  "whale",
		"Makefile":                "gear",
		"README.md":               "book",
		"docs/guide.md":           "md",
		"LICENSE":                 "ribbon",
		"config.YAML":             "bars",
		"package.json":            "cube",
		"go.sum":                  "padlock",
		".gitignore":              "git",
		".env.local":              "sliders",
		".github/CODEOWNERS":      "people",
		"logo.svg":                "vector",
		"song.mp3":                "note",
		"clip.mp4":                "film",
		"report.pdf":              "page",
		"archive.tar.gz":          "box",
		"notes.txt":               "lines",
		"no-extension":            "plain",
		"weird.unknownext":        "plain",
	}
	for name, want := range cases {
		if got := kindOf(name).icon; got != want {
			t.Errorf("kindOf(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestAllKindsRender(t *testing.T) {
	check := func(where string, k fileKind) {
		_, sym := symbols[k.icon]
		_, shape := shapes[k.icon]
		switch {
		case !sym && !shape:
			t.Errorf("%s: unknown icon %q", where, k.icon)
		case shape && k.label == "":
			t.Errorf("%s: shape %q needs a label", where, k.icon)
		}
		if out := renderKind(k); strings.Count(out, "<") != strings.Count(out, ">") {
			t.Errorf("%s: unbalanced markup", where)
		}
	}
	for ext, k := range byExt {
		check("."+ext, k)
	}
	for name, k := range byName {
		check(name, k)
	}
}
