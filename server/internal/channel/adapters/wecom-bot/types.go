package wecombot

import "encoding/json"

// --- Proxy API request types ---

// proxySessionRef is an optional reference to a CoStrict session. The proxy
// decides (based on its bot.session_link_mode config) whether to render it as
// a clickable markdown link appended to the content. Server provides the title
// (already event-appropriate) and the absolute URL verbatim — no markdown here.
type proxySessionRef struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type proxySendRequest struct {
	UserID     string             `json:"user_id"`
	ChatType   string             `json:"chat_type"`
	MsgType    string             `json:"msg_type"`
	Content    string             `json:"content"`
	TaskID     string             `json:"task_id,omitempty"`
	SessionRef *proxySessionRef   `json:"session_ref,omitempty"`
}

type proxyReplyRequest struct {
	ReqID   string `json:"req_id"`
	MsgType string `json:"msg_type"`
	Content string `json:"content"`
}

type proxyResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// --- Inbound message (from proxy, aligns with channel.InboundMessage) ---

type proxyInboundMessage struct {
	ExternalChatID    string         `json:"externalChatId"`
	ExternalChatType  string         `json:"externalChatType"`
	ExternalUserID    string         `json:"externalUserId"`
	ExternalMessageID string         `json:"externalMessageId"`
	ContentType       string         `json:"contentType"`
	Content           string         `json:"content"`
	Metadata          map[string]any `json:"metadata,omitempty"`
}

// ParseInboundRaw deserializes a proxy inbound JSON body.
func ParseInboundRaw(data json.RawMessage) (*proxyInboundMessage, error) {
	var msg proxyInboundMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}
