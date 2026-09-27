package server_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"onegit/internal/config"
	"onegit/internal/hooks"
	"onegit/internal/server"
	"onegit/internal/testutil"
)

// TestMain lets the test binary act as the onegit binary: the repository's
// hooks exec `<this binary> hook <name>`, exactly as in production.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "hook" {
		os.Exit(hooks.RunHook(os.Args[2], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

type harness struct {
	t   *testing.T
	cfg *config.Config
	srv *server.Server
	url string
}

const adminInitial = "initial-admin-pass"

// start runs a full server on a random port against isolated services.
func start(t *testing.T, tweak func(*config.Config)) *harness {
	t.Helper()
	cfg := testutil.Config(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.HTTP.Addr = ln.Addr().String()
	cfg.HTTP.BaseURL = "http://" + ln.Addr().String()
	cfg.Admin.InitialPassword = adminInitial
	cfg.Secrets.Key = "e2e secret key"
	if tweak != nil {
		tweak(cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv, err := server.New(ctx, cfg, testutil.Logger())
	if err != nil {
		cancel()
		ln.Close()
		t.Fatalf("server.New: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	h := &harness{t: t, cfg: cfg, srv: srv, url: cfg.HTTP.BaseURL}
	testutil.Eventually(t, 10*time.Second, "server up", func() bool {
		resp, err := http.Get(h.url + "/healthz")
		if err == nil {
			resp.Body.Close()
		}
		return err == nil && resp.StatusCode == http.StatusOK
	})
	return h
}

// browser is a cookie-keeping client that does not follow redirects.
type browser struct {
	h *harness
	c *http.Client
}

func (h *harness) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{h: h, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
}

type page struct {
	status   int
	body     string
	location string
	header   http.Header
}

func (b *browser) do(req *http.Request) page {
	b.h.t.Helper()
	resp, err := b.c.Do(req)
	if err != nil {
		b.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return page{status: resp.StatusCode, body: string(body), location: resp.Header.Get("Location"), header: resp.Header}
}

func (b *browser) get(path string) page {
	b.h.t.Helper()
	req, _ := http.NewRequest("GET", b.h.url+path, nil)
	return b.do(req)
}

func (b *browser) post(path string, kv ...string) page {
	b.h.t.Helper()
	form := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		form.Add(kv[i], kv[i+1])
	}
	req, _ := http.NewRequest("POST", b.h.url+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return b.do(req)
}

// ok fetches a page and fails unless it is a 200 containing every string.
func (b *browser) ok(path string, want ...string) string {
	b.h.t.Helper()
	p := b.get(path)
	if p.status != http.StatusOK {
		b.h.t.Fatalf("GET %s: %d (location %q)\n%.500s", path, p.status, p.location, p.body)
	}
	for _, w := range want {
		if !strings.Contains(p.body, w) {
			b.h.t.Errorf("GET %s: page lacks %q", path, w)
		}
	}
	return p.body
}

// login signs in; with a temporary password it also sets newPassword.
func (h *harness) login(user, password, newPassword string) *browser {
	h.t.Helper()
	b := h.browser()
	p := b.post("/login", "username", user, "password", password, "next", "/")
	if p.status != http.StatusSeeOther {
		h.t.Fatalf("login %s: %d\n%.300s", user, p.status, p.body)
	}
	if newPassword != "" {
		if p := b.post("/password/change", "current", password, "new", newPassword, "confirm", newPassword); p.status != http.StatusSeeOther {
			h.t.Fatalf("change password of %s: %d\n%.500s", user, p.status, p.body)
		}
	}
	return b
}

// admin returns a browser for the bootstrap admin, changing the initial
// password on first use.
func (h *harness) admin() *browser {
	h.t.Helper()
	b := h.browser()
	p := b.post("/login", "username", "admin", "password", "admin-password-1", "next", "/")
	if p.status == http.StatusSeeOther {
		return b
	}
	return h.login("admin", adminInitial, "admin-password-1")
}

// createUser adds a user through the admin UI and signs them in once to
// replace the temporary password with Password(name).
func (h *harness) createUser(admin *browser, name, role string) *browser {
	h.t.Helper()
	p := admin.post("/admin/users", "username", name, "email", name+"@example.com", "full_name", strings.ToUpper(name),
		"role", role, "password", "temporary-"+name)
	if p.status != http.StatusSeeOther {
		h.t.Fatalf("create user %s: %d", name, p.status)
	}
	return h.login(name, "temporary-"+name, testutil.Password(name))
}

// remote is the clone URL with credentials.
func (h *harness) remote(user, secret string) string {
	u, _ := url.Parse(h.cfg.HTTPCloneURL())
	u.User = url.UserPassword(user, secret)
	return u.String()
}

// git runs git in dir and returns combined output (remote messages included).
func git(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

var runnerTokenRe = regexp.MustCompile(`ogrun_[A-Za-z0-9_-]+`)
var accessTokenRe = regexp.MustCompile(`og_[A-Za-z0-9_-]{20,}`)
