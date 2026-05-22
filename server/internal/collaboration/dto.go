package collaboration

import (
	"time"

	"gorm.io/datatypes"
)

// Space DTOs

type CreateSpaceRequest struct {
	Name        string `json:"name" binding:"required"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
}

type UpdateSpaceRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type SpaceResponse struct {
	Space     *Space          `json:"space"`
	Members   []SpaceMember   `json:"members,omitempty"`
	MyRole    string          `json:"myRole,omitempty"`
}

type SpacesResponse struct {
	Spaces []Space `json:"spaces"`
}

// Member DTOs

type AddMemberRequest struct {
	UserID string `json:"userId" binding:"required"`
	Role   string `json:"role" binding:"required"`
}

type MembersResponse struct {
	Members []SpaceMember `json:"members"`
}

// Issue DTOs

type CreateIssueRequest struct {
	Title         string         `json:"title" binding:"required"`
	Description   string         `json:"description"`
	Status        string         `json:"status"`
	Priority      string         `json:"priority"`
	AssigneeType  *string        `json:"assigneeType"`
	AssigneeID    *string        `json:"assigneeId"`
	ParentIssueID *string        `json:"parentIssueId"`
	Position      float64        `json:"position"`
	DueDate       *time.Time     `json:"dueDate"`
	Metadata      datatypes.JSON `json:"metadata"`
}

type UpdateIssueRequest struct {
	Title         *string        `json:"title"`
	Description   *string        `json:"description"`
	Status        *string        `json:"status"`
	Priority      *string        `json:"priority"`
	AssigneeType  *string        `json:"assigneeType"`
	AssigneeID    *string        `json:"assigneeId"`
	ParentIssueID *string        `json:"parentIssueId"`
	Position      *float64       `json:"position"`
	DueDate       *time.Time     `json:"dueDate"`
	Metadata      datatypes.JSON `json:"metadata"`
}

type IssueResponse struct {
	Issue    *Issue          `json:"issue"`
	Comments []IssueComment  `json:"comments,omitempty"`
}

type IssuesResponse struct {
	Issues []Issue `json:"issues"`
}

// Comment DTOs

type CreateCommentRequest struct {
	Content string `json:"content" binding:"required"`
}

type CommentsResponse struct {
	Comments []IssueComment `json:"comments"`
}

// Squad DTOs

type CreateSquadRequest struct {
	Name         string `json:"name" binding:"required"`
	Description  string `json:"description"`
	LeaderID     string `json:"leaderId"`
	Instructions string `json:"instructions"`
	AvatarURL    string `json:"avatarUrl"`
}

type UpdateSquadRequest struct {
	Name         *string    `json:"name"`
	Description  *string    `json:"description"`
	LeaderID     *string    `json:"leaderId"`
	Instructions *string    `json:"instructions"`
	AvatarURL    *string    `json:"avatarUrl"`
	ArchivedAt   *time.Time `json:"archivedAt"`
}

type SquadResponse struct {
	Squad   *Squad        `json:"squad"`
	Members []SquadMember `json:"members,omitempty"`
}

type SquadsResponse struct {
	Squads []Squad `json:"squads"`
}

// Squad member DTOs

type AddSquadMemberRequest struct {
	UserID string `json:"userId" binding:"required"`
	Role   string `json:"role"`
}

type SquadMembersResponse struct {
	Members []SquadMember `json:"members"`
}
