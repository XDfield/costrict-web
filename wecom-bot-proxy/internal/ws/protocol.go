package ws

import (
	"encoding/json"
	"fmt"
)

// WS commands (client → server)
const (
	CmdSubscribe       = "aibot_subscribe"
	CmdPing            = "ping"
	CmdRespondMsg      = "aibot_respond_msg"
	CmdRespondWelcome  = "aibot_respond_welcome_msg"
	CmdRespondUpdate   = "aibot_respond_update_msg"
	CmdSendMsg         = "aibot_send_msg"
	CmdUploadInit      = "aibot_upload_media_init"
	CmdUploadChunk     = "aibot_upload_media_chunk"
	CmdUploadFinish    = "aibot_upload_media_finish"
)

// WS commands (server → client)
const (
	CmdMsgCallback    = "aibot_msg_callback"
	CmdEventCallback  = "aibot_event_callback"
)

// Event types within aibot_event_callback
const (
	EventTypeEnterChat     = "enter_chat"
	EventTypeTemplateCard  = "template_card_event"
	EventTypeFeedback      = "feedback_event"
	EventTypeDisconnected  = "disconnected_event"
)

// WSFrame is the universal envelope for all WS messages.
type WSFrame struct {
	Cmd     string          `json:"cmd"`
	Headers WSHeaders       `json:"headers,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`

	// Response fields (present in server responses)
	ErrCode int    `json:"errcode,omitempty"`
	ErrMsg  string `json:"errmsg,omitempty"`
}

type WSHeaders struct {
	ReqID string `json:"req_id,omitempty"`
}

// SubscribeRequest is the aibot_subscribe body.
type SubscribeRequest struct {
	BotID  string `json:"bot_id"`
	Secret string `json:"secret"`
}

// PingRequest is the ping body (empty, but headers required).
type PingRequest struct{}

// --- Inbound (server → client) ---

// MsgCallbackBody is the body of aibot_msg_callback.
type MsgCallbackBody struct {
	MsgID    string       `json:"msgid"`
	BotID    string       `json:"aibotid"`
	ChatID   string       `json:"chatid,omitempty"`
	ChatType string       `json:"chattype"`
	From     MsgFrom      `json:"from"`
	MsgType  string       `json:"msgtype"`
	Text     *TextContent `json:"text,omitempty"`
}

type MsgFrom struct {
	UserID string `json:"userid"`
}

type TextContent struct {
	Content string `json:"content"`
}

// EventCallbackBody is the body of aibot_event_callback.
type EventCallbackBody struct {
	MsgID      string       `json:"msgid"`
	CreateTime int64        `json:"create_time"`
	BotID      string       `json:"aibotid"`
	ChatID     string       `json:"chatid,omitempty"`
	ChatType   string       `json:"chattype,omitempty"`
	From       MsgFrom      `json:"from,omitempty"`
	MsgType    string       `json:"msgtype"`
	Event      EventDetail  `json:"event"`
}

type EventDetail struct {
	EventType    string              `json:"eventtype"`
	TaskID       string              `json:"task_id,omitempty"`
	ResponseCode string              `json:"response_code,omitempty"`
	Feedback     *EventFeedback      `json:"feedback,omitempty"`
	SelectedItems []SelectedCardItem `json:"selected_items,omitempty"`
}

type EventFeedback struct {
	ID string `json:"id"`
}

type SelectedCardItem struct {
	QuestionKey string   `json:"question_key"`
	OptionIDs   []string `json:"option_ids"`
}

// --- Outbound (client → server) ---

// SendMsgBody is the body of aibot_send_msg.
type SendMsgBody struct {
	ChatID       string          `json:"chatid"`
	ChatType     uint32          `json:"chat_type,omitempty"`
	MsgType      string          `json:"msgtype"`
	Markdown     *MarkdownBody   `json:"markdown,omitempty"`
	Text         *TextBody       `json:"text,omitempty"`
	TemplateCard json.RawMessage `json:"template_card,omitempty"`
}

type MarkdownBody struct {
	Content  string        `json:"content"`
	Feedback *FeedbackBody `json:"feedback,omitempty"`
}

type TextBody struct {
	Content string `json:"content"`
}

type FeedbackBody struct {
	ID string `json:"id"`
}

// RespondMsgBody is the body of aibot_respond_msg.
type RespondMsgBody struct {
	MsgType      string          `json:"msgtype"`
	Text         *TextBody       `json:"text,omitempty"`
	Markdown     *MarkdownBody   `json:"markdown,omitempty"`
	TemplateCard json.RawMessage `json:"template_card,omitempty"`
	Stream       *StreamBody     `json:"stream,omitempty"`
}

type StreamBody struct {
	ID      string        `json:"id"`
	Finish  bool          `json:"finish"`
	Content string        `json:"content"`
	Feedback *FeedbackBody `json:"feedback,omitempty"`
}

// RespondWelcomeBody is the body of aibot_respond_welcome_msg.
type RespondWelcomeBody struct {
	MsgType  string        `json:"msgtype"`
	Text     *TextBody     `json:"text,omitempty"`
	Markdown *MarkdownBody `json:"markdown,omitempty"`
}

// RespondUpdateBody is the body of aibot_respond_update_msg.
type RespondUpdateBody struct {
	ResponseType  string          `json:"response_type"`
	TemplateCard  json.RawMessage `json:"template_card"`
}

// ChatType constants for aibot_send_msg.
const (
	ChatTypeUnspecified uint32 = 0
	ChatTypeSingle      uint32 = 1
	ChatTypeGroup       uint32 = 2
)

// ParseFrame parses a raw WS message into a WSFrame.
func ParseFrame(data []byte) (*WSFrame, error) {
	var frame WSFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return nil, fmt.Errorf("parse ws frame: %w", err)
	}
	return &frame, nil
}

// NewCommand creates a WSFrame for a client command.
func NewCommand(cmd string, reqID string, body any) (*WSFrame, error) {
	var bodyJSON json.RawMessage
	if body != nil {
		var err error
		bodyJSON, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	return &WSFrame{
		Cmd: cmd,
		Headers: WSHeaders{
			ReqID: reqID,
		},
		Body: bodyJSON,
	}, nil
}
