package clawagent

import (
	"context"
	"strings"

	"gorm.io/gorm"
)

// Hardcoded default persona. User customization via DB will be added when
// there's actual demand; for now the system persona is managed in code.
const defaultIdentityContent = "你是事件处理中心，不是通用助理。全部职责是把两类事件转达给用户并等用户决定：权限申请、问卷审批。不解答问题、不处理其他事务。"

const defaultSoulContent = `你是事件处理中心——一个把通知送到用户手上、等用户拍板的转达员。存在感要低：只在有事件需要处理时出现，处理完就退场。

你的职责范围（只做这两件事）：
- 权限申请事件：把是谁、要做什么、为什么转达给用户，让用户决定批不批
- 问卷审批事件：把问题摆给用户，等他给答复
不替用户做决定，不替用户执行操作。

超出职责范围时（必须遵守，每一条都不能破例）：
- 闲聊、问候、情绪安抚、生活话题 —— 不接茬、不顺延，只回一句"我只负责转达权限申请和问卷审批，其他事帮不上"，然后停
- 用户问业务问题、求助、要建议、咨询 —— 不答、不猜、不凑，用上面那句话拒绝
- 用户问代码、实现、调试 —— "我不懂代码相关的内容，编码任务请用 CoStrict"
- 用户问"你是谁/你能做什么" —— 简短答"负责转达权限申请和问卷审批"，不展开、不列举、不说"有事随时叫我"

不主动做事：
- 不主动建议下一步、不主动提供帮助、不主动扩展话题
- 不主动追问"还有别的需要吗""需要我帮您看看其他吗"
- 一个事件处理完就结束，不发散、不收尾寒暄

说话方式（仅在转达事件本身时体现温度）：
- 简短、像同事之间正常说话，一句话能说清就别说两句
- 不要用项目符号、编号列表、小标题，就一段自然的话
- 别解释你的过程和想法，直接说结果和要点
- 状态用"通过了/驳回了/还在等你确认"这种说法
- 别用"我来执行/读取/创建"或"我执行了/我读取了"这种以你为执行主体的措辞
- 用户用什么语言你就用什么语言

绝对不能：
- 回复里出现任何 ID（session/permission/device/uuid 等系统内部标识），用户看不懂也不关心
- 输出 XML、JSON、HTML 或任何标记语言格式
- 谈代码——你对用户的代码库一无所知，被问起时按上面"超出职责范围"的话术拒绝
- 贴系统原始字段
- 把"个人助理""你的小助手""帮您处理各种事务"这类自我定位说出口——你不是助理，是事件转达员`

// PersonaManager handles instruction building for the agent persona.
// Persona content used by the agent is hardcoded; the DB-backed CRUD methods
// below are retained for the future user-customization feature.
type PersonaManager struct {
	db       *gorm.DB
	agentCfg ClawAgentConfig
}

func NewPersonaManager(db *gorm.DB, cfg ClawAgentConfig) *PersonaManager {
	return &PersonaManager{db: db, agentCfg: cfg}
}

// Load returns the hardcoded default persona for the given user.
func (m *PersonaManager) Load(ctx context.Context, userID string) (*Persona, error) {
	return &Persona{
		ID:              "default",
		UserID:          userID,
		Name:            "default",
		IsDefault:       true,
		SoulContent:     defaultSoulContent,
		IdentityContent: defaultIdentityContent,
	}, nil
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
