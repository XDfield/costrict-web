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
	logger *slog.Logger
	http   *http.Client

	healthy     bool
	lastSuccess time.Time
}

func NewClient(cfg config.BackendConfig, logger *slog.Logger) *Client {
	return &Client{
		cfg:     cfg,
		logger:  logger,
		http:    &http.Client{Timeout: cfg.Timeout},
		healthy: true,
	}
}

// Forward sends an inbound message to the backend and returns the response body
// so callers can read backend-provided metadata (e.g., firstContact/welcome).
func (c *Client) Forward(ctx context.Context, msg *InboundMessage) ([]byte, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal inbound: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.cfg.Retry; attempt++ {
		if attempt > 0 {
			c.logger.Warn("retrying forward", "attempt", attempt, "error", lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
			}
		}

		respBody, lastErr := c.doForward(ctx, body)
		if lastErr == nil {
			c.healthy = true
			c.lastSuccess = time.Now()
			return respBody, nil
		}
	}

	c.healthy = false
	return nil, fmt.Errorf("forward failed after %d retries: %w", c.cfg.Retry, lastErr)
}

func (c *Client) doForward(ctx context.Context, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Bot-Proxy-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))

	// HMAC signature
	timestamp := req.Header.Get("X-Bot-Proxy-Timestamp")
	sig := c.computeHMAC(body, timestamp)
	req.Header.Set("X-Bot-Proxy-Signature", sig)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read backend response: %w", err)
	}

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("backend returned %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
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
