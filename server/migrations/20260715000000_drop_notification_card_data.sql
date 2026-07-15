-- +goose Up
-- +goose StatementBegin

ALTER TABLE system_notifications DROP COLUMN IF EXISTS card_data;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE system_notifications ADD COLUMN IF NOT EXISTS card_data JSONB;
COMMENT ON COLUMN system_notifications.card_data IS 'Interactive card data for WeCom/vote cards, used to update card status after user action';

-- +goose StatementEnd
