package clawagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/costrict/costrict-web/server/internal/gateway"
	"gorm.io/gorm"
)

// DeviceProxyError represents an error from device proxy operations.
type DeviceProxyError struct {
	StatusCode int
	Path       string
	Body       string
}

func (e *DeviceProxyError) Error() string {
	msg := fmt.Sprintf("proxy to %s failed with status %d", e.Path, e.StatusCode)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

var (
	ErrDeviceOffline     = fmt.Errorf("device is offline")
	ErrDeviceTimeout     = fmt.Errorf("device proxy timeout")
	ErrWorkspaceNotFound = fmt.Errorf("workspace not found on device")
	ErrConversationLost  = fmt.Errorf("conversation not found on device")
)

// DeviceProxyClient proxies HTTP requests to device-side cs-cloud localserver.
type DeviceProxyClient struct {
	gwRegistry *gateway.GatewayRegistry
	gwClient   *gateway.Client
	db         *gorm.DB
}

// NewDeviceProxyClient creates a new DeviceProxyClient.
func NewDeviceProxyClient(gwRegistry *gateway.GatewayRegistry, gwClient *gateway.Client, db *gorm.DB) *DeviceProxyClient {
	return &DeviceProxyClient{
		gwRegistry: gwRegistry,
		gwClient:   gwClient,
		db:         db,
	}
}

// DeviceHealth represents device health information.
type DeviceHealth struct {
	Status      string `json:"status"`
	WorkspaceOK bool   `json:"workspace_ok"`
}

// VCSInfo represents git status information.
type VCSInfo struct {
	Branch    string `json:"branch"`
	Dirty     bool   `json:"dirty"`
	Commit    string `json:"commit"`
	Untracked int    `json:"untracked"`
}

// FileInfo represents a file in the workspace.
type FileInfo struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
}

// Conversation represents a device-side conversation.
type Conversation struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

// DeviceMessage represents a message in a device conversation.
type DeviceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// DeviceEvent represents a device-side event.
type DeviceEvent struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
}

// proxyToDevice sends a request through the gateway tunnel to a device.
func (c *DeviceProxyClient) proxyToDevice(ctx context.Context, deviceID, method, path string, directory string, body []byte) ([]byte, error) {

	gw, err := c.gwRegistry.GetDeviceGateway(deviceID)
	if err != nil {
		return nil, ErrDeviceOffline
	}

	req, err := http.NewRequest(method, "http://placeholder"+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if directory != "" {
		req.Header.Set("X-Opencode-Directory", directory)
	}

	crw := &capturingResponseWriter{headers: make(http.Header)}
	if err := c.gwClient.ProxyRequest(gw.InternalURL, deviceID, req, crw); err != nil {
		return nil, fmt.Errorf("proxy request: %w", err)
	}

	if crw.statusCode >= 400 {
		return nil, &DeviceProxyError{
			StatusCode: crw.statusCode,
			Path:       path,
			Body:       string(crw.body.Bytes()),
		}
	}

	return crw.body.Bytes(), nil
}

// capturingResponseWriter implements http.ResponseWriter to capture the response.
type capturingResponseWriter struct {
	statusCode int
	body       bytes.Buffer
	headers    http.Header
}

func (w *capturingResponseWriter) Header() http.Header { return w.headers }
func (w *capturingResponseWriter) Write(b []byte) (int, error) { return w.body.Write(b) }
func (w *capturingResponseWriter) WriteHeader(statusCode int) { w.statusCode = statusCode }
func (w *capturingResponseWriter) Flush() {}

// parseDeviceResponse parses the standard device response envelope.
func parseDeviceResponse[T any](data []byte) (*T, error) {
	var envelope struct {
		OK   bool            `json:"ok"`
		Data *T              `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parse envelope: %w", err)
	}
	if !envelope.OK {
		if envelope.Error != nil {
			return nil, fmt.Errorf("device error: %s", envelope.Error.Message)
		}
		return nil, fmt.Errorf("device error: %s", string(data))
	}
	return envelope.Data, nil
}

// --- Session/Conversation ---

func (c *DeviceProxyClient) CreateConversation(ctx context.Context, deviceID, workspaceDir string) (*Conversation, error) {
	body := map[string]any{}
	if workspaceDir != "" {
		body["workspace_dir"] = workspaceDir
	}
	bodyBytes, _ := json.Marshal(body)

	respBytes, err := c.proxyToDevice(ctx, deviceID, "POST", "/api/v1/conversations", workspaceDir, bodyBytes)
	if err != nil {
		return nil, err
	}
	return parseDeviceResponse[Conversation](respBytes)
}

func (c *DeviceProxyClient) SendPromptAsync(ctx context.Context, deviceID, convID, content string) error {
	body := map[string]any{"content": content}
	bodyBytes, _ := json.Marshal(body)
	_, err := c.proxyToDevice(ctx, deviceID, "POST", fmt.Sprintf("/api/v1/conversations/%s/prompt/async", convID), "", bodyBytes)
	return err
}

func (c *DeviceProxyClient) AbortPrompt(ctx context.Context, deviceID, convID string) error {
	_, err := c.proxyToDevice(ctx, deviceID, "POST", fmt.Sprintf("/api/v1/conversations/%s/abort", convID), "", nil)
	return err
}

func (c *DeviceProxyClient) GetMessages(ctx context.Context, deviceID, convID string) ([]DeviceMessage, error) {
	respBytes, err := c.proxyToDevice(ctx, deviceID, "GET", fmt.Sprintf("/api/v1/conversations/%s/messages", convID), "", nil)
	if err != nil {
		return nil, err
	}
	result, err := parseDeviceResponse[[]DeviceMessage](respBytes)
	if err != nil {
		return nil, err
	}
	return *result, nil
}

// --- Workspace/Runtime ---

func (c *DeviceProxyClient) GetHealth(ctx context.Context, deviceID string) (*DeviceHealth, error) {
	respBytes, err := c.proxyToDevice(ctx, deviceID, "GET", "/api/v1/runtime/health", "", nil)
	if err != nil {
		return nil, err
	}
	return parseDeviceResponse[DeviceHealth](respBytes)
}

func (c *DeviceProxyClient) GetVCS(ctx context.Context, deviceID, workspaceDir string) (*VCSInfo, error) {
	respBytes, err := c.proxyToDevice(ctx, deviceID, "GET", "/api/v1/runtime/vcs", workspaceDir, nil)
	if err != nil {
		return nil, err
	}
	return parseDeviceResponse[VCSInfo](respBytes)
}

func (c *DeviceProxyClient) ListFiles(ctx context.Context, deviceID, workspaceDir, subPath string) ([]FileInfo, error) {
	path := "/api/v1/runtime/files"
	if subPath != "" {
		path += "?path=" + subPath
	}
	respBytes, err := c.proxyToDevice(ctx, deviceID, "GET", path, workspaceDir, nil)
	if err != nil {
		return nil, err
	}
	result2, err := parseDeviceResponse[[]FileInfo](respBytes)
	if err != nil {
		return nil, err
	}
	return *result2, nil
}

func (c *DeviceProxyClient) ReadFile(ctx context.Context, deviceID, workspaceDir, filePath string) (string, error) {
	respBytes, err := c.proxyToDevice(ctx, deviceID, "GET", "/api/v1/runtime/files/content?path="+filePath, workspaceDir, nil)
	if err != nil {
		return "", err
	}
	result3, err := parseDeviceResponse[string](respBytes)
	if err != nil {
		return "", err
	}
	return *result3, nil
}

func (c *DeviceProxyClient) WriteFile(ctx context.Context, deviceID, workspaceDir, filePath, content string) error {
	body := map[string]any{"path": filePath, "content": content}
	bodyBytes, _ := json.Marshal(body)
	_, err := c.proxyToDevice(ctx, deviceID, "PUT", "/api/v1/runtime/files/content", workspaceDir, bodyBytes)
	return err
}

func (c *DeviceProxyClient) GetInitStatus(ctx context.Context, deviceID, workspaceDir string) (map[string]any, error) {
	respBytes, err := c.proxyToDevice(ctx, deviceID, "GET", "/api/v1/runtime/init-status", workspaceDir, nil)
	if err != nil {
		return nil, err
	}
	result4, err := parseDeviceResponse[map[string]any](respBytes)
	if err != nil {
		return nil, err
	}
	return *result4, nil
}

// --- Events ---

func (c *DeviceProxyClient) SubscribeEvents(ctx context.Context, deviceID, workspaceDir string) (<-chan *DeviceEvent, error) {
	eventCh := make(chan *DeviceEvent, 256)

	gw, err := c.gwRegistry.GetDeviceGateway(deviceID)
	if err != nil {
		return nil, ErrDeviceOffline
	}

	go func() {
		defer close(eventCh)

		targetURL := fmt.Sprintf("%s/device/%s/proxy/api/v1/events", gw.InternalURL, deviceID)
		req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
		if err != nil {
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		if workspaceDir != "" {
			req.Header.Set("X-Opencode-Directory", workspaceDir)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()

		scanner := NewSSEScanner(resp.Body)
		for scanner.Scan() {
			msg := scanner.Message()
			if msg == nil || msg.Data == "" {
				continue
			}
			var evt DeviceEvent
			if err := json.Unmarshal([]byte(msg.Data), &evt); err != nil {
				continue
			}
			select {
			case eventCh <- &evt:
			case <-ctx.Done():
				return
			}
		}
	}()

	return eventCh, nil
}

// --- Permissions ---

func (c *DeviceProxyClient) ReplyPermission(ctx context.Context, deviceID, permissionID, optionID string) error {
	body := map[string]any{"option_id": optionID}
	bodyBytes, _ := json.Marshal(body)
	_, err := c.proxyToDevice(ctx, deviceID, "POST", fmt.Sprintf("/api/v1/permissions/%s/reply", permissionID), "", bodyBytes)
	return err
}

func (c *DeviceProxyClient) ReplyQuestion(ctx context.Context, deviceID, questionID, answer string) error {
	body := map[string]any{"answer": answer}
	bodyBytes, _ := json.Marshal(body)
	_, err := c.proxyToDevice(ctx, deviceID, "POST", fmt.Sprintf("/api/v1/questions/%s/reply", questionID), "", bodyBytes)
	return err
}
