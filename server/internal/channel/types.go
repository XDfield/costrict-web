package channel

import (
	"context"
	"encoding/json"
)

type ChannelCapabilities struct {
	InboundMessages  bool     `json:"inboundMessages"`
	OutboundMessages bool     `json:"outboundMessages"`
	DirectChat       bool     `json:"directChat"`
	GroupChat        bool     `json:"groupChat"`
	Markdown         bool     `json:"markdown"`
	Media            bool     `json:"media"`
	MentionRequired  bool     `json:"mentionRequired"`
	ContentTypes     []string `json:"contentTypes"`
}

type InboundMessage struct {
	ExternalChatID    string         `json:"externalChatId"`
	ExternalChatType  string         `json:"externalChatType"`
	ExternalUserID    string         `json:"externalUserId"`
	ExternalMessageID string         `json:"externalMessageId"`
	Content           string         `json:"content"`
	ContentType       string         `json:"contentType"`
	Metadata          map[string]any `json:"metadata,omitempty"`
}

type OutboundMessage struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type ReplyTarget struct {
	ExternalChatID string `json:"externalChatId"`
	ExternalUserID string `json:"externalUserId,omitempty"`
	ContextToken   string `json:"contextToken,omitempty"`
}

type ReplyContext struct {
	ChannelConfigID string
	ChannelType     string
	UserID          string
	Target          ReplyTarget
}

type Sender interface {
	Send(ctx context.Context, content string) error
	SendMessage(ctx context.Context, msg OutboundMessage) error
	ReplyContext() ReplyContext
}

type ChannelEvent struct {
	EventType string         `json:"eventType"`
	ChatID    string         `json:"chatId"`
	UserID    string         `json:"userId"`
	Data      map[string]any `json:"data,omitempty"`
}

type ConfigField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Placeholder string `json:"placeholder,omitempty"`
	HelpText    string `json:"helpText,omitempty"`
}

type InboundMessageHandler = func(ctx context.Context, msg *InboundMessage, sender Sender) error

type RawMessage = json.RawMessage
