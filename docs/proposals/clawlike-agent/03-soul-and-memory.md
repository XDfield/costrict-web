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

详见 [10-database.md](./10-database.md#agent_personas-表)。

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

// 构建 trpc-agent-go instruction（注入 system prompt，含 memory）
func (m *PersonaManager) BuildInstruction(persona *Persona, memory string) string {
    var sb strings.Builder
    sb.WriteString(persona.SoulContent)
    if persona.UserContext != "" {
        sb.WriteString("\n\n# User Context\n\n")
        sb.WriteString(persona.UserContext)
    }
    if strings.TrimSpace(memory) != "" {
        sb.WriteString("\n\n# Memory\n\n")
        sb.WriteString(memory)
    }
    return sb.String()
}
```

注入方式（通过 `llmagent.WithInstruction`）：

```go
agent := llmagent.New("claw-agent",
    llmagent.WithModel(model),
    llmagent.WithInstruction(personaMgr.BuildInstruction(persona, memory)),
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
2. 通过 workspace_delegate 工具向工作区下发任务

# Behavioral Rules

- 委托任务前，优先查找已有 workspace；没有合适的再按 device 新建
- 记住用户的偏好、常用 workspace 和项目路径
- 用中文回复，除非用户使用其他语言
`
```

## 3.2 Memory（记忆）

### 设计思路（简化版）

**每个用户维护一份唯一的 memory content（TEXT 字段）**，每轮对话前**全量预加载**到 system prompt，每轮对话**结束后异步合并更新**。

放弃 trpc-agent-go 的 `memory/postgres` 关键词检索后端，原因：

| 维度 | TF-IDF 检索（原方案） | 单内容（新方案） |
|------|----------------------|-----------------|
| 数据结构 | 多条 memory + 索引 | 单条 TEXT |
| 注入方式 | 每轮按相关性 top-K 检索后注入 | 全量拼接到 system prompt |
| 写入方式 | Agent 调用 `memory_add` 等工具 | 每轮结束后 LLM 合并 |
| 用户可见性 | 需通过 `memory_search` 才能看到 | 用户可在 settings 直接查看/编辑 |
| Token 成本 | 仅注入相关条目，省 token | 全量注入，但单条体积可控（限制 ≤ 4KB） |
| 实现复杂度 | 高（提取器、TF-IDF、工具） | 低（一个表 + 一个 hook） |
| 召回质量 | 关键词命中差时漏召回 | 全量上下文，无召回失败 |

**适用前提**：单用户 memory 总量 ≤ 4KB（约 1000-1500 个汉字）。超出后通过 LLM 自动裁剪保留关键信息。

### 数据库表

详见 [10-database.md](./10-database.md#agent_memories-表)。

```sql
CREATE TABLE agent_memories (
    user_id    VARCHAR(255) PRIMARY KEY,
    content    TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

### Memory 加载与注入

```go
// server/internal/clawagent/memory.go

type MemoryManager struct {
    db *gorm.DB
}

const MaxMemoryBytes = 4 * 1024  // 4KB 上限

func (m *MemoryManager) Load(ctx context.Context, userID string) (string, error) {
    var mem Memory
    err := m.db.Where("user_id = ?", userID).First(&mem).Error
    if err == gorm.ErrRecordNotFound {
        return "", nil  // 未初始化视为空 memory
    }
    return mem.Content, err
}

func (m *MemoryManager) Save(ctx context.Context, userID, content string) error {
    if len(content) > MaxMemoryBytes {
        content = content[:MaxMemoryBytes]  // 硬截断兜底
    }
    return m.db.Exec(`
        INSERT INTO agent_memories (user_id, content, updated_at)
        VALUES (?, ?, NOW())
        ON CONFLICT (user_id)
        DO UPDATE SET content = EXCLUDED.content, updated_at = NOW()
    `, userID, content).Error
}
```

### Memory 工具（仅 2 个）

| 工具 | 用途 |
|------|------|
| `memory_view` | Agent 主动查看完整 memory（罕见，因为已注入） |
| `memory_update` | Agent 在对话中主动改写 memory（罕见，主路径走自动 hook） |

**默认依赖自动 hook**，工具仅作降级/调试用。

### 注入流程

```go
// AgentFactory 构造 per-user Agent 时
memory, _ := memoryMgr.Load(ctx, userID)
instruction := personaMgr.BuildInstruction(persona, memory)

return llmagent.New("user-agent-"+userID,
    llmagent.WithModels(models),
    llmagent.WithInstruction(instruction),  // memory 已拼进 system prompt
    llmagent.WithTools(tools),               // 含 memory_view / memory_update
    llmagent.WithPreloadMemory(0),           // 关闭 trpc-agent-go 自带的预加载
), nil
```

### 每轮对话结束后的异步更新

**触发点**：`streamResponse()` 检测到 `evt.IsFinalResponse()` 后，启动 goroutine 触发一次"memory 合并"。

```go
// server/internal/clawagent/handler.go

func (rt *ClawAgentRuntime) streamResponse(
    ctx context.Context,
    eventCh <-chan *event.Event,
    sender channel.Sender,
    userID, sessionID, userMessage string,
) {
    var assistantReply strings.Builder

    for evt := range eventCh {
        // ... 流式输出逻辑 ...
        if evt.IsFinalResponse() {
            // 异步触发 memory 更新（不阻塞回复）
            go rt.refreshMemory(rt.bgCtx, userID, userMessage, assistantReply.String())
            break
        }
    }
}
```

### Memory 合并 LLM 调用

```go
// server/internal/clawagent/memory.go

const memoryMergePrompt = `你是一个 memory 管理器。请基于以下信息更新用户记忆：

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

func (m *MemoryManager) Refresh(
    ctx context.Context,
    userID, userMessage, assistantReply string,
    mergeModel model.Model,  // 用用户的 default provider
) error {
    oldMemory, _ := m.Load(ctx, userID)

    prompt := fmt.Sprintf(memoryMergePrompt, oldMemory, userMessage, assistantReply)
    resp, err := mergeModel.Generate(ctx, model.NewUserMessage(prompt))
    if err != nil {
        return err  // 失败时保留旧 memory，下次再试
    }

    newMemory := extractText(resp)
    if newMemory == "" {
        return nil  // LLM 没输出就保留旧 memory
    }
    return m.Save(ctx, userID, newMemory)
}
```

### 失败容忍

- LLM 调用失败：保留旧 memory，记日志，**不重试**（避免无限失败循环）
- LLM 输出为空：保留旧 memory
- 输出超 4KB：硬截断（兜底），下次合并会自然压缩
- 用户主动编辑：以用户编辑为准，下次 hook 触发时基于用户版本合并

### 用户编辑入口

通过 REST API 让用户直接查看和编辑自己的 memory：

```
GET  /api/clawagent/memory       → { content: "..." }
PUT  /api/clawagent/memory       → { content: "新内容" }
```

详见 [09-api.md](./09-api.md#memory-api)。

### 与 Persona 的关系

| 维度 | Persona | Memory |
|------|---------|--------|
| 写入方 | 用户主动编辑 | LLM 自动合并 + 用户可覆盖 |
| 体积 | 无限制（用户掌控） | ≤ 4KB（程序兜底） |
| 注入位置 | system prompt 头部 | system prompt 尾部（"# Memory"段） |
| 更新频率 | 用户改时 | 每轮对话结束 |
| 表 | `agent_personas`（一对多，选 default） | `agent_memories`（一对一） |

### 与 OpenClaw MEMORY.md 的对比

| 维度 | OpenClaw MEMORY.md | 本方案 |
|------|-------------------|--------|
| 存储 | 本地文件（用户 home） | PostgreSQL `agent_memories` 表 |
| 加载 | 每次启动读取整个文件 | 每轮对话前从 DB 读取（缓存可选） |
| 写入 | Agent 调用工具显式写 | LLM 自动合并（hook 触发） |
| 横向扩展 | 文件系统绑死单机 | 多实例共享 PostgreSQL |

### 横向扩展兼容性

- Memory 全量在 PostgreSQL，任何实例都能读写
- 异步更新 goroutine 使用独立 background context，不与请求 ctx 绑定
- 多个实例并发更新同一用户 memory 的风险：通过 `ON CONFLICT DO UPDATE` + 最后写入获胜（last-write-wins）解决；并发更新同一用户场景罕见（用户通常单设备使用）

### 与现有 memory 模块的关系

| 维度 | `server/internal/memory/` | `clawagent` Memory |
|------|--------------------------|---------------------|
| 服务对象 | device opencode agent | 云端个人助手 |
| 存储 | 文件系统 (storage.Backend) | PostgreSQL (`agent_memories` 表) |
| 数据结构 | skill 文件 | 单条 TEXT |
| 写入方 | device 上报 | LLM 自动合并 + 用户编辑 |

**不合并**：两者职责不同，各自独立运行。
