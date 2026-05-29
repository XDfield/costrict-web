package notification

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

type Store struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

type CreateNotificationInput struct {
	UserID      string
	Type        string
	Title       string
	Content     string
	SessionID   string
	DeviceID    string
	WorkspaceID string
	ActionType  string
	ActionData  []byte
	CardData    []byte
}

func (s *Store) Create(input CreateNotificationInput) (*models.SystemNotification, error) {
	token, err := generateActionToken()
	if err != nil {
		return nil, err
	}

	expiresAt := time.Now().Add(30 * time.Minute)

	n := &models.SystemNotification{
		UserID:      input.UserID,
		Type:        input.Type,
		Status:      "pending",
		Title:       input.Title,
		Content:     input.Content,
		SessionID:   input.SessionID,
		DeviceID:    input.DeviceID,
		ActionType:  input.ActionType,
		ActionToken: token,
		ExpiresAt:   &expiresAt,
	}
	if input.WorkspaceID != "" {
		n.WorkspaceID = &input.WorkspaceID
	}

	if input.ActionData != nil {
		n.ActionData = input.ActionData
	}
	if input.CardData != nil {
		n.CardData = input.CardData
	}

	if err := s.db.Create(n).Error; err != nil {
		return nil, err
	}
	return n, nil
}

func (s *Store) ExecuteAction(token string, result map[string]any) (*models.SystemNotification, error) {
	var n models.SystemNotification
	if err := s.db.Where("action_token = ? AND deleted_at IS NULL AND status = 'pending'", token).First(&n).Error; err != nil {
		return nil, err
	}

	// 被动过期检查：拒绝已过期 token
	if n.ExpiresAt != nil && n.ExpiresAt.Before(time.Now()) {
		s.db.Model(&n).Update("status", "expired")
		return nil, gorm.ErrRecordNotFound
	}

	now := time.Now()
	updates := map[string]any{
		"status":   "acted",
		"acted_at": now,
	}
	if result != nil {
		resultJSON, _ := json.Marshal(result)
		updates["action_result"] = resultJSON
	}

	if err := s.db.Model(&n).Updates(updates).Error; err != nil {
		return nil, err
	}
	n.Status = "acted"
	n.ActedAt = &now

	return &n, nil
}

func (s *Store) MarkRespondedBySession(sessionID string) error {
	now := time.Now()
	return s.db.Model(&models.SystemNotification{}).
		Where("session_id = ? AND status = 'pending' AND deleted_at IS NULL", sessionID).
		Updates(map[string]any{
			"status":   "acted",
			"acted_at": now,
		}).Error
}

func (s *Store) GetByToken(token string) (*models.SystemNotification, error) {
	var n models.SystemNotification
	if err := s.db.Where("action_token = ? AND deleted_at IS NULL", token).First(&n).Error; err != nil {
		return nil, err
	}
	return &n, nil
}

func (s *Store) GetPendingByUser(userID string) ([]models.SystemNotification, error) {
	var notifications []models.SystemNotification
	if err := s.db.Where("user_id = ? AND status = 'pending' AND deleted_at IS NULL", userID).
		Order("created_at DESC").
		Find(&notifications).Error; err != nil {
		return nil, err
	}
	return notifications, nil
}

func (s *Store) MarkRead(id string) error {
	now := time.Now()
	return s.db.Model(&models.SystemNotification{}).
		Where("id = ? AND deleted_at IS NULL", id).
		Updates(map[string]any{"status": "read", "read_at": now}).Error
}

// MarkExpired 批量过期清理：将超过 expires_at 的 pending 记录标记为 expired
func (s *Store) MarkExpired() {
	threshold := time.Now()
	result := s.db.Model(&models.SystemNotification{}).
		Where("status = 'pending' AND expires_at IS NOT NULL AND expires_at < ? AND deleted_at IS NULL", threshold).
		Update("status", "expired")

	if result.RowsAffected > 0 {
		slog.Info("[notification-sweep] expired notifications", "count", result.RowsAffected)
	}
}

// SweepStaleNotifications 扫描超过 staleThreshold 的 pending 记录，供 worker 兜底补发用
func (s *Store) SweepStaleNotifications(staleThreshold time.Duration) ([]models.SystemNotification, error) {
	cutoff := time.Now().Add(-staleThreshold)
	var stale []models.SystemNotification
	if err := s.db.Where(
		"status = 'pending' AND created_at < ? AND deleted_at IS NULL", cutoff,
	).Find(&stale).Error; err != nil {
		return nil, err
	}
	return stale, nil
}

// StaleDispatcher 兜底分发接口，避免 notification → dispatcher 循环依赖
type StaleDispatcher interface {
	DispatchStaleNotification(n models.SystemNotification)
}

// StartSweep 定时清理入口：合并过期清理 + 缓冲兜底
func (s *Store) StartSweep(ctx context.Context, disp StaleDispatcher) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.MarkExpired()

			if disp != nil {
				stale, err := s.SweepStaleNotifications(120 * time.Second)
				if err != nil {
					slog.Error("[notification-sweep] sweep stale failed", "error", err)
					continue
				}
				for _, n := range stale {
					disp.DispatchStaleNotification(n)
				}
			}
		}
	}
}

func generateActionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
