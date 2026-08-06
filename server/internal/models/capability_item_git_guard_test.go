package models

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newGuardTestDB builds an in-memory SQLite DB with a hand-written
// capability_items table. AutoMigrate is avoided for the same reason as
// elsewhere in this repo: the model carries PostgreSQL-only defaults
// (gen_random_uuid(), jsonb) that SQLite rejects at CREATE TABLE time.
func newGuardTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Exec(`CREATE TABLE capability_items (
		id TEXT PRIMARY KEY, registry_id TEXT NOT NULL, repo_id TEXT NOT NULL,
		slug TEXT NOT NULL, item_type TEXT NOT NULL, name TEXT NOT NULL,
		description TEXT, descriptions TEXT NOT NULL DEFAULT '{}', category TEXT,
		version TEXT DEFAULT '1.0.0', content TEXT, content_md5 TEXT DEFAULT '',
		current_revision INTEGER NOT NULL DEFAULT 1, metadata TEXT DEFAULT '{}',
		health TEXT DEFAULT '{}', evaluation TEXT DEFAULT '{}',
		source_path TEXT, catalog_entry_dir TEXT NOT NULL DEFAULT '', source_sha TEXT,
		source_type TEXT DEFAULT 'direct', source TEXT DEFAULT '',
		forked_from_item_id TEXT, forked_from_owner_id TEXT, parent_plugin_id TEXT,
		source_repo_url TEXT NOT NULL DEFAULT '', source_repo_ref TEXT NOT NULL DEFAULT 'main',
		source_repo_path TEXT NOT NULL DEFAULT '', content_backend TEXT NOT NULL DEFAULT 'db',
		source_git_server_id TEXT NOT NULL DEFAULT '', source_git_repo_id INTEGER NOT NULL DEFAULT 0,
		source_git_entry_key TEXT NOT NULL DEFAULT '', git_sha TEXT NOT NULL DEFAULT '',
		git_last_synced_at DATETIME, git_sync_status TEXT NOT NULL DEFAULT '',
		git_sync_error TEXT NOT NULL DEFAULT '', git_lifecycle_reason TEXT, git_lifecycle_changed_at DATETIME, git_visibility_verified_at DATETIME,
		is_built_in INTEGER DEFAULT 0, preview_count INTEGER DEFAULT 0,
		install_count INTEGER DEFAULT 0, favorite_count INTEGER DEFAULT 0,
		status TEXT DEFAULT 'active', security_status TEXT DEFAULT 'unscanned',
		last_scan_id TEXT, created_by TEXT NOT NULL, updated_by TEXT,
		experience_score REAL DEFAULT 0, created_at DATETIME, updated_at DATETIME,
		UNIQUE(repo_id, item_type, slug)
	)`).Error; err != nil {
		t.Fatalf("create capability_items: %v", err)
	}
	return db
}

func seedGuardItem(t *testing.T, db *gorm.DB, id, backend string) CapabilityItem {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	item := CapabilityItem{
		ID:              id,
		RegistryID:      "reg-1",
		RepoID:          "public",
		Slug:            id,
		ItemType:        "skill",
		Name:            "seed " + id,
		Description:     "seed description",
		Descriptions:    datatypes.JSON([]byte(`{}`)),
		Category:        "development",
		Version:         "1.0.0",
		Content:         "seed content",
		ContentMD5:      strings.Repeat("a", 64),
		CurrentRevision: 1,
		Metadata:        datatypes.JSON([]byte(`{}`)),
		SourcePath:      "skill.md",
		ContentBackend:  backend,
		Status:          "active",
		SecurityStatus:  "unscanned",
		CreatedBy:       "seeder",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if backend == ContentBackendGit {
		item.SourceRepoURL = "https://git.example.com/owner/repo"
		item.SourceRepoRef = "main"
		item.SourceRepoPath = "skill.md"
		item.SourceGitServerID = "srv-1"
		item.SourceGitRepoID = 42
		item.GitSHA = strings.Repeat("b", 40)
		item.GitSyncStatus = "synced"
	}
	if err := db.Create(&item).Error; err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	var stored CapabilityItem
	if err := db.First(&stored, "id = ?", id).Error; err != nil {
		t.Fatalf("reload %s: %v", id, err)
	}
	return stored
}

func assertGitOwnedRejected(t *testing.T, err error, label string) {
	t.Helper()
	if !errors.Is(err, ErrGitOwnedField) {
		t.Fatalf("%s: expected ErrGitOwnedField, got %v", label, err)
	}
}

// --- upsert backstop ------------------------------------------------------

// db.Save(&[]CapabilityItem{...}) never reaches BeforeUpdate: GORM turns a
// slice destination into Create + ON CONFLICT UpdateAll (finisher_api.go), so
// the create callback chain runs instead. That is the shape someone reaches for
// when writing a batch back-fill, which is exactly the "future developer" case
// R1.1 exists to cover.
func TestGitOwnedGuard_RejectsSliceSaveUpsert(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "git-slice", ContentBackendGit)

	rewritten := item
	rewritten.Content = "rewritten through a slice save"
	err := db.Save(&[]CapabilityItem{rewritten}).Error
	assertGitOwnedRejected(t, err, "Save(&[]CapabilityItem)")

	var after CapabilityItem
	if err := db.First(&after, "id = ?", item.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Content != "seed content" {
		t.Fatalf("content was written despite the guard: %q", after.Content)
	}
}

// A DB-backed row must still be upsertable — the guard may not change existing
// behaviour for rows it does not own.
func TestGitOwnedGuard_SliceSaveUpsertAllowedForDBRows(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "db-slice", ContentBackendDB)

	rewritten := item
	rewritten.Content = "rewritten"
	if err := db.Save(&[]CapabilityItem{rewritten}).Error; err != nil {
		t.Fatalf("db-backed slice save must succeed: %v", err)
	}
}

// A plain INSERT has to stay open: Git discovery creates its own rows through
// the same create callback chain, and a new row cannot overwrite repository
// truth. Only the upsert form is guarded.
func TestGitOwnedGuard_PlainInsertOfGitRowIsAllowed(t *testing.T) {
	db := newGuardTestDB(t)
	if got := seedGuardItem(t, db, "git-fresh", ContentBackendGit); got.ID != "git-fresh" {
		t.Fatalf("seeding a git-backed row must succeed, got %+v", got)
	}
}

// The Git writer's own upsert goes through, like every other guarded path.
func TestGitOwnedGuard_SliceSaveUpsertHonoursBypass(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "git-bypass-slice", ContentBackendGit)

	rewritten := item
	rewritten.Content = "written by the git writer"
	if err := db.Set(GitSyncBypassSetting, true).Save(&[]CapabilityItem{rewritten}).Error; err != nil {
		t.Fatalf("bypassed slice save must succeed: %v", err)
	}
	var after CapabilityItem
	if err := db.First(&after, "id = ?", item.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Content != "written by the git writer" {
		t.Fatalf("bypass did not write: %q", after.Content)
	}
}

// --- W5/W25 backstop: the three write shapes ------------------------------

// Updates(map) — the shape used by reconcileItemCurrentRevision (W23),
// MoveItem/TransferItem (W9/W10) and catalog ingest's applyMetadataDelta (W17-h).
func TestGitOwnedGuard_RejectsUpdatesMap(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "git-1", ContentBackendGit)

	err := db.Model(&item).Updates(map[string]any{"content": "rewritten"}).Error
	assertGitOwnedRejected(t, err, "Updates(map)")

	var after CapabilityItem
	if err := db.First(&after, "id = ?", item.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Content != "seed content" {
		t.Fatalf("content was written despite the guard: %q", after.Content)
	}
}

// Save(&item) — the shape used by updateItemFromJSON (W5), catalog ingest's
// updateItem (W17-c), legacy SyncService (W19-x) and GenerateService (W25).
// Save appends "*" to Selects, so it rewrites every column including zero
// values; a half-loaded struct must not be able to blank Git-owned columns.
func TestGitOwnedGuard_RejectsSaveWithBlankedFields(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "git-2", ContentBackendGit)

	// Simulates GenerateService.ImproveSkill: load, mutate content, Save.
	item.Content = "llm-improved"
	err := db.Save(&item).Error
	assertGitOwnedRejected(t, err, "Save(&item)")

	// And the nastier variant: a struct that was never fully loaded, whose
	// zero-valued Git columns would be written back as empty strings.
	partial := CapabilityItem{ID: item.ID, RegistryID: "reg-1", RepoID: "public",
		Slug: item.Slug, ItemType: "skill", Name: "renamed", CreatedBy: "seeder"}
	assertGitOwnedRejected(t, db.Save(&partial).Error, "Save(partial struct)")

	var after CapabilityItem
	if err := db.First(&after, "id = ?", item.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Content != "seed content" || after.GitSHA == "" || after.Name != "seed git-2" {
		t.Fatalf("row was mutated: content=%q gitSha=%q name=%q", after.Content, after.GitSHA, after.Name)
	}
}

// Select(...).Updates(...) — Selects narrows the written column set, so the
// guard must read it the same way GORM's ConvertToAssignments does.
func TestGitOwnedGuard_HonoursSelect(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "git-3", ContentBackendGit)

	// Selected column is Git-owned -> rejected.
	err := db.Model(&CapabilityItem{}).Where("id = ?", item.ID).
		Select("name", "install_count").
		Updates(map[string]any{"name": "renamed", "install_count": 7}).Error
	assertGitOwnedRejected(t, err, "Select(git-owned).Updates")

	// The very same map, with the Git-owned column excluded by Select, is a
	// legitimate runtime write and must go through.
	if err := db.Model(&CapabilityItem{}).Where("id = ?", item.ID).
		Select("install_count").
		Updates(map[string]any{"name": "renamed", "install_count": 7}).Error; err != nil {
		t.Fatalf("Select(runtime-only).Updates: %v", err)
	}

	var after CapabilityItem
	if err := db.First(&after, "id = ?", item.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Name != "seed git-3" {
		t.Fatalf("name changed despite Select exclusion: %q", after.Name)
	}
	if after.InstallCount != 7 {
		t.Fatalf("install_count not written: %d", after.InstallCount)
	}
}

// --- W21: the authoritative Git writer's explicit opt-out -----------------

func TestGitOwnedGuard_BypassLetsGitWriterThrough(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "git-4", ContentBackendGit)

	if err := db.Set(GitSyncBypassSetting, true).
		Model(&CapabilityItem{}).
		Where("id = ? AND content_backend = ?", item.ID, ContentBackendGit).
		Updates(map[string]any{
			"name":            "from manifest",
			"description":     "from manifest",
			"category":        "docs",
			"version":         "2.0.0",
			"git_sha":         strings.Repeat("c", 40),
			"git_sync_status": "synced",
		}).Error; err != nil {
		t.Fatalf("git writer was blocked by its own guard: %v", err)
	}

	var after CapabilityItem
	if err := db.First(&after, "id = ?", item.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Name != "from manifest" || after.Version != "2.0.0" {
		t.Fatalf("git writer update did not land: %+v", after)
	}
}

// The marker must not leak to the next statement on the same handle.
func TestGitOwnedGuard_BypassIsStatementLocal(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "git-5", ContentBackendGit)

	if err := db.Set(GitSyncBypassSetting, true).
		Model(&CapabilityItem{}).Where("id = ?", item.ID).
		Updates(map[string]any{"name": "from manifest"}).Error; err != nil {
		t.Fatalf("marked write: %v", err)
	}
	err := db.Model(&CapabilityItem{}).Where("id = ?", item.ID).
		Updates(map[string]any{"name": "from user"}).Error
	assertGitOwnedRejected(t, err, "unmarked write after marked write")
}

// --- Zero regression for DB-backed rows (the whole point of the rollout) ---

func TestGitOwnedGuard_DBBackedRowsUnaffected(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "db-1", ContentBackendDB)

	if err := db.Model(&item).Updates(map[string]any{
		"content": "rewritten", "name": "renamed", "current_revision": 3,
	}).Error; err != nil {
		t.Fatalf("Updates(map) on db-backed row: %v", err)
	}
	item.Content = "rewritten again"
	item.Version = "9.9.9"
	if err := db.Save(&item).Error; err != nil {
		t.Fatalf("Save on db-backed row: %v", err)
	}

	var after CapabilityItem
	if err := db.First(&after, "id = ?", item.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Content != "rewritten again" || after.Version != "9.9.9" || after.CurrentRevision != 3 {
		t.Fatalf("db-backed row did not take the writes: %+v", after)
	}
}

// A batch update that also covers DB-backed rows must not be waved through
// just because most of its targets are writable (W17-e / W17-k shape).
func TestGitOwnedGuard_BatchUpdateRejectedWhenAnyGitRowMatches(t *testing.T) {
	db := newGuardTestDB(t)
	seedGuardItem(t, db, "batch-db", ContentBackendDB)
	seedGuardItem(t, db, "batch-git", ContentBackendGit)

	err := db.Model(&CapabilityItem{}).Where("registry_id = ?", "reg-1").
		Updates(map[string]any{"category": "bulk"}).Error
	assertGitOwnedRejected(t, err, "batch Updates")

	// Restricting the same batch to DB-backed rows is the supported way out —
	// this is what the catalog-ingest / migrate pipelines now do.
	if err := db.Model(&CapabilityItem{}).
		Where("registry_id = ? AND content_backend = ?", "reg-1", ContentBackendDB).
		Updates(map[string]any{"category": "bulk"}).Error; err != nil {
		t.Fatalf("db-scoped batch update: %v", err)
	}
	var git CapabilityItem
	if err := db.First(&git, "id = ?", "batch-git").Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if git.Category != "development" {
		t.Fatalf("git row was caught by the db-scoped batch: %q", git.Category)
	}
}

// --- W13 / W22 / W23b / W31 / W32: runtime + admin columns stay writable ---

func TestGitOwnedGuard_RuntimeColumnsRemainWritable(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "git-6", ContentBackendGit)

	cases := []struct {
		name    string
		updates map[string]any
	}{
		// W22 scan worker (security_status / last_scan_id only).
		{"scan status", map[string]any{"security_status": "low", "last_scan_id": "scan-1"}},
		// W13 admin status.
		{"admin status", map[string]any{"status": "archived"}},
		// W31 experience score.
		{"experience score", map[string]any{"experience_score": 0.75}},
		// W23b download counter (LogBehavior uses UpdateColumn, this is the
		// hook-visible equivalent).
		{"install count", map[string]any{"install_count": gorm.Expr("install_count + 1")}},
		{"preview count", map[string]any{"preview_count": gorm.Expr("preview_count + 1")}},
		// W32 subscribe/distribute counter.
		{"favorite count", map[string]any{"favorite_count": gorm.Expr("favorite_count + 1")}},
		// Fork back-fills descriptions right after creating the Git row.
		{"descriptions", map[string]any{"descriptions": datatypes.JSON([]byte(`{"zh":"x"}`))}},
		{"is_built_in", map[string]any{"is_built_in": true}},
		{"parent_plugin_id", map[string]any{"parent_plugin_id": nil}},
	}
	for _, tc := range cases {
		if err := db.Model(&CapabilityItem{}).Where("id = ?", item.ID).
			Updates(tc.updates).Error; err != nil {
			t.Fatalf("%s must stay writable on a git-backed row: %v", tc.name, err)
		}
	}

	var after CapabilityItem
	if err := db.First(&after, "id = ?", item.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.InstallCount != 1 || after.FavoriteCount != 1 || after.PreviewCount != 1 {
		t.Fatalf("counters did not increment: %+v", after)
	}
	if after.SecurityStatus != "low" || after.Status != "archived" {
		t.Fatalf("runtime/admin state did not land: %+v", after)
	}
}

// UpdateColumn sets SkipHooks, which is how behavior_service bumps counters.
// Pinned so a future refactor to Update() does not silently start tripping the
// guard inside a goroutine whose error is discarded.
func TestGitOwnedGuard_UpdateColumnStillWorks(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "git-7", ContentBackendGit)

	if err := db.Model(&CapabilityItem{}).Where("id = ?", item.ID).
		UpdateColumn("install_count", gorm.Expr("install_count + 1")).Error; err != nil {
		t.Fatalf("UpdateColumn(install_count): %v", err)
	}
}

// --- R1.6: a status-only PUT on a Git-backed row stays legal --------------

// updateItemFromJSON loads the row, mutates only status, then Save()s the
// whole struct. Save rewrites every column, so without a diff the guard would
// reject a write that changes nothing Git owns.
func TestGitOwnedGuard_SaveWithoutGitOwnedChangesIsAllowed(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "git-8", ContentBackendGit)

	item.Status = "archived"
	item.UpdatedBy = "admin"
	if err := db.Save(&item).Error; err != nil {
		t.Fatalf("status-only Save on a git-backed row must be allowed: %v", err)
	}

	var after CapabilityItem
	if err := db.First(&after, "id = ?", item.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Status != "archived" || after.UpdatedBy != "admin" {
		t.Fatalf("status-only Save did not land: %+v", after)
	}
	if after.Content != "seed content" || after.GitSHA == "" {
		t.Fatalf("git-owned columns drifted: %+v", after)
	}
}

// --- content_backend is itself guarded (escape-hatch closure) -------------

func TestGitOwnedGuard_ContentBackendCannotBeFlipped(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "git-9", ContentBackendGit)

	err := db.Model(&CapabilityItem{}).Where("id = ?", item.ID).
		Updates(map[string]any{"content_backend": ContentBackendDB}).Error
	assertGitOwnedRejected(t, err, "flip content_backend")
}

// --- W24b/c/d: documented hook blind spots --------------------------------

// tx.Table(...) and raw Exec bypass model hooks entirely. This is pinned as a
// test so the blind spot is a stated property rather than a surprise: the
// migrate pipeline carries its own content_backend='db' predicate in SQL.
func TestGitOwnedGuard_RawSQLAndTableBypassTheHook(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "git-10", ContentBackendGit)

	if err := db.Table("capability_items").Where("id = ?", item.ID).
		Update("current_revision", 5).Error; err != nil {
		t.Fatalf("Table() update unexpectedly failed: %v", err)
	}
	if err := db.Exec("UPDATE capability_items SET slug = ? WHERE id = ?", "renamed", item.ID).Error; err != nil {
		t.Fatalf("raw Exec unexpectedly failed: %v", err)
	}

	var after CapabilityItem
	if err := db.First(&after, "id = ?", item.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.CurrentRevision != 5 || after.Slug != "renamed" {
		t.Fatalf("blind spot no longer reproduces; the SQL-level filters in cmd/migrate may now be the only guard: %+v", after)
	}
}

// --- W24: schemas that predate the content_backend column -----------------

// backfillCapabilityContentVersioning writes content_md5 + current_revision
// (both Git-owned) inside one transaction and its caller log.Fatalf's. It can
// be invoked as a standalone subcommand against a schema where the column has
// not been added yet, so the guard must be inert there instead of referencing
// a column that does not exist.
func TestGitOwnedGuard_InertWhenContentBackendColumnIsMissing(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Exec(`CREATE TABLE capability_items (
		id TEXT PRIMARY KEY, registry_id TEXT NOT NULL, repo_id TEXT NOT NULL,
		slug TEXT NOT NULL, item_type TEXT NOT NULL, name TEXT NOT NULL,
		content TEXT, content_md5 TEXT DEFAULT '',
		current_revision INTEGER NOT NULL DEFAULT 1,
		status TEXT DEFAULT 'active', created_by TEXT NOT NULL,
		created_at DATETIME, updated_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create legacy capability_items: %v", err)
	}
	if err := db.Exec(`INSERT INTO capability_items (id, registry_id, repo_id, slug, item_type, name, content, current_revision, status, created_by)
		VALUES ('legacy-1','reg','public','legacy','skill','Legacy','body',0,'active','system')`).Error; err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	if err := db.Model(&CapabilityItem{}).Where("id = ?", "legacy-1").Updates(map[string]any{
		"content_md5": "deadbeef", "current_revision": 1,
	}).Error; err != nil {
		t.Fatalf("guard must be inert without the content_backend column: %v", err)
	}
}

// --- the guarded column set itself ---------------------------------------

func TestGitOwnedCapabilityColumns_CoversTheDesignSet(t *testing.T) {
	for _, column := range []string{
		"content", "content_md5", "name", "description", "category", "version",
		"source_path", "catalog_entry_dir", "source_sha", "item_type", "slug",
		"current_revision", "content_backend",
		"git_sha", "git_last_synced_at", "git_sync_status", "git_sync_error",
		"source_repo_url", "source_repo_ref", "source_repo_path",
		"source_git_server_id", "source_git_repo_id", "source_git_entry_key",
	} {
		if !IsGitOwnedCapabilityColumn(column) {
			t.Errorf("%s must be Git-owned", column)
		}
	}
	// Both writers are legitimate for these; guarding them would break the
	// scan worker, admin status changes, subscribe counters and fork.
	for _, column := range []string{
		"status", "security_status", "last_scan_id", "is_built_in",
		"preview_count", "install_count", "favorite_count", "experience_score",
		"registry_id", "repo_id", "descriptions", "metadata", "updated_by",
	} {
		if IsGitOwnedCapabilityColumn(column) {
			t.Errorf("%s must not be Git-owned", column)
		}
	}
}
