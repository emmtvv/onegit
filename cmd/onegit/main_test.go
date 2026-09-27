package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"onegit/internal/auth"
	"onegit/internal/config"
	"onegit/internal/store"
	"onegit/internal/testutil"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "onegit-cli")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "onegit")
	out, err := exec.Command("go", "build", "-ldflags", "-X main.version=1.2.3 -X main.commit=abc", "-o", binary, ".").CombinedOutput()
	if err != nil {
		os.Stderr.Write(out)
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// run executes the CLI with extra environment and returns output and exit code.
func run(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func TestUsageAndVersion(t *testing.T) {
	if out, code := run(t, nil, "version"); code != 0 || strings.TrimSpace(out) != "onegit 1.2.3 (commit abc)" {
		t.Errorf("version: %d %q", code, out)
	}
	if out, code := run(t, nil, "--help"); code != 0 || !strings.Contains(out, "onegit serve") {
		t.Errorf("help: %d %q", code, out)
	}
	for _, args := range [][]string{{}, {"bogus"}, {"hook"}} {
		if out, code := run(t, nil, args...); code != 2 || !strings.Contains(out, "Usage:") {
			t.Errorf("%v: %d %q", args, code, out)
		}
	}
	if out, code := run(t, nil, "admin"); code != 1 || !strings.Contains(out, "missing subcommand") {
		t.Errorf("admin: %d %q", code, out)
	}
	if out, code := run(t, []string{"ONEGIT_RUNNER_URL=", "ONEGIT_RUNNER_TOKEN="}, "runner"); code != 1 || !strings.Contains(out, "-url and -token") {
		t.Errorf("runner without settings: %d %q", code, out)
	}
	// A hook outside a server push allows everything.
	if _, code := run(t, []string{"ONEGIT_INTERNAL_URL="}, "hook", "pre-receive"); code != 0 {
		t.Errorf("hook without server: %d", code)
	}
	// serve refuses an incomplete configuration.
	if out, code := run(t, []string{"ONEGIT_DATABASE_HOST="}, "serve"); code != 1 || !strings.Contains(out, "missing required settings") {
		t.Errorf("serve without config: %d %q", code, out)
	}
}

func TestHealthcheck(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	healthy := true
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy || r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	env := []string{"ONEGIT_HTTP_ADDR=:" + port, "ONEGIT_DATABASE_HOST="}
	if out, code := run(t, env, "healthcheck"); code != 0 {
		t.Errorf("healthy: %d %q", code, out)
	}
	if out, code := run(t, env, "healthcheck", "-path", "/readyz"); code != 1 || !strings.Contains(out, "503") {
		t.Errorf("unhealthy path: %d %q", code, out)
	}
	if _, code := run(t, []string{"ONEGIT_HTTP_ADDR=127.0.0.1:1", "ONEGIT_DATABASE_HOST="}, "healthcheck"); code != 1 {
		t.Errorf("nothing listening: %d", code)
	}
}

// cfgEnv turns a test configuration into ONEGIT_* variables.
func cfgEnv(c *config.Config) []string {
	return []string{
		"ONEGIT_DATABASE_HOST=" + c.Database.Host, "ONEGIT_DATABASE_PORT=" + strconv.Itoa(c.Database.Port),
		"ONEGIT_DATABASE_USER=" + c.Database.User, "ONEGIT_DATABASE_PASSWORD=" + c.Database.Password,
		"ONEGIT_DATABASE_NAME=" + c.Database.Name, "ONEGIT_DATABASE_SSL_MODE=" + c.Database.SSLMode,
		"ONEGIT_REDIS_URL=" + c.Redis.URL, "ONEGIT_REDIS_KEY_PREFIX=" + c.Redis.KeyPrefix,
		"ONEGIT_S3_ENDPOINT=" + c.S3.Endpoint, "ONEGIT_S3_BUCKET=" + c.S3.Bucket,
		"ONEGIT_S3_ACCESS_KEY=" + c.S3.AccessKey, "ONEGIT_S3_SECRET_KEY=" + c.S3.SecretKey,
	}
}

func TestAdminCommands(t *testing.T) {
	cfg := testutil.Config(t)
	st := testutil.Store(t, cfg)
	testutil.User(t, st, "alice", store.RoleWrite)
	env := cfgEnv(cfg)

	out, code := run(t, env, "admin", "list-users")
	if code != 0 || !strings.Contains(out, "alice") || !strings.Contains(out, "write") {
		t.Errorf("list-users: %d %q", code, out)
	}
	if out, code := run(t, env, "admin", "reset-password", "-username", "alice", "-password", "short"); code != 1 || !strings.Contains(out, "at least") {
		t.Errorf("short password: %d %q", code, out)
	}
	if out, code := run(t, env, "admin", "reset-password", "-username", "nobody", "-password", "long-enough-pass"); code != 1 {
		t.Errorf("unknown user: %d %q", code, out)
	}
	if out, code := run(t, env, "admin", "reset-password", "-username", "alice", "-password", "temporary-pass"); code != 0 {
		t.Fatalf("reset-password: %d %q", code, out)
	}
	u, _ := st.UserByUsername(context.Background(), "alice")
	if !u.MustChangePassword {
		t.Error("reset password is not temporary")
	}
	a := &auth.Service{Store: st, KV: testutil.KV(t, cfg)}
	if _, err := a.CheckPassword(context.Background(), "alice", "temporary-pass", "1.1.1.1"); err != nil {
		t.Errorf("new password: %v", err)
	}
	if out, code := run(t, env, "admin", "registry-gc", "-grace", "1h"); code != 0 || !strings.Contains(out, "removed 0 stale uploads and 0 blobs") {
		t.Errorf("registry-gc: %d %q", code, out)
	}
	if out, code := run(t, env, "admin", "frobnicate"); code != 1 || !strings.Contains(out, "unknown admin subcommand") {
		t.Errorf("unknown subcommand: %d %q", code, out)
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("ONEGIT_TEST_ENVOR", "set")
	if envOr("ONEGIT_TEST_ENVOR", "def") != "set" || envOr("ONEGIT_TEST_ENVOR_MISSING", "def") != "def" {
		t.Error("envOr")
	}
}
