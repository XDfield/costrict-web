-- +goose Up

-- Create tag dictionary table
CREATE TABLE IF NOT EXISTS item_tag_dicts (
    id uuid DEFAULT gen_random_uuid() PRIMARY KEY,
    slug text NOT NULL,
    tag_class text NOT NULL DEFAULT 'custom',
    names jsonb NOT NULL DEFAULT '{}',
    descriptions jsonb DEFAULT '{}',
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT NOW(),
    updated_at timestamptz NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_item_tag_dicts_slug UNIQUE (slug)
);

-- Create item-tag join table
CREATE TABLE IF NOT EXISTS item_tags (
    id uuid DEFAULT gen_random_uuid() PRIMARY KEY,
    item_id uuid NOT NULL,
    tag_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_item_tag UNIQUE (item_id, tag_id),
    CONSTRAINT fk_item_tag_item FOREIGN KEY (item_id) REFERENCES capability_items(id) ON DELETE CASCADE,
    CONSTRAINT fk_item_tag_dict FOREIGN KEY (tag_id) REFERENCES item_tag_dicts(id) ON DELETE CASCADE
);

CREATE INDEX idx_item_tags_tag_id ON item_tags(tag_id);

-- Seed system tags from item_type values
INSERT INTO item_tag_dicts (slug, tag_class, names, descriptions, created_by, created_at, updated_at)
VALUES
    ('skill', 'system', '{"en":"Skill","zh":"技能"}', '{"en":"General skill item"}', 'system', NOW(), NOW()),
    ('mcp', 'system', '{"en":"MCP","zh":"MCP 服务"}', '{"en":"MCP server configuration"}', 'system', NOW(), NOW()),
    ('command', 'system', '{"en":"Command","zh":"命令"}', '{"en":"Command item"}', 'system', NOW(), NOW()),
    ('subagent', 'system', '{"en":"Sub-Agent","zh":"子代理"}', '{"en":"Sub-agent definition"}', 'system', NOW(), NOW()),
    ('hook', 'system', '{"en":"Hook","zh":"钩子"}', '{"en":"Hook configuration"}', 'system', NOW(), NOW())
ON CONFLICT (slug) DO NOTHING;

-- Seed functional tags from existing categories
INSERT INTO item_tag_dicts (slug, tag_class, names, descriptions, created_by, created_at, updated_at)
SELECT
    category,
    'functional',
    jsonb_build_object('en', category),
    '{}'::jsonb,
    'system',
    NOW(),
    NOW()
FROM (
    SELECT DISTINCT category FROM capability_items
    WHERE category IS NOT NULL AND category <> ''
) AS cats
ON CONFLICT (slug) DO NOTHING;

-- Auto-tag existing items based on their item_type
INSERT INTO item_tags (item_id, tag_id, created_at)
SELECT ci.id, itd.id, NOW()
FROM capability_items ci
JOIN item_tag_dicts itd ON itd.slug = ci.item_type AND itd.tag_class = 'system'
WHERE ci.status = 'active'
ON CONFLICT (item_id, tag_id) DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS item_tags;
DROP TABLE IF EXISTS item_tag_dicts;
