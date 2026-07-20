package models

import "time"

// Sync-status vocabulary for user_gitea_binding (Phase E3a.1).
//
// No DB-level CHECK constraint (matching the audit_log decision in
// 20260720150000_create_user_center_audit_log.sql) so new states can be
// appended without a migration. App-layer validation rejects anything
// outside this set where it matters.
//
// E3a.1 state machine:
//
//	pending ── POST /admin/users 201 ──► synced
//	   │
//	   └── 4xx / 5xx / network / timeout ──► error
//
// Reconciliation (pending → synced on next attempt) is E3a.2; the row stays
// in its terminal state until then.
const (
	// GiteaSyncStatusPending — INSERT default; Gitea account not yet created.
	GiteaSyncStatusPending = "pending"
	// GiteaSyncStatusSynced — POST /admin/users succeeded; gitea_uid populated.
	GiteaSyncStatusSynced = "synced"
	// GiteaSyncStatusError — provisioning failed; last_error populated.
	// Reconciliation cron (E3a.2) will retry.
	GiteaSyncStatusError = "error"
)

// UserGiteaBinding records the 1:1 mapping between a cs-user user and the
// Gitea account auto-provisioned for them (Phase E3a.1).
//
// Lifecycle: one row per (user_subject_id, tenant_id). Created by
// giteasync.Service.Provision on first signup; updated as the state machine
// transitions. Survives users hard-delete (no FK) so reconciliation cron can
// detect orphan Gitea accounts.
//
// Nullable fields use *int64 / *string / *time.Time so absent values
// serialize as JSON null rather than zero values; the migration's DEFAULT
// now() fills created_at / updated_at server-side, but the Go layer also
// stamps them for sqlite-backed test parity.
type UserGiteaBinding struct {
	UserSubjectID string     `gorm:"column:user_subject_id;primaryKey;type:text" json:"user_subject_id"`
	TenantID      string     `gorm:"column:tenant_id;primaryKey;type:text" json:"tenant_id"`
	GiteaUID      *int64     `gorm:"column:gitea_uid;type:bigint" json:"gitea_uid,omitempty"`
	GiteaUsername string     `gorm:"column:gitea_username;type:varchar(64);not null" json:"gitea_username"`
	SyncStatus    string     `gorm:"column:sync_status;type:varchar(32);not null;default:pending" json:"sync_status"`
	LastSyncedAt  *time.Time `gorm:"column:last_synced_at" json:"last_synced_at,omitempty"`
	LastError     *string    `gorm:"column:last_error;type:text" json:"last_error,omitempty"`
	CreatedAt     time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt     time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName overrides GORM's default pluralization (user_gitea_bindings) to
// match the migration's singular user_gitea_binding.
func (UserGiteaBinding) TableName() string { return "user_gitea_binding" }
