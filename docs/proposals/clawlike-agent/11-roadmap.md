# 11. 实施计划

## P0 — 基础骨架（预计 3 天）

**目标**：Agent 能对话，替换 NoopMessageHandler

- [ ] 引入 `trpc-agent-go` 到 `go.work`
- [ ] 创建 `server/internal/clawagent/` 包
- [ ] 实现 `ClawAgentRuntime` 基础结构
- [ ] 实现 `channel.MessageHandler` 接口（Handler）
- [ ] 使用平台默认 Provider（`config.LLMConfig`）创建 Model
- [ ] 初始化 `memory/postgres` 后端（配置 DSN + 表名）
- [ ] 初始化 `session/postgres` 后端（配置 DSN + 表名）
- [ ] 创建基础 LLMAgent + Runner
- [ ] 实现事件流消费 → 渠道回复
- [ ] 集成到 `main.go`（替换 NoopMessageHandler）
- [ ] 端到端测试：企微发消息 → Agent 回复

**验收标准**：通过企微给 bot 发消息，能收到 AI 回复。重启服务后历史对话上下文不丢失。

## P1 — Soul + Memory（预计 1.5 天）

**目标**：Agent 有人格、有记忆

- [ ] 创建 `agent_personas` 表 + migration
- [ ] 实现 `PersonaManager`（加载/保存/构建 instruction）
- [ ] 默认 Persona 模板
- [ ] 注册 memory tools 到 Agent（memory_add/update/delete/search/clear/load）
- [ ] 配置 `WithPreloadMemory(10)` + 自动记忆提取（DefaultExtractor）
- [ ] 配置 `WithSoftDelete(true)` + `WithMinSearchScore()` TF-IDF 参数
- [ ] 实现 per-user AgentFactory（加载 Persona + Memory service）
- [ ] Persona REST API

**验收标准**：Agent 用自定义人格回复；跨会话、跨重启能记住用户偏好（Memory 持久化到 PostgreSQL）。

## P2 — Provider 管理（预计 2 天）

**目标**：用户可自配多模型

- [ ] 创建 `agent_providers` 表 + migration
- [ ] 实现 `ProviderManager`（CRUD + CreateModel）
- [ ] API Key AES-256-GCM 加密/解密
- [ ] 在 AgentFactory 中加载用户 Providers
- [ ] 默认 Provider 降级逻辑
- [ ] Provider REST API + 测试连通性

**验收标准**：用户通过 API 配置 DeepSeek Provider，Agent 使用 DeepSeek 模型回复。

## ~~P3 — Skill 集成（预计 1.5 天）~~ **暂时禁用**

**目标**：从 Capability Hub 按需加载 Skill（内存模式）

**禁用原因**：简化首期实施，优先验证核心对话能力和设备委托机制

**未来实施**（当需要时）：
- [ ] 实现 `DBSkillRepository`（实现 `skill.Repository` 接口）
- [ ] 从 `capability_items.content` 按需加载 + 内存缓存
- [ ] 缓存增量失效（item 增/改/删钩子）
- [ ] 注册 skill tools 到 Agent（skill_load 可用，skill_run 不可用）
- [ ] 在 item handler 钩子中触发 `Invalidate()`

**验收标准**：在 Capability Hub 创建 skill，Agent 能 `skill_load` 并使用其内容指导操作。

## P4 — Workspace 委托 + DeviceProxyClient（预计 4 天）

**目标**：Agent 能以 Workspace 为单位委托任务到 device，支持异步结果回传和崩溃恢复

- [ ] 实现 `DeviceProxyClient` 接口集（封装 cs-cloud localserver API）
  - [ ] `CreateConversation()` / `SendAsyncPrompt()` — 创建设备端会话 + 发送异步任务
  - [ ] `GetConversationMessages()` — 拉取对话结果
  - [ ] `GetRuntimeHealth()` / `ListFiles()` / `GetFileContent()` — Workspace 探测
  - [ ] `SubscribeEvents()` (SSE) — 实时事件流消费（按 conversation_id 过滤）
  - [ ] `GetPermissionRequests()` / `ReplyPermission()` — 权限交互
- [ ] 实现 `workspace_list` tool（查询 `models.Workspace`）
- [ ] 实现 `workspace_delegate` tool（阻塞 + 非阻塞两种模式）
- [ ] 实现 `workspace_create` tool（新建 Workspace + Directory）
- [ ] 扩展 `cloud.ConnectionManager` 添加 `ProxyHTTP()` 方法（HTTP 代理到设备端隧道）
- [ ] 创建 `agent_workspace_tasks` 表（含状态机 + delivery_status + progress_summary）
- [ ] 实现内部 `EventBus`（announce 协调 + 超时检测，不对外暴露 SSE）
- [ ] 实现 `watchAndAnnounce()` — 消费 cs-cloud SSE → 更新任务状态 → announce 回传
- [ ] 实现 `announceToAgent()` — 通过 `runner.Run()` 注入结果到 Agent session + 指数退避重试
- [ ] 实现崩溃恢复 — 启动时扫描非终态任务，重新订阅 SSE 或标记 lost
- [ ] 实现超时检测 goroutine — 定期扫描 `running` 任务检查 `last_event_at`
- [ ] 文档补充：前端通过 gateway proxy 直接消费 cs-cloud SSE（零新代码）

**验收标准**：
1. 阻塞模式：企微对话 → Agent 委托任务 → 同步等待返回结果
2. 非阻塞模式：Agent 委托后继续对话 → 设备完成后 announce 回传 → Agent 基于结果做下一步决策
3. 崩溃恢复：服务重启后 running 任务自动恢复或标记 lost
4. 前端通过 `/cloud/device/{id}/proxy/api/v1/events` 实时观察委托任务进展

## P5 — AI 驱动的企微通知处理（预计 7 天）

**目标**：将现有权限请求和问卷的按钮卡片交互升级为 AI 驱动的自然语言交互

**核心特性**：本阶段为 ClawAgent 的核心价值体现，必须实施。

**依赖**：P4 Workspace 委托 + DeviceProxyClient（复用设备代理调用机制）

### Phase 5.1: 基础集成（2 天）

- [ ] 在 `Dispatcher` 中添加 `EventForwarder`（事件转发器）
- [ ] 在 `ClawAgent` 中添加 `EventHandler`（事件处理器）
- [ ] 实现基础的事件描述和 AI 对话注入
- [ ] 创建 `ai_interaction_preferences` 表 + migration
- [ ] 添加用户设置：AI 交互模式开关
- [ ] 集成到现有 dispatcher 流程（保留传统卡片作为降级方案）

### Phase 5.2: 意图识别（2 天）

- [ ] 实现 `IntentHandler` 意图识别模块
- [ ] 复用 `DeviceProxyClient` 进行设备代理调用
- [ ] 添加权限批准/拒绝的意图处理（`/api/v1/permissions/{id}/reply`）
- [ ] 添加问卷回答的意图处理（`/api/v1/questions/{id}/reply`）
- [ ] 实现置信度阈值机制（低于阈值时主动澄清）
- [ ] 创建 `ai_event_conversations` 表记录对话历史

### Phase 5.3: 高级特性（2 天）

- [ ] 批量权限处理（一次性处理多个相关权限）
- [ ] 澄清问题生成（模糊表达时主动询问）
- [ ] 基于用户历史的个性化建议（结合 Memory 系统）
- [ ] 错误处理和重试机制
- [ ] 与 Persona 系统集成（个性化语气）

### Phase 5.4: 优化与监控（1 天）

- [ ] 性能优化（事件转发延迟 < 500ms）
- [ ] 监控指标（处理成功率、平均置信度、用户满意度）
- [ ] 日志优化（审计日志 + AI 决策日志）
- [ ] 用户反馈收集机制
- [ ] 文档完善

**验收标准**：
1. 权限事件触发 AI 自然语言对话，用户能用自然语言批准/拒绝（如"批准"、"OK，让他执行"、"拒绝，这个命令危险"）
2. 问卷事件由 AI 处理，理解用户自然语言答案（如"选第一个"、"用生产环境"、"随便"）
3. AI 能处理批量权限，主动澄清模糊表达
4. 提供个性化建议和基于历史的学习能力
5. 置信度低于阈值时主动澄清，避免误操作
6. 完整审计日志可追溯所有 AI 决策

**详见**：[ai-driven-notification-handling.md](./ai-driven-notification-handling.md)

## P6 — OpenAI 兼容 API + Web Chat（预计 2 天）

**目标**：支持第三方客户端和浏览器对话

- [ ] 封装 trpc-agent-go `server/openai/`
- [ ] OpenAI API 认证中间件（JWT + API Key）
- [ ] Web Chat REST API（`POST /api/clawagent/chat` + SSE）
- [ ] Session 历史 API
- [ ] 前端 demo 验证

**验收标准**：ChatBox 等第三方客户端能通过 OpenAI API 对接；浏览器能 Web Chat 对话。

## P7 — 生产化（预计 2 天）

- [ ] 并发安全验证（多用户同时对话）
- [ ] PostgreSQL 连接池调优（Memory + Session 后端共享连接池）
- [ ] 监控指标（对话量、延迟、成功率、token 消耗）
- [ ] 日志优化（区分 Agent 思考日志和用户可见日志）
- [ ] 错误恢复（Runner 异常后的处理）
- [ ] 横向扩展压测（多实例 + 共享 PostgreSQL）
- [ ] 部署文档更新

## 总计

| 阶段 | 预估工时 | 累计 | 状态 |
|------|---------|------|------|
| P0 基础骨架 | 3 天 | 3 天 | ✅ 必需 |
| P1 Soul+Memory | 1.5 天 | 4.5 天 | ✅ 必需 |
| P2 Provider | 2 天 | 6.5 天 | ✅ 必需 |
| ~~P3 Skill~~ | ~~1.5 天~~ | ~~8 天~~ | ❌ **暂时禁用** |
| P4 Workspace 委托 | 4 天 | 10.5 天 | ✅ 必需 |
| P5 AI 驱动通知处理 | 7 天 | 17.5 天 | ✅ **核心特性** |
| P6 OpenAI+Web | 2 天 | 19.5 天 | ✅ 必需 |
| P7 生产化 | 2 天 | 21.5 天 | ✅ 必需 |

**首期实施总工期：21.5 天**（不含暂时禁用的 P3 Skill 集成）

> **重要**：P5（AI 驱动通知处理）是 ClawAgent 的核心特性，体现 AI 助手从"被动响应"升级为"主动理解"的能力跃迁，不可省略或推迟。
