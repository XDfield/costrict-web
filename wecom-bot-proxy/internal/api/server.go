package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"unicode/utf8"

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

	// Hard cap on user input length — reject oversized payloads before doing
	// any backend work, and tell the user to shorten their message.
	if p.cfg.Bot.InputMaxLength > 0 && inbound.ContentType == "text" {
		contentLen := utf8.RuneCountInString(inbound.Content)
		if contentLen > p.cfg.Bot.InputMaxLength {
			p.logger.Info("input exceeded max length, rejected",
				"msgId", inbound.ExternalMessageID,
				"contentLen", contentLen,
				"maxLength", p.cfg.Bot.InputMaxLength,
			)
			if p.sdk != nil && p.sdk.IsConnected() {
				notice := fmt.Sprintf("消息太长了，目前单条最多支持 %d 个字，请精简后再发一次。", p.cfg.Bot.InputMaxLength)
				if _, err := p.sdk.SendMarkdown(inbound.ExternalChatID, notice); err != nil {
					p.logger.Warn("failed to send length-limit notice", "error", err)
				}
			}
			return
		}
	}

	// Resolve encrypted open_userid to plaintext userid. When the mapper is
	// configured but conversion fails (IP whitelist, secret, transient WeCom
	// API error), short-circuit single-chat inbound: send the user a retry
	// notice and DO NOT forward the encrypted open_userid to the backend.
	// Forwarding it would miss the idtrust identity lookup (which expects
	// plaintext) and create a spurious agent session under an unbound identity.
	// Group chat is intentionally left to fall through — the group-rejection
	// branch below has its own user-facing message and avoids broadcasting
	// this notice to the whole group.
	if p.userIDMap != nil && inbound.ExternalUserID != "" {
		originalUserID := inbound.ExternalUserID
		resolved, err := p.userIDMap.Resolve(originalUserID)
		if err != nil {
			p.logger.Warn("open_userid resolution failed, dropping inbound",
				"openUserID", originalUserID,
				"chatType", inbound.ExternalChatType,
				"chatID", inbound.ExternalChatID,
				"error", err,
			)
			if inbound.ExternalChatType != "group" && p.sdk != nil && p.sdk.IsConnected() {
				notice := "账号身份解析暂时失败，请稍后重试。若反复出现，请联系管理员检查企微可信 IP 与应用 secret 配置。"
				if _, nerr := p.sdk.SendMarkdown(inbound.ExternalChatID, notice); nerr != nil {
					p.logger.Warn("failed to send resolution-failure notice", "error", nerr)
				}
			}
			return
		}
		if resolved != originalUserID {
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
			_, _ = p.sdk.SendMarkdown(inbound.ExternalChatID,
				"抱歉，机器人暂不支持群聊场景。\n\n请添加机器人为联系人后**私聊**使用，任务通知也将通过私聊推送给你。")
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

	respBody, err := p.backend.Forward(context.Background(), msg)
	if err != nil {
		p.logger.Error("failed to forward to backend", "error", err)
		return
	}

	// Parse backend response to act on firstContact / error feedback.
	var br struct {
		Success      bool   `json:"success"`
		FirstContact bool   `json:"firstContact"`
		Bound        bool   `json:"bound"`
		Welcome      string `json:"welcome"`
		ErrMsg       string `json:"error"`
	}
	if err := json.Unmarshal(respBody, &br); err != nil {
		// Non-JSON response (e.g., "success" from non-wecom-bot paths) — nothing to act on.
		return
	}

	if p.sdk == nil || !p.sdk.IsConnected() {
		return
	}

	if br.ErrMsg != "" {
		if _, err := p.sdk.SendMarkdown(inbound.ExternalChatID, br.ErrMsg); err != nil {
			p.logger.Warn("failed to send error notice to user", "error", err)
		}
		return
	}
	if br.FirstContact && br.Welcome != "" {
		if _, err := p.sdk.SendMarkdown(inbound.ExternalChatID, br.Welcome); err != nil {
			p.logger.Warn("failed to send welcome to user", "error", err)
		}
	}
}

// --- HTTP Handlers ---

func (p *Proxy) RegisterRoutes(r *gin.Engine) {
	bot := r.Group("/api/bot")
	{
		bot.POST("/send", p.authMiddleware(), p.handleSend)
		bot.POST("/reply", p.authMiddleware(), p.handleReply)
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

	// card is structurally different: content is a JSON-marshaled template_card
	// payload (card_type + main_title + button_list/checkbox/jump_list + task_id).
	// Forward it to the SDK's SendTemplateCard so it renders as an interactive
	// card instead of leaking raw JSON as text. session_ref does not apply to
	// cards — interactive cards have their own jump_list / card_action for links.
	if req.MsgType == "card" {
		var templateCard aibot.TemplateCard
		if err := json.Unmarshal([]byte(req.Content), &templateCard); err != nil {
			p.logger.Warn("failed to parse card content as template_card", "error", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid template_card json"})
			return
		}
		if _, err := p.sdk.SendTemplateCard(req.UserID, templateCard); err != nil {
			p.logger.Error("failed to send template card via sdk", "error", err)
			c.JSON(http.StatusBadGateway, gin.H{"error": "send failed"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
		return
	}

	// text / markdown / default: compose final content. If session_ref is
	// provided and link mode is enabled, append a markdown link so users can
	// jump to the related CoStrict session. In restricted mode, session_ref is
	// silently dropped — content is forwarded verbatim, allowing the same
	// server payload to serve both modes.
	//
	// [disabled] 查看会话 link appending — commented out. To re-enable,
	// uncomment the block below.
	finalContent := req.Content
	/*
	if req.SessionRef != nil && req.SessionRef.URL != "" && p.cfg.Bot.SessionLinkMode != "restricted" {
		title := truncateRunes(req.SessionRef.Title, p.cfg.Bot.SessionTitleMaxLength)
		if title == "" {
			title = "查看会话"
		}
		finalContent = fmt.Sprintf("%s\n\n[%s](%s)", finalContent, title, req.SessionRef.URL)
	}
	*/

	if _, err := p.sdk.SendMarkdown(req.UserID, finalContent); err != nil {
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

// truncateRunes caps s at max runes, appending "…" when truncated. Returns s
// unchanged if its rune count is already within the limit.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	n := utf8.RuneCountInString(s)
	if n <= max {
		return s
	}
	out := make([]rune, 0, max)
	for i, r := range []rune(s) {
		if i >= max-1 {
			out = append(out, r)
			break
		}
		out = append(out, r)
	}
	return string(out) + "…"
}
