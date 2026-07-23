package models

import "time"

// Sync-status vocabulary for user_git_binding (Git Ownership Refactor).
//
// No DB-level CHECK constraint (matching the cs-user decision) so new states
// can be appended without a migration.
//
// State machine:
//
//	pending ── POST /admin/users 201 ──► synced
//	   │
//	   └── 4xx / 5xx / network / timeout ──► error
const (
	GitSyncStatusPending = "pending"
	GitSyncStatusSynced  = "synced"
	GitSyncStatusError   = "error"
)

// UserGitBinding records the 1:1 mapping between a cs-user user and the
// Git server account auto-provisioned for them. Provider-agnostic —
// provider_kind denormalizes git_servers.kind so the row is self-describing
// without a JOIN.
//
// Mirrors migration 20260722150010 + 20260722300000_rename_to_user_git_binding.
type UserGitBinding struct {
	UserSubjectID string     `gorm:"column:user_subject_id;primaryKey;type:text" json:"user_subject_id"`
	TenantID      string     `gorm:"column:tenant_id;primaryKey;type:text" json:"tenant_id"`
	GitUID        *int64     `gorm:"column:git_uid;type:bigint" json:"git_uid,omitempty"`
	GitUsername   string     `gorm:"column:git_username;type:varchar(64);not null" json:"git_username"`
	ProviderKind  string     `gorm:"column:provider_kind;type:varchar(32);not null;default:gitea" json:"provider_kind"`
	SyncStatus    string     `gorm:"column:sync_status;type:varchar(32);not null;default:pending" json:"sync_status"`
	LastSyncedAt  *time.Time `gorm:"column:last_synced_at" json:"last_synced_at,omitempty"`
	LastError     *string    `gorm:"column:last_error;type:text" json:"last_error,omitempty"`
	CreatedAt     time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt     time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName pins the table name (singular, mirroring cs-user schema shape).
func (UserGitBinding) TableName() string { return "user_git_binding" }

// TenantGitServerBinding records the 1:1 mapping between a tenant and its
// bound git_server. server has no tenants main table (tenants live in cs-user);
// this is server's local binding record.
//
// Mirrors migration 20260722150020_create_tenant_git_server_binding.sql 1:1.
type TenantGitServerBinding struct {
	TenantID    string    `gorm:"primaryKey;type:varchar(191)" json:"tenant_id"`
	GitServerID string    `gorm:"type:varchar(64);not null" json:"git_server_id"`
	BoundAt     time.Time `gorm:"type:timestamptz;not null;default:now()" json:"bound_at"`
	UpdatedAt   time.Time `gorm:"type:timestamptz;not null;default:now()" json:"updated_at"`
}

// TableName pins the table name.
func (TenantGitServerBinding) TableName() string { return "tenant_git_server_binding" }
