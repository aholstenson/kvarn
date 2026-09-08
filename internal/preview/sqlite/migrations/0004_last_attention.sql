-- Idle reaping now measures when a person was last plausibly looking at a
-- preview, not merely when a request last arrived: a page left open in a
-- background tab polls on its own and used to hold a VM up until the lifetime
-- cap. Ingress grades each request and stamps this column only for the ones a
-- person produces.
--
-- 0 for an existing row means "never seen", which readers resolve to the row's
-- last request time, so an upgrade does not reap everything that is running.
ALTER TABLE previews ADD COLUMN last_attention_at INTEGER NOT NULL DEFAULT 0;
