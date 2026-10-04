package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// PullOwner is one owner of a group of a PR's changed files.
type PullOwner struct {
	Pattern string // the CODEOWNERS pattern
	Key     string // user:<name>, team:<name> or email:<address>, lower-cased
}

// PullFiles is an open PR's changed files, for computing its owners.
type PullFiles struct {
	ID    int64
	Files []string
}

// OpenPullBases lists the base branches of open PRs.
func (s *Store) OpenPullBases(ctx context.Context) ([]string, error) {
	rows, _ := s.db.Query(ctx, `SELECT DISTINCT base_branch FROM pulls WHERE state = 'open'`)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// PullsNeedingOwners lists open PRs into base whose changed files are
// computed but whose owners are not: never (all = false), or for rules
// other than rev (all = true).
func (s *Store) PullsNeedingOwners(ctx context.Context, base, rev string, all bool, limit int) ([]*PullFiles, error) {
	cond, arg := `owners_rev = ''`, any(nil)
	if all {
		cond, arg = `owners_rev <> $3`, rev
	}
	args := []any{base, limit}
	if arg != nil {
		args = append(args, arg)
	}
	rows, _ := s.db.Query(ctx, `SELECT id, changed_files FROM pulls WHERE state = 'open' AND base_branch = $1 AND `+cond+`
		AND files_sha = head_sha ORDER BY id LIMIT $2`, args...)
	return pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[PullFiles])
}

// SetPullOwners replaces a PR's owners, computed with the rules rev.
func (s *Store) SetPullOwners(ctx context.Context, id int64, rev string, owners []PullOwner) error {
	patterns := make([]string, len(owners))
	keys := make([]string, len(owners))
	for i, o := range owners {
		patterns[i], keys[i] = o.Pattern, o.Key
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		// Only while still open: a PR closed meanwhile keeps no owners.
		tag, err := tx.Exec(ctx, `UPDATE pulls SET owners_rev = $2 WHERE id = $1 AND state = 'open'`, id, rev)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM pull_owners WHERE pull_id = $1`, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO pull_owners (pull_id, pattern, owner_key)
			SELECT $1, p, k FROM unnest($2::text[], $3::text[]) u(p, k) ON CONFLICT DO NOTHING`, id, patterns, keys)
		return err
	})
}

// OwnedPull is an open PR with the first pattern that names one of the
// keys it was found by.
type OwnedPull struct {
	Pull    *Pull
	Pattern string
}

// OwnedPullsAwaiting lists open PRs, newest first, that one of keys owns
// and that userID neither opened nor gave a verdict on.
func (s *Store) OwnedPullsAwaiting(ctx context.Context, userID int64, keys []string, limit int) ([]OwnedPull, error) {
	rows, err := s.db.Query(ctx, `SELECT `+pullCols+`, o.pattern FROM (
			SELECT DISTINCT ON (pull_id) pull_id, pattern FROM pull_owners WHERE owner_key = ANY($2) ORDER BY pull_id DESC, pattern
		) o JOIN pulls p ON p.id = o.pull_id`+pullUsers+`
		WHERE p.state = 'open' AND p.author_id IS DISTINCT FROM $1
		AND NOT EXISTS (SELECT 1 FROM pull_reviews r WHERE r.pull_id = p.id AND r.reviewer_id = $1 AND r.state <> 'commented')
		ORDER BY p.id DESC LIMIT $3`, userID, keys, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OwnedPull
	for rows.Next() {
		var o OwnedPull
		p, err := scanPullWith(rows, &o.Pattern)
		if err != nil {
			return nil, err
		}
		o.Pull = p
		out = append(out, o)
	}
	return out, rows.Err()
}

// ReviewedPull is an open PR with a user's latest verdict on it.
type ReviewedPull struct {
	Pull      *Pull
	State     ReviewState
	CommitSHA string
}

// ReviewedPullsChanged lists open PRs, newest first, whose head moved since
// userID's latest verdict.
func (s *Store) ReviewedPullsChanged(ctx context.Context, userID int64, limit int) ([]ReviewedPull, error) {
	rows, err := s.db.Query(ctx, `SELECT `+pullCols+`, v.state, v.commit_sha FROM (
			SELECT DISTINCT ON (pull_id) pull_id, state, commit_sha FROM pull_reviews
			WHERE reviewer_id = $1 AND state <> 'commented' ORDER BY pull_id DESC, id DESC
		) v JOIN pulls p ON p.id = v.pull_id`+pullUsers+`
		WHERE p.state = 'open' AND v.commit_sha <> p.head_sha ORDER BY p.id DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewedPull
	for rows.Next() {
		var r ReviewedPull
		p, err := scanPullWith(rows, &r.State, &r.CommitSHA)
		if err != nil {
			return nil, err
		}
		r.Pull = p
		out = append(out, r)
	}
	return out, rows.Err()
}

// PullOwnerKeys returns the owner keys of some PRs by PR and pattern.
func (s *Store) PullOwnerKeys(ctx context.Context, ids []int64) (map[int64]map[string][]string, error) {
	rows, err := s.db.Query(ctx, `SELECT pull_id, pattern, owner_key FROM pull_owners WHERE pull_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[string][]string{}
	for rows.Next() {
		var id int64
		var pattern, key string
		if err := rows.Scan(&id, &pattern, &key); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string][]string{}
		}
		out[id][pattern] = append(out[id][pattern], key)
	}
	return out, rows.Err()
}

// WriterOwnerKeys returns the owner keys (user, email and team) of the
// active users with write access among ids: whose approvals count.
func (s *Store) WriterOwnerKeys(ctx context.Context, ids []int64) (map[int64][]string, error) {
	rows, err := s.db.Query(ctx, `SELECT u.id, 'user:' || lower(u.username) FROM users u
			WHERE u.id = ANY($1) AND u.active AND u.role IN ('write', 'admin')
		UNION ALL SELECT u.id, 'email:' || lower(u.email) FROM users u
			WHERE u.id = ANY($1) AND u.active AND u.role IN ('write', 'admin') AND u.email <> ''
		UNION ALL SELECT u.id, 'team:' || lower(t.name) FROM users u JOIN team_members m ON m.user_id = u.id JOIN teams t ON t.id = m.team_id
			WHERE u.id = ANY($1) AND u.active AND u.role IN ('write', 'admin')`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, err
		}
		out[id] = append(out[id], key)
	}
	return out, rows.Err()
}
