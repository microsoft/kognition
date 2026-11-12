-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 017_usage_purpose: bounded classification for immutable usage events.

ALTER TABLE cost_records
    ADD COLUMN IF NOT EXISTS usage_purpose TEXT NOT NULL DEFAULT ''
        CHECK (usage_purpose IN ('', 'reasoning-probe')),
    ADD COLUMN IF NOT EXISTS usage_estimated BOOLEAN NOT NULL DEFAULT FALSE;
