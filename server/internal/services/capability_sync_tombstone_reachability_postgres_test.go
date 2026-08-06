package services

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Tombstone writing under a search_path that does not name CURRENT_SCHEMA().
//
// # What this is defending
//
// recordHolderTombstonesTx is the single choke point every archive path goes
// through (Git lifecycle, moderation take-down, hard delete, flatten
// migration). It used to open with
//
//	if !tx.Migrator().HasTable(&models.CapabilitySyncTombstone{}) { return 0, nil }
//
// which on PostgreSQL asks about CURRENT_SCHEMA() while the INSERT beneath it
// resolves through the whole search_path. Where those disagree the archive
// still happened, not one tombstone was written, and the caller got `nil`. That
// is F-27 — capability stays installed on every holder's device forever —
// restored in full and with no error anywhere, which is strictly worse than the
// original, where a CHECK violation at least rolled the transaction back.
//
// The fixture below is deliberately the ordinary shape, not a contrived one:
// PostgreSQL's stock `search_path = "$user", public` produces exactly this
// divergence as soon as a schema named after the connecting role exists.

// tombstoneReachabilityFixture describes which half of the trap to build.
type tombstoneReachabilityFixture struct {
	// divergent puts the tables in a schema that search_path reaches but
	// CURRENT_SCHEMA() does not name.
	divergent bool
	// withTombstoneTable applies the snapshot migrations that own
	// capability_sync_tombstones.
	withTombstoneTable bool
	// dropTables are removed after the schema is built, to model a database
	// missing one of the tables the holder set is derived from.
	dropTables []string
}

func newTombstoneReachabilityDB(t *testing.T, fixture tombstoneReachabilityFixture) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL tombstone reachability test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	stamp := time.Now().UnixNano()
	front := fmt.Sprintf("tomb_front_%d", stamp)
	back := fmt.Sprintf("tomb_back_%d", stamp)

	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	schemas := []string{back}
	if fixture.divergent {
		schemas = append([]string{front}, schemas...)
	}
	for _, schema := range schemas {
		if err := admin.Exec("CREATE SCHEMA " + quoteReachabilitySchema(schema)).Error; err != nil {
			t.Fatalf("create schema %s: %v", schema, err)
		}
	}
	t.Cleanup(func() {
		for _, schema := range schemas {
			_ = admin.Exec("DROP SCHEMA " + quoteReachabilitySchema(schema) + " CASCADE").Error
		}
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	// Build the schema through a connection pointed straight at `back`, so the
	// unqualified DDL in the shared fixture/migration text lands there.
	builder := openReachabilityDB(t, parsed, back)
	for _, ddl := range snapshotFixtureTables {
		if err := builder.Exec(ddl).Error; err != nil {
			t.Fatalf("create fixture table: %v\nSQL: %s", err, ddl)
		}
	}
	if fixture.withTombstoneTable {
		for _, file := range snapshotMigrations {
			if err := builder.Exec(readSnapshotMigrationUp(t, file)).Error; err != nil {
				t.Fatalf("apply migration %s: %v", file, err)
			}
		}
	}
	for _, table := range fixture.dropTables {
		if err := builder.Exec("DROP TABLE " + quoteReachabilitySchema(back) + "." + table + " CASCADE").Error; err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	if sqlDB, err := builder.DB(); err == nil {
		_ = sqlDB.Close()
	}

	searchPath := back
	if fixture.divergent {
		searchPath = front + "," + back
	}
	db := openReachabilityDB(t, parsed, searchPath)
	if fixture.divergent {
		var current string
		if err := db.Raw("SELECT current_schema()").Scan(&current).Error; err != nil {
			t.Fatalf("read current_schema(): %v", err)
		}
		if current != front {
			t.Fatalf("fixture did not reproduce the divergence: current_schema() = %q, want %q", current, front)
		}
	}
	return db
}

func openReachabilityDB(t *testing.T, base *url.URL, searchPath string) *gorm.DB {
	t.Helper()
	parsed := *base
	query := parsed.Query()
	query.Set("search_path", searchPath)
	parsed.RawQuery = query.Encode()
	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL with search_path=%s: %v", searchPath, err)
	}
	return db
}

func quoteReachabilitySchema(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// archiveInOneTransaction models every real caller: the status flip and the
// tombstone write share one transaction, so a refusal to record the removal
// must take the archive down with it.
func archiveInOneTransaction(db *gorm.DB, itemID, lifecycleReason string) (int, error) {
	written := 0
	err := db.Transaction(func(tx *gorm.DB) error {
		res := tx.Exec(`UPDATE capability_items SET status = 'archived' WHERE id = ? AND status = 'active'`, itemID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("compare-and-set claimed no transition for %s", itemID)
		}
		n, err := RecordGitArchiveTombstonesTx(tx, itemID, lifecycleReason, time.Now())
		if err != nil {
			return err
		}
		written = n
		return nil
	})
	return written, err
}

func itemStatus(t *testing.T, db *gorm.DB, itemID string) string {
	t.Helper()
	var status string
	if err := db.Raw(`SELECT status FROM capability_items WHERE id = ?`, itemID).Scan(&status).Error; err != nil {
		t.Fatalf("read status of %s: %v", itemID, err)
	}
	return status
}

func countTombstonesForItem(t *testing.T, db *gorm.DB, itemID string) int64 {
	t.Helper()
	var n int64
	if err := db.Raw(`SELECT count(*) FROM capability_sync_tombstones WHERE item_id = ?`, itemID).Scan(&n).Error; err != nil {
		t.Fatalf("count tombstones of %s: %v", itemID, err)
	}
	return n
}

// TestArchive_WritesTombstonesWhenTheTableIsOnlyReachableViaSearchPath is the
// regression test for the silent failure. Before the fix this archived the item
// and returned (0, nil): no tombstone, no error, capability installed forever.
func TestArchive_WritesTombstonesWhenTheTableIsOnlyReachableViaSearchPath(t *testing.T) {
	db := newTombstoneReachabilityDB(t, tombstoneReachabilityFixture{
		divergent:          true,
		withTombstoneTable: true,
	})

	// Pin the trap itself. If gorm ever starts resolving through search_path
	// this fails loudly rather than letting the test pass for the wrong reason.
	if db.Migrator().HasTable(&models.CapabilitySyncTombstone{}) {
		t.Fatal("fixture no longer reproduces the CURRENT_SCHEMA()/search_path divergence")
	}

	item := snapshotItemID(91)
	seedSnapshotItem(t, db, item, "reachable", "active")
	seedSnapshotFavorite(t, db, item, snapshotUserA)
	seedModerationDistribution(t, db, item, snapshotUserB)

	written, err := archiveInOneTransaction(db, item, models.GitLifecycleReasonManifestRemoved)
	if err != nil {
		t.Fatalf("archive must succeed against a schema whose tombstone table the INSERT reaches: %v", err)
	}
	if written != 2 {
		t.Fatalf("tombstones written = %d, want 2 (one favorite holder, one distribution holder)", written)
	}
	if got := countTombstonesForItem(t, db, item); got != 2 {
		t.Fatalf("rows in capability_sync_tombstones = %d, want 2", got)
	}
	if got := itemStatus(t, db, item); got != "archived" {
		t.Fatalf("item status = %q, want archived", got)
	}
}

// TestArchive_RefusesWhenTheTombstoneTableIsUnreachable covers the other half of
// the contract. A schema that genuinely cannot record the removal must not be
// allowed to archive: the removal instruction and the archive are one
// transaction, so refusing keeps the two consistent.
func TestArchive_RefusesWhenTheTombstoneTableIsUnreachable(t *testing.T) {
	db := newTombstoneReachabilityDB(t, tombstoneReachabilityFixture{withTombstoneTable: false})

	item := snapshotItemID(92)
	seedSnapshotItem(t, db, item, "unreachable", "active")
	seedSnapshotFavorite(t, db, item, snapshotUserA)

	_, err := archiveInOneTransaction(db, item, models.GitLifecycleReasonRepositoryDeleted)
	if err == nil {
		t.Fatal("archiving against a schema with no tombstone table must fail, not silently succeed")
	}
	if !errors.Is(err, models.ErrSchemaObjectUnreachable) {
		t.Fatalf("error must identify the unreachable object, got: %v", err)
	}
	if got := itemStatus(t, db, item); got != "active" {
		t.Fatalf("item status = %q, want active: the refused tombstone must roll the archive back", got)
	}
}

// TestArchive_RefusesWhenAHolderTableIsUnreachable guards the other silent
// undercount. A missing item_favorites used to mean "nobody favorited this",
// so the archive succeeded, reported a plausible non-zero count from the
// distribution side, and left every favorite holder's device untouched.
func TestArchive_RefusesWhenAHolderTableIsUnreachable(t *testing.T) {
	db := newTombstoneReachabilityDB(t, tombstoneReachabilityFixture{
		withTombstoneTable: true,
		dropTables:         []string{"item_favorites"},
	})

	item := snapshotItemID(93)
	seedSnapshotItem(t, db, item, "no-favorites-table", "active")
	seedModerationDistribution(t, db, item, snapshotUserB)

	_, err := archiveInOneTransaction(db, item, models.GitLifecycleReasonManifestRemoved)
	if err == nil {
		t.Fatal("a holder set that cannot be computed must not be reported as a complete one")
	}
	if !errors.Is(err, models.ErrSchemaObjectUnreachable) {
		t.Fatalf("error must identify the unreachable object, got: %v", err)
	}
	if got := itemStatus(t, db, item); got != "active" {
		t.Fatalf("item status = %q, want active", got)
	}
	if got := countTombstonesForItem(t, db, item); got != 0 {
		t.Fatalf("rows in capability_sync_tombstones = %d, want 0: a partial holder set must write nothing", got)
	}
}

// TestArchive_RecordsEveryHolderWhenNobodyHoldsIt keeps the precondition honest
// for the case that used to hide it: an item with no holders writes no row, so
// without a positive probe a broken schema stays invisible until the first
// archived item somebody had installed.
func TestArchive_RecordsEveryHolderWhenNobodyHoldsIt(t *testing.T) {
	db := newTombstoneReachabilityDB(t, tombstoneReachabilityFixture{
		divergent:          true,
		withTombstoneTable: true,
	})

	item := snapshotItemID(94)
	seedSnapshotItem(t, db, item, "unheld", "active")

	written, err := archiveInOneTransaction(db, item, models.GitLifecycleReasonDefaultBranchMissing)
	if err != nil {
		t.Fatalf("archiving an item nobody holds must succeed: %v", err)
	}
	if written != 0 {
		t.Fatalf("tombstones written = %d, want 0", written)
	}
	if got := itemStatus(t, db, item); got != "archived" {
		t.Fatalf("item status = %q, want archived", got)
	}
}
