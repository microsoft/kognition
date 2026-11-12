-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 027_memory_mutation_receipts: durable Memory store/delete preparation and outcome receipts.

CREATE TABLE IF NOT EXISTS memory_mutation_receipts (
    namespace            TEXT NOT NULL,
    invocation_id        TEXT NOT NULL,
    binding              TEXT NOT NULL,
    operation            TEXT NOT NULL CHECK (operation IN ('store', 'delete')),
    status               TEXT NOT NULL CHECK (status IN ('Prepared', 'Succeeded', 'Failed')),
    result               JSONB,
    error_message        TEXT NOT NULL DEFAULT '',
    preparation_token    TEXT NOT NULL DEFAULT '',
    preparation_deadline TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at         TIMESTAMPTZ,
    PRIMARY KEY (namespace, invocation_id),
    CONSTRAINT memory_mutation_receipts_preparation_check CHECK (
        (status = 'Prepared' AND preparation_token <> '' AND preparation_deadline IS NOT NULL AND completed_at IS NULL)
        OR
        (status <> 'Prepared' AND preparation_token = '' AND preparation_deadline IS NULL AND completed_at IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS idx_memory_mutation_receipts_created
    ON memory_mutation_receipts (created_at);

CREATE INDEX IF NOT EXISTS idx_memory_mutation_receipts_preparation_deadline
    ON memory_mutation_receipts (preparation_deadline, namespace, invocation_id)
    WHERE status = 'Prepared';
