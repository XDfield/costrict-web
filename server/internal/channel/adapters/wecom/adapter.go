package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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

func (a *WeComAdapter) SendInteractiveCard(ctx context.Context, userID string, card InteractiveCard, taskid string) error {
	cfg := &a.sysConfig

	cacheKey := fmt.Sprintf("%s:%d", cfg.CorpID, cfg.AgentID)
	cacheVal, _ := a.tokenCache.LoadOrStore(cacheKey, &tokenCacheEntry{})
	cache := cacheVal.(*tokenCacheEntry)

	accessToken, err := getAccessToken(cfg, a.client, cache)
	if err != nil {
		return fmt.Errorf("failed to get access token: %w", err)
	}

	subTitle := card.Description
	if card.URL != "" {
		subTitle += fmt.Sprintf("\n\n<a href=\"%s\">在会话中查看</a>", card.URL)
	}

	buttons := make([]WeComCardButton, len(card.Buttons))
	for i, b := range card.Buttons {
		buttons[i] = WeComCardButton{Text: b.Text, Style: b.Style, Key: b.Key}
	}

	reqBody := map[string]any{
		"touser":  userID,
		"msgtype": "template_card",
		"agentid": cfg.AgentID,
		"template_card": map[string]any{
			"card_type":      "button_interaction",
			"task_id":        taskid,
			"main_title":     map[string]string{"title": card.Title},
			"sub_title_text": subTitle,
			"button_list":    buttons,
		},
	}

	body, _ := json.Marshal(reqBody)
	slog.Info("[wecom] sending interactive card", "userID", userID, "taskid", taskid, "cardTitle", card.Title, "buttonCount", len(buttons), "body", string(body))
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
		return fmt.Errorf("wecom send interactive card error: %d %s", result.ErrCode, result.ErrMsg)
	}

	return nil
}

func (a *WeComAdapter) SendVoteCard(ctx context.Context, userID string, card VoteCard, taskid string) error {
	cfg := &a.sysConfig

	cacheKey := fmt.Sprintf("%s:%d", cfg.CorpID, cfg.AgentID)
	cacheVal, _ := a.tokenCache.LoadOrStore(cacheKey, &tokenCacheEntry{})
	cache := cacheVal.(*tokenCacheEntry)

	accessToken, err := getAccessToken(cfg, a.client, cache)
	if err != nil {
		return fmt.Errorf("failed to get access token: %w", err)
	}

	templateCard := map[string]any{
		"card_type":  "vote_interaction",
		"task_id":    taskid,
		"main_title": map[string]string{"title": card.Title},
		"checkbox":   card.Checkbox,
	}

	// Add subtitle if provided
	if card.SubTitle != "" {
		templateCard["sub_title_text"] = card.SubTitle
	}

	// Add submit button
	templateCard["submit_button"] = card.SubmitButton

	reqBody := map[string]any{
		"touser":        userID,
		"msgtype":       "template_card",
		"agentid":       cfg.AgentID,
		"template_card": templateCard,
	}

	body, _ := json.Marshal(reqBody)
	slog.Info("[wecom] sending vote card", "userID", userID, "taskid", taskid, "cardTitle", card.Title, "optionCount", len(card.Checkbox.OptionList), "body", string(body))

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
		return fmt.Errorf("wecom send vote card error: %d %s", result.ErrCode, result.ErrMsg)
	}

	return nil
}

func (a *WeComAdapter) UpdateCardStatus(responseCode, statusText string, cardData []byte) error {
	cfg := &a.sysConfig

	cacheKey := fmt.Sprintf("%s:%d", cfg.CorpID, cfg.AgentID)
	cacheVal, _ := a.tokenCache.LoadOrStore(cacheKey, &tokenCacheEntry{})
	cache := cacheVal.(*tokenCacheEntry)

	accessToken, err := getAccessToken(cfg, a.client, cache)
	if err != nil {
		return fmt.Errorf("failed to get access token: %w", err)
	}

	// Use stored card data as base
	var templateCard map[string]any
	if len(cardData) > 0 {
		if err := json.Unmarshal(cardData, &templateCard); err != nil {
			return fmt.Errorf("unmarshal card data failed: %w", err)
		}
	} else {
		templateCard = map[string]any{
			"card_type": "vote_interaction",
		}
	}

	cardType, _ := templateCard["card_type"].(string)

	if cardType == "vote_interaction" {
		// vote_interaction 更新：禁用选项、移除提交按钮、设置替换文案
		if checkbox, ok := templateCard["checkbox"].(map[string]any); ok {
			checkbox["disable"] = true
		}
		delete(templateCard, "submit_button")
	}

	templateCard["replace_text"] = statusText

	body, _ := json.Marshal(map[string]any{
		"agentid":       cfg.AgentID,
		"response_code": responseCode,
		"template_card": templateCard,
	})

	slog.Info("[wecom] updating card status", "responseCode", responseCode, "statusText", statusText, "body", string(body))

	url := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/message/update_template_card?access_token=%s&debug=1", accessToken)
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
		slog.Error("[wecom] update card error", "errcode", result.ErrCode, "errmsg", result.ErrMsg)
		return fmt.Errorf("wecom update card error: %d %s", result.ErrCode, result.ErrMsg)
	}

	slog.Info("[wecom] card status updated successfully")
	return nil
}
