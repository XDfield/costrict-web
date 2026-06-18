package clawagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

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
	rt        *ClawAgentRuntime
	llmClient *LLMClient
	mu        sync.Mutex
	sessions  map[string]*ConversationSession
}

// ConversationSession holds the in-memory state of a conversation.
type ConversationSession struct {
	SessionID string
	UserID    string
	Messages  []ChatMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewAgentRunner creates a new AgentRunner.
func NewAgentRunner(rt *ClawAgentRuntime, llmClient *LLMClient) *AgentRunner {
	return &AgentRunner{
		rt:        rt,
		llmClient: llmClient,
		sessions:  make(map[string]*ConversationSession),
	}
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

		sess := r.getOrCreateSession(sessionID, userID)

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

		// Build messages array with system prompt + history + new message
		messages := r.buildMessages(sess, systemPrompt, message)

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
					// Save assistant message to session
					r.addAssistantMessage(sessionID, fullResponse)
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

func (r *AgentRunner) getOrCreateSession(sessionID, userID string) *ConversationSession {
	r.mu.Lock()
	defer r.mu.Unlock()

	if sess, ok := r.sessions[sessionID]; ok {
		sess.UpdatedAt = time.Now()
		return sess
	}

	sess := &ConversationSession{
		SessionID: sessionID,
		UserID:    userID,
		Messages: []ChatMessage{
			{Role: "system", Content: "You are a helpful AI assistant."},
		},
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

func (r *AgentRunner) buildMessages(sess *ConversationSession, systemPrompt, userMessage string) []ChatMessage {
	// Start with system prompt
	messages := []ChatMessage{
		{Role: "system", Content: systemPrompt},
	}

	// Add conversation history (skip first system message)
	r.mu.Lock()
	for i, msg := range sess.Messages {
		if i == 0 {
			continue // skip default system message
		}
		messages = append(messages, msg)
	}
	// Add new user message
	sess.Messages = append(sess.Messages, ChatMessage{Role: "user", Content: userMessage})
	r.mu.Unlock()

	messages = append(messages, ChatMessage{Role: "user", Content: userMessage})
	return messages
}

func (r *AgentRunner) addAssistantMessage(sessionID, content string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if sess, ok := r.sessions[sessionID]; ok {
		sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: content})
		sess.UpdatedAt = time.Now()
	}
}

// SummarizeSession compresses session data using LLM.
func (r *AgentRunner) SummarizeSession(ctx context.Context, sessionID string, keepRecent int) (string, error) {
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

	r.mu.Lock()
	messages := sess.Messages
	r.mu.Unlock()

	// Prepare messages for summarization
	var promptBuilder string
	promptBuilder = "请将以下对话历史压缩为一段简洁的摘要，保留关键信息和决策。\n\n"

	totalMsgs := len(messages)
	keepStart := totalMsgs - keepRecent*2
	if keepStart < 0 {
		keepStart = 0
	}

	for i, msg := range messages {
		if msg.Role == "system" {
			continue
		}
		if i >= keepStart {
			promptBuilder += fmt.Sprintf("[%s]: %s\n", msg.Role, msg.Content)
		}
	}

	promptBuilder += "\n请输出JSON格式：{\"summary\": \"摘要内容\", \"key_points\": [\"要点1\", \"要点2\"]}"

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

// GetSession returns a copy of the conversation session.
func (r *AgentRunner) GetSession(sessionID string) *ConversationSession {
	r.mu.Lock()
	defer r.mu.Unlock()

	sess, ok := r.sessions[sessionID]
	if !ok {
		return nil
	}

	// Return a copy
	msgs := make([]ChatMessage, len(sess.Messages))
	copy(msgs, sess.Messages)
	return &ConversationSession{
		SessionID: sess.SessionID,
		UserID:    sess.UserID,
		Messages:  msgs,
		CreatedAt: sess.CreatedAt,
		UpdatedAt: sess.UpdatedAt,
	}
}

// NewSessionID generates a new session ID with version suffix.
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
