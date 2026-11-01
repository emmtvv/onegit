package web

import "testing"

func TestFileLanguage(t *testing.T) {
	for p, want := range map[string]string{
		"main.go":                 "Go",
		"services/api/app.py":     "Python",
		"web/index.HTML":          "HTML",
		"Dockerfile":              "Dockerfile",
		"deploy/Dockerfile.prod":  "Dockerfile",
		"db/001.pgsql":            "PLpgSQL",
		"scripts/run.sh":          "Shell",
		"README.md":               "",
		"config.yaml":             "",
		"vendor/lib/x.go":         "",
		"web/node_modules/a/i.js": "",
		"static/app.min.js":       "",
		"api/v1/service.pb.go":    "",
		"LICENSE":                 "",
	} {
		l, ok := fileLanguage(p)
		if got := l.name; got != want || ok != (want != "") {
			t.Errorf("fileLanguage(%q) = %q, %v; want %q", p, got, ok, want)
		}
	}
}
