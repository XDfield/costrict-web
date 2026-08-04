package sender

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/safetch"
)

type WebhookConfig struct {
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
}

type WebhookSender struct {
	client *http.Client
}

// NewWebhookSender returns a sender whose underlying *http.Client is hardened
// against SSRF at every stage — DialContext re-resolves and re-checks the IP
// at dial time (defeating DNS rebinding / TOCTOU) and CheckRedirect re-runs
// ValidateURL on each redirect hop. Write-time validation is enforced in
// ValidateUserConfig and Send; send-time re-validation in Send is
// defense-in-depth for the case where a stored webhook URL is reused later
// against a target whose DNS now resolves internally.
func NewWebhookSender() *WebhookSender {
	return &WebhookSender{
		client: safetch.NewClient(safetch.Options{}),
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
	if err := safetch.ValidateURL(cfg.URL); err != nil {
		return fmt.Errorf("url: %w", err)
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

	// Re-validate at send time. A webhook URL persisted earlier may later
	// resolve to an internal address (DNS rotation or rebinding); the
	// DialContext in safetch.NewClient also guards the connect, but an early
	// rejection here yields a cleaner error and an additional layer.
	if err := safetch.ValidateURL(cfg.URL); err != nil {
		return fmt.Errorf("url: %w", err)
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
