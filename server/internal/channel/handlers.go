package channel

import (
	"encoding/json"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
)

func channelResponse(svc *ChannelService, ch models.ChannelConfig) gin.H {
	masked := getConfigWithMask(ch)
	webhookURL := svc.GetWebhookURL(ch.ChannelType)
	return gin.H{
		"channel":    masked,
		"webhookUrl": webhookURL,
	}
}

func channelsResponse(svc *ChannelService, configs []models.ChannelConfig) gin.H {
	masked := make([]models.ChannelConfig, len(configs))
	for i, cfg := range configs {
		masked[i] = getConfigWithMask(cfg)
	}
	return gin.H{"channels": masked}
}

func WebhookHandler(svc *ChannelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		channelType := c.Param("type")
		body, statusCode, err := svc.HandleWebhook(channelType, c.Request)
		if err != nil {
			c.String(statusCode, err.Error())
			return
		}
		c.String(statusCode, body)
	}
}

func ListAvailableTypesHandler(svc *ChannelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"channelTypes": svc.GetAvailableChannelTypes()})
	}
}

func ListConfigsHandler(svc *ChannelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		configs, err := svc.ListConfigs(userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list channels"})
			return
		}

		c.JSON(http.StatusOK, channelsResponse(svc, configs))
	}
}

func CreateConfigHandler(svc *ChannelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var req struct {
			ChannelType string          `json:"channelType" binding:"required"`
			Name        string          `json:"name" binding:"required"`
			Config      json.RawMessage `json:"config" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		ch, err := svc.CreateConfig(userID, req.ChannelType, req.Name, req.Config)
		if err != nil {
			code := http.StatusBadRequest
			if err.Error() == "failed to create channel config" {
				code = http.StatusInternalServerError
			}
			c.JSON(code, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusCreated, channelResponse(svc, *ch))
	}
}

func GetConfigHandler(svc *ChannelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		ch, err := svc.GetConfig(userID, c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
			return
		}

		c.JSON(http.StatusOK, channelResponse(svc, *ch))
	}
}

func UpdateConfigHandler(svc *ChannelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var req struct {
			Name    string          `json:"name"`
			Config  json.RawMessage `json:"config"`
			Enabled *bool           `json:"enabled"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		updates := map[string]any{}
		if req.Name != "" {
			updates["name"] = req.Name
		}
		if len(req.Config) > 0 {
			updates["config"] = req.Config
		}
		if req.Enabled != nil {
			updates["enabled"] = *req.Enabled
		}

		ch, err := svc.UpdateConfig(userID, c.Param("id"), updates)
		if err != nil {
			code := http.StatusBadRequest
			if err.Error() == "channel config not found" {
				code = http.StatusNotFound
			}
			c.JSON(code, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, channelResponse(svc, *ch))
	}
}

func DeleteConfigHandler(svc *ChannelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		if err := svc.DeleteConfig(userID, c.Param("id")); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

func TestConfigHandler(svc *ChannelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		if err := svc.SendTestMessage(userID, c.Param("id")); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

func WeChatLoginQRCodeHandler(svc *ChannelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		qrcodeID, qrcodeImage, err := svc.GetQRCode(c.Request.Context(), "wechat")
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"qrcode":         qrcodeID,
			"qrcodeImageUrl": qrcodeImage,
		})
	}
}

func WeChatLoginStatusHandler(svc *ChannelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		qrcode := c.Query("qrcode")
		if qrcode == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "qrcode parameter is required"})
			return
		}

		status, token, err := svc.GetLoginStatus(c.Request.Context(), "wechat", qrcode)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}

		resp := gin.H{"status": status}
		if status == "confirmed" && token != "" {
			resp["token"] = token
		}
		c.JSON(http.StatusOK, resp)
	}
}
