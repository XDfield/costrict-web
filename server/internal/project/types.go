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

type UpdateProjectArchiveTimeRequest struct {
	ArchivedAt *time.Time `json:"archivedAt" binding:"required"`
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

type ProjectResponse struct {
	Project *models.Project `json:"project"`
}

type ProjectBasicInfo struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	EnabledAt   *time.Time `json:"enabledAt,omitempty"`
	ArchivedAt  *time.Time `json:"archivedAt,omitempty"`
}

type ProjectBasicInfoResponse struct {
	Project *ProjectBasicInfo `json:"project"`
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
