package internal

import (
	"net/http"
	"time"

	"github.com/costrict/costrict-web/gateway/internal/logger"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

const (
	heartbeatInterval  = 30 * time.Second
	heartbeatTimeout   = 10 * time.Second
	maxFailedHeartbeat = 3
)

func DeviceTunnelHandler(manager *TunnelManager, cfg *Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		deviceID := c.Param("deviceID")

		// Authenticate device token before upgrading to WebSocket
		token := ExtractDeviceToken(c.Request)
		if token == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "device token required"})
			return
		}

		_, err := VerifyDeviceToken(cfg.ServerURL, cfg.InternalSecret, deviceID, token)
		if err != nil {
			logger.Error("[Gateway] device %s auth failed: %v", deviceID, err)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid device token"})
			return
		}

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

		failedHeartbeats := 0

		ws.SetPongHandler(func(string) error {
			failedHeartbeats = 0
			ws.SetReadDeadline(time.Now().Add(heartbeatInterval + heartbeatTimeout))
			return nil
		})

		go func() {
			ticker := time.NewTicker(heartbeatInterval)
			defer ticker.Stop()
			for range ticker.C {
				if session.IsClosed() {
					return
				}
				failedHeartbeats++
				if failedHeartbeats > maxFailedHeartbeat {
					logger.Error("[Gateway] device %s heartbeat failed %d times, closing connection", deviceID, failedHeartbeats-1)
					session.Close()
					return
				}
				conn.mu.Lock()
				err := ws.WriteMessage(websocket.PingMessage, nil)
				conn.mu.Unlock()
				if err != nil {
					logger.Error("[Gateway] device %s ping write error: %v", deviceID, err)
					session.Close()
					return
				}
			}
		}()

		manager.Register(deviceID, session)

		go func() {
			if err := NotifyOnline(cfg.ServerURL, cfg.GatewayID, deviceID, cfg.InternalSecret); err != nil {
				logger.Error("[Gateway] notify online failed for device %s: %v", deviceID, err)
			}
		}()

		<-session.CloseChan()

		// 排查用：session 关闭原因诊断。failedHeartbeats 由 ping ticker 协程写入，
		// 此处读取可能有轻微数据竞争，但足以区分关闭类型：
		//   值≈4  → ping/pong 连续失败判死（device 不回 pong）
		//   值较小 → yamux 写超时静默关、ping 写失败、或对端正常关闭
		logger.Warn("[TUNNEL-CLOSE] device %s tunnel closed (failedHeartbeats=%d)", deviceID, failedHeartbeats)

		manager.UnregisterIf(deviceID, session)
		go func() {
			if err := NotifyOffline(cfg.ServerURL, cfg.GatewayID, deviceID, cfg.InternalSecret); err != nil {
				logger.Error("[Gateway] notify offline failed for device %s: %v", deviceID, err)
			}
		}()
	}
}
