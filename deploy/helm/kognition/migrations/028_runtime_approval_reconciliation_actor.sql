-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

ALTER TABLE runtime_approvals
    ADD COLUMN IF NOT EXISTS reconciled_by_issuer TEXT,
    ADD COLUMN IF NOT EXISTS reconciled_by_subject TEXT;

ALTER TABLE runtime_approvals
    DROP CONSTRAINT IF EXISTS chk_runtime_approval_reconciliation_actor;

ALTER TABLE runtime_approvals
    ADD CONSTRAINT chk_runtime_approval_reconciliation_actor CHECK (
        (reconciled_by_issuer IS NULL AND reconciled_by_subject IS NULL)
        OR (
            reconciled_by_issuer IS NOT NULL
            AND reconciled_by_subject IS NOT NULL
            AND char_length(reconciled_by_issuer) BETWEEN 1 AND 512
            AND char_length(reconciled_by_subject) BETWEEN 1 AND 512
        )
    );
