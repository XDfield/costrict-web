-- +goose Up
-- Capability names are presentation text, but slugs are installed by CSC as
-- command/directory identifiers. Normalize legacy invocable rows created
-- before the API enforced lowercase kebab-case. Display-only capability types
-- retain their existing identifier contracts.
--
-- Process rows one at a time so the immediate unique index on
-- (repo_id, item_type, slug) cannot observe a transient collision. Existing
-- canonical rows win; colliding legacy rows receive a stable UUID suffix.
-- +goose StatementBegin
DO $migration$
DECLARE
    capability_row RECORD;
    candidate TEXT;
    suffix_counter INTEGER;
BEGIN
    FOR capability_row IN
        SELECT normalized.*
        FROM (
            SELECT
                id,
                repo_id,
                item_type,
                slug,
                created_at,
                COALESCE(
                    NULLIF(
                        trim(BOTH '-' FROM regexp_replace(
                            lower(
                                CASE
                                    WHEN trim(COALESCE(slug, '')) = '' THEN name
                                    ELSE slug
                                END
                            ),
                            '[^a-z0-9]+',
                            '-',
                            'g'
                        )),
                        ''
                    ),
                    CASE lower(trim(item_type))
                        WHEN 'agent' THEN 'agent'
                        WHEN 'command' THEN 'command'
                        WHEN 'subagent' THEN 'subagent'
                        ELSE 'skill'
                    END || '-' || replace(id::text, '-', '')
                ) AS base_slug
            FROM capability_items
            WHERE lower(trim(item_type)) IN (
                'skill',
                'command',
                'subagent',
                'agent'
            )
        ) AS normalized
        ORDER BY
            (slug = base_slug) DESC,
            created_at,
            id
    LOOP
        IF capability_row.slug = capability_row.base_slug THEN
            CONTINUE;
        END IF;

        candidate := capability_row.base_slug;
        IF EXISTS (
            SELECT 1
            FROM capability_items
            WHERE repo_id = capability_row.repo_id
              AND item_type = capability_row.item_type
              AND slug = candidate
              AND id <> capability_row.id
        ) THEN
            candidate := capability_row.base_slug
                || '-migrated-'
                || replace(capability_row.id::text, '-', '');
            suffix_counter := 2;
            WHILE EXISTS (
                SELECT 1
                FROM capability_items
                WHERE repo_id = capability_row.repo_id
                  AND item_type = capability_row.item_type
                  AND slug = candidate
                  AND id <> capability_row.id
            ) LOOP
                candidate := capability_row.base_slug
                    || '-migrated-'
                    || replace(capability_row.id::text, '-', '')
                    || '-'
                    || suffix_counter::text;
                suffix_counter := suffix_counter + 1;
            END LOOP;
        END IF;

        UPDATE capability_items
        SET slug = candidate
        WHERE id = capability_row.id;
    END LOOP;
END
$migration$;
-- +goose StatementEnd

-- +goose Down
-- This data cleanup is intentionally irreversible: the former non-canonical
-- slug is not a supported command identifier and is not stored separately.
