# 5. Skill 与 Capability Hub 对接

## 5.1 设计目标

将 costrict-web Capability Hub 中的 **skill 类型** Capability 直接对接到 trpc-agent-go 的 Skill 系统。

**核心约束**：纯云端环境，不引入物理文件系统。Skill 内容从数据库按需加载到内存。

## 5.2 技术方案：内存 Skill Repository

trpc-agent-go 的 `skill.Repository` 接口只有 3 个方法：

```go
type Repository interface {
    Summaries() []Summary              // 列出所有 skill 摘要
    Get(name string) (*Skill, error)   // 按名称获取完整 skill
    Path(name string) (string, error)  // 返回 skill 目录路径
}
```

本方案实现一个**基于数据库 + 内存缓存**的 Repository，替代默认的 `FSRepository`：

```go
// server/internal/clawagent/skills.go

type DBSkillRepository struct {
    db    *gorm.DB
    mu    sync.RWMutex
    cache map[string]*skillCacheEntry  // name → cache
}

type skillCacheEntry struct {
    summary   skill.Summary
    full      *skill.Skill
    updatedAt time.Time
}

func NewDBSkillRepository(db *gorm.DB) *DBSkillRepository {
    return &DBSkillRepository{
        db:    db,
        cache: make(map[string]*skillCacheEntry),
    }
}
```

### Summaries() — 列出所有 skill

```go
func (r *DBSkillRepository) Summaries() []skill.Summary {
    r.mu.RLock()
    // 优先从缓存返回
    if len(r.cache) > 0 {
        out := make([]skill.Summary, 0, len(r.cache))
        for _, e := range r.cache {
            out = append(out, e.summary)
        }
        sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
        return out
    }
    r.mu.RUnlock()

    // 缓存为空，从 DB 加载
    r.refreshCache()
    return r.Summaries()
}
```

### Get(name) — 按需加载完整 skill

```go
func (r *DBSkillRepository) Get(name string) (*skill.Skill, error) {
    r.mu.RLock()
    if entry, ok := r.cache[name]; ok && entry.full != nil {
        r.mu.RUnlock()
        return entry.full, nil
    }
    r.mu.RUnlock()

    // 从 DB 查询
    var item models.CapabilityItem
    err := r.db.Where("item_type = ? AND slug = ? AND security_status IN ?",
        "skill", name, []string{"approved", "scanned"}).First(&item).Error
    if err != nil {
        return nil, fmt.Errorf("skill %q not found: %w", name, err)
    }

    // 解析 SKILL.md 内容（YAML frontmatter + Markdown body）
    parsed := parseSkillContent(item.Slug, item.Content)

    // 缓存
    r.mu.Lock()
    r.cache[name] = &skillCacheEntry{
        summary: parsed.Summary,
        full:    parsed,
        updatedAt: time.Now(),
    }
    r.mu.Unlock()

    return parsed, nil
}
```

### Path(name) — skill_run 执行所需

`Path()` 用于 `skill_run` 工具在文件系统中执行 skill。由于本方案不使用文件系统，`skill_run` 的本地执行能力**不可用**。

但 `skill_load`（加载 skill 内容到对话上下文）完全可用——这正是 Agent 使用 skill 的主要方式。Agent 加载 skill 后，按照 skill 中的指导自行完成任务，或通过 `workspace_delegate` 委托到 workspace 执行。

```go
func (r *DBSkillRepository) Path(name string) (string, error) {
    return "", fmt.Errorf("DBSkillRepository does not support filesystem paths; use skill_load instead of skill_run")
}
```

### 解析 SKILL.md 内容

```go
func parseSkillContent(name, content string) *skill.Skill {
    // content 来自 capability_items.content，格式为 SKILL.md
    // 解析 YAML frontmatter + Markdown body
    fm, body := splitFrontMatter(content)

    return &skill.Skill{
        Summary: skill.Summary{
            Name:        firstNonEmpty(fm["name"], name),
            Description: fm["description"],
        },
        Body: body,
    }
}
```

## 5.3 缓存刷新

```go
// Refresh 重新从 DB 加载所有 skill 摘要
func (r *DBSkillRepository) Refresh() error {
    r.refreshCache()
    return nil
}

func (r *DBSkillRepository) refreshCache() {
    var items []models.CapabilityItem
    r.db.Where("item_type = ? AND security_status IN ?",
        "skill", []string{"approved", "scanned"}).Find(&items)

    r.mu.Lock()
    defer r.mu.Unlock()

    // 只更新 summary，full 按需加载
    newCache := make(map[string]*skillCacheEntry, len(items))
    for _, item := range items {
        fm, _ := splitFrontMatter(item.Content)
        name := firstNonEmpty(fm["name"], item.Slug)
        newCache[item.Slug] = &skillCacheEntry{
            summary: skill.Summary{
                Name:        name,
                Description: fm["description"],
            },
        }
    }
    r.cache = newCache
}
```

### 增量失效

当 Capability Hub 中的 skill 增/改/删时，增量失效缓存：

```go
func (r *DBSkillRepository) Invalidate(name string) {
    r.mu.Lock()
    delete(r.cache, name)
    r.mu.Unlock()
}

func (r *DBSkillRepository) InvalidateAll() {
    r.mu.Lock()
    r.cache = make(map[string]*skillCacheEntry)
    r.mu.Unlock()
}
```

在 item handler 的 create/update/delete 钩子中调用：

```go
func OnItemChanged(item *models.CapabilityItem) {
    if item.ItemType == "skill" {
        skillRepo.Invalidate(item.Slug)
    }
}
```

## 5.4 注入到 Agent

```go
// 创建 DBSkillRepository
skillRepo := NewDBSkillRepository(db)
skillRepo.refreshCache()  // 启动时全量加载摘要

// 注入到 Agent
llmagent.WithSkillsRepository(skillRepo)
```

## 5.5 Skill 工具可用性

| 工具 | 可用 | 说明 |
|------|------|------|
| `skill_load` | ✅ | 加载 skill 全文到对话上下文（主要使用方式） |
| `skill_list_docs` | ⚠️ | 无辅助文档（DB 中只有 content 字段） |
| `skill_select_docs` | ⚠️ | 同上 |
| `skill_run` | ❌ | 需要文件系统 Path，本方案不支持 |
| `skill_exec` | ❌ | 同上 |

**替代方案**：Agent `skill_load` 后，通过 `workspace_delegate` 将任务委托到 workspace 执行。workspace（device 端）可以使用完整的 `skill_run`。

## 5.6 用户私有 Skill

```go
type DBSkillRepository struct {
    db    *gorm.DB
    mu    sync.RWMutex
    cache map[string]*skillCacheEntry
    userID string  // 可选：per-user 隔离
}

// 在查询时增加 user 过滤
func (r *DBSkillRepository) Get(name string) (*skill.Skill, error) {
    query := r.db.Where("item_type = ? AND slug = ?", "skill", name)
    if r.userID != "" {
        query = query.Where("user_id = ? OR visibility = 'public'", r.userID)
    }
    // ...
}
```

## 5.7 MCP 类型 Capability

对于 `item_type = "mcp"` 的 Capability，通过 trpc-agent-go 的 MCP 工具集成，按需从 DB 加载 MCP server 配置：

```go
// 从 capability_items.metadata 解析 mcp_config
// 创建 MCP client 连接
mcpTool := mcptool.New(serverConn)
llmagent.WithTools([]tool.Tool{mcpTool})
```

MCP 配置同样从 DB 加载到内存，不涉及文件系统。
