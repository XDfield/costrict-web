// Package models defines cs-user's GORM entity structs.
//
// These mirror the production schema in migrations/*.sql 1:1 (table + column
// names + indexes). The structs are extracted verbatim from
// server/internal/models/models.go so the strangler-fig cutover (P0-7 / P0-8)
// can swap the data owner without changing the row shape.
package models

import (
	"time"

	"gorm.io/gorm"
)

// User represents a local user record synchronized from Casdoor.
//
// SubjectID is the stable application-level identifier used across business
// tables and request context; the auto-increment ID is internal-only.
type User struct {
	ID                 uint           `gorm:"primaryKey;autoIncrement" json:"id"`
	TenantID           string         `gorm:"type:text;size:191;not null;default:default;index:idx_users_tenant_id" json:"tenant_id"`
	SubjectID          string         `gorm:"uniqueIndex:idx_user_subject_id;not null;size:191" json:"subject_id"`
	Username           string         `gorm:"uniqueIndex:idx_user_username;not null;size:191" json:"username"`
	DisplayName        *string        `gorm:"size:191" json:"display_name"`
	Email              *string        `gorm:"index:idx_user_email;size:191" json:"email"`
	Phone              *string        `gorm:"index:idx_user_phone;size:64" json:"phone"`
	AvatarURL          *string        `gorm:"type:text" json:"avatar_url"`
	AuthProvider       *string        `gorm:"index:idx_user_auth_provider;size:64" json:"auth_provider"`
	ExternalKey        *string        `gorm:"uniqueIndex:idx_user_external_key;size:255" json:"external_key"`
	ProviderUserID     *string        `gorm:"index:idx_user_provider_user_id;size:191" json:"provider_user_id"`
	CasdoorID          *string        `gorm:"index:idx_user_casdoor_id;size:191" json:"casdoor_id"`
	CasdoorUniversalID *string        `gorm:"index:idx_user_casdoor_universal_id;size:191" json:"casdoor_universal_id"`
	CasdoorSub         *string        `gorm:"index:idx_user_casdoor_sub;size:191" json:"casdoor_sub"`
	Organization       *string        `gorm:"index:idx_user_organization;size:191" json:"organization"`
	IsActive           bool           `gorm:"not null;default:true" json:"is_active"`
	Status             string         `gorm:"size:32;not null;default:'active';index" json:"status"`
	// ProfileCompletedAt is NULL until the user finishes the first-time
	// registration flow (custom username + display_name per
	// REGISTRATION_PROFILE_DESIGN). The server's RequireProfileComplete
	// middleware consults this column (via cs-user RPC) to gate API access.
	// Existing rows are backfilled to created_at by the migration that
	// introduces the column, so only users created post-migration can be
	// gated.
	ProfileCompletedAt *time.Time     `json:"profile_completed_at"`
	LastLoginAt        *time.Time     `json:"last_login_at"`
	LastSyncAt         *time.Time     `json:"last_sync_at"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	DeletedAt          gorm.DeletedAt `gorm:"index" json:"-"`
}

// TableName pins the table name so GORM doesn't pluralise / pluralize oddly.
func (User) TableName() string { return "users" }

// UserAuthIdentity stores one external login identity bound to a local user.
//
// ExternalKey (issuer + provider + external subject) is the unique stable
// handle across IdPs; IsPrimary disambiguates the display identity; the
// soft-delete + ExplicitlyUnbound combo supports "claim a previously unbound
// identity" semantics in bind/transfer flows.
type UserAuthIdentity struct {
	ID                uint           `gorm:"primaryKey;autoIncrement" json:"id"`
	TenantID          string         `gorm:"type:text;size:191;not null;default:default;index:idx_user_auth_identities_tenant_id" json:"tenant_id"`
	UserSubjectID     string         `gorm:"index:idx_user_auth_identities_user_subject_id;not null;size:191" json:"user_subject_id"`
	Provider          string         `gorm:"size:64;not null" json:"provider"`
	Issuer            *string        `gorm:"size:255" json:"issuer"`
	ExternalKey       string         `gorm:"uniqueIndex:idx_user_auth_identities_external_key;not null;size:255" json:"external_key"`
	ExternalSubject   *string        `gorm:"size:191" json:"external_subject"`
	ExternalUserID    *string        `gorm:"size:191" json:"external_user_id"`
	ProviderUserID    *string        `gorm:"size:191" json:"provider_user_id"`
	DisplayName       *string        `gorm:"size:191" json:"display_name"`
	Email             *string        `gorm:"size:191" json:"email"`
	Phone             *string        `gorm:"size:64" json:"phone"`
	AvatarURL         *string        `gorm:"type:text" json:"avatar_url"`
	Organization      *string        `gorm:"size:191" json:"organization"`
	IsPrimary         bool           `gorm:"not null;default:false" json:"is_primary"`
	ExplicitlyUnbound bool           `gorm:"not null;default:false" json:"explicitly_unbound"`
	LastLoginAt       *time.Time     `json:"last_login_at"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
	DeletedAt         gorm.DeletedAt `gorm:"index" json:"-"`
}

func (UserAuthIdentity) TableName() string { return "user_auth_identities" }
