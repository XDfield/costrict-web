package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

func Register(serverURL, gatewayID, endpoint, internalURL, region string, capacity int) error {
	body := map[string]any{
		"gatewayID":   gatewayID,
		"endpoint":    endpoint,
		"internalURL": internalURL,
		"region":      region,
		"capacity":    capacity,
	}
	data, _ := json.Marshal(body)
	resp, err := http.Post(serverURL+"/internal/gateway/register", "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("register failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("register failed with status %d", resp.StatusCode)
	}
	return nil
}

func Heartbeat(serverURL, gatewayID string, currentConns int) (int64, error) {
	body := map[string]any{"currentConns": currentConns}
	data, _ := json.Marshal(body)
	url := fmt.Sprintf("%s/internal/gateway/%s/heartbeat", serverURL, gatewayID)
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
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

func (m *TunnelManager) NotifyAllOnline(serverURL, gatewayID string) {
	m.mu.RLock()
	deviceIDs := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		deviceIDs = append(deviceIDs, id)
	}
	m.mu.RUnlock()

	for _, deviceID := range deviceIDs {
		if err := NotifyOnline(serverURL, gatewayID, deviceID); err != nil {
			log.Printf("[Gateway] re-notify online failed for device %s: %v", deviceID, err)
		}
	}
}

func NotifyOnline(serverURL, gatewayID, deviceID string) error {
	body := map[string]any{"deviceID": deviceID, "gatewayID": gatewayID}
	data, _ := json.Marshal(body)
	resp, err := http.Post(serverURL+"/internal/gateway/device/online", "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("notify online failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("notify online failed with status %d", resp.StatusCode)
	}
	return nil
}

func NotifyOffline(serverURL, gatewayID, deviceID string) error {
	body := map[string]any{"deviceID": deviceID, "gatewayID": gatewayID}
	data, _ := json.Marshal(body)
	resp, err := http.Post(serverURL+"/internal/gateway/device/offline", "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("notify offline failed: %w", err)
	}
	defer resp.Body.Close()
	return nil
}
