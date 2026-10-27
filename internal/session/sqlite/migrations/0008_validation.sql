-- The validation steps a submission added to the ones its project declares,
-- as JSON. They are part of what was asked for, so they wait in the backlog
-- with the prompt and travel with a retry. Empty means the submission added
-- none.
ALTER TABLE sessions ADD COLUMN validation_json TEXT NOT NULL DEFAULT '';
