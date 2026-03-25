package gateway

import (
	"net/http"
	"strings"

	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

// GatewayRegisterHandler godoc
// @Summary      Register gateway
// @Description  Register a new gateway instance with the server. The gateway receives a heartbeat interval in the response.
// @Tags         internal-gateway
// @Accept       json
// @Produce      json
// @Param        X-Internal-Secret  header  string  true  "Internal shared secret"
// @Param        body  body  object{gatewayID=string,endpoint=string,internalURL=string,region=string,capacity=integer}  true  "Gateway registration data"
// @Success      200  {object}  object{success=boolean,heartbeatInterval=integer}
// @Failure      400  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /internal/gateway/register [post]
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

// GatewayHeartbeatHandler godoc
// @Summary      Gateway heartbeat
// @Description  Report gateway health and current connection count. Returns the current server epoch for consistency checks.
// @Tags         internal-gateway
// @Accept       json
// @Produce      json
// @Param        X-Internal-Secret  header  string  true   "Internal shared secret"
// @Param        gatewayID          path    string  true   "Gateway ID"
// @Param        body  body  object{currentConns=integer}  true  "Heartbeat data"
// @Success      200  {object}  object{success=boolean,serverEpoch=integer}
// @Failure      400  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /internal/gateway/{gatewayID}/heartbeat [post]
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

// DeviceOnlineHandler godoc
// @Summary      Device online
// @Description  Notify the server that a device has connected to a gateway. Binds the device to the gateway and marks it online.
// @Tags         internal-gateway
// @Accept       json
// @Produce      json
// @Param        X-Internal-Secret  header  string  true  "Internal shared secret"
// @Param        body  body  object{deviceID=string,gatewayID=string}  true  "Device online event"
// @Success      200  {object}  object{success=boolean}
// @Failure      400  {object}  object{error=string}
// @Router       /internal/gateway/device/online [post]
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

// DeviceOfflineHandler godoc
// @Summary      Device offline
// @Description  Notify the server that a device has disconnected from a gateway. Unbinds the device and marks it offline.
// @Tags         internal-gateway
// @Accept       json
// @Produce      json
// @Param        X-Internal-Secret  header  string  true  "Internal shared secret"
// @Param        body  body  object{deviceID=string,gatewayID=string}  true  "Device offline event"
// @Success      200  {object}  object{success=boolean}
// @Failure      400  {object}  object{error=string}
// @Router       /internal/gateway/device/offline [post]
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

// GatewayAssignHandler godoc
// @Summary      Assign gateway to device
// @Description  Allocate an available gateway for a device. Requires a valid device Bearer token. Verifies that the token matches the claimed deviceID. If a version is provided and differs from the stored value, the device version is updated.
// @Tags         cloud
// @Accept       json
// @Produce      json
// @Param        Authorization  header  string  true  "Device Bearer token"
// @Param        body  body  object{deviceID=string,region=string,version=string}  true  "Assignment request"
// @Success      200  {object}  object{gatewayID=string,gatewayURL=string}
// @Failure      400  {object}  object{error=string}
// @Failure      401  {object}  object{error=string}
// @Failure      503  {object}  object{error=string}
// @Router       /cloud/device/gateway-assign [post]
func GatewayAssignHandler(registry *GatewayRegistry, deviceSvc *services.DeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Authenticate device token
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "device token required"})
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")

		var body struct {
			DeviceID string `json:"deviceID" binding:"required"`
			Region   string `json:"region"`
			Version  string `json:"version"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		// Verify token matches the claimed deviceID
		device, err := deviceSvc.VerifyDeviceToken(token)
		if err != nil || device.DeviceID != body.DeviceID {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid device token"})
			return
		}

		// Update device version if provided and changed
		if body.Version != "" && body.Version != device.Version {
			_ = deviceSvc.UpdateVersion(device.DeviceID, body.Version)
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

// DeviceProxyHandler godoc
// @Summary      Proxy request to device
// @Description  Forward an HTTP request to a device via its connected gateway. Requires user authentication (RequireAuth middleware) and verifies that the authenticated user owns the device.
// @Tags         cloud
// @Produce      json
// @Security     BearerAuth
// @Param        deviceID  path  string  true  "Device ID"
// @Param        path      path  string  true  "Proxy path to forward"
// @Success      200  "Proxied response from the device"
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Failure      502  {object}  object{error=string}
// @Router       /cloud/device/{deviceID}/proxy/{path} [get]
// @Router       /cloud/device/{deviceID}/proxy/{path} [post]
// @Router       /cloud/device/{deviceID}/proxy/{path} [put]
// @Router       /cloud/device/{deviceID}/proxy/{path} [delete]
// @Router       /cloud/device/{deviceID}/proxy/{path} [patch]
func DeviceProxyHandler(registry *GatewayRegistry, client *Client, deviceSvc *services.DeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		deviceID := c.Param("deviceID")

		// Verify caller owns this device (RequireAuth middleware sets userId)
		userID := c.GetString("userId")
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		if _, err := deviceSvc.VerifyDeviceOwnership(deviceID, userID); err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "device does not belong to you"})
			return
		}

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

// GatewayDeregisterHandler godoc
// @Summary      Deregister gateway
// @Description  Remove a gateway instance from the registry.
// @Tags         internal-gateway
// @Produce      json
// @Param        X-Internal-Secret  header  string  true  "Internal shared secret"
// @Param        gatewayID          path    string  true  "Gateway ID"
// @Success      200  {object}  object{success=boolean}
// @Failure      500  {object}  object{error=string}
// @Router       /internal/gateway/{gatewayID} [delete]
func GatewayDeregisterHandler(registry *GatewayRegistry) gin.HandlerFunc {
	return func(c *gin.Context) {
		gatewayID := c.Param("gatewayID")
		if err := registry.Deregister(gatewayID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to deregister gateway"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

// DeviceVerifyTokenHandler godoc
// @Summary      Verify device token
// @Description  Verify that a device token is valid and matches the claimed deviceID. Used by gateways for internal authentication.
// @Tags         internal-gateway
// @Accept       json
// @Produce      json
// @Param        X-Internal-Secret  header  string  true  "Internal shared secret"
// @Param        body  body  object{deviceID=string,token=string}  true  "Verification request"
// @Success      200  {object}  object{valid=boolean,userID=string}
// @Failure      400  {object}  object{error=string}
// @Router       /internal/gateway/device/verify-token [post]
func DeviceVerifyTokenHandler(deviceSvc *services.DeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			DeviceID string `json:"deviceID" binding:"required"`
			Token    string `json:"token" binding:"required"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		device, err := deviceSvc.VerifyDeviceToken(body.Token)
		if err != nil || device.DeviceID != body.DeviceID {
			c.JSON(http.StatusOK, gin.H{"valid": false})
			return
		}

		c.JSON(http.StatusOK, gin.H{"valid": true, "userID": device.UserID})
	}
}

func RegisterInternalRoutes(group *gin.RouterGroup, registry *GatewayRegistry, deviceSvc *services.DeviceService) {
	gatewayGroup := group.Group("/gateway")
	gatewayGroup.POST("/register", GatewayRegisterHandler(registry))
	gatewayGroup.POST("/:gatewayID/heartbeat", GatewayHeartbeatHandler(registry))
	gatewayGroup.DELETE("/:gatewayID", GatewayDeregisterHandler(registry))
	gatewayGroup.POST("/device/online", DeviceOnlineHandler(registry, deviceSvc))
	gatewayGroup.POST("/device/offline", DeviceOfflineHandler(registry, deviceSvc))
	gatewayGroup.POST("/device/verify-token", DeviceVerifyTokenHandler(deviceSvc))
}
