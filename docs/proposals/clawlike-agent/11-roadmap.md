# 11. 实施计划

## P0 — 基础骨架（预计 3 天）

**目标**：Agent 能对话，替换 NoopMessageHandler

- [ ] 引入 `trpc-agent-go` 到 `go.work`
- [ ] 创建 `server/internal/clawagent/` 包
- [ ] 实现 `ClawAgentRuntime` 基础结构
- [ ] 实现 `channel.MessageHandler` 接口（Handler）
- [ ] 使用平台默认 Provider（`config.LLMConfig`）创建 Model（**API Key 走加密链路**，见 [04-providers.md §4.7](./04-providers.md)）
- [ ] 初始化 `session/postgres` 后端（配置 DSN + 表名）
- [ ] **sessionID 命名规范**：`agent:clawagent:{channelType}:{chatID}:{userID}` 等可解析结构（见 [12-session-design.md §12.3.1](./12-session-design.md)）
- [ ] **创建 `agent_session_meta` 表 + migration**（见 [12-session-design.md §12.3.2](./12-session-design.md)）
- [ ] **Handler 入口接入 `resolveActiveSession()` + 基础 freshness 检查**（daily reset 单聊，详见 [12-session-design.md §12.3.3](./12-session-design.md)）
- [ ] 创建基础 LLMAgent + Runner
- [ ] 实现事件流消费 → 渠道回复
- [ ] 集成到 `main.go`（替换 NoopMessageHandler）
- [ ] 端到端测试：企微发消息 → Agent 回复

**验收标准**：通过企微给 bot 发消息，能收到 AI 回复。重启服务后历史对话上下文不丢失。sessionID 在 DB 中以 `agent:clawagent:...:v1` 格式存储。

## P1 — Soul + Memory（预计 1.5 天）

**目标**：Agent 有人格、有记忆

- [ ] 创建 `agent_personas` + `agent_memories` 表 + migration
- [ ] 实现 `PersonaManager`（加载/保存/构建 instruction，拼入 memory）
- [ ] 默认 Persona 模板
- [ ] 实现 `MemoryManager`（Load/Save/Refresh，4KB 截断）
- [ ] 实现 memory 异步刷新：`streamResponse` 检测到 `IsFinalResponse()` 后 `go memoryMgr.Refresh()`
- [ ] LLM 合并 prompt 模板 + 失败容忍（保留旧值，不重试）
- [ ] 注册 memory 工具（仅 `memory_view` + `memory_update`，备用）
- [ ] 实现 per-user AgentFactory（加载 Persona + memory 全量拼到 system prompt）
- [ ] Persona REST API + Memory REST API（`GET/PUT /api/clawagent/memory`）

**验收标准**：Agent 用自定义人格回复；跨会话、跨重启能记住用户偏好（Memory 持久化到 PostgreSQL）；用户可通过 API 查看/编辑 memory。

## P2 — Provider 管理（预计 2 天）

**目标**：用户可自配多模型

- [ ] 创建 `agent_providers` 表 + migration
- [ ] 实现 `ProviderManager`（CRUD + CreateModel）
- [ ] API Key AES-256-GCM 加密/解密（密钥从 `CLAWAGENT_ENCRYPTION_KEY` 读取，setup.go 启动时校验非空）
- [ ] **默认 Provider 也走加密链路**：`platformDefault()` 就地 `encryptAPIKey(cfg.LLM.APIKey)`，不进入运行时明文
- [ ] 在 AgentFactory 中加载用户 Providers（持久化为空时降级到默认）
- [ ] cfg 明文 API Key 不直接进入 Model 构造路径（统一通过 `decryptAPIKey()`）
- [ ] Provider REST API + 测试连通性

**验收标准**：
1. 用户通过 API 配置 DeepSeek Provider，Agent 使用 DeepSeek 模型回复
2. 未配置任何 Provider 的用户也能正常对话（用平台默认，加密后走相同解密路径）
3. `CLAWAGENT_ENCRYPTION_KEY` 未设置时进程拒绝启动

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
- [ ] **任务表字段：`agent_session_base_key`（不含版本后缀）**，announce 时通过 `sessionMeta.ResolveActive()` 动态解析当前 active sessionID（见 [12-session-design.md §12.3.6](./12-session-design.md)）
- [ ] 实现内部 `EventBus`（announce 协调 + 超时检测，不对外暴露 SSE）
- [ ] 实现 `watchAndAnnounce()` — 消费 cs-cloud SSE → 更新任务状态 → announce 回传
- [ ] 实现 `announceToAgent()` — 通过 `runner.Run()` 注入结果到 **当前 active sessionID**（动态解析）+ 指数退避重试 + session pruned 时标记 `delivery_status=failed`
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

**依赖**：P4 Workspace 委托 + DeviceProxyClient（复用设备代理调用机制）+ **P4.5 Session 维护**（事件专用 session 需要 freshness 管理）

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

## P4.5 — Session 维护（预计 1 天，新增）

**目标**：补齐 session 生命周期管理，参考 openclaw 的 freshness / reset / prune 模型

**详见**：[12-session-design.md](./12-session-design.md)

**P0 阶段已交付**（增量，含在 P0 工时内）：
- [ ] sessionID 命名规范（`agent:clawagent:...:v{N}` 前缀）
- [ ] `agent_session_meta` 表 + migration
- [ ] Handler 入口接入 `resolveActiveSession()` + freshness 检查

**P4 阶段已交付**（增量，含在 P4 工时内）：
- [ ] 任务表字段 `agent_session_base_key`（DDL 已采用此命名）— announce 时通过 `sessionMeta.ResolveActive()` 动态解析 active 版本
- [ ] announce 时通过 `sessionMeta.ResolveActive()` 动态解析 active session

**P4.5 本阶段新增**：
- [ ] 实现 `SessionMetaManager`（Create / Active / Archive / ResolveActive / Get）
- [ ] freshness 双模式：daily reset（单聊默认 4am）+ idle reset（群聊 30min / 通知 60min / 任务 120min）
- [ ] session_data 压缩（compaction goroutine）：`maybeCompact()` 实现（乐观锁并发安全 + message_count 二次检查 + `SummarizeSession` LLM 调用 + `session_summary` event 格式 + `Replace` 原子写入 + 失败容错）
- [ ] token 估算机制：`estimateTokens()` 启发式（`len(json)/4`）+ 每轮 `updateSessionMeta()` 异步更新 `agent_session_meta.token_estimate`
- [ ] 扩展 SessionService wrapper：`Get()` / `Replace()` / `AppendEventWithEstimate()`
- [ ] prune goroutine：每小时扫描归档过期 session（默认 30 天清理）+ per-user 容量上限（默认 200）
- [ ] 群聊 Persona/Memory 混合模式：群 session 共享，userContext + memory 按发送者隔离
- [ ] 配置项 + 启动校验（`clawagent.session.*`）
- [ ] 监控指标（session 数 / 平均 tokens / reset 频率 / prune 量 / compaction 触发数）

**验收标准**：
1. 单聊对话跨日（>4am）后，新消息自动开 v2 session，旧 v1 归档
2. 群聊 30 分钟无活动后 reset，事件通知 60 分钟无活动后 reset，任务回传 120 分钟无活动后 reset
3. session_data 超 8000 tokens 自动压缩，输出 `type: "session_summary"` 事件 + 保留最近 10 轮原文
4. 压缩期间新消息到达时，静默放弃本次压缩，下轮重试（不丢失数据）
5. 30 天归档 session 物理删除
6. 委托任务 announce 时，即使原 session 已 reset，仍能定位到当前 active session 回传结果

## 总计

| 阶段 | 预估工时 | 累计 | 状态 |
|------|---------|------|------|
| P0 基础骨架（含 session 命名 + meta 表 + freshness 检查 +0.5d） | 3.5 天 | 3.5 天 | ✅ 必需 |
| P1 Soul+Memory | 1.5 天 | 5 天 | ✅ 必需 |
| P2 Provider | 2 天 | 7 天 | ✅ 必需 |
| ~~P3 Skill~~ | ~~1.5 天~~ | ~~8.5 天~~ | ❌ **暂时禁用** |
| P4 Workspace 委托（含 announce session 动态解析 base_key +0.5d） | 4.5 天 | 11.5 天 | ✅ 必需 |
| **P4.5 Session 维护（新增）** | 1 天 | 12.5 天 | ✅ **必需** |
| P5 AI 驱动通知处理 | 7 天 | 19.5 天 | ✅ **核心特性** |
| P6 OpenAI+Web | 2 天 | 21.5 天 | ✅ 必需 |
| P7 生产化 | 2 天 | 23.5 天 | ✅ 必需 |

**首期实施总工期：23.5 天**（不含暂时禁用的 P3 Skill 集成）

> **重要**：
> - P5（AI 驱动通知处理）是 ClawAgent 的核心特性，体现 AI 助手从"被动响应"升级为"主动理解"的能力跃迁，不可省略或推迟。
> - P4.5（Session 维护）是补齐基础能力的必需阶段，否则长期运行会出现 token 失控、announce 丢失、DB 无限增长等稳定性问题。
