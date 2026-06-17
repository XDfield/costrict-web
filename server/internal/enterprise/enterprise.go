package enterprise

import (
	"github.com/costrict/costrict-web/server/internal/systemrole"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Module struct {
	Service *Service
	db      *gorm.DB
}

func New(db *gorm.DB) *Module {
	return &Module{Service: NewService(db), db: db}
}

// RegisterRoutes wires the enterprise-customer endpoints under the authed /api
// group. The read endpoint is open to any authenticated user (the store renders
// big-customer branding for everyone); write endpoints require platform admin.
func (m *Module) RegisterRoutes(apiGroup *gin.RouterGroup) {
	apiGroup.GET("/enterprise-customers", ListEnterpriseCustomersHandler(m.Service))

	admin := apiGroup.Group("/admin/enterprise-customers")
	admin.Use(systemrole.RequirePlatformAdmin(m.db))
	{
		// Admin list returns raw universal_id + resolved members (who is configured);
		// the public GET above only exposes resolved subject_ids.
		admin.GET("", ListEnterpriseCustomersAdminHandler(m.Service))
		admin.POST("", CreateEnterpriseCustomerHandler(m.Service))
		admin.PUT("/:id", UpdateEnterpriseCustomerHandler(m.Service))
		admin.DELETE("/:id", DeleteEnterpriseCustomerHandler(m.Service))
	}
}
