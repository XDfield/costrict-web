package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
		role, _ := msg["role"].(string)
		if role == "" {
			role = "unknown"
		}

		content, _ := msg["content"].(string)
		if content == "" {
			if text, ok := msg["text"].(string); ok {
				content = text
			}
		}

		if len([]rune(content)) > 200 {
			content = string([]rune(content)[:200]) + "..."
		}

		result += fmt.Sprintf("[%d] %s: %s\n", i+1, role, content)
	}

	return result
}
