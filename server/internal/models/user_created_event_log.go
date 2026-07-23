package models

import "time"

// user_created_event_log status 词汇（Git Ownership Refactor Phase 3）。
const (
	UserCreatedEventStatusProcessed  = "processed"
	UserCreatedEventStatusSoftSkipped = "soft_skipped"
	UserCreatedEventStatusFailed     = "failed"
)

// UserCreatedEventLog 记录 server 端处理过的每个 user.created 事件，作为
// 幂等去重的事实来源。cs-user outbox 是 at-least-once 投递，重复 event_id
// 在本表里命中后直接返回 2xx，不再调 ProvisionUser。
//
// Mirrors migration 20260722160000_create_user_created_event_log.sql 1:1.
type UserCreatedEventLog struct {
	EventID      string    `gorm:"column:event_id;primaryKey;type:uuid" json:"event_id"`
	EventType    string    `gorm:"column:event_type;type:varchar(64);not null;default:user.created" json:"event_type"`
	SubjectID    string    `gorm:"column:subject_id;type:text;not null" json:"subject_id"`
	TenantID     string    `gorm:"column:tenant_id;type:text;not null;default:default" json:"tenant_id"`
	Status       string    `gorm:"column:status;type:varchar(32);not null" json:"status"`
	ErrorMessage *string   `gorm:"column:error_message;type:text" json:"error_message,omitempty"`
	ProcessedAt  time.Time `gorm:"column:processed_at;type:timestamptz;not null;default:now()" json:"processed_at"`
}

// TableName pins the table name.
func (UserCreatedEventLog) TableName() string { return "user_created_event_log" }
