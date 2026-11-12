-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 008_tool_invocation_receipts: durable exactly-once Tool Gateway receipts.

CREATE TABLE IF NOT EXISTS tool_invocation_receipts (
    namespace      TEXT NOT NULL,
    invocation_id  TEXT NOT NULL,
    binding        TEXT NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('Pending', 'Succeeded', 'Failed')),
    output         JSONB,
    execution_mode TEXT NOT NULL DEFAULT '',
    duration_ms    BIGINT NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    error_message  TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at   TIMESTAMPTZ,
    PRIMARY KEY (namespace, invocation_id)
);

CREATE INDEX IF NOT EXISTS idx_tool_invocation_receipts_created
    ON tool_invocation_receipts (created_at);