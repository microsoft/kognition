-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

CREATE TABLE IF NOT EXISTS model_dispatches (
    namespace TEXT NOT NULL,
    operation TEXT NOT NULL,
    session_id TEXT NOT NULL,
    response JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (namespace, operation),
    CHECK ((response IS NULL) = (completed_at IS NULL))
);