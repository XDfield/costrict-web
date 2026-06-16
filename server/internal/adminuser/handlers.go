package adminuser

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/audit"
	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// adminUserResponse is the flat, frontend-facing shape for one member row. It is
// derived from the local users table (NOT the Casdoor-shaped SearchedUser), so
// the admin console reads stable subject_id / status / organization fields.
type adminUserResponse struct {
	SubjectID    string   `json:"subject_id"`
	Username     string   `json:"username"`
	DisplayName  string   `json:"displayName"`
	Email        string   `json:"email"`
	AvatarURL    string   `json:"avatarUrl"`
	Organization string   `json:"organization"`
	Status       string   `json:"status"`
	Roles        []string `json:"roles"`
	LastLoginAt  *string  `json:"lastLoginAt"`
	CreatedAt    string   `json:"createdAt"`
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func toResponse(u models.User, roles []string) adminUserResponse {
	r := adminUserResponse{
		SubjectID:    u.SubjectID,
		Username:     u.Username,
		DisplayName:  derefStr(u.DisplayName),
		Email:        derefStr(u.Email),
		AvatarURL:    derefStr(u.AvatarURL),
		Organization: derefStr(u.Organization),
		Status:       u.Status,
		Roles:        roles,
		CreatedAt:    u.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if r.Status == "" {
		r.Status = userpkg.UserStatusActive
	}
	if r.Roles == nil {
		r.Roles = []string{}
	}
	if u.LastLoginAt != nil {
		s := u.LastLoginAt.Format("2006-01-02T15:04:05Z07:00")
		r.LastLoginAt = &s
	}
	return r
}

// ListUsersHandler godoc
//
//	@Summary		List members (admin)
//	@Description	Paginated/searchable/status-filtered user list for the admin console (platform admin only)
//	@Tags			admin/users
//	@Produce		json
//	@Security		BearerAuth
//	@Param			search			query		string	false	"username/display name/email LIKE"
//	@Param			organization	query		string	false	"Exact organization filter"
//	@Param			status			query		string	false	"Exact status filter (active|disabled|banned)"
//	@Param			page			query		int		false	"Page number (1-based)"
//	@Param			pageSize		query		int		false	"Page size (default 20, max 200)"
//	@Success		200				{object}	object{users=[]object,total=int,page=int,pageSize=int}
//	@Failure		401				{object}	object{error=string}
//	@Failure		500				{object}	object{error=string}
//	@Router			/admin/users [get]
func (m *Module) ListUsersHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		page := atoiDefault(c.Query("page"), 1)
		pageSize := atoiDefault(c.Query("pageSize"), 20)

		users, total, err := m.users.ListUsers(userpkg.ListUsersParams{
			Keyword:      c.Query("search"),
			Organization: c.Query("organization"),
			Status:       c.Query("status"),
			Page:         page,
			PageSize:     pageSize,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list users"})
			return
		}

		ids := make([]string, 0, len(users))
		for _, u := range users {
			ids = append(ids, u.SubjectID)
		}
		rolesByUser := m.users.RolesForUsers(ids)

		out := make([]adminUserResponse, 0, len(users))
		for _, u := range users {
			out = append(out, toResponse(u, rolesByUser[u.SubjectID]))
		}

		c.JSON(http.StatusOK, gin.H{
			"users":    out,
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
		})
	}
}

// GetUserProfileHandler godoc
//
//	@Summary		Member profile (admin)
//	@Description	Aggregated activity (created items, distributions sent/received) + roles for one member (platform admin only)
//	@Tags			admin/users
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id	path		string	true	"Member subject id"
//	@Success		200	{object}	object{user=object,profile=object}
//	@Failure		401	{object}	object{error=string}
//	@Failure		404	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/admin/users/{id}/profile [get]
func (m *Module) GetUserProfileHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		subjectID := c.Param("id")

		u, err := m.users.GetUserByID(subjectID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}

		profile, err := m.users.GetUserProfile(subjectID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user profile"})
			return
		}

		roles := m.users.RolesForUsers([]string{subjectID})[subjectID]

		c.JSON(http.StatusOK, gin.H{
			"user":    toResponse(*u, roles),
			"profile": profile,
		})
	}
}

type setStatusRequest struct {
	Status string `json:"status" binding:"required"`
}

// SetUserStatusHandler godoc
//
//	@Summary		Set member status (admin)
//	@Description	Enable/disable/ban a member. Refuses to change the operator's own status (platform admin only)
//	@Tags			admin/users
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id		path		string							true	"Member subject id"
//	@Param			body	body		object{status=string}			true	"New status (active|disabled|banned)"
//	@Success		200		{object}	object{success=bool}
//	@Failure		400		{object}	object{error=string}
//	@Failure		401		{object}	object{error=string}
//	@Failure		404		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/admin/users/{id}/status [put]
func (m *Module) SetUserStatusHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		operatorID := c.GetString(appmiddleware.UserIDKey)
		if operatorID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		subjectID := c.Param("id")

		var req setStatusRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		if err := m.users.SetUserStatus(subjectID, req.Status, operatorID); err != nil {
			switch {
			case errors.Is(err, userpkg.ErrInvalidUserStatus):
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
			case errors.Is(err, userpkg.ErrCannotChangeOwnStatus):
				c.JSON(http.StatusBadRequest, gin.H{"error": "cannot change your own status"})
			case errors.Is(err, userpkg.ErrAdminUserNotFound):
				c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update user status"})
			}
			return
		}

		// Drop any cached status so the new banned/disabled/active state is enforced
		// immediately rather than after the status-cache TTL elapses.
		appmiddleware.InvalidateStatusCache(subjectID)

		audit.Record(operatorID, audit.ActionUserStatusChange, audit.TargetUser, subjectID, gin.H{
			"status": req.Status,
		})

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

// ListOrganizationsHandler godoc
//
//	@Summary		List organizations (admin)
//	@Description	Roll up users by organization with member counts, busiest first (platform admin only)
//	@Tags			admin/users
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	object{organizations=[]object{organization=string,memberCount=int}}
//	@Failure		401	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/admin/organizations [get]
func (m *Module) ListOrganizationsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		orgs, err := m.users.ListOrganizations()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list organizations"})
			return
		}
		if orgs == nil {
			orgs = []userpkg.OrganizationCount{}
		}
		c.JSON(http.StatusOK, gin.H{"organizations": orgs})
	}
}
