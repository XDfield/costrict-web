package models

import (
	"time"
	"gorm.io/datatypes"
)

// Organization represents a Casdoor organization
type Organization struct {
	ID          string    `gorm:"type:uuid;primaryKey" json:"id"`
	Name        string    `gorm:"type:varchar(255);not null" json:"name"`
	DisplayName string    `gorm:"type:varchar(255)" json:"displayName"`
	CreatedAt   time.Time `gorm:"autoCreateTime" json:"createdAt"`
	UpdatedAt   time.Time `gorm:"autoUpdateTime" json:"updatedAt"`
}

// SkillRepository represents a repository that contains skills/agents/commands
type SkillRepository struct {
	ID             string    `gorm:"type:uuid;primaryKey" json:"id"`
	Name           string    `gorm:"type:varchar(255);not null" json:"name"`
	Description    string    `gorm:"type:text" json:"description"`
	Visibility     string    `gorm:"type:varchar(50);default:'private'" json:"visibility"` // 'public' or 'private'
	OwnerID        string    `gorm:"type:varchar(191);not null" json:"ownerId"`
	OrganizationID *string   `gorm:"type:varchar(191)" json:"organizationId,omitempty"`
	GroupID        *string   `gorm:"type:varchar(191)" json:"groupId,omitempty"`
	CreatedAt      time.Time `gorm:"autoCreateTime" json:"createdAt"`
	UpdatedAt      time.Time `gorm:"autoUpdateTime" json:"updatedAt"`
}

// Skill represents a skill
type Skill struct {
	ID           string  `gorm:"type:uuid;primaryKey" json:"id"`
	Name         string  `gorm:"type:varchar(255);not null" json:"name"`
	Description  string  `gorm:"type:text" json:"description"`
	Version      string  `gorm:"type:varchar(50)" json:"version"`
	Author       string  `gorm:"type:varchar(191)" json:"author"`
	RepoID       string  `gorm:"type:uuid;not null" json:"repoId"`
	IsPublic     bool    `gorm:"default:false" json:"isPublic"`
	InstallCount int     `gorm:"default:0" json:"installCount"`
	Rating       float64 `gorm:"default:0.00" json:"rating"`
	CreatedAt    time.Time `gorm:"autoCreateTime" json:"createdAt"`
	UpdatedAt    time.Time `gorm:"autoUpdateTime" json:"updatedAt"`

	// Relationships
	Repository *SkillRepository `gorm:"foreignKey:RepoID" json:"repository,omitempty"`
}

// Agent represents an AI agent
type Agent struct {
	ID        string    `gorm:"type:uuid;primaryKey" json:"id"`
	Name      string    `gorm:"type:varchar(255);not null" json:"name"`
	Description string    `gorm:"type:text" json:"description"`
	Version   string    `gorm:"type:varchar(50)" json:"version"`
	Author    string    `gorm:"type:varchar(191)" json:"author"`
	RepoID    string    `gorm:"type:uuid;not null" json:"repoId"`
	IsPublic bool      `gorm:"default:false" json:"isPublic"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updatedAt"`

	// Relationships
	Repository *SkillRepository `gorm:"foreignKey:RepoID" json:"repository,omitempty"`
}

// Command represents a command
type Command struct {
	ID        string    `gorm:"type:uuid;primaryKey" json:"id"`
	Name      string    `gorm:"type:varchar(255);not null" json:"name"`
	Description string    `gorm:"type:text" json:"description"`
	RepoID    string    `gorm:"type:uuid;not null" json:"repoId"`
	Author    string    `gorm:"type:varchar(191)" json:"author"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updatedAt"`

	// Relationships
	Repository *SkillRepository `gorm:"foreignKey:RepoID" json:"repository,omitempty"`
}

// MCPServer represents an MCP server
type MCPServer struct {
	ID        string    `gorm:"type:uuid;primaryKey" json:"id"`
	Name      string    `gorm:"type:varchar(255);not null" json:"name"`
	Description string    `gorm:"type:text" json:"description"`
	RepoID    string    `gorm:"type:uuid;not null" json:"repoId"`
	Author    string    `gorm:"type:varchar(191)" json:"author"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updatedAt"`

	// Relationships
	Repository *SkillRepository `gorm:"foreignKey:RepoID" json:"repository,omitempty"`
}

// SkillRating represents a skill rating
type SkillRating struct {
	ID        string    `gorm:"type:uuid;primaryKey" json:"id"`
	SkillID   string    `gorm:"type:uuid;not null" json:"skillId"`
	UserID    string    `gorm:"type:varchar(191);not null" json:"userId"`
	Rating    int       `gorm:"not null" json:"rating"` // 1-5
	Comment    string    `gorm:"type:text" json:"comment"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt"`

	// Relationships
	Skill *Skill `gorm:"foreignKey:SkillID" json:"skill,omitempty"`
}

// UserPreference represents user preferences
type UserPreference struct {
	ID                  string  `gorm:"type:uuid;primaryKey" json:"id"`
	UserID              string  `gorm:"type:varchar(191);uniqueIndex" json:"userId"`
	DefaultRepositoryID *string `gorm:"type:uuid" json:"defaultRepositoryId,omitempty"`
	FavoriteSkills      string  `gorm:"type:text" json:"favoriteSkills"` // JSON array
	SkillPermissions    string  `gorm:"type:text" json:"skillPermissions"` // JSON object
	CreatedAt           time.Time `gorm:"autoCreateTime" json:"createdAt"`
	UpdatedAt           time.Time `gorm:"autoUpdateTime" json:"updatedAt"`

	// Relationships
	DefaultRepository *SkillRepository `gorm:"foreignKey:DefaultRepositoryID" json:"defaultRepository,omitempty"`
}

type SkillRegistry struct {
	ID          string     `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	Name        string     `gorm:"not null" json:"name"`
	Description string     `json:"description"`
	SourceType  string     `gorm:"not null;default:'internal'" json:"sourceType"`
	ExternalURL    string     `json:"externalUrl"`
	ExternalBranch string     `gorm:"default:'main'" json:"externalBranch"`
	SyncEnabled    bool       `gorm:"default:false" json:"syncEnabled"`
	SyncInterval   int        `gorm:"default:3600" json:"syncInterval"`
	LastSyncedAt   *time.Time `json:"lastSyncedAt"`
	LastSyncSHA    string     `json:"lastSyncSha"`
	Visibility string `gorm:"default:'org'" json:"visibility"`
	OrgID      string `json:"orgId"`
	OwnerID    string `gorm:"not null" json:"ownerId"`
	Items []SkillItem `gorm:"foreignKey:RegistryID" json:"items,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type SkillItem struct {
	ID         string `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	RegistryID string `gorm:"not null" json:"registryId"`
	Slug       string `gorm:"not null" json:"slug"`
	ItemType   string `gorm:"not null" json:"itemType"`
	Name       string `gorm:"not null" json:"name"`
	Description string `json:"description"`
	Category   string `json:"category"`
	Version    string `gorm:"default:'1.0.0'" json:"version"`
	Content    string `gorm:"type:text" json:"content"`
	Metadata   datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	SourcePath string `json:"sourcePath"`
	SourceSHA  string `json:"sourceSha"`
	Visibility string `json:"visibility"`
	InstallCount int    `gorm:"default:0" json:"installCount"`
	Status       string `gorm:"default:'active'" json:"status"`
	CreatedBy string `gorm:"not null" json:"createdBy"`
	UpdatedBy string `json:"updatedBy"`
	Registry  *SkillRegistry  `gorm:"foreignKey:RegistryID" json:"registry,omitempty"`
	Versions  []SkillVersion  `gorm:"foreignKey:ItemID" json:"versions,omitempty"`
	Artifacts []SkillArtifact `gorm:"foreignKey:ItemID" json:"artifacts,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type SkillVersion struct {
	ID        string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	ItemID    string         `gorm:"not null" json:"itemId"`
	Version   int            `gorm:"not null" json:"version"`
	Content   string         `gorm:"type:text;not null" json:"content"`
	Metadata  datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	CommitMsg string         `json:"commitMsg"`
	CreatedBy string         `gorm:"not null" json:"createdBy"`
	CreatedAt time.Time      `json:"createdAt"`
}

type SkillArtifact struct {
	ID              string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	ItemID          string    `gorm:"not null" json:"itemId"`
	Filename        string    `gorm:"not null" json:"filename"`
	FileSize        int64     `gorm:"not null" json:"fileSize"`
	ChecksumSHA256  string    `gorm:"not null" json:"checksumSha256"`
	MimeType        string    `json:"mimeType"`
	StorageBackend  string    `gorm:"default:'local'" json:"storageBackend"`
	StorageKey      string    `gorm:"not null" json:"storageKey"`
	ArtifactVersion string    `gorm:"not null" json:"artifactVersion"`
	IsLatest        bool      `gorm:"default:false" json:"isLatest"`
	DownloadCount   int       `gorm:"default:0" json:"downloadCount"`
	UploadedBy      string    `gorm:"not null" json:"uploadedBy"`
	CreatedAt       time.Time `json:"createdAt"`
}
