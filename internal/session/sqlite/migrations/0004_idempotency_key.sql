-- A caller-chosen key that makes StartJob safe to retry: the client that times
-- out mid-request replays it and gets the session the first attempt created
-- rather than a second VM and a second pull request.
ALTER TABLE sessions ADD COLUMN idempotency_key TEXT NOT NULL DEFAULT '';

-- Partial, so only sessions that actually claim a key take part: '' is "no key
-- supplied" and every keyless session must remain free to coexist. The unique
-- index is what settles two concurrent copies of one retried request — the
-- loser gets a constraint violation and reads the winner's session back.
CREATE UNIQUE INDEX idx_sessions_idempotency
  ON sessions (project_name, idempotency_key)
  WHERE idempotency_key != '';
