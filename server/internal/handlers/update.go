package handlers

import (
	"net/http"
	"strings"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

func UpdateCheckHandler(updateSvc *services.UpdateService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req services.UpdateCheckRequest
		if err := c.ShouldBindQuery(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "platform and version are required"})
			return
		}

		result, err := updateSvc.CheckForUpdate(req)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update check failed"})
			return
		}

		c.JSON(http.StatusOK, result)
	}
}

func DeviceHeartbeatHandler(deviceSvc *services.DeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "device token required"})
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")

		device, err := deviceSvc.VerifyDeviceToken(token)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid device token"})
			return
		}

		deviceID := c.Param("deviceID")
		if device.DeviceID != deviceID {
			c.JSON(http.StatusForbidden, gin.H{"error": "device token does not match"})
			return
		}

		var body struct {
			DeviceID string `json:"deviceID"`
			Version  string `json:"version"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		if err := deviceSvc.UpdateLastSeen(device.DeviceID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update last seen"})
			return
		}

		if body.Version != "" && body.Version != device.Version {
			_ = deviceSvc.UpdateVersion(device.DeviceID, body.Version)
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

func CreateReleaseHandler(updateSvc *services.UpdateService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var req services.CreateReleaseRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		releases, err := updateSvc.CreateRelease(userID, req)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create release"})
			return
		}

		c.JSON(http.StatusCreated, gin.H{"releases": releases})
	}
}
