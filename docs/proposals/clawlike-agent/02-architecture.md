# 2. 整体架构

## 2.1 架构总览

```
┌─ 用户 ────────────────────────────────────────────────────────────┐
│  企微 / Web Chat / OpenAI 兼容客户端                                │
└──────────────┬──────────────────────────┬──────────────────────────┘
               │ IM 消息                   │ HTTP / SSE
               ↓                          ↓
┌────────────────────────────────────────────────────────────────── ┐
│                    costrict-web server (Gin)                      │
│                                                                   │
│  ┌─────────────┐  ┌──────────────────┐  ┌──────────────────────┐ │
│  │ Channel     │  │ ClawAgent        │  │ OpenAI Server        │ │
│  │ Adapters    │  │ Handler          │  │ /v1/chat/completions │ │
│  │ (wecom...)  │  │ (替换 NoopHandler)│  │ (trpc-agent-go)     │ │
│  └──────┬──────┘  └────────┬─────────┘  └──────────┬───────────┘ │
│         │                  │                       │             │
│  ┌──────┴──────────────────┴───────────────────────┴───────────┐ │
│  │              ClawAgent Runtime (新模块)                      │ │
│  │              server/internal/clawagent/                      │ │
│  │                                                              │ │
│  │  ┌──────────┐  ┌───────────┐  ┌──────────┐  ┌────────────┐ │ │
│  │  │ Persona  │  │ Memory    │  │~~Skills~~│  │ Providers  │ │ │
│  │  │ Manager  │  │ Manager   │  │(暂时禁用)│  │ Manager    │ │ │
│  │  │ (GORM)   │  │ (TEXT)    │  │          │  │ (GORM)     │ │ │
│  │  └──────────┘  └───────────┘  └──────────┘  └────────────┘ │ │
│  │                                                              │ │
│  │  ┌─────────────────────────────────────────────────────────┐ │ │
│  │  │           trpc-agent-go Runner + LLMAgent               │ │ │
│  │  │  - Session Service (postgres)                           │ │ │
│  │  │  - AgentFactory (per-user 动态构建)                      │ │ │
│  │  │  - Event Channel → 流式输出                              │ │ │
│  │  │  - Tools: memory_view/update, workspace_*               │ │ │
│  │  │  - Post-run hook: 异步 LLM 合并 memory                  │ │ │
│  │  └─────────────────────────────────────────────────────────┘ │ │
│  │                                                              │ │
│  │  ┌─────────────────────────────────────────────────────────┐ │ │
│  │  │  EventBus (内部专用 — 不对外暴露 SSE)                    │ │ │
│  │  │  - 委托任务事件扇出（announce goroutine + 超时检测）      │ │ │
│  │  │  - 崩溃恢复协调                                          │ │ │
│  │  └─────────────────────────────────────────────────────────┘ │ │
│  │                                                              │ │
│  │  ┌─────────────────────────────────────────────────────────┐ │ │
│  │  │           DeviceProxyClient (对接 cs-cloud)              │ │ │
│  │  │  - CreateConversation / SendAsyncPrompt                  │ │ │
│  │  │  - GetConversationMessages / GetEvents (SSE)            │ │ │
│  │  │  - GetRuntimeHealth / ListFiles / GetFileContent        │ │ │
│  │  │  - GetPermissionRequests / ReplyPermission              │ │ │
│  │  └─────────────────────────────────────────────────────────┘ │ │
│  └──────────────────────────────────────────────────────────────┘ │
│                              │                                    │
│  ┌───────────────────────────┴──────────────────────────────────┐│
│  │  现有模块                                                     ││
│  │  cloud/ (SSE+Gateway)  channel/  models/  config/           ││
│  │  workspaces (models.Workspace + WorkspaceDirectory)          ││
│  └──────────────────────────────────────────────────────────────┘│
│                              │                                    │
│  ┌───────────────────────────┴──────────────────────────────────┐│
│  │  PostgreSQL                                                  ││
│  │  agent_personas | agent_providers | agent_workspace_tasks   ││
│  │  agent_memories | clawagent_sessions (auto)                ││
│  └──────────────────────────────────────────────────────────────┘│
└──────────────────────────────────┬────────────────────────────────┘
                                   │ HTTP Proxy (via cloud tunnel)
                                   ↓
┌────────────────────────────────────────────────────────────────── ┐
│                    Device (cs-cloud / opencode)                   │
│  localserver HTTP API (/api/v1/*)                                │
│  - conversations API (创建/发送/获取消息)                          │
│  - events SSE (实时状态推送)                                      │
│  - workspace runtime (文件/VCS/diff)                              │
│  - permissions (权限请求/回复)                                     │
│  Workspace: X-Workspace-Directory header 指定工作区               │
└────────────────────────────────────────────────────────────────── ┘
```

## 2.2 模块划分

新增 `server/internal/clawagent/` 包：

```
server/internal/clawagent/
├── runtime.go              # ClawAgentRuntime 主入口
├── handler.go              # 实现 channel.MessageHandler 接口（含 memory 异步刷新触发）
├── persona.go              # Persona 管理
├── persona_models.go       # Persona GORM 模型
├── memory.go               # Memory 管理（单 TEXT 字段，LLM 合并刷新）
├── memory_models.go        # Memory GORM 模型
├── providers.go            # Provider 管理
├── provider_models.go      # Provider GORM 模型
├── ~~skills.go~~           # ~~DBSkillRepository~~ **暂时禁用**
├── device_proxy.go         # DeviceProxyClient（对接 cs-cloud localserver API）
├── workspace_delegate.go   # Workspace 委托工具（调用 DeviceProxyClient + announce）
├── event_bus.go            # 内部 EventBus（announce 回调 + 崩溃恢复协调，不对外暴露 SSE）
├── workspace_tools.go      # workspace_list / workspace_create 工具
├── event_handler.go        # AI 驱动通知处理：接收 Dispatcher 转发的事件并注入 AI 对话
├── intent_handler.go       # AI 驱动通知处理：识别用户意图并调用 DeviceProxyClient
├── tools.go                # 内置 Tool 注册
├── openai_server.go        # OpenAI 兼容 Server 封装
└── setup.go                # 初始化、Runner/Agent 构建（postgres 后端配置）
```

### 关键设计决策

| 组件 | 选型 | 理由 |
|------|------|------|
| Memory 后端 | 自建 `agent_memories` 表（单 TEXT） | 简化：单用户单条 memory，全量注入 system prompt，无关键词检索/向量检索 |
| Memory 刷新 | 每轮 final response 后异步 LLM 合并 | 不阻塞回复，失败保留旧值 |
| Session 后端 | `session/postgres` | 会话上下文持久化，服务重启不丢失对话历史 |
| ~~Skill 后端~~ | ~~自定义 `DBSkillRepository`~~ | **暂时禁用** |
| 设备委托 | `DeviceProxyClient` → cs-cloud API | 通过 HTTP 代理调用设备端 localserver API，不依赖专用 RPC 协议 |
| 委托单位 | Workspace（非 Device） | 以 workspace 为核心，workspace 绑定 device |
| 实时事件流 | 复用 cs-cloud `/api/v1/events` SSE 透传 | 前端直连 gateway proxy，服务端不新建 SSE 端点 |
| EventBus | 内部专用（不对外暴露） | 用于 announce 回调、崩溃恢复协调、超时检测 |
| 横向扩展 | 全状态持久化到 PostgreSQL | 进程内无不可恢复状态，实例可随时增减 |

## 2.3 集成方式

### 替换 NoopMessageHandler

**当前** (`server/cmd/api/main.go`):
```go
channelModule = channel.New(db, &channel.NoopMessageHandler{}, ...)
```

**目标**:
```go
clawRT, err := clawagent.New(db, cfg, cloudMgr)
// ...
channelModule = channel.New(db, clawRT, ...)
```

### 路由注册

```go
// 在 main.go 的 authed group 下
agentAPI := clawRT.RegisterRoutes(authed)

// OpenAI 兼容 API (可选独立 group)
openaiGroup := r.Group("/")
openaiGroup.Use(clawRT.AuthMiddleware())
openaiGroup.Any("/v1/*path", clawRT.OpenAIHandler())
```

## 2.4 与现有系统的关系

| 现有模块 | 关系 | 说明 |
|----------|------|------|
| `channel/` | **复用** | ClawAgent 实现 `channel.MessageHandler` 接口 |
| `channel/adapters/wecom*/` | **复用** | 企微 webhook + 长连接机器人适配器，零改动 |
| `cloud/` | **扩展** | 新增 `ProxyHTTP()` 方法，通过隧道代理 HTTP 请求到设备端 |
| `models.Workspace` | **复用** | 委托以 workspace 为单位，复用现有 Workspace + WorkspaceDirectory 模型 |
| `config/` | **扩展** | 新增 ClawAgent 相关配置项 |
| `dispatcher/` | **核心扩展** | 新增 `EventForwarder`，将 permission/question 事件转发到 ClawAgent（保留传统按钮卡片作为降级） |
| `notification/` | **复用** | AI 处理失败时回退到现有按钮卡片通知机制 |
| `models.CapabilityItem` | ~~**只读**~~ | **暂时禁用**：Skill 内容从 `capability_items.content` 按需加载到内存 |
| `middleware/` | **复用** | Casdoor JWT 认证，通过 `middleware.UserIDKey` 获取 userID |
| `llm/` | **不复用** | Chat 调用由 trpc-agent-go Model 替代 |

## 2.5 核心数据流

```
用户消息 (企微/Web/OpenAI API)
    │
    ├─[渠道路径] channel.Adapter.ParseInbound → InboundMessage
    │                                        → ClawAgentHandler.Handle()
    │
    ├─[OpenAI路径] POST /v1/chat/completions → OpenAI Server → Runner.Run()
    │
    └─[Web路径] POST /api/clawagent/chat     → ClawAgentHandler.Chat()
                                                    │
                                                    ↓
                                    ClawAgentRuntime.HandleMessage()
                                    ├── 从 Casdoor context 获取 userID
                                    ├── 构造 baseKey（agent:clawagent:{chan}:{chat}:{user}）
                                    ├── resolveActiveSession(userID, baseKey) → activeSessionID
                                    │   ├── 首次: 建 v1
                                    │   ├── stale: 归档旧版 + 建 v(N+1)
                                    │   └── fresh: 复用当前 active
                                    ├── memoryMgr.Load(userID) → memory content
                                    ├── 调用 runner.Run(ctx, userID, sessionID, msg)
                                    │   ├── AgentFactory 加载该用户的 Persona
                                    │   ├── AgentFactory 加载该用户的 Providers
                                    │   ├── AgentFactory 把 memory 拼到 system prompt
                                    │   ├── ~~AgentFactory 注入 DBSkillRepository~~ (暂时禁用)
                                    │   └── AgentFactory 注入 workspace_* tools
                                    │       └── workspace_delegate → DeviceProxyClient
                                    │           ├── CreateConversation (cs-cloud API)
                                    │           ├── SendAsyncPrompt
                                    │           ├── 消费 SSE 事件流 (GetEvents)
                                    │           └── GetConversationMessages
                                    │
                                    └── 消费 event channel
                                        ├── 流式文本 → 回复用户
                                        ├── tool_call → 执行工具
                                        │   ├── memory_view/update (备用，主路径走 hook)
                                        │   ├── ~~skill_load~~     → (暂时禁用)
                                        │   └── workspace_delegate → DeviceProxyClient → cs-cloud
                                        │       ├── (阻塞) SSE 等待 → 同步返回结果
                                        │       └── (非阻塞) 立即返回 → 异步 watchAndAnnounce:
                                        │           ├── 消费 cs-cloud SSE → 更新 progress
                                        │           ├── session.idle → announceToAgent()
                                        │           │   → runner.Run() 注入结果到 Agent session
                                        │           └── EventBus 内部扇出（超时检测等）
                                        ├── tool_result → 继续对话
                                        └── IsFinalResponse → go memoryMgr.Refresh()
                                                ├── LLM 合并: 旧 memory + 本轮对话 → 新 memory
                                                ├── Save(userID, newMemory)（失败保留旧值）
                                                └── 不阻塞，不影响主回复流

前端实时监控:
  GET /cloud/device/{id}/proxy/api/v1/events → cs-cloud SSE 透传（按 conversation_id 过滤）
```
