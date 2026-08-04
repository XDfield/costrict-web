# 13. 统一大脑改造方案

> **状态**：重构提案（基于已落地实现的偏差盘点）
>
> **对应代码**：`server/internal/clawagent/`
>
> **关联文档**：[README.md](./README.md) §定位、[ai-driven-notification-handling.md](./ai-driven-notification-handling.md)、[11-roadmap.md](./11-roadmap.md)

## 13.1 背景

`clawlike-agent` 提案的定位原文（README.md:14）：

> 在 costrict-web 平台上构建 **云端个人 AI 助手**

它本应是一个**统一的智能大脑**：

- 不与任何具体业务绑定——业务（权限审批、问卷应答等）以**插件**形式注入，大脑只负责对话、记忆、人格、Provider 路由；
- 不与任何具体 IM 绑定——IM 只作为文本传输管道，渠道差异在 channel adapter 层吸收；
- 业务事件统一抽象为 **Task**，大脑以同一套协议处理任意业务类型的请求。

首期实现已落地（P0–P7、AI 通知处理），但在快速交付过程中，**业务语义从插件层泄漏到了核心层**。本提案盘点偏差、给出**保持向后兼容的渐进式改造路径**，使运行时回到"业务无关大脑 + 业务插件"的目标架构。

## 13.2 设计结论

| # | 结论 |
|---|------|
| 1 | **IM 渠道层抽象到位**——`channel.MessageHandler` / `InboundMessage` / `OutboundMessage` / `Sender` 以纯文本契约为边界，无 IM 反向耦合。本提案不动 |
| 2 | **人格（Persona）泄漏业务硬编码**——`persona.go:12-36` 写死了"权限申请 + 问卷审批"两类业务文案；`Load()`（`persona.go:51-60`）直接返回常量，**完全忽略 DB**。DB 写入路径成了死代码 |
| 3 | **工具系统已具备插件形态**——`tools/registry.go` 提供 `Tool` 接口 + `Registry` 注册；但 `tools/permission.go` / `tools/question.go` / `workspace_tools.go` 全是业务直注：依赖 `models.SystemNotification`、`DeviceProxy.ReplyPermission` 等具体业务客户端 |
| 4 | **事件类型在多处硬编码 switch**——`event_handler.go:520-522`（`needsEventProcessing`）、`agent.go:1195-1245`（`formatPendingEventsBlock`）写死 `permission` / `permission_batch` / `question` 三类，新增业务类型需改核心 |
| 5 | **意图识别走关键词匹配而非 LLM**——`intent_handler.go:87-90` 写死中文批准/拒绝关键词列表；`intent_handler.go:51-85` 的 `parseUserIntent` 直接 `switch ctx.EventType` 分支。该路径是 P5 的占位实现，与"统一大脑"愿景矛盾 |
| 6 | **Run 主流程有 4 条岔路**——`Run` / `RunWithSystem` / `RunEventNotifyRelay` / `RunEventReply`（`agent.go:191, 305, 624, 780`）。岔路存在的根因是事件协议未统一——通知中继路径无工具、用户回复路径有工具，必须分两个入口 |

## 13.3 目标

- **G1**：核心层（`clawagent` 顶层包）零业务依赖——除 `tools/` 子包外，不出现 `permission` / `question` / `SystemNotification` 等业务符号
- **G2**：业务事件统一为 `Task` 协议——新增业务类型只改插件，不改 `agent.go` / `event_handler.go` / `intent_handler.go`
- **G3**：人格、记忆、Provider 三套用户态资源**真**持久化——`PersonaManager.Load()` 走 DB
- **G4**：意图识别 LLM 化或下放到插件——核心层不持有业务关键词
- **G5**：每一步可独立合并、独立回滚——避免一次性大重构

## 13.4 非目标

- **不**重写 trpc-agent-go Runner 与 Session 后端（postgres）——抽象边界已经够用
- **不**修改 IM channel 层——已经达成"渠道无关"
- **不**改 Provider 加密（AES-256-GCM）、SSRF 校验、ownership 检查等已通过 review 的横切关注点
- **不**承诺 100% 文本等价——重构允许微调系统提示，但用户感知的回复质量不下降

## 13.5 偏差盘点（含 file:line 锚点）

### 13.5.1 Persona 业务硬编码（关键）

```go
// server/internal/clawagent/persona.go:12-36
const defaultIdentityContent = "你是用户的个人助理，当前只协助处理权限申请和问卷审批两类事项。"
const defaultSoulContent = `...权限申请、问卷审批...`
```

```go
// server/internal/clawagent/persona.go:51-60
func (m *PersonaManager) Load(ctx context.Context, userID string) (*Persona, error) {
    return &Persona{ID:"default", SoulContent: defaultSoulContent, ...}, nil  // 完全无视 DB
}
```

**问题**：
1. 业务文案写在核心层常量里
2. `LoadByID` / `ListByUser` / `Create` / `Update` / `Delete` / `SetDefault`（`persona.go:62-123`）全是死代码——HTTP 路由 `/personas` 能写入，但运行时永远读不到
3. 用户在 Web 设置页改人格无效

### 13.5.2 事件类型多处理点硬编码

```go
// server/internal/clawagent/event_handler.go:520-522
func needsEventProcessing(eventType string) bool {
    return eventType == "permission" || eventType == "permission_batch" || eventType == "question"
}
```

```go
// server/internal/clawagent/agent.go:1195-1245 (formatPendingEventsBlock)
switch ec.EventType {
case "permission", "permission_batch": ...
case "question": ...
default: ... "[未知事件类型]"
}
```

新增一类业务（例如 `config_change`）要同时改这两个点 + intent_handler 的 switch，**没有扩展点**。

### 13.5.3 意图识别走关键词

```go
// server/internal/clawagent/intent_handler.go:87-90
var (
    approvalKeywords  = []string{"批准", "同意", "允许", "好", "可以", "确认", "让他执行"}
    rejectionKeywords = []string{"拒绝", "不同意", "不允许", "不行", "不要", "危险", "禁止"}
)
```

```go
// server/internal/clawagent/intent_handler.go:60-82
if ctx.EventType == "permission" {
    switch { case isApproval(response): ... case isRejection(response): ... }
} else if ctx.EventType == "question" { ... }
```

**问题**：
1. 业务关键词（"批准"、"危险"）写在核心层
2. `switch EventType` 又一次硬编码业务分支
3. 代码注释 `// In production, this would use the LLM` 自承认是占位实现

### 13.5.4 Run 主流程四岔路

| 入口 | 行号 | 用途 | 有无工具 |
|------|------|------|---------|
| `Run` | `agent.go:191` | Web Chat 用户消息 | 无（用户态） |
| `RunWithSystem` | `agent.go:305` | 注入额外 system prompt | 无 |
| `RunEventNotifyRelay` | `agent.go:624` | 通知中继：通知文本 → AI 转述给用户 | 无 |
| `RunEventReply` | `agent.go:780` | 用户对通知的回复 + 工具调用 | 有 |

四条路径**本质相同**（构造 prompt → Runner → 流式回包），但因为"是否带工具 + 是否带 pending block + system 注入来源"维度耦合，被迫拆成四个 API。

### 13.5.5 工具系统已插件化但内容仍是业务直注

`tools/registry.go` 给的扩展点是干净的；但 `tools/permission.go:10` `import "github.com/costrict/costrict-web/server/internal/models"`、`tools/permission.go` 调用 `DeviceProxy.ReplyPermission`、`workspace_tools.go` 引用 `WorkspaceTask` 等——业务符号通过工具实现潜入。

这意味着即便把工具按子目录拆开（已经拆了），**新增一个不依赖 DeviceProxy 的纯工具**仍然要放进同一个 `tools` 包，与业务工具混在一起。

## 13.6 改造路径（7 阶段，每阶段独立可合并）

每阶段都遵循：**先加抽象层（不改行为）→ 迁移调用方 → 删除旧路径**。

### Phase 0：抽象骨架（无行为变更）

**目的**：把"统一大脑"该有的接口先建出来，全是空壳，落地为零业务影响。

**新增文件**：

```
server/internal/clawagent/core/
├── task.go          // 通用 Task 协议
├── plugin.go        // Plugin 接口：注册工具 + 提供人格片段 + 处理 Task
└── persona.go       // PersonaProvider 接口
```

**`core/task.go`** 关键类型（草案）：

```go
package core

// Task 是任意业务事件在核心层的统一表示。
// 业务插件负责把自己的原始事件（permission/question/...）翻译成 Task。
type Task struct {
    ID         string         // 全局唯一，由插件负责生成（可保留原 permission_id 等）
    Kind       string         // 业务种类：自由字符串，例如 "permission"、"question"
    UserID     string
    DeviceID   string
    SessionID  string
    Summary    string         // 给 LLM 看的一句话描述
    Detail     map[string]any // 原始 payload，插件自解
    PendingIDs []string       // 回复时要带的 ID（permission_ids / question_id 等）
}

// TaskHandler 由插件实现：把用户自然语言回复解析成结构化决议、并下发到设备。
type TaskHandler interface {
    HandleReply(ctx context.Context, t Task, userText string) (ReplyResult, error)
}

type ReplyResult struct {
    Resolved bool   // 是否终结该 Task
    Reply    string // 给用户的反馈文本（被 LLM 包一层后输出）
}
```

**`core/plugin.go`**：

```go
type Plugin interface {
    Name() string                              // 例如 "deviceops"
    Tools() []tools.Tool                       // 该插件注册到 LLM 的工具
    TaskKinds() []string                       // 该插件声明能处理的 Task.Kind
    RenderPending(t Task) string               // 渲染 [EVENT_PENDING] 块
}
```

**改动**：仅新增文件，不改 `agent.go` / `event_handler.go`。

**验收**：`go build ./...` 通过；现有测试全绿。

**回滚**：删除 `core/` 目录。

---

### Phase 1：Persona 解耦（关键）

**目的**：让 `PersonaManager.Load()` 真正走 DB；把业务文案从核心层常量搬到 seed 数据。

**改动**：

1. **删除** `persona.go:12-36` 的两个 `const`（`defaultIdentityContent` / `defaultSoulContent`）。
2. 改写 `persona.go:51-60` `Load()`：

   ```go
   func (m *PersonaManager) Load(ctx context.Context, userID string) (*Persona, error) {
       // 1. 找该用户的 default persona
       var p Persona
       err := m.db.WithContext(ctx).
           Where("user_id = ? AND is_default = true AND deleted_at IS NULL", userID).
           First(&p).Error
       if err == nil { return &p, nil }
       if !errors.Is(err, gorm.ErrRecordNotFound) { return nil, err }
       // 2. 兜底：种子默认人格（一次性写入）
       return m.seedDefault(ctx, userID)
   }
   ```

3. `seedDefault` 从**配置**而非常量读取 seed 内容——新增 `ClawAgentConfig.SeedPersona struct { Identity, Soul string }`，由 `setup.go` 从环境变量或 `seed.yaml` 注入。
4. **业务文案下沉**：默认 seed 改为业务无关的通用语（"你是用户的个人助理，会协助处理各类事项"）；权限/问卷的具体话术放到 `core/plugins/deviceops/persona_fragment.md`（Phase 2 落地，本阶段先留 TODO）。

**迁移**：首次启动时（迁移脚本 / 启动钩子）为每个已有用户写一条 default persona 到 DB；空库直接走 seed。

**验收**：
- `PersonaManager.Load()` 在 DB 有记录时返回 DB 内容
- Web 设置页改人格 → 下次对话系统提示跟着变
- 现有 `persona_test.go` 全绿（需补 `TestLoad_ReadsFromDB`）

**回滚**：恢复 `Load()` 的硬编码返回 + 两个 const。

---

### Phase 2：工具系统按业务分目录（关键）

**目的**：核心工具（如果有）与业务工具物理隔离；为 Phase 3 的 Plugin 化做铺垫。

**改动**：

```
server/internal/clawagent/tools/         // 仅保留 Registry / Context / Definition / 接口
└── deviceops/
    ├── permission.go                    // 从 tools/permission.go 迁入
    ├── question.go                      // 从 tools/question.go 迁入
    └── workspace.go                     // 从 workspace_tools.go 迁入
```

1. **移动** `tools/permission.go` → `tools/deviceops/permission.go`，包名改为 `deviceops`。
2. **移动** `tools/question.go` → `tools/deviceops/question.go`。
3. **移动** `workspace_tools.go` → `tools/deviceops/workspace.go`（注意：当前 `workspace_tools.go` 在 `clawagent` 顶层包，需同时改包名）。
4. **保留** `tools/session_info.go` 与 `tools/instructions.go` 在原位置——它们是核心工具，不依赖业务。
5. **改 setup.go**：注册工具改为 `registry.Register(deviceops.NewPermissionTool())` 等。

**验收**：
- `tools/` 包不再 `import "server/internal/models"`
- `clawagent` 顶层包不再持有 `WorkspaceTask` 引用
- 现有测试（`permission_test.go` 等）随包迁移后全绿

**回滚**：`git revert`。

---

### Phase 3：Task 协议落地

**目的**：用统一 `Task` 替换 `EventContext` 在核心层的传递；新增业务类型不再改 `agent.go` / `event_handler.go`。

**改动**：

1. **`core/plugins/deviceops/`** 实装 `Plugin`：

   ```go
   func (p *Plugin) TaskKinds() []string { return []string{"permission", "permission_batch", "question"} }
   func (p *Plugin) RenderPending(t core.Task) string { ... } // 把当前 formatPendingEventsBlock 的 permission/question 分支搬来
   ```

2. **`event_handler.go:520-522`** `needsEventProcessing` 改为查 Plugin 注册表：

   ```go
   func (rt *ClawAgentRuntime) needsEventProcessing(eventType string) bool {
       return rt.plugins.HandlesKind(eventType)
   }
   ```

3. **`agent.go:1187-1248`** `formatPendingEventsBlock` 改为遍历 plugins，让每个 plugin 渲染自己：

   ```go
   func formatPendingEventsBlock(tasks []core.Task, plugins *PluginSet) string {
       for _, t := range tasks {
           p := plugins.ForKind(t.Kind)
           b.WriteString(p.RenderPending(t))
       }
   }
   ```

4. **`EventContext` 保留**作为外部协议（来自 `AIEventRequest`），在 `event_handler.go` 入口被翻译为 `core.Task`。

**验收**：
- 新增一类业务（demo `kind=config_change`）只需 `deviceops` 内增加 `RenderPending` 实现 + 一个 tool，**零改动**核心层
- `event_handler.go` / `agent.go` 不再出现 `"permission"` / `"question"` 字面量

**回滚**：还原 `formatPendingEventsBlock` switch + `needsEventProcessing` 硬编码。

---

### Phase 4：Run 主流程合一

**目的**：把 `Run` / `RunWithSystem` / `RunEventNotifyRelay` / `RunEventReply` 四入口收敛为单一 `RunOnce`，差异通过参数表达。

**改动**：

1. 引入 `RunOptions`：

   ```go
   type RunOptions struct {
       UserID       string
       SessionID    string
       UserMessage  string            // 用户文本；空表示系统触发
       SystemExtra  string            // 额外 system 注入
       PendingTasks []core.Task       // pending 块；空则不渲染
       EnableTools  bool              // 是否启用工具
   }
   func (r *AgentRunner) RunOnce(ctx context.Context, opts RunOptions) (<-chan AgentEvent, error)
   ```

2. 旧四入口保留为薄壳，转调 `RunOnce`（保持外部 API 不变）：

   ```go
   func (r *AgentRunner) Run(...) { return r.RunOnce(ctx, RunOptions{...}) }
   ```

3. 内部所有共享逻辑（`startRun`、session 解析、prompt 装配）只保留一份。

**验收**：
- 四旧入口行为等价（用现有 `agent_test.go` + SSE 回放 diff 验证）
- `startRun` 仍是单 session 单 in-flight Run 的协调点，无并发回归

**回滚**：旧入口本就没删，直接停用 `RunOnce`。

---

### Phase 5：意图识别 LLM 化或下沉

**目的**：核心层不再持有"批准 / 拒绝"等业务关键词。

**两条路径二选一**：

**路径 A（推荐）：完全删除 `IntentHandler`，让 LLM 通过 tool call 自然决策**

- 用户对通知的回复走 `RunOnce(EnableTools=true)`，LLM 自己决定调 `reply_permission` 还是 `reply_question`
- 删除 `intent_handler.go` 全文（约 200 行）
- `intent_handler_test.go` 一并删除——意图识别的"正确性"由 `tools/deviceops` 的端到端测试覆盖

**路径 B：保留结构，关键词下沉到 `deviceops`**

- `approvalKeywords` / `rejectionKeywords` 从 `intent_handler.go:87-90` 移到 `tools/deviceops/keywords.go`
- `parseUserIntent` 改为调用 plugin 提供的 `ParseReply` 方法
- 代价：仍然保留非 LLM 占位实现，与愿景不一致

**验收**：
- `clawagent` 顶层包不再出现 "批准" / "同意" / "拒绝" 等业务关键词
- 端到端通知回复测试（P5 既有 case）全绿

**回滚**：路径 A 较激进——回滚即恢复 `intent_handler.go`。建议在主干分支合入前用 feature flag `CLAWAGENT_LLM_INTENT=true` 灰度。

---

### Phase 6：Schema 与命名收敛

**目的**：把"事件"语言彻底替换为"任务"语言，使代码读起来跟愿景一致。

**改动**：
- DB 字段重命名（带迁移）：`agent_workspace_tasks.source_event_type` → `task_kind`；`system_notifications.event_type` 保留（外部 API 已暴露）
- SSE event 名：`event_pending` → `task_pending`、`event_resolved` → `task_resolved`（前端同步改）
- 内部类型：`EventContext` → 改名 `IncomingTaskPayload`，仅用于和 cs-cloud 协议边界
- 文档：`ai-driven-notification-handling.md` 加补丁段说明本次改名

**验收**：核心包内 `grep -r "Event" server/internal/clawagent/*.go` 仅剩 cs-cloud 边界类型。

**回滚**：DB 迁移有 down script；SSE 改名版本兼容一个版本（双发）。

## 13.7 阶段交付与里程碑

| Phase | 工期估算 | 阻塞关系 | 风险 |
|-------|---------|---------|------|
| 0 抽象骨架 | 1 天 | 无 | 低（纯加文件） |
| 1 Persona 解耦 | 2 天 | 0 | 中（DB 迁移） |
| 2 工具分目录 | 1.5 天 | 无 | 低（机械重构） |
| 3 Task 协议 | 3 天 | 0、2 | 高（核心层动态分派） |
| 4 Run 合一 | 3 天 | 3 | 中（行为等价验证） |
| 5 意图 LLM 化 | 2 天 | 4 | 中-高（LLM 行为不可控） |
| 6 命名收敛 | 2 天 | 1、3 | 低（机械重命名 + 迁移） |
| **合计** | **14.5 天** | | |

可以并行：Phase 2 与 Phase 1 互不阻塞；Phase 6 可在 Phase 3 后任意时间插入。

## 13.8 风险与缓解

| 风险 | 缓解 |
|------|------|
| Persona DB 迁移漏用户（部分用户首次启动拿不到 default） | seed 兜底：`Load()` 在 DB 未命中时**写入**默认 persona，保证幂等 |
| Task 协议过度抽象导致权限/问卷特有字段（如 `pendingIDs`）无处安放 | `Detail map[string]any` 兜底，插件按 kind 自解；核心层不读 |
| LLM 意图化后回复质量下降（不再 100% 命中批准/拒绝） | Phase 5 走 feature flag 灰度；保留 tool schema 严格约束 LLM 输出 |
| 大重构引入并发回归 | 每个 Phase 都保留旧入口为薄壳，分阶段合入主干；现有 SSE 流式回放 diff 作为黄金测试 |
| 改名（Phase 6）破坏外部 API | SSE 事件双发一个版本；DB 字段改名走标准 gorm migration + down script |

## 13.9 验收清单（整体）

- [ ] `clawagent` 顶层包 `grep -E "permission|question|approval|批准|拒绝"` 命中数为 0（除测试与 cs-cloud 边界）
- [ ] `PersonaManager.Load()` 在 DB 有/无记录两条路径都有测试覆盖
- [ ] 新增一个 `demo` 业务 Task 类型，核心层零改动即可端到端流转
- [ ] `Run` / `RunWithSystem` / `RunEventNotifyRelay` / `RunEventReply` 共用 `RunOnce`，行为等价（SSE diff 测试通过）
- [ ] `intent_handler.go` 删除或纯化，无业务关键词
- [ ] 现有测试 100% 通过，新增测试覆盖：persona DB 路径、plugin 路由、RunOnce 参数组合

## 13.10 与原 roadmap 的关系

本改造跨 P1（Persona）/ P5（AI 通知处理）两个原阶段，**不**改变 P0/P2/P4/P4.5/P6/P7 已交付的形态。改造产物：

- **P1 增量**：Persona DB 真启用（原 P1 只建了 CRUD 没接通 Load）
- **P5 增量**：意图识别从关键词升级为 LLM tool call（原 P5 自承认占位实现）
- **新增**：Plugin 抽象、Task 协议、RunOnce 合一——为未来业务（不只权限/问卷）接入铺路

建议作为 P7（生产硬化）之前的**P6.5 统一大脑改造**插入 [11-roadmap.md](./11-roadmap.md)。
