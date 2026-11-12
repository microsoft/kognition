-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 014_canary_finalization: durable acknowledgements for canary terminal effects.

ALTER TABLE canary_operations
    ADD COLUMN IF NOT EXISTS usage_event BYTEA,
    ADD COLUMN IF NOT EXISTS audit_event BYTEA,
    ADD COLUMN IF NOT EXISTS reservation_finalized BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS usage_applied BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS audit_delivered BOOLEAN NOT NULL DEFAULT FALSE;