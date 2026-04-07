-- +goose Up
ALTER TABLE security_scans
  ADD COLUMN IF NOT EXISTS category VARCHAR(128) NOT NULL DEFAULT '';

INSERT INTO item_categories (id, slug, names, descriptions, sort_order, created_by, created_at, updated_at)
VALUES
  (gen_random_uuid(), 'frontend-development', '{"zh":"前端开发","en":"Frontend Development"}'::jsonb, '{}'::jsonb, 10, 'system', NOW(), NOW()),
  (gen_random_uuid(), 'backend-development', '{"zh":"后端开发","en":"Backend Development"}'::jsonb, '{}'::jsonb, 20, 'system', NOW(), NOW()),
  (gen_random_uuid(), 'system-architecture', '{"zh":"系统架构","en":"System Architecture"}'::jsonb, '{}'::jsonb, 30, 'system', NOW(), NOW()),
  (gen_random_uuid(), 'requirements-analysis', '{"zh":"需求分析","en":"Requirements Analysis"}'::jsonb, '{}'::jsonb, 40, 'system', NOW(), NOW()),
  (gen_random_uuid(), 'system-design', '{"zh":"系统设计","en":"System Design"}'::jsonb, '{}'::jsonb, 50, 'system', NOW(), NOW()),
  (gen_random_uuid(), 'data-processing', '{"zh":"数据处理","en":"Data Processing"}'::jsonb, '{}'::jsonb, 60, 'system', NOW(), NOW()),
  (gen_random_uuid(), 'software-testing', '{"zh":"软件测试","en":"Software Testing"}'::jsonb, '{}'::jsonb, 70, 'system', NOW(), NOW()),
  (gen_random_uuid(), 'tdd-development', '{"zh":"TDD 开发","en":"TDD Development"}'::jsonb, '{}'::jsonb, 80, 'system', NOW(), NOW()),
  (gen_random_uuid(), 'information-security', '{"zh":"信息安全","en":"Information Security"}'::jsonb, '{}'::jsonb, 90, 'system', NOW(), NOW()),
  (gen_random_uuid(), 'command-execution', '{"zh":"命令执行","en":"Command Execution"}'::jsonb, '{}'::jsonb, 100, 'system', NOW(), NOW()),
  (gen_random_uuid(), 'tool-invocation', '{"zh":"工具调用","en":"Tool Invocation"}'::jsonb, '{}'::jsonb, 110, 'system', NOW(), NOW()),
  (gen_random_uuid(), 'deployment-operations', '{"zh":"部署运维","en":"Deployment and Operations"}'::jsonb, '{}'::jsonb, 120, 'system', NOW(), NOW())
ON CONFLICT (slug) DO UPDATE
SET
  names = EXCLUDED.names,
  sort_order = EXCLUDED.sort_order,
  updated_at = NOW();

-- +goose Down
DELETE FROM item_categories
WHERE slug IN (
  'frontend-development',
  'backend-development',
  'system-architecture',
  'requirements-analysis',
  'system-design',
  'data-processing',
  'software-testing',
  'tdd-development',
  'information-security',
  'command-execution',
  'tool-invocation',
  'deployment-operations'
);

ALTER TABLE security_scans
  DROP COLUMN IF EXISTS category;
