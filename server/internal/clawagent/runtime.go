package clawagent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/channel"
	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/gateway"
	// sessionurl disabled — re-add when re-enabling 查看会话 link feature.
	// "github.com/costrict/costrict-web/server/internal/sessionurl"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ClawAgentConfig holds configuration for ClawAgent.
type ClawAgentConfig struct {
	EncryptionKey           string
	DefaultProvider         string
	DefaultModelName        string
	DefaultBaseURL          string
	DefaultAPIKey           string
	SessionDailyResetHour   int
	SessionGroupIdleMinutes int
	SessionEventIdleMinutes int
	SessionTaskIdleMinutes  int
	SessionPruneAfterDays   int
	SessionMaxPerUser       int
	SessionMaxTokens        int
	CompactionKeepRecent    int
	NotificationDelay       time.Duration
}

// ClawAgentRuntime is the main entry point for the ClawAgent system.
type ClawAgentRuntime struct {
	mu        sync.RWMutex
	db        *gorm.DB
	cfg       *config.Config
	agentCfg  ClawAgentConfig

	PersonaMgr   *PersonaManager
	MemoryMgr    *MemoryManager
	ProviderMgr  *ProviderManager
	SessionMeta  *SessionMetaManager
	MsgMgr       *MessageManager
	DeviceProxy  *DeviceProxyClient
	EventBus     *EventBus
	TaskRegistry *TaskRegistry
	EventHandler *EventHandler
	IntentHndlr  *IntentHandler

	gwRegistry *gateway.GatewayRegistry
	gwClient   *gateway.Client

	runner      *AgentRunner
	bgCtx       context.Context
	bgCancel    context.CancelFunc
	startedAt   time.Time

}


// anyChannelEnabled returns true if any notification channel is enabled.
func anyChannelEnabled(ch config.ChannelSystemConfig) bool {
	return ch.WeComEnabled || ch.WeComWebhookEnabled || ch.WeComBotEnabled ||
		ch.WeChatEnabled || ch.WebhookEnabled || len(ch.EnabledTypes) > 0
}

// New creates a new ClawAgentRuntime.
func New(db *gorm.DB, cfg *config.Config, gwRegistry *gateway.GatewayRegistry, gwClient *gateway.Client) (*ClawAgentRuntime, error) {
	encryptionKey := cfg.ClawAgent.EncryptionKey
	if encryptionKey == "" && anyChannelEnabled(cfg.Channels) {
		return nil, fmt.Errorf("CLAWAGENT_ENCRYPTION_KEY is required when notification channels are enabled")
	}

	agentCfg := ClawAgentConfig{
		EncryptionKey:           encryptionKey,
		DefaultProvider:         cfg.LLM.Provider,
		DefaultModelName:        cfg.LLM.Model,
		DefaultBaseURL:          cfg.LLM.BaseURL,
		DefaultAPIKey:           cfg.LLM.APIKey,
		SessionDailyResetHour:   cfg.ClawAgent.Session.DailyResetHour,
		SessionGroupIdleMinutes: cfg.ClawAgent.Session.GroupIdleMinutes,
		SessionEventIdleMinutes: cfg.ClawAgent.Session.EventIdleMinutes,
		SessionTaskIdleMinutes:  cfg.ClawAgent.Session.TaskIdleMinutes,
		SessionPruneAfterDays:   cfg.ClawAgent.Session.PruneAfterDays,
		SessionMaxPerUser:       cfg.ClawAgent.Session.MaxSessionsPerUser,
		SessionMaxTokens:        cfg.ClawAgent.Session.MaxSessionTokens,
		CompactionKeepRecent:    cfg.ClawAgent.Session.CompactionKeepRecentMessages,
			NotificationDelay:       time.Duration(cfg.ClawAgent.Session.NotificationDelaySeconds) * time.Second,
	}

	if agentCfg.SessionDailyResetHour == 0 {
		agentCfg.SessionDailyResetHour = 4
	}
	if agentCfg.SessionGroupIdleMinutes == 0 {
		agentCfg.SessionGroupIdleMinutes = 30
	}
	if agentCfg.SessionEventIdleMinutes == 0 {
		agentCfg.SessionEventIdleMinutes = 60
	}
	if agentCfg.SessionTaskIdleMinutes == 0 {
		agentCfg.SessionTaskIdleMinutes = 120
	}
	if agentCfg.SessionPruneAfterDays == 0 {
		agentCfg.SessionPruneAfterDays = 30
	}
	if agentCfg.SessionMaxPerUser == 0 {
		agentCfg.SessionMaxPerUser = 200
	}
	if agentCfg.SessionMaxTokens == 0 {
		agentCfg.SessionMaxTokens = 8000
	}
	if agentCfg.CompactionKeepRecent == 0 {
		agentCfg.CompactionKeepRecent = 10
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())

	rt := &ClawAgentRuntime{
		db:         db,
		cfg:        cfg,
		agentCfg:   agentCfg,
		gwRegistry: gwRegistry,
		gwClient:   gwClient,
		bgCtx:      bgCtx,
		bgCancel:   bgCancel,
		startedAt:  time.Now(),
	}

	// Initialize sub-managers
	rt.PersonaMgr = NewPersonaManager(db, agentCfg)
	rt.MemoryMgr = NewMemoryManager(db)
	rt.ProviderMgr = NewProviderManager(db, agentCfg)
	rt.SessionMeta = NewSessionMetaManager(db)
	rt.MsgMgr = NewMessageManager(db)
	rt.DeviceProxy = NewDeviceProxyClient(gwRegistry, gwClient, db)
	rt.EventBus = NewEventBus()
	rt.TaskRegistry = NewTaskRegistry(db)
	rt.EventHandler = NewEventHandler(rt)
	rt.IntentHndlr = NewIntentHandler(rt.DeviceProxy)


	// Initialize LLM client and runner
	llmClient := NewLLMClient()
	rt.runner = NewAgentRunner(rt, llmClient)
	rt.runner.SetMsgMgr(rt.MsgMgr)

	// AutoMigrate tables
	if err := rt.autoMigrate(); err != nil {
		bgCancel()
		return nil, fmt.Errorf("failed to migrate clawagent tables: %w", err)
	}

	// Start background goroutines
	rt.startBackgroundTasks()

	// Crash recovery: scan non-final tasks and recover
	rt.recoverTasks()

	return rt, nil
}

func (rt *ClawAgentRuntime) autoMigrate() error {
	return rt.db.AutoMigrate(
		&Persona{},
		&Provider{},
		&Memory{},
		&WorkspaceTask{},
		&SessionMeta{},
		&SessionMessage{},
	)
}

func (rt *ClawAgentRuntime) startBackgroundTasks() {
	// Session prune goroutine (hourly)
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rt.pruneSessions()
			case <-rt.bgCtx.Done():
				return
			}
		}
	}()

	// Task timeout check goroutine (every 30s)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rt.checkTimeouts()
			case <-rt.bgCtx.Done():
				return
			}
		}
	}()

	// Session compaction goroutine (every 5 min)
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rt.maybeCompactAll()
			case <-rt.bgCtx.Done():
				return
			}
		}
	}()
}

// Handle implements channel.MessageHandler for IM channel messages.
func (rt *ClawAgentRuntime) Handle(ctx context.Context, msg *channel.InboundMessage, sender channel.Sender) error {
	// Use platform userID from channel config, with external ID as fallback
	userID := sender.ReplyContext().UserID
	if userID == "" {
		userID = msg.ExternalUserID
	}
	if userID == "" {
		return sender.Send(ctx, "抱歉，无法识别您的身份。")
	}

	slog.Info("[clawagent] Handle",
		"platformUserID", sender.ReplyContext().UserID,
		"externalUserID", msg.ExternalUserID,
		"resolvedUserID", userID,
		"chatType", msg.ExternalChatType,
		"chatID", msg.ExternalChatID,
	)

	// BaseKey 使用平台 userID（而非 ExternalChatID）确保发送端（corp userID）和
	// 接收端（openID）使用一致的 session key。ExternalChatID 随发送/接收方向不同格式各异。
	baseKey := fmt.Sprintf("agent:clawagent:%s:%s",
		msg.ExternalChatType, userID)
	resetType := "direct"

	sessionID, err := rt.resolveActiveSession(userID, baseKey, resetType)
	if err != nil {
		return fmt.Errorf("resolve session: %w", err)
	}

	// Use background context for async LLM calls (request ctx may be cancelled after response)
	runCtx := rt.bgCtx

	// Check if this session has a pending event that needs tool-based processing
	sess := rt.runner.GetSession(sessionID)
	if sess != nil {
		slog.Info("[runtime] Handle: session state", "sessionID", sessionID, "hasEventData", sess.EventData != nil)
	}
	// If no pending EventData in memory, try loading from DB (horizontal scaling support)
	if sess != nil && (sess.EventData == nil || sess.EventData.IsProcessed) {
		ec, err := rt.SessionMeta.GetEventData(runCtx, sessionID)
		if err == nil && ec != nil && !ec.IsProcessed {
			slog.Info("[runtime] Handle: loaded EventData from DB", "sessionID", sessionID, "eventType", ec.EventType)
			rt.runner.SetEventData(sessionID, ec)
			sess = rt.runner.GetSession(sessionID)
		}
	}
	if sess != nil && sess.EventData != nil && !sess.EventData.IsProcessed {
		slog.Info("[runtime] Handle: pending event found, using RunEventReply", "sessionID", sessionID, "eventType", sess.EventData.EventType)

		// Cancel the deferred notification immediately — the user has responded.
		// This must happen before RunEventReply (which may take seconds for LLM processing)
		// to prevent the 30s deferred timer from firing before tool execution completes.
		// Use sess.EventData.SessionID (device session ID) since the dispatcher's
		// deferred timer is keyed by device session ID, not the agent session ID.
		if rt.runner.OnEventProcessed != nil {
			deviceSessionID := sess.EventData.SessionID
			if deviceSessionID == "" {
				deviceSessionID = sessionID // fallback
			}
			slog.Info("[runtime] Handle: user responded, notifying dispatcher", "agentSessionID", sessionID, "deviceSessionID", deviceSessionID)
			rt.runner.OnEventProcessed(deviceSessionID)
		}

		// Event context found: add user message to session and use RunEventReply (with tools)
		rt.runner.AddUserMessage(runCtx, sessionID, msg.Content)
		eventCh, runErr := rt.runner.RunEventReply(runCtx, userID, sessionID)
		if runErr != nil {
			return fmt.Errorf("agent event reply failed: %w", runErr)
		}
		go rt.streamResponse(runCtx, eventCh, sender, userID, msg.Content, sessionID, sess.EventData)
		return nil
	}

	slog.Info("[runtime] Handle: no pending event, using normal Run", "sessionID", sessionID)

	var eventCh <-chan AgentEvent
	var runErr error
	if msg.ExternalChatType == "group" {
		eventCh, runErr = rt.runner.Run(runCtx, userID, sessionID, msg.Content, msg.ExternalUserID)
	} else {
		eventCh, runErr = rt.runner.Run(runCtx, userID, sessionID, msg.Content)
	}
	if runErr != nil {
		return fmt.Errorf("agent run failed: %w", runErr)
	}

	go rt.streamResponse(runCtx, eventCh, sender, userID, msg.Content, sessionID, nil)
	return nil
}

// streamResponse consumes the event channel and dispatches the final assembled
// reply. Non-streaming: content is accumulated and sent once on the final
// event, so adapters see a single OutboundMessage. When eventData is non-nil
// (event-driven reply path), a session_ref is attached as metadata so channels
// like wecom-bot can render a clickable link back to the related session.
func (rt *ClawAgentRuntime) streamResponse(
	ctx context.Context,
	eventCh <-chan AgentEvent,
	sender channel.Sender,
	userID, userMessage, sessionID string,
	eventData *EventContext,
) {
	var assistantReply strings.Builder

	slog.Info("[stream] starting", "sessionID", sessionID, "userID", userID, "hasEventData", eventData != nil)

	// Best-effort session_ref: resolve workspaceID once up-front. If lookup
	// fails or components are missing, proceed without the link — the reply
	// itself is the priority.
	//
	// [disabled] 查看会话 link feature — commented out. To re-enable, uncomment
	// the block below.
	/*
	var metadata map[string]any
	if eventData != nil && eventData.SessionID != "" && eventData.Path != "" && eventData.DeviceID != "" {
		workspaceID, err := sessionurl.ResolveWorkspaceID(rt.db, eventData.DeviceID, eventData.Path)
		if err != nil {
			slog.Warn("[stream] failed to resolve workspaceID for session_ref",
				"sessionID", sessionID, "deviceID", eventData.DeviceID, "error", err)
		} else if url := sessionurl.Build(rt.cfg.AppURL, workspaceID, eventData.SessionID); url != "" {
			metadata = map[string]any{
				"session_ref": map[string]any{
					"title": "查看会话",
					"url":   url,
				},
			}
		}
	}
	*/
	var metadata map[string]any

	send := func(content string) {
		msg := channel.OutboundMessage{ContentType: "text", Content: content, Metadata: metadata}
		if err := sender.SendMessage(ctx, msg); err != nil {
			slog.Error("[stream] SendMessage error", "sessionID", sessionID, "error", err)
		}
	}

	for evt := range eventCh {
		if evt.Type == "tool_call" || evt.Type == "tool_result" {
			slog.Debug("[runtime] streamResponse: "+evt.Type, "tool", evt.Tool, "sessionID", sessionID)
			continue
		}
		if evt.Error != "" {
			slog.Error("[stream] LLM returned error", "sessionID", sessionID, "error", evt.Error)
			send(fmt.Sprintf("⚠️ %s", evt.Error))
			continue
		}
		if evt.IsFinal {
			slog.Info("[stream] final event received", "sessionID", sessionID, "replyLen", assistantReply.Len())
			reply := assistantReply.String()
			// Defense-in-depth: even when the producing path is Run() (no tools)
			// and the model hallucinates <tool_call>…</tool_call> as text, strip
			// it before the message reaches the user. parseTextToolCalls returns
			// the cleaned content; we discard any parsed calls because this path
			// has no tool registry wired in.
			stripped := false
			if _, cleaned := parseTextToolCalls(reply); cleaned != reply {
				slog.Warn("[stream] stripped text-encoded tool_call XML from reply (Run path doesn't execute tools)",
					"sessionID", sessionID, "before", len(reply), "after", len(cleaned))
				reply = cleaned
				stripped = true
			}
			// If the model emitted nothing but tool-call XML, the user would
			// see an empty message. Substitute a fallback so they at least know
			// an event is pending and how to respond.
			if reply == "" && stripped {
				slog.Warn("[stream] reply empty after XML strip, sending fallback", "sessionID", sessionID)
				reply = "收到一个新的申请，请回复「允许」或「拒绝」来处理。"
			}
			if reply != "" {
				send(reply)
			}
			go rt.SessionMeta.ClearEventData(rt.bgCtx, sessionID)
			if reply != "" {
				go rt.MemoryMgr.Refresh(rt.bgCtx, userID, userMessage, reply, rt.runner.llmClient, rt.agentCfg)
			}
			go rt.SessionMeta.IncrementMessageCount(sessionID)
			go rt.maybeCompact(rt.bgCtx, sessionID)
			break
		}
		if evt.Content != "" {
			assistantReply.WriteString(evt.Content)
		}
	}
}

// RegisterRoutes registers ClawAgent REST API routes.
func (rt *ClawAgentRuntime) RegisterRoutes(g *gin.RouterGroup) {
	agent := g.Group("/clawagent")
	{
		// Chat
		agent.POST("/chat", rt.handleChat)

		// Sessions
		agent.GET("/sessions", rt.handleListSessions)
		agent.GET("/sessions/:id", rt.handleGetSession)
		agent.DELETE("/sessions/:id", rt.handleDeleteSession)

		// Personas
		agent.GET("/personas", rt.handleListPersonas)
		agent.POST("/personas", rt.handleCreatePersona)
		agent.PUT("/personas/:id", rt.handleUpdatePersona)
		agent.DELETE("/personas/:id", rt.handleDeletePersona)
		agent.POST("/personas/:id/default", rt.handleSetDefaultPersona)

		// Providers
		agent.GET("/providers", rt.handleListProviders)
		agent.POST("/providers", rt.handleCreateProvider)
		agent.PUT("/providers/:id", rt.handleUpdateProvider)
		agent.DELETE("/providers/:id", rt.handleDeleteProvider)
		agent.POST("/providers/:id/test", rt.handleTestProvider)

		// Memory
		agent.GET("/memory", rt.handleGetMemory)
		agent.PUT("/memory", rt.handleUpdateMemory)

		// Workspaces
		agent.GET("/workspaces", rt.handleListWorkspaces)
		agent.GET("/workspaces/:id/tasks", rt.handleListDelegationTasks)
		agent.GET("/workspaces/:id/tasks/:taskId", rt.handleGetDelegationTask)
		agent.POST("/workspaces/:id/tasks/:taskId/abort", rt.handleAbortDelegationTask)
	}
}

// resolveUserID extracts userID from various context sources.
func (rt *ClawAgentRuntime) resolveUserID(c *gin.Context) string {
	if uid := c.GetString("userId"); uid != "" {
		return uid
	}
	if uid := c.GetString("user_id"); uid != "" {
		return uid
	}
	return ""
}

// bgContext returns the background context for async operations.
func (rt *ClawAgentRuntime) bgContext() context.Context {
	return rt.bgCtx
}

// Stop gracefully stops background goroutines.
func (rt *ClawAgentRuntime) Stop() {
	rt.bgCancel()
}
