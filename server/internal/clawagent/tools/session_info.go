package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// SessionInfoTool lets the AI query session/conversation metadata from the device.
type SessionInfoTool struct{}

func NewSessionInfoTool() *SessionInfoTool {
	return &SessionInfoTool{}
}

func (t *SessionInfoTool) Name() string {
	return "query_session_info"
}

func (t *SessionInfoTool) Definition() Definition {
	return Definition{
		Name:        "query_session_info",
		Description: "查询当前设备端会话的元信息（标题、创建时间等），用于了解权限请求所在的会话上下文",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

func (t *SessionInfoTool) Execute(ctx context.Context, argsJSON string, toolCtx *Context) (string, error) {
	slog.Debug("[tool] query_session_info: execute", "sessionID", toolCtx.SessionID, "deviceID", toolCtx.DeviceID)

	if toolCtx.DeviceID == "" || toolCtx.SessionID == "" {
		return "", fmt.Errorf("missing deviceID or sessionID")
	}

	info, err := toolCtx.DeviceProxy.GetSessionInfo(ctx, toolCtx.DeviceID, toolCtx.SessionID, toolCtx.Directory)
	if err != nil {
		slog.Error("[tool] query_session_info: failed", "sessionID", toolCtx.SessionID, "error", err)
		return "", fmt.Errorf("query session info: %w", err)
	}

	formatted := formatSessionInfo(info)
	slog.Debug("[tool] query_session_info: success", "sessionID", toolCtx.SessionID, "result", formatted)
	return formatted, nil
}

func formatSessionInfo(info map[string]any) string {
	if info == nil {
		return "无会话信息"
	}

	title, _ := info["title"].(string)
	created, _ := info["created_at"].(string)
	if created == "" {
		created, _ = info["createdAt"].(string)
	}

	result := fmt.Sprintf("会话标题: %s\n创建时间: %s", title, created)

	if msgCount, ok := info["message_count"]; ok {
		result += fmt.Sprintf("\n消息数量: %v", msgCount)
	}
	if status, ok := info["status"]; ok {
		result += fmt.Sprintf("\n会话状态: %v", status)
	}

	return result
}

// --- Recent Messages Tool ---

type RecentMessagesTool struct{}

func NewRecentMessagesTool() *RecentMessagesTool {
	return &RecentMessagesTool{}
}

func (t *RecentMessagesTool) Name() string {
	return "query_recent_messages"
}

func (t *RecentMessagesTool) Definition() Definition {
	return Definition{
		Name:        "query_recent_messages",
		Description: "查询当前设备端会话最近的对话消息，用于了解权限请求的具体上下文（如用户正在执行什么任务）",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit": map[string]any{
					"type":        "integer",
					"description": "返回最近的消息数量，默认 5 条",
					"default":     5,
				},
			},
		},
	}
}

func (t *RecentMessagesTool) Execute(ctx context.Context, argsJSON string, toolCtx *Context) (string, error) {
	slog.Debug("[tool] query_recent_messages: execute", "sessionID", toolCtx.SessionID, "deviceID", toolCtx.DeviceID)

	if toolCtx.DeviceID == "" || toolCtx.SessionID == "" {
		return "", fmt.Errorf("missing deviceID or sessionID")
	}

	var args struct {
		Limit int `json:"limit"`
	}
	if argsJSON != "" && argsJSON != "{}" {
		_ = json.Unmarshal([]byte(argsJSON), &args)
	}
	limit := args.Limit
	if limit <= 0 || limit > 20 {
		limit = 5
	}

	messages, err := toolCtx.DeviceProxy.GetRecentMessages(ctx, toolCtx.DeviceID, toolCtx.SessionID, toolCtx.Directory, limit)
	if err != nil {
		slog.Error("[tool] query_recent_messages: failed", "sessionID", toolCtx.SessionID, "error", err)
		return "", fmt.Errorf("query recent messages: %w", err)
	}

	formatted := formatRecentMessages(messages, limit)
	slog.Debug("[tool] query_recent_messages: success", "sessionID", toolCtx.SessionID, "count", len(messages))
	return formatted, nil
}

func formatRecentMessages(messages []map[string]any, limit int) string {
	if len(messages) == 0 {
		return "没有找到会话消息"
	}

	start := 0
	if len(messages) > limit {
		start = len(messages) - limit
	}
	recent := messages[start:]

	result := fmt.Sprintf("最近 %d 条消息:\n", len(recent))
	for i, msg := range recent {
		role := extractMessageRole(msg)
		content := extractMessageText(msg)

		if len([]rune(content)) > 200 {
			content = string([]rune(content)[:200]) + "..."
		}

		if role == "" || content == "" {
			slog.Debug("[tool] query_recent_messages: incomplete message",
				"index", i+1, "role", role, "contentLen", len(content),
				"keys", formatMapKeys(msg))
		}

		if role == "" {
			role = "unknown"
		}
		if content == "" {
			content = "(无文本内容)"
		}

		result += fmt.Sprintf("[%d] %s: %s\n", i+1, role, content)
	}

	return result
}

// extractMessageRole pulls the role out of a device message.
// opencode v2 messages endpoint returns items of shape {info: Message, parts: Part[]},
// where Message has top-level `role` ("user"|"assistant"). Legacy/simple shapes
// keep role at the message top-level too, so we try direct first, then info.role.
func extractMessageRole(msg map[string]any) string {
	if role, ok := msg["role"].(string); ok && role != "" {
		return role
	}
	if info, ok := msg["info"].(map[string]any); ok {
		if role, ok := info["role"].(string); ok && role != "" {
			return role
		}
	}
	// Some shapeshifted variants nest role under author/sender.
	for _, key := range []string{"author", "sender", "from"} {
		if v, ok := msg[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// extractMessageText pulls human-readable text out of a device message.
// opencode v2 message shape: {info: Message, parts: Part[]} where Part has
// discriminated `type` field. We surface text parts plus brief markers for
// other part kinds so the AI sees what the assistant was doing (tool calls,
// reasoning, file edits) even on messages with no plain text.
func extractMessageText(msg map[string]any) string {
	partsAny, hasParts := msg["parts"].([]any)
	if hasParts {
		var b strings.Builder
		for _, p := range partsAny {
			pMap, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, marker := extractPartText(pMap)
			if text == "" && marker == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString(" ")
			}
			if text != "" {
				b.WriteString(text)
			} else {
				b.WriteString(marker)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}

	// info.content (some adapters nest the simple shape under info).
	if info, ok := msg["info"].(map[string]any); ok {
		if s := topLevelString(info, "content", "text"); s != "" {
			return s
		}
	}

	// Top-level string content (simple shape).
	if s := topLevelString(msg, "content", "text"); s != "" {
		return s
	}

	// content as array of parts (some adapters).
	if contentArr, ok := msg["content"].([]any); ok {
		var b strings.Builder
		for _, c := range contentArr {
			cMap, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := cMap["text"].(string); ok && text != "" {
				if b.Len() > 0 {
					b.WriteString(" ")
				}
				b.WriteString(text)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}

	return ""
}

// extractPartText handles opencode v2 Part variants. Returns (text, marker);
// text is real content (for TextPart), marker is a placeholder for non-text
// parts (tool calls, reasoning, etc.) so the AI sees the message shape.
func extractPartText(part map[string]any) (text string, marker string) {
	pType, _ := part["type"].(string)
	switch pType {
	case "text":
		// Skip synthetic/ignored text parts — they're auto-generated metadata,
		// not real user input or assistant output.
		if syn, _ := part["synthetic"].(bool); syn {
			return "", ""
		}
		if ign, _ := part["ignored"].(bool); ign {
			return "", ""
		}
		if t, ok := part["text"].(string); ok && t != "" {
			return t, ""
		}
	case "tool":
		tool, _ := part["tool"].(string)
		state, _ := part["state"].(map[string]any)
		status, _ := state["status"].(string)
		if tool != "" {
			return "", fmt.Sprintf("[调用工具 %s, 状态:%s]", tool, status)
		}
	case "reasoning":
		return "", "[推理过程]"
	case "step-start":
		return "", "[开始步骤]"
	case "step-finish":
		return "", "[结束步骤]"
	case "file":
		path, _ := part["path"].(string)
		if path != "" {
			return "", fmt.Sprintf("[文件:%s]", path)
		}
		return "", "[文件附件]"
	case "patch":
		path, _ := part["path"].(string)
		if path != "" {
			return "", fmt.Sprintf("[编辑:%s]", path)
		}
		return "", "[代码补丁]"
	case "snapshot":
		return "", "[快照]"
	case "subtask":
		agent, _ := part["agent"].(string)
		desc, _ := part["description"].(string)
		return "", fmt.Sprintf("[子任务→%s: %s]", agent, desc)
	case "agent":
		agent, _ := part["agent"].(string)
		return "", fmt.Sprintf("[切换Agent:%s]", agent)
	case "retry":
		return "", "[重试]"
	case "compaction":
		return "", "[上下文压缩]"
	}
	return "", ""
}

func topLevelString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func formatMapKeys(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return fmt.Sprintf("%v", keys)
}
