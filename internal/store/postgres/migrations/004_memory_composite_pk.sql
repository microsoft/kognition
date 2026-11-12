-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 004_memory_composite_pk: change memory_vectors PK to (id, volume_id) for
-- cross-volume isolation. Without this, two volumes cannot independently store
-- a record with the same id, and ON CONFLICT (id, volume_id) has no backing
-- unique constraint.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM   pg_index i
        JOIN   pg_class c ON c.oid = i.indrelid
        JOIN   pg_attribute a ON a.attrelid = c.oid AND a.attnum = ANY (i.indkey)
        WHERE  i.indisprimary
        AND    c.relname = 'memory_vectors'
        GROUP  BY i.indexrelid
        HAVING array_agg(a.attname ORDER BY array_position(i.indkey, a.attnum))
               <> ARRAY['id','volume_id']::name[]
    ) THEN
        ALTER TABLE memory_vectors DROP CONSTRAINT memory_vectors_pkey;
        ALTER TABLE memory_vectors ADD PRIMARY KEY (id, volume_id);
    END IF;
END$$;
