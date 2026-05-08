package cloud

import (
	"github.com/costrict/costrict-web/server/internal/gateway"
	"github.com/costrict/costrict-web/server/internal/notification"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Module struct {
	Manager             *ConnectionManager
	Router              *EventRouter
	NotificationService *notification.NotificationService
	DB                  *gorm.DB
}

func New(gatewayRegistry *gateway.GatewayRegistry, gatewayClient *gateway.Client) *Module {
	manager := NewConnectionManager()
	router := NewEventRouter(manager, gatewayRegistry, gatewayClient)
	return &Module{
		Manager: manager,
		Router:  router,
	}
}

func (m *Module) RegisterRoutes(cloudGroup *gin.RouterGroup, deviceSvc *services.DeviceService, casdoorEndpoint string) {
	cloudGroup.GET("/workspace/:workspaceID/event", UserSSEHandler(m.Manager))
	cloudGroup.POST("/session/:sessionID/subscribe", SubscribeHandler(m.Manager))
	cloudGroup.POST("/session/:sessionID/unsubscribe", UnsubscribeHandler(m.Manager))
	cloudGroup.POST("/event", DeviceEventHandler(m.Router))
	cloudGroup.POST("/command", UserCommandHandler(m.Router))
	cloudGroup.GET("/stats", StatsHandler(m.Manager))

	_ = casdoorEndpoint
}

func (m *Module) RegisterDeviceRoutes(cloudGroup *gin.RouterGroup, deviceSvc *services.DeviceService) {
	cloudGroup.POST("/device/notify", DeviceNotifyHandler(m.Manager, deviceSvc, m.NotificationService))
	cloudGroup.POST("/devices/:deviceID/commands/:commandID/result", DeviceCommandResultHandler(m.Manager, deviceSvc, m.DB))
}
