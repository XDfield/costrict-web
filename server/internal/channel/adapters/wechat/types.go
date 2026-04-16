package wechat

import "encoding/json"

const ILINKBaseURL = "https://ilinkai.weixin.qq.com"

type WeChatConfig struct {
	Token string `json:"token"`
}

type QRCodeResponse struct {
	QRCode         string `json:"qrcode"`
	QRCodeImgContent string `json:"qrcode_img_content"`
}

type QRCodeStatusResponse struct {
	Status   string `json:"status"`
	BotToken string `json:"bot_token,omitempty"`
	BaseURL  string `json:"baseurl,omitempty"`
}

type WeixinMessage struct {
	Seq          int64        `json:"seq,omitempty"`
	MessageID    int64        `json:"message_id,omitempty"`
	FromUserID   string       `json:"from_user_id,omitempty"`
	ToUserID     string       `json:"to_user_id,omitempty"`
	CreateTimeMs int64        `json:"create_time_ms,omitempty"`
	SessionID    string       `json:"session_id,omitempty"`
	MessageType  int          `json:"message_type,omitempty"`
	MessageState int          `json:"message_state,omitempty"`
	ItemList     []MessageItem `json:"item_list,omitempty"`
	ContextToken string       `json:"context_token,omitempty"`
}

type MessageItem struct {
	Type      int        `json:"type"`
	TextItem  *TextItem  `json:"text_item,omitempty"`
}

type TextItem struct {
	Text string `json:"text"`
}

type GetUpdatesRequest struct {
	GetUpdatesBuf string `json:"get_updates_buf"`
}

type GetUpdatesResponse struct {
	Ret                int             `json:"ret"`
	ErrCode            int             `json:"errcode,omitempty"`
	ErrMsg             string          `json:"errmsg,omitempty"`
	Msgs               []WeixinMessage `json:"msgs,omitempty"`
	GetUpdatesBuf      string          `json:"get_updates_buf"`
	LongpollingTimeoutMs int           `json:"longpolling_timeout_ms,omitempty"`
}

type SendMessageRequest struct {
	Msg struct {
		ToUserID     string        `json:"to_user_id"`
		ContextToken string        `json:"context_token"`
		ItemList     []MessageItem `json:"item_list"`
	} `json:"msg"`
}

type SendTypingRequest struct {
	ILinkUserID string `json:"ilink_user_id"`
	TypingTicket string `json:"typing_ticket"`
	Status       int    `json:"status"`
}

func ParseConfig(raw json.RawMessage) (*WeChatConfig, error) {
	var cfg WeChatConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
