-- Pull requests. The id doubles as the user-visible PR number.
CREATE TABLE pulls (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    title       TEXT NOT NULL,
    body        TEXT NOT NULL DEFAULT '',
    author_id   BIGINT REFERENCES users(id) ON DELETE SET NULL,
    head_branch TEXT NOT NULL,
    base_branch TEXT NOT NULL,
    -- Last known head commit and merge base; frozen once the PR is merged or
    -- closed so its diff stays viewable (refs/pull/N/head keeps objects alive).
    head_sha    TEXT NOT NULL,
    merge_base  TEXT NOT NULL,
    state       TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'closed', 'merged')),
    merge_sha   TEXT,
    merge_style TEXT,
    merged_by   BIGINT REFERENCES users(id) ON DELETE SET NULL,
    merged_at   TIMESTAMPTZ,
    closed_at   TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX pulls_open_branches ON pulls (head_branch, base_branch) WHERE state = 'open';
CREATE INDEX pulls_state ON pulls (state, id DESC);

-- The latest non-"commented" review per reviewer decides their verdict.
CREATE TABLE pull_reviews (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pull_id     BIGINT NOT NULL REFERENCES pulls(id) ON DELETE CASCADE,
    reviewer_id BIGINT REFERENCES users(id) ON DELETE SET NULL,
    state       TEXT NOT NULL CHECK (state IN ('approved', 'changes_requested', 'commented')),
    body        TEXT NOT NULL DEFAULT '',
    commit_sha  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX pull_reviews_pull ON pull_reviews (pull_id, id);

-- General comments (path IS NULL) and line comments anchored to a line of
-- the diff at commit_sha. diff_hunk keeps a few lines of context so the
-- comment stays readable after the code moves on.
CREATE TABLE pull_comments (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pull_id    BIGINT NOT NULL REFERENCES pulls(id) ON DELETE CASCADE,
    author_id  BIGINT REFERENCES users(id) ON DELETE SET NULL,
    body       TEXT NOT NULL,
    path       TEXT,
    side       TEXT CHECK (side IN ('old', 'new')),
    line       INTEGER,
    commit_sha TEXT,
    diff_hunk  TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((path IS NULL) = (line IS NULL) AND (path IS NULL) = (side IS NULL) AND (path IS NULL) = (commit_sha IS NULL))
);
CREATE INDEX pull_comments_pull ON pull_comments (pull_id, id);

-- Timeline events: pushes, state changes, branch deletion, retitling.
CREATE TABLE pull_events (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pull_id    BIGINT NOT NULL REFERENCES pulls(id) ON DELETE CASCADE,
    actor_id   BIGINT REFERENCES users(id) ON DELETE SET NULL,
    kind       TEXT NOT NULL,
    data       JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX pull_events_pull ON pull_events (pull_id, id);

-- Branch protection rules; pattern is an exact branch name or a glob
-- (path.Match syntax, e.g. "release/*"). The most specific match wins.
CREATE TABLE branch_protections (
    id                         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pattern                    TEXT NOT NULL UNIQUE,
    require_pull_request       BOOLEAN NOT NULL DEFAULT true,
    required_approvals         INTEGER NOT NULL DEFAULT 1 CHECK (required_approvals >= 0),
    dismiss_stale_approvals    BOOLEAN NOT NULL DEFAULT false,
    require_code_owner_review  BOOLEAN NOT NULL DEFAULT false,
    block_on_changes_requested BOOLEAN NOT NULL DEFAULT true,
    allow_force_push           BOOLEAN NOT NULL DEFAULT false,
    allow_deletion             BOOLEAN NOT NULL DEFAULT false,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now()
);
