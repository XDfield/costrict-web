-- 初始化外键约束
--
-- 1. sync_logs.registry_id → capability_registries.id
--    确保同步日志记录必须关联到有效的注册表
--
-- 2. sync_jobs.registry_id → capability_registries.id
--    确保同步任务必须关联到有效的注册表
--
-- 3. capability_registries.last_sync_log_id → sync_logs.id (ON DELETE SET NULL)
--    关联最后一条同步日志，日志删除时自动设为 NULL
--
-- 4. capability_versions.item_id → capability_items.id (ON DELETE CASCADE, ON UPDATE CASCADE)
--    删除 item 时自动级联删除关联的 versions，更新 item id 时自动同步更新

-- +goose Up
-- +goose StatementBegin

-- sync_logs 外键
ALTER TABLE sync_logs DROP CONSTRAINT IF EXISTS fk_sync_logs_registry;
ALTER TABLE sync_logs 
ADD CONSTRAINT fk_sync_logs_registry 
FOREIGN KEY (registry_id) REFERENCES capability_registries(id);

-- sync_jobs 外键
ALTER TABLE sync_jobs DROP CONSTRAINT IF EXISTS fk_sync_jobs_registry;
ALTER TABLE sync_jobs 
ADD CONSTRAINT fk_sync_jobs_registry 
FOREIGN KEY (registry_id) REFERENCES capability_registries(id);

-- capability_registries 外键（关联最后一条同步日志）
ALTER TABLE capability_registries DROP CONSTRAINT IF EXISTS fk_capability_registries_last_sync_log;
ALTER TABLE capability_registries 
ADD CONSTRAINT fk_capability_registries_last_sync_log 
FOREIGN KEY (last_sync_log_id) REFERENCES sync_logs(id) ON DELETE SET NULL;

-- capability_versions 级联删除外键
ALTER TABLE capability_versions DROP CONSTRAINT IF EXISTS fk_capability_items_versions;
ALTER TABLE capability_versions 
ADD CONSTRAINT fk_capability_items_versions 
FOREIGN KEY (item_id) REFERENCES capability_items(id) ON DELETE CASCADE ON UPDATE CASCADE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE sync_logs DROP CONSTRAINT IF EXISTS fk_sync_logs_registry;
ALTER TABLE sync_jobs DROP CONSTRAINT IF EXISTS fk_sync_jobs_registry;
ALTER TABLE capability_registries DROP CONSTRAINT IF EXISTS fk_capability_registries_last_sync_log;
ALTER TABLE capability_versions DROP CONSTRAINT IF EXISTS fk_capability_items_versions;

-- +goose StatementEnd
