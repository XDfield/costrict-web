package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/backend"
	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/config"
	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/dedup"
	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/router"
	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/ws"
	"github.com/gin-gonic/gin"
)

// Proxy is the core orchestrator that wires together WS, router, dedup, and backends.
type Proxy struct {
	cfg     *config.Config
	logger  *slog.Logger
	wsConn  *ws.Conn
	routes  *router.Table
	backends *backend.Manager
	dedup   *dedup.Store
}

func NewProxy(
	cfg *config.Config,
	logger *slog.Logger,
	wsConn *ws.Conn,
	routes *router.Table,
	backends *backend.Manager,
	dedupStore *dedup.Store,
) *Proxy {
	p := &Proxy{
		cfg:      cfg,
		logger:   logger,
		wsConn:   wsConn,
		routes:   routes,
		backends: backends,
		dedup:    dedupStore,
	}

	return p
}

func (p *Proxy) SetWSConn(conn *ws.Conn) {
	p.wsConn = conn
}

// HandleWSFrame is the InboundHandler for ws.Conn.
func (p *Proxy) HandleWSFrame(frame *ws.WSFrame) {
	var inbound *InboundMsg
	var err error

	switch frame.Cmd {
	case ws.CmdMsgCallback:
		inbound, err = TranslateMsgCallback(frame)
	case ws.CmdEventCallback:
		inbound, err = TranslateEventCallback(frame)
	default:
		p.logger.Debug("ignoring ws frame", "cmd", frame.Cmd)
		return
	}

	if err != nil {
		p.logger.Error("failed to translate ws frame", "cmd", frame.Cmd, "error", err)
		return
	}

	// nil means don't forward (e.g., disconnected_event)
	if inbound == nil {
		return
	}

	// Dedup
	if p.dedup != nil && inbound.ExternalMessageID != "" {
		if p.dedup.Check(inbound.ExternalMessageID) {
			p.logger.Debug("duplicate message dropped", "msgId", inbound.ExternalMessageID)
			return
		}
	}

	// Route
	targetBackend := p.resolveRoute(inbound)

	p.logger.Info("routing inbound",
		"msgId", inbound.ExternalMessageID,
		"contentType", inbound.ContentType,
		"backend", targetBackend,
	)

	// Convert to backend.InboundMessage and forward
	msg := &backend.InboundMessage{
		ExternalChatID:    inbound.ExternalChatID,
		ExternalChatType:  inbound.ExternalChatType,
		ExternalUserID:    inbound.ExternalUserID,
		ExternalMessageID: inbound.ExternalMessageID,
		ContentType:       inbound.ContentType,
		Content:           inbound.Content,
		Metadata:          inbound.Metadata,
	}

	if err := p.backends.Forward(context.Background(), targetBackend, msg); err != nil {
		p.logger.Error("failed to forward to backend",
			"backend", targetBackend,
			"error", err,
		)
	}
}

func (p *Proxy) resolveRoute(msg *InboundMsg) string {
	if msg.ContentType == "action_callback" {
		taskID, _ := msg.Metadata["taskId"].(string)
		if taskID != "" {
			return p.routes.RouteForCardEvent(taskID)
		}
	}
	return p.routes.DefaultBackend()
}

// --- HTTP Handlers ---

func (p *Proxy) RegisterRoutes(r *gin.Engine) {
	bot := r.Group("/api/bot")
	{
		bot.POST("/send", p.authMiddleware(), p.handleSend)
		bot.POST("/reply", p.authMiddleware(), p.handleReply)
		bot.POST("/reply/stream", p.authMiddleware(), p.handleStreamReply)
		bot.POST("/welcome", p.authMiddleware(), p.handleWelcome)
		bot.POST("/card/update", p.authMiddleware(), p.handleCardUpdate)
		bot.GET("/health", p.handleHealth)
	}
}

func (p *Proxy) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization required"})
			return
		}

		backendName := p.cfg.FindBackendByToken(authHeader)
		if backendName == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}

		c.Set("backend_name", backendName)
		c.Next()
	}
}

func (p *Proxy) handleSend(c *gin.Context) {
	var req SendRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if req.UserID == "" || req.MsgType == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id and msg_type are required"})
		return
	}

	// Register task route if task_id provided
	backendName := c.GetString("backend_name")
	if req.TaskID != "" {
		p.routes.Register(req.TaskID, backendName)
		p.logger.Info("registered task route", "taskId", req.TaskID, "backend", backendName)
	}

	// Translate and send via WS
	frame, err := TranslateSend(&req)
	if err != nil {
		p.logger.Error("failed to translate send", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "translation failed"})
		return
	}

	if err := p.wsConn.Send(frame); err != nil {
		p.logger.Error("failed to send via ws", "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "ws send failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (p *Proxy) handleReply(c *gin.Context) {
	var req ReplyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if req.ReqID == "" || req.MsgType == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "req_id and msg_type are required"})
		return
	}

	frame, err := TranslateReply(&req)
	if err != nil {
		p.logger.Error("failed to translate reply", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "translation failed"})
		return
	}

	if err := p.wsConn.Send(frame); err != nil {
		p.logger.Error("failed to send reply via ws", "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "ws send failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (p *Proxy) handleStreamReply(c *gin.Context) {
	var req StreamReplyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if req.ReqID == "" || req.StreamID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "req_id and stream_id are required"})
		return
	}

	frame, err := TranslateStreamReply(&req)
	if err != nil {
		p.logger.Error("failed to translate stream reply", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "translation failed"})
		return
	}

	if err := p.wsConn.Send(frame); err != nil {
		p.logger.Error("failed to send stream reply via ws", "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "ws send failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (p *Proxy) handleWelcome(c *gin.Context) {
	var req WelcomeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if req.ReqID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "req_id is required"})
		return
	}

	frame, err := TranslateWelcome(&req)
	if err != nil {
		p.logger.Error("failed to translate welcome", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "translation failed"})
		return
	}

	if err := p.wsConn.Send(frame); err != nil {
		p.logger.Error("failed to send welcome via ws", "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "ws send failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (p *Proxy) handleCardUpdate(c *gin.Context) {
	var req CardUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if req.ReqID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "req_id is required"})
		return
	}

	frame, err := TranslateCardUpdate(&req)
	if err != nil {
		p.logger.Error("failed to translate card update", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "translation failed"})
		return
	}

	if err := p.wsConn.Send(frame); err != nil {
		p.logger.Error("failed to send card update via ws", "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "ws send failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (p *Proxy) handleHealth(c *gin.Context) {
	backends := gin.H{}
	for name, client := range p.backends.All() {
		backends[name] = gin.H{
			"healthy":      client.Healthy(),
			"last_success": client.LastSuccess(),
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"status":       p.wsConn.State().String(),
		"bot_id":       p.cfg.Bot.BotID,
		"connected_at": p.wsConn.ConnectedAt(),
		"last_pong":    p.wsConn.LastPong(),
		"task_routes":  p.routes.Size(),
		"backends":     backends,
	})
}

// HealthStatus is returned by the health endpoint.
type HealthStatus struct {
	Status      string `json:"status"`
	BotID       string `json:"bot_id"`
	ConnectedAt string `json:"connected_at,omitempty"`
	LastPong    string `json:"last_pong,omitempty"`
}

// marshalJSON is a helper to check JSON marshaling works.
func marshalJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
