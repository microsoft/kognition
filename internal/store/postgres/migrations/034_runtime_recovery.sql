-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

CREATE TABLE IF NOT EXISTS runtime_recovery (
    namespace TEXT NOT NULL,
    session_id TEXT NOT NULL,
    binding_digest TEXT NOT NULL CHECK (length(binding_digest) = 64),
    phase TEXT NOT NULL CHECK (phase IN ('prepared', 'claimed', 'terminal', 'retired')),
    owner TEXT NOT NULL DEFAULT '',
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    ciphertext JSONB,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (namespace, session_id),
    CHECK (ciphertext IS NULL OR pg_column_size(ciphertext) <= 786432),
    CHECK ((phase = 'retired' AND ciphertext IS NULL) OR (phase <> 'retired' AND ciphertext IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS runtime_recovery_terminal_expiry
    ON runtime_recovery (expires_at) WHERE phase = 'terminal';
