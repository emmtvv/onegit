package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "-1": 0, "1024": 1024, "40MiB": 40 << 20, "1MB": 1000000, "2 GiB": 2 << 30} {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := parseSize("lots"); err == nil {
		t.Error("parseSize accepted garbage")
	}
}

func TestDatabaseURL(t *testing.T) {
	c := Default()
	c.Database.Host, c.Database.User, c.Database.Password, c.Database.SSLMode = "db.local", "og", "p@ss:w/rd?", "disable"
	if got, want := c.DatabaseURL(), "postgres://og:p%40ss%3Aw%2Frd%3F@db.local:5432/onegit?sslmode=disable"; got != want {
		t.Errorf("DatabaseURL() = %q; want %q", got, want)
	}
	c.Database.Host, c.Database.Password = "::1", ""
	if got, want := c.DatabaseURL(), "postgres://og@[::1]:5432/onegit?sslmode=disable"; got != want {
		t.Errorf("DatabaseURL() = %q; want %q", got, want)
	}
}

// requiredEnv makes Load's validation pass.
func requiredEnv(t *testing.T) {
	t.Setenv("ONEGIT_DATABASE_HOST", "db")
	t.Setenv("ONEGIT_DATABASE_USER", "og")
	t.Setenv("ONEGIT_REDIS_URL", "redis://redis:6379/0")
	t.Setenv("ONEGIT_S3_ENDPOINT", "http://s3:8333")
	t.Setenv("ONEGIT_S3_BUCKET", "onegit")
}

func TestLoadDefaults(t *testing.T) {
	requiredEnv(t)
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTP.Addr != ":3000" || c.Repo.Name != "monorepo" || c.Repo.DefaultBranch != "main" || !c.Registry.Enabled ||
		c.Database.Port != 5432 || c.Redis.KeyPrefix != "onegit:" || !c.Auth.PasswordLogin || c.OIDC.DefaultRole != "read" {
		t.Errorf("defaults = %+v", c)
	}
	if c.RepoPath() != "/data/repo/repo.git" {
		t.Errorf("RepoPath = %q", c.RepoPath())
	}
	if c.HTTPCloneURL() != "http://localhost:3000/monorepo.git" {
		t.Errorf("HTTPCloneURL = %q", c.HTTPCloneURL())
	}
	if c.RegistryHost() != "localhost:3000" {
		t.Errorf("RegistryHost = %q", c.RegistryHost())
	}
}

func TestLoadEnvOverridesFile(t *testing.T) {
	requiredEnv(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "onegit.yaml")
	os.WriteFile(file, []byte(`
repo_dir: ./data
http:
  base_url: https://git.example.com/
  trust_proxy: true
repo:
  name: fromfile
  public_read: true
database:
  port: 6543
oidc:
  scopes: [openid]
registry:
  max_blob_size: 1GiB
`), 0o644)
	t.Setenv("ONEGIT_REPO_NAME", "fromenv")
	t.Setenv("ONEGIT_DATABASE_MAX_CONNS", "3")
	t.Setenv("ONEGIT_OIDC_SCOPES", "openid, email ,, groups")
	t.Setenv("ONEGIT_REGISTRY_ENABLED", "false")
	t.Setenv("ONEGIT_REGISTRY_MAX_TOTAL_SIZE", "10GB")
	c, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if c.Repo.Name != "fromenv" {
		t.Errorf("env must win: repo name %q", c.Repo.Name)
	}
	if !c.Repo.PublicRead || !c.HTTP.TrustProxy || c.Database.Port != 6543 {
		t.Errorf("file values lost: %+v", c)
	}
	if c.HTTP.BaseURL != "https://git.example.com" {
		t.Errorf("trailing slash kept: %q", c.HTTP.BaseURL)
	}
	if !filepath.IsAbs(c.RepoDir) {
		t.Errorf("repo dir not absolute: %q", c.RepoDir)
	}
	if c.Database.MaxConns != 3 || c.Registry.Enabled {
		t.Errorf("env int/bool not applied: %+v", c.Database)
	}
	if !slices.Equal(c.OIDC.Scopes, []string{"openid", "email", "groups"}) {
		t.Errorf("scopes = %q", c.OIDC.Scopes)
	}
	if c.Registry.MaxBlobBytes != 1<<30 || c.Registry.MaxTotalBytes != 10e9 {
		t.Errorf("sizes = %d, %d", c.Registry.MaxBlobBytes, c.Registry.MaxTotalBytes)
	}
	if c.RegistryHost() != "git.example.com" {
		t.Errorf("RegistryHost = %q", c.RegistryHost())
	}
}

func TestLoadMissingFileIsFine(t *testing.T) {
	requiredEnv(t)
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err != nil {
		t.Errorf("a missing config file must be ignored: %v", err)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]struct {
		env  map[string]string
		file string
		want string
	}{
		"missing settings": {env: map[string]string{"ONEGIT_DATABASE_HOST": "", "ONEGIT_REDIS_URL": ""},
			want: "ONEGIT_DATABASE_HOST"},
		"bad bool":       {env: map[string]string{"ONEGIT_PUBLIC_READ": "maybe"}, want: "ONEGIT_PUBLIC_READ"},
		"bad int":        {env: map[string]string{"ONEGIT_DATABASE_PORT": "x"}, want: "ONEGIT_DATABASE_PORT"},
		"bad size":       {env: map[string]string{"ONEGIT_REGISTRY_MAX_BLOB_SIZE": "huge"}, want: "MAX_BLOB_SIZE"},
		"bad repo name":  {env: map[string]string{"ONEGIT_REPO_NAME": "a/b"}, want: "repo name"},
		"empty name":     {env: map[string]string{"ONEGIT_REPO_NAME": ""}, want: "repo name"},
		"oidc no issuer": {env: map[string]string{"ONEGIT_OIDC_ENABLED": "true"}, want: "oidc"},
		"bad yaml":       {file: "http: [", want: "parse"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			requiredEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			path := ""
			if c.file != "" {
				path = filepath.Join(t.TempDir(), "c.yaml")
				os.WriteFile(path, []byte(c.file), 0o644)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestLoadUnreadableFile(t *testing.T) {
	requiredEnv(t)
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("reading a directory as config succeeded")
	}
}

func TestRegistryHostFallback(t *testing.T) {
	c := Default()
	c.HTTP.BaseURL = "not a url"
	if got := c.RegistryHost(); got != "localhost" {
		t.Errorf("RegistryHost = %q", got)
	}
}
