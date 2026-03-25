-- 为 devices 表添加复合索引，优化带软删除条件的查询性能
-- 解决 SetOnline/SetOffline 等操作中的慢 SQL 问题（>200ms）

-- +goose NO TRANSACTION

-- +goose Up
-- 方法1: 使用 CONCURRENTLY 避免锁表（推荐用于生产环境）
-- 注意：需要在无事务的情况下执行
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_devices_device_id_deleted_at 
ON devices(device_id, deleted_at);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_devices_device_id_deleted_at;

-- 方法2: 标准索引创建（如果上面的方法失败，可以使用这个）
-- CREATE INDEX IF NOT EXISTS idx_devices_device_id_deleted_at 
-- ON devices(device_id, deleted_at);

-- 可选：添加部分索引（仅包含未删除的记录，更节省空间）
-- CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_devices_device_id_not_deleted 
-- ON devices(device_id) 
-- WHERE deleted_at IS NULL;
