-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 003_instance_events.sql
-- Event log for resource instances (execution tracking).

CREATE TABLE IF NOT EXISTS instance_events (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    kind       TEXT NOT NULL,
    namespace  TEXT NOT NULL,
    name       TEXT NOT NULL,
    event_type TEXT NOT NULL,
    reason     TEXT NOT NULL,
    message    TEXT NOT NULL,
    details    JSONB,
    created_at TIMESTAMPTZ NOT NULL
);

-- Index for listing events of a resource in chronological order.
CREATE INDEX IF NOT EXISTS idx_instance_events_resource ON instance_events (kind, namespace, name, created_at);
