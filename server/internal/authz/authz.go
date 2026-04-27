package authz

import (
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Module struct {
	Service *Service
}

func New(db *gorm.DB, roleProvider RoleProvider, capabilityProvider CapabilityProvider, casdoorEndpoint string, jwksProvider *middleware.JWKSProvider) (*Module, error) {
	svc, err := NewService(db, roleProvider, capabilityProvider, casdoorEndpoint, jwksProvider)
	if err != nil {
		return nil, err
	}
	return &Module{Service: svc}, nil
}

func (m *Module) RegisterAPIRoutes(apiGroup *gin.RouterGroup) {
	// Public authenticated endpoint for the frontend to fetch permissions.
	apiGroup.GET("/auth/permissions", GetUserPermissionsHandler(m.Service))
}

func (m *Module) RegisterInternalRoutes(internalGroup *gin.RouterGroup) {
	// Internal endpoint for gateway/services to verify a token against a resource.
	internalGroup.POST("/auth/verify", VerifyTokenHandler(m.Service))
}
