-- Monorepo features: matrix jobs, artifacts, caches, scheduled pipelines,
-- auto-merge, merge queue, projects.

-- Matrix jobs share a base name ("test" for "test (1.22, linux)"); needs
-- refer to base names and wait for every job of the matrix.
ALTER TABLE ci_jobs ADD COLUMN base_name TEXT NOT NULL DEFAULT '';

-- Files a job kept (one tar.gz per job in S3), handed to the jobs that need
-- it and downloadable from the job page until they expire.
CREATE TABLE ci_artifacts (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id     BIGINT NOT NULL REFERENCES ci_jobs(id) ON DELETE CASCADE,
    run_id     BIGINT NOT NULL REFERENCES ci_runs(id) ON DELETE CASCADE,
    size       BIGINT NOT NULL,
    blob_key   TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (job_id)
);
CREATE INDEX ci_artifacts_run ON ci_artifacts (run_id);
CREATE INDEX ci_artifacts_expires ON ci_artifacts (expires_at);

-- Caches by key, scoped to the ref that saved them (refs/heads/x,
-- refs/pull/N/head): a job reads its own ref's caches and the default
-- branch's, and writes only its own, so a branch cannot poison main.
CREATE TABLE ci_caches (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    scope        TEXT NOT NULL,
    key          TEXT NOT NULL,
    size         BIGINT NOT NULL,
    blob_key     TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (scope, key)
);
CREATE INDEX ci_caches_used ON ci_caches (last_used_at);

-- When each schedule of a pipeline last fired (pipelines come from the
-- default branch; a schedule seen for the first time waits for its next
-- time instead of firing at once).
CREATE TABLE ci_schedules (
    pipeline TEXT NOT NULL,
    cron     TEXT NOT NULL,
    timezone TEXT NOT NULL DEFAULT '',
    last_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (pipeline, cron, timezone)
);

-- Changed files of a PR (as of files_sha = head_sha) for project filters;
-- auto-merge settings while it is enabled.
ALTER TABLE pulls
    ADD COLUMN changed_files TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN files_sha TEXT NOT NULL DEFAULT '',
    ADD COLUMN auto_merge_by BIGINT REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN auto_merge_style TEXT NOT NULL DEFAULT '',
    ADD COLUMN auto_merge_title TEXT NOT NULL DEFAULT '',
    ADD COLUMN auto_merge_message TEXT NOT NULL DEFAULT '',
    ADD COLUMN auto_merge_delete_branch BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX pulls_auto_merge ON pulls (id) WHERE state = 'open' AND auto_merge_by IS NOT NULL;

ALTER TABLE branch_protections
    ADD COLUMN require_merge_queue BOOLEAN NOT NULL DEFAULT false,
    -- How many queued PRs are tested at once, each on top of the ones
    -- before it.
    ADD COLUMN merge_queue_depth INTEGER NOT NULL DEFAULT 5 CHECK (merge_queue_depth BETWEEN 1 AND 50);

-- Merge queue entries. Active ones (queued, testing) are processed in id
-- order per base branch: entry k is tested as base + PR 1..k (test_sha,
-- built on base_sha) and lands once its checks pass.
CREATE TABLE merge_queue (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pull_id       BIGINT NOT NULL REFERENCES pulls(id) ON DELETE CASCADE,
    base_branch   TEXT NOT NULL,
    state         TEXT NOT NULL CHECK (state IN ('queued', 'testing', 'merged', 'failed', 'removed')),
    style         TEXT NOT NULL,
    title         TEXT NOT NULL DEFAULT '',
    message       TEXT NOT NULL DEFAULT '',
    delete_branch BOOLEAN NOT NULL DEFAULT false,
    enqueued_by   BIGINT REFERENCES users(id) ON DELETE SET NULL,
    head_sha      TEXT NOT NULL,
    base_sha      TEXT NOT NULL DEFAULT '',
    test_sha      TEXT NOT NULL DEFAULT '',
    reason        TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at   TIMESTAMPTZ
);
CREATE UNIQUE INDEX merge_queue_active_pull ON merge_queue (pull_id) WHERE state IN ('queued', 'testing');
CREATE INDEX merge_queue_active ON merge_queue (base_branch, id) WHERE state IN ('queued', 'testing');
CREATE INDEX merge_queue_test_sha ON merge_queue (test_sha) WHERE state = 'testing';

-- Server settings edited in the UI (e.g. how projects are discovered).
CREATE TABLE settings (
    name       TEXT PRIMARY KEY,
    value      JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
