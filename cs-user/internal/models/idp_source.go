// models/idp_source.go — idp_sources table GORM model.
//
// E2.1: Model for per-tenant identity provider sources.
// See migrations/20260722100000_create_idp_sources.sql for schema.

package models

import (
	"encoding/json"
	"time"
)

// IdPSource represents a tenant-level identity provider configuration.
// This enables E2 tenant-level IdP integration where each tenant can have
// their own set of enabled identity providers.
type IdPSource struct {
	ID        int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	TenantID  string    `gorm:"column:tenant_id;type:varchar(64);not null" json:"tenant_id"`
	Provider  string    `gorm:"column:provider;type:varchar(64);not null" json:"provider"`
	Config    string    `gorm:"column:config;type:json;not null" json:"config"` // JSON-encoded provider config
	Scope     string    `gorm:"column:scope;type:varchar(64);not null;default:tenant-specific" json:"scope"`
	Enabled   bool      `gorm:"column:enabled;type:bool;not null" json:"enabled"`
	Priority  int       `gorm:"column:priority;type:int;not null;default:0" json:"priority"`
	CreatedAt time.Time `gorm:"column:created_at;type:datetime(3)" json:"created_at,omitempty"`
	UpdatedAt time.Time `gorm:"column:updated_at;type:datetime(3)" json:"updated_at,omitempty"`
	CreatedBy string    `gorm:"column:created_by;type:varchar(64)" json:"created_by,omitempty"`
	UpdatedBy string    `gorm:"column:updated_by;type:varchar(64)" json:"updated_by,omitempty"`
}

// TableName specifies the table name for GORM.
func (IdPSource) TableName() string {
	return "idp_sources"
}

// ParsedConfig is a helper type for the typed view of the Config JSON field.
// Provider-specific configs will have different schemas:
//   - OAuth/OIDC: client_id, client_secret, authorization_url, token_url, userinfo_url
//   - LDAP: host, port, bind_dn, bind_password, base_dn, user_filter
type ParsedConfig map[string]interface{}

// GetConfig parses the Config JSON field into a map.
func (i *IdPSource) GetConfig() (ParsedConfig, error) {
	var cfg ParsedConfig
	if i.Config == "" || i.Config == "{}" {
		return cfg, nil
	}
	err := json.Unmarshal([]byte(i.Config), &cfg)
	return cfg, err
}

// SetConfig serializes a map into the Config JSON field.
func (i *IdPSource) SetConfig(cfg ParsedConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	i.Config = string(data)
	return nil
}
