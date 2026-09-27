// Package store is the PostgreSQL-backed persistence layer.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrDuplicate = errors.New("already exists")
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Store struct {
	db *pgxpool.Pool
}

func Open(ctx context.Context, url string, maxConns int) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("database url: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = int32(maxConns)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	s := &Store{db: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close()                         { s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.Ping(ctx) }

// migrationLockID is an arbitrary constant for pg_advisory_lock so replicas
// starting at the same time do not race on migrations.
const migrationLockID = 0x6f6e65676974

// migrate applies migrations/NNN_*.sql in order, each in its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockID) }()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	var current int
	if err := conn.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}

	files, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		base := strings.TrimPrefix(f, "migrations/")
		num, _, _ := strings.Cut(base, "_")
		v, err := strconv.Atoi(num)
		if err != nil {
			return fmt.Errorf("bad migration name %s", base)
		}
		if v <= current {
			continue
		}
		sql, err := migrationsFS.ReadFile(f)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, v)
			return err
		})
		if err != nil {
			return fmt.Errorf("%s: %w", base, err)
		}
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// Secret returns a server-wide secret, generating and storing it on first use.
// Concurrent callers on different replicas always end up with the same value.
func (s *Store) Secret(ctx context.Context, name string, generate func() ([]byte, error)) ([]byte, error) {
	var v []byte
	err := s.db.QueryRow(ctx, `SELECT value FROM server_secrets WHERE name = $1`, name).Scan(&v)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if v, err = generate(); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO server_secrets (name, value) VALUES ($1, $2) ON CONFLICT (name) DO NOTHING`, name, v); err != nil {
		return nil, err
	}
	err = s.db.QueryRow(ctx, `SELECT value FROM server_secrets WHERE name = $1`, name).Scan(&v)
	return v, err
}
