# 7. Workspace 委托机制

## 7.1 设计目标

以 **Workspace** 为委托单位，通过 cs-cloud 桥接层将任务下发到设备端执行。

### 核心机制

cs-cloud 设备端通过 **WebSocket/yamux 隧道** 暴露完整的 localserver HTTP API。Server 层不需要专用 RPC 协议，只需通过现有 Gateway Proxy 代理 HTTP 请求到设备端 localserver。

```
Server ClawAgent
    │
    │  workspace_delegate tool_call
    ↓
DeviceProxyClient (server/internal/clawagent/device_proxy.go)
    │  通过 cloud.ConnectionManager → Gateway → yamux tunnel
    ↓
cs-cloud localserver (127.0.0.1:port/api/v1/*)
    │  代理到 agent backend (cs serve / csc serve)
    ↓
Workspace 项目目录
```

## 7.2 cs-cloud localserver API 契约

cs-cloud 在设备端暴露的 `/api/v1/` HTTP API（全部可通过隧道从 server 调用）：

### 会话/任务接口（核心）

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/conversations` | 创建会话（指定 workspace 目录） |
| POST | `/api/v1/conversations/{id}/prompt/async` | 异步发送任务 |
| POST | `/api/v1/conversations/{id}/abort` | 中止任务 |
| GET | `/api/v1/conversations/{id}/messages` | 获取消息历史 |
| GET | `/api/v1/events` | SSE 事件流（任务进展、结果） |

### Workspace 接口

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/runtime/health` | 设备健康 + workspace 状态 |
| GET | `/api/v1/runtime/files` | 列出文件 |
| GET | `/api/v1/runtime/files/content` | 读文件内容 |
| PUT | `/api/v1/runtime/files/content` | 写文件内容 |
| GET | `/api/v1/runtime/vcs` | Git 状态 |
| GET | `/api/v1/runtime/diff` | Git diff |
| GET | `/api/v1/runtime/init-status` | workspace 初始化状态 |

### 关键 Header

所有请求通过 `X-Workspace-Directory` 指定目标工作区：

```
X-Workspace-Directory: /home/user/my-project
```

## 7.3 Server 层 DeviceProxyClient

在 server 层实现统一的设备代理客户端，封装对 cs-cloud localserver 的调用：

```go
// server/internal/clawagent/device_proxy.go

type DeviceProxyClient struct {
    cloudMgr  *cloud.ConnectionManager
    db        *gorm.DB
}

// CreateConversation 在设备端创建会话
func (c *DeviceProxyClient) CreateConversation(
    ctx context.Context, deviceID, workspaceDir string,
) (*ConversationResponse, error) {
    resp, err := c.proxyToDevice(ctx, deviceID, proxyRequest{
        Method: "POST",
        Path:   "/api/v1/conversations",
        Headers: map[string]string{
            "X-Workspace-Directory": workspaceDir,
        },
        Body: map[string]any{},
    })
    // ...
}

// SendPrompt 异步发送任务到设备端会话
func (c *DeviceProxyClient) SendPrompt(
    ctx context.Context, deviceID, convID, task, skill string,
) (*PromptResponse, error) {
    body := map[string]any{
        "content": task,
    }
    if skill != "" {
        body["agent"] = skill
    }

    resp, err := c.proxyToDevice(ctx, deviceID, proxyRequest{
        Method: "POST",
        Path:   fmt.Sprintf("/api/v1/conversations/%s/prompt/async", convID),
        Body:   body,
    })
    // ...
}

// AbortPrompt 中止设备端任务
func (c *DeviceProxyClient) AbortPrompt(
    ctx context.Context, deviceID, convID string,
) error

// GetMessages 获取会话消息历史
func (c *DeviceProxyClient) GetMessages(
    ctx context.Context, deviceID, convID string,
) ([]*Message, error)

// GetVCS 获取 workspace 的 Git 状态
func (c *DeviceProxyClient) GetVCS(
    ctx context.Context, deviceID, workspaceDir string,
) (*VCSInfo, error)

// ReadFile 读取 workspace 中的文件
func (c *DeviceProxyClient) ReadFile(
    ctx context.Context, deviceID, workspaceDir, filePath string,
) (string, error)

// ListFiles 列出 workspace 文件
func (c *DeviceProxyClient) ListFiles(
    ctx context.Context, deviceID, workspaceDir, subPath string,
) ([]*FileInfo, error)

// SubscribeEvents 订阅设备 SSE 事件流
func (c *DeviceProxyClient) SubscribeEvents(
    ctx context.Context, deviceID, workspaceDir string,
) (<-chan *DeviceEvent, error)
```

### proxyToDevice 核心方法

```go
type proxyRequest struct {
    Method  string
    Path    string
    Headers map[string]string
    Body    any
    Timeout time.Duration
}

func (c *DeviceProxyClient) proxyToDevice(
    ctx context.Context, deviceID string, req proxyRequest,
) (*http.Response, error) {
    // 通过 cloud.ConnectionManager 的设备代理路由
    // 复用现有的 /cloud/device/:deviceID/proxy/* 机制
    // 构造 HTTP 请求转发到设备隧道
    return c.cloudMgr.ProxyToDevice(ctx, deviceID, req.Method, req.Path,
        req.Headers, req.Body, req.Timeout)
}
```

## 7.4 Workspace 委托工具

### workspace_list — 列出可用工作区

```go
type WorkspaceListOutput struct {
    Workspaces []WorkspaceInfo `json:"workspaces"`
}

type WorkspaceInfo struct {
    ID           string   `json:"workspace_id"`
    Name         string   `json:"name"`
    DeviceID     string   `json:"device_id"`
    DeviceStatus string   `json:"device_status"`
    Directories  []string `json:"directories"`
    IsDefault    bool     `json:"is_default"`
}
```

Agent 从 `models.Workspace` + `models.WorkspaceDirectory` 查询。

### workspace_delegate — 委托任务

```go
type WorkspaceDelegateInput struct {
    WorkspaceID string `json:"workspace_id" description:"目标工作区 ID"`
    Task        string `json:"task" description:"任务描述"`
    Skill       string `json:"skill,omitempty" description:"指定 agent 模式（如 build/code）"`
    Blocking    bool   `json:"blocking,omitempty" description:"是否等待完成（默认 true）"`
    Timeout     string `json:"timeout,omitempty" description:"超时（默认 10m）"`
}

type WorkspaceDelegateOutput struct {
    TaskID        string `json:"task_id"`
    WorkspaceID   string `json:"workspace_id"`
    DeviceID      string `json:"device_id"`
    ConversationID string `json:"conversation_id"`
    Status        string `json:"status"`      // pending/running/completed/failed/timeout
    Output        string `json:"output,omitempty"`
    Error         string `json:"error,omitempty"`
}
```

### workspace_info — 查询工作区信息

```go
type WorkspaceInfoInput struct {
    WorkspaceID string `json:"workspace_id"`
}

type WorkspaceInfoOutput struct {
    WorkspaceName string      `json:"workspace_name"`
    DeviceID      string      `json:"device_id"`
    Directory     string      `json:"directory"`
    VCS           *VCSInfo    `json:"vcs,omitempty"`     // Git 信息
    Files         []FileInfo  `json:"files,omitempty"`   // 文件列表
    Health        string      `json:"health"`            // healthy/unhealthy
}
```

Agent 调用流程：

```
tool_call → workspace_info(workspace_id="ws-001")
  │
  ├── 查 DB 获取 workspace → deviceID + directory
  ├── DeviceProxyClient.GetVCS(deviceID, directory)
  ├── DeviceProxyClient.ListFiles(deviceID, directory, "")
  └── 返回 workspace 信息
```

## 7.5 委托执行完整流程

```
workspace_delegate(workspace_id="ws-001", task="为项目写测试")
  │
  ├── 1. 查 DB: ws-001 → deviceID="dev-001", dir="/home/user/project"
  │
  ├── 2. DeviceProxyClient.CreateConversation(dev-001, "/home/user/project")
  │     → 返回 conversationID="conv-abc"
  │
  ├── 3. DeviceProxyClient.SendPrompt(dev-001, conv-abc, "为项目写测试", "")
  │     → 返回 accepted (异步执行)
  │
  ├── 4. DeviceProxyClient.SubscribeEvents(dev-001, conv-abc)
  │     → SSE 流接收：
  │       - message.part.updated → 流式文本片段
  │       - message.tool_call    → 工具调用进展
  │       - session.idle         → 任务完成
  │       - session.error        → 错误
  │
  ├── 5. 收集 session.idle 事件 → 提取最终输出
  │     DeviceProxyClient.GetMessages(dev-001, conv-abc)
  │     → 获取完整的消息历史和结果
  │
  ├── 6. 记录到 agent_workspace_tasks 表
  │
  └── 7. 返回 WorkspaceDelegateOutput
```

### 非阻塞模式

```go
if !input.Blocking {
    // 立即返回 taskID + conversationID
    // 异步 goroutine 通过 EventBus 订阅设备 SSE 事件
    // 完成后通过 announce 机制回传结果到 Agent session（见 7.8）
    go c.watchAndAnnounce(ctx, taskID, agentSessionID, deviceID, convID)
    return &WorkspaceDelegateOutput{
        TaskID: taskID,
        Status: "running",
        ConversationID: convID,
    }, nil
}
```

## 7.6 Agent 决策流程

```
用户: "帮我分析后端服务的代码质量"
                │
                ↓
    ┌───────────┴───────────┐
    │ workspace_list()       │
    └───────────┬───────────┘
         ┌──────┴──────┐
         │             │
    有匹配 workspace   无匹配
         │             │
         ↓             ↓
  workspace_info     workspace_list(全部)
  获取 Git/文件信息   让用户选择或新建
         │
         ↓
  workspace_delegate
  委托执行任务
         │
         ↓
  返回结果给用户
```

### 完整对话示例

```
用户: 帮我在后端项目里加个日志中间件

Agent: tool_call → workspace_list()
← [{id:"ws-001", name:"后端服务", device_id:"dev-001", dirs:["/home/dev/backend"]}]

Agent: tool_call → workspace_info(workspace_id="ws-001")
← {vcs:{branch:"main", dirty:false}, files:["main.go","go.mod","handler/..."]}

Agent: tool_call → workspace_delegate(
    workspace_id="ws-001",
    task="为项目添加日志中间件，记录每个请求的 method/path/status/latency"
)
  ↓ DeviceProxyClient → CreateConversation + SendPrompt + SSE 等待
← {status:"succeeded", output:"已在 middleware/logger.go 添加日志中间件..."}

Agent: ✅ 已在后端服务 workspace 中添加日志中间件。
       文件：middleware/logger.go
       功能：记录 method/path/status/latency
```

## 7.7 与 OpenClaw 的对比

| 维度 | OpenClaw | 本方案 |
|------|---------|--------|
| 委托单位 | 本地子进程 | Workspace（DB 持久化，绑定 Device） |
| 通信方式 | 内存事件总线 | HTTP 代理 + SSE（通过 yamux 隧道） |
| 执行方式 | 同进程 TypeScript | 设备端独立 agent runtime (cs/csc) |
| 结果回传 | 内存回调 | Announce 机制：SSE 监听 → runner.Run() 注入 |
| 接口契约 | 内部协议 | 标准 HTTP REST API |

## 7.8 异步结果回传（Announce）

借鉴 agent-runtime 提案的 `announceToParent()` 机制。当非阻塞委托任务在设备端完成后，需要将结果**异步注入回 Agent session**，驱动 Agent 做下一步决策。

### Announce 流程

```
非阻塞 workspace_delegate 返回 taskID
    │
    │  异步 goroutine: watchAndAnnounce()
    │
    ├── DeviceProxyClient.SubscribeEvents(deviceID, convID)
    │     → 消费 cs-cloud /api/v1/events SSE 流
    │     → 过滤 conversation_id == convID 的事件
    │
    ├── 收到 message.part.updated → 更新 progress_summary + last_event_at
    │                             → EventBus.Publish(task.progress)
    │
    ├── 收到 session.idle → 任务成功完成
    │     ├── DeviceProxyClient.GetMessages() 获取完整输出
    │     ├── TaskRegistry.Complete(taskID, output) → status=succeeded
    │     └── announceToAgent(task) ──────────────────────┐
    │                                                      │
    └── 收到 session.error → 任务失败                       │
          ├── TaskRegistry.Fail(taskID, errMsg) → status=failed
          └── announceToAgent(task) ──────────────────────┤
                                                           │
                    ┌──────────────────────────────────────┘
                    │
                    │  announceToAgent(task):
                    │
                    │  构造回调消息:
                    │  "[系统通知] 委托任务 [{taskID}] 已完成\n
                    │   - Workspace: {workspaceName}\n
                    │   - 状态: {status}\n
                    │   - 结果: {output/summary}\n
                    │   请基于此结果决定下一步。"
                    │
                    ├── runner.Run(ctx, userID, agentSessionID, callbackMsg)
                    │     → Agent 基于结果做下一步决策
                    │
                    └── TaskRegistry.MarkDelivered(taskID) → delivery_status=delivered
```

### Announce 重试机制

```go
const MaxAnnounceRetry = 5

func announceRetryDelay(retryCount int) time.Duration {
    return time.Duration(1<<retryCount) * time.Second  // 1s, 2s, 4s, 8s, 16s
}

func (rt *ClawAgentRuntime) announceToAgent(task *DelegationTask) {
    if task.DeliveryStatus == "not_applicable" {
        return  // 阻塞模式，同步返回，无需 announce
    }

    callbackMsg := buildCallbackMessage(task)
    _, err := rt.runner.Run(ctx, task.UserID, task.AgentSessionID,
        model.NewUserMessage(callbackMsg))
    if err != nil {
        retryCount := task.AnnounceRetryCount + 1
        if retryCount >= MaxAnnounceRetry {
            rt.tasks.MarkDeliveryFailed(task.TaskID)  // delivery_status=failed
            return
        }
        rt.tasks.UpdateAnnounceRetry(task.TaskID, retryCount)
        time.AfterFunc(announceRetryDelay(retryCount), func() {
            rt.announceToAgent(task)  // 指数退避重试
        })
        return
    }

    rt.tasks.MarkDelivered(task.TaskID)  // delivery_status=delivered
}
```

## 7.9 崩溃恢复

服务重启后，需处理非终态的委托任务（借鉴 agent-runtime 的 `recoverLostTasks()`）。

### 恢复流程

```
服务启动
    │
    ├── 查询 status IN ('queued', 'running') 的任务
    │
    ├── 对每个任务:
    │     ├── 检查 device_id 是否在线（cloud.ConnectionManager）
    │     │
    │     ├── 设备在线 + conversation_id 存在:
    │     │     ├── 重新订阅 cs-cloud SSE 流
    │     │     ├── 恢复 watchAndAnnounce goroutine
    │     │     └── 更新 last_event_at
    │     │
    │     ├── 设备在线 + 无 conversation_id (queued 未发送):
    │     │     ├── 补发 CreateConversation + SendPrompt
    │     │     └── 转入 running 状态
    │     │
    │     └── 设备离线:
    │           └── TaskRegistry.MarkLost(taskID) → status=lost
    │
    └── EventBus 广播恢复完成事件
```

### 超时检测

独立的 goroutine 定期扫描 `running` 状态任务：

```go
func (rt *ClawAgentRuntime) checkTimeouts() {
    for task := range rt.tasks.ListRunning() {
        if time.Since(task.LastEventAt) > task.Timeout {
            rt.tasks.TimedOut(task.TaskID)  // status=timed_out
            rt.announceToAgent(task)        // 通知 Agent 超时
        }
    }
}
```

## 7.10 前端实时事件流（复用 cs-cloud SSE）

前端无需新建 SSE 端点。直接复用 cs-cloud 的 `/api/v1/events` SSE 流（通过 gateway proxy 透传）：

```
GET /cloud/device/{deviceID}/proxy/api/v1/events
X-Workspace-Directory: /home/user/project
Accept: text/event-stream
```

### 事件类型（cs-cloud 原生）

| 事件 | 说明 | 对应委托状态 |
|------|------|-------------|
| `message.part.updated` | Agent 正在输出文本片段 | running（更新 progress_summary） |
| `message.tool_call` | Agent 调用工具 | running |
| `session.idle` | 会话空闲，任务完成 | succeeded |
| `session.error` | 会话错误 | failed |
| `host.file.changed` | 文件变更（file watcher） | running |
| `host.git.commit` | Git 提交（git watcher） | running |

### 前端过滤

SSE 流是 per-device 的，前端按 `conversation_id` / `sessionID` 过滤事件：

```javascript
const eventSource = new EventSource(
  `/cloud/device/${deviceId}/proxy/api/v1/events?X-Workspace-Directory=${encodeURIComponent(dir)}`
);

eventSource.onmessage = (e) => {
  const event = JSON.parse(e.data);
  // 只处理当前委托任务的事件
  if (event.properties?.conversation_id !== currentConversationId) return;

  switch (event.type) {
    case 'message.part.updated':
      updateProgress(event.properties);
      break;
    case 'session.idle':
      markTaskSucceeded();
      eventSource.close();
      break;
    case 'session.error':
      markTaskFailed(event.properties);
      eventSource.close();
      break;
  }
};
```

> **注意**：前端直接消费 cs-cloud 原生 SSE 事件，服务端不需要新建 SSE 端点或事件转换层。服务端 EventBus 是内部组件，仅用于 announce 回调和崩溃恢复，不对外暴露。
