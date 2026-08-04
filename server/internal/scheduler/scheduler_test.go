package scheduler

import (
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newSchedulerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.Exec(`CREATE TABLE capability_registries (
		id TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT, source_type TEXT,
		external_url TEXT, external_branch TEXT, sync_enabled INTEGER, sync_interval INTEGER,
		last_synced_at DATETIME, last_sync_sha TEXT, sync_status TEXT, sync_config TEXT,
		last_sync_log_id TEXT, repo_id TEXT, owner_id TEXT NOT NULL, created_at DATETIME, updated_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func seedRegistry(t *testing.T, db *gorm.DB, id, sourceType, externalURL string, syncEnabled bool) {
	t.Helper()
	if err := db.Exec(
		`INSERT INTO capability_registries (id, name, source_type, external_url, sync_enabled, sync_interval, owner_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, id, sourceType, externalURL, syncEnabled, 3600, "owner-1",
	).Error; err != nil {
		t.Fatalf("seed registry %s: %v", id, err)
	}
}

// A Git-backed registry is written by the Git discovery pipeline purely to
// carry the repository-level projection; its capabilities are reconciled from
// push webhooks. Letting the legacy clone poller adopt it puts two writers on
// the same capability_items rows, and the poller's closing sweep archives every
// row whose source path its own include patterns do not match.
func TestSchedulerSkipsGitBackedRegistries(t *testing.T) {
	db := newSchedulerTestDB(t)
	seedRegistry(t, db, "git-registry", "git", "https://git.example/u-alice/plugin", true)

	s := &Scheduler{DB: db}
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	t.Cleanup(s.Stop)

	if _, scheduled := s.jobMap["git-registry"]; scheduled {
		t.Fatal("Git-backed registry was scheduled for legacy clone sync")
	}
}

// The guard must also disarm rows that the discovery pipeline already wrote
// with sync_enabled = true before the fix, so no data migration is needed.
func TestSchedulerRejectsGitBackedRegistryOnDirectRegistration(t *testing.T) {
	db := newSchedulerTestDB(t)
	s := &Scheduler{DB: db}
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	t.Cleanup(s.Stop)

	registry := &models.CapabilityRegistry{
		ID:          "git-registry",
		Name:        "u-alice/plugin",
		SourceType:  "git",
		ExternalURL: "https://git.example/u-alice/plugin",
		SyncEnabled: true,
	}
	if err := s.RegisterRegistry(registry); err != nil {
		t.Fatalf("register registry: %v", err)
	}
	if _, scheduled := s.jobMap["git-registry"]; scheduled {
		t.Fatal("Git-backed registry was scheduled for legacy clone sync")
	}
}

// Reverse guard: the source_type filter must not disturb the registries the
// legacy pipeline owns. A NULL source_type is the trap here — `source_type <>
// 'git'` evaluates to NULL for those rows and would silently drop them.
func TestSchedulerStillRegistersLegacyRegistries(t *testing.T) {
	db := newSchedulerTestDB(t)
	seedRegistry(t, db, "external-registry", "external", "https://github.com/costrict/catalog", true)
	seedRegistry(t, db, "internal-registry", "internal", "https://github.com/costrict/seed", true)
	if err := db.Exec(
		`INSERT INTO capability_registries (id, name, source_type, external_url, sync_enabled, sync_interval, owner_id)
		 VALUES (?, ?, NULL, ?, ?, ?, ?)`,
		"null-source-registry", "legacy", "https://github.com/costrict/legacy", true, 3600, "owner-1",
	).Error; err != nil {
		t.Fatalf("seed NULL source_type registry: %v", err)
	}

	s := &Scheduler{DB: db}
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	t.Cleanup(s.Stop)

	for _, id := range []string{"external-registry", "internal-registry", "null-source-registry"} {
		if _, scheduled := s.jobMap[id]; !scheduled {
			t.Errorf("legacy registry %s lost its schedule", id)
		}
	}
}
