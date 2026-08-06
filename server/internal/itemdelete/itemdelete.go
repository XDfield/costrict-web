// Package itemdelete owns the single source of truth for hard-deleting a
// capability item and ALL of its associated data.
//
// The public DeleteItem handler (internal/handlers) and the platform-admin
// delete service (internal/adminitem) historically carried byte-for-byte
// identical cascade logic. Divergence between the two was a latent bug source
// (a fix to one path silently skipped the other), so the cascade lives here and
// both callers — plus the admin batch-delete endpoint — share it.
//
// Cascade semantics (within the caller-provided transaction):
//   - every current holder is tombstoned FIRST, before any relationship row is
//     removed. The holder set is derived from item_favorites and the live
//     distribution receipts, so once the cascade below has deleted those the
//     question "who had this?" is unanswerable — a tombstone written afterwards
//     silently tombstones nobody.
//   - bundled sub-skills (parent_plugin_id = id) are HARD-deleted recursively,
//     each carrying its own dependent rows. This replaces the previous
//     soft-archive ("status='archived'") behavior: a deleted plugin must not
//     leave dangling sub-skill rows pointing at a non-existent parent.
//   - dependent rows keyed by item_id are hard-deleted (versions/assets/
//     artifacts/favorites/tags/scans/behavior logs/mcp user configs).
//   - distribution rows and their receipts are cleared (previously orphaned).
//   - the item row itself is hard-deleted last.
//   - a Git-backed row anywhere in the cascade aborts the whole thing with
//     models.ErrGitBackedItemsPresent (handlers map it to 409). Deleting one
//     would not remove the capability: the repository is still bound, and the
//     next push recreates it under a new uuid.
//
// Forks are deliberately left intact: a fork (forked_from_item_id = id, owned by
// another user) is that user's own copy and survives the source's deletion;
// forked_from_owner_id preserves attribution for the now-missing source.
package itemdelete

import (
	"fmt"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"gorm.io/gorm"
)

// CascadeDelete hard-deletes item id and all of its associated data inside tx.
// The caller owns the transaction (so a batch can roll the whole set back on a
// single failure). It is the caller's responsibility to confirm the item exists
// and to enforce authorization before calling this.
func CascadeDelete(tx *gorm.DB, id string) error {
	return cascadeDelete(tx, id, map[string]bool{})
}

// cascadeDelete is the recursive worker. visited guards against pathological
// self-referential parent_plugin_id data (a row should never be its own
// ancestor, but a cycle in dirty data must not loop forever).
func cascadeDelete(tx *gorm.DB, id string, visited map[string]bool) error {
	if visited[id] {
		return nil
	}
	visited[id] = true

	// 0) Git-backed rows are never hard-deleted here. The repository outlives the
	//    row, so the next push finds no bound item and recreates the capability
	//    under a new uuid, leaving every reference to the old id dangling. Callers
	//    pre-flight this (and produce a better message), but the check is repeated
	//    per row because the recursion below reaches ids the caller never named:
	//    a DB-backed plugin may bundle a Git-backed sub-skill.
	if err := models.RefuseGitBackedItems(tx, models.CapabilityItemsWithIDs(id)); err != nil {
		return err
	}

	// 1) Tombstone every current holder — BEFORE anything below removes the
	//    rows that identify them.
	//
	//    A hard delete ends the entitlement of everyone who favorited the item
	//    or holds a live distribution receipt for it, and under snapshot v2 an
	//    item's absence never authorizes a client to unload it. Without this the
	//    capability stays installed on every one of those devices permanently,
	//    and nothing reports a fault: the server has no row left to notice is
	//    missing, and the client was never told anything.
	//
	//    The ordering is the whole point and it is not recoverable afterwards.
	//    Step 3 deletes item_favorites and step 4 deletes the distribution
	//    receipts; run this after either of them and the holder query returns
	//    an empty set, writes nothing, and returns success. Unlike an archive —
	//    where the row survives and the answer can be recomputed — the evidence
	//    here is gone for good.
	if err := recordDeletionTombstones(tx, id); err != nil {
		return err
	}

	// 2) Recurse into bundled sub-skills first so each child clears its own
	//    dependent rows before the parent row goes away.
	//
	//    Deliberately unguarded: capability_items is not optional here — step 5
	//    deletes from it. A probe in front of this could only ever skip the
	//    recursion on a schema where the parent delete then succeeds, leaving
	//    exactly the dangling sub-skill rows this package's doc comment says the
	//    cascade exists to prevent. If the table is unreachable, the Pluck says
	//    so and the whole delete rolls back, which is the correct outcome.
	var subIDs []string
	if err := tx.Model(&models.CapabilityItem{}).
		Where("parent_plugin_id = ?", id).
		Pluck("id", &subIDs).Error; err != nil {
		return fmt.Errorf("failed to list sub-skills of %s: %w", id, err)
	}
	for _, sid := range subIDs {
		if sid == id || sid == "" {
			continue
		}
		if err := cascadeDelete(tx, sid, visited); err != nil {
			return err
		}
	}

	// 3) Dependent rows keyed by item_id. Best-effort across schemas: older
	//    deployments / SQLite unit fixtures may lack some tables, so skip any
	//    table that does not exist rather than failing the whole delete.
	//
	//    "Optional" is the honest word for these — none of them decides whether
	//    a device keeps a capability — but the probe must still resolve the
	//    table the DELETE would resolve. models.TableReachable does; gorm's
	//    Migrator().HasTable answers about CURRENT_SCHEMA() instead, so on a
	//    connection whose search_path reaches these tables through a later entry
	//    every one of them would be skipped while step 5 deleted the item row
	//    anyway, littering the schema with rows pointing at a row that is gone.
	deletions := []struct {
		model any
		name  string
	}{
		{&models.BehaviorLog{}, "behavior logs"},
		{&models.ItemFavorite{}, "item favorites"},
		{&models.ItemTag{}, "item tags"},
		{&models.ScanJob{}, "scan jobs"},
		{&models.SecurityScan{}, "security scans"},
		{&models.CapabilityVersionAsset{}, "capability version assets"},
		{&models.CapabilityAsset{}, "capability assets"},
		{&models.CapabilityArtifact{}, "capability artifacts"},
		{&models.CapabilityVersion{}, "capability versions"},
		{&models.MCPUserConfig{}, "mcp user configs"},
	}
	for _, d := range deletions {
		present, err := models.TableReachable(tx, d.model)
		if err != nil {
			return fmt.Errorf("failed to probe %s for %s: %w", d.name, id, err)
		}
		if !present {
			continue
		}
		query := tx.Where("item_id = ?", id)
		if _, ok := d.model.(*models.CapabilityVersionAsset); ok {
			// version assets reference versions, not the item directly. The
			// subquery needs capability_versions; if that table is absent
			// (asymmetric/partial schema) skip rather than erroring the whole
			// cascade — the probe above only covers the asset table.
			versions, err := models.TableReachable(tx, &models.CapabilityVersion{})
			if err != nil {
				return fmt.Errorf("failed to probe capability versions for %s: %w", id, err)
			}
			if !versions {
				continue
			}
			query = tx.Where("version_id IN (?)",
				tx.Model(&models.CapabilityVersion{}).Select("id").Where("item_id = ?", id))
		}
		if err := query.Delete(d.model).Error; err != nil {
			return fmt.Errorf("failed to delete %s for %s: %w", d.name, id, err)
		}
	}

	// 4) Distribution receipts reference distributions, which reference the item.
	//    Delete receipts first, then the distributions themselves. (Receipts may
	//    carry a forked_item_id pointing at a fork copy — that fork is another
	//    user's item and is NOT touched here, only the receipt row is removed.)
	//    Step 1 already required both of these to be reachable (they define the
	//    holder set), so in practice the probes below cannot fail here — they
	//    are kept so this block stays correct if it is ever reached on its own.
	distributions, err := models.TableReachable(tx, &models.ItemDistribution{})
	if err != nil {
		return fmt.Errorf("failed to probe distributions for %s: %w", id, err)
	}
	if distributions {
		receipts, err := models.TableReachable(tx, &models.ItemDistributionReceipt{})
		if err != nil {
			return fmt.Errorf("failed to probe distribution receipts for %s: %w", id, err)
		}
		if receipts {
			if err := tx.Where("distribution_id IN (?)",
				tx.Model(&models.ItemDistribution{}).Select("id").Where("item_id = ?", id)).
				Delete(&models.ItemDistributionReceipt{}).Error; err != nil {
				return fmt.Errorf("failed to delete distribution receipts for %s: %w", id, err)
			}
		}
		if err := tx.Where("item_id = ?", id).Delete(&models.ItemDistribution{}).Error; err != nil {
			return fmt.Errorf("failed to delete distributions for %s: %w", id, err)
		}
	}

	// 5) The item row itself, last.
	if err := tx.Delete(&models.CapabilityItem{}, "id = ?", id).Error; err != nil {
		return fmt.Errorf("failed to delete item %s: %w", id, err)
	}
	return nil
}

// recordDeletionTombstones writes the catalog tombstones for one item.
//
// The tombstone table carries no foreign key to capability_items precisely so
// these rows outlive the item — the instruction to remove a capability must not
// disappear together with the capability.
func recordDeletionTombstones(tx *gorm.DB, id string) error {
	if _, err := services.RecordItemDeleteTombstonesTx(tx, id, time.Now()); err != nil {
		return fmt.Errorf("failed to record removal tombstones for %s: %w", id, err)
	}
	return nil
}

// CascadeDeleteMany hard-deletes every id in ids inside tx, sharing one visited
// set so a sub-skill removed as part of an earlier plugin is not double-deleted.
// An id that no longer exists when its turn comes (already removed as a sub-skill
// of an earlier id, or never existed) is reported in skipped instead of deleted.
// The caller owns tx; on the first hard error this returns it so the caller's
// transaction rolls the entire batch back.
func CascadeDeleteMany(tx *gorm.DB, ids []string) (deleted, skipped []string, err error) {
	visited := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			continue
		}
		// Already removed in this batch (e.g. as a sub-skill of an earlier id).
		if visited[id] {
			skipped = append(skipped, id)
			continue
		}
		var count int64
		if err = tx.Model(&models.CapabilityItem{}).Where("id = ?", id).Count(&count).Error; err != nil {
			return nil, nil, err
		}
		if count == 0 {
			skipped = append(skipped, id)
			continue
		}
		if err = cascadeDelete(tx, id, visited); err != nil {
			return nil, nil, err
		}
		deleted = append(deleted, id)
	}
	return deleted, skipped, nil
}
