package store

import (
	"context"
	"time"
)

type AvatarSource string

const (
	AvatarManual AvatarSource = "manual" // uploaded by the user
	AvatarOIDC   AvatarSource = "oidc"   // from the identity provider, read-only
)

// Avatar describes a user's profile picture; the image itself is in the
// blob store (see package avatars).
type Avatar struct {
	UserID      int64
	Source      AvatarSource
	ContentType string
	UpdatedAt   time.Time
}

// AvatarRef points at a stored avatar together with its owner, for the
// lookup index the templates use.
type AvatarRef struct {
	UserID    int64
	Username  string
	Email     string
	Source    AvatarSource
	UpdatedAt time.Time
}

func (s *Store) Avatar(ctx context.Context, userID int64) (*Avatar, error) {
	a := Avatar{UserID: userID}
	err := s.db.QueryRow(ctx, `SELECT source, content_type, updated_at FROM user_avatars WHERE user_id=$1`, userID).
		Scan(&a.Source, &a.ContentType, &a.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &a, nil
}

func (s *Store) SetAvatar(ctx context.Context, a *Avatar) error {
	return s.db.QueryRow(ctx,
		`INSERT INTO user_avatars (user_id, source, content_type) VALUES ($1, $2, $3)
		 ON CONFLICT (user_id) DO UPDATE SET source=$2, content_type=$3, updated_at=now()
		 RETURNING updated_at`,
		a.UserID, a.Source, a.ContentType).Scan(&a.UpdatedAt)
}

// DeleteAvatar removes a user's avatar if it came from source and reports
// whether there was one.
func (s *Store) DeleteAvatar(ctx context.Context, userID int64, source AvatarSource) (bool, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM user_avatars WHERE user_id=$1 AND source=$2`, userID, source)
	return tag.RowsAffected() > 0, err
}

// SetAvatarSource relabels a user's avatar, e.g. an IdP picture becomes a
// manual one when the account is unlinked from SSO.
func (s *Store) SetAvatarSource(ctx context.Context, userID int64, source AvatarSource) error {
	_, err := s.db.Exec(ctx, `UPDATE user_avatars SET source=$2 WHERE user_id=$1`, userID, source)
	return err
}

// AvatarRefs lists every stored avatar with its owner's username and email.
func (s *Store) AvatarRefs(ctx context.Context) ([]AvatarRef, error) {
	rows, err := s.db.Query(ctx, `SELECT a.user_id, u.username, u.email, a.source, a.updated_at
		FROM user_avatars a JOIN users u ON u.id = a.user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AvatarRef
	for rows.Next() {
		var r AvatarRef
		if err := rows.Scan(&r.UserID, &r.Username, &r.Email, &r.Source, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
