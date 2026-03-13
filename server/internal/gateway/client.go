package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	httpClient *http.Client
}

func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

type Event struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties,omitempty"`
}

func (c *Client) SendToDevice(gatewayInternalURL, deviceID string, event Event) error {
	body := map[string]any{"event": event}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal failed: %w", err)
	}

	url := fmt.Sprintf("%s/internal/device/%s/send", gatewayInternalURL, deviceID)
	resp, err := c.httpClient.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("gateway unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("device not connected")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway returned status %d", resp.StatusCode)
	}
	return nil
}
