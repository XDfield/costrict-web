package clawagent

import (
	"context"
	"fmt"
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
	llmClient    *LLMClient
	msgMgr       *MessageManager
	mu           sync.Mutex
	sessions     map[string]*ConversationSession
	toolRegistry *tools.Registry

	// OnEventProcessed is called when a pending event (permission/question) is
	// successfully processed via tool execution. The sessionID is passed as argument.
	// Used by the dispatcher to cancel deferred notifications.
	OnEventProcessed func(sessionID string)
}

// ConversationSession holds the in-memory state of a conversation (metadata only, messages are in DB).
type ConversationSession struct {
	SessionID string
	UserID    string
	CreatedAt time.Time
	UpdatedAt time.Time

	// EventData holds pending event context for tool execution (permission/question).
	// When non-nil and IsProcessed is false, Handle() uses RunEventReply instead of Run().
	EventData *EventContext
}

// EventContext holds pending event data for AI tool execution.
type EventContext struct {
	EventType  string         `json:"eventType"`    // "permission" or "question"
	SessionID  string         `json:"sessionId"`    // device-side session ID from the dispatcher
	DeviceID   string         `json:"deviceId"`
	Path       string         `json:"path"`         // workspace directory for device proxy routing
	ActionData map[string]any `json:"actionData"`   // raw action data from dispatcher

	// Resolved fields extracted from ActionData
	PermissionID string         `json:"permissionId,omitempty"` // for permission events
	Questions    []QuestionItem `json:"questions,omitempty"`    // for question events
	IsProcessed  bool           `json:"isProcessed"`            // marked true after successful tool call
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
		rt:        rt,
		llmClient: llmClient,
		sessions:  make(map[string]*ConversationSession),
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

// Run starts or continues a conversation.
// For group chats, senderUserID can be passed separately from the session owner
// to enable persona/memory isolation while sharing conversation context.
func (r *AgentRunner) Run(ctx context.Context, userID, sessionID, message string, senderUserID ...string) (<-chan AgentEvent, error) {
	eventCh := make(chan AgentEvent, 128)

	go func() {
		defer close(eventCh)
		// Use senderUserID for group persona/memory isolation
		promptUserID := userID
		if len(senderUserID) > 0 && senderUserID[0] != "" {
			promptUserID = senderUserID[0]
		}

		r.getOrCreateSession(sessionID, userID)

		// Build system prompt with persona + memory
		systemPrompt, err := r.buildSystemPrompt(ctx, promptUserID)
		if err != nil {
			eventCh <- AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to build context: %v", err), IsError: true, IsFinal: true}
			return
		}

		// Resolve provider config
		provCfg, err := r.resolveProvider(ctx, userID)
		if err != nil {
			eventCh <- AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to resolve provider: %v", err), IsError: true, IsFinal: true}
			return
		}

		// Save user message to DB
		if err := r.msgMgr.AppendMessage(ctx, sessionID, ChatMessage{Role: "user", Content: message}); err != nil {
			slog.Error("[agent] Run: failed to save user message", "sessionID", sessionID, "error", err)
		}

		// Build messages array with system prompt + history + new message
		messages, err := r.buildMessages(ctx, sessionID, systemPrompt)
		if err != nil {
			eventCh <- AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to build messages: %v", err), IsError: true, IsFinal: true}
			return
		}

		// Stream the response
		streamCh, errCh := r.llmClient.GenerateStream(ctx, *provCfg, messages)

		var fullResponse string
		for evt := range streamCh {
			for _, choice := range evt.Choices {
				if choice.Delta.Content != "" {
					fullResponse += choice.Delta.Content
					eventCh <- AgentEvent{
						Type:    "token",
						Content: choice.Delta.Content,
					}
				}
				if choice.FinishReason != "" {
					// Strip any hallucinated <tool_call> text XML before persisting —
					// Run() has no tool registry, so the model's attempt to call tools
					// here is just noise that shouldn't pollute session history.
					persisted := fullResponse
					if _, cleaned := parseTextToolCalls(persisted); cleaned != persisted {
						slog.Warn("[agent] Run: stripped text-encoded tool_call XML before persisting",
							"sessionID", sessionID, "before", len(persisted), "after", len(cleaned))
						persisted = cleaned
					}
					r.addAssistantMessage(ctx, sessionID, persisted)
					eventCh <- AgentEvent{
						Type:    "done",
						IsFinal: true,
					}
				}
			}
		}

		// Check for streaming errors
		select {
		case err := <-errCh:
			if err != nil {
				if fullResponse == "" {
					eventCh <- AgentEvent{Type: "error", Error: err.Error(), IsError: true, IsFinal: true}
				} else {
					// Partial response, still mark as done
					eventCh <- AgentEvent{Type: "done", IsFinal: true}
				}
			}
		default:
		}
	}()

	return eventCh, nil
}

// SetMsgMgr sets the MessageManager after construction (called during runtime init).
func (r *AgentRunner) SetMsgMgr(mgr *MessageManager) {
	r.msgMgr = mgr
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
	// Start with system prompt
	messages := []ChatMessage{
		{Role: "system", Content: systemPrompt},
	}

	// Load conversation history from DB
	history, err := r.msgMgr.LoadMessages(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	// Append history (all messages already saved, including the latest user message)
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
		EventData: sess.EventData,
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
func (r *AgentRunner) RunEventReply(ctx context.Context, userID, sessionID string) (<-chan AgentEvent, error) {
	eventCh := make(chan AgentEvent, 128)

	go func() {
		defer close(eventCh)

		slog.Debug("[agent] RunEventReply: starting", "sessionID", sessionID, "userID", userID)

		sess := r.getOrCreateSession(sessionID, userID)

		// Build system prompt with event context + tools
		systemPrompt, err := r.buildSystemPrompt(ctx, userID)
		if err != nil {
			eventCh <- AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to build context: %v", err), IsError: true, IsFinal: true}
			return
		}

		// Append event-specific instructions based on EventData
		if sess.EventData != nil && !sess.EventData.IsProcessed {
			eventInstructions := tools.BuildInstructions(sess.EventData.EventType)
			systemPrompt += eventInstructions
		}

		// Resolve provider config
		provCfg, err := r.resolveProvider(ctx, userID)
		if err != nil {
			eventCh <- AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to resolve provider: %v", err), IsError: true, IsFinal: true}
			return
		}

		toolDefs := r.toolDefinitions()
		maxToolIterations := 10

		for iter := 0; iter < maxToolIterations; iter++ {
			// Load messages from DB
			history, err := r.msgMgr.LoadMessages(ctx, sessionID)
			if err != nil {
				eventCh <- AgentEvent{Type: "error", Error: fmt.Sprintf("Failed to load messages: %v", err), IsError: true, IsFinal: true}
				return
			}

			// Build messages array
			messages := []ChatMessage{
				{Role: "system", Content: systemPrompt},
			}
			messages = append(messages, history...)

			slog.Debug("[agent] RunEventReply: LLM call", "sessionID", sessionID, "iter", iter)
			resp, err := r.llmClient.GenerateWithTools(ctx, *provCfg, messages, toolDefs)
			if err != nil {
				eventCh <- AgentEvent{Type: "error", Error: fmt.Sprintf("LLM call failed: %v", err), IsError: true, IsFinal: true}
				return
			}

			if len(resp.Choices) == 0 {
				eventCh <- AgentEvent{Type: "error", Error: "LLM returned no choices", IsError: true, IsFinal: true}
				return
			}

			choice := resp.Choices[0]

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

			// Save assistant message to DB
			r.addAssistantMessage(ctx, sessionID, choice.Message.Content)

			// When tool calls were recovered from text, surface the leftover
			// chat content (e.g., "好的，已经批准了...") before executing tools
			// so the user sees the acknowledgement that preceded the leaked
			// <tool_call> block.
			if textParsed && choice.Message.Content != "" {
				eventCh <- AgentEvent{Type: "token", Content: choice.Message.Content}
			}

			// Check for tool calls
			if len(choice.Message.ToolCalls) > 0 {
				for _, tc := range choice.Message.ToolCalls {
					eventCh <- AgentEvent{
						Type: "tool_call",
						Tool: tc.Function.Name,
						Args: tc.Function.Arguments,
					}

					// Execute tool via registry
					deviceID := ""
					directory := ""
					deviceSessionID := ""
					if sess.EventData != nil {
						deviceID = sess.EventData.DeviceID
						directory = sess.EventData.Path
						deviceSessionID = sess.EventData.SessionID
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
						MarkProcessed: func() { r.markEventProcessed(sessionID) },
					}
					result, execErr := r.toolRegistry.Execute(ctx, tc.Function.Name, tc.Function.Arguments, toolCtx)
					slog.Debug("[agent] RunEventReply: tool executed", "sessionID", sessionID, "tool", tc.Function.Name, "error", execErr)
					if execErr != nil {
						result = fmt.Sprintf("工具执行失败: %v", execErr)
					}

					eventCh <- AgentEvent{
						Type:   "tool_result",
						Tool:   tc.Function.Name,
						Result: result,
					}

					// Add tool call + result to DB
					r.addToolResult(ctx, sessionID, tc, result)
				}
				// Continue loop to let LLM process tool results
				continue
			}

			// No tool calls — this is the final response
			fullResponse := choice.Message.Content
			if fullResponse != "" {
				eventCh <- AgentEvent{Type: "token", Content: fullResponse}
			}
			eventCh <- AgentEvent{Type: "done", IsFinal: true}
			return
		}

		slog.Error("[agent] RunEventReply: tool call iteration limit reached", "sessionID", sessionID)
		eventCh <- AgentEvent{Type: "error", Error: "Tool call iteration limit reached", IsError: true, IsFinal: true}
	}()

	return eventCh, nil
}

// resolveDeviceIDFromSession finds the device ID from the user's active session EventData.
func (r *AgentRunner) resolveDeviceIDFromSession(userID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, sess := range r.sessions {
		if sess.UserID == userID && sess.EventData != nil && sess.EventData.DeviceID != "" {
			return sess.EventData.DeviceID
		}
	}
	return ""
}

// SetEventData sets the EventContext on a specific session.
// If the session doesn't exist in memory yet, it pre-creates it with EventData.
// This handles the case where SetEventData is called before Run() (which creates
// the in-memory session via getOrCreateSession).
func (r *AgentRunner) SetEventData(sessionID string, ec *EventContext) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sess, ok := r.sessions[sessionID]; ok {
		sess.EventData = ec
	} else {
		// Pre-create session so EventData survives until Run() fills in remaining fields
		r.sessions[sessionID] = &ConversationSession{
			SessionID: sessionID,
			EventData: ec,
		}
	}
}

// markEventProcessed sets IsProcessed on the session's EventData.
// Also calls OnEventProcessed callback if set (for deferred notification cancellation).
func (r *AgentRunner) markEventProcessed(sessionID string) {
	slog.Debug("[agent] markEventProcessed", "sessionID", sessionID)

	r.mu.Lock()
	sess, ok := r.sessions[sessionID]
	hasCallback := r.OnEventProcessed != nil
	r.mu.Unlock()

	if ok && sess.EventData != nil {
		r.mu.Lock()
		sess.EventData.IsProcessed = true
		r.mu.Unlock()

		if hasCallback {
			r.OnEventProcessed(sessionID)
		}
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
