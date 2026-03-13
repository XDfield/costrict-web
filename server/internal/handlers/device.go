package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

func RegisterDeviceHandler(svc *services.DeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var req services.RegisterDeviceRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		device, token, err := svc.RegisterDevice(userID, req)
		if err != nil {
			if errors.Is(err, services.ErrDeviceAlreadyRegistered) {
				c.JSON(http.StatusConflict, gin.H{
					"error":    "device already registered",
					"deviceId": req.DeviceID,
				})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to register device"})
			return
		}

		c.JSON(http.StatusCreated, gin.H{
			"device": device,
			"token":  token,
		})
	}
}

func ListDevicesHandler(svc *services.DeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		devices, err := svc.ListDevices(userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list devices"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"devices": devices})
	}
}

func GetDeviceHandler(svc *services.DeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		deviceID := c.Param("deviceID")
		device, err := svc.GetDevice(deviceID, userID)
		if err != nil {
			if errors.Is(err, services.ErrDeviceNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "device not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get device"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"device": device})
	}
}

func UpdateDeviceHandler(svc *services.DeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		deviceID := c.Param("deviceID")

		var req services.UpdateDeviceRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		device, err := svc.UpdateDevice(deviceID, userID, req)
		if err != nil {
			if errors.Is(err, services.ErrDeviceNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "device not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update device"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"device": device})
	}
}

func DeleteDeviceHandler(svc *services.DeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		deviceID := c.Param("deviceID")
		if err := svc.DeleteDevice(deviceID, userID); err != nil {
			if errors.Is(err, services.ErrDeviceNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "device not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete device"})
			return
		}

		c.Status(http.StatusNoContent)
	}
}

func RotateDeviceTokenHandler(svc *services.DeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		deviceID := c.Param("deviceID")
		token, rotatedAt, err := svc.RotateToken(deviceID, userID)
		if err != nil {
			if errors.Is(err, services.ErrDeviceNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "device not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to rotate token"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"token":     token,
			"rotatedAt": rotatedAt,
		})
	}
}

func ListWorkspaceDevicesHandler(svc *services.DeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		workspaceID := c.Param("workspaceID")

		page := 1
		pageSize := 20
		if p := c.Query("page"); p != "" {
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				page = n
			}
		}
		if ps := c.Query("pageSize"); ps != "" {
			if n, err := strconv.Atoi(ps); err == nil && n > 0 && n <= 100 {
				pageSize = n
			}
		}

		devices, total, err := svc.ListWorkspaceDevices(workspaceID, userID, page, pageSize)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list workspace devices"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"devices":  devices,
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
			"hasMore":  int64(page*pageSize) < total,
		})
	}
}
