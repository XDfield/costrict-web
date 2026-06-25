-- +goose Up
-- source_url holds the upstream repo clone target (with branch + subdir), e.g.
-- "https://github.com/owner/repo/tree/main/subdir". Distinct from `source`
-- (a provenance label). Backend lazy-clones from this to pack lossless ZIP
-- bundles for the DB+HTTP distribution channel. TEXT (not VARCHAR) because
-- monorepo subdir URLs can exceed 255 chars.
ALTER TABLE capability_items
  ADD COLUMN IF NOT EXISTS source_url TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE capability_items
  DROP COLUMN IF EXISTS source_url;
