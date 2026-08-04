package models

import "time"

const (
	GitCapabilityIdentificationClean    = "clean"
	GitCapabilityIdentificationWarning  = "warning"
	GitCapabilityIdentificationPolluted = "polluted"
	GitCapabilityIdentificationUnknown  = "unknown"
)

// GitCapabilityRepository records the stable repository-level result of the
// first capability discovery pass. CapabilityItem keeps the locked per-file
// type identity; this row lets an unknown repository remain observable even
// when discovery cannot create an item.
type GitCapabilityRepository struct {
	ID                   string     `gorm:"column:id;primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	GitServerID          string     `gorm:"column:git_server_id;type:varchar(64);not null;uniqueIndex:uq_git_capability_repositories_identity,priority:1" json:"git_server_id"`
	GitRepoID            int64      `gorm:"column:git_repo_id;not null;uniqueIndex:uq_git_capability_repositories_identity,priority:2" json:"git_repo_id"`
	RepositoryID         string     `gorm:"column:repository_id;type:uuid;not null;uniqueIndex" json:"repository_id"`
	RegistryID           string     `gorm:"column:registry_id;type:uuid;not null;uniqueIndex" json:"registry_id"`
	FullName             string     `gorm:"column:full_name;type:text;not null" json:"full_name"`
	RepoKind             string     `gorm:"column:repo_kind;type:varchar(32);not null;default:'standalone'" json:"repo_kind"`
	IdentificationStatus string     `gorm:"column:identification_status;type:varchar(32);not null;default:'unknown'" json:"identification_status"`
	Visibility           string     `gorm:"column:visibility;type:varchar(16);not null;default:'public'" json:"visibility"`
	GitRemoteURL         string     `gorm:"column:git_remote_url;type:text;not null" json:"git_remote_url"`
	DefaultBranch        string     `gorm:"column:default_branch;type:text;not null" json:"default_branch"`
	LastSyncedCommit     string     `gorm:"column:last_synced_commit;type:varchar(40);not null;default:''" json:"last_synced_commit"`
	LastSyncedAt         *time.Time `gorm:"column:last_synced_at" json:"last_synced_at,omitempty"`
	LastError            string     `gorm:"column:last_error;type:text;not null;default:''" json:"last_error,omitempty"`
	CreatedBy            string     `gorm:"column:created_by;type:varchar(191);not null" json:"created_by"`
	CreatedAt            time.Time  `gorm:"column:created_at;not null;default:now()" json:"created_at"`
	UpdatedAt            time.Time  `gorm:"column:updated_at;not null;default:now()" json:"updated_at"`
}

func (GitCapabilityRepository) TableName() string { return "git_capability_repositories" }
