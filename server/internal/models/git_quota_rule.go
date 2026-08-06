package models

import (
	"time"
)

// GitQuotaRule is one row of the push-quota matrix that costrict-web pushes to
// a CoStrict Gitea fork (Gitea fork integration FI-4).
//
// Mirrors migration 20260806000200_create_git_quota_rules.sql 1:1, and through
// it the fork's own modules/costrict/quota.Rule — the wire shape accepted by
// POST /api/internal/costrict/quota-invalidate. Fields the fork cannot enforce
// are deliberately absent.
//
// Semantics inherited from the fork, not chosen here:
//
//   - Repo == "" is the owner-level default. The fork looks up owner/repo
//     first, then owner/"", then falls back to its own app.ini defaults, and
//     the first match wins WHOLE — there is no field-level merge, so a rule
//     with RepoQuotaMB == 0 means "unlimited", not "inherit".
//   - 0 means unlimited for both limits.
//   - Owner / Repo are matched against Gitea's Repository.OwnerName and .Name
//     with exact string equality, so their case must match Gitea exactly or the
//     override silently never fires.
//
// Like every other table in the Git domain this model is intentionally NOT
// registered in any AutoMigrate list; the goose migration is the only schema
// authority.
type GitQuotaRule struct {
	GitServerID   string    `gorm:"column:git_server_id;primaryKey;type:varchar(64)" json:"git_server_id"`
	Owner         string    `gorm:"column:owner;primaryKey;type:varchar(255)" json:"owner"`
	Repo          string    `gorm:"column:repo;primaryKey;type:varchar(255);not null;default:''" json:"repo"`
	MaxFileSizeMB int64     `gorm:"column:max_file_size_mb;not null;default:0" json:"max_file_size_mb"`
	RepoQuotaMB   int64     `gorm:"column:repo_quota_mb;not null;default:0" json:"repo_quota_mb"`
	UpdatedBy     string    `gorm:"column:updated_by;type:varchar(191);not null;default:''" json:"updated_by"`
	CreatedAt     time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName pins the table name.
func (GitQuotaRule) TableName() string { return "git_quota_rules" }
