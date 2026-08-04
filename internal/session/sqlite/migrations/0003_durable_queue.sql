-- The durable backlog. A submission is persisted before its RPC returns and
-- sits in state 'pending' until a dispatcher claims it, so a queued job is a
-- row rather than a goroutine holding a clone and survives a restart.
ALTER TABLE sessions ADD COLUMN key_id    TEXT    NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN priority  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN attempts  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN queued_at INTEGER NOT NULL DEFAULT 0;

-- Sessions written before this migration have queued_at 0, which would read as
-- an infinitely old backlog entry. They are all terminal or about to be
-- reconciled, so seeding from created_at costs one pass and removes the case.
UPDATE sessions SET queued_at = created_at WHERE queued_at = 0;

-- Partial, so the dispatcher's scan covers the backlog rather than the whole
-- session history — which is what lets the backlog be bounded far higher than
-- the in-memory queue it feeds.
CREATE INDEX idx_sessions_pending ON sessions (priority DESC, queued_at ASC)
  WHERE state = 'pending';
