package wechat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"time"
)

type WeChatClient struct {
	client *http.Client
	token  string
}

func NewWeChatClient(token string) *WeChatClient {
	return &WeChatClient{
		client: &http.Client{Timeout: 40 * time.Second},
		token:  token,
	}
}

func (c *WeChatClient) doRequest(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var reqBody *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(b)
	} else {
		reqBody = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, ILINKBaseURL+"/ilink/bot/"+path, reqBody)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	uin := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d", rand.Uint32())))
	req.Header.Set("X-WECHAT-UIN", uin)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *WeChatClient) GetQRCode(ctx context.Context) (*QRCodeResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", ILINKBaseURL+"/ilink/bot/get_bot_qrcode?bot_type=3", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result QRCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *WeChatClient) GetQRCodeStatus(ctx context.Context, qrcode string) (*QRCodeStatusResponse, error) {
	url := fmt.Sprintf("%s/ilink/bot/get_qrcode_status?qrcode=%s", ILINKBaseURL, qrcode)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result QRCodeStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *WeChatClient) GetUpdates(ctx context.Context, getUpdatesBuf string) (*GetUpdatesResponse, error) {
	raw, err := c.doRequest(ctx, "POST", "getupdates", GetUpdatesRequest{GetUpdatesBuf: getUpdatesBuf})
	if err != nil {
		return nil, err
	}

	var resp GetUpdatesResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.Ret != 0 {
		if resp.ErrCode == -14 {
			return &resp, errSessionExpired
		}
		return &resp, fmt.Errorf("getupdates error: ret=%d errcode=%d errmsg=%s", resp.Ret, resp.ErrCode, resp.ErrMsg)
	}
	return &resp, nil
}

func (c *WeChatClient) SendMessage(ctx context.Context, toUserID, contextToken string, itemList []MessageItem) error {
	req := SendMessageRequest{}
	req.Msg.ToUserID = toUserID
	req.Msg.ContextToken = contextToken
	req.Msg.ItemList = itemList

	raw, err := c.doRequest(ctx, "POST", "sendmessage", req)
	if err != nil {
		return err
	}

	var result struct {
		Ret     int    `json:"ret"`
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if result.Ret != 0 {
		return fmt.Errorf("sendmessage error: ret=%d errcode=%d errmsg=%s", result.Ret, result.ErrCode, result.ErrMsg)
	}
	return nil
}
