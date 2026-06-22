package authz

import (
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/systemrole"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Module struct {
	Service           *Service
	db                *gorm.DB
	systemRoleService *systemrole.SystemRoleService
}

func New(db *gorm.DB, roleProvider RoleProvider, capabilityProvider CapabilityProvider, casdoorEndpoint string, jwksProvider *middleware.JWKSProvider) (*Module, error) {
	svc, err := NewService(db, roleProvider, capabilityProvider, casdoorEndpoint, jwksProvider)
	if err != nil {
		return nil, err
	}
	return &Module{
		Service:           svc,
		db:                db,
		systemRoleService: systemrole.NewSystemRoleService(db),
	}, nil
}

func (m *Module) RegisterAPIRoutes(apiGroup *gin.RouterGroup) {
	// Public authenticated endpoint for the frontend to fetch permissions.
	apiGroup.GET("/auth/permissions", GetUserPermissionsHandler(m.Service))

	// Authenticated endpoint: the current user's metrics-dashboard visibility
	// scope (which department subtrees they may see). Any logged-in user may query
	// their own scope; reused by the metrics dashboard (指标看板) to enforce
	// "see only your own department subtree unless specially opened up".
	apiGroup.GET("/auth/dept-scope", GetUserScopeHandler(m.Service))
}

func (m *Module) RegisterAdminRoutes(adminGroup *gin.RouterGroup) {
	// Admin endpoint to grant a system role (module permission) to a user.
	adminGroup.POST("/permissions/users/:userId/grant", GrantUserRoleHandler(m.systemRoleService))

	// Resource permission matrix: list every resource permission and edit a
	// single resource's allowed roles (reloads the in-memory registry on write).
	adminGroup.GET("/resource-permissions", ListResourcePermissionsHandler(m.Service))
	adminGroup.PUT("/resource-permissions/:code", UpdateResourcePermissionHandler(m.Service))

	// Fine-grained permission grants (mentor RBAC): grant a permission code to a
	// user or department (department grants inherit to descendants by dept_path).
	adminGroup.GET("/permission-grants", ListPermissionGrantsHandler(m.Service))
	adminGroup.POST("/permission-grants", GrantPermissionHandler(m.Service))
	adminGroup.DELETE("/permission-grants/:id", RevokePermissionHandler(m.Service))
}

func (m *Module) RegisterInternalRoutes(internalGroup *gin.RouterGroup) {
	// Internal endpoint for gateway/services to verify a token against a resource.
	internalGroup.POST("/auth/verify", VerifyTokenHandler(m.Service))
}
