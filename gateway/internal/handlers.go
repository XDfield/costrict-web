package internal

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

func DeviceSSEHandler(manager *ConnectionManager, cfg *Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		deviceID := c.Param("deviceID")

		conn := manager.Register(deviceID)

		go func() {
			if err := NotifyOnline(cfg.ServerURL, cfg.GatewayID, deviceID); err != nil {
				log.Printf("[Gateway] notify online failed for device %s: %v", deviceID, err)
			}
		}()

		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Transfer-Encoding", "chunked")
		c.Header("X-Accel-Buffering", "no")

		connectedEvent := Event{
			Type: "device.connected",
			Properties: map[string]any{
				"deviceID":  deviceID,
				"gatewayID": cfg.GatewayID,
			},
		}
		data, _ := json.Marshal(connectedEvent)
		fmt.Fprintf(c.Writer, "event: message\ndata: %s\n\n", data)
		c.Writer.Flush()

		for {
			select {
			case payload, ok := <-conn.Send:
				if !ok {
					manager.Close(deviceID)
					go notifyOfflineAsync(cfg, deviceID)
					return
				}
				c.Writer.Write(payload)
				c.Writer.Flush()
			case <-conn.Done:
				go notifyOfflineAsync(cfg, deviceID)
				return
			case <-c.Request.Context().Done():
				manager.Close(deviceID)
				go notifyOfflineAsync(cfg, deviceID)
				return
			}
		}
	}
}

func notifyOfflineAsync(cfg *Config, deviceID string) {
	if err := NotifyOffline(cfg.ServerURL, cfg.GatewayID, deviceID); err != nil {
		log.Printf("[Gateway] notify offline failed for device %s: %v", deviceID, err)
	}
}

func SendToDeviceHandler(manager *ConnectionManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		deviceID := c.Param("deviceID")

		var body struct {
			Event Event `json:"event"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		data, _ := json.Marshal(body.Event)
		payload := fmt.Sprintf("event: message\ndata: %s\n\n", data)

		if err := manager.Send(deviceID, []byte(payload)); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "device not connected"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}
