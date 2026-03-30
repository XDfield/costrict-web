-- 移除设备表中不属于该字段的工作空间ID字段
--
-- 设计变更：一个设备可以被多个工作空间绑定，设备不应该属于特定的工作空间
-- 只需要工作空间引用设备，不需要设备引用工作空间
--
-- 移除 device.workspace_id 字段

-- +goose Up
-- +goose StatementBegin

ALTER TABLE devices DROP COLUMN IF EXISTS workspace_id;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE devices ADD COLUMN workspace_id VARCHAR(191);
CREATE INDEX idx_devices_workspace_id ON devices(workspace_id);

-- +goose StatementEnd
