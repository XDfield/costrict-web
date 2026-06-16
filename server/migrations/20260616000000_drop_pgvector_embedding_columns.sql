-- +goose Up
-- Remove pgvector embedding columns that are no longer used.
ALTER TABLE capability_items DROP COLUMN IF EXISTS embedding;
ALTER TABLE capability_items DROP COLUMN IF EXISTS embedding_updated_at;

-- +goose Down
-- Re-add pgvector embedding columns. Note: this requires the vector extension.
ALTER TABLE capability_items ADD COLUMN IF NOT EXISTS embedding vector(1024);
ALTER TABLE capability_items ADD COLUMN IF NOT EXISTS embedding_updated_at timestamptz;
