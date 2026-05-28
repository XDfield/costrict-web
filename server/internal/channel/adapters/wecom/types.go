package wecom

import (
	"encoding/json"
	"encoding/xml"
)

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
	MsgId        int64  `xml:"MsgId"`
	AgentID      int    `xml:"AgentID"`
	ChatType     string `xml:"ChatType"`
	Event         string          `xml:"Event"`
	TaskId        string          `xml:"TaskId"`
	ResponseCode  string          `xml:"ResponseCode"`
	EventKey      string          `xml:"EventKey"`
	SelectedItems []SelectedItem  `xml:"SelectedItems>SelectedItem"`
}

type SelectedItem struct {
	XMLName     xml.Name `xml:"SelectedItem"`
	QuestionKey string   `xml:"QuestionKey"`
	OptionIds   []string `xml:"OptionIds>OptionId"`
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

// --- Interactive Card Types ---

type InteractiveCard struct {
	Title               string
	Description         string
	SubTitle            string
	URL                 string
	HorizontalContentList []HorizontalContentItem
	Buttons             []CardButton
}

type CardButton struct {
	Text  string
	Key   string
	Style int
}

type HorizontalContentItem struct {
	KeyName string
	Value   string
	Type    int    // 0=text, 1=url link, 2=download, 3=member detail
	URL     string
}

type WeComTemplateCard struct {
	CardType     string            `json:"card_type"`
	MainTitle    *WeComTitle       `json:"main_title,omitempty"`
	SubTitleText string            `json:"sub_title_text,omitempty"`
	ButtonList   []WeComCardButton `json:"button_list,omitempty"`
}

type WeComTitle struct {
	Title string `json:"title"`
}

type WeComCardButton struct {
	Text  string `json:"text"`
	Style int    `json:"style"`
	Key   string `json:"key"`
}

// --- Vote Interaction Card Types ---

type VoteCard struct {
	Title        string
	SubTitle     string
	Checkbox     WeComCheckbox
	SubmitButton WeComSubmitButton
	ReplaceText  string // Text to show after submission (e.g., "已提交")
}

type WeComCheckbox struct {
	QuestionKey string            `json:"question_key"`
	OptionList  []WeComVoteOption `json:"option_list"`
	Mode        int               `json:"mode"` // 0=single select, 1=multi select
	Disable     bool              `json:"disable,omitempty"` // Whether to disable the checkbox
}

type WeComVoteOption struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	IsChecked bool   `json:"is_checked"`
}

type WeComSubmitButton struct {
	Text string `json:"text"`
	Key  string `json:"key"`
}

type WeComCardSource struct {
	IconURL string `json:"icon_url,omitempty"`
	Desc    string `json:"desc,omitempty"`
}
