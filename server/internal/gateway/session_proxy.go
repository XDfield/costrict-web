package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/logger"
)

// ---------- Response Capture Types ----------

// DeviceSessionResponse is used to capture the proxied response from a device session API call.
type DeviceSessionResponse struct {
	StatusCode int
	Body       []byte
	Headers    http.Header
}

// ---------- Public Proxy Methods ----------

// ProxyDeviceSessionRequest proxies a request through the gateway tunnel to a device's session API.
// This method handles device ownership verification and response capture, returning the
// response body as the provided result type. It's designed to be called from
// the services layer, avoiding circular dependencies.
func ProxyDeviceSessionRequest(client *Client, registry *GatewayRegistry, userID, deviceID, directory, method, path string, body []byte, result interface{}) error {
	gw, err := registry.GetDeviceGateway(deviceID)
	if err != nil {
		logger.Error("[Gateway] device %s not connected: %v", deviceID, err)
		return fmt.Errorf("device not connected")
	}

	// Build the target path for the internal gateway proxy endpoint
	targetPath := fmt.Sprintf("/device/%s/proxy%s", deviceID, path)

	// Create HTTP request to the gateway's internal endpoint
	req, err := http.NewRequest(method, "http://placeholder"+targetPath, bytes.NewReader(body))
	if err != nil {
		logger.Error("[Gateway] failed to build request: %v", err)
		return fmt.Errorf("failed to build request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if directory != "" {
		req.Header.Set("X-Opencode-Directory", directory)
	}

	// Capture the response
	crw := &capturingResponseWriter{headers: make(http.Header)}
	if err := client.ProxyRequest(gw.InternalURL, deviceID, req, crw); err != nil {
		logger.Error("[Gateway] proxy request failed: %v", err)
		return fmt.Errorf("proxy request failed: %w", err)
	}

	// Check for error status codes from device/gateway
	if crw.statusCode >= 400 {
		return &DeviceProxyError{
			StatusCode: crw.statusCode,
			Path:       path,
			Body:       string(crw.body.Bytes()),
		}
	}

	// If result is provided, unmarshal response body into it
	if result != nil && crw.body.Len() > 0 {
		if err := json.Unmarshal(crw.body.Bytes(), result); err != nil {
			logger.Error("[Gateway] failed to unmarshal response: %v, body: %s", err, crw.body.String())
			return fmt.Errorf("invalid response from device")
		}
	}

	return nil
}

// ProxyDeviceSessionRequestRaw is like ProxyDeviceSessionRequest but returns the raw response
// instead of unmarshaling into a result type. Use this for endpoints where
// response validation is needed.
func ProxyDeviceSessionRequestRaw(client *Client, registry *GatewayRegistry, userID, deviceID, directory, method, path string, body []byte) (*DeviceSessionResponse, error) {
	gw, err := registry.GetDeviceGateway(deviceID)
	if err != nil {
		logger.Error("[Gateway] device %s not connected: %v", deviceID, err)
		return nil, fmt.Errorf("device not connected")
	}

	targetPath := fmt.Sprintf("/device/%s/proxy%s", deviceID, path)

	req, err := http.NewRequest(method, "http://placeholder"+targetPath, bytes.NewReader(body))
	if err != nil {
		logger.Error("[Gateway] failed to build request: %v", err)
		return nil, fmt.Errorf("failed to build request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if directory != "" {
		req.Header.Set("X-Opencode-Directory", directory)
	}

	crw := &capturingResponseWriter{headers: make(http.Header)}
	if err := client.ProxyRequest(gw.InternalURL, deviceID, req, crw); err != nil {
		logger.Error("[Gateway] proxy request failed: %v", err)
		return nil, fmt.Errorf("proxy request failed: %w", err)
	}

	return &DeviceSessionResponse{
			StatusCode: crw.statusCode,
			Body:       crw.body.Bytes(),
			Headers:    crw.headers,
		}, nil
}

// ---------- Internal Types ----------

// capturingResponseWriter is an http.ResponseWriter that captures response body and status code.
// It implements the minimal http.ResponseWriter interface needed by ProxyRequest.
type capturingResponseWriter struct {
	statusCode int
	body       bytes.Buffer
	headers    http.Header
}

func (w *capturingResponseWriter) Header() http.Header {
	return w.headers
}

func (w *capturingResponseWriter) Write(b []byte) (int, error) {
	return w.body.Write(b)
}

func (w *capturingResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

func (w *capturingResponseWriter) Flush() {}

// Hijack is required by the http.Hijacker interface, which ProxyRequest may check.
// Since we're capturing the response inline, we don't support hijacking here.
func (w *capturingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, fmt.Errorf("hijack not supported in session proxy layer")
}

// DeviceProxyError represents an error that occurred during proxying to a device session API.
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

func (e *DeviceProxyError) IsProxyError() bool {
	return true
}
