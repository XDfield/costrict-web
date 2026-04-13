package team

import (
	"time"

	"github.com/lib/pq"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// TeamSession represents a collaborative team session.
type TeamSession struct {
	ID              string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	Name            string         `gorm:"not null" json:"name"`
	CreatorID       string         `gorm:"not null;index" json:"creatorId"`
	Status          string         `gorm:"not null;default:'active'" json:"status"`
	LeaderMachineID string         `gorm:"index" json:"leaderMachineId,omitempty"`
	LeaderUserID    string         `gorm:"index" json:"leaderUserId,omitempty"`
	FencingToken    int64          `gorm:"not null;default:0" json:"fencingToken"`
	Metadata        datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"metadata" swaggertype:"object"`
	CreatedAt       time.Time      `json:"createdAt"`
	UpdatedAt       time.Time      `json:"updatedAt"`
	DeletedAt       gorm.DeletedAt `gorm:"index" json:"-"`
}

// TeamSessionMember represents a member (machine) in a team session.
type TeamSessionMember struct {
	ID            string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	SessionID     string         `gorm:"type:uuid;not null;index;uniqueIndex:idx_team_member_session_machine" json:"sessionId"`
	UserID        string         `gorm:"not null;index" json:"userId"`
	MachineID     string         `gorm:"not null;uniqueIndex:idx_team_member_session_machine" json:"machineId"`
	MachineName   string         `json:"machineName,omitempty"`
	Role          string         `gorm:"not null;default:'teammate'" json:"role"`
	Status        string         `gorm:"not null;default:'online'" json:"status"`
	ConnectedAt   time.Time      `json:"connectedAt"`
	LastHeartbeat time.Time      `json:"lastHeartbeat"`
	CreatedAt     time.Time      `json:"createdAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
	DeletedAt     gorm.DeletedAt `gorm:"index" json:"-"`
}

// TeamTask represents a task within a team session.
type TeamTask struct {
	ID               string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	SessionID        string         `gorm:"type:uuid;not null;index" json:"sessionId"`
	Description      string         `gorm:"type:text;not null" json:"description"`
	RepoAffinity     pq.StringArray `gorm:"type:text[]" json:"repoAffinity,omitempty"`
	FileHints        pq.StringArray `gorm:"type:text[]" json:"fileHints,omitempty"`
	Dependencies     pq.StringArray `gorm:"type:text[]" json:"dependencies,omitempty"`
	AssignedMemberID *string        `gorm:"type:uuid;index" json:"assignedMemberId,omitempty"`
	Status           string         `gorm:"not null;default:'pending';index" json:"status"`
	Priority         int            `gorm:"not null;default:5" json:"priority"`
	Result           datatypes.JSON `gorm:"type:jsonb" json:"result" swaggertype:"object"`
	RetryCount       int            `gorm:"not null;default:0" json:"retryCount"`
	MaxRetries       int            `gorm:"not null;default:3" json:"maxRetries"`
	ErrorMessage     string         `gorm:"type:text" json:"errorMessage,omitempty"`
	CreatedAt        time.Time      `json:"createdAt"`
	ClaimedAt        *time.Time     `json:"claimedAt,omitempty"`
	StartedAt        *time.Time     `json:"startedAt,omitempty"`
	CompletedAt      *time.Time     `json:"completedAt,omitempty"`
	UpdatedAt        time.Time      `json:"updatedAt"`
}

// TeamApprovalRequest represents a permission approval request (Phase 2 schema).
type TeamApprovalRequest struct {
	ID                string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	SessionID         string         `gorm:"type:uuid;not null;index" json:"sessionId"`
	RequesterID       string         `gorm:"type:uuid;not null;index" json:"requesterId"`
	RequesterName     string         `json:"requesterName,omitempty"`
	ToolName          string         `gorm:"not null" json:"toolName"`
	ToolInput         datatypes.JSON `gorm:"type:jsonb" json:"toolInput" swaggertype:"object"`
	Description       string         `gorm:"type:text" json:"description,omitempty"`
	RiskLevel         string         `gorm:"not null;default:'medium'" json:"riskLevel"`
	Status            string         `gorm:"not null;default:'pending';index" json:"status"`
	Feedback          string         `json:"feedback,omitempty"`
	PermissionUpdates datatypes.JSON `gorm:"type:jsonb" json:"permissionUpdates" swaggertype:"object"`
	CreatedAt         time.Time      `json:"createdAt"`
	ResolvedAt        *time.Time     `json:"resolvedAt,omitempty"`
}

// TeamRepoAffinity represents a teammate's local repository mapping (Phase 2 schema).
type TeamRepoAffinity struct {
	ID                    string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	SessionID             string    `gorm:"type:uuid;not null;index;uniqueIndex:idx_team_repo_unique" json:"sessionId"`
	MemberID              string    `gorm:"type:uuid;not null;index;uniqueIndex:idx_team_repo_unique" json:"memberId"`
	RepoRemoteURL         string    `gorm:"not null;uniqueIndex:idx_team_repo_unique" json:"repoRemoteUrl"`
	RepoLocalPath         string    `json:"repoLocalPath,omitempty"`
	CurrentBranch         string    `json:"currentBranch,omitempty"`
	HasUncommittedChanges bool      `gorm:"not null;default:false" json:"hasUncommittedChanges"`
	LastSyncedAt          time.Time `json:"lastSyncedAt"`
	CreatedAt             time.Time `json:"createdAt"`
	UpdatedAt             time.Time `json:"updatedAt"`
}