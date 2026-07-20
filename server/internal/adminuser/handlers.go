package adminuser

import (
	"errors"
	"net/http"
	"strconv"

	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// adminUserResponse is the flat, frontend-facing shape for one member row.
// Identity + status fields are sourced from cs-user via RPC (Commit 6);
// roles are local to @server (user_system_roles lives in costrict_db).
//
// Note: universalId is preserved in the response shape for caller
// compatibility but is no longer populated — cs-user's privacy-scoped admin
// surface (adminUserProfileDTO) intentionally omits casdoor_* identifiers.
// Selection/echo-back UIs that depended on universalId need a follow-up
// slice to expose it via a separate non-admin endpoint.
type adminUserResponse struct {
	SubjectID    string   `json:"subject_id"`
	UniversalID  string   `json:"universalId"`
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

// toResponseFromAdminUser maps a cs-user list row (userpkg.AdminUser) into the
// adminUserResponse shape used by the list endpoint.
func toResponseFromAdminUser(u userpkg.AdminUser, roles []string) adminUserResponse {
	r := adminUserResponse{
		SubjectID:    u.SubjectID,
		Username:     u.Username,
		DisplayName:  derefStr(u.DisplayName),
		Email:        derefStr(u.Email),
		AvatarURL:    derefStr(u.AvatarURL),
		Organization: derefStr(u.Organization),
		Status:       u.Status,
		Roles:        roles,
		CreatedAt:    u.CreatedAt,
	}
	if r.Status == "" {
		r.Status = userpkg.UserStatusActive
	}
	if r.Roles == nil {
		r.Roles = []string{}
	}
	return r
}

// toResponseFromProfile maps a cs-user profile (userpkg.AdminUserProfile) —
// which carries more fields than the list row, including Phone, AuthProvider,
// and LastLoginAt — into adminUserResponse. Used by the detail endpoint.
func toResponseFromProfile(u userpkg.AdminUserProfile, roles []string) adminUserResponse {
	r := adminUserResponse{
		SubjectID:    u.SubjectID,
		Username:     u.Username,
		DisplayName:  derefStr(u.DisplayName),
		Email:        derefStr(u.Email),
		AvatarURL:    derefStr(u.AvatarURL),
		Organization: derefStr(u.Organization),
		Status:       u.Status,
		Roles:        roles,
		LastLoginAt:  u.LastLoginAt,
		CreatedAt:    u.CreatedAt,
	}
	if r.Status == "" {
		r.Status = userpkg.UserStatusActive
	}
	if r.Roles == nil {
		r.Roles = []string{}
	}
	return r
}

// rpcUnavailable reports whether the cs-user RPC backend is not wired. The
// admin surface depends on cs-user for identity + status (Commit 7); handlers
// return 503 when this is true so the frontend can surface a clean outage.
func (m *Module) rpcUnavailable() bool {
	return m.rpc == nil || !m.rpc.Configured()
}

// ListUsersHandler godoc
//
//	@Summary		List members (admin)
//	@Description	Paginated/searchable/status-filtered user list for the admin console (platform admin only). Identity + status are proxied to cs-user; roles are loaded from the local user_system_roles table.
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
//	@Failure		503				{object}	object{error=string}
//	@Router			/admin/users [get]
func (m *Module) ListUsersHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if m.rpcUnavailable() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user service unavailable"})
			return
		}

		page := atoiDefault(c.Query("page"), 1)
		pageSize := atoiDefault(c.Query("pageSize"), 20)

		result, err := m.rpc.ListUsers(c.Request.Context(), userpkg.AdminUserListParams{
			Keyword:      c.Query("search"),
			Organization: c.Query("organization"),
			Status:       c.Query("status"),
			Page:         page,
			PageSize:     pageSize,
		})
		if err != nil {
			if errors.Is(err, userpkg.ErrRPCUnavailable) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user service unavailable"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list users"})
			return
		}

		ids := make([]string, 0, len(result.Users))
		for _, u := range result.Users {
			ids = append(ids, u.SubjectID)
		}
		rolesByUser := m.users.RolesForUsers(ids)

		out := make([]adminUserResponse, 0, len(result.Users))
		for _, u := range result.Users {
			out = append(out, toResponseFromAdminUser(u, rolesByUser[u.SubjectID]))
		}

		c.JSON(http.StatusOK, gin.H{
			"users":    out,
			"total":    result.Total,
			"page":     page,
			"pageSize": pageSize,
		})
	}
}

// GetUserProfileHandler godoc
//
//	@Summary		Member profile (admin)
//	@Description	Identity slice (cs-user) + locally-computed activity counts (capability_items, distributions sent/received) + roles (platform admin only)
//	@Tags			admin/users
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id	path		string	true	"Member subject id"
//	@Success		200	{object}	object{user=object,profile=object}
//	@Failure		401	{object}	object{error=string}
//	@Failure		404	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Failure		503	{object}	object{error=string}
//	@Router			/admin/users/{id}/profile [get]
func (m *Module) GetUserProfileHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if m.rpcUnavailable() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user service unavailable"})
			return
		}

		subjectID := c.Param("id")

		identity, err := m.rpc.GetUserProfile(c.Request.Context(), subjectID)
		if err != nil {
			switch {
			case errors.Is(err, userpkg.ErrAdminUserRPCNotFound):
				c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			case errors.Is(err, userpkg.ErrRPCUnavailable):
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user service unavailable"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user identity"})
			}
			return
		}

		activity, err := m.users.GetUserProfile(subjectID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user profile"})
			return
		}

		roles := m.users.RolesForUsers([]string{subjectID})[subjectID]

		c.JSON(http.StatusOK, gin.H{
			"user":    toResponseFromProfile(*identity, roles),
			"profile": activity,
		})
	}
}

type setStatusRequest struct {
	Status string `json:"status" binding:"required"`
}

// SetUserStatusHandler godoc
//
//	@Summary		Set member status (admin)
//	@Description	Enable/disable/ban a member. Refuses to change the operator's own status (platform admin only). Proxied to cs-user — the source of truth for user.status.
//	@Tags			admin/users
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id		path		string							true	"Member subject id"
//	@Param			body	body		object{status=string}			true	"New status (active|disabled|banned)"
//	@Success		200		{object}	object{success=bool,from_status=string,to_status=string}
//	@Failure		400		{object}	object{error=string}
//	@Failure		401		{object}	object{error=string}
//	@Failure		404		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Failure		503		{object}	object{error=string}
//	@Router			/admin/users/{id}/status [put]
func (m *Module) SetUserStatusHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if m.rpcUnavailable() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user service unavailable"})
			return
		}

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

		res, err := m.rpc.SetUserStatus(c.Request.Context(), subjectID, req.Status, operatorID)
		if err != nil {
			switch {
			case errors.Is(err, userpkg.ErrAdminUserRPCInvalidStatus):
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
			case errors.Is(err, userpkg.ErrAdminUserRPCCannotChangeOwn):
				// Preserve legacy HTTP code (400) for self-lock to avoid
				// breaking frontend expectations; cs-user itself returns 409.
				c.JSON(http.StatusBadRequest, gin.H{"error": "cannot change your own status"})
			case errors.Is(err, userpkg.ErrAdminUserRPCNotFound):
				c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			case errors.Is(err, userpkg.ErrRPCUnavailable):
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user service unavailable"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update user status"})
			}
			return
		}

		// Drop any cached status so the new banned/disabled/active state is enforced
		// immediately rather than after the status-cache TTL elapses.
		appmiddleware.InvalidateStatusCache(subjectID)

		// Audit: cs-user writes the authoritative user_center_audit_log row
		// keyed action=user.status_changed (see cs-user SetUserStatus handler).
		// No local audit.Record call — single source of truth per ADR D1.

		c.JSON(http.StatusOK, gin.H{
			"success":     true,
			"from_status": res.FromStatus,
			"to_status":   res.ToStatus,
		})
	}
}

// ListOrganizationsHandler godoc
//
//	@Summary		List organizations (admin)
//	@Description	Roll up users by organization with member counts, busiest first (platform admin only). Proxied to cs-user.
//	@Tags			admin/users
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	object{organizations=[]object{organization=string,memberCount=int}}
//	@Failure		401	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Failure		503	{object}	object{error=string}
//	@Router			/admin/organizations [get]
func (m *Module) ListOrganizationsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if m.rpcUnavailable() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user service unavailable"})
			return
		}

		orgs, err := m.rpc.ListOrganizations(c.Request.Context())
		if err != nil {
			if errors.Is(err, userpkg.ErrRPCUnavailable) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user service unavailable"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list organizations"})
			return
		}
		if orgs == nil {
			orgs = []userpkg.AdminOrganization{}
		}
		c.JSON(http.StatusOK, gin.H{"organizations": orgs})
	}
}
