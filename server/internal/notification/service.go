package notification

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/notification/sender"
	"github.com/costrict/costrict-web/server/internal/pathutil"
	"gorm.io/gorm"
)

type NotificationService struct {
	db           *gorm.DB
	cloudBaseURL string
}

func NewNotificationService(db *gorm.DB, cloudBaseURL string) *NotificationService {
	sender.Register(sender.NewWeComSender())
	sender.Register(sender.NewWebhookSender())
	return &NotificationService{db: db, cloudBaseURL: cloudBaseURL}
}

type sessionInfo struct {
	eventType   string
	sessionID   string
	deviceID    string
	path        string
	workspaceID string
}

func (s *NotificationService) TriggerNotifications(userID, eventType, sessionID, deviceID, path string) {
	go func() {
		info := sessionInfo{
			eventType: eventType,
			sessionID: sessionID,
			deviceID:  deviceID,
			path:      path,
		}
		workspaceID, err := s.getWorkspaceID(deviceID, path)
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
//
// Recipient resolution runs synchronously (so the caller gets an accurate count
// and any scope error), but the per-user writes are batched on a background
// goroutine to keep the request fast for large organizations. When pushExternal
// is set we always go to the background, because external channel delivery can
// block the request thread on slow third-party endpoints.
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

	// External pushes can block on slow third-party endpoints, so they always go
	// to the background. In-app-only broadcasts to small audiences deliver inline
	// (keeping tests/synchronous callers simple); larger ones go to the background
	// so the HTTP response (with the recipient count) returns promptly.
	if pushExternal || len(recipients) > 50 {
		go deliver()
	} else {
		deliver()
	}

	return len(recipients), nil
}

// getWorkspaceID 根据设备标识符和路径查找工作空间ID。
// 注意：传入的 deviceID 是 devices.device_id（外部设备标识符），
// 而 workspaces.device_id 存储的是 devices.id（UUID主键），
// 因此需要先 JOIN devices 表进行转换，不能直接用 deviceID 匹配 workspaces.device_id。
func (s *NotificationService) getWorkspaceID(deviceID, path string) (string, error) {
	var workspaceID string
	normalizedPath := pathutil.NormalizeWorkspacePath(path)
	err := s.db.Table("workspace_directories wd").
		Select("w.id").
		Joins("JOIN workspaces w ON w.id = wd.workspace_id").
		Joins("JOIN devices d ON CAST(d.id AS TEXT) = w.device_id").
		Where("wd.path = ? AND d.device_id = ?", normalizedPath, deviceID).
		Where("wd.deleted_at IS NULL AND w.deleted_at IS NULL AND d.deleted_at IS NULL").
		Scan(&workspaceID).Error
	if err == nil && workspaceID == "" && normalizedPath != path {
		err = s.db.Table("workspace_directories wd").
			Select("w.id").
			Joins("JOIN workspaces w ON w.id = wd.workspace_id").
			Joins("JOIN devices d ON CAST(d.id AS TEXT) = w.device_id").
			Where("wd.path = ? AND d.device_id = ?", path, deviceID).
			Where("wd.deleted_at IS NULL AND w.deleted_at IS NULL AND d.deleted_at IS NULL").
			Scan(&workspaceID).Error
	}
	if err != nil {
		return "", err
	}
	if workspaceID == "" {
		return "", fmt.Errorf("workspace not found for deviceID=%s, path=%s", deviceID, normalizedPath)
	}
	return workspaceID, nil
}

func (s *NotificationService) send(userID, eventType string, msg sender.NotificationMessage) {
	var channels []models.UserNotificationChannel
	s.db.Where(
		"user_id = ? AND enabled = true AND ? = ANY(trigger_events) AND deleted_at IS NULL",
		userID, eventType,
	).Find(&channels)

	if len(channels) == 0 {
		slog.Info("no notification channels found", "userID", userID, "eventType", eventType)
		return
	}

	for _, ch := range channels {
		snd, ok := sender.Get(ch.ChannelType)
		if !ok {
			continue
		}

		sentAt := time.Now()
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
	var systemChannels []models.SystemNotificationChannel
	s.db.Where("enabled = true AND deleted_at IS NULL").Find(&systemChannels)

	result := make([]map[string]any, 0, len(systemChannels)+1)
	seen := map[string]bool{}

	for _, sc := range systemChannels {
		if seen[sc.Type] {
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

	if webhookSnd, ok := sender.Get("webhook"); ok && !seen["webhook"] {
		result = append(result, map[string]any{
			"systemChannelId": "",
			"type":            "webhook",
			"name":            "自定义 Webhook",
			"schema":          webhookSnd.UserConfigSchema(),
		})
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
	titles := map[string]string{
		"session.completed": "会话执行完成",
		"session.failed":    "会话执行失败",
		"session.aborted":   "会话已中止",
		"device.offline":    "设备已离线",
		"permission":        "需要授权确认",
		"question":          "需要回答问题",
		"idle":              "会话等待中",
	}

	title, ok := titles[info.eventType]
	if !ok {
		title = "CoStrict 通知"
	}

	sessionURL := ""
	if s.cloudBaseURL != "" && info.workspaceID != "" && info.sessionID != "" {
		pathID := base64.RawURLEncoding.EncodeToString([]byte(info.path))
		sessionURL = fmt.Sprintf("%s/workspace/%s/%s/session/%s", s.cloudBaseURL, info.workspaceID, pathID, info.sessionID)
	} else {
		slog.Warn("sessionURL assembly failed.", "workspaceID", info.workspaceID, "sessionID", info.sessionID)
		sessionURL = s.cloudBaseURL
	}

	return sender.NotificationMessage{
		Title:     title,
		Body:      fmt.Sprintf("**状态更新:** <font color=\"info\">%s</font>\n> **设备**: %s\n**会话**: %s", title, info.deviceID, info.sessionID),
		EventType: info.eventType,
		SessionID: info.sessionID,
		DeviceID:  info.deviceID,
		Metadata:  map[string]any{"status": info.eventType, "sessionUrl": sessionURL},
	}
}
