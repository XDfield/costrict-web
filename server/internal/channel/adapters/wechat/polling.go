package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/costrict/costrict-web/server/internal/channel"
)

var errSessionExpired = errors.New("session expired, re-login required")

type Poller struct {
	configID string
	rawCfg   json.RawMessage
	adapter  *WeChatAdapter
	client   *WeChatClient
	handler  channel.InboundMessageHandler
}

func NewPoller(configID string, rawCfg json.RawMessage, config *WeChatConfig, handler channel.InboundMessageHandler) *Poller {
	return &Poller{
		configID: configID,
		rawCfg:   rawCfg,
		adapter:  &WeChatAdapter{},
		client:   NewWeChatClient(config.Token),
		handler:  handler,
	}
}

func (p *Poller) Start(ctx context.Context) error {
	var cursor string
	backoff := time.Second

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		resp, err := p.client.GetUpdates(ctx, cursor)
		if err != nil {
			if errors.Is(err, errSessionExpired) {
				log.Printf("WeChat Poller: session expired (errcode=-14), user must re-login. Stopping poller.")
				return fmt.Errorf("session expired: please re-scan QR code to login")
			}
			log.Printf("WeChat Poller: getupdates error: %v, retrying in %s", err, backoff)
			time.Sleep(backoff)
			backoff = backoff * 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}
		backoff = time.Second

		if resp.GetUpdatesBuf != "" {
			cursor = resp.GetUpdatesBuf
		}

		for _, msg := range resp.Msgs {
			if msg.MessageType != 1 {
				continue
			}

			content := ""
			for _, item := range msg.ItemList {
				if item.TextItem != nil {
					content = item.TextItem.Text
				}
			}
			if content == "" {
				continue
			}

			rc := channel.ReplyContext{
				ChannelConfigID: p.configID,
				ChannelType:     "wechat",
				Target: channel.ReplyTarget{
					ExternalChatID: msg.FromUserID,
					ExternalUserID: msg.FromUserID,
					ContextToken:   msg.ContextToken,
				},
			}

			sender := channel.NewAdapterSender(p.adapter, p.rawCfg, rc)

			inbound := &channel.InboundMessage{
				ExternalChatID:    msg.FromUserID,
				ExternalChatType:  "direct",
				ExternalUserID:    msg.FromUserID,
				ExternalMessageID: fmt.Sprintf("%d", msg.MessageID),
				Content:           content,
				ContentType:       "text",
				Metadata: map[string]any{
					"contextToken": msg.ContextToken,
					"sessionID":    msg.SessionID,
				},
			}

			if err := p.handler(ctx, inbound, sender); err != nil {
				log.Printf("WeChat Poller: handler error: %v", err)
			}
		}
	}
}
