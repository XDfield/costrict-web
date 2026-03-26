-- +goose Up
INSERT INTO system_notification_channels (type, name, created_by)
SELECT 'wecom', '企业微信', 'admin'
WHERE NOT EXISTS (
    SELECT 1 FROM system_notification_channels
    WHERE type = 'wecom' AND name = '企业微信' AND deleted_at IS NULL
);

-- +goose Down
DELETE FROM system_notification_channels
WHERE type = 'wecom' AND name = '企业微信' AND created_by = 'admin';
