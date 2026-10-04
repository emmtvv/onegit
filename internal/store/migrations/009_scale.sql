-- Indexes and lookup tables for large installations: millions of CI runs
-- and pull requests, thousands of projects.

-- The directories (with a trailing slash, up to 6 levels deep) that hold a
-- file changed by a run or a pull request, so "runs/PRs of a project" is an
-- index lookup instead of a scan of every changed_files array.
CREATE TABLE ci_run_dirs (
    dir    TEXT NOT NULL,
    run_id BIGINT NOT NULL REFERENCES ci_runs(id) ON DELETE CASCADE,
    PRIMARY KEY (dir, run_id)
);
CREATE INDEX ci_run_dirs_run ON ci_run_dirs (run_id);

CREATE TABLE pull_dirs (
    dir     TEXT NOT NULL,
    pull_id BIGINT NOT NULL REFERENCES pulls(id) ON DELETE CASCADE,
    PRIMARY KEY (dir, pull_id)
);
CREATE INDEX pull_dirs_pull ON pull_dirs (pull_id);

INSERT INTO ci_run_dirs (dir, run_id)
SELECT DISTINCT array_to_string(p.parts[1:i], '/') || '/', r.id
FROM ci_runs r
CROSS JOIN LATERAL unnest(r.changed_files) f(path)
CROSS JOIN LATERAL (SELECT string_to_array(f.path, '/') AS parts) p
CROSS JOIN LATERAL generate_series(1, least(cardinality(p.parts) - 1, 6)) i;

INSERT INTO pull_dirs (dir, pull_id)
SELECT DISTINCT array_to_string(p.parts[1:i], '/') || '/', pl.id
FROM pulls pl
CROSS JOIN LATERAL unnest(pl.changed_files) f(path)
CROSS JOIN LATERAL (SELECT string_to_array(f.path, '/') AS parts) p
CROSS JOIN LATERAL generate_series(1, least(cardinality(p.parts) - 1, 6)) i;

-- Latest pipeline of a branch (project pages).
CREATE INDEX ci_runs_ref ON ci_runs (ref, id DESC);
CREATE INDEX ci_runs_pull ON ci_runs (pull_id) WHERE pull_id IS NOT NULL;

-- What is deployed where: the latest successful deploy per target.
CREATE INDEX ci_jobs_deployed ON ci_jobs (target_key, id DESC) WHERE kind = 'deploy' AND status = 'success';

-- Background loops that would otherwise scan whole tables every few seconds.
CREATE INDEX ci_jobs_running ON ci_jobs (heartbeat_at) WHERE status = 'running';
-- Log retention finds the first job to keep by creation time.
CREATE INDEX ci_jobs_created ON ci_jobs (created_at);
CREATE INDEX pulls_files_stale ON pulls (id) WHERE files_sha <> head_sha;
CREATE INDEX merge_queue_finished ON merge_queue (finished_at DESC) WHERE finished_at IS NOT NULL;

-- Keyset pagination of closed pull requests.
CREATE INDEX pulls_closed ON pulls (id DESC) WHERE state <> 'open';
