package collaboration

import (
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Space represents a collaboration space (like a workspace for teams).
type Space struct {
	ID          string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	Name        string         `gorm:"not null"                                       json:"name"`
	Slug        string         `gorm:"uniqueIndex;not null"                           json:"slug"`
	Description string         `                                                      json:"description,omitempty"`
	Settings    datatypes.JSON `gorm:"type:jsonb;default:'{}'"                        json:"settings,omitempty" swaggertype:"object"`
	CreatedAt   time.Time      `                                                      json:"createdAt"`
	UpdatedAt   time.Time      `                                                      json:"updatedAt"`
	DeletedAt   gorm.DeletedAt `gorm:"index"                                          json:"-"`
}

// SpaceMember represents a user's membership in a space.
type SpaceMember struct {
	ID        string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	SpaceID   string         `gorm:"not null;index"                                 json:"spaceId"`
	UserID    string         `gorm:"not null;index"                                 json:"userId"`
	Role      string         `gorm:"not null;default:'member'"                      json:"role"` // owner | admin | member
	CreatedAt time.Time      `                                                      json:"createdAt"`
	UpdatedAt time.Time      `                                                      json:"updatedAt"`
	DeletedAt gorm.DeletedAt `gorm:"index"                                          json:"-"`

	// Unique index on (space_id, user_id) is handled by the migration.
}

// Issue represents a task or ticket within a space.
type Issue struct {
	ID            string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	SpaceID       string         `gorm:"not null;index"                                 json:"spaceId"`
	Number        int            `gorm:"not null"                                       json:"number"`
	Title         string         `gorm:"not null"                                       json:"title"`
	Description   string         `gorm:"type:text"                                      json:"description,omitempty"`
	Status        string         `gorm:"not null;default:'backlog'"                     json:"status"`        // backlog | todo | in_progress | in_review | done | blocked | cancelled
	Priority      string         `gorm:"not null;default:'none'"                        json:"priority"`      // urgent | high | medium | low | none
	AssigneeType  *string        `                                                      json:"assigneeType,omitempty"`  // member | squad
	AssigneeID    *string        `gorm:"index"                                          json:"assigneeId,omitempty"`
	CreatorID     string         `gorm:"not null;index"                                 json:"creatorId"`
	ParentIssueID *string        `gorm:"index"                                          json:"parentIssueId,omitempty"`
	Position      float64        `gorm:"not null;default:0"                             json:"position"`
	DueDate       *time.Time     `                                                      json:"dueDate,omitempty"`
	Metadata      datatypes.JSON `gorm:"type:jsonb;default:'{}'"                        json:"metadata,omitempty" swaggertype:"object"`
	CreatedAt     time.Time      `                                                      json:"createdAt"`
	UpdatedAt     time.Time      `                                                      json:"updatedAt"`
	DeletedAt     gorm.DeletedAt `gorm:"index"                                          json:"-"`
}

// IssueComment represents a comment on an issue.
type IssueComment struct {
	ID        string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	IssueID   string    `gorm:"not null;index"                                 json:"issueId"`
	UserID    string    `gorm:"not null"                                       json:"userId"`
	Content   string    `gorm:"type:text;not null"                             json:"content"`
	CreatedAt time.Time `                                                      json:"createdAt"`
	UpdatedAt time.Time `                                                      json:"updatedAt"`
}

// Squad represents a team or group within a space.
type Squad struct {
	ID           string     `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	SpaceID      string     `gorm:"not null;index"                                 json:"spaceId"`
	Name         string     `gorm:"not null"                                       json:"name"`
	Description  string     `gorm:"type:text"                                      json:"description,omitempty"`
	LeaderID     string     `gorm:"index"                                          json:"leaderId,omitempty"`
	Instructions string     `gorm:"type:text"                                      json:"instructions,omitempty"`
	AvatarURL    string     `                                                      json:"avatarUrl,omitempty"`
	ArchivedAt   *time.Time `gorm:"index"                                          json:"archivedAt,omitempty"`
	CreatedAt    time.Time  `                                                      json:"createdAt"`
	UpdatedAt    time.Time  `                                                      json:"updatedAt"`
}

// SquadMember represents a user's membership in a squad.
type SquadMember struct {
	SquadID  string    `gorm:"primaryKey"         json:"squadId"`
	UserID   string    `gorm:"primaryKey"         json:"userId"`
	Role     string    `gorm:"not null;default:'member'" json:"role"` // leader | member
	JoinedAt time.Time `gorm:"not null"           json:"joinedAt"`
}
