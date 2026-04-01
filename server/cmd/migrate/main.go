package main

import (
	"fmt"
	"log"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	migrations "github.com/costrict/costrict-web/server/migrations"
	"github.com/pressly/goose/v3"
	"gorm.io/gorm"
)

func main() {
	cfg := config.Load()

	db, err := database.Initialize(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}

	if err := runPreMigrations(db); err != nil {
		log.Fatalf("Failed to run pre-migrations: %v", err)
	}

	err = db.AutoMigrate(
		&models.User{},
		&models.Repository{},
		&models.RepoMember{},
		&models.RepoInvitation{},
		&models.SyncLog{},
		&models.SyncJob{},
		&models.CapabilityRegistry{},
		&models.CapabilityItem{},
		&models.CapabilityVersion{},
		&models.CapabilityAsset{},
		&models.CapabilityArtifact{},
		&models.BehaviorLog{},
		&models.ItemFavorite{},
		&models.SecurityScan{},
		&models.ScanJob{},
		&models.Device{},
		&models.Workspace{},
		&models.WorkspaceDirectory{},
		&models.SystemNotificationChannel{},
		&models.UserNotificationChannel{},
		&models.UserConfig{},
		&models.NotificationLog{},
	)
	if err != nil {
		log.Fatalf("Failed to auto-migrate database: %v", err)
	}

	if err := runGooseMigrations(db); err != nil {
		log.Fatalf("Failed to run goose migrations: %v", err)
	}

	log.Println("All migrations completed successfully")
}

func runGooseMigrations(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("failed to get underlying sql.DB: %w", err)
	}

	goose.SetBaseFS(migrations.FS)

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("failed to set goose dialect: %w", err)
	}

	if err := goose.Up(sqlDB, "."); err != nil {
		return fmt.Errorf("goose migration failed: %w", err)
	}

	log.Println("Goose migrations completed successfully")
	return nil
}

func runPreMigrations(db *gorm.DB) error {
	bootstrapStmts := []string{
		`CREATE TABLE IF NOT EXISTS sync_logs (
			id uuid DEFAULT gen_random_uuid() PRIMARY KEY,
			registry_id uuid NOT NULL,
			trigger_type text NOT NULL,
			trigger_user text,
			status text NOT NULL DEFAULT 'running',
			commit_sha text,
			previous_sha text,
			total_items bigint DEFAULT 0,
			added_items bigint DEFAULT 0,
			updated_items bigint DEFAULT 0,
			deleted_items bigint DEFAULT 0,
			skipped_items bigint DEFAULT 0,
			failed_items bigint DEFAULT 0,
			error_message text,
			duration_ms bigint,
			started_at timestamptz NOT NULL DEFAULT now(),
			finished_at timestamptz,
			created_at timestamptz
		)`,
		`CREATE TABLE IF NOT EXISTS capability_registries (
			id uuid DEFAULT gen_random_uuid() PRIMARY KEY,
			name text NOT NULL,
			description text,
			source_type text NOT NULL DEFAULT 'internal',
			external_url text,
			external_branch text DEFAULT 'main',
			sync_enabled boolean DEFAULT false,
			sync_interval bigint DEFAULT 3600,
			last_synced_at timestamptz,
			last_sync_sha text,
			sync_status text DEFAULT 'idle',
			sync_config JSONB DEFAULT '{}',
			last_sync_log_id uuid,
			visibility text DEFAULT 'repo',
			repo_id text,
			owner_id text NOT NULL,
			created_at timestamptz,
			updated_at timestamptz
		)`,
	}
	for _, stmt := range bootstrapStmts {
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("bootstrap failed: %w", err)
		}
	}

	preMigrations := []struct {
		check string
		stmts []string
	}{
		{
			check: `SELECT 1 FROM information_schema.columns WHERE table_name='capability_versions' AND column_name='version'`,
			stmts: []string{
				`ALTER TABLE capability_versions ADD COLUMN IF NOT EXISTS revision bigint`,
				`UPDATE capability_versions SET revision = version WHERE revision IS NULL`,
				`ALTER TABLE capability_versions ALTER COLUMN revision SET NOT NULL`,
				`ALTER TABLE capability_versions ALTER COLUMN revision SET DEFAULT 1`,
			},
		},
		{
			check: `SELECT 1 FROM information_schema.columns WHERE table_name='capability_items' AND column_name='visibility'`,
			stmts: []string{
				`ALTER TABLE capability_items DROP COLUMN IF EXISTS visibility`,
			},
		},
		// Drop visibility column from capability_registries — visibility is now
		// derived from the parent repository's visibility field.
		{
			check: `SELECT 1 FROM information_schema.columns WHERE table_name='capability_registries' AND column_name='visibility'`,
			stmts: []string{
				`ALTER TABLE capability_registries DROP COLUMN IF EXISTS visibility`,
			},
		},
		{
			check: `SELECT 1 FROM information_schema.columns WHERE table_name='security_scans' AND column_name='revision_id'`,
			stmts: []string{
				`ALTER TABLE security_scans DROP COLUMN IF EXISTS revision_id`,
			},
		},
		{
			check: `SELECT 1 FROM pg_indexes WHERE indexname = 'idx_item_slug'`,
			stmts: []string{
				`DROP INDEX IF EXISTS idx_item_slug`,
			},
		},
		{
			check: `SELECT 1 FROM pg_indexes WHERE indexname = 'idx_item_slug_global'`,
			stmts: []string{
				`DROP INDEX IF EXISTS idx_item_slug_global`,
			},
		},
		{
			check: `SELECT 1 FROM information_schema.columns WHERE table_name='capability_items' AND column_name='id' AND NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='capability_items' AND column_name='repo_id')`,
			stmts: []string{
				`ALTER TABLE capability_items ADD COLUMN repo_id text NOT NULL DEFAULT 'public'`,
			},
		},
	}

	for _, m := range preMigrations {
		var exists int
		if err := db.Raw(m.check).Scan(&exists).Error; err != nil {
			return fmt.Errorf("pre-migration check failed (%s): %w", m.check, err)
		}
		if exists != 1 {
			continue
		}
		for _, stmt := range m.stmts {
			if err := db.Exec(stmt).Error; err != nil {
				return fmt.Errorf("pre-migration failed (%s): %w", stmt, err)
			}
		}
	}

	if err := backfillCapabilityItemRepoIDs(db); err != nil {
		return fmt.Errorf("failed to backfill capability_items.repo_id before migrations: %w", err)
	}

	if err := deduplicateSlugs(db); err != nil {
		return fmt.Errorf("failed to deduplicate slugs before composite unique index: %w", err)
	}
	return nil
}

func backfillCapabilityItemRepoIDs(db *gorm.DB) error {
	var tableExists int
	if err := db.Raw(`SELECT 1 FROM information_schema.tables WHERE table_schema = CURRENT_SCHEMA() AND table_name = 'capability_items'`).Scan(&tableExists).Error; err != nil {
		return fmt.Errorf("checking capability_items existence: %w", err)
	}
	if tableExists != 1 {
		return nil
	}

	var needsBackfill int
	if err := db.Raw(`SELECT 1 FROM capability_items WHERE repo_id = 'public' LIMIT 1`).Scan(&needsBackfill).Error; err != nil {
		return fmt.Errorf("checking capability_items repo_id backfill: %w", err)
	}
	if needsBackfill != 1 {
		return nil
	}

	if err := db.Exec(`UPDATE capability_items SET repo_id = COALESCE(
		(SELECT COALESCE(NULLIF(cr.repo_id,''), 'public')
		 FROM capability_registries cr
		 WHERE cr.id = capability_items.registry_id),
		'public'
	) WHERE repo_id = 'public'`).Error; err != nil {
		return fmt.Errorf("backfilling capability_items.repo_id: %w", err)
	}

	return nil
}

func deduplicateSlugs(db *gorm.DB) error {
	var tableExists int
	if err := db.Raw(`SELECT 1 WHERE to_regclass('public.capability_items') IS NOT NULL`).Scan(&tableExists).Error; err != nil {
		return fmt.Errorf("checking capability_items existence: %w", err)
	}
	if tableExists != 1 {
		return nil
	}

	var idxExists int
	db.Raw(`SELECT 1 FROM pg_indexes WHERE indexname = 'idx_item_repo_type_slug'`).Scan(&idxExists)
	if idxExists == 1 {
		return nil
	}

	type row struct {
		ID       string
		RepoID   string `gorm:"column:repo_id"`
		ItemType string `gorm:"column:item_type"`
		Slug     string
	}

	var rows []row
	err := db.Raw(`
		SELECT id, repo_id, item_type, slug
		FROM capability_items
		WHERE (repo_id, item_type, slug) IN (
			SELECT repo_id, item_type, slug
			FROM capability_items
			GROUP BY repo_id, item_type, slug HAVING COUNT(*) > 1
		)
		ORDER BY repo_id, item_type, slug, created_at ASC, id ASC`,
	).Scan(&rows).Error
	if err != nil {
		return fmt.Errorf("querying duplicate slugs: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	type groupKey struct{ RepoID, ItemType, Slug string }
	type group struct {
		ids []string
	}
	groups := make(map[groupKey]*group)
	var keys []groupKey
	for _, r := range rows {
		k := groupKey{r.RepoID, r.ItemType, r.Slug}
		g, ok := groups[k]
		if !ok {
			groups[k] = &group{ids: []string{r.ID}}
			keys = append(keys, k)
		} else {
			g.ids = append(g.ids, r.ID)
		}
	}

	return db.Transaction(func(tx *gorm.DB) error {
		for _, k := range keys {
			g := groups[k]
			for i := 1; i < len(g.ids); i++ {
				candidate := ""
				for n := i + 1; ; n++ {
					candidate = fmt.Sprintf("%s-%d", k.Slug, n)
					var count int64
					if err := tx.Raw(
						`SELECT COUNT(*) FROM capability_items WHERE repo_id = ? AND item_type = ? AND slug = ?`,
						k.RepoID, k.ItemType, candidate,
					).Scan(&count).Error; err != nil {
						return fmt.Errorf("checking slug %q: %w", candidate, err)
					}
					if count == 0 {
						break
					}
				}
				log.Printf("[deduplicateSlugs] renaming item %s slug %q -> %q (repo=%s, type=%s)",
					g.ids[i], k.Slug, candidate, k.RepoID, k.ItemType)
				if err := tx.Exec(
					`UPDATE capability_items SET slug = ? WHERE id = ?`, candidate, g.ids[i],
				).Error; err != nil {
					return fmt.Errorf("renaming slug for item %s: %w", g.ids[i], err)
				}
			}
		}
		return nil
	})
}
