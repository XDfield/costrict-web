package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// PostgreSQL-only by construction. The command's whole safety argument is
// `IS NOT DISTINCT FROM` on a nullable uuid inside a compare-and-set UPDATE,
// plus REPEATABLE READ around the inventory, plus the CHECK constraints on the
// run/row tables. None of those exist on SQLite, so a green SQLite suite would
// certify nothing about the engine this runs on.

func newPluginFlattenPostgresDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL plugin flatten test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	schema := fmt.Sprintf("plugin_flatten_%d", time.Now().UnixNano())
	quoted := `"` + strings.ReplaceAll(schema, `"`, `""`) + `"`

	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + quoted).Error; err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_ = admin.Exec("DROP SCHEMA " + quoted + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	query := parsed.Query()
	// Only the scratch schema: the fixtures below are the whole world for this
	// test, and falling back to public would let it read production rows.
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL in %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	for _, ddl := range []string{
		`CREATE TABLE capability_items (
			id UUID PRIMARY KEY,
			registry_id UUID NOT NULL DEFAULT gen_random_uuid(),
			slug TEXT NOT NULL DEFAULT '',
			item_type TEXT NOT NULL DEFAULT 'skill',
			status TEXT NOT NULL DEFAULT 'active',
			source_type TEXT NOT NULL DEFAULT 'direct',
			content_backend VARCHAR(16) NOT NULL DEFAULT 'db',
			catalog_entry_dir TEXT,
			source_path TEXT,
			source_sha TEXT,
			metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			source_git_server_id VARCHAR(64) NOT NULL DEFAULT '',
			source_git_repo_id BIGINT NOT NULL DEFAULT 0,
			source_repo_path TEXT NOT NULL DEFAULT '',
			source_git_entry_key TEXT NOT NULL DEFAULT '',
			forked_from_item_id UUID,
			parent_plugin_id UUID,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE item_favorites (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			item_id UUID NOT NULL,
			user_id VARCHAR(191) NOT NULL
		)`,
		`CREATE TABLE item_distributions (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			item_id UUID NOT NULL,
			distributor_id TEXT NOT NULL DEFAULT '',
			target_id TEXT NOT NULL DEFAULT '',
			status VARCHAR(32) DEFAULT 'active',
			revoked_at TIMESTAMPTZ
		)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create fixture table: %v", err)
		}
	}
	if err := ensurePluginFlattenTables(db); err != nil {
		t.Fatalf("ensure flatten tables: %v", err)
	}
	return db
}

type flattenFixture struct {
	ID              string
	ItemType        string
	Slug            string
	Status          string
	SourceType      string
	ContentBackend  string
	CatalogEntryDir string
	Metadata        string
	GitServerID     string
	GitRepoID       int64
	GitRepoPath     string
	ForkedFrom      *string
	Parent          *string
}

func seedFlattenItem(t *testing.T, db *gorm.DB, f flattenFixture) {
	t.Helper()
	if f.Status == "" {
		f.Status = "active"
	}
	if f.SourceType == "" {
		f.SourceType = "direct"
	}
	if f.ContentBackend == "" {
		f.ContentBackend = "db"
	}
	if f.Metadata == "" {
		f.Metadata = "{}"
	}
	if f.ItemType == "" {
		f.ItemType = "skill"
	}
	if f.Slug == "" {
		f.Slug = "slug-" + f.ID[:8]
	}
	err := db.Exec(`INSERT INTO capability_items
		(id, slug, item_type, status, source_type, content_backend, catalog_entry_dir, source_path,
		 source_sha, metadata, source_git_server_id, source_git_repo_id, source_repo_path,
		 forked_from_item_id, parent_plugin_id)
		VALUES (?::uuid, ?, ?, ?, ?, ?, ?, ?, ?, ?::jsonb, ?, ?, ?, ?::uuid, ?::uuid)`,
		f.ID, f.Slug, f.ItemType, f.Status, f.SourceType, f.ContentBackend, f.CatalogEntryDir,
		f.CatalogEntryDir+"/SKILL.md", "sha-"+f.ID[:8], f.Metadata,
		f.GitServerID, f.GitRepoID, f.GitRepoPath, f.ForkedFrom, f.Parent).Error
	if err != nil {
		t.Fatalf("seed item %s: %v", f.ID, err)
	}
}

func ptr(s string) *string { return &s }

const (
	fxPlugin      = "11111111-1111-4111-8111-000000000001"
	fxCatalog1    = "11111111-1111-4111-8111-000000000002"
	fxCatalog2    = "11111111-1111-4111-8111-000000000003"
	fxArchive     = "11111111-1111-4111-8111-000000000004"
	fxIndependent = "11111111-1111-4111-8111-000000000005"
	fxDangling    = "11111111-1111-4111-8111-000000000006"
	fxBanned      = "11111111-1111-4111-8111-000000000007"
	fxForkSource  = "11111111-1111-4111-8111-000000000008"
	fxFork        = "11111111-1111-4111-8111-000000000009"
	fxStandalone  = "11111111-1111-4111-8111-00000000000a"
	fxMissing     = "11111111-1111-4111-8111-0000000000ff"
)

// seedFlattenWorld builds one fixture set exercising every classification.
func seedFlattenWorld(t *testing.T, db *gorm.DB) {
	t.Helper()
	seedFlattenItem(t, db, flattenFixture{ID: fxPlugin, ItemType: "plugin", CatalogEntryDir: "plugins/host"})
	seedFlattenItem(t, db, flattenFixture{
		ID: fxCatalog1, CatalogEntryDir: "skills/child-one",
		Metadata: `{"bundled_in":"host"}`, Parent: ptr(fxPlugin)})
	seedFlattenItem(t, db, flattenFixture{
		ID: fxCatalog2, ItemType: "mcp", CatalogEntryDir: "mcp/child-two",
		Metadata: `{"bundled_in":"host"}`, Parent: ptr(fxPlugin)})
	seedFlattenItem(t, db, flattenFixture{
		ID: fxArchive, SourceType: "archive", CatalogEntryDir: "", Parent: ptr(fxPlugin)})
	seedFlattenItem(t, db, flattenFixture{
		ID: fxIndependent, ContentBackend: "git", SourceType: "git",
		GitServerID: "gitea-local", GitRepoID: 42, GitRepoPath: "SKILL.md", Parent: ptr(fxPlugin)})
	seedFlattenItem(t, db, flattenFixture{
		ID: fxDangling, SourceType: "fork", Parent: ptr(fxMissing), ForkedFrom: ptr(fxCatalog1)})
	seedFlattenItem(t, db, flattenFixture{
		ID: fxBanned, Status: "banned", CatalogEntryDir: "skills/banned-child",
		Metadata: `{"bundled_in":"host"}`, Parent: ptr(fxPlugin)})
	// A fork whose source is itself a package child: generated provenance.
	seedFlattenItem(t, db, flattenFixture{
		ID: fxForkSource, CatalogEntryDir: "skills/fork-source",
		Metadata: `{"bundled_in":"host"}`, Parent: ptr(fxPlugin)})
	seedFlattenItem(t, db, flattenFixture{
		ID: fxFork, SourceType: "fork", Parent: ptr(fxPlugin), ForkedFrom: ptr(fxForkSource)})
	// A fork of a standalone capability that somehow carries a parent link:
	// provenance cannot prove it was generated, so it must be skipped.
	seedFlattenItem(t, db, flattenFixture{ID: fxStandalone, CatalogEntryDir: "skills/standalone"})
	seedFlattenItem(t, db, flattenFixture{
		ID: "11111111-1111-4111-8111-00000000000b", SourceType: "fork",
		Parent: ptr(fxPlugin), ForkedFrom: ptr(fxStandalone)})
}

func planFor(t *testing.T, db *gorm.DB, artifact string) (string, []pluginFlattenPlanRow) {
	t.Helper()
	runID, err := planPluginFlatten(db, pluginFlattenOptions{
		ArtifactPath: artifact, ReportLimit: 0, CreatedBy: "test",
	}, io.Discard)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := loadPluginFlattenRows(db, runID)
	if err != nil {
		t.Fatalf("load plan rows: %v", err)
	}
	return runID, rows
}

func rowsByItem(rows []pluginFlattenPlanRow) map[string]pluginFlattenPlanRow {
	out := make(map[string]pluginFlattenPlanRow, len(rows))
	for _, r := range rows {
		out[r.ItemID] = r
	}
	return out
}

func liveItem(t *testing.T, db *gorm.DB, id string) (status string, parent *string) {
	t.Helper()
	var row struct {
		Status         string
		ParentPluginID *string
	}
	if err := db.Table("capability_items").
		Select("status, parent_plugin_id::text AS parent_plugin_id").
		Where("id = ?::uuid", id).Scan(&row).Error; err != nil {
		t.Fatalf("load live item %s: %v", id, err)
	}
	return row.Status, row.ParentPluginID
}

// AC-FP6 + AC-FP13: the plan classifies by provenance, its totals reconcile with
// the database exactly, it writes nothing to capability_items, and the exported
// artifact carries one row per candidate.
func TestPluginFlatten_PlanClassifiesByProvenanceAndWritesNothing(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)
	if err := db.Exec(`INSERT INTO item_favorites (item_id, user_id) VALUES (?::uuid, 'user-1')`, fxCatalog1).Error; err != nil {
		t.Fatalf("seed favorite: %v", err)
	}
	if err := db.Exec(`INSERT INTO item_distributions (item_id, status) VALUES (?::uuid, 'active'), (?::uuid, 'revoked')`,
		fxCatalog1, fxCatalog2).Error; err != nil {
		t.Fatalf("seed distributions: %v", err)
	}

	var beforeParents, beforeArchived int64
	db.Table("capability_items").Where("parent_plugin_id IS NOT NULL").Count(&beforeParents)
	db.Table("capability_items").Where("status = 'archived'").Count(&beforeArchived)

	artifact := filepath.Join(t.TempDir(), "plan.json")
	runID, rows := planFor(t, db, artifact)

	if int64(len(rows)) != beforeParents {
		t.Fatalf("plan has %d rows, database has %d parent-linked rows", len(rows), beforeParents)
	}
	byItem := rowsByItem(rows)
	for _, tc := range []struct {
		id    string
		class string
		act   string
	}{
		{fxCatalog1, flattenClassDerivedCatalog, flattenActionArchiveAndUnlink},
		{fxCatalog2, flattenClassDerivedCatalog, flattenActionArchiveAndUnlink},
		{fxArchive, flattenClassDerivedArchive, flattenActionArchiveAndUnlink},
		{fxForkSource, flattenClassDerivedCatalog, flattenActionArchiveAndUnlink},
		{fxFork, flattenClassDerivedFork, flattenActionArchiveAndUnlink},
		{fxIndependent, flattenClassIndependent, flattenActionUnlinkOnly},
		{fxDangling, flattenClassAmbiguous, flattenActionSkip},
		{fxBanned, flattenClassAmbiguous, flattenActionSkip},
		{"11111111-1111-4111-8111-00000000000b", flattenClassAmbiguous, flattenActionSkip},
	} {
		got, ok := byItem[tc.id]
		if !ok {
			t.Fatalf("%s missing from plan", tc.id)
		}
		if got.Classification != tc.class || got.Action != tc.act {
			t.Errorf("%s classified %s/%s, want %s/%s (%s)", tc.id, got.Classification, got.Action, tc.class, tc.act, got.Reason)
		}
		if got.Action == flattenActionSkip && got.Reason == "" {
			t.Errorf("%s skipped without a reason", tc.id)
		}
	}
	if got := byItem[fxCatalog1]; got.FavoriteCount != 1 || got.DistributionCount != 1 {
		t.Errorf("consumer impact not reported: fav=%d dist=%d", got.FavoriteCount, got.DistributionCount)
	}
	// A revoked distribution is not live impact.
	if got := byItem[fxCatalog2]; got.DistributionCount != 0 {
		t.Errorf("revoked distribution counted as live impact: %d", got.DistributionCount)
	}
	// An archive-and-unlink row must never plan to keep its parent.
	for _, r := range rows {
		if r.Action == flattenActionArchiveAndUnlink && (r.AfterStatus != "archived" || r.AfterParentPluginID != nil) {
			t.Fatalf("row %s plans an inconsistent end state: %+v", r.ItemID, r)
		}
		if r.Action == flattenActionSkip && r.AfterStatus != r.BeforeStatus {
			t.Fatalf("skipped row %s plans a state change: %+v", r.ItemID, r)
		}
	}

	var afterParents, afterArchived int64
	db.Table("capability_items").Where("parent_plugin_id IS NOT NULL").Count(&afterParents)
	db.Table("capability_items").Where("status = 'archived'").Count(&afterArchived)
	if afterParents != beforeParents || afterArchived != beforeArchived {
		t.Fatalf("plan wrote to capability_items: parents %d->%d archived %d->%d",
			beforeParents, afterParents, beforeArchived, afterArchived)
	}

	raw, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	var exported pluginFlattenArtifact
	if err := json.Unmarshal(raw, &exported); err != nil {
		t.Fatalf("decode artifact: %v", err)
	}
	if exported.RunID != runID || len(exported.Rows) != len(rows) {
		t.Fatalf("artifact describes run=%s rows=%d, plan is run=%s rows=%d",
			exported.RunID, len(exported.Rows), runID, len(rows))
	}
	if exported.Totals["candidates"] != len(rows) {
		t.Fatalf("artifact totals %d disagree with its %d rows", exported.Totals["candidates"], len(exported.Rows))
	}
	if _, err := readPluginFlattenArtifact(artifact); err != nil {
		t.Fatalf("artifact does not verify against its own digest: %v", err)
	}
}

// AC-FP7 + AC-FP8 + AC-FP9 + AC-FP10: apply touches only classified rows, keeps
// independent rows active, leaves ambiguous rows alone, preserves favorites, and
// a second apply is a no-op.
func TestPluginFlatten_ApplyRetiresOnlyDerivedRowsAndRerunIsNoop(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)
	if err := db.Exec(`INSERT INTO item_favorites (item_id, user_id) VALUES (?::uuid, 'user-1')`, fxCatalog1).Error; err != nil {
		t.Fatalf("seed favorite: %v", err)
	}
	runID, _ := planFor(t, db, "")

	// Dry-run apply must still write nothing.
	if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, ReportLimit: 0}, io.Discard); err != nil {
		t.Fatalf("dry-run apply: %v", err)
	}
	if status, parent := liveItem(t, db, fxCatalog1); status != "active" || parent == nil {
		t.Fatalf("dry-run apply changed data: status=%s parent=%v", status, parent)
	}

	if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, Confirm: true, ReportLimit: 0}, io.Discard); err != nil {
		t.Fatalf("apply: %v", err)
	}

	for _, id := range []string{fxCatalog1, fxCatalog2, fxArchive, fxForkSource, fxFork} {
		status, parent := liveItem(t, db, id)
		if status != "archived" || parent != nil {
			t.Errorf("derived row %s: status=%s parent=%v, want archived/nil", id, status, parent)
		}
	}
	if status, parent := liveItem(t, db, fxIndependent); status != "active" || parent != nil {
		t.Errorf("independent row: status=%s parent=%v, want active/nil", status, parent)
	}
	// Ambiguous rows keep BOTH their status and their parent link.
	for _, id := range []string{fxDangling, fxBanned, "11111111-1111-4111-8111-00000000000b"} {
		_, parent := liveItem(t, db, id)
		if parent == nil {
			t.Errorf("ambiguous row %s was unlinked", id)
		}
	}
	if status, _ := liveItem(t, db, fxBanned); status != "banned" {
		t.Errorf("banned row status became %s", status)
	}
	if status, _ := liveItem(t, db, fxPlugin); status != "active" {
		t.Errorf("parent plugin was archived: %s", status)
	}
	// SD-3: the favorite is preserved on the archived row, not moved.
	var favOnChild, favOnPlugin int64
	db.Table("item_favorites").Where("item_id = ?::uuid", fxCatalog1).Count(&favOnChild)
	db.Table("item_favorites").Where("item_id = ?::uuid", fxPlugin).Count(&favOnPlugin)
	if favOnChild != 1 || favOnPlugin != 0 {
		t.Errorf("favorite moved: child=%d plugin=%d", favOnChild, favOnPlugin)
	}
	// Nothing is hard-deleted.
	var itemCount int64
	db.Table("capability_items").Count(&itemCount)
	if itemCount != 11 {
		t.Errorf("item count = %d, want 11; the migration must not delete rows", itemCount)
	}

	run, err := loadPluginFlattenRun(db, runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.Status != flattenRunApplied {
		t.Fatalf("run status = %s, want applied", run.Status)
	}

	// Rerun: no pending work, no further change.
	if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, Confirm: true, ReportLimit: 0}, io.Discard); err != nil {
		t.Fatalf("rerun apply: %v", err)
	}
	rows, err := loadPluginFlattenRows(db, runID)
	if err != nil {
		t.Fatalf("reload rows: %v", err)
	}
	for _, r := range rows {
		if r.Action != flattenActionSkip && r.RowState != flattenRowApplied {
			t.Errorf("row %s is %s after a successful rerun", r.ItemID, r.RowState)
		}
		if r.Conflict != "" {
			t.Errorf("rerun invented a conflict on %s: %s", r.ItemID, r.Conflict)
		}
	}
}

// AC-FP14: a row somebody changed between plan and apply fails the
// compare-and-set, is reported, and is left exactly as the third party left it.
func TestPluginFlatten_ConcurrentChangeIsSkippedNotOverwritten(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)
	runID, _ := planFor(t, db, "")

	// Two different concurrent edits: a status change and a re-parent.
	if err := db.Exec(`UPDATE capability_items SET status = 'inactive' WHERE id = ?::uuid`, fxCatalog1).Error; err != nil {
		t.Fatalf("concurrent status change: %v", err)
	}
	if err := db.Exec(`UPDATE capability_items SET parent_plugin_id = NULL WHERE id = ?::uuid`, fxCatalog2).Error; err != nil {
		t.Fatalf("concurrent unlink: %v", err)
	}

	if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, Confirm: true, ReportLimit: 0}, io.Discard); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if status, parent := liveItem(t, db, fxCatalog1); status != "inactive" || parent == nil {
		t.Errorf("concurrent status change was overwritten: status=%s parent=%v", status, parent)
	}
	// fxCatalog2 was unlinked but not archived; the plan wanted archived+NULL, so
	// its live state is neither the before-state nor the after-state and it must
	// be reported rather than completed halfway.
	if status, parent := liveItem(t, db, fxCatalog2); status != "active" || parent != nil {
		t.Errorf("partially changed row was written anyway: status=%s parent=%v", status, parent)
	}

	rows, err := loadPluginFlattenRows(db, runID)
	if err != nil {
		t.Fatalf("load rows: %v", err)
	}
	byItem := rowsByItem(rows)
	for _, id := range []string{fxCatalog1, fxCatalog2} {
		r := byItem[id]
		if r.RowState != flattenRowSkipped {
			t.Errorf("row %s state = %s, want skipped", id, r.RowState)
		}
		if !strings.Contains(r.Conflict, "concurrent change") {
			t.Errorf("row %s conflict = %q, want a concurrent-change explanation", id, r.Conflict)
		}
	}
	run, err := loadPluginFlattenRun(db, runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.Status != flattenRunApplied {
		t.Fatalf("run status = %s; skipped rows are a completed outcome, not pending work", run.Status)
	}
	if run.Totals["state_"+flattenRowSkipped] < 2 {
		t.Errorf("run totals under-report skips: %+v", run.Totals)
	}
}

// AC-FP16: a run interrupted between batches resumes by id, completing exactly
// the rows that were still pending and re-applying none.
func TestPluginFlatten_CrashBetweenBatchesResumesByRunID(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)
	runID, rows := planFor(t, db, "")

	pending := make([]pluginFlattenPlanRow, 0, len(rows))
	for _, r := range rows {
		if r.Action != flattenActionSkip {
			pending = append(pending, r)
		}
	}
	if len(pending) < 3 {
		t.Fatalf("fixture too small to interrupt: %d actionable rows", len(pending))
	}

	// Simulate the process dying after the first batch committed: apply one
	// batch by hand, then leave the run in `applying`.
	applied, skipped, err := applyPluginFlattenBatch(db, runID, pending[:1])
	if err != nil || applied != 1 || skipped != 0 {
		t.Fatalf("first batch: applied=%d skipped=%d err=%v", applied, skipped, err)
	}
	if err := db.Exec(`UPDATE plugin_flatten_migration_runs SET status = ?, started_at = now() WHERE id = ?::uuid`,
		flattenRunApplying, runID).Error; err != nil {
		t.Fatalf("mark applying: %v", err)
	}

	firstAppliedAt := rowAppliedAt(t, db, runID, pending[0].ItemID)

	// Resume.
	if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, Confirm: true, ReportLimit: 0}, io.Discard); err != nil {
		t.Fatalf("resume apply: %v", err)
	}
	if got := rowAppliedAt(t, db, runID, pending[0].ItemID); !got.Equal(firstAppliedAt) {
		t.Errorf("already-applied row was touched again: %s -> %s", firstAppliedAt, got)
	}
	for _, r := range pending {
		status, parent := liveItem(t, db, r.ItemID)
		if status != r.AfterStatus || parent != nil {
			t.Errorf("row %s did not converge: status=%s parent=%v", r.ItemID, status, parent)
		}
	}
	run, err := loadPluginFlattenRun(db, runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.Status != flattenRunApplied {
		t.Fatalf("resumed run status = %s, want applied", run.Status)
	}
	if run.Totals["state_"+flattenRowPending] != 0 {
		t.Fatalf("resumed run still reports pending rows: %+v", run.Totals)
	}
}

func rowAppliedAt(t *testing.T, db *gorm.DB, runID, itemID string) time.Time {
	t.Helper()
	var at *time.Time
	if err := db.Table("plugin_flatten_migration_rows").
		Where("run_id = ?::uuid AND item_id = ?::uuid", runID, itemID).
		Pluck("applied_at", &at).Error; err != nil {
		t.Fatalf("read applied_at: %v", err)
	}
	if at == nil {
		t.Fatalf("row %s has no applied_at", itemID)
	}
	return *at
}

// AC-FP16: a plan whose stored rows no longer hash to the recorded digest is
// refused before any write.
func TestPluginFlatten_ApplyRefusesTamperedPlan(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)
	runID, _ := planFor(t, db, "")

	if err := db.Exec(`UPDATE plugin_flatten_migration_rows
		SET after_status = 'inactive' WHERE run_id = ?::uuid AND item_id = ?::uuid`,
		runID, fxCatalog1).Error; err != nil {
		t.Fatalf("tamper: %v", err)
	}
	err := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, Confirm: true, ReportLimit: 0}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("apply accepted a tampered plan: %v", err)
	}
	if status, parent := liveItem(t, db, fxCatalog2); status != "active" || parent == nil {
		t.Fatalf("a refused apply still wrote: status=%s parent=%v", status, parent)
	}
}

// An artifact edited on disk must not pass verification either, even when its
// own planDigest header was left untouched.
func TestPluginFlatten_ArtifactVerificationRejectsEdits(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)
	artifact := filepath.Join(t.TempDir(), "plan.json")
	runID, _ := planFor(t, db, artifact)

	raw, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	var decoded pluginFlattenArtifact
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for i := range decoded.Rows {
		if decoded.Rows[i].ItemID == fxBanned {
			decoded.Rows[i].Action = flattenActionArchiveAndUnlink
			decoded.Rows[i].AfterStatus = "archived"
		}
	}
	edited, _ := json.MarshalIndent(decoded, "", "  ")
	if err := os.WriteFile(artifact, edited, 0o644); err != nil {
		t.Fatalf("write edited artifact: %v", err)
	}
	if _, err := readPluginFlattenArtifact(artifact); err == nil {
		t.Fatal("edited artifact verified successfully")
	}
	err = applyPluginFlatten(db, pluginFlattenOptions{
		RunID: runID, ArtifactPath: artifact, Confirm: true, ReportLimit: 0,
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("apply accepted an edited artifact: %v", err)
	}
	if status, _ := liveItem(t, db, fxBanned); status != "banned" {
		t.Fatalf("banned row was archived by an edited artifact: %s", status)
	}
}

// AC-FP17: rollback plans and applies as its own run, restores only rows the
// migration actually applied, and never overwrites a change made afterwards.
func TestPluginFlatten_RollbackRestoresOnlyUnchangedAppliedRows(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)
	runID, _ := planFor(t, db, "")
	if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, Confirm: true, ReportLimit: 0}, io.Discard); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// After the migration, somebody legitimately reactivates one archived row.
	if err := db.Exec(`UPDATE capability_items SET status = 'active' WHERE id = ?::uuid`, fxCatalog2).Error; err != nil {
		t.Fatalf("post-migration change: %v", err)
	}

	rollbackArtifact := filepath.Join(t.TempDir(), "rollback.json")
	rollbackID, err := planPluginFlattenRollback(db, pluginFlattenOptions{
		RunID: runID, ArtifactPath: rollbackArtifact, ReportLimit: 0, CreatedBy: "test",
	}, io.Discard)
	if err != nil {
		t.Fatalf("rollback-plan: %v", err)
	}
	// Planning a rollback changes nothing.
	if status, _ := liveItem(t, db, fxCatalog1); status != "archived" {
		t.Fatalf("rollback-plan wrote data: %s", status)
	}

	rollbackRows, err := loadPluginFlattenRows(db, rollbackID)
	if err != nil {
		t.Fatalf("load rollback rows: %v", err)
	}
	byItem := rowsByItem(rollbackRows)
	// Rows the migration skipped are not restorable — it never changed them.
	for _, id := range []string{fxDangling, fxBanned} {
		if _, present := byItem[id]; present {
			t.Errorf("rollback plans to touch %s, which the migration never changed", id)
		}
	}
	if r, ok := byItem[fxCatalog1]; !ok || r.AfterStatus != "active" || derefOr(r.AfterParentPluginID, "") != fxPlugin {
		t.Fatalf("rollback does not restore %s to its pre-migration state: %+v", fxCatalog1, r)
	}

	// Dry-run rollback writes nothing.
	if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: rollbackID, ReportLimit: 0}, io.Discard); err != nil {
		t.Fatalf("rollback dry-run: %v", err)
	}
	if status, parent := liveItem(t, db, fxCatalog1); status != "archived" || parent != nil {
		t.Fatalf("rollback dry-run wrote: status=%s parent=%v", status, parent)
	}

	if err := applyPluginFlatten(db, pluginFlattenOptions{
		RunID: rollbackID, ArtifactPath: rollbackArtifact, Confirm: true, ReportLimit: 0,
	}, io.Discard); err != nil {
		t.Fatalf("rollback apply: %v", err)
	}

	if status, parent := liveItem(t, db, fxCatalog1); status != "active" || derefOr(parent, "") != fxPlugin {
		t.Errorf("rollback did not restore %s: status=%s parent=%v", fxCatalog1, status, parent)
	}
	// The post-migration change wins: its row's compare-and-set fails.
	if status, parent := liveItem(t, db, fxCatalog2); status != "active" || parent != nil {
		t.Errorf("rollback overwrote a post-migration change: status=%s parent=%v", status, parent)
	}
	rollbackRows, err = loadPluginFlattenRows(db, rollbackID)
	if err != nil {
		t.Fatalf("reload rollback rows: %v", err)
	}
	if r := rowsByItem(rollbackRows)[fxCatalog2]; r.RowState != flattenRowSkipped || r.Conflict == "" {
		t.Errorf("post-migration change not reported as a conflict: %+v", r)
	}
	run, err := loadPluginFlattenRun(db, rollbackID)
	if err != nil {
		t.Fatalf("load rollback run: %v", err)
	}
	if run.Status != flattenRunRolledBack {
		t.Fatalf("rollback run status = %s, want rolled_back", run.Status)
	}
}

// The window is a policy gate on rollback, and --force is the only way past it.
// It is checked at plan time so an operator learns before building the plan.
func TestPluginFlatten_RollbackWindowIsEnforced(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)
	runID, _ := planFor(t, db, "")
	if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, Confirm: true, ReportLimit: 0}, io.Discard); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := db.Exec(`UPDATE plugin_flatten_migration_runs SET planned_at = now() - interval '400 days' WHERE id = ?::uuid`,
		runID).Error; err != nil {
		t.Fatalf("age the run: %v", err)
	}

	_, err := planPluginFlattenRollback(db, pluginFlattenOptions{RunID: runID, ReportLimit: 0}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "compatibility window") {
		t.Fatalf("stale run was rollback-planned without --force: %v", err)
	}
	if _, err := planPluginFlattenRollback(db, pluginFlattenOptions{
		RunID: runID, Force: true, ReportLimit: 0,
	}, io.Discard); err != nil {
		t.Fatalf("--force did not override the window: %v", err)
	}
}

// The classifier is a pure function; these are the cases that decide whether a
// row is touched at all, pinned without a database.
func TestClassifyFlattenCandidate_Rules(t *testing.T) {
	base := flattenCandidate{
		ItemID: "item-1", ItemType: "skill", Status: "active", SourceType: "direct",
		ContentBackend: "db", ParentPluginID: "plugin-1", ParentExists: true, ParentItemType: "plugin",
		CatalogEntryDir: "skills/child", Metadata: []byte(`{"bundled_in":"host"}`),
	}
	mutate := func(fn func(*flattenCandidate)) flattenCandidate {
		c := base
		fn(&c)
		return c
	}

	for _, tc := range []struct {
		name  string
		in    flattenCandidate
		class string
		act   string
	}{
		{"catalog child", base, flattenClassDerivedCatalog, flattenActionArchiveAndUnlink},
		{"catalog child without bundled_in", mutate(func(c *flattenCandidate) { c.Metadata = []byte(`{}`) }),
			flattenClassAmbiguous, flattenActionSkip},
		{"catalog child without entry dir", mutate(func(c *flattenCandidate) { c.CatalogEntryDir = "" }),
			flattenClassAmbiguous, flattenActionSkip},
		{"archive child", mutate(func(c *flattenCandidate) { c.SourceType = "archive" }),
			flattenClassDerivedArchive, flattenActionArchiveAndUnlink},
		{"fork of a package child", mutate(func(c *flattenCandidate) {
			c.SourceType = "fork"
			c.ForkedFromItemID = ptr("src-1")
			c.ForkSourceLinked = true
		}), flattenClassDerivedFork, flattenActionArchiveAndUnlink},
		{"fork of a standalone", mutate(func(c *flattenCandidate) {
			c.SourceType = "fork"
			c.ForkedFromItemID = ptr("src-1")
		}), flattenClassAmbiguous, flattenActionSkip},
		{"fork without a source", mutate(func(c *flattenCandidate) { c.SourceType = "fork" }),
			flattenClassAmbiguous, flattenActionSkip},
		{"complete git coordinate", mutate(func(c *flattenCandidate) {
			c.ContentBackend = "git"
			c.GitServerID = "gitea"
			c.GitRepoID = 7
			c.GitRepoPath = "SKILL.md"
		}), flattenClassIndependent, flattenActionUnlinkOnly},
		{"partial git coordinate", mutate(func(c *flattenCandidate) {
			c.ContentBackend = "git"
			c.GitServerID = "gitea"
		}), flattenClassAmbiguous, flattenActionSkip},
		{"dangling parent", mutate(func(c *flattenCandidate) { c.ParentExists = false }),
			flattenClassAmbiguous, flattenActionSkip},
		{"non-plugin parent", mutate(func(c *flattenCandidate) { c.ParentItemType = "skill" }),
			flattenClassAmbiguous, flattenActionSkip},
		{"self link", mutate(func(c *flattenCandidate) { c.ParentPluginID = c.ItemID }),
			flattenClassAmbiguous, flattenActionSkip},
		{"plugin with a parent", mutate(func(c *flattenCandidate) { c.ItemType = "plugin" }),
			flattenClassAmbiguous, flattenActionSkip},
		{"banned row", mutate(func(c *flattenCandidate) { c.Status = "banned" }),
			flattenClassAmbiguous, flattenActionSkip},
		{"unknown source type", mutate(func(c *flattenCandidate) { c.SourceType = "wormhole" }),
			flattenClassAmbiguous, flattenActionSkip},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyFlattenCandidate(tc.in)
			if got.Classification != tc.class || got.Action != tc.act {
				t.Fatalf("got %s/%s, want %s/%s (%s)", got.Classification, got.Action, tc.class, tc.act, got.Reason)
			}
			if got.Action == flattenActionSkip {
				if got.Reason == "" {
					t.Error("skip without a reason")
				}
				if got.AfterStatus != tc.in.Status || derefOr(got.AfterParent, "") != tc.in.ParentPluginID {
					t.Errorf("skip plans a change: status=%s parent=%v", got.AfterStatus, got.AfterParent)
				}
			}
			if got.Action == flattenActionArchiveAndUnlink && (got.AfterStatus != "archived" || got.AfterParent != nil) {
				t.Errorf("archive verdict is inconsistent: %+v", got)
			}
			if got.Action == flattenActionUnlinkOnly && (got.AfterStatus != tc.in.Status || got.AfterParent != nil) {
				t.Errorf("unlink verdict is inconsistent: %+v", got)
			}
		})
	}
}
