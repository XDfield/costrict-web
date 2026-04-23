package kanban

import (
	"github.com/costrict/costrict-web/server/internal/authz"
	"github.com/gin-gonic/gin"
)

type Module struct{}

func New() *Module {
	return &Module{}
}

func (m *Module) RegisterRoutes(apiGroup *gin.RouterGroup, authzSvc *authz.Service) {
	kanban := apiGroup.Group("/kanban")
	kanban.Use(authz.RequirePermission(authzSvc, "api.kanban.overview"))
	{
		kanban.GET("/overview", GetOverviewHandler())
	}
}
