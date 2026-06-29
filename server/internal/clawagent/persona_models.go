package clawagent

import "time"

// Persona represents a user's agent persona (soul/identity/user context).
type Persona struct {
	ID              string    `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	UserID          string    `gorm:"size:255;not null;index"`
	Name            string    `gorm:"size:255;not null"`
	SoulContent     string    `gorm:"type:text;not null"`
	IdentityContent string    `gorm:"type:text"`
	UserContext     string    `gorm:"type:text"`
	IsDefault       bool      `gorm:"default:false"`
	CreatedAt       time.Time `gorm:"autoCreateTime"`
	UpdatedAt       time.Time `gorm:"autoUpdateTime"`
}

func (Persona) TableName() string {
	return "agent_personas"
}
