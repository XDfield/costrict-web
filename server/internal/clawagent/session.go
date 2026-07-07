package clawagent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

var (
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionPruned   = errors.New("session was pruned")
	ErrSessionArchived = errors.New("session was archived")
)

// SessionMetaManager manages session metadata (freshness, version, archive).
type SessionMetaManager struct {
	db *gorm.DB
}

// NewSessionMetaManager creates a new SessionMetaManager.
func NewSessionMetaManager(db *gorm.DB) *SessionMetaManager {
	return &SessionMetaManager{db: db}
}

// Create creates a new session metadata entry.
func (m *SessionMetaManager) Create(ctx context.Context, userID, baseKey string, version int, resetType string) (*SessionMeta, error) {
	sid := fmt.Sprintf("%s:v%d", baseKey, version)
	meta := &SessionMeta{
		SessionID:     sid,
		UserID:        userID,
		BaseKey:       baseKey,
		Version:       version,
		ResetType:     resetType,
		LastMessageAt: time.Now(),
		MessageCount:  0,
	}
	if err := m.db.WithContext(ctx).Create(meta).Error; err != nil {
		return nil, err
	}
	return meta, nil
}

// Active returns the currently active (non-archived) session for a user/baseKey.
func (m *SessionMetaManager) Active(ctx context.Context, userID, baseKey string) (*SessionMeta, error) {
	var meta SessionMeta
	err := m.db.WithContext(ctx).
		Where("user_id = ? AND base_key = ? AND is_archived = false", userID, baseKey).
		First(&meta).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

// Get returns a session meta by session ID.
func (m *SessionMetaManager) Get(ctx context.Context, sessionID string) (*SessionMeta, error) {
	var meta SessionMeta
	err := m.db.WithContext(ctx).First(&meta, "session_id = ?", sessionID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

// Archive marks a session as archived.
func (m *SessionMetaManager) Archive(ctx context.Context, sessionID string) error {
	now := time.Now()
	return m.db.WithContext(ctx).
		Model(&SessionMeta{}).
		Where("session_id = ?", sessionID).
		Updates(map[string]any{
			"is_archived": true,
			"archived_at": now,
		}).Error
}

// IncrementMessageCount increments the message count and updates last_message_at.
func (m *SessionMetaManager) IncrementMessageCount(sessionID string) {
	_ = m.db.Model(&SessionMeta{}).
		Where("session_id = ?", sessionID).
		Updates(map[string]any{
			"message_count":   gorm.Expr("message_count + 1"),
			"last_message_at": time.Now(),
		}).Error
}

// UpdateTokenEstimate updates the token estimate for a session.
func (m *SessionMetaManager) UpdateTokenEstimate(ctx context.Context, sessionID string, estimate int) error {
	return m.db.WithContext(ctx).
		Model(&SessionMeta{}).
		Where("session_id = ?", sessionID).
		Update("token_estimate", estimate).Error
}

// ResolveActive resolves the active session ID for a user/baseKey.
// Used by announceToAgent for dynamic session resolution.
func (m *SessionMetaManager) ResolveActive(ctx context.Context, userID, baseKey string) (string, error) {
	meta, err := m.Active(ctx, userID, baseKey)
	if err == ErrSessionNotFound {
		return "", ErrSessionPruned
	}
	if err != nil {
		return "", err
	}
	return meta.SessionID, nil
}

// ListByUser lists all session metas for a user (active and archived).
func (m *SessionMetaManager) ListByUser(ctx context.Context, userID string) ([]SessionMeta, error) {
	var metas []SessionMeta
	if err := m.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("last_message_at DESC").
		Limit(50).
		Find(&metas).Error; err != nil {
		return nil, err
	}
	return metas, nil
}

// GetBySessionID retrieves a session meta by its session ID.
func (m *SessionMetaManager) GetBySessionID(ctx context.Context, sessionID string) (*SessionMeta, error) {
	var meta SessionMeta
	err := m.db.WithContext(ctx).First(&meta, "session_id = ?", sessionID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

// DeleteArchivedBefore deletes archived sessions older than the given time.
func (m *SessionMetaManager) DeleteArchivedBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	result := m.db.WithContext(ctx).
		Where("is_archived = true AND last_message_at < ?", cutoff).
		Delete(&SessionMeta{})
	return result.RowsAffected, result.Error
}

// PruneExcess deletes the oldest archived sessions exceeding the max per user.
func (m *SessionMetaManager) PruneExcess(ctx context.Context, maxPerUser int) (int64, error) {
	result := m.db.WithContext(ctx).
		Exec(`
			DELETE FROM agent_session_meta
			WHERE is_archived = true
			AND session_id IN (
				SELECT session_id FROM (
					SELECT session_id, ROW_NUMBER() OVER (PARTITION BY user_id ORDER BY last_message_at DESC) AS rn
					FROM agent_session_meta
					WHERE is_archived = true
				) ranked
				WHERE rn > ?
			)
		`, maxPerUser)
	return result.RowsAffected, result.Error
}

// (removed) SetEventData/GetEventData/ClearEventData — superseded by the
// chat_messages EVENT_PENDING/EVENT_RESOLVED rows (sole source of truth for
// pending event state). The session_meta.event_data column is no longer
// read by new code; the column itself is left in place to avoid forcing a
// destructive migration on existing deployments.
