// Package config loads onegit configuration from environment variables,
// optionally layered over a YAML file. Environment always wins, which keeps
// container deployments 12-factor.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/dustin/go-humanize"
	"gopkg.in/yaml.v3"
)

type Config struct {
	// RepoDir holds the bare git repository — the only on-disk state.
	RepoDir string `yaml:"repo_dir" env:"ONEGIT_REPO_DIR"`

	HTTP struct {
		Addr    string `yaml:"addr" env:"ONEGIT_HTTP_ADDR"`
		BaseURL string `yaml:"base_url" env:"ONEGIT_BASE_URL"`
		// TrustProxy makes onegit take the client IP from X-Real-IP /
		// X-Forwarded-For. Enable only behind a reverse proxy.
		TrustProxy bool `yaml:"trust_proxy" env:"ONEGIT_TRUST_PROXY"`
	} `yaml:"http"`

	Repo struct {
		Name          string `yaml:"name" env:"ONEGIT_REPO_NAME"`
		DefaultBranch string `yaml:"default_branch" env:"ONEGIT_DEFAULT_BRANCH"`
		// PublicRead allows anonymous clone and browsing.
		PublicRead bool `yaml:"public_read" env:"ONEGIT_PUBLIC_READ"`
	} `yaml:"repo"`

	Database struct {
		Host     string `yaml:"host" env:"ONEGIT_DATABASE_HOST"`
		Port     int    `yaml:"port" env:"ONEGIT_DATABASE_PORT"`
		User     string `yaml:"user" env:"ONEGIT_DATABASE_USER"`
		Password string `yaml:"password" env:"ONEGIT_DATABASE_PASSWORD"`
		Name     string `yaml:"name" env:"ONEGIT_DATABASE_NAME"`
		// SSLMode is libpq's sslmode: disable, require, verify-ca, verify-full.
		SSLMode  string `yaml:"ssl_mode" env:"ONEGIT_DATABASE_SSL_MODE"`
		MaxConns int    `yaml:"max_conns" env:"ONEGIT_DATABASE_MAX_CONNS"`
	} `yaml:"database"`

	Redis struct {
		URL string `yaml:"url" env:"ONEGIT_REDIS_URL"`
		// KeyPrefix namespaces all keys, so installations can share a
		// Redis database. Replicas of one installation must use the same.
		KeyPrefix string `yaml:"key_prefix" env:"ONEGIT_REDIS_KEY_PREFIX"`
	} `yaml:"redis"`

	S3 struct {
		Endpoint  string `yaml:"endpoint" env:"ONEGIT_S3_ENDPOINT"`
		Region    string `yaml:"region" env:"ONEGIT_S3_REGION"`
		Bucket    string `yaml:"bucket" env:"ONEGIT_S3_BUCKET"`
		AccessKey string `yaml:"access_key" env:"ONEGIT_S3_ACCESS_KEY"`
		SecretKey string `yaml:"secret_key" env:"ONEGIT_S3_SECRET_KEY"`
		UseSSL    bool   `yaml:"use_ssl" env:"ONEGIT_S3_USE_SSL"`
		// CreateBucket creates the bucket on startup if it is missing.
		CreateBucket bool `yaml:"create_bucket" env:"ONEGIT_S3_CREATE_BUCKET"`
	} `yaml:"s3"`

	Registry struct {
		// Enabled serves the container registry (OCI distribution API) at
		// /v2/ on the HTTP listener. Images are named <host>/<name>.
		Enabled bool `yaml:"enabled" env:"ONEGIT_REGISTRY_ENABLED"`
		// Size limits like Gitea's LIMIT_SIZE_CONTAINER / LIMIT_TOTAL_OWNER_SIZE:
		// "500MiB", "10GB", plain bytes; empty or -1 = unlimited.
		MaxBlobSize  string `yaml:"max_blob_size" env:"ONEGIT_REGISTRY_MAX_BLOB_SIZE"`
		MaxTotalSize string `yaml:"max_total_size" env:"ONEGIT_REGISTRY_MAX_TOTAL_SIZE"`
		// Parsed from the strings above; 0 = unlimited.
		MaxBlobBytes  int64 `yaml:"-"`
		MaxTotalBytes int64 `yaml:"-"`
	} `yaml:"registry"`

	CI struct {
		// MaxArtifactSize caps one job's artifact archive ("1GiB").
		MaxArtifactSize string `yaml:"max_artifact_size" env:"ONEGIT_CI_MAX_ARTIFACT_SIZE"`
		// ArtifactRetentionDays applies when a job sets no expire-in.
		ArtifactRetentionDays int `yaml:"artifact_retention_days" env:"ONEGIT_CI_ARTIFACT_RETENTION_DAYS"`
		// MaxCacheSize caps the total size of CI caches; the least recently
		// used are evicted. Caches unused for CacheRetentionDays go too.
		MaxCacheSize       string `yaml:"max_cache_size" env:"ONEGIT_CI_MAX_CACHE_SIZE"`
		CacheRetentionDays int    `yaml:"cache_retention_days" env:"ONEGIT_CI_CACHE_RETENTION_DAYS"`
		// LogRetentionDays removes job logs older than this; 0 keeps them.
		LogRetentionDays int `yaml:"log_retention_days" env:"ONEGIT_CI_LOG_RETENTION_DAYS"`
		// Parsed from the strings above; 0 = unlimited.
		MaxArtifactBytes int64 `yaml:"-"`
		MaxCacheBytes    int64 `yaml:"-"`
	} `yaml:"ci"`

	Secrets struct {
		// Key encrypts deploy secrets in the database (any string; hashed to a
		// 256-bit key). Without it a key is generated and stored in the
		// database. Changing it makes existing secrets unreadable.
		Key string `yaml:"key" env:"ONEGIT_SECRET_KEY"`
	} `yaml:"secrets"`

	Admin struct {
		// The initial admin is created on first start only. The password
		// must be changed at first login.
		Username        string `yaml:"username" env:"ONEGIT_ADMIN_USERNAME"`
		Email           string `yaml:"email" env:"ONEGIT_ADMIN_EMAIL"`
		InitialPassword string `yaml:"initial_password" env:"ONEGIT_ADMIN_PASSWORD"`
	} `yaml:"admin"`

	Auth struct {
		// PasswordLogin enables local username/password login in the web UI.
		PasswordLogin bool `yaml:"password_login" env:"ONEGIT_PASSWORD_LOGIN"`
	} `yaml:"auth"`

	OIDC struct {
		Enabled      bool     `yaml:"enabled" env:"ONEGIT_OIDC_ENABLED"`
		DisplayName  string   `yaml:"display_name" env:"ONEGIT_OIDC_DISPLAY_NAME"`
		Issuer       string   `yaml:"issuer" env:"ONEGIT_OIDC_ISSUER"`
		ClientID     string   `yaml:"client_id" env:"ONEGIT_OIDC_CLIENT_ID"`
		ClientSecret string   `yaml:"client_secret" env:"ONEGIT_OIDC_CLIENT_SECRET"`
		Scopes       []string `yaml:"scopes" env:"ONEGIT_OIDC_SCOPES"`
		AutoRegister bool     `yaml:"auto_register" env:"ONEGIT_OIDC_AUTO_REGISTER"`
		// GroupsClaim is the claim holding the user's groups.
		GroupsClaim string `yaml:"groups_claim" env:"ONEGIT_OIDC_GROUPS_CLAIM"`
		// AdminGroup / WriteGroup map IdP groups to roles on every login.
		// Users in neither group get DefaultRole.
		AdminGroup  string `yaml:"admin_group" env:"ONEGIT_OIDC_ADMIN_GROUP"`
		WriteGroup  string `yaml:"write_group" env:"ONEGIT_OIDC_WRITE_GROUP"`
		DefaultRole string `yaml:"default_role" env:"ONEGIT_OIDC_DEFAULT_ROLE"`
	} `yaml:"oidc"`
}

func Default() *Config {
	c := &Config{RepoDir: "/data/repo"}
	c.HTTP.Addr = ":3000"
	c.HTTP.BaseURL = "http://localhost:3000"
	c.Repo.Name = "monorepo"
	c.Repo.DefaultBranch = "main"
	c.Database.Port = 5432
	c.Database.Name = "onegit"
	c.Database.SSLMode = "prefer"
	c.Database.MaxConns = 10
	c.Redis.KeyPrefix = "onegit:"
	c.S3.Region = "us-east-1"
	c.S3.CreateBucket = true
	c.Registry.Enabled = true
	c.CI.MaxArtifactSize = "1GiB"
	c.CI.ArtifactRetentionDays = 30
	c.CI.MaxCacheSize = "20GiB"
	c.CI.CacheRetentionDays = 7
	c.Admin.Username = "admin"
	c.Auth.PasswordLogin = true
	c.OIDC.DisplayName = "SSO"
	c.OIDC.Scopes = []string{"openid", "profile", "email", "groups"}
	c.OIDC.AutoRegister = true
	c.OIDC.GroupsClaim = "groups"
	c.OIDC.DefaultRole = "read"
	return c
}

// Load reads the optional YAML file, then applies ONEGIT_* environment
// variables on top.
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(b, c); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
		case os.IsNotExist(err):
		default:
			return nil, err
		}
	}
	if err := applyEnv(reflect.ValueOf(c).Elem()); err != nil {
		return nil, err
	}

	c.HTTP.BaseURL = strings.TrimRight(c.HTTP.BaseURL, "/")
	abs, err := filepath.Abs(c.RepoDir)
	if err != nil {
		return nil, err
	}
	c.RepoDir = abs
	if c.Registry.MaxBlobBytes, err = parseSize(c.Registry.MaxBlobSize); err != nil {
		return nil, fmt.Errorf("ONEGIT_REGISTRY_MAX_BLOB_SIZE: %w", err)
	}
	if c.Registry.MaxTotalBytes, err = parseSize(c.Registry.MaxTotalSize); err != nil {
		return nil, fmt.Errorf("ONEGIT_REGISTRY_MAX_TOTAL_SIZE: %w", err)
	}
	if c.CI.MaxArtifactBytes, err = parseSize(c.CI.MaxArtifactSize); err != nil {
		return nil, fmt.Errorf("ONEGIT_CI_MAX_ARTIFACT_SIZE: %w", err)
	}
	if c.CI.MaxCacheBytes, err = parseSize(c.CI.MaxCacheSize); err != nil {
		return nil, fmt.Errorf("ONEGIT_CI_MAX_CACHE_SIZE: %w", err)
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	var missing []string
	if c.Database.Host == "" {
		missing = append(missing, "ONEGIT_DATABASE_HOST")
	}
	if c.Database.User == "" {
		missing = append(missing, "ONEGIT_DATABASE_USER")
	}
	if c.Redis.URL == "" {
		missing = append(missing, "ONEGIT_REDIS_URL")
	}
	if c.S3.Endpoint == "" || c.S3.Bucket == "" {
		missing = append(missing, "ONEGIT_S3_ENDPOINT/ONEGIT_S3_BUCKET")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required settings: %s", strings.Join(missing, ", "))
	}
	if c.Repo.Name == "" || strings.ContainsAny(c.Repo.Name, "/\\ ") {
		return fmt.Errorf("repo name must be a simple name, got %q", c.Repo.Name)
	}
	if c.CI.ArtifactRetentionDays <= 0 || c.CI.CacheRetentionDays <= 0 {
		return fmt.Errorf("ci artifact and cache retention must be at least one day")
	}
	if c.CI.LogRetentionDays < 0 {
		return fmt.Errorf("ci log retention must be 0 (keep) or a number of days")
	}
	if c.OIDC.Enabled && (c.OIDC.Issuer == "" || c.OIDC.ClientID == "") {
		return fmt.Errorf("oidc issuer and client_id are required when oidc is enabled")
	}
	return nil
}

// RepoPath is the bare repository directory; its basename is used by
// git http-backend.
func (c *Config) RepoPath() string { return filepath.Join(c.RepoDir, "repo.git") }

func (c *Config) HTTPCloneURL() string { return c.HTTP.BaseURL + "/" + c.Repo.Name + ".git" }

// RegistryHost is the host[:port] images are tagged with.
func (c *Config) RegistryHost() string {
	if u, err := url.Parse(c.HTTP.BaseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "localhost"
}

// DatabaseURL assembles the Postgres connection URL from the separate
// settings, escaping the credentials.
func (c *Config) DatabaseURL() string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.Database.User, c.Database.Password),
		Host:     net.JoinHostPort(c.Database.Host, strconv.Itoa(c.Database.Port)),
		Path:     "/" + c.Database.Name,
		RawQuery: url.Values{"sslmode": {c.Database.SSLMode}}.Encode(),
	}
	if c.Database.Password == "" {
		u.User = url.User(c.Database.User)
	}
	return u.String()
}

// applyEnv walks the struct and sets every field that has an `env` tag and a
// matching environment variable.
func applyEnv(v reflect.Value) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f, fv := t.Field(i), v.Field(i)
		if f.Type.Kind() == reflect.Struct {
			if err := applyEnv(fv); err != nil {
				return err
			}
			continue
		}
		key := f.Tag.Get("env")
		raw, ok := os.LookupEnv(key)
		if key == "" || !ok {
			continue
		}
		switch f.Type.Kind() {
		case reflect.String:
			fv.SetString(raw)
		case reflect.Bool:
			b, err := strconv.ParseBool(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			fv.SetBool(b)
		case reflect.Int:
			n, err := strconv.Atoi(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			fv.SetInt(int64(n))
		case reflect.Slice:
			var items []string
			for _, s := range strings.Split(raw, ",") {
				if s = strings.TrimSpace(s); s != "" {
					items = append(items, s)
				}
			}
			fv.Set(reflect.ValueOf(items))
		}
	}
	return nil
}

// parseSize parses "10GiB", "500MB", "1024"; "" and negative values mean
// unlimited (0).
func parseSize(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, "-") {
		return 0, nil
	}
	n, err := humanize.ParseBytes(v)
	return int64(n), err
}
