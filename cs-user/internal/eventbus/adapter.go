// Adapter from *eventbus.Outbox to user.EventPublisher (Git Ownership
// Refactor Phase 2).
//
// Why a separate file: importing eventbus from user would close an import
// cycle (user → eventbus → user), so the user package only sees the
// EventPublisher interface. main.go wires the concrete adapter defined here.

package eventbus

import (
	"context"

	"github.com/costrict/costrict-web/cs-user/internal/models"
)

// userCreatedEventType is the event_type written into user_events. Single
// source of truth — the consumer (server) greps for the same literal.
const userCreatedEventType = "user.created"

// UserPublisher adapts *Outbox to user.EventPublisher.
type UserPublisher struct {
	outbox *Outbox
}

// NewUserPublisher wraps an Outbox. Panics if nil — main.go is the only
// constructor and a nil outbox means a wiring bug, not a runtime state.
func NewUserPublisher(o *Outbox) *UserPublisher {
	if o == nil {
		panic("eventbus: NewUserPublisher(nil)")
	}
	return &UserPublisher{outbox: o}
}

// PublishUserCreated implements user.EventPublisher. Maps the user row to
// the event payload and forwards to Outbox.Enqueue.
func (p *UserPublisher) PublishUserCreated(ctx context.Context, u *models.User) error {
	if p == nil || p.outbox == nil || u == nil {
		return nil
	}
	tenantID := u.TenantID
	if tenantID == "" {
		tenantID = "default"
	}
	return p.outbox.Enqueue(ctx, userCreatedEventType, u.SubjectID, tenantID, UserPayload{
		SubjectID:   u.SubjectID,
		TenantID:    tenantID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Email:       u.Email,
	})
}
