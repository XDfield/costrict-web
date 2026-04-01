# 2. 整体架构

## 2.1 架构总览

```
┌──────────────────────────────────────────────────────────────┐
│                      costrict-web server                     │
│                        (Gin + GORM)                          │
├──────────────────────────────────────────────────────────────┤
│                                                              │
│  ┌──────────────┐  ┌───────────────┐  ┌──────────────────┐  │
│  │  REST API    │  │  WebSocket    │  │  SSE             │  │
│  │  /agent/*    │  │  /ws/agent    │  │  /agent/events   │  │
│  └──────┬───────┘  └───────┬───────┘  └────────┬─────────┘  │
│         │                  │                    │            │
│  ┌──────┴──────────────────┴────────────────────┴─────────┐  │
│  │               Agent Runtime Layer                      │  │
│  │                (server/internal/agentrt/)               │  │
│  │                                                        │  │
│  │  ┌────────────────┐       ┌─────────────────────────┐  │  │
│  │  │  TaskRegistry  │       │  SubagentRegistry       │  │  │
│  │  │  (GORM 持久化) │       │  (内存 + DB 持久化)     │  │  │
│  │  └───────┬────────┘       └──────────┬──────────────┘  │  │
│  │          │                           │                 │  │
│  │  ┌───────┴───────────────────────────┴──────────────┐  │  │
│  │  │              AgentRuntime (胶水层)               │  │  │
│  │  │  - 任务生命周期管理                              │  │  │
│  │  │  - Event 消费与广播                              │  │  │
│  │  │  - 子任务完成回调                                │  │  │
│  │  └───────────────────┬──────────────────────────────┘  │  │
│  │                      │                                 │  │
│  │  ┌───────────────────┴──────────────────────────────┐  │  │
│  │  │            trpc-agent-go Runner                  │  │  │
│  │  │                                                  │  │  │
│  │  │  ┌──────────┐ ┌───────────┐ ┌────────────────┐  │  │  │
│  │  │  │LLMAgent  │ │ChainAgent │ │ParallelAgent   │  │  │  │
│  │  │  │          │ │(链式)     │ │(并行)          │  │  │  │
│  │  │  └──────────┘ └───────────┘ └────────────────┘  │  │  │
│  │  │  ┌──────────┐ ┌───────────────────────────────┐  │  │  │
│  │  │  │GraphAgent│ │Callbacks: Before/AfterAgent   │  │  │  │
│  │  │  │(图式)    │ │Before/AfterTool               │  │  │  │
│  │  │  └──────────┘ └───────────────────────────────┘  │  │  │
│  │  └──────────────────────────────────────────────────┘  │  │
│  └────────────────────────────────────────────────────────┘  │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐  │
│  │     现有模块: handlers / services / worker / llm       │  │
│  └────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────┘
```

## 2.2 模块划分

新增 `server/internal/agentrt/` 包，内部模块如下：

```
server/internal/agentrt/
├── runtime.go              # AgentRuntime 主入口，胶水层
├── task_registry.go        # TaskRegistry 任务注册表
├── task_models.go          # Task 相关数据模型
├── subagent_registry.go    # SubagentRegistry 子代理注册表
├── subagent_models.go      # Subagent 相关数据模型
├── event_bus.go            # EventBus 事件总线
├── announce.go             # 子任务完成回调通知逻辑
├── callbacks.go            # trpc-agent-go callback 注册
└── setup.go                # 初始化、Runner/Agent 构建
```

## 2.3 与现有系统的关系

| 现有模块 | 关系 | 说明 |
|----------|------|------|
| `server/internal/llm/` | 复用 | AgentRuntime 复用现有 LLM Client 配置（API Key、Base URL、Model），通过 trpc-agent-go 的 `openai.New()` 适配 |
| `server/internal/handlers/` | 扩展 | 新增 `agent_handler.go` 注册 `/agent/*` 路由 |
| `server/internal/services/` | 扩展 | 新增 `agent_service.go` 封装业务逻辑 |
| `server/internal/worker/` | 并行 | AgentRuntime 有独立的异步执行机制，不依赖现有 worker |
| `server/internal/config/` | 扩展 | 新增 Agent 相关配置项 |
| `server/internal/database/` | 复用 | TaskRecord 通过 GORM 持久化到现有 PostgreSQL |
| `server/internal/cloud/` | 协作 | Agent 事件可通过现有 Cloud SSE 通道推送到前端 |
