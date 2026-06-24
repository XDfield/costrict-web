# 8. Server 层设备代理接口集

## 8.1 设计目标

在 server 层实现统一的 **DeviceProxyClient**，封装对 cs-cloud localserver 的所有 HTTP 调用。这是 ClawAgent Runtime 与设备端之间的桥梁。

### 架构

```
ClawAgent Runtime
    │
    │  workspace_* tool_call
    ↓
DeviceProxyClient (server/internal/clawagent/device_proxy.go)
    │
    │  HTTP 请求 (通过 cloud.ConnectionManager 设备代理路由)
    ↓
Gateway Proxy → WebSocket/yamux 隧道
    │
    │  原始 HTTP 请求转发
    ↓
cs-cloud localserver (设备端 127.0.0.1:port/api/v1/*)
    │
    │  路由重写 + 反向代理
    ↓
Agent Backend (cs serve / csc serve)
```

## 8.2 DeviceProxyClient 接口定义

```go
// server/internal/clawagent/device_proxy.go

type DeviceProxyClient struct {
    cloudMgr *cloud.ConnectionManager
    db       *gorm.DB
}

func NewDeviceProxyClient(cloudMgr *cloud.ConnectionManager, db *gorm.DB) *DeviceProxyClient
```

### 会话管理

```go
// CreateConversation 在设备端创建会话
func (c *DeviceProxyClient) CreateConversation(
    ctx context.Context,
    deviceID string,
    workspaceDir string,
) (*Conversation, error)

// DeleteConversation 删除设备端会话
func (c *DeviceProxyClient) DeleteConversation(
    ctx context.Context,
    deviceID string,
    convID string,
) error

// GetConversations 列出设备端会话
func (c *DeviceProxyClient) GetConversations(
    ctx context.Context,
    deviceID string,
    workspaceDir string,
) ([]*Conversation, error)
```

### 任务执行

```go
// SendPromptAsync 异步发送任务（非阻塞，结果通过 SSE 返回）
func (c *DeviceProxyClient) SendPromptAsync(
    ctx context.Context,
    deviceID string,
    convID string,
    task string,
    opts ...PromptOption,  // WithModel, WithAgent(skill), WithFiles
) error

// AbortPrompt 中止正在执行的任务
func (c *DeviceProxyClient) AbortPrompt(
    ctx context.Context,
    deviceID string,
    convID string,
) error

// GetMessages 获取会话消息历史
func (c *DeviceProxyClient) GetMessages(
    ctx context.Context,
    deviceID string,
    convID string,
) ([]*DeviceMessage, error)
```

### 事件订阅

```go
// SubscribeEvents 订阅设备 SSE 事件流
// 返回 channel，持续推送事件直到 ctx 取消
func (c *DeviceProxyClient) SubscribeEvents(
    ctx context.Context,
    deviceID string,
    workspaceDir string,
) (<-chan *DeviceEvent, error)

type DeviceEvent struct {
    Type       string         `json:"type"`
    Properties map[string]any `json:"properties"`
}
```

事件类型：

| 事件 | 说明 |
|------|------|
| `message.part.updated` | 流式文本/工具调用片段 |
| `message.updated` | 消息更新 |
| `session.status` | 会话状态变更 |
| `session.idle` | Agent 完成，进入空闲 |
| `session.error` | 错误 |
| `permission.asked` | 需要用户授权（如执行命令） |
| `question.asked` | Agent 向用户提问 |
| `host.file.*` | 文件变更事件 |
| `host.git.*` | Git 变更事件 |

### Workspace 查询

```go
// GetHealth 设备健康检查
func (c *DeviceProxyClient) GetHealth(
    ctx context.Context,
    deviceID string,
) (*DeviceHealth, error)

// GetVCS 获取 Git 状态
func (c *DeviceProxyClient) GetVCS(
    ctx context.Context,
    deviceID string,
    workspaceDir string,
) (*VCSInfo, error)

// GetDiff 获取 Git diff
func (c *DeviceProxyClient) GetDiff(
    ctx context.Context,
    deviceID string,
    workspaceDir string,
) (*DiffInfo, error)

// ReadFile 读取文件内容
func (c *DeviceProxyClient) ReadFile(
    ctx context.Context,
    deviceID string,
    workspaceDir string,
    filePath string,
) (string, error)

// ListFiles 列出目录文件
func (c *DeviceProxyClient) ListFiles(
    ctx context.Context,
    deviceID string,
    workspaceDir string,
    subPath string,
    recursive bool,
) ([]*FileInfo, error)

// WriteFile 写入文件内容
func (c *DeviceProxyClient) WriteFile(
    ctx context.Context,
    deviceID string,
    workspaceDir string,
    filePath string,
    content string,
) error

// FindFile 模糊搜索文件
func (c *DeviceProxyClient) FindFile(
    ctx context.Context,
    deviceID string,
    workspaceDir string,
    query string,
) ([]*FileInfo, error)

// GetInitStatus 获取 workspace 初始化状态
func (c *DeviceProxyClient) GetInitStatus(
    ctx context.Context,
    deviceID string,
    workspaceDir string,
) (*InitStatus, error)
```

### 权限/交互

```go
// ReplyPermission 回复权限请求
func (c *DeviceProxyClient) ReplyPermission(
    ctx context.Context,
    deviceID string,
    permissionID string,
    optionID string,
) error

// ReplyQuestion 回复 Agent 提问
func (c *DeviceProxyClient) ReplyQuestion(
    ctx context.Context,
    deviceID string,
    questionID string,
    answer string,
) error
```

## 8.3 proxyToDevice 实现

```go
type proxyRequest struct {
    Method  string
    Path    string                       // /api/v1/... 路径
    Headers map[string]string            // 含 X-Workspace-Directory
    Body    any                          // JSON body
    Timeout time.Duration                // 默认 120s
}

func (c *DeviceProxyClient) proxyToDevice(
    ctx context.Context,
    deviceID string,
    req proxyRequest,
) ([]byte, error) {
    // 1. 检查设备在线
    if !c.cloudMgr.IsDeviceOnline(ctx, deviceID) {
        return nil, fmt.Errorf("device %s is not online", deviceID)
    }

    // 2. 构造代理请求
    //    复用 cloud 模块的设备代理路由：
    //    /cloud/device/:deviceID/proxy/* → gateway → yamux tunnel → device localserver
    if req.Timeout == 0 {
        req.Timeout = 120 * time.Second
    }

    // 3. 通过 cloud.ConnectionManager 发送
    respBody, err := c.cloudMgr.ProxyToDevice(ctx, &cloud.DeviceProxyRequest{
        DeviceID: deviceID,
        Method:   req.Method,
        Path:     req.Path,
        Headers:  req.Headers,
        Body:     marshalBody(req.Body),
        Timeout:  req.Timeout,
    })
    if err != nil {
        return nil, fmt.Errorf("device proxy failed: %w", err)
    }

    // 4. 解析响应
    var envelope struct {
        OK    bool            `json:"ok"`
        Data  json.RawMessage `json:"data"`
        Error *struct {
            Code    string `json:"code"`
            Message string `json:"message"`
        } `json:"error"`
    }
    if err := json.Unmarshal(respBody, &envelope); err != nil {
        return nil, fmt.Errorf("failed to parse device response: %w", err)
    }
    if !envelope.OK {
        return nil, fmt.Errorf("device error: %s", envelope.Error.Message)
    }
    return envelope.Data, nil
}
```

### SSE 特殊处理

SSE 事件流需要长连接，不能走普通 HTTP 代理：

```go
func (c *DeviceProxyClient) SubscribeEvents(
    ctx context.Context,
    deviceID, workspaceDir string,
) (<-chan *DeviceEvent, error) {
    eventCh := make(chan *DeviceEvent, 256)

    go func() {
        defer close(eventCh)

        // 通过 cloud SSE 代理建立长连接
        // 复用 /cloud/device/:deviceID/proxy/api/v1/events 路由
        stream, err := c.cloudMgr.ProxyToDeviceSSE(ctx, deviceID, &cloud.DeviceProxyRequest{
            Method: "GET",
            Path:   "/api/v1/events",
            Headers: map[string]string{
                "X-Workspace-Directory": workspaceDir,
                "Accept":                "text/event-stream",
            },
        })
        if err != nil {
            return
        }

        scanner := bufio.NewScanner(stream)
        for scanner.Scan() {
            line := scanner.Text()
            if strings.HasPrefix(line, "data: ") {
                var evt DeviceEvent
                if json.Unmarshal([]byte(line[6:]), &evt) == nil {
                    select {
                    case eventCh <- &evt:
                    case <-ctx.Done():
                        return
                    }
                }
            }
        }
    }()

    return eventCh, nil
}
```

## 8.4 cloud.ConnectionManager 扩展

需要在现有 `cloud.ConnectionManager` 上新增设备代理方法：

```go
// server/internal/cloud/connection_manager.go (扩展)

type DeviceProxyRequest struct {
    DeviceID string
    Method   string
    Path     string
    Headers  map[string]string
    Body     []byte
    Timeout  time.Duration
}

// ProxyToDevice 通过隧道代理 HTTP 请求到设备端
func (m *ConnectionManager) ProxyToDevice(
    ctx context.Context, req *DeviceProxyRequest,
) ([]byte, error)

// ProxyToDeviceSSE 通过隧道代理 SSE 流到设备端
func (m *ConnectionManager) ProxyToDeviceSSE(
    ctx context.Context, deviceID string, req *DeviceProxyRequest,
) (io.ReadCloser, error)
```

底层通过现有的 `/cloud/device/:deviceID/proxy/*` 路由实现，该路由已有 gateway 隧道代理能力。

## 8.5 向量映射

| cs-cloud localserver | DeviceProxyClient 方法 | ClawAgent Tool |
|---------------------|----------------------|----------------|
| `POST /conversations` | `CreateConversation()` | `workspace_delegate` (内部) |
| `POST /conversations/{id}/prompt/async` | `SendPromptAsync()` | `workspace_delegate` (内部) |
| `GET /events` | `SubscribeEvents()` | `workspace_delegate` (内部) |
| `POST /conversations/{id}/abort` | `AbortPrompt()` | (未来 tool) |
| `GET /conversations/{id}/messages` | `GetMessages()` | (未来 tool) |
| `GET /runtime/vcs` | `GetVCS()` | `workspace_info` |
| `GET /runtime/files` | `ListFiles()` | `workspace_info` |
| `GET /runtime/files/content` | `ReadFile()` | `workspace_info` |
| `PUT /runtime/files/content` | `WriteFile()` | (未来 tool) |
| `GET /runtime/health` | `GetHealth()` | `workspace_list` (内部) |
| `GET /runtime/init-status` | `GetInitStatus()` | `workspace_info` (内部) |

## 8.6 错误处理

```go
var (
    ErrDeviceOffline     = errors.New("device is offline")
    ErrDeviceTimeout     = errors.New("device proxy timeout")
    ErrWorkspaceNotFound = errors.New("workspace not found on device")
    ErrConversationLost  = errors.New("conversation not found on device (may have been disposed)")
)
```

- 设备离线：返回错误，Agent 可提示用户
- 超时：返回部分结果 + 超时标记
- 会话丢失：自动重建会话（通过 `CreateConversation` + `GetInitStatus`）
