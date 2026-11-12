-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 023_runtime_approval_compaction.sql
-- Transactional RuntimeApproval quota accounting, bounded replay tombstones,
-- compaction, and retention parity with Kubernetes mode.

CREATE TABLE IF NOT EXISTS runtime_approval_quota_tickets (
    workspace            TEXT NOT NULL,
    invocation_id        TEXT NOT NULL,
    runtime_approval_uid UUID NOT NULL,
    agent_uid            TEXT NOT NULL,
    charged_bytes        BIGINT NOT NULL CHECK (charged_bytes > 0),
    phase                TEXT NOT NULL CHECK (phase IN ('Live', 'Compacted')),
    created_at           TIMESTAMPTZ NOT NULL,
    compacted_at         TIMESTAMPTZ,
    retention_deadline   TIMESTAMPTZ,
    PRIMARY KEY (workspace, invocation_id),
    CONSTRAINT chk_runtime_approval_ticket_phase_fields CHECK (
        (phase = 'Live' AND compacted_at IS NULL AND retention_deadline IS NULL) OR
        (phase = 'Compacted' AND compacted_at IS NOT NULL AND retention_deadline > compacted_at)
    )
);

CREATE INDEX IF NOT EXISTS idx_runtime_approval_quota_tickets_agent
    ON runtime_approval_quota_tickets (workspace, agent_uid);

CREATE TABLE IF NOT EXISTS runtime_approval_tombstones (
    workspace                  TEXT NOT NULL,
    invocation_id              TEXT NOT NULL,
    runtime_approval_uid       UUID NOT NULL,
    name                       TEXT NOT NULL,
    plane                      TEXT NOT NULL,
    operation                  TEXT NOT NULL,
    agent_ref                  JSONB NOT NULL,
    initial_decision           TEXT NOT NULL CHECK (initial_decision IN ('Pending', 'NotRequired')),
    binding_format             TEXT NOT NULL,
    binding_algorithm          TEXT NOT NULL,
    binding_domain             TEXT NOT NULL,
    binding_epoch              BIGINT NOT NULL CHECK (binding_epoch > 0),
    binding_key_version        BIGINT NOT NULL CHECK (binding_key_version > 0),
    binding_tag                TEXT NOT NULL,
    decision_state             TEXT NOT NULL CHECK (decision_state IN ('Approved', 'Rejected', 'Expired', 'NotRequired')),
    execution_state            TEXT NOT NULL CHECK (execution_state IN ('NotStarted', 'Completed', 'Failed')),
    execution_disposition      TEXT,
    created_at                 TIMESTAMPTZ NOT NULL,
    expires_at                 TIMESTAMPTZ NOT NULL,
    compacted_at               TIMESTAMPTZ NOT NULL,
    retention_deadline         TIMESTAMPTZ NOT NULL CHECK (retention_deadline > compacted_at),
    PRIMARY KEY (workspace, invocation_id),
    CONSTRAINT uq_runtime_approval_tombstone_uid UNIQUE (runtime_approval_uid),
    CONSTRAINT chk_runtime_approval_tombstone_terminal CHECK (
        (decision_state IN ('Rejected', 'Expired') AND execution_state = 'NotStarted') OR
        (decision_state IN ('Approved', 'NotRequired') AND execution_state IN ('Completed', 'Failed'))
    )
);

CREATE INDEX IF NOT EXISTS idx_runtime_approval_tombstones_retention
    ON runtime_approval_tombstones (retention_deadline);

CREATE OR REPLACE FUNCTION account_runtime_approval_ticket() RETURNS trigger AS $$
DECLARE
    charge BIGINT;
    agent TEXT;
    workspace_records BIGINT;
    workspace_bytes BIGINT;
    agent_records BIGINT;
    agent_bytes BIGINT;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('kognition.runtime-approval.quota:' || NEW.workspace, 0));
    charge := octet_length(NEW.data::text);
    agent := NEW.data #>> '{spec,agentRef,uid}';
    IF COALESCE(agent, '') = '' THEN
        RAISE EXCEPTION 'RuntimeApproval Agent UID is required for quota accounting';
    END IF;

    SELECT count(*), COALESCE(sum(charged_bytes), 0)
      INTO workspace_records, workspace_bytes
      FROM runtime_approval_quota_tickets
     WHERE workspace = NEW.workspace;
    SELECT count(*), COALESCE(sum(charged_bytes), 0)
      INTO agent_records, agent_bytes
      FROM runtime_approval_quota_tickets
     WHERE workspace = NEW.workspace AND agent_uid = agent;
    IF workspace_records + 1 > 10000 OR workspace_bytes + charge > 67108864 THEN
        RAISE EXCEPTION 'RuntimeApproval workspace quota exceeded';
    END IF;
    IF agent_records + 1 > 1000 OR agent_bytes + charge > 8388608 THEN
        RAISE EXCEPTION 'RuntimeApproval Agent quota exceeded';
    END IF;

    INSERT INTO runtime_approval_quota_tickets
        (workspace, invocation_id, runtime_approval_uid, agent_uid, charged_bytes, phase, created_at)
    VALUES
        (NEW.workspace, NEW.invocation_id, NEW.uid, agent, charge, 'Live', NEW.created_at);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_runtime_approval_ticket_accounting ON runtime_approvals;
CREATE TRIGGER trg_runtime_approval_ticket_accounting
    AFTER INSERT ON runtime_approvals
    FOR EACH ROW EXECUTE FUNCTION account_runtime_approval_ticket();

CREATE OR REPLACE FUNCTION compact_runtime_approval(
    target_workspace TEXT,
    target_invocation_id TEXT,
    retention INTERVAL DEFAULT interval '90 days'
) RETURNS BOOLEAN AS $$
DECLARE
    approval runtime_approvals%ROWTYPE;
    compacted TIMESTAMPTZ;
BEGIN
    IF retention < interval '90 days' THEN
        RAISE EXCEPTION 'RuntimeApproval retention must be at least 90 days';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('kognition.runtime-approval.quota:' || target_workspace, 0));
    SELECT * INTO approval
      FROM runtime_approvals
     WHERE workspace = target_workspace AND invocation_id = target_invocation_id
     FOR UPDATE;
    IF NOT FOUND THEN
        RETURN FALSE;
    END IF;
    IF COALESCE((approval.data #>> '{status,decisionEvidenceDelivered}')::boolean, false) IS NOT TRUE OR NOT (
        (approval.decision_state IN ('Rejected', 'Expired') AND approval.execution_state = 'NotStarted') OR
        (approval.decision_state IN ('Approved', 'NotRequired') AND approval.execution_state IN ('Completed', 'Failed'))
    ) THEN
        RETURN FALSE;
    END IF;
    compacted := clock_timestamp();
    INSERT INTO runtime_approval_tombstones (
        workspace, invocation_id, runtime_approval_uid, name, plane, operation, agent_ref,
        initial_decision, binding_format, binding_algorithm, binding_domain, binding_epoch,
        binding_key_version, binding_tag, decision_state, execution_state, execution_disposition,
        created_at, expires_at, compacted_at, retention_deadline)
    VALUES (
        approval.workspace, approval.invocation_id, approval.uid, approval.name,
        approval.data #>> '{spec,plane}', approval.data #>> '{spec,operation}', approval.data #> '{spec,agentRef}',
        approval.data #>> '{spec,initialDecision}', approval.data #>> '{spec,binding,format}',
        approval.data #>> '{spec,binding,algorithm}', approval.data #>> '{spec,binding,domain}',
        approval.binding_epoch, approval.binding_key_version, approval.binding_tag,
        approval.decision_state, approval.execution_state, approval.execution_disposition,
        approval.created_at, approval.expires_at, compacted, compacted + retention);
    UPDATE runtime_approval_quota_tickets
       SET phase = 'Compacted', compacted_at = compacted, retention_deadline = compacted + retention
     WHERE workspace = approval.workspace AND invocation_id = approval.invocation_id
       AND runtime_approval_uid = approval.uid AND phase = 'Live';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'RuntimeApproval quota ticket is missing or inconsistent';
    END IF;
    DELETE FROM runtime_approvals
     WHERE workspace = approval.workspace AND invocation_id = approval.invocation_id;
    RETURN TRUE;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION retire_runtime_approval_tombstones(cutoff TIMESTAMPTZ) RETURNS BIGINT AS $$
DECLARE
    retired BIGINT;
BEGIN
    WITH due AS (
        SELECT workspace, invocation_id
          FROM runtime_approval_tombstones
         WHERE retention_deadline <= cutoff
         FOR UPDATE
    ), deleted_tickets AS (
        DELETE FROM runtime_approval_quota_tickets AS ticket
         USING due
         WHERE ticket.workspace = due.workspace AND ticket.invocation_id = due.invocation_id
           AND ticket.phase = 'Compacted'
    ), deleted_tombstones AS (
        DELETE FROM runtime_approval_tombstones AS tombstone
         USING due
         WHERE tombstone.workspace = due.workspace AND tombstone.invocation_id = due.invocation_id
         RETURNING 1
    )
    SELECT count(*) INTO retired FROM deleted_tombstones;
    RETURN retired;
END;
$$ LANGUAGE plpgsql;
