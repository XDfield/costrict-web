package project

import (
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
)

type CreateProjectRequest struct {
	Name        string     `json:"name" binding:"required"`
	Description string     `json:"description"`
	EnabledAt   *time.Time `json:"enabledAt"`
}

type UpdateProjectRequest struct {
	Name        *string    `json:"name"`
	Description *string    `json:"description"`
	EnabledAt   *time.Time `json:"enabledAt"`
}

type CreateInvitationRequest struct {
	InviteeID string `json:"inviteeId" binding:"required"`
	Role      string `json:"role"`
	Message   string `json:"message"`
}

type RespondInvitationRequest struct {
	Accept bool `json:"accept"`
}

type UpdateMemberRoleRequest struct {
	Role string `json:"role" binding:"required"`
}

type SetProjectPinRequest struct {
	Pinned bool `json:"pinned"`
}

type CreateProjectRepositoryRequest struct {
	GitRepoURL  string `json:"gitRepoUrl" binding:"required"`
	DisplayName string `json:"displayName"`
}

type ProjectRepositoryResponse struct {
	Repository *models.ProjectRepository `json:"repository"`
}

type ListProjectRepositoriesResponse struct {
	Repositories []models.ProjectRepository `json:"repositories"`
}

type ProjectRepoActivitySummary struct {
	MemberCount           int   `json:"member_count"`
	RepositoryCount       int   `json:"repository_count"`
	ActiveMemberCount     int   `json:"active_member_count"`
	ActiveRepositoryCount int   `json:"active_repository_count"`
	TotalRequests         int64 `json:"total_requests"`
}

type ProjectRepoActivityRange struct {
	Days int    `json:"days"`
	From string `json:"from"`
	To   string `json:"to"`
}

type ProjectRepoActivityProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ProjectMemberActiveRepo struct {
	RepositoryID   string  `json:"repositoryId"`
	DisplayName    string  `json:"displayName"`
	GitRepoURL     string  `json:"gitRepoUrl"`
	RequestCount   int64   `json:"requestCount"`
	LastActiveDate string  `json:"lastActiveDate"`
	InputTokens    int64   `json:"inputTokens"`
	OutputTokens   int64   `json:"outputTokens"`
	Cost           float64 `json:"cost"`
}

type ProjectRepoActiveMember struct {
	UserID         string `json:"userId"`
	Username       string `json:"username"`
	RequestCount   int64  `json:"requestCount"`
	LastActiveDate string `json:"lastActiveDate"`
}

type ProjectMemberRepoActivityItem struct {
	UserID          string                    `json:"userId"`
	Username        string                    `json:"username"`
	Role            string                    `json:"role"`
	ActiveRepoCount int                       `json:"activeRepoCount"`
	TotalRequests   int64                     `json:"totalRequests"`
	ActiveRepos     []ProjectMemberActiveRepo `json:"activeRepos"`
}

type ProjectRepositoryRepoActivityItem struct {
	RepositoryID      string                    `json:"repositoryId"`
	DisplayName       string                    `json:"displayName"`
	GitRepoURL        string                    `json:"gitRepoUrl"`
	ActiveMemberCount int                       `json:"activeMemberCount"`
	TotalRequests     int64                     `json:"totalRequests"`
	ActiveMembers     []ProjectRepoActiveMember `json:"activeMembers"`
}

type ProjectRepoActivityResponse struct {
	Project      ProjectRepoActivityProject            `json:"project"`
	Range        ProjectRepoActivityRange              `json:"range"`
	Summary      ProjectRepoActivitySummary            `json:"summary"`
	Members      []ProjectMemberRepoActivityItem       `json:"members"`
	Repositories []ProjectRepositoryRepoActivityItem   `json:"repositories"`
}

type ProjectResponse struct {
	Project *models.Project `json:"project"`
}

type ProjectsResponse struct {
	Projects []models.Project `json:"projects"`
}

type InvitationResponse struct {
	Invitation *models.ProjectInvitation `json:"invitation"`
}

type InvitationsResponse struct {
	Invitations []models.ProjectInvitation `json:"invitations"`
}

type MemberResponse struct {
	Member *models.ProjectMember `json:"member"`
}

type MembersResponse struct {
	Members []models.ProjectMember `json:"members"`
}
