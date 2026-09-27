package hooks

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

const (
	oldSHA = "1111111111111111111111111111111111111111"
	newSHA = "2222222222222222222222222222222222222222"
)

// hookServer records the request and answers with resp.
func hookServer(t *testing.T, status int, resp Response) (*httptest.Server, *Request, *string) {
	t.Helper()
	var got Request
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &path
}

func setEnv(t *testing.T, url string) {
	t.Setenv(EnvInternalURL, url)
	t.Setenv(EnvInternalToken, "secret")
	t.Setenv(EnvPusherID, "42")
	t.Setenv(EnvPusherName, "alice")
}

func TestRunHookForwardsUpdates(t *testing.T) {
	srv, got, path := hookServer(t, http.StatusOK, Response{Message: "remote says hi\n"})
	setEnv(t, srv.URL)
	t.Setenv("GIT_QUARANTINE_PATH", "/repo/objects/incoming-x")
	t.Setenv("GIT_OBJECT_DIRECTORY", "/repo/objects/incoming-x")
	t.Setenv("GIT_PUSH_OPTION_COUNT", "2")
	t.Setenv("GIT_PUSH_OPTION_0", "ci.skip")
	t.Setenv("GIT_PUSH_OPTION_1", "reviewer=bob")

	stdin := strings.NewReader(oldSHA + " " + newSHA + " refs/heads/main\n" + "garbage line\n" +
		oldSHA + " " + newSHA + " refs/tags/v1\n")
	var stdout, stderr bytes.Buffer
	if code := RunHook("pre-receive", stdin, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, stderr.String())
	}
	if *path != "/internal/hook/pre-receive" {
		t.Errorf("path = %q", *path)
	}
	if got.PusherID != 42 || got.PusherName != "alice" || len(got.Updates) != 2 ||
		got.Updates[1] != (RefUpdate{OldSHA: oldSHA, NewSHA: newSHA, Ref: "refs/tags/v1"}) {
		t.Errorf("request = %+v", got)
	}
	if !slices.Contains(got.GitEnv, "GIT_QUARANTINE_PATH=/repo/objects/incoming-x") || len(got.GitEnv) != 2 {
		t.Errorf("git env = %v", got.GitEnv)
	}
	if !slices.Equal(got.PushOptions, []string{"ci.skip", "reviewer=bob"}) {
		t.Errorf("push options = %v", got.PushOptions)
	}
	if stderr.String() != "remote says hi\n" {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunHookReject(t *testing.T) {
	srv, _, _ := hookServer(t, http.StatusOK, Response{Reject: true, Message: "onegit: no"})
	setEnv(t, srv.URL)
	var stderr bytes.Buffer
	if code := RunHook("pre-receive", strings.NewReader(""), &bytes.Buffer{}, &stderr); code != 1 {
		t.Errorf("exit code %d", code)
	}
	if !strings.Contains(stderr.String(), "onegit: no") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunHookFailures(t *testing.T) {
	// A broken pre-receive blocks the push; post-receive failures don't.
	srv, _, _ := hookServer(t, http.StatusInternalServerError, Response{})
	for _, c := range []struct {
		url  string
		hook string
		want int
	}{
		{srv.URL, "pre-receive", 1},
		{srv.URL, "post-receive", 0},
		{"http://127.0.0.1:1", "pre-receive", 1},
		{"http://127.0.0.1:1", "post-receive", 0},
	} {
		setEnv(t, c.url)
		var stderr bytes.Buffer
		if code := RunHook(c.hook, strings.NewReader(""), &bytes.Buffer{}, &stderr); code != c.want {
			t.Errorf("%s via %s: exit %d, want %d", c.hook, c.url, code, c.want)
		}
		if !strings.Contains(stderr.String(), "onegit: hook "+c.hook) {
			t.Errorf("stderr = %q", stderr.String())
		}
	}
	// Wrong token → 403 → blocked.
	setEnv(t, srv.URL)
	t.Setenv(EnvInternalToken, "wrong")
	if code := RunHook("pre-receive", strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Errorf("wrong token: exit %d", code)
	}
}

func TestRunHookWithoutServer(t *testing.T) {
	// Pushes that don't come through onegit (no env) are allowed.
	t.Setenv(EnvInternalURL, "")
	t.Setenv(EnvInternalToken, "")
	if code := RunHook("pre-receive", strings.NewReader(oldSHA+" "+newSHA+" refs/heads/x\n"), &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Errorf("exit code %d", code)
	}
}
