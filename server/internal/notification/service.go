package notification

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/notification/sender"
	"github.com/costrict/costrict-web/server/internal/sessionurl"
	"github.com/lib/pq"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type NotificationService struct {
	db           *gorm.DB
	cloudBaseURL string
	// System-level channel availability controls
	webhookEnabled  bool
	weComEnabled    bool
	weComBotEnabled bool

}

func NewNotificationService(db *gorm.DB, cloudBaseURL string, webhookEnabled, weComEnabled, weComBotEnabled bool, wecomBotProxyURL, wecomBotAuthToken string) *NotificationService {
	sender.Register(sender.NewWeComSender())
	sender.Register(sender.NewWebhookSender())
	sender.Register(sender.NewWeComBotSender(wecomBotProxyURL, wecomBotAuthToken))
	return &NotificationService{
		db:              db,
		cloudBaseURL:    cloudBaseURL,
		webhookEnabled:  webhookEnabled,
		weComEnabled:    weComEnabled,
		weComBotEnabled: weComBotEnabled,
	}
}


// resolveWeComUserID resolves the platform user UUID to a WeChat Work userId
// by querying the UserAuthIdentity table for an idtrust binding.
func (s *NotificationService) resolveWeComUserID(appUserID string) string {
	var identity models.UserAuthIdentity
	if err := s.db.Where("user_subject_id = ? AND provider = ? AND deleted_at IS NULL", appUserID, "idtrust").
		First(&identity).Error; err != nil {
		return ""
	}
	if identity.ProviderUserID != nil {
		return *identity.ProviderUserID
	}
	return ""
}

// ensureWeComBotChannel ensures that the user has a wecom-bot notification channel
// If it doesn't exist, it creates one with default configuration
func (s *NotificationService) ensureWeComBotChannel(userID string) error {
	// Check if user already has a wecom-bot channel
	var existingChannel models.UserNotificationChannel
	err := s.db.Where("user_id = ? AND channel_type = ? AND deleted_at IS NULL", userID, "wecom-bot").
		First(&existingChannel).Error

	if err == nil {
		// Channel already exists, no need to create
		return nil
	}

	if err != gorm.ErrRecordNotFound {
		// Database error
		return err
	}

	// Channel doesn't exist, create default wecom-bot channel
	channel := models.UserNotificationChannel{
		UserID:        userID,
		ChannelType:   "wecom-bot",
		Name:          "企微机器人",
		Enabled:       true,
		UserConfig:    datatypes.JSON(`{"enabled":true}`),
		TriggerEvents: pq.StringArray{"permission", "question", "idle"},
		// SystemChannelID is left empty (zero value) for wecom-bot
	}

	if err := s.db.Create(&channel).Error; err != nil {
		return err
	}

	return nil
}

type sessionInfo struct {
	eventType   string
	sessionID   string
	deviceID    string
	path        string
	workspaceID string
	actionData  map[string]any
}

func (s *NotificationService) TriggerNotifications(userID, eventType, sessionID, deviceID, path string, actionData map[string]any) {
	go func() {
		info := sessionInfo{
			eventType:  eventType,
			sessionID:  sessionID,
			deviceID:   deviceID,
			path:       path,
			actionData: actionData,
		}
		workspaceID, err := sessionurl.ResolveWorkspaceID(s.db, deviceID, path)
		if err != nil {
			slog.Error(
				"failed to get workspaceID", "userID", userID,
				"deviceID", deviceID, "path", path, "error", err,
			)
		}
		info.workspaceID = workspaceID


		msg := s.buildMessage(info)
		s.send(userID, eventType, msg)
	}()
}


func (s *NotificationService) TriggerMessage(userID, eventType string, msg sender.NotificationMessage) {
	go s.send(userID, eventType, msg)
}

// BroadcastScope selects the recipients of an announcement.
//   - "all":          every user with a subject_id
//   - "organization": every user whose organization == TargetID
//   - "user":         the single user TargetID
type BroadcastScope struct {
	Type     string
	TargetID string
}

// resolveBroadcastRecipients expands a BroadcastScope into a list of recipient
// subject_ids. Mirrors the distribution service's org→subject_id expansion.
func (s *NotificationService) resolveBroadcastRecipients(scope BroadcastScope) ([]string, error) {
	switch scope.Type {
	case "user":
		if scope.TargetID == "" {
			return nil, fmt.Errorf("targetId is required for user scope")
		}
		return []string{scope.TargetID}, nil
	case "organization":
		if scope.TargetID == "" {
			return nil, fmt.Errorf("targetId is required for organization scope")
		}
		var userIDs []string
		if err := s.db.Model(&models.User{}).
			Where("organization = ? AND subject_id <> ''", scope.TargetID).
			Pluck("subject_id", &userIDs).Error; err != nil {
			return nil, err
		}
		return userIDs, nil
	case "all":
		var userIDs []string
		if err := s.db.Model(&models.User{}).
			Where("subject_id <> ''").
			Pluck("subject_id", &userIDs).Error; err != nil {
			return nil, err
		}
		return userIDs, nil
	default:
		return nil, fmt.Errorf("unsupported broadcast scope: %s", scope.Type)
	}
}

// Broadcast sends an in-app announcement to every user matched by scope. For
// each recipient it writes one per-user system_notification (in-app inbox) and,
// when pushExternal is set, also fires the user's configured external channels
// via the standard send path. Returns the number of recipients reached.
func (s *NotificationService) Broadcast(scope BroadcastScope, title, content string, pushExternal bool, operatorID string) (int, error) {
	recipients, err := s.resolveBroadcastRecipients(scope)
	if err != nil {
		return 0, err
	}
	if len(recipients) == 0 {
		return 0, nil
	}

	store := NewStore(s.db)

	deliver := func() {
		for _, uid := range recipients {
			if _, err := store.Create(CreateNotificationInput{
				UserID:  uid,
				Type:    EventSystemNotification,
				Title:   title,
				Content: content,
			}); err != nil {
				slog.Warn("broadcast: failed to create in-app notification", "userID", uid, "error", err)
				continue
			}
			if pushExternal {
				s.send(uid, EventSystemNotification, sender.NotificationMessage{
					Title:     title,
					Body:      content,
					EventType: EventSystemNotification,
				})
			}
		}
	}

	if pushExternal || len(recipients) > 50 {
		go deliver()
	} else {
		deliver()
	}

	return len(recipients), nil
}

func (s *NotificationService) send(userID, eventType string, msg sender.NotificationMessage) {
	slog.Info("[notify:send] entering send", "userID", userID, "eventType", eventType, "sessionID", msg.SessionID)

	var channels []models.UserNotificationChannel
	s.db.Where(
		"user_id = ? AND enabled = true AND ? = ANY(trigger_events) AND deleted_at IS NULL",
		userID, eventType,
	).Find(&channels)

	slog.Info("[notify:send] channel query result", "userID", userID, "eventType", eventType, "count", len(channels))

	if len(channels) == 0 {
		slog.Info("no notification channels found", "userID", userID, "eventType", eventType)
		return
	}

	for _, ch := range channels {
		// Check system-level channel availability control
		var isSystemEnabled bool
		switch ch.ChannelType {
		case "webhook":
			isSystemEnabled = s.webhookEnabled
		case "wecom":
			isSystemEnabled = s.weComEnabled
		case "wecom-bot":
			isSystemEnabled = s.weComBotEnabled
		default:
			// For unknown channel types, assume disabled
			isSystemEnabled = false
		}

		if !isSystemEnabled {
			slog.Info("channel type disabled at system level, skipping",
				"userID", userID, "channelType", ch.ChannelType, "eventType", eventType)
			continue
		}

		snd, ok := sender.Get(ch.ChannelType)
		if !ok {
			continue
		}

		sentAt := time.Now()
		msg.UserID = userID

		// For wecom-bot, resolve platform UUID to WeChat Work userId
		if ch.ChannelType == "wecom-bot" {
			resolvedID := s.resolveWeComUserID(userID)
			if resolvedID == "" {
				slog.Error("[notify:send] failed to resolve wecom user id, skipping",
					"userID", userID, "channelType", ch.ChannelType)
				continue
			}
			msg.UserID = resolvedID
			slog.Info("[notify:send] resolved wecom user id",
				"platformUserID", userID, "wecomUserID", resolvedID)
		}

		err := snd.Send(json.RawMessage(ch.UserConfig), msg)

		logEntry := models.NotificationLog{
			UserChannelID: ch.ID,
			UserID:        userID,
			ChannelType:   ch.ChannelType,
			EventType:     eventType,
			SessionID:     msg.SessionID,
			DeviceID:      msg.DeviceID,
			SentAt:        &sentAt,
		}

		if err != nil {
			logEntry.Status = "failed"
			logEntry.Error = err.Error()
			s.db.Model(&ch).Update("last_error", err.Error())
		} else {
			logEntry.Status = "success"
			s.db.Model(&ch).Updates(map[string]any{
				"last_used_at": sentAt,
				"last_error":   "",
			})
		}

		s.db.Create(&logEntry)
	}
}

func (s *NotificationService) SendTest(userChannelID, userID string) error {
	var ch models.UserNotificationChannel
	if err := s.db.Where("id = ? AND user_id = ? AND deleted_at IS NULL", userChannelID, userID).
		First(&ch).Error; err != nil {
		return fmt.Errorf("notification channel not found")
	}

	snd, ok := sender.Get(ch.ChannelType)
	if !ok {
		return fmt.Errorf("unsupported channel type: %s", ch.ChannelType)
	}

	msg := sender.NotificationMessage{
		Title:     "测试通知",
		Body:      "这是一条来自 CoStrict 的测试通知",
		EventType: "test",
		UserID:    userID,
	}

	return snd.Send(json.RawMessage(ch.UserConfig), msg)
}

func (s *NotificationService) ListLogs(userChannelID, userID string, limit int) ([]models.NotificationLog, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	var ch models.UserNotificationChannel
	if err := s.db.Where("id = ? AND user_id = ? AND deleted_at IS NULL", userChannelID, userID).
		First(&ch).Error; err != nil {
		return nil, fmt.Errorf("notification channel not found")
	}

	var logs []models.NotificationLog
	if err := s.db.Where("user_channel_id = ?", userChannelID).
		Order("created_at DESC").
		Limit(limit).
		Find(&logs).Error; err != nil {
		return nil, err
	}

	return logs, nil
}

func (s *NotificationService) GetAvailableChannelTypes() []map[string]any {
	// Map of system-level availability controls
	channelEnabled := map[string]bool{
		"webhook":    s.webhookEnabled,
		"wecom":      s.weComEnabled,
		"wecom-bot":  s.weComBotEnabled,
	}

	var systemChannels []models.SystemNotificationChannel
	s.db.Where("enabled = true AND deleted_at IS NULL").Find(&systemChannels)

	result := make([]map[string]any, 0, len(systemChannels)+1)
	seen := map[string]bool{}

	// First, add system channels that are both enabled and have config
	for _, sc := range systemChannels {
		if seen[sc.Type] {
			continue
		}
		// Check system-level availability
		if enabled, ok := channelEnabled[sc.Type]; !ok || !enabled {
			continue
		}
		seen[sc.Type] = true

		snd, ok := sender.Get(sc.Type)
		if !ok {
			continue
		}

		result = append(result, map[string]any{
			"systemChannelId": sc.ID,
			"type":            sc.Type,
			"name":            sc.Name,
			"schema":          snd.UserConfigSchema(),
		})
	}

	// Then, add built-in channels that are enabled but not seen in system channels
	for channelType, enabled := range channelEnabled {
		if !enabled || seen[channelType] {
			continue
		}

		snd, ok := sender.Get(channelType)
		if !ok {
			continue
		}

		// Use default names for built-in channels
		name := map[string]string{
			"webhook":   "自定义 Webhook",
			"wecom":     "企微应用通知",
			"wecom-bot": "企微机器人",
		}[channelType]

		result = append(result, map[string]any{
			"systemChannelId": "",
			"type":            channelType,
			"name":            name,
			"schema":          snd.UserConfigSchema(),
		})
		seen[channelType] = true
	}

	return result
}

func (s *NotificationService) GetSupportedTriggerEvents() []string {
	return []string{
		EventSessionCompleted,
		EventSessionFailed,
		EventSessionAborted,
		EventDeviceOffline,
		EventPermissionRequired,
		EventQuestionRequired,
		EventSessionIdle,
		EventProjectInvitationCreated,
		EventSystemNotification,
		EventItemDistributed,
		EventItemRevoked,
		EventItemPaused,
	}
}

func (s *NotificationService) IsSupportedTriggerEvent(event string) bool {
	for _, supported := range s.GetSupportedTriggerEvents() {
		if supported == event {
			return true
		}
	}
	return false
}

func (s *NotificationService) buildMessage(info sessionInfo) sender.NotificationMessage {
	// Build body from actionData for permission/question events
	var title string
	var bodyParts []string

	switch info.eventType {
	case "permission":
		title = "需要授权确认"
		if info.actionData != nil {
			permType, _ := info.actionData["permission"].(string)
			cmd := extractCommand(info.actionData)
			if permType != "" {
				title = fmt.Sprintf("权限请求: %s", permType)
			}
			bodyParts = append(bodyParts, fmt.Sprintf("**请求**: %s", title))
			if cmd != "" {
				bodyParts = append(bodyParts, fmt.Sprintf("**命令**: %s", cmd))
			}
		} else {
			bodyParts = append(bodyParts, fmt.Sprintf("**请求**: %s", title))
		}

	case "question":
		title = "需要回答问题"
		bodyParts = append(bodyParts, fmt.Sprintf("**请求**: %s", title))
		if info.actionData != nil {
			if questions, ok := info.actionData["questions"].([]any); ok && len(questions) > 0 {
				if q, ok := questions[0].(map[string]any); ok {
					if qText, _ := q["question"].(string); qText != "" {
						bodyParts = append(bodyParts, fmt.Sprintf("> %s", qText))
					}
				}
			}
		}

	case "idle":
		title = "会话等待中"
		bodyParts = append(bodyParts, "**会话等待中**")

	default:
		titles := map[string]string{
			"session.completed": "会话执行完成",
			"session.failed":    "会话执行失败",
			"session.aborted":   "会话已中止",
			"device.offline":    "设备已离线",
		}
		title = titles[info.eventType]
		if title == "" {
			title = "CoStrict 通知"
		}
		bodyParts = append(bodyParts, fmt.Sprintf("**状态更新**: %s", title))
	}

	sessionURL := sessionurl.Build(s.cloudBaseURL, info.workspaceID, info.sessionID)
	if sessionURL == "" {
		slog.Warn("sessionURL assembly failed.", "workspaceID", info.workspaceID, "sessionID", info.sessionID)
		sessionURL = s.cloudBaseURL
	}

	body := strings.Join(bodyParts, "\n")

	return sender.NotificationMessage{
		Title:     title,
		Body:      body,
		EventType: info.eventType,
		SessionID: info.sessionID,
		DeviceID:  info.deviceID,
		Metadata:  map[string]any{"status": info.eventType, "sessionUrl": sessionURL},
	}
}

// extractCommand extracts the command/pattern from permission actionData
func extractCommand(data map[string]any) string {
	// Try patterns first (array of strings)
	if patterns, ok := data["patterns"].([]any); ok && len(patterns) > 0 {
		if cmd, ok := patterns[0].(string); ok && cmd != "" {
			return cmd
		}
	}
	// Try metadata.input.command
	if metadata, ok := data["metadata"].(map[string]any); ok {
		if input, ok := metadata["input"].(map[string]any); ok {
			if cmd, ok := input["command"].(string); ok && cmd != "" {
				return cmd
			}
		}
	}
	return ""
}
