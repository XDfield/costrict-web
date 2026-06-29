-- +goose Up
-- Adds conn_id column to gateway_device_bindings to track the connection ID
-- associated with each device-gateway binding (required by the updated Store
-- interface for device clone / migration recovery support).
ALTER TABLE gateway_device_bindings ADD COLUMN IF NOT EXISTS conn_id TEXT;

-- +goose Down
ALTER TABLE gateway_device_bindings DROP COLUMN IF EXISTS conn_id;
