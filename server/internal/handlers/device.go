package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

// RegisterDeviceHandler godoc
// @Summary      Register a new device
// @Description  Register a device for the authenticated user
// @Tags         devices
// @Accept       json
// @Produce      json
// @Param        body  body      object{deviceId=string,displayName=string,platform=string,version=string,workspaceId=string}  true  "Device registration data"
// @Success      201   {object}  object{device=object,token=string}
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      409   {object}  object{error=string,device=object,token=string}  "Same user: returns existing device and token"
// @Failure      409   {object}  object{error=string,deviceId=string}             "Different user: unauthorized device registration"
// @Failure      500   {object}  object{error=string}
// @Router       /devices [post]
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
			if errors.Is(err, services.ErrDeviceOwnedByCaller) {
				// Same user re-registering: return existing device info with token
				c.JSON(http.StatusConflict, gin.H{
					"error":  "device already registered",
					"device": device,
					"token":  token,
				})
				return
			}
			if errors.Is(err, services.ErrDeviceAlreadyRegistered) {
				c.JSON(http.StatusConflict, gin.H{
					"error":    "unauthorized device registration: device already belongs to another user",
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

// ListDevicesHandler godoc
// @Summary      List user devices
// @Description  Get all devices registered by the authenticated user
// @Tags         devices
// @Produce      json
// @Success      200   {object}  object{devices=[]object}
// @Failure      401   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /devices [get]
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

// GetDeviceHandler godoc
// @Summary      Get device details
// @Description  Get details of a specific device
// @Tags         devices
// @Produce      json
// @Param        deviceID  path      string  true  "Device ID"
// @Success      200   {object}  object{device=object}
// @Failure      401   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /devices/{deviceID} [get]
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

// UpdateDeviceHandler godoc
// @Summary      Update device
// @Description  Update device information
// @Tags         devices
// @Accept       json
// @Produce      json
// @Param        deviceID  path      string  true  "Device ID"
// @Param        body      body      object{displayName=string,description=string,label=string,workspaceId=string}  true  "Device update data"
// @Success      200   {object}  object{device=object}
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /devices/{deviceID} [put]
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

// DeleteDeviceHandler godoc
// @Summary      Delete device
// @Description  Delete a device registration
// @Tags         devices
// @Param        deviceID  path      string  true  "Device ID"
// @Success      204
// @Failure      401   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /devices/{deviceID} [delete]
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

// RotateDeviceTokenHandler godoc
// @Summary      Rotate device token
// @Description  Rotate the authentication token for a device
// @Tags         devices
// @Produce      json
// @Param        deviceID  path      string  true  "Device ID"
// @Success      200   {object}  object{token=string,rotatedAt=string}
// @Failure      401   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /devices/{deviceID}/rotate-token [post]
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

// ListWorkspaceDevicesHandler godoc
// @Summary      List workspace devices
// @Description  Get all devices in a workspace with pagination
// @Tags         devices
// @Produce      json
// @Param        workspaceID  path      string  true   "Workspace ID"
// @Param        page         query     int     false  "Page number (default: 1)"
// @Param        pageSize     query     int     false  "Page size (default: 20, max: 100)"
// @Success      200   {object}  object{devices=[]object,total=integer,page=integer,pageSize=integer,hasMore=boolean}
// @Failure      401   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /workspaces/{workspaceID}/devices [get]
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
