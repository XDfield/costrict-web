package sender

import "encoding/json"

type NotificationMessage struct {
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	EventType string         `json:"eventType"`
	SessionID string         `json:"sessionId,omitempty"`
	DeviceID  string         `json:"deviceId,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// ChannelSender 通知渠道发送接口
type ChannelSender interface {
	Type() string
	Send(userConfig json.RawMessage, msg NotificationMessage) error
	ValidateUserConfig(userConfig json.RawMessage) error
	UserConfigSchema() []ConfigField
}

// ConfigField 用户配置字段描述（供前端渲染表单）
type ConfigField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"`     // "text" | "password" | "url"
	Required    bool   `json:"required"`
	Placeholder string `json:"placeholder,omitempty"`
	HelpText    string `json:"helpText,omitempty"`
}

var registry = map[string]ChannelSender{}

func Register(s ChannelSender) {
	registry[s.Type()] = s
}

func Get(channelType string) (ChannelSender, bool) {
	s, ok := registry[channelType]
	return s, ok
}

func All() map[string]ChannelSender {
	return registry
}
