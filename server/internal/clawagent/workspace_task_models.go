package clawagent

import "time"

const (
	TaskStatusQueued    = "queued"
	TaskStatusRunning   = "running"
	TaskStatusSucceeded = "succeeded"
	TaskStatusFailed    = "failed"
	TaskStatusTimedOut  = "timed_out"
	TaskStatusCancelled = "cancelled"
	TaskStatusLost      = "lost"

	DeliveryStatusPending       = "pending"
	DeliveryStatusDelivered     = "delivered"
	DeliveryStatusFailed        = "failed"
	DeliveryStatusNotApplicable = "not_applicable"
)

// WorkspaceTask records a workspace delegation task's lifecycle.
type WorkspaceTask struct {
	ID                  string    `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	TaskID              string    `gorm:"size:64;not null;uniqueIndex"`
	UserID              string    `gorm:"size:255;not null;index"`
	WorkspaceID         string    `gorm:"size:255;not null;index"`
	DeviceID            string    `gorm:"size:255;not null;index"`
	DirectoryPath       string    `gorm:"type:text"`
	Task                string    `gorm:"type:text;not null"`
	Skill               string    `gorm:"size:255"`
	AgentSessionBaseKey string    `gorm:"size:255;index"`
	ConversationID      string    `gorm:"size:255;index"`
	Status              string    `gorm:"size:20;not null;default:queued;index"`
	DeliveryStatus      string    `gorm:"size:20;not null;default:pending"`
	ProgressSummary     string    `gorm:"type:text"`
	Output              string    `gorm:"type:text"`
	Error               string    `gorm:"type:text"`
	AnnounceRetryCount  int       `gorm:"default:0"`
	StartedAt           *time.Time
	CompletedAt         *time.Time
	LastEventAt         *time.Time
	CreatedAt           time.Time `gorm:"autoCreateTime"`
}

func (WorkspaceTask) TableName() string {
	return "agent_workspace_tasks"
}
