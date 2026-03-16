> **实现状态：已完成**
>
> - 状态：✅ 已完成
> - 实现位置：`server/internal/cloud/`（`cloud.go`, `connection_manager.go`, `event_router.go`, `handlers.go`, `types.go`）
> - 说明：提案中设计的 Cloud SSE 服务端已完整实现，包括连接管理、事件路由和处理器。

---

# Cloud SSE Server 实现设计文档

## 目录

- [概述](#概述)
- [可行性分析](#可行性分析)
- [架构设计](#架构设计)
- [模块设计](#模块设计)
- [API 设计](#api-设计)
- [认证集成](#认证集成)
- [数据流设计](#数据流设计)
- [连接生命周期](#连接生命周期)
- [错误处理](#错误处理)
- [扩展方案](#扩展方案)
- [实施计划](#实施计划)

---

## 概述

### 背景

costrict-web 作为 CoStrict 平台的云端服务器，需要支持 opencode 设备端（CLI 工具）与 Console App（Web 前端）之间的实时事件通信。参考 `opencode/docs/cloud-sse-architecture.md` 设计文档，本项目承担其中"云端服务器（Console Core）"角色。

### 角色定位

```
opencode CLI (设备端)
    │  POST /cloud/event（推送执行事件）
    │  GET  /cloud/device/:deviceID/event（接收控制指令）
    ▼
costrict-web server（本项目，云端 SSE 中转服务器）
    │  GET  /cloud/workspace/:workspaceID/event（推送给 Console App）
    ▼
Console App / Web 前端
```

### 目标

1. 接收设备端推送的执行事件，路由转发给订阅了对应会话的 Console App 用户
2. 接收 Console App 的控制指令（abort、message），转发给执行该会话的设备端
3. 最小化连接数，每设备 1 个 SSE 连接，每用户 1 个 SSE 连接
4. 支持心跳保活和连接超时自动清理

---

## 可行性分析

### 技术栈匹配度

| 需求 | 现有条件 | 结论 |
|------|---------|------|
| SSE 长连接 | Gin 框架 + `gin-contrib/sse` 已在 go.mod | **直接可用** |
| 认证鉴权 | `middleware.RequireAuth()` Casdoor JWT 验证 | **直接复用** |
| userID 提取 | `middleware.UserIDKey` 常量，从 token 解析 | **直接复用** |
| workspaceID | 对应现有 `Organization.ID`，概念完全一致 | **直接映射** |
| 路由扩展 | 现有 `/api` 路由组，新增 `/cloud` 组无冲突 | **零影响** |
| 并发安全 | Go 标准库 `sync.RWMutex` | **直接使用** |

### 需要新增的内容

| 内容 | 工作量 | 说明 |
|------|--------|------|
| `internal/cloud/` 模块 | 中 | ConnectionManager + EventRouter + Handlers |
| `/cloud` 路由组注册 | 小 | main.go 追加约 15 行 |
| deviceID 概念 | 小 | 初期纯内存，无需新建数据库表 |
| Redis Pub/Sub（可选） | 大 | 单机阶段不需要，水平扩展时再引入 |

### 风险点

1. **Nginx/反向代理缓冲**：SSE 需要设置 `X-Accel-Buffering: no` 响应头，否则事件会被缓冲延迟
2. **连接泄漏**：客户端异常断开时需通过 `context.Done()` 感知，及时清理 Map 索引
3. **goroutine 泄漏**：每个 SSE 连接对应一个 goroutine，必须在连接关闭时正确退出

---

## 架构设计

### 整体架构

```
┌─────────────────────────────────────────────────────────────────┐
│                    Console App / Web 前端                       │
│  GET /cloud/workspace/:workspaceID/event  (SSE 订阅)            │
│  POST /cloud/session/:sessionID/subscribe (订阅会话)            │
└────────────────────────────┬────────────────────────────────────┘
                             │ SSE (每用户 1 连接)
┌────────────────────────────┴────────────────────────────────────┐
│              costrict-web server (本项目)                       │
│                                                                  │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │  /cloud 路由组 (cloud/handlers.go)                      │    │
│  │  - GET  /workspace/:workspaceID/event  → UserSSEHandler │    │
│  │  - GET  /device/:deviceID/event        → DeviceSSEHandler│   │
│  │  - POST /session/:sessionID/subscribe  → SubscribeHandler│   │
│  │  - POST /session/:sessionID/unsubscribe→ UnsubHandler    │   │
│  │  - POST /event                         → EventHandler    │   │
│  │  - GET  /stats                         → StatsHandler    │   │
│  └─────────────────────────────────────────────────────────┘    │
│                                                                  │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │  ConnectionManager (cloud/connection_manager.go)        │    │
│  │  - connections: map[connID]*SSEConnection               │    │
│  │  - userConnections: map[userID]map[connID]struct{}      │    │
│  │  - deviceConnections: map[deviceID]connID               │    │
│  │  - sessionSubscriptions: map[sessionID]map[connID]struct{}│  │
│  │  - 心跳 goroutine (30s)                                 │    │
│  │  - 超时清理 goroutine (10s 检查, 60s 超时)              │    │
│  └─────────────────────────────────────────────────────────┘    │
│                                                                  │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │  EventRouter (cloud/event_router.go)                    │    │
│  │  - 事件类型路由规则                                      │    │
│  │  - 16ms 批处理队列                                       │    │
│  │  - 过期 delta 事件过滤                                   │    │
│  └─────────────────────────────────────────────────────────┘    │
└────────────────────────────┬────────────────────────────────────┘
                             │ SSE (每设备 1 连接)
┌────────────────────────────┴────────────────────────────────────┐
│                    opencode CLI (设备端)                        │
│  GET /cloud/device/:deviceID/event  (SSE 接收控制指令)          │
│  POST /cloud/event                  (推送执行事件)              │
└─────────────────────────────────────────────────────────────────┘
```

### 新增目录结构

```
server/internal/
├── cloud/
│   ├── connection_manager.go   # 连接管理器（核心）
│   ├── event_router.go         # 事件路由器
│   ├── handlers.go             # Gin HTTP Handler
│   ├── types.go                # 公共类型定义
│   └── cloud.go                # 模块初始化入口
├── casdoor/
├── config/
├── database/
├── handlers/                   # 现有 handlers，不修改
├── middleware/
├── models/
└── storage/
```

---

## 模块设计

### types.go — 公共类型

```go
package cloud

// SSEConnection 代表一个活跃的 SSE 长连接
type SSEConnection struct {
    ID          string          // 连接唯一 ID，uuid
    Type        ConnType        // "user" | "device"
    UserID      string          // 所属用户 ID（来自 Casdoor JWT sub）
    DeviceID    string          // 设备 ID（仅 device 类型连接有值）
    WorkspaceID string          // 工作空间 ID（对应 Organization.ID）
    Send        chan Event       // 发送通道，Handler 监听此通道写入 SSE
    Done        chan struct{}    // 关闭信号
    LastActivity int64          // Unix 毫秒时间戳，用于超时检测
}

type ConnType string

const (
    ConnTypeUser   ConnType = "user"
    ConnTypeDevice ConnType = "device"
)

// Event 是在系统内部流转的事件结构
type Event struct {
    Type       string          `json:"type"`
    Properties map[string]any  `json:"properties,omitempty"`
}

// 事件类型常量
const (
    EventCloudConnected   = "cloud.connected"
    EventDeviceConnected  = "device.connected"
    EventHeartbeat        = "heartbeat"
    EventSessionStatus    = "session.status"
    EventSessionCreated   = "session.created"
    EventSessionUpdated   = "session.updated"
    EventMessagePartUpdated = "message.part.updated"
    EventMessagePartDelta   = "message.part.delta"
    EventDeviceStatus     = "device.status"
    EventSessionAbort     = "session.abort"
    EventSessionMessage   = "session.message"
    EventBatch            = "batch"
)

// 连接限制常量
const (
    MaxConnectionsPerUser    = 5
    MaxSubscriptionsPerUser  = 50
    HeartbeatIntervalMs      = 30_000
    ConnectionTimeoutMs      = 60_000
    CleanupIntervalMs        = 10_000
    BatchFlushIntervalMs     = 16
)
```

### connection_manager.go — 连接管理器

```go
package cloud

type ConnectionManager struct {
    mu                   sync.RWMutex
    connections          map[string]*SSEConnection
    userConnections      map[string]map[string]struct{}   // userID -> set<connID>
    deviceConnections    map[string]string                // deviceID -> connID
    sessionSubscriptions map[string]map[string]struct{}  // sessionID -> set<connID>
}

// 对外暴露的方法签名
func NewConnectionManager() *ConnectionManager

func (m *ConnectionManager) RegisterUserConnection(conn *SSEConnection) error
    // 注册用户连接，超出 MaxConnectionsPerUser 返回 error

func (m *ConnectionManager) RegisterDeviceConnection(conn *SSEConnection) error
    // 注册设备连接，已有旧连接则先关闭旧连接（强制单设备单连接）

func (m *ConnectionManager) SubscribeToSession(sessionID, connID string) error
    // 将连接订阅到指定会话，超出 MaxSubscriptionsPerUser 返回 error

func (m *ConnectionManager) UnsubscribeFromSession(sessionID, connID string)
    // 取消订阅

func (m *ConnectionManager) CloseConnection(connID string)
    // 关闭连接，清理所有 Map 索引，向 Done 通道发送信号

func (m *ConnectionManager) RouteEvent(event Event, targetConnIDs []string)
    // 将事件发送到指定连接的 Send 通道

func (m *ConnectionManager) FindUserConnsBySession(sessionID string) []string
    // 查找订阅了该会话的所有用户连接 ID

func (m *ConnectionManager) FindUserConnsByDevice(deviceID string) []string
    // 查找设备所有者的用户连接 ID（通过 deviceConnections 反查）

func (m *ConnectionManager) GetDeviceConn(deviceID string) *SSEConnection
    // 获取设备连接（用于向设备发送控制指令）

func (m *ConnectionManager) Stats() ManagerStats
    // 返回连接统计信息

func (m *ConnectionManager) startHeartbeat()
    // 内部 goroutine：每 30s 向所有连接发送 heartbeat 事件

func (m *ConnectionManager) startCleanup()
    // 内部 goroutine：每 10s 检查超时连接（超过 60s 未活动则关闭）
```

**并发安全设计：**
- 读操作（查找连接、路由事件）使用 `RLock`
- 写操作（注册、关闭、订阅）使用 `Lock`
- `Send` 通道容量设为 64，防止慢消费者阻塞路由

### event_router.go — 事件路由器

```go
package cloud

type EventRouter struct {
    manager    *ConnectionManager
    batchQueue map[string][]Event  // connID -> 待发送事件列表
    staleDeltas map[string]struct{} // 过期 delta key 集合
    mu         sync.Mutex
}

func NewEventRouter(manager *ConnectionManager) *EventRouter

// RouteDeviceEvent 处理来自设备端的事件，路由到对应用户连接
func (r *EventRouter) RouteDeviceEvent(deviceID string, event Event)

// RouteUserCommand 处理来自用户的控制指令，路由到对应设备连接
func (r *EventRouter) RouteUserCommand(sessionID string, event Event)

func (r *EventRouter) startBatchFlush()
    // 内部 goroutine：每 16ms 批量发送队列中的事件
```

**路由规则：**

| 事件类型 | 来源 | 目标连接查找方式 |
|---------|------|----------------|
| `session.*` | 设备 → 用户 | `FindUserConnsBySession(sessionID)` |
| `message.*` | 设备 → 用户 | `FindUserConnsBySession(sessionID)` |
| `device.status` | 设备 → 用户 | `FindUserConnsByDevice(deviceID)` |
| `session.abort` | 用户 → 设备 | `GetDeviceConn(deviceID)` |
| `session.message` | 用户 → 设备 | `GetDeviceConn(deviceID)` |

**过期 delta 过滤逻辑：**

当收到 `message.part.updated` 事件时，将对应的 `{sessionID}:{messageID}:{partID}` 加入 `staleDeltas`。
批量发送时跳过 `staleDeltas` 中存在的 `message.part.delta` 事件。
每次 flush 后清空 `staleDeltas`。

### handlers.go — HTTP Handler

```go
package cloud

// UserSSEHandler 处理 Console App 的 SSE 订阅请求
// GET /cloud/workspace/:workspaceID/event
// 认证：RequireAuth，从 token 提取 userID
// 流程：
//   1. 校验 URL 中 workspaceID 与用户组织成员资格（查 OrgMember 表）
//   2. 创建 SSEConnection，注册到 ConnectionManager
//   3. 发送 cloud.connected 确认事件
//   4. 进入 c.Stream() 循环，监听 conn.Send 通道
//   5. 连接断开时调用 CloseConnection 清理
func UserSSEHandler(manager *ConnectionManager) gin.HandlerFunc

// DeviceSSEHandler 处理 opencode 设备端的 SSE 订阅请求
// GET /cloud/device/:deviceID/event
// 认证：RequireAuth，从 token 提取 userID（设备归属验证）
// 流程：
//   1. 创建 device 类型 SSEConnection，注册到 ConnectionManager
//   2. 发送 device.connected 确认事件
//   3. 进入 c.Stream() 循环，监听 conn.Send 通道（接收控制指令）
//   4. 连接断开时调用 CloseConnection 清理
func DeviceSSEHandler(manager *ConnectionManager) gin.HandlerFunc

// SubscribeHandler 处理会话订阅请求
// POST /cloud/session/:sessionID/subscribe
// Body: { "deviceID": "string" }（可选，用于建立 session→device 映射）
// 认证：RequireAuth
func SubscribeHandler(manager *ConnectionManager) gin.HandlerFunc

// UnsubscribeHandler 处理取消订阅请求
// POST /cloud/session/:sessionID/unsubscribe
// 认证：RequireAuth
func UnsubscribeHandler(manager *ConnectionManager) gin.HandlerFunc

// DeviceEventHandler 接收设备端推送的事件
// POST /cloud/event
// Body: { "deviceID": "string", "sessionID": "string", "event": Event }
// 认证：RequireAuth（设备端携带 token）
func DeviceEventHandler(router *EventRouter) gin.HandlerFunc

// UserCommandHandler 接收用户发出的控制指令（abort/message）
// POST /cloud/command
// Body: { "sessionID": "string", "deviceID": "string", "event": Event }
// 认证：RequireAuth
func UserCommandHandler(router *EventRouter) gin.HandlerFunc

// StatsHandler 返回连接统计信息
// GET /cloud/stats
// 认证：RequireAuth（建议限制为管理员）
func StatsHandler(manager *ConnectionManager) gin.HandlerFunc
```

**SSE 响应头设置：**

```go
c.Header("Content-Type", "text/event-stream")
c.Header("Cache-Control", "no-cache")
c.Header("Connection", "keep-alive")
c.Header("Transfer-Encoding", "chunked")
c.Header("X-Accel-Buffering", "no")   // 禁用 Nginx 缓冲，关键！
```

### cloud.go — 模块初始化入口

```go
package cloud

// Module 是 cloud 模块的顶层对象，持有所有子组件
type Module struct {
    Manager *ConnectionManager
    Router  *EventRouter
}

// New 创建并启动 cloud 模块（启动后台 goroutine）
func New() *Module

// RegisterRoutes 将 /cloud 路由组注册到 Gin Engine
func (m *Module) RegisterRoutes(rg *gin.RouterGroup, casdoorEndpoint string)
```

---

## API 设计

### 端点汇总

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| `GET` | `/cloud/workspace/:workspaceID/event` | RequireAuth | Console App SSE 订阅 |
| `GET` | `/cloud/device/:deviceID/event` | RequireAuth | 设备端 SSE 订阅 |
| `POST` | `/cloud/session/:sessionID/subscribe` | RequireAuth | 订阅会话事件 |
| `POST` | `/cloud/session/:sessionID/unsubscribe` | RequireAuth | 取消订阅会话 |
| `POST` | `/cloud/event` | RequireAuth | 设备推送事件 |
| `POST` | `/cloud/command` | RequireAuth | 用户发送控制指令 |
| `GET` | `/cloud/stats` | RequireAuth | 连接统计信息 |

### 请求/响应格式

#### POST /cloud/session/:sessionID/subscribe

```json
// Request
{
  "deviceID": "device-uuid-xxx"
}

// Response 200
{
  "success": true,
  "connectionID": "conn-uuid-xxx"
}

// Response 429（超出订阅上限）
{
  "error": "subscription limit exceeded",
  "limit": 50
}
```

#### POST /cloud/event

```json
// Request
{
  "deviceID": "device-uuid-xxx",
  "sessionID": "session-uuid-xxx",
  "event": {
    "type": "message.part.delta",
    "properties": {
      "messageID": "msg-uuid",
      "partID": "part-0",
      "delta": "Hello, "
    }
  }
}

// Response 200
{
  "success": true,
  "routedTo": 2
}
```

#### POST /cloud/command

```json
// Request
{
  "sessionID": "session-uuid-xxx",
  "deviceID": "device-uuid-xxx",
  "event": {
    "type": "session.abort",
    "properties": {}
  }
}

// Response 200
{
  "success": true
}

// Response 404（设备未连接）
{
  "error": "device not connected",
  "deviceID": "device-uuid-xxx"
}
```

#### GET /cloud/stats

```json
{
  "totalConnections": 42,
  "userConnections": 20,
  "deviceConnections": 22,
  "sessionSubscriptions": 85,
  "uptime": 3600
}
```

### SSE 事件格式

所有 SSE 事件统一使用 `data:` 字段携带 JSON：

```
event: message
data: {"type":"cloud.connected","properties":{"connectionID":"conn-xxx","workspaceID":"org-xxx"}}

event: message
data: {"type":"heartbeat","properties":{"timestamp":1741600000000}}

event: message
data: {"type":"batch","properties":{"events":[{"type":"message.part.delta",...},...]}}
```

---

## 认证集成

### 复用现有中间件

`/cloud` 路由组统一应用 `middleware.RequireAuth(casdoorEndpoint)`，与现有 `/api` 路由完全一致：

```go
// main.go 中注册
cloudModule := cloud.New()
cloudGroup := r.Group("/cloud")
cloudGroup.Use(middleware.RequireAuth(cfg.CasdoorEndpoint))
cloudModule.RegisterRoutes(cloudGroup, cfg.CasdoorEndpoint)
```

### Handler 中提取用户信息

```go
// 复用现有常量
userID := c.GetString(middleware.UserIDKey)   // Casdoor JWT sub
```

### workspaceID 权限校验

用户订阅 `/cloud/workspace/:workspaceID/event` 时，需验证该用户是该 Organization 的成员：

```go
// 查询 OrgMember 表
var member models.OrgMember
result := db.Where("org_id = ? AND user_id = ?", workspaceID, userID).First(&member)
if result.Error != nil {
    c.AbortWithStatusJSON(403, gin.H{"error": "not a member of this workspace"})
    return
}
```

### deviceID 归属验证

设备端连接 `/cloud/device/:deviceID/event` 时，初期方案：deviceID 由设备端自行生成（UUID），连接时携带有效 JWT 即可，`userID` 作为设备归属标识存入 `SSEConnection.UserID`。

---

## 数据流设计

### 流程 1：设备执行会话，实时推送消息给 Console App

```
opencode CLI                costrict-web server              Console App
    │                              │                               │
    │  POST /cloud/event           │                               │
    │  { deviceID, sessionID,      │                               │
    │    event: message.part.delta}│                               │
    │─────────────────────────────>│                               │
    │                              │ EventRouter.RouteDeviceEvent()│
    │                              │ FindUserConnsBySession(sid)   │
    │                              │ → [connID-A, connID-B]        │
    │                              │ 加入 batchQueue               │
    │                              │ (16ms 后批量发送)             │
    │  { success: true }           │                               │
    │<─────────────────────────────│                               │
    │                              │ SSE batch 事件                │
    │                              │──────────────────────────────>│
    │                              │                               │ UI 更新
```

### 流程 2：Console App 中止会话

```
Console App                 costrict-web server              opencode CLI
    │                              │                               │
    │  POST /cloud/command         │                               │
    │  { sessionID, deviceID,      │                               │
    │    event: session.abort }    │                               │
    │─────────────────────────────>│                               │
    │                              │ EventRouter.RouteUserCommand()│
    │                              │ GetDeviceConn(deviceID)       │
    │                              │ → conn.Send <- abort event    │
    │  { success: true }           │                               │
    │<─────────────────────────────│                               │
    │                              │ SSE session.abort 事件        │
    │                              │──────────────────────────────>│
    │                              │                               │ 中止执行
```

### 流程 3：Console App 建立 SSE 连接并订阅会话

```
Console App                 costrict-web server
    │                              │
    │  GET /cloud/workspace/       │
    │    :workspaceID/event        │
    │─────────────────────────────>│
    │                              │ 验证 workspaceID 成员资格
    │                              │ 创建 SSEConnection
    │                              │ RegisterUserConnection()
    │  SSE: cloud.connected        │
    │<─────────────────────────────│
    │                              │
    │  POST /cloud/session/        │
    │    :sessionID/subscribe      │
    │─────────────────────────────>│
    │                              │ SubscribeToSession(sid, connID)
    │  { success: true }           │
    │<─────────────────────────────│
    │                              │
    │  (持续接收 SSE 事件...)      │
```

---

## 连接生命周期

### 用户连接生命周期

```
建立连接 (GET /cloud/workspace/:id/event)
    │
    ├─ 验证 JWT → 提取 userID
    ├─ 验证 workspaceID 成员资格
    ├─ 检查单用户连接数 ≤ 5
    ├─ 创建 SSEConnection{Type: "user", ...}
    ├─ RegisterUserConnection()
    ├─ 发送 cloud.connected 事件
    │
    └─ c.Stream() 循环
           │
           ├─ case conn.Send: 写入 SSE 事件，更新 lastActivity
           ├─ case conn.Done: 退出循环
           └─ case ctx.Done(): 客户端断开，退出循环
                   │
                   └─ defer CloseConnection(connID)
                          ├─ 从 userConnections 移除
                          ├─ 从 sessionSubscriptions 移除
                          └─ 关闭 Send/Done 通道
```

### 设备连接生命周期

```
建立连接 (GET /cloud/device/:deviceID/event)
    │
    ├─ 验证 JWT → 提取 userID（设备归属）
    ├─ 创建 SSEConnection{Type: "device", DeviceID: deviceID, ...}
    ├─ RegisterDeviceConnection()（若已有旧连接，先关闭旧连接）
    ├─ 发送 device.connected 事件
    │
    └─ c.Stream() 循环（监听控制指令）
           │
           └─ 断开时 defer CloseConnection(connID)
                  └─ 从 deviceConnections 移除
```

### 超时清理

```
startCleanup() goroutine（每 10s 执行）
    │
    └─ 遍历所有 connections
           │
           └─ if now - conn.LastActivity > 60_000ms
                  └─ CloseConnection(connID)
```

### 心跳机制

```
startHeartbeat() goroutine（每 30s 执行）
    │
    └─ 遍历所有 connections
           │
           └─ 非阻塞发送 heartbeat 事件到 conn.Send
                  └─ 更新 conn.LastActivity
```

---

## 错误处理

### HTTP 错误响应规范

| 场景 | HTTP 状态码 | 响应体 |
|------|------------|--------|
| 未携带 token | 401 | `{"error": "Authentication required"}` |
| token 无效/过期 | 401 | `{"error": "Invalid token"}` |
| 非 workspace 成员 | 403 | `{"error": "not a member of this workspace"}` |
| 连接数超限 | 429 | `{"error": "connection limit exceeded", "limit": 5}` |
| 订阅数超限 | 429 | `{"error": "subscription limit exceeded", "limit": 50}` |
| 设备未连接 | 404 | `{"error": "device not connected", "deviceID": "..."}` |
| 请求体解析失败 | 400 | `{"error": "invalid request body"}` |

### Send 通道满时的处理

当 `conn.Send` 通道已满（容量 64）时，采用非阻塞发送，丢弃事件并记录日志：

```go
select {
case conn.Send <- event:
    // 发送成功
default:
    // 通道已满，丢弃事件，记录 warn 日志
    log.Printf("[CloudSSE] WARN: connection %s send buffer full, dropping event %s",
        conn.ID, event.Type)
}
```

### 连接关闭的幂等性

`CloseConnection` 需要保证幂等，多次调用不 panic：

```go
func (m *ConnectionManager) CloseConnection(connID string) {
    m.mu.Lock()
    defer m.mu.Unlock()

    conn, ok := m.connections[connID]
    if !ok {
        return  // 已清理，直接返回
    }

    // 使用 sync.Once 或 select 保证 Done 通道只关闭一次
    select {
    case <-conn.Done:
        // 已关闭
    default:
        close(conn.Done)
    }

    // 清理所有 Map 索引...
    delete(m.connections, connID)
}
```

---

## 扩展方案

### 阶段一：单机内存方案（当前设计）

- ConnectionManager 使用进程内 `sync.RWMutex` + `map`
- 适合单实例部署，开发验证阶段使用
- 重启后所有连接断开，客户端自动重连即可

### 阶段二：Redis Pub/Sub 水平扩展

当需要多实例部署时，引入 Redis：

```
实例 A (用户连接)          Redis Pub/Sub           实例 B (设备连接)
    │                           │                        │
    │                           │  PUBLISH               │
    │                           │  channel: workspace:xxx│
    │                           │<───────────────────────│
    │  SUBSCRIBE                │                        │
    │  channel: workspace:xxx   │                        │
    │──────────────────────────>│                        │
    │  收到事件                 │                        │
    │<──────────────────────────│                        │
    │ 推送给本地用户连接         │                        │
```

**Redis 数据结构设计（预留）：**

```
# 设备→实例映射（用于跨实例路由）
SET device:{deviceID}:instance {instanceID} EX 120

# 会话→设备映射
SET session:{sessionID}:device {deviceID} EX 3600

# Pub/Sub 频道命名
workspace:{workspaceID}   → 推送给该 workspace 下的用户连接
device:{deviceID}          → 推送给指定设备连接
```

**引入依赖（阶段二时添加）：**

```
github.com/redis/go-redis/v9
```

---

## 实施计划

### 阶段一：核心功能（优先级 P0）

1. **`types.go`** — 定义 `SSEConnection`、`Event`、常量
2. **`connection_manager.go`** — 实现 Map 管理、注册/注销、心跳、超时清理
3. **`event_router.go`** — 实现路由规则、批处理队列、delta 过滤
4. **`handlers.go`** — 实现 7 个 HTTP Handler
5. **`cloud.go`** — 模块初始化，`RegisterRoutes`
6. **`main.go` 修改** — 注册 `/cloud` 路由组（约 10 行）

### 阶段二：稳定性增强（优先级 P1）

- 连接数/订阅数限制的单元测试
- 并发安全测试（race detector）
- 超时清理的集成测试
- 监控指标暴露（Prometheus `/metrics` 端点）

### 阶段三：水平扩展（优先级 P2）

- 引入 Redis Pub/Sub
- 跨实例事件广播
- 连接状态持久化

### main.go 修改点（最小侵入）

```go
// 在现有路由注册之后追加以下内容

// Cloud SSE module
cloudModule := cloud.New()
cloudGroup := r.Group("/cloud")
cloudGroup.Use(middleware.RequireAuth(cfg.CasdoorEndpoint))
cloudModule.RegisterRoutes(cloudGroup)
```

---

## 与 opencode 设计文档的对应关系

| opencode 设计文档组件 | 本项目实现位置 |
|----------------------|--------------|
| 云端服务器 (Console Core) | `server/internal/cloud/` 整个模块 |
| ConnectionManager | `cloud/connection_manager.go` |
| EventRouter | `cloud/event_router.go` |
| CloudRoutes (API 端点) | `cloud/handlers.go` + `cloud/cloud.go` |
| 设备端 EventForwarder | opencode CLI 侧实现，本项目提供接收端点 |
| Console App 扩展 | Web 前端侧实现，本项目提供 SSE 端点 |
| Redis 集群 | 阶段二引入，当前阶段纯内存 |

---

**文档版本：** 1.0.0
**创建日期：** 2026-03-10
**维护者：** CoStrict Team
