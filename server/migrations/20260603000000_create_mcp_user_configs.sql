-- +goose Up
-- Per-user filled values for MCP resources that ship with placeholder config
-- (local script PATH, API tokens). Stored plaintext jsonb, consistent with
-- channel_configs / device tokens; never serialized back via ItemResponse
-- (GORM field tagged json:"-"), only surfaced masked through mcpConfig status.
CREATE TABLE IF NOT EXISTS mcp_user_configs (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      VARCHAR(191) NOT NULL,
  item_id      UUID NOT NULL,
  field_values JSONB NOT NULL DEFAULT '{}',
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One config row per (user, item); upserts target this constraint.
CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_user_config
  ON mcp_user_configs (user_id, item_id);

-- Serves cleanup/aggregation by item.
CREATE INDEX IF NOT EXISTS idx_mcp_user_config_item
  ON mcp_user_configs (item_id);

-- +goose Down
DROP INDEX IF EXISTS idx_mcp_user_config_item;
DROP INDEX IF EXISTS idx_mcp_user_config;
DROP TABLE IF EXISTS mcp_user_configs;
