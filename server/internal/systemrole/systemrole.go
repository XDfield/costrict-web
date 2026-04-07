package systemrole

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Module struct {
	Service *SystemRoleService
	db      *gorm.DB
}

func New(db *gorm.DB) *Module {
	return &Module{Service: NewSystemRoleService(db), db: db}
}

func (m *Module) RegisterRoutes(apiGroup *gin.RouterGroup) {
	admin := apiGroup.Group("/admin/system-roles")
	admin.Use(RequirePlatformAdmin(m.db))
	{
		admin.GET("", ListUsersBySystemRoleHandler(m.Service))
		admin.GET("/users/:userId", GetUserSystemRolesHandler(m.Service))
		admin.POST("/users/:userId", GrantSystemRoleHandler(m.Service))
		admin.DELETE("/users/:userId/:role", RevokeSystemRoleHandler(m.Service))
	}

	auth := apiGroup.Group("/auth/system-roles")
	{
		auth.GET("/me", GetMySystemRolesHandler(m.Service))
	}
}
