package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var deviceAuthClient = &http.Client{Timeout: 5 * time.Second}

// VerifyDeviceToken validates a device token by calling the server's internal API.
// Returns the device's owning userID on success, or an error if verification fails.
func VerifyDeviceToken(serverURL, internalSecret, deviceID, token string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"deviceID": deviceID,
		"token":    token,
	})

	url := serverURL + "/internal/gateway/device/verify-token"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if internalSecret != "" {
		req.Header.Set(internalSecretHeader, internalSecret)
	}

	resp, err := deviceAuthClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("verify-token request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("verify-token returned status %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response body failed: %w", err)
	}

	var result struct {
		Valid  bool   `json:"valid"`
		UserID string `json:"userID"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("decode response failed: %w", err)
	}

	if !result.Valid {
		return "", fmt.Errorf("invalid device token")
	}

	return result.UserID, nil
}

// ExtractDeviceToken extracts the device token from the request.
// It checks the "token" query parameter first (for WebSocket connections),
// then falls back to the Authorization: Bearer header.
func ExtractDeviceToken(r *http.Request) string {
	// WebSocket clients cannot set custom headers, so check query param first
	if token := r.URL.Query().Get("token"); token != "" {
		return token
	}

	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}

	return ""
}
