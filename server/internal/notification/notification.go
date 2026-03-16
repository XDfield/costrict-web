package notification

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Module struct {
	Service *NotificationService
}

func New(db *gorm.DB, cloudBaseURL string) *Module {
	return &Module{
		Service: NewNotificationService(db, cloudBaseURL),
	}
}

func (m *Module) RegisterRoutes(apiGroup *gin.RouterGroup) {
	admin := apiGroup.Group("/admin/notification-channels")
	{
		admin.GET("", AdminListSystemChannelsHandler(m.Service))
		admin.POST("", AdminCreateSystemChannelHandler(m.Service))
		admin.PUT("/:id", AdminUpdateSystemChannelHandler(m.Service))
		admin.DELETE("/:id", AdminDeleteSystemChannelHandler(m.Service))
	}

	channels := apiGroup.Group("/notification-channels")
	{
		channels.GET("/available", ListAvailableTypesHandler(m.Service))
		channels.GET("", ListMyChannelsHandler(m.Service))
		channels.POST("", CreateMyChannelHandler(m.Service))
		channels.GET("/:id", GetMyChannelHandler(m.Service))
		channels.PUT("/:id", UpdateMyChannelHandler(m.Service))
		channels.DELETE("/:id", DeleteMyChannelHandler(m.Service))
		channels.POST("/:id/test", TestMyChannelHandler(m.Service))
		channels.GET("/:id/logs", ListLogsHandler(m.Service))
	}
}
