package clawagent

import (
	"context"
	"log"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/channel"
	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/gateway"
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


// New creates a new ClawAgentRuntime.
func New(db *gorm.DB, cfg *config.Config, gwRegistry *gateway.GatewayRegistry, gwClient *gateway.Client) (*ClawAgentRuntime, error) {
	encryptionKey := cfg.ClawAgent.EncryptionKey
	if encryptionKey == "" {
		return nil, fmt.Errorf("CLAWAGENT_ENCRYPTION_KEY is required")
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
	rt.DeviceProxy = NewDeviceProxyClient(gwRegistry, gwClient, db)
	rt.EventBus = NewEventBus()
	rt.TaskRegistry = NewTaskRegistry(db)
	rt.EventHandler = NewEventHandler(rt)
	rt.IntentHndlr = NewIntentHandler(rt.DeviceProxy)

	// Initialize LLM client and runner
	llmClient := NewLLMClient()
	rt.runner = NewAgentRunner(rt, llmClient)

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

	baseKey := fmt.Sprintf("agent:clawagent:%s:%s:%s",
		msg.ExternalChatType, msg.ExternalChatID, userID)
	resetType := "direct"
	if msg.ExternalChatType == "group" {
		resetType = "group"
		baseKey = fmt.Sprintf("agent:clawagent:%s:%s:group", msg.ExternalChatType, msg.ExternalChatID)
	}

	sessionID, err := rt.resolveActiveSession(userID, baseKey, resetType)
	if err != nil {
		return fmt.Errorf("resolve session: %w", err)
	}

	// Use background context for async LLM calls (request ctx may be cancelled after response)
	runCtx := rt.bgCtx
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

	go rt.streamResponse(runCtx, eventCh, sender, userID, msg.Content, sessionID)
	return nil
}

// streamResponse consumes the event channel and sends responses to the channel sender.
// When the sender implements StreamSender, it uses streaming for incremental delivery.
func (rt *ClawAgentRuntime) streamResponse(
	ctx context.Context,
	eventCh <-chan AgentEvent,
	sender channel.Sender,
	userID, userMessage, sessionID string,
) {
	var buf strings.Builder
	var lastFlush time.Time
	var assistantReply strings.Builder

	streamSender, hasStream := sender.(channel.StreamSender)

	flush := func(finish bool) {
		if buf.Len() > 0 || finish {
			content := buf.String()
			if hasStream {
				if err := streamSender.SendStream(ctx, content, finish); err != nil {
					log.Printf("[clawagent:stream] error: %v", err)
				}
			} else {
				if err := sender.Send(ctx, content); err != nil {
					log.Printf("[clawagent:send] error: %v", err)
				}
			}
			buf.Reset()
			lastFlush = time.Now()
		}
	}

	for evt := range eventCh {
		if evt.Error != "" {
			if hasStream {
				_ = streamSender.SendStream(ctx, fmt.Sprintf("⚠️ %s", evt.Error), true)
			} else {
				_ = sender.Send(ctx, fmt.Sprintf("⚠️ %s", evt.Error))
			}
			continue
		}
		if evt.IsFinal {
			flush(true)
			// Async memory refresh
			reply := assistantReply.String()
			if reply != "" {
				go rt.MemoryMgr.Refresh(rt.bgCtx, userID, userMessage, reply, rt.runner.llmClient, rt.agentCfg)
			}
			// Async session meta update
			go rt.SessionMeta.IncrementMessageCount(sessionID)
			// Async compaction check
			go rt.maybeCompact(rt.bgCtx, sessionID)
			break
		}
		if evt.Content != "" {
			buf.WriteString(evt.Content)
			assistantReply.WriteString(evt.Content)
			if buf.Len() > 500 || (!lastFlush.IsZero() && time.Since(lastFlush) > 2*time.Second) {
				flush(false)
			}
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
