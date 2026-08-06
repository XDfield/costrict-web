package itemdelete

import (
	"testing"

	"gorm.io/gorm"
)

// Review finding F-27, catalog half: a hard delete ended every holder's
// entitlement and told nobody.
//
// The delete path has a failure mode the archive path does not: it destroys the
// evidence. The holder set lives in item_favorites and the distribution
// receipts, and the cascade deletes both — so a tombstone written even one step
// too late tombstones NOBODY, returns success, and leaves the capability
// installed on every device with nothing anywhere to notice. These tests exist
// mainly to hold that ordering in place.

type deletionTombstoneRow struct {
	UserID  string `gorm:"column:user_id"`
	ItemID  string `gorm:"column:item_id"`
	Reason  string `gorm:"column:reason"`
	Source  string `gorm:"column:source"`
	EventID string `gorm:"column:event_id"`
}

func loadDeletionTombstones(t *testing.T, db *gorm.DB, itemID string) []deletionTombstoneRow {
	t.Helper()
	var rows []deletionTombstoneRow
	if err := db.Raw(`SELECT user_id, item_id, reason, source, event_id
		FROM capability_sync_tombstones WHERE item_id = ? ORDER BY user_id`, itemID).
		Scan(&rows).Error; err != nil {
		t.Fatalf("load tombstones: %v", err)
	}
	return rows
}

func TestCascadeDelete_TombstonesEveryHolderBeforeDestroyingTheEvidence(t *testing.T) {
	db := setupFullSchema(t)

	mustExec(t, db, `INSERT INTO capability_items (id, item_type, name, status, created_by) VALUES ('I','skill','Item','active','author')`)
	mustExec(t, db, `INSERT INTO item_favorites (id, item_id, user_id) VALUES ('f1','I','holder-fav')`)
	mustExec(t, db, `INSERT INTO item_distributions (id, item_id, status) VALUES ('d1','I','active')`)
	mustExec(t, db, `INSERT INTO item_distribution_receipts (id, distribution_id, user_id, receipt_status) VALUES ('r1','d1','holder-dist','unread')`)

	if err := db.Transaction(func(tx *gorm.DB) error {
		return CascadeDelete(tx, "I")
	}); err != nil {
		t.Fatalf("cascade delete: %v", err)
	}

	rows := loadDeletionTombstones(t, db, "I")
	if len(rows) != 2 {
		t.Fatalf("tombstones = %d, want one per holder; the favorites and receipts are gone, so a later write could never have found them", len(rows))
	}
	seen := map[string]bool{}
	for _, row := range rows {
		seen[row.UserID] = true
		if row.Reason != "item_deleted" {
			t.Errorf("user %s: reason = %q, want item_deleted", row.UserID, row.Reason)
		}
		if row.Source != "catalog" {
			t.Errorf("user %s: source = %q, want catalog", row.UserID, row.Source)
		}
		if row.EventID == "" {
			t.Errorf("user %s: empty event id; csc has nothing to dedup on", row.UserID)
		}
	}
	if !seen["holder-fav"] || !seen["holder-dist"] {
		t.Fatalf("holders tombstoned = %v, want both the favorite and the distribution recipient", seen)
	}

	// The row itself is gone, and the instruction outlived it. That is why the
	// table carries no foreign key to capability_items.
	if count(t, db, `SELECT COUNT(*) FROM capability_items WHERE id='I'`) != 0 {
		t.Fatal("the item row should have been deleted")
	}
}

// A dismissed receipt is not a live entitlement — that device already removed
// the capability — so it must not be tombstoned again.
func TestCascadeDelete_DismissedReceiptIsNotAHolder(t *testing.T) {
	db := setupFullSchema(t)

	mustExec(t, db, `INSERT INTO capability_items (id, item_type, name, status, created_by) VALUES ('I','skill','Item','active','author')`)
	mustExec(t, db, `INSERT INTO item_distributions (id, item_id, status) VALUES ('d1','I','active')`)
	mustExec(t, db, `INSERT INTO item_distribution_receipts (id, distribution_id, user_id, receipt_status) VALUES ('r1','d1','dismissed-user','dismissed')`)
	mustExec(t, db, `INSERT INTO item_distributions (id, item_id, status) VALUES ('d2','I','revoked')`)
	mustExec(t, db, `INSERT INTO item_distribution_receipts (id, distribution_id, user_id, receipt_status) VALUES ('r2','d2','revoked-user','unread')`)

	if err := db.Transaction(func(tx *gorm.DB) error {
		return CascadeDelete(tx, "I")
	}); err != nil {
		t.Fatalf("cascade delete: %v", err)
	}
	if rows := loadDeletionTombstones(t, db, "I"); len(rows) != 0 {
		t.Fatalf("tombstones = %d, want 0: neither a dismissed receipt nor a revoked distribution is a live entitlement", len(rows))
	}
}

// A principal holding both relationships is one holder. The UNIQUE
// (user_id, item_id) constraint turns a non-deduplicating writer into a failed
// delete rather than a silent duplicate, so this is load-bearing.
func TestCascadeDelete_DualSourceHolderGetsOneTombstone(t *testing.T) {
	db := setupFullSchema(t)

	mustExec(t, db, `INSERT INTO capability_items (id, item_type, name, status, created_by) VALUES ('I','skill','Item','active','author')`)
	mustExec(t, db, `INSERT INTO item_favorites (id, item_id, user_id) VALUES ('f1','I','holder')`)
	mustExec(t, db, `INSERT INTO item_distributions (id, item_id, status) VALUES ('d1','I','active')`)
	mustExec(t, db, `INSERT INTO item_distribution_receipts (id, distribution_id, user_id, receipt_status) VALUES ('r1','d1','holder','unread')`)

	if err := db.Transaction(func(tx *gorm.DB) error {
		return CascadeDelete(tx, "I")
	}); err != nil {
		t.Fatalf("cascade delete: %v", err)
	}
	if rows := loadDeletionTombstones(t, db, "I"); len(rows) != 1 {
		t.Fatalf("tombstones = %d, want 1 for one principal holding two relationships", len(rows))
	}
}

// Deleting a plugin hard-deletes its bundled sub-skills, and a user may have
// favorited a sub-skill directly. Each id in the recursion tombstones its own
// holders, so a sub-skill's holder is not silently dropped along the way.
func TestCascadeDelete_SubSkillHoldersAreTombstonedToo(t *testing.T) {
	db := setupFullSchema(t)

	mustExec(t, db, `INSERT INTO capability_items (id, item_type, name, status, created_by) VALUES ('P','plugin','Plug','active','author')`)
	mustExec(t, db, `INSERT INTO capability_items (id, item_type, name, status, created_by, parent_plugin_id) VALUES ('S','skill','Sub','active','author','P')`)
	mustExec(t, db, `INSERT INTO item_favorites (id, item_id, user_id) VALUES ('f1','P','plugin-holder')`)
	mustExec(t, db, `INSERT INTO item_favorites (id, item_id, user_id) VALUES ('f2','S','subskill-holder')`)

	if err := db.Transaction(func(tx *gorm.DB) error {
		return CascadeDelete(tx, "P")
	}); err != nil {
		t.Fatalf("cascade delete: %v", err)
	}

	if rows := loadDeletionTombstones(t, db, "P"); len(rows) != 1 || rows[0].UserID != "plugin-holder" {
		t.Fatalf("plugin tombstones = %+v, want one for plugin-holder", rows)
	}
	if rows := loadDeletionTombstones(t, db, "S"); len(rows) != 1 || rows[0].UserID != "subskill-holder" {
		t.Fatalf("sub-skill tombstones = %+v, want one for subskill-holder: a user who favorited the sub-skill directly keeps it installed forever otherwise", rows)
	}
}

// The batch path shares the same cascade, so it must produce the same
// instructions — and a batch that fails must leave none of them behind.
func TestCascadeDeleteMany_TombstonesEveryItemsHolders(t *testing.T) {
	db := setupFullSchema(t)

	mustExec(t, db, `INSERT INTO capability_items (id, item_type, name, status, created_by) VALUES ('A','skill','A','active','author')`)
	mustExec(t, db, `INSERT INTO capability_items (id, item_type, name, status, created_by) VALUES ('B','skill','B','active','author')`)
	mustExec(t, db, `INSERT INTO item_favorites (id, item_id, user_id) VALUES ('f1','A','holder-a')`)
	mustExec(t, db, `INSERT INTO item_favorites (id, item_id, user_id) VALUES ('f2','B','holder-b')`)

	var deleted, skipped []string
	if err := db.Transaction(func(tx *gorm.DB) error {
		var e error
		deleted, skipped, e = CascadeDeleteMany(tx, []string{"A", "B", "missing"})
		return e
	}); err != nil {
		t.Fatalf("cascade delete many: %v", err)
	}
	if len(deleted) != 2 || len(skipped) != 1 {
		t.Fatalf("deleted = %v, skipped = %v", deleted, skipped)
	}
	for id, holder := range map[string]string{"A": "holder-a", "B": "holder-b"} {
		rows := loadDeletionTombstones(t, db, id)
		if len(rows) != 1 || rows[0].UserID != holder {
			t.Errorf("%s: tombstones = %+v, want one for %s", id, rows, holder)
		}
	}
	// An id that never existed produces no instruction: nothing was removed.
	if rows := loadDeletionTombstones(t, db, "missing"); len(rows) != 0 {
		t.Errorf("a skipped id produced %d tombstones", len(rows))
	}
}
