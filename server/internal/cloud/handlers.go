package cloud

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/costrict/costrict-web/server/internal/dispatcher"
	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/notification"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Notifier interface {
	Dispatch(input dispatcher.DispatchInput)
}

func UserSSEHandler(manager *ConnectionManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		workspaceID := c.Param("workspaceID")

		conn, err := manager.RegisterUserConnection(userID, workspaceID)
		if err != nil {
			if errors.Is(err, ErrConnectionLimitExceeded) {
				c.JSON(http.StatusTooManyRequests, gin.H{"error": "connection limit exceeded", "limit": MaxConnectionsPerUser})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to register connection"})
			return
		}

		setSseHeaders(c)

		w := c.Writer
		writeSseEvent(c, Event{
			Type: EventCloudConnected,
			Properties: map[string]any{
				"connectionID": conn.ID,
				"workspaceID":  workspaceID,
			},
		})
		w.Flush()

		for {
			select {
			case event, ok := <-conn.Send:
				if !ok {
					manager.CloseConnection(conn.ID)
					return
				}
				writeSseEvent(c, event)
				w.Flush()
			case <-conn.Done:
				return
			case <-c.Request.Context().Done():
				manager.CloseConnection(conn.ID)
				return
			}
		}
	}
}

func SubscribeHandler(manager *ConnectionManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		sessionID := c.Param("sessionID")

		var connID string
		manager.mu.RLock()
		for id, conn := range manager.connections {
			if conn.UserID == userID && conn.Type == ConnTypeUser {
				connID = id
				break
			}
		}
		manager.mu.RUnlock()

		if connID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no active SSE connection found"})
			return
		}

		if err := manager.SubscribeToSession(sessionID, connID); err != nil {
			if errors.Is(err, ErrSubscriptionLimitExceeded) {
				c.JSON(http.StatusTooManyRequests, gin.H{"error": "subscription limit exceeded", "limit": MaxSubscriptionsPerUser})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to subscribe"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "connectionID": connID})
	}
}

func UnsubscribeHandler(manager *ConnectionManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		sessionID := c.Param("sessionID")

		var connID string
		manager.mu.RLock()
		for id, conn := range manager.connections {
			if conn.UserID == userID && conn.Type == ConnTypeUser {
				connID = id
				break
			}
		}
		manager.mu.RUnlock()

		if connID != "" {
			manager.UnsubscribeFromSession(sessionID, connID)
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

func DeviceEventHandler(router *EventRouter) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var body struct {
			DeviceID  string `json:"deviceID" binding:"required"`
			SessionID string `json:"sessionID" binding:"required"`
			Event     Event  `json:"event" binding:"required"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		router.RouteDeviceEvent(body.DeviceID, body.SessionID, body.Event)

		targets := router.manager.FindUserConnsBySession(body.SessionID)
		c.JSON(http.StatusOK, gin.H{"success": true, "routedTo": len(targets)})
	}
}

func UserCommandHandler(router *EventRouter) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var body struct {
			SessionID string `json:"sessionID" binding:"required"`
			DeviceID  string `json:"deviceID" binding:"required"`
			Event     Event  `json:"event" binding:"required"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		if err := router.RouteUserCommand(body.DeviceID, body.Event); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "device not connected", "deviceID": body.DeviceID})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

func StatsHandler(manager *ConnectionManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}
		c.JSON(http.StatusOK, manager.Stats())
	}
}

func isNotifiableEvent(eventType string) bool {
	switch eventType {
	case "session.completed", "session.failed", "session.aborted",
		"permission", "permission_batch", "question":
		return true
	}
	return false
}

func DeviceNotifyHandler(manager *ConnectionManager, deviceSvc *services.DeviceService, notificationSvc *notification.NotificationService, notificationStore *notification.Store, disp Notifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := ""
		auth := c.GetHeader("Authorization")
		if len(auth) > 7 && auth[:7] == "Bearer " {
			token = auth[7:]
		}
		if token == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "device token required"})
			return
		}

		device, err := deviceSvc.VerifyDeviceToken(token)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid device token"})
			return
		}

		var body struct {
			DeviceID  string `json:"deviceID"`
			Type      string `json:"type" binding:"required"`
			SessionID string `json:"sessionID" binding:"required"`
			Path      string `json:"path"`
			Data      any    `json:"data"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		event := Event{
			Type: EventInterventionRequired,
			Properties: map[string]any{
				"type":      body.Type,
				"sessionID": body.SessionID,
				"data":      body.Data,
				"deviceID":  device.DeviceID,
			},
		}

		manager.mu.RLock()
		connIDs := make([]string, 0)
		for id, conn := range manager.connections {
			if conn.UserID == device.UserID {
				connIDs = append(connIDs, id)
			}
		}
		manager.mu.RUnlock()

		manager.RouteEvent(event, connIDs)

			var actionData map[string]any
		if isNotifiableEvent(body.Type) {
			// Interactive dispatcher: checks channel_configs for interactive channels
			if disp != nil {
				actionData = normalizeActionData(body.Data)
				logger.Info("[notify] dispatch input", "eventType", body.Type, "sessionID", body.SessionID, "actionData", actionData)

				go disp.Dispatch(dispatcher.DispatchInput{
					UserID:     device.UserID,
					EventType:  body.Type,
					SessionID:  body.SessionID,
					DeviceID:   device.DeviceID,
					Path:       body.Path,
					ActionData: actionData,
				})
			}

			// One-way notification fallback: checks user_notification_channels
			// Permission/question events are handled by the dispatcher's deferred notification
			// system (30s timer). Only immediately notify for other events when dispatcher exists.
			if notificationSvc != nil {
				if disp == nil || (body.Type != "permission" && body.Type != "question") {
					notificationSvc.TriggerNotifications(device.UserID, body.Type, body.SessionID, device.DeviceID, body.Path, actionData)
				}
			}
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "routedTo": len(connIDs)})
	}
}

func NotifyRespondedHandler(store *notification.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := ""
		auth := c.GetHeader("Authorization")
		if len(auth) > 7 && auth[:7] == "Bearer " {
			token = auth[7:]
		}
		if token == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "device token required"})
			return
		}

		var body struct {
			SessionID string `json:"sessionID" binding:"required"`
			Type      string `json:"type"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		if store != nil {
			if err := store.MarkRespondedBySession(body.SessionID); err != nil {
				logger.Error("[notify-responded] mark failed", "sessionID", body.SessionID, "error", err)
			}
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

func setSseHeaders(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Transfer-Encoding", "chunked")
	c.Header("X-Accel-Buffering", "no")
}

func writeSseEvent(c *gin.Context, event Event) {
	c.SSEvent("message", event)
}

func DeviceCommandResultHandler(manager *ConnectionManager, deviceSvc *services.DeviceService, db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := ""
		auth := c.GetHeader("Authorization")
		if len(auth) > 7 && auth[:7] == "Bearer " {
			token = auth[7:]
		}
		if token == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "device token required"})
			return
		}

		device, err := deviceSvc.VerifyDeviceToken(token)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid device token"})
			return
		}

		deviceID := c.Param("deviceID")
		commandID := c.Param("commandID")
		if deviceID == "" || commandID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "deviceID and commandID are required"})
			return
		}

		if device.DeviceID != deviceID {
			c.JSON(http.StatusForbidden, gin.H{"error": "device token does not match requested deviceID"})
			return
		}

		var body struct {
			CommandID   string          `json:"command_id"`
			Type        string          `json:"type"`
			Status      string          `json:"status"`
			StartedAt   string          `json:"started_at,omitempty"`
			CompletedAt string          `json:"completed_at,omitempty"`
			Result      json.RawMessage `json:"result,omitempty"`
			Error       string          `json:"error,omitempty"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		if body.Status == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "status is required"})
			return
		}

		var resultJSON datatypes.JSON
		if len(body.Result) > 0 {
			resultJSON = datatypes.JSON(body.Result)
		}

		var startedAt *time.Time
		if body.StartedAt != "" {
			if t, parseErr := time.Parse(time.RFC3339, body.StartedAt); parseErr == nil {
				startedAt = &t
			}
		}

		var completedAt *time.Time
		if body.CompletedAt != "" {
			if t, parseErr := time.Parse(time.RFC3339, body.CompletedAt); parseErr == nil {
				completedAt = &t
			}
		}

		cmdResult := models.DeviceCommandResult{
			DeviceID:    deviceID,
			CommandID:   commandID,
			Type:        body.Type,
			Status:      body.Status,
			Result:      resultJSON,
			Error:       body.Error,
			StartedAt:   startedAt,
			CompletedAt: completedAt,
		}

		if err := db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "device_id"}, {Name: "command_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"status", "result", "error", "started_at", "completed_at", "type", "updated_at"}),
		}).Create(&cmdResult).Error; err != nil {
			logger.Error("[command-result] failed to save command result for %s/%s: %v", deviceID, commandID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save command result"})
			return
		}

		logger.Info("[command-result] saved command result device=%s command=%s status=%s", deviceID, commandID, body.Status)

		event := Event{
			Type: EventCommandResult,
			Properties: map[string]any{
				"deviceID":  deviceID,
				"commandID": commandID,
				"type":      body.Type,
				"status":    body.Status,
			},
		}

		manager.mu.RLock()
		connIDs := make([]string, 0)
		for id, conn := range manager.connections {
			if conn.UserID == device.UserID {
				connIDs = append(connIDs, id)
			}
		}
		manager.mu.RUnlock()
		manager.RouteEvent(event, connIDs)

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

// normalizeActionData unwraps nested data structures from cs-cloud notifications.
// cs-cloud sends event data inside a "data" wrapper, but the dispatcher expects
// fields like "questions" and "tool" at the top level.
func normalizeActionData(raw any) map[string]any {
	if raw == nil {
		return nil
	}
	if m, ok := raw.(map[string]any); ok {
		return m
	}
	return nil
}
