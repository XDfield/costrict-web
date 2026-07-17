package models

import "time"

// Platform admin scope constants — see migration
// 20260717190000_create_platform_admins.sql and MULTI_TENANCY_DESIGN §14.3.
//
// No CHECK constraint at the DB level (so adding a new scope does not require
// a migration); app-layer validation rejects anything outside this set.
const (
	// PlatformScopeFull grants all platform operations: tenant CRUD, cross-
	// tenant audit, configuration changes, granting/revoking other admins.
	PlatformScopeFull = "full"
	// PlatformScopeSupport limits the holder to read tenant data + reset
	// tenant_admin passwords; cannot change tenant configuration.
	PlatformScopeSupport = "support"
	// PlatformScopeReadOnly is read-only across the platform — all platform
	// write operations are rejected.
	PlatformScopeReadOnly = "read_only"
)

// PlatformAdmin records one user's platform-level admin grant (Phase C1).
//
// Unlike TenantAdmin (multi-row per user across tenants), PlatformAdmin has a
// primary key on user_id alone — a user is either a platform admin or not,
// with exactly one scope. The cross-tenant authority is global, not per-
// tenant.
//
// Lifecycle:
//
//   - INSERT with scope=full|support|read_only to grant.
//   - UPDATE scope=... to change scope without dropping the grant.
//   - DELETE to revoke (no soft-delete / revoked_at column — audit trail
//     lives in user_center_audit_log per MULTI_TENANCY §16.2, not in this
//     table). The lower frequency of platform_admin changes vs tenant_admin
//     role changes makes this asymmetry acceptable.
//
// granted_by is RESTRICT on delete (can't delete a user who has granted
// platform_admin without first re-assigning); user_id is CASCADE (deleting
// the admin user revokes their platform grant automatically).
//
// C1 does NOT bootstrap any initial platform_admin via migration. Operator
// must manually INSERT the first platform_admin after the first user logs in
// (typically `INSERT INTO platform_admins (user_id, granted_by, scope)
// VALUES ('usr_xxx', 'usr_xxx', 'full');`). See migration header for
// rationale.
type PlatformAdmin struct {
	UserID    string    `gorm:"primaryKey;type:varchar(191);column:user_id" json:"user_id"`
	GrantedBy string    `gorm:"column:granted_by;type:varchar(191);not null" json:"granted_by"`
	GrantedAt time.Time `gorm:"not null" json:"granted_at"`
	Scope     string    `gorm:"type:varchar(32);not null;default:full" json:"scope"`
}

func (PlatformAdmin) TableName() string { return "platform_admins" }
