package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrReferenced is returned when deleting a manifest that an index still
// points to.
var ErrReferenced = errors.New("still referenced")

// MissingRefsError lists blobs or child manifests a pushed manifest refers to
// but the registry does not have.
type MissingRefsError struct{ Digests []string }

func (e *MissingRefsError) Error() string { return "manifest references unknown blobs or manifests" }

type RegistryBlob struct {
	Digest string
	Size   int64
	S3Key  string
}

type RegistryUpload struct {
	ID        string
	Repo      string
	Size      int64
	PartKeys  []string
	HashState []byte
	UserID    *int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type RegistryManifest struct {
	ID        int64
	Repo      string
	Digest    string
	MediaType string
	Content   []byte
	TotalSize int64
	Platform  string
	PushedBy  *int64
	CreatedAt time.Time
	// Provenance: set when a CI job pushed the manifest.
	BuildJobID *int64
	BuildSHA   string
	BuildRef   string
}

func isFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// ---- blobs ----

func (s *Store) RegistryBlob(ctx context.Context, digest string) (*RegistryBlob, error) {
	b := RegistryBlob{Digest: digest}
	err := s.db.QueryRow(ctx, `SELECT size, s3_key FROM registry_blobs WHERE digest = $1`, digest).Scan(&b.Size, &b.S3Key)
	if err != nil {
		return nil, notFound(err)
	}
	return &b, nil
}

// TouchRegistryBlob marks a blob as recently used (at most once an hour), so
// a client that skipped uploading an existing layer can still reference it.
func (s *Store) TouchRegistryBlob(ctx context.Context, digest string) error {
	_, err := s.db.Exec(ctx, `UPDATE registry_blobs SET last_used_at = now()
		WHERE digest = $1 AND last_used_at < now() - interval '1 hour'`, digest)
	return err
}

// ---- uploads ----

func (s *Store) CreateRegistryUpload(ctx context.Context, u *RegistryUpload) error {
	return s.db.QueryRow(ctx,
		`INSERT INTO registry_uploads (id, repo, hash_state, user_id) VALUES ($1, $2, $3, $4) RETURNING created_at, updated_at`,
		u.ID, u.Repo, u.HashState, u.UserID).Scan(&u.CreatedAt, &u.UpdatedAt)
}

func (s *Store) RegistryUpload(ctx context.Context, id string) (*RegistryUpload, error) {
	u := RegistryUpload{ID: id}
	err := s.db.QueryRow(ctx,
		`SELECT repo, size, part_keys, hash_state, user_id, created_at, updated_at FROM registry_uploads WHERE id = $1`, id).
		Scan(&u.Repo, &u.Size, &u.PartKeys, &u.HashState, &u.UserID, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &u, nil
}

// AdvanceRegistryUpload records one more stored (non-empty) part. It is a
// compare-and-swap on size: it reports false when another request appended
// to the upload meanwhile.
func (s *Store) AdvanceRegistryUpload(ctx context.Context, u *RegistryUpload, key string, newSize int64, hashState []byte) (bool, error) {
	tag, err := s.db.Exec(ctx,
		`UPDATE registry_uploads SET size = $3, part_keys = array_append(part_keys, $4), hash_state = $5, updated_at = now()
		 WHERE id = $1 AND size = $2`, u.ID, u.Size, newSize, key, hashState)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	u.Size, u.HashState = newSize, hashState
	u.PartKeys = append(u.PartKeys, key)
	return true, nil
}

func (s *Store) DeleteRegistryUpload(ctx context.Context, id string) (bool, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM registry_uploads WHERE id = $1`, id)
	return tag.RowsAffected() > 0, err
}

// FinishRegistryUpload turns an upload into a blob. It fails with ErrNotFound
// when the upload is gone or has moved on. If the blob already exists the
// existing one is kept; the returned key is the one now in use, so the caller
// deletes its own object when it differs.
func (s *Store) FinishRegistryUpload(ctx context.Context, u *RegistryUpload, b *RegistryBlob) (string, error) {
	var key string
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM registry_uploads WHERE id = $1 AND size = $2`, u.ID, u.Size)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return tx.QueryRow(ctx,
			`INSERT INTO registry_blobs (digest, size, s3_key) VALUES ($1, $2, $3)
			 ON CONFLICT (digest) DO UPDATE SET last_used_at = now() RETURNING s3_key`,
			b.Digest, b.Size, b.S3Key).Scan(&key)
	})
	return key, err
}

// ---- manifests and tags ----

const manifestCols = `id, repo, digest, media_type, content, total_size, platform, pushed_by, created_at,
	build_job_id, build_sha, build_ref`

func scanManifest(row pgx.Row) (*RegistryManifest, error) {
	var m RegistryManifest
	err := row.Scan(&m.ID, &m.Repo, &m.Digest, &m.MediaType, &m.Content, &m.TotalSize, &m.Platform, &m.PushedBy, &m.CreatedAt,
		&m.BuildJobID, &m.BuildSHA, &m.BuildRef)
	if err != nil {
		return nil, notFound(err)
	}
	return &m, nil
}

func (s *Store) RegistryManifestByDigest(ctx context.Context, repo, digest string) (*RegistryManifest, error) {
	return scanManifest(s.db.QueryRow(ctx,
		`SELECT `+manifestCols+` FROM registry_manifests WHERE repo = $1 AND digest = $2`, repo, digest))
}

func (s *Store) RegistryManifestByTag(ctx context.Context, repo, tag string) (*RegistryManifest, error) {
	return scanManifest(s.db.QueryRow(ctx,
		`SELECT `+manifestCols+` FROM registry_manifests WHERE id =
		   (SELECT manifest_id FROM registry_tags WHERE repo = $1 AND name = $2)`, repo, tag))
}

// PutRegistryManifest stores a manifest (idempotent per repo+digest), links
// it to its blobs and child manifests, and points tag at it when tag != "".
// For an index, the children's sizes are added to m.TotalSize.
func (s *Store) PutRegistryManifest(ctx context.Context, m *RegistryManifest, blobs, children []string, tag string) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var missing []string
		rows, _ := tx.Query(ctx, `SELECT DISTINCT d FROM unnest($1::text[]) d
			WHERE NOT EXISTS (SELECT 1 FROM registry_blobs b WHERE b.digest = d)`, blobs)
		blobMissing, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		missing = append(missing, blobMissing...)

		var childIDs []int64
		if len(children) > 0 {
			rows, _ := tx.Query(ctx, `SELECT d, m.id, m.total_size FROM unnest($2::text[]) d
				LEFT JOIN registry_manifests m ON m.repo = $1 AND m.digest = d`, m.Repo, children)
			type child struct {
				Digest string
				ID     *int64
				Size   *int64
			}
			found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[child])
			if err != nil {
				return err
			}
			for _, c := range found {
				if c.ID == nil {
					missing = append(missing, c.Digest)
					continue
				}
				childIDs = append(childIDs, *c.ID)
				m.TotalSize += *c.Size
			}
		}
		if len(missing) > 0 {
			return &MissingRefsError{Digests: missing}
		}

		err = tx.QueryRow(ctx,
			`INSERT INTO registry_manifests (repo, digest, media_type, content, total_size, platform, pushed_by,
			 build_job_id, build_sha, build_ref)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT (repo, digest) DO NOTHING RETURNING id, created_at`,
			m.Repo, m.Digest, m.MediaType, m.Content, m.TotalSize, m.Platform, m.PushedBy, m.BuildJobID, m.BuildSHA, m.BuildRef).Scan(&m.ID, &m.CreatedAt)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// Re-push of a known manifest: its references are already in place.
			// Identical bytes built by CI get the provenance they lacked.
			err = tx.QueryRow(ctx, `UPDATE registry_manifests SET
				build_job_id = CASE WHEN build_sha = '' AND $3 <> '' THEN $4 ELSE build_job_id END,
				build_ref = CASE WHEN build_sha = '' AND $3 <> '' THEN $5 ELSE build_ref END,
				build_sha = CASE WHEN build_sha = '' THEN $3 ELSE build_sha END
				WHERE repo = $1 AND digest = $2 RETURNING id, created_at`,
				m.Repo, m.Digest, m.BuildSHA, m.BuildJobID, m.BuildRef).Scan(&m.ID, &m.CreatedAt)
			if err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			// The foreign keys catch blobs or children deleted concurrently.
			if _, err := tx.Exec(ctx, `INSERT INTO registry_manifest_blobs (manifest_id, digest)
				SELECT DISTINCT $1::bigint, unnest($2::text[]) ON CONFLICT DO NOTHING`, m.ID, blobs); err != nil {
				if isFKViolation(err) {
					return &MissingRefsError{Digests: blobs}
				}
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO registry_manifest_children (parent_id, child_id)
				SELECT DISTINCT $1::bigint, unnest($2::bigint[]) ON CONFLICT DO NOTHING`, m.ID, childIDs); err != nil {
				if isFKViolation(err) {
					return &MissingRefsError{Digests: children}
				}
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE registry_blobs SET last_used_at = now() WHERE digest = ANY($1)`, blobs); err != nil {
			return err
		}
		if tag == "" {
			return nil
		}
		_, err = tx.Exec(ctx, `INSERT INTO registry_tags (repo, name, manifest_id, pushed_by) VALUES ($1, $2, $3, $4)
			ON CONFLICT (repo, name) DO UPDATE SET manifest_id = EXCLUDED.manifest_id, pushed_by = EXCLUDED.pushed_by, updated_at = now()`,
			m.Repo, tag, m.ID, m.PushedBy)
		return err
	})
}

// DeleteRegistryManifest deletes a manifest and its tags, then prunes child
// manifests that are left untagged and unreferenced. It returns ErrReferenced
// when an index still points to the manifest.
func (s *Store) DeleteRegistryManifest(ctx context.Context, repo, digest string) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var id int64
		if err := tx.QueryRow(ctx, `SELECT id FROM registry_manifests WHERE repo = $1 AND digest = $2 FOR UPDATE`,
			repo, digest).Scan(&id); err != nil {
			return notFound(err)
		}
		var referenced bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM registry_manifest_children WHERE child_id = $1)`,
			id).Scan(&referenced); err != nil {
			return err
		}
		if referenced {
			return ErrReferenced
		}
		if _, err := tx.Exec(ctx, `DELETE FROM registry_tags WHERE manifest_id = $1`, id); err != nil {
			return err
		}
		return pruneManifest(ctx, tx, id)
	})
}

// DeleteRegistryTag removes a tag. With prune, the manifest it pointed to is
// deleted too once nothing else tags or references it ("delete version").
func (s *Store) DeleteRegistryTag(ctx context.Context, repo, tag string, prune bool) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var id int64
		if err := tx.QueryRow(ctx, `DELETE FROM registry_tags WHERE repo = $1 AND name = $2 RETURNING manifest_id`,
			repo, tag).Scan(&id); err != nil {
			return notFound(err)
		}
		if !prune {
			return nil
		}
		return pruneManifest(ctx, tx, id)
	})
}

// pruneManifest deletes a manifest if it has no tags and no parent index, then
// recurses into its children.
func pruneManifest(ctx context.Context, tx pgx.Tx, id int64) error {
	rows, _ := tx.Query(ctx, `SELECT child_id FROM registry_manifest_children WHERE parent_id = $1`, id)
	children, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM registry_manifests m WHERE id = $1
		AND NOT EXISTS (SELECT 1 FROM registry_tags t WHERE t.manifest_id = m.id)
		AND NOT EXISTS (SELECT 1 FROM registry_manifest_children c WHERE c.child_id = m.id)`, id)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	for _, c := range children {
		if err := pruneManifest(ctx, tx, c); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RegistryRepoExists(ctx context.Context, repo string) (bool, error) {
	var ok bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM registry_manifests WHERE repo = $1)`, repo).Scan(&ok)
	return ok, err
}

// RegistryTagNames lists tag names after `last` in lexical order.
func (s *Store) RegistryTagNames(ctx context.Context, repo, last string, n int) ([]string, error) {
	rows, _ := s.db.Query(ctx, `SELECT name FROM registry_tags WHERE repo = $1 AND name COLLATE "C" > $2 ORDER BY name COLLATE "C" LIMIT $3`,
		repo, last, n)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// RegistryCatalog lists repository names after `last` in lexical order.
func (s *Store) RegistryCatalog(ctx context.Context, last string, n int) ([]string, error) {
	rows, _ := s.db.Query(ctx, `SELECT DISTINCT repo COLLATE "C" FROM registry_manifests WHERE repo COLLATE "C" > $1 ORDER BY 1 LIMIT $2`,
		last, n)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ---- UI listings ----

type RegistryImage struct {
	Repo      string
	Tags      int
	Size      int64 // of the most recently pushed tag
	UpdatedAt time.Time
}

func (s *Store) ListRegistryImages(ctx context.Context) ([]RegistryImage, error) {
	rows, _ := s.db.Query(ctx, `
		SELECT m.repo, count(t.name)::int,
		       COALESCE((SELECT lm.total_size FROM registry_tags lt JOIN registry_manifests lm ON lm.id = lt.manifest_id
		                  WHERE lt.repo = m.repo ORDER BY lt.updated_at DESC LIMIT 1), 0),
		       GREATEST(max(m.created_at), COALESCE(max(t.updated_at), max(m.created_at)))
		FROM registry_manifests m LEFT JOIN registry_tags t ON t.manifest_id = m.id
		GROUP BY m.repo ORDER BY 4 DESC`)
	return pgx.CollectRows(rows, pgx.RowToStructByPos[RegistryImage])
}

// RegistryVersion is a tag, or an untagged top-level manifest (named by its
// digest) — the "package versions" of Gitea.
type RegistryVersion struct {
	Name      string
	Tagged    bool
	UpdatedAt time.Time
	PushedBy  string
	Manifest  RegistryManifest
}

type RegistryVersionFilter struct {
	Repo       string // exact image name
	RepoPrefix string // e.g. "owner/"
	RepoQuery  string // substring of the image name
	Query      string // substring of the version name
	Limit      int    // 0 = no limit
	Offset     int
}

// ListRegistryVersions returns matching versions (tagged first, newest first)
// and the total count.
func (s *Store) ListRegistryVersions(ctx context.Context, f RegistryVersionFilter) ([]RegistryVersion, int, error) {
	limit := any(nil)
	if f.Limit > 0 {
		limit = f.Limit
	}
	rows, err := s.db.Query(ctx, `
		WITH v AS (
			SELECT t.repo, t.name, true AS tagged, t.updated_at, t.pushed_by, t.manifest_id FROM registry_tags t
			UNION ALL
			SELECT m.repo, m.digest, false, m.created_at, m.pushed_by, m.id FROM registry_manifests m
			WHERE NOT EXISTS (SELECT 1 FROM registry_tags t WHERE t.manifest_id = m.id)
			  AND NOT EXISTS (SELECT 1 FROM registry_manifest_children c WHERE c.child_id = m.id)
		)
		SELECT v.name, v.tagged, v.updated_at, COALESCE(u.username, 'ghost'),
		       m.id, m.repo, m.digest, m.media_type, m.content, m.total_size, m.platform, m.pushed_by, m.created_at,
		       m.build_job_id, m.build_sha, m.build_ref, count(*) OVER ()
		FROM v JOIN registry_manifests m ON m.id = v.manifest_id LEFT JOIN users u ON u.id = v.pushed_by
		WHERE ($1 = '' OR v.repo = $1)
		  AND ($2 = '' OR starts_with(v.repo, $2))
		  AND ($3 = '' OR strpos(v.repo, lower($3)) > 0)
		  AND ($4 = '' OR strpos(lower(v.name), lower($4)) > 0)
		ORDER BY v.tagged DESC, v.updated_at DESC, v.repo, v.name
		LIMIT $5 OFFSET $6`, f.Repo, f.RepoPrefix, f.RepoQuery, f.Query, limit, f.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []RegistryVersion
	total := 0
	for rows.Next() {
		var v RegistryVersion
		m := &v.Manifest
		if err := rows.Scan(&v.Name, &v.Tagged, &v.UpdatedAt, &v.PushedBy,
			&m.ID, &m.Repo, &m.Digest, &m.MediaType, &m.Content, &m.TotalSize, &m.Platform, &m.PushedBy, &m.CreatedAt,
			&m.BuildJobID, &m.BuildSHA, &m.BuildRef, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(out) == 0 && f.Offset > 0 {
		// Past the last page: the window count is missing, fetch it.
		f.Limit, f.Offset = 1, 0
		_, n, err := s.ListRegistryVersions(ctx, f)
		return nil, n, err
	}
	return out, total, nil
}

// RegistryTagsFor lists the tags pointing to a manifest.
func (s *Store) RegistryTagsFor(ctx context.Context, manifestID int64) ([]string, error) {
	rows, _ := s.db.Query(ctx, `SELECT name FROM registry_tags WHERE manifest_id = $1 ORDER BY name`, manifestID)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// DeleteRegistryRepo deletes an image with all its tags and manifests.
func (s *Store) DeleteRegistryRepo(ctx context.Context, repo string) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		// Children are always in the same repository; unlink them first so the
		// RESTRICT key does not depend on the deletion order.
		if _, err := tx.Exec(ctx, `DELETE FROM registry_manifest_children WHERE parent_id IN
			(SELECT id FROM registry_manifests WHERE repo = $1)`, repo); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM registry_manifests WHERE repo = $1`, repo)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return err
	})
}

// RegistryTotalSize is the size of all stored blobs.
func (s *Store) RegistryTotalSize(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRow(ctx, `SELECT COALESCE(sum(size), 0)::bigint FROM registry_blobs`).Scan(&n)
	return n, err
}

// ---- cleanup rule ----

type RegistryCleanupRule struct {
	Enabled       bool
	KeepCount     int
	KeepPattern   string
	RemoveDays    int
	RemovePattern string
	MatchFullName bool
	LastRunAt     *time.Time
	LastRemoved   int
}

func (s *Store) RegistryCleanupRule(ctx context.Context) (*RegistryCleanupRule, error) {
	var r RegistryCleanupRule
	err := s.db.QueryRow(ctx, `SELECT enabled, keep_count, keep_pattern, remove_days, remove_pattern, match_full_name,
		last_run_at, last_removed FROM registry_cleanup_rule WHERE id = 1`).
		Scan(&r.Enabled, &r.KeepCount, &r.KeepPattern, &r.RemoveDays, &r.RemovePattern, &r.MatchFullName, &r.LastRunAt, &r.LastRemoved)
	if errors.Is(err, pgx.ErrNoRows) {
		return &r, nil
	}
	return &r, err
}

func (s *Store) SaveRegistryCleanupRule(ctx context.Context, r *RegistryCleanupRule) error {
	_, err := s.db.Exec(ctx, `INSERT INTO registry_cleanup_rule (id, enabled, keep_count, keep_pattern, remove_days, remove_pattern, match_full_name)
		VALUES (1, $1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET enabled = $1, keep_count = $2, keep_pattern = $3, remove_days = $4,
		  remove_pattern = $5, match_full_name = $6, updated_at = now()`,
		r.Enabled, r.KeepCount, r.KeepPattern, r.RemoveDays, r.RemovePattern, r.MatchFullName)
	return err
}

func (s *Store) MarkRegistryCleanupRun(ctx context.Context, removed int) error {
	_, err := s.db.Exec(ctx, `INSERT INTO registry_cleanup_rule (id, last_run_at, last_removed) VALUES (1, now(), $1)
		ON CONFLICT (id) DO UPDATE SET last_run_at = now(), last_removed = $1`, removed)
	return err
}

// ---- garbage collection ----

// DeleteStaleRegistryUploads removes uploads idle for longer than age and
// returns their ids so the caller can delete the stored parts.
func (s *Store) DeleteStaleRegistryUploads(ctx context.Context, age time.Duration) ([]string, error) {
	rows, _ := s.db.Query(ctx, `DELETE FROM registry_uploads WHERE updated_at < now() - $1::interval RETURNING id`, age)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// OrphanRegistryBlobs lists blobs no manifest references that were last used
// more than age ago.
func (s *Store) OrphanRegistryBlobs(ctx context.Context, age time.Duration) ([]string, error) {
	rows, _ := s.db.Query(ctx, `SELECT digest FROM registry_blobs b WHERE last_used_at < now() - $1::interval
		AND NOT EXISTS (SELECT 1 FROM registry_manifest_blobs r WHERE r.digest = b.digest)`, age)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// DeleteOrphanRegistryBlob deletes the blob row if it is still an orphan and
// returns its S3 key ("" when it was kept, e.g. a push referenced it
// meanwhile — the RESTRICT foreign key makes that race safe).
func (s *Store) DeleteOrphanRegistryBlob(ctx context.Context, digest string, age time.Duration) (string, error) {
	var key string
	err := s.db.QueryRow(ctx, `DELETE FROM registry_blobs b WHERE digest = $1 AND last_used_at < now() - $2::interval
		AND NOT EXISTS (SELECT 1 FROM registry_manifest_blobs r WHERE r.digest = b.digest) RETURNING s3_key`,
		digest, age).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) || isFKViolation(err) {
		return "", nil
	}
	return key, err
}

// TryAdvisoryLock runs fn while holding a session advisory lock; it reports
// false without running fn when another session holds the lock.
func (s *Store) TryAdvisoryLock(ctx context.Context, id int64, fn func() error) (bool, error) {
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, id).Scan(&ok); err != nil || !ok {
		return false, err
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, id) }()
	return true, fn()
}
