-- Container registry (OCI distribution). Blob contents live in S3; this is
-- the metadata. Repositories are implicit: a name exists while it has
-- manifests.

-- Content-addressed blobs (layers, configs), shared by all repositories.
-- last_used_at is bumped on upload and existence checks so garbage
-- collection never races a push that is about to reference the blob.
CREATE TABLE registry_blobs (
    digest       TEXT PRIMARY KEY,
    size         BIGINT NOT NULL,
    s3_key       TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- In-progress uploads. Every non-empty PATCH stores one S3 object (listed in
-- order in part_keys); hash_state is the serialised running sha256 so any
-- replica can continue the upload. size doubles as the compare-and-swap
-- version for appends.
CREATE TABLE registry_uploads (
    id         TEXT PRIMARY KEY,
    repo       TEXT NOT NULL,
    size       BIGINT NOT NULL DEFAULT 0,
    part_keys  TEXT[] NOT NULL DEFAULT '{}',
    hash_state BYTEA NOT NULL,
    user_id    BIGINT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE registry_manifests (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    repo       TEXT NOT NULL,
    digest     TEXT NOT NULL,
    media_type TEXT NOT NULL,
    content    BYTEA NOT NULL,
    -- Config + layers for an image manifest, sum of children for an index.
    total_size BIGINT NOT NULL,
    -- "os/arch[/variant]" from the image config; empty for indexes and
    -- non-image artifacts.
    platform   TEXT NOT NULL DEFAULT '',
    pushed_by  BIGINT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (repo, digest)
);

-- Blobs referenced by a manifest. RESTRICT makes a referenced blob
-- undeletable, which is what keeps garbage collection safe.
CREATE TABLE registry_manifest_blobs (
    manifest_id BIGINT NOT NULL REFERENCES registry_manifests(id) ON DELETE CASCADE,
    digest      TEXT NOT NULL REFERENCES registry_blobs(digest) ON DELETE RESTRICT,
    PRIMARY KEY (manifest_id, digest)
);
CREATE INDEX registry_manifest_blobs_digest ON registry_manifest_blobs (digest);

-- Index (multi-platform) manifests → their child manifests.
CREATE TABLE registry_manifest_children (
    parent_id BIGINT NOT NULL REFERENCES registry_manifests(id) ON DELETE CASCADE,
    child_id  BIGINT NOT NULL REFERENCES registry_manifests(id) ON DELETE RESTRICT,
    PRIMARY KEY (parent_id, child_id)
);
CREATE INDEX registry_manifest_children_child ON registry_manifest_children (child_id);

CREATE TABLE registry_tags (
    repo        TEXT NOT NULL,
    name        TEXT NOT NULL,
    manifest_id BIGINT NOT NULL REFERENCES registry_manifests(id) ON DELETE CASCADE,
    pushed_by   BIGINT REFERENCES users(id) ON DELETE SET NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repo, name)
);
CREATE INDEX registry_tags_manifest ON registry_tags (manifest_id);
