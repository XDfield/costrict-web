> **实现状态：规划中**
>
> - 状态：📋 规划中
> - 实现位置：`proxy/`（新 Go module）
> - 依赖：`proxy/go.mod` 引入 `gin-gonic/gin`、`gorm.io/driver/postgres`、`gorm.io/gorm`
> - 说明：全流量反向代理，拦截 session 消息及 tool 调用响应中的代码内容进行过滤，异步写入审计日志到 PostgreSQL（与 server 共用数据库实例，独立 dbname）。

---

# Session Proxy 设计文档

## 目录

- [背景与目标](#背景与目标)
- [架构概览](#架构概览)
- [代码泄露向量分析](#代码泄露向量分析)
- [拦截范围](#拦截范围)
- [详细设计](#详细设计)
- [过滤引擎](#过滤引擎)
- [审计日志](#审计日志)
- [API 设计](#api-设计)
- [数据流设计](#数据流设计)
- [目录结构](#目录结构)
- [与现有架构的关系](#与现有架构的关系)
- [配置项](#配置项)
- [实施计划](#实施计划)

---

## 背景与目标

### 问题

当前 UI（`app-ai-native`）通过 `server → gateway → device` 链路访问设备的 session 数据。session 交互过程中存在多个代码内容泄露向量：

1. **AI 回复中的代码段**：AI 助手在 TextPart 中以 markdown fenced code blocks 形式返回完整源代码
2. **Tool 调用结果**：ToolPart 的 `state.output` 包含工具执行返回的原始内容（文件内容、shell 输出、diff 等）
3. **Runtime 文件/差异接口**：`/api/v1/runtime/files/content`、`/api/v1/runtime/diff/content` 直接返回设备上的完整文件和 diff
4. **SSE 流式事件**：`message.part.updated` 事件流式推送 ToolPart/TextPart，包含实时产出的代码内容

这些向量在移动端场景下风险尤为突出：设备安全性低（丢失、公共网络）、屏幕可见性高（公共场合）、缺少物理安全边界。

### 目标

在 `server` 之前增加一层轻量级 **Session Proxy**，实现：

1. **全流量反向代理**：所有客户端（Mobile + Desktop）请求统一经过 proxy 转发到 server
2. **多维度代码过滤**：拦截 session 消息中的 TextPart（markdown code blocks）、ToolPart（tool output）、Runtime 文件接口返回值
3. **行为审计**：异步记录所有 session 相关请求/响应的审计日志到 PostgreSQL

```
所有客户端 ──HTTP──▶ Session Proxy ──HTTP──▶ Server ──HTTP──▶ Gateway ──▶ Device
                        │
                        ├─ 非 session 请求：透传（零改写）
                        ├─ session 消息：过滤 TextPart code blocks + ToolPart output
                        ├─ runtime 文件/diff：过滤文件内容和 diff
                        └─ 异步写入审计日志（PostgreSQL）
```

---

## 架构概览

```
┌─────────────────────────────────────────────────────────────┐
│                      所有客户端                               │
│               （Mobile / Desktop / 其他）                     │
└───────────────────────────┬─────────────────────────────────┘
                            │ HTTP
                            ▼
┌─────────────────────────────────────────────────────────────┐
│                    Session Proxy (:8090)                      │
│                                                              │
│  ┌──────────┐  ┌──────────────┐  ┌──────────────┐           │
│  │  Router   │  │ Filter Engine│  │ Audit Worker │           │
│  │          │  │              │  │              │           │
│  │ pass-thru│  │ Part-aware   │  │ async write  │           │
│  │ intercept│  │ filtering    │  │ to PostgreSQL│           │
│  └────┬─────┘  │              │  └──────────────┘           │
│       │        │ ┌──────────┐ │         │                    │
│       │        │ │TextPart  │ │         │                    │
│       │        │ │ code blks│ │         ▼                    │
│       │        │ ├──────────┤ │   ┌──────────┐              │
│       │        │ │ToolPart  │ │   │ PostgreSQL│              │
│       │        │ │ output   │ │   │ (独立db)  │              │
│       │        │ ├──────────┤ │   └──────────┘              │
│       │        │ │Runtime   │ │                               │
│       │        │ │ file/diff│ │                               │
│       │        │ └──────────┘ │                               │
│       │        │ SSE stream   │                               │
│       │        └──────────────┘                               │
│       │                                                       │
│       │  ┌──────────────────┐                                 │
│       │  │ Reverse Proxy    │                                 │
│       │  │ (to server)      │                                 │
│       │  └────────┬─────────┘                                 │
└───────┼───────────┼─────────────────────────────────────────┘
        │           │
        │           ▼
        │  ┌─────────────────┐
        │  │ costrict-web    │
        │  │ server          │
        │  └────────┬────────┘
        │           │
        │           ▼
        │  ┌─────────────────┐
        │  │ Gateway         │
        │  └────────┬────────┘
        │           │
        │           ▼
        │  ┌─────────────────┐
        │  │ Device (cs serve)│
        │  └─────────────────┘
        │
  pass-through 路径：直接转发，不做任何改写
  intercept 路径：转发 → 拦截响应 → 过滤 → 审计 → 返回
```

---

## 代码泄露向量分析

基于 `@opencode-ai/sdk` v2 的 Part 类型体系，session 交互中存在以下代码泄露向量：

### 向量 1：TextPart — AI 回复中的 Markdown Code Blocks

```typescript
// SDK 类型定义
type TextPart = {
  type: "text"
  text: string    // ← 可能包含 ```python\n...完整代码...\n```
}
```

AI 助手在回复中以 markdown fenced code blocks 形式输出完整源代码。这是最常见的泄露向量。

**触发场景**：用户要求 AI 编写/解释代码、代码审查、生成配置文件等。

### 向量 2：ToolPart — 工具调用返回的代码内容

```typescript
// SDK 类型定义
type ToolPart = {
  type: "tool"
  tool: string           // ← 工具名称，如 "read_file"、"edit_file"、"bash"
  state: ToolState
}

type ToolStateCompleted = {
  status: "completed"
  output: string         // ← 工具执行结果，可能包含完整文件内容、diff、shell 输出
  metadata: { ... }      // ← 可能包含额外上下文
}
```

工具调用的 `state.output` 是最大的泄露风险点，以下是高风险工具：

| 工具名称 | output 内容 | 风险等级 |
|----------|-------------|----------|
| `read_file` / `file_read` | 完整文件内容 | 🔴 高 |
| `edit_file` / `write_file` | 修改后的文件内容或 diff | 🔴 高 |
| `bash` / `shell` | 命令执行输出（可能包含代码） | 🟡 中 |
| `list_directory` | 目录列表 | 🟢 低 |
| `grep` / `search` | 搜索结果（代码片段） | 🟡 中 |
| `git_diff` / `diff` | 完整 diff 内容 | 🔴 高 |

**触发场景**：AI 读取文件理解代码、执行代码修改、运行 shell 命令查看输出。

### 向量 3：Runtime 文件/差异 API — 直接访问设备文件系统

```typescript
// device-client.ts 中的 runtime API
runtime.fileRead(path)         // GET /api/v1/runtime/files/content → 返回完整文件内容
runtime.diffContent(input)     // GET /api/v1/runtime/diff/content  → 返回 diff + before/after
```

这些接口直接返回设备上的文件内容和代码差异，绕过了 Part 体系。

**触发场景**：用户在 UI 中浏览文件、查看 diff、查看工作区变更。

### 向量 4：SSE 流式事件 — 实时推送 Part 更新

```typescript
// SSE 事件类型
type EventMessagePartUpdated = {
  type: "message.part.updated"
  properties: {
    part: Part   // ← 可能是 TextPart/ToolPart，包含实时产出的代码内容
  }
}
```

`message.part.updated` 事件在流式传输中逐步推送 Part 内容，ToolPart 的 `state` 会从 `pending` → `running` → `completed` 渐进更新，每次更新都可能包含新的代码片段。

### 向量 5：Conversation Diff — 会话关联的代码变更

```typescript
conversation.diff(id)    // GET /api/v1/conversations/:id/diff → 返回会话产生的代码变更
```

返回某次会话中 AI 产生的所有代码变更 diff。

### 向量 6：ToolResultPart — 独立的工具结果

```typescript
// csc transcriptReader 中的独立 Part 类型
type ToolResultPart = {
  type: "tool-result"
  toolUseID: string
  content: unknown    // ← 原始工具返回内容，可能包含代码
}
```

与 ToolPart 不同，`tool-result` 是 csc 内部解构的独立类型，`content` 字段直接包含工具返回的原始结果。

### 向量 7：ReasoningPart — AI 推理过程中的代码片段

```typescript
type ReasoningPart = {
  type: "reasoning"
  text: string        // ← 推理思考文本，可能包含引用的代码片段
}
```

AI 在思考过程中可能引用完整代码片段进行分析。虽然通常不如 TextPart/ToolPart 直接，但长推理中可能泄露代码。

### 向量 8：Shell 端点 — 直接命令执行输出

```
POST /api/v1/conversations/:id/shell
Body: { "command": "cat secret.py" }
Response: SSE 流 — 命令执行输出
```

与 ToolPart 的 bash 工具不同，这是直接执行 shell 命令的端点，输出走独立的 SSE 流而非 Part 体系。

### 向量 9：Terminal 流 — 终端 I/O

```
GET  /api/v1/terminal/:id/stream   — 终端输出流
POST /api/v1/terminal/:id/input    — 终端输入
```

用户可通过终端执行 `cat`、`less` 等命令查看文件内容，输出会出现在 terminal stream 中。

---

## 拦截范围

### Intercept 路径（需要过滤）

根据泄露向量分析，以下路径的响应需要拦截：

#### A. Session 消息路径（向量 1、2、4）

| 路径 | 方法 | 响应类型 | 处理方式 |
|------|------|----------|----------|
| `*/conversations/:id/messages` | GET | JSON | 解析 messages，遍历 parts[]，过滤 TextPart 和 ToolPart |
| `*/conversations/:id` | GET | JSON | 同上（conversation detail 可能含 preview） |
| `*/conversations` | GET | JSON | 列表接口，仅审计不过滤 |
| `*/events` | GET | SSE 流 | 逐 event 过滤 `message.part.updated` 中的 TextPart/ToolPart |
| `*/conversations/:id/prompt` | POST | JSON/SSE | 审计用户输入，过滤流式返回的 AI 回复 |
| `*/conversations/:id/diff` | GET | JSON | 过滤 diff 内容中的代码变更 |

#### B. Runtime 文件系统路径（向量 3）

| 路径 | 方法 | 响应类型 | 处理方式 |
|------|------|----------|----------|
| `*/runtime/files/content` | GET | JSON | 过滤 `content` 字段中的文件内容 |
| `*/runtime/diff/content` | GET | JSON | 过滤 `diff`、`before`、`after` 字段 |
| `*/runtime/diff` | GET | JSON | diff stat 通常无代码内容，仅审计 |

#### C. Shell / 禁用路径

| 路径 | 方法 | 响应类型 | 处理方式 |
|------|------|----------|----------|
| `*/conversations/:id/shell` | POST | SSE 流 | 命令输出按字符量阈值过滤（同 bash ToolPart 策略） |
| `*/terminal/*` | ALL | — | **直接禁用**，返回 403 `{ "error": "terminal disabled", "code": "TERMINAL_DISABLED" }` |
| `*/terminal/input-ws` | GET | WS | **直接禁用**，返回 403 `{ "error": "terminal disabled", "code": "TERMINAL_DISABLED" }` |

Proxy 对 UI 完全透明，不提供额外的能力查询端点。UI 侧在创建 terminal tab 时，proxy 返回 403 + `TERMINAL_DISABLED` 错误码，UI 检测后直接渲染"终端已禁用"提示（而非正常终端界面）。

#### D. 仅审计路径（请求/响应不含代码，但需记录行为）

| 路径 | 方法 | 审计原因 |
|------|------|----------|
| `*/runtime/files/content` | PUT | 请求体含写入的完整文件内容（`content` 字段），审计记录 |
| `*/conversations/:id/prompt` | POST | 请求体含用户 prompt，可能包含粘贴的代码 |
| `*/conversations/:id/command` | POST | slash 命令，可能触发代码相关操作 |
| `*/permissions` | GET | 权限请求中 `input` 字段可能含文件路径或代码上下文 |
| `*/questions` | GET | AI 提问中可能含代码上下文 |
| `*/conversations/:id/todo` | GET | Todo `content` 字段可能包含代码片段或引用 |
| `*/conversations/:id/tasks` | GET | Task 信息可能含代码变更摘要 |

#### E. Pass-through 路径（直接透传，无风险）

- `*/devices/*` — 设备管理
- `*/capability-items/*` — 能力项 store
- `*/workspaces/*` — 工作区
- `*/auth/*` — 认证
- `*/users/*` — 用户
- `*/runtime/files`（列表）、`*/runtime/files/meta`（元信息）、`*/runtime/find/file` — 无文件内容，仅路径/大小
- `*/runtime/health`、`*/runtime/config`、`*/runtime/path`、`*/runtime/vcs` — 无代码内容
- `*/agents/*` — Agent 元信息，无代码
- `*/conversations/:id/revert`、`*/conversations/:id/summarize` — 仅返回元数据
- `*/commands`、`*/commands/status` — 远程命令调度，无代码
- 其他所有未匹配 intercept 规则的路径

#### F. WebSocket 处理

路由层识别 `Connection: Upgrade` + `Upgrade: websocket` 请求：
- `*/terminal/input-ws` → 返回 403 `TERMINAL_DISABLED`
- 其他 WebSocket 路径 → 直接透传（`httputil.ReverseProxy` 内置支持 HTTP/1.1 WebSocket 代理）
- 不做缓冲、不做过滤、不做审计

---

## 详细设计

### 1. 反向代理核心

基于 `net/http/httputil.ReverseProxy` 实现，核心职责：

```
请求转发流程：
1. 接收客户端请求
2. 判断路由类型：
   - pass-through：直接转发，不缓存响应
   - intercept：使用 ResponseWriter wrapper 缓存响应体
3. 复制 request 到 target (SERVER_URL)
4. 转发请求到 server
5. 对 intercept 路径：
   a. 读取完整响应体（JSON）或逐行读取（SSE）
   b. 根据 Content-Type 调用对应过滤器
   c. 写入过滤后的响应
   d. 异步发送审计日志
```

### 2. Response 拦截 Writer

对 intercept 路径（非 SSE），使用自定义 `http.ResponseWriter` 缓存响应体：

```go
type interceptWriter struct {
    http.ResponseWriter
    buf        bytes.Buffer
    statusCode int
    maxBytes   int                       // 最大缓存字节数，超限降级
    overflowed bool                      // 已超限标记
}
```

- `WriteHeader()` 记录状态码
- `Write()` 写入 buffer，检查是否超过 `MAX_INTERCEPT_BODY_SIZE`
  - 未超限：正常缓存
  - 超限：设置 `overflowed = true`，后续写入直接透传到真实 ResponseWriter（不过滤）
  - 超限事件记 warn 日志（`path`, `size`）
- 请求完成后：`overflowed` 为 false → 从 buffer 过滤后写入；`overflowed` 为 true → 已透传，仅记录审计

### 3. Part-aware 过滤（核心）

过滤引擎需要理解 SDK 的 Part 类型体系，对不同类型实施不同过滤策略。过滤后会在 Part 的 `metadata` 中注入 `_filtered` 标记，供 UI 侧识别并做人性化渲染。

```
Part 类型判断流程：
1. 解析 part.type 字段
2. 根据类型分发：

   type = "text" (TextPart)
     → 提取 part.text
     → Markdown code block 解析 + 过滤
     → 替换 part.text
     → 注入 part.metadata._filtered

   type = "tool" (ToolPart)
     → 判断 part.state.status
     → status = "completed" 时：
        → 提取 part.state.output
        → 判断 part.tool 名称（read_file/edit_file/bash 等）
        → 应用对应的 tool output 过滤策略
        → 替换 part.state.output
        → 注入 part.state.metadata._filtered
     → status = "running" 时：
        → 提取 part.state.progress[]（如有）
        → 过滤 progress 中的代码片段
     → status = "pending" 时：
        → part.state.raw / part.state.input 包含调用参数
        → 可能含文件路径、搜索内容，审计记录

   type = "tool-result" (ToolResultPart)
     → csc transcriptReader 独立类型，content 字段含原始工具结果
     → 视为纯代码文本，整体应用过滤策略
     → 注入 metadata._filtered

   type = "reasoning" (ReasoningPart)
     → part.text 可能含代码推理过程中的代码片段
     → 按字符量阈值判断是否过滤（默认不过滤，可配置）
     → 超过阈值时整体 redact

   type = "snapshot" / "step-start"
     → part.snapshot 字段含代码快照，需过滤

   type = "patch"
      → part.files 列出修改文件路径，审计记录

   其他类型
      → 原样透传
```

### 4. Tool Output 过滤策略

与 TextPart 的 markdown code block 过滤不同，ToolPart 的 output 是纯文本（非 markdown），需要不同的过滤方式：

| 工具类型 | 过滤方式 |
|----------|----------|
| `read_file`、`write_file`、`edit_file` | 整体 redact/strip/mask |
| `bash`、`shell` | 按字符量阈值判断，超过阈值整体 redact（不做内容检测） |
| `diff`、`git_diff` | 解析 unified diff 格式，过滤代码行 |
| `grep`、`search` | 过滤搜索结果中的代码片段 |

Shell 输出不做内容是否为代码的检测，仅按长度判断：`len(output) > shell_char_threshold` 则整体 redact，否则放行。阈值默认 120 字符，可通过 `filter_rules.yaml` 配置。

### 5. SSE 流式过滤

SSE 响应不能缓存完整 body（流式传输），需要逐 event 处理。

#### SSE TextPart 的 code block 分片问题

SSE 场景下，每次 `message.part.updated` 事件携带的是该 Part 的**累积完整文本**（非 delta）。但流式生成过程中，code block 可能尚未闭合：

```
Event N:   text = "Here is the code:\n```python\ndef hello():"
Event N+1: text = "Here is the code:\n```python\ndef hello():\n    print('hi')"
Event N+2: text = "Here is the code:\n```python\ndef hello():\n    print('hi')\n```"
```

Event N 和 N+1 存在**未闭合的 code block**（只有开头的 ` ``` ` 没有结尾）。

#### 解决方案：逐 event 全量重解析 + streaming 状态标记

由于每个事件都携带完整累积文本，且 UI 侧也是全量替换渲染，因此：

```
SSE TextPart 过滤流程（每个 message.part.updated 事件）：

1. 提取 part.text（完整累积文本）
2. 全量重新解析，提取所有 code block：
   a. 已闭合的 code block（```...``` 配对）→ 应用过滤策略
      - 需过滤 → 标记 filtered / filtered-<lang>，内容替换为 [code filtered]
      - 无需过滤 → 原样保留
   b. 尾部未闭合的 code block（``` 开头但无配对闭合）→ 标记为 streaming 状态
      - 标记 streaming / streaming-<lang>，内容替换为空
      - 不做过滤判断（内容不完整，无法确定最终结果）
3. 组装过滤后文本，写回 part.text
4. 下一个事件到来时，文本更长，重新执行步骤 1-3：
   → 之前 streaming 的 block 此时可能已闭合，全量重解析：
     - 已闭合 + 需过滤 → 自动转为 filtered-<lang>
     - 已闭合 + 无需过滤 → 恢复正常渲染
   → 仍未闭合 → 保持 streaming 标记
5. 最终事件（stream 结束）时所有 block 都已闭合，过滤结果完整
```

#### Streaming 示例

```
Event N（code block 未闭合）:
  原始: "Here is the code:\n```python\ndef hello():"
  过滤: "Here is the code:\n```streaming-python\n```"

Event N+1（仍未闭合）:
  原始: "Here is the code:\n```python\ndef hello():\n    print('hi')"
  过滤: "Here is the code:\n```streaming-python\n```"

Event N+2（已闭合）:
  原始: "Here is the code:\n```python\ndef hello():\n    print('hi')\n```"
  过滤: "Here is the code:\n```filtered-python\n[code filtered]\n```"
```

UI 侧：
- 收到 `streaming-python` → 渲染"代码生成中..."骨架屏动画
- 收到 `filtered-python` → 渲染"代码已被过滤"提示卡片
- 两者视觉平滑过渡，无闪烁

#### 语言标识前缀总览

| 前缀 | 含义 | UI 渲染 |
|------|------|---------|
| `streaming` | 未闭合 code block，无语言标识 | 代码生成中...骨架屏 |
| `streaming-<lang>` | 未闭合 code block，有语言标识 | 代码生成中...（显示语言名）骨架屏 |
| `filtered` | 已过滤的 code block，无语言标识 | 代码已过滤提示卡片 |
| `filtered-<lang>` | 已过滤的 code block，有语言标识 | 代码已过滤提示卡片（显示语言名） |
| `<lang>` | 正常 code block | 正常语法高亮 |

UI 统一匹配规则：`streaming*` → 骨架屏，`filtered*` → 过滤卡片，其他 → 正常渲染。

**审计记录**：每个动作（文件访问、工具调用、code block 过滤）发生时立即写入审计日志。`session_id` 作为查询聚合维度，不需等待 SSE 连接结束。

#### SSE 完整处理流程

Proxy 需处理两类 SSE 流：

**类型 1：Event Bus SSE**（`GET /events`）
- 事件类型：`message.part.updated`、`message.updated`、`session.status` 等
- 处理：逐 event Part-aware 过滤（见上方流程）

**类型 2：Prompt SSE**（`POST /conversations/:id/prompt`、`POST /conversations/:id/shell`）
- 事件类型：`message`（assistant/tool_progress）、`result`、`control_request`、`system`
- csc 的 `session.ts` 中 `ssePrompt()` 直接将 AI 回复和工具进度以 SSE event 形式流式返回
- 处理：对 `event: message` 中 `type: 'assistant'` 的消息做 code block 过滤，对 `type: 'tool_progress'` 按阈值过滤

```
SSE 过滤流程：
1. 代理发起 SSE 请求到 server
2. 逐行读取 SSE 响应
3. 对每个 data: 行：
   a. 解析 JSON
   b. 判断 event.type / SSE event name
   c. Event Bus SSE：
      - "message.part.updated" → 按 Part 类型分发过滤
      - "message.updated" → 审计记录
      - 其他 → 原样转发
   d. Prompt SSE：
      - type: 'assistant' → 提取 content 过滤 code blocks
      - type: 'tool_progress' → 按字符量阈值过滤
      - type: 'result' / 'control_request' → 原样转发
   e. Shell SSE → 同 tool_progress 策略
4. Flush 每个完整 event 后
```

### 6. Runtime 文件接口过滤

针对直接返回文件内容的 Runtime API：

```
GET /api/v1/runtime/files/content?path=xxx
Response: { "content": "...完整文件内容...", "lines": 100, ... }

过滤方式：
1. 提取 content 字段
2. 视为纯代码文本，整体应用过滤策略
3. 保留行数和偏移信息（结构不变）
4. redact 时按行替换为 [filtered]
```

```
GET /api/v1/runtime/diff/content?path=xxx
Response: { "diff": "...unified diff...", "before": "...", "after": "..." }

过滤方式：
1. diff 字段：解析 unified diff，过滤 + 开头的代码行
2. before/after 字段：整体过滤
```

### 7. UI 侧过滤标记识别

Proxy 过滤后会在响应中注入可识别的标记，UI 侧检测后可做人性化渲染（如提示卡片、折叠面板等）。核心原则：**每个被过滤的代码片段独立标记**，而非整个 Part 一个标记。

#### TextPart — 每个代码块独立标记（`filtered-` 语言前缀）

TextPart 中可能包含多个 code block 与普通文本混排。Proxy 将被过滤的 code block 语言标识改为 `filtered` 或 `filtered-<lang>` 前缀（有语言标识时），UI 的 markdown 渲染器逐个识别并渲染为过滤提示卡片。

过滤前：

````markdown
Here is some explanation:

```python
def hello():
    print("secret code")
```

And some config:

```json
{"key": "value"}
```

Final note.
````

过滤后（假设 python 被过滤、json 在白名单放行、无语言标识 block 也被过滤）：

````markdown
Here is some explanation:

```filtered-python
[code filtered]
```

And some config:

```json
{"key": "value"}
```

A plain block:

```filtered
[code filtered]
```

Final note.
````

规则：有语言标识 → `filtered-<lang>`，无语言标识 → `filtered`。UI 统一按 `filtered` 前缀匹配即可。

UI 渲染时，markdown 渲染器检测到 `filtered-<lang>` 前缀即可：
- 不按语法高亮渲染
- 替换为"代码内容已被过滤"提示卡片
- 展示原始语言（从前缀提取）、原始大小等信息

#### ToolPart — `state.metadata._filtered` 标记

```json
{
  "type": "tool",
  "tool": "read_file",
  "state": {
    "status": "completed",
    "output": "[code filtered]",
    "metadata": {
      "_filtered": {
        "strategy": "redact",
        "reason": "tool_output",
        "toolName": "read_file",
        "originalSize": 3842
      }
    }
  }
}
```

#### Runtime 接口 — 顶层 `_filtered` 标记

```json
{
  "content": "[code filtered]",
  "lines": 100,
  "_filtered": {
    "strategy": "redact",
    "reason": "runtime_file",
    "path": "src/main.go",
    "originalSize": 5120
  }
}
```

#### `_filtered` 字段规范

| 字段 | 类型 | 说明 |
|------|------|------|
| `strategy` | string | 使用的过滤策略（`redact`） |
| `reason` | string | 触发原因：`code_block` / `tool_output` / `tool_result` / `tool_shell_threshold` / `reasoning_threshold` / `runtime_file` / `runtime_diff` / `shell_output` / `terminal_output` |
| `language` | string | 原始语言标识（仅 code_block，TextPart 通过 `filtered-<lang>` 前缀传递） |
| `toolName` | string | 工具名称（仅 tool_output） |
| `path` | string | 文件路径（仅 runtime） |
| `originalSize` | int | 原始内容字节数 |

#### UI 渲染建议

**TextPart（markdown 场景）**：
- Markdown 渲染器注册 `filtered-*` 语言识别规则
- 匹配到 `filtered-<lang>` 时，不渲染为代码块，改为渲染过滤提示卡片
- 卡片内展示：原始语言、过滤原因、"申请查看原始内容"链接（后续对接审批流程）

**ToolPart（工具卡片场景）**：
- Tool 卡片组件检测 `state.metadata._filtered` 存在
- 将工具 output 区域替换为过滤提示
- 展示工具名称、原始大小等上下文

**Runtime（文件查看器场景）**：
- 文件查看组件检测顶层 `_filtered`
- 整体替换为过滤提示视图

### 8. 优雅关闭

收到 SIGTERM/SIGINT 后按序关闭，不丢数据：

```
优雅关闭流程：
1. 停止接受新请求（关闭 listener）
2. 等待 in-flight 请求完成（超时 30s，由 SHUTDOWN_TIMEOUT 配置）
3. 关闭审计 channel（不再接受新条目）
4. 等待 worker 消费完 channel 中剩余条目
5. 执行最后一次 flush（确保审计数据写入 DB）
6. 关闭 DB 连接
7. 刷日志缓冲
8. 退出
```

配置项：

| 环境变量 | 默认值 | 说明 |
|---------|--------|------|
| `SHUTDOWN_TIMEOUT` | `30` | 优雅关闭最大等待时间（秒） |

### 9. 可测试性设计

模块拆分遵循**依赖倒置**，核心逻辑通过接口解耦，便于 mock 测试：

#### 接口定义

```go
// AuditStore — 审计存储抽象，worker 依赖此接口而非具体实现
type AuditStore interface {
    InsertBatch(entries []*AuditLog) error
    CleanBefore(before time.Time) error
}

// FilterStrategy — 过滤策略抽象
type FilterStrategy interface {
    Apply(content string, rules FilterRules) (string, []FilterAction, error)
}

// RuleProvider — 规则提供者，默认实现从 YAML 加载，测试可注入固定规则
type RuleProvider interface {
    Load() (*FilterRules, error)
}
```

#### 测试分层

| 层 | 范围 | 依赖 | 示例 |
|---|---|---|---|
| **纯函数单测** | markdown/code/diff/shell 过滤器 | 无外部依赖 | 输入 markdown → 提取 code blocks → 验证替换结果 |
| **策略单测** | strategy.go | 无外部依赖 | redact 策略：输入多行代码 → 验证输出 `[code filtered]` |
| **Part 路由单测** | part.go + tool.go | mock FilterStrategy | 构造各 Part 类型 JSON → 验证分发到正确过滤器 |
| **SSE 解析单测** | sse.go | 无外部依赖 | 构造 SSE 文本流 → 验证 event 解析 + 未闭合→闭合过渡 |
| **Writer 单测** | writer.go | `httptest.NewRecorder` | 写入超过 MAX_INTERCEPT_BODY_SIZE → 验证 overflowed 标记 + 透传行为 |
| **JWT 单测** | jwt.go | 无外部依赖 | 构造 JWT payload → 验证字段提取 / 无效 token / 字段缺失 |
| **规则加载单测** | rules.go | 临时 YAML 文件 | 文件缺失 → 默认值 / 格式错误 → 默认值 / 正常解析 |
| **Worker 单测** | worker.go | mock AuditStore | channel 满载 + AUDIT_SEND_TIMEOUT → 验证超时丢弃 / 正常批量写入 |
| **集成测试** | 完整过滤管线 | `httptest.Server` 模拟 upstream | 构造 JSON 响应 → 端到端验证过滤结果 + 审计记录 |

#### 关键测试用例

**MarkdownFilter**：
- 单个已闭合 code block（有/无语言标识）
- 多个 code block 与普通文本混排
- 尾部未闭合 code block → streaming 标记
- 未闭合→闭合过渡：Event N 为 streaming，Event N+1 为 filtered
- 嵌套 ``` 转义
- 空 code block

**DiffFilter**：
- 标准 unified diff 格式
- 多文件 diff（`--- a/` / `+++ b/`）
- 仅上下文行无代码变更 → 不过滤
- preserve_file_paths=true/false

**ShellFilter**：
- 恰好等于阈值 → 不过滤
- 超过阈值 1 字符 → 过滤
- 空输出

**interceptWriter**：
- 正常写入 → buffer 缓存 → 过滤后输出
- 写入恰好达到 MAX → 不触发降级
- 超过 MAX 1 字节 → overflowed=true，后续透传
- 多次 Write 调用累积超限

**JWT 中间件**：
- 有效 JWT → context 中有 user_id/user_name/user_sub
- 无 Authorization header → user_id 为空，请求放行
- 无效 base64 → user_id 为空，请求放行
- JWT payload 缺少 universal_id → user_id 为空

---

## 过滤引擎

### Content Filter 架构

```
┌─────────────────────────────────────────────────────┐
│                  Content Filter                      │
│                                                      │
│  输入: contentType + content                         │
│                                                      │
│  ┌─────────────────────────────────────────────────┐ │
│  │            Content Type Router                   │ │
│  │                                                   │ │
│  │  "markdown" → MarkdownFilter                     │ │
│  │  "code"     → CodeFilter                         │ │
│  │  "diff"     → DiffFilter                         │ │
│  │  "shell"    → ShellFilter                        │ │
│  └─────────────┬───────────────────────────────────┘ │
│                │                                      │
│                ▼                                      │
│  ┌─────────────────────────────────────────────────┐ │
│  │          Strategy Engine                         │ │
│  │                                                   │ │
│  │  redact / strip / mask / allow                   │ │
│  └─────────────┬───────────────────────────────────┘ │
│                │                                      │
│                ▼                                      │
│  输出: filteredContent + filterActions[]             │
└─────────────────────────────────────────────────────┘
```

### Markdown Code Block 解析器（TextPart）

使用正则提取 fenced code blocks：

```
匹配模式：```(\w*)\n([\s\S]*?)```
- group 1: 语言标识（可选）
- group 2: 代码内容
```

支持嵌套场景处理（代码块内部可能包含 ``` 转义）。

### Unified Diff 解析器（Diff Content）

```
解析 unified diff 格式：
--- a/path/to/file
+++ b/path/to/file
@@ -10,6 +10,8 @@
 context line
-removed line
+added line      ← 这些行包含实际代码变更

过滤策略：
- redact：替换 `+`/`-` 行内容为 [filtered]，保留行号信息
- strip：移除所有 `+`/`-` 行，只保留上下文行
- mask：保留行号和结构，掩码标识符
```

### 过滤策略

| 策略 | 行为 | 状态 |
|------|------|------|
| `redact`（默认） | 替换内容为 `[code filtered]`，保留结构 | ✅ 首批实现 |
| `strip` | 完整移除内容（包括结构标记） | TODO |
| `mask` | 保留结构，但将标识符/字符串/数字替换为 `***` | TODO |
| `allow` | 白名单内容直接放行，非白名单 redact | TODO |

### 规则配置

`filter_rules.yaml`（可选，文件不存在时使用代码内硬编码默认值，记 warn 日志继续运行）：

```yaml
# 默认策略
default_strategy: redact

# Part 类型级别策略覆盖
part_strategies:
  text:
    default_strategy: redact          # TextPart 中 markdown code blocks
  tool:
    default_strategy: redact          # ToolPart output
    tool_overrides:                   # 按工具名覆盖策略
      list_directory: redact          # 目录列表 redact
      bash: redact                    # Shell 输出 redact
      # TODO: strip / mask 策略实现后可按工具名配置
  runtime:
    file_content:
      default_strategy: redact        # /runtime/files/content
    diff_content:
      default_strategy: redact        # /runtime/diff/content

# 语言白名单（allow 策略时生效，TODO）
allowed_languages:
  - json
  - yaml
  - toml
  - markdown
  - plaintext

# 工具白名单（白名单内的工具 output 不过滤，TODO）
allowed_tools: []

# Shell 输出字符量阈值
shell_char_threshold: 120

# ReasoningPart 字符量阈值（超过阈值则过滤，-1 表示不过滤）
reasoning_threshold: -1

# 内容长度阈值（超过阈值则过滤，-1 表示全部过滤）
content_length_threshold: -1

# 是否保留语言标识 / 文件路径等元信息
preserve_language_hint: true
preserve_file_paths: true       # diff 中保留文件路径

# 过滤后替换文本
redact_placeholder: "[code filtered]"

# 是否对 pass-through 请求也做审计
audit_passthrough: false
```

---

## 审计日志

审计回答三个核心问题：**谁发了什么**、**谁看了什么**、**看的频率**。

### 用户身份识别

Proxy 从请求的 `Authorization: Bearer <token>` 中解码 JWT payload（Casdoor access token），提取用户身份。仅做 base64 解码，不做签名验证（签名验证由 server 侧完成）。

提取的字段：

| JWT Claim | 审计字段 | 说明 |
|-----------|---------|------|
| `sub` | `user_sub` | Casdoor sub，如 `org/alice` |
| `universal_id` | `user_id` | Casdoor 稳定 ID，主键级标识 |
| `preferred_username` | `user_name` | 显示名 |

JWT 解码失败时，`user_id` 记录为空字符串，审计日志照常写入（可通过 `client_ip` 追溯）。

### 设计原则

1. **避免 JSON 文本存储**：结构化字段全部提到列级别，便于直接查询和建索引
2. **按动作即时记录**：每次文件访问、工具调用、code block 过滤等动作发生时立即写入审计日志，不等待 SSE 连接结束；`session_id` 作为查询聚合维度
3. **文件访问独立建表**：`audit_files` 关联表，支持"谁看了某文件"的反向查询
4. **所有 intercept 路径默认记录**：无论是否触发过滤。`audit_passthrough` 仅控制 pass-through 路径

### Schema

审计使用独立 PostgreSQL 数据库（与 server 共用数据库实例，通过 `DATABASE_URL` 中指定不同 dbname），使用 GORM AutoMigrate 管理表结构。

```sql
-- 主表：每个动作一条记录（文件访问、工具调用、code block 过滤等）
CREATE TABLE IF NOT EXISTS audit_logs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- 谁
    user_id         VARCHAR(191) NOT NULL,    -- Casdoor universal_id（JWT payload 解码）
    user_name       VARCHAR(255),             -- Casdoor preferred_username
    user_sub        VARCHAR(255),             -- Casdoor sub（如 org/alice）
    device_id       VARCHAR(191),
    client_ip       VARCHAR(45) NOT NULL,
    client_type     VARCHAR(32) NOT NULL DEFAULT '',  -- 从 UA 提取：mobile / desktop / bot / ''

    -- 看了什么 / 发了什么
    api_path        VARCHAR(512) NOT NULL,
    method          VARCHAR(16) NOT NULL,
    session_id      VARCHAR(191),
    conversation_id VARCHAR(191),
    status_code     INTEGER,

    -- 用户发了什么
    request_summary TEXT,                     -- 请求体前 500 字符（prompt、写入内容等）

    -- 用户看了什么（结构化统计）
    files_count     INTEGER DEFAULT 0,        -- 本次动作涉及的文件数
    tools_count     INTEGER DEFAULT 0,        -- 本次动作涉及的工具调用数
    code_blocks_total   INTEGER DEFAULT 0,    -- 原始 code block 总数
    code_blocks_filtered INTEGER DEFAULT 0,   -- 被过滤的 code block 数

    -- 过滤结果
    filtered        BOOLEAN DEFAULT FALSE,

    -- 元信息
    latency_ms      INTEGER,
    is_sse          BOOLEAN DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 文件访问明细表：支持"谁看了某文件"的反向查询
CREATE TABLE IF NOT EXISTS audit_files (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    audit_id        UUID NOT NULL REFERENCES audit_logs(id),
    file_path       VARCHAR(1024) NOT NULL,
    access_type     VARCHAR(32) NOT NULL,     -- read / write / diff / search
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 工具访问明细表：支持按工具类型统计
CREATE TABLE IF NOT EXISTS audit_tools (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    audit_id        UUID NOT NULL REFERENCES audit_logs(id),
    tool_name       VARCHAR(191) NOT NULL,    -- read_file / bash / edit_file 等
    filtered        BOOLEAN DEFAULT FALSE,    -- 该工具输出是否被过滤
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 索引
CREATE INDEX idx_audit_user_time ON audit_logs(user_id, created_at);
CREATE INDEX idx_audit_session ON audit_logs(session_id);
CREATE INDEX idx_audit_path_time ON audit_logs(api_path, created_at);
CREATE INDEX idx_audit_device ON audit_logs(device_id);
CREATE INDEX idx_audit_filtered ON audit_logs(filtered);
CREATE INDEX idx_audit_files_path ON audit_files(file_path);
CREATE INDEX idx_audit_files_audit ON audit_files(audit_id);
CREATE INDEX idx_audit_tools_name ON audit_tools(tool_name);
CREATE INDEX idx_audit_tools_audit ON audit_tools(audit_id);
```

### client_type 提取规则

从 `User-Agent` header 提取简短标识，不存原始 UA 字符串：

| UA 特征 | client_type |
|---------|-------------|
| 包含 `Mobile`、`iPhone`、`Android` | `mobile` |
| 包含 `bot`、`crawl`、`spider` | `bot` |
| 其他 | `desktop` |

### 查询示例

```sql
-- 某用户近 7 天的访问次数（按天）
SELECT date(created_at) AS day, count(*) AS requests
FROM audit_logs
WHERE user_id = ? AND created_at > NOW() - INTERVAL '7 days'
GROUP BY day ORDER BY day;

-- 某用户访问了哪些 session，各多少次
SELECT session_id, count(*) AS times,
       min(created_at) AS first_access, max(created_at) AS last_access
FROM audit_logs
WHERE user_id = ? AND session_id != ''
GROUP BY session_id ORDER BY times DESC;

-- 谁看了某个文件
SELECT al.user_id, al.user_name, al.created_at, af.access_type
FROM audit_files af
JOIN audit_logs al ON al.id = af.audit_id
WHERE af.file_path = ?
ORDER BY al.created_at DESC;

-- 哪些文件被查看最频繁
SELECT file_path, count(*) AS views
FROM audit_files
GROUP BY file_path ORDER BY views DESC
LIMIT 20;

-- 哪些用户触发了过滤
SELECT user_id, user_name, count(*) AS filtered_requests
FROM audit_logs
WHERE filtered = 1
GROUP BY user_id ORDER BY filtered_requests DESC;

-- 某用户使用了哪些工具
SELECT at.tool_name, count(*) AS times, sum(CASE WHEN at.filtered THEN 1 ELSE 0 END) AS filtered
FROM audit_tools at
JOIN audit_logs al ON al.id = at.audit_id
WHERE al.user_id = ?
GROUP BY at.tool_name ORDER BY times DESC;
```

### SSE 审计策略

SSE 连接（`GET /events`、`POST /prompt`）中的事件**即时记录**，不等待连接结束：

- 每收到一个事件，提取 `session_id` 后立即构建审计 entry 并发送到 channel
- `audit_files` 和 `audit_tools` 明细同步写入，关联到对应 audit_id
- `is_sse = true`
- 查询时通过 `session_id` 聚合即可还原某 session 的完整访问链路

```
SSE 事件流即时审计：

  event(session=abc, tool=read_file, file="src/main.go")
    → audit_logs{session_id="abc", api_path="/events", is_sse=true}
    → audit_tools{audit_id, tool_name="read_file"}
    → audit_files{audit_id, file_path="src/main.go", access_type="read"}

  event(session=xyz, tool=bash)
    → audit_logs{session_id="xyz", api_path="/events", is_sse=true}
    → audit_tools{audit_id, tool_name="bash"}

  event(session=abc, textPart with 3 code blocks, 2 filtered)
    → audit_logs{session_id="abc", code_blocks_total=3, code_blocks_filtered=2, filtered=true}
```

### 异步写入

```
审计日志写入流程：
1. 动作发生时构建 AuditEntry
2. 发送到 buffered channel（容量 4096），带最大阻塞超时（默认 5s，由 AUDIT_SEND_TIMEOUT 配置）
   - 超时未发送 → 丢弃该条目，记 error 日志
   - 保证代理转发不被审计拖垮
3. 后台 worker goroutine 消费 channel
4. 批量 INSERT（每 100 条或每 5s 刷一次）
5. 写入失败时记 error 日志，重试当前批次（最多 3 次），仍失败则丢弃并记 error
```

### 数据保留

- 默认保留 90 天
- 可配置 `AUDIT_RETENTION_DAYS`
- 后台 goroutine 每天凌晨清理过期记录（同时清理 audit_files 和 audit_tools 关联记录）

---

## API 设计

### Proxy 自身 API

以下端点由 proxy 自身处理，不转发到 server：

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/health` | 存活检查（Liveness），始终返回 200 |
| GET | `/health/ready` | 就绪检查（Readiness），检测 DB 连通性 + server 可达性 |

响应格式：

```json
// GET /health
{ "status": "ok" }

// GET /health/ready（正常）
{ "status": "ok", "db": "ok", "upstream": "ok" }

// GET /health/ready（异常）
{ "status": "degraded", "db": "ok", "upstream": "error: connection refused" }
```

`/health/ready` 返回 503 当 `db` 或 `upstream` 任一不可达。

> **审计查询 API 暂不实现**：审计日志直接查 PostgreSQL，后续按需开放。
> **规则热加载暂不实现**：修改规则后重启 proxy 即可，后续按需加 `POST /proxy/rules/reload` 和文件监听。

---

## 数据流设计

### Part-aware JSON 响应过滤流程

```
Client Request (conversations/:id/messages)
     │
     ▼
┌─────────────┐
│   Router     │  匹配 intercept 路径
└─────┬───────┘
      │
      ▼
┌─────────────────────────┐
│  转发到 Server           │
│  (使用 interceptWriter)  │
└─────┬───────────────────┘
      │ 响应写入 buffer
      ▼
┌─────────────────────────┐
│  解析 JSON               │
│  遍历 messages[].parts[] │
└─────┬───────────────────┘
      │
      ▼ (每个 part)
┌─────────────────────────┐
│  Part Type Router        │
│                          │
│  part.type == "text"     │
│  → MarkdownFilter        │
│    → code block 提取     │
│    → 策略应用            │
│                          │
│  part.type == "tool"     │
│  → ToolOutputFilter      │
│    → 判断 tool 名称      │
│    → 判断 state.status   │
│    → 过滤 state.output   │
│                          │
│  part.type == "snapshot" │
│  → CodeFilter            │
│    → 过滤 snapshot 内容  │
│                          │
│  其他 type               │
│  → 原样透传              │
└─────┬───────────────────┘
      │
      ▼
┌─────────────────────────┐
│  重序列化 JSON            │
│  写入真实 ResponseWriter  │
└─────┬───────────────────┘
      │
      ▼ (async)
┌─────────────────────────┐
│  Audit Worker            │
│  → PostgreSQL            │
└─────────────────────────┘
```

### SSE 流式过滤流程（Part-aware）

```
Client Request (Accept: text/event-stream)
     │
     ▼
┌─────────────┐
│   Router     │  匹配 */events
└─────┬───────┘
      │
      ▼
┌──────────────────────────────┐
│  发起 SSE 请求到 Server       │
│  逐行读取响应                  │
└─────┬────────────────────────┘
      │
      ▼  (循环)
┌──────────────────────────────┐
│  读取一行                      │
│  空行 = event 边界 → flush     │
│  data: 行 → 解析 JSON         │
└─────┬────────────────────────┘
      │
      ▼
┌──────────────────────────────┐
│  Event Type Router            │
│                               │
│  type == "message.part.updated"│
│  → 提取 properties.part       │
│  → Part Type Router（同上）    │
│  → 重写 part → 新 JSON        │
│                               │
│  type == "message.updated"    │
│  → 审计记录                    │
│  → 原样转发                    │
│                               │
│  其他 type                    │
│  → 原样转发                    │
└─────┬────────────────────────┘
      │
      ▼
   写回客户端 + flush
```

### Runtime 文件接口过滤流程

```
Client Request (runtime/files/content?path=xxx)
     │
     ▼
┌─────────────┐
│   Router     │  匹配 intercept 路径
└─────┬───────┘
      │
      ▼
┌─────────────────────────┐
│  转发到 Server           │
│  (使用 interceptWriter)  │
└─────┬───────────────────┘
      │
      ▼
┌─────────────────────────┐
│  解析 JSON               │
│  提取 content 字段        │
└─────┬───────────────────┘
      │
      ▼
┌─────────────────────────┐
│  CodeFilter              │
│  - 按 code 整体过滤      │
│  - redact: 按行替换      │
│  - mask: 保留结构掩码    │
│  - 保留行数/偏移信息     │
└─────┬───────────────────┘
      │
      ▼
┌─────────────────────────┐
│  重序列化 JSON            │
│  写入 ResponseWriter      │
└─────┬───────────────────┘
      │
      ▼ (async)
┌─────────────────────────┐
│  Audit Worker            │
│  source_type: runtime_file│
│  → PostgreSQL            │
└─────────────────────────┘
```

---

## 目录结构

```
proxy/
├── cmd/
│   └── main.go                        # 入口：解析配置、初始化、启动
├── internal/
│   ├── config.go                      # 环境变量 + 配置加载
│   ├── router.go                      # 路由注册：pass-through vs intercept
│   ├── proxy/
│   │   ├── reverse.go                 # 反向代理核心（ReverseProxy 封装）
│   │   ├── writer.go                  # interceptWriter（响应拦截 wrapper）
│   │   └── writer_test.go             # interceptWriter 单测（含超限降级）
│   ├── filter/
│   │   ├── engine.go                  # 过滤引擎入口（Content Type Router）
│   │   ├── engine_test.go             # 引入口集成测试
│   │   ├── markdown.go                # Markdown code block 解析 + 过滤（TextPart）
│   │   ├── markdown_test.go           # code block 提取 / 闭合 / 未闭合 / 多 block
│   │   ├── code.go                    # 纯代码文本过滤（ToolPart output、Runtime file）
│   │   ├── code_test.go               # redact 按行替换
│   │   ├── diff.go                    # Unified diff 解析 + 过滤
│   │   ├── diff_test.go               # diff 格式解析 / +行过滤 / 路径保留
│   │   ├── shell.go                   # Shell 输出字符量阈值过滤
│   │   ├── shell_test.go              # 阈值边界 / 恰好等于 / 超过
│   │   ├── strategy.go                # 策略实现（redact/strip/mask/allow）
│   │   ├── strategy_test.go           # 各策略输入输出
│   │   ├── part.go                    # Part-aware 路由（按 Part 类型分发）
│   │   ├── part_test.go               # 各 Part 类型分发正确性
│   │   ├── tool.go                    # Tool output 过滤（按工具名判断内容类型）
│   │   ├── tool_test.go               # 各工具名策略覆盖
│   │   ├── rules.go                   # 规则加载（启动时加载，暂不支持热加载）
│   │   ├── rules_test.go              # YAML 解析 / 文件缺失走默认 / 格式错误
│   │   ├── sse.go                     # SSE 流式过滤（逐 event + Part-aware）
│   │   └── sse_test.go                # SSE event 解析 / 未闭合→闭合过渡
│   ├── audit/
│   │   ├── model.go                   # AuditLog 模型定义
│   │   ├── store.go                   # GORM PostgreSQL 初始化 + CRUD
│   │   ├── worker.go                  # channel + goroutine 异步写入
│   │   └── worker_test.go             # channel 满载超时 / 批量聚合（用 mock store）
│   ├── middleware/
│   │   ├── jwt.go                     # JWT base64 解码中间件
│   │   └── jwt_test.go                # 正常解码 / 无 header / 无效 token / 字段缺失
│   └── logger/
│       └── logger.go                  # 结构化日志
├── filter_rules.yaml                  # 过滤规则配置文件
├── Dockerfile                         # 多阶段构建
├── go.mod
└── go.sum
```

---

## 与现有架构的关系

### 组件交互

```
┌──────────────────────────────────────────────────────────┐
│                    costrict-web (go.work)                  │
│                                                           │
│  proxy/          server/          gateway/                │
│  (新 module)     (现有)           (现有)                  │
│      │               │               │                    │
│      │  HTTP 转发     │  HTTP 转发    │  yamux 隧道        │
│      └──────►────────┘──────►────────┘                    │
│                                                           │
│  client/go      (go.work 已有)                             │
└──────────────────────────────────────────────────────────┘
```

### 改动范围

| 组件 | 改动 |
|------|------|
| `proxy/` | **新增** — 整个 proxy module |
| `go.work` | 添加 `use ./proxy` |
| `docker-compose.yml` | 添加 proxy service |
| `server/` | **无改动** — proxy 对 server 完全透明 |
| `gateway/` | **无改动** |
| `app-ai-native` | 环境变量 `API_BASE_URL` 指向 proxy 地址 |

### 对 server 的透明性

Proxy 对 server 来说就是一个普通 HTTP 客户端。所有 headers（包括 `Authorization`、`X-Workspace-Directory` 等）原样透传。Server 无需感知 proxy 的存在。

---

## 配置项

| 环境变量 | 默认值 | 说明 |
|---------|--------|------|
| `LISTEN_ADDR` | `:8090` | Proxy 监听地址 |
| `SERVER_URL` | `http://server:8080` | 后端 server 地址 |
| `DATABASE_URL` | — | PostgreSQL 连接串（与 server 同实例，独立 dbname，如 `postgres://costrict:pwd@host:5432/costrict_audit?sslmode=disable`）**必填** |
| `DB_MAX_OPEN_CONNS` | `10` | 数据库最大打开连接数 |
| `DB_MAX_IDLE_CONNS` | `5` | 数据库最大空闲连接数 |
| `DB_CONN_MAX_LIFETIME` | `300` | 数据库连接最大存活时间（秒） |
| `AUDIT_RETENTION_DAYS` | `90` | 审计日志保留天数 |
| `FILTER_RULES_PATH` | `./filter_rules.yaml` | 过滤规则文件路径（文件不存在时使用默认值） |
| `MAX_INTERCEPT_BODY_SIZE` | `52428800` | intercept 路径响应体最大缓存字节数（默认 50MB），超限降级为透传 |
| `AUDIT_CHANNEL_SIZE` | `4096` | 审计日志 channel 容量 |
| `AUDIT_BATCH_SIZE` | `100` | 批量写入大小 |
| `AUDIT_FLUSH_INTERVAL_MS` | `5000` | 批量写入刷新间隔 |
| `AUDIT_SEND_TIMEOUT` | `5` | 审计 channel 发送最大阻塞超时（秒），超时丢弃 |
| `LOG_LEVEL` | `info` | 日志级别 |
| `SHUTDOWN_TIMEOUT` | `30` | 优雅关闭最大等待时间（秒） |
| `FILTER_FAILURE_MODE` | `block` | 过滤失败时行为：`block` 返回 502，`passthrough` 降级透传原始响应 + 记 error 日志 |

---

## 启动校验

Proxy 启动时执行以下校验，失败则直接退出：

1. `DATABASE_URL` 非空且格式合法
2. PostgreSQL 连通性测试（`db.Ping()`）
3. GORM AutoMigrate 执行成功（创建/更新表结构）
4. `SERVER_URL` 可达（可选，失败仅 warn，不阻塞启动）

---

## 文件日志

使用 `zap` + `lumberjack` 结构化日志（复用 `gateway/internal/logger` 方案），输出两个日志文件：

| 文件 | 级别 | 说明 |
|------|------|------|
| `app.log` | DEBUG 及以上 | 全量日志，包含请求转发、过滤命中、审计写入等 |
| `error.log` | ERROR 及以上 | 仅错误，自动附带调用栈 |

### 日志轮转

- 单文件最大 100MB，保留 7 天，最多 10 个备份，自动 gzip 压缩
- 通过 `LOG_DIR` 环境变量指定目录

### 记录场景

| 场景 | 级别 | 示例字段 |
|------|------|----------|
| 请求转发完成 | INFO | `method`, `path`, `status`, `latency_ms` |
| 过滤命中 | INFO | `session_id`, `filter_type`, `blocks_filtered` |
| 审计写入失败 | ERROR | `error`, `audit_id`，附带调用栈 |
| 审计写入重试 | WARN | `retry_count`, `batch_size` |
| 审计条目丢弃 | ERROR | `reason`（channel_full / db_unavailable） |
| Proxy → Server 连接失败 | ERROR | `error`, `upstream_url`，附带调用栈 |
| 过滤引擎异常 | ERROR | `error`, `path`，附带调用栈 |
| SSE 连接断开 | INFO | `session_id`, `duration_ms`, `events_processed` |
| JWT 解码失败 | WARN | `error`（不阻断请求） |
| 规则配置文件异常 | WARN | `error`, `path`（降级为默认规则） |

---

## 错误处理与降级原则

Proxy 的核心原则：**宁可服务中断，不可泄露代码**。

| 场景 | 行为 |
|------|------|
| Proxy → Server 连接失败 | 返回 502 `{ "error": "proxy: upstream unavailable", "code": "UPSTREAM_ERROR" }` |
| 过滤引擎 JSON 解析失败 | 返回 502 `{ "error": "proxy: filter parse error", "code": "FILTER_ERROR" }`，记录 error 日志 |
| 过滤引擎正则匹配异常 | 返回 502 `{ "error": "proxy: filter engine error", "code": "FILTER_ERROR" }`，记录 error 日志 |
| 审计 channel 满载 | 阻塞等待最多 5s，超时则丢弃并记 error 日志，不拖垮代理转发 |
| 数据库写入失败 | 降级为文件日志，proxy 继续运行 |
| SSE 连接中途断开 | 已发送的 event 不回滚，审计记录断点状态 |
| Intercept 响应体超限（>MAX_INTERCEPT_BODY_SIZE） | 降级为透传（不过滤），记 warn 日志，审计记录 |
| 规则配置文件缺失 | 使用代码内默认值，记录 warn 日志，proxy 正常启动 |
| 规则配置文件格式错误 | 使用代码内默认值，记录 warn 日志，proxy 正常启动 |

所有 proxy 自身返回的错误响应格式统一为：

```json
{
  "error": "人类可读的错误描述",
  "code": "ERROR_CODE"
}
```

错误码枚举：

| code | HTTP | 触发场景 |
|------|------|----------|
| `UPSTREAM_ERROR` | 502 | Server 不可达 |
| `FILTER_ERROR` | 502 | 过滤引擎解析/处理异常 |
| `TERMINAL_DISABLED` | 403 | Terminal 功能被禁用 |

---

## 实施计划

详见 `todo/SESSION_PROXY_PROGRESS.md`（按依赖关系排序的 9 阶段任务列表）。
