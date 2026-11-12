-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

CREATE TABLE IF NOT EXISTS tool_resilience_state (
    tool_key TEXT PRIMARY KEY,
    state JSONB NOT NULL DEFAULT '{}',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp()
);