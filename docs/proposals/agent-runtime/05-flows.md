# 5. 关键流程

## 5.1 任务下发流程

```
用户 ──POST /agent/tasks──► AgentHandler
                                │
                   1. 参数校验 + 鉴权
                                │
                   2. TaskRegistry.Create(status=queued)
                                │
                   3. runner.Run(ctx, userID, sessionID, message,
                   │     agent.WithRequestID(taskID))
                   │     → 返回 <-chan *event.Event
                   │
                   4. TaskRegistry.MarkRunning(taskID, requestID)
                                │
                   5. go rt.consumeEvents(eventCh, taskID) ─────┐
                                │                                │
                   6. 返回 TaskRecord (含 taskID)                │
                                                                 │
                   ┌─────────────────────────────────────────────┘
                   │  consumeEvents goroutine:
                   │
                   │  for event := range eventCh {
                   │      - 解析 event 类型
                   │      - EventBus.Publish(taskID, runtimeEvent)
                   │      - 根据类型更新 TaskRegistry:
                   │          tool_call  → UpdateProgress
                   │          completion → Complete
                   │          error      → Fail
                   │      - 若有 parentTaskID → announceToParent()
                   │  }
```

## 5.2 Subagent 委托执行流程

主 Agent 在 ReAct 循环中通过 `transfer_to_agent` tool 自主选择委托子 Agent：

```
主 Agent (LLMAgent)
    │
    │  LLM 判断需要委托子任务
    │  → tool_call: transfer_to_agent(agentName="researcher", task="...")
    │
    ├─ trpc-agent-go 内部处理 transfer ──────────────┐
    │                                                  │
    │  BeforeAgent callback 拦截:                      │
    │  1. SubagentRegistry.Register(runID, childSession, task)
    │  2. TaskRegistry.Create(runtime=subagent, parentTaskID=主任务ID)
    │  3. EventBus.Publish(EventSubagentSpawn)         │
    │                                                  │
    │  子 Agent 开始执行 ◄─────────────────────────────┘
    │  (在独立 session 中运行)
    │      │
    │      ├─ 执行过程中: event → EventBus.Publish
    │      │
    │      └─ 执行完成:
    │          1. SubagentRegistry.MarkEnded(outcome, result)
    │          2. TaskRegistry.Complete(childTaskID, summary)
    │          3. announceToParent(childTask) ──────────┐
    │                                                    │
    │  主 Agent session 收到回调消息 ◄──────────────────┘
    │  → LLM 基于子任务结果做下一步决策
```

## 5.3 任务状态查询流程

```
用户 ──GET /agent/tasks/:id──► AgentHandler
                                    │
                       TaskRegistry.Get(taskID)
                                    │
                       返回 TaskRecord {
                           taskID, status, progressSummary,
                           terminalSummary, createdAt, startedAt, ...
                       }
```

支持批量查询：

```
用户 ──GET /agent/tasks?status=running──► AgentHandler
                                              │
                                 TaskRegistry.ListByUser(userID, opts)
                                              │
                                 返回 []*TaskRecord
```

## 5.4 状态回调与 AI 决策流程

对标 openclaw `src/agents/subagent-announce.ts` 的 announce 机制：

```go
// server/internal/agentrt/announce.go

func (rt *AgentRuntime) announceToParent(childTask *TaskRecord) {
    if childTask.ParentTaskID == nil {
        return  // 顶层任务，无需回调
    }

    parentTask, _ := rt.tasks.Get(*childTask.ParentTaskID)

    // 构造回调消息
    callbackMsg := fmt.Sprintf(
        "[系统通知] 子任务 [%s] 已完成\n"+
            "- Agent: %s\n"+
            "- 状态: %s\n"+
            "- 结果摘要: %s\n"+
            "请基于此结果决定下一步操作。",
        childTask.TaskID, childTask.AgentID,
        childTask.Status, safeDeref(childTask.TerminalSummary),
    )

    // 通过 Runner 注入新消息触发主 Agent 重新决策
    eventCh, err := rt.runner.Run(ctx,
        parentTask.UserID,
        parentTask.RequesterSession,
        model.NewUserMessage(callbackMsg),
        agent.WithRequestID(parentTask.RequestID+"-callback"),
    )
    if err != nil {
        // 标记交付失败，安排重试
        rt.scheduleAnnounceRetry(childTask)
        return
    }

    // 更新交付状态
    rt.tasks.MarkDelivered(childTask.TaskID)
    rt.subagents.MarkAnnounced(childTask.RunID)

    // 消费回调产生的事件（更新主任务进度）
    go rt.consumeEvents(eventCh, parentTask.TaskID)
}
```

重试机制（对标 openclaw 指数退避）：

```go
const MaxAnnounceRetry = 5

func announceRetryDelay(retryCount int) time.Duration {
    return time.Duration(1<<retryCount) * time.Second  // 1s, 2s, 4s, 8s, 16s
}
```

## 5.5 任务取消流程

```
用户 ──POST /agent/tasks/:id/cancel──► AgentHandler
                                            │
                               1. TaskRegistry.Get(taskID)
                               2. runner.Cancel(task.RequestID)
                                  → trpc-agent-go 取消 context
                               3. TaskRegistry.Cancel(taskID)
                               4. EventBus.Publish(EventTaskCancelled)
                                            │
                               5. 若有子任务: 递归取消所有子任务
                                  for child := range tasks.ListByParent(taskID) {
                                      rt.CancelTask(child.TaskID)
                                  }
```
