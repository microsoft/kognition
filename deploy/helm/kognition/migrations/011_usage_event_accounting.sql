-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 011_usage_event_accounting: immutable model usage facts and pricing snapshots.

ALTER TABLE cost_records
    ADD COLUMN IF NOT EXISTS event_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS event_fingerprint TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS reservation_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS model_class TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS model_class_version TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS model_cost_tier TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS pricing_configured BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS input_cost_microusd_per_million_tokens BIGINT NOT NULL DEFAULT 0 CHECK (input_cost_microusd_per_million_tokens >= 0),
    ADD COLUMN IF NOT EXISTS output_cost_microusd_per_million_tokens BIGINT NOT NULL DEFAULT 0 CHECK (output_cost_microusd_per_million_tokens >= 0),
    ADD COLUMN IF NOT EXISTS provider_duration_ms BIGINT NOT NULL DEFAULT 0 CHECK (provider_duration_ms >= 0);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cost_records_event_id
    ON cost_records (event_id)
    WHERE event_id <> '';

CREATE TABLE IF NOT EXISTS usage_event_receipts (
    event_id          TEXT PRIMARY KEY,
    event_fingerprint TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);