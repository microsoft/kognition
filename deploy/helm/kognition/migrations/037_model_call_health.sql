-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

CREATE TABLE IF NOT EXISTS model_call_health (
    reporter TEXT NOT NULL,
    target TEXT NOT NULL,
    calls INTEGER NOT NULL CHECK (calls >= 0),
    faults INTEGER NOT NULL CHECK (faults >= 0 AND faults <= calls),
    consecutive_faults INTEGER NOT NULL CHECK (consecutive_faults >= 0),
    consecutive_successes INTEGER NOT NULL CHECK (consecutive_successes >= 0),
    streak_started_at TIMESTAMPTZ,
    last_fault_reason TEXT NOT NULL DEFAULT '',
    last_fault_at TIMESTAMPTZ,
    last_success_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    PRIMARY KEY (reporter, target)
);

-- Databases that applied the first revision of this migration have the table already.
ALTER TABLE model_call_health ADD COLUMN IF NOT EXISTS streak_started_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS model_call_health_updated_at_idx ON model_call_health (updated_at);
