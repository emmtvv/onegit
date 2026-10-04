-- Code owners of open pull requests, so "PRs waiting on my review" is an
-- index lookup instead of reading CODEOWNERS and diffing every open PR.
-- Kept by the pulls background loop from changed_files and the base
-- branch's owner rules; owner_key is user:<name>, team:<name> or
-- email:<address>, lower-cased.
CREATE TABLE pull_owners (
    pull_id   BIGINT NOT NULL REFERENCES pulls(id) ON DELETE CASCADE,
    pattern   TEXT NOT NULL,
    owner_key TEXT NOT NULL,
    PRIMARY KEY (pull_id, pattern, owner_key)
);
CREATE INDEX pull_owners_key ON pull_owners (owner_key, pull_id);

-- The owner rules (CODEOWNERS and branch protection owners) pull_owners
-- was computed from; '' means not computed for the current changed files.
ALTER TABLE pulls ADD COLUMN owners_rev TEXT NOT NULL DEFAULT '';
CREATE INDEX pulls_open_base ON pulls (base_branch, owners_rev) WHERE state = 'open';

-- A user's verdicts, to find open PRs that changed since they reviewed.
CREATE INDEX pull_reviews_reviewer ON pull_reviews (reviewer_id, pull_id) WHERE state <> 'commented';

-- Only open PRs keep owners: closing drops them, reopening recomputes them.
CREATE FUNCTION pulls_owners_on_state() RETURNS trigger AS $$
BEGIN
    IF NEW.state <> OLD.state THEN
        DELETE FROM pull_owners WHERE pull_id = OLD.id;
        NEW.owners_rev := '';
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER pulls_owners_on_state BEFORE UPDATE OF state ON pulls
    FOR EACH ROW EXECUTE FUNCTION pulls_owners_on_state();
