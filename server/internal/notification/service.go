package notification

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/notification/sender"
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

func (s *NotificationService) TriggerNotifications(userID, eventType, sessionID, deviceID string) {
	go func() {
		msg := buildMessage(eventType, sessionID, deviceID, s.cloudBaseURL)
		s.send(userID, eventType, msg)
	}()
}

func (s *NotificationService) send(userID, eventType string, msg sender.NotificationMessage) {
	var channels []models.UserNotificationChannel
	s.db.Where(
		"user_id = ? AND enabled = true AND ? = ANY(trigger_events) AND deleted_at IS NULL",
		userID, eventType,
	).Find(&channels)

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

func buildMessage(eventType, sessionID, deviceID, cloudBaseURL string) sender.NotificationMessage {
	titles := map[string]string{
		"session.completed": "会话执行完成",
		"session.failed":    "会话执行失败",
		"session.aborted":   "会话已中止",
		"device.offline":    "设备已离线",
	}

	title, ok := titles[eventType]
	if !ok {
		title = "CoStrict 通知"
	}

	sessionURL := ""
	if cloudBaseURL != "" && deviceID != "" && sessionID != "" {
		sessionURL = fmt.Sprintf("%s/devices/%s/sessions/%s", cloudBaseURL, deviceID, sessionID)
	}

	return sender.NotificationMessage{
		Title:     title,
		Body:      fmt.Sprintf("设备 %s 的会话 %s 状态更新：%s", deviceID, sessionID, title),
		EventType: eventType,
		SessionID: sessionID,
		DeviceID:  deviceID,
		Metadata:  map[string]any{"status": eventType, "sessionUrl": sessionURL},
	}
}
