package clawagent

import (
	"context"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// PersonaManager handles CRUD and instruction building for agent personas.
type PersonaManager struct {
	db       *gorm.DB
	agentCfg ClawAgentConfig
}

func NewPersonaManager(db *gorm.DB, cfg ClawAgentConfig) *PersonaManager {
	return &PersonaManager{db: db, agentCfg: cfg}
}

func (m *PersonaManager) Load(ctx context.Context, userID string) (*Persona, error) {
	var persona Persona
	err := m.db.WithContext(ctx).
		Where("user_id = ? AND is_default = true", userID).
		First(&persona).Error
	if err == gorm.ErrRecordNotFound {
		return m.defaultPersona(userID)
	}
	if err != nil {
		return nil, err
	}
	return &persona, nil
}

func (m *PersonaManager) LoadByID(ctx context.Context, id string) (*Persona, error) {
	var persona Persona
	if err := m.db.WithContext(ctx).First(&persona, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &persona, nil
}

func (m *PersonaManager) ListByUser(ctx context.Context, userID string) ([]Persona, error) {
	var personas []Persona
	if err := m.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("created_at DESC").
		Find(&personas).Error; err != nil {
		return nil, err
	}
	return personas, nil
}

func (m *PersonaManager) Create(ctx context.Context, p *Persona) error {
	if p.ID == "" {
		p.ID = uuidString()
	}
	if p.IsDefault {
		_ = m.db.WithContext(ctx).
			Model(&Persona{}).
			Where("user_id = ? AND is_default = true", p.UserID).
			Update("is_default", false).Error
	}
	return m.db.WithContext(ctx).Create(p).Error
}

func (m *PersonaManager) Update(ctx context.Context, p *Persona) error {
	if p.IsDefault {
		_ = m.db.WithContext(ctx).
			Model(&Persona{}).
			Where("user_id = ? AND is_default = true AND id != ?", p.UserID, p.ID).
			Update("is_default", false).Error
	}
	return m.db.WithContext(ctx).Save(p).Error
}

func (m *PersonaManager) Delete(ctx context.Context, id string) error {
	return m.db.WithContext(ctx).Delete(&Persona{}, "id = ?", id).Error
}

func (m *PersonaManager) SetDefault(ctx context.Context, userID, id string) error {
	tx := m.db.WithContext(ctx).Begin()
	if err := tx.Model(&Persona{}).
		Where("user_id = ?", userID).
		Update("is_default", false).Error; err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Model(&Persona{}).
		Where("id = ? AND user_id = ?", id, userID).
		Update("is_default", true).Error; err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit().Error
}

func (m *PersonaManager) BuildInstruction(persona *Persona, memory string) string {
	var sb strings.Builder
	if persona.IdentityContent != "" {
		sb.WriteString("# Identity\n\n")
		sb.WriteString(persona.IdentityContent)
		sb.WriteString("\n\n")
	}
	sb.WriteString(persona.SoulContent)
	if persona.UserContext != "" {
		sb.WriteString("\n\n# User Context\n\n")
		sb.WriteString(persona.UserContext)
	}
	trimmedMemory := strings.TrimSpace(memory)
	if trimmedMemory != "" {
		sb.WriteString("\n\n# Memory\n\n")
		sb.WriteString(trimmedMemory)
	}
	return sb.String()
}

func (m *PersonaManager) defaultPersona(userID string) (*Persona, error) {
	p := &Persona{
		ID:        uuidString(),
		UserID:    userID,
		Name:      "default",
		IsDefault: true,
		SoulContent: `# Capabilities

1. 回答问题（使用你的知识和记忆）
2. 通过 workspace_delegate 工具向工作区下发任务

# Behavioral Rules

- 委托任务前，优先查找已有 workspace；没有合适的再按 device 新建
- 记住用户的偏好、常用 workspace 和项目路径
- 用中文回复，除非用户使用其他语言`,
		IdentityContent: "你是 costrict 平台用户的个人 AI 助手。",
	}
	if err := m.db.Create(p).Error; err != nil {
		return nil, fmt.Errorf("create default persona: %w", err)
	}
	return p, nil
}
