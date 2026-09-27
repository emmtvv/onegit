package transport_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"onegit/internal/auth"
	"onegit/internal/store"
	"onegit/internal/testutil"
	"onegit/internal/transport"
)

// newHTTP serves the bare repository at /mono.git. Without a store, only
// anonymous access works (auth is nil).
func newHTTP(t *testing.T, publicRead bool, as *auth.Service) (*httptest.Server, *testutil.Work, string) {
	t.Helper()
	repo := testutil.Bare(t)
	w := testutil.NewWork(t)
	sha := w.Commit("init", map[string]string{"README.md": "hi\n"})
	w.Push(repo, "main")
	h, err := transport.NewHTTP(transport.Deps{
		RepoName: "mono", RepoPath: repo.Path, PublicRead: publicRead, Auth: as,
		GitEnv: func(u *store.User) []string { return []string{"ONEGIT_TEST_PUSHER=" + u.Username} },
		Log:    testutil.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, w, sha
}

func TestMatch(t *testing.T) {
	h, err := transport.NewHTTP(transport.Deps{RepoName: "mono"})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/mono.git/info/refs":        "/info/refs",
		"/mono/info/refs":            "/info/refs",
		"/mono.git/git-upload-pack":  "/git-upload-pack",
		"/mono/git-receive-pack":     "/git-receive-pack",
		"/mono.git/objects/info/x":   "",
		"/other.git/info/refs":       "",
		"/mono.git/info/refs/extra":  "",
		"/tree/main/mono/info/refs":  "",
		"/monorepo.git/info/refs":    "",
		"/mono.gitx/git-upload-pack": "",
	} {
		got, ok := h.Match(httptest.NewRequest("GET", path, nil))
		if got != want || ok != (want != "") {
			t.Errorf("Match(%s) = %q, %v", path, got, ok)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/elsewhere", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unmatched path: %d", rec.Code)
	}
}

func TestPublicClone(t *testing.T) {
	srv, _, sha := newHTTP(t, true, nil)
	w := testutil.NewWork(t)
	w.Git("clone", "-q", srv.URL+"/mono.git", "clone")
	w.Dir += "/clone"
	if got := w.Git("rev-parse", "HEAD"); got != sha {
		t.Errorf("cloned HEAD = %s", got)
	}
	// Protocol v2 and a clone URL without .git.
	w2 := testutil.NewWork(t)
	w2.Git("-c", "protocol.version=2", "clone", "-q", srv.URL+"/mono", "c2")

	// Anonymous pushes are always challenged.
	w.Commit("change", map[string]string{"x": "y"})
	if _, err := w.TryGit("push", "origin", "main"); err == nil {
		t.Error("anonymous push succeeded")
	}
}

func TestProtocolErrors(t *testing.T) {
	srv, _, _ := newHTTP(t, true, nil)
	get := func(path string) *http.Response {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := get("/mono.git/info/refs"); r.StatusCode != http.StatusForbidden {
		t.Errorf("dumb protocol: %d", r.StatusCode)
	}
	r := get("/mono.git/info/refs?service=git-receive-pack")
	if r.StatusCode != http.StatusUnauthorized || !strings.Contains(r.Header.Get("WWW-Authenticate"), "Basic") {
		t.Errorf("anonymous receive-pack advertisement: %d %v", r.StatusCode, r.Header)
	}
	if r := get("/mono.git/info/refs?service=git-upload-pack"); r.StatusCode != http.StatusOK ||
		r.Header.Get("Content-Type") != "application/x-git-upload-pack-advertisement" {
		t.Errorf("upload-pack advertisement: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}

	post := func(ctype, enc string, body io.Reader) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+"/mono.git/git-upload-pack", body)
		req.Header.Set("Content-Type", ctype)
		if enc != "" {
			req.Header.Set("Content-Encoding", enc)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	if r := post("text/plain", "", strings.NewReader("")); r.StatusCode != http.StatusBadRequest {
		t.Errorf("wrong content type: %d", r.StatusCode)
	}
	if r := post("application/x-git-upload-pack-request", "br", strings.NewReader("")); r.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("unknown encoding: %d", r.StatusCode)
	}
	if r := post("application/x-git-upload-pack-request", "gzip", strings.NewReader("not gzip")); r.StatusCode != http.StatusBadRequest {
		t.Errorf("bad gzip: %d", r.StatusCode)
	}
	// A gzip-compressed request (what git sends for large negotiations).
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte("0000"))
	zw.Close()
	if r := post("application/x-git-upload-pack-request", "gzip", &gz); r.StatusCode != http.StatusOK ||
		r.Header.Get("Content-Type") != "application/x-git-upload-pack-result" {
		t.Errorf("gzip body: %d", r.StatusCode)
	}
}

func TestAuthenticatedAccess(t *testing.T) {
	cfg := testutil.Config(t)
	st := testutil.Store(t, cfg)
	as := &auth.Service{Store: st, KV: testutil.KV(t, cfg)}
	srv, w, _ := newHTTP(t, false, as)
	testutil.User(t, st, "reader", store.RoleRead)
	testutil.User(t, st, "writer", store.RoleWrite)
	temp := testutil.User(t, st, "temp", store.RoleWrite)
	temp.MustChangePassword = true
	testutil.Must(t, st.UpdateUser(context.Background(), temp))
	tok, _, err := as.NewToken(context.Background(), temp.ID, "t")
	testutil.Must(t, err)

	url := func(user, pw string) string {
		return strings.Replace(srv.URL, "http://", "http://"+user+":"+pw+"@", 1) + "/mono.git"
	}
	if _, err := w.TryGit("ls-remote", srv.URL+"/mono.git"); err == nil {
		t.Error("anonymous read without public read succeeded")
	}
	if _, err := w.TryGit("ls-remote", url("reader", "wrong-password")); err == nil {
		t.Error("wrong password accepted")
	}
	if _, err := w.TryGit("ls-remote", url("reader", testutil.Password("reader"))); err != nil {
		t.Errorf("reader clone: %v", err)
	}
	w.Commit("second", map[string]string{"b.txt": "b"})
	_, err = w.TryGit("push", url("reader", testutil.Password("reader")), "main")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("reader push: %v", err)
	}
	if _, err := w.TryGit("push", url("writer", testutil.Password("writer")), "main"); err != nil {
		t.Errorf("writer push: %v", err)
	}
	// A temporary password is refused over git; a token of the same user works.
	_, err = w.TryGit("ls-remote", url("temp", testutil.Password("temp")))
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("temporary password: %v", err)
	}
	if _, err := w.TryGit("ls-remote", url("anything", tok)); err != nil {
		t.Errorf("token clone: %v", err)
	}
	// Bearer tokens work too.
	req, _ := http.NewRequest("GET", srv.URL+"/mono.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("bearer token: %v %v", resp.StatusCode, err)
	}
	resp.Body.Close()
}
