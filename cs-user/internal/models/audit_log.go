package models

import (
	"time"
)

// Audit action vocabulary — see migration
// 20260720150000_create_user_center_audit_log.sql and MULTI_TENANCY_DESIGN §16.2.
//
// No CHECK constraint at the DB level (so adding a new action does not require
// a migration); app-layer validation rejects anything outside this set when
// needed, but the typical posture is permissive — log the action string
// verbatim and let downstream consumers (list endpoint, dashboards) interpret.
//
// C4.1 lands the first 6 actions; future slices (C4.2 active detection,
// C4.3 user ops audit, Phase E webhook fanout) append to this set.
const (
	// ActionTenantCreate — platform_admin creates a new tenant.
	// Target: tenant:<new tenant_id>.
	ActionTenantCreate = "tenant.create"
	// ActionTenantSuspend — platform_admin suspends an active tenant.
	// Target: tenant:<tenant_id>.
	ActionTenantSuspend = "tenant.suspend"
	// ActionTenantRestore — platform_admin restores a suspended tenant.
	// Target: tenant:<tenant_id>.
	ActionTenantRestore = "tenant.restore"
	// ActionTenantDeletionRequested — platform_admin marks tenant for 30-day
	// grace deletion. Target: tenant:<tenant_id>.
	ActionTenantDeletionRequested = "tenant.deletion_requested"
	// ActionTenantConfigUpdate — tenant_admin / platform_admin updates the
	// raw tenant_config YAML blob. Target: tenant_config:<tenant_id>.
	ActionTenantConfigUpdate = "tenant_config.update"
	// ActionProviderMappingUpdate — tenant_admin / platform_admin updates
	// the typed provider_mapping subtree. Target: provider_mapping:<tenant_id>.
	ActionProviderMappingUpdate = "provider_mapping.update"
)

// Target type constants — pair with target_id to identify the audit row's
// resource. Kept as constants (not stringly-typed in callers) so renames
// surface as compile errors.
const (
	TargetTypeTenant          = "tenant"
	TargetTypeTenantConfig    = "tenant_config"
	TargetTypeProviderMapping = "provider_mapping"
)

// AuditLog records one admin write operation (Phase C4.1).
//
// Lifecycle: append-only. The service layer must never issue UPDATE or DELETE
// against this table; only INSERT. Retention / TTL is a separate ops concern
// (C4.x out-of-scope) — the table grows unbounded until a cron lands.
//
// Foreign keys: deliberately none. Audit rows must survive their target
// tenant's hard-delete (regulator-visible action history per §16.2). The
// actor_subject_id and tenant_id columns are bare references — joining to
// users/tenants is caller's responsibility (left join + NULL tolerance).
//
// Nullable fields use *string / *time.Time so absent values serialize as JSON
// null rather than empty strings; the migration's DEFAULT now() fills
// created_at server-side, but the Go layer also sets it explicitly for
// sqlite-backed test parity.
type AuditLog struct {
	ID                 int64     `gorm:"primaryKey;column:id" json:"id"`
	TenantID           *string   `gorm:"column:tenant_id;type:text" json:"tenant_id,omitempty"`
	ActorSubjectID     *string   `gorm:"column:actor_subject_id;type:text" json:"actor_subject_id,omitempty"`
	ActorTenantRole    *string   `gorm:"column:actor_tenant_role;type:varchar(32)" json:"actor_tenant_role,omitempty"`
	ActorPlatformScope *string   `gorm:"column:actor_platform_scope;type:varchar(32)" json:"actor_platform_scope,omitempty"`
	Action             string    `gorm:"column:action;type:varchar(64);not null" json:"action"`
	TargetType         *string   `gorm:"column:target_type;type:varchar(32)" json:"target_type,omitempty"`
	TargetID           *string   `gorm:"column:target_id;type:text" json:"target_id,omitempty"`
	Payload            []byte    `gorm:"column:payload;type:jsonb" json:"payload,omitempty"`
	IP                 *string   `gorm:"column:ip;type:varchar(45)" json:"ip,omitempty"`
	UserAgent          *string   `gorm:"column:user_agent;type:text" json:"user_agent,omitempty"`
	CreatedAt          time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName overrides the default GORM pluralization (audit_logs) to match
// the migration's singular user_center_audit_log.
func (AuditLog) TableName() string { return "user_center_audit_log" }
