-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 004_memory_composite_pk: change memory_vectors PK to (id, volume_id) for
-- cross-volume isolation. Without this, two volumes cannot independently store
-- a record with the same id, and ON CONFLICT (id, volume_id) has no backing
-- unique constraint.

ALTER TABLE memory_vectors DROP CONSTRAINT IF EXISTS memory_vectors_pkey;
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'memory_vectors'::regclass
           AND contype = 'p'
    ) THEN
        ALTER TABLE memory_vectors ADD PRIMARY KEY (id, volume_id);
    END IF;
END $$;
