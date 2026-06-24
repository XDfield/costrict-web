package clawagent

import "time"

// SessionMeta stores business metadata for agent sessions.
// Used for freshness check, versioning, archive state, and compaction.
type SessionMeta struct {
	SessionID     string     `gorm:"size:255;primaryKey"`
	UserID        string     `gorm:"size:255;not null;index"`
	BaseKey       string     `gorm:"size:255;not null"`
	Version       int        `gorm:"not null;default:1"`
	ResetType     string     `gorm:"size:20;not null"`
	LastMessageAt time.Time  `gorm:"not null;autoUpdateTime"`
	MessageCount  int        `gorm:"not null;default:0"`
	TokenEstimate int        `gorm:"not null;default:0"`
	EventData     string     `gorm:"type:text"` // JSON-serialized EventContext for pending events
	IsArchived    bool       `gorm:"not null;default:false;index:,name:idx_session_meta_base_active,where:is_archived = FALSE"`
	ArchivedAt    *time.Time
	CreatedAt     time.Time  `gorm:"autoCreateTime"`
}

func (SessionMeta) TableName() string {
	return "agent_session_meta"
}
