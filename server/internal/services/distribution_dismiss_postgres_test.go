package services

import (
	"context"
	"errors"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
)

// F-31 on a real PostgreSQL. The SQLite fixture cannot enforce
// chk_capability_sync_tombstones_cause (the legal-triple CHECK from migration
// 20260805000700) or the production column types, so a green SQLite run does
// not prove the dismiss-path tombstone is actually writable in production.
// This test reuses the snapshot harness, which applies the tombstone
// migrations verbatim from server/migrations into a throwaway schema.
//
// All three holder shapes run against the same schema:
//   - receipt-only (the F-31 regression: auto-favorite never succeeded),
//   - favorite-and-receipt (the convergence case: two same-tx writes must
//     upsert into ONE terminal row through the real unique index),
//   - favorite-only / no receipt (no transition: nothing may be written).
func TestDismissReceipt_PostgresTombstonePassesProductionSchema(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := NewDistributionService(db, NewBehaviorService(db), nil)
	ctx := context.Background()

	// Guard the guard: if the harness ever stops applying the cause CHECK,
	// this test would silently prove nothing.
	var causeChecks int64
	if err := db.Raw(`SELECT count(*) FROM pg_constraint c
		JOIN pg_namespace n ON n.oid = c.connamespace
		WHERE c.conname = 'chk_capability_sync_tombstones_cause' AND n.nspname = current_schema()`).
		Scan(&causeChecks).Error; err != nil {
		t.Fatalf("probe cause constraint: %v", err)
	}
	if causeChecks != 1 {
		t.Fatalf("chk_capability_sync_tombstones_cause not present in the test schema (found %d)", causeChecks)
	}

	seed := func(t *testing.T, userID string, withReceipt, withFavorite bool) (itemID, distID string) {
		t.Helper()
		itemID = uuid.NewString()
		distID = uuid.NewString()
		if err := db.Exec(`INSERT INTO capability_items (id, name, status, favorite_count) VALUES (?, 'Code Reviewer', 'active', ?)`,
			itemID, map[bool]int{true: 1, false: 0}[withFavorite]).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
		if err := db.Create(&models.ItemDistribution{ID: distID, ItemID: itemID, DistributorID: "admin-a", PermissionMode: "dismissible", Status: "active", ScopeType: "user", TargetID: userID}).Error; err != nil {
			t.Fatalf("seed dist: %v", err)
		}
		if withReceipt {
			if err := db.Create(&models.ItemDistributionReceipt{ID: uuid.NewString(), DistributionID: distID, UserID: userID, ReceiptStatus: "read"}).Error; err != nil {
				t.Fatalf("seed receipt: %v", err)
			}
		}
		if withFavorite {
			if err := db.Exec(`INSERT INTO item_favorites (item_id, user_id) VALUES (?, ?)`, itemID, userID).Error; err != nil {
				t.Fatalf("seed favorite: %v", err)
			}
		}
		return itemID, distID
	}

	loadOnly := func(t *testing.T, itemID, userID string) models.CapabilitySyncTombstone {
		t.Helper()
		var rows []models.CapabilitySyncTombstone
		if err := db.Where("item_id = ? AND user_id = ?", itemID, userID).Find(&rows).Error; err != nil {
			t.Fatalf("load tombstones: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("expected exactly one tombstone for %s/%s, got %d", userID, itemID, len(rows))
		}
		return rows[0]
	}

	t.Run("receipt-only", func(t *testing.T) {
		itemID, distID := seed(t, "pg-user-a", true, false)

		if err := svc.DismissReceipt(ctx, distID, "pg-user-a"); err != nil {
			t.Fatalf("dismiss: %v", err)
		}

		tombstone := loadOnly(t, itemID, "pg-user-a")
		if tombstone.Reason != models.SyncTombstoneReasonUnfavorited ||
			tombstone.Source != models.SyncTombstoneSourceFavorite ||
			tombstone.LifecycleReason != nil {
			t.Fatalf("tombstone triple = (%s, %s, %v), want (unfavorited, favorite, NULL)",
				tombstone.Reason, tombstone.Source, tombstone.LifecycleReason)
		}
		if tombstone.EventID == "" {
			t.Fatal("tombstone has no event id")
		}
	})

	t.Run("favorite-and-receipt", func(t *testing.T) {
		itemID, distID := seed(t, "pg-user-b", true, true)

		if err := svc.DismissReceipt(ctx, distID, "pg-user-b"); err != nil {
			t.Fatalf("dismiss: %v", err)
		}

		var favorites int64
		if err := db.Table("item_favorites").Where("item_id = ? AND user_id = ?", itemID, "pg-user-b").
			Count(&favorites).Error; err != nil {
			t.Fatalf("count favorites: %v", err)
		}
		if favorites != 0 {
			t.Fatalf("favorite survived the dismissal: %d", favorites)
		}
		// The favorite path and the receipt path both wrote in one
		// transaction; the production unique index must fold them into one
		// terminal row.
		tombstone := loadOnly(t, itemID, "pg-user-b")
		if tombstone.Reason != models.SyncTombstoneReasonUnfavorited {
			t.Fatalf("converged tombstone reason = %s, want unfavorited", tombstone.Reason)
		}
	})

	t.Run("favorite-only", func(t *testing.T) {
		itemID, distID := seed(t, "pg-user-c", false, true)

		if err := svc.DismissReceipt(ctx, distID, "pg-user-c"); err == nil {
			t.Fatal("dismissing without a receipt reported success")
		}

		var tombstones int64
		if err := db.Model(&models.CapabilitySyncTombstone{}).
			Where("item_id = ? AND user_id = ?", itemID, "pg-user-c").
			Count(&tombstones).Error; err != nil {
			t.Fatalf("count tombstones: %v", err)
		}
		if tombstones != 0 {
			t.Fatalf("a failed dismissal wrote %d tombstones", tombstones)
		}
	})

	// F-32 on the same schema, for completeness of the gate under production
	// column types: a readonly active distribution refuses, and writes nothing.
	t.Run("readonly-gate", func(t *testing.T) {
		itemID, distID := seed(t, "pg-user-d", true, true)
		if err := db.Exec(`UPDATE item_distributions SET permission_mode = 'readonly' WHERE id = ?`, distID).Error; err != nil {
			t.Fatalf("flip to readonly: %v", err)
		}

		err := svc.DismissReceipt(ctx, distID, "pg-user-d")
		if !errors.Is(err, ErrReceiptNotDismissible) {
			t.Fatalf("dismiss of readonly distribution: err = %v, want ErrReceiptNotDismissible", err)
		}

		var receiptStatus string
		if err := db.Table("item_distribution_receipts").Where("distribution_id = ?", distID).
			Pluck("receipt_status", &receiptStatus).Error; err != nil {
			t.Fatalf("reload receipt: %v", err)
		}
		if receiptStatus != "read" {
			t.Fatalf("refused dismissal moved the receipt to %q", receiptStatus)
		}
		var tombstones int64
		if err := db.Model(&models.CapabilitySyncTombstone{}).
			Where("item_id = ? AND user_id = ?", itemID, "pg-user-d").
			Count(&tombstones).Error; err != nil {
			t.Fatalf("count tombstones: %v", err)
		}
		if tombstones != 0 {
			t.Fatalf("refused dismissal wrote %d tombstones", tombstones)
		}
	})
}
