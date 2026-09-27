CREATE TABLE users (
    id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username             TEXT NOT NULL,
    email                TEXT NOT NULL DEFAULT '',
    full_name            TEXT NOT NULL DEFAULT '',
    password_hash        TEXT NOT NULL DEFAULT '',
    must_change_password BOOLEAN NOT NULL DEFAULT false,
    role                 TEXT NOT NULL DEFAULT 'read' CHECK (role IN ('read', 'write', 'admin')),
    active               BOOLEAN NOT NULL DEFAULT true,
    oidc_subject         TEXT UNIQUE,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_username_lower ON users (lower(username));

CREATE TABLE ssh_keys (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    fingerprint TEXT NOT NULL UNIQUE,
    public_key  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used   TIMESTAMPTZ
);
CREATE INDEX ssh_keys_user ON ssh_keys (user_id);

CREATE TABLE tokens (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    hash       TEXT NOT NULL UNIQUE,
    prefix     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used  TIMESTAMPTZ
);
CREATE INDEX tokens_user ON tokens (user_id);

-- Server-wide secrets (SSH host key, ...) so replicas stay stateless.
CREATE TABLE server_secrets (
    name       TEXT PRIMARY KEY,
    value      BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
