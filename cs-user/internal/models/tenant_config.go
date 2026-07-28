package models

import (
	"time"
)

// TenantConfig is the per-tenant YAML configuration store (Phase A minimal
// shape). Each row holds one tenant_id + a single config_yaml blob.
//
// Phase A does not parse the YAML — it stores the blob verbatim. A4
// (ApplyEnterpriseMapping) introduces the first typed reader for the
// employment_providers section. Phase B expands this into typed subsections
// and adds the tenants(tenant_id) FK once the tenants table lands (B1).
//
// tenant_id is text (not UUID) in Phase A so the project is not locked to
// UUID-before-Phase-B1-lands. The A6 bootstrap row uses tenant_id="default".
type TenantConfig struct {
	TenantID   string    `gorm:"primaryKey;type:text" json:"tenant_id"`
	ConfigYAML string    `gorm:"type:text;not null;default:'{}'" json:"config_yaml"`
	UpdatedBy  *string   `gorm:"type:text" json:"updated_by"`
	UpdatedAt  time.Time `json:"updated_at"`
	CreatedAt  time.Time `json:"created_at"`
}

func (TenantConfig) TableName() string { return "tenant_configs" }
