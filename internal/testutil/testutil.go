// Package testutil provides isolated backing services and git helpers for
// tests.
//
// Integration tests need Postgres, Redis and S3. Their addresses come from
// the environment (docker-compose.test.yml provides all three):
//
//	ONEGIT_TEST_POSTGRES       postgres://user:pass@host:port/db (a database to connect to for CREATE DATABASE)
//	ONEGIT_TEST_REDIS          redis://host:port/0
//	ONEGIT_TEST_S3             http://host:port
//	ONEGIT_TEST_S3_ACCESS_KEY  / ONEGIT_TEST_S3_SECRET_KEY
//
// Without them integration tests are skipped, unless ONEGIT_TEST_REQUIRE=1
// (set in CI), which turns the skip into a failure. Every Config gets its
// own database, Redis key prefix and S3 bucket, all removed afterwards.
package testutil

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/redis/go-redis/v9"

	"onegit/internal/auth"
	"onegit/internal/blob"
	"onegit/internal/config"
	"onegit/internal/kv"
	"onegit/internal/store"
)

const (
	EnvPostgres    = "ONEGIT_TEST_POSTGRES"
	EnvRedis       = "ONEGIT_TEST_REDIS"
	EnvS3          = "ONEGIT_TEST_S3"
	EnvS3AccessKey = "ONEGIT_TEST_S3_ACCESS_KEY"
	EnvS3SecretKey = "ONEGIT_TEST_S3_SECRET_KEY"
	EnvRequire     = "ONEGIT_TEST_REQUIRE"
)

// RequireServices skips (or, with ONEGIT_TEST_REQUIRE=1, fails) the test when
// the backing services are not configured.
func RequireServices(t testing.TB) {
	t.Helper()
	var missing []string
	for _, k := range []string{EnvPostgres, EnvRedis, EnvS3} {
		if os.Getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return
	}
	msg := "integration test: set " + strings.Join(missing, ", ") + " (see docker-compose.test.yml)"
	if os.Getenv(EnvRequire) == "1" {
		t.Fatal(msg)
	}
	t.Skip(msg)
}

// Config returns a valid configuration backed by fresh, isolated services.
func Config(t testing.TB) *config.Config {
	t.Helper()
	RequireServices(t)
	ctx := context.Background()
	id := strings.ToLower(strconv.FormatInt(time.Now().UnixNano(), 36) + auth.RandomString(4))
	id = strings.NewReplacer("-", "", "_", "").Replace(id)

	cfg := config.Default()
	cfg.RepoDir = t.TempDir()
	cfg.HTTP.BaseURL = "http://127.0.0.1:1"
	cfg.Admin.InitialPassword = "initial-admin-pass"

	// Postgres: a database per test.
	admin, err := url.Parse(os.Getenv(EnvPostgres))
	if err != nil {
		t.Fatalf("%s: %v", EnvPostgres, err)
	}
	dbName := "onegit_test_" + id
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize()); err != nil {
		conn.Close(ctx)
		t.Fatalf("create database: %v", err)
	}
	conn.Close(ctx)
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), admin.String())
		if err != nil {
			return
		}
		defer conn.Close(context.Background())
		conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{dbName}.Sanitize()+" WITH (FORCE)")
	})
	cfg.Database.Host = admin.Hostname()
	if p := admin.Port(); p != "" {
		cfg.Database.Port, _ = strconv.Atoi(p)
	}
	cfg.Database.User = admin.User.Username()
	cfg.Database.Password, _ = admin.User.Password()
	cfg.Database.Name = dbName
	cfg.Database.SSLMode = "disable"
	if m := admin.Query().Get("sslmode"); m != "" {
		cfg.Database.SSLMode = m
	}
	cfg.Database.MaxConns = 8

	// Redis: a key prefix per test.
	cfg.Redis.URL = os.Getenv(EnvRedis)
	cfg.Redis.KeyPrefix = "onegit-test:" + id + ":"
	t.Cleanup(func() { cleanRedis(cfg.Redis.URL, cfg.Redis.KeyPrefix) })

	// S3: a bucket per test.
	cfg.S3.Endpoint = os.Getenv(EnvS3)
	cfg.S3.AccessKey = os.Getenv(EnvS3AccessKey)
	cfg.S3.SecretKey = os.Getenv(EnvS3SecretKey)
	cfg.S3.Bucket = "onegit-test-" + id
	cfg.S3.CreateBucket = true
	t.Cleanup(func() { cleanBucket(cfg) })
	return cfg
}

func cleanRedis(u, prefix string) {
	opt, err := redis.ParseURL(u)
	if err != nil {
		return
	}
	rdb := redis.NewClient(opt)
	defer rdb.Close()
	ctx := context.Background()
	iter := rdb.Scan(ctx, 0, prefix+"*", 500).Iterator()
	for iter.Next(ctx) {
		rdb.Del(ctx, iter.Val())
	}
}

func cleanBucket(cfg *config.Config) {
	u, err := url.Parse(cfg.S3.Endpoint)
	if err != nil {
		return
	}
	c, err := minio.New(u.Host, &minio.Options{
		Creds: credentials.NewStaticV4(cfg.S3.AccessKey, cfg.S3.SecretKey, ""), Secure: u.Scheme == "https",
		Region: cfg.S3.Region,
	})
	if err != nil {
		return
	}
	ctx := context.Background()
	if ok, _ := c.BucketExists(ctx, cfg.S3.Bucket); !ok {
		return
	}
	for o := range c.ListObjects(ctx, cfg.S3.Bucket, minio.ListObjectsOptions{Recursive: true}) {
		if o.Err == nil {
			c.RemoveObject(ctx, cfg.S3.Bucket, o.Key, minio.RemoveObjectOptions{})
		}
	}
	c.RemoveBucket(ctx, cfg.S3.Bucket)
}

// Store opens the test database (running migrations).
func Store(t testing.TB, cfg *config.Config) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), cfg.DatabaseURL(), cfg.Database.MaxConns)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// KV connects to Redis with the test's key prefix.
func KV(t testing.TB, cfg *config.Config) *kv.KV {
	t.Helper()
	k, err := kv.Open(context.Background(), cfg.Redis.URL, cfg.Redis.KeyPrefix)
	if err != nil {
		t.Fatalf("open kv: %v", err)
	}
	t.Cleanup(func() { k.Close() })
	return k
}

// Blob opens the test bucket.
func Blob(t testing.TB, cfg *config.Config) *blob.Store {
	t.Helper()
	b, err := blob.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	return b
}

// Logger discards output unless ONEGIT_TEST_LOG=1.
func Logger() *slog.Logger {
	w := io.Discard
	if os.Getenv("ONEGIT_TEST_LOG") == "1" {
		w = os.Stderr
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// User creates an active user with a usable password ("password-" + name).
func User(t testing.TB, st *store.Store, name string, role store.Role) *store.User {
	t.Helper()
	hash, err := auth.HashPassword(Password(name))
	if err != nil {
		t.Fatal(err)
	}
	u := &store.User{Username: name, Email: name + "@example.com", FullName: strings.ToUpper(name[:1]) + name[1:],
		PasswordHash: hash, Role: role, Active: true}
	if err := st.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	return u
}

// Password is the password User gives a user.
func Password(name string) string { return "password-" + name }

// Eventually polls cond until it returns true or the timeout passes.
func Eventually(t testing.TB, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Must fails the test on error.
func Must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
