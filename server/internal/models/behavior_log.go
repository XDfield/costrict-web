package models

import (
	"time"

	"gorm.io/datatypes"
)

// ActionType defines the type of user action
type ActionType string

const (
	ActionView     ActionType = "view"
	ActionClick    ActionType = "click"
	ActionInstall  ActionType = "install"
	ActionUse      ActionType = "use"
	ActionSuccess  ActionType = "success"
	ActionFail     ActionType = "fail"
	ActionFeedback ActionType = "feedback"
	ActionIgnore   ActionType = "ignore"
)

// ContextType defines the context in which an action occurred
type ContextType string

const (
	ContextSearch        ContextType = "search_query"
	ContextRecommend     ContextType = "recommendation"
	ContextDirectAccess  ContextType = "direct_access"
	ContextBrowse        ContextType = "browse"
)

// BehaviorLog tracks user interactions with capability items
type BehaviorLog struct {
	ID         string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	UserID     string         `gorm:"type:varchar(191);index" json:"userId"`
	ItemID     string         `gorm:"type:uuid;index" json:"itemId"`
	RegistryID string         `gorm:"type:uuid;index" json:"registryId"`
	ActionType ActionType     `gorm:"type:varchar(32);not null;index" json:"actionType"`
	Context    ContextType    `gorm:"type:varchar(32)" json:"context"`
	SearchQuery string        `json:"searchQuery"`
	SessionID  string         `gorm:"type:varchar(191);index" json:"sessionId"`
	Metadata   datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"metadata" swaggertype:"object"`
	DurationMs int64          `json:"durationMs"`
	Rating     int            `json:"rating"` // 1-5 for feedback
	Feedback   string         `gorm:"type:text" json:"feedback"`
	CreatedAt  time.Time      `gorm:"autoCreateTime;index" json:"createdAt"`

	// Relations
	Item     *CapabilityItem     `gorm:"foreignKey:ItemID" json:"item,omitempty"`
	Registry *CapabilityRegistry `gorm:"foreignKey:RegistryID" json:"registry,omitempty"`
}

// TableName specifies the table name for BehaviorLog
func (BehaviorLog) TableName() string {
	return "behavior_logs"
}

// BehaviorMetadata contains additional context for behavior logs
type BehaviorMetadata struct {
	ClientIP     string            `json:"clientIp,omitempty"`
	UserAgent    string            `json:"userAgent,omitempty"`
	Source       string            `json:"source,omitempty"` // web, cli, api
	ErrorType    string            `json:"errorType,omitempty"`
	ErrorMessage string            `json:"errorMessage,omitempty"`
	Resolution   string            `json:"resolution,omitempty"`
	Tags         []string          `json:"tags,omitempty"`
	Extra        map[string]string `json:"extra,omitempty"`
}

// UserBehaviorSummary aggregates user behavior statistics
type UserBehaviorSummary struct {
	UserID          string   `json:"userId"`
	TotalViews      int64    `json:"totalViews"`
	TotalInstalls   int64    `json:"totalInstalls"`
	TotalUses       int64    `json:"totalUses"`
	SuccessRate     float64  `json:"successRate"`
	FavoriteTypes   []string `json:"favoriteTypes"`
	FavoriteCategories []string `json:"favoriteCategories"`
}
