# 1. 概述

## 1.1 背景与动机

costrict-web 作为云端平台，当前支持用户将操作下发到设备端执行。随着平台能力演进，需要引入 **AI Agent Runtime**，使平台具备：

- 用户通过自然语言下发复杂任务，由 AI Agent 自主规划和执行
- 将大任务拆解并委托给多个专业化的 Subagent 协作完成
- 用户可实时查询任务进展和状态
- 子任务完成后自动回调通知主 Agent，驱动下一步决策

参考项目 [openclaw](https://github.com/openclaw/openclaw) 已有成熟的 TypeScript 实现（`src/tasks/task-registry.ts` + `src/agents/subagent-registry.ts`），本方案在 Go 技术栈上对标实现同等能力。

## 1.2 核心需求

| 编号 | 需求 | 说明 |
|------|------|------|
| R1 | 任务下发 | 用户通过 API 提交任务，系统异步执行并返回 taskID |
| R2 | Subagent 委托 | 主 Agent 可将子任务委托给专业化 Subagent 异步执行 |
| R3 | 状态查询 | 用户可通过 taskID 查询任务状态、进度和结果 |
| R4 | 实时事件推送 | 通过 SSE/WebSocket 推送任务执行过程中的实时事件 |
| R5 | 状态回调决策 | 子任务完成后回调主 Agent，驱动 AI 做下一步决策 |
| R6 | 任务取消 | 用户可取消正在执行的任务 |
| R7 | 多 Agent 编排 | 支持链式、并行、图式等多种 Agent 协作模式 |

## 1.3 框架选型

经过对 GitHub 上 Go 技术栈 AI Agent 框架的调研，对比了以下候选：

| 框架 | Stars | 维护方 | 多 Agent 编排 | 任务管理 | Callback 体系 | 评估 |
|------|-------|--------|--------------|----------|--------------|------|
| **trpc-agent-go** | 1,048 | 腾讯 tRPC | Chain/Parallel/Cycle/Graph | Runner+RequestID+Cancel | Before/AfterAgent | **首选** |
| google/adk-go | 7,298 | Google | Sequential/Parallel/Loop+A2A | Session+Event stream | 完整 callback 链 | 次选 |
| Protocol-Lattice/go-agent | 145 | 社区 | Agent-as-Tool+UTCP | Checkpoint/Restore | 弱 | 备选 |
| fastclaw | - | 社区 | 同步 SpawnSubAgent | 内部 TaskQueue | Hook 拦截器 | 不推荐 |

**选择 trpc-agent-go 的理由**：

1. **编排能力最强**：ChainAgent / ParallelAgent / CycleAgent / GraphAgent 四种模式，GraphAgent 对标 LangGraph
2. **Runner 架构**：统一的 Runner 层管理 session、memory、event stream，天然适合 server 端嵌入
3. **ManagedRunner**：内置 `RequestID` + `Cancel(requestID)` + `RunStatus(requestID)`，已有任务级管控基础
4. **生产验证**：腾讯元宝、腾讯视频等产品线使用
5. **A2A 协议**：配套 `trpc-a2a-go` 库支持跨服务 Agent 互操作
6. **技术栈兼容**：纯 Go 实现，可直接作为 module 引入现有 Gin + GORM 项目
