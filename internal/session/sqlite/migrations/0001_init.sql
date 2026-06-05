CREATE TABLE sessions (
    id               TEXT PRIMARY KEY,
    project_name     TEXT NOT NULL,
    prompt           TEXT NOT NULL,
    mode             TEXT NOT NULL,
    state            TEXT NOT NULL,
    message          TEXT NOT NULL DEFAULT '',
    error            TEXT NOT NULL DEFAULT '',
    pull_request_url TEXT NOT NULL DEFAULT '',
    cost_json        TEXT NOT NULL DEFAULT '{}',
    created_at       INTEGER NOT NULL,   -- unix micros UTC
    updated_at       INTEGER NOT NULL
);
CREATE INDEX idx_sessions_project_created ON sessions (project_name, created_at DESC, id DESC);
CREATE INDEX idx_sessions_created         ON sessions (created_at DESC, id DESC);

CREATE TABLE session_events (
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    seq         INTEGER NOT NULL,   -- per-session, starts at 1
    kind        TEXT NOT NULL,
    payload     BLOB NOT NULL,      -- JSON
    recorded_at INTEGER NOT NULL,
    PRIMARY KEY (session_id, seq)
);
