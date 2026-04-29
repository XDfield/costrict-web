package internal

import (
	"net/http"

	"github.com/costrict/costrict-web/gateway/internal/logger"
	"github.com/gin-gonic/gin"
)

// InternalSecretAuth validates requests from the API server using a shared secret.
// If secret is empty, all requests are rejected to prevent misconfiguration.
func InternalSecretAuth(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if secret == "" {
			logger.Error("[Gateway] INTERNAL_SECRET not configured, rejecting proxy request")
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "internal API not available"})
			return
		}

		provided := c.GetHeader(internalSecretHeader)
		if provided == "" || provided != secret {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "invalid internal secret"})
			return
		}

		c.Next()
	}
}

func SetupRouter(manager *TunnelManager, cfg *Config) *gin.Engine {
	r := gin.Default()

	// Health check endpoint for Kubernetes probes
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Status endpoint for runtime connection metrics
	r.GET("/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":             "ok",
			"gatewayId":          cfg.GatewayID,
			"currentConnections": manager.Count(),
			"capacity":           cfg.Capacity,
		})
	})

	// Device tunnel: authenticated by device token
	r.GET("/device/:deviceID/tunnel", DeviceTunnelHandler(manager, cfg))

	// Device proxy: only callable by API server via internal secret
	r.Any("/device/:deviceID/proxy/*path", InternalSecretAuth(cfg.InternalSecret), DeviceProxyHandler(manager))

	return r
}
