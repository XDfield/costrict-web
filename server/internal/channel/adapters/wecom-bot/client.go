package wecombot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// BotProxyClient calls the wecom-bot-proxy HTTP API.
type BotProxyClient struct {
	baseURL    string
	authToken  string
	httpClient *http.Client
}

func NewBotProxyClient(baseURL, authToken string) *BotProxyClient {
	return &BotProxyClient{
		baseURL:   baseURL,
		authToken: authToken,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (c *BotProxyClient) Send(ctx context.Context, userID, chatType, msgType, content, taskID string) error {
	return c.SendWithSessionRef(ctx, userID, chatType, msgType, content, taskID, nil)
}

// SendWithSessionRef is like Send but optionally attaches a session_ref the
// proxy may render as a clickable link. Callers that don't need the link
// should use Send; pass nil sessionRef to behave identically.
func (c *BotProxyClient) SendWithSessionRef(ctx context.Context, userID, chatType, msgType, content, taskID string, sessionRef *proxySessionRef) error {
	req := proxySendRequest{
		UserID:     userID,
		ChatType:   chatType,
		MsgType:    msgType,
		Content:    content,
		TaskID:     taskID,
		SessionRef: sessionRef,
	}
	return c.doPost(ctx, "/api/bot/send", req)
}

func (c *BotProxyClient) Reply(ctx context.Context, reqID, msgType, content string) error {
	req := proxyReplyRequest{
		ReqID:   reqID,
		MsgType: msgType,
		Content: content,
	}
	return c.doPost(ctx, "/api/bot/reply", req)
}

func (c *BotProxyClient) doPost(ctx context.Context, path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.authToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("proxy returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result proxyResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil // accept non-JSON success responses
	}
	if !result.Success {
		return fmt.Errorf("proxy error: %s", result.Error)
	}

	return nil
}
