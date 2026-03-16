package gateway

import (
	"net/http"

	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

func GatewayRegisterHandler(registry *GatewayRegistry) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			GatewayID   string `json:"gatewayID" binding:"required"`
			Endpoint    string `json:"endpoint" binding:"required"`
			InternalURL string `json:"internalURL" binding:"required"`
			Region      string `json:"region"`
			Capacity    int    `json:"capacity"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		info := &GatewayInfo{
			ID:          body.GatewayID,
			Endpoint:    body.Endpoint,
			InternalURL: body.InternalURL,
			Region:      body.Region,
			Capacity:    body.Capacity,
		}
		if err := registry.Register(info); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to register gateway"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "heartbeatInterval": 30})
	}
}

func GatewayHeartbeatHandler(registry *GatewayRegistry) gin.HandlerFunc {
	return func(c *gin.Context) {
		gatewayID := c.Param("gatewayID")

		var body struct {
			CurrentConns int `json:"currentConns"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		if err := registry.Heartbeat(gatewayID, body.CurrentConns); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "gateway not found"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "serverEpoch": registry.Epoch()})
	}
}

func DeviceOnlineHandler(registry *GatewayRegistry, deviceSvc *services.DeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			DeviceID  string `json:"deviceID" binding:"required"`
			GatewayID string `json:"gatewayID" binding:"required"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		registry.BindDevice(body.DeviceID, body.GatewayID)
		_ = deviceSvc.SetOnline(body.DeviceID)

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

func DeviceOfflineHandler(registry *GatewayRegistry, deviceSvc *services.DeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			DeviceID  string `json:"deviceID" binding:"required"`
			GatewayID string `json:"gatewayID" binding:"required"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		registry.UnbindDevice(body.DeviceID)
		_ = deviceSvc.SetOffline(body.DeviceID)

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

func GatewayAssignHandler(registry *GatewayRegistry) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			DeviceID string `json:"deviceID" binding:"required"`
			Region   string `json:"region"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		gw, err := registry.Allocate(body.Region)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no gateway available"})
			return
		}

		c.JSON(http.StatusOK, DeviceAllocation{
			GatewayID:  gw.ID,
			GatewayURL: gw.Endpoint,
		})
	}
}

func DeviceProxyHandler(registry *GatewayRegistry, client *Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		deviceID := c.Param("deviceID")

		gw, err := registry.GetDeviceGateway(deviceID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "device not connected"})
			return
		}

		c.Request.URL.Path = c.Param("path")
		if err := client.ProxyRequest(gw.InternalURL, deviceID, c.Request, c.Writer); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		}
	}
}

func RegisterInternalRoutes(group *gin.RouterGroup, registry *GatewayRegistry, deviceSvc *services.DeviceService) {
	gatewayGroup := group.Group("/gateway")
	gatewayGroup.POST("/register", GatewayRegisterHandler(registry))
	gatewayGroup.POST("/:gatewayID/heartbeat", GatewayHeartbeatHandler(registry))
	gatewayGroup.POST("/device/online", DeviceOnlineHandler(registry, deviceSvc))
	gatewayGroup.POST("/device/offline", DeviceOfflineHandler(registry, deviceSvc))
}
