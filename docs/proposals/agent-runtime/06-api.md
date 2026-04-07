# 6. API 设计

## 6.1 REST API

在 `server/internal/handlers/agent_handler.go` 中注册路由：

```go
func RegisterAgentRoutes(r *gin.RouterGroup, rt *agentrt.AgentRuntime) {
    g := r.Group("/agent")
    {
        g.POST("/tasks",             submitTask(rt))
        g.GET("/tasks",              listTasks(rt))
        g.GET("/tasks/:id",          getTaskStatus(rt))
        g.POST("/tasks/:id/cancel",  cancelTask(rt))
        g.GET("/tasks/:id/events",   streamTaskEvents(rt))    // SSE
        g.GET("/tasks/:id/children", listChildTasks(rt))
        g.GET("/subagents",          listSubagentRuns(rt))
    }
}
```

### POST /agent/tasks — 下发任务

请求：

```json
{
    "task": "分析项目代码结构并生成文档",
    "agentId": "code-analyzer",
    "model": "gpt-4o",
    "timeout": 300
}
```

响应 `201 Created`：

```json
{
    "taskId": "task-20260331-001",
    "status": "queued",
    "createdAt": "2026-03-31T10:00:00Z"
}
```

### GET /agent/tasks/:id — 查询任务状态

响应 `200 OK`：

```json
{
    "taskId": "task-20260331-001",
    "status": "running",
    "agentId": "code-analyzer",
    "task": "分析项目代码结构并生成文档",
    "progressSummary": "已完成 3/5 个文件的分析",
    "terminalSummary": null,
    "createdAt": "2026-03-31T10:00:00Z",
    "startedAt": "2026-03-31T10:00:01Z",
    "endedAt": null,
    "children": [
        {
            "taskId": "task-20260331-002",
            "status": "succeeded",
            "agentId": "file-reader",
            "terminalSummary": "已读取 main.go, handler.go, service.go"
        }
    ]
}
```

### POST /agent/tasks/:id/cancel — 取消任务

响应 `200 OK`：

```json
{
    "taskId": "task-20260331-001",
    "status": "cancelled"
}
```

### GET /agent/tasks — 任务列表

查询参数：`?status=running&limit=20&offset=0`

响应 `200 OK`：

```json
{
    "tasks": [...],
    "total": 42,
    "limit": 20,
    "offset": 0
}
```

## 6.2 SSE 实时事件

### GET /agent/tasks/:id/events — SSE 事件流

通过 Server-Sent Events 推送任务执行过程中的实时事件：

```
GET /agent/tasks/task-20260331-001/events
Accept: text/event-stream

event: task.running
data: {"taskId":"task-20260331-001","timestamp":"..."}

event: tool.call
data: {"taskId":"task-20260331-001","toolName":"read_file","args":{"path":"main.go"}}

event: tool.result
data: {"taskId":"task-20260331-001","toolName":"read_file","result":"..."}

event: task.progress
data: {"taskId":"task-20260331-001","summary":"已分析 main.go"}

event: subagent.spawn
data: {"taskId":"task-20260331-001","childTaskId":"task-002","agentName":"researcher"}

event: subagent.done
data: {"taskId":"task-20260331-001","childTaskId":"task-002","outcome":"ok"}

event: agent.response
data: {"taskId":"task-20260331-001","content":"根据分析结果..."}

event: task.completed
data: {"taskId":"task-20260331-001","summary":"项目分析文档已生成"}
```

与现有 Cloud SSE 的集成：Agent 事件可复用 `server/internal/cloud/` 的 SSE 基础设施。
