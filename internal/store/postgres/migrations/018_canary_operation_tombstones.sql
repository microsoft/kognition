-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 018_canary_operation_tombstones: permanent bounded replay fences for completed canaries.

CREATE TABLE IF NOT EXISTS canary_operation_tombstones (
    operation_id   TEXT PRIMARY KEY,
    request_digest TEXT NOT NULL,
    result         BYTEA NOT NULL CHECK (octet_length(result) <= 65536),
    result_digest  TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    completed_at   TIMESTAMPTZ NOT NULL,
    compacted_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE OR REPLACE FUNCTION reject_canary_operation_tombstone_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'canary operation tombstones are immutable';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_canary_operation_tombstones_immutable ON canary_operation_tombstones;
CREATE TRIGGER trg_canary_operation_tombstones_immutable
    BEFORE UPDATE OR DELETE ON canary_operation_tombstones
    FOR EACH ROW EXECUTE FUNCTION reject_canary_operation_tombstone_mutation();