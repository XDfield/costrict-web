-- +goose Up
-- Capability names are presentation text, but slugs are installed by CSC as
-- command/directory identifiers. Normalize legacy skill rows created before
-- the API enforced lowercase kebab-case. Other capability types retain their
-- existing identifier contracts.
--
-- Process rows one at a time so the immediate unique index on
-- (repo_id, item_type, slug) cannot observe a transient collision. Existing
-- canonical rows win; colliding legacy rows receive a stable UUID suffix.
-- +goose StatementBegin
DO $migration$
DECLARE
    skill_row RECORD;
    candidate TEXT;
    suffix_counter INTEGER;
BEGIN
    FOR skill_row IN
        SELECT
            id,
            repo_id,
            slug,
            COALESCE(
                NULLIF(trim(BOTH '-' FROM regexp_replace(lower(slug), '[^a-z0-9]+', '-', 'g')), ''),
                'skill-' || replace(id::text, '-', '')
            ) AS base_slug
        FROM capability_items
        WHERE item_type = 'skill'
        ORDER BY
            (
                slug = COALESCE(
                    NULLIF(trim(BOTH '-' FROM regexp_replace(lower(slug), '[^a-z0-9]+', '-', 'g')), ''),
                    'skill-' || replace(id::text, '-', '')
                )
            ) DESC,
            created_at,
            id
    LOOP
        IF skill_row.slug = skill_row.base_slug THEN
            CONTINUE;
        END IF;

        candidate := skill_row.base_slug;
        IF EXISTS (
            SELECT 1
            FROM capability_items
            WHERE repo_id = skill_row.repo_id
              AND item_type = 'skill'
              AND slug = candidate
              AND id <> skill_row.id
        ) THEN
            candidate := skill_row.base_slug
                || '-migrated-'
                || replace(skill_row.id::text, '-', '');
            suffix_counter := 2;
            WHILE EXISTS (
                SELECT 1
                FROM capability_items
                WHERE repo_id = skill_row.repo_id
                  AND item_type = 'skill'
                  AND slug = candidate
                  AND id <> skill_row.id
            ) LOOP
                candidate := skill_row.base_slug
                    || '-migrated-'
                    || replace(skill_row.id::text, '-', '')
                    || '-'
                    || suffix_counter::text;
                suffix_counter := suffix_counter + 1;
            END LOOP;
        END IF;

        UPDATE capability_items
        SET slug = candidate
        WHERE id = skill_row.id;
    END LOOP;
END
$migration$;
-- +goose StatementEnd

-- +goose Down
-- This data cleanup is intentionally irreversible: the former non-canonical
-- slug is not a supported command identifier and is not stored separately.
