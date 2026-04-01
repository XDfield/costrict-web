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

// AdminListSystemChannelsHandler godoc
//
//	@Summary		List system notification channels
//	@Description	Get all system notification channels (admin only)
//	@Tags			admin/notification-channels
//	@Produce		json
//	@Success		200	{object}	object{channels=[]models.SystemNotificationChannel}
//	@Failure		401	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/admin/notification-channels [get]
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

// AdminCreateSystemChannelHandler godoc
//
//	@Summary		Create system notification channel
//	@Description	Create a new system notification channel (admin only)
//	@Tags			admin/notification-channels
//	@Accept			json
//	@Produce		json
//	@Param			body	body		object{type=string,name=string,workspaceId=string,systemConfig=object}	true	"Channel data"
//	@Success		201		{object}	object{channel=models.SystemNotificationChannel}
//	@Failure		400		{object}	object{error=string}
//	@Failure		401		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/admin/notification-channels [post]
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

// AdminUpdateSystemChannelHandler godoc
//
//	@Summary		Update system notification channel
//	@Description	Update a system notification channel (admin only)
//	@Tags			admin/notification-channels
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string													true	"Channel ID"
//	@Param			body	body		object{name=string,enabled=bool,systemConfig=object}	true	"Update data"
//	@Success		200		{object}	object{channel=models.SystemNotificationChannel}
//	@Failure		400		{object}	object{error=string}
//	@Failure		401		{object}	object{error=string}
//	@Failure		404		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/admin/notification-channels/{id} [put]
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

// AdminDeleteSystemChannelHandler godoc
//
//	@Summary		Delete system notification channel
//	@Description	Delete a system notification channel (admin only)
//	@Tags			admin/notification-channels
//	@Produce		json
//	@Param			id	path		string	true	"Channel ID"
//	@Success		200	{object}	object{success=bool}
//	@Failure		401	{object}	object{error=string}
//	@Failure		404	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/admin/notification-channels/{id} [delete]
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

// ListAvailableTypesHandler godoc
//
//	@Summary		List available notification channel types
//	@Description	Get all available notification channel types with their config schemas
//	@Tags			notification-channels
//	@Produce		json
//	@Success		200	{object}	object{channelTypes=[]object}
//	@Failure		401	{object}	object{error=string}
//	@Router			/notification-channels/available [get]
func ListAvailableTypesHandler(svc *NotificationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"channelTypes": svc.GetAvailableChannelTypes(), "supportedEvents": svc.GetSupportedTriggerEvents()})
	}
}

// --- 用户：自己的渠道配置管理 ---

// ListMyChannelsHandler godoc
//
//	@Summary		List user notification channels
//	@Description	Get all notification channels configured by the authenticated user
//	@Tags			notification-channels
//	@Produce		json
//	@Success		200	{object}	object{channels=[]models.UserNotificationChannel}
//	@Failure		401	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/notification-channels [get]
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

// CreateMyChannelHandler godoc
//
//	@Summary		Create user notification channel
//	@Description	Create a new notification channel for the authenticated user
//	@Tags			notification-channels
//	@Accept			json
//	@Produce		json
//	@Param			body	body		object{systemChannelId=string,channelType=string,name=string,userConfig=object,triggerEvents=[]string}	true	"Channel data"
//	@Success		201		{object}	object{channel=models.UserNotificationChannel}
//	@Failure		400		{object}	object{error=string}
//	@Failure		401		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/notification-channels [post]
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

		for _, event := range req.TriggerEvents {
			if !svc.IsSupportedTriggerEvent(event) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported trigger event: " + event})
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

// GetMyChannelHandler godoc
//
//	@Summary		Get notification channel details
//	@Description	Get details of a specific notification channel
//	@Tags			notification-channels
//	@Produce		json
//	@Param			id	path		string	true	"Channel ID"
//	@Success		200	{object}	object{channel=models.UserNotificationChannel}
//	@Failure		401	{object}	object{error=string}
//	@Failure		404	{object}	object{error=string}
//	@Router			/notification-channels/{id} [get]
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

// UpdateMyChannelHandler godoc
//
//	@Summary		Update notification channel
//	@Description	Update a notification channel configuration
//	@Tags			notification-channels
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string																		true	"Channel ID"
//	@Param			body	body		object{name=string,userConfig=object,triggerEvents=[]string,enabled=bool}	true	"Update data"
//	@Success		200		{object}	object{channel=models.UserNotificationChannel}
//	@Failure		400		{object}	object{error=string}
//	@Failure		401		{object}	object{error=string}
//	@Failure		404		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/notification-channels/{id} [put]
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
			for _, event := range req.TriggerEvents {
				if !svc.IsSupportedTriggerEvent(event) {
					c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported trigger event: " + event})
					return
				}
			}
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

// DeleteMyChannelHandler godoc
//
//	@Summary		Delete notification channel
//	@Description	Delete a notification channel
//	@Tags			notification-channels
//	@Produce		json
//	@Param			id	path		string	true	"Channel ID"
//	@Success		200	{object}	object{success=bool}
//	@Failure		401	{object}	object{error=string}
//	@Failure		404	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/notification-channels/{id} [delete]
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

// TestMyChannelHandler godoc
//
//	@Summary		Test notification channel
//	@Description	Send a test notification to verify channel configuration
//	@Tags			notification-channels
//	@Produce		json
//	@Param			id	path		string	true	"Channel ID"
//	@Success		200	{object}	object{success=bool}
//	@Failure		401	{object}	object{error=string}
//	@Failure		404	{object}	object{error=string}
//	@Failure		500	{object}	object{success=bool,error=string}
//	@Router			/notification-channels/{id}/test [post]
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

// ListLogsHandler godoc
//
//	@Summary		List notification logs
//	@Description	Get notification sending logs for a specific channel
//	@Tags			notification-channels
//	@Produce		json
//	@Param			id		path		string	true	"Channel ID"
//	@Param			limit	query		int		false	"Number of logs to return (default 20)"
//	@Success		200		{object}	object{logs=[]models.NotificationLog}
//	@Failure		401		{object}	object{error=string}
//	@Failure		404		{object}	object{error=string}
//	@Router			/notification-channels/{id}/logs [get]
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
