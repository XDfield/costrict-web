package settings

import (
	"github.com/costrict/costrict-web/server/internal/systemrole"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Module wires the system-settings endpoints. Mirrors the internal/enterprise
// three-layer structure (Module / RegisterRoutes / handlers / service).
type Module struct {
	Service *Service
	db      *gorm.DB
}

func New(db *gorm.DB) *Module {
	return &Module{Service: NewService(db), db: db}
}

// RegisterRoutes mounts the system-settings endpoints under the authed /api
// group. All endpoints require platform admin (settings are global / system
// level), matching the rest of the M5 admin surface.
func (m *Module) RegisterRoutes(apiGroup *gin.RouterGroup) {
	admin := apiGroup.Group("/admin/settings")
	admin.Use(systemrole.RequirePlatformAdmin(m.db))
	{
		admin.GET("", ListSettingsHandler(m.Service))
		admin.PUT("/:key", UpdateSettingHandler(m.Service))
	}
}
