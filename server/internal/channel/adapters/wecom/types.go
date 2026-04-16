package wecom

import "encoding/json"

type WeComUserConfig struct {
	UserID string `json:"userId"`
}

type WeComCallbackRequest struct {
	ToUserName string `xml:"ToUserName"`
	AgentID    string `xml:"AgentID"`
	Encrypt    string `xml:"Encrypt"`
}

type WeComCallbackMessage struct {
	ToUserName   string `xml:"ToUserName"`
	FromUserName string `xml:"FromUserName"`
	CreateTime   int64  `xml:"CreateTime"`
	MsgType      string `xml:"MsgType"`
	Content      string `xml:"Content"`
	MsgId       int64  `xml:"MsgId"`
	AgentID     string `xml:"AgentID"`
	ChatType    string `xml:"ChatType"`
	Event       string `xml:"Event"`
}

type WeComMessageResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

type WeComAccessTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	ErrCode     int    `json:"errcode"`
	ErrMsg      string `json:"errmsg"`
}

type WeComSendRequest struct {
	ToUser  string              `json:"touser"`
	ToParty string              `json:"toparty,omitempty"`
	ToTag   string              `json:"totag,omitempty"`
	MsgType string              `json:"msgtype"`
	AgentID int                 `json:"agentid"`
	Text    *WeComSendText       `json:"text,omitempty"`
	Markdown *WeComSendMarkdown `json:"markdown,omitempty"`
}

type WeComSendText struct {
	Content string `json:"content"`
}

type WeComSendMarkdown struct {
	Content string `json:"content"`
}

func ParseUserConfig(raw json.RawMessage) (*WeComUserConfig, error) {
	var cfg WeComUserConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
