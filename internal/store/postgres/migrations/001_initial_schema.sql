-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 001_initial_schema.sql
-- Initial database schema for kognition control plane.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "vector";

-- resources: generic resource storage table
CREATE TABLE IF NOT EXISTS resources (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    kind             TEXT NOT NULL,
    namespace        TEXT NOT NULL DEFAULT 'default',
    name             TEXT NOT NULL,
    data             JSONB NOT NULL,
    resource_version BIGINT NOT NULL DEFAULT 1,
    generation       BIGINT NOT NULL DEFAULT 1,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at       TIMESTAMPTZ,

    CONSTRAINT uq_resources_kind_ns_name UNIQUE (kind, namespace, name)
);

-- Index for listing by kind + namespace
CREATE INDEX IF NOT EXISTS idx_resources_kind_ns ON resources (kind, namespace) WHERE deleted_at IS NULL;

-- Index for watch (ordering by resource_version)
CREATE INDEX IF NOT EXISTS idx_resources_watch ON resources (kind, resource_version) WHERE deleted_at IS NULL;

-- workspaces: tenant isolation
CREATE TABLE IF NOT EXISTS workspaces (
    name        TEXT PRIMARY KEY,
    display_name TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at  TIMESTAMPTZ
);

-- Insert default workspace
INSERT INTO workspaces (name, display_name) VALUES ('default', 'Default Workspace')
ON CONFLICT DO NOTHING;

-- memory_vectors: vector storage for agent memory (pgvector)
CREATE TABLE IF NOT EXISTS memory_vectors (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    volume_id   UUID NOT NULL,
    content     TEXT NOT NULL,
    embedding   vector(1536),
    metadata    JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- HNSW index for similarity search
CREATE INDEX IF NOT EXISTS idx_memory_vectors_embedding ON memory_vectors
    USING hnsw (embedding vector_cosine_ops);

-- events: audit trail and watch notification source
CREATE TABLE IF NOT EXISTS events (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    event_type      TEXT NOT NULL, -- CREATED, UPDATED, DELETED
    resource_kind   TEXT NOT NULL,
    resource_ns     TEXT NOT NULL,
    resource_name   TEXT NOT NULL,
    resource_version BIGINT NOT NULL,
    payload         JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_events_kind_version ON events (resource_kind, resource_version);

-- Notify channel for watch
CREATE OR REPLACE FUNCTION notify_resource_change() RETURNS trigger AS $$
DECLARE
    op TEXT := TG_ARGV[0];
BEGIN
    -- Detect soft-delete: if deleted_at was just set, treat as DELETE.
    IF op = 'UPDATE' AND NEW.deleted_at IS NOT NULL AND (OLD.deleted_at IS NULL) THEN
        op := 'DELETE';
    END IF;

    PERFORM pg_notify('resource_changes', json_build_object(
        'operation', op,
        'kind', NEW.kind,
        'namespace', NEW.namespace,
        'name', NEW.name,
        'resource_version', NEW.resource_version
    )::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE TRIGGER trg_resources_insert
    AFTER INSERT ON resources
    FOR EACH ROW EXECUTE FUNCTION notify_resource_change('INSERT');

CREATE OR REPLACE TRIGGER trg_resources_update
    AFTER UPDATE ON resources
    FOR EACH ROW EXECUTE FUNCTION notify_resource_change('UPDATE');
