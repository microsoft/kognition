-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

LOCK TABLE memory_vectors IN ACCESS EXCLUSIVE MODE;

DROP INDEX idx_memory_vectors_embedding;
ALTER TABLE memory_vectors ALTER COLUMN embedding TYPE vector;

CREATE TABLE memory_volume_dimensions (
    volume_id TEXT COLLATE "C" PRIMARY KEY,
    embedding_dimension INTEGER NOT NULL CHECK (embedding_dimension BETWEEN 1 AND 16000)
);

INSERT INTO memory_volume_dimensions (volume_id, embedding_dimension)
SELECT DISTINCT volume_id, 1536 FROM memory_vectors;

ALTER TABLE memory_vectors ADD CONSTRAINT memory_vectors_volume_dimensions_fk
    FOREIGN KEY (volume_id) REFERENCES memory_volume_dimensions (volume_id) ON DELETE CASCADE;

CREATE FUNCTION enforce_memory_vector_dimensions() RETURNS trigger AS $$
DECLARE
    dimension INTEGER;
BEGIN
    -- Preserve the default contract for writers that store without provisioning.
    INSERT INTO memory_volume_dimensions (volume_id, embedding_dimension)
    VALUES (NEW.volume_id, 1536) ON CONFLICT (volume_id) DO NOTHING;
    SELECT embedding_dimension INTO dimension FROM memory_volume_dimensions
        WHERE volume_id = NEW.volume_id FOR KEY SHARE;
    IF NEW.embedding IS NOT NULL AND vector_dims(NEW.embedding) <> dimension THEN
        RAISE EXCEPTION 'memory volume % expects % dimensions, got %',
            NEW.volume_id, dimension, vector_dims(NEW.embedding) USING ERRCODE = '22000';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER memory_vectors_dimensions
    BEFORE INSERT OR UPDATE ON memory_vectors
    FOR EACH ROW EXECUTE FUNCTION enforce_memory_vector_dimensions();

CREATE INDEX idx_memory_vectors_embedding ON memory_vectors
    USING hnsw ((embedding::vector(1536)) vector_cosine_ops)
    WHERE vector_dims(embedding) = 1536;
CREATE INDEX idx_memory_vectors_embedding384 ON memory_vectors
    USING hnsw ((embedding::vector(384)) vector_cosine_ops)
    WHERE vector_dims(embedding) = 384;
