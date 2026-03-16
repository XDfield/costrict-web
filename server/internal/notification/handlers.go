package notification

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/notification/sender"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"gorm.io/datatypes"
)

func marshalJSON(v any) (datatypes.JSON, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(b), nil
}

// --- 管理员：系统渠道管理 ---

func AdminListSystemChannelsHandler(svc *NotificationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var channels []models.SystemNotificationChannel
		if err := svc.db.Where("deleted_at IS NULL").Find(&channels).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list channels"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"channels": channels})
	}
}

func AdminCreateSystemChannelHandler(svc *NotificationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var req struct {
			Type         string          `json:"type" binding:"required"`
			Name         string          `json:"name" binding:"required"`
			WorkspaceID  string          `json:"workspaceId"`
			SystemConfig json.RawMessage `json:"systemConfig"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		if _, ok := sender.Get(req.Type); !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported channel type: " + req.Type})
			return
		}

		systemConfig := datatypes.JSON(req.SystemConfig)
		if len(systemConfig) == 0 {
			systemConfig = datatypes.JSON("{}")
		}

		ch := models.SystemNotificationChannel{
			Type:         req.Type,
			Name:         req.Name,
			WorkspaceID:  req.WorkspaceID,
			Enabled:      true,
			SystemConfig: systemConfig,
			CreatedBy:    userID,
		}

		if err := svc.db.Create(&ch).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create channel"})
			return
		}

		c.JSON(http.StatusCreated, gin.H{"channel": ch})
	}
}

func AdminUpdateSystemChannelHandler(svc *NotificationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		channelID := c.Param("id")
		var ch models.SystemNotificationChannel
		if err := svc.db.Where("id = ? AND deleted_at IS NULL", channelID).First(&ch).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
			return
		}

		var req struct {
			Name         string          `json:"name"`
			Enabled      *bool           `json:"enabled"`
			SystemConfig json.RawMessage `json:"systemConfig"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		updates := map[string]any{}
		if req.Name != "" {
			updates["name"] = req.Name
		}
		if req.Enabled != nil {
			updates["enabled"] = *req.Enabled
		}
		if len(req.SystemConfig) > 0 {
			updates["system_config"] = datatypes.JSON(req.SystemConfig)
		}

		if err := svc.db.Model(&ch).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update channel"})
			return
		}

		_ = userID
		c.JSON(http.StatusOK, gin.H{"channel": ch})
	}
}

func AdminDeleteSystemChannelHandler(svc *NotificationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		channelID := c.Param("id")
		var ch models.SystemNotificationChannel
		if err := svc.db.Where("id = ? AND deleted_at IS NULL", channelID).First(&ch).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
			return
		}

		if err := svc.db.Delete(&ch).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete channel"})
			return
		}

		_ = userID
		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

// --- 用户：可用渠道类型查询 ---

func ListAvailableTypesHandler(svc *NotificationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"channelTypes": svc.GetAvailableChannelTypes()})
	}
}

// --- 用户：自己的渠道配置管理 ---

func ListMyChannelsHandler(svc *NotificationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var channels []models.UserNotificationChannel
		if err := svc.db.Where("user_id = ? AND deleted_at IS NULL", userID).
			Find(&channels).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list channels"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"channels": channels})
	}
}

func CreateMyChannelHandler(svc *NotificationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var req struct {
			SystemChannelID string          `json:"systemChannelId"`
			ChannelType     string          `json:"channelType" binding:"required"`
			Name            string          `json:"name" binding:"required"`
			UserConfig      json.RawMessage `json:"userConfig" binding:"required"`
			TriggerEvents   []string        `json:"triggerEvents"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		snd, ok := sender.Get(req.ChannelType)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported channel type: " + req.ChannelType})
			return
		}

		if err := snd.ValidateUserConfig(req.UserConfig); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if req.SystemChannelID != "" {
			var sc models.SystemNotificationChannel
			if err := svc.db.Where("id = ? AND enabled = true AND deleted_at IS NULL", req.SystemChannelID).
				First(&sc).Error; err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "system channel not found or disabled"})
				return
			}
		}

		ch := models.UserNotificationChannel{
			UserID:          userID,
			SystemChannelID: req.SystemChannelID,
			ChannelType:     req.ChannelType,
			Name:            req.Name,
			Enabled:         true,
			UserConfig:      datatypes.JSON(req.UserConfig),
			TriggerEvents:   pq.StringArray(req.TriggerEvents),
		}

		if err := svc.db.Create(&ch).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create channel"})
			return
		}

		c.JSON(http.StatusCreated, gin.H{"channel": ch})
	}
}

func GetMyChannelHandler(svc *NotificationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		channelID := c.Param("id")
		var ch models.UserNotificationChannel
		if err := svc.db.Where("id = ? AND user_id = ? AND deleted_at IS NULL", channelID, userID).
			First(&ch).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "notification channel not found"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"channel": ch})
	}
}

func UpdateMyChannelHandler(svc *NotificationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		channelID := c.Param("id")
		var ch models.UserNotificationChannel
		if err := svc.db.Where("id = ? AND user_id = ? AND deleted_at IS NULL", channelID, userID).
			First(&ch).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "notification channel not found"})
			return
		}

		var req struct {
			Name          string          `json:"name"`
			UserConfig    json.RawMessage `json:"userConfig"`
			TriggerEvents []string        `json:"triggerEvents"`
			Enabled       *bool           `json:"enabled"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		updates := map[string]any{}
		if req.Name != "" {
			updates["name"] = req.Name
		}
		if len(req.UserConfig) > 0 {
			snd, ok := sender.Get(ch.ChannelType)
			if !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported channel type"})
				return
			}
			if err := snd.ValidateUserConfig(req.UserConfig); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			updates["user_config"] = datatypes.JSON(req.UserConfig)
		}
		if req.TriggerEvents != nil {
			updates["trigger_events"] = pq.StringArray(req.TriggerEvents)
		}
		if req.Enabled != nil {
			updates["enabled"] = *req.Enabled
		}

		if err := svc.db.Model(&ch).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update channel"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"channel": ch})
	}
}

func DeleteMyChannelHandler(svc *NotificationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		channelID := c.Param("id")
		var ch models.UserNotificationChannel
		if err := svc.db.Where("id = ? AND user_id = ? AND deleted_at IS NULL", channelID, userID).
			First(&ch).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "notification channel not found"})
			return
		}

		if err := svc.db.Delete(&ch).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete channel"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

func TestMyChannelHandler(svc *NotificationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		channelID := c.Param("id")
		if err := svc.SendTest(channelID, userID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

func ListLogsHandler(svc *NotificationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		channelID := c.Param("id")
		limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))

		logs, err := svc.ListLogs(channelID, userID, limit)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"logs": logs})
	}
}
