package channel

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Module struct {
	Service *ChannelService
}

func New(db *gorm.DB, handler MessageHandler, cloudBaseURL string, enabledTypes []string) *Module {
	return &Module{
		Service: NewChannelService(db, handler, cloudBaseURL, enabledTypes),
	}
}

func (m *Module) RegisterRoutes(publicGroup *gin.RouterGroup, authedGroup *gin.RouterGroup) {
	publicGroup.GET("/webhooks/channels/:type", WebhookHandler(m.Service))
	publicGroup.POST("/webhooks/channels/:type", WebhookHandler(m.Service))

	channels := authedGroup.Group("/channels")
	{
		channels.GET("/available", ListAvailableTypesHandler(m.Service))
		channels.GET("", ListConfigsHandler(m.Service))
		channels.POST("", CreateConfigHandler(m.Service))
		channels.GET("/:id", GetConfigHandler(m.Service))
		channels.PUT("/:id", UpdateConfigHandler(m.Service))
		channels.DELETE("/:id", DeleteConfigHandler(m.Service))
		channels.POST("/:id/test", TestConfigHandler(m.Service))
		channels.POST("/wechat/login/qrcode", WeChatLoginQRCodeHandler(m.Service))
		channels.GET("/wechat/login/status", WeChatLoginStatusHandler(m.Service))
	}
}
