-- Performance indexes for capability_items
--
-- 1. pg_trgm GIN indexes on name/description to accelerate ILIKE '%...%' searches
--    (replaces full table scans in ListAllItems, ListItems, KeywordSearch, HybridSearch)
--
-- 2. Composite index (status, created_at DESC) for the most common query pattern:
--    WHERE status = 'active' ORDER BY created_at DESC
--
-- 3. Index on created_by for ListMyItems filtering
--
-- 4. Index on category for ListAllItems category filtering
--
-- 5. Index on status (standalone) for all queries filtering by status

-- +goose Up
-- +goose StatementBegin

-- Enable pg_trgm extension (idempotent)
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- GIN trigram indexes for fast ILIKE '%...%' searches
CREATE INDEX IF NOT EXISTS idx_item_name_trgm
    ON capability_items USING GIN (name gin_trgm_ops);

CREATE INDEX IF NOT EXISTS idx_item_description_trgm
    ON capability_items USING GIN (description gin_trgm_ops);

-- Composite index for the dominant query pattern: active items ordered by creation time
CREATE INDEX IF NOT EXISTS idx_item_status_created
    ON capability_items (status, created_at DESC);

-- Index for ListMyItems (WHERE created_by = ?)
CREATE INDEX IF NOT EXISTS idx_item_created_by
    ON capability_items (created_by);

-- Index for category filtering
CREATE INDEX IF NOT EXISTS idx_item_category
    ON capability_items (category);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_item_name_trgm;
DROP INDEX IF EXISTS idx_item_description_trgm;
DROP INDEX IF EXISTS idx_item_status_created;
DROP INDEX IF EXISTS idx_item_created_by;
DROP INDEX IF EXISTS idx_item_category;

-- +goose StatementEnd
