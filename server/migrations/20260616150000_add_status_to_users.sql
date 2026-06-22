-- +goose Up
-- +goose StatementBegin

-- 成员管理（M1）独立账号状态列。与 is_active 解耦：is_active 由登录同步逻辑读写
-- （GetOrCreateUser 会把 is_active "复活" 成 true），不能当封禁开关；status 专做管理员
-- 显式置位的封禁/禁用，登录链路不会触碰它。
--   active   = 正常
--   disabled = 管理员禁用（拒绝新请求）
--   banned   = 管理员封禁（拒绝新请求）
ALTER TABLE users ADD COLUMN IF NOT EXISTS status VARCHAR(32) NOT NULL DEFAULT 'active';
CREATE INDEX IF NOT EXISTS idx_user_status ON users(status);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_user_status;
ALTER TABLE users DROP COLUMN IF EXISTS status;

-- +goose StatementEnd
