package models

import (
	"time"
)

// Outbox status vocabulary (Git Ownership Refactor Phase 2).
//
// No DB-level CHECK constraint — app-level enum so new states can be
// appended without a migration.
const (
	UserEventStatusPending   = "pending"
	UserEventStatusDelivered = "delivered"
	UserEventStatusFailed    = "failed"
)

// UserEvent is one row in the cs-user outbox. Mirrors migration
// 20260722200000_create_user_events.sql 1:1.
//
// Lifecycle:
//
//	pending ── worker POST ── 2xx ──► delivered
//	   │
//	   └── 4xx / 5xx / network ──► pending (attempts++ + backoff) ──► failed (after max)
type UserEvent struct {
	EventID     string     `gorm:"primaryKey;type:uuid" json:"event_id"`
	EventType   string     `gorm:"type:varchar(64);not null" json:"event_type"`
	SubjectID   string     `gorm:"type:text;not null" json:"subject_id"`
	TenantID    string     `gorm:"type:text;not null;default:'default'" json:"tenant_id"`
	Payload     string     `gorm:"type:jsonb;not null" json:"payload"`
	Status      string     `gorm:"type:varchar(16);not null;default:pending" json:"status"`
	Attempts    int        `gorm:"not null;default:0" json:"attempts"`
	LastError   *string    `gorm:"type:text" json:"last_error,omitempty"`
	AvailableAt time.Time  `gorm:"type:timestamptz;not null;default:now()" json:"available_at"`
	DeliveredAt *time.Time `gorm:"type:timestamptz" json:"delivered_at,omitempty"`
	CreatedAt   time.Time  `gorm:"not null" json:"created_at"`
}

// TableName pins the table name.
func (UserEvent) TableName() string { return "user_events" }
