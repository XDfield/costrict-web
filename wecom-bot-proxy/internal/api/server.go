package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/backend"
	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/config"
	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/dedup"
	"github.com/gin-gonic/gin"
	"github.com/go-sphere/wecom-aibot-go-sdk/aibot"
)

// Proxy is the core orchestrator that wires together WS, dedup, and backend.
type Proxy struct {
	cfg       *config.Config
	logger    *slog.Logger
	sdk       *aibot.WSClient
	backend   *backend.Client
	dedup     *dedup.Store
	userIDMap *UserIDMapper
}

func NewProxy(
	cfg *config.Config,
	logger *slog.Logger,
	sdk *aibot.WSClient,
	backend *backend.Client,
	dedupStore *dedup.Store,
) *Proxy {
	p := &Proxy{
		cfg:       cfg,
		logger:    logger,
		sdk:       sdk,
		backend:   backend,
		dedup:     dedupStore,
		userIDMap: NewUserIDMapper(cfg.WecomAPI),
	}
	return p
}

// setupSDKHandlers registers message/event handlers on the SDK client.
func (p *Proxy) SetupSDKHandlers() {
	if p.sdk == nil {
		return
	}

	p.sdk.OnConnected(func() {
		p.logger.Info("ws connected")
	})

	p.sdk.OnAuthenticated(func() {
		p.logger.Info("ws authenticated")
	})

	p.sdk.OnDisconnected(func(reason string) {
		p.logger.Warn("ws disconnected", "reason", reason)
	})

	p.sdk.OnReconnecting(func(attempt int) {
		p.logger.Info("ws reconnecting", "attempt", attempt)
	})

	p.sdk.OnError(func(err error) {
		p.logger.Error("ws error", "error", err)
	})

	// Route all message types to our handler
	p.sdk.OnMessage(func(frame *aibot.WsFrame) {
		p.handleInbound(frame)
	})
	p.sdk.OnMessageText(func(frame *aibot.WsFrame) {
		p.handleInbound(frame)
	})
	p.sdk.OnEvent(func(frame *aibot.WsFrame) {
		p.handleInbound(frame)
	})
	p.sdk.OnEventTemplateCardEvent(func(frame *aibot.WsFrame) {
		p.handleInbound(frame)
	})
}

// handleInbound translates a WS frame and forwards it to the backend.
func (p *Proxy) handleInbound(frame *aibot.WsFrame) {
	var inbound *InboundMsg
	var err error

	switch frame.Cmd {
	case aibot.WsCmd.CALLBACK:
		inbound, err = TranslateMsgCallback(frame)
	case aibot.WsCmd.EVENT_CALLBACK:
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

	// Resolve encrypted open_userid to plaintext userid
	originalUserID := inbound.ExternalUserID
	if p.userIDMap != nil && inbound.ExternalUserID != "" {
		resolved := p.userIDMap.Resolve(inbound.ExternalUserID)
		if resolved != inbound.ExternalUserID {
			inbound.ExternalUserID = resolved
			// For single chat, ExternalChatID was set to the encrypted userID — update it too
			if inbound.ExternalChatType == "single" {
				inbound.ExternalChatID = resolved
			}
			p.logger.Debug("resolved open_userid",
				"openUserID", originalUserID,
				"userID", resolved,
			)
		}
	}

	// Group chat is not supported — reply directly via aibot_send_msg and skip backend forwarding.
	if inbound.ExternalChatType == "group" {
		p.logger.Info("group chat rejected, replying directly",
			"msgId", inbound.ExternalMessageID,
			"chatID", inbound.ExternalChatID,
			"userID", inbound.ExternalUserID,
		)
		if p.sdk != nil && p.sdk.IsConnected() {
			_, _ = p.sdk.SendMarkdown(inbound.ExternalChatID, "当前不支持群聊功能，请进行私聊。")
		}
		return
	}

	p.logger.Info("routing inbound",
		"msgId", inbound.ExternalMessageID,
		"contentType", inbound.ContentType,
		"userID", inbound.ExternalUserID,
		"chatID", inbound.ExternalChatID,
		"chatType", inbound.ExternalChatType,
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

	if err := p.backend.Forward(context.Background(), msg); err != nil {
		p.logger.Error("failed to forward to backend", "error", err)
	}
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

		if !p.cfg.ValidateAuthToken(authHeader) {
			p.logger.Warn("auth failed: invalid token")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}

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

	if p.sdk == nil || !p.sdk.IsConnected() {
		c.JSON(http.StatusBadGateway, gin.H{"error": "ws not connected"})
		return
	}

	// Send via SDK
	var err error
	switch req.MsgType {
	case "text":
		_, err = p.sdk.SendMarkdown(req.UserID, req.Content)
	case "markdown":
		_, err = p.sdk.SendMarkdown(req.UserID, req.Content)
	case "card":
		_, err = p.sdk.SendMarkdown(req.UserID, req.Content)
	default:
		_, err = p.sdk.SendMarkdown(req.UserID, req.Content)
	}

	if err != nil {
		p.logger.Error("failed to send via sdk", "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "send failed"})
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

	if p.sdk == nil || !p.sdk.IsConnected() {
		c.JSON(http.StatusBadGateway, gin.H{"error": "ws not connected"})
		return
	}

	// Create minimal frame with req_id for SDK reply
	frame := &aibot.WsFrame{
		Headers: aibot.WsFrameHeaders{ReqID: req.ReqID},
	}

	var body any
	switch req.MsgType {
	case "text":
		body = aibot.CreateTextReplyBody(req.Content)
	case "markdown":
		body = aibot.CreateMarkdownReplyBody(req.Content)
	default:
		body = aibot.CreateTextReplyBody(req.Content)
	}

	_, err := p.sdk.Reply(frame, body, "")
	if err != nil {
		p.logger.Error("failed to reply via sdk", "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "reply failed"})
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

	if p.sdk == nil || !p.sdk.IsConnected() {
		c.JSON(http.StatusBadGateway, gin.H{"error": "ws not connected"})
		return
	}

	frame := &aibot.WsFrame{
		Headers: aibot.WsFrameHeaders{ReqID: req.ReqID},
	}

	_, err := p.sdk.ReplyStream(frame, req.StreamID, req.Content, req.Finish, nil, nil)
	if err != nil {
		p.logger.Error("failed to stream reply via sdk", "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "stream reply failed"})
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

	if p.sdk == nil || !p.sdk.IsConnected() {
		c.JSON(http.StatusBadGateway, gin.H{"error": "ws not connected"})
		return
	}

	frame := &aibot.WsFrame{
		Headers: aibot.WsFrameHeaders{ReqID: req.ReqID},
	}

	body := aibot.CreateWelcomeReplyBody(req.Content)
	_, err := p.sdk.ReplyWelcome(frame, body)
	if err != nil {
		p.logger.Error("failed to send welcome via sdk", "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "welcome failed"})
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

	if p.sdk == nil || !p.sdk.IsConnected() {
		c.JSON(http.StatusBadGateway, gin.H{"error": "ws not connected"})
		return
	}

	frame := &aibot.WsFrame{
		Headers: aibot.WsFrameHeaders{ReqID: req.ReqID},
	}

	var templateCard aibot.TemplateCard
	if err := json.Unmarshal([]byte(req.Content), &templateCard); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid template card json"})
		return
	}

	_, err := p.sdk.UpdateTemplateCard(frame, templateCard, nil)
	if err != nil {
		p.logger.Error("failed to update card via sdk", "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "card update failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (p *Proxy) handleHealth(c *gin.Context) {
	connected := false
	if p.sdk != nil {
		connected = p.sdk.IsConnected()
	}

	c.JSON(http.StatusOK, gin.H{
		"status":       map[bool]string{true: "connected", false: "disconnected"}[connected],
		"bot_id":       p.cfg.Bot.BotID,
		"connected":    connected,
		"backend_healthy": p.backend.Healthy(),
		"backend_last_success": p.backend.LastSuccess(),
	})
}
