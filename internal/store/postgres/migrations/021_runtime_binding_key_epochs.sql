-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 021_runtime_binding_key_epochs.sql
-- Durable serialization witness for RuntimeApproval binding-key activation.

ALTER TABLE runtime_approvals
    ALTER COLUMN binding_key_version TYPE BIGINT,
    ADD COLUMN IF NOT EXISTS binding_epoch BIGINT NOT NULL DEFAULT 1 CHECK (binding_epoch > 0);

UPDATE runtime_approvals
SET data = jsonb_set(
        jsonb_set(data, '{spec,binding,epoch}', to_jsonb(binding_epoch), true),
        '{spec,initialDecision}',
        to_jsonb(CASE WHEN decision_state = 'NotRequired' THEN 'NotRequired' ELSE 'Pending' END),
        true
    )
WHERE data #> '{spec,binding,epoch}' IS NULL
   OR data #> '{spec,initialDecision}' IS NULL;

CREATE TABLE IF NOT EXISTS runtime_binding_key_state (
    singleton          BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    epoch              BIGINT NOT NULL CHECK (epoch > 0),
    active_key_version BIGINT NOT NULL CHECK (active_key_version > 0),
    key_metadata       JSONB NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp()
);