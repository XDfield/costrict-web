package models

import "time"

// TeamNamespace is the per-team Gitea namespace binding (Phase E3c).
//
// Each row records: this logical team_id (cs-user team) maps to that Gitea
// org (team_ns_org = "t-<team_short>") on this git_server. The lifecycle is
// managed by teamns.Service: created on first team sync, archived on PatchTeam
// status=archived, dissolved (retained for 90 days) on DissolveTeam.
//
// Mirrors migration 20260722000000_create_team_ns_and_bot_credentials.sql 1:1.
type TeamNamespace struct {
	TeamID           string     `gorm:"primaryKey;type:varchar(191)"                                   json:"team_id"`
	TenantID         string     `gorm:"type:text;not null"                                             json:"tenant_id"`
	TeamDisplayName  string     `gorm:"type:varchar(191);not null"                                     json:"team_display_name"`
	TeamNSOrg        string     `gorm:"type:varchar(64);not null;uniqueIndex:uq_team_ns_org"           json:"team_ns_org"`
	TeamShort        string     `gorm:"type:varchar(32);not null"                                      json:"team_short"`
	GitServerID      string     `gorm:"type:varchar(64);not null"                                      json:"git_server_id"`
	Status           string     `gorm:"type:varchar(32);not null;default:active"                       json:"status"`
	DissolvedAt      *time.Time `gorm:"type:timestamptz"                                               json:"dissolved_at,omitempty"`
	DissolutionReason *string  `gorm:"type:varchar(64)"                                               json:"dissolution_reason,omitempty"`
	RetentionUntil   *time.Time `gorm:"type:timestamptz"                                               json:"retention_until,omitempty"`
	CreatedAt        time.Time  `gorm:"type:timestamptz;not null;default:now()"                        json:"created_at"`
	UpdatedAt        time.Time  `gorm:"type:timestamptz;not null;default:now()"                        json:"updated_at"`
}

// TableName pins the table name.
func (TeamNamespace) TableName() string { return "team_ns" }

// TeamBotCredentials is the per-team Gitea bot account + PAT (Phase E3c).
//
// Mirrors migration 20260722000000_create_team_ns_and_bot_credentials.sql.
// The plaintext token is NEVER persisted — only token_encrypted (AES-GCM
// ciphertext, base64) and token_sha256 (for leak detection / revocation
// lookups). Plaintext is returned to the caller exactly once at Provision /
// Rotate time.
type TeamBotCredentials struct {
	TeamID         string     `gorm:"primaryKey;type:varchar(191)"                                                       json:"team_id"`
	TenantID       string     `gorm:"type:text;not null"                                                                 json:"tenant_id"`
	GitServerID    string     `gorm:"type:varchar(64);not null"                                                          json:"git_server_id"`
	GiteaUsername  string     `gorm:"type:varchar(191);not null;uniqueIndex:uq_team_bot_credentials_gitea_username,where:revoked_at IS NULL" json:"gitea_username"`
	GiteaUserID    int64      `gorm:"not null"                                                                           json:"gitea_user_id"`
	GiteaTokenID   int64      `gorm:"not null"                                                                           json:"gitea_token_id"`
	TokenEncrypted string     `gorm:"type:text;not null"                                                                 json:"-"`
	TokenSHA256    string     `gorm:"type:char(64);not null;index:idx_team_bot_credentials_sha256"                       json:"-"`
	CreatedAt      time.Time  `gorm:"type:timestamptz;not null;default:now()"                                            json:"created_at"`
	RotatedAt      *time.Time `gorm:"type:timestamptz"                                                                   json:"rotated_at,omitempty"`
	RevokedAt      *time.Time `gorm:"type:timestamptz"                                                                   json:"revoked_at,omitempty"`
}

// TableName pins the table name.
func (TeamBotCredentials) TableName() string { return "team_bot_credentials" }
