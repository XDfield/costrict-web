package wecombot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/channel"
	"github.com/costrict/costrict-web/server/internal/channel/adapters/wecom"
	"github.com/costrict/costrict-web/server/internal/config"
)

type WeComBotAdapter struct {
	sysConfig config.WeComBotSystemConfig
	client    *BotProxyClient
}

func NewWeComBotAdapter(sysCfg config.WeComBotSystemConfig) *WeComBotAdapter {
	var client *BotProxyClient
	if sysCfg.ProxyURL != "" {
		client = NewBotProxyClient(sysCfg.ProxyURL, sysCfg.AuthToken)
	}
	return &WeComBotAdapter{
		sysConfig: sysCfg,
		client:    client,
	}
}

func (a *WeComBotAdapter) Type() string { return "wecom-bot" }

func (a *WeComBotAdapter) Capabilities() channel.ChannelCapabilities {
	return channel.ChannelCapabilities{
		InboundMessages:  true,
		OutboundMessages: true,
		DirectChat:       true,
		GroupChat:        true,
		Markdown:         true,
		Media:            false,
		MentionRequired:  false,
		ContentTypes:     []string{"text", "markdown", "card"},
	}
}

func (a *WeComBotAdapter) ValidateConfig(config json.RawMessage) error {
	// wecom-bot 无需用户配置，直接绑定当前用户的 idtrust 认证
	return nil
}

func (a *WeComBotAdapter) ConfigSchema() []channel.ConfigField {
	// wecom-bot 无需用户配置，直接绑定当前用户的 idtrust 认证
	return nil
}

func (a *WeComBotAdapter) ParseInbound(r *http.Request, _ json.RawMessage) (*channel.InboundMessage, error) {
	var msg proxyInboundMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		return nil, fmt.Errorf("decode inbound: %w", err)
	}
	return &channel.InboundMessage{
		ExternalChatID:    msg.ExternalChatID,
		ExternalChatType:  msg.ExternalChatType,
		ExternalUserID:    msg.ExternalUserID,
		ExternalMessageID: msg.ExternalMessageID,
		ContentType:       msg.ContentType,
		Content:           msg.Content,
		Metadata:          msg.Metadata,
	}, nil
}

func (a *WeComBotAdapter) HandleVerification(_ *http.Request, _ json.RawMessage) (string, bool, error) {
	return "", false, nil
}

func (a *WeComBotAdapter) Reply(ctx context.Context, _ json.RawMessage, target channel.ReplyTarget, message channel.OutboundMessage) error {
	if a.client == nil {
		return fmt.Errorf("wecom-bot proxy not configured")
	}

	msgType := message.ContentType
	if msgType == "" {
		msgType = "text"
	}

	return a.client.Send(ctx, target.ExternalChatID, "single", msgType, message.Content, "")
}

func (a *WeComBotAdapter) SendInteractiveCard(ctx context.Context, userID string, card wecom.InteractiveCard, taskID string) error {
	if a.client == nil {
		return fmt.Errorf("wecom-bot proxy not configured")
	}

	cardJSON, err := json.Marshal(map[string]any{
		"card_type": "button_interaction",
		"task_id":   taskID,
		"main_title": map[string]string{
			"title": card.Title,
			"desc":  card.Description,
		},
		"button_list": cardToButtons(card.Buttons),
	})
	if err != nil {
		return err
	}
	return a.client.Send(ctx, userID, "single", "card", string(cardJSON), taskID)
}

func (a *WeComBotAdapter) SendVoteCard(ctx context.Context, userID string, card wecom.VoteCard, taskID string) error {
	if a.client == nil {
		return fmt.Errorf("wecom-bot proxy not configured")
	}

	cardJSON, err := json.Marshal(map[string]any{
		"card_type":     "vote_interaction",
		"task_id":       taskID,
		"main_title":    map[string]string{"title": card.Title, "desc": card.SubTitle},
		"checkbox":      card.Checkbox,
		"submit_button": card.SubmitButton,
	})
	if err != nil {
		return err
	}
	return a.client.Send(ctx, userID, "single", "card", string(cardJSON), taskID)
}

func (a *WeComBotAdapter) SendTextNoticeCard(ctx context.Context, userID string, card wecom.TextNoticeCard, taskID string) error {
	if a.client == nil {
		return fmt.Errorf("wecom-bot proxy not configured")
	}

	templateCard := map[string]any{
		"card_type":  "text_notice",
		"task_id":    taskID,
		"main_title": map[string]string{"title": card.Title},
	}
	if card.SubTitle != "" {
		templateCard["sub_title_text"] = card.SubTitle
	}
	if len(card.JumpList) > 0 {
		jumps := make([]map[string]any, len(card.JumpList))
		for i, j := range card.JumpList {
			jumps[i] = map[string]any{"type": 1, "title": j.Title, "url": j.URL}
		}
		templateCard["jump_list"] = jumps
		templateCard["card_action"] = map[string]any{"type": 1, "url": card.JumpList[0].URL}
	}

	cardJSON, err := json.Marshal(templateCard)
	if err != nil {
		return err
	}
	return a.client.Send(ctx, userID, "single", "card", string(cardJSON), taskID)
}

func (a *WeComBotAdapter) UpdateCardStatus(responseCode, statusText, action string, cardData []byte, externalUserID string) error {
	slog.Warn("[wecom-bot] UpdateCardStatus called without reqID context; use card update via proxy API instead")
	return fmt.Errorf("wecom-bot adapter requires reqID-based card update via proxy API")
}

func cardToButtons(buttons []wecom.CardButton) []map[string]any {
	result := make([]map[string]any, len(buttons))
	for i, b := range buttons {
		result[i] = map[string]any{
			"text":  b.Text,
			"style": b.Style,
			"key":   b.Key,
		}
	}
	return result
}
