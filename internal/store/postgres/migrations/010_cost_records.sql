-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 010_cost_records: durable normalized cost attribution records.

CREATE TABLE IF NOT EXISTS cost_records (
    id                BIGSERIAL PRIMARY KEY,
    created_at        TIMESTAMPTZ NOT NULL,
    workspace         TEXT NOT NULL,
    agent_identity    TEXT NOT NULL DEFAULT '',
    agent_namespace   TEXT NOT NULL DEFAULT '',
    solution          TEXT NOT NULL DEFAULT '',
    solution_rev      BIGINT NOT NULL DEFAULT 0,
    session_id        TEXT NOT NULL DEFAULT '',
    operation_type    TEXT NOT NULL,
    model_name        TEXT NOT NULL DEFAULT '',
    model_provider    TEXT NOT NULL DEFAULT '',
    tool_name         TEXT NOT NULL DEFAULT '',
    prompt_tokens     BIGINT NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
    completion_tokens BIGINT NOT NULL DEFAULT 0 CHECK (completion_tokens >= 0),
    total_tokens      BIGINT NOT NULL DEFAULT 0 CHECK (total_tokens >= 0),
    cost_units        BIGINT NOT NULL CHECK (cost_units >= 0),
    trace_id          TEXT NOT NULL DEFAULT '',
    span_id           TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cost_records_trace_span
    ON cost_records (trace_id, span_id)
    WHERE trace_id <> '' AND span_id <> '';

CREATE INDEX IF NOT EXISTS idx_cost_records_workspace_created
    ON cost_records (workspace, created_at, id);

CREATE INDEX IF NOT EXISTS idx_cost_records_workspace_agent_created
    ON cost_records (workspace, agent_identity, created_at);

CREATE INDEX IF NOT EXISTS idx_cost_records_workspace_solution_created
    ON cost_records (workspace, solution, solution_rev, created_at)
    WHERE solution <> '';

CREATE INDEX IF NOT EXISTS idx_cost_records_session_created
    ON cost_records (session_id, created_at)
    WHERE session_id <> '';