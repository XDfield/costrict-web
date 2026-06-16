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
}

func (m *Module) RegisterAdminRoutes(adminGroup *gin.RouterGroup) {
	// Admin endpoint to grant a system role (module permission) to a user.
	adminGroup.POST("/permissions/users/:userId/grant", GrantUserRoleHandler(m.systemRoleService))

	// Resource permission matrix: list every resource permission and edit a
	// single resource's allowed roles (reloads the in-memory registry on write).
	adminGroup.GET("/resource-permissions", ListResourcePermissionsHandler(m.Service))
	adminGroup.PUT("/resource-permissions/:code", UpdateResourcePermissionHandler(m.Service))
}

func (m *Module) RegisterInternalRoutes(internalGroup *gin.RouterGroup) {
	// Internal endpoint for gateway/services to verify a token against a resource.
	internalGroup.POST("/auth/verify", VerifyTokenHandler(m.Service))
}
