package handlers

import (
	"net/http"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/datatypes"
)

// Review finding F-27 on the second take-down path. PUT /items/:id is the other
// way an item leaves the shelf — adminitem.SetStatus is not the only one — and
// it wrote no removal instruction either. Fixing only the admin service would
// have left the identical hole one endpoint to the side.

func seedModerationTombstoneItem(t *testing.T, itemID, status string) {
	t.Helper()
	if err := database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-" + itemID, Name: "reg-" + itemID, SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	}).Error; err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	if err := database.DB.Create(&models.CapabilityItem{
		ID: itemID, RegistryID: "reg-" + itemID, RepoID: "repo-1", Slug: "slug-" + itemID,
		ItemType: "skill", Name: "Item", Status: status, CreatedBy: "u1",
		Metadata: datatypes.JSON([]byte("{}")), CurrentRevision: 1,
	}).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
}

func seedModerationTombstoneFavorite(t *testing.T, itemID, userID string) {
	t.Helper()
	if err := database.DB.Exec(`INSERT INTO item_favorites (id, item_id, user_id) VALUES (?, ?, ?)`,
		userID+":"+itemID, itemID, userID).Error; err != nil {
		t.Fatalf("seed favorite: %v", err)
	}
}

func loadPutTombstones(t *testing.T, itemID string) []models.CapabilitySyncTombstone {
	t.Helper()
	var rows []models.CapabilitySyncTombstone
	if err := database.DB.Where("item_id = ?", itemID).Order("user_id").Find(&rows).Error; err != nil {
		t.Fatalf("load tombstones: %v", err)
	}
	return rows
}

func TestUpdateItem_ArchiveTombstonesHolders(t *testing.T) {
	defer setupTestDB(t)()
	seedModerationTombstoneItem(t, "item-arch", "active")
	seedModerationTombstoneFavorite(t, "item-arch", "holder")

	w := putJSON(newItemRouter("u1"), "/api/items/item-arch", map[string]interface{}{
		"status": "archived",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	rows := loadPutTombstones(t, "item-arch")
	if len(rows) != 1 {
		t.Fatalf("tombstones = %d, want 1: the holder's device otherwise keeps the capability forever", len(rows))
	}
	if rows[0].Reason != models.SyncTombstoneReasonAdminArchived {
		t.Errorf("reason = %q, want admin_archived", rows[0].Reason)
	}
	if rows[0].Source != models.SyncTombstoneSourceModeration {
		t.Errorf("source = %q, want moderation", rows[0].Source)
	}
	if rows[0].LifecycleReason != nil {
		t.Errorf("lifecycleReason = %q, want NULL: no Git event happened", *rows[0].LifecycleReason)
	}
}

// The archive rides along with the versioned save path (content/metadata
// changed), which commits through a different branch of the handler. Both
// branches have to carry the instruction, or whether a device is ever told
// depends on whether the moderator happened to rename the item at the same
// time.
func TestUpdateItem_ArchiveWithContentChangeStillTombstones(t *testing.T) {
	defer setupTestDB(t)()
	seedModerationTombstoneItem(t, "item-both", "active")
	seedModerationTombstoneFavorite(t, "item-both", "holder")

	w := putJSON(newItemRouter("u1"), "/api/items/item-both", map[string]interface{}{
		"status": "archived", "name": "Renamed While Archiving",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if rows := loadPutTombstones(t, "item-both"); len(rows) != 1 {
		t.Fatalf("tombstones = %d, want 1 on the versioned save path too", len(rows))
	}
}

// `banned` is off the shelf exactly as `archived` is — the snapshot's active
// set is `status = 'active'` — so it ends entitlements the same way. Keying the
// tombstone on the literal "archived" would leave this hole open.
func TestUpdateItem_BanTombstonesHolders(t *testing.T) {
	defer setupTestDB(t)()
	seedModerationTombstoneItem(t, "item-ban", "active")
	seedModerationTombstoneFavorite(t, "item-ban", "holder")

	w := putJSON(newItemRouter("u1"), "/api/items/item-ban", map[string]interface{}{
		"status": "banned",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if rows := loadPutTombstones(t, "item-ban"); len(rows) != 1 {
		t.Fatalf("tombstones = %d, want 1: banning takes the item off the shelf just as archiving does", len(rows))
	}
}

// No transition, no instruction — and above all no event-id rotation, which
// would make every later poll look like a fresh removal.
func TestUpdateItem_RepeatedArchiveDoesNotRotateTheEventID(t *testing.T) {
	defer setupTestDB(t)()
	seedModerationTombstoneItem(t, "item-again", "active")
	seedModerationTombstoneFavorite(t, "item-again", "holder")

	router := newItemRouter("u1")
	if w := putJSON(router, "/api/items/item-again", map[string]interface{}{"status": "archived"}); w.Code != http.StatusOK {
		t.Fatalf("first archive: %d %s", w.Code, w.Body.String())
	}
	first := loadPutTombstones(t, "item-again")
	if len(first) != 1 {
		t.Fatalf("tombstones after first archive = %d, want 1", len(first))
	}

	if w := putJSON(router, "/api/items/item-again", map[string]interface{}{"status": "archived"}); w.Code != http.StatusOK {
		t.Fatalf("second archive: %d %s", w.Code, w.Body.String())
	}
	second := loadPutTombstones(t, "item-again")
	if len(second) != 1 {
		t.Fatalf("tombstones after second archive = %d, want 1", len(second))
	}
	if second[0].EventID != first[0].EventID {
		t.Fatal("re-archiving an already-archived item rotated the event id; nothing was removed, so every poll would replay the removal")
	}
}

// An edit that does not touch status is not a removal.
func TestUpdateItem_PlainEditWritesNoTombstone(t *testing.T) {
	defer setupTestDB(t)()
	seedModerationTombstoneItem(t, "item-edit", "active")
	seedModerationTombstoneFavorite(t, "item-edit", "holder")

	w := putJSON(newItemRouter("u1"), "/api/items/item-edit", map[string]interface{}{
		"name": "Just A Rename",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if rows := loadPutTombstones(t, "item-edit"); len(rows) != 0 {
		t.Fatalf("a rename produced %d tombstones", len(rows))
	}
}

// Restoring writes no tombstone and deletes none: the active row supersedes the
// old tombstone when the snapshot is built, so the record survives for a device
// that has been offline since the take-down.
func TestUpdateItem_RestoreLeavesTheTombstoneInPlace(t *testing.T) {
	defer setupTestDB(t)()
	seedModerationTombstoneItem(t, "item-restore", "active")
	seedModerationTombstoneFavorite(t, "item-restore", "holder")

	router := newItemRouter("u1")
	if w := putJSON(router, "/api/items/item-restore", map[string]interface{}{"status": "archived"}); w.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", w.Code, w.Body.String())
	}
	archived := loadPutTombstones(t, "item-restore")
	if len(archived) != 1 {
		t.Fatalf("tombstones after archive = %d, want 1", len(archived))
	}

	if w := putJSON(router, "/api/items/item-restore", map[string]interface{}{"status": "active"}); w.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	restored := loadPutTombstones(t, "item-restore")
	if len(restored) != 1 || restored[0].EventID != archived[0].EventID {
		t.Fatalf("restore must leave the tombstone untouched, got %+v", restored)
	}
}
