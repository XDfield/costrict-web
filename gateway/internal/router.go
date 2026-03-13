package internal

import "github.com/gin-gonic/gin"

func SetupRouter(manager *ConnectionManager, cfg *Config) *gin.Engine {
	r := gin.Default()

	r.GET("/device/:deviceID/event", DeviceSSEHandler(manager, cfg))
	r.POST("/internal/device/:deviceID/send", SendToDeviceHandler(manager))

	return r
}
