// Package avatars stores user profile pictures: the image in S3 under
// avatars/<user id>, its source and type in Postgres.
package avatars

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"

	"onegit/internal/blob"
	"onegit/internal/store"
)

// MaxSize caps avatar images.
const MaxSize = 1 << 20

var (
	ErrTooLarge    = errors.New("the image is larger than 1 MB")
	ErrUnsupported = errors.New("the image is not a PNG, JPEG, GIF or WebP")
)

type Service struct {
	Store *store.Store
	Blob  *blob.Store
}

func key(userID int64) string { return "avatars/" + strconv.FormatInt(userID, 10) }

// ContentType sniffs an image and reports whether it is a format browsers
// render safely as <img> (no SVG: it can carry script).
func ContentType(data []byte) (string, bool) {
	ct := http.DetectContentType(data)
	switch ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return ct, true
	}
	return "", false
}

// Set stores data as the user's avatar from source, replacing any other.
func (s *Service) Set(ctx context.Context, userID int64, source store.AvatarSource, data []byte) error {
	if len(data) > MaxSize {
		return ErrTooLarge
	}
	ct, ok := ContentType(data)
	if !ok {
		return ErrUnsupported
	}
	// The object first: the row is what makes it visible, and its new
	// updated_at changes the image URL so caches pick the new one up.
	if err := s.Blob.Put(ctx, key(userID), bytes.NewReader(data), int64(len(data)), ct); err != nil {
		return err
	}
	return s.Store.SetAvatar(ctx, &store.Avatar{UserID: userID, Source: source, ContentType: ct})
}

// Delete removes the user's avatar if it came from source.
func (s *Service) Delete(ctx context.Context, userID int64, source store.AvatarSource) error {
	ok, err := s.Store.DeleteAvatar(ctx, userID, source)
	if err != nil || !ok {
		return err
	}
	return s.Blob.Delete(ctx, key(userID))
}

// Purge removes the image of a deleted user (the row goes with the user).
func (s *Service) Purge(ctx context.Context, userID int64) error {
	return s.Blob.Delete(ctx, key(userID))
}

// Open returns the user's avatar and a reader over its image.
func (s *Service) Open(ctx context.Context, userID int64) (*store.Avatar, blob.Object, error) {
	a, err := s.Store.Avatar(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	obj, err := s.Blob.Open(ctx, key(userID))
	if errors.Is(err, blob.ErrNotFound) {
		return nil, nil, store.ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	return a, obj, nil
}
