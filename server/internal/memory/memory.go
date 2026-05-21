package memory

import (
	"github.com/costrict/costrict-web/server/internal/storage"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Module 记忆模块
type Module struct {
	Service *Service
}

// New 创建记忆模块
func New(db *gorm.DB, st storage.Backend) *Module {
	return &Module{Service: NewService(db, st)}
}

// RegisterRoutes 注册记忆模块路由
func (m *Module) RegisterRoutes(apiGroup *gin.RouterGroup) {
	memories := apiGroup.Group("/memories")
	{
		memories.GET("", ListMemoriesHandler(m.Service))
		memories.POST("", CreateMemoryHandler(m.Service))
		memories.GET("/:id", GetMemoryHandler(m.Service))
		memories.PUT("/:id", UpdateMemoryHandler(m.Service))
		memories.DELETE("/:id", DeleteMemoryHandler(m.Service))
		memories.GET("/:id/content", GetMemoryContentHandler(m.Service))
		memories.GET("/:id/versions", ListVersionsHandler(m.Service))
		memories.GET("/:id/versions/:version/content", GetVersionContentHandler(m.Service))
	}
}
