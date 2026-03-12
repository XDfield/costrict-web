package models

import (
	"time"

	"gorm.io/datatypes"
)

// ExperienceType defines the type of experience candidate
type ExperienceType string

const (
	ExperienceError      ExperienceType = "error"
	ExperienceLearning   ExperienceType = "learning"
	ExperienceFeature    ExperienceType = "feature_request"
	ExperiencePractice   ExperienceType = "best_practice"
	ExperienceRule       ExperienceType = "behavior_rule"
)

// CandidateStatus defines the status of an experience candidate
type CandidateStatus string

const (
	StatusPending   CandidateStatus = "pending"
	StatusApproved  CandidateStatus = "approved"
	StatusRejected  CandidateStatus = "rejected"
	StatusPromoted  CandidateStatus = "promoted"
)

// SourceType defines where the experience came from
type SourceType string

const (
	SourceBehavior  SourceType = "behavior_log"
	SourceManual    SourceType = "manual"
	SourceAutoDetect SourceType = "auto_detect"
)

// PromotionType defines how the experience was promoted
type PromotionType string

const (
	PromotionBestPractice PromotionType = "best_practice"
	PromotionBehaviorRule PromotionType = "behavior_rule"
	PromotionNewWorkflow  PromotionType = "new_workflow"
	PromotionNewSkill     PromotionType = "new_skill"
)

// ExperienceCandidate represents a candidate for promotion to an experience
type ExperienceCandidate struct {
	ID            string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	ItemID        string         `gorm:"type:uuid;index" json:"itemId"`
	Type          ExperienceType `gorm:"type:varchar(32);not null;index" json:"type"`
	Title         string         `gorm:"type:varchar(255);not null" json:"title"`
	Description   string         `gorm:"type:text" json:"description"`
	Context       string         `gorm:"type:text" json:"context"`       // What happened
	Resolution    string         `gorm:"type:text" json:"resolution"`    // How it was resolved / what was learned
	SourceType    SourceType     `gorm:"type:varchar(32);not null" json:"sourceType"`
	SourceLogID   string         `gorm:"type:uuid" json:"sourceLogId"`  // Reference to behavior log if applicable
	Frequency     int            `gorm:"default:1" json:"frequency"`     // How many times this pattern occurred
	ImpactScore   float64        `gorm:"default:0" json:"impactScore"`   // Computed impact score
	Status        CandidateStatus `gorm:"type:varchar(32);default:'pending';index" json:"status"`
	PromotionType PromotionType  `gorm:"type:varchar(32)" json:"promotionType"`
	PromotedAt    *time.Time     `json:"promotedAt"`
	PromotedBy    string         `gorm:"type:varchar(191)" json:"promotedBy"`
	CreatedAt     time.Time      `gorm:"autoCreateTime" json:"createdAt"`
	UpdatedAt     time.Time      `gorm:"autoUpdateTime" json:"updatedAt"`

	// Relations
	Item *CapabilityItem `gorm:"foreignKey:ItemID" json:"item,omitempty"`
}

// TableName specifies the table name for ExperienceCandidate
func (ExperienceCandidate) TableName() string {
	return "experience_candidates"
}

// ExperiencePromotion records the promotion of an experience to an item's metadata
type ExperiencePromotion struct {
	ID             string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	CandidateID    string         `gorm:"type:uuid;not null;index" json:"candidateId"`
	ItemID         string         `gorm:"type:uuid;not null;index" json:"itemId"`
	PromotionType  PromotionType  `gorm:"type:varchar(32);not null" json:"promotionType"`
	PromotedBy     string         `gorm:"type:varchar(191);not null" json:"promotedBy"`
	MetadataBefore datatypes.JSON `gorm:"type:jsonb" json:"metadataBefore" swaggertype:"object"`
	MetadataAfter  datatypes.JSON `gorm:"type:jsonb" json:"metadataAfter" swaggertype:"object"`
	CreatedAt      time.Time      `gorm:"autoCreateTime" json:"createdAt"`

	// Relations
	Candidate *ExperienceCandidate `gorm:"foreignKey:CandidateID" json:"candidate,omitempty"`
	Item      *CapabilityItem      `gorm:"foreignKey:ItemID" json:"item,omitempty"`
}

// TableName specifies the table name for ExperiencePromotion
func (ExperiencePromotion) TableName() string {
	return "experience_promotions"
}

// ErrorRecord represents an error pattern within item metadata
type ErrorRecord struct {
	Timestamp  time.Time `json:"timestamp"`
	Context    string    `json:"context"`
	Message    string    `json:"message"`
	Resolution string    `json:"resolution,omitempty"`
}

// LearningRecord represents a learning insight within item metadata
type LearningRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Insight   string    `json:"insight"`
	Impact    string    `json:"impact"`
}

// FeatureRequest represents a feature request within item metadata
type FeatureRequest struct {
	Timestamp time.Time `json:"timestamp"`
	Request   string    `json:"request"`
	Status    string    `json:"status"` // pending, in_progress, completed, rejected
}

// BestPractice represents a best practice within item metadata
type BestPractice struct {
	Practice    string    `json:"practice"`
	Score       float64   `json:"score"`
	PromotedAt  time.Time `json:"promotedAt"`
	SourceCount int       `json:"sourceCount"`
}

// BehaviorRule represents a behavior rule within item metadata
type BehaviorRule struct {
	Rule    string `json:"rule"`
	Trigger string `json:"trigger"`
	Action  string `json:"action"`
}
