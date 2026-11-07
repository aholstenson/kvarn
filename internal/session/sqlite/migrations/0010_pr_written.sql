-- The title and body this session last wrote to its pull request, as a JSON
-- object, or '' when it wrote none. It is what makes a pull request ours to
-- revise: a follow-up rewrites a field only while the forge still holds exactly
-- what kvarn wrote there, so a description a person edited stays as they left
-- it.
ALTER TABLE sessions ADD COLUMN pr_written_json TEXT NOT NULL DEFAULT '';
