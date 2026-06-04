-- Migration: Add is_built_in column to capability_items
-- Date: 2026-06-04

ALTER TABLE capability_items
ADD COLUMN IF NOT EXISTS is_built_in BOOLEAN NOT NULL DEFAULT false;

-- Create index for efficient builtin plugin queries
CREATE INDEX IF NOT EXISTS idx_capability_items_builtin
ON capability_items(item_type, is_built_in)
WHERE is_built_in = true;
