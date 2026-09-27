package store

import (
	"context"
	"time"
)

type Token struct {
	ID        int64
	UserID    int64
	Name      string
	Prefix    string
	CreatedAt time.Time
	LastUsed  *time.Time
}

func (s *Store) AddToken(ctx context.Context, t *Token, hash string) error {
	return s.db.QueryRow(ctx,
		`INSERT INTO tokens (user_id, name, hash, prefix) VALUES ($1, $2, $3, $4) RETURNING id, created_at`,
		t.UserID, t.Name, hash, t.Prefix).Scan(&t.ID, &t.CreatedAt)
}

func (s *Store) ListTokens(ctx context.Context, userID int64) ([]*Token, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, user_id, name, prefix, created_at, last_used FROM tokens WHERE user_id=$1 ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.CreatedAt, &t.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteToken(ctx context.Context, userID, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM tokens WHERE id=$1 AND user_id=$2`, id, userID)
	return err
}

// UserByTokenHash returns the token owner and marks the token as used.
func (s *Store) UserByTokenHash(ctx context.Context, hash string) (*User, error) {
	var uid int64
	err := s.db.QueryRow(ctx, `UPDATE tokens SET last_used=now() WHERE hash=$1 RETURNING user_id`, hash).Scan(&uid)
	if err != nil {
		return nil, notFound(err)
	}
	return s.UserByID(ctx, uid)
}
