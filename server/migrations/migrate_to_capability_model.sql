-- Migration: Migrate from old skill model to new capability model
-- Date: 2025-03-12
-- Description: Delete old tables and rename skill_* tables to capability_*

-- ============================================
-- Step 1: Drop old tables that are no longer needed
-- ============================================

DROP TABLE IF EXISTS user_preferences CASCADE;
DROP TABLE IF EXISTS skill_ratings CASCADE;
DROP TABLE IF EXISTS skills CASCADE;
DROP TABLE IF EXISTS mcp_servers CASCADE;
DROP TABLE IF EXISTS commands CASCADE;
DROP TABLE IF EXISTS agents CASCADE;
DROP TABLE IF EXISTS skill_repositories CASCADE;

-- ============================================
-- Step 2: Rename skill_* tables to capability_*
-- ============================================

-- Rename skill_registries to capability_registries
ALTER TABLE IF EXISTS skill_registries RENAME TO capability_registries;

-- Rename skill_items to capability_items
ALTER TABLE IF EXISTS skill_items RENAME TO capability_items;

-- Rename skill_versions to capability_versions
ALTER TABLE IF EXISTS skill_versions RENAME TO capability_versions;

-- Rename skill_artifacts to capability_artifacts
ALTER TABLE IF EXISTS skill_artifacts RENAME TO capability_artifacts;

-- ============================================
-- Step 3: Rename foreign key columns if needed
-- ============================================

-- Rename registry_id in capability_items (was already correct)
-- Rename item_id in capability_versions (was already correct)
-- Rename item_id in capability_artifacts (was already correct)

-- ============================================
-- Step 4: Rename indexes
-- ============================================

-- Rename indexes for capability_registries
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_skill_registries_org_id') THEN
        ALTER INDEX idx_skill_registries_org_id RENAME TO idx_capability_registries_org_id;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_skill_registries_owner_id') THEN
        ALTER INDEX idx_skill_registries_owner_id RENAME TO idx_capability_registries_owner_id;
    END IF;
END $$;

-- Rename indexes for capability_items
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_skill_items_registry_id') THEN
        ALTER INDEX idx_skill_items_registry_id RENAME TO idx_capability_items_registry_id;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_skill_items_slug') THEN
        ALTER INDEX idx_skill_items_slug RENAME TO idx_capability_items_slug;
    END IF;
END $$;

-- Rename indexes for capability_versions
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_skill_versions_item_id') THEN
        ALTER INDEX idx_skill_versions_item_id RENAME TO idx_capability_versions_item_id;
    END IF;
END $$;

-- Rename indexes for capability_artifacts
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_skill_artifacts_item_id') THEN
        ALTER INDEX idx_skill_artifacts_item_id RENAME TO idx_capability_artifacts_item_id;
    END IF;
END $$;

-- ============================================
-- Step 5: Rename foreign key constraints
-- ============================================

-- Note: GORM will recreate constraints with correct names on next AutoMigrate
-- This is a best-effort rename

DO $$
DECLARE
    constraint_record RECORD;
BEGIN
    -- Rename foreign key constraints for capability_items
    FOR constraint_record IN
        SELECT conname, conrelid::regclass as tbl
        FROM pg_constraint
        WHERE contype = 'f'
        AND conrelid = 'capability_items'::regclass
        AND conname LIKE 'fk_skill_%'
    LOOP
        EXECUTE format('ALTER TABLE capability_items RENAME CONSTRAINT %I TO %s',
            constraint_record.conname,
            replace(constraint_record.conname, 'fk_skill_', 'fk_capability_'));
    END LOOP;

    -- Rename foreign key constraints for capability_versions
    FOR constraint_record IN
        SELECT conname, conrelid::regclass as tbl
        FROM pg_constraint
        WHERE contype = 'f'
        AND conrelid = 'capability_versions'::regclass
        AND conname LIKE 'fk_skill_%'
    LOOP
        EXECUTE format('ALTER TABLE capability_versions RENAME CONSTRAINT %I TO %s',
            constraint_record.conname,
            replace(constraint_record.conname, 'fk_skill_', 'fk_capability_'));
    END LOOP;

    -- Rename foreign key constraints for capability_artifacts
    FOR constraint_record IN
        SELECT conname, conrelid::regclass as tbl
        FROM pg_constraint
        WHERE contype = 'f'
        AND conrelid = 'capability_artifacts'::regclass
        AND conname LIKE 'fk_skill_%'
    LOOP
        EXECUTE format('ALTER TABLE capability_artifacts RENAME CONSTRAINT %I TO %s',
            constraint_record.conname,
            replace(constraint_record.conname, 'fk_skill_', 'fk_capability_'));
    END LOOP;
END $$;

-- ============================================
-- Step 6: Create sync_jobs table if not exists
-- ============================================

CREATE TABLE IF NOT EXISTS sync_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    registry_id UUID NOT NULL,
    trigger_type VARCHAR(32) NOT NULL,
    trigger_user VARCHAR(191),
    priority INTEGER NOT NULL DEFAULT 5,
    status VARCHAR(32) NOT NULL DEFAULT 'pending',
    payload JSONB DEFAULT '{}',
    retry_count INTEGER DEFAULT 0,
    max_attempts INTEGER DEFAULT 3,
    last_error TEXT,
    scheduled_at TIMESTAMP WITH TIME ZONE NOT NULL,
    started_at TIMESTAMP WITH TIME ZONE,
    finished_at TIMESTAMP WITH TIME ZONE,
    sync_log_id UUID,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_sync_jobs_registry_id ON sync_jobs(registry_id);
CREATE INDEX IF NOT EXISTS idx_sync_jobs_status ON sync_jobs(status);
CREATE INDEX IF NOT EXISTS idx_sync_jobs_scheduled_at ON sync_jobs(scheduled_at);

-- ============================================
-- Step 7: Create sync_logs table if not exists
-- ============================================

CREATE TABLE IF NOT EXISTS sync_logs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    registry_id UUID NOT NULL,
    trigger_type VARCHAR(32) NOT NULL,
    trigger_user VARCHAR(191),
    status VARCHAR(32) NOT NULL DEFAULT 'running',
    commit_sha VARCHAR(64),
    previous_sha VARCHAR(64),
    total_items INTEGER DEFAULT 0,
    added_items INTEGER DEFAULT 0,
    updated_items INTEGER DEFAULT 0,
    deleted_items INTEGER DEFAULT 0,
    skipped_items INTEGER DEFAULT 0,
    failed_items INTEGER DEFAULT 0,
    error_message TEXT,
    duration_ms BIGINT,
    started_at TIMESTAMP WITH TIME ZONE NOT NULL,
    finished_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_sync_logs_registry_id ON sync_logs(registry_id);

-- ============================================
-- Done
-- ============================================

-- Show final table list
\echo 'Migration completed. Current tables:'
\dt
