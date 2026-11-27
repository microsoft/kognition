-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

ALTER TABLE runtime_approval_quota_tickets
    ADD COLUMN IF NOT EXISTS actor_kind TEXT NOT NULL DEFAULT 'Agent',
    ADD COLUMN IF NOT EXISTS executor_principal JSONB;
ALTER TABLE runtime_approval_quota_tickets ALTER COLUMN agent_uid DROP NOT NULL;
ALTER TABLE runtime_approval_quota_tickets DROP CONSTRAINT IF EXISTS chk_runtime_approval_quota_actor;
ALTER TABLE runtime_approval_quota_tickets ADD CONSTRAINT chk_runtime_approval_quota_actor CHECK (
    (actor_kind = 'Agent' AND agent_uid IS NOT NULL AND executor_principal IS NULL) OR
    (actor_kind = 'WorkflowExecutor' AND agent_uid IS NULL AND COALESCE(
        jsonb_typeof(executor_principal) = 'object' AND
        length(executor_principal->>'method') BETWEEN 1 AND 64 AND
        length(executor_principal->>'issuer') BETWEEN 1 AND 512 AND
        length(executor_principal->>'subject') BETWEEN 1 AND 512, FALSE))
);
CREATE INDEX IF NOT EXISTS idx_runtime_approval_quota_executor
    ON runtime_approval_quota_tickets (workspace, executor_principal)
    WHERE actor_kind = 'WorkflowExecutor';

ALTER TABLE runtime_approval_tombstones
    ADD COLUMN IF NOT EXISTS actor_kind TEXT NOT NULL DEFAULT 'Agent',
    ADD COLUMN IF NOT EXISTS executor_action JSONB,
    ADD COLUMN IF NOT EXISTS workflow_context JSONB;
ALTER TABLE runtime_approval_tombstones ALTER COLUMN agent_ref DROP NOT NULL;
ALTER TABLE runtime_approval_tombstones DROP CONSTRAINT IF EXISTS chk_runtime_approval_tombstone_actor;
ALTER TABLE runtime_approval_tombstones ADD CONSTRAINT chk_runtime_approval_tombstone_actor CHECK (
    (actor_kind = 'Agent' AND agent_ref IS NOT NULL AND executor_action IS NULL) OR
    (actor_kind = 'WorkflowExecutor' AND agent_ref IS NULL AND
        executor_action IS NOT NULL AND workflow_context IS NOT NULL)
);

CREATE OR REPLACE FUNCTION account_runtime_approval_ticket() RETURNS trigger AS $$
DECLARE
    charge BIGINT;
    actor TEXT;
    agent TEXT;
    executor JSONB;
    workspace_records BIGINT;
    workspace_bytes BIGINT;
    actor_records BIGINT;
    actor_bytes BIGINT;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('kognition.runtime-approval.quota:' || NEW.workspace, 0));
    charge := octet_length(NEW.data::text);
    actor := COALESCE(NULLIF(NEW.data #>> '{spec,actorKind}', ''), 'Agent');
    IF actor = 'WorkflowExecutor' THEN
        executor := NEW.data #> '{spec,executorAction,executor}';
        IF executor IS NULL OR NEW.data #> '{spec,agentRef}' IS NOT NULL THEN
            RAISE EXCEPTION 'RuntimeApproval requires a distinct executor principal';
        END IF;
    ELSIF actor = 'Agent' THEN
        agent := NEW.data #>> '{spec,agentRef,uid}';
        IF COALESCE(agent, '') = '' OR NEW.data #> '{spec,executorAction}' IS NOT NULL THEN
            RAISE EXCEPTION 'RuntimeApproval requires a distinct Agent UID';
        END IF;
    ELSE
        RAISE EXCEPTION 'RuntimeApproval actor kind is unsupported';
    END IF;
    SELECT count(*), COALESCE(sum(charged_bytes), 0)
      INTO workspace_records, workspace_bytes
      FROM runtime_approval_quota_tickets WHERE workspace = NEW.workspace;
    SELECT count(*), COALESCE(sum(charged_bytes), 0)
      INTO actor_records, actor_bytes
      FROM runtime_approval_quota_tickets
     WHERE workspace = NEW.workspace AND actor_kind = actor AND
        ((actor = 'Agent' AND agent_uid = agent) OR
         (actor = 'WorkflowExecutor' AND executor_principal = executor));
    IF workspace_records + 1 > 10000 OR workspace_bytes + charge > 67108864 THEN
        RAISE EXCEPTION 'RuntimeApproval workspace quota exceeded';
    END IF;
    IF actor_records + 1 > 1000 OR actor_bytes + charge > 8388608 THEN
        RAISE EXCEPTION 'RuntimeApproval actor quota exceeded';
    END IF;
    INSERT INTO runtime_approval_quota_tickets
        (workspace, invocation_id, runtime_approval_uid, actor_kind, agent_uid, executor_principal, charged_bytes, phase, created_at)
    VALUES (NEW.workspace, NEW.invocation_id, NEW.uid, actor, agent, executor, charge, 'Live', NEW.created_at);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION compact_runtime_approval(
    target_workspace TEXT, target_invocation_id TEXT,
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
    SELECT * INTO approval FROM runtime_approvals
     WHERE workspace = target_workspace AND invocation_id = target_invocation_id FOR UPDATE;
    IF NOT FOUND THEN RETURN FALSE; END IF;
    IF COALESCE((approval.data #>> '{status,decisionEvidenceDelivered}')::boolean, false) IS NOT TRUE OR NOT (
        (approval.decision_state IN ('Rejected', 'Expired') AND approval.execution_state = 'NotStarted') OR
        (approval.decision_state IN ('Approved', 'NotRequired') AND approval.execution_state IN ('Completed', 'Failed'))
    ) THEN RETURN FALSE; END IF;
    compacted := clock_timestamp();
    INSERT INTO runtime_approval_tombstones (
        workspace, invocation_id, runtime_approval_uid, name, plane, operation, agent_ref,
        actor_kind, executor_action, workflow_context,
        initial_decision, binding_format, binding_algorithm, binding_domain, binding_epoch,
        binding_key_version, binding_tag, decision_state, execution_state, execution_disposition,
        created_at, expires_at, compacted_at, retention_deadline)
    VALUES (
        approval.workspace, approval.invocation_id, approval.uid, approval.name,
        approval.data #>> '{spec,plane}', approval.data #>> '{spec,operation}', approval.data #> '{spec,agentRef}',
        COALESCE(NULLIF(approval.data #>> '{spec,actorKind}', ''), 'Agent'),
        approval.data #> '{spec,executorAction}', approval.data #> '{spec,workflowContext}',
        approval.data #>> '{spec,initialDecision}', approval.data #>> '{spec,binding,format}',
        approval.data #>> '{spec,binding,algorithm}', approval.data #>> '{spec,binding,domain}',
        approval.binding_epoch, approval.binding_key_version, approval.binding_tag,
        approval.decision_state, approval.execution_state, approval.execution_disposition,
        approval.created_at, approval.expires_at, compacted, compacted + retention);
    UPDATE runtime_approval_quota_tickets
       SET phase = 'Compacted', compacted_at = compacted, retention_deadline = compacted + retention
     WHERE workspace = approval.workspace AND invocation_id = approval.invocation_id
       AND runtime_approval_uid = approval.uid AND phase = 'Live';
    IF NOT FOUND THEN RAISE EXCEPTION 'RuntimeApproval quota ticket is missing or inconsistent'; END IF;
    DELETE FROM runtime_approvals WHERE workspace = approval.workspace AND invocation_id = approval.invocation_id;
    RETURN TRUE;
END;
$$ LANGUAGE plpgsql;