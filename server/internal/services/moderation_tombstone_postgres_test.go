package services

import (
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// Moderation and catalog tombstones, against the production schema.
//
// PostgreSQL rather than SQLite for the same reason the rest of this suite
// insists on it: the property under test is partly the CHECK constraint that
// pairs reason with source, and SQLite enforces none of it.

func seedModerationDistribution(t *testing.T, db *gorm.DB, itemID, userID string) {
	t.Helper()
	var distributionID string
	if err := db.Raw(`INSERT INTO item_distributions (item_id, status) VALUES (?, 'active') RETURNING id`, itemID).
		Row().Scan(&distributionID); err != nil {
		t.Fatalf("seed distribution: %v", err)
	}
	if err := db.Exec(`INSERT INTO item_distribution_receipts (distribution_id, user_id, receipt_status)
		VALUES (?, ?, 'unread')`, distributionID, userID).Error; err != nil {
		t.Fatalf("seed receipt: %v", err)
	}
}

func loadModerationTombstone(t *testing.T, db *gorm.DB, itemID, userID string) *models.CapabilitySyncTombstone {
	t.Helper()
	var rows []models.CapabilitySyncTombstone
	if err := db.Where("item_id = ? AND user_id = ?", itemID, userID).Find(&rows).Error; err != nil {
		t.Fatalf("load tombstone: %v", err)
	}
	if len(rows) == 0 {
		return nil
	}
	if len(rows) > 1 {
		t.Fatalf("expected at most one tombstone per (user,item), found %d", len(rows))
	}
	return &rows[0]
}

// TestModerationTombstone_AdminArchiveNamesItselfTruthfully pins the wire
// vocabulary. The reason csc receives is shown to the user, so borrowing
// `git_archived` or `unfavorited` for a moderation take-down would not be a
// harmless internal detail — it would tell the user something false about why
// their capability vanished.
func TestModerationTombstone_AdminArchiveNamesItselfTruthfully(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	item := snapshotItemID(1)
	seedSnapshotItem(t, db, item, "one", "active")
	seedSnapshotFavorite(t, db, item, snapshotUserA)

	if err := db.Transaction(func(tx *gorm.DB) error {
		_, err := RecordAdminArchiveTombstonesTx(tx, item, time.Now())
		return err
	}); err != nil {
		t.Fatalf("record admin archive tombstones: %v", err)
	}

	tombstone := loadModerationTombstone(t, db, item, snapshotUserA)
	if tombstone == nil {
		t.Fatal("a moderation take-down must record a tombstone for every holder")
	}
	if tombstone.Reason != models.SyncTombstoneReasonAdminArchived {
		t.Errorf("reason = %q, want admin_archived", tombstone.Reason)
	}
	if tombstone.Source != models.SyncTombstoneSourceModeration {
		t.Errorf("source = %q, want moderation", tombstone.Source)
	}
	if tombstone.LifecycleReason != nil {
		t.Errorf("lifecycleReason = %v, want NULL: no Git event happened", *tombstone.LifecycleReason)
	}
}

func TestModerationTombstone_DeleteNamesItselfTruthfully(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	item := snapshotItemID(2)
	seedSnapshotItem(t, db, item, "two", "active")
	seedSnapshotFavorite(t, db, item, snapshotUserA)

	if err := db.Transaction(func(tx *gorm.DB) error {
		_, err := RecordItemDeleteTombstonesTx(tx, item, time.Now())
		return err
	}); err != nil {
		t.Fatalf("record delete tombstones: %v", err)
	}

	tombstone := loadModerationTombstone(t, db, item, snapshotUserA)
	if tombstone == nil {
		t.Fatal("a hard delete must record a tombstone for every holder")
	}
	if tombstone.Reason != models.SyncTombstoneReasonItemDeleted {
		t.Errorf("reason = %q, want item_deleted", tombstone.Reason)
	}
	if tombstone.Source != models.SyncTombstoneSourceCatalog {
		t.Errorf("source = %q, want catalog", tombstone.Source)
	}
	if tombstone.LifecycleReason != nil {
		t.Errorf("lifecycleReason = %v, want NULL", *tombstone.LifecycleReason)
	}
}

// A principal who both favorited the item AND holds a live distribution receipt
// is one holder, not two. The table records the end of an entitlement, and the
// unique (user_id, item_id) constraint would reject the second row outright —
// so a writer that did not deduplicate would fail the whole take-down.
func TestModerationTombstone_DualSourceHolderGetsExactlyOneRow(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	item := snapshotItemID(3)
	seedSnapshotItem(t, db, item, "three", "active")
	seedSnapshotFavorite(t, db, item, snapshotUserA)
	seedModerationDistribution(t, db, item, snapshotUserA)

	var written int
	if err := db.Transaction(func(tx *gorm.DB) error {
		count, err := RecordAdminArchiveTombstonesTx(tx, item, time.Now())
		written = count
		return err
	}); err != nil {
		t.Fatalf("record admin archive tombstones: %v", err)
	}
	if written != 1 {
		t.Fatalf("holders written = %d, want 1: one principal holding two relationships is still one holder", written)
	}

	var rows int64
	db.Table("capability_sync_tombstones").Where("item_id = ?", item).Count(&rows)
	if rows != 1 {
		t.Fatalf("tombstone rows = %d, want 1", rows)
	}
}

// A receipt-only holder — distributed the item but never favorited it — is a
// holder. Deriving the set from favorites alone would leave exactly the users
// who never chose the capability themselves unable to get rid of it.
func TestModerationTombstone_ReceiptOnlyHolderIsTombstoned(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	item := snapshotItemID(4)
	seedSnapshotItem(t, db, item, "four", "active")
	seedModerationDistribution(t, db, item, snapshotUserB)

	if err := db.Transaction(func(tx *gorm.DB) error {
		_, err := RecordAdminArchiveTombstonesTx(tx, item, time.Now())
		return err
	}); err != nil {
		t.Fatalf("record admin archive tombstones: %v", err)
	}
	if loadModerationTombstone(t, db, item, snapshotUserB) == nil {
		t.Fatal("a distribution-only holder must be tombstoned too")
	}
}

// THE invariant of this slice.
//
// LifecyclePropagation is the GIT rollout kill switch. A moderation take-down
// is not a Git event and must not be suppressed by it — if it were, every
// deployment that has not yet enabled Git (which is the default: the flag is
// off) would ship exactly the F-27 bug the new reasons were introduced to fix,
// now wearing a different name. The test asserts both halves at once, on one
// principal, so the flag cannot be shown to work by suppressing everything.
func TestModerationTombstone_SurvivesTheGitLifecycleKillSwitch(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	gitItem := snapshotItemID(5)
	adminItem := snapshotItemID(6)
	deletedItem := snapshotItemID(7)
	seedSnapshotItem(t, db, gitItem, "git", "active")
	seedSnapshotItem(t, db, adminItem, "admin", "active")
	seedSnapshotItem(t, db, deletedItem, "deleted", "active")
	seedSnapshotFavorite(t, db, gitItem, snapshotUserA)
	seedSnapshotFavorite(t, db, adminItem, snapshotUserA)
	seedSnapshotFavorite(t, db, deletedItem, snapshotUserA)

	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`UPDATE capability_items SET status='archived' WHERE id IN ?`,
			[]string{gitItem, adminItem}).Error; err != nil {
			return err
		}
		if _, err := RecordGitArchiveTombstonesTx(tx, gitItem, models.GitLifecycleReasonManifestRemoved, time.Now()); err != nil {
			return err
		}
		if _, err := RecordAdminArchiveTombstonesTx(tx, adminItem, time.Now()); err != nil {
			return err
		}
		// The delete path records holders before the cascade removes them, so
		// the tombstone outlives both the favorite and the item row.
		if _, err := RecordItemDeleteTombstonesTx(tx, deletedItem, time.Now()); err != nil {
			return err
		}
		if err := tx.Exec(`DELETE FROM item_favorites WHERE item_id = ?`, deletedItem).Error; err != nil {
			return err
		}
		return tx.Exec(`DELETE FROM capability_items WHERE id = ?`, deletedItem).Error
	}); err != nil {
		t.Fatalf("seed removals: %v", err)
	}

	off := collectSnapshot(t, newSnapshotService(db, false), snapshotUserA)
	if err := verifyCollected(t, off); err != nil {
		t.Fatalf("verify with propagation off: %v", err)
	}
	delivered := tombstoneItemIDs(t, off)

	if _, suppressed := delivered[gitItem]; suppressed {
		t.Error("the Git kill switch must still suppress git_archived")
	}
	admin, ok := delivered[adminItem]
	if !ok {
		t.Fatal("a moderation take-down was suppressed by the GIT rollout flag: with the flag off (the default) every admin archive would again leave the capability installed forever — F-27 under a new name")
	}
	if admin["reason"] != "admin_archived" || admin["source"] != "moderation" {
		t.Errorf("moderation tombstone on the wire = reason %v / source %v", admin["reason"], admin["source"])
	}
	if admin["lifecycleReason"] != nil {
		t.Errorf("lifecycleReason on the wire = %v, want null", admin["lifecycleReason"])
	}
	deleted, ok := delivered[deletedItem]
	if !ok {
		t.Fatal("a hard delete was suppressed by the GIT rollout flag")
	}
	if deleted["reason"] != "item_deleted" || deleted["source"] != "catalog" {
		t.Errorf("catalog tombstone on the wire = reason %v / source %v", deleted["reason"], deleted["source"])
	}

	// With the flag on, all three are delivered — so the assertion above is
	// about the flag's scope, not about the moderation rows being the only ones
	// that ever arrive.
	on := collectSnapshot(t, newSnapshotService(db, true), snapshotUserA)
	if err := verifyCollected(t, on); err != nil {
		t.Fatalf("verify with propagation on: %v", err)
	}
	if len(on.Tombstones) != 3 {
		t.Fatalf("with propagation on the snapshot carried %d tombstones, want 3", len(on.Tombstones))
	}
}

// Restoring an item does not delete its tombstone; the active row supersedes it
// when the snapshot is built. That is what lets an offline device that never
// saw the take-down still hold the record, and it is why the writers never
// delete tombstones on restore.
func TestModerationTombstone_RestoreSupersedesWithoutDeleting(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	item := snapshotItemID(8)
	seedSnapshotItem(t, db, item, "eight", "active")
	seedSnapshotFavorite(t, db, item, snapshotUserA)

	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`UPDATE capability_items SET status='archived' WHERE id = ?`, item).Error; err != nil {
			return err
		}
		_, err := RecordAdminArchiveTombstonesTx(tx, item, time.Now())
		return err
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	archived := collectSnapshot(t, newSnapshotService(db, false), snapshotUserA)
	if len(archived.Tombstones) != 1 {
		t.Fatalf("archived snapshot carried %d tombstones, want 1", len(archived.Tombstones))
	}

	if err := db.Exec(`UPDATE capability_items SET status='active' WHERE id = ?`, item).Error; err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored := collectSnapshot(t, newSnapshotService(db, false), snapshotUserA)
	if err := verifyCollected(t, restored); err != nil {
		t.Fatalf("verify restored: %v", err)
	}
	if len(restored.Tombstones) != 0 {
		t.Fatalf("restored snapshot still carried %d tombstones", len(restored.Tombstones))
	}
	if len(restored.Items) != 1 {
		t.Fatalf("restored snapshot carried %d active items, want 1", len(restored.Items))
	}

	var stored int64
	db.Table("capability_sync_tombstones").Where("item_id = ?", item).Count(&stored)
	if stored != 1 {
		t.Fatalf("restore deleted the tombstone (%d rows remain); a device that has not polled since the take-down would lose the record", stored)
	}
}

// The rotation rule, on the moderation path. Re-archiving without an
// intervening restore is not a new removal, and the caller — not this function
// — is what decides that, so the guarantee has to be tested where the decision
// is made (see the adminitem suite). Here we pin the other half: a genuine
// second removal DOES mint a new id, because a device that already applied the
// first would otherwise dedup the second away and keep the capability forever.
func TestModerationTombstone_SecondRemovalRotatesTheEventID(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	item := snapshotItemID(9)
	seedSnapshotItem(t, db, item, "nine", "active")
	seedSnapshotFavorite(t, db, item, snapshotUserA)

	archive := func() {
		t.Helper()
		if err := db.Transaction(func(tx *gorm.DB) error {
			_, err := RecordAdminArchiveTombstonesTx(tx, item, time.Now())
			return err
		}); err != nil {
			t.Fatalf("archive: %v", err)
		}
	}

	archive()
	first := loadModerationTombstone(t, db, item, snapshotUserA)
	archive()
	second := loadModerationTombstone(t, db, item, snapshotUserA)

	if first == nil || second == nil {
		t.Fatal("both removals must leave a tombstone")
	}
	if first.EventID == second.EventID {
		t.Fatal("a second removal transition must rotate the event id, or a client that applied the first dedups the second away and keeps the capability installed")
	}
}
