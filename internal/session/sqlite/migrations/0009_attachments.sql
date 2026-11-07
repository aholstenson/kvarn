-- The files a submission carried. The row holds only their description, as a
-- JSON array, so listings never read the content. The content lives in
-- attachment_blobs keyed by its SHA-256: a retry, or two jobs sent the same
-- file, share one copy. Nothing references a blob by foreign key, because the
-- reference is inside the JSON; pruning sessions also deletes every blob that
-- no remaining session names.
ALTER TABLE sessions ADD COLUMN attachments_json TEXT NOT NULL DEFAULT '[]';

CREATE TABLE attachment_blobs (
    sha256 TEXT PRIMARY KEY,
    data   BLOB NOT NULL
);
