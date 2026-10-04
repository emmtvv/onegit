package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Setting decodes a server setting into v and reports whether it is set.
func (s *Store) Setting(ctx context.Context, name string, v any) (bool, error) {
	var raw []byte
	err := s.db.QueryRow(ctx, `SELECT value FROM settings WHERE name = $1`, name).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

// SetSetting stores a server setting; nil removes it.
func (s *Store) SetSetting(ctx context.Context, name string, v any) error {
	if v == nil {
		_, err := s.db.Exec(ctx, `DELETE FROM settings WHERE name = $1`, name)
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO settings (name, value) VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE SET value = $2, updated_at = now()`, name, b)
	return err
}
