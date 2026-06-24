package clawagent

import "time"

// Memory represents a single TEXT record per user for agent memory.
type Memory struct {
	UserID    string    `gorm:"size:255;primaryKey"`
	Content   string    `gorm:"type:text;not null;default:''"`
	UpdatedAt time.Time `gorm:"autoUpdateTime"`
}

func (Memory) TableName() string {
	return "agent_memories"
}
