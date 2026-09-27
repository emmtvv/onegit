-- A requirement can exempt commits already on some branches (e.g. require an
-- approval for development deploys unless the commit is on main).
ALTER TABLE deploy_rules ADD COLUMN except_branches TEXT[] NOT NULL DEFAULT '{}';
