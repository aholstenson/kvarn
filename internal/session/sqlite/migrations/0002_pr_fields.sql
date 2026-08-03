ALTER TABLE sessions ADD COLUMN pr_ref            TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN head_branch       TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN base_branch       TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN parent_session_id TEXT NOT NULL DEFAULT '';

-- Serves both the per-PR single-flight check and the parent lookup. Partial so
-- the index only covers sessions that actually have a pull request.
CREATE INDEX idx_sessions_project_pr ON sessions (project_name, pr_ref) WHERE pr_ref != '';
