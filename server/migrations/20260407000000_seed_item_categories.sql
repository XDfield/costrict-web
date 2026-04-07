-- +goose Up
-- Seed item_categories from existing capability_items.category values.
INSERT INTO item_categories (id, slug, names, descriptions, created_by, created_at, updated_at)
SELECT
    gen_random_uuid(),
    category,
    jsonb_build_object('en', category),
    '{}'::jsonb,
    'system',
    NOW(),
    NOW()
FROM (
    SELECT DISTINCT category
    FROM capability_items
    WHERE category IS NOT NULL AND category <> ''
) AS cats
ON CONFLICT (slug) DO NOTHING;

-- +goose Down
-- no-op: seed data is not removed
