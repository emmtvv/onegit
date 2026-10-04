-- The Home page: a user's open pull requests and the runs they started.
CREATE INDEX pulls_open_author ON pulls (author_id, id DESC) WHERE state = 'open';
CREATE INDEX ci_runs_triggered_by ON ci_runs (triggered_by, id DESC) WHERE triggered_by IS NOT NULL;
