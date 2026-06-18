-- +goose Up
-- Remove embedding columns that are no longer used.
ALTER TABLE capability_items DROP COLUMN IF EXISTS embedding;
ALTER TABLE capability_items DROP COLUMN IF EXISTS embedding_updated_at;

-- +goose Down
-- NOTE: Re-adding vector columns would require the pgvector extension, which
-- has been removed from the project. If you truly need to roll back, install
-- pgvector manually first, then uncomment and run the statements below.
-- ALTER TABLE capability_items ADD COLUMN IF NOT EXISTS embedding vector(1024);
-- ALTER TABLE capability_items ADD COLUMN IF NOT EXISTS embedding_updated_at timestamptz;
