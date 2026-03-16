package internal

import (
	"log"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

func DeviceTunnelHandler(manager *TunnelManager, cfg *Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		deviceID := c.Param("deviceID")

		ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}

		conn := &wsConn{Conn: ws}
		yamuxCfg := yamux.DefaultConfig()
		yamuxCfg.EnableKeepAlive = false
		yamuxCfg.ConnectionWriteTimeout = 60 * time.Second
		session, err := yamux.Server(conn, yamuxCfg)
		if err != nil {
			ws.Close()
			return
		}

		ws.SetPongHandler(func(string) error {
			ws.SetReadDeadline(time.Now().Add(70 * time.Second))
			return nil
		})
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				conn.mu.Lock()
				err := ws.WriteMessage(websocket.PingMessage, nil)
				conn.mu.Unlock()
				if err != nil {
					return
				}
			}
		}()

		manager.Register(deviceID, session)

		go func() {
			if err := NotifyOnline(cfg.ServerURL, cfg.GatewayID, deviceID); err != nil {
				log.Printf("[Gateway] notify online failed for device %s: %v", deviceID, err)
			}
		}()

		<-session.CloseChan()

		manager.Close(deviceID)
		go func() {
			if err := NotifyOffline(cfg.ServerURL, cfg.GatewayID, deviceID); err != nil {
				log.Printf("[Gateway] notify offline failed for device %s: %v", deviceID, err)
			}
		}()
	}
}
