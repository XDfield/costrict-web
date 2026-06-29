package clawagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// SessionMessage represents a single message in a conversation session, persisted in DB.
type SessionMessage struct {
	ID         uint      `gorm:"primaryKey;autoIncrement"`
	SessionID  string    `gorm:"size:255;not null;index:idx_sm_session;index:idx_sm_session_created,priority:1"`
	Role       string    `gorm:"size:20;not null"`
	Content    string    `gorm:"type:text"`
	ToolCallID string    `gorm:"size:255"`
	ToolCalls  string    `gorm:"type:text"` // JSON-serialized []ToolCall
	CreatedAt  time.Time `gorm:"autoCreateTime;index:idx_sm_session_created,priority:2"`
}

func (SessionMessage) TableName() string {
	return "agent_session_messages"
}

// toChatMessage converts a SessionMessage to a ChatMessage for LLM requests.
func (sm *SessionMessage) toChatMessage() ChatMessage {
	msg := ChatMessage{
		Role:       sm.Role,
		Content:    sm.Content,
		ToolCallID: sm.ToolCallID,
	}
	if sm.ToolCalls != "" {
		var tcs []ToolCall
		if err := json.Unmarshal([]byte(sm.ToolCalls), &tcs); err == nil {
			msg.ToolCalls = tcs
		}
	}
	return msg
}

// chatMessageToRecord builds a SessionMessage from a ChatMessage (does not yet insert).
func chatMessageToRecord(sessionID string, msg ChatMessage) *SessionMessage {
	sm := &SessionMessage{
		SessionID:  sessionID,
		Role:       msg.Role,
		Content:    msg.Content,
		ToolCallID: msg.ToolCallID,
	}
	if len(msg.ToolCalls) > 0 {
		data, err := json.Marshal(msg.ToolCalls)
		if err == nil {
			sm.ToolCalls = string(data)
		}
	}
	return sm
}

// MessageManager handles CRUD of session messages in the database.
type MessageManager struct {
	db *gorm.DB
}

// NewMessageManager creates a new MessageManager.
func NewMessageManager(db *gorm.DB) *MessageManager {
	return &MessageManager{db: db}
}

// LoadMessages retrieves all messages for a session, ordered by creation time.
func (m *MessageManager) LoadMessages(ctx context.Context, sessionID string) ([]ChatMessage, error) {
	var records []SessionMessage
	err := m.db.WithContext(ctx).
		Where("session_id = ?", sessionID).
		Order("created_at ASC, id ASC").
		Find(&records).Error
	if err != nil {
		return nil, err
	}
	msgs := make([]ChatMessage, len(records))
	for i, r := range records {
		msgs[i] = r.toChatMessage()
	}
	return msgs, nil
}

// AppendMessage saves a new message to the session.
func (m *MessageManager) AppendMessage(ctx context.Context, sessionID string, msg ChatMessage) error {
	record := chatMessageToRecord(sessionID, msg)
	return m.db.WithContext(ctx).Create(record).Error
}

// AppendMessages saves multiple messages atomically.
func (m *MessageManager) AppendMessages(ctx context.Context, sessionID string, msgs []ChatMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	records := make([]SessionMessage, len(msgs))
	for i, msg := range msgs {
		records[i] = *chatMessageToRecord(sessionID, msg)
	}
	return m.db.WithContext(ctx).Create(&records).Error
}

// IsEmpty checks if a session has any messages.
func (m *MessageManager) IsEmpty(ctx context.Context, sessionID string) (bool, error) {
	var count int64
	err := m.db.WithContext(ctx).
		Model(&SessionMessage{}).
		Where("session_id = ?", sessionID).
		Count(&count).Error
	return count == 0, err
}

// DeleteSessionMessages removes all messages for a session.
func (m *MessageManager) DeleteSessionMessages(ctx context.Context, sessionID string) error {
	return m.db.WithContext(ctx).
		Where("session_id = ?", sessionID).
		Delete(&SessionMessage{}).Error
}

// UpdateTokenEstimate saves the token estimate for a session (via SessionMeta).
func (m *MessageManager) UpdateTokenEstimate(ctx context.Context, sessionID string, estimate int) error {
	return m.db.WithContext(ctx).
		Model(&SessionMeta{}).
		Where("session_id = ?", sessionID).
		Update("token_estimate", estimate).Error
}

// formatMessagesForSummary formats messages for LLM summarization.
func formatMessagesForSummary(msgs []ChatMessage, keepRecent int) string {
	var b strings.Builder
	totalMsgs := len(msgs)
	keepStart := totalMsgs - keepRecent*2
	if keepStart < 0 {
		keepStart = 0
	}
	for i, msg := range msgs {
		if msg.Role == "system" {
			continue
		}
		if i >= keepStart {
			b.WriteString(fmt.Sprintf("[%s]: %s\n", msg.Role, msg.Content))
		}
	}
	return b.String()
}
