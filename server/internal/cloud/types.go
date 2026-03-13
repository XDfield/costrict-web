package cloud

import "errors"

type SSEConnection struct {
	ID          string
	Type        ConnType
	UserID      string
	WorkspaceID string
	Send        chan Event
	Done        chan struct{}
	LastActivity int64
}

type ConnType string

const (
	ConnTypeUser ConnType = "user"
)

type Event struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties,omitempty"`
}

type ManagerStats struct {
	TotalConnections     int   `json:"totalConnections"`
	UserConnections      int   `json:"userConnections"`
	SessionSubscriptions int   `json:"sessionSubscriptions"`
	Uptime               int64 `json:"uptime"`
}

const (
	EventCloudConnected     = "cloud.connected"
	EventDeviceConnected    = "device.connected"
	EventHeartbeat          = "heartbeat"
	EventSessionStatus      = "session.status"
	EventSessionCreated     = "session.created"
	EventSessionUpdated     = "session.updated"
	EventMessagePartUpdated = "message.part.updated"
	EventMessagePartDelta   = "message.part.delta"
	EventDeviceStatus       = "device.status"
	EventSessionAbort       = "session.abort"
	EventSessionMessage     = "session.message"
	EventBatch              = "batch"
)

const (
	MaxConnectionsPerUser   = 5
	MaxSubscriptionsPerUser = 50
	HeartbeatIntervalMs     = 30_000
	ConnectionTimeoutMs     = 60_000
	CleanupIntervalMs       = 10_000
	BatchFlushIntervalMs    = 16
	SendChannelCapacity     = 64
)

var (
	ErrConnectionLimitExceeded   = errors.New("connection limit exceeded")
	ErrSubscriptionLimitExceeded = errors.New("subscription limit exceeded")
	ErrConnectionNotFound        = errors.New("connection not found")
)
