-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 013_canary_operations: durable replay fence for ReasoningProbe provider calls.

CREATE TABLE IF NOT EXISTS canary_operations (
    operation_id   TEXT PRIMARY KEY,
    request_digest TEXT NOT NULL,
    phase          TEXT NOT NULL CHECK (phase IN (
        'CanaryClaimed',
        'ProviderStarted',
        'ProviderCompleted',
        'CanaryFinalizing',
        'CanaryCompleted'
    )),
    result         JSONB,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_canary_operations_updated
    ON canary_operations (updated_at);

ALTER TABLE token_reservations
    ADD COLUMN IF NOT EXISTS canary BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE IF NOT EXISTS canary_reservation_dispositions (
    reservation_id TEXT PRIMARY KEY,
    state          TEXT NOT NULL CHECK (state IN ('settled', 'released')),
    input_tokens   BIGINT NOT NULL CHECK (input_tokens >= 0),
    output_tokens  BIGINT NOT NULL CHECK (output_tokens >= 0),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);