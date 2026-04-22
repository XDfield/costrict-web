package team

import "time"

// CloudEvent is the unified event envelope — identical to the server definition.
type CloudEvent struct {
	EventID   string         `json:"eventId"`
	Type      string         `json:"type"`
	SessionID string         `json:"sessionId"`
	Timestamp int64          `json:"timestamp"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// Task mirrors the server's TeamTask fields that the client cares about.
type Task struct {
	ID               string     `json:"id"`
	SessionID        string     `json:"sessionId"`
	Description      string     `json:"description"`
	RepoAffinity     []string   `json:"repoAffinity,omitempty"`
	FileHints        []string   `json:"fileHints,omitempty"`
	Dependencies     []string   `json:"dependencies,omitempty"`
	AssignedMemberID *string    `json:"assignedMemberId,omitempty"`
	Status           string     `json:"status"`
	Priority         int        `json:"priority"`
	RetryCount       int        `json:"retryCount"`
	MaxRetries       int        `json:"maxRetries"`
	ErrorMessage     string     `json:"errorMessage,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
	ClaimedAt        *time.Time `json:"claimedAt,omitempty"`
	StartedAt        *time.Time `json:"startedAt,omitempty"`
	CompletedAt      *time.Time `json:"completedAt,omitempty"`
}

// TaskSpec is what the LeaderPlugin returns — the plan for a single task.
// Set ID to a pre-generated UUID if you want dependency DAGs to work;
// the server will use the provided ID instead of generating a new one.
type TaskSpec struct {
	ID               string   `json:"id,omitempty"`
	Description      string   `json:"description"`
	RepoAffinity     []string `json:"repoAffinity,omitempty"`
	FileHints        []string `json:"fileHints,omitempty"`
	Dependencies     []string `json:"dependencies,omitempty"` // references IDs in the same batch
	AssignedMemberID string   `json:"assignedMemberId,omitempty"`
	Priority         int      `json:"priority,omitempty"`
	MaxRetries       int      `json:"maxRetries,omitempty"`
}

// TaskResult is returned by TeammatePlugin.ExecuteTask on success.
type TaskResult struct {
	Output    string         `json:"output,omitempty"`
	Files     []string       `json:"files,omitempty"`
	ExtraData map[string]any `json:"extraData,omitempty"`
}

// ApprovalRequest is pushed to the leader when a teammate needs permission.
type ApprovalRequest struct {
	ID          string         `json:"id"`
	SessionID   string         `json:"sessionId"`
	RequesterID string         `json:"requesterId"`
	ToolName    string         `json:"toolName"`
	ToolInput   map[string]any `json:"toolInput"`
	Description string         `json:"description,omitempty"`
	RiskLevel   string         `json:"riskLevel"`
	Status      string         `json:"status"`
	CreatedAt   time.Time      `json:"createdAt"`
}

// ExploreQuery represents a single code-intelligence query.
type ExploreQuery struct {
	Type   string         `json:"type"`   // file_tree | symbol_search | content_search | git_log | dependency_graph
	Params map[string]any `json:"params"`
}

// ExploreRequest is sent to the teammate that owns the target repo.
type ExploreRequest struct {
	RequestID     string         `json:"requestId"`
	SessionID     string         `json:"sessionId"`
	FromMachineID string         `json:"fromMachineId"`
	Queries       []ExploreQuery `json:"queries"`
}

// ExploreQueryResult holds the output for one query in an ExploreRequest.
type ExploreQueryResult struct {
	Type      string `json:"type"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}

// ExploreResult is returned by ExplorePlugin and sent back to the leader.
type ExploreResult struct {
	RequestID    string               `json:"requestId"`
	QueryResults []ExploreQueryResult `json:"queryResults"`
	Error        string               `json:"error,omitempty"`
}

// Member represents a session participant, used when planning tasks.
type Member struct {
	ID          string `json:"id"`
	SessionID   string `json:"sessionId"`
	MachineID   string `json:"machineId"`
	MachineName string `json:"machineName,omitempty"`
	Role        string `json:"role"`
	Status      string `json:"status"`
}

// PlanTasksInput is passed to LeaderPlugin.PlanTasks with full session context.
type PlanTasksInput struct {
	Goal      string
	SessionID string
	Members   []Member // current session participants (for assignment decisions)
}

// ─── Event type constants (Client → Cloud) ────────────────────────────────

const (
	EventSessionCreate   = "session.create"
	EventSessionJoin     = "session.join"
	EventTaskPlanSubmit  = "task.plan.submit"
	EventTaskClaim       = "task.claim"
	EventTaskProgress    = "task.progress"
	EventTaskComplete    = "task.complete"
	EventTaskFail        = "task.fail"
	EventApprovalRequest = "approval.request"
	EventApprovalRespond = "approval.respond"
	EventMessageSend     = "message.send"
	EventRepoRegister    = "repo.register"
	EventExploreRequest  = "explore.request"
	EventExploreResult   = "explore.result"
	EventLeaderElect     = "leader.elect"
	EventLeaderHeartbeat = "leader.heartbeat"
)

// ─── Event type constants (Cloud → Client) ────────────────────────────────

const (
	EventTaskAssigned     = "task.assigned"
	EventApprovalPush     = "approval.push"
	EventApprovalResponse = "approval.response"
	EventMessageReceive   = "message.receive"
	EventSessionUpdated   = "session.updated"
	EventTeammateStatus   = "teammate.status"
	EventLeaderElected    = "leader.elected"
	EventLeaderExpired    = "leader.expired"
	EventError            = "error"
)

// ─── Status constants ─────────────────────────────────────────────────────

const (
	SessionStatusActive    = "active"
	SessionStatusPaused    = "paused"
	SessionStatusCompleted = "completed"
	SessionStatusFailed    = "failed"
)

const (
	MemberStatusOnline  = "online"
	MemberStatusOffline = "offline"
	MemberStatusBusy    = "busy"
)

const (
	MemberRoleLeader   = "leader"
	MemberRoleTeammate = "teammate"
)

const (
	TaskStatusPending   = "pending"
	TaskStatusAssigned  = "assigned"
	TaskStatusClaimed   = "claimed"
	TaskStatusRunning   = "running"
	TaskStatusCompleted = "completed"
	TaskStatusFailed    = "failed"
)

// ─── Internal timing constants ────────────────────────────────────────────

const (
	leaderHeartbeatSec = 10
	wsReconnectInitial = 1  // seconds
	wsReconnectMax     = 30 // seconds
)
