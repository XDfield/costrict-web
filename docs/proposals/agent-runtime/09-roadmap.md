# 9. 实施计划

## P0 — 基础骨架（预计 3-5 天）

- [ ] 引入 `trpc-agent-go` 作为 Go module 依赖
- [ ] 实现 `AgentRuntime` + `TaskRegistry` 基础 CRUD
- [ ] 实现单 Agent（LLMAgent）+ Runner 集成
- [ ] 实现 REST API：`POST /agent/tasks`、`GET /agent/tasks/:id`、`POST /agent/tasks/:id/cancel`
- [ ] 实现 `consumeEvents` 消费 trpc-agent-go event channel
- [ ] 数据库 migration：`agent_tasks` 表
- [ ] 基本集成测试

## P1 — Subagent 编排（预计 3-5 天）

- [ ] 实现 `SubagentRegistry`
- [ ] 实现 ChainAgent / ParallelAgent 多 Agent 编排
- [ ] 实现 `BeforeAgent` / `AfterAgent` callback 注册
- [ ] 数据库 migration：`agent_subagent_runs` 表
- [ ] 子任务父子关系追踪
- [ ] API：`GET /agent/tasks/:id/children`、`GET /agent/subagents`

## P2 — Announce 回调机制（预计 2-3 天）

- [ ] 实现 `announceToParent()` — 子任务完成回调主 Agent
- [ ] 实现指数退避重试（最多 5 次）
- [ ] 实现启动恢复：`recoverLostTasks()` + `PendingAnnouncements()`
- [ ] 交付状态流转管理

## P3 — 实时事件推送（预计 2-3 天）

- [ ] 实现 `EventBus` 发布/订阅
- [ ] 实现 SSE endpoint：`GET /agent/tasks/:id/events`
- [ ] 与现有 Cloud SSE 基础设施对接（可选）
- [ ] 前端事件消费 demo

## P4 — 高级编排 + 生产化（预计 3-5 天）

- [ ] GraphAgent 复杂工作流支持
- [ ] DB 持久化 Session（替换 InMemory）
- [ ] 动态 Agent 创建（`NewRunnerWithAgentFactory`）
- [ ] 任务超时自动处理
- [ ] 监控指标（任务量、延迟、成功率）
- [ ] 压力测试和并发安全验证
