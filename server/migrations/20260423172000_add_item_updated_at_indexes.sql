-- Add indexes to support default and secondary item list sorting by updated_at.

-- +goose Up
-- +goose StatementBegin

CREATE INDEX IF NOT EXISTS idx_capability_items_updated_at
  ON capability_items(updated_at DESC);

CREATE INDEX IF NOT EXISTS idx_capability_items_registry_updated_at
  ON capability_items(registry_id, updated_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_capability_items_registry_updated_at;
DROP INDEX IF EXISTS idx_capability_items_updated_at;

-- +goose StatementEnd
