package clawagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	// MaxMemoryBytes is the maximum size of a memory content in bytes.
	MaxMemoryBytes = 4 * 1024 // 4KB

	memoryMergePrompt = `你是一个 memory 管理器。请基于以下信息更新用户记忆：

## 旧 Memory
%s

## 本轮对话
用户: %s
助手: %s

## 要求
1. 合并旧 memory 与本轮对话中的新事实（用户偏好、决策、常用 workspace 等）
2. 丢弃过时信息，保留关键事实
3. 输出格式：纯文本，不超过 800 字
4. 直接输出新 memory 内容，不要解释

## 新 Memory:`
)

// MemoryManager handles loading, saving, and refreshing agent memory.
type MemoryManager struct {
	db *gorm.DB
}

// NewMemoryManager creates a new MemoryManager.
func NewMemoryManager(db *gorm.DB) *MemoryManager {
	return &MemoryManager{db: db}
}

// Load loads the memory content for a user.
func (m *MemoryManager) Load(ctx context.Context, userID string) (string, error) {
	var mem Memory
	err := m.db.WithContext(ctx).Where("user_id = ?", userID).First(&mem).Error
	if err == gorm.ErrRecordNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return mem.Content, nil
}

// Save saves memory content for a user (upsert).
func (m *MemoryManager) Save(ctx context.Context, userID, content string) error {
	if len(content) > MaxMemoryBytes {
		content = content[:MaxMemoryBytes]
	}
	// Use GORM Clause for cross-DB upsert (works with PostgreSQL and SQLite)
	return m.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"content", "updated_at"}),
	}).Create(&Memory{
		UserID:    userID,
		Content:   content,
		UpdatedAt: time.Now(),
	}).Error
}

// Refresh performs an async LLM merge of old memory with the current conversation.
// This is called after each final response.
func (m *MemoryManager) Refresh(
	ctx context.Context,
	userID, userMessage, assistantReply string,
	llmClient llmGenerator,
	cfg ClawAgentConfig,
) error {
	oldMemory, _ := m.Load(ctx, userID)

	prompt := fmt.Sprintf(memoryMergePrompt, oldMemory, userMessage, assistantReply)

	provCfg := ProviderConfig{
		ProviderType: cfg.DefaultProvider,
		APIKey:       cfg.DefaultAPIKey,
		BaseURL:      strings.TrimRight(cfg.DefaultBaseURL, "/"),
		ModelName:    cfg.DefaultModelName,
	}

	resp, err := llmClient.Generate(ctx, provCfg, []ChatMessage{
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return err
	}

	if len(resp.Choices) == 0 {
		return nil
	}

	newMemory := strings.TrimSpace(resp.Choices[0].Message.Content)
	if newMemory == "" {
		return nil
	}

	return m.Save(ctx, userID, newMemory)
}
