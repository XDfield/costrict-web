package models

import (
	"time"
)

// TenantConfig is the per-tenant YAML configuration store. Each row holds
// one tenant_id + a single config_yaml blob.
//
// The store persists the blob verbatim and does not parse the YAML on the
// raw-blob CRUD path. ApplyEnterpriseMapping consumes the first typed
// subsection (employment_providers); subsequent typed subsections sit on
// top of the same blob.
//
// tenant_id is TEXT (not UUID) so the project is not locked to a single
// identifier format. The bootstrap row uses tenant_id="default".
type TenantConfig struct {
	TenantID   string    `gorm:"primaryKey;type:text" json:"tenant_id"`
	ConfigYAML string    `gorm:"type:text;not null;default:'{}'" json:"config_yaml"`
	UpdatedBy  *string   `gorm:"type:text" json:"updated_by"`
	UpdatedAt  time.Time `json:"updated_at"`
	CreatedAt  time.Time `json:"created_at"`
}

func (TenantConfig) TableName() string { return "tenant_configs" }
