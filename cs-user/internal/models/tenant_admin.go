package models

import (
	"time"
)

// Tenant role constants — see migration
// 20260717100000_create_tenants_and_tenant_admins.sql.
//
// B1 does not add a CHECK constraint at the DB level (so adding a new role
// does not require a migration); app-layer validation rejects anything outside
// this set.
const (
	TenantRoleOwner   = "owner"   // full control incl. delete + role grant
	TenantRoleAdmin   = "admin"   // day-to-day admin (invite, configure)
	TenantRoleBilling = "billing" // billing-only (read quota, manage plan)
)

// TenantAdmin records one user's role grant on one tenant (Phase B1).
// Composite primary key (tenant_id, user_id) enforces "one active grant per
// user per tenant" — the same user can still hold grants in MULTIPLE tenants
// (multi-tenant membership).
//
// Lifecycle:
//
//   - INSERT with revoked_at=NULL to grant a role.
//   - UPDATE revoked_at=now() to revoke (NOT DELETE — preserve audit trail).
//   - The migration's idx_tenant_admins_user_active partial index covers
//     the hot path "given user_id, find all tenants where they're active".
//
// granted_by is RESTRICT on delete (can't delete a user who has granted
// roles without first re-assigning those grants); user_id is CASCADE
// (deleting a user revokes all their grants automatically).
type TenantAdmin struct {
	TenantID  string     `gorm:"primaryKey;type:text" json:"tenant_id"`
	UserID    string     `gorm:"primaryKey;size:191" json:"user_id"`
	Role      string     `gorm:"size:32;not null" json:"role"`
	GrantedBy string     `gorm:"column:granted_by;size:191;not null" json:"granted_by"`
	GrantedAt time.Time  `gorm:"not null" json:"granted_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

func (TenantAdmin) TableName() string { return "tenant_admins" }
