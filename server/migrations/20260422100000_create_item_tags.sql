-- +goose Up

-- Create tag dictionary table
CREATE TABLE IF NOT EXISTS item_tag_dicts (
    id uuid DEFAULT gen_random_uuid() PRIMARY KEY,
    slug text NOT NULL,
    tag_class text NOT NULL DEFAULT 'custom',
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT NOW(),
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

-- Seed system tags
INSERT INTO item_tag_dicts (slug, tag_class, created_by, created_at)
VALUES
    ('official', 'system', 'system', NOW()),
    ('best-practice', 'system', 'system', NOW())
ON CONFLICT (slug) DO NOTHING;

-- Seed builtin stage tags
INSERT INTO item_tag_dicts (slug, tag_class, created_by, created_at)
VALUES
    ('planning', 'builtin', 'system', NOW()),
    ('design', 'builtin', 'system', NOW()),
    ('development', 'builtin', 'system', NOW()),
    ('testing', 'builtin', 'system', NOW()),
    ('staging', 'builtin', 'system', NOW()),
    ('release', 'builtin', 'system', NOW()),
    ('maintenance', 'builtin', 'system', NOW())
ON CONFLICT (slug) DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS item_tags;
DROP TABLE IF EXISTS item_tag_dicts;
