package audit

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Module wires the audit-log query endpoint. Mirrors the internal/enterprise and
// internal/settings module structure. The write path is the package-level Logger
// (Init/Record), not this module.
//
// NOTE: this package does not depend on internal/systemrole — management
// handlers (enterprise, systemrole, …) call audit.Record, so audit must sit
// below them in the import graph. The platform-admin guard is therefore applied
// by the caller (main.go mounts RegisterRoutes onto an already-guarded /admin
// group), not here.
type Module struct {
	Service *Service
	db      *gorm.DB
}

func NewModule(db *gorm.DB) *Module {
	return &Module{Service: NewService(db), db: db}
}

// RegisterRoutes mounts the audit-log query endpoint. The provided group is
// expected to already enforce platform-admin auth (e.g. main.go's /admin group).
func (m *Module) RegisterRoutes(adminGroup *gin.RouterGroup) {
	adminGroup.GET("/audit-logs", ListAuditLogsHandler(m.Service))
}
