-- The delivery of a run's output to the forge, kept until it has finished. A
-- forge outage can outlast both the run and the process that started the
-- delivery, so the plan and its retry schedule must be a row, not a goroutine.
-- A session with a row here is left alone by startup reconciliation. The row
-- goes with its session when the session is pruned.
CREATE TABLE deliveries (
    session_id      TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    plan            BLOB NOT NULL,              -- JSON, owned by the orchestrator
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,           -- unix micros UTC
    deadline        INTEGER NOT NULL,           -- unix micros UTC
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,           -- unix micros UTC
    updated_at      INTEGER NOT NULL
);
