CREATE TABLE previews (
    id              TEXT PRIMARY KEY,
    project         TEXT NOT NULL,
    ref             TEXT NOT NULL,
    state           TEXT NOT NULL,
    sites_json      TEXT NOT NULL DEFAULT '[]',
    session_id      TEXT NOT NULL DEFAULT '',
    error           TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,   -- unix micros UTC
    updated_at      INTEGER NOT NULL,
    started_at      INTEGER NOT NULL DEFAULT 0,
    last_request_at INTEGER NOT NULL DEFAULT 0,
    expires_at      INTEGER NOT NULL DEFAULT 0
);

-- One preview per ref: a second request for a ref that already has one joins
-- the existing preview rather than booting a rival VM under the same name.
CREATE UNIQUE INDEX idx_previews_project_ref ON previews (project, ref);

-- Hostnames are the routing key, so they are a table with a primary key rather
-- than a field inside sites_json: two previews claiming one name has to fail at
-- the write, not be discovered when a request arrives.
CREATE TABLE preview_hosts (
    host       TEXT PRIMARY KEY,
    preview_id TEXT NOT NULL REFERENCES previews(id) ON DELETE CASCADE
);
CREATE INDEX idx_preview_hosts_preview ON preview_hosts (preview_id);
