package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/channel"
	"github.com/costrict/costrict-web/server/internal/config"
)

type WeComAdapter struct {
	client     *http.Client
	sysConfig  config.WeComSystemConfig
	tokenCache sync.Map
}

func NewWeComAdapter(sysCfg config.WeComSystemConfig) *WeComAdapter {
	return &WeComAdapter{
		client:    &http.Client{Timeout: 30 * time.Second},
		sysConfig: sysCfg,
	}
}

func (a *WeComAdapter) Type() string { return "wecom" }

func (a *WeComAdapter) Capabilities() channel.ChannelCapabilities {
	return channel.ChannelCapabilities{
		InboundMessages:  true,
		OutboundMessages: true,
		DirectChat:       true,
		GroupChat:        true,
		Markdown:         true,
		Media:            false,
		MentionRequired:  true,
		ContentTypes:     []string{"text", "markdown"},
	}
}

func (a *WeComAdapter) ValidateConfig(config json.RawMessage) error {
	_, err := ParseUserConfig(config)
	return err
}

func (a *WeComAdapter) ConfigSchema() []channel.ConfigField {
	return []channel.ConfigField{
		{Key: "userId", Label: "企微账号 (UserID)", Type: "text", Required: true, Placeholder: "zhangsan", HelpText: "在企业微信通讯录中的账号"},
	}
}

func (a *WeComAdapter) ParseInbound(r *http.Request, _ json.RawMessage) (*channel.InboundMessage, error) {
	return ParseInboundMessage(r, &a.sysConfig)
}

func (a *WeComAdapter) HandleVerification(r *http.Request, _ json.RawMessage) (string, bool, error) {
	return HandleVerify(r, &a.sysConfig)
}

func (a *WeComAdapter) Reply(ctx context.Context, _ json.RawMessage, target channel.ReplyTarget, message channel.OutboundMessage) error {
	cfg := &a.sysConfig

	cacheKey := fmt.Sprintf("%s:%d", cfg.CorpID, cfg.AgentID)
	cacheVal, _ := a.tokenCache.LoadOrStore(cacheKey, &tokenCacheEntry{})
	cache := cacheVal.(*tokenCacheEntry)

	accessToken, err := getAccessToken(cfg, a.client, cache)
	if err != nil {
		return fmt.Errorf("failed to get access token: %w", err)
	}

	body, _ := json.Marshal(WeComSendRequest{
		ToUser:  target.ExternalChatID,
		MsgType: "text",
		AgentID: cfg.AgentID,
		Text:    &WeComSendText{Content: message.Content},
	})

	if message.ContentType == "markdown" {
		req := WeComSendRequest{
			ToUser:   target.ExternalChatID,
			MsgType:  "markdown",
			AgentID:  cfg.AgentID,
			Markdown: &WeComSendMarkdown{Content: message.Content},
		}
		body, _ = json.Marshal(req)
	}

	url := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/message/send?access_token=%s", accessToken)
	resp, err := a.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result WeComMessageResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if result.ErrCode != 0 {
		return fmt.Errorf("wecom send error: %d %s", result.ErrCode, result.ErrMsg)
	}

	return nil
}
