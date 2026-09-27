package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Selector restricts a rule, secret or runner to targets: dimension → globs.
// A dimension that is absent matches every value.
type Selector map[string][]string

// Principal is who a grant or an approval requirement refers to:
// user:<name>, team:<name>, role:<read|write|admin>, or codeowners (the
// CODEOWNERS owners of the target's path on the default branch).
type Principal struct {
	Type  string `json:"type"`
	Value string `json:"value,omitempty"`
}

// ---- dimensions ----

type DeployDimension struct {
	Name        string
	Position    int
	Source      string // "list" or "paths"
	Values      []string
	PathPattern string
}

func (s *Store) ListDeployDimensions(ctx context.Context) ([]*DeployDimension, error) {
	rows, _ := s.db.Query(ctx, `SELECT name, position, source, value_list, path_pattern FROM deploy_dimensions ORDER BY position, name`)
	return pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[DeployDimension])
}

func (s *Store) SaveDeployDimension(ctx context.Context, d *DeployDimension) error {
	if d.Values == nil {
		d.Values = []string{}
	}
	_, err := s.db.Exec(ctx, `INSERT INTO deploy_dimensions (name, position, source, value_list, path_pattern) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (name) DO UPDATE SET position = $2, source = $3, value_list = $4, path_pattern = $5`,
		d.Name, d.Position, d.Source, d.Values, d.PathPattern)
	return err
}

func (s *Store) DeleteDeployDimension(ctx context.Context, name string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM deploy_dimensions WHERE name = $1`, name)
	return err
}

// ---- rules ----

type DeployRule struct {
	ID                  int64
	Kind                string // "grant" or "require"
	Description         string
	Selector            Selector
	Principals          []Principal // grant
	Branches            []string    // require: commit must be on one of these branches (globs)
	RequireChecks       bool
	RequireCIArtifact   bool
	Approvals           int
	Approvers           []Principal
	AllowAuthorApproval bool
	Freeze              bool
	FreezeMessage       string
	UpdatedAt           time.Time
	// ExceptBranches: the requirement does not apply to commits already on
	// one of these branches (globs).
	ExceptBranches []string
}

const ruleCols = `id, kind, description, selector, principals, branches, require_checks, require_ci_artifact, approvals,
	approvers, allow_author_approval, frozen, freeze_message, updated_at, except_branches`

func (s *Store) ListDeployRules(ctx context.Context) ([]*DeployRule, error) {
	rows, _ := s.db.Query(ctx, `SELECT `+ruleCols+` FROM deploy_rules ORDER BY kind, id`)
	return pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[DeployRule])
}

func (s *Store) DeployRuleByID(ctx context.Context, id int64) (*DeployRule, error) {
	rows, _ := s.db.Query(ctx, `SELECT `+ruleCols+` FROM deploy_rules WHERE id = $1`, id)
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByPos[DeployRule])
	return r, notFound(err)
}

func (s *Store) SaveDeployRule(ctx context.Context, r *DeployRule) error {
	if r.Selector == nil {
		r.Selector = Selector{}
	}
	if r.Principals == nil {
		r.Principals = []Principal{}
	}
	if r.Approvers == nil {
		r.Approvers = []Principal{}
	}
	if r.Branches == nil {
		r.Branches = []string{}
	}
	if r.ExceptBranches == nil {
		r.ExceptBranches = []string{}
	}
	if r.ID == 0 {
		return s.db.QueryRow(ctx, `INSERT INTO deploy_rules (kind, description, selector, principals, branches, require_checks,
			require_ci_artifact, approvals, approvers, allow_author_approval, frozen, freeze_message, except_branches)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id, updated_at`,
			r.Kind, r.Description, r.Selector, r.Principals, r.Branches, r.RequireChecks, r.RequireCIArtifact, r.Approvals,
			r.Approvers, r.AllowAuthorApproval, r.Freeze, r.FreezeMessage, r.ExceptBranches).Scan(&r.ID, &r.UpdatedAt)
	}
	_, err := s.db.Exec(ctx, `UPDATE deploy_rules SET description = $2, selector = $3, principals = $4, branches = $5,
		require_checks = $6, require_ci_artifact = $7, approvals = $8, approvers = $9, allow_author_approval = $10,
		frozen = $11, freeze_message = $12, except_branches = $13, updated_at = now() WHERE id = $1`,
		r.ID, r.Description, r.Selector, r.Principals, r.Branches, r.RequireChecks, r.RequireCIArtifact, r.Approvals,
		r.Approvers, r.AllowAuthorApproval, r.Freeze, r.FreezeMessage, r.ExceptBranches)
	return err
}

func (s *Store) DeleteDeployRule(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM deploy_rules WHERE id = $1`, id)
	return err
}

// ---- secrets ----

type DeploySecret struct {
	ID        int64
	Name      string
	Selector  Selector
	Value     []byte // encrypted
	UpdatedBy string
	UpdatedAt time.Time
}

func (s *Store) ListDeploySecrets(ctx context.Context) ([]*DeploySecret, error) {
	rows, _ := s.db.Query(ctx, `SELECT s.id, s.name, s.selector, s.value, COALESCE(u.username, 'ghost'), s.updated_at
		FROM deploy_secrets s LEFT JOIN users u ON u.id = s.updated_by ORDER BY s.name, s.id`)
	return pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[DeploySecret])
}

func (s *Store) SaveDeploySecret(ctx context.Context, sec *DeploySecret, by int64) error {
	if sec.Selector == nil {
		sec.Selector = Selector{}
	}
	if sec.ID == 0 {
		return s.db.QueryRow(ctx, `INSERT INTO deploy_secrets (name, selector, value, updated_by) VALUES ($1, $2, $3, $4) RETURNING id`,
			sec.Name, sec.Selector, sec.Value, by).Scan(&sec.ID)
	}
	_, err := s.db.Exec(ctx, `UPDATE deploy_secrets SET name = $2, selector = $3,
		value = CASE WHEN length($4::bytea) > 0 THEN $4::bytea ELSE value END, updated_by = $5, updated_at = now() WHERE id = $1`,
		sec.ID, sec.Name, sec.Selector, sec.Value, by)
	return err
}

func (s *Store) DeleteDeploySecret(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM deploy_secrets WHERE id = $1`, id)
	return err
}

// ---- deployments ----

type Deployment struct {
	ID              int64
	SHA             string
	Ref             string
	RecipeSHA       string
	Targets         []map[string]string
	RequestedBy     *int64
	RequestedByName string
	Comment         string
	Status          string
	Evaluation      map[string]any
	RunID           *int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
	FinishedAt      *time.Time
}

const deploymentCols = `d.id, d.sha, d.ref, d.recipe_sha, d.targets, d.requested_by, COALESCE(u.username, 'ghost'), d.comment,
	d.status, d.evaluation, d.run_id, d.created_at, d.updated_at, d.finished_at`
const deploymentFrom = ` FROM deployments d LEFT JOIN users u ON u.id = d.requested_by `

func (s *Store) CreateDeployment(ctx context.Context, d *Deployment) error {
	if d.Evaluation == nil {
		d.Evaluation = map[string]any{}
	}
	return s.db.QueryRow(ctx, `INSERT INTO deployments (sha, ref, recipe_sha, targets, requested_by, comment, status, evaluation)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id, created_at, updated_at`,
		d.SHA, d.Ref, d.RecipeSHA, d.Targets, d.RequestedBy, d.Comment, d.Status, d.Evaluation).Scan(&d.ID, &d.CreatedAt, &d.UpdatedAt)
}

func (s *Store) DeploymentByID(ctx context.Context, id int64) (*Deployment, error) {
	rows, _ := s.db.Query(ctx, `SELECT `+deploymentCols+deploymentFrom+`WHERE d.id = $1`, id)
	d, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByPos[Deployment])
	return d, notFound(err)
}

func (s *Store) ListDeployments(ctx context.Context, status string, limit, offset int) ([]*Deployment, error) {
	rows, _ := s.db.Query(ctx, `SELECT `+deploymentCols+deploymentFrom+`WHERE ($1 = '' OR d.status = $1)
		ORDER BY d.id DESC LIMIT $2 OFFSET $3`, status, limit, offset)
	return pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[Deployment])
}

// UpdateDeploymentLocked runs fn on the deployment under a row lock and saves
// status, evaluation and run id.
func (s *Store) UpdateDeploymentLocked(ctx context.Context, id int64, fn func(d *Deployment) error) (*Deployment, error) {
	var out *Deployment
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		// NO KEY UPDATE: inserting a run that references this row (FK, KEY
		// SHARE) from another connection must not block on our lock.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM deployments WHERE id = $1 FOR NO KEY UPDATE`, id); err != nil {
			return err
		}
		rows, _ := tx.Query(ctx, `SELECT `+deploymentCols+deploymentFrom+`WHERE d.id = $1`, id)
		d, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByPos[Deployment])
		if err != nil {
			return notFound(err)
		}
		if err := fn(d); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE deployments SET status = $2, evaluation = $3, run_id = $4, updated_at = now(),
			finished_at = CASE WHEN $2 IN ('success', 'failure', 'cancelled', 'rejected') THEN now() END WHERE id = $1`,
			d.ID, d.Status, d.Evaluation, d.RunID)
		out = d
		return err
	})
	return out, err
}

type DeploymentApproval struct {
	UserID    int64
	Username  string
	Verdict   string
	Comment   string
	CreatedAt time.Time
}

func (s *Store) SetDeploymentApproval(ctx context.Context, deploymentID, userID int64, verdict, comment string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO deployment_approvals (deployment_id, user_id, verdict, comment) VALUES ($1, $2, $3, $4)
		ON CONFLICT (deployment_id, user_id) DO UPDATE SET verdict = $3, comment = $4, created_at = now()`,
		deploymentID, userID, verdict, comment)
	return err
}

func (s *Store) DeploymentApprovals(ctx context.Context, deploymentID int64) ([]*DeploymentApproval, error) {
	rows, _ := s.db.Query(ctx, `SELECT a.user_id, u.username, a.verdict, a.comment, a.created_at FROM deployment_approvals a
		JOIN users u ON u.id = a.user_id WHERE a.deployment_id = $1 ORDER BY a.created_at`, deploymentID)
	return pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[DeploymentApproval])
}
