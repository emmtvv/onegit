package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Merge queue entry states.
const (
	QueueQueued  = "queued"  // waiting for its turn to be tested
	QueueTesting = "testing" // TestSHA is being checked
	QueueMerged  = "merged"
	QueueFailed  = "failed"
	QueueRemoved = "removed"
)

type QueueEntry struct {
	ID           int64
	PullID       int64
	BaseBranch   string
	State        string
	Style        string
	Title        string
	Message      string
	DeleteBranch bool
	EnqueuedBy   *int64
	HeadSHA      string // the PR head when it was queued
	BaseSHA      string // what TestSHA was built on
	TestSHA      string // base + this PR (and the ones before it)
	Reason       string // why it failed or was removed
	CreatedAt    time.Time
	UpdatedAt    time.Time
	FinishedAt   *time.Time

	// Joined.
	PullTitle      string
	EnqueuedByName string
}

func (e *QueueEntry) Active() bool { return e.State == QueueQueued || e.State == QueueTesting }

const queueCols = `q.id, q.pull_id, q.base_branch, q.state, q.style, q.title, q.message, q.delete_branch, q.enqueued_by,
	q.head_sha, q.base_sha, q.test_sha, q.reason, q.created_at, q.updated_at, q.finished_at, p.title, COALESCE(u.username, 'ghost')`
const queueFrom = ` FROM merge_queue q JOIN pulls p ON p.id = q.pull_id LEFT JOIN users u ON u.id = q.enqueued_by `

func collectQueue(rows pgx.Rows) ([]*QueueEntry, error) {
	return pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[QueueEntry])
}

// Enqueue adds a PR to its base branch's queue; ErrDuplicate means it is
// already queued.
func (s *Store) Enqueue(ctx context.Context, e *QueueEntry) error {
	err := s.db.QueryRow(ctx, `INSERT INTO merge_queue (pull_id, base_branch, state, style, title, message, delete_branch,
		enqueued_by, head_sha) VALUES ($1, $2, 'queued', $3, $4, $5, $6, $7, $8) RETURNING id, state, created_at, updated_at`,
		e.PullID, e.BaseBranch, e.Style, e.Title, e.Message, e.DeleteBranch, e.EnqueuedBy, e.HeadSHA).
		Scan(&e.ID, &e.State, &e.CreatedAt, &e.UpdatedAt)
	if isUniqueViolation(err) {
		return ErrDuplicate
	}
	return err
}

// ActiveQueue lists a branch's queued and testing entries in queue order.
func (s *Store) ActiveQueue(ctx context.Context, base string) ([]*QueueEntry, error) {
	rows, _ := s.db.Query(ctx, `SELECT `+queueCols+queueFrom+`WHERE q.base_branch = $1 AND q.state IN ('queued', 'testing')
		ORDER BY q.id`, base)
	return collectQueue(rows)
}

// QueueBranches lists the branches with active queue entries.
func (s *Store) QueueBranches(ctx context.Context) ([]string, error) {
	rows, _ := s.db.Query(ctx, `SELECT DISTINCT base_branch FROM merge_queue WHERE state IN ('queued', 'testing') ORDER BY 1`)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ActiveQueueEntry returns the PR's active entry.
func (s *Store) ActiveQueueEntry(ctx context.Context, pullID int64) (*QueueEntry, error) {
	rows, _ := s.db.Query(ctx, `SELECT `+queueCols+queueFrom+`WHERE q.pull_id = $1 AND q.state IN ('queued', 'testing')`, pullID)
	e, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByPos[QueueEntry])
	return e, notFound(err)
}

// RecentQueueEntries lists finished entries, newest first.
func (s *Store) RecentQueueEntries(ctx context.Context, limit int) ([]*QueueEntry, error) {
	rows, _ := s.db.Query(ctx, `SELECT `+queueCols+queueFrom+`WHERE q.finished_at IS NOT NULL ORDER BY q.finished_at DESC LIMIT $1`, limit)
	return collectQueue(rows)
}

// StartQueueTest records a candidate built for an active entry.
func (s *Store) StartQueueTest(ctx context.Context, id int64, baseSHA, testSHA string) error {
	_, err := s.db.Exec(ctx, `UPDATE merge_queue SET state = 'testing', base_sha = $2, test_sha = $3, updated_at = now()
		WHERE id = $1 AND state IN ('queued', 'testing')`, id, baseSHA, testSHA)
	return err
}

// ResetQueueEntry puts an entry back to waiting (e.g. beyond the depth after
// the queue was reordered).
func (s *Store) ResetQueueEntry(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `UPDATE merge_queue SET state = 'queued', base_sha = '', test_sha = '', updated_at = now()
		WHERE id = $1 AND state IN ('queued', 'testing')`, id)
	return err
}

// FinishQueueEntry ends an active entry; it returns false when the entry
// was no longer active.
func (s *Store) FinishQueueEntry(ctx context.Context, id int64, state, reason string) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE merge_queue SET state = $2, reason = $3, updated_at = now(), finished_at = now()
		WHERE id = $1 AND state IN ('queued', 'testing')`, id, state, reason)
	return tag.RowsAffected() == 1, err
}
