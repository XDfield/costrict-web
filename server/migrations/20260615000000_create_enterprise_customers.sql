-- +goose Up
-- +goose StatementBegin

-- 大客户（enterprise customers）品牌配置。一行 = 一个大客户：name + logo(base64 data URI)
-- + account_ids(JSONB 字符串数组)。account_ids 存的是 users.subject_id 列表（与
-- capability_items.created_by、user_system_roles.user_id 同口径），前端用
-- matchEnterprise(item.createdBy) 即 account_ids.includes(createdBy) 命中渲染大客户标识。
CREATE TABLE IF NOT EXISTS enterprise_customers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    logo TEXT NOT NULL,                       -- base64 data URI（避免前端 canvas 抽色跨域污染）
    account_ids JSONB NOT NULL DEFAULT '[]',  -- ["usr_a","usr_b"]，即 users.subject_id 列表
    created_by VARCHAR(191),                  -- 操作者 subject_id
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_enterprise_customers_deleted_at ON enterprise_customers(deleted_at);

COMMENT ON TABLE enterprise_customers IS '大客户品牌配置（平台管理员配置）';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS enterprise_customers;

-- +goose StatementEnd
