package models

import (
	"time"
)

// Tenant is the canonical tenant entity (Phase B1). One row per tenant.
// Paired with TenantConfig (per-tenant YAML, A2) and TenantAdmin (user ×
// tenant membership, B1).
//
// Schema decisions (see migration 20260717100000_create_tenants_and_tenant_admins.sql):
//
//   - tenant_id is TEXT (UUID-format string), matching tenant_configs.tenant_id
//     and users.id conventions — cs-user is not yet locked to UUID column types.
//   - email_domains / features / limits / settings are TEXT columns holding JSON
//     text (not JSONB / TEXT[]), matching the EmploymentIdentity.Attributes
//     pattern. App-layer marshaling; B2/B3 introduce typed readers.
//   - timestamps use TIMESTAMPTZ (RFC 3339 best practice; diverges from the
//     legacy users table which uses TIMESTAMPTZ-less TIMESTAMP).
//
// The default tenant (tenant_id="default", slug="default") is bootstrapped by
// the migration. Phase B code runs in default-tenant context until B5
// activates dynamic tenant routing.
type Tenant struct {
	TenantID            string     `gorm:"primaryKey;type:text" json:"tenant_id"`
	Slug                string     `gorm:"size:32;not null;uniqueIndex:uq_tenants_slug" json:"slug"`
	DisplayName         string     `gorm:"size:191;not null" json:"display_name"`
	Status              string     `gorm:"size:32;not null;default:active" json:"status"`
	Edition             string     `gorm:"size:32;not null;default:team" json:"edition"`
	EmailDomains        string     `gorm:"type:text;not null;default:'[]'" json:"email_domains"`
	Features            string     `gorm:"type:text;not null;default:'{}'" json:"features"`
	Limits              string     `gorm:"type:text;not null;default:'{}'" json:"limits"`
	Settings            string     `gorm:"type:text;not null;default:'{}'" json:"settings"`
	DeletionRequestedAt *time.Time `json:"deletion_requested_at,omitempty"`
	DeletedAt           *time.Time `json:"deleted_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

func (Tenant) TableName() string { return "tenants" }
