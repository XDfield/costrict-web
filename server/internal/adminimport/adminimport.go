package adminimport

import (
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/costrict/costrict-web/server/internal/storage"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Module wires the admin import endpoints + the leader-elected background runner.
type Module struct {
	svc    *Service
	runner *ImportRunner
}

// New constructs the module around a shared DB handle, the catalog ingest service
// (reused from the migrate CLI path), and the storage backend for uploaded bundles.
func New(db *gorm.DB, ingest *services.CatalogIngestService, backend storage.Backend) *Module {
	return &Module{
		svc:    NewService(db, backend),
		runner: NewImportRunner(db, backend, ingest),
	}
}

// RegisterRoutes mounts the import endpoints. The provided group is expected to
// already enforce platform-admin auth (main.go's /admin group).
func (m *Module) RegisterRoutes(admin *gin.RouterGroup) {
	admin.POST("/import-jobs", m.CreateImportJobHandler())
	admin.GET("/import-jobs", m.ListImportJobsHandler())
	admin.GET("/import-jobs/:id", m.GetImportJobHandler())
	admin.POST("/import-jobs/:id/confirm", m.ConfirmImportJobHandler())
	admin.GET("/import-jobs/:id/errors.log", m.ImportJobErrorsLogHandler())
	admin.GET("/import-stats", m.ImportStatsHandler())
}

// Runner returns the leader-elected background runner so main.go can drive its
// lifecycle under leader election (Start on become-leader, Stop on lose-leader).
func (m *Module) Runner() *ImportRunner { return m.runner }
