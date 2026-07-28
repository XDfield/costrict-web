// Package adminuser exposes the platform-admin member-management surface
// (M1 · 成员管理): a paginated/searchable/status-filtered user list, an
// account-status switch, an organization roll-up, and a per-user profile.
//
// Per the admin-user-migration slice (option A full migration), cs-user is the
// single source of truth for user identity + status. The handlers proxy
// identity/status reads + writes to cs-user via *userpkg.RPCClient
// (Commit 6); activity counts (capability_items, item_distributions) and
// per-user system roles still come from local costrict_db because those tables
// are not replicated to cs_user.
//
// The platform-admin guard is applied by the caller (main.go mounts
// RegisterRoutes onto an already-guarded /admin group), matching the
// internal/audit module convention.
package adminuser

import (
	"context"

	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// AdminUserRPC is the cs-user admin surface the module depends on. The
// production implementation is *userpkg.RPCClient; declaring it as an
// interface lets tests substitute a stub without spinning up an HTTP server.
// Method set mirrors Commit 6's RPCClient exactly.
type AdminUserRPC interface {
	Configured() bool
	ListUsers(ctx context.Context, p userpkg.AdminUserListParams) (*userpkg.AdminUserListResult, error)
	SetUserStatus(ctx context.Context, subjectID, status, operatorID string) (*userpkg.AdminSetUserStatusResult, error)
	ListOrganizations(ctx context.Context) ([]userpkg.AdminOrganization, error)
	GetUserProfile(ctx context.Context, subjectID string) (*userpkg.AdminUserProfile, error)
	// R5 — admin override (mutates username + display_name).
	AdminUpdateProfile(ctx context.Context, subjectID string, args userpkg.AdminUpdateProfileArgs) (*userpkg.AdminUserProfile, error)
}

// Module wires the admin member-management endpoints. It holds two deps:
//
//   - rpc: cs-user RPC client for identity + status (Commit 6 surface). May be
//     nil when USER_SERVICE_BACKEND != rpc; handlers return 503 in that case.
//   - users: local UserService for activity counts (capability_items /
//     item_distributions / item_distribution_receipts) and batch role lookup
//     (user_system_roles). These tables live in costrict_db and are not
//     replicated to cs_user, so the cross-DB join is split between the two
//     services per ADR D1.
type Module struct {
	rpc   AdminUserRPC
	users *userpkg.UserService
}

// New constructs the module. rpc may be nil if cs-user RPC is not configured;
// handlers degrade to 503 in that mode. users must be non-nil — activity
// counts and roles are always local.
func New(rpc AdminUserRPC, users *userpkg.UserService) *Module {
	return &Module{rpc: rpc, users: users}
}

// RegisterRoutes mounts the member-management endpoints. The provided group is
// expected to already enforce platform-admin auth (e.g. main.go's /admin group).
func (m *Module) RegisterRoutes(adminGroup *gin.RouterGroup) {
	adminGroup.GET("/users", m.ListUsersHandler())
	adminGroup.GET("/users/:id/profile", m.GetUserProfileHandler())
	adminGroup.PUT("/users/:id/profile", m.UpdateProfileHandler())
	adminGroup.PUT("/users/:id/status", m.SetUserStatusHandler())
	adminGroup.GET("/organizations", m.ListOrganizationsHandler())
}
