-- Registry cleanup rule (Gitea's package cleanup rules, one rule since there
-- is a single owner and package type). Applied by the background job.
CREATE TABLE registry_cleanup_rule (
    id              INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    enabled         BOOLEAN NOT NULL DEFAULT false,
    keep_count      INTEGER NOT NULL DEFAULT 0 CHECK (keep_count >= 0),
    keep_pattern    TEXT NOT NULL DEFAULT '',
    remove_days     INTEGER NOT NULL DEFAULT 0 CHECK (remove_days >= 0),
    remove_pattern  TEXT NOT NULL DEFAULT '',
    -- Match patterns against "<image>/<version>" instead of the version alone.
    match_full_name BOOLEAN NOT NULL DEFAULT false,
    last_run_at     TIMESTAMPTZ,
    last_removed    INTEGER NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
