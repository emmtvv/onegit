package web

import (
	"bytes"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"onegit/internal/git"
	"onegit/internal/store"
)

func TestTemplatesParse(t *testing.T) {
	sets, err := loadTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []string{"tree", "blob", "pull", "deploy_view", "admin_deploy", "package_version", "error", "login"} {
		if sets[page] == nil {
			t.Errorf("template %s missing", page)
		}
	}
}

func TestMarkdownIsSafe(t *testing.T) {
	out := string(renderMarkdown([]byte("# Title\n\n<script>alert(1)</script>\n\n[x](javascript:alert(1))\n\n| a |\n|---|\n| b |\n\n- [x] done\n")))
	if strings.Contains(out, "<script>") || strings.Contains(out, `href="javascript:`) {
		t.Errorf("unsafe HTML rendered:\n%s", out)
	}
	for _, want := range []string{`<h1 id="title">Title</h1>`, "<table>", `type="checkbox"`} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown output lacks %q:\n%s", want, out)
		}
	}
}

func TestHighlight(t *testing.T) {
	out := string(highlight("main.go", []byte("package main\n\nfunc main() {}\n")))
	if !strings.Contains(out, `id="L1"`) || !strings.Contains(out, "chroma") {
		t.Errorf("highlight output:\n%s", out)
	}
	if out := string(highlight("x.unknownext", []byte("<b>plain</b>"))); strings.Contains(out, "<b>") {
		t.Errorf("unescaped fallback: %s", out)
	}
	for _, theme := range []string{"light", "dark"} {
		css := string(chromaCSS()[theme])
		if !strings.Contains(css, ".chroma") || strings.Contains(css, "background-color: #") {
			t.Errorf("chroma CSS %s not themed:\n%.300s", theme, css)
		}
		rec := httptest.NewRecorder()
		serveChromaCSS(theme)(rec, httptest.NewRequest("GET", "/static/chroma-"+theme+".css", nil))
		if rec.Header().Get("Content-Type") != "text/css; charset=utf-8" {
			t.Errorf("chroma-%s.css content type %q", theme, rec.Header().Get("Content-Type"))
		}
	}
	if bytes.Equal(chromaCSS()["light"], chromaCSS()["dark"]) {
		t.Error("light and dark chroma CSS are the same")
	}
}

func TestFileKinds(t *testing.T) {
	for name, want := range map[string][3]bool{ // markdown, image, readme
		"README.md": {true, false, true}, "readme": {false, false, true}, "doc.MARKDOWN": {true, false, false},
		"logo.PNG": {false, true, false}, "main.go": {false, false, false}, "Readme.rst": {false, false, true},
	} {
		if got := [3]bool{isMarkdown(name), isImage(name), isReadme(name)}; got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"": "/", "/pulls/1?x=y": "/pulls/1?x=y", "https://evil.example": "/", "//evil.example": "/", `/\evil.example`: "/",
		"relative": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatHelpers(t *testing.T) {
	now := time.Now()
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now", 90 * time.Second: "1 minute ago", 3 * time.Hour: "3 hours ago",
		50 * time.Hour: "2 days ago", 70 * 24 * time.Hour: "2 months ago", 800 * 24 * time.Hour: "2 years ago",
	} {
		if got := timeAgo(now.Add(-d)); got != want {
			t.Errorf("timeAgo(-%s) = %q, want %q", d, got, want)
		}
	}
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 5 << 20: "5.0 MiB", 3 << 30: "3.0 GiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q", n, got)
		}
	}
	if got := pathEscape("dir with space/a#b", "/c?d/"); got != "dir%20with%20space/a%23b/c%3Fd" {
		t.Errorf("pathEscape = %q", got)
	}
	cs := crumbs("a/b/c")
	if len(cs) != 3 || cs[1].Path != "a/b" || !cs[2].Last || cs[0].Last || crumbs("") != nil {
		t.Errorf("crumbs = %+v", cs)
	}
	av := string(avatar("<script>", "X@Example.com", "lg"))
	if !strings.Contains(av, "avatar avatar-lg") || strings.Contains(av, "<script>") || !strings.Contains(av, "&lt;") {
		t.Errorf("avatar = %s", av)
	}
	hue := func(h string) string { return h[strings.Index(h, "--hue:"):strings.Index(h, `" title`)] }
	if hue(string(avatar("Ann", "ann@x.com"))) != hue(string(avatar("Other name", "ANN@x.com "))) {
		t.Error("avatar colour must depend on the email only")
	}
	if !strings.Contains(string(avatar("", "")), ">?<") {
		t.Error("avatar without a name")
	}
	if lineClass(git.LineAdd) != "add" || lineClass(git.LineDel) != "del" || lineClass(git.LineContext) != "ctx" {
		t.Error("lineClass")
	}
	hl := hunkLines("+added\n-removed\n context\n\n")
	if len(hl) != 3 || hl[0].Class != "add" || hl[1].Marker != "-" || hl[2].Text != "context" {
		t.Errorf("hunkLines = %+v", hl)
	}
	if titleFor("repo", "") != "repo" || titleFor("repo", "a/b") != "a/b · repo" {
		t.Error("titleFor")
	}
	if humanizeBranch("feature/add-login_page") != "Add login page" || humanizeBranch("") != "" {
		t.Errorf("humanizeBranch = %q", humanizeBranch("feature/add-login_page"))
	}
	if packageURL("team/my app") != "/packages/team/my%20app" {
		t.Errorf("packageURL = %q", packageURL("team/my app"))
	}
	if passwordError(errors.New("password too short")) != "Password too short." {
		t.Error("passwordError")
	}
	for fn, cases := range map[string]map[string]string{
		"refname": {"refs/heads/main": "main", "refs/tags/v1": "v1", "refs/pull/7/head": "PR #7", "abc": "abc"},
		"deploystatus": {"approve": "success", "reject": "failure", "rejected": "failure", "pending": "pending",
			"running": "running"},
	} {
		f := funcs[fn].(func(string) string)
		for in, want := range cases {
			if got := f(in); got != want {
				t.Errorf("%s(%q) = %q, want %q", fn, in, got, want)
			}
		}
	}
	if funcs["parentdir"].(func(string) string)("a/b/c") != "a/b" || funcs["parentdir"].(func(string) string)("c") != "" {
		t.Error("parentdir")
	}
	if funcs["dur"].(func(time.Duration) string)(0) != "" {
		t.Error("dur(0)")
	}
	if m := funcs["dict"].(func(...any) map[string]any)("a", 1, "b"); m["a"] != 1 || len(m) != 1 {
		t.Errorf("dict = %v", m)
	}
}

func TestSelectorsAndPrincipals(t *testing.T) {
	dims := []*store.DeployDimension{{Name: "project"}, {Name: "environment"}}
	sel, err := parseSelector("project = api, web ;\nenvironment=prod*,", dims)
	if err != nil || !slices.Equal(sel["project"], []string{"api", "web"}) || !slices.Equal(sel["environment"], []string{"prod*"}) {
		t.Fatalf("parseSelector = %v, %v", sel, err)
	}
	if got := formatSelector(sel); got != "environment=prod*; project=api,web" {
		t.Errorf("formatSelector = %q", got)
	}
	if back, _ := parseSelector(formatSelector(sel), dims); formatSelector(back) != formatSelector(sel) {
		t.Error("selector does not round-trip")
	}
	for _, bad := range []string{"region=eu", "project", "=x"} {
		if _, err := parseSelector(bad, dims); err == nil {
			t.Errorf("parseSelector(%q) accepted", bad)
		}
	}
	if sel, err := parseSelector("  ", dims); err != nil || len(sel) != 0 {
		t.Errorf("empty selector = %v, %v", sel, err)
	}

	ps, err := parsePrincipals("team:sre, user:alice\nrole:admin codeowners")
	if err != nil || len(ps) != 4 || ps[3] != (store.Principal{Type: "codeowners"}) {
		t.Fatalf("parsePrincipals = %+v, %v", ps, err)
	}
	if got := formatPrincipals(ps); got != "team:sre, user:alice, role:admin, codeowners" {
		t.Errorf("formatPrincipals = %q", got)
	}
	for _, bad := range []string{"team:", "role:root", "group:x", "alice"} {
		if _, err := parsePrincipals(bad); err == nil {
			t.Errorf("parsePrincipals(%q) accepted", bad)
		}
	}
	if got := splitList(" main, release/* \n hotfix "); !slices.Equal(got, []string{"main", "release/*", "hotfix"}) {
		t.Errorf("splitList = %q", got)
	}
}

func TestTargetsFromForm(t *testing.T) {
	dims := []*store.DeployDimension{{Name: "project"}, {Name: "environment"}}
	targets, err := targetsFromForm(dims, url.Values{"project": {"api", "web"}, "environment": {"dev", "prod"}})
	if err != nil || len(targets) != 4 || targets[3]["project"] != "web" || targets[3]["environment"] != "prod" {
		t.Errorf("targets = %v, %v", targets, err)
	}
	if _, err := targetsFromForm(dims, url.Values{"project": {"api"}}); err == nil {
		t.Error("missing dimension accepted")
	}
	many := make([]string, 15)
	for i := range many {
		many[i] = string(rune('a' + i))
	}
	if _, err := targetsFromForm(dims, url.Values{"project": many, "environment": many}); err == nil {
		t.Error("more than 200 targets accepted")
	}
	d := &store.Deployment{SHA: "abc", Targets: []map[string]string{{"project": "api", "environment": "dev"}, {"project": "web", "environment": "dev"}}}
	u, _ := url.Parse(redeployURL(d))
	q := u.Query()
	if u.Path != "/deploy/new" || q.Get("ref") != "abc" || !slices.Equal(q["project"], []string{"api", "web"}) || len(q["environment"]) != 1 {
		t.Errorf("redeployURL = %s", u)
	}
}

func TestDescribeVersion(t *testing.T) {
	idx := store.RegistryVersion{Name: "1.0", Manifest: store.RegistryManifest{Repo: "app", MediaType: "application/vnd.oci.image.index.v1+json",
		Content: []byte(`{"manifests":[{"platform":{"os":"linux","architecture":"amd64"}},{"platform":{"os":"unknown","architecture":"unknown"}}]}`)}}
	pv := describeVersion(idx)
	if pv.Kind != "image" || !slices.Equal(pv.Platforms, []string{"linux/amd64"}) || pv.URL != "/packages/app/-/1.0" {
		t.Errorf("single-platform index = %+v", pv)
	}
	img := store.RegistryVersion{Name: "x", Manifest: store.RegistryManifest{Repo: "app", MediaType: "application/vnd.oci.image.manifest.v1+json",
		Platform: "linux/arm64"}}
	if pv := describeVersion(img); !slices.Equal(pv.Platforms, []string{"linux/arm64"}) {
		t.Errorf("image = %+v", pv)
	}
}

func TestSecurityHeadersAndFlash(t *testing.T) {
	rec := httptest.NewRecorder()
	securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "same-origin"} {
		if rec.Header().Get(k) != v {
			t.Errorf("%s = %q", k, rec.Header().Get(k))
		}
	}
	rec = httptest.NewRecorder()
	setFlash(rec, "Saved; all good")
	c := rec.Result().Cookies()[0]
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	if got := popFlash(rec, req); got != "Saved; all good" {
		t.Errorf("popFlash = %q", got)
	}
	if rec.Result().Cookies()[0].MaxAge >= 0 {
		t.Error("flash cookie not cleared")
	}
	if popFlash(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil)) != "" {
		t.Error("flash without a cookie")
	}
}

func TestStaticAssets(t *testing.T) {
	static, _ := fs.Sub(assets, "static")
	h := http.StripPrefix("/static/", staticHandler(static))
	for path, ctype := range map[string]string{
		"/static/style.css":               "text/css; charset=utf-8",
		"/static/fonts/onest-latin.woff2": "font/woff2",
		"/static/favicon.svg":             "image/svg+xml",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != ctype || rec.Header().Get("Cache-Control") == "" {
			t.Errorf("%s: %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}
