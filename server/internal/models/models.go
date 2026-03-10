package models

import (
	"time"
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
