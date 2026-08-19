-- A preview can now keep declared state between boots: it is tarred out of the
-- guest on every graceful stop and unpacked again on the next boot. These
-- columns describe the archive on disk (when it was written, how large it is,
-- and why the last attempt to write one failed), plus the one fact that decides
-- whether an archive may exist at all — a preview of a fork's pull request
-- never keeps state.
ALTER TABLE previews ADD COLUMN state_saved_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE previews ADD COLUMN state_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE previews ADD COLUMN state_error TEXT NOT NULL DEFAULT '';
ALTER TABLE previews ADD COLUMN fork INTEGER NOT NULL DEFAULT 0;
