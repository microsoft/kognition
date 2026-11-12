-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 020_tool_grant_rate_limits.sql
-- Durable rate counters shared by standalone API server replicas.

CREATE TABLE IF NOT EXISTS tool_grant_rate_limits (
    rate_key     TEXT PRIMARY KEY,
    minute_count BIGINT NOT NULL DEFAULT 0 CHECK (minute_count >= 0),
    hour_count   BIGINT NOT NULL DEFAULT 0 CHECK (hour_count >= 0),
    day_count    BIGINT NOT NULL DEFAULT 0 CHECK (day_count >= 0),
    minute_reset TIMESTAMPTZ NOT NULL,
    hour_reset   TIMESTAMPTZ NOT NULL,
    day_reset    TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp()
);