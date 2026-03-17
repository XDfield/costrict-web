package internal

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func SetupRouter(manager *TunnelManager, cfg *Config) *gin.Engine {
	r := gin.Default()

	// Health check endpoint for Kubernetes probes
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	r.GET("/device/:deviceID/tunnel", DeviceTunnelHandler(manager, cfg))
	r.Any("/device/:deviceID/proxy/*path", DeviceProxyHandler(manager))

	return r
}
