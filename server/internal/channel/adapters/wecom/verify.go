package wecom

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/channel"
	"github.com/costrict/costrict-web/server/internal/config"
)

func HandleVerify(r *http.Request, cfg *config.WeComSystemConfig) (string, bool, error) {
	msgSignature := r.URL.Query().Get("msg_signature")
	timestamp := r.URL.Query().Get("timestamp")
	nonce := r.URL.Query().Get("nonce")
	echostr := r.URL.Query().Get("echostr")

	if echostr == "" {
		return "", false, nil
	}

	signature := VerifySignature(cfg.Token, timestamp, nonce, echostr)
	if signature != msgSignature {
		return "", false, fmt.Errorf("signature mismatch")
	}

	plain, err := Decrypt(cfg.EncodingAESKey, echostr)
	if err != nil {
		return "", false, fmt.Errorf("decrypt failed: %w", err)
	}

	return string(plain), true, nil
}

func ParseInboundMessage(r *http.Request, cfg *config.WeComSystemConfig) (*channel.InboundMessage, error) {
	var req struct {
		XMLName   xml.Name `xml:"xml"`
		ToUserName string   `xml:"ToUserName"`
		Encrypt   string   `xml:"Encrypt"`
	}
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, fmt.Errorf("decode xml failed: %w", err)
	}

	msgSignature := r.URL.Query().Get("msg_signature")
	timestamp := r.URL.Query().Get("timestamp")
	nonce := r.URL.Query().Get("nonce")

	signature := VerifySignature(cfg.Token, timestamp, nonce, req.Encrypt)
	if signature != msgSignature {
		return nil, fmt.Errorf("signature mismatch")
	}

	plain, err := Decrypt(cfg.EncodingAESKey, req.Encrypt)
	if err != nil {
		return nil, fmt.Errorf("decrypt failed: %w", err)
	}

	var msg WeComCallbackMessage
	if err := xml.Unmarshal(plain, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal message failed: %w", err)
	}

	if msg.MsgType != "text" && !(msg.MsgType == "event" && msg.Event == "template_card_event") {
		return nil, nil
	}

	// Handle interactive card callback
	if msg.MsgType == "event" && msg.Event == "template_card_event" {
		action, token := parseEventKey(msg.EventKey)

		metadata := map[string]any{
			"actionToken":  token,
			"responseCode": msg.ResponseCode,
			"taskId":       msg.TaskId,
		}

		// For vote_interaction callbacks, derive action from selected options
		if action == "submit" && len(msg.SelectedItems) > 0 {
			selected := msg.SelectedItems[0]
			if len(selected.OptionIds) > 0 {
				optionID := selected.OptionIds[0]
				if optionID == "approve" || optionID == "reject" {
					action = optionID
				} else {
					action = "select:" + strings.Join(selected.OptionIds, ",")
				}
			}
			metadata["selectedOptions"] = selected.OptionIds
		}

		return &channel.InboundMessage{
			ExternalChatID:    msg.FromUserName,
			ExternalUserID:    msg.FromUserName,
			ContentType:       "action_callback",
			Content:           action,
			Metadata:          metadata,
		}, nil
	}

	chatType := "direct"
	if msg.ChatType == "group" {
		chatType = "group"
	}

	return &channel.InboundMessage{
		ExternalChatID:    msg.FromUserName,
		ExternalChatType:  chatType,
		ExternalUserID:    msg.FromUserName,
		ExternalMessageID: fmt.Sprintf("%d", msg.MsgId),
		Content:           msg.Content,
		ContentType:       "text",
		Metadata: map[string]any{
			"toUserName": msg.ToUserName,
			"agentId":    msg.AgentID,
		},
	}, nil
}

// parseEventKey parses the EventKey from WeCom interactive card callback.
// "approve:TOKEN"     → ("approve", "TOKEN")
// "reject:TOKEN"      → ("reject", "TOKEN")
// "select:TOKEN:0"    → ("select:0", "TOKEN")
// "navigate:URL"      → ("navigate", "")
func parseEventKey(key string) (action string, token string) {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) < 2 {
		return key, ""
	}
	if parts[0] == "select" && len(parts) == 3 {
		return "select:" + parts[2], parts[1]
	}
	return parts[0], parts[1]
}

type tokenCacheEntry struct {
	token      string
	expireTime time.Time
}

func getAccessToken(cfg *config.WeComSystemConfig, client *http.Client, cache *tokenCacheEntry) (string, error) {
	if cache != nil && time.Now().Before(cache.expireTime) {
		return cache.token, nil
	}

	url := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/gettoken?corpid=%s&corpsecret=%s", cfg.CorpID, cfg.Secret)
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result WeComAccessTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if result.ErrCode != 0 {
		return "", fmt.Errorf("wecom api error: %d %s", result.ErrCode, result.ErrMsg)
	}

	if cache != nil {
		cache.token = result.AccessToken
		cache.expireTime = time.Now().Add(time.Duration(result.ExpiresIn-200) * time.Second)
	}

	return result.AccessToken, nil
}
