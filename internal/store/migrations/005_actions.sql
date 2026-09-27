-- Phase 4: CI pipelines, deployments with server-side policy, teams.

-- Teams group users for deploy rules and CODEOWNERS (@team). Membership is
-- managed by hand or synced from an OIDC group on every login.
CREATE TABLE teams (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        TEXT NOT NULL CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]*$'),
    description TEXT NOT NULL DEFAULT '',
    oidc_group  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX teams_name ON teams (lower(name));

CREATE TABLE team_members (
    team_id BIGINT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (team_id, user_id)
);
CREATE INDEX team_members_user ON team_members (user_id);

-- Runners poll for jobs with their token. "ci" runners take pipeline jobs
-- (untrusted code, no secrets); "deploy" runners take approved deployment
-- jobs whose target matches their selector.
CREATE TABLE runners (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    kind         TEXT NOT NULL CHECK (kind IN ('ci', 'deploy')),
    labels       TEXT[] NOT NULL DEFAULT '{}',
    targets      JSONB NOT NULL DEFAULT '{}',
    token_hash   TEXT NOT NULL UNIQUE,
    enabled      BOOLEAN NOT NULL DEFAULT true,
    version      TEXT NOT NULL DEFAULT '',
    last_seen_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ci_runs (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind          TEXT NOT NULL CHECK (kind IN ('pipeline', 'deploy')),
    name          TEXT NOT NULL,
    file          TEXT NOT NULL DEFAULT '',
    event         TEXT NOT NULL,
    ref           TEXT NOT NULL,
    sha           TEXT NOT NULL,
    before_sha    TEXT NOT NULL DEFAULT '',
    changed_files TEXT[] NOT NULL DEFAULT '{}',
    pull_id       BIGINT REFERENCES pulls(id) ON DELETE SET NULL,
    deployment_id BIGINT,
    triggered_by  BIGINT REFERENCES users(id) ON DELETE SET NULL,
    status        TEXT NOT NULL DEFAULT 'queued',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ
);
CREATE INDEX ci_runs_sha ON ci_runs (sha);
CREATE INDEX ci_runs_kind ON ci_runs (kind, id DESC);

-- Job states: waiting (on needs), queued, running, success, failure,
-- cancelled, skipped.
CREATE TABLE ci_jobs (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id           BIGINT NOT NULL REFERENCES ci_runs(id) ON DELETE CASCADE,
    name             TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('ci', 'deploy')),
    needs            TEXT[] NOT NULL DEFAULT '{}',
    runs_on          TEXT[] NOT NULL DEFAULT '{}',
    spec             JSONB NOT NULL,
    target           JSONB,
    target_key       TEXT,
    status           TEXT NOT NULL,
    steps            JSONB NOT NULL DEFAULT '[]',
    runner_id        BIGINT REFERENCES runners(id) ON DELETE SET NULL,
    runner_name      TEXT NOT NULL DEFAULT '',
    token_hash       TEXT UNIQUE,
    cancel_requested BOOLEAN NOT NULL DEFAULT false,
    message          TEXT NOT NULL DEFAULT '',
    heartbeat_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at       TIMESTAMPTZ,
    finished_at      TIMESTAMPTZ
);
CREATE INDEX ci_jobs_queued ON ci_jobs (id) WHERE status = 'queued';
CREATE INDEX ci_jobs_run ON ci_jobs (run_id, id);
CREATE INDEX ci_jobs_target ON ci_jobs (target_key, id DESC) WHERE target_key IS NOT NULL;

CREATE TABLE ci_logs (
    job_id BIGINT NOT NULL REFERENCES ci_jobs(id) ON DELETE CASCADE,
    seq    INTEGER NOT NULL,
    data   TEXT NOT NULL,
    PRIMARY KEY (job_id, seq)
);

-- Check results per commit ("<pipeline> / <job>"), shown on PRs and usable
-- as required checks.
CREATE TABLE commit_statuses (
    sha         TEXT NOT NULL,
    context     TEXT NOT NULL,
    state       TEXT NOT NULL CHECK (state IN ('pending', 'success', 'failure', 'error', 'skipped')),
    description TEXT NOT NULL DEFAULT '',
    target_url  TEXT NOT NULL DEFAULT '',
    job_id      BIGINT REFERENCES ci_jobs(id) ON DELETE SET NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (sha, context)
);

ALTER TABLE branch_protections
    ADD COLUMN required_checks TEXT[] NOT NULL DEFAULT '{}',
    -- Server-side owners in CODEOWNERS syntax; enforced in addition to the
    -- CODEOWNERS file and cannot be changed by a commit.
    ADD COLUMN owners TEXT NOT NULL DEFAULT '';

-- Registry provenance: which CI job built a manifest, from which commit.
ALTER TABLE registry_manifests
    ADD COLUMN build_job_id BIGINT REFERENCES ci_jobs(id) ON DELETE SET NULL,
    ADD COLUMN build_sha TEXT NOT NULL DEFAULT '',
    ADD COLUMN build_ref TEXT NOT NULL DEFAULT '';

-- ---- deployments ----

-- A deploy target is a value for every dimension. Values come from a fixed
-- list or from the default branch tree (path_pattern with one "*" segment,
-- e.g. projects/*/project.yaml).
CREATE TABLE deploy_dimensions (
    name         TEXT PRIMARY KEY CHECK (name ~ '^[a-z][a-z0-9_]*$'),
    position     INTEGER NOT NULL DEFAULT 0,
    source       TEXT NOT NULL CHECK (source IN ('list', 'paths')),
    value_list   TEXT[] NOT NULL DEFAULT '{}',
    path_pattern TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Grants say who may request a deployment (any matching grant suffices);
-- requirements say under which conditions (every matching one applies), so
-- adding a requirement can only tighten access. Selector: {dimension:
-- [glob, ...]}; a missing dimension matches every value.
CREATE TABLE deploy_rules (
    id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind                  TEXT NOT NULL CHECK (kind IN ('grant', 'require')),
    description           TEXT NOT NULL DEFAULT '',
    selector              JSONB NOT NULL DEFAULT '{}',
    principals            JSONB NOT NULL DEFAULT '[]',
    branches              TEXT[] NOT NULL DEFAULT '{}',
    require_checks        BOOLEAN NOT NULL DEFAULT false,
    require_ci_artifact   BOOLEAN NOT NULL DEFAULT false,
    approvals             INTEGER NOT NULL DEFAULT 0 CHECK (approvals >= 0),
    approvers             JSONB NOT NULL DEFAULT '[]',
    allow_author_approval BOOLEAN NOT NULL DEFAULT false,
    frozen                BOOLEAN NOT NULL DEFAULT false,
    freeze_message        TEXT NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Encrypted secrets handed only to deployment jobs whose target matches.
-- The same name may exist with different selectors; the most specific wins.
CREATE TABLE deploy_secrets (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       TEXT NOT NULL CHECK (name ~ '^[A-Za-z_][A-Za-z0-9_]*$'),
    selector   JSONB NOT NULL DEFAULT '{}',
    value      BYTEA NOT NULL,
    updated_by BIGINT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A deployment request: one commit to one or more targets. The recipe is
-- pinned to recipe_sha (the default branch when requested), so approvers
-- approve exactly what will run.
CREATE TABLE deployments (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    sha          TEXT NOT NULL,
    ref          TEXT NOT NULL,
    recipe_sha   TEXT NOT NULL,
    targets      JSONB NOT NULL,
    requested_by BIGINT REFERENCES users(id) ON DELETE SET NULL,
    comment      TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL CHECK (status IN ('pending', 'rejected', 'running', 'success', 'failure', 'cancelled')),
    evaluation   JSONB NOT NULL DEFAULT '{}',
    run_id       BIGINT REFERENCES ci_runs(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ
);
CREATE INDEX deployments_status ON deployments (status, id DESC);
ALTER TABLE ci_runs ADD FOREIGN KEY (deployment_id) REFERENCES deployments(id) ON DELETE SET NULL;

CREATE TABLE deployment_approvals (
    deployment_id BIGINT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    user_id       BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    verdict       TEXT NOT NULL CHECK (verdict IN ('approve', 'reject')),
    comment       TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (deployment_id, user_id)
);

-- Who changed rules, secrets, runners and teams, and who deployed what.
CREATE TABLE audit_log (
    id       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor_id BIGINT REFERENCES users(id) ON DELETE SET NULL,
    action   TEXT NOT NULL,
    subject  TEXT NOT NULL DEFAULT '',
    data     JSONB NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_log_at ON audit_log (id DESC);
