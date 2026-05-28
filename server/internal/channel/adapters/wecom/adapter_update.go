package wecom

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/costrict/costrict-web/server/internal/channel"
)

func (a *WeComAdapter) UpdateCardStatus(responseCode, statusText, _ string, _ []byte, externalUserID string) error {
	cfg := &a.sysConfig

	cacheKey := fmt.Sprintf("%s:%d", cfg.CorpID, cfg.AgentID)
	cacheVal, _ := a.tokenCache.LoadOrStore(cacheKey, &tokenCacheEntry{})
	cache := cacheVal.(*tokenCacheEntry)

	accessToken, err := getAccessToken(cfg, a.client, cache)
	if err != nil {
		return fmt.Errorf("failed to get access token: %w", err)
	}

	reqBody := map[string]any{
		"agentid":       cfg.AgentID,
		"response_code": responseCode,
		"button": map[string]any{
			"replace_name": statusText,
		},
	}
	if externalUserID != "" {
		reqBody["userids"] = []string{externalUserID}
	}

	body, _ := json.Marshal(reqBody)

	slog.Info("[wecom] updating card status", "responseCode", responseCode, "statusText", statusText, "externalUserID", externalUserID, "body", string(body))

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

var _ channel.CardStatusUpdater = (*WeComAdapter)(nil)
