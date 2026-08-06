package adminitem

import (
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// Review finding F-27, moderation half: taking an item off the shelf ended
// every holder's entitlement and told nobody. Under snapshot v2 an item's
// absence never authorizes a client to unload it, so the capability stayed
// installed on every one of those devices forever — with no error anywhere,
// because the server had nothing to notice and the client was never told.
//
// These tests are about WHEN the instruction is issued, which is a different
// question from what it says (services' Postgres suite covers the wire shape
// and the constraint). The distinction that matters here is "a removal
// happened" versus "the row is already off the shelf", because the event id
// rotates on the first and must not on the second.

func adminSeedHolder(t *testing.T, db *gorm.DB, itemID, userID string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO item_favorites (id, item_id, user_id) VALUES (?, ?, ?)`,
		userID+":"+itemID, itemID, userID).Error; err != nil {
		t.Fatalf("seed favorite: %v", err)
	}
}

func adminSeedDistributionHolder(t *testing.T, db *gorm.DB, itemID, userID string) {
	t.Helper()
	distributionID := "dist:" + itemID + ":" + userID
	if err := db.Exec(`INSERT INTO item_distributions (id, item_id, status) VALUES (?, ?, 'active')`,
		distributionID, itemID).Error; err != nil {
		t.Fatalf("seed distribution: %v", err)
	}
	if err := db.Exec(`INSERT INTO item_distribution_receipts (id, distribution_id, user_id, receipt_status)
		VALUES (?, ?, ?, 'unread')`, "receipt:"+distributionID, distributionID, userID).Error; err != nil {
		t.Fatalf("seed receipt: %v", err)
	}
}

func adminLoadTombstones(t *testing.T, db *gorm.DB, itemID string) []models.CapabilitySyncTombstone {
	t.Helper()
	var rows []models.CapabilitySyncTombstone
	if err := db.Where("item_id = ?", itemID).Order("user_id").Find(&rows).Error; err != nil {
		t.Fatalf("load tombstones: %v", err)
	}
	return rows
}

func adminItemStatus(t *testing.T, db *gorm.DB, itemID string) string {
	t.Helper()
	var status string
	if err := db.Raw(`SELECT status FROM capability_items WHERE id = ?`, itemID).Row().Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return status
}

func TestSetStatus_ArchiveTombstonesEveryHolder(t *testing.T) {
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "item-1", "One", "skill", "active", "clean", "author", 1)
	adminSeedHolder(t, db, "item-1", "holder-fav")
	adminSeedDistributionHolder(t, db, "item-1", "holder-dist")

	svc := NewService(db)
	if err := svc.SetStatus("item-1", StatusArchived); err != nil {
		t.Fatalf("archive: %v", err)
	}

	if got := adminItemStatus(t, db, "item-1"); got != StatusArchived {
		t.Fatalf("status = %q, want archived", got)
	}
	rows := adminLoadTombstones(t, db, "item-1")
	if len(rows) != 2 {
		t.Fatalf("tombstones = %d, want one per holder (favorite + distribution)", len(rows))
	}
	for _, row := range rows {
		if row.Reason != models.SyncTombstoneReasonAdminArchived {
			t.Errorf("user %s: reason = %q, want admin_archived", row.UserID, row.Reason)
		}
		if row.Source != models.SyncTombstoneSourceModeration {
			t.Errorf("user %s: source = %q, want moderation", row.UserID, row.Source)
		}
		if row.LifecycleReason != nil {
			t.Errorf("user %s: lifecycleReason = %q, want NULL", row.UserID, *row.LifecycleReason)
		}
	}
}

// The rotation rule's negative half, and the reason SetStatus needed a
// compare-and-set rather than the read-then-write it used to do.
//
// EventID is csc's dedup key. Rotating it when nothing changed makes every
// subsequent poll look like a fresh removal and re-runs removal work
// indefinitely; the row was already off the shelf, so no entitlement ended.
func TestSetStatus_RepeatedArchiveDoesNotRotateTheEventID(t *testing.T) {
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "item-1", "One", "skill", "active", "clean", "author", 1)
	adminSeedHolder(t, db, "item-1", "holder")

	svc := NewService(db)
	if err := svc.SetStatus("item-1", StatusArchived); err != nil {
		t.Fatalf("first archive: %v", err)
	}
	first := adminLoadTombstones(t, db, "item-1")
	if len(first) != 1 {
		t.Fatalf("tombstones after first archive = %d, want 1", len(first))
	}

	if err := svc.SetStatus("item-1", StatusArchived); err != nil {
		t.Fatalf("second archive: %v", err)
	}
	second := adminLoadTombstones(t, db, "item-1")
	if len(second) != 1 {
		t.Fatalf("tombstones after second archive = %d, want 1", len(second))
	}
	if second[0].EventID != first[0].EventID {
		t.Fatal("re-archiving an already-archived item rotated the event id; no entitlement ended, so every poll would look like a fresh removal and re-run removal work forever")
	}
	if !second[0].RemovedAt.Equal(first[0].RemovedAt) {
		t.Error("re-archiving rewrote removed_at for a transition that did not happen")
	}
}

// The rotation rule's positive half. unarchive -> re-archive IS a second
// removal, and a client that already applied the first would dedup the second
// away on a stable id, leaving the capability installed forever.
func TestSetStatus_RestoreThenArchiveRotatesTheEventID(t *testing.T) {
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "item-1", "One", "skill", "active", "clean", "author", 1)
	adminSeedHolder(t, db, "item-1", "holder")

	svc := NewService(db)
	if err := svc.SetStatus("item-1", StatusArchived); err != nil {
		t.Fatalf("first archive: %v", err)
	}
	first := adminLoadTombstones(t, db, "item-1")[0]

	if err := svc.SetStatus("item-1", StatusActive); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// A restore deletes nothing: the active row supersedes the tombstone when
	// the snapshot is built, which keeps the record for a device that has been
	// offline since the take-down.
	if rows := adminLoadTombstones(t, db, "item-1"); len(rows) != 1 {
		t.Fatalf("restore changed the tombstone count to %d, want the row left in place", len(rows))
	}

	if err := svc.SetStatus("item-1", StatusArchived); err != nil {
		t.Fatalf("second archive: %v", err)
	}
	second := adminLoadTombstones(t, db, "item-1")
	if len(second) != 1 {
		t.Fatalf("tombstones = %d, want one terminal record per (user,item)", len(second))
	}
	if second[0].EventID == first.EventID {
		t.Fatal("a genuine second removal must rotate the event id, or a client that applied the first dedups the second away")
	}
}

func TestSetStatus_ArchiveWithNoHoldersWritesNothing(t *testing.T) {
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "item-1", "One", "skill", "active", "clean", "author", 1)

	if err := NewService(db).SetStatus("item-1", StatusArchived); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if rows := adminLoadTombstones(t, db, "item-1"); len(rows) != 0 {
		t.Fatalf("tombstones = %d, want 0: nobody held the capability", len(rows))
	}
}

// A holder who both favorited the item and holds a live receipt is one holder.
// The UNIQUE (user_id, item_id) constraint would reject a second row, so a
// writer that did not deduplicate would fail the take-down outright rather than
// duplicate quietly.
func TestSetStatus_DualSourceHolderGetsOneTombstone(t *testing.T) {
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "item-1", "One", "skill", "active", "clean", "author", 1)
	adminSeedHolder(t, db, "item-1", "holder")
	adminSeedDistributionHolder(t, db, "item-1", "holder")

	if err := NewService(db).SetStatus("item-1", StatusArchived); err != nil {
		t.Fatalf("archive: %v", err)
	}
	rows := adminLoadTombstones(t, db, "item-1")
	if len(rows) != 1 {
		t.Fatalf("tombstones = %d, want 1 for one principal holding two relationships", len(rows))
	}
}

// A batch take-down is the same event as N single ones. Nothing about batching
// changes what a removal is, so the batch path shares SetStatus's per-item
// transition logic rather than carrying its own copy.
func TestBatchSetStatus_ArchiveTombstonesEveryHolder(t *testing.T) {
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "item-1", "One", "skill", "active", "clean", "author", 1)
	seedItem(t, db, "item-2", "Two", "skill", "active", "clean", "author", 1)
	seedItem(t, db, "item-3", "Three", "skill", "archived", "clean", "author", 1)
	adminSeedHolder(t, db, "item-1", "holder-a")
	adminSeedHolder(t, db, "item-2", "holder-b")
	adminSeedHolder(t, db, "item-3", "holder-c")

	updated, skipped, err := NewService(db).BatchSetStatus(
		[]string{"item-1", "item-2", "item-3", "missing"}, StatusArchived)
	if err != nil {
		t.Fatalf("batch archive: %v", err)
	}
	if len(updated) != 3 {
		t.Fatalf("updated = %v, want the three existing ids", updated)
	}
	if len(skipped) != 1 || skipped[0] != "missing" {
		t.Fatalf("skipped = %v, want [missing]", skipped)
	}

	for _, id := range []string{"item-1", "item-2"} {
		if rows := adminLoadTombstones(t, db, id); len(rows) != 1 {
			t.Errorf("%s: tombstones = %d, want 1", id, len(rows))
		}
	}
	// item-3 was ALREADY archived: reporting it as updated is the endpoint's
	// long-standing contract (the requested status now holds), but no
	// entitlement ended, so it must not produce a removal instruction.
	if rows := adminLoadTombstones(t, db, "item-3"); len(rows) != 0 {
		t.Errorf("an already-archived item produced %d tombstones; no entitlement ended", len(rows))
	}
}

// The status flip and its removal instruction must land together. A committed
// take-down with no instruction is F-27 itself; a committed instruction with no
// take-down would unload a capability that is still on sale.
//
// Failing the SECOND item is what makes this test worth having: the first
// item's flip and tombstone are already written when the failure arrives, so a
// green result means the transaction really did take both back — not that
// nothing was ever attempted.
func TestBatchSetStatus_FailureRollsBackTombstonesToo(t *testing.T) {
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "item-1", "One", "skill", "active", "clean", "author", 1)
	seedItem(t, db, "item-2", "Two", "skill", "active", "clean", "author", 1)
	adminSeedHolder(t, db, "item-1", "holder-a")
	adminSeedHolder(t, db, "item-2", "holder-b")

	if err := db.Exec(`CREATE TRIGGER refuse_item_2 BEFORE UPDATE ON capability_items
		WHEN NEW.id = 'item-2'
		BEGIN SELECT RAISE(ABORT, 'simulated mid-batch failure'); END`).Error; err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	_, _, err := NewService(db).BatchSetStatus([]string{"item-1", "item-2"}, StatusArchived)
	if err == nil {
		t.Fatal("a hard failure mid-batch must abort the whole batch")
	}

	for _, id := range []string{"item-1", "item-2"} {
		if got := adminItemStatus(t, db, id); got != "active" {
			t.Errorf("%s: status = %q, want the row rolled back to active", id, got)
		}
		if rows := adminLoadTombstones(t, db, id); len(rows) != 0 {
			t.Errorf("%s: a rolled-back batch left %d tombstones behind, instructing devices to unload a capability that is still on sale", id, len(rows))
		}
	}
}
