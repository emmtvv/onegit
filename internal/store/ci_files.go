package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---- artifacts ----

type Artifact struct {
	ID        int64
	JobID     int64
	RunID     int64
	JobName   string
	Size      int64
	BlobKey   string
	ExpiresAt time.Time
	CreatedAt time.Time
}

func (a *Artifact) Expired() bool { return time.Now().After(a.ExpiresAt) }

const artifactCols = `a.id, a.job_id, a.run_id, j.name, a.size, a.blob_key, a.expires_at, a.created_at`
const artifactFrom = ` FROM ci_artifacts a JOIN ci_jobs j ON j.id = a.job_id `

// SaveArtifact records a job's artifact archive. A job has at most one: a
// retried upload replaces the previous one, whose blob key is returned so
// the caller can delete it.
func (s *Store) SaveArtifact(ctx context.Context, a *Artifact) (replaced string, err error) {
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT blob_key FROM ci_artifacts WHERE job_id = $1 FOR UPDATE`, a.JobID).Scan(&replaced)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO ci_artifacts (job_id, run_id, size, blob_key, expires_at) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (job_id) DO UPDATE SET size = $3, blob_key = $4, expires_at = $5, created_at = now()
			RETURNING id, created_at`, a.JobID, a.RunID, a.Size, a.BlobKey, a.ExpiresAt).Scan(&a.ID, &a.CreatedAt)
	})
	return replaced, err
}

func (s *Store) ArtifactByID(ctx context.Context, id int64) (*Artifact, error) {
	rows, _ := s.db.Query(ctx, `SELECT `+artifactCols+artifactFrom+`WHERE a.id = $1`, id)
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByPos[Artifact])
	return a, notFound(err)
}

// ArtifactsForRun lists the artifacts of a run's jobs.
func (s *Store) ArtifactsForRun(ctx context.Context, runID int64) ([]*Artifact, error) {
	rows, _ := s.db.Query(ctx, `SELECT `+artifactCols+artifactFrom+`WHERE a.run_id = $1 ORDER BY a.job_id`, runID)
	return pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[Artifact])
}

// ExpiredArtifacts removes up to limit expired artifact rows and returns
// their blob keys.
func (s *Store) ExpiredArtifacts(ctx context.Context, limit int) ([]string, error) {
	rows, _ := s.db.Query(ctx, `DELETE FROM ci_artifacts WHERE id IN (
		SELECT id FROM ci_artifacts WHERE expires_at < now() ORDER BY expires_at LIMIT $1) RETURNING blob_key`, limit)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ---- caches ----

type Cache struct {
	ID         int64
	Scope      string
	Key        string
	Size       int64
	BlobKey    string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

const cacheCols = `id, scope, key, size, blob_key, created_at, last_used_at`

// FindCache looks for key in the scopes, in order of preference; with
// prefix it falls back to the newest cache whose key starts with prefix.
// A hit counts as a use.
func (s *Store) FindCache(ctx context.Context, scopes []string, key, prefix string) (*Cache, error) {
	try := func(q string, args ...any) (*Cache, error) {
		rows, _ := s.db.Query(ctx, `SELECT `+cacheCols+` FROM ci_caches WHERE `+q, args...)
		c, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByPos[Cache])
		return c, notFound(err)
	}
	var c *Cache
	var err error
	for _, scope := range scopes {
		if c, err = try(`scope = $1 AND key = $2`, scope, key); !errors.Is(err, ErrNotFound) {
			break
		}
	}
	if errors.Is(err, ErrNotFound) && prefix != "" {
		for _, scope := range scopes {
			c, err = try(`scope = $1 AND starts_with(key, $2) ORDER BY created_at DESC LIMIT 1`, scope, prefix)
			if !errors.Is(err, ErrNotFound) {
				break
			}
		}
	}
	if err != nil {
		return nil, err
	}
	_, err = s.db.Exec(ctx, `UPDATE ci_caches SET last_used_at = now() WHERE id = $1`, c.ID)
	return c, err
}

func (s *Store) CacheByID(ctx context.Context, id int64) (*Cache, error) {
	rows, _ := s.db.Query(ctx, `SELECT `+cacheCols+` FROM ci_caches WHERE id = $1`, id)
	c, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByPos[Cache])
	return c, notFound(err)
}

// SaveCache records a cache. Caches are immutable: ErrDuplicate means the
// key already exists in the scope (another job saved it first).
func (s *Store) SaveCache(ctx context.Context, c *Cache) error {
	err := s.db.QueryRow(ctx, `INSERT INTO ci_caches (scope, key, size, blob_key) VALUES ($1, $2, $3, $4)
		RETURNING id, created_at, last_used_at`, c.Scope, c.Key, c.Size, c.BlobKey).Scan(&c.ID, &c.CreatedAt, &c.LastUsedAt)
	if isUniqueViolation(err) {
		return ErrDuplicate
	}
	return err
}

// EvictCaches removes caches unused for maxAge and then the least recently
// used ones until the total size fits maxTotal (0 = no limit). It returns
// the blob keys to delete.
func (s *Store) EvictCaches(ctx context.Context, maxAge time.Duration, maxTotal int64) ([]string, error) {
	rows, _ := s.db.Query(ctx, `DELETE FROM ci_caches WHERE last_used_at < now() - $1::interval RETURNING blob_key`, maxAge)
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || maxTotal <= 0 {
		return keys, err
	}
	rows, _ = s.db.Query(ctx, `DELETE FROM ci_caches WHERE id IN (
		SELECT id FROM (SELECT id, sum(size) OVER (ORDER BY last_used_at DESC, id DESC) AS running FROM ci_caches) c
		WHERE running > $1) RETURNING blob_key`, maxTotal)
	more, err := pgx.CollectRows(rows, pgx.RowTo[string])
	return append(keys, more...), err
}

// CacheUsage is the number and total size of caches.
func (s *Store) CacheUsage(ctx context.Context) (n int, size int64, err error) {
	err = s.db.QueryRow(ctx, `SELECT count(*), COALESCE(sum(size), 0) FROM ci_caches`).Scan(&n, &size)
	return
}

// ---- schedules ----

type ScheduleState struct {
	Pipeline string
	Cron     string
	Timezone string
	LastAt   time.Time
}

func (s *Store) ScheduleStates(ctx context.Context) ([]*ScheduleState, error) {
	rows, _ := s.db.Query(ctx, `SELECT pipeline, cron, timezone, last_at FROM ci_schedules`)
	return pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[ScheduleState])
}

// AdvanceSchedule moves a schedule's last firing from old (zero: not seen
// yet) to at. It returns false when another replica got there first.
func (s *Store) AdvanceSchedule(ctx context.Context, st *ScheduleState, old, at time.Time) (bool, error) {
	var tag interface{ RowsAffected() int64 }
	var err error
	if old.IsZero() {
		tag, err = s.db.Exec(ctx, `INSERT INTO ci_schedules (pipeline, cron, timezone, last_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING`, st.Pipeline, st.Cron, st.Timezone, at)
	} else {
		tag, err = s.db.Exec(ctx, `UPDATE ci_schedules SET last_at = $5 WHERE pipeline = $1 AND cron = $2 AND timezone = $3
			AND last_at = $4`, st.Pipeline, st.Cron, st.Timezone, old, at)
	}
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
