package internal

import "github.com/gin-gonic/gin"

func SetupRouter(manager *TunnelManager, cfg *Config) *gin.Engine {
	r := gin.Default()

	r.GET("/device/:deviceID/tunnel", DeviceTunnelHandler(manager, cfg))
	r.Any("/device/:deviceID/proxy/*path", DeviceProxyHandler(manager))

	return r
}
