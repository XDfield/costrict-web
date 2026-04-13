package team

import "github.com/gorilla/websocket"

// CloudEvent is the unified event envelope for all team communication.
type CloudEvent struct {
	EventID   string         `json:"eventId"`
	Type      string         `json:"type"`
	SessionID string         `json:"sessionId"`
	Timestamp int64          `json:"timestamp"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// WSConnection represents an active WebSocket connection from a client.
type WSConnection struct {
	ID           string
	UserID       string
	MachineID    string
	SessionID    string
	Conn         *websocket.Conn
	Send         chan []byte
	Done         chan struct{}
	LastActivity int64
}

// Event types: Client → Cloud
const (
	EventSessionCreate     = "session.create"
	EventSessionJoin       = "session.join"
	EventTaskPlanSubmit    = "task.plan.submit"
	EventTaskClaim         = "task.claim"
	EventTaskProgress      = "task.progress"
	EventTaskComplete      = "task.complete"
	EventTaskFail          = "task.fail"
	EventApprovalRequest   = "approval.request"
	EventApprovalRespond   = "approval.respond"
	EventMessageSend       = "message.send"
	EventRepoRegister      = "repo.register"
	EventExploreRequest    = "explore.request"
	EventExploreResult     = "explore.result"
	EventLeaderElect       = "leader.elect"
	EventLeaderHeartbeat   = "leader.heartbeat"
)

// Event types: Cloud → Client
const (
	EventTaskAssigned      = "task.assigned"
	EventApprovalPush      = "approval.push"
	EventApprovalResponse  = "approval.response"
	EventMessageReceive    = "message.receive"
	EventSessionUpdated    = "session.updated"
	EventTeammateStatus    = "teammate.status"
	EventLeaderElected     = "leader.elected"
	EventLeaderExpired     = "leader.expired"
	EventError             = "error"
)

// Session statuses
const (
	SessionStatusActive    = "active"
	SessionStatusPaused    = "paused"
	SessionStatusCompleted = "completed"
	SessionStatusFailed    = "failed"
)

// Member statuses
const (
	MemberStatusOnline  = "online"
	MemberStatusOffline = "offline"
	MemberStatusBusy    = "busy"
)

// Member roles
const (
	MemberRoleLeader   = "leader"
	MemberRoleTeammate = "teammate"
)

// Task statuses
const (
	TaskStatusPending   = "pending"
	TaskStatusAssigned  = "assigned"
	TaskStatusClaimed   = "claimed"
	TaskStatusRunning   = "running"
	TaskStatusCompleted = "completed"
	TaskStatusFailed    = "failed"
)

// WebSocket configuration defaults
const (
	DefaultWSReadBufferSize          = 1024
	DefaultWSWriteBufferSize         = 1024
	DefaultLeaderLockTTLSec          = 30
	DefaultLeaderHeartbeatSec        = 10
	DefaultEventBacklogTTLMin        = 60
	DefaultMaxConnectionsPerSession  = 20
	DefaultSendChannelCapacity       = 256
	WSPingIntervalSec                = 30
	WSWriteWaitSec                   = 10
	WSPongWaitSec                    = 60
)

// Redis key patterns
const (
	redisKeyLeaderLock   = "team:session:%s:leader_lock"
	redisKeyFencingToken = "team:session:%s:fencing_token"
	redisKeyPresence     = "team:session:%s:presence:%s"
	redisKeyBacklog      = "team:session:%s:backlog:%s"
)