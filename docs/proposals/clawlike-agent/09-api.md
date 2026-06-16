# 9. API 设计

## 9.1 路由注册

```go
func (rt *ClawAgentRuntime) RegisterRoutes(r *gin.RouterGroup) {
    g := r.Group("/clawagent")
    {
        // 对话
        g.POST("/chat",              rt.chat)              // 流式 SSE
        g.GET("/sessions",           rt.listSessions)
        g.GET("/sessions/:id",       rt.getSession)
        g.DELETE("/sessions/:id",    rt.deleteSession)
        g.GET("/sessions/:id/history", rt.getHistory)

        // Persona
        g.GET("/personas",           rt.listPersonas)
        g.POST("/personas",          rt.createPersona)
        g.PUT("/personas/:id",       rt.updatePersona)
        g.DELETE("/personas/:id",    rt.deletePersona)
        g.POST("/personas/:id/default", rt.setDefaultPersona)

        // Provider
        g.GET("/providers",          rt.listProviders)
        g.POST("/providers",         rt.createProvider)
        g.PUT("/providers/:id",      rt.updateProvider)
        g.DELETE("/providers/:id",   rt.deleteProvider)
        g.POST("/providers/:id/test", rt.testProvider)

        // Memory
        g.GET("/memory",             rt.getMemory)
        g.PUT("/memory",             rt.updateMemory)

        // Workspace 委托
        g.GET("/workspaces",              rt.listWorkspaces)
        g.POST("/workspaces",             rt.createWorkspace)
        g.GET("/workspaces/:id/tasks",    rt.listDelegationTasks)
        g.GET("/workspaces/:id/tasks/:taskId", rt.getDelegationTask)
        g.POST("/workspaces/:id/tasks/:taskId/abort", rt.abortDelegationTask)
    }
}
```

## 9.2 对话 API

### POST /api/clawagent/chat — 流式对话

请求：

```json
{
    "message": "帮我分析 my-project 的代码结构",
    "sessionId": "sess-abc123",
    "stream": true
}
```

响应 (`text/event-stream`)：

```
event: token
data: {"content":"好的"}

event: token
data: {"content":"，我来"}

event: tool_call
data: {"tool":"workspace_list","args":{}}

event: tool_result
data: {"tool":"workspace_list","result":{"workspaces":[...]}}

event: token
data: {"content":"我发现 dev-001 上有 my-project workspace"}

event: done
data: {"sessionId":"sess-abc123","messageId":"msg-001"}
```

非流式模式返回完整 JSON。

### GET /api/clawagent/sessions — 会话列表

```json
{
    "sessions": [
        {
            "id": "wecom-bot:chat123:user456",
            "lastMessage": "帮我写个测试",
            "lastActiveAt": "2026-06-15T10:00:00Z",
            "messageCount": 12
        }
    ]
}
```

## 9.3 Persona API

### POST /api/clawagent/personas — 创建 Persona

```json
{
    "name": "tech-advisor",
    "soulContent": "你是一位资深技术顾问...",
    "identityContent": "Name: TechBot\nEmoji: 🔧",
    "userContext": "用户是 Go 后端开发者，Windows 环境",
    "isDefault": true
}
```

响应 `201 Created`：

```json
{
    "id": "uuid-here",
    "name": "tech-advisor",
    "isDefault": true,
    "createdAt": "2026-06-15T10:00:00Z"
}
```

## 9.4 Provider API

### POST /api/clawagent/providers — 添加 Provider

```json
{
    "name": "my-deepseek",
    "providerType": "deepseek",
    "apiKey": "sk-xxxx",
    "baseURL": "https://api.deepseek.com/v1",
    "modelName": "deepseek-chat",
    "isDefault": true
}
```

响应 `201 Created`：

```json
{
    "id": 1,
    "name": "my-deepseek",
    "providerType": "deepseek",
    "modelName": "deepseek-chat",
    "isDefault": true
}
```

> 注意：API Key 不会被返回，只存储加密后的值。

### POST /api/clawagent/providers/:id/test — 测试连通性

```json
{
    "success": true,
    "model": "deepseek-chat",
    "latency": 234
}
```

## 9.5 Memory API

### GET /api/clawagent/memory — 获取当前用户的 memory

```json
{
    "content": "用户偏好使用 Go 语言，常用 workspace 是 ws-001（后端服务）。倾向于简洁代码风格，不喜欢多余注释。最近在做 cs-cloud 集成。",
    "updatedAt": "2026-06-15T10:30:00Z",
    "lengthBytes": 187
}
```

### PUT /api/clawagent/memory — 用户手动覆盖 memory

```json
{
    "content": "用户手动编辑后的 memory 内容..."
}
```

响应 `200 OK`：

```json
{
    "content": "用户手动编辑后的 memory 内容...",
    "updatedAt": "2026-06-15T10:31:00Z",
    "lengthBytes": 156,
    "truncated": false
}
```

- 超过 4KB 自动截断，返回 `truncated: true`
- 用户覆盖后，下一轮对话的 LLM 合并会基于用户版本

> Memory 是每用户一份的 TEXT 字段，详见 [03-soul-and-memory.md](./03-soul-and-memory.md#32-memory记忆)。无列表、无搜索、无分页。

## 9.6 Workspace 委托 API

### GET /api/clawagent/workspaces — 列出可用 Workspace

```json
{
    "workspaces": [
        {
            "id": "ws-001",
            "name": "my-project",
            "deviceId": "dev-001",
            "deviceStatus": "online",
            "directories": [
                {"path": "/home/user/my-project", "name": "main"}
            ]
        }
    ]
}
```

### GET /api/clawagent/workspaces/:id/tasks — 委托任务列表

```json
{
    "tasks": [
        {
            "taskId": "task-abc123",
            "workspaceId": "ws-001",
            "deviceId": "dev-001",
            "task": "运行测试并报告结果",
            "conversationId": "conv-xyz789",
            "status": "succeeded",
            "deliveryStatus": "delivered",
            "progressSummary": "已完成 3/5 个测试文件",
            "output": "All 42 tests passed.",
            "startedAt": "2026-06-15T10:00:00Z",
            "completedAt": "2026-06-15T10:00:30Z"
        }
    ]
}
```

### GET /api/clawagent/workspaces/:id/tasks/:taskId — 委托任务详情

```json
{
    "taskId": "task-abc123",
    "workspaceId": "ws-001",
    "deviceId": "dev-001",
    "task": "运行测试并报告结果",
    "conversationId": "conv-xyz789",
    "agentSessionBaseKey": "agent:clawagent:wecom-bot:chat123:user456",
    "activeSessionId":    "agent:clawagent:wecom-bot:chat123:user456:v2",
    "status": "running",
    "deliveryStatus": "pending",
    "progressSummary": "正在分析 test/handler_test.go ...",
    "startedAt": "2026-06-15T10:00:00Z",
    "lastEventAt": "2026-06-15T10:00:15Z"
}
```

### POST /api/clawagent/workspaces/:id/tasks/:taskId/abort — 终止委托任务

向设备端 cs-cloud 发送 `POST /conversations/{conversationId}/abort`，终止正在执行的任务。

```json
{
    "success": true,
    "message": "Task aborted"
}
```

> 委托任务的创建由 Agent 内部 `workspace_delegate` tool 触发，不通过 REST API 直接调用。REST API 用于查看和管理委托任务历史。

### 委托任务实时事件流（复用 cs-cloud SSE）

前端**无需调用 server 端 SSE 端点**。直接通过 gateway proxy 透传连接 cs-cloud 的 `/api/v1/events`：

```
GET /cloud/device/{deviceID}/proxy/api/v1/events
X-Workspace-Directory: /home/user/project
Accept: text/event-stream
```

前端按 `conversation_id` 过滤事件（`session.idle` = 完成，`session.error` = 失败，`message.part.updated` = 进度）。详见 [07-device-delegation.md §7.10](./07-device-delegation.md)。

## 9.7 OpenAI 兼容 API

```
POST /v1/chat/completions
Authorization: Bearer <jwt-or-apikey>
X-Session-ID: optional-session-id

{
    "model": "clawagent",
    "messages": [
        {"role": "user", "content": "你好"}
    ],
    "stream": true
}
```

由 trpc-agent-go 的 `server/openai/` 包处理，支持流式和非流式。

## 9.8 认证

所有 API 通过现有 Casdoor JWT 认证。`middleware.UserIDKey` 提供 userID。

OpenAI 兼容 API 额外支持 API Key 认证（可选，用于第三方客户端）。
