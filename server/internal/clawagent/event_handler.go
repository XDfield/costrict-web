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

// SetOnEventProcessed registers a callback invoked when a pending event is
// resolved via tool execution (e.g., permission approved, question answered).
// The callback receives the agent's userID (NOT a session ID) — the
// dispatcher keys pending-state backlogs per user so a single reply drains
// every notification for that user.
func (h *EventHandler) SetOnEventProcessed(f func(userID string)) {
	h.runtime.runner.OnEventProcessed = f
}

// HandleAIEventBatch processes a batch of events through the AI runtime as a
// single notification. Used by the dispatcher after debounce coalesces
// multiple events for the same user into one fire. Writes one EVENT_PENDING
// row per event (preserving each device session's context), then has the AI
// RELAY the batch to the user in natural language.
//
// The AI on this path gets ONLY query tools (query_session_info,
// query_recent_messages) — enough to fetch context for an accurate
// description, but no way to approve/reject/answer. The actual tool-driven
// decision happens later, on the user→AI path triggered by the user's reply
// (runtime.Handle → RunEventReply, which arms reply tools and injects the
// real IDs into the prompt).
//
// Splitting the two paths lets each side focus on what it's good at:
//   - Notification → AI: rich natural-language description, no decision pressure
//   - User → AI: reply-tool calls driven by explicit user intent
//
// The extra system message is NOT persisted — it lives only in this LLM
// request so subsequent turns don't see stale event prompts.
func (h *EventHandler) HandleAIEventBatch(ctx context.Context, userID string, inputs []DispatchInput, sender channel.Sender) error {
	if len(inputs) == 0 {
		return nil
	}
	slog.Info("[event_handler] HandleAIEventBatch enter",
		"userID", userID, "count", len(inputs), "hasSender", sender != nil)

	chatType := "single"
	if sender != nil {
		rc := sender.ReplyContext()
		if rc.Target.ExternalChatType != "" {
			chatType = rc.Target.ExternalChatType
		}
	}

	// Resolve a single agent session for this user. All events share it —
	// they're all going to the same user via the same channel.
	baseKey := fmt.Sprintf("agent:clawagent:%s:%s", chatType, userID)
	sessionID, err := h.runtime.resolveActiveSession(userID, baseKey, "direct")
	if err != nil {
		return fmt.Errorf("resolve event session: %w", err)
	}

	// Reconcile stale pending state against device reality before appending
	// new events. Earlier AI runs that terminated without resolving (relay,
	// error, etc.) leave EVENT_PENDING rows behind; this marks rows resolved
	// whose corresponding permission/question is no longer pending on the
	// device, so the AI's relay reflects only what's actually still open.
	h.runtime.ReconcilePendingEventsWithDevice(ctx, userID, sessionID)

	// Write EVENT_PENDING row for each event. Each carries its own device
	// session ID so the right row gets transitioned to EVENT_RESOLVED when
	// the user resolves a specific permission/question via tool call (on the
	// user→AI path).
	var ecHead *EventContext
	for _, input := range inputs {
		if !needsEventProcessing(input.EventType) {
			continue
		}
		ec := buildEventContext(AIEventRequest{
			UserID:     userID,
			EventType:  input.EventType,
			SessionID:  input.SessionID,
			DeviceID:   input.DeviceID,
			Path:       input.Path,
			ActionData: input.ActionData,
		})
		if err := h.runtime.MsgMgr.AppendEventPending(ctx, sessionID, ec); err != nil {
			slog.Error("[event_handler] AppendEventPending failed",
				"sessionID", sessionID, "deviceSessionID", input.SessionID, "error", err)
			continue
		}
		if ecHead == nil {
			ecHead = ec
		}
	}

	// Compose the extra system message describing the batch (relay-only).
	// No tool IDs are exposed on this path — the AI is told to translate the
	// batch to natural language and wait for the user's reply.
	extraSystem := h.buildBatchExtraSystem(ctx, userID, inputs)

	// User-side placeholder: short, storable, makes history read naturally
	// in future turns ("AI notified me about N pending tasks"). The full
	// prompt is in the extra system message, not here.
	userMessage := fmt.Sprintf("（系统通知：收到 %d 项待处理任务申请。）", len(inputs))

	// Persist the user-side placeholder so it appears in history. RunWithSystem
	// doesn't append the user message itself; it relies on history already
	// containing it.
	h.runtime.runner.AddUserMessage(ctx, sessionID, userMessage)

	// RunEventNotifyRelay gives the AI query-only tools (query_session_info,
	// query_recent_messages) so it can fetch context to describe the batch
	// accurately, but no reply tools — it can only relay. When the user
	// replies, runtime.Handle probes the EVENT_PENDING row and routes
	// through RunEventReply (full tool set including replies).
	eventCh := h.runtime.runner.RunEventNotifyRelay(ctx, userID, sessionID, extraSystem, ecHead)

	if sender != nil {
		slog.Info("[event_handler] launching streamResponse", "sessionID", sessionID, "senderType", fmt.Sprintf("%T", sender))
		go h.runtime.streamResponse(ctx, eventCh, sender, userID, userMessage, sessionID, ecHead)
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

// DispatchInput is the dispatcher's payload type. Re-declared here so this
// package doesn't depend on the dispatcher package (which would create an
// import cycle: dispatcher → clawagent → dispatcher).
type DispatchInput struct {
	UserID      string
	WorkspaceID string
	EventType   string
	SessionID   string
	DeviceID    string
	Path        string
	SessionURL  string
	ActionData  map[string]any
}

// buildBatchExtraSystem composes the extra system message that drives the
// notification→AI relay path. The AI is told: relay the batch to the user
// in natural language with enough detail that the user can decide. The AI
// has query tools (query_session_info, query_recent_messages) for fetching
// extra context, but NO reply tools — decisions are made on the user→AI
// path when the user actually replies.
//
// The message lists each event with identity context (device, session,
// task intent) so the AI can describe each one accurately without
// hallucinating. Permission/question IDs are intentionally NOT included
// here — the AI has no reply tools to use them, and they would only
// confuse the user if echoed. IDs are injected on the user→AI path instead.
//
// Enrichment per event is best-effort (device name lookup); missing data is
// silently skipped.
func (h *EventHandler) buildBatchExtraSystem(ctx context.Context, userID string, inputs []DispatchInput) string {
	var b strings.Builder
	b.WriteString("【系统通知 — 本轮实时数据，权威来源】以下是设备上 ")
	fmt.Fprintf(&b, "%d", len(inputs))
	b.WriteString(" 项待处理申请（dispatcher 刚从设备 pending 列表实时拉取，包含命令/路径/数量等真实字段。历史 assistant 消息里的旧转述是之前 batch 的，跟本轮可能完全不同，禁止照抄——以下方实际内容为准）：\n\n")
	for i, input := range inputs {
		req := AIEventRequest{
			UserID:     userID,
			EventType:  input.EventType,
			SessionID:  input.SessionID,
			DeviceID:   input.DeviceID,
			Path:       input.Path,
			ActionData: input.ActionData,
		}
		desc := h.describeEvent(req)
		enriched := h.enrichContext(ctx, req)
		fmt.Fprintf(&b, "%d. ", i+1)
		if enriched != "" {
			b.WriteString(enriched)
			b.WriteString("。")
		}
		b.WriteString(desc)
		b.WriteString("\n")
	}

	b.WriteString("\n【你的任务】\n")
	b.WriteString("你这轮回复的唯一任务是把以上申请**完整、清楚**地转述给用户，让 ta 一眼就能看明白在等什么、需要给什么回应。\n\n")

	b.WriteString("【转述要求】\n")
	b.WriteString("- **必须说清是什么申请**——绝对不要只写「收到一个新的申请，请回复处理」这种空泛的话；要写明：哪个设备、哪个会话/任务、想做什么动作（命令/文件/访问）、动作的目的（如果上下文能看出来）\n")
	b.WriteString("- 多项申请用编号或换行逐项列出，每项独立描述，**不要**合并成「N 个申请」一句话\n")
	b.WriteString("- 问题类申请要把问题原文转述，列出选项（如果有），让用户能直接选\n")
	b.WriteString("- 高风险动作（rm 删除、覆盖、推送、外发、sudo 等）要明确标注风险，提醒用户谨慎\n")
	b.WriteString("- 如果觉得上下文还不够（不知道是什么任务、想做的动作目的不明显），**先调用 query_recent_messages 拉一下最近对话**，再转述\n")
	b.WriteString("- 可以基于上下文给用户一个**提示**（例如「这条看上去是常规的命令，可以考虑允许」），但**最终决定权在用户**——不要替 ta 拍板\n")
	b.WriteString("- **不要**自己做批准/拒绝/回答的决定——本轮没有 reply_permission / reply_question 工具，决策留到用户回复后再处理\n")
	b.WriteString("- 不要在转述里出现任何 permissionID / questionID 之类的内部标识\n")
	b.WriteString("- 转述完就结束回合，等用户回复\n")

	return b.String()
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

// EventForwarder (removed): the dispatcher now calls EventHandler.HandleAIEventBatch
// directly via the AIEventHandler callback wired in main.go.
