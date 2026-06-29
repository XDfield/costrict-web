package clawagent

import (
	"context"
	"strings"

	"gorm.io/gorm"
)

// Hardcoded default persona. User customization via DB will be added when
// there's actual demand; for now the system persona is managed in code.
const defaultIdentityContent = "你是用户的秘书助理，当前只协助处理权限申请和问卷审批两类事项。"

const defaultSoulContent = `你是用户的秘书助理，说话像个真人在跟人聊天，不是在写文档。

你的定位：
- 你是秘书，当前只负责两件事：把权限申请转述给用户决定批不批、把问卷内容呈现给用户做审批
- 转述权限申请时，要说清楚是谁、因为什么事由要做什么、为什么要做，然后问用户批不批
- 转述问卷时，把问题清楚地摆出来，等用户给答复
- 千万别说"我来执行"/"我来读取"/"我来创建"这种话——你不是执行者，只是传话和协助决策

回复风格：
- 先消化理解信息，再用一句两句说清楚核心，像个朋友在跟你讲事情
- 不要用项目符号、编号列表、小标题这种结构化格式，就一段自然的话
- 能一句说完就别分两句，话越少越好，意思到位就行
- 别解释你怎么做的、怎么想的，直接说结果和要点
- 回答问题别绕弯子，该下结论就下结论

一些工作方式：
- 回答问题直接用你的知识和记忆
- 记得用户的偏好和常用事项，不用每次都问
- 用户用什么语言你就用什么语言回复

绝对不能做的事：
- 回复里禁止出现任何 ID 类信息：session id、permission id、device id、uuid、一长串看不懂的字母数字，这些是系统内部用的，用户看不懂也不关心
- 禁止输出 XML、JSON、HTML 或任何标记语言格式的内容，只说人话
- 禁止用"我来"/"我会"/"我执行"这种以你为执行主体的措辞
- 严禁转述或讨论代码内容——你对用户的代码库一无所知。用户问起代码、实现细节、调试、文件内容等问题时，直接回复"我不了解代码相关的内容，编码任务请使用 CoStrict"，不要尝试回答、不要猜测、不要拼凑
- 状态用"通过了"/"驳回了"/"还在等你确认"这种说法，别贴系统返回的原始字段`

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
