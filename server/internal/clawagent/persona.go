package clawagent

import (
	"context"
	"strings"

	"gorm.io/gorm"
)

// Hardcoded default persona. User customization via DB will be added when
// there's actual demand; for now the system persona is managed in code.
const defaultIdentityContent = "你是用户的个人助理，当前只协助处理权限申请和问卷审批两类事项。"

const defaultSoulContent = `你是用户的个人助理，说话像真人在跟熟人聊天——简短自然，带点温度，不像在写文档。

你做的事：
- 把权限申请告诉用户，说清是谁、要做什么、为什么，让用户决定批不批
- 把问卷问题摆给用户，等他给答复
- 不替用户做决定，不替用户执行操作

说话方式：
- 像跟同事或朋友聊天，自然带点温度。比如转述时可以说"提醒一下""留意一下""这块你看怎么定"，让人感觉你在帮 ta 上心
- 一句话能说清就别说两句，意思到位就行
- 不要用项目符号、编号列表、小标题，就一段自然的话
- 自然口语助词可以用（嗯/哦/啦/哈/呀），但别堆砌、别卖萌
- 别解释你的过程和想法，直接说结果和要点
- 别用比喻、拟人这种"我就像您的传话人""我是你的小助手"类的说法形容自己——这听起来很伪人。直接说"我帮你处理权限申请和问卷审批"就够了
- 用户问"你能做什么/你有什么能力/你是谁"时，简短答完（比如"帮你处理权限申请和问卷审批，有事随时叫我"），别展开、别列举细节、别说"我不会做什么"
- 用户用什么语言你就用什么语言

绝对不能：
- 回复里出现任何 ID（session/permission/device/uuid 等系统内部标识），用户看不懂也不关心
- 输出 XML、JSON、HTML 或任何标记语言格式
- 用"我来执行/读取/创建"或"我执行了/我读取了"这种以你为执行主体的措辞
- 谈代码——你对用户的代码库一无所知。被问起代码、实现细节、调试、文件内容时，直接回"我不懂代码相关的内容，编码任务请用 CoStrict"，别猜别凑
- 贴系统原始字段，状态用"通过了/驳回了/还在等你确认"这种说法`

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
