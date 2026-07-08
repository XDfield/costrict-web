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

// AnonymousUserID is the sentinel user id stored for behavior logs written
// without an authenticated user. Trust/ranking aggregations MUST exclude it so
// unauthenticated (or historically injected) rows can never influence stats,
// trending or recommendations. See SRC-2026-4791.
const AnonymousUserID = "anonymous"

var validActionTypes = map[ActionType]bool{
	ActionView:     true,
	ActionClick:    true,
	ActionInstall:  true,
	ActionUse:      true,
	ActionSuccess:  true,
	ActionFail:     true,
	ActionFeedback: true,
	ActionIgnore:   true,
}

// IsValid reports whether the action type is one of the known enum values.
// Rejecting unknown strings keeps arbitrary/polluting action types out of the
// behavior log.
func (a ActionType) IsValid() bool {
	return validActionTypes[a]
}

// RequiresAuth reports whether writing this action type must be attributed to an
// authenticated user. Trust/counting signals (install, use, success, fail,
// feedback, ignore) drive ranking, success rate, ratings and recommendations,
// so they must never be written anonymously. Only the low-trust browsing
// signals (view/click) may be recorded anonymously — and those are still
// excluded from stats aggregation.
func (a ActionType) RequiresAuth() bool {
	switch a {
	case ActionView, ActionClick:
		return false
	default:
		return true
	}
}

// ContextType defines the context in which an action occurred
type ContextType string

const (
	ContextSearch       ContextType = "search_query"
	ContextRecommend    ContextType = "recommendation"
	ContextDirectAccess ContextType = "direct_access"
	ContextBrowse       ContextType = "browse"
)

// BehaviorLog tracks user interactions with capability items
type BehaviorLog struct {
	ID          string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	UserID      string         `gorm:"type:varchar(191);index" json:"userId"`
	ItemID      string         `gorm:"type:uuid;index;default:null" json:"itemId"`
	RegistryID  string         `gorm:"type:uuid;index;default:null" json:"registryId"`
	ActionType  ActionType     `gorm:"type:varchar(32);not null;index" json:"actionType"`
	Context     ContextType    `gorm:"type:varchar(32)" json:"context"`
	SearchQuery string         `json:"searchQuery"`
	SessionID   string         `gorm:"type:varchar(191);index" json:"sessionId"`
	Metadata    datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"metadata" swaggertype:"object"`
	DurationMs  int64          `json:"durationMs"`
	Rating      int            `json:"rating"` // 1-5 for feedback
	Feedback    string         `gorm:"type:text" json:"feedback"`
	CreatedAt   time.Time      `gorm:"autoCreateTime;index" json:"createdAt"`

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
	UserID             string   `json:"userId"`
	TotalViews         int64    `json:"totalViews"`
	TotalInstalls      int64    `json:"totalInstalls"`
	TotalUses          int64    `json:"totalUses"`
	SuccessRate        float64  `json:"successRate"`
	FavoriteTypes      []string `json:"favoriteTypes"`
	FavoriteCategories []string `json:"favoriteCategories"`
}
