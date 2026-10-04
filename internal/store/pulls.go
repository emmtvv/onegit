package store

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type PullState string

const (
	PullOpen   PullState = "open"
	PullClosed PullState = "closed"
	PullMerged PullState = "merged"
)

type Pull struct {
	ID         int64
	Title      string
	Body       string
	AuthorID   *int64
	HeadBranch string
	BaseBranch string
	HeadSHA    string
	MergeBase  string
	State      PullState
	MergeSHA   *string
	MergeStyle *string
	MergedBy   *int64
	MergedAt   *time.Time
	ClosedAt   *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time

	// ChangedFiles as of FilesSHA (the head they were computed for).
	ChangedFiles []string
	FilesSHA     string
	// Auto-merge: set while enabled; the PR merges (or joins the merge
	// queue) as AutoMergeBy once every requirement is met.
	AutoMergeBy           *int64
	AutoMergeStyle        string
	AutoMergeTitle        string
	AutoMergeMessage      string
	AutoMergeDeleteBranch bool

	// Joined from users; "ghost" when the user was deleted.
	AuthorName      string
	AuthorEmail     string
	MergedByName    string
	AutoMergeByName string
}

func (p *Pull) IsOpen() bool    { return p.State == PullOpen }
func (p *Pull) AutoMerge() bool { return p.AutoMergeBy != nil }
func (p *Pull) IsMerged() bool  { return p.State == PullMerged }
func (p *Pull) IsAuthor(u *User) bool {
	return u != nil && p.AuthorID != nil && *p.AuthorID == u.ID
}

const pullCols = `p.id, p.title, p.body, p.author_id, p.head_branch, p.base_branch, p.head_sha, p.merge_base, p.state,
	p.merge_sha, p.merge_style, p.merged_by, p.merged_at, p.closed_at, p.created_at, p.updated_at,
	p.changed_files, p.files_sha, p.auto_merge_by, p.auto_merge_style, p.auto_merge_title, p.auto_merge_message,
	p.auto_merge_delete_branch,
	COALESCE(a.username, 'ghost'), COALESCE(a.email, ''), COALESCE(m.username, 'ghost'), COALESCE(am.username, 'ghost')`

const pullFrom = ` FROM pulls p LEFT JOIN users a ON a.id = p.author_id LEFT JOIN users m ON m.id = p.merged_by
	LEFT JOIN users am ON am.id = p.auto_merge_by `

func scanPull(row pgx.Row) (*Pull, error) {
	var p Pull
	err := row.Scan(&p.ID, &p.Title, &p.Body, &p.AuthorID, &p.HeadBranch, &p.BaseBranch, &p.HeadSHA, &p.MergeBase, &p.State,
		&p.MergeSHA, &p.MergeStyle, &p.MergedBy, &p.MergedAt, &p.ClosedAt, &p.CreatedAt, &p.UpdatedAt,
		&p.ChangedFiles, &p.FilesSHA, &p.AutoMergeBy, &p.AutoMergeStyle, &p.AutoMergeTitle, &p.AutoMergeMessage,
		&p.AutoMergeDeleteBranch,
		&p.AuthorName, &p.AuthorEmail, &p.MergedByName, &p.AutoMergeByName)
	if err != nil {
		return nil, notFound(err)
	}
	return &p, nil
}

func collectPulls(rows pgx.Rows, err error) ([]*Pull, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Pull
	for rows.Next() {
		p, err := scanPull(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CreatePull inserts p; ErrDuplicate means an open PR for the same
// head/base pair already exists.
func (s *Store) CreatePull(ctx context.Context, p *Pull) error {
	err := s.db.QueryRow(ctx,
		`INSERT INTO pulls (title, body, author_id, head_branch, base_branch, head_sha, merge_base)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, state, created_at, updated_at`,
		p.Title, p.Body, p.AuthorID, p.HeadBranch, p.BaseBranch, p.HeadSHA, p.MergeBase,
	).Scan(&p.ID, &p.State, &p.CreatedAt, &p.UpdatedAt)
	if isUniqueViolation(err) {
		return ErrDuplicate
	}
	return err
}

func (s *Store) PullByID(ctx context.Context, id int64) (*Pull, error) {
	return scanPull(s.db.QueryRow(ctx, `SELECT `+pullCols+pullFrom+`WHERE p.id = $1`, id))
}

// ListPulls lists PRs by state ("open", or "closed" meaning closed+merged).
func (s *Store) ListPulls(ctx context.Context, state string, limit, offset int) ([]*Pull, error) {
	return s.ListPullsIn(ctx, state, "", limit, offset)
}

// touchesDir matches PRs that change a file under dir (a "dir/" prefix);
// $1 is the prefix, ” matching every PR.
const touchesDir = `($1 = '' OR EXISTS (SELECT 1 FROM unnest(p.changed_files) f WHERE starts_with(f, $1)))`

// ListPullsIn lists PRs by state that change files under dir ("" = all).
func (s *Store) ListPullsIn(ctx context.Context, state, dir string, limit, offset int) ([]*Pull, error) {
	cond := `p.state = 'open'`
	if state == "closed" {
		cond = `p.state <> 'open'`
	}
	return collectPulls(s.db.Query(ctx,
		`SELECT `+pullCols+pullFrom+`WHERE `+cond+` AND `+touchesDir+` ORDER BY p.id DESC LIMIT $2 OFFSET $3`, dirPrefix(dir), limit, offset))
}

func (s *Store) CountPulls(ctx context.Context) (open, closed int, err error) {
	return s.CountPullsIn(ctx, "")
}

func (s *Store) CountPullsIn(ctx context.Context, dir string) (open, closed int, err error) {
	err = s.db.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE state = 'open'), count(*) FILTER (WHERE state <> 'open') FROM pulls p WHERE `+touchesDir,
		dirPrefix(dir)).Scan(&open, &closed)
	return
}

func dirPrefix(dir string) string {
	if dir == "" {
		return ""
	}
	return strings.TrimSuffix(dir, "/") + "/"
}

// SetPullFiles stores the changed files computed for head sha.
func (s *Store) SetPullFiles(ctx context.Context, id int64, files []string, sha string) error {
	if files == nil {
		files = []string{}
	}
	_, err := s.db.Exec(ctx, `UPDATE pulls SET changed_files = $2, files_sha = $3 WHERE id = $1`, id, files, sha)
	return err
}

// PullsNeedingFiles lists PRs whose changed files are not computed for
// their current head, open ones first.
func (s *Store) PullsNeedingFiles(ctx context.Context, limit int) ([]*Pull, error) {
	return collectPulls(s.db.Query(ctx, `SELECT `+pullCols+pullFrom+`WHERE p.files_sha <> p.head_sha
		ORDER BY p.state = 'open' DESC, p.id DESC LIMIT $1`, limit))
}

// AutoMergePulls lists open PRs with auto-merge enabled.
func (s *Store) AutoMergePulls(ctx context.Context) ([]*Pull, error) {
	return collectPulls(s.db.Query(ctx, `SELECT `+pullCols+pullFrom+`WHERE p.state = 'open' AND p.auto_merge_by IS NOT NULL ORDER BY p.id`))
}

// OpenPullsForBranch returns open PRs whose head or base is branch.
func (s *Store) OpenPullsForBranch(ctx context.Context, branch string) ([]*Pull, error) {
	return collectPulls(s.db.Query(ctx,
		`SELECT `+pullCols+pullFrom+`WHERE p.state = 'open' AND (p.head_branch = $1 OR p.base_branch = $1) ORDER BY p.id`, branch))
}

// OpenPullsByHead maps head branch name to its open PRs.
func (s *Store) OpenPullsByHead(ctx context.Context) (map[string][]*Pull, error) {
	list, err := collectPulls(s.db.Query(ctx, `SELECT `+pullCols+pullFrom+`WHERE p.state = 'open' ORDER BY p.id`))
	if err != nil {
		return nil, err
	}
	m := make(map[string][]*Pull)
	for _, p := range list {
		m[p.HeadBranch] = append(m[p.HeadBranch], p)
	}
	return m, nil
}

func (s *Store) OpenPullFor(ctx context.Context, head, base string) (*Pull, error) {
	return scanPull(s.db.QueryRow(ctx,
		`SELECT `+pullCols+pullFrom+`WHERE p.state = 'open' AND p.head_branch = $1 AND p.base_branch = $2`, head, base))
}

// UpdatePullLocked loads the PR with a row lock, lets fn mutate it and
// writes back the mutable fields, all in one transaction. fn returning an
// error aborts without changes. This serialises merges, closes and pushes
// touching the same PR across replicas.
func (s *Store) UpdatePullLocked(ctx context.Context, id int64, fn func(p *Pull) error) (*Pull, error) {
	var out *Pull
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM pulls WHERE id = $1 FOR UPDATE`, id); err != nil {
			return err
		}
		p, err := scanPull(tx.QueryRow(ctx, `SELECT `+pullCols+pullFrom+`WHERE p.id = $1`, id))
		if err != nil {
			return err
		}
		if err := fn(p); err != nil {
			return err
		}
		if !p.IsOpen() {
			p.AutoMergeBy = nil // auto-merge ends with the PR
		}
		err = tx.QueryRow(ctx,
			`UPDATE pulls SET title=$2, body=$3, head_sha=$4, merge_base=$5, state=$6, merge_sha=$7, merge_style=$8,
			 merged_by=$9, merged_at=$10, closed_at=$11, auto_merge_by=$12, auto_merge_style=$13, auto_merge_title=$14,
			 auto_merge_message=$15, auto_merge_delete_branch=$16, updated_at=now() WHERE id=$1 RETURNING updated_at`,
			p.ID, p.Title, p.Body, p.HeadSHA, p.MergeBase, p.State, p.MergeSHA, p.MergeStyle,
			p.MergedBy, p.MergedAt, p.ClosedAt, p.AutoMergeBy, p.AutoMergeStyle, p.AutoMergeTitle,
			p.AutoMergeMessage, p.AutoMergeDeleteBranch).Scan(&p.UpdatedAt)
		if isUniqueViolation(err) {
			return ErrDuplicate // reopening while another open PR has the same branches
		}
		out = p
		return err
	})
	return out, err
}

// ---- timeline: events, comments, reviews ----

type PullEvent struct {
	ID        int64
	PullID    int64
	ActorName string
	Kind      string
	Data      map[string]any
	CreatedAt time.Time
}

func (s *Store) AddPullEvent(ctx context.Context, pullID int64, actorID *int64, kind string, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO pull_events (pull_id, actor_id, kind, data) VALUES ($1, $2, $3, $4)`, pullID, actorID, kind, b)
	return err
}

func (s *Store) ListPullEvents(ctx context.Context, pullID int64) ([]*PullEvent, error) {
	rows, err := s.db.Query(ctx,
		`SELECT e.id, e.pull_id, COALESCE(u.username, 'ghost'), e.kind, e.data, e.created_at
		 FROM pull_events e LEFT JOIN users u ON u.id = e.actor_id WHERE e.pull_id = $1 ORDER BY e.id`, pullID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PullEvent
	for rows.Next() {
		var e PullEvent
		var raw []byte
		if err := rows.Scan(&e.ID, &e.PullID, &e.ActorName, &e.Kind, &raw, &e.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &e.Data) // written by AddPullEvent, always an object
		out = append(out, &e)
	}
	return out, rows.Err()
}

type PullComment struct {
	ID          int64
	PullID      int64
	AuthorID    *int64
	AuthorName  string
	AuthorEmail string
	Body        string
	Path        *string
	Side        *string
	Line        *int
	CommitSHA   *string
	DiffHunk    string
	CreatedAt   time.Time
}

func (c *PullComment) IsLine() bool { return c.Path != nil }

// CanDelete: authors may delete their own comments, admins any comment.
func (c *PullComment) CanDelete(u *User) bool {
	return u != nil && (u.IsAdmin() || c.AuthorID != nil && *c.AuthorID == u.ID)
}

func (s *Store) AddPullComment(ctx context.Context, c *PullComment) error {
	return s.db.QueryRow(ctx,
		`INSERT INTO pull_comments (pull_id, author_id, body, path, side, line, commit_sha, diff_hunk)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id, created_at`,
		c.PullID, c.AuthorID, c.Body, c.Path, c.Side, c.Line, c.CommitSHA, c.DiffHunk).Scan(&c.ID, &c.CreatedAt)
}

const commentCols = `c.id, c.pull_id, c.author_id, COALESCE(u.username, 'ghost'), COALESCE(u.email, ''), c.body,
	c.path, c.side, c.line, c.commit_sha, c.diff_hunk, c.created_at`

func scanComment(row pgx.Row) (*PullComment, error) {
	var c PullComment
	err := row.Scan(&c.ID, &c.PullID, &c.AuthorID, &c.AuthorName, &c.AuthorEmail, &c.Body,
		&c.Path, &c.Side, &c.Line, &c.CommitSHA, &c.DiffHunk, &c.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &c, nil
}

func (s *Store) ListPullComments(ctx context.Context, pullID int64) ([]*PullComment, error) {
	rows, err := s.db.Query(ctx,
		`SELECT `+commentCols+` FROM pull_comments c LEFT JOIN users u ON u.id = c.author_id WHERE c.pull_id = $1 ORDER BY c.id`, pullID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PullComment
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) PullCommentByID(ctx context.Context, pullID, id int64) (*PullComment, error) {
	return scanComment(s.db.QueryRow(ctx,
		`SELECT `+commentCols+` FROM pull_comments c LEFT JOIN users u ON u.id = c.author_id WHERE c.pull_id = $1 AND c.id = $2`, pullID, id))
}

func (s *Store) DeletePullComment(ctx context.Context, pullID, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM pull_comments WHERE pull_id = $1 AND id = $2`, pullID, id)
	return err
}

type ReviewState string

const (
	ReviewApproved         ReviewState = "approved"
	ReviewChangesRequested ReviewState = "changes_requested"
	ReviewCommented        ReviewState = "commented"
)

type PullReview struct {
	ID            int64
	PullID        int64
	ReviewerID    *int64
	ReviewerName  string
	ReviewerEmail string
	State         ReviewState
	Body          string
	CommitSHA     string
	CreatedAt     time.Time
}

func (s *Store) AddPullReview(ctx context.Context, r *PullReview) error {
	return s.db.QueryRow(ctx,
		`INSERT INTO pull_reviews (pull_id, reviewer_id, state, body, commit_sha) VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at`,
		r.PullID, r.ReviewerID, r.State, r.Body, r.CommitSHA).Scan(&r.ID, &r.CreatedAt)
}

func (s *Store) ListPullReviews(ctx context.Context, pullID int64) ([]*PullReview, error) {
	rows, err := s.db.Query(ctx,
		`SELECT r.id, r.pull_id, r.reviewer_id, COALESCE(u.username, 'ghost'), COALESCE(u.email, ''), r.state, r.body, r.commit_sha, r.created_at
		 FROM pull_reviews r LEFT JOIN users u ON u.id = r.reviewer_id WHERE r.pull_id = $1 ORDER BY r.id`, pullID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PullReview
	for rows.Next() {
		var r PullReview
		if err := rows.Scan(&r.ID, &r.PullID, &r.ReviewerID, &r.ReviewerName, &r.ReviewerEmail, &r.State, &r.Body, &r.CommitSHA, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// ---- branch protection ----

type BranchProtection struct {
	ID                      int64
	Pattern                 string
	RequirePullRequest      bool
	RequiredApprovals       int
	DismissStaleApprovals   bool
	RequireCodeOwnerReview  bool
	BlockOnChangesRequested bool
	AllowForcePush          bool
	AllowDeletion           bool
	RequiredChecks          []string // commit status contexts that must pass before merging
	Owners                  string   // server-side CODEOWNERS rules, always enforced
	// RequireMergeQueue: PRs land only through the merge queue, which tests
	// up to MergeQueueDepth of them at once, each on top of the previous.
	RequireMergeQueue bool
	MergeQueueDepth   int
	CreatedAt         time.Time
}

const protCols = `id, pattern, require_pull_request, required_approvals, dismiss_stale_approvals, require_code_owner_review,
	block_on_changes_requested, allow_force_push, allow_deletion, required_checks, owners, require_merge_queue,
	merge_queue_depth, created_at`

func (s *Store) ListBranchProtections(ctx context.Context) ([]*BranchProtection, error) {
	rows, err := s.db.Query(ctx, `SELECT `+protCols+` FROM branch_protections ORDER BY pattern`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*BranchProtection
	for rows.Next() {
		var b BranchProtection
		if err := rows.Scan(&b.ID, &b.Pattern, &b.RequirePullRequest, &b.RequiredApprovals, &b.DismissStaleApprovals,
			&b.RequireCodeOwnerReview, &b.BlockOnChangesRequested, &b.AllowForcePush, &b.AllowDeletion, &b.RequiredChecks,
			&b.Owners, &b.RequireMergeQueue, &b.MergeQueueDepth, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

// SaveBranchProtection creates (ID == 0) or updates a rule.
func (s *Store) SaveBranchProtection(ctx context.Context, b *BranchProtection) error {
	var err error
	if b.RequiredChecks == nil {
		b.RequiredChecks = []string{}
	}
	if b.MergeQueueDepth <= 0 {
		b.MergeQueueDepth = 5
	}
	if b.ID == 0 {
		err = s.db.QueryRow(ctx,
			`INSERT INTO branch_protections (pattern, require_pull_request, required_approvals, dismiss_stale_approvals,
			 require_code_owner_review, block_on_changes_requested, allow_force_push, allow_deletion, required_checks, owners,
			 require_merge_queue, merge_queue_depth)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING id, created_at`,
			b.Pattern, b.RequirePullRequest, b.RequiredApprovals, b.DismissStaleApprovals, b.RequireCodeOwnerReview,
			b.BlockOnChangesRequested, b.AllowForcePush, b.AllowDeletion, b.RequiredChecks, b.Owners,
			b.RequireMergeQueue, b.MergeQueueDepth).Scan(&b.ID, &b.CreatedAt)
	} else {
		_, err = s.db.Exec(ctx,
			`UPDATE branch_protections SET pattern=$2, require_pull_request=$3, required_approvals=$4, dismiss_stale_approvals=$5,
			 require_code_owner_review=$6, block_on_changes_requested=$7, allow_force_push=$8, allow_deletion=$9,
			 required_checks=$10, owners=$11, require_merge_queue=$12, merge_queue_depth=$13 WHERE id=$1`,
			b.ID, b.Pattern, b.RequirePullRequest, b.RequiredApprovals, b.DismissStaleApprovals, b.RequireCodeOwnerReview,
			b.BlockOnChangesRequested, b.AllowForcePush, b.AllowDeletion, b.RequiredChecks, b.Owners,
			b.RequireMergeQueue, b.MergeQueueDepth)
	}
	if isUniqueViolation(err) {
		return ErrDuplicate
	}
	return err
}

func (s *Store) DeleteBranchProtection(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM branch_protections WHERE id = $1`, id)
	return err
}

// BranchProtectionFor returns the rule for a branch, or nil: an exact
// pattern wins over globs, and among globs the longest pattern wins.
func (s *Store) BranchProtectionFor(ctx context.Context, branch string) (*BranchProtection, error) {
	rules, err := s.ListBranchProtections(ctx)
	if err != nil {
		return nil, err
	}
	return MatchBranchProtection(rules, branch), nil
}

func MatchBranchProtection(rules []*BranchProtection, branch string) *BranchProtection {
	var best *BranchProtection
	for _, r := range rules {
		if r.Pattern == branch {
			return r
		}
		if ok, _ := path.Match(r.Pattern, branch); ok && (best == nil || len(r.Pattern) > len(best.Pattern)) {
			best = r
		}
	}
	return best
}

// UsersByLogin resolves usernames (case-insensitive) and emails to users.
func (s *Store) UsersByLogin(ctx context.Context, names, emails []string) ([]*User, error) {
	rows, err := s.db.Query(ctx,
		`SELECT `+userCols+` FROM users WHERE lower(username) = ANY($1) OR (email <> '' AND lower(email) = ANY($2))`, names, emails)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
