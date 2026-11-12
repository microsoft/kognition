-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 025_tool_invocation_preparation: persist the shared pre-dispatch preparation fence.

ALTER TABLE tool_invocation_receipts
    ADD COLUMN IF NOT EXISTS preparation_token TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS preparation_deadline TIMESTAMPTZ;

ALTER TABLE tool_invocation_receipts
    DROP CONSTRAINT IF EXISTS tool_invocation_receipts_status_check;

ALTER TABLE tool_invocation_receipts
    ADD CONSTRAINT tool_invocation_receipts_status_check
    CHECK (status IN ('Pending', 'AwaitingApproval', 'Prepared', 'Succeeded', 'Failed'));

ALTER TABLE tool_invocation_receipts
    ADD CONSTRAINT tool_invocation_receipts_preparation_check
    CHECK (
        (status = 'Prepared' AND preparation_token <> '' AND preparation_deadline IS NOT NULL)
        OR
        (status <> 'Prepared' AND preparation_token = '' AND preparation_deadline IS NULL)
    );