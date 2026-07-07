package clawagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/channel"
	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/gateway"
	"github.com/costrict/costrict-web/server/internal/purify"
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
	Purifier     *purify.Purifier

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
	rt.Purifier = purify.New()


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

	// Defense-in-depth: purify user input before it enters chat_messages or
	// reaches the LLM. Standardize whitespace/control chars, block on length
	// cap (120 runes by default) and on suspected prompt injection. Redact
	// is opt-in (not registered by default).
	if rt.Purifier != nil {
		result := rt.Purifier.Purify(msg.Content)
		if result.Blocked {
			slog.Warn("[runtime] Handle: input blocked by purifier",
				"sessionID", sessionID, "reason", result.BlockReason)
			// Distinguish user-facing phrasing by reason category so the user
			// gets actionable feedback (e.g., "too long" tells them to shorten).
			hint := "您的消息无法处理，请重新输入。"
			switch {
			case strings.Contains(result.BlockReason, "length"):
				hint = fmt.Sprintf("您的消息过长（上限 %d 字符），请精简后重试。", rt.Purifier.MaxLength())
			case strings.Contains(result.BlockReason, "injection"):
				hint = "您的消息包含疑似注入内容，已被拒绝。"
			case strings.Contains(result.BlockReason, "empty"):
				hint = "您的消息内容为空，请重新输入。"
			}
			return sender.Send(ctx, hint)
		}
		if result.HasRedactions() {
			slog.Info("[runtime] Handle: input redacted",
				"sessionID", sessionID, "redactions", result.Redactions)
		}
		if len(result.Warnings) > 0 {
			slog.Warn("[runtime] Handle: purifier warnings",
				"sessionID", sessionID, "warnings", result.Warnings)
		}
		msg.Content = result.Cleaned
	}

	// AI normalize: run the purified input through an LLM with the recent
	// session context to produce a structured classification + canonical
	// rewrite. NO FALLBACK — if the LLM call fails for any reason (no
	// provider, timeout, empty/malformed response), the input is blocked.
	// When the LLM classifies the input as injection/jailbreak, the audit
	// row is already persisted inside NormalizeInput and we surface a
	// distinct rejection message to the user.
	originalPurified := msg.Content
	normalized, _, _, normErr := rt.runner.NormalizeInput(runCtx, userID, sessionID, msg.Content)
	if normErr != nil {
		errMsg := normErr.Error()
		switch {
		case strings.Contains(errMsg, "injection detected"):
			// LLM classified input as injection/jailbreak — audit row + warn
			// log already done inside NormalizeInput. Surface distinct msg
			// so user understands the rejection reason.
			return sender.Send(ctx, "您的消息包含疑似注入或越狱内容，已被拒绝。")
		case strings.Contains(errMsg, "no provider"),
			strings.Contains(errMsg, "timeout"),
			strings.Contains(errMsg, "LLM call failed"),
			strings.Contains(errMsg, "no choices"),
			strings.Contains(errMsg, "empty content"),
			strings.Contains(errMsg, "structured parse failed"),
			strings.Contains(errMsg, "missing required"),
			strings.Contains(errMsg, "invalid intent"):
			// System degraded — distinguish from injection rejection.
			slog.Warn("[runtime] Handle: AI normalize failed, blocking input",
				"sessionID", sessionID, "original", originalPurified, "error", normErr)
			return sender.Send(ctx, "您的消息暂时无法被规范化处理，请稍后重试。")
		default:
			slog.Warn("[runtime] Handle: AI normalize unknown failure, blocking input",
				"sessionID", sessionID, "original", originalPurified, "error", normErr)
			return sender.Send(ctx, "您的消息暂时无法被规范化处理，请稍后重试。")
		}
	}
	msg.Content = normalized

	// Persist the user message BEFORE invoking Run / RunEventReply. Run no
	// longer appends the user message itself — keeping the DB write outside
	// the cancellable Run goroutine guarantees user input is never lost when
	// a subsequent inbound coalesces and cancels an in-flight Run.
	rt.runner.AddUserMessage(runCtx, sessionID, msg.Content)

	// Pending event state lives in chat_messages as the latest EVENT_PENDING
	// system row (sole source of truth; multi-pod safe). Probe the DB.
	ec, err := rt.MsgMgr.LoadPendingEvent(runCtx, sessionID)
	if err != nil {
		slog.Error("[runtime] Handle: LoadPendingEvent failed", "sessionID", sessionID, "error", err)
		// Fall through to normal Run — better to respond than to drop the message.
		ec = nil
	}
	if ec != nil {
		slog.Info("[runtime] Handle: pending event found, using RunEventReply", "sessionID", sessionID, "eventType", ec.EventType, "deviceSessionID", ec.SessionID)

		// Cancel the deferred notification immediately — the user has responded.
		// This must happen before RunEventReply (which may take seconds for LLM processing)
		// to prevent the debounce timer from firing before tool execution completes.
		// Dispatcher keys by userID (per-user debounce backlog).
		if rt.runner.OnEventProcessed != nil {
			slog.Info("[runtime] Handle: user responded, notifying dispatcher", "agentSessionID", sessionID, "userID", userID)
			rt.runner.OnEventProcessed(userID)
		}

		// Event context found: use RunEventReply (with tools). User message
		// was already persisted above (unified with the Run path).
		eventCh, runErr := rt.runner.RunEventReply(runCtx, userID, sessionID, ec)
		if runErr != nil {
			return fmt.Errorf("agent event reply failed: %w", runErr)
		}
		go rt.streamResponse(runCtx, eventCh, sender, userID, msg.Content, sessionID, ec)
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

// ReconcilePendingEventsWithDevice queries the device for each pending event
// in the agent session and marks EVENT_RESOLVED any whose corresponding
// permission/question is no longer pending on the device side. Called at
// HandleAIEventBatch entry to prevent stale EVENT_PENDING rows — left over
// from earlier AI runs that terminated without resolving (relay, error, tool
// iteration limit) — from polluting the AI's view of an incoming batch.
//
// Without this, the AI would see a confusing pile of old + new events and
// frequently choose to relay instead of acting on any of them, leaving the
// new event unhandled. With this, the AI sees only events that are still
// genuinely pending on the device, giving it a clean shot at handling them.
//
// Best-effort: device query failures are logged and the group is skipped
// (we can't confirm device state, so don't risk marking rows resolved).
// Reuses the same device endpoints the dispatcher's IsStillPending uses
// (/api/v1/permissions, /api/v1/questions), via the same gateway proxy.
func (rt *ClawAgentRuntime) ReconcilePendingEventsWithDevice(ctx context.Context, userID, sessionID string) {
	if rt == nil || rt.MsgMgr == nil || rt.gwClient == nil || rt.gwRegistry == nil {
		return
	}
	ecs, err := rt.MsgMgr.LoadAllPendingEvents(ctx, sessionID)
	if err != nil {
		slog.Warn("[runtime] reconcile: load pending failed",
			"sessionID", sessionID, "error", err)
		return
	}
	if len(ecs) == 0 {
		return
	}

	// Group by (deviceID, path, eventType) — the device API is per-device and
	// the two event types hit different endpoints.
	type groupKey struct {
		deviceID  string
		path      string
		eventType string
	}
	groups := make(map[groupKey][]*EventContext)
	for _, ec := range ecs {
		k := groupKey{ec.DeviceID, ec.Path, ec.EventType}
		groups[k] = append(groups[k], ec)
	}

	for key, groupEcs := range groups {
		var proxyPath, wrapperKey string
		switch key.eventType {
		case "permission", "permission_batch":
			proxyPath = "/api/v1/permissions"
			wrapperKey = "permissions"
		case "question":
			proxyPath = "/api/v1/questions"
			wrapperKey = "questions"
		default:
			continue
		}

		var rawMsg json.RawMessage
		if err := gateway.ProxyDeviceSessionRequest(rt.gwClient, rt.gwRegistry,
			userID, key.deviceID, key.path, "GET", proxyPath, nil, &rawMsg); err != nil {
			slog.Warn("[runtime] reconcile: device query failed, skipping group",
				"sessionID", sessionID, "deviceID", key.deviceID,
				"path", key.path, "eventType", key.eventType, "error", err)
			continue
		}
		deviceIDs, err := parseDeviceIDList(rawMsg, wrapperKey)
		if err != nil {
			slog.Warn("[runtime] reconcile: parse failed, skipping group",
				"sessionID", sessionID, "eventType", key.eventType, "error", err)
			continue
		}
		deviceSet := make(map[string]struct{}, len(deviceIDs))
		for _, id := range deviceIDs {
			deviceSet[id] = struct{}{}
		}

		for _, ec := range groupEcs {
			var ids []string
			if key.eventType == "question" {
				for _, q := range ec.Questions {
					if q.ID != "" {
						ids = append(ids, q.ID)
					}
				}
			} else {
				ids = PermissionIDsFromEvent(ec)
			}
			if len(ids) == 0 {
				continue
			}
			anyPending := false
			for _, id := range ids {
				if _, ok := deviceSet[id]; ok {
					anyPending = true
					break
				}
			}
			if !anyPending {
				for _, id := range ids {
					if err := rt.MsgMgr.MarkEventResolvedByID(ctx, sessionID, id, ResolvedReasonDeviceAlreadyDone); err != nil {
						slog.Warn("[runtime] reconcile: mark resolved failed",
							"sessionID", sessionID, "id", id, "error", err)
					}
				}
				slog.Info("[runtime] reconcile: marked stale event resolved (device already done)",
					"sessionID", sessionID, "eventType", key.eventType,
					"ids", ids, "deviceID", key.deviceID)
			}
		}
	}
}

// parseDeviceIDList mirrors dispatcher.parseIDList — duplicated here to avoid
// pulling dispatcher into clawagent (cycle). Handles both bare-array and
// {"wrapperKey": [...]} response shapes returned by the csc device API.
func parseDeviceIDList(rawMsg json.RawMessage, wrapperKey string) ([]string, error) {
	trimmed := bytes.TrimSpace(rawMsg)
	if len(trimmed) == 0 {
		return []string{}, nil
	}
	// Try wrapper form first.
	var wrapped map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &wrapped); err == nil {
		if inner, ok := wrapped[wrapperKey]; ok {
			return decodeDeviceIDs(inner)
		}
	}
	// Bare array.
	return decodeDeviceIDs(trimmed)
}

func decodeDeviceIDs(rawMsg json.RawMessage) ([]string, error) {
	var entries []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rawMsg, &entries); err != nil {
		return nil, fmt.Errorf("decode id list: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.ID != "" {
			ids = append(ids, e.ID)
		}
	}
	return ids, nil
}

// Stop gracefully stops background goroutines.
func (rt *ClawAgentRuntime) Stop() {
	rt.bgCancel()
}
