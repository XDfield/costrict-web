-- 为 behavior_logs 添加 (item_id, action_type, user_id) 复合索引（SRC-2026-4791 P1-1）
-- 用途：
--   1) isFirstUserAction 的 COUNT(item_id, user_id, action_type) 走 index-only，
--      仅命中该用户在该 item 该动作的少数几行（而非扫全表/全 item 行）。
--   2) GetItemBehaviorStats / updateExperienceScore 的
--      COUNT(DISTINCT user_id) WHERE item_id=? AND action_type=? 走覆盖索引。
-- 生产 behavior_logs 可能很大，用 CONCURRENTLY 避免建索引时锁表。

-- +goose NO TRANSACTION

-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_behavior_logs_item_action_user
ON behavior_logs(item_id, action_type, user_id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_behavior_logs_item_action_user;
