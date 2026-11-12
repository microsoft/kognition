-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 009_cognitive_audit_log: append-only CognitiveAuditLog persistence.

CREATE TABLE IF NOT EXISTS cognitive_audit_log (
    id             TEXT PRIMARY KEY,
    created_at     TIMESTAMPTZ NOT NULL,
    agent_identity TEXT NOT NULL DEFAULT '',
    workspace      TEXT NOT NULL,
    solution       TEXT NOT NULL DEFAULT '',
    solution_rev   BIGINT NOT NULL DEFAULT 0,
    session_id     TEXT NOT NULL DEFAULT '',
    event_type     TEXT NOT NULL,
    subject_kind   TEXT NOT NULL DEFAULT '',
    subject_ref    TEXT NOT NULL DEFAULT '',
    verb           TEXT NOT NULL DEFAULT '',
    details        JSONB,
    trace_id       TEXT NOT NULL DEFAULT '',
    span_id        TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_cognitive_audit_log_workspace_created
    ON cognitive_audit_log (workspace, created_at, id);

CREATE INDEX IF NOT EXISTS idx_cognitive_audit_log_session_created
    ON cognitive_audit_log (session_id, created_at, id)
    WHERE session_id <> '';

CREATE OR REPLACE FUNCTION reject_cognitive_audit_log_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'cognitive_audit_log is append-only';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS cognitive_audit_log_append_only ON cognitive_audit_log;
CREATE TRIGGER cognitive_audit_log_append_only
    BEFORE UPDATE OR DELETE ON cognitive_audit_log
    FOR EACH ROW EXECUTE FUNCTION reject_cognitive_audit_log_mutation();