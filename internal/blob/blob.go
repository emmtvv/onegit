// Package blob stores large objects (registry layers, CI logs and
// artifacts, uploads) in S3-compatible storage.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"onegit/internal/config"
)

var ErrNotFound = errors.New("blob not found")

type Store struct {
	c      *minio.Client
	bucket string
}

func Open(ctx context.Context, cfg *config.Config) (*Store, error) {
	endpoint, secure := cfg.S3.Endpoint, cfg.S3.UseSSL
	// Accept both "host:port" and full URLs.
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		endpoint, secure = u.Host, u.Scheme == "https"
	}
	c, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3.AccessKey, cfg.S3.SecretKey, ""),
		Secure: secure,
		Region: cfg.S3.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client: %w", err)
	}
	s := &Store{c: c, bucket: cfg.S3.Bucket}
	ok, err := c.BucketExists(ctx, s.bucket)
	if err != nil {
		return nil, fmt.Errorf("connect to s3: %w", err)
	}
	if !ok {
		if !cfg.S3.CreateBucket {
			return nil, fmt.Errorf("s3 bucket %q does not exist", s.bucket)
		}
		if err := c.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{Region: cfg.S3.Region}); err != nil {
			// Another replica may have created it concurrently.
			if ok, _ := c.BucketExists(ctx, s.bucket); !ok {
				return nil, fmt.Errorf("create s3 bucket: %w", err)
			}
		}
	}
	return s, nil
}

func (s *Store) Ping(ctx context.Context) error {
	_, err := s.c.BucketExists(ctx, s.bucket)
	return err
}

// streamPartSize bounds the memory used for uploads of unknown size (minio
// otherwise buffers ~500 MiB parts); it allows objects up to ~160 GiB.
const streamPartSize = 16 << 20

// Put uploads r; size may be -1 when unknown.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	opts := minio.PutObjectOptions{ContentType: contentType}
	if size < 0 {
		opts.PartSize = streamPartSize
	}
	_, err := s.c.PutObject(ctx, s.bucket, key, r, size, opts)
	return err
}

// Object is a seekable reader over a stored object (for HTTP range requests).
type Object interface {
	io.ReadSeekCloser
}

// Open returns a seekable handle; reads are fetched lazily from S3.
func (s *Store) Open(ctx context.Context, key string) (Object, error) {
	obj, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return obj, nil
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	obj, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, err
	}
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		if isNotFound(err) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	return obj, st.Size, nil
}

func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.c.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if isNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) Delete(ctx context.Context, key string) error {
	return s.c.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

// DeletePrefix removes every object whose key starts with prefix.
func (s *Store) DeletePrefix(ctx context.Context, prefix string) error {
	objs := s.c.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true})
	for o := range objs {
		if o.Err != nil {
			return o.Err
		}
		if err := s.Delete(ctx, o.Key); err != nil {
			return err
		}
	}
	return nil
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	code := minio.ToErrorResponse(err).Code
	return code == "NoSuchKey" || code == "NotFound" || strings.Contains(err.Error(), "does not exist")
}
