package models

import "time"

// UserCredentials records the provider PAT minted for a user's personal namespace
// (Phase E3c extension — personal space).
//
// Each row corresponds to one user's active PAT on one git_server under one
// tenant. The plaintext token is NEVER persisted — only token_encrypted
// (AES-GCM ciphertext, base64) and token_sha256 (for leak detection /
// revocation lookups). Plaintext is returned to the caller exactly once at
// ProvisionUser time and each subsequent Rotate time.
//
// Provider-agnostic: column names use the "git_" prefix matching UserGitBinding
// and GitServerID — the concrete provider is determined by git_servers.kind.
type UserCredentials struct {
	UserSubjectID  string     `gorm:"primaryKey;type:varchar(191)"                                                               json:"user_subject_id"`
	TenantID       string     `gorm:"type:text;not null"                                                                          json:"tenant_id"`
	GitServerID    string     `gorm:"type:varchar(64);not null"                                                                   json:"git_server_id"`
	GitUsername    string     `gorm:"type:varchar(191);not null;uniqueIndex:uq_user_credentials_git_username,where:revoked_at IS NULL" json:"git_username"`
	GitUserID      int64      `gorm:"not null"                                                                                    json:"git_user_id"`
	GitTokenID     int64      `gorm:"not null"                                                                                    json:"git_token_id"`
	TokenEncrypted string     `gorm:"type:text;not null"                                                                          json:"-"`
	TokenSHA256    string     `gorm:"type:char(64);not null;index:idx_user_credentials_sha256"                                    json:"-"`
	CreatedAt      time.Time  `gorm:"type:timestamptz;not null;default:now()"                                                     json:"created_at"`
	RotatedAt      *time.Time `gorm:"type:timestamptz"                                                                            json:"rotated_at,omitempty"`
	RevokedAt      *time.Time `gorm:"type:timestamptz"                                                                            json:"revoked_at,omitempty"`
}

// TableName pins the table name.
func (UserCredentials) TableName() string { return "user_credentials" }
