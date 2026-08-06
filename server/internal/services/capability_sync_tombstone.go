// Package services — durable per-user removal records for the csc snapshot v2
// contract.
//
// This file owns ONE rule, and it is the rule the database cannot express.
package services

import (
	"errors"
	"fmt"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// EntitlementRemoval describes one transition of a user's entitlement from
// present to absent.
type EntitlementRemoval struct {
	UserID string
	ItemID string
	// Reason and Source must be one of the contract's legal pairs; the database
	// enforces the triple (chk_capability_sync_tombstones_cause) and this
	// package checks it first so the failure names the field rather than the
	// constraint.
	Reason string
	// LifecycleReason is required for, and only for, Reason == git_archived.
	LifecycleReason string
	// RemovedAt is the server time of the transition. Zero means "now".
	RemovedAt time.Time
}

// ErrInvalidTombstoneCause reports a (reason, source, lifecycleReason) triple
// the contract does not define.
var ErrInvalidTombstoneCause = errors.New("invalid capability sync tombstone cause")

// RecordEntitlementRemovalTx writes or ROTATES the tombstone for one user/item
// inside the caller's transaction.
//
// # The calling contract, which is the whole point of this function
//
// Call it exactly at the instant an entitlement transitioned from present to
// absent, and never otherwise. Every call rotates EventID.
//
// # Why rotation cannot be decided here, and why the failure is silent forever
//
// EventID is the client's dedup key: csc remembers the ids it has applied and
// treats a repeat as a no-op. Consider unfavorite(e1) -> refavorite -> unfavorite.
// The second removal UPDATEs the same (user, item) row. If it kept e1, csc would
// recognize an id it already processed, skip it, and the capability would stay
// installed on the device FOREVER — with no error anywhere, because from the
// server's side the tombstone exists and from the client's side the instruction
// was "already handled".
//
// The reverse mistake is just as bad: regenerating the id on a snapshot rebuild
// that changed nothing would make every poll look like a fresh removal and
// re-run removal work indefinitely.
//
// No row predicate distinguishes the two — the difference is not in the
// tombstone, it is in whether an entitlement existed in between. Only the
// caller performing the removal knows that, which is why this function trusts
// the call itself as the signal and why every caller must gate on its own
// compare-and-set:
//
//   - unfavoriteItemTx calls this only when the DELETE affected a row.
//   - the Git lifecycle writer (Phase C) calls RecordGitArchiveTombstonesTx
//     only inside the transaction that actually flipped the item to archived.
//   - the moderation paths (adminitem.SetStatus / BatchSetStatus and
//     PUT /items/:id) call RecordAdminArchiveTombstonesTx only when their own
//     `WHERE status = 'active'` predicate claimed the transition.
//   - the cascade delete calls RecordItemDeleteTombstonesTx once per item, and
//     before it removes the favorites and receipts the holder set is read from.
//   - `migrate flatten-plugins` calls RecordPackageFlattenTombstonesTx only when
//     its own compare-and-set UPDATE reported one affected row.
//
// Over-rotation is chosen where the two are genuinely ambiguous (a user
// unfavoriting an item that a Git archive already tombstoned): csc re-applying
// a removal it already applied is idempotent, whereas failing to rotate leaves
// the capability installed permanently. The asymmetry decides it.
func RecordEntitlementRemovalTx(tx *gorm.DB, removal EntitlementRemoval) error {
	source, err := tombstoneSourceFor(removal.Reason, removal.LifecycleReason)
	if err != nil {
		return err
	}
	if removal.UserID == "" || removal.ItemID == "" {
		return fmt.Errorf("%w: user and item are required", ErrInvalidTombstoneCause)
	}

	removedAt := removal.RemovedAt
	if removedAt.IsZero() {
		removedAt = time.Now()
	}
	var lifecycleReason *string
	if removal.LifecycleReason != "" {
		value := removal.LifecycleReason
		lifecycleReason = &value
	}

	tombstone := models.CapabilitySyncTombstone{
		ID:              uuid.NewString(),
		UserID:          removal.UserID,
		ItemID:          removal.ItemID,
		Reason:          removal.Reason,
		LifecycleReason: lifecycleReason,
		Source:          source,
		// A fresh id on every call. See the doc comment: this is the rotation.
		EventID:   uuid.NewString(),
		RemovedAt: removedAt,
	}

	// Upsert on (user_id, item_id): the table holds one terminal record per
	// user/item, so a re-removal replaces the cause, the time and the id rather
	// than accumulating rows a materializer would then have to rank.
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "item_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"reason", "lifecycle_reason", "source", "event_id", "removed_at", "updated_at",
		}),
	}).Create(&tombstone).Error
}

// RecordGitArchiveTombstonesTx is the Git lifecycle entry point, reserved for
// the Phase C lifecycle writer.
//
// It is deliberately narrow: it tombstones every principal who currently holds
// an entitlement to the item, and it must be called only from inside the
// transaction that actually archived the item — the same transaction whose
// compare-and-set proved the item was NOT already archived. Calling it on an
// item that was already archived would rotate event ids for a transition that
// did not happen, which is exactly the over-rotation the rotation rule exists
// to bound.
//
// Favorites and distribution receipts are deliberately NOT deleted here. The
// contract preserves the relationship (so a restore reactivates the same item
// for the same people) and expresses the removal through the tombstone.
func RecordGitArchiveTombstonesTx(tx *gorm.DB, itemID, lifecycleReason string, archivedAt time.Time) (int, error) {
	return recordHolderTombstonesTx(tx, itemID, models.SyncTombstoneReasonGitArchived, lifecycleReason, archivedAt)
}

// RecordAdminArchiveTombstonesTx is the moderation entry point: an operator
// moved the item off the shelf through adminitem.SetStatus / BatchSetStatus or
// PUT /items/:id.
//
// Same calling contract as the Git writer, for the same reason: call it ONLY
// from inside the transaction whose compare-and-set proved the row went
// active -> archived. Archiving an already-archived row rotates event ids for a
// transition that did not happen, and a client that has already applied the
// removal would re-run removal work on every poll.
//
// Favorites and distribution receipts are deliberately left in place. The
// relationship survives the take-down, so restoring the item makes it active
// again for the same people — the snapshot supersedes the tombstone on its own
// (a currently active item wins), with no tombstone deletion and therefore no
// loss of the record an offline device still needs.
func RecordAdminArchiveTombstonesTx(tx *gorm.DB, itemID string, archivedAt time.Time) (int, error) {
	return recordHolderTombstonesTx(tx, itemID, models.SyncTombstoneReasonAdminArchived, "", archivedAt)
}

// RecordItemDeleteTombstonesTx is the catalog entry point: the item row and its
// dependents are being hard-deleted.
//
// # Call it BEFORE the cascade removes favorites and receipts
//
// The holder set is derived from exactly those rows. Once they are gone the
// database can no longer answer "who had this?", so a tombstone written after
// the cascade tombstones nobody — silently, and with a success return. This is
// the one ordering constraint in this file that cannot be recovered from later:
// unlike an archive, there is no row left to re-derive the answer from.
//
// There is no compare-and-set to gate on here. A delete happens once (the
// caller has already established the row exists, and the cascade's visited set
// makes re-entry a no-op), so every call is a real transition.
func RecordItemDeleteTombstonesTx(tx *gorm.DB, itemID string, deletedAt time.Time) (int, error) {
	return recordHolderTombstonesTx(tx, itemID, models.SyncTombstoneReasonItemDeleted, "", deletedAt)
}

// RecordPackageFlattenTombstonesTx is the data-migration entry point:
// `migrate flatten-plugins` archived a package-derived Plugin child row under
// the flat capability model.
//
// Same calling contract as the other archiving writers: call it ONLY from inside
// the transaction whose compare-and-set proved this run moved the row off the
// shelf. The command applies its writes as raw SQL, so `RowsAffected == 1` on
// its own UPDATE is the proof — see recordPluginFlattenRemovalTx.
//
// It has its own reason rather than reusing admin_archived because the reason is
// shown to the user. Nothing about this removal is a moderation decision: no one
// looked at the capability, and the row is restorable by `rollback-apply`.
// Favorites and receipts are preserved, so a rollback reactivates the item and
// the active item supersedes this tombstone on its own.
func RecordPackageFlattenTombstonesTx(tx *gorm.DB, itemID string, archivedAt time.Time) (int, error) {
	return recordHolderTombstonesTx(tx, itemID, models.SyncTombstoneReasonPackageFlattened, "", archivedAt)
}

// recordHolderTombstonesTx tombstones every current holder of an item with one
// cause. Shared by all four entry points so "who held this" has exactly one
// definition.
func recordHolderTombstonesTx(tx *gorm.DB, itemID, reason, lifecycleReason string, removedAt time.Time) (int, error) {
	if itemID == "" {
		return 0, fmt.Errorf("%w: item is required", ErrInvalidTombstoneCause)
	}
	// A schema with no tombstone table has no snapshot-v2 contract to satisfy.
	// Production always has it (goose owns the DDL); the check exists for the
	// partial in-memory fixtures several packages hand-build, which is the same
	// reason itemdelete's cascade probes for each table it touches.
	if !tx.Migrator().HasTable(&models.CapabilitySyncTombstone{}) {
		return 0, nil
	}
	userIDs, err := entitledUserIDsTx(tx, itemID)
	if err != nil {
		return 0, err
	}
	for _, userID := range userIDs {
		if err := RecordEntitlementRemovalTx(tx, EntitlementRemoval{
			UserID:          userID,
			ItemID:          itemID,
			Reason:          reason,
			LifecycleReason: lifecycleReason,
			RemovedAt:       removedAt,
		}); err != nil {
			return 0, err
		}
	}
	return len(userIDs), nil
}

// entitledUserIDsTx lists every principal currently entitled to an item, by the
// same definition the snapshot's active set uses: a favorite, or a live
// distribution receipt.
//
// A principal holding BOTH is returned once, so the caller writes one tombstone
// for them — matching the table's one-row-per-(user,item) shape, which records
// the end of the entitlement rather than the removal of each relationship row.
func entitledUserIDsTx(tx *gorm.DB, itemID string) ([]string, error) {
	seen := make(map[string]struct{})
	ordered := make([]string, 0, 8)
	add := func(ids []string) {
		for _, id := range ids {
			if id == "" {
				continue
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			ordered = append(ordered, id)
		}
	}

	if tx.Migrator().HasTable(&models.ItemFavorite{}) {
		var favoriteUserIDs []string
		if err := tx.Model(&models.ItemFavorite{}).
			Where("item_id = ?", itemID).
			Pluck("user_id", &favoriteUserIDs).Error; err != nil {
			return nil, fmt.Errorf("load favorite holders of %s: %w", itemID, err)
		}
		add(favoriteUserIDs)
	}

	if tx.Migrator().HasTable(&models.ItemDistribution{}) && tx.Migrator().HasTable(&models.ItemDistributionReceipt{}) {
		var distributedUserIDs []string
		if err := tx.Model(&models.ItemDistributionReceipt{}).
			Joins("JOIN item_distributions ON item_distributions.id = item_distribution_receipts.distribution_id").
			Where("item_distributions.item_id = ? AND item_distributions.status = ? AND item_distribution_receipts.receipt_status != ?",
				itemID, "active", "dismissed").
			Pluck("item_distribution_receipts.user_id", &distributedUserIDs).Error; err != nil {
			return nil, fmt.Errorf("load distribution holders of %s: %w", itemID, err)
		}
		add(distributedUserIDs)
	}

	return ordered, nil
}

// tombstoneSourceFor derives Source from Reason and validates the lifecycle
// reason, mirroring chk_capability_sync_tombstones_cause.
//
// Source is derived rather than accepted because the pairing is fixed by the
// contract — an entitlement ends because Git archived the item, because the
// user unfavorited it, or because a distribution was revoked, and each has
// exactly one producing subsystem. Letting a caller supply both invites the
// exact row the CHECK was written to reject: a git_archived tombstone
// attributed to `favorite`, which carries no Git cause at all.
func tombstoneSourceFor(reason, lifecycleReason string) (string, error) {
	switch reason {
	case models.SyncTombstoneReasonGitArchived:
		switch lifecycleReason {
		case models.GitLifecycleReasonManifestRemoved,
			models.GitLifecycleReasonDefaultBranchMissing,
			models.GitLifecycleReasonRepositoryDeleted:
			return models.SyncTombstoneSourceGitLifecycle, nil
		default:
			return "", fmt.Errorf("%w: git_archived requires a lifecycle reason, got %q", ErrInvalidTombstoneCause, lifecycleReason)
		}
	case models.SyncTombstoneReasonUnfavorited:
		if lifecycleReason != "" {
			return "", fmt.Errorf("%w: unfavorited carries no lifecycle reason, got %q", ErrInvalidTombstoneCause, lifecycleReason)
		}
		return models.SyncTombstoneSourceFavorite, nil
	case models.SyncTombstoneReasonDistributionRevoked:
		if lifecycleReason != "" {
			return "", fmt.Errorf("%w: distribution_revoked carries no lifecycle reason, got %q", ErrInvalidTombstoneCause, lifecycleReason)
		}
		return models.SyncTombstoneSourceDistribution, nil
	case models.SyncTombstoneReasonAdminArchived:
		if lifecycleReason != "" {
			return "", fmt.Errorf("%w: admin_archived carries no lifecycle reason, got %q", ErrInvalidTombstoneCause, lifecycleReason)
		}
		return models.SyncTombstoneSourceModeration, nil
	case models.SyncTombstoneReasonItemDeleted:
		if lifecycleReason != "" {
			return "", fmt.Errorf("%w: item_deleted carries no lifecycle reason, got %q", ErrInvalidTombstoneCause, lifecycleReason)
		}
		return models.SyncTombstoneSourceCatalog, nil
	case models.SyncTombstoneReasonPackageFlattened:
		if lifecycleReason != "" {
			return "", fmt.Errorf("%w: package_flattened carries no lifecycle reason, got %q", ErrInvalidTombstoneCause, lifecycleReason)
		}
		return models.SyncTombstoneSourceDataMigration, nil
	default:
		return "", fmt.Errorf("%w: unknown reason %q", ErrInvalidTombstoneCause, reason)
	}
}
