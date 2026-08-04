package sender

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/costrict/costrict-web/server/internal/safetch"
)

type WeComConfig struct {
	WebhookURL string `json:"webhookUrl"`
}

type WeComSender struct {
	client *http.Client
}

// NewWeComSender returns a sender whose underlying *http.Client is hardened
// against SSRF at every stage — DialContext re-resolves and re-checks the IP
// at dial time (defeating DNS rebinding / TOCTOU) and CheckRedirect re-runs
// ValidateURL on each redirect hop. The destination is further constrained
// to https-only via validateWeComURL, enforced at both write time
// (ValidateUserConfig) and send time (Send). Send-time re-validation is
// defense-in-depth for the case where a stored webhook URL is reused later
// against a target whose DNS now resolves internally.
func NewWeComSender() *WeComSender {
	return &WeComSender{
		client: safetch.NewClient(safetch.Options{}),
	}
}

func (s *WeComSender) Type() string { return "wecom" }

// validateWeComURL enforces that raw is a public https URL. safetch.ValidateURL
// handles scheme http/https allow-list, host-literal blocking, and public-IP
// DNS resolution; the additional https-only check here prevents plaintext-link
// SSRF and on-wire tampering, which is appropriate for WeCom's official
// webhook endpoint that only serves TLS.
func validateWeComURL(raw string) error {
	if err := safetch.ValidateURL(raw); err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if strings.ToLower(u.Scheme) != "https" {
		return fmt.Errorf("webhookUrl scheme must be https")
	}
	return nil
}

func (s *WeComSender) ValidateUserConfig(userConfig json.RawMessage) error {
	var cfg WeComConfig
	if err := json.Unmarshal(userConfig, &cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	if cfg.WebhookURL == "" {
		return fmt.Errorf("webhookUrl is required")
	}
	if err := validateWeComURL(cfg.WebhookURL); err != nil {
		return fmt.Errorf("webhookUrl: %w", err)
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
			HelpText:    "在企微群中添加机器人后获取的 Webhook 地址（必须为 https）",
		},
	}
}

func (s *WeComSender) Send(userConfig json.RawMessage, msg NotificationMessage) error {
	var cfg WeComConfig
	if err := json.Unmarshal(userConfig, &cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	// Re-validate at send time. A webhook URL persisted earlier may later
	// resolve to an internal address (DNS rotation or rebinding); the
	// DialContext in safetch.NewClient also guards the connect, but an early
	// rejection here yields a cleaner error and an additional layer.
	if err := validateWeComURL(cfg.WebhookURL); err != nil {
		return fmt.Errorf("webhookUrl: %w", err)
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
