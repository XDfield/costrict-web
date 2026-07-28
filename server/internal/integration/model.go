package integration

import "time"

// IntegrationEvent records every accepted envelope for idempotency: retries
// from the sender carry the same (source, event_id) pair and are ACKed
// without re-delivery.
type IntegrationEvent struct {
	ID        string    `gorm:"primaryKey;size:36" json:"id"`
	Source    string    `gorm:"size:64;not null;uniqueIndex:idx_integration_events_source_event" json:"source"`
	EventID   string    `gorm:"size:64;not null;uniqueIndex:idx_integration_events_source_event" json:"event_id"`
	EventType string    `gorm:"size:128;not null" json:"event_type"`
	CreatedAt time.Time `json:"created_at"`
}

func (IntegrationEvent) TableName() string {
	return "integration_events"
}
