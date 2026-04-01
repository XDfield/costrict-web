# AI Agent Runtime 集成设计

> **实现状态：提案中**
>
> - 状态：设计阶段
> - 目标位置：`server/internal/agentrt/`
> - 依赖框架：[trpc-group/trpc-agent-go](https://github.com/trpc-group/trpc-agent-go)
> - 参考实现：[openclaw](https://github.com/openclaw/openclaw) `src/tasks/` + `src/agents/`

---

## 文档索引

| 文档 | 说明 |
|------|------|
| [01-overview.md](./01-overview.md) | 背景动机、核心需求、框架选型对比 |
| [02-architecture.md](./02-architecture.md) | 架构总览、模块划分、与现有系统关系 |
| [03-data-models.md](./03-data-models.md) | TaskRecord / SubagentRunRecord 定义、状态机 |
| [04-components.md](./04-components.md) | AgentRuntime / TaskRegistry / SubagentRegistry / EventBus |
| [05-flows.md](./05-flows.md) | 任务下发、Subagent 委托、回调决策、取消等流程 |
| [06-api.md](./06-api.md) | REST API 路由、请求响应示例、SSE 事件流 |
| [07-integration.md](./07-integration.md) | 与 trpc-agent-go Runner/Callback/Session/编排的集成 |
| [08-database.md](./08-database.md) | 数据库表结构 DDL |
| [09-roadmap.md](./09-roadmap.md) | P0-P4 分阶段实施计划 |
