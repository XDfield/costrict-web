package dispatcher

import (
	"encoding/json"
	"fmt"
	"strings"
	"log/slog"
	"time"

	"github.com/costrict/costrict-web/server/internal/channel/adapters/wecom"
	"github.com/costrict/costrict-web/server/internal/gateway"
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
	gwClient        *gateway.Client
	gwRegistry      *gateway.GatewayRegistry
	appURL          string
}

func NewDispatcher(db *gorm.DB, notificationSvc *notification.NotificationService, store *notification.Store, appURL string, wecomAdapter *wecom.WeComAdapter, gwClient *gateway.Client, gwRegistry *gateway.GatewayRegistry) *Dispatcher {
	return &Dispatcher{
		db:              db,
		store:           store,
		notificationSvc: notificationSvc,
		wecomAdapter:    wecomAdapter,
		gwClient:        gwClient,
		gwRegistry:      gwRegistry,
		appURL:          appURL,
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

func (d *Dispatcher) Dispatch(input DispatchInput) {
	if d.store == nil {
		return
	}

	// Auto-accept permission requests when workspace has autoAccept enabled
	if input.EventType == "permission" && d.isAutoAccept(input) {
		slog.Info("[dispatcher] auto-accept enabled, auto-approving permission", "sessionID", input.SessionID)
		d.autoApprovePermission(input)
		return
	}

	// Permission batch: multiple permissions collected in a window
	if input.EventType == "permission_batch" {
		d.dispatchPermissionBatch(input)
		return
	}

	channels := d.selectChannels(input.UserID)
	d.dispatchNow(input, channels)
}

// --- Event Classification ---

func needsInteraction(eventType string) bool {
	return eventType == "permission" || eventType == "question"
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

// --- Permission Card (Button Interaction) ---

func (d *Dispatcher) sendApprovalCard(input DispatchInput, wecomUserID string) {
	info := extractPermissionInfo(input)
	sessionTitle := d.fetchSessionTitle(input)
	if sessionTitle != "" {
		info.Description = fmt.Sprintf("会话「%s」%s", sessionTitle, info.Description)
	}

	buttons := []wecom.CardButton{
		{Text: "批准", Key: "", Style: 1},
		{Text: "拒绝", Key: "", Style: 3},
		{Text: "自批准", Key: "", Style: 2},
	}

	cardData := map[string]any{
		"card_type":  "button_interaction",
		"main_title": map[string]string{"title": info.Title, "desc": info.Description},
		"sub_title_text": info.Command,
		"horizontal_content_list": info.HorizontalItems,
		"button_list": buttons,
	}

	n, err := d.store.Create(notification.CreateNotificationInput{
		UserID:      input.UserID,
		Type:        "permission",
		Title:       info.Title,
		SessionID:   input.SessionID,
		DeviceID:    input.DeviceID,
		WorkspaceID: d.resolveWorkspaceID(input),
		ActionType:  "permission",
		ActionData:  mustMarshal(input.ActionData),
		CardData:    mustMarshal(cardData),
	})
	if err != nil {
		slog.Error("[dispatcher] create permission notification failed", "error", err)
		return
	}

	buttons[0].Key = "approve:" + n.ActionToken
	buttons[1].Key = "reject:" + n.ActionToken
	buttons[2].Key = "auto_approve:" + n.ActionToken
	cardData["button_list"] = buttons
	d.updateNotificationData(n.ActionToken, info.Title, cardData)

	if d.wecomAdapter != nil {
		sessionURL := d.buildSessionURL(input)
		taskID := fmt.Sprintf("perm_%s_%d", input.SessionID, time.Now().UnixMilli())
		card := wecom.InteractiveCard{
			Title:               info.Title,
			Description:         info.Description,
			SubTitle:            info.Command,
			URL:                 sessionURL,
			HorizontalContentList: info.HorizontalItems,
			Buttons:             buttons,
		}
		if err := d.wecomAdapter.SendInteractiveCard(nil, wecomUserID, card, taskID); err != nil {
			slog.Error("[dispatcher] send wecom approval card failed", "error", err)
		}
	}
}

// --- Question Vote Cards ---

// sendVoteCards creates new notifications and sends vote_interaction cards (direct path)
func (d *Dispatcher) sendVoteCards(input DispatchInput, wecomUserID string) {
	questions := extractQuestionInfos(input.ActionData)
	if len(questions) == 0 {
		d.sendGuidanceCard(input, wecomUserID, false)
		return
	}

	// 多题问卷走文本通知卡片
	if len(questions) > 1 {
		slog.Info("[dispatcher] multi-question questionnaire, using text notice card", "questionCount", len(questions))
		d.sendSessionNoticeCard(input, wecomUserID, "会话通知", fmt.Sprintf("有 %d 道问题需要回答，请点击下方链接前往会话操作", len(questions)))
		return
	}

	// Send notice card first, then vote card for single question
	d.sendSessionNoticeCard(input, wecomUserID, "会话通知", "有问题需要回答")
	d.sendSingleVoteCard(input, wecomUserID, questions[0], 0)
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
		"main_title":    map[string]string{"title": title, "desc": q.Question},
		"checkbox":      checkbox,
		"submit_button": submitBtn,
	}

	actionData := make(map[string]any)
	for k, v := range input.ActionData {
		actionData[k] = v
	}
	actionData["questionIndex"] = questionIndex

	n, err := d.store.Create(notification.CreateNotificationInput{
		UserID:      input.UserID,
		Type:        "question",
		Title:       title,
		SessionID:   input.SessionID,
		DeviceID:    input.DeviceID,
		WorkspaceID: d.resolveWorkspaceID(input),
		ActionType:  "question",
		ActionData:  mustMarshal(actionData),
		CardData:    mustMarshal(cardData),
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
		UserID:      input.UserID,
		Type:        input.EventType,
		Title:       title,
		SessionID:   input.SessionID,
		DeviceID:    input.DeviceID,
		WorkspaceID: d.resolveWorkspaceID(input),
		ActionType:  input.EventType,
		ActionData:  mustMarshal(input.ActionData),
		CardData:    mustMarshal(cardData),
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

// --- Session Notice Card (text_notice with jump_list) ---

func (d *Dispatcher) isAutoAccept(input DispatchInput) bool {
	if input.Path == "" || input.DeviceID == "" {
		return false
	}
	normalizedPath := strings.ReplaceAll(input.Path, "\\", "/")
	var dev models.Device
	if err := d.db.Where("device_id = ?", input.DeviceID).First(&dev).Error; err != nil {
		return false
	}
	var ws models.Workspace
	if err := d.db.
		Joins("JOIN workspace_directories ON workspace_directories.workspace_id = workspaces.id").
		Where("workspaces.user_id = ? AND workspaces.device_id = ?", input.UserID, dev.ID).
		Where("REPLACE(workspace_directories.path, chr(92), chr(47)) = ?", normalizedPath).
		Where("workspace_directories.deleted_at IS NULL").
		First(&ws).Error; err != nil {
		return false
	}
	var settings map[string]any
	if ws.Settings != nil {
		json.Unmarshal(ws.Settings, &settings)
	}
	if v, ok := settings["autoAccept"]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

func (d *Dispatcher) autoApprovePermission(input DispatchInput) {
	if d.gwClient == nil || d.gwRegistry == nil {
		return
	}
	if input.ActionData == nil {
		return
	}
	id, _ := input.ActionData["id"].(string)
	if id == "" {
		return
	}
	proxyPath := fmt.Sprintf("/api/v1/permissions/%s/reply", id)
	bodyBytes, _ := json.Marshal(map[string]any{"approved": true})
	var result map[string]any
	directory := input.Path
	if err := gateway.ProxyDeviceSessionRequest(d.gwClient, d.gwRegistry, input.UserID, input.DeviceID, directory, "POST", proxyPath, bodyBytes, &result); err != nil {
		slog.Error("[dispatcher] auto-approve permission failed", "error", err)
		return
	}
	slog.Info("[dispatcher] auto-approved permission", "sessionID", input.SessionID, "permissionID", id)
}


// dispatchPermissionBatch handles a batch of permission requests collected by cs-cloud.
// If autoAccept is enabled: approve all permissions via gateway proxy.
// If autoAccept is disabled: send a single guidance card (text_notice).
func (d *Dispatcher) dispatchPermissionBatch(input DispatchInput) {
	if d.isAutoAccept(input) {
		slog.Info("[dispatcher] auto-accept enabled, batch-approving permissions", "sessionID", input.SessionID)
		d.batchApprovePermissions(input)
		return
	}

	wecomUserID := d.resolveWeComUserID(input.UserID)
	if wecomUserID == "" {
		slog.Error("[dispatcher] cannot resolve wecom user id for permission batch", "appUserID", input.UserID)
		return
	}

	count := 0
	if perms, ok := input.ActionData["permissions"].([]any); ok {
		count = len(perms)
	}
	subTitle := fmt.Sprintf("有 %d 个权限请求待处理，请点击下方链接前往会话操作", count)
	d.sendSessionNoticeCard(input, wecomUserID, "权限请求", subTitle)
}

// batchApprovePermissions approves all permissions in a batch via gateway proxy.
func (d *Dispatcher) batchApprovePermissions(input DispatchInput) {
	if d.gwClient == nil || d.gwRegistry == nil {
		return
	}
	if input.ActionData == nil {
		return
	}
	perms, ok := input.ActionData["permissions"].([]any)
	if !ok {
		return
	}
	approved := 0
	for _, p := range perms {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		proxyPath := fmt.Sprintf("/api/v1/permissions/%s/reply", id)
		bodyBytes, _ := json.Marshal(map[string]any{"approved": true})
		var result map[string]any
		if err := gateway.ProxyDeviceSessionRequest(d.gwClient, d.gwRegistry, input.UserID, input.DeviceID, input.Path, "POST", proxyPath, bodyBytes, &result); err != nil {
			slog.Error("[dispatcher] batch auto-approve permission failed", "permissionID", id, "error", err)
			continue
		}
		approved++
	}
	slog.Info("[dispatcher] batch auto-approved permissions", "sessionID", input.SessionID, "total", len(perms), "approved", approved)
}
// resolveWorkspaceID returns the workspace ID for the given input by looking up
// userID + deviceUUID + path. Returns empty string if not found.
func (d *Dispatcher) resolveWorkspaceID(input DispatchInput) string {
	if input.Path == "" || input.DeviceID == "" {
		return ""
	}
	normalizedPath := strings.ReplaceAll(input.Path, "\\", "/")
	var dev models.Device
	if err := d.db.Where("device_id = ?", input.DeviceID).First(&dev).Error; err != nil {
		return ""
	}
	var ws models.Workspace
	if err := d.db.
		Joins("JOIN workspace_directories ON workspace_directories.workspace_id = workspaces.id").
		Where("workspaces.user_id = ? AND workspaces.device_id = ?", input.UserID, dev.ID).
		Where("REPLACE(workspace_directories.path, chr(92), chr(47)) = ?", normalizedPath).
		Where("workspace_directories.deleted_at IS NULL").
		First(&ws).Error; err != nil {
		return ""
	}
	return ws.ID
}

func (d *Dispatcher) buildSessionURL(input DispatchInput) string {
	if input.SessionURL != "" {
		return input.SessionURL
	}
	wsID := d.resolveWorkspaceID(input)
	if wsID == "" {
		return ""
	}
	return fmt.Sprintf("%s/m/workspace/%s/?session=%s", d.appURL, wsID, input.SessionID)
}

func (d *Dispatcher) fetchSessionTitle(input DispatchInput) string {
	if d.gwClient == nil || d.gwRegistry == nil {
		return ""
	}
	if input.DeviceID == "" || input.SessionID == "" {
		return ""
	}
	directory := input.Path
	proxyPath := fmt.Sprintf("/api/v1/conversations/%s", input.SessionID)
	var result map[string]any
	if err := gateway.ProxyDeviceSessionRequest(d.gwClient, d.gwRegistry, input.UserID, input.DeviceID, directory, "GET", proxyPath, nil, &result); err != nil {
		slog.Warn("[dispatcher] failed to fetch session title", "sessionID", input.SessionID, "error", err)
		return ""
	}
	if title, ok := result["title"].(string); ok && title != "" {
		slog.Info("[dispatcher] fetched session title", "sessionID", input.SessionID, "title", title)
		return title
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

	// Fetch session title for the subtitle
	sessionTitle := d.fetchSessionTitle(input)
	cardSubTitle := subTitle
	if sessionTitle != "" {
		cardSubTitle = fmt.Sprintf("会话「%s」%s", sessionTitle, subTitle)
	}

	taskID := fmt.Sprintf("notice_%s_%d", input.SessionID, time.Now().UnixMilli())
	card := wecom.TextNoticeCard{
		Title:    title,
		SubTitle: cardSubTitle,
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

type permissionInfo struct {
	Title           string
	Description     string
	Command         string
	HorizontalItems []wecom.HorizontalContentItem
}

func extractPermissionInfo(input DispatchInput) permissionInfo {
	info := permissionInfo{Title: "权限请求"}
	if input.ActionData == nil {
		return info
	}

	// Extract permission type (e.g. "bash")
	permType, _ := input.ActionData["permission"].(string)
	if permType != "" {
		info.Title = fmt.Sprintf("权限请求: %s", permType)
		info.Description = fmt.Sprintf("请求使用 %s 权限", permType)
	}

	// Extract command from patterns or metadata
	if patterns, ok := input.ActionData["patterns"].([]any); ok && len(patterns) > 0 {
		if cmd, ok := patterns[0].(string); ok {
			info.Command = cmd
		}
	}
	if metadata, ok := input.ActionData["metadata"].(map[string]any); ok {
		if inputField, ok := metadata["input"].(map[string]any); ok {
			if cmd, ok := inputField["command"].(string); ok && cmd != "" {
				info.Command = cmd
			}
		}
	}

	// Build horizontal content items
	info.HorizontalItems = []wecom.HorizontalContentItem{
		{KeyName: "权限类型", Value: permType},
	}
	if info.Command != "" {
		// Truncate long commands for display
		cmdDisplay := info.Command
		if len([]rune(cmdDisplay)) > 40 {
			cmdDisplay = string([]rune(cmdDisplay)[:40]) + "..."
		}
		info.HorizontalItems = append(info.HorizontalItems, wecom.HorizontalContentItem{
			KeyName: "执行命令", Value: cmdDisplay,
		})
	}

	return info
}

func buildPermissionTitle(input DispatchInput) string {
	return extractPermissionInfo(input).Title
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
		text := opt.Label
		if opt.Description != "" {
			text = fmt.Sprintf("%s. %s", opt.Label, opt.Description)
		}
		options[i] = wecom.WeComVoteOption{
			ID:   fmt.Sprintf("opt_%d", i),
			Text: text,
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
