-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 019_runtime_approval_foundation.sql
-- ADR-0014 stable standalone identities, TrustProfile incarnation uniqueness,
-- and the first linearizable RuntimeApproval decision ledger.

-- PostgreSQL row identity is the canonical standalone object incarnation.
UPDATE resources
SET data = jsonb_set(data, '{metadata,uid}', to_jsonb(id::text), true);

ALTER TABLE resources DROP CONSTRAINT IF EXISTS uq_resources_kind_ns_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_resources_live_kind_ns_name
    ON resources (kind, namespace, name)
    WHERE deleted_at IS NULL;

-- Bind legacy TrustProfiles to the matching live Agent incarnation. Orphaned
-- profiles receive a stable non-Agent value so Gate 3 rejects them closed.
UPDATE resources AS profile
SET data = jsonb_set(
    profile.data,
    '{spec,agentIncarnationUID}',
        to_jsonb(COALESCE((
                SELECT candidate.id::text
                FROM resources AS candidate
                WHERE candidate.kind = 'Agent'
                    AND candidate.namespace = profile.namespace
                    AND candidate.name = profile.data #>> '{spec,agentRef,name}'
                    AND candidate.deleted_at IS NULL
                LIMIT 1
        ), 'orphan-' || profile.id::text)),
    true
)
WHERE profile.kind = 'TrustProfile'
  AND profile.deleted_at IS NULL
  AND COALESCE(profile.data #>> '{spec,agentIncarnationUID}', '') = '';

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM resources
        WHERE kind = 'TrustProfile' AND deleted_at IS NULL
        GROUP BY namespace, data #>> '{spec,agentIncarnationUID}'
        HAVING COUNT(*) > 1
    ) THEN
        RAISE EXCEPTION 'duplicate live TrustProfiles exist for one Agent incarnation';
    END IF;
END
$$;

CREATE UNIQUE INDEX IF NOT EXISTS idx_trust_profiles_agent_incarnation
    ON resources (namespace, (data #>> '{spec,agentIncarnationUID}'))
    WHERE kind = 'TrustProfile' AND deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS runtime_approvals (
    uid                 UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name                TEXT NOT NULL,
    workspace           TEXT NOT NULL,
    invocation_id       TEXT NOT NULL,
    binding_tag         TEXT NOT NULL,
    binding_key_version BIGINT NOT NULL CHECK (binding_key_version > 0),
    decision_state      TEXT NOT NULL CHECK (decision_state IN ('Pending', 'NotRequired', 'Approved', 'Rejected', 'Expired')),
    execution_state     TEXT NOT NULL CHECK (execution_state IN ('NotStarted', 'Claimed', 'Uncertain', 'Completed', 'Failed')),
    expires_at          TIMESTAMPTZ NOT NULL,
    data                JSONB NOT NULL,
    resource_version    BIGINT NOT NULL DEFAULT 1,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT uq_runtime_approval_name UNIQUE (workspace, name),
    CONSTRAINT uq_runtime_approval_invocation UNIQUE (workspace, invocation_id)
);

CREATE INDEX IF NOT EXISTS idx_runtime_approvals_decision_expiry
    ON runtime_approvals (decision_state, expires_at)
    WHERE decision_state = 'Pending';
