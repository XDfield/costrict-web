package main

import (
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	migrations "github.com/costrict/costrict-web/server/migrations"
	"github.com/pressly/goose/v3"
	"gorm.io/gorm"
)

var errDryRunRollback = errors.New("dry-run rollback")

func main() {
	cfg := config.Load()

	db, err := database.Initialize(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "user-subject-ids":
			dryRun := len(os.Args) > 2 && os.Args[2] == "--dry-run"
			if err := backfillLegacyUserReferences(db, dryRun); err != nil {
				if dryRun && errors.Is(err, errDryRunRollback) {
					log.Println("Legacy user reference dry-run completed successfully")
					return
				}
				log.Fatalf("Failed to backfill legacy user references: %v", err)
			}
			if dryRun {
				log.Println("Legacy user reference dry-run completed successfully")
			} else {
				log.Println("Legacy user reference backfill completed successfully")
			}
			return
		}
	}

	if err := runPreMigrations(db); err != nil {
		log.Fatalf("Failed to run pre-migrations: %v", err)
	}

	err = db.AutoMigrate(
			&models.UserSystemRole{},
			&models.Repository{},
			&models.RepoMember{},
			&models.RepoInvitation{},
		&models.Project{},
		&models.ProjectMember{},
		&models.ProjectInvitation{},
		&models.ProjectRepository{},
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
		&models.ItemCategory{},
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

	if err := goose.Up(sqlDB, ".", goose.WithAllowMissing()); err != nil {
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

func backfillLegacyUserReferences(db *gorm.DB, dryRun bool) error {
	type userMapping struct {
		SubjectID          string
		CasdoorUniversalID *string
		CasdoorID          *string
		CasdoorSub         *string
	}

	var users []userMapping
	if err := db.Table("users").Select("subject_id, casdoor_universal_id, casdoor_id, casdoor_sub").Find(&users).Error; err != nil {
		return fmt.Errorf("load user mappings: %w", err)
	}

	mapping := make(map[string]string, len(users)*4)
	for _, user := range users {
		if user.SubjectID == "" {
			continue
		}
		mapping[user.SubjectID] = user.SubjectID
		if user.CasdoorUniversalID != nil && *user.CasdoorUniversalID != "" {
			mapping[*user.CasdoorUniversalID] = user.SubjectID
		}
		if user.CasdoorID != nil && *user.CasdoorID != "" {
			mapping[*user.CasdoorID] = user.SubjectID
		}
		if user.CasdoorSub != nil && *user.CasdoorSub != "" {
			mapping[*user.CasdoorSub] = user.SubjectID
		}
	}

	updates := []struct {
		table  string
		column string
	}{
		{"system_notification_channels", "created_by"},
		{"user_notification_channels", "user_id"},
		{"user_configs", "user_id"},
		{"notification_logs", "user_id"},
		{"devices", "user_id"},
		{"repositories", "owner_id"},
		{"repo_members", "user_id"},
		{"repo_invitations", "inviter_id"},
		{"repo_invitations", "invitee_id"},
		{"projects", "creator_id"},
		{"project_members", "user_id"},
		{"project_invitations", "inviter_id"},
		{"project_invitations", "invitee_id"},
		{"project_repositories", "bound_by_user_id"},
		{"user_system_roles", "user_id"},
		{"user_system_roles", "granted_by"},
		{"capability_registries", "owner_id"},
		{"sync_jobs", "trigger_user"},
		{"sync_logs", "trigger_user"},
		{"capability_items", "created_by"},
		{"capability_items", "updated_by"},
		{"item_categories", "created_by"},
		{"item_favorites", "user_id"},
		{"capability_versions", "created_by"},
		{"capability_artifacts", "uploaded_by"},
		{"scan_jobs", "trigger_user"},
		{"workspaces", "user_id"},
		{"behavior_logs", "user_id"},
	}

	type tableStat struct {
		UpdatedRows int
		Unresolved  map[string]int
	}
	stats := make(map[string]*tableStat, len(updates))
	getStat := func(table, column string) *tableStat {
		key := table + "." + column
		if stats[key] == nil {
			stats[key] = &tableStat{Unresolved: map[string]int{}}
		}
		return stats[key]
	}

	return db.Transaction(func(tx *gorm.DB) error {
		for _, target := range updates {
			rows, err := tx.Table(target.table).Select(target.column).Where(target.column+" IS NOT NULL AND "+target.column+" <> ''").Rows()
			if err != nil {
				return fmt.Errorf("scan %s.%s: %w", target.table, target.column, err)
			}
			values := map[string]struct{}{}
			for rows.Next() {
				var value string
				if err := rows.Scan(&value); err != nil {
					rows.Close()
					return fmt.Errorf("read %s.%s: %w", target.table, target.column, err)
				}
				if value != "" {
					values[value] = struct{}{}
				}
			}
			rows.Close()

			for legacyValue := range values {
				subjectID, ok := mapping[legacyValue]
				if !ok {
					getStat(target.table, target.column).Unresolved[legacyValue]++
					continue
				}
				if subjectID == legacyValue {
					continue
				}

				var affected int64
				if dryRun {
					if err := tx.Table(target.table).Where(target.column+" = ?", legacyValue).Count(&affected).Error; err != nil {
						return fmt.Errorf("count %s.%s for %s: %w", target.table, target.column, legacyValue, err)
					}
				} else {
					result := tx.Exec(
						fmt.Sprintf("UPDATE %s SET %s = ? WHERE %s = ?", target.table, target.column, target.column),
						subjectID,
						legacyValue,
					)
					if result.Error != nil {
						return fmt.Errorf("update %s.%s from %s to %s: %w", target.table, target.column, legacyValue, subjectID, result.Error)
					}
					affected = result.RowsAffected
				}
				getStat(target.table, target.column).UpdatedRows += int(affected)
			}
		}

		log.Printf("user-subject-ids summary (dry-run=%v)", dryRun)
		for _, target := range updates {
			key := target.table + "." + target.column
			stat := stats[key]
			if stat == nil {
				continue
			}
			if stat.UpdatedRows > 0 {
				log.Printf("  [%s] updated rows: %d", key, stat.UpdatedRows)
			}
			if len(stat.Unresolved) > 0 {
				log.Printf("  [%s] unresolved identifiers:", key)
				for legacyValue, seenCount := range stat.Unresolved {
					log.Printf("    - %s (seen %d times)", legacyValue, seenCount)
				}
			}
		}

		if dryRun {
			return errDryRunRollback
		}
		return nil
	})
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
