package models

import (
	"time"

	"gorm.io/gorm"
)

// EmploymentIdentity is the per-user enterprise-identity snapshot written by
// ApplyEnterpriseMapping in the OAuth callback (lands in Phase A4).
//
// Phase B2 added TenantID + FK to tenants(tenant_id) + the (tenant_id,
// user_subject_id) composite index. The (tenant_id, enterprise_uid) unique
// index still waits on enterprise_uid itself landing in a later Phase B step
// (MULTI_TENANCY §6.5.1, §8.3) — the A1 partial unique index on
// user_subject_id WHERE deleted_at IS NULL stays as the per-tenant-scope
// uniqueness guarantee until then.
//
// App-layer references to users.subject_id (no SQL FK — same convention as
// UserAuthIdentity). Sync cadence is driven by next_sync_due_at + the
// provider TTL configured in tenant_configs.
type EmploymentIdentity struct {
	ID                       uint           `gorm:"primaryKey;autoIncrement" json:"id"`
	TenantID                 string         `gorm:"type:text;size:191;not null;default:default;index:idx_employment_identities_tenant_id,priority:1;index:idx_employment_identities_tenant_user,priority:1" json:"tenant_id"`
	UserSubjectID            string         `gorm:"index:idx_employment_identities_user_subject_id;not null;size:191;index:idx_employment_identities_tenant_user,priority:2" json:"user_subject_id"`
	Provider                 string         `gorm:"index:idx_employment_identities_provider;size:64;not null" json:"provider"`
	EmployeeNumber           *string        `gorm:"size:191" json:"employee_number"`
	CostCenter               *string        `gorm:"index:idx_employment_identities_cost_center;size:191" json:"cost_center"`
	OrgPath                  *string        `gorm:"type:text" json:"org_path"`
	DirectManagerSubjectID   *string        `gorm:"index:idx_employment_identities_manager;size:191" json:"direct_manager_subject_id"`
	DirectManagerExternalRef *string        `gorm:"type:text" json:"direct_manager_external_ref"`
	JobTitle                 *string        `gorm:"size:191" json:"job_title"`
	JobLevel                 *string        `gorm:"size:32" json:"job_level"`
	EmploymentType           *string        `gorm:"size:32" json:"employment_type"`
	HireDate                 *time.Time     `gorm:"type:date" json:"hire_date"`
	RegularDate              *time.Time     `gorm:"type:date" json:"regular_date"`
	WorkLocation             *string        `gorm:"size:191" json:"work_location"`
	Attributes               string         `gorm:"type:text;not null;default:'{}'" json:"attributes"`
	SyncStatus               string         `gorm:"size:32;not null;default:'fresh'" json:"sync_status"`
	LastSyncedAt             time.Time      `gorm:"not null;default:CURRENT_TIMESTAMP" json:"last_synced_at"`
	NextSyncDueAt            time.Time      `gorm:"not null;default:CURRENT_TIMESTAMP" json:"next_sync_due_at"`
	RawPayloadHash           *string        `gorm:"size:255" json:"raw_payload_hash"`
	CreatedAt                time.Time      `json:"created_at"`
	UpdatedAt                time.Time      `json:"updated_at"`
	DeletedAt                gorm.DeletedAt `gorm:"index" json:"-"`
}

func (EmploymentIdentity) TableName() string { return "employment_identities" }
