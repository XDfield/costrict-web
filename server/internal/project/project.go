package project

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/costrict/costrict-web/server/internal/notification"
)

type Module struct {
	Service *ProjectService
}

func New(db *gorm.DB, notificationSvc *notification.NotificationService) *Module {
	return &Module{Service: NewProjectService(db, notificationSvc)}
}

func (m *Module) RegisterRoutes(apiGroup *gin.RouterGroup) {
	projects := apiGroup.Group("/projects")
	{
		projects.GET("", ListProjectsHandler(m.Service))
		projects.POST("", CreateProjectHandler(m.Service))
		projects.GET("/:id", GetProjectHandler(m.Service))
		projects.PUT("/:id", UpdateProjectHandler(m.Service))
		projects.DELETE("/:id", DeleteProjectHandler(m.Service))
		projects.POST("/:id/archive", ArchiveProjectHandler(m.Service))
		projects.POST("/:id/unarchive", UnarchiveProjectHandler(m.Service))
		projects.GET("/:id/members", ListMembersHandler(m.Service))
		projects.DELETE("/:id/members/:userId", RemoveMemberHandler(m.Service))
		projects.PUT("/:id/members/:userId/role", UpdateMemberRoleHandler(m.Service))
		projects.POST("/:id/invitations", CreateInvitationHandler(m.Service))
		projects.GET("/:id/invitations", ListInvitationsHandler(m.Service))
	}

	invitations := apiGroup.Group("/invitations")
	{
		invitations.GET("", ListMyInvitationsHandler(m.Service))
		invitations.POST("/:id/respond", RespondInvitationHandler(m.Service))
		invitations.DELETE("/:id", CancelInvitationHandler(m.Service))
	}
}
