package blob_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"onegit/internal/blob"
	"onegit/internal/testutil"
)

func TestBlobStore(t *testing.T) {
	cfg := testutil.Config(t)
	s := testutil.Blob(t, cfg)
	ctx := context.Background()
	testutil.Must(t, s.Ping(ctx))

	testutil.Must(t, s.Put(ctx, "dir/a", strings.NewReader("hello world"), 11, "text/plain"))
	// Unknown size streams in parts.
	big := bytes.Repeat([]byte("0123456789"), 100_000)
	testutil.Must(t, s.Put(ctx, "dir/b", bytes.NewReader(big), -1, ""))

	rc, size, err := s.Get(ctx, "dir/a")
	testutil.Must(t, err)
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello world" || size != 11 {
		t.Errorf("Get = %q (%d)", b, size)
	}
	obj, err := s.Open(ctx, "dir/b")
	testutil.Must(t, err)
	if _, err := obj.Seek(int64(len(big)-10), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	tail, _ := io.ReadAll(obj)
	obj.Close()
	if string(tail) != "0123456789" {
		t.Errorf("seek + read = %q", tail)
	}

	if ok, err := s.Exists(ctx, "dir/a"); !ok || err != nil {
		t.Errorf("Exists(dir/a) = %v, %v", ok, err)
	}
	if ok, err := s.Exists(ctx, "nope"); ok || err != nil {
		t.Errorf("Exists(nope) = %v, %v", ok, err)
	}
	if _, _, err := s.Get(ctx, "nope"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get(nope) error = %v", err)
	}
	if _, err := s.Open(ctx, "nope"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Open(nope) error = %v", err)
	}

	testutil.Must(t, s.Put(ctx, "other", strings.NewReader("x"), 1, ""))
	testutil.Must(t, s.DeletePrefix(ctx, "dir/"))
	for _, k := range []string{"dir/a", "dir/b"} {
		if ok, _ := s.Exists(ctx, k); ok {
			t.Errorf("%s survived DeletePrefix", k)
		}
	}
	if ok, _ := s.Exists(ctx, "other"); !ok {
		t.Error("DeletePrefix removed an object outside the prefix")
	}
	testutil.Must(t, s.Delete(ctx, "other"))
	if ok, _ := s.Exists(ctx, "other"); ok {
		t.Error("Delete did not delete")
	}
}

func TestOpenBucket(t *testing.T) {
	cfg := testutil.Config(t)
	ctx := context.Background()
	cfg.S3.CreateBucket = false
	if _, err := blob.Open(ctx, cfg); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing bucket without create_bucket: %v", err)
	}
	cfg.S3.CreateBucket = true
	testutil.Blob(t, cfg)
	// Opening again finds the existing bucket.
	cfg.S3.CreateBucket = false
	if _, err := blob.Open(ctx, cfg); err != nil {
		t.Errorf("existing bucket: %v", err)
	}
	// A plain host:port endpoint works too.
	cfg.S3.Endpoint = strings.TrimPrefix(cfg.S3.Endpoint, "http://")
	if _, err := blob.Open(ctx, cfg); err != nil {
		t.Errorf("host:port endpoint: %v", err)
	}
	cfg.S3.SecretKey = "wrong"
	if _, err := blob.Open(ctx, cfg); err == nil {
		t.Error("wrong credentials accepted")
	}
}
