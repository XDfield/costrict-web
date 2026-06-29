package clawagent

import "time"

// Provider represents a user-configured LLM provider.
type Provider struct {
	ID              uint      `gorm:"primaryKey"`
	UserID          string    `gorm:"size:255;not null;index"`
	Name            string    `gorm:"size:255;not null"`
	ProviderType    string    `gorm:"size:50;not null"`
	APIKeyEncrypted string    `gorm:"type:text"`
	BaseURL         string    `gorm:"type:text"`
	ModelName       string    `gorm:"size:255;not null"`
	Models          string    `gorm:"type:text"`
	IsDefault       bool      `gorm:"default:false"`
	CreatedAt       time.Time `gorm:"autoCreateTime"`
	UpdatedAt       time.Time `gorm:"autoUpdateTime"`
}

func (Provider) TableName() string {
	return "agent_providers"
}
