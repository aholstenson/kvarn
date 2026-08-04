-- Whether the submission named an existing pull request to continue, rather
-- than asking for a new one. pr_ref cannot answer this on its own: a fresh job
-- acquires one too, the moment it opens its pull request. Telling the two apart
-- is what lets a retry resubmit the run it is retrying instead of guessing, and
-- what stops a finished fresh job from being resubmitted into a second pull
-- request for the same task.
ALTER TABLE sessions ADD COLUMN continuation INTEGER NOT NULL DEFAULT 0;

-- Backfill: before this column existed, a continuation was exactly a session
-- created in feedback mode with a pull request already attached, which is the
-- rule the code applied.
UPDATE sessions SET continuation = 1 WHERE mode = 'feedback' AND pr_ref != '';
