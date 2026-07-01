// Package adminitem wires the platform-admin content-management endpoints
// (M6 · 内容管理): a cross-registry item list, an across-author status switch
// (上下架), and an across-author delete.
//
// The HTTP handlers live in handlers.go; the data logic lives on Service
// (service.go). The platform-admin guard is applied by the caller (main.go
// mounts RegisterRoutes onto an already-guarded /admin group), matching the
// internal/adminuser and internal/audit module conventions.
package adminitem

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Module wires the admin content-management endpoints around a Service.
type Module struct {
	svc *Service
}

// New constructs the module around a fresh Service bound to db.
func New(db *gorm.DB) *Module {
	return &Module{svc: NewService(db)}
}

// RegisterRoutes mounts the content-management endpoints. The provided group is
// expected to already enforce platform-admin auth (e.g. main.go's /admin group).
func (m *Module) RegisterRoutes(adminGroup *gin.RouterGroup) {
	adminGroup.GET("/items", m.ListItemsHandler())
	adminGroup.GET("/items/export.csv", m.ExportItemsCSVHandler())
	adminGroup.PUT("/items/:id/status", m.SetItemStatusHandler())
	adminGroup.POST("/items/batch-delete", m.BatchDeleteItemsHandler())
	adminGroup.POST("/items/batch-status", m.BatchSetStatusHandler())
	adminGroup.DELETE("/items/:id", m.DeleteItemHandler())
}
