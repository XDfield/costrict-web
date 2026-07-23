package models

import (
	"time"
)

// GitServerKindVocab — supported Git server kinds. v1 only "gitea"; the enum
// is here so future kinds (gitlab, gitea-enterprise, ...) land as a
// single-source change.
const (
	GitServerKindGitea = "gitea"
)

// GitServer is the per-tenant Git server configuration row (Git Ownership
// Refactor Phase 1).
//
// Mirrors migration 20260722150000_create_git_servers.sql 1:1.
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

// TableName pins the table name.
func (GitServer) TableName() string { return "git_servers" }
