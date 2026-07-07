package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/config"
)

// UserIDMapper converts encrypted WeCom open_userid to plaintext userid,
// with an in-memory cache to avoid repeated API calls.
type UserIDMapper struct {
	corpID      string
	agentSecret string
	http        *http.Client

	mu        sync.RWMutex
	token     string
	tokenExp  time.Time

	cache sync.Map // open_userid → plaintext userid
}

// NewUserIDMapper creates a mapper. Returns nil if corp_id is not configured
// (conversion is disabled, encrypted userIDs pass through as-is).
func NewUserIDMapper(cfg config.WecomAPIConfig) *UserIDMapper {
	if cfg.CorpID == "" || cfg.AgentSecret == "" {
		return nil
	}
	return &UserIDMapper{
		corpID:      cfg.CorpID,
		agentSecret: cfg.AgentSecret,
		http:        &http.Client{Timeout: 10 * time.Second},
	}
}

// Resolve converts an encrypted open_userid to plaintext userid.
// Results are cached in memory. Returns:
//   - (userID, nil) when mapper is disabled (m == nil) or input is empty —
//     caller should pass the value through unchanged.
//   - (userID, nil) on successful conversion (also cached).
//   - (original openUserID, err) when the mapper is configured but conversion
//     failed (token fetch / API error / not in corp). Caller MUST handle this
//     — silently forwarding the encrypted open_userid leaks an unresolved
//     identity downstream and creates spurious sessions.
func (m *UserIDMapper) Resolve(openUserID string) (string, error) {
	if m == nil || openUserID == "" {
		return openUserID, nil
	}

	// Check cache
	if v, ok := m.cache.Load(openUserID); ok {
		return v.(string), nil
	}

	token, err := m.getAccessToken()
	if err != nil {
		slog.Warn("open_userid resolution failed: get access token",
			"openUserID", openUserID, "error", err)
		return openUserID, fmt.Errorf("get access token: %w", err)
	}

	userID, err := m.convertUserID(token, openUserID)
	if err != nil {
		slog.Warn("open_userid resolution failed: convert userid",
			"openUserID", openUserID, "error", err)
		return openUserID, fmt.Errorf("convert userid: %w", err)
	}

	m.cache.Store(openUserID, userID)
	return userID, nil
}

func (m *UserIDMapper) getAccessToken() (string, error) {
	m.mu.RLock()
	if m.token != "" && time.Now().Before(m.tokenExp) {
		token := m.token
		m.mu.RUnlock()
		return token, nil
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if m.token != "" && time.Now().Before(m.tokenExp) {
		return m.token, nil
	}

	url := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/gettoken?corpid=%s&corpsecret=%s",
		m.corpID, m.agentSecret)
	resp, err := m.http.Get(url)
	if err != nil {
		return "", fmt.Errorf("get access_token: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if result.ErrCode != 0 {
		return "", fmt.Errorf("wecom api error: %d %s", result.ErrCode, result.ErrMsg)
	}

	m.token = result.AccessToken
	m.tokenExp = time.Now().Add(time.Duration(result.ExpiresIn-200) * time.Second)
	return m.token, nil
}

func (m *UserIDMapper) convertUserID(token, openUserID string) (string, error) {
	reqBody := fmt.Sprintf(`{"open_userid_list":["%s"]}`, openUserID)
	url := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/batch/openuserid_to_userid?access_token=%s", token)

	resp, err := m.http.Post(url, "application/json", strings.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("convert userid: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	var result struct {
		ErrCode            int `json:"errcode"`
		ErrMsg             string `json:"errmsg"`
		UserIDList         []struct {
			OpenUserID string `json:"open_userid"`
			UserID     string `json:"userid"`
		} `json:"userid_list"`
		InvalidOpenUserIDList []string `json:"invalid_open_userid_list"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode convert response: %w", err)
	}
	if result.ErrCode != 0 {
		return "", fmt.Errorf("wecom api error: %d %s", result.ErrCode, result.ErrMsg)
	}
	if len(result.UserIDList) > 0 {
		return result.UserIDList[0].UserID, nil
	}

	return "", fmt.Errorf("open_userid not converted (possibly already plaintext or invalid)")
}
