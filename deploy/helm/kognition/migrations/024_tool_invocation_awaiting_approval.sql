-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 024_tool_invocation_awaiting_approval.sql
-- Allow Gate 3 invocations to park durably while awaiting a decision and be
-- atomically reclaimed by a same-binding retry after approval.

ALTER TABLE tool_invocation_receipts
    DROP CONSTRAINT IF EXISTS tool_invocation_receipts_status_check;

ALTER TABLE tool_invocation_receipts
    ADD CONSTRAINT tool_invocation_receipts_status_check
    CHECK (status IN ('Pending', 'AwaitingApproval', 'Succeeded', 'Failed'));