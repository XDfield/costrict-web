-- +goose Up
-- +goose StatementBegin

-- 口径迁移：enterprise_customers.account_ids 由 users.subject_id 改存 Casdoor universal_id。
-- 系统内"指代一个人"统一锚定到 casdoor_universal_id（email 在 GitHub/手机登录可能为空，不可靠）。
-- 存储格式不变（仍是 JSONB 字符串数组），只是元素语义从 subject_id 变为 universal_id。
--
-- 做法：把每个 account_id（当前是 subject_id）按原顺序展开，对每个元素用 LATERAL
-- 子查询只取一行对应的 casdoor_universal_id；解析不到（用户不存在 / 已软删 / 无
-- universal_id）的元素丢弃；无任何可解析元素时回退 '[]'::jsonb。
-- 解析口径与运行时 GORM resolver 一致：只匹配 deleted_at IS NULL 的用户。
-- 用 LATERAL ... LIMIT 1 保证即便（理论上）subject_id 有多行也不会扇出、不破坏数组长度/顺序。
--
-- 去重：多个不同 subject_id 可能解析到同一 universal_id（或 account_ids 本身含重复
-- subject_id），直接 jsonb_agg 会写入重复 universal_id 元素。故先 DISTINCT ON
-- (casdoor_universal_id) 按 ord 取每个 universal_id 的首次出现名次，再按该 ord
-- 排序聚合——结果既去重又保持稳定顺序（保留每个 universal_id 第一次出现的位置）。
--
-- 非幂等警告：本迁移**不是幂等**的。对已迁移过的数据再次运行会把 account_ids 清空——
-- 因为此时元素已经是 universal_id，按 subject_id 去 JOIN users 必然 join 不到，
-- 全部被过滤为 []（数据丢失，不可自动恢复）。正确性仅依赖 goose 版本表保证“只执行一次”，
-- 切勿手动重跑本 Up。
UPDATE enterprise_customers ec
SET account_ids = COALESCE(
    (
        SELECT jsonb_agg(dedup.casdoor_universal_id ORDER BY dedup.ord)
        FROM (
            SELECT DISTINCT ON (u.casdoor_universal_id) u.casdoor_universal_id, elem.ord
            FROM jsonb_array_elements_text(ec.account_ids) WITH ORDINALITY AS elem(val, ord)
            JOIN LATERAL (
                SELECT casdoor_universal_id
                FROM users
                WHERE subject_id = elem.val
                  AND deleted_at IS NULL
                  AND casdoor_universal_id IS NOT NULL
                  AND casdoor_universal_id <> ''
                ORDER BY id
                LIMIT 1
            ) u ON true
            ORDER BY u.casdoor_universal_id, elem.ord
        ) dedup
    ),
    '[]'::jsonb
)
WHERE jsonb_typeof(ec.account_ids) = 'array'
  AND jsonb_array_length(ec.account_ids) > 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- 反向口径迁移：universal_id → subject_id。
-- casdoor_universal_id 只是普通 index（非唯一），同一 universal_id 可能命中多行用户，
-- 普通 JOIN 会扇出、破坏数组长度与顺序。故对每个元素用 LATERAL ... LIMIT 1（按 id 取最小行），
-- 只取一行 subject_id，保证可逆性与顺序稳定。
-- 同样只匹配 deleted_at IS NULL 的用户，与 Up / 运行时口径一致。
--
-- 去重：多个不同 universal_id 可能解析到同一 subject_id（或 account_ids 本身含重复
-- universal_id），直接 jsonb_agg 会写入重复 subject_id 元素。与 Up 对称，先 DISTINCT ON
-- (subject_id) 按 ord 取每个 subject_id 的首次出现名次，再按该 ord 排序聚合——去重且顺序稳定。
--
-- 非幂等警告：与 Up 对称，Down 也**不是幂等**的；对已是 subject_id 的数据重跑会清空 account_ids。
-- 仅依赖 goose 版本表保证一次性回滚。
UPDATE enterprise_customers ec
SET account_ids = COALESCE(
    (
        SELECT jsonb_agg(dedup.subject_id ORDER BY dedup.ord)
        FROM (
            SELECT DISTINCT ON (u.subject_id) u.subject_id, elem.ord
            FROM jsonb_array_elements_text(ec.account_ids) WITH ORDINALITY AS elem(val, ord)
            JOIN LATERAL (
                SELECT subject_id
                FROM users
                WHERE casdoor_universal_id = elem.val
                  AND deleted_at IS NULL
                  AND subject_id IS NOT NULL
                  AND subject_id <> ''
                ORDER BY id
                LIMIT 1
            ) u ON true
            ORDER BY u.subject_id, elem.ord
        ) dedup
    ),
    '[]'::jsonb
)
WHERE jsonb_typeof(ec.account_ids) = 'array'
  AND jsonb_array_length(ec.account_ids) > 0;

-- +goose StatementEnd
