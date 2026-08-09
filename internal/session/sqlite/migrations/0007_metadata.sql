-- Caller-supplied annotations on the submission, as a JSON object.
--
-- They are kvarn's contribution to somebody else's record keeping: the system
-- that submitted the job stamps it with its own identifiers, and reads them
-- back — or finds the job by them — long after the request is gone. Nothing
-- below the request boundary interprets a key, so one document is the right
-- shape; filtering walks it with json_each rather than joining a side table.
ALTER TABLE sessions ADD COLUMN metadata_json TEXT NOT NULL DEFAULT '{}';
