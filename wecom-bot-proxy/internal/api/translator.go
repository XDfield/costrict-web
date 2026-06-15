package api

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/ws"
)

// --- Business request types (costrict-web → proxy) ---

type SendRequest struct {
	UserID   string `json:"user_id"`
	ChatType string `json:"chat_type"`
	MsgType  string `json:"msg_type"`
	Content  string `json:"content"`
	TaskID   string `json:"task_id,omitempty"`
}

type ReplyRequest struct {
	ReqID   string `json:"req_id"`
	MsgType string `json:"msg_type"`
	Content string `json:"content"`
}

type StreamReplyRequest struct {
	ReqID    string `json:"req_id"`
	StreamID string `json:"stream_id"`
	Finish   bool   `json:"finish"`
	Content  string `json:"content"`
}

type WelcomeRequest struct {
	ReqID   string `json:"req_id"`
	MsgType string `json:"msg_type"`
	Content string `json:"content"`
}

type CardUpdateRequest struct {
	ReqID    string `json:"req_id"`
	CardType string `json:"card_type,omitempty"`
	Content  string `json:"content"`
	TaskID   string `json:"task_id,omitempty"`
}

// --- Inbound translation (WS frame → standardized inbound) ---

type InboundMsg struct {
	ExternalChatID    string         `json:"externalChatId"`
	ExternalChatType  string         `json:"externalChatType"`
	ExternalUserID    string         `json:"externalUserId"`
	ExternalMessageID string         `json:"externalMessageId"`
	ContentType       string         `json:"contentType"`
	Content           string         `json:"content"`
	Metadata          map[string]any `json:"metadata,omitempty"`
}

// TranslateMsgCallback translates an aibot_msg_callback WS frame to an InboundMsg.
func TranslateMsgCallback(frame *ws.WSFrame) (*InboundMsg, error) {
	var body ws.MsgCallbackBody
	if err := json.Unmarshal(frame.Body, &body); err != nil {
		return nil, fmt.Errorf("unmarshal msg callback: %w", err)
	}

	chatID := body.ChatID
	chatType := body.ChatType
	if chatType == "single" {
		chatID = body.From.UserID
	}

	content := ""
	contentType := "text"

	switch body.MsgType {
	case "text":
		if body.Text != nil {
			content = body.Text.Content
		}
		contentType = "text"
	case "image":
		contentType = "image"
	case "file":
		contentType = "file"
	case "voice":
		contentType = "voice"
	case "video":
		contentType = "video"
	case "mixed":
		contentType = "mixed"
	default:
		contentType = body.MsgType
	}

	return &InboundMsg{
		ExternalChatID:    chatID,
		ExternalChatType:  chatType,
		ExternalUserID:    body.From.UserID,
		ExternalMessageID: body.MsgID,
		ContentType:       contentType,
		Content:           content,
		Metadata: map[string]any{
			"reqId":    frame.Headers.ReqID,
			"botId":    body.BotID,
			"chatId":   body.ChatID,
			"chatType": body.ChatType,
			"msgType":  body.MsgType,
		},
	}, nil
}

// TranslateEventCallback translates an aibot_event_callback WS frame to an InboundMsg.
func TranslateEventCallback(frame *ws.WSFrame) (*InboundMsg, error) {
	var body ws.EventCallbackBody
	if err := json.Unmarshal(frame.Body, &body); err != nil {
		return nil, fmt.Errorf("unmarshal event callback: %w", err)
	}

	eventType := body.Event.EventType
	chatID := body.ChatID
	chatType := body.ChatType
	if chatType == "single" || chatType == "" {
		chatID = body.From.UserID
		if chatType == "" {
			chatType = "single"
		}
	}

	contentType := "event"
	content := eventType
	metadata := map[string]any{
		"reqId":      frame.Headers.ReqID,
		"botId":      body.BotID,
		"eventType":  eventType,
		"chatId":     body.ChatID,
		"chatType":   body.ChatType,
		"timestamp":  body.CreateTime,
	}

	switch eventType {
	case ws.EventTypeTemplateCard:
		contentType = "action_callback"
		content = body.Event.EventType
		metadata["taskId"] = body.Event.TaskID
		metadata["responseCode"] = body.Event.ResponseCode
		if body.Event.Feedback != nil {
			metadata["feedbackId"] = body.Event.Feedback.ID
		}
		if len(body.Event.SelectedItems) > 0 {
			metadata["selectedItems"] = body.Event.SelectedItems
		}
	case ws.EventTypeDisconnected:
		// Don't forward disconnect events to backends
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

// --- Outbound translation (business request → WS command frame) ---

// TranslateSend converts a business SendRequest to an aibot_send_msg WS frame.
func TranslateSend(req *SendRequest) (*ws.WSFrame, error) {
	chatType := ws.ChatTypeUnspecified
	switch req.ChatType {
	case "single":
		chatType = ws.ChatTypeSingle
	case "group":
		chatType = ws.ChatTypeGroup
	}

	body := &ws.SendMsgBody{
		ChatID:   req.UserID,
		ChatType: chatType,
		MsgType:  req.MsgType,
	}

	switch req.MsgType {
	case "text":
		body.Text = &ws.TextBody{Content: req.Content}
	case "markdown":
		body.Markdown = &ws.MarkdownBody{Content: req.Content}
	case "card":
		body.MsgType = "template_card"
		body.TemplateCard = json.RawMessage(req.Content)
	default:
		body.Text = &ws.TextBody{Content: req.Content}
	}

	return ws.NewCommand(ws.CmdSendMsg, generateReqID(), body)
}

// TranslateReply converts a business ReplyRequest to an aibot_respond_msg WS frame.
func TranslateReply(req *ReplyRequest) (*ws.WSFrame, error) {
	body := &ws.RespondMsgBody{
		MsgType: req.MsgType,
	}

	switch req.MsgType {
	case "text":
		body.Text = &ws.TextBody{Content: req.Content}
	case "markdown":
		body.Markdown = &ws.MarkdownBody{Content: req.Content}
	default:
		body.Text = &ws.TextBody{Content: req.Content}
	}

	return ws.NewCommand(ws.CmdRespondMsg, req.ReqID, body)
}

// TranslateStreamReply converts a business StreamReplyRequest to an aibot_respond_msg WS frame.
func TranslateStreamReply(req *StreamReplyRequest) (*ws.WSFrame, error) {
	body := &ws.RespondMsgBody{
		MsgType: "stream",
		Stream: &ws.StreamBody{
			ID:      req.StreamID,
			Finish:  req.Finish,
			Content: req.Content,
		},
	}

	return ws.NewCommand(ws.CmdRespondMsg, req.ReqID, body)
}

// TranslateWelcome converts a business WelcomeRequest to an aibot_respond_welcome_msg WS frame.
func TranslateWelcome(req *WelcomeRequest) (*ws.WSFrame, error) {
	body := &ws.RespondWelcomeBody{
		MsgType: req.MsgType,
	}

	switch req.MsgType {
	case "text":
		body.Text = &ws.TextBody{Content: req.Content}
	case "markdown":
		body.Markdown = &ws.MarkdownBody{Content: req.Content}
	default:
		body.Text = &ws.TextBody{Content: req.Content}
	}

	return ws.NewCommand(ws.CmdRespondWelcome, req.ReqID, body)
}

// TranslateCardUpdate converts a business CardUpdateRequest to an aibot_respond_update_msg WS frame.
func TranslateCardUpdate(req *CardUpdateRequest) (*ws.WSFrame, error) {
	body := &ws.RespondUpdateBody{
		ResponseType: "update_template_card",
		TemplateCard: json.RawMessage(req.Content),
	}

	return ws.NewCommand(ws.CmdRespondUpdate, req.ReqID, body)
}

func generateReqID() string {
	return fmt.Sprintf("proxy_%d", time.Now().UnixNano())
}
