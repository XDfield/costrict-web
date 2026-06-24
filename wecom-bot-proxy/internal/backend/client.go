package backend

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/config"
)

type InboundMessage struct {
	ExternalChatID    string         `json:"externalChatId"`
	ExternalChatType  string         `json:"externalChatType"`
	ExternalUserID    string         `json:"externalUserId"`
	ExternalMessageID string         `json:"externalMessageId"`
	ContentType       string         `json:"contentType"`
	Content           string         `json:"content"`
	Metadata          map[string]any `json:"metadata,omitempty"`
}

type Client struct {
	cfg    config.BackendConfig
	name   string
	logger *slog.Logger
	http   *http.Client

	healthy     bool
	lastSuccess time.Time
}

func NewClient(name string, cfg config.BackendConfig, logger *slog.Logger) *Client {
	return &Client{
		name:    name,
		cfg:     cfg,
		logger:  logger.With("backend", name),
		http:    &http.Client{Timeout: cfg.Timeout},
		healthy: true,
	}
}

// Forward sends an inbound message to the backend.
func (c *Client) Forward(ctx context.Context, msg *InboundMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal inbound: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.cfg.Retry; attempt++ {
		if attempt > 0 {
			c.logger.Warn("retrying forward", "attempt", attempt, "error", lastErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}

		lastErr = c.doForward(ctx, body)
		if lastErr == nil {
			c.healthy = true
			c.lastSuccess = time.Now()
			return nil
		}
	}

	c.healthy = false
	return fmt.Errorf("forward failed after %d retries: %w", c.cfg.Retry, lastErr)
}

func (c *Client) doForward(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Bot-Proxy-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set("X-Bot-Proxy-Msg-ID", "")

	// HMAC signature
	timestamp := req.Header.Get("X-Bot-Proxy-Timestamp")
	sig := c.computeHMAC(body, timestamp)
	req.Header.Set("X-Bot-Proxy-Signature", sig)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("backend returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func (c *Client) computeHMAC(body []byte, timestamp string) string {
	if c.cfg.HMACSecret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(c.cfg.HMACSecret))
	mac.Write([]byte(timestamp))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (c *Client) Healthy() bool {
	return c.healthy
}

func (c *Client) LastSuccess() time.Time {
	return c.lastSuccess
}

func (c *Client) Name() string {
	return c.name
}

// Manager manages all backend clients.
type Manager struct {
	mu       map[string]*Client
	logger   *slog.Logger
}

func NewManager(backends config.BackendsMap, logger *slog.Logger) *Manager {
	clients := make(map[string]*Client, len(backends))
	for name, cfg := range backends {
		clients[name] = NewClient(name, cfg, logger)
	}
	return &Manager{
		mu:     clients,
		logger: logger,
	}
}

func (m *Manager) Get(name string) (*Client, bool) {
	c, ok := m.mu[name]
	return c, ok
}

func (m *Manager) All() map[string]*Client {
	return m.mu
}

func (m *Manager) Forward(ctx context.Context, backendName string, msg *InboundMessage) error {
	client, ok := m.Get(backendName)
	if !ok {
		return fmt.Errorf("backend %q not found", backendName)
	}
	return client.Forward(ctx, msg)
}
