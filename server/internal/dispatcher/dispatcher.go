package dispatcher

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/channel/adapters/wecom"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/notification"
	"gorm.io/gorm"
)

type DispatchInput struct {
	UserID      string
	WorkspaceID string
	EventType   string
	SessionID   string
	DeviceID    string
	Path        string
	SessionURL  string
	ActionData  map[string]any
}

type Dispatcher struct {
	db              *gorm.DB
	store           *notification.Store
	notificationSvc *notification.NotificationService
	wecomAdapter    *wecom.WeComAdapter
	appURL          string
	bufferPeriod    time.Duration
	pendingMap      sync.Map
}

func NewDispatcher(db *gorm.DB, notificationSvc *notification.NotificationService, store *notification.Store, appURL string, bufferSeconds int, wecomAdapter *wecom.WeComAdapter) *Dispatcher {
	bufferPeriod := time.Duration(bufferSeconds) * time.Second
	return &Dispatcher{
		db:              db,
		store:           store,
		notificationSvc: notificationSvc,
		wecomAdapter:    wecomAdapter,
		appURL:          appURL,
		bufferPeriod:    bufferPeriod,
	}
}

// --- WeCom UserID Resolution ---

// resolveWeComUserID 从用户的 IDTrust 身份认证信息中获取企微员工 userId
func (d *Dispatcher) resolveWeComUserID(appUserID string) string {
	var identity models.UserAuthIdentity
	if err := d.db.Where("user_subject_id = ? AND provider = ? AND deleted_at IS NULL", appUserID, "idtrust").
		First(&identity).Error; err != nil {
		slog.Error("[dispatcher] failed to resolve wecom user id from idtrust", "appUserID", appUserID, "error", err)
		return ""
	}
	if identity.ProviderUserID != nil {
		return *identity.ProviderUserID
	}
	return ""
}

// --- Public Interface ---

// DispatchStaleNotification implements notification.StaleDispatcher interface
func (d *Dispatcher) DispatchStaleNotification(n models.SystemNotification) {
	if d.wecomAdapter == nil {
		return
	}
	slog.Info("[dispatcher] stale notification dispatching", "sessionID", n.SessionID, "type", n.Type, "userID", n.UserID)

	wecomUserID := d.resolveWeComUserID(n.UserID)
	if wecomUserID == "" {
		slog.Error("[dispatcher] cannot resolve wecom user id for stale notification", "appUserID", n.UserID)
		return
	}

	input := DispatchInput{
		UserID:    n.UserID,
		EventType: n.Type,
		SessionID: n.SessionID,
		DeviceID:  n.DeviceID,
	}
	if n.ActionData != nil {
		json.Unmarshal(n.ActionData, &input.ActionData)
	}

	d.dispatchToWeCom(input, n.ActionToken, wecomUserID)
}

func (d *Dispatcher) Dispatch(input DispatchInput) {
	if d.store == nil {
		return
	}

	channels := d.selectChannels(input.UserID)

	if needsInteraction(input.EventType) && d.bufferPeriod > 0 {
		d.bufferDispatch(input, channels)
		return
	}

	d.dispatchNow(input, channels)
}

func (d *Dispatcher) OnInterventionResponse(actionToken string) {
	val, ok := d.pendingMap.LoadAndDelete(actionToken)
	if !ok {
		return
	}
	pending := val.(*pendingNotification)
	pending.Timer.Stop()
	slog.Info("[dispatcher] buffer cancelled by intervention response", "actionToken", actionToken)
}

// --- Event Classification ---

func needsInteraction(eventType string) bool {
	return eventType == "permission" || eventType == "question"
}

// --- Buffer Mechanism ---

type pendingNotification struct {
	Input       DispatchInput
	ActionToken string
	CreatedAt   time.Time
	Timer       *time.Timer
}

func (d *Dispatcher) bufferDispatch(input DispatchInput, channels *SelectedChannels) {
	n, err := d.store.Create(notification.CreateNotificationInput{
		UserID:     input.UserID,
		Type:       input.EventType,
		Title:      mapEventTypeToTitle(input.EventType),
		SessionID:  input.SessionID,
		DeviceID:   input.DeviceID,
		ActionType: input.EventType,
		ActionData: mustMarshal(input.ActionData),
	})
	if err != nil {
		slog.Error("[dispatcher] create buffered notification failed", "error", err)
		d.dispatchNow(input, channels)
		return
	}

	pending := &pendingNotification{
		Input:       input,
		ActionToken: n.ActionToken,
		CreatedAt:   time.Now(),
	}

	d.pendingMap.Store(n.ActionToken, pending)

	pending.Timer = time.AfterFunc(d.bufferPeriod, func() {
		d.handleBufferTimeout(n.ActionToken)
	})
}

func (d *Dispatcher) handleBufferTimeout(actionToken string) {
	val, ok := d.pendingMap.LoadAndDelete(actionToken)
	if !ok {
		return
	}

	pending := val.(*pendingNotification)

	if d.isTokenHandled(actionToken) {
		slog.Info("[dispatcher] buffer timeout but already handled", "actionToken", actionToken)
		return
	}

	wecomUserID := d.resolveWeComUserID(pending.Input.UserID)
	if wecomUserID == "" {
		slog.Error("[dispatcher] cannot resolve wecom user id for buffered notification", "appUserID", pending.Input.UserID)
		return
	}

	d.dispatchToWeCom(pending.Input, actionToken, wecomUserID)
}

func (d *Dispatcher) isTokenHandled(actionToken string) bool {
	var count int64
	d.db.Model(&models.SystemNotification{}).
		Where("action_token = ? AND status = ?", actionToken, "acted").
		Count(&count)
	return count > 0
}

// --- Channel Selection ---

type SelectedChannels struct {
	Interactive []models.ChannelConfig
}

func (d *Dispatcher) selectChannels(userID string) *SelectedChannels {
	result := &SelectedChannels{}
	d.db.Where(
		"user_id = ? AND channel_type IN ('wecom') AND enabled = true AND deleted_at IS NULL",
		userID,
	).Find(&result.Interactive)
	return result
}

// --- Dispatch Routing ---

func (d *Dispatcher) dispatchNow(input DispatchInput, channels *SelectedChannels) {
	if needsInteraction(input.EventType) && len(channels.Interactive) > 0 {
		d.dispatchInteractive(input, channels)
		return
	}
	d.dispatchNotification(input)
}

func (d *Dispatcher) dispatchInteractive(input DispatchInput, channels *SelectedChannels) {
	wecomUserID := d.resolveWeComUserID(input.UserID)
	if wecomUserID == "" {
		slog.Error("[dispatcher] cannot resolve wecom user id", "appUserID", input.UserID)
		d.dispatchNotification(input)
		return
	}

	for _, ch := range channels.Interactive {
		if ch.ChannelType == "wecom" {
			d.dispatchToWeComNew(input, wecomUserID)
			return
		}
	}
	d.dispatchNotification(input)
}

func (d *Dispatcher) dispatchNotification(input DispatchInput) {
	if d.notificationSvc != nil {
		d.notificationSvc.TriggerNotifications(input.UserID, input.EventType, input.SessionID, input.DeviceID, input.Path)
	}
}

// dispatchToWeComNew is the direct dispatch path: creates new notifications and sends cards
func (d *Dispatcher) dispatchToWeComNew(input DispatchInput, wecomUserID string) {
	switch input.EventType {
	case "permission":
		d.sendApprovalCard(input, wecomUserID)
	case "question":
		d.sendVoteCards(input, wecomUserID)
	default:
		d.sendGuidanceCard(input, wecomUserID, false)
	}
}

// dispatchToWeCom is the buffered/stale dispatch path: uses existing action token
func (d *Dispatcher) dispatchToWeCom(input DispatchInput, actionToken string, wecomUserID string) {
	switch input.EventType {
	case "permission":
		d.sendApprovalCardWithToken(input, actionToken, wecomUserID)
	case "question":
		d.sendVoteCardsWithToken(input, actionToken, wecomUserID)
	default:
		d.sendGuidanceCardWithToken(input, actionToken, wecomUserID, false)
	}
}

// --- Permission Card (Button Interaction) ---

func (d *Dispatcher) sendApprovalCard(input DispatchInput, wecomUserID string) {
	title := buildPermissionTitle(input)

	buttons := []wecom.CardButton{
		{Text: "批准", Key: "", Style: 1},
		{Text: "拒绝", Key: "", Style: 0},
	}

	cardData := map[string]any{
		"card_type":  "button_interaction",
		"main_title": map[string]string{"title": title},
		"button_list": buttons,
	}

	n, err := d.store.Create(notification.CreateNotificationInput{
		UserID:     input.UserID,
		Type:       "permission",
		Title:      title,
		SessionID:  input.SessionID,
		DeviceID:   input.DeviceID,
		ActionType: "permission",
		ActionData: mustMarshal(input.ActionData),
		CardData:   mustMarshal(cardData),
	})
	if err != nil {
		slog.Error("[dispatcher] create permission notification failed", "error", err)
		return
	}

	// Update card data with real action token in button keys
	buttons[0].Key = "approve:" + n.ActionToken
	buttons[1].Key = "reject:" + n.ActionToken
	cardData["button_list"] = buttons
	d.updateNotificationData(n.ActionToken, title, cardData)

	if d.wecomAdapter != nil {
		taskID := fmt.Sprintf("perm_%s_%d", input.SessionID, time.Now().UnixMilli())
		card := wecom.InteractiveCard{
			Title:   title,
			Buttons: buttons,
		}
		if input.Path != "" {
			card.URL = fmt.Sprintf("%s%s", d.appURL, input.Path)
		}
		if err := d.wecomAdapter.SendInteractiveCard(nil, wecomUserID, card, taskID); err != nil {
			slog.Error("[dispatcher] send wecom approval card failed", "error", err)
		}
	}
}

func (d *Dispatcher) sendApprovalCardWithToken(input DispatchInput, actionToken string, wecomUserID string) {
	title := buildPermissionTitle(input)

	buttons := []wecom.CardButton{
		{Text: "批准", Key: "approve:" + actionToken, Style: 1},
		{Text: "拒绝", Key: "reject:" + actionToken, Style: 0},
	}

	cardData := map[string]any{
		"card_type":   "button_interaction",
		"main_title":  map[string]string{"title": title},
		"button_list": buttons,
	}

	d.updateNotificationData(actionToken, title, cardData)

	if d.wecomAdapter != nil {
		taskID := fmt.Sprintf("perm_%s_%d", input.SessionID, time.Now().UnixMilli())
		card := wecom.InteractiveCard{
			Title:   title,
			Buttons: buttons,
		}
		if input.Path != "" {
			card.URL = fmt.Sprintf("%s%s", d.appURL, input.Path)
		}
		if err := d.wecomAdapter.SendInteractiveCard(nil, wecomUserID, card, taskID); err != nil {
			slog.Error("[dispatcher] send wecom approval card failed", "error", err)
		}
	}
}

// --- Question Vote Cards ---

// sendVoteCards creates new notifications and sends vote_interaction cards (direct path)
// 多选问卷视为复杂场景，走引导卡片
func (d *Dispatcher) sendVoteCards(input DispatchInput, wecomUserID string) {
	questions := extractQuestionInfos(input.ActionData)
	if len(questions) == 0 {
		d.sendGuidanceCard(input, wecomUserID, false)
		return
	}

	if hasMultipleSelect(questions) {
		// Complex survey: only send notice card with session link
		d.sendSessionNoticeCard(input, wecomUserID, "需要你的操作", "有复杂问题需要回答，请点击下方会话地址跳转查看")
		return
	}

	// Simple questions: send notice card first, then vote cards
	d.sendSessionNoticeCard(input, wecomUserID, "会话通知", "有以下问题需要回答")

	for i, q := range questions {
		d.sendSingleVoteCard(input, wecomUserID, q, i)
	}
}

// sendVoteCardsWithToken uses existing action token for first question, creates new tokens for rest (buffered/stale path)
// 多选问卷视为复杂场景，走引导卡片
func (d *Dispatcher) sendVoteCardsWithToken(input DispatchInput, actionToken string, wecomUserID string) {
	questions := extractQuestionInfos(input.ActionData)
	if len(questions) == 0 {
		d.sendGuidanceCardWithToken(input, actionToken, wecomUserID, false)
		return
	}

	if hasMultipleSelect(questions) {
		d.sendSessionNoticeCard(input, wecomUserID, "需要你的操作", "有复杂问题需要回答，请点击下方会话地址跳转查看")
		return
	}

	// Simple questions: send notice card first, then vote cards
	d.sendSessionNoticeCard(input, wecomUserID, "会话通知", "有以下问题需要回答")

	// First question reuses the existing action token
	d.sendSingleVoteCardWithToken(input, actionToken, wecomUserID, questions[0], 0)

	// Remaining questions need their own tokens
	for i := 1; i < len(questions); i++ {
		q := questions[i]
		title := q.Header
		if title == "" {
			title = q.Question
		}

		mode := 0
		if q.Multiple {
			mode = 1
		}
		options := buildVoteOptions(q.Options)
		checkbox := wecom.WeComCheckbox{
			QuestionKey: fmt.Sprintf("q_%d", i),
			OptionList:  options,
			Mode:        mode,
		}
		submitBtn := wecom.WeComSubmitButton{Text: "提交", Key: ""}

		cardData := map[string]any{
			"card_type":     "vote_interaction",
			"main_title":    map[string]string{"title": title},
			"checkbox":      checkbox,
			"submit_button": submitBtn,
		}

		actionData := make(map[string]any)
		for k, v := range input.ActionData {
			actionData[k] = v
		}
		actionData["questionIndex"] = i

		n, err := d.store.Create(notification.CreateNotificationInput{
			UserID:     input.UserID,
			Type:       "question",
			Title:      title,
			SessionID:  input.SessionID,
			DeviceID:   input.DeviceID,
			ActionType: "question",
			ActionData: mustMarshal(actionData),
			CardData:   mustMarshal(cardData),
		})
		if err != nil {
			slog.Error("[dispatcher] create question notification failed", "index", i, "error", err)
			continue
		}

		submitBtn.Key = "submit:" + n.ActionToken
		cardData["submit_button"] = submitBtn
		d.updateNotificationData(n.ActionToken, title, cardData)

		if d.wecomAdapter != nil {
			taskID := fmt.Sprintf("q_%s_%d_%d", input.SessionID, time.Now().UnixMilli(), i)
			voteCard := wecom.VoteCard{
				Title:        title,
				SubTitle:     q.Question,
				Checkbox:     checkbox,
				SubmitButton: submitBtn,
				ReplaceText:  "已提交",
			}
			if err := d.wecomAdapter.SendVoteCard(nil, wecomUserID, voteCard, taskID); err != nil {
				slog.Error("[dispatcher] send wecom question card failed", "index", i, "error", err)
			}
		}
	}
}

// sendSingleVoteCard creates a new notification and sends one vote_interaction card (direct path)
func (d *Dispatcher) sendSingleVoteCard(input DispatchInput, wecomUserID string, q questionInfo, questionIndex int) {
	title := q.Header
	if title == "" {
		title = q.Question
	}

	mode := 0
	if q.Multiple {
		mode = 1
	}
	options := buildVoteOptions(q.Options)
	checkbox := wecom.WeComCheckbox{
		QuestionKey: fmt.Sprintf("q_%d", questionIndex),
		OptionList:  options,
		Mode:        mode,
	}
	submitBtn := wecom.WeComSubmitButton{Text: "提交", Key: ""}

	cardData := map[string]any{
		"card_type":     "vote_interaction",
		"main_title":    map[string]string{"title": title},
		"checkbox":      checkbox,
		"submit_button": submitBtn,
	}

	actionData := make(map[string]any)
	for k, v := range input.ActionData {
		actionData[k] = v
	}
	actionData["questionIndex"] = questionIndex

	n, err := d.store.Create(notification.CreateNotificationInput{
		UserID:     input.UserID,
		Type:       "question",
		Title:      title,
		SessionID:  input.SessionID,
		DeviceID:   input.DeviceID,
		ActionType: "question",
		ActionData: mustMarshal(actionData),
		CardData:   mustMarshal(cardData),
	})
	if err != nil {
		slog.Error("[dispatcher] create question notification failed", "index", questionIndex, "error", err)
		return
	}

	submitBtn.Key = "submit:" + n.ActionToken
	cardData["submit_button"] = submitBtn
	d.updateNotificationData(n.ActionToken, title, cardData)

	if d.wecomAdapter != nil {
		taskID := fmt.Sprintf("q_%s_%d_%d", input.SessionID, time.Now().UnixMilli(), questionIndex)
		voteCard := wecom.VoteCard{
			Title:        title,
			SubTitle:     q.Question,
			Checkbox:     checkbox,
			SubmitButton: submitBtn,
			ReplaceText:  "已提交",
		}
		if err := d.wecomAdapter.SendVoteCard(nil, wecomUserID, voteCard, taskID); err != nil {
			slog.Error("[dispatcher] send wecom question card failed", "index", questionIndex, "error", err)
		}
	}
}

// sendSingleVoteCardWithToken sends one vote_interaction card using existing action token (buffered/stale path)
func (d *Dispatcher) sendSingleVoteCardWithToken(input DispatchInput, actionToken string, wecomUserID string, q questionInfo, questionIndex int) {
	title := q.Header
	if title == "" {
		title = q.Question
	}

	mode := 0
	if q.Multiple {
		mode = 1
	}
	options := buildVoteOptions(q.Options)
	checkbox := wecom.WeComCheckbox{
		QuestionKey: fmt.Sprintf("q_%d", questionIndex),
		OptionList:  options,
		Mode:        mode,
	}
	submitBtn := wecom.WeComSubmitButton{
		Text: "提交",
		Key:  "submit:" + actionToken,
	}

	cardData := map[string]any{
		"card_type":     "vote_interaction",
		"main_title":    map[string]string{"title": title},
		"checkbox":      checkbox,
		"submit_button": submitBtn,
	}

	d.updateNotificationData(actionToken, title, cardData)

	if d.wecomAdapter != nil {
		taskID := fmt.Sprintf("q_%s_%d_%d", input.SessionID, time.Now().UnixMilli(), questionIndex)
			voteCard := wecom.VoteCard{
			Title:        title,
			SubTitle:     q.Question,
			Checkbox:     checkbox,
			SubmitButton: submitBtn,
			ReplaceText:  "已提交",
		}
		if err := d.wecomAdapter.SendVoteCard(nil, wecomUserID, voteCard, taskID); err != nil {
			slog.Error("[dispatcher] send wecom question card failed", "index", questionIndex, "error", err)
		}
	}
}

// --- Guidance Card ---

func (d *Dispatcher) sendGuidanceCard(input DispatchInput, wecomUserID string, isMultiselect bool) {
	title := "需要你的操作"
	subTitle := "请点击下方链接在会话中查看详情"

	if isMultiselect {
		subTitle = "由于企业微信控件限制，无法在卡片中完成多选操作，请点击下方链接操作"
	}

	url := input.SessionURL
	if url == "" && input.Path != "" {
		url = fmt.Sprintf("%s%s", d.appURL, input.Path)
	}

	var buttons []wecom.CardButton
	if url != "" {
		buttons = append(buttons, wecom.CardButton{Text: "在会话中查看", Key: "", Style: 1})
	}

	cardData := map[string]any{
		"card_type":      "button_interaction",
		"main_title":     map[string]string{"title": title},
		"sub_title_text": subTitle,
		"button_list":    buttons,
	}

	n, err := d.store.Create(notification.CreateNotificationInput{
		UserID:     input.UserID,
		Type:       input.EventType,
		Title:      title,
		SessionID:  input.SessionID,
		DeviceID:   input.DeviceID,
		ActionType: input.EventType,
		ActionData: mustMarshal(input.ActionData),
		CardData:   mustMarshal(cardData),
	})
	if err != nil {
		slog.Error("[dispatcher] create guidance notification failed", "error", err)
		return
	}

	if len(buttons) > 0 {
		buttons[0].Key = "navigate:" + n.ActionToken
		cardData["button_list"] = buttons
		d.updateNotificationData(n.ActionToken, title, cardData)
	}

	if d.wecomAdapter != nil {
		taskID := fmt.Sprintf("guide_%s_%d", input.SessionID, time.Now().UnixMilli())
		card := wecom.InteractiveCard{
			Title:       title,
			Description: subTitle,
			URL:         url,
			Buttons:     buttons,
		}
		if err := d.wecomAdapter.SendInteractiveCard(nil, wecomUserID, card, taskID); err != nil {
			slog.Error("[dispatcher] send wecom guidance card failed", "error", err)
		}
	}
}

func (d *Dispatcher) sendGuidanceCardWithToken(input DispatchInput, actionToken string, wecomUserID string, isMultiselect bool) {
	title := "需要你的操作"
	subTitle := "请点击下方链接在会话中查看详情"

	if isMultiselect {
		subTitle = "由于企业微信控件限制，无法在卡片中完成多选操作，请点击下方链接操作"
	}

	url := input.SessionURL
	if url == "" && input.Path != "" {
		url = fmt.Sprintf("%s%s", d.appURL, input.Path)
	}

	var buttons []wecom.CardButton
	if url != "" {
		buttons = append(buttons, wecom.CardButton{
			Text:  "在会话中查看",
			Key:   "navigate:" + actionToken,
			Style: 1,
		})
	}

	cardData := map[string]any{
		"card_type":      "button_interaction",
		"main_title":     map[string]string{"title": title},
		"sub_title_text": subTitle,
		"button_list":    buttons,
	}

	d.updateNotificationData(actionToken, title, cardData)

	if d.wecomAdapter != nil {
		taskID := fmt.Sprintf("guide_%s_%d", input.SessionID, time.Now().UnixMilli())
		card := wecom.InteractiveCard{
			Title:       title,
			Description: subTitle,
			URL:         url,
			Buttons:     buttons,
		}
		if err := d.wecomAdapter.SendInteractiveCard(nil, wecomUserID, card, taskID); err != nil {
			slog.Error("[dispatcher] send wecom guidance card failed", "error", err)
		}
	}
}


// --- Session Notice Card (text_notice with jump_list) ---

func (d *Dispatcher) buildSessionURL(input DispatchInput) string {
	if input.SessionURL != "" {
		return input.SessionURL
	}
	if input.Path != "" {
		return fmt.Sprintf("%s%s", d.appURL, input.Path)
	}
	return ""
}

func (d *Dispatcher) sendSessionNoticeCard(input DispatchInput, wecomUserID string, title string, subTitle string) {
	if d.wecomAdapter == nil {
		return
	}
	sessionURL := d.buildSessionURL(input)
	slog.Info("[dispatcher] session notice card", "sessionURL", sessionURL, "input.SessionURL", input.SessionURL, "input.Path", input.Path, "appURL", d.appURL)
	if sessionURL == "" {
		return
	}
	taskID := fmt.Sprintf("notice_%s_%d", input.SessionID, time.Now().UnixMilli())
	card := wecom.TextNoticeCard{
		Title:    title,
		SubTitle: subTitle,
		JumpList: []wecom.TextNoticeJump{
			{Title: "点击跳转到会话页面查看", URL: sessionURL},
		},
	}
	if err := d.wecomAdapter.SendTextNoticeCard(nil, wecomUserID, card, taskID); err != nil {
		slog.Error("[dispatcher] send wecom session notice card failed", "error", err)
	}
}

// --- Helpers ---

func (d *Dispatcher) updateNotificationData(actionToken string, title string, cardData map[string]any) {
	updates := map[string]any{"title": title}
	if cardData != nil {
		updates["card_data"] = mustMarshal(cardData)
	}
	d.db.Model(&models.SystemNotification{}).
		Where("action_token = ?", actionToken).
		Updates(updates)
}

func buildPermissionTitle(input DispatchInput) string {
	title := "权限请求"
	if input.ActionData != nil {
		if toolName, ok := input.ActionData["toolName"].(string); ok {
			title = fmt.Sprintf("权限请求: %s", toolName)
		}
	}
	return title
}

func hasMultipleSelect(questions []questionInfo) bool {
	for _, q := range questions {
		if q.Multiple {
			return true
		}
	}
	return false
}

func buildVoteOptions(opts []questionOption) []wecom.WeComVoteOption {
	options := make([]wecom.WeComVoteOption, len(opts))
	for i, opt := range opts {
		options[i] = wecom.WeComVoteOption{
			ID:   fmt.Sprintf("opt_%d", i),
			Text: opt.Label,
		}
	}
	return options
}

func mapEventTypeToTitle(eventType string) string {
	switch eventType {
	case "session.completed":
		return "会话已完成"
	case "session.failed":
		return "会话失败"
	case "session.aborted":
		return "会话已中断"
	case "permission":
		return "权限请求"
	case "question":
		return "问题"
	case "idle":
		return "空闲超时"
	default:
		return eventType
	}
}

// --- Question Data Extraction ---

type questionInfo struct {
	Question string
	Header   string
	Options  []questionOption
	Multiple bool
	Custom   bool
}

type questionOption struct {
	Label       string
	Description string
}

func extractQuestionInfos(data map[string]any) []questionInfo {
	if data == nil {
		return nil
	}
	questionsVal, ok := data["questions"].([]any)
	if !ok {
		return nil
	}

	var result []questionInfo
	for _, q := range questionsVal {
		qMap, ok := q.(map[string]any)
		if !ok {
			continue
		}

		qi := questionInfo{
			Question: strVal(qMap, "question"),
			Header:   strVal(qMap, "header"),
		}
		if m, ok := qMap["multiple"].(bool); ok {
			qi.Multiple = m
		}
		if c, ok := qMap["custom"].(bool); ok {
			qi.Custom = c
		}

		if optsVal, ok := qMap["options"].([]any); ok {
			for _, o := range optsVal {
				if oMap, ok := o.(map[string]any); ok {
					qi.Options = append(qi.Options, questionOption{
						Label:       strVal(oMap, "label"),
						Description: strVal(oMap, "description"),
					})
				}
			}
		}

		result = append(result, qi)
	}
	return result
}

func strVal(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
