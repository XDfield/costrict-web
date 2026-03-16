package cloud

import (
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/notification"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

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
	case "session.completed", "session.failed", "session.aborted":
		return true
	}
	return false
}

func DeviceNotifyHandler(manager *ConnectionManager, deviceSvc *services.DeviceService, notificationSvc *notification.NotificationService) gin.HandlerFunc {
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

		if notificationSvc != nil && isNotifiableEvent(body.Type) {
			notificationSvc.TriggerNotifications(device.UserID, body.Type, body.SessionID, device.DeviceID)
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "routedTo": len(connIDs)})
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
