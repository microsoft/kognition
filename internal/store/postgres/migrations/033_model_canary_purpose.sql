-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

ALTER TABLE cost_records
    DROP CONSTRAINT IF EXISTS cost_records_usage_purpose_check;

ALTER TABLE cost_records
    ADD CONSTRAINT cost_records_usage_purpose_check
        CHECK (usage_purpose IN ('', 'reasoning-probe', 'model-version-canary'));
