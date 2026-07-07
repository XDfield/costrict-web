package clawagent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"log/slog"
	"time"

	"github.com/costrict/costrict-web/server/internal/clawagent/tools"
	"github.com/google/uuid"
)

// AgentEvent represents an event in the agent's response stream.
type AgentEvent struct {
	Type    string `json:"type"`    // "token", "tool_call", "tool_result", "error", "done"
	Content string `json:"content,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Args    string `json:"args,omitempty"`
	Result  string `json:"result,omitempty"`
	Error   string `json:"error,omitempty"`
	IsFinal bool   `json:"is_final"`
	IsError bool   `json:"is_error"`
}

// AgentRunner orchestrates the conversation flow with LLM.
type AgentRunner struct {
	rt           *ClawAgentRuntime
	llmClient    llmGenerator
	msgMgr       *MessageManager
	mu           sync.Mutex
	sessions     map[string]*ConversationSession
	toolRegistry *tools.Registry

	// In-flight Run tracking: each sessionID can have at most one active Run
	// goroutine. startRun atomically swaps in a new entry and cancels the
	// previous one (Option C coalesce: cancel + re-run with full DB history).
	inflightMu      sync.Mutex
	inflightCancels map[string]*inflightEntry

	// OnEventProcessed is called when a pending event (permission/question) is
	// resolved via tool execution. The agent's userID is passed as argument;
	// the dispatcher keys per-user backlogs so a single reply drains every
	// pending notification for that user.
	OnEventProcessed func(userID string)
}

// inflightEntry tracks one in-flight Run. The identity pointer gives
// unregister a way to detect "I'm still the registered entry" without
// comparing cancel funcs (which aren't comparable) — if startRun swapped a
// newer entry in, our deferred unregister leaves it alone.
type inflightEntry struct {
	cancel   context.CancelFunc
	identity *struct{} // unique per entry, used only for pointer identity
}

// ConversationSession holds the in-memory state of a conversation (metadata only, messages are in DB).
type ConversationSession struct {
	SessionID string
	UserID    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// EventContext holds pending event data for AI tool execution.
// The pending/resolved state is derived from the latest event-kind row in
// agent_session_messages — IsProcessed was removed to avoid divergence between
// local truth and device truth. LoadPendingEvent / MarkEventResolved are the
// canonical read/write paths.
type EventContext struct {
	EventType  string         `json:"eventType"`    // "permission" or "question"
	SessionID  string         `json:"sessionId"`    // device-side session ID from the dispatcher
	DeviceID   string         `json:"deviceId"`
	Path       string         `json:"path"`         // workspace directory for device proxy routing
	ActionData map[string]any `json:"actionData"`   // raw action data from dispatcher

	// Resolved fields extracted from ActionData
	PermissionID string         `json:"permissionId,omitempty"` // for permission events
	Questions    []QuestionItem `json:"questions,omitempty"`    // for question events
}

// QuestionItem represents a single question from a device event.
type QuestionItem struct {
	ID       string           `json:"id,omitempty"`
	Question string           `json:"question"`
	Header   string           `json:"header,omitempty"`
	Multiple bool             `json:"multiple"`
	Options  []QuestionOption `json:"options,omitempty"`
}

// QuestionOption represents an option in a question.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// NewAgentRunner creates a new AgentRunner.
func NewAgentRunner(rt *ClawAgentRuntime, llmClient *LLMClient) *AgentRunner {
	r := &AgentRunner{
		rt:              rt,
		llmClient:       llmClient,
		sessions:        make(map[string]*ConversationSession),
		inflightCancels: make(map[string]*inflightEntry),
	}

	// Initialize and register event tools
	reg := tools.NewRegistry()
	reg.Register(tools.NewPermissionTool())
	reg.Register(tools.NewQuestionTool())
	reg.Register(tools.NewSessionInfoTool())
	reg.Register(tools.NewRecentMessagesTool())
	r.toolRegistry = reg

	return r
}

// startRun is the single entry point for spawning a Run-style goroutine on a
// sessionID. It enforces the Option C coalesce invariant: at most one in-flight
// Run per sessionID. A new invocation atomically swaps its entry into the map
// and cancels the previous Run's ctx. The cancelled Run's goroutine observes
// ctx.Done() and exits silently (no user-visible error event, no assistant
// message persisted) — see the runCtx checks sprinkled in runFn bodies.
//
// runFn performs the actual work (LLM call, tool calls, etc.) and writes
// events to eventCh. startRun owns eventCh creation, close, and lifecycle.
// runFn MUST:
//   - Treat runCtx as authoritative — ctx may live longer than runCtx
//   - Use sendEvent (or equivalent select-on-runCtx) when writing to eventCh
//     to avoid blocking on a full buffer after the consumer has exited
//   - Skip error-event emission when runCtx.Err() != nil (silent cancel)
func (r *AgentRunner) startRun(ctx context.Context, sessionID string, runFn func(runCtx context.Context, eventCh chan<- AgentEvent)) <-chan AgentEvent {
	runCtx, cancel := context.WithCancel(ctx)
	entry := &inflightEntry{
		cancel:   cancel,
		identity: &struct{}{},
	}

	// Atomic swap: read old + write new under a single lock, then cancel old
	// outside the lock to avoid blocking the old goroutine's unregister path.
	r.inflightMu.Lock()
	old := r.inflightCancels[sessionID]
	r.inflightCancels[sessionID] = entry
	r.inflightMu.Unlock()

	if old != nil {
		old.cancel()
	}

	eventCh := make(chan AgentEvent, 128)
	go func() {
		defer close(eventCh)
		defer cancel()
		defer func() {
			// Only remove if we're still the registered entry — a newer Run
			// may have already swapped in, and we must not evict it.
			r.inflightMu.Lock()
			if cur := r.inflightCancels[sessionID]; cur == entry {
				delete(r.inflightCancels, sessionID)
			}
			r.inflightMu.Unlock()
		}()
		runFn(runCtx, eventCh)
	}()
	return eventCh
}

// sendEvent writes ev to ch, bailing out silently if runCtx is cancelled.
// Use this from inside runFn instead of a bare `ch <- ev` — when the
// streamResponse consumer has already exited (e.g. Run was cancelled),
// a bare send would block forever once the 128-event buffer fills.
func sendEvent(runCtx context.Context, ch chan<- AgentEvent, ev AgentEvent) bool {
	select {
	case <-runCtx.Done():
		return false
	case ch <- ev:
		return true
	}
}

// Run starts or continues a conversation.
// For group chats, senderUserID can be passed separately from the session owner
// to enable persona/memory isolation while sharing conversation context.
//
// The user message MUST be persisted by the caller (runtime.Handle) before
// invoking Run — Run itself does not append the user message, so a cancelled
// Run can never lose user input. Run loads the conversation history (including
// the just-appended user message) from DB to build the LLM context.
func (r *AgentRunner) Run(ctx context.Context, userID, sessionID, message string, senderUserID ...string) (<-chan AgentEvent, error) {
	// Use senderUserID for group persona/memory isolation
	promptUserID := userID
	if len(senderUserID) > 0 && senderUserID[0] != "" {
		promptUserID = senderUserID[0]
	}

	return r.startRun(ctx, sessionID, func(runCtx context.Context, eventCh chan<- AgentEvent) {
		r.getOrCreateSession(sessionID, userID)

		// Build system prompt with persona + memory
		systemPrompt, err := r.buildSystemPrompt(runCtx, promptUserID)
		if err != nil {
			if runCtx.Err() == nil {
				sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to build context: %v", err), IsError: true, IsFinal: true})
			}
			return
		}

		// Resolve provider config
		provCfg, err := r.resolveProvider(runCtx, userID)
		if err != nil {
			if runCtx.Err() == nil {
				sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to resolve provider: %v", err), IsError: true, IsFinal: true})
			}
			return
		}

		// Build messages array with system prompt + history (caller has already
		// appended the user message to DB).
		messages, err := r.buildMessages(runCtx, sessionID, systemPrompt)
		if err != nil {
			if runCtx.Err() == nil {
				sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to build messages: %v", err), IsError: true, IsFinal: true})
			}
			return
		}

		// Stream the response
		streamCh, errCh := r.llmClient.GenerateStream(runCtx, *provCfg, messages)

		var fullResponse string
		for evt := range streamCh {
			for _, choice := range evt.Choices {
				if choice.Delta.Content != "" {
					fullResponse += choice.Delta.Content
					if !sendEvent(runCtx, eventCh, AgentEvent{
						Type:    "token",
						Content: choice.Delta.Content,
					}) {
						return
					}
				}
				if choice.FinishReason != "" {
					// Cancelled Runs must not persist partial output or emit IsFinal —
					// Option C requires the cancelled Run to die silently so the newer
					// Run produces the canonical response with full history. Without
					// this guard, a cancel racing between FinishReason and persistence
					// leaves an orphan assistant row that the user never sees.
					if runCtx.Err() != nil {
						return
					}
					// Strip any hallucinated <tool_call> text XML before persisting —
					// Run() has no tool registry, so the model's attempt to call tools
					// here is just noise that shouldn't pollute session history.
					persisted := fullResponse
					if _, cleaned := parseTextToolCalls(persisted); cleaned != persisted {
						slog.Warn("[agent] Run: stripped text-encoded tool_call XML before persisting",
							"sessionID", sessionID, "before", len(persisted), "after", len(cleaned))
						persisted = cleaned
					}
					r.addAssistantMessage(runCtx, sessionID, persisted)
					sendEvent(runCtx, eventCh, AgentEvent{
						Type:    "done",
						IsFinal: true,
					})
				}
			}
		}

		// Check for streaming errors. Suppress error event when runCtx was
		// cancelled — cancellation is the Option C coalesce path, not a real
		// error, and the user must not see a "⚠️ context canceled" message.
		select {
		case err := <-errCh:
			if err != nil && runCtx.Err() == nil {
				if fullResponse == "" {
					sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: err.Error(), IsError: true, IsFinal: true})
				} else {
					// Partial response, still mark as done
					sendEvent(runCtx, eventCh, AgentEvent{Type: "done", IsFinal: true})
				}
			}
		default:
		}
	}), nil
}

// SetMsgMgr sets the MessageManager after construction (called during runtime init).
func (r *AgentRunner) SetMsgMgr(mgr *MessageManager) {
	r.msgMgr = mgr
}

// RunWithSystem is like Run but inserts an extra system message between the
// persona+memory prompt and the conversation history. Used by event
// notifications to deliver context (pending events list, real permission IDs)
// without polluting the user's persistent user-message history. The extra
// system message is not persisted to the DB — it lives only in this LLM
// request, so subsequent turns won't see it (avoiding stale "pending" prompts
// after the event has been resolved).
//
// Caller must persist the user-side placeholder via AddUserMessage first;
// this method reads history from DB and does not append a user message
// itself (matching the contract of Run after the runtime.Handle refactor).
func (r *AgentRunner) RunWithSystem(ctx context.Context, userID, sessionID, extraSystem string) <-chan AgentEvent {
	return r.startRun(ctx, sessionID, func(runCtx context.Context, eventCh chan<- AgentEvent) {
		r.getOrCreateSession(sessionID, userID)

		systemPrompt, err := r.buildSystemPrompt(runCtx, userID)
		if err != nil {
			if runCtx.Err() == nil {
				sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to build context: %v", err), IsError: true, IsFinal: true})
			}
			return
		}

		provCfg, err := r.resolveProvider(runCtx, userID)
		if err != nil {
			if runCtx.Err() == nil {
				sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to resolve provider: %v", err), IsError: true, IsFinal: true})
			}
			return
		}

		messages, err := r.buildMessagesWithExtra(runCtx, sessionID, systemPrompt, extraSystem)
		if err != nil {
			if runCtx.Err() == nil {
				sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to build messages: %v", err), IsError: true, IsFinal: true})
			}
			return
		}

		streamCh, errCh := r.llmClient.GenerateStream(runCtx, *provCfg, messages)

		var fullResponse string
		for evt := range streamCh {
			for _, choice := range evt.Choices {
				if choice.Delta.Content != "" {
					fullResponse += choice.Delta.Content
					if !sendEvent(runCtx, eventCh, AgentEvent{
						Type:    "token",
						Content: choice.Delta.Content,
					}) {
						return
					}
				}
				if choice.FinishReason != "" {
					if runCtx.Err() != nil {
						return
					}
					persisted := fullResponse
					if _, cleaned := parseTextToolCalls(persisted); cleaned != persisted {
						slog.Warn("[agent] RunWithSystem: stripped text-encoded tool_call XML before persisting",
							"sessionID", sessionID, "before", len(persisted), "after", len(cleaned))
						persisted = cleaned
					}
					r.addAssistantMessage(runCtx, sessionID, persisted)
					sendEvent(runCtx, eventCh, AgentEvent{
						Type:    "done",
						IsFinal: true,
					})
				}
			}
		}

		select {
		case err := <-errCh:
			if err != nil && runCtx.Err() == nil {
				sendEvent(runCtx, eventCh, AgentEvent{
					Type:    "error",
					Error:   err.Error(),
					IsError: true,
					IsFinal: true,
				})
			}
		default:
		}
	})
}

func (r *AgentRunner) getOrCreateSession(sessionID, userID string) *ConversationSession {
	r.mu.Lock()
	defer r.mu.Unlock()

	if sess, ok := r.sessions[sessionID]; ok {
		sess.UpdatedAt = time.Now()
		if sess.UserID == "" {
			sess.UserID = userID
		}
		return sess
	}

	sess := &ConversationSession{
		SessionID: sessionID,
		UserID:    userID,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	r.sessions[sessionID] = sess
	return sess
}

func (r *AgentRunner) buildSystemPrompt(ctx context.Context, userID string) (string, error) {
	persona, err := r.rt.PersonaMgr.Load(ctx, userID)
	if err != nil {
		return "", err
	}

	memoryContent, err := r.rt.MemoryMgr.Load(ctx, userID)
	if err != nil {
		return "", err
	}

	return r.rt.PersonaMgr.BuildInstruction(persona, memoryContent), nil
}

func (r *AgentRunner) resolveProvider(ctx context.Context, userID string) (*ProviderConfig, error) {
	providers, err := r.rt.ProviderMgr.LoadByUser(ctx, userID)
	if err != nil {
		return nil, err
	}

	if len(providers) == 0 {
		return nil, fmt.Errorf("no LLM provider available")
	}

	prov := providers[0]
	apiKey, err := DecryptAPIKey(prov.APIKeyEncrypted, r.rt.agentCfg.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt API key: %w", err)
	}

	baseURL := prov.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	return &ProviderConfig{
		ProviderType: prov.ProviderType,
		APIKey:       apiKey,
		BaseURL:      baseURL,
		ModelName:    prov.ModelName,
	}, nil
}

func (r *AgentRunner) buildMessages(ctx context.Context, sessionID, systemPrompt string) ([]ChatMessage, error) {
	return r.buildMessagesWithExtra(ctx, sessionID, systemPrompt, "")
}

// buildMessagesWithExtra builds the LLM message stream with an extra system
// message inserted between the primary system prompt and history. Used by
// event notifications to inject context that should not leak into long-term
// memory (which is the user's conversation).
func (r *AgentRunner) buildMessagesWithExtra(ctx context.Context, sessionID, systemPrompt, extraSystem string) ([]ChatMessage, error) {
	messages := []ChatMessage{
		{Role: "system", Content: systemPrompt},
	}
	if extraSystem != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: extraSystem})
	}

	history, err := r.msgMgr.LoadMessages(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	messages = append(messages, history...)
	return messages, nil
}

func (r *AgentRunner) addAssistantMessage(ctx context.Context, sessionID, content string) {
	if err := r.msgMgr.AppendMessage(ctx, sessionID, ChatMessage{Role: "assistant", Content: content}); err != nil {
		slog.Error("[agent] addAssistantMessage: failed to save", "sessionID", sessionID, "error", err)
	}
	r.mu.Lock()
	if sess, ok := r.sessions[sessionID]; ok {
		sess.UpdatedAt = time.Now()
	}
	r.mu.Unlock()
}

// AddUserMessage appends a user message to the session history in DB.
func (r *AgentRunner) AddUserMessage(ctx context.Context, sessionID, content string) {
	if err := r.msgMgr.AppendMessage(ctx, sessionID, ChatMessage{Role: "user", Content: content}); err != nil {
		slog.Error("[agent] AddUserMessage: failed to save", "sessionID", sessionID, "error", err)
	}
	r.mu.Lock()
	if sess, ok := r.sessions[sessionID]; ok {
		sess.UpdatedAt = time.Now()
	}
	r.mu.Unlock()
}

// SummarizeSession compresses session data using LLM.
func (r *AgentRunner) SummarizeSession(ctx context.Context, sessionID string, keepRecent int) (string, error) {
	// Find the session in memory for userID
	r.mu.Lock()
	sess, ok := r.sessions[sessionID]
	r.mu.Unlock()

	if !ok {
		return "", fmt.Errorf("session not found: %s", sessionID)
	}

	provCfg, err := r.resolveProvider(ctx, sess.UserID)
	if err != nil {
		return "", err
	}

	// Load messages from DB
	messages, err := r.msgMgr.LoadMessages(ctx, sessionID)
	if err != nil {
		return "", err
	}

	// Prepare messages for summarization
	summaryInput := formatMessagesForSummary(messages, keepRecent)

	promptBuilder := "请将以下对话历史压缩为一段简洁的摘要，保留关键信息和决策。\n\n" +
		summaryInput +
		"\n请输出JSON格式：{\"summary\": \"摘要内容\", \"key_points\": [\"要点1\", \"要点2\"]}"

	resp, err := r.llmClient.Generate(ctx, *provCfg, []ChatMessage{
		{Role: "user", Content: promptBuilder},
	})
	if err != nil {
		return "", err
	}

	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("no response from LLM")
	}

	return resp.Choices[0].Message.Content, nil
}

// GetSession returns a copy of the conversation session (without messages — messages are in DB).
// The caller should use MessageManager.LoadMessages() to retrieve messages separately.
func (r *AgentRunner) GetSession(sessionID string) *ConversationSession {
	r.mu.Lock()
	defer r.mu.Unlock()

	sess, ok := r.sessions[sessionID]
	if !ok {
		return nil
	}

	return &ConversationSession{
		SessionID: sess.SessionID,
		UserID:    sess.UserID,
		CreatedAt: sess.CreatedAt,
		UpdatedAt: sess.UpdatedAt,
	}
}

// Tool names for event handling
const (
	ToolReplyPermission = "reply_permission"
	ToolReplyQuestion   = "reply_question"
)

// toolDefinitions converts tools.Definition to []ToolDefinition for the LLM API.
func (r *AgentRunner) toolDefinitions() []ToolDefinition {
	all := r.toolRegistry.All()
	defs := make([]ToolDefinition, len(all))
	for i, t := range all {
		d := t.Definition()
		defs[i] = ToolDefinition{
			Type: "function",
			Function: ToolFunctionDef{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.Parameters,
			},
		}
	}
	return defs
}


// RunEventReply runs a tool-capable LLM call for handling event replies (permission/question).
// Unlike Run(), this uses non-streaming GenerateWithTools and handles tool execution.
// ec is the EventContext loaded from chat_messages (LoadPendingEvent); it provides
// the device ID, path, and device session ID needed for tool execution.
func (r *AgentRunner) RunEventReply(ctx context.Context, userID, sessionID string, ec *EventContext) (<-chan AgentEvent, error) {
	return r.startRun(ctx, sessionID, func(runCtx context.Context, eventCh chan<- AgentEvent) {
		slog.Debug("[agent] RunEventReply: starting", "sessionID", sessionID, "userID", userID)

		sess := r.getOrCreateSession(sessionID, userID)
		_ = sess // sess no longer carries EventData; ec is authoritative

		// Build system prompt with event context + tools
		systemPrompt, err := r.buildSystemPrompt(runCtx, userID)
		if err != nil {
			if runCtx.Err() == nil {
				sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to build context: %v", err), IsError: true, IsFinal: true})
			}
			return
		}

		// Append event-specific instructions based on EventContext
		if ec != nil {
			eventInstructions := tools.BuildInstructions(ec.EventType)
			systemPrompt += eventInstructions
		}

		// Resolve provider config
		provCfg, err := r.resolveProvider(runCtx, userID)
		if err != nil {
			if runCtx.Err() == nil {
				sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to resolve provider: %v", err), IsError: true, IsFinal: true})
			}
			return
		}

		toolDefs := r.toolDefinitions()
		maxToolIterations := 10

		for iter := 0; iter < maxToolIterations; iter++ {
			if runCtx.Err() != nil {
				return
			}
			// Load messages from DB
			history, err := r.msgMgr.LoadMessages(runCtx, sessionID)
			if err != nil {
				if runCtx.Err() == nil {
					sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to load messages: %v", err), IsError: true, IsFinal: true})
				}
				return
			}

			// Build messages array
			messages := []ChatMessage{
				{Role: "system", Content: systemPrompt},
			}
			messages = append(messages, history...)

			slog.Debug("[agent] RunEventReply: LLM call", "sessionID", sessionID, "iter", iter)
			resp, err := r.llmClient.GenerateWithTools(runCtx, *provCfg, messages, toolDefs)
			if err != nil {
				if runCtx.Err() == nil {
					sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: fmt.Sprintf("LLM call failed: %v", err), IsError: true, IsFinal: true})
				}
				return
			}

			if len(resp.Choices) == 0 {
				if runCtx.Err() == nil {
					sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: "LLM returned no choices", IsError: true, IsFinal: true})
				}
				return
			}

			choice := resp.Choices[0]
			// DEBUG: log raw LLM decision so we can diagnose "AI said approve
			// but no tool was invoked" cases. finish_reason reveals whether
			// the LLM considers the turn done ("stop") vs expects tool
			// dispatch ("tool_calls"). Structured tool_calls count + content
			// preview help distinguish "LLM emitted text-only reply" from
			// "LLM emitted tool call but it got dropped somewhere".
			slog.Info("[agent] RunEventReply: LLM choice",
				"sessionID", sessionID, "iter", iter,
				"finishReason", choice.FinishReason,
				"structuredToolCalls", len(choice.Message.ToolCalls),
				"contentLen", len(choice.Message.Content),
				"contentPreview", contentPreview(choice.Message.Content, 240))

			// Some LLMs (notably GLM-family) sometimes emit tool calls as text
			// inside content using an XML-like convention instead of — or in
			// addition to — the structured tool_calls field. Always run the
			// parser so the XML is stripped from content even when structured
			// ToolCalls are also present (in that case we keep the structured
			// calls and only use the parser to clean the leaked text).
			textParsed := false
			if parsed, cleaned := parseTextToolCalls(choice.Message.Content); len(parsed) > 0 {
				slog.Info("[agent] RunEventReply: recovered text-encoded tool calls from content",
					"sessionID", sessionID, "count", len(parsed),
					"hadStructured", len(choice.Message.ToolCalls) > 0)
				if len(choice.Message.ToolCalls) == 0 {
					choice.Message.ToolCalls = parsed
				}
				choice.Message.Content = cleaned
				textParsed = true
			}

			// Save assistant message to DB. Guard with runCtx.Err() so a cancelled
			// Run (Option C coalesce path) doesn't leave an orphan assistant row
			// when cancel races between choice arrival and persistence.
			if runCtx.Err() != nil {
				return
			}
			r.addAssistantMessage(runCtx, sessionID, choice.Message.Content)

			// When tool calls were recovered from text, surface the leftover
			// chat content (e.g., "好的，已经批准了...") before executing tools
			// so the user sees the acknowledgement that preceded the leaked
			// <tool_call> block.
			if textParsed && choice.Message.Content != "" {
				if !sendEvent(runCtx, eventCh, AgentEvent{Type: "token", Content: choice.Message.Content}) {
					return
				}
			}

			// Check for tool calls
			if len(choice.Message.ToolCalls) > 0 {
				for _, tc := range choice.Message.ToolCalls {
					if !sendEvent(runCtx, eventCh, AgentEvent{
						Type: "tool_call",
						Tool: tc.Function.Name,
						Args: tc.Function.Arguments,
					}) {
						return
					}

					// Execute tool via registry. Tool execution is NOT cancellable —
					// once we've sent the request to the gateway (e.g. permission
					// reply proxy), the side-effect has happened and we let it
					// complete. ctx here is runCtx for consistency, but tool
					// implementations should use it for transport cancellation
					// only, not for aborting committed actions.
					deviceID := ""
					directory := ""
					deviceSessionID := ""
					if ec != nil {
						deviceID = ec.DeviceID
						directory = ec.Path
						deviceSessionID = ec.SessionID
					}
					if deviceID == "" {
						slog.Error("[agent] RunEventReply: empty deviceID for tool execution", "sessionID", sessionID, "tool", tc.Function.Name)
					}
					toolCtx := &tools.Context{
						DeviceID:      deviceID,
						Directory:     directory,
						SessionID:     deviceSessionID,
						UserID:        userID,
						DB:            r.rt.db,
						DeviceProxy:   r.rt.DeviceProxy,
						MarkProcessed: func() { r.markEventResolved(userID, sessionID, deviceSessionID) },
						DrainSessionPermissions: r.makePermissionDrainer(userID, sessionID, deviceID, deviceSessionID),
					}
					result, execErr := r.toolRegistry.Execute(runCtx, tc.Function.Name, tc.Function.Arguments, toolCtx)
					slog.Debug("[agent] RunEventReply: tool executed", "sessionID", sessionID, "tool", tc.Function.Name, "error", execErr)
					if execErr != nil {
						result = fmt.Sprintf("工具执行失败: %v", execErr)
					}

					if !sendEvent(runCtx, eventCh, AgentEvent{
						Type:   "tool_result",
						Tool:   tc.Function.Name,
						Result: result,
					}) {
						return
					}

					// Add tool call + result to DB
					r.addToolResult(runCtx, sessionID, tc, result)
				}
				// Continue loop to let LLM process tool results
				continue
			}

			// No tool calls — this is the final response
			slog.Info("[agent] RunEventReply: terminating turn without tool call (LLM chose to relay only)",
				"sessionID", sessionID, "iter", iter,
				"finishReason", choice.FinishReason,
				"hadEventContext", ec != nil,
				"eventType", func() string {
					if ec != nil {
						return ec.EventType
					}
					return ""
				}(),
				"contentLen", len(choice.Message.Content),
				"contentPreview", contentPreview(choice.Message.Content, 240))
			fullResponse := choice.Message.Content
			if fullResponse != "" {
				if !sendEvent(runCtx, eventCh, AgentEvent{Type: "token", Content: fullResponse}) {
					return
				}
			}
			sendEvent(runCtx, eventCh, AgentEvent{Type: "done", IsFinal: true})
			return
		}

		slog.Error("[agent] RunEventReply: tool call iteration limit reached", "sessionID", sessionID)
		sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: "Tool call iteration limit reached", IsError: true, IsFinal: true})
	}), nil
}

// RunEventReplyWithSystem is the batch-notification counterpart of
// RunEventReply. It accepts an extra (non-persisted) system message and arms
// the tool registry so the AI can choose, per event in the batch, whether to
// auto-execute (call reply_permission / reply_question directly based on
// memory + context) or relay to the user in natural language.
//
// Device/session context for each tool call is resolved at tool-call time by
// looking up the ID in the args against the session's pending events list —
// this is what makes batch mode work: a single tool registry invocation can
// address any event in the batch, and MarkEventResolved flips exactly the
// matching row (not all pending rows).
func (r *AgentRunner) RunEventReplyWithSystem(ctx context.Context, userID, sessionID, extraSystem string) (<-chan AgentEvent, error) {
	return r.startRun(ctx, sessionID, func(runCtx context.Context, eventCh chan<- AgentEvent) {
		slog.Info("[agent] RunEventReplyWithSystem: starting",
			"sessionID", sessionID, "userID", userID, "extraSystemLen", len(extraSystem))

		r.getOrCreateSession(sessionID, userID)

		systemPrompt, err := r.buildSystemPrompt(runCtx, userID)
		if err != nil {
			if runCtx.Err() == nil {
				sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to build context: %v", err), IsError: true, IsFinal: true})
			}
			return
		}
		// Append generic event instructions so AI knows the tools' calling
		// convention. The extraSystem prompt carries the per-batch specifics.
		systemPrompt += tools.BuildInstructions("permission")
		systemPrompt += tools.BuildInstructions("question")

		provCfg, err := r.resolveProvider(runCtx, userID)
		if err != nil {
			if runCtx.Err() == nil {
				sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to resolve provider: %v", err), IsError: true, IsFinal: true})
			}
			return
		}

		toolDefs := r.toolDefinitions()
		maxToolIterations := 10
		// Tracks whether we've already used the one-shot relay fallback for
		// this run. When the LLM picks stop+pendingEvents>0+contentLen>0, we
		// inject a system reminder and give it one more turn instead of
		// terminating — catches the "AI wrote 'I decided to approve' prose
		// but didn't actually call reply_permission" failure mode. One-shot
		// per run to avoid infinite relay loops.
		relayFallbackUsed := false

		for iter := 0; iter < maxToolIterations; iter++ {
			if runCtx.Err() != nil {
				return
			}
			// Reload pending events each iteration — auto-executed events
			// transition to resolved between iterations, and we want the
			// lookup to reflect current state (so AI doesn't double-resolve).
			pendingEvents, err := r.msgMgr.LoadAllPendingEvents(runCtx, sessionID)
			if err != nil {
				slog.Warn("[agent] RunEventReplyWithSystem: LoadAllPendingEvents failed",
					"sessionID", sessionID, "error", err)
			}

			history, err := r.msgMgr.LoadMessages(runCtx, sessionID)
			if err != nil {
				if runCtx.Err() == nil {
					sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to load messages: %v", err), IsError: true, IsFinal: true})
				}
				return
			}

			messages := []ChatMessage{{Role: "system", Content: systemPrompt}}
			if extraSystem != "" {
				messages = append(messages, ChatMessage{Role: "system", Content: extraSystem})
			}
			messages = append(messages, history...)

			resp, err := r.llmClient.GenerateWithTools(runCtx, *provCfg, messages, toolDefs)
			if err != nil {
				if runCtx.Err() == nil {
					sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: fmt.Sprintf("LLM call failed: %v", err), IsError: true, IsFinal: true})
				}
				return
			}
			if len(resp.Choices) == 0 {
				if runCtx.Err() == nil {
					sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: "LLM returned no choices", IsError: true, IsFinal: true})
				}
				return
			}
			choice := resp.Choices[0]
			// DEBUG: log raw LLM decision so we can diagnose "AI said approve
			// but no tool was invoked" cases. See RunEventReply for rationale.
			slog.Info("[agent] RunEventReplyWithSystem: LLM choice",
				"sessionID", sessionID, "iter", iter,
				"finishReason", choice.FinishReason,
				"structuredToolCalls", len(choice.Message.ToolCalls),
				"contentLen", len(choice.Message.Content),
				"contentPreview", contentPreview(choice.Message.Content, 240),
				"pendingEvents", len(pendingEvents))

			// Recover text-encoded tool calls (GLM-family quirk).
			if parsed, cleaned := parseTextToolCalls(choice.Message.Content); len(parsed) > 0 {
				if len(choice.Message.ToolCalls) == 0 {
					choice.Message.ToolCalls = parsed
				}
				choice.Message.Content = cleaned
				slog.Info("[agent] RunEventReplyWithSystem: recovered text-encoded tool calls",
					"sessionID", sessionID, "count", len(parsed),
					"hadStructured", len(choice.Message.ToolCalls) > 0)
			}

			if runCtx.Err() != nil {
				return
			}
			r.addAssistantMessage(runCtx, sessionID, choice.Message.Content)

			if len(choice.Message.ToolCalls) == 0 {
				// Relay path: LLM chose to write prose without calling tools.
				// Fallback: if there are still pending events AND the LLM
				// actually wrote something (signal it tried but didn't follow
				// through with a tool call) AND we haven't used the one-shot
				// retry yet — inject a system reminder and run one more
				// iteration instead of terminating. This catches the common
				// failure mode where the AI writes "我决定自动批准" and stops,
				// leaving the request stuck.
				if len(pendingEvents) > 0 && len(choice.Message.Content) > 0 && !relayFallbackUsed {
					relayFallbackUsed = true
					slog.Info("[agent] RunEventReplyWithSystem: relay fallback triggered — injecting reminder",
						"sessionID", sessionID, "iter", iter,
						"pendingEvents", len(pendingEvents),
						"contentLen", len(choice.Message.Content))
					// Surface the LLM's prose to the user so they see the
					// reasoning, then continue to the next iteration where
					// the system reminder will push it toward a tool call.
					if !sendEvent(runCtx, eventCh, AgentEvent{Type: "token", Content: choice.Message.Content}) {
						return
					}
					reminder := fmt.Sprintf(
						"【系统提醒】你刚才的回复没有调用任何工具，但仍有 %d 条 pending 申请未处理。"+
							"必须立即采取行动：要么调用 reply_permission / reply_question 工具处理它们，"+
							"要么明确说明为什么不能处理（如需要用户提供更多信息）。"+
							"不要再写「我决定批准」「我会处理」之类的描述性文字而不落实为工具调用——那等于没处理。",
						len(pendingEvents))
					if err := r.msgMgr.AppendMessage(runCtx, sessionID, ChatMessage{Role: "system", Content: reminder}); err != nil {
						slog.Error("[agent] RunEventReplyWithSystem: append reminder failed",
							"sessionID", sessionID, "error", err)
					}
					continue
				}

				// Final natural-language reply (relay path).
				slog.Info("[agent] RunEventReplyWithSystem: terminating turn without tool call (LLM chose to relay only)",
					"sessionID", sessionID, "iter", iter,
					"finishReason", choice.FinishReason,
					"pendingEvents", len(pendingEvents),
					"contentLen", len(choice.Message.Content),
					"relayFallbackUsed", relayFallbackUsed,
					"contentPreview", contentPreview(choice.Message.Content, 240))
				fullResponse := choice.Message.Content
				if fullResponse != "" {
					if !sendEvent(runCtx, eventCh, AgentEvent{Type: "token", Content: fullResponse}) {
						return
					}
				}
				sendEvent(runCtx, eventCh, AgentEvent{Type: "done", IsFinal: true})
				return
			}

			// Execute each tool call. Resolve device/session per-call by ID.
			for _, tc := range choice.Message.ToolCalls {
				if !sendEvent(runCtx, eventCh, AgentEvent{
					Type: "tool_call", Tool: tc.Function.Name, Args: tc.Function.Arguments,
				}) {
					return
				}

				deviceID, directory, deviceSessionID := resolveToolDeviceContext(tc.Function.Name, tc.Function.Arguments, pendingEvents)

				// Semantic fast-path: when a reply-class tool can't resolve a
				// device context, the ID in args doesn't match any pending
				// event (likely already resolved by drain/device auto-accept,
				// or hallucinated by the LLM). Returning a hard error here
				// historically caused the LLM to retry the same ID, wasting
				// iterations. Instead, surface a clear semantic result so the
				// LLM understands there's nothing to do and can move on.
				var result string
				if deviceID == "" && (tc.Function.Name == "reply_permission" || tc.Function.Name == "reply_question") {
					id := extractIDFromToolCallArgs(tc.Function.Arguments)
					switch {
					case id == "":
						result = "工具调用缺少 ID 参数，无法处理。请提供 permissionID 或 questionID 后重试。"
					case len(pendingEvents) == 0:
						result = "当前没有 pending 申请待处理，无需调用此工具。请直接结束本轮（finishReason=stop）或向用户汇报已处理的申请。"
					default:
						result = fmt.Sprintf(
							"ID %s 不在当前 pending 列表里（可能已被自动处理、已被其它工具调用处理，或 ID 错误），无需再处理。"+
								"当前剩余 pending 数量: %d。请只处理仍在 pending 列表里的 ID。",
							id, len(pendingEvents))
					}
					slog.Info("[agent] RunEventReplyWithSystem: reply tool has no device context — semantic result",
						"sessionID", sessionID, "tool", tc.Function.Name,
						"id", id, "pendingCount", len(pendingEvents))
				} else {
					toolCtx := &tools.Context{
						DeviceID:    deviceID,
						Directory:   directory,
						SessionID:   deviceSessionID,
						UserID:      userID,
						DB:          r.rt.db,
						DeviceProxy: r.rt.DeviceProxy,
						MarkProcessed: func() {
							r.markEventResolvedByID(userID, sessionID, tc.Function.Name, tc.Function.Arguments)
						},
						DrainSessionPermissions: r.makePermissionDrainer(userID, sessionID, deviceID, deviceSessionID),
					}
					var execErr error
					result, execErr = r.toolRegistry.Execute(runCtx, tc.Function.Name, tc.Function.Arguments, toolCtx)
					if execErr != nil {
						result = fmt.Sprintf("工具执行失败: %v", execErr)
					}
				}

				if !sendEvent(runCtx, eventCh, AgentEvent{
					Type: "tool_result", Tool: tc.Function.Name, Result: result,
				}) {
					return
				}
				r.addToolResult(runCtx, sessionID, tc, result)
			}
			// Loop to let LLM process tool results (may emit another round of
			// tool calls for additional events in the batch, or a final relay).
		}

		slog.Error("[agent] RunEventReplyWithSystem: tool iteration limit reached", "sessionID", sessionID)
		sendEvent(runCtx, eventCh, AgentEvent{Type: "error", Error: "Tool call iteration limit reached", IsError: true, IsFinal: true})
	}), nil
}

// resolveToolDeviceContext finds the deviceID / directory / deviceSessionID
// matching the ID embedded in a tool call's args. Used by RunEventReplyWithSystem
// to wire the correct device context per call when AI is processing a batch of
// pending events. Returns empty strings when no match is found — the tool will
// surface a clear error.
// resolveToolDeviceContext maps a tool call to the (deviceID, directory,
// sessionID) triple the tool's DeviceProxy calls should target.
//
// Two resolution paths:
//
//  1. Reply-class tools (reply_permission, reply_question): the LLM emits the
//     target event's permissionID/questionID in args. We resolve to the
//     matching EventContext — exact match, works for any batch size.
//
//  2. Query-class tools (query_session_info, query_recent_messages): args do
//     not carry an event ID — these tools semantically ask "the current
//     session". Fall back to the first pending event. This is unambiguous when
//     the batch has one event (the common case after debounce); for multi-
//     event batches the LLM should use reply_permission/reply_question with
//     explicit IDs to disambiguate.
func resolveToolDeviceContext(toolName, argsJSON string, ecs []*EventContext) (deviceID, directory, deviceSessionID string) {
	if id := extractIDFromToolCallArgs(argsJSON); id != "" {
		if match := FindEventByID(ecs, id); match != nil {
			return match.DeviceID, match.Path, match.SessionID
		}
		return "", "", ""
	}
	if len(ecs) > 0 {
		return ecs[0].DeviceID, ecs[0].Path, ecs[0].SessionID
	}
	return "", "", ""
}

// extractIDFromToolCallArgs pulls permissionID or questionID out of the JSON
// args string emitted by the LLM. Both reply_permission and reply_question
// follow the same calling convention (camelCase id field at top level).
func extractIDFromToolCallArgs(argsJSON string) string {
	if argsJSON == "" {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ""
	}
	if v, ok := args["permissionID"].(string); ok && v != "" {
		return v
	}
	if v, ok := args["questionID"].(string); ok && v != "" {
		return v
	}
	return ""
}

// markEventResolvedByID is the ID-keyed variant of markEventResolved. Used by
// the batch tool path where the runner learns the target event from the tool
// call args rather than from a single EventContext baked in at run start.
func (r *AgentRunner) markEventResolvedByID(userID, sessionID, toolName, argsJSON string) {
	id := extractIDFromToolCallArgs(argsJSON)
	if id == "" {
		slog.Warn("[agent] markEventResolvedByID: no ID in tool args",
			"sessionID", sessionID, "tool", toolName, "args", argsJSON)
		return
	}
	if err := r.msgMgr.MarkEventResolvedByID(r.rt.bgCtx, sessionID, id, ResolvedReasonToolSuccess); err != nil {
		slog.Error("[agent] markEventResolvedByID: failed",
			"sessionID", sessionID, "id", id, "error", err)
		return
	}
	r.mu.Lock()
	hasCallback := r.OnEventProcessed != nil
	r.mu.Unlock()
	if hasCallback {
		r.OnEventProcessed(userID)
	}
}

// markEventResolved transitions the EVENT_PENDING row for deviceSessionID to
// EVENT_RESOLVED in chat_messages (sole source of truth for event state),
// then notifies the dispatcher to drain its per-user backlog.
func (r *AgentRunner) markEventResolved(userID, sessionID, deviceSessionID string) {
	if deviceSessionID == "" {
		return
	}
	if err := r.msgMgr.MarkEventResolved(r.rt.bgCtx, sessionID, deviceSessionID, ResolvedReasonToolSuccess); err != nil {
		slog.Error("[agent] markEventResolved: failed to update message", "sessionID", sessionID, "deviceSessionID", deviceSessionID, "error", err)
	}
	r.mu.Lock()
	hasCallback := r.OnEventProcessed != nil
	r.mu.Unlock()
	if hasCallback {
		r.OnEventProcessed(userID)
	}
}

// makePermissionDrainer builds the DrainSessionPermissions closure for a
// specific (userID, sessionID, deviceID, deviceSessionID) tuple.
//
// The drainer reads pending events from chat_messages (the agent's source of
// truth for AI-relayed permissions), filters to those targeting the same
// device session as the just-replied one, and for each remaining permission:
//   - calls DeviceProxy.ReplyPermission to approve on the device side
//   - marks the EVENT_PENDING row resolved so the next LLM iteration's
//     LoadAllPendingEvents stops surfacing it
//
// This is the AI path's counterpart to action_callback.go's
// BatchApproveSessionPermissions. That function operates on system_notifications
// (the UI-card flow's source of truth); the AI path doesn't create those rows,
// so we read from chat_messages instead. Without this drainer, the AI's
// enableAutoAccept branch would set autoAccept for FUTURE permissions but
// leave already-pending ones stuck — and worse, the next iteration would
// re-read stale EVENT_PENDING rows and try to reply them again ("deviceID is
// empty" errors or device-side double-reply errors).
//
// deviceID/deviceSessionID may be empty in degenerate tool calls — we surface
// that as a soft error so the AI sees a clear reason.
func (r *AgentRunner) makePermissionDrainer(userID, sessionID, deviceID, deviceSessionID string) func(ctx context.Context, excludePermissionID string) ([]string, error) {
	return func(ctx context.Context, excludePermissionID string) ([]string, error) {
		if deviceID == "" || deviceSessionID == "" {
			slog.Warn("[agent] makePermissionDrainer: empty context, skipping",
				"sessionID", sessionID, "deviceID", deviceID, "deviceSessionID", deviceSessionID,
				"excludePermissionID", excludePermissionID)
			return nil, fmt.Errorf("deviceID or sessionID empty; cannot drain siblings")
		}
		ecs, err := r.msgMgr.LoadAllPendingEvents(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("load pending events: %w", err)
		}
		slog.Info("[agent] makePermissionDrainer: drain start",
			"sessionID", sessionID, "deviceID", deviceID, "deviceSessionID", deviceSessionID,
			"excludePermissionID", excludePermissionID, "pendingCount", len(ecs))
		var drainedIDs []string
		for _, ec := range ecs {
			// Only drain permission events — question events have separate
			// semantics (per-question answers) and shouldn't be auto-replied.
			if ec.EventType != "permission" && ec.EventType != "permission_batch" {
				continue
			}
			// Scope to the same device session. Pending events from other
			// devices/sessions aren't siblings of the just-replied one.
			if ec.SessionID != deviceSessionID {
				slog.Info("[agent] makePermissionDrainer: skipping event with different deviceSessionID",
					"sessionID", sessionID,
					"deviceSessionID", deviceSessionID, "eventDeviceSessionID", ec.SessionID,
					"eventType", ec.EventType, "permissionIDs", PermissionIDsFromEvent(ec))
				continue
			}
			for _, pid := range PermissionIDsFromEvent(ec) {
				if pid == "" || pid == excludePermissionID {
					continue
				}
				// Already drained via an earlier sibling in this same loop?
				// Skip — avoids double-reply when a batch event has duplicate
				// IDs or when two pending events overlap.
				if slices.Contains(drainedIDs, pid) {
					continue
				}
				if replyErr := r.rt.DeviceProxy.ReplyPermission(ctx, deviceID, pid, true, ec.Path); replyErr != nil {
					slog.Warn("[agent] makePermissionDrainer: device reply failed",
						"sessionID", sessionID, "permissionID", pid, "deviceID", deviceID, "error", replyErr)
					continue
				}
				if markErr := r.msgMgr.MarkEventResolvedByID(ctx, sessionID, pid, ResolvedReasonAutoAcceptDrain); markErr != nil {
					slog.Warn("[agent] makePermissionDrainer: mark drained permission resolved failed",
						"sessionID", sessionID, "permissionID", pid, "error", markErr)
				} else {
					slog.Info("[agent] makePermissionDrainer: drained sibling permission",
						"sessionID", sessionID, "permissionID", pid, "deviceID", deviceID)
				}
				drainedIDs = append(drainedIDs, pid)
			}
		}
		slog.Info("[agent] makePermissionDrainer: drain complete",
			"sessionID", sessionID, "deviceID", deviceID, "drainedCount", len(drainedIDs), "drainedIDs", drainedIDs)
		return drainedIDs, nil
	}
}

// addToolResult adds a tool call + result to the session history in DB.
func (r *AgentRunner) addToolResult(ctx context.Context, sessionID string, tc ToolCall, result string) {
	msgs := []ChatMessage{
		{
			Role:      "assistant",
			Content:   "",
			ToolCalls: []ToolCall{tc},
		},
		{
			Role:       "tool",
			ToolCallID: tc.ID,
			Content:    result,
		},
	}
	if err := r.msgMgr.AppendMessages(ctx, sessionID, msgs); err != nil {
		slog.Error("[agent] addToolResult: failed to save", "sessionID", sessionID, "error", err)
	}
}

// GetSessionByBaseKey retrieves the latest session for a given user and base key pattern.
func (r *AgentRunner) GetSessionByBaseKey(userID, baseKeyPrefix string) *ConversationSession {
	r.mu.Lock()
	defer r.mu.Unlock()

	var latest *ConversationSession
	for _, sess := range r.sessions {
		if sess.UserID == userID && strings.HasPrefix(sess.SessionID, baseKeyPrefix) {
			if latest == nil || sess.UpdatedAt.After(latest.UpdatedAt) {
				s := *sess
				latest = &s
			}
		}
	}
	return latest
}
func NewSessionID(baseKey string, version int) string {
	return fmt.Sprintf("%s:v%d", baseKey, version)
}

func uuidString() string {
	return uuid.New().String()
}

func resetTypeOf(baseKey string) string {
	parts := strings.Split(baseKey, ":")
	if len(parts) >= 4 {
		last := parts[len(parts)-1]
		if last == "group" {
			return "group"
		}
		if strings.Contains(baseKey, ":thread:") {
			return "thread"
		}
	}
	return "direct"
}

// contentPreview returns the first maxChars of content with all newlines and
// tabs replaced by spaces, used for slog so logs stay on one line. Returns ""
// for empty input. Used by LLM-choice logging to surface what the model said
// when diagnosing "AI claimed it acted but no tool was invoked" cases.
func contentPreview(content string, maxChars int) string {
	if content == "" {
		return ""
	}
	flat := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(content)
	// Collapse runs of whitespace so previews stay readable.
	flat = strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(flat, " "))
	if len(flat) > maxChars {
		return flat[:maxChars] + "…"
	}
	return flat
}
