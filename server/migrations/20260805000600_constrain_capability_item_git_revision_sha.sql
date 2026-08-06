-- Review finding F-3: git_sha was only checked non-empty.
--
-- `CHECK (git_sha <> '')` accepts 'x', a 39-character prefix, and an uppercase
-- SHA. All three are wrong in ways that only surface far from the insert:
--
--   * a short or malformed value cannot be resolved against Gitea, so the
--     revision's coordinate is unusable exactly when someone follows it;
--   * an uppercase value is a DIFFERENT string from the lowercase one the sync
--     writer stores, so any later comparison against
--     capability_items.git_sha (which the projection writes lowercased) silently
--     fails to match.
--
-- The writer already lowercases and the reader already assumes 40 hex
-- characters, so this constraint states what the rest of the system was
-- assuming for free. Verified against the live index before applying: all
-- existing rows already match, so the constraint validates without a cleanup
-- pass.
--
-- Lowercase specifically, not a case-insensitive class: accepting both cases
-- would leave two spellings of one commit representable and put the burden of
-- normalizing back on every reader.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE capability_item_git_revisions
    DROP CONSTRAINT IF EXISTS chk_capability_item_git_revisions_sha;
ALTER TABLE capability_item_git_revisions
    ADD CONSTRAINT chk_capability_item_git_revisions_sha
    CHECK (git_sha ~ '^[0-9a-f]{40}$');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE capability_item_git_revisions
    DROP CONSTRAINT IF EXISTS chk_capability_item_git_revisions_sha;
ALTER TABLE capability_item_git_revisions
    ADD CONSTRAINT chk_capability_item_git_revisions_sha
    CHECK (git_sha <> '');

-- +goose StatementEnd
