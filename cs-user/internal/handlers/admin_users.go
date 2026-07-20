// Package handlers — admin user-management internal endpoints.
//
// These power @server's /api/admin/users/* surface, migrated to cs-user
// as the single source of truth for user identity + status (admin-user-
// migration slice, option A full migration). All routes live under
// /api/internal/users/* and are gated by the existing X-Internal-Token
// middleware — same auth contract as the consumer endpoints.

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	userpkg "github.com/costrict/costrict-web/cs-user/internal/user"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// AdminUsersService is the consumer-facing surface of the admin user
// methods. Declared as an interface so tests can substitute a fake; the
// production implementation is *user.Service via admin_service.go.
type AdminUsersService interface {
	ListUsers(ctx context.Context, p userpkg.ListUsersParams) ([]*models.User, int64, error)
}

// Ensure *UsersAPI can also serve admin endpoints by composing the admin
// service interface. The handler is wired onto the same UsersAPI struct
// (it already carries the underlying *user.Service) so we don't need a
// separate top-level handler — just additional methods.

// adminUserListResponse shapes the ListUsers payload. Mirrors @server's
// adminUserResponse structurally so @server's RPC client can pass the
// payload through with no field reshaping.
type adminUserListItem struct {
	SubjectID    string  `json:"subject_id"`
	Username     string  `json:"username"`
	DisplayName  *string `json:"display_name,omitempty"`
	Email        *string `json:"email,omitempty"`
	AvatarURL    *string `json:"avatar_url,omitempty"`
	Organization *string `json:"organization,omitempty"`
	Status       string  `json:"status"`
	IsActive     bool    `json:"is_active"`
	CreatedAt    string  `json:"created_at"`
}

type adminUserListResponse struct {
	Users []adminUserListItem `json:"users"`
	Total int64               `json:"total"`
	Page  int                 `json:"page"`
	Size  int                 `json:"page_size"`
}

// ListUsers godoc
//
//	@Summary		List users for admin console
//	@Description	Paginated list of all users in the current tenant scope, optionally filtered by keyword / organization / status. Unlike /search, this surface includes disabled + banned accounts so admins can see the full roster. Used by @server's GET /api/admin/users (admin-user-migration slice).
//	@Tags			users,admin
//	@Produce		json
//	@Security		InternalToken
//	@Param			keyword			query		string	false	"Matched against username / display_name / email (LIKE %keyword%)"
//	@Param			organization	query		string	false	"Exact organization match"
//	@Param			status			query		string	false	"Account status: active | disabled | banned"	Enums(active,disabled,banned)
//	@Param			page			query		int		false	"1-based page number (default 1)"
//	@Param			page_size		query		int		false	"Page size (default 20, max 200)"
//	@Success		200				{object}	adminUserListResponse
//	@Failure		400				{object}	object{error=string}
//	@Failure		500				{object}	object{error=string}
//	@Router			/api/internal/users/list [get]
func (a *UsersAPI) ListUsers(c *gin.Context) {
	page := 1
	if v := c.Query("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "page must be a positive integer"})
			return
		}
		page = n
	}
	pageSize := 0
	if v := c.Query("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "page_size must be a positive integer"})
			return
		}
		pageSize = n
	}
	status := c.Query("status")
	if status != "" && !userpkg.IsValidUserStatus(status) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be one of active|disabled|banned"})
		return
	}

	params := userpkg.ListUsersParams{
		Keyword:      c.Query("keyword"),
		Organization: c.Query("organization"),
		Status:       status,
		Page:         page,
		PageSize:     pageSize,
	}

	users, total, err := a.Svc.ListUsers(c.Request.Context(), params)
	if err != nil {
		// All ListUsers errors are infrastructural (DB); never reveal
		// internals. Admin-facing surface treats them uniformly as 500.
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusOK, adminUserListResponse{Users: []adminUserListItem{}, Total: 0, Page: page, Size: pageSize})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	items := make([]adminUserListItem, 0, len(users))
	for _, u := range users {
		items = append(items, adminUserListItem{
			SubjectID:    u.SubjectID,
			Username:     u.Username,
			DisplayName:  u.DisplayName,
			Email:        u.Email,
			AvatarURL:    u.AvatarURL,
			Organization: u.Organization,
			Status:       u.Status,
			IsActive:     u.IsActive,
			CreatedAt:    u.CreatedAt.Format("2006-01-02T15:04:05Z"),
		})
	}
	c.JSON(http.StatusOK, adminUserListResponse{
		Users: items,
		Total: total,
		Page:  page,
		Size:  pageSize,
	})
}
