-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

ALTER TABLE cost_records
    ADD COLUMN IF NOT EXISTS agent TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS workflow TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS step TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_cost_records_agent_attribution
    ON cost_records (workspace, agent, created_at DESC)
    WHERE agent <> '';

CREATE INDEX IF NOT EXISTS idx_cost_records_workflow_attribution
    ON cost_records (workspace, workflow, step, created_at DESC)
    WHERE workflow <> '';