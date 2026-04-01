ALTER TABLE capability_items
  ADD COLUMN IF NOT EXISTS preview_count INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS favorite_count INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS item_favorites (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  item_id UUID NOT NULL REFERENCES capability_items(id) ON DELETE CASCADE,
  user_id VARCHAR(191) NOT NULL,
  created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_item_favorite ON item_favorites(item_id, user_id);
CREATE INDEX IF NOT EXISTS idx_item_favorites_user_id ON item_favorites(user_id);
CREATE INDEX IF NOT EXISTS idx_capability_items_preview_count ON capability_items(preview_count DESC);
CREATE INDEX IF NOT EXISTS idx_capability_items_favorite_count ON capability_items(favorite_count DESC);

UPDATE capability_items ci
SET preview_count = stats.view_count
FROM (
  SELECT item_id, COUNT(*)::INTEGER AS view_count
  FROM behavior_logs
  WHERE item_id IS NOT NULL AND action_type = 'view'
  GROUP BY item_id
) AS stats
WHERE ci.id = stats.item_id
  AND ci.preview_count = 0;

UPDATE capability_items ci
SET favorite_count = stats.favorite_count
FROM (
  SELECT item_id, COUNT(*)::INTEGER AS favorite_count
  FROM item_favorites
  GROUP BY item_id
) AS stats
WHERE ci.id = stats.item_id
  AND ci.favorite_count = 0;
