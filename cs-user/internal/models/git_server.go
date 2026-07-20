package models

import (
	"time"
)

// GitServerKindVocab is the set of supported Git server kinds. v1 supports
// only "gitea"; the enum is here so future kinds (gitlab, gitea-enterprise,
// ...) land as a single-source change.
const (
	GitServerKindGitea = "gitea"
)

// GitServer is the per-tenant Git server configuration row (Phase E3b.1.1).
// One row per server_id; tenants.git_server_id binds each tenant to exactly
// one row (1:1 unique).
//
// Schema (see migration 20260721160000_create_git_servers.sql):
//
//   - server_id is the application-layer stable PK (e.g. "gs-template-..." or
//     "gs-<uuid>"). Matches tenant_id's TEXT-style convention.
//   - config holds a JSON blob: {"admin_token": "..."}. Vault integration is
//     deferred (TODO marker in migration); for now the token lives in JSONB.
//     Stored as TEXT and (un)marshalled at the app layer, matching the
//     tenant_config.config_yaml pattern.
//   - is_template marks the bootstrap row cloned for new tenants. A partial
//     unique index (see migration) guarantees at most one template row.
//   - enabled = false makes the resolver refuse to return the row; operators
//     use this to drain a server before decommission.
type GitServer struct {
	ServerID    string    `gorm:"primaryKey;type:varchar(64)" json:"server_id"`
	Kind        string    `gorm:"type:varchar(32);not null" json:"kind"`
	Endpoint    string    `gorm:"type:text;not null" json:"endpoint"`
	DisplayName string    `gorm:"type:text;not null" json:"display_name"`
	Config      string    `gorm:"type:jsonb;not null;default:'{}'" json:"config"`
	IsTemplate  bool      `gorm:"not null;default:false" json:"is_template"`
	Enabled     bool      `gorm:"not null;default:true" json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (GitServer) TableName() string { return "git_servers" }
