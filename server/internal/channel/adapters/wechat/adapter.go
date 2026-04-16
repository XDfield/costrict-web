package wechat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/channel"
)

type WeChatAdapter struct{}

func NewWeChatAdapter() *WeChatAdapter {
	return &WeChatAdapter{}
}

func (a *WeChatAdapter) Type() string { return "wechat" }

func (a *WeChatAdapter) Capabilities() channel.ChannelCapabilities {
	return channel.ChannelCapabilities{
		InboundMessages:  true,
		OutboundMessages: true,
		DirectChat:       true,
		GroupChat:        true,
		Markdown:         false,
		Media:            false,
		ContentTypes:     []string{"text", "image"},
	}
}

func (a *WeChatAdapter) ValidateConfig(config json.RawMessage) error {
	cfg, err := ParseConfig(config)
	if err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	if cfg.Token == "" {
		return fmt.Errorf("token is required")
	}
	return nil
}

func (a *WeChatAdapter) ConfigSchema() []channel.ConfigField {
	return []channel.ConfigField{
		{Key: "token", Label: "Bearer Token", Type: "password", Required: true, HelpText: "通过扫码登录自动获取"},
	}
}

func (a *WeChatAdapter) ParseInbound(_ *http.Request, _ json.RawMessage) (*channel.InboundMessage, error) {
	return nil, nil
}

func (a *WeChatAdapter) HandleVerification(_ *http.Request, _ json.RawMessage) (string, bool, error) {
	return "", false, nil
}

func (a *WeChatAdapter) Reply(ctx context.Context, config json.RawMessage, target channel.ReplyTarget, message channel.OutboundMessage) error {
	cfg, err := ParseConfig(config)
	if err != nil {
		return err
	}

	client := NewWeChatClient(cfg.Token)
	textItem := MessageItem{Type: 1, TextItem: &TextItem{Text: message.Content}}
	return client.SendMessage(ctx, target.ExternalChatID, target.ContextToken, []MessageItem{textItem})
}

func (a *WeChatAdapter) Start(ctx context.Context, config json.RawMessage, handler channel.InboundMessageHandler, opts channel.StartOptions) error {
	cfg, err := ParseConfig(config)
	if err != nil {
		return err
	}

	poller := NewPoller(opts.ConfigID, config, cfg, handler)
	return poller.Start(ctx)
}

func (a *WeChatAdapter) GetQRCode(ctx context.Context) (string, string, error) {
	client := NewWeChatClient("")
	result, err := client.GetQRCode(ctx)
	if err != nil {
		return "", "", err
	}
	return result.QRCode, result.QRCodeImgContent, nil
}

func (a *WeChatAdapter) GetLoginStatus(ctx context.Context, qrcodeID string) (string, string, error) {
	client := NewWeChatClient("")
	result, err := client.GetQRCodeStatus(ctx, qrcodeID)
	if err != nil {
		return "", "", err
	}
	if result.Status == "confirmed" {
		return result.Status, result.BotToken, nil
	}
	return result.Status, "", nil
}
