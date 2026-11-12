-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 016_canary_reservation_presence: persist whether pre-call finalization must release quota.

ALTER TABLE canary_operations
    ADD COLUMN IF NOT EXISTS reservation_created BOOLEAN NOT NULL DEFAULT FALSE;