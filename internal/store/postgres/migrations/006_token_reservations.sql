-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 006_token_reservations: durable hybrid token-accounting reservations.

CREATE TABLE IF NOT EXISTS token_reservations (
    reservation_id  TEXT PRIMARY KEY,
    namespace       TEXT NOT NULL,
    reserved_input  BIGINT NOT NULL CHECK (reserved_input >= 0),
    reserved_output BIGINT NOT NULL CHECK (reserved_output >= 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_token_reservations_namespace_created
    ON token_reservations (namespace, created_at);