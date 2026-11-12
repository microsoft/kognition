-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 002_revisions.sql
-- Immutable revision history for resources.

CREATE TABLE IF NOT EXISTS resource_revisions (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    kind             TEXT NOT NULL,
    namespace        TEXT NOT NULL,
    name             TEXT NOT NULL,
    revision         BIGINT NOT NULL,
    spec             JSONB NOT NULL,
    data             JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by       TEXT NOT NULL DEFAULT '',

    CONSTRAINT uq_revisions_kind_ns_name_rev UNIQUE (kind, namespace, name, revision)
);

-- Index for listing revisions of a resource in order.
CREATE INDEX IF NOT EXISTS idx_revisions_resource ON resource_revisions (kind, namespace, name, revision);
