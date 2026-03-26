package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/costrict/costrict-web/gateway/internal/logger"
)

const internalSecretHeader = "X-Internal-Secret"

// internalPost sends a POST request to a server internal endpoint with the shared secret.
func internalPost(url, secret string, data []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set(internalSecretHeader, secret)
	}
	return http.DefaultClient.Do(req)
}

// internalRequest sends an arbitrary HTTP request to a server internal endpoint with the shared secret.
func internalRequest(method, url, secret string) (*http.Response, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	if secret != "" {
		req.Header.Set(internalSecretHeader, secret)
	}
	return http.DefaultClient.Do(req)
}

func Register(serverURL, gatewayID, endpoint, internalURL, region, secret string, capacity int) error {
	body := map[string]any{
		"gatewayID":   gatewayID,
		"endpoint":    endpoint,
		"internalURL": internalURL,
		"region":      region,
		"capacity":    capacity,
	}
	data, _ := json.Marshal(body)
	resp, err := internalPost(serverURL+"/internal/gateway/register", secret, data)
	if err != nil {
		return fmt.Errorf("register failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("register failed with status %d", resp.StatusCode)
	}
	return nil
}

func Heartbeat(serverURL, gatewayID, secret string, currentConns int) (int64, error) {
	body := map[string]any{"currentConns": currentConns}
	data, _ := json.Marshal(body)
	url := fmt.Sprintf("%s/internal/gateway/%s/heartbeat", serverURL, gatewayID)
	resp, err := internalPost(url, secret, data)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, fmt.Errorf("gateway not registered (status 404)")
	}
	var result struct {
		ServerEpoch int64 `json:"serverEpoch"`
	}
	if raw, err := io.ReadAll(resp.Body); err == nil {
		json.Unmarshal(raw, &result)
	}
	return result.ServerEpoch, nil
}

func (m *TunnelManager) NotifyAllOnline(serverURL, gatewayID, secret string) {
	m.mu.RLock()
	deviceIDs := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		deviceIDs = append(deviceIDs, id)
	}
	m.mu.RUnlock()

	for _, deviceID := range deviceIDs {
		if err := NotifyOnline(serverURL, gatewayID, deviceID, secret); err != nil {
			logger.Error("[Gateway] re-notify online failed for device %s: %v", deviceID, err)
		}
	}
}

func NotifyOnline(serverURL, gatewayID, deviceID, secret string) error {
	body := map[string]any{"deviceID": deviceID, "gatewayID": gatewayID}
	data, _ := json.Marshal(body)
	resp, err := internalPost(serverURL+"/internal/gateway/device/online", secret, data)
	if err != nil {
		return fmt.Errorf("notify online failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("notify online failed with status %d", resp.StatusCode)
	}
	return nil
}

func NotifyOffline(serverURL, gatewayID, deviceID, secret string) error {
	body := map[string]any{"deviceID": deviceID, "gatewayID": gatewayID}
	data, _ := json.Marshal(body)
	resp, err := internalPost(serverURL+"/internal/gateway/device/offline", secret, data)
	if err != nil {
		return fmt.Errorf("notify offline failed: %w", err)
	}
	defer resp.Body.Close()
	return nil
}

func (m *TunnelManager) NotifyAllOffline(serverURL, gatewayID, secret string) {
	m.mu.RLock()
	deviceIDs := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		deviceIDs = append(deviceIDs, id)
	}
	m.mu.RUnlock()

	for _, deviceID := range deviceIDs {
		if err := NotifyOffline(serverURL, gatewayID, deviceID, secret); err != nil {
			logger.Error("[Gateway] notify offline failed for device %s: %v", deviceID, err)
		}
	}
}

func Deregister(serverURL, gatewayID, secret string) error {
	url := fmt.Sprintf("%s/internal/gateway/%s", serverURL, gatewayID)
	resp, err := internalRequest(http.MethodDelete, url, secret)
	if err != nil {
		return fmt.Errorf("deregister failed: %w", err)
	}
	defer resp.Body.Close()
	return nil
}
