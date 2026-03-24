package sender

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type WeComConfig struct {
	WebhookURL string `json:"webhookUrl"`
}

type WeComSender struct {
	client *http.Client
}

func NewWeComSender() *WeComSender {
	return &WeComSender{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (s *WeComSender) Type() string { return "wecom" }

func (s *WeComSender) ValidateUserConfig(userConfig json.RawMessage) error {
	var cfg WeComConfig
	if err := json.Unmarshal(userConfig, &cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	if cfg.WebhookURL == "" {
		return fmt.Errorf("webhookUrl is required")
	}
	return nil
}

func (s *WeComSender) UserConfigSchema() []ConfigField {
	return []ConfigField{
		{
			Key:         "webhookUrl",
			Label:       "企微群机器人 Webhook URL",
			Type:        "url",
			Required:    true,
			Placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx",
			HelpText:    "在企微群中添加机器人后获取的 Webhook 地址",
		},
	}
}

func (s *WeComSender) Send(userConfig json.RawMessage, msg NotificationMessage) error {
	var cfg WeComConfig
	if err := json.Unmarshal(userConfig, &cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	sessionURL, _ := msg.Metadata["sessionUrl"].(string)
	content := fmt.Sprintf("## %s %s\n%s", eventIcon(msg.EventType), msg.Title, msg.Body)
	if sessionURL != "" {
		content += fmt.Sprintf("\n**详情**: [点击访问](%s)", sessionURL)
	}

	payload := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]any{
			"content": content,
		},
	}

	body, _ := json.Marshal(payload)
	resp, err := s.client.Post(cfg.WebhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("wecom returned %d", resp.StatusCode)
	}
	return nil
}

func eventIcon(eventType string) string {
	switch eventType {
	case "session.completed":
		return "✅"
	case "session.failed":
		return "❌"
	case "session.aborted":
		return "⚠️"
	case "device.offline":
		return "📴"
	case "permission":
		return "🔐"
	case "question":
		return "❓"
	case "idle":
		return "⏸️"
	default:
		return "🔔"
	}
}
