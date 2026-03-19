-- 删除旧的外键约束（如果存在）
ALTER TABLE capability_versions DROP CONSTRAINT IF EXISTS fk_capability_items_versions;

-- 添加新的级联删除外键约束
ALTER TABLE capability_versions 
ADD CONSTRAINT fk_capability_items_versions 
FOREIGN KEY (item_id) REFERENCES capability_items(id) ON DELETE CASCADE;
