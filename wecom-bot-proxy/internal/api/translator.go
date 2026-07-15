package api

import (
	"encoding/json"
	"fmt"

	"github.com/go-sphere/wecom-aibot-go-sdk/aibot"
)

// --- Business request types (costrict-web → proxy) ---

// SessionRef carries an optional reference to a CoStrict session. The proxy
// decides (based on bot.session_link_mode) whether to render it as a markdown
// link appended to the content. Server provides title + url verbatim.
type SessionRef struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type SendRequest struct {
	UserID     string       `json:"user_id"`
	ChatType   string       `json:"chat_type"`
	MsgType    string       `json:"msg_type"`
	Content    string       `json:"content"`
	TaskID     string       `json:"task_id,omitempty"`
	SessionRef *SessionRef  `json:"session_ref,omitempty"`
}

type ReplyRequest struct {
	ReqID   string `json:"req_id"`
	MsgType string `json:"msg_type"`
	Content string `json:"content"`
}

type WelcomeRequest struct {
	ReqID   string `json:"req_id"`
	MsgType string `json:"msg_type"`
	Content string `json:"content"`
}

// --- Inbound translation (SDK WsFrame → standardized inbound) ---

type InboundMsg struct {
	ExternalChatID    string         `json:"externalChatId"`
	ExternalChatType  string         `json:"externalChatType"`
	ExternalUserID    string         `json:"externalUserId"`
	ExternalMessageID string         `json:"externalMessageId"`
	ContentType       string         `json:"contentType"`
	Content           string         `json:"content"`
	Metadata          map[string]any `json:"metadata,omitempty"`
}

// TranslateMsgCallback translates a message callback WsFrame to an InboundMsg.
func TranslateMsgCallback(frame *aibot.WsFrame) (*InboundMsg, error) {
	var body struct {
		MsgID    string          `json:"msgid"`
		BotID    string          `json:"aibotid"`
		ChatID   string          `json:"chatid,omitempty"`
		ChatType string          `json:"chattype"`
		From     aibot.MessageFrom `json:"from"`
		MsgType string          `json:"msgtype"`
		Text    *aibot.TextContent `json:"text,omitempty"`
	}
	if err := json.Unmarshal(frame.Body, &body); err != nil {
		return nil, fmt.Errorf("unmarshal msg callback: %w", err)
	}

	chatID := body.ChatID
	chatType := body.ChatType
	if chatType == "single" {
		chatID = body.From.UserID
	}

	return &InboundMsg{
		ExternalChatID:    chatID,
		ExternalChatType:  chatType,
		ExternalUserID:    body.From.UserID,
		ExternalMessageID: body.MsgID,
		ContentType:       body.MsgType,
		Content:           extractTextContent(body.MsgType, body.Text),
		Metadata: map[string]any{
			"reqId":    frame.Headers.ReqID,
			"botId":    body.BotID,
			"chatId":   body.ChatID,
			"chatType": body.ChatType,
			"msgType":  body.MsgType,
		},
	}, nil
}

// TranslateEventCallback translates an event callback WsFrame to an InboundMsg.
func TranslateEventCallback(frame *aibot.WsFrame) (*InboundMsg, error) {
	var body struct {
		MsgID      string          `json:"msgid"`
		CreateTime int64           `json:"create_time"`
		BotID      string          `json:"aibotid"`
		ChatID     string          `json:"chatid,omitempty"`
		ChatType   string          `json:"chattype,omitempty"`
		From       aibot.MessageFrom `json:"from,omitempty"`
		MsgType    string          `json:"msgtype"`
		Event      json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(frame.Body, &body); err != nil {
		return nil, fmt.Errorf("unmarshal event callback: %w", err)
	}

	// Parse event type from raw event
	var eventMeta struct {
		EventType string `json:"eventtype"`
	}
	if err := json.Unmarshal(body.Event, &eventMeta); err != nil {
		return nil, fmt.Errorf("parse event type: %w", err)
	}

	chatID := body.ChatID
	chatType := body.ChatType
	if chatType == "single" || chatType == "" {
		chatID = body.From.UserID
		if chatType == "" {
			chatType = "single"
		}
	}

	contentType := "event"
	content := eventMeta.EventType
	metadata := map[string]any{
		"reqId":      frame.Headers.ReqID,
		"botId":      body.BotID,
		"eventType":  eventMeta.EventType,
		"chatId":     body.ChatID,
		"chatType":   body.ChatType,
		"timestamp":  body.CreateTime,
		"event":      body.Event,
	}

	switch eventMeta.EventType {
	case "disconnected_event", "enter_chat":
		return nil, nil
	}

	return &InboundMsg{
		ExternalChatID:    chatID,
		ExternalChatType:  chatType,
		ExternalUserID:    body.From.UserID,
		ExternalMessageID: body.MsgID,
		ContentType:       contentType,
		Content:           content,
		Metadata:          metadata,
	}, nil
}

func extractTextContent(msgType string, text *aibot.TextContent) string {
	if text == nil {
		return ""
	}
	switch msgType {
	case "text":
		return text.Content
	case "image":
		return ""
	case "file":
		return ""
	case "voice":
		return ""
	default:
		return text.Content
	}
}
