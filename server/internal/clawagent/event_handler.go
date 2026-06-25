package clawagent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/costrict/costrict-web/server/internal/channel"
)

// AIEventRequest represents an event forwarded from the dispatcher.
type AIEventRequest struct {
	UserID     string
	EventType  string // "permission" or "question"
	SessionID  string
	DeviceID   string
	ActionData map[string]any
	Path       string

	// Sender is an optional channel sender for streaming the AI response back to the user.
	// When set, the response is sent through this sender and the conversation session
	// is shared with the user's chat session for reply continuity.
	Sender channel.Sender
}

// EventHandler handles AI-driven event processing for notifications.
type EventHandler struct {
	runtime *ClawAgentRuntime
}

// NewEventHandler creates a new EventHandler.
func NewEventHandler(runtime *ClawAgentRuntime) *EventHandler {
	return &EventHandler{runtime: runtime}
}

// SetOnEventProcessed registers a callback invoked when a pending event is processed
// via tool execution (e.g., permission approved, question answered).
// This is propagated to the AgentRunner, which triggers it during markEventProcessed.
func (h *EventHandler) SetOnEventProcessed(f func(sessionID string)) {
	h.runtime.runner.OnEventProcessed = f
}

// HandleAIEvent processes an event through the AI runtime.
// When req.Sender is set, the AI response is generated immediately and saved to the session,
// but the notification to the user is deferred by a configurable delay (AI_NOTIFICATION_DELAY_SECONDS,
// default 30s). This gives the user processing time and allows event batching.
// If the user replies before the timer fires, the deferred notification is cancelled.
func (h *EventHandler) HandleAIEvent(ctx context.Context, req AIEventRequest) error {
	slog.Info("[event_handler] HandleAIEvent enter", "eventType", req.EventType, "sessionID", req.SessionID, "deviceID", req.DeviceID, "hasSender", req.Sender != nil)

	eventDesc := h.describeEvent(req)

	message := h.buildPrompt(req, eventDesc)

	// Enrich message with device-side context (session info + recent messages)
	enriched := h.enrichContext(ctx, req)
	if enriched != "" {
		message = enriched + "\n\n" + message
	}
	slog.Info("[event_handler] enrichContext result", "sessionID", req.SessionID, "deviceID", req.DeviceID, "path", req.Path, "enriched_len", len(enriched), "enriched_preview", truncateForLog(enriched, 200))

	// Determine session key: use platform userID for single chat to avoid
	// ExternalChatID mismatch between send (corp userID) and receive (openID).
	var baseKey string
	var resetType string
	if req.Sender != nil {
		rc := req.Sender.ReplyContext()
		chatType := rc.Target.ExternalChatType
		if chatType == "" {
			chatType = "single"
		}
		baseKey = fmt.Sprintf("agent:clawagent:%s:%s", chatType, req.UserID)
		resetType = "direct"
	} else {
		baseKey = fmt.Sprintf("agent:clawagent:event:%s:%s", req.EventType, req.SessionID)
		resetType = "event"
	}

	sessionID, err := h.runtime.resolveActiveSession(req.UserID, baseKey, resetType)
	if err != nil {
		return fmt.Errorf("resolve event session: %w", err)
	}

	// Store EventContext in session for tool execution when user replies
	if needsEventProcessing(req.EventType) {
		ec := buildEventContext(req)

		// Store in DB for horizontal scaling
		if err := h.runtime.SessionMeta.SetEventData(ctx, sessionID, ec); err != nil {
			slog.Error("[event_handler] failed to store event data in DB", "sessionID", sessionID, "error", err)
		} else {
			slog.Info("[event_handler] stored event data in DB", "sessionID", sessionID, "eventType", req.EventType)
		}

		// Also set in-memory for same-instance fast path
		h.runtime.runner.SetEventData(sessionID, ec)
		sess := h.runtime.runner.GetSession(sessionID)
		if sess != nil {
			slog.Info("[event_handler] post SetEventData", "sessionID", sessionID, "hasEventData", sess.EventData != nil, "eventType", req.EventType, "deviceSessionID", req.SessionID)
		} else {
			slog.Info("[event_handler] post SetEventData: session not found", "sessionID", sessionID)
		}
	}

	eventCh, err := h.runtime.runner.Run(ctx, req.UserID, sessionID, message)
	if err != nil {
		return fmt.Errorf("AI event run failed: %w", err)
	}

	// Stream the AI response through the sender immediately (no deferred delay).
	// The deferral happens BEFORE this point (in dispatcher.startDeferredAI timer).
	// EventContext is already stored in SessionMeta.EventData (DB) and in-memory,
	// available to tool calls when the user replies — no need to inject XML into
	// the message content (that would leak raw metadata to the user via the channel).
	if req.Sender != nil {
		slog.Info("[event_handler] launching streamResponse", "sessionID", sessionID, "senderType", fmt.Sprintf("%T", req.Sender))
		go h.runtime.streamResponse(ctx, eventCh, req.Sender, req.UserID, message, sessionID)
	} else {
		go func() {
			for evt := range eventCh {
				if evt.IsFinal {
					break
				}
			}
		}()
	}

	return nil
}

// enrichContext builds a concise task identity for the AI to refer to the task
// by name: which device, which session (title), what the user originally asked.
// Recent messages are included briefly so the AI understands where the task is now.
// Returns empty string on failure (non-fatal — AI still gets the basic event description).
func (h *EventHandler) enrichContext(ctx context.Context, req AIEventRequest) string {
	dp := h.runtime.DeviceProxy
	if dp == nil || req.DeviceID == "" || req.SessionID == "" {
		slog.Warn("[event_handler] enrichContext: missing prerequisites, skipping", "hasDeviceProxy", dp != nil, "deviceID", req.DeviceID, "sessionID", req.SessionID)
		return ""
	}

	slog.Info("[event_handler] enrichContext: querying device", "deviceID", req.DeviceID, "sessionID", req.SessionID, "path", req.Path)

	// Device display name — so the AI can say "dev-laptop 上那个任务" not just "设备"
	deviceName, _ := dp.GetDeviceDisplayName(ctx, req.DeviceID)
	if deviceName == "" {
		slog.Debug("[event_handler] enrichContext: device name not found", "deviceID", req.DeviceID)
	}

	// Session title
	var sessionTitle string
	if info, err := dp.GetSessionInfo(ctx, req.DeviceID, req.SessionID, req.Path); err != nil {
		slog.Warn("[event_handler] enrichContext: GetSessionInfo failed", "sessionID", req.SessionID, "deviceID", req.DeviceID, "path", req.Path, "error", err)
	} else {
		slog.Info("[event_handler] enrichContext: GetSessionInfo success", "sessionID", req.SessionID, "keys", formatMapKeys(info))
		sessionTitle, _ = info["title"].(string)
	}

	// Recent messages — first user message is the task intent; rest is recent progress
	var taskIntent, recentTail string
	if msgs, err := dp.GetRecentMessages(ctx, req.DeviceID, req.SessionID, req.Path, 8); err != nil {
		slog.Warn("[event_handler] enrichContext: GetRecentMessages failed", "sessionID", req.SessionID, "deviceID", req.DeviceID, "path", req.Path, "error", err)
	} else {
		taskIntent = extractTaskIntent(msgs)
		recentTail = summarizeRecentTail(msgs)
	}

	var identity string
	if deviceName != "" || sessionTitle != "" {
		where := "一个会话"
		if deviceName != "" && sessionTitle != "" {
			where = fmt.Sprintf("「%s」上的「%s」会话", deviceName, sessionTitle)
		} else if deviceName != "" {
			where = fmt.Sprintf("「%s」上的会话", deviceName)
		} else {
			where = fmt.Sprintf("「%s」会话", sessionTitle)
		}
		identity = fmt.Sprintf("申请来源：%s", where)
	}
	if taskIntent != "" {
		if identity != "" {
			identity += "。"
		}
		identity += fmt.Sprintf("用户最初让任务做的是：%s", taskIntent)
	}
	if recentTail != "" {
		if identity != "" {
			identity += "。"
		}
		identity += "最近进展：" + recentTail
	}

	// Real IDs for tool calls. The AI must use these exact IDs when calling
	// reply_permission / reply_question — never invent its own. Marked clearly
	// so the AI doesn't echo them to the user.
	if ids := extractPendingIDs(req); ids != "" {
		if identity != "" {
			identity += "。"
		}
		identity += ids
	}

	slog.Info("[event_handler] enrichContext: done", "sessionID", req.SessionID, "result_len", len(identity), "deviceName", deviceName, "sessionTitle", sessionTitle)
	return identity
}

// extractTaskIntent finds the first user-authored message in the conversation —
// that's the original instruction the task was spawned to carry out.
func extractTaskIntent(msgs []map[string]any) string {
	for _, msg := range msgs {
		role, _ := msg["role"].(string)
		if role != "user" {
			continue
		}
		content := messageText(msg)
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		if len([]rune(content)) > 120 {
			content = string([]rune(content)[:120]) + "..."
		}
		return content
	}
	return ""
}

// summarizeRecentTail condenses the last few messages (excluding the very latest
// user instruction, which the task intent already covers) into one short line.
func summarizeRecentTail(msgs []map[string]any) string {
	if len(msgs) < 2 {
		return ""
	}
	// Last 3 messages, in order, abbreviated.
	start := len(msgs) - 3
	if start < 0 {
		start = 0
	}
	tail := msgs[start:]
	var pieces []string
	for _, msg := range tail {
		role, _ := msg["role"].(string)
		content := messageText(msg)
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		if len([]rune(content)) > 60 {
			content = string([]rune(content)[:60]) + "..."
		}
		label := "用户"
		if role == "assistant" {
			label = "任务"
		} else if role == "tool" {
			label = "工具结果"
		} else if role != "user" && role != "" {
			label = role
		}
		pieces = append(pieces, fmt.Sprintf("%s：%s", label, content))
	}
	return strings.Join(pieces, " → ")
}

// messageText extracts the text content from a device message map.
func messageText(msg map[string]any) string {
	if content, ok := msg["content"].(string); ok && content != "" {
		return content
	}
	if text, ok := msg["text"].(string); ok {
		return text
	}
	return ""
}

// extractPendingIDs pulls the real permission / question IDs from the event's
// action data and wraps them in a clear instruction: these IDs are for tool
// calls only, never echo them to the user. This is the single source of truth
// the AI should use when calling reply_permission / reply_question.
func extractPendingIDs(req AIEventRequest) string {
	switch req.EventType {
	case "permission":
		if id, ok := req.ActionData["id"].(string); ok && id != "" {
			return fmt.Sprintf("待回复的权限 ID（调用 reply_permission 时用这个，别自己编，也别告诉用户）：%s", id)
		}
	case "permission_batch":
		perms, _ := req.ActionData["permissions"].([]any)
		var ids []string
		for _, p := range perms {
			if pMap, ok := p.(map[string]any); ok {
				if id, ok := pMap["id"].(string); ok && id != "" {
					ids = append(ids, id)
				}
			}
		}
		if len(ids) > 0 {
			return fmt.Sprintf("待回复的权限 ID（调用 reply_permission 时逐个用这些真实 ID，别自己编，也别告诉用户）：%s", strings.Join(ids, ", "))
		}
	case "question":
		// Question ID is at top level (data["id"]) from csc, not per-question.
		// All questions in a single question.asked event share the same request ID.
		if id, ok := req.ActionData["id"].(string); ok && id != "" {
			return fmt.Sprintf("待回复的问题 ID（调用 reply_question 时用这个，别自己编，也别告诉用户）：%s", id)
		}
		// Fallback: try per-question IDs (used when questions are fetched from API)
		questions, _ := req.ActionData["questions"].([]any)
		var ids []string
		for _, q := range questions {
			if qMap, ok := q.(map[string]any); ok {
				if id, ok := qMap["id"].(string); ok && id != "" {
					ids = append(ids, id)
				}
			}
		}
		if len(ids) > 0 {
			return fmt.Sprintf("待回复的问题 ID（调用 reply_question 时用这些真实 ID，别自己编，也别告诉用户）：%s", strings.Join(ids, ", "))
		}
	}
	return ""
}

// buildPrompt generates the AI prompt tailored to the event type.
func (h *EventHandler) buildPrompt(req AIEventRequest, eventDesc string) string {
	if req.EventType == "question" {
		return fmt.Sprintf(`设备上的任务向你提了个问题，需要你帮忙回答：%s

你是用户的秘书，负责帮用户处理设备端任务提出的问题。请根据问题内容和选项：
- 如果有选项，分析各选项含义，结合上下文给用户推荐一个合适的选项，然后问用户确认
- 如果是开放性问题，把问题转述给用户，让用户给出回答
- 如果上面给了来源信息（设备名、会话名、任务意图），说话时用这些来指代任务，比如「dev-laptop 上 cs-cloud 重构那个任务在问…」
- 记住你是转述和辅助，不是你自己在执行任务`, eventDesc)
	}

	return fmt.Sprintf(`设备上跑的任务发起了个申请：%s
你是用户的秘书，转告用户是哪个任务要做什么、为什么，然后问ta批不批。如果上面给了来源信息（设备名、会话名、任务意图），说话时用这些来指代任务，比如「dev-laptop 上 cs-cloud 重构那个任务要…」。记住你是转述，不是你自己在执行。`, eventDesc)
}

// describeEvent generates a natural language description of the event.
func (h *EventHandler) describeEvent(req AIEventRequest) string {
	switch req.EventType {
	case "permission":
		return h.describePermission(req)
	case "permission_batch":
		return h.describePermissionBatch(req)
	case "question":
		return h.describeQuestion(req)
	default:
		return fmt.Sprintf("未知事件类型: %s", req.EventType)
	}
}

func (h *EventHandler) describePermission(req AIEventRequest) string {
	return describeSinglePermission(req.ActionData)
}

func (h *EventHandler) describePermissionBatch(req AIEventRequest) string {
	perms, _ := req.ActionData["permissions"].([]any)
	if len(perms) == 0 {
		return "任务发起了个权限申请，具体做什么没说清"
	}
	var lines []string
	for _, p := range perms {
		if pMap, ok := p.(map[string]any); ok {
			lines = append(lines, describeSinglePermission(pMap))
		}
	}
	summary := strings.Join(lines, "；")
	return fmt.Sprintf("任务一次性要干 %d 件事：%s", len(perms), summary)
}

// describeSinglePermission turns a permission actionData map into one natural
// spoken sentence (no key=value, no bullets) so the AI mirrors a human tone.
// The subject is always the running task, never "you" or "I" — the AI is a
// secretary relaying what the task wants to do.
func describeSinglePermission(data map[string]any) string {
	permType, _ := data["permission"].(string)
	desc := extractDescription(data)
	cmd := extractInputField(data, "command")
	filePath := extractInputField(data, "filePath")
	path := extractInputField(data, "path")

	target := ""
	switch permType {
	case "bash":
		if cmd != "" {
			target = "跑命令 " + cmd
		}
	case "edit", "write":
		if filePath != "" {
			target = "改文件 " + filePath
		} else if path != "" {
			target = "改文件 " + path
		}
	case "read":
		if filePath != "" {
			target = "读文件 " + filePath
		} else if path != "" {
			target = "读路径 " + path
		}
	case "webfetch":
		target = "访问网络"
	default:
		if filePath != "" {
			target = "动文件 " + filePath
		} else if path != "" {
			target = "动路径 " + path
		} else if cmd != "" {
			target = "跑 " + cmd
		}
	}

	if target == "" {
		if desc != "" {
			return "任务要做什么不大清楚，描述是：" + desc
		}
		return "任务发起了个权限申请，具体做什么没说清"
	}
	if desc != "" {
		return "任务要" + target + "（" + desc + "）"
	}
	return "任务要" + target
}

func extractDescription(data map[string]any) string {
	if metadata, ok := data["metadata"].(map[string]any); ok {
		if input, ok := metadata["input"].(map[string]any); ok {
			if desc, ok := input["description"].(string); ok {
				return desc
			}
		}
	}
	return ""
}

func extractInputField(data map[string]any, field string) string {
	if metadata, ok := data["metadata"].(map[string]any); ok {
		if input, ok := metadata["input"].(map[string]any); ok {
			if val, ok := input[field].(string); ok {
				return val
			}
		}
	}
	return ""
}

func (h *EventHandler) describeQuestion(req AIEventRequest) string {
	if questions, ok := req.ActionData["questions"].([]any); ok && len(questions) > 0 {
		parts := make([]string, 0, len(questions))
		for _, qRaw := range questions {
			if qMap, ok := qRaw.(map[string]any); ok {
				qText, _ := qMap["question"].(string)
				header, _ := qMap["header"].(string)
				var opts []string
				if options, ok := qMap["options"].([]any); ok {
					for _, oRaw := range options {
						if oMap, ok := oRaw.(map[string]any); ok {
							label, _ := oMap["label"].(string)
							desc, _ := oMap["description"].(string)
							if label != "" && desc != "" {
								opts = append(opts, label+"："+desc)
							} else if label != "" {
								opts = append(opts, label)
							} else if desc != "" {
								opts = append(opts, desc)
							}
						}
					}
				}
				one := qText
				if header != "" && one != "" {
					one = header + "：" + one
				} else if header != "" {
					one = header
				}
				if len(opts) > 0 {
					one += "（可选：" + strings.Join(opts, " / ") + "）"
				}
				if one != "" {
					parts = append(parts, one)
				}
			}
		}
		if len(parts) > 0 {
			return "设备在问：" + strings.Join(parts, "；")
		}
	}
	if question, _ := req.ActionData["question"].(string); question != "" {
		return "设备在问：" + question
	}
	return "设备有个问题想问你"
}

// EventForwarder forwards events from the dispatcher to the AI runtime.
type EventForwarder struct {
	eventHandler *EventHandler
}

// NewEventForwarder creates a new EventForwarder.
func NewEventForwarder(eventHandler *EventHandler) *EventForwarder {
	return &EventForwarder{eventHandler: eventHandler}
}

// ShouldUseAIInteraction checks whether AI interaction should be used for this event.
func (f *EventForwarder) ShouldUseAIInteraction(userID string, eventType string) bool {
	return eventType == "permission" || eventType == "permission_batch" || eventType == "question"
}

// needsEventProcessing checks if the event type requires tool-based processing.
func needsEventProcessing(eventType string) bool {
	return eventType == "permission" || eventType == "permission_batch" || eventType == "question"
}

// buildEventContext extracts structured event context from an AIEventRequest.
func buildEventContext(req AIEventRequest) *EventContext {
	ec := &EventContext{
		EventType:  req.EventType,
		SessionID:  req.SessionID,
		DeviceID:   req.DeviceID,
		Path:       req.Path,
		ActionData: req.ActionData,
	}

	switch req.EventType {
	case "permission_batch":
		// Extract first permission ID for tool context; the full list is in ActionData
		if perms, ok := req.ActionData["permissions"].([]any); ok && len(perms) > 0 {
			if firstPerm, ok := perms[0].(map[string]any); ok {
				if id, ok := firstPerm["id"].(string); ok {
					ec.PermissionID = id
				}
			}
		}

	case "permission":
		if id, ok := req.ActionData["id"].(string); ok {
			ec.PermissionID = id
		}

	case "question":
		// Question ID is at top level (data["id"]) from csc SSE events,
		// shared across all questions in the same event.
		topID := strVal(req.ActionData, "id")
		if questionsRaw, ok := req.ActionData["questions"].([]any); ok {
			for _, qRaw := range questionsRaw {
				if qMap, ok := qRaw.(map[string]any); ok {
					qi := QuestionItem{
						Question: strVal(qMap, "question"),
						Header:   strVal(qMap, "header"),
					}
					if m, ok := qMap["multiple"].(bool); ok {
						qi.Multiple = m
					}
					if optsRaw, ok := qMap["options"].([]any); ok {
						for _, oRaw := range optsRaw {
							if oMap, ok := oRaw.(map[string]any); ok {
								qi.Options = append(qi.Options, QuestionOption{
									Label:       strVal(oMap, "label"),
									Description: strVal(oMap, "description"),
								})
							}
						}
					}
					if qi.ID == "" {
						qi.ID = topID
					}
					if qi.ID == "" {
						qi.ID = strVal(qMap, "id")
					}
					ec.Questions = append(ec.Questions, qi)
				}
			}
		}
	}

	return ec
}

// strVal safely extracts a string value from a map.
func strVal(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func formatMapKeys(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return strings.Join(keys, ",")
}

func truncateForLog(s string, max int) string {
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "..."
}

// EventForwarder forwards events from the dispatcher to the AI runtime.

func (f *EventForwarder) ForwardToAI(ctx context.Context, input struct {
	UserID    string
	EventType string
	SessionID string
	DeviceID  string
	Path      string
	Data      map[string]any
}) error {
	req := AIEventRequest{
		UserID:     input.UserID,
		EventType:  input.EventType,
		SessionID:  input.SessionID,
		DeviceID:   input.DeviceID,
		ActionData: input.Data,
		Path:       input.Path,
	}
	return f.eventHandler.HandleAIEvent(ctx, req)
}
