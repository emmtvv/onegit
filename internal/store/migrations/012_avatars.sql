-- Profile pictures. The image lives in S3 under avatars/<user_id>; this row
-- says where it came from. 'manual' ones are uploaded by the user; 'oidc'
-- ones are copied from the identity provider's picture claim at sign-in and
-- cannot be changed in onegit.
CREATE TABLE user_avatars (
    user_id      BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    source       TEXT NOT NULL CHECK (source IN ('manual', 'oidc')),
    content_type TEXT NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
