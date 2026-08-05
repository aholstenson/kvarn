-- The mode definition a submission carried inline, as JSON, and the written
-- result the run produced.
--
-- mode_spec_json exists because a mode is no longer only a name the
-- orchestrator can resolve on its own: a caller may supply the whole definition
-- with the request. The backlog stores what was asked for, so the definition
-- has to outlive the request the same way the prompt does. Empty means the
-- session named a mode instead of defining one.
ALTER TABLE sessions ADD COLUMN mode_spec_json TEXT NOT NULL DEFAULT '';

-- result_text is what the run produced in writing: a read-only mode's final
-- answer, or the summary that became the commit message for one that changed
-- something. A mode that delivers nowhere has this as its only output, so it is
-- persisted rather than left in the event log for a reader to reassemble.
ALTER TABLE sessions ADD COLUMN result_text TEXT NOT NULL DEFAULT '';
