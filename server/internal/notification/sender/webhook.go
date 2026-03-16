package sender

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type WebhookConfig struct {
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
}

type WebhookSender struct {
	client *http.Client
}

func NewWebhookSender() *WebhookSender {
	return &WebhookSender{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (s *WebhookSender) Type() string { return "webhook" }

func (s *WebhookSender) ValidateUserConfig(userConfig json.RawMessage) error {
	var cfg WebhookConfig
	if err := json.Unmarshal(userConfig, &cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	if cfg.URL == "" {
		return fmt.Errorf("url is required")
	}
	return nil
}

func (s *WebhookSender) UserConfigSchema() []ConfigField {
	return []ConfigField{
		{
			Key:         "url",
			Label:       "Webhook URL",
			Type:        "url",
			Required:    true,
			Placeholder: "https://your-server.com/notify",
		},
		{
			Key:      "secret",
			Label:    "签名密钥（可选）",
			Type:     "password",
			Required: false,
			HelpText: "配置后，请求头将附加 X-Notification-Signature: sha256=<HMAC-SHA256(body, secret)>",
		},
	}
}

func (s *WebhookSender) Send(userConfig json.RawMessage, msg NotificationMessage) error {
	var cfg WebhookConfig
	if err := json.Unmarshal(userConfig, &cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	body, _ := json.Marshal(msg)

	req, err := http.NewRequest("POST", cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	if cfg.Secret != "" {
		mac := hmac.New(sha256.New, []byte(cfg.Secret))
		mac.Write(body)
		sig := hex.EncodeToString(mac.Sum(nil))
		req.Header.Set("X-Notification-Signature", "sha256="+sig)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}
