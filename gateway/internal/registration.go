package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
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

func StartHeartbeat(serverURL, gatewayID string, manager *ConnectionManager) {
	ticker := time.NewTicker(HeartbeatInterval * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		body := map[string]any{"currentConns": manager.Count()}
		data, _ := json.Marshal(body)
		url := fmt.Sprintf("%s/internal/gateway/%s/heartbeat", serverURL, gatewayID)
		resp, err := http.Post(url, "application/json", bytes.NewReader(data))
		if err != nil {
			log.Printf("[Gateway] heartbeat failed: %v", err)
			continue
		}
		resp.Body.Close()
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
