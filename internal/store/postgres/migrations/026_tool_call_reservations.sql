-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 026_tool_call_reservations: durable invocation-keyed Tool quota preparation.

CREATE TABLE IF NOT EXISTS tool_call_reservations (
    namespace            TEXT NOT NULL,
    invocation_id        TEXT NOT NULL,
    binding              TEXT NOT NULL,
    preparation_token    TEXT NOT NULL,
    preparation_deadline TIMESTAMPTZ NOT NULL,
    quota_periods         JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (namespace, invocation_id),
    CHECK (binding <> ''),
    CHECK (preparation_token <> '')
);

CREATE INDEX IF NOT EXISTS idx_tool_call_reservations_deadline
    ON tool_call_reservations (preparation_deadline);