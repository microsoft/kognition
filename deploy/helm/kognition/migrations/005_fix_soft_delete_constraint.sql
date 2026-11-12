-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 005_fix_soft_delete_constraint.sql
-- Fix: unique constraint must exclude soft-deleted rows to allow re-creation
-- of resources that were previously deleted.

-- Drop the old constraint that blocks re-creation of soft-deleted resources.
ALTER TABLE resources DROP CONSTRAINT IF EXISTS uq_resources_kind_ns_name;

-- Create a partial unique index that only enforces uniqueness among live rows.
CREATE UNIQUE INDEX IF NOT EXISTS uq_resources_kind_ns_name ON resources (kind, namespace, name) WHERE deleted_at IS NULL;
