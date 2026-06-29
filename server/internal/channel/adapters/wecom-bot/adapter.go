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

	return a.client.SendWithSessionRef(ctx, target.ExternalChatID, "single", msgType, message.Content, "", extractSessionRef(message.Metadata))
}

// extractSessionRef pulls an optional {title, url} pair from outbound metadata.
// Adapters should not assume the key exists — conversational replies typically
// omit it; event-driven replies (permission/question/etc.) populate it so the
// proxy can render a clickable link back to the related CoStrict session.
func extractSessionRef(metadata map[string]any) *proxySessionRef {
	if metadata == nil {
		return nil
	}
	raw, ok := metadata["session_ref"]
	if !ok {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	title, _ := m["title"].(string)
	url, _ := m["url"].(string)
	if url == "" {
		return nil
	}
	return &proxySessionRef{Title: title, URL: url}
}

// Card delivery is intentionally disabled for wecom-bot. Notifications now flow
// as markdown text (with session_ref link) via Reply/Send; interactive cards are
// not used in the current UX. Methods remain as no-ops so dispatcher's call
// sites and notification-data tracking stay intact — re-enable by restoring the
// card JSON marshal + a.client.Send(..., "card", ...) body.

func (a *WeComBotAdapter) SendInteractiveCard(ctx context.Context, userID string, card wecom.InteractiveCard, taskID string) error {
	slog.Info("[wecom-bot] SendInteractiveCard skipped (cards disabled)", "userID", userID, "taskID", taskID)
	return nil
}

func (a *WeComBotAdapter) SendVoteCard(ctx context.Context, userID string, card wecom.VoteCard, taskID string) error {
	slog.Info("[wecom-bot] SendVoteCard skipped (cards disabled)", "userID", userID, "taskID", taskID)
	return nil
}

func (a *WeComBotAdapter) SendTextNoticeCard(ctx context.Context, userID string, card wecom.TextNoticeCard, taskID string) error {
	slog.Info("[wecom-bot] SendTextNoticeCard skipped (cards disabled)", "userID", userID, "taskID", taskID)
	return nil
}

func (a *WeComBotAdapter) UpdateCardStatus(responseCode, statusText, action string, cardData []byte, externalUserID string) error {
	slog.Warn("[wecom-bot] UpdateCardStatus called without reqID context; use card update via proxy API instead")
	return fmt.Errorf("wecom-bot adapter requires reqID-based card update via proxy API")
}
