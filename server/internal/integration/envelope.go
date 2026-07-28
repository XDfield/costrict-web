// Package integration receives event envelopes from external systems
// (currently: the Multica server) and routes them into the existing
// notification pipeline. The bridge is one-way HTTPS + HMAC; Multica knows
// nothing about channels, and this package knows nothing about Multica
// internals beyond the versioned envelope contract.
package integration

import (
	"time"

	"github.com/costrict/costrict-web/server/internal/notification"
)

// EventMulticaIssueStatusChanged is the only envelope type currently
// processed. Unknown types are ACKed (200) without delivery so the contract
// can grow without breaking older receivers. The constant lives in the
// notification package so channel subscriptions share one definition.
const EventMulticaIssueStatusChanged = notification.EventMulticaIssueStatusChanged

// Envelope mirrors the v1 contract produced by Multica's
// internal/integration.Notifier. Unknown fields are ignored.
type Envelope struct {
	Version    int          `json:"version"`
	EventID    string       `json:"event_id"`
	Type       string       `json:"type"`
	OccurredAt time.Time    `json:"occurred_at"`
	Workspace  WorkspaceRef `json:"workspace"`
	Actor      ActorRef     `json:"actor"`
	Issue      IssueRef     `json:"issue"`
	Recipients []string     `json:"recipients"`
}

type WorkspaceRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ActorRef struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type IssueRef struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	PrevStatus string `json:"prev_status"`
	Status     string `json:"status"`
	URL        string `json:"url"`
}
