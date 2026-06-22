// Package adminuser exposes the platform-admin member-management surface
// (M1 · 成员管理): a paginated/searchable/status-filtered user list, an
// account-status switch, an organization roll-up, and a per-user profile.
//
// The HTTP handlers live here; the data logic lives on user.UserService
// (admin_service.go). The platform-admin guard is applied by the caller
// (main.go mounts RegisterRoutes onto an already-guarded /admin group), matching
// the internal/audit module convention.
package adminuser

import (
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// Module wires the admin member-management endpoints. It depends only on the
// user service (which owns the queries).
type Module struct {
	users *userpkg.UserService
}

// New constructs the module around an existing user service so it shares the
// same DB handle / caches as the rest of the app.
func New(users *userpkg.UserService) *Module {
	return &Module{users: users}
}

// RegisterRoutes mounts the member-management endpoints. The provided group is
// expected to already enforce platform-admin auth (e.g. main.go's /admin group).
func (m *Module) RegisterRoutes(adminGroup *gin.RouterGroup) {
	adminGroup.GET("/users", m.ListUsersHandler())
	adminGroup.GET("/users/:id/profile", m.GetUserProfileHandler())
	adminGroup.PUT("/users/:id/status", m.SetUserStatusHandler())
	adminGroup.GET("/organizations", m.ListOrganizationsHandler())
}
