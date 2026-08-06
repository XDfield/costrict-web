package models

import "time"

// Reasons a user's entitlement to a capability ended.
//
// The set is OPEN by contract: a tombstone's presence is the removal
// instruction and the reason only explains it, so csc removes on a reason it
// does not recognise (reporting the string verbatim) rather than ignoring the
// row. Adding one therefore needs no client release — but it does need a
// truthful name. Reusing an existing reason for a different kind of event is
// the failure mode this set exists to prevent, not a shortcut around it.
const (
	SyncTombstoneReasonGitArchived         = "git_archived"
	SyncTombstoneReasonUnfavorited         = "unfavorited"
	SyncTombstoneReasonDistributionRevoked = "distribution_revoked"
	// SyncTombstoneReasonAdminArchived is a moderation take-down: an operator
	// moved the item off the shelf (adminitem.SetStatus / PUT /items/:id).
	SyncTombstoneReasonAdminArchived = "admin_archived"
	// SyncTombstoneReasonItemDeleted is a catalog hard delete: the item row and
	// its dependents were removed outright.
	SyncTombstoneReasonItemDeleted = "item_deleted"
	// SyncTombstoneReasonPackageFlattened is a data migration retiring a
	// package-derived Plugin child row (`migrate flatten-plugins`): the row was a
	// duplicate projection of a file inside another item's package, and the flat
	// capability model has no place for it.
	//
	// It exists rather than borrowing admin_archived because no moderator looked
	// at anything — the reason reaches the device and the user, and telling them
	// an administrator judged their capability would be false.
	SyncTombstoneReasonPackageFlattened = "package_flattened"
)

// Which subsystem produced the tombstone. Determined by Reason; the pairing is
// enforced as a triple by chk_capability_sync_tombstones_cause.
const (
	SyncTombstoneSourceGitLifecycle = "git_lifecycle"
	SyncTombstoneSourceFavorite     = "favorite"
	SyncTombstoneSourceDistribution = "distribution"
	SyncTombstoneSourceModeration   = "moderation"
	SyncTombstoneSourceCatalog      = "catalog"
	// SyncTombstoneSourceDataMigration is an operational data migration. Spelled
	// out rather than "migration" because this same column already carries
	// "moderation", and the two are near-homographs in a log line.
	SyncTombstoneSourceDataMigration = "data_migration"
)

// CapabilitySyncTombstone is the durable, explicit "this user no longer has
// this capability" record that the csc snapshot v2 contract requires.
//
// It exists because absence must never authorize removal: a truncated page, an
// auth failure or an empty error response all look like "the item is gone" to a
// client that infers removal from a missing entry. Only a tombstone inside a
// complete, newer snapshot may unload anything.
//
// There is at most one row per (UserID, ItemID) — the record describes the end
// of the user's entitlement, not the deletion of one favorite/distribution row,
// so a user who both favorited and was distributed an item is tombstoned only
// when the last relationship ends.
//
// EventID is the client-facing dedup key and must be ROTATED on every new
// removal transition. A stable EventID would make unfavorite → refavorite →
// unfavorite dedupe to a no-op on the client, leaving the capability installed
// forever.
//
// ItemID has no foreign key on purpose: the tombstone must outlive an operator
// hard-deleting the item, or the instruction to remove it disappears together
// with the item.
//
// Mirrors migration 20260805000200_create_capability_sync_tombstones.sql, which
// is the sole owner of the DDL (not part of autoMigrateAll).
type CapabilitySyncTombstone struct {
	ID     string `gorm:"column:id;primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	UserID string `gorm:"column:user_id;type:varchar(191);not null;uniqueIndex:uq_capability_sync_tombstones_user_item,priority:1" json:"-"`
	ItemID string `gorm:"column:item_id;type:uuid;not null;uniqueIndex:uq_capability_sync_tombstones_user_item,priority:2;index" json:"itemId"`
	Reason string `gorm:"column:reason;type:varchar(32);not null" json:"reason"`
	// LifecycleReason is set only when Source is git_lifecycle, and then it
	// carries the capability_items.git_lifecycle_reason value observed at
	// archive time.
	LifecycleReason *string   `gorm:"column:lifecycle_reason;type:varchar(32)" json:"lifecycleReason,omitempty"`
	Source          string    `gorm:"column:source;type:varchar(32);not null" json:"source"`
	EventID         string    `gorm:"column:event_id;type:varchar(64);not null;uniqueIndex" json:"eventId"`
	RemovedAt       time.Time `gorm:"column:removed_at;type:timestamptz;not null" json:"removedAt"`
	CreatedAt       time.Time `gorm:"column:created_at;type:timestamptz;not null;default:now()" json:"-"`
	UpdatedAt       time.Time `gorm:"column:updated_at;type:timestamptz;not null;default:now()" json:"-"`
}

// TableName pins the production table name.
func (CapabilitySyncTombstone) TableName() string { return "capability_sync_tombstones" }
