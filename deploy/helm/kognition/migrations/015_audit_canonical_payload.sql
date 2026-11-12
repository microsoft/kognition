-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 015_audit_canonical_payload: preserve exact immutable audit bytes for duplicate fencing.

ALTER TABLE cognitive_audit_log
    ADD COLUMN IF NOT EXISTS canonical_payload BYTEA;

UPDATE cognitive_audit_log
SET canonical_payload = convert_to(details::text, 'UTF8')
WHERE canonical_payload IS NULL;

ALTER TABLE cognitive_audit_log
    ALTER COLUMN canonical_payload SET NOT NULL;