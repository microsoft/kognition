-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 022_runtime_approval_execution.sql
-- ADR-0014 execution-state machine columns and reaping index for the
-- RuntimeApproval durable claim, preparation deadline, fencing token, and
-- Uncertain-recovery evidence references. All columns are nullable because a
-- freshly created RuntimeApproval is Pending/NotRequired with execution
-- NotStarted, and Prepare/Claim/Finalize/Reap transitions populate them in
-- monotone order under CAS on resource_version.

ALTER TABLE runtime_approvals
    ADD COLUMN IF NOT EXISTS execution_disposition       TEXT,
    ADD COLUMN IF NOT EXISTS preparation_token           TEXT,
    ADD COLUMN IF NOT EXISTS preparation_deadline        TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS claim_workload_uid          TEXT,
    ADD COLUMN IF NOT EXISTS claim_process_start_nonce   TEXT,
    ADD COLUMN IF NOT EXISTS claim_fencing_token         BIGINT,
    ADD COLUMN IF NOT EXISTS claim_claimed_at            TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS claim_last_heartbeat        TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS claim_lease_deadline        TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS termination_proof_ref       TEXT,
    ADD COLUMN IF NOT EXISTS reconciliation_evidence_ref TEXT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'chk_runtime_approval_execution_disposition'
          AND conrelid = 'runtime_approvals'::regclass
    ) THEN
        ALTER TABLE runtime_approvals
            ADD CONSTRAINT chk_runtime_approval_execution_disposition
            CHECK (execution_disposition IS NULL OR execution_disposition IN (
                'QuotaDenied', 'ReceiptConflict', 'PolicyChanged', 'TargetChanged',
                'SessionChanged', 'PreparationRejected', 'ApprovalExpiredBeforeClaim',
                'AbandonedPreparation', 'ReconciledCompleted', 'ReconciledNotExecuted',
                'Succeeded', 'Failed'
            ));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'chk_runtime_approval_preparation_fields'
          AND conrelid = 'runtime_approvals'::regclass
    ) THEN
        ALTER TABLE runtime_approvals
            ADD CONSTRAINT chk_runtime_approval_preparation_fields
            CHECK ((preparation_token IS NULL AND preparation_deadline IS NULL)
                OR (preparation_token IS NOT NULL AND preparation_deadline IS NOT NULL
                    AND char_length(preparation_token) BETWEEN 32 AND 128));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'chk_runtime_approval_claim_fields'
          AND conrelid = 'runtime_approvals'::regclass
    ) THEN
        ALTER TABLE runtime_approvals
            ADD CONSTRAINT chk_runtime_approval_claim_fields
            CHECK (
                (claim_workload_uid IS NULL
                    AND claim_process_start_nonce IS NULL
                    AND claim_fencing_token IS NULL
                    AND claim_claimed_at IS NULL)
                OR
                (claim_workload_uid IS NOT NULL
                    AND char_length(claim_workload_uid) BETWEEN 1 AND 253
                    AND claim_process_start_nonce IS NOT NULL
                    AND char_length(claim_process_start_nonce) BETWEEN 16 AND 128
                    AND claim_fencing_token IS NOT NULL
                    AND claim_fencing_token > 0
                    AND claim_claimed_at IS NOT NULL)
            );
    END IF;
END
$$;

CREATE INDEX IF NOT EXISTS idx_runtime_approvals_preparation_reap
    ON runtime_approvals (preparation_deadline)
    WHERE execution_state = 'NotStarted' AND preparation_token IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_runtime_approvals_claim_lease
    ON runtime_approvals (claim_lease_deadline)
    WHERE execution_state = 'Claimed';
