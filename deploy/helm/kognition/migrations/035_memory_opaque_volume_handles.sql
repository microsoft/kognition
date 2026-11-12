-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- Backend handles are opaque and case-sensitive; retain legacy UUIDs as text.
ALTER TABLE memory_vectors
    ALTER COLUMN volume_id TYPE TEXT COLLATE "C" USING volume_id::text;
