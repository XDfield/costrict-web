package project

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
)

func writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrProjectNotFound), errors.Is(err, ErrInvitationNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, ErrPermissionDenied), errors.Is(err, ErrNotMember), errors.Is(err, ErrOnlyInviterCanCancel):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	case errors.Is(err, ErrProjectNameExists), errors.Is(err, ErrCannotInviteSelf), errors.Is(err, ErrUserAlreadyInProject), errors.Is(err, ErrInvitationAlreadyExists), errors.Is(err, ErrInvitationExpired), errors.Is(err, ErrInvitationHandled), errors.Is(err, ErrInvalidRole), errors.Is(err, ErrProjectArchived), errors.Is(err, ErrCannotRemoveLastAdmin), errors.Is(err, ErrProjectAlreadyArchived), errors.Is(err, ErrProjectNotArchived):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, ErrRepositoryAlreadyBound):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, ErrRepositoryBindingNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
	}
}

func currentUserID(c *gin.Context) string {
	return c.GetString(middleware.UserIDKey)
}

// ListProjectsHandler godoc
// @Summary      List my projects
// @Description  List all projects the authenticated user belongs to
// @Tags         projects
// @Produce      json
// @Security     BearerAuth
// @Param        includeArchived  query     bool  false  "Include archived projects"
// @Param        pinned           query     bool  false  "Only return projects pinned by current user"
// @Success      200  {object}  project.ProjectsResponse
// @Failure      401  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /projects [get]
func ListProjectsHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		startedAt := time.Now()
		userID := currentUserID(c)
		includeArchived := c.Query("includeArchived") == "true"
		pinnedOnly := c.Query("pinned") == "true"
		projects, err := svc.ListProjects(userID, includeArchived, pinnedOnly)
		queryElapsed := time.Since(startedAt)
		if err != nil {
			logger.Warn("[projects.list] user=%s includeArchived=%t pinned=%t query_ms=%d total_ms=%d err=%v", userID, includeArchived, pinnedOnly, queryElapsed.Milliseconds(), time.Since(startedAt).Milliseconds(), err)
			writeError(c, err)
			return
		}
		logger.Info("[projects.list] user=%s includeArchived=%t pinned=%t count=%d query_ms=%d total_ms=%d", userID, includeArchived, pinnedOnly, len(projects), queryElapsed.Milliseconds(), time.Since(startedAt).Milliseconds())
		c.JSON(http.StatusOK, ProjectsResponse{Projects: projects})
	}
}

// CreateProjectHandler godoc
// @Summary      Create project
// @Description  Create a new project and add the creator as project admin
// @Tags         projects
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      project.CreateProjectRequest  true  "Project data"
// @Success      201  {object}  project.ProjectResponse
// @Failure      400  {object}  object{error=string}
// @Failure      401  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /projects [post]
func CreateProjectHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateProjectRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		project, err := svc.CreateProject(currentUserID(c), req.Name, req.Description, req.EnabledAt)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, ProjectResponse{Project: project})
	}
}

// GetProjectHandler godoc
// @Summary      Get project
// @Description  Get project details for a project member
// @Tags         projects
// @Produce      json
// @Security     BearerAuth
// @Param        id  path      string  true  "Project ID"
// @Success      200  {object}  project.ProjectResponse
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /projects/{id} [get]
func GetProjectHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID := c.Param("id")
		project, err := svc.GetProjectForUser(projectID, currentUserID(c))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, ProjectResponse{Project: project})
	}
}

// GetProjectBasicInfoHandler godoc
// @Summary      Get project basic info
// @Description  Get basic project information for a project member
// @Tags         projects
// @Produce      json
// @Security     BearerAuth
// @Param        id  path      string  true  "Project ID"
// @Success      200  {object}  project.ProjectBasicInfoResponse
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /projects/{id}/basic [get]
func GetProjectBasicInfoHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		project, err := svc.GetProjectBasicInfoForUser(c.Param("id"), currentUserID(c))
		if err != nil {
			writeError(c, err)
			return
		}

		c.JSON(http.StatusOK, ProjectBasicInfoResponse{Project: &ProjectBasicInfo{
			ID:          project.ID,
			Name:        project.Name,
			Description: project.Description,
			EnabledAt:   project.EnabledAt,
			ArchivedAt:  project.ArchivedAt,
		}})
	}
}

// SetProjectPinHandler godoc
// @Summary      Pin or unpin project
// @Description  Set personal pin status for a project the authenticated user belongs to
// @Tags         projects
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path      string                     true  "Project ID"
// @Param        body  body      project.SetProjectPinRequest true  "Pin state"
// @Success      200  {object}  project.ProjectResponse
// @Failure      400  {object}  object{error=string}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /projects/{id}/pin [put]
func SetProjectPinHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req SetProjectPinRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := svc.SetProjectPin(c.Param("id"), currentUserID(c), req.Pinned); err != nil {
			writeError(c, err)
			return
		}
		project, err := svc.GetProjectForUser(c.Param("id"), currentUserID(c))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, ProjectResponse{Project: project})
	}
}

// UpdateProjectHandler godoc
// @Summary      Update project
// @Description  Update project information as project admin
// @Tags         projects
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path      string                       true  "Project ID"
// @Param        body  body      project.UpdateProjectRequest true  "Project update data"
// @Success      200  {object}  project.ProjectResponse
// @Failure      400  {object}  object{error=string}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /projects/{id} [put]
func UpdateProjectHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req UpdateProjectRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		updates := map[string]any{}
		if req.Name != nil {
			updates["name"] = *req.Name
		}
		if req.Description != nil {
			updates["description"] = *req.Description
		}
		if req.EnabledAt != nil {
			updates["enabled_at"] = *req.EnabledAt
		}
		if err := svc.UpdateProject(c.Param("id"), currentUserID(c), updates); err != nil {
			writeError(c, err)
			return
		}
		project, _ := svc.GetProject(c.Param("id"))
		c.JSON(http.StatusOK, ProjectResponse{Project: project})
	}
}

// DeleteProjectHandler godoc
// @Summary      Delete project
// @Description  Soft delete a project as project admin
// @Tags         projects
// @Produce      json
// @Security     BearerAuth
// @Param        id  path  string  true  "Project ID"
// @Success      204
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /projects/{id} [delete]
func DeleteProjectHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := svc.DeleteProject(c.Param("id"), currentUserID(c)); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// ArchiveProjectHandler godoc
// @Summary      Archive project
// @Description  Archive a project as project admin
// @Tags         projects
// @Produce      json
// @Security     BearerAuth
// @Param        id  path      string  true  "Project ID"
// @Success      200  {object}  project.ProjectResponse
// @Failure      400  {object}  object{error=string}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /projects/{id}/archive [post]
func ArchiveProjectHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := svc.ArchiveProject(c.Param("id"), currentUserID(c)); err != nil {
			writeError(c, err)
			return
		}
		project, _ := svc.GetProject(c.Param("id"))
		c.JSON(http.StatusOK, ProjectResponse{Project: project})
	}
}

// UnarchiveProjectHandler godoc
// @Summary      Unarchive project
// @Description  Unarchive a project as project admin
// @Tags         projects
// @Produce      json
// @Security     BearerAuth
// @Param        id  path      string  true  "Project ID"
// @Success      200  {object}  project.ProjectResponse
// @Failure      400  {object}  object{error=string}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /projects/{id}/unarchive [post]
func UnarchiveProjectHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := svc.UnarchiveProject(c.Param("id"), currentUserID(c)); err != nil {
			writeError(c, err)
			return
		}
		project, _ := svc.GetProject(c.Param("id"))
		c.JSON(http.StatusOK, ProjectResponse{Project: project})
	}
}

// UpdateProjectArchiveTimeHandler godoc
// @Summary      Update project archive time
// @Description  Update archivedAt for an already archived project as project admin
// @Tags         projects
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path      string                                 true  "Project ID"
// @Param        body  body      project.UpdateProjectArchiveTimeRequest true  "Archive time update data"
// @Success      200  {object}  project.ProjectBasicInfoResponse
// @Failure      400  {object}  object{error=string}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /projects/{id}/archive-time [put]
func UpdateProjectArchiveTimeHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req UpdateProjectArchiveTimeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.ArchivedAt == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "archivedAt is required"})
			return
		}

		if err := svc.UpdateProjectArchiveTime(c.Param("id"), currentUserID(c), *req.ArchivedAt); err != nil {
			writeError(c, err)
			return
		}

		project, err := svc.GetProjectForUser(c.Param("id"), currentUserID(c))
		if err != nil {
			writeError(c, err)
			return
		}

		c.JSON(http.StatusOK, ProjectBasicInfoResponse{Project: &ProjectBasicInfo{
			ID:          project.ID,
			Name:        project.Name,
			Description: project.Description,
			EnabledAt:   project.EnabledAt,
			ArchivedAt:  project.ArchivedAt,
		}})
	}
}

// ListMembersHandler godoc
// @Summary      List project members
// @Description  List all members of a project for project members
// @Tags         projects
// @Produce      json
// @Security     BearerAuth
// @Param        id  path      string  true  "Project ID"
// @Success      200  {object}  project.MembersResponse
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /projects/{id}/members [get]
func ListMembersHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := svc.checkPermission(c.Param("id"), currentUserID(c), RoleMember); err != nil {
			writeError(c, err)
			return
		}
		members, err := svc.ListMembers(c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, MembersResponse{Members: members})
	}
}

// RemoveMemberHandler godoc
// @Summary      Remove project member
// @Description  Remove a member from a project as project admin
// @Tags         projects
// @Produce      json
// @Security     BearerAuth
// @Param        id      path  string  true  "Project ID"
// @Param        userId  path  string  true  "Target user ID"
// @Success      204
// @Failure      400  {object}  object{error=string}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /projects/{id}/members/{userId} [delete]
func RemoveMemberHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := svc.RemoveMember(c.Param("id"), currentUserID(c), c.Param("userId")); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// UpdateMemberRoleHandler godoc
// @Summary      Update project member role
// @Description  Update a project member role as project admin
// @Tags         projects
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id      path      string                          true  "Project ID"
// @Param        userId  path      string                          true  "Target user ID"
// @Param        body    body      project.UpdateMemberRoleRequest true  "Role update data"
// @Success      200  {object}  project.MemberResponse
// @Failure      400  {object}  object{error=string}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /projects/{id}/members/{userId}/role [put]
func UpdateMemberRoleHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req UpdateMemberRoleRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := svc.UpdateMemberRole(c.Param("id"), currentUserID(c), c.Param("userId"), req.Role); err != nil {
			writeError(c, err)
			return
		}
		member, _ := svc.GetMember(c.Param("id"), c.Param("userId"))
		c.JSON(http.StatusOK, MemberResponse{Member: member})
	}
}

// CreateInvitationHandler godoc
// @Summary      Create project invitation
// @Description  Invite a user to join a project as project admin
// @Tags         project-invitations
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path      string                          true  "Project ID"
// @Param        body  body      project.CreateInvitationRequest true  "Invitation data"
// @Success      201  {object}  project.InvitationResponse
// @Failure      400  {object}  object{error=string}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /projects/{id}/invitations [post]
func CreateInvitationHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateInvitationRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		invitation, err := svc.CreateInvitation(c.Param("id"), currentUserID(c), req.InviteeID, req.Role, req.Message)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, InvitationResponse{Invitation: invitation})
	}
}

// ListInvitationsHandler godoc
// @Summary      List project invitations
// @Description  List invitation records for a project as project admin
// @Tags         project-invitations
// @Produce      json
// @Security     BearerAuth
// @Param        id  path      string  true  "Project ID"
// @Success      200  {object}  project.InvitationsResponse
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /projects/{id}/invitations [get]
func ListInvitationsHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := svc.checkPermission(c.Param("id"), currentUserID(c), RoleAdmin); err != nil {
			writeError(c, err)
			return
		}
		invitations, err := svc.ListInvitations(c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, InvitationsResponse{Invitations: invitations})
	}
}

// ListMyInvitationsHandler godoc
// @Summary      List my project invitations
// @Description  List all project invitations for the authenticated user
// @Tags         project-invitations
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  project.InvitationsResponse
// @Failure      401  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /invitations [get]
func ListMyInvitationsHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		invitations, err := svc.ListMyInvitations(currentUserID(c))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, InvitationsResponse{Invitations: invitations})
	}
}

// RespondInvitationHandler godoc
// @Summary      Respond to project invitation
// @Description  Accept or reject a project invitation as the invitee
// @Tags         project-invitations
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path      string                           true  "Invitation ID"
// @Param        body  body      project.RespondInvitationRequest true  "Invitation response data"
// @Success      200  {object}  project.InvitationResponse
// @Failure      400  {object}  object{error=string}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /invitations/{id}/respond [post]
func RespondInvitationHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req RespondInvitationRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := svc.RespondInvitation(c.Param("id"), currentUserID(c), req.Accept); err != nil {
			writeError(c, err)
			return
		}
		invitations, _ := svc.ListMyInvitations(currentUserID(c))
		for i := range invitations {
			if invitations[i].ID == c.Param("id") {
				c.JSON(http.StatusOK, InvitationResponse{Invitation: &invitations[i]})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

// CancelInvitationHandler godoc
// @Summary      Cancel project invitation
// @Description  Cancel a pending invitation as the inviter
// @Tags         project-invitations
// @Produce      json
// @Security     BearerAuth
// @Param        id  path  string  true  "Invitation ID"
// @Success      204
// @Failure      400  {object}  object{error=string}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /invitations/{id} [delete]
func CancelInvitationHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := svc.CancelInvitation(c.Param("id"), currentUserID(c)); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func ListProjectRepositoriesHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		repositories, err := svc.ListRepositories(c.Param("id"), currentUserID(c))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, ListProjectRepositoriesResponse{Repositories: repositories})
	}
}

func ListProjectRepositoryCandidatesHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		days := 30
		if raw := c.Query("days"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 90 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "days must be between 1 and 90"})
				return
			}
			days = parsed
		}
		repositories, err := svc.ListRepositoryCandidates(c.Param("id"), currentUserID(c), days)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, ProjectRepositoryCandidatesResponse{Repositories: repositories})
	}
}

func BindProjectRepositoryHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateProjectRepositoryRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		repository, err := svc.BindRepository(c.Param("id"), currentUserID(c), req.GitRepoURL, req.DisplayName)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, ProjectRepositoryResponse{Repository: repository})
	}
}

func UnbindProjectRepositoryHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := svc.UnbindRepository(c.Param("id"), c.Param("repoBindingId"), currentUserID(c)); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func GetProjectRepoActivityHandler(svc *ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		days := 7
		if raw := c.Query("days"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 90 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "days must be between 1 and 90"})
				return
			}
			days = parsed
		}
		includeInactive := true
		if raw := c.Query("includeInactive"); raw != "" {
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "includeInactive must be a boolean"})
				return
			}
			includeInactive = parsed
		}

		resp, err := svc.GetProjectRepoActivity(c.Param("id"), currentUserID(c), days, includeInactive)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, resp)
	}
}
