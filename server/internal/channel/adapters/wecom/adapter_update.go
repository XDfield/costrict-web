package wecom

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/costrict/costrict-web/server/internal/channel"
)

func (a *WeComAdapter) UpdateCardStatus(responseCode, statusText, action string, cardData []byte, externalUserID string) error {
	cfg := &a.sysConfig

	cacheKey := fmt.Sprintf("%s:%d", cfg.CorpID, cfg.AgentID)
	cacheVal, _ := a.tokenCache.LoadOrStore(cacheKey, &tokenCacheEntry{})
	cache := cacheVal.(*tokenCacheEntry)

	accessToken, err := getAccessToken(cfg, a.client, cache)
	if err != nil {
		return fmt.Errorf("failed to get access token: %w", err)
	}

	var reqBody map[string]any

	// If we have cardData with card_type, build a full template_card update
	if cardData != nil && len(cardData) > 0 {
		var data map[string]any
		if json.Unmarshal(cardData, &data) == nil {
			if cardType, _ := data["card_type"].(string); cardType == "vote_interaction" {
				reqBody = a.buildVoteUpdateRequest(responseCode, statusText, action, data, externalUserID)
			}
		}
	}

	// Fallback: simple button replace
	if reqBody == nil {
		reqBody = map[string]any{
			"agentid":       cfg.AgentID,
			"response_code": responseCode,
			"button": map[string]any{
				"replace_name": statusText,
			},
		}
		if externalUserID != "" {
			reqBody["userids"] = []string{externalUserID}
		}
	}

	body, _ := json.Marshal(reqBody)
	slog.Info("[wecom] updating card status", "responseCode", responseCode, "statusText", statusText, "action", action, "externalUserID", externalUserID, "body", string(body))

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

func (a *WeComAdapter) buildVoteUpdateRequest(responseCode, statusText, action string, cardData map[string]any, externalUserID string) map[string]any {
	// Extract original title and description
	mainTitle := ""
	mainDesc := ""
	if mt, ok := cardData["main_title"].(map[string]any); ok {
		mainTitle, _ = mt["title"].(string)
		mainDesc, _ = mt["desc"].(string)
	}

	// Extract selected option text from action string (e.g. "select:opt_0")
	selectedText := ""
	optIdx := -1
	if strings.HasPrefix(action, "select:opt_") {
		idx := 0
		if _, err := fmt.Sscanf(action, "select:opt_%d", &idx); err == nil {
			optIdx = idx
		}
	}

	if optIdx >= 0 {
		if checkbox, ok := cardData["checkbox"].(map[string]any); ok {
			if options, ok := checkbox["option_list"].([]any); ok && optIdx < len(options) {
				if opt, ok := options[optIdx].(map[string]any); ok {
					selectedText, _ = opt["text"].(string)
				}
			}
		}
	}

	templateCard := map[string]any{
		"card_type": "text_notice",
	}

	// main_title: show original title + description
	mt := map[string]string{}
	if mainTitle != "" {
		mt["title"] = mainTitle
	}
	if mainDesc != "" {
		mt["desc"] = mainDesc
	}
	if len(mt) > 0 {
		templateCard["main_title"] = mt
	}

	// sub_title_text: show selected result
	if selectedText != "" {
		templateCard["sub_title_text"] = fmt.Sprintf("已选择：%s", selectedText)
	}

	// card_action is required for text_notice; use type 1 with empty url as no-op
	templateCard["card_action"] = map[string]any{"type": 1, "url": "https://placeholder"}

	reqBody := map[string]any{
		"agentid":       a.sysConfig.AgentID,
		"response_code": responseCode,
		"template_card": templateCard,
	}
	if externalUserID != "" {
		reqBody["userids"] = []string{externalUserID}
	}

	return reqBody
}

var _ channel.CardStatusUpdater = (*WeComAdapter)(nil)
