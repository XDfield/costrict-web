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
//   - bundled sub-skills (parent_plugin_id = id) are HARD-deleted recursively,
//     each carrying its own dependent rows. This replaces the previous
//     soft-archive ("status='archived'") behavior: a deleted plugin must not
//     leave dangling sub-skill rows pointing at a non-existent parent.
//   - dependent rows keyed by item_id are hard-deleted (versions/assets/
//     artifacts/favorites/tags/scans/behavior logs/mcp user configs).
//   - distribution rows and their receipts are cleared (previously orphaned).
//   - the item row itself is hard-deleted last.
//
// Forks are deliberately left intact: a fork (forked_from_item_id = id, owned by
// another user) is that user's own copy and survives the source's deletion;
// forked_from_owner_id preserves attribution for the now-missing source.
package itemdelete

import (
	"fmt"

	"github.com/costrict/costrict-web/server/internal/models"
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

	// 1) Recurse into bundled sub-skills first so each child clears its own
	//    dependent rows before the parent row goes away.
	if tx.Migrator().HasTable(&models.CapabilityItem{}) {
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
	}

	// 2) Dependent rows keyed by item_id. Best-effort across schemas: older
	//    deployments / SQLite unit fixtures may lack some tables, so skip any
	//    table that does not exist rather than failing the whole delete.
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
		if !tx.Migrator().HasTable(d.model) {
			continue
		}
		query := tx.Where("item_id = ?", id)
		if _, ok := d.model.(*models.CapabilityVersionAsset); ok {
			// version assets reference versions, not the item directly. The
			// subquery needs capability_versions; if that table is absent
			// (asymmetric/partial schema) skip rather than erroring the whole
			// cascade — the HasTable above only covers the asset table.
			if !tx.Migrator().HasTable(&models.CapabilityVersion{}) {
				continue
			}
			query = tx.Where("version_id IN (?)",
				tx.Model(&models.CapabilityVersion{}).Select("id").Where("item_id = ?", id))
		}
		if err := query.Delete(d.model).Error; err != nil {
			return fmt.Errorf("failed to delete %s for %s: %w", d.name, id, err)
		}
	}

	// 3) Distribution receipts reference distributions, which reference the item.
	//    Delete receipts first, then the distributions themselves. (Receipts may
	//    carry a forked_item_id pointing at a fork copy — that fork is another
	//    user's item and is NOT touched here, only the receipt row is removed.)
	if tx.Migrator().HasTable(&models.ItemDistribution{}) {
		if tx.Migrator().HasTable(&models.ItemDistributionReceipt{}) {
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

	// 4) The item row itself, last.
	if err := tx.Delete(&models.CapabilityItem{}, "id = ?", id).Error; err != nil {
		return fmt.Errorf("failed to delete item %s: %w", id, err)
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
