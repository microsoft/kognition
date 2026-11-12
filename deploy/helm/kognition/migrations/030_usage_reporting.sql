-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

ALTER TABLE cost_records
    ADD COLUMN IF NOT EXISTS usage_reported BOOLEAN NOT NULL DEFAULT FALSE;