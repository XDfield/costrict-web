package sender

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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

func NewWeComBotSender() *WeComBotSender {
	proxyURL := os.Getenv("WECOM_BOT_PROXY_URL")
	if proxyURL == "" {
		proxyURL = "http://localhost:9090" // 默认地址，wecom-bot-proxy 服务端口
	}

	authToken := os.Getenv("WECOM_BOT_PROXY_AUTH_TOKEN")
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
	var cfg WeComBotConfig
	if err := json.Unmarshal(userConfig, &cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	if !cfg.Enabled {
		return nil // 用户已禁用，不发送
	}

	// 构造发送到 wecom-bot-proxy 的消息
	proxyMsg := map[string]interface{}{
		"user_id":    msg.UserID,
		"chat_type":  "individual",
		"msg_type":   "text",
		"content":    fmt.Sprintf("%s\n\n%s", msg.Title, msg.Body),
		"task_id":    fmt.Sprintf("notify_%s_%d", msg.SessionID, time.Now().Unix()),
	}

	body, err := json.Marshal(proxyMsg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	// 发送到 wecom-bot-proxy
	url := fmt.Sprintf("%s/api/bot/send", s.proxyURL)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", s.authToken)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("send to proxy: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("proxy returned %d", resp.StatusCode)
	}

	return nil
}