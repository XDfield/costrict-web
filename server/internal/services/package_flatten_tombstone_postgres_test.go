package services

import (
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// The `package_flattened` removal cause, against the production schema.
//
// PostgreSQL rather than SQLite for the same reason as the rest of this suite:
// half of what is asserted below IS the CHECK constraint that pairs reason with
// source, and SQLite enforces none of it.

// TestPackageFlattenTombstone_NamesItselfTruthfully pins the wire vocabulary.
//
// This cause exists because `migrate flatten-plugins` used to borrow
// `admin_archived`/`moderation` — the only existing triple whose every OTHER
// claim was true. The one that was not: no moderator looked at anything. csc
// logs the reason verbatim and derives the user's wording from it, so borrowing
// it points a user asking why their capability vanished at a content decision
// nobody made.
func TestPackageFlattenTombstone_NamesItselfTruthfully(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	item := snapshotItemID(21)
	seedSnapshotItem(t, db, item, "flattened", "active")
	seedSnapshotFavorite(t, db, item, snapshotUserA)

	if err := db.Transaction(func(tx *gorm.DB) error {
		_, err := RecordPackageFlattenTombstonesTx(tx, item, time.Now())
		return err
	}); err != nil {
		t.Fatalf("record package flatten tombstones: %v", err)
	}

	tombstone := loadModerationTombstone(t, db, item, snapshotUserA)
	if tombstone == nil {
		t.Fatal("archiving a package child must record a tombstone for every holder")
	}
	if tombstone.Reason != models.SyncTombstoneReasonPackageFlattened {
		t.Errorf("reason = %q, want package_flattened", tombstone.Reason)
	}
	if tombstone.Source != models.SyncTombstoneSourceDataMigration {
		t.Errorf("source = %q, want data_migration", tombstone.Source)
	}
	if tombstone.LifecycleReason != nil {
		t.Errorf("lifecycleReason = %v, want NULL: no Git event happened", *tombstone.LifecycleReason)
	}
}

// THE invariant of this cause.
//
// LifecyclePropagation is the GIT rollout kill switch, and it is OFF by default.
// The flatten migration is an operational data migration, not a Git event, so a
// switch that suppressed it would make every archive that command performs
// invisible on every deployment that has not enabled Git — which is F-27 again,
// wearing the newest name yet. That is not hypothetical: `git_archived` was
// rejected as this cause's name partly BECAUSE it is the one reason the switch
// suppresses, so the property has to be pinned rather than assumed.
//
// Both halves are asserted on one principal in one snapshot, so the switch
// cannot be shown to work by suppressing everything.
func TestPackageFlattenTombstone_SurvivesTheGitLifecycleKillSwitch(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	gitItem := snapshotItemID(22)
	flattenedItem := snapshotItemID(23)
	seedSnapshotItem(t, db, gitItem, "git", "active")
	seedSnapshotItem(t, db, flattenedItem, "flattened", "active")
	seedSnapshotFavorite(t, db, gitItem, snapshotUserA)
	seedSnapshotFavorite(t, db, flattenedItem, snapshotUserA)

	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`UPDATE capability_items SET status='archived' WHERE id IN ?`,
			[]string{gitItem, flattenedItem}).Error; err != nil {
			return err
		}
		if _, err := RecordGitArchiveTombstonesTx(tx, gitItem, models.GitLifecycleReasonManifestRemoved, time.Now()); err != nil {
			return err
		}
		_, err := RecordPackageFlattenTombstonesTx(tx, flattenedItem, time.Now())
		return err
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
	flattened, ok := delivered[flattenedItem]
	if !ok {
		t.Fatal("the flatten migration's removal was suppressed by the GIT rollout flag: with the flag off (the default) every row `migrate flatten-plugins` archives would stay installed on every holder's machine forever")
	}
	if flattened["reason"] != models.SyncTombstoneReasonPackageFlattened ||
		flattened["source"] != models.SyncTombstoneSourceDataMigration {
		t.Errorf("flatten tombstone on the wire = reason %v / source %v",
			flattened["reason"], flattened["source"])
	}
	if flattened["lifecycleReason"] != nil {
		t.Errorf("lifecycleReason on the wire = %v, want null", flattened["lifecycleReason"])
	}

	// With the flag on, both arrive — so the assertion above is about the flag's
	// scope, not about the flatten row being the only one that ever arrives.
	on := collectSnapshot(t, newSnapshotService(db, true), snapshotUserA)
	if err := verifyCollected(t, on); err != nil {
		t.Fatalf("verify with propagation on: %v", err)
	}
	if len(on.Tombstones) != 2 {
		t.Fatalf("with propagation on the snapshot carried %d tombstones, want 2", len(on.Tombstones))
	}
}

// The cause is legal only as the exact triple. A caller that supplies a
// lifecycle reason is describing a Git event that did not happen, and the
// service rejects it before the CHECK has to.
func TestPackageFlattenTombstone_RejectsALifecycleReason(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	item := snapshotItemID(24)
	seedSnapshotItem(t, db, item, "flattened", "active")

	err := db.Transaction(func(tx *gorm.DB) error {
		return RecordEntitlementRemovalTx(tx, EntitlementRemoval{
			UserID:          snapshotUserA,
			ItemID:          item,
			Reason:          models.SyncTombstoneReasonPackageFlattened,
			LifecycleReason: models.GitLifecycleReasonManifestRemoved,
		})
	})
	if err == nil {
		t.Fatal("package_flattened accepted a Git lifecycle reason")
	}
	var rows int64
	db.Table("capability_sync_tombstones").Where("item_id = ?", item).Count(&rows)
	if rows != 0 {
		t.Fatalf("a rejected cause still wrote %d row(s)", rows)
	}
}

// Rollback restores the item, and an active item supersedes its own older
// tombstone at materialization time. `migrate flatten-plugins rollback-apply` is
// the supported undo, so this is the path an operator actually takes — and it
// must not require deleting the tombstone, which would erase the instruction a
// device that has not polled since the migration still needs.
func TestPackageFlattenTombstone_RestoreSupersedesWithoutDeleting(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	item := snapshotItemID(25)
	seedSnapshotItem(t, db, item, "flattened", "active")
	seedSnapshotFavorite(t, db, item, snapshotUserA)

	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`UPDATE capability_items SET status='archived' WHERE id = ?`, item).Error; err != nil {
			return err
		}
		_, err := RecordPackageFlattenTombstonesTx(tx, item, time.Now())
		return err
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if archived := collectSnapshot(t, newSnapshotService(db, false), snapshotUserA); len(archived.Tombstones) != 1 {
		t.Fatalf("archived snapshot carried %d tombstones, want 1", len(archived.Tombstones))
	}

	if err := db.Exec(`UPDATE capability_items SET status='active' WHERE id = ?`, item).Error; err != nil {
		t.Fatalf("rollback-apply equivalent: %v", err)
	}
	restored := collectSnapshot(t, newSnapshotService(db, false), snapshotUserA)
	if err := verifyCollected(t, restored); err != nil {
		t.Fatalf("verify restored: %v", err)
	}
	if len(restored.Tombstones) != 0 || len(restored.Items) != 1 {
		t.Fatalf("restored snapshot carried %d tombstones and %d items, want 0 and 1",
			len(restored.Tombstones), len(restored.Items))
	}

	var stored int64
	db.Table("capability_sync_tombstones").Where("item_id = ?", item).Count(&stored)
	if stored != 1 {
		t.Fatalf("restore deleted the tombstone (%d rows remain); a device that has not polled since the migration would lose the record", stored)
	}
}
