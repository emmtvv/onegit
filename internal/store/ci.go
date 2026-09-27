package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Job and run states.
const (
	JobWaiting   = "waiting"
	JobQueued    = "queued"
	JobRunning   = "running"
	JobSuccess   = "success"
	JobFailure   = "failure"
	JobCancelled = "cancelled"
	JobSkipped   = "skipped"
)

func JobTerminal(s string) bool {
	return s == JobSuccess || s == JobFailure || s == JobCancelled || s == JobSkipped
}

// ---- runners ----

type Runner struct {
	ID         int64
	Name       string
	Kind       string // "ci" or "deploy"
	Labels     []string
	Targets    map[string][]string // deploy runners: selector of allowed targets
	Enabled    bool
	Version    string
	LastSeenAt *time.Time
	CreatedAt  time.Time
}

const runnerCols = `id, name, kind, labels, targets, enabled, version, last_seen_at, created_at`

func scanRunner(row pgx.Row) (*Runner, error) {
	var r Runner
	if err := row.Scan(&r.ID, &r.Name, &r.Kind, &r.Labels, &r.Targets, &r.Enabled, &r.Version, &r.LastSeenAt, &r.CreatedAt); err != nil {
		return nil, notFound(err)
	}
	return &r, nil
}

func (s *Store) CreateRunner(ctx context.Context, r *Runner, tokenHash string) error {
	if r.Targets == nil {
		r.Targets = map[string][]string{}
	}
	err := s.db.QueryRow(ctx, `INSERT INTO runners (name, kind, labels, targets, token_hash) VALUES ($1, $2, $3, $4, $5)
		RETURNING id, enabled, created_at`, r.Name, r.Kind, r.Labels, r.Targets, tokenHash).Scan(&r.ID, &r.Enabled, &r.CreatedAt)
	if isUniqueViolation(err) {
		return ErrDuplicate
	}
	return err
}

func (s *Store) UpdateRunner(ctx context.Context, r *Runner) error {
	_, err := s.db.Exec(ctx, `UPDATE runners SET labels = $2, targets = $3, enabled = $4 WHERE id = $1`,
		r.ID, r.Labels, r.Targets, r.Enabled)
	return err
}

func (s *Store) SetRunnerToken(ctx context.Context, id int64, tokenHash string) error {
	_, err := s.db.Exec(ctx, `UPDATE runners SET token_hash = $2 WHERE id = $1`, id, tokenHash)
	return err
}

func (s *Store) ListRunners(ctx context.Context) ([]*Runner, error) {
	rows, err := s.db.Query(ctx, `SELECT `+runnerCols+` FROM runners ORDER BY kind, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Runner
	for rows.Next() {
		r, err := scanRunner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) RunnerByID(ctx context.Context, id int64) (*Runner, error) {
	return scanRunner(s.db.QueryRow(ctx, `SELECT `+runnerCols+` FROM runners WHERE id = $1`, id))
}

func (s *Store) RunnerByTokenHash(ctx context.Context, hash string) (*Runner, error) {
	return scanRunner(s.db.QueryRow(ctx, `SELECT `+runnerCols+` FROM runners WHERE token_hash = $1`, hash))
}

func (s *Store) TouchRunner(ctx context.Context, id int64, version string) error {
	_, err := s.db.Exec(ctx, `UPDATE runners SET last_seen_at = now(), version = $2 WHERE id = $1`, id, version)
	return err
}

func (s *Store) DeleteRunner(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM runners WHERE id = $1`, id)
	return err
}

// ---- runs and jobs ----

type Run struct {
	ID              int64
	Kind            string // "pipeline" or "deploy"
	Name            string
	File            string
	Event           string
	Ref             string
	SHA             string
	BeforeSHA       string
	ChangedFiles    []string
	PullID          *int64
	DeploymentID    *int64
	TriggeredBy     *int64
	TriggeredByName string
	Status          string
	CreatedAt       time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
}

type StepState struct {
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type Job struct {
	ID              int64
	RunID           int64
	Name            string
	Kind            string // "ci" or "deploy"
	Needs           []string
	RunsOn          []string
	Spec            json.RawMessage
	Target          map[string]string
	TargetKey       *string
	Status          string
	Steps           []StepState
	RunnerID        *int64
	RunnerName      string
	CancelRequested bool
	Message         string
	HeartbeatAt     *time.Time
	CreatedAt       time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
}

func (j *Job) Duration() time.Duration {
	if j.StartedAt == nil {
		return 0
	}
	end := time.Now()
	if j.FinishedAt != nil {
		end = *j.FinishedAt
	}
	return end.Sub(*j.StartedAt).Round(time.Second)
}

const runCols = `r.id, r.kind, r.name, r.file, r.event, r.ref, r.sha, r.before_sha, r.changed_files, r.pull_id, r.deployment_id,
	r.triggered_by, COALESCE(u.username, 'ghost'), r.status, r.created_at, r.started_at, r.finished_at`
const runFrom = ` FROM ci_runs r LEFT JOIN users u ON u.id = r.triggered_by `

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Kind, &r.Name, &r.File, &r.Event, &r.Ref, &r.SHA, &r.BeforeSHA, &r.ChangedFiles, &r.PullID,
		&r.DeploymentID, &r.TriggeredBy, &r.TriggeredByName, &r.Status, &r.CreatedAt, &r.StartedAt, &r.FinishedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &r, nil
}

const jobCols = `id, run_id, name, kind, needs, runs_on, spec, target, target_key, status, steps, runner_id, runner_name,
	cancel_requested, message, heartbeat_at, created_at, started_at, finished_at`

func scanJob(row pgx.Row) (*Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.RunID, &j.Name, &j.Kind, &j.Needs, &j.RunsOn, &j.Spec, &j.Target, &j.TargetKey, &j.Status,
		&j.Steps, &j.RunnerID, &j.RunnerName, &j.CancelRequested, &j.Message, &j.HeartbeatAt, &j.CreatedAt, &j.StartedAt, &j.FinishedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &j, nil
}

// CreateRun inserts a run with its jobs; the caller sets each job's initial
// status (queued, or waiting when it has needs).
func (s *Store) CreateRun(ctx context.Context, r *Run, jobs []*Job) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if r.ChangedFiles == nil {
			r.ChangedFiles = []string{}
		}
		err := tx.QueryRow(ctx, `INSERT INTO ci_runs (kind, name, file, event, ref, sha, before_sha, changed_files, pull_id,
			deployment_id, triggered_by, status) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING id, created_at`,
			r.Kind, r.Name, r.File, r.Event, r.Ref, r.SHA, r.BeforeSHA, r.ChangedFiles, r.PullID, r.DeploymentID, r.TriggeredBy, r.Status).
			Scan(&r.ID, &r.CreatedAt)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			j.RunID = r.ID
			if j.Needs == nil {
				j.Needs = []string{}
			}
			if j.RunsOn == nil {
				j.RunsOn = []string{}
			}
			if err := tx.QueryRow(ctx, `INSERT INTO ci_jobs (run_id, name, kind, needs, runs_on, spec, target, target_key, status, message,
				finished_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, CASE WHEN $9 = 'skipped' THEN now() END)
				RETURNING id, created_at, finished_at`,
				j.RunID, j.Name, j.Kind, j.Needs, j.RunsOn, j.Spec, j.Target, j.TargetKey, j.Status, j.Message).
				Scan(&j.ID, &j.CreatedAt, &j.FinishedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) RunByID(ctx context.Context, id int64) (*Run, error) {
	return scanRun(s.db.QueryRow(ctx, `SELECT `+runCols+runFrom+`WHERE r.id = $1`, id))
}

type RunFilter struct {
	Kind   string
	SHA    string
	Limit  int
	Offset int
}

func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]*Run, error) {
	rows, err := s.db.Query(ctx, `SELECT `+runCols+runFrom+`WHERE ($1 = '' OR r.kind = $1) AND ($2 = '' OR r.sha = $2)
		ORDER BY r.id DESC LIMIT $3 OFFSET $4`, f.Kind, f.SHA, f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) JobsForRun(ctx context.Context, runID int64) ([]*Job, error) {
	return s.queryJobs(ctx, `SELECT `+jobCols+` FROM ci_jobs WHERE run_id = $1 ORDER BY id`, runID)
}

func (s *Store) queryJobs(ctx context.Context, q string, args ...any) ([]*Job, error) {
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) JobByID(ctx context.Context, id int64) (*Job, error) {
	return scanJob(s.db.QueryRow(ctx, `SELECT `+jobCols+` FROM ci_jobs WHERE id = $1`, id))
}

// RunningJobByTokenHash resolves a job token; tokens only work while the job
// runs.
func (s *Store) RunningJobByTokenHash(ctx context.Context, hash string) (*Job, error) {
	return scanJob(s.db.QueryRow(ctx, `SELECT `+jobCols+` FROM ci_jobs WHERE token_hash = $1 AND status = 'running'`, hash))
}

// ClaimJob assigns the oldest queued job the runner can take. accept filters
// candidates (e.g. deploy target selectors). Deploy jobs for the same target
// never run concurrently.
func (s *Store) ClaimJob(ctx context.Context, r *Runner, tokenHash string, accept func(*Job) bool) (*Job, error) {
	var claimed *Job
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT `+jobCols+` FROM ci_jobs WHERE status = 'queued' AND kind = $1 AND runs_on <@ $2
			ORDER BY id LIMIT 50 FOR UPDATE SKIP LOCKED`, r.Kind, r.Labels)
		cands, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Job, error) { return scanJob(row) })
		if err != nil {
			return err
		}
		for _, j := range cands {
			if !accept(j) {
				continue
			}
			if j.TargetKey != nil {
				if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, *j.TargetKey); err != nil {
					return err
				}
				var busy bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ci_jobs WHERE target_key = $1 AND status = 'running')`,
					*j.TargetKey).Scan(&busy); err != nil {
					return err
				}
				if busy {
					continue
				}
			}
			if err := tx.QueryRow(ctx, `UPDATE ci_jobs SET status = 'running', runner_id = $2, runner_name = $3, token_hash = $4,
				started_at = now(), heartbeat_at = now() WHERE id = $1 RETURNING `+jobCols, j.ID, r.ID, r.Name, tokenHash).
				Scan(&j.ID, &j.RunID, &j.Name, &j.Kind, &j.Needs, &j.RunsOn, &j.Spec, &j.Target, &j.TargetKey, &j.Status,
					&j.Steps, &j.RunnerID, &j.RunnerName, &j.CancelRequested, &j.Message, &j.HeartbeatAt, &j.CreatedAt,
					&j.StartedAt, &j.FinishedAt); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE ci_runs SET status = 'running', started_at = COALESCE(started_at, now())
				WHERE id = $1 AND status IN ('queued', 'running')`, j.RunID); err != nil {
				return err
			}
			claimed = j
			return nil
		}
		return nil
	})
	return claimed, err
}

// Heartbeat records liveness and reports whether cancellation was requested.
func (s *Store) Heartbeat(ctx context.Context, jobID int64) (cancel bool, err error) {
	err = s.db.QueryRow(ctx, `UPDATE ci_jobs SET heartbeat_at = now() WHERE id = $1 AND status = 'running' RETURNING cancel_requested`,
		jobID).Scan(&cancel)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil // no longer running: stop
	}
	return cancel, err
}

// AppendLog stores a log chunk; retries with the same seq are ignored.
func (s *Store) AppendLog(ctx context.Context, jobID int64, seq int, data string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO ci_logs (job_id, seq, data) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, jobID, seq, data)
	return err
}

type LogChunk struct {
	Seq  int    `json:"seq"`
	Data string `json:"data"`
}

func (s *Store) LogChunks(ctx context.Context, jobID int64, afterSeq int) ([]LogChunk, error) {
	rows, _ := s.db.Query(ctx, `SELECT seq, data FROM ci_logs WHERE job_id = $1 AND seq > $2 ORDER BY seq`, jobID, afterSeq)
	return pgx.CollectRows(rows, pgx.RowToStructByPos[LogChunk])
}

func (s *Store) SetJobSteps(ctx context.Context, jobID int64, steps []StepState) error {
	_, err := s.db.Exec(ctx, `UPDATE ci_jobs SET steps = $2, heartbeat_at = now() WHERE id = $1 AND status = 'running'`, jobID, steps)
	return err
}

// FinishJob ends a running (or, with force, any non-terminal) job, then
// advances the run: waiting jobs are queued once their needs succeeded or
// skipped when a need did not, and the run gets its final status when every
// job is done. It returns the run and every job whose status changed.
func (s *Store) FinishJob(ctx context.Context, jobID int64, status, message string) (*Run, []*Job, error) {
	var run *Run
	var changed []*Job
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var runID int64
		if err := tx.QueryRow(ctx, `SELECT run_id FROM ci_jobs WHERE id = $1`, jobID).Scan(&runID); err != nil {
			return notFound(err)
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM ci_runs WHERE id = $1 FOR UPDATE`, runID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE ci_jobs SET status = $2, message = $3, finished_at = now(), token_hash = NULL
			WHERE id = $1 AND status NOT IN ('success', 'failure', 'cancelled', 'skipped')`, jobID, status, message)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil // already finished (e.g. a retried request)
		}
		run, changed, err = advanceRun(ctx, tx, runID)
		if err == nil {
			if j, err2 := scanJob(tx.QueryRow(ctx, `SELECT `+jobCols+` FROM ci_jobs WHERE id = $1`, jobID)); err2 == nil {
				changed = append([]*Job{j}, changed...)
			}
		}
		return err
	})
	return run, changed, err
}

// CancelRun cancels queued and waiting jobs at once and asks runners to stop
// running ones.
func (s *Store) CancelRun(ctx context.Context, runID int64) (*Run, []*Job, error) {
	var run *Run
	var changed []*Job
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM ci_runs WHERE id = $1 FOR UPDATE`, runID); err != nil {
			return err
		}
		rows, _ := tx.Query(ctx, `UPDATE ci_jobs SET status = 'cancelled', finished_at = now(), message = 'cancelled'
			WHERE run_id = $1 AND status IN ('queued', 'waiting') RETURNING `+jobCols, runID)
		var err error
		changed, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Job, error) { return scanJob(row) })
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE ci_jobs SET cancel_requested = true WHERE run_id = $1 AND status = 'running'`, runID); err != nil {
			return err
		}
		var more []*Job
		run, more, err = advanceRun(ctx, tx, runID)
		changed = append(changed, more...)
		return err
	})
	return run, changed, err
}

// advanceRun propagates job results inside a transaction holding the run lock.
func advanceRun(ctx context.Context, tx pgx.Tx, runID int64) (*Run, []*Job, error) {
	rows, _ := tx.Query(ctx, `SELECT `+jobCols+` FROM ci_jobs WHERE run_id = $1 ORDER BY id`, runID)
	jobs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Job, error) { return scanJob(row) })
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]*Job{}
	for _, j := range jobs {
		byName[j.Name] = j
	}
	var changed []*Job
	for progress := true; progress; {
		progress = false
		for _, j := range jobs {
			if j.Status != JobWaiting {
				continue
			}
			next := JobQueued
			for _, n := range j.Needs {
				dep := byName[n]
				switch {
				case dep == nil || dep.Status == JobFailure || dep.Status == JobCancelled || dep.Status == JobSkipped:
					next = JobSkipped
				case dep.Status != JobSuccess && next != JobSkipped:
					next = JobWaiting
				}
			}
			if next == JobWaiting {
				continue
			}
			j.Status = next
			if next == JobSkipped {
				j.Message = "a required job did not succeed"
			}
			if _, err := tx.Exec(ctx, `UPDATE ci_jobs SET status = $2, message = $3,
				finished_at = CASE WHEN $2 = 'skipped' THEN now() END WHERE id = $1`, j.ID, j.Status, j.Message); err != nil {
				return nil, nil, err
			}
			changed = append(changed, j)
			progress = true
		}
	}
	status := JobSuccess
	done := true
	for _, j := range jobs {
		switch {
		case !JobTerminal(j.Status):
			done = false
		case j.Status == JobFailure:
			status = JobFailure
		case j.Status == JobCancelled && status != JobFailure:
			status = JobCancelled
		}
	}
	if done {
		if _, err := tx.Exec(ctx, `UPDATE ci_runs SET status = $2, finished_at = now() WHERE id = $1 AND finished_at IS NULL`,
			runID, status); err != nil {
			return nil, nil, err
		}
	}
	run, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+runFrom+`WHERE r.id = $1`, runID))
	return run, changed, err
}

// StaleJobs lists running jobs whose runner stopped reporting.
func (s *Store) StaleJobs(ctx context.Context, silence time.Duration) ([]int64, error) {
	rows, _ := s.db.Query(ctx, `SELECT id FROM ci_jobs WHERE status = 'running' AND heartbeat_at < now() - $1::interval`, silence)
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// ---- commit statuses ----

type CommitStatus struct {
	SHA         string
	Context     string
	State       string
	Description string
	TargetURL   string
	UpdatedAt   time.Time
}

func (st *CommitStatus) OK() bool { return st.State == "success" || st.State == "skipped" }

func (s *Store) SetCommitStatus(ctx context.Context, st *CommitStatus, jobID *int64) error {
	_, err := s.db.Exec(ctx, `INSERT INTO commit_statuses (sha, context, state, description, target_url, job_id)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (sha, context) DO UPDATE SET state = $3, description = $4,
		target_url = $5, job_id = $6, updated_at = now()`, st.SHA, st.Context, st.State, st.Description, st.TargetURL, jobID)
	return err
}

func (s *Store) CommitStatuses(ctx context.Context, sha string) ([]*CommitStatus, error) {
	rows, _ := s.db.Query(ctx, `SELECT sha, context, state, description, target_url, updated_at FROM commit_statuses
		WHERE sha = $1 ORDER BY context`, sha)
	return pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[CommitStatus])
}

// ---- deploy history ----

// TargetDeploy is the latest finished deploy job for a target.
type TargetDeploy struct {
	Target       map[string]string
	TargetKey    string
	JobID        int64
	Status       string
	SHA          string
	DeploymentID *int64
	By           string
	At           time.Time
}

// LatestDeploys returns, per target, the latest successful deploy and, when
// newer, the latest finished attempt.
func (s *Store) LatestDeploys(ctx context.Context) ([]*TargetDeploy, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ON (j.target_key) j.target, j.target_key, j.id, j.status, r.sha, r.deployment_id,
		       COALESCE(u.username, 'ghost'), COALESCE(j.finished_at, j.created_at)
		FROM ci_jobs j JOIN ci_runs r ON r.id = j.run_id LEFT JOIN users u ON u.id = r.triggered_by
		WHERE j.kind = 'deploy' AND j.status = 'success'
		ORDER BY j.target_key, j.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TargetDeploy
	for rows.Next() {
		var d TargetDeploy
		if err := rows.Scan(&d.Target, &d.TargetKey, &d.JobID, &d.Status, &d.SHA, &d.DeploymentID, &d.By, &d.At); err != nil {
			return nil, err
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// TargetHistory lists deploy jobs of one target, newest first.
func (s *Store) TargetHistory(ctx context.Context, targetKey string, limit int) ([]*TargetDeploy, error) {
	rows, err := s.db.Query(ctx, `
		SELECT j.target, j.target_key, j.id, j.status, r.sha, r.deployment_id, COALESCE(u.username, 'ghost'), j.created_at
		FROM ci_jobs j JOIN ci_runs r ON r.id = j.run_id LEFT JOIN users u ON u.id = r.triggered_by
		WHERE j.kind = 'deploy' AND j.target_key = $1 ORDER BY j.id DESC LIMIT $2`, targetKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TargetDeploy
	for rows.Next() {
		var d TargetDeploy
		if err := rows.Scan(&d.Target, &d.TargetKey, &d.JobID, &d.Status, &d.SHA, &d.DeploymentID, &d.By, &d.At); err != nil {
			return nil, err
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// HasLabels reports whether a runner has every label.
func HasLabels(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// AdvanceRun re-evaluates waiting jobs and the run status.
func (s *Store) AdvanceRun(ctx context.Context, runID int64) (*Run, []*Job, error) {
	var run *Run
	var changed []*Job
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM ci_runs WHERE id = $1 FOR UPDATE`, runID); err != nil {
			return err
		}
		var err error
		run, changed, err = advanceRun(ctx, tx, runID)
		return err
	})
	return run, changed, err
}
