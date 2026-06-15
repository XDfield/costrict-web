# 3. Soul/Persona 与 Memory 系统

## 3.1 Persona（人格 / Soul）

### 设计思路

对标 OpenClaw 的 `SOUL.md` + `IDENTITY.md` + `USER.md` 三件套，用数据库表存储，每个用户可自定义。

| OpenClaw 文件 | 本方案字段 | 说明 |
|---------------|-----------|------|
| `SOUL.md` | `soul_content` | 人格核心：语气、行为准则、能力声明 |
| `IDENTITY.md` | `identity_content` | 身份信息：名称、头像 emoji、角色定位 |
| `USER.md` | `user_context` | 用户画像：职业、偏好、常用 workspace |

### 数据库表

详见 [09-database.md](./09-database.md#agent_personas-表)。

### Persona 加载与注入

```go
// server/internal/clawagent/persona.go

type PersonaManager struct {
    db *gorm.DB
}

func (m *PersonaManager) Load(ctx context.Context, userID string) (*Persona, error) {
    var persona Persona
    err := m.db.Where("user_id = ? AND is_default = true", userID).First(&persona).Error
    if err == gorm.ErrRecordNotFound {
        return m.defaultPersona(userID), nil
    }
    return &persona, err
}

// 构建 trpc-agent-go instruction（注入 system prompt）
func (m *PersonaManager) BuildInstruction(persona *Persona) string {
    var sb strings.Builder
    sb.WriteString(persona.SoulContent)
    if persona.UserContext != "" {
        sb.WriteString("\n\n# User Context\n\n")
        sb.WriteString(persona.UserContext)
    }
    return sb.String()
}
```

注入方式（通过 `llmagent.WithInstruction`）：

```go
agent := llmagent.New("claw-agent",
    llmagent.WithModel(model),
    llmagent.WithInstruction(personaMgr.BuildInstruction(persona)),
    llmagent.WithGlobalInstruction(persona.IdentityContent),
)
```

### 默认 Persona

```go
const DefaultSoulPrompt = `
# Identity

你是 costrict 平台用户的个人 AI 助手。

# Capabilities

1. 回答问题（使用你的知识和记忆）
2. 从 Capability Hub 加载和使用 Skill
3. 通过 workspace_delegate 工具向工作区下发任务

# Behavioral Rules

- 委托任务前，优先查找已有 workspace；没有合适的再按 device 新建
- 记住用户的偏好、常用 workspace 和项目路径
- 用中文回复，除非用户使用其他语言
`
```

## 3.2 Memory（记忆）

### 存储后端

使用 trpc-agent-go 的 **`memory/postgres`** 后端：

- **PostgreSQL 持久化**，服务重启后记忆不丢失
- **关键词搜索**（TF-IDF 相关性排序），不需要 pgvector 向量扩展
- 支持 fact / episode 两种记忆类型
- 支持自动记忆提取（通过 extractor）
- **支持无状态横向扩展**：多个 server 实例共享同一个 PostgreSQL，记忆读写一致

```go
import memorypostgres "trpc.group/trpc-go/trpc-agent-go/memory/postgres"

func NewMemoryService(cfg *config.Config) (memory.Service, error) {
    return memorypostgres.NewService(
        memorypostgres.WithPostgresClientDSN(cfg.Database.DSN),
        memorypostgres.WithTableName("clawagent_memories"),
        memorypostgres.WithSoftDelete(true),
        memorypostgres.WithMinSearchScore(0.3),
        memorypostgres.WithMaxResults(10),
        memorypostgres.WithAutoMemoryEnabled(true),
    )
}
```

### 数据库表结构

trpc-agent-go 的 postgres 后端自动创建 `clawagent_memories` 表：

```sql
CREATE TABLE clawagent_memories (
    memory_id   TEXT PRIMARY KEY,
    app_name    TEXT NOT NULL,
    user_id     TEXT NOT NULL,
    memory_data JSONB NOT NULL,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at  TIMESTAMP NULL DEFAULT NULL
);
CREATE INDEX idx_clawagent_memories_app_user   ON clawagent_memories(app_name, user_id);
CREATE INDEX idx_clawagent_memories_updated_at ON clawagent_memories(updated_at DESC);
CREATE INDEX idx_clawagent_memories_deleted_at ON clawagent_memories(deleted_at);
```

无需手动 migration，trpc-agent-go 初始化时自动创建。

### Memory 隔离

通过 `UserKey{AppName, UserID}` 按 userID 严格隔离：

```go
userKey := memory.UserKey{
    AppName: "clawagent",
    UserID:  subjectID,  // c.GetString(middleware.UserIDKey)
}
```

### 记忆工具

| 工具 | 用途 |
|------|------|
| `memory_add` | 添加记忆（Agent 自主调用） |
| `memory_update` | 更新已有记忆 |
| `memory_delete` | 删除记忆 |
| `memory_clear` | 清空用户所有记忆 |
| `memory_search` | 关键词搜索记忆 |
| `memory_load` | 加载最近记忆 |

### 预加载策略

```go
llmagent.WithPreloadMemory(10)  // 每次对话预加载 10 条最近记忆
```

### 横向扩展兼容性

由于 Memory 和 Session 都使用 PostgreSQL 后端：

- 多个 server 实例共享同一数据库
- 记忆读写通过数据库 ACID 保证一致性
- 任何实例都可以服务任何用户请求
- 无本地状态依赖（满足无状态横向扩展）

### 与现有 memory 模块的关系

| 维度 | `server/internal/memory/` | `clawagent` Memory |
|------|--------------------------|---------------------|
| 服务对象 | device opencode agent | 云端个人助手 |
| 存储 | 文件系统 (storage.Backend) | PostgreSQL (`clawagent_memories` 表) |
| 检索 | 按 slug/version | 关键词 TF-IDF 排序 |
| 持久化 | 文件持久化 | DB 持久化 |
| 写入方 | device 上报 | Agent 自主写入 + 自动提取 |

**不合并**：两者职责不同，各自独立运行。
