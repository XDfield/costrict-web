package models

import "time"

// CapabilitySyncSnapshotContractVersion is the csc sync contract version this
// schema materializes. It is stored per snapshot rather than assumed, so a
// future v3 can coexist and legacy clients stay identifiable.
const CapabilitySyncSnapshotContractVersion = 2

// CapabilitySyncSnapshotGeneration is the per-principal generation allocator
// for csc snapshot v2.
//
// csc orders snapshots by Generation alone — never by wall clock, never by the
// opaque snapshot id — and refuses anything not strictly greater than what it
// last applied. Allocation is therefore a correctness component:
//
//	INSERT INTO capability_sync_snapshot_generations
//	    (principal_id, generation, last_allocated_at)
//	VALUES (?, 1, now())
//	ON CONFLICT (principal_id) DO UPDATE
//	    SET generation        = capability_sync_snapshot_generations.generation + 1,
//	        last_allocated_at = now(),
//	        updated_at        = now()
//	RETURNING generation
//
// run as the FIRST statement of a REPEATABLE READ transaction. That single
// statement both allocates the number and takes the row lock, so one
// principal's snapshot builds are serialized and each build's data view is
// established at (not before) its own allocation — which is what makes
// "generation order == data order" true. A loser blocks and then retries on
// serialization_failure with a strictly newer generation over a strictly newer
// view, instead of quietly serving stale content under a higher number.
//
// Gaps are permitted; csc requires strict increase, not density.
//
// Mirrors migration 20260805000300_create_capability_sync_snapshot_generations.sql.
type CapabilitySyncSnapshotGeneration struct {
	PrincipalID     string     `gorm:"column:principal_id;primaryKey;type:varchar(191)" json:"principalId"`
	Generation      int64      `gorm:"column:generation;not null;default:0" json:"generation"`
	LastSnapshotID  *string    `gorm:"column:last_snapshot_id;type:uuid" json:"lastSnapshotId,omitempty"`
	LastAllocatedAt *time.Time `gorm:"column:last_allocated_at;type:timestamptz" json:"lastAllocatedAt,omitempty"`
	CreatedAt       time.Time  `gorm:"column:created_at;type:timestamptz;not null;default:now()" json:"-"`
	UpdatedAt       time.Time  `gorm:"column:updated_at;type:timestamptz;not null;default:now()" json:"-"`
}

// TableName pins the production table name.
func (CapabilitySyncSnapshotGeneration) TableName() string {
	return "capability_sync_snapshot_generations"
}

// CapabilitySyncSnapshot is the immutable manifest every page of a paged
// snapshot repeats verbatim: contract version, opaque id, generation, page
// count, total item/tombstone counts, canonical digest and completeness.
//
// It is persisted rather than recomputed because page 3 cannot honestly derive
// whole-snapshot counts or a whole-snapshot digest from live data, and an
// expired cursor must be rejected rather than answered from data that has since
// changed.
//
// SnapshotDigest is the lowercase SHA-256 hex over the RFC 8785 canonical
// serialization of the reassembled snapshot (shared manifest + items sorted by
// item id + tombstones sorted by (itemId, eventId); page-local cursor/index and
// `complete` excluded). It stays NULL until the build finalizes, and Complete
// may only become true together with a digest and at least one page — an
// interrupted build can never present itself as authoritative.
//
// Mirrors migration 20260805000300_create_capability_sync_snapshot_generations.sql.
type CapabilitySyncSnapshot struct {
	ID              string  `gorm:"column:id;primaryKey;type:uuid;default:gen_random_uuid()" json:"snapshotId"`
	PrincipalID     string  `gorm:"column:principal_id;type:varchar(191);not null;uniqueIndex:uq_capability_sync_snapshots_generation,priority:1" json:"-"`
	Generation      int64   `gorm:"column:generation;not null;uniqueIndex:uq_capability_sync_snapshots_generation,priority:2" json:"generation"`
	ContractVersion int     `gorm:"column:contract_version;not null;default:2" json:"contractVersion"`
	PageCount       int     `gorm:"column:page_count;not null;default:0" json:"pageCount"`
	ItemCount       int     `gorm:"column:item_count;not null;default:0" json:"itemCount"`
	TombstoneCount  int     `gorm:"column:tombstone_count;not null;default:0" json:"tombstoneCount"`
	SnapshotDigest  *string `gorm:"column:snapshot_digest;type:varchar(64)" json:"snapshotDigest,omitempty"`
	// Complete is false for a partially materialized snapshot. A false snapshot
	// can never authorize a client removal.
	Complete    bool      `gorm:"column:complete;not null;default:false" json:"complete"`
	GeneratedAt time.Time `gorm:"column:generated_at;type:timestamptz;not null;default:now()" json:"generatedAt"`
	ExpiresAt   time.Time `gorm:"column:expires_at;type:timestamptz;not null" json:"-"`
	CreatedAt   time.Time `gorm:"column:created_at;type:timestamptz;not null;default:now()" json:"-"`
}

// TableName pins the production table name.
func (CapabilitySyncSnapshot) TableName() string { return "capability_sync_snapshots" }
