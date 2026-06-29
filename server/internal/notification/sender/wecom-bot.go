package sender

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

type WeComBotConfig struct {
	Enabled bool   `json:"enabled"`
	AuthToken string `json:"auth_token"`
}

type WeComBotSender struct {
	proxyURL  string
	authToken string
	client    *http.Client
}

func NewWeComBotSender(proxyURL, authToken string) *WeComBotSender {
	if proxyURL == "" {
		proxyURL = "http://localhost:9090" // 默认地址，wecom-bot-proxy 服务端口
	}

	if authToken == "" {
		authToken = "default-token" // 默认 token，需要与 wecom-bot-proxy 配置匹配
	}

	return &WeComBotSender{
		proxyURL:  proxyURL,
		authToken: authToken,
		client:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (s *WeComBotSender) Type() string { return "wecom-bot" }

func (s *WeComBotSender) ValidateUserConfig(userConfig json.RawMessage) error {
	var cfg WeComBotConfig
	if err := json.Unmarshal(userConfig, &cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	return nil
}

func (s *WeComBotSender) UserConfigSchema() []ConfigField {
	return []ConfigField{
		{
			Key:         "enabled",
			Label:       "启用企微机器人",
			Type:        "boolean",
			Required:    false,
			DefaultValue: true,
			HelpText:    "启用后将通过企微机器人长连接发送通知消息",
		},
	}
}

func (s *WeComBotSender) Send(userConfig json.RawMessage, msg NotificationMessage) error {
	slog.Info("[wecom-bot:sender] Send called",
		"userID", msg.UserID,
		"eventType", msg.EventType,
		"sessionID", msg.SessionID,
		"title", msg.Title,
	)

	var cfg WeComBotConfig
	if err := json.Unmarshal(userConfig, &cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	if !cfg.Enabled {
		slog.Info("[wecom-bot:sender] user disabled, skipping", "userID", msg.UserID)
		return nil // 用户已禁用，不发送
	}

	// Construct message to wecom-bot-proxy. session_ref is optional: when the
	// notification carries a usable session URL (set by NotificationService from
	// workspace + session IDs), pass {title, url} and let the proxy decide
	// whether to render it as a markdown link based on its session_link_mode.
	proxyMsg := map[string]interface{}{
		"user_id":   msg.UserID,
		"chat_type": "individual",
		"msg_type":  "text",
		"content":   fmt.Sprintf("%s\n\n%s", msg.Title, msg.Body),
		"task_id":   fmt.Sprintf("notify_%s_%d", msg.SessionID, time.Now().Unix()),
	}
	// [disabled] session_ref attachment — commented out. To re-enable, uncomment
	// the block below.
	/*
	if sessionURL, _ := msg.Metadata["sessionUrl"].(string); sessionURL != "" {
		proxyMsg["session_ref"] = map[string]string{
			"title": msg.Title,
			"url":   sessionURL,
		}
	}
	*/

	body, err := json.Marshal(proxyMsg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	// Send to wecom-bot-proxy
	url := fmt.Sprintf("%s/api/bot/send", s.proxyURL)
	slog.Info("[wecom-bot:sender] posting to proxy",
		"url", url,
		"proxyUserID", msg.UserID,
		"contentLen", len(msg.Title)+len(msg.Body),
	)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", s.authToken)

	resp, err := s.client.Do(req)
	if err != nil {
		slog.Error("[wecom-bot:sender] proxy request failed", "error", err)
		return fmt.Errorf("send to proxy: %w", err)
	}
	defer resp.Body.Close()

	slog.Info("[wecom-bot:sender] proxy response", "statusCode", resp.StatusCode)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("proxy returned %d", resp.StatusCode)
	}

	return nil
}