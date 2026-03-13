# Device Gateway 架构设计文档

## 目录

- [背景与问题](#背景与问题)
- [架构设计](#架构设计)
- [组件职责](#组件职责)
- [连接生命周期](#连接生命周期)
- [模块设计](#模块设计)
- [API 设计](#api-设计)
- [数据流设计](#数据流设计)
- [与现有代码的关系](#与现有代码的关系)
- [私有化部署](#私有化部署)
- [Redis 水平扩展方案](#redis-水平扩展方案)
- [实施计划](#实施计划)

---

## 背景与问题

### 现有实现的缺陷

当前 `server/internal/cloud/` 中，设备端通过 `GET /cloud/device/:deviceID/event` 与 costrict-web server 直接保持 SSE 长连接（见 `handlers.go:DeviceSSEHandler`，`connection_manager.go:deviceConnections`）。该设计存在以下问题：

1. **空闲连接浪费**：所有注册设备无论是否有用户使用，均持续占用 server 连接数，连接数与设备总数挂钩而非活跃用户数
2. **server 无法主动访问设备**：企业场景下设备位于内网，NAT/防火墙隔离导致 server 无法主动推送，"按需建连"方案不可行
3. **实时性要求**：指令队列+轮询方案引入延迟，不满足实时性要求
4. **水平扩展困难**：设备连接分散在多个 server 实例，跨实例路由复杂

### 解决思路

引入独立的 **Device Gateway** 服务，专职维护设备长连接；costrict-web server 新增 **Gateway 注册中心**（`internal/gateway/` 模块），负责管理 Gateway 实例的注册、心跳、分配，以及设备→Gateway 的映射关系。

**设备连接策略调整：设备启动后立即连接 Gateway，全程保持连接直到设备主动退出。**

---

## 架构设计

### 整体架构

```
Console App (浏览器)
    │ SSE（用户事件订阅，不变）
    ▼
costrict-web server（业务层 + Gateway 注册中心）
    │ 按需 HTTP 调用（指令投递）
    ▼
Device Gateway（独立服务，专职连接管理）
    │ SSE 长连接（设备启动即连接，主动退出才断开）
    ▼
opencode CLI 设备（企业内网）
```

### 网络拓扑

```
企业内网                         │  公网 / DMZ
                                 │
opencode CLI ───────────────────>│  Device Gateway <──── costrict-web server
（设备主动出站，启动即连接）      │  （可内网部署）        （server 调 Gateway）
```

设备始终主动出站连接，完全兼容企业 NAT/防火墙环境。

### 新增目录结构

```
server/internal/
├── cloud/                        # 现有模块，仅修改 event_router.go
│   ├── connection_manager.go     # 移除 deviceConnections 相关逻辑
│   ├── event_router.go           # RouteUserCommand 改为调用 gateway.Client
│   ├── handlers.go               # 移除 DeviceSSEHandler
│   ├── types.go                  # 不变
│   └── cloud.go                  # RegisterRoutes 移除设备SSE路由
├── gateway/                      # 新增模块
│   ├── types.go                  # GatewayInfo、DeviceAllocation 等类型
│   ├── store.go                  # Store 接口 + MemoryStore（阶段一）
│   ├── store_redis.go            # RedisStore（阶段三，需 go-redis/v9）
│   ├── registry.go               # GatewayRegistry：依赖 Store 接口
│   ├── client.go                 # 调用 Gateway 内部接口的 HTTP 客户端
│   └── handlers.go               # server 侧 Handler（注册/心跳/设备上下线/设备分配）
├── casdoor/
├── config/
├── database/
├── handlers/                     # 不变
├── middleware/                   # 不变
├── models/                       # 不变（Device 模型复用）
└── services/                     # DeviceService 复用 SetOnline/SetOffline
```

---

## 组件职责

| 组件 | 职责 |
|------|------|
| costrict-web server | 业务逻辑、用户 SSE 管理、Gateway 注册中心、设备 Gateway 分配、设备→Gateway 映射维护 |
| Device Gateway | 专职维护设备 SSE 长连接、接收 server 指令并投递给设备、设备上下线时回调 server |
| opencode CLI | 启动时向 server 申请 Gateway 地址，立即连接并全程保持，主动退出时断开 |
| Console App | 连接 server SSE 订阅事件，向 server 发送控制指令（不变） |

---

## 连接生命周期

### 设备连接生命周期

```
设备启动
  │
  │ POST /cloud/device/gateway-assign（向 server 申请 Gateway 地址）
  ▼
server 按 region + 负载分配 Gateway，返回 { gatewayURL }
  │
  │ GET {gatewayURL}/device/:deviceID/event（连接 Gateway SSE，暂不校验认证）
  ▼
Gateway 建立设备 SSE 连接
  │
  │ POST {serverURL}/internal/gateway/device/online（回调 server）
  ▼
server 记录 deviceID → gatewayID 映射，调用 DeviceService.SetOnline()
  │
  └─ SSE 长连接保持（设备主动退出前不断开）
         │
         ├─ 接收 server 通过 Gateway 下发的控制指令（session.abort / session.message）
         ├─ Gateway 每 30s 发送 heartbeat 事件保活
         └─ 设备主动断开（进程退出 / 用户主动退出）
                │
                ▼
              Gateway 感知连接关闭
                │
                │ POST {serverURL}/internal/gateway/device/offline（回调 server）
                ▼
              server 清理 deviceID → gatewayID 映射，调用 DeviceService.SetOffline()
```

### Gateway 生命周期

```
Gateway 启动
  │
  │ POST {serverURL}/internal/gateway/register
  ▼
server GatewayRegistry.Register() 记录 Gateway 信息
  │
  └─ 每 30s POST {serverURL}/internal/gateway/:gatewayID/heartbeat
         │ 上报当前连接数 currentConns
         ▼
       server GatewayRegistry.Heartbeat() 更新时间戳和连接数

Gateway 下线（超过 60s 未心跳）
  │
  ▼
server startCleanup() 将该 Gateway 标记为不可用，停止向其分配新设备
```

---

## 模块设计

### gateway/types.go

```go
package gateway

// GatewayInfo 代表一个已注册的 Device Gateway 实例
type GatewayInfo struct {
    ID            string // Gateway 唯一 ID
    Endpoint      string // 设备连接用的公网地址，如 https://gw-01.example.com
    InternalURL   string // server 调用 Gateway 的内部地址，如 http://gw-01.internal:8080
    Region        string // 部署区域，用于就近分配
    Capacity      int    // 最大设备连接数
    CurrentConns  int    // 当前连接数（心跳时上报）
    LastHeartbeat int64  // Unix 毫秒时间戳
}

// DeviceAllocation 设备分配结果
type DeviceAllocation struct {
    GatewayID  string `json:"gatewayID"`
    GatewayURL string `json:"gatewayURL"` // 设备连接用的公网地址
}

const (
    GatewayHeartbeatTimeoutMs = 60_000 // 超过 60s 未心跳则视为下线
    GatewayCleanupIntervalMs  = 10_000
)
```

### gateway/registry.go

```go
package gateway

import "sync"

type GatewayRegistry struct {
    mu            sync.RWMutex
    gateways      map[string]*GatewayInfo // gatewayID → info
    deviceGateway map[string]string       // deviceID → gatewayID
}

func NewGatewayRegistry() *GatewayRegistry

// Register 注册或更新 Gateway 信息（Gateway 启动时调用）
func (r *GatewayRegistry) Register(info *GatewayInfo) error

// Heartbeat 更新 Gateway 心跳时间和当前连接数
func (r *GatewayRegistry) Heartbeat(gatewayID string, currentConns int) error

// Allocate 按 region + 最少连接数策略分配 Gateway
// 过滤心跳超时的 Gateway → 优先同 region → 选 CurrentConns/Capacity 最低者
func (r *GatewayRegistry) Allocate(region string) (*GatewayInfo, error)

// BindDevice 记录 deviceID → gatewayID 映射（Gateway 回调设备上线时触发）
func (r *GatewayRegistry) BindDevice(deviceID, gatewayID string)

// UnbindDevice 解除映射（Gateway 回调设备下线时触发）
func (r *GatewayRegistry) UnbindDevice(deviceID string)

// GetDeviceGateway 查询设备当前所在 Gateway（用于指令路由）
func (r *GatewayRegistry) GetDeviceGateway(deviceID string) (*GatewayInfo, error)

// startCleanup 内部 goroutine：每 10s 清理心跳超时的 Gateway
func (r *GatewayRegistry) startCleanup()
```

### gateway/client.go

```go
package gateway

import (
    "net/http"
    "github.com/costrict/costrict-web/server/internal/cloud"
)

// Client 是 server 调用 Gateway 内部接口的 HTTP 客户端
type Client struct {
    httpClient *http.Client
}

func NewClient() *Client

// SendToDevice 向指定设备投递事件
// POST {gatewayInternalURL}/internal/device/{deviceID}/send
func (c *Client) SendToDevice(gatewayInternalURL, deviceID string, event cloud.Event) error
```

### gateway/handlers.go（server 侧）

```go
package gateway

import (
    "github.com/costrict/costrict-web/server/internal/services"
    "github.com/gin-gonic/gin"
)

// GatewayRegisterHandler 处理 Gateway 注册
// POST /internal/gateway/register
// 不经过 RequireAuth（内网调用）
func GatewayRegisterHandler(registry *GatewayRegistry) gin.HandlerFunc

// GatewayHeartbeatHandler 处理 Gateway 心跳
// POST /internal/gateway/:gatewayID/heartbeat
// Body: { "currentConns": 42 }
func GatewayHeartbeatHandler(registry *GatewayRegistry) gin.HandlerFunc

// DeviceOnlineHandler 处理设备上线回调（Gateway → server）
// POST /internal/gateway/device/online
// Body: { "deviceID": "xxx", "gatewayID": "gw-01" }
// 触发：GatewayRegistry.BindDevice() + DeviceService.SetOnline()
func DeviceOnlineHandler(registry *GatewayRegistry, deviceSvc *services.DeviceService) gin.HandlerFunc

// DeviceOfflineHandler 处理设备下线回调（Gateway → server）
// POST /internal/gateway/device/offline
// Body: { "deviceID": "xxx", "gatewayID": "gw-01" }
// 触发：GatewayRegistry.UnbindDevice() + DeviceService.SetOffline()
func DeviceOfflineHandler(registry *GatewayRegistry, deviceSvc *services.DeviceService) gin.HandlerFunc

// GatewayAssignHandler 设备申请 Gateway 地址
// POST /cloud/device/gateway-assign
// 认证：暂不校验（打通链路阶段）
// Body: { "deviceID": "xxx", "region": "cn-shanghai" }
func GatewayAssignHandler(registry *GatewayRegistry) gin.HandlerFunc
```

### cloud/event_router.go 改动

```go
// 原实现（直接写设备 channel）：
func (r *EventRouter) RouteUserCommand(deviceID string, event Event) error {
    conn := r.manager.GetDeviceConn(deviceID)
    if conn == nil {
        return ErrDeviceNotConnected
    }
    conn.Send <- event
    return nil
}

// 新实现（调用 Gateway HTTP 接口）：
func (r *EventRouter) RouteUserCommand(deviceID string, event Event) error {
    gw, err := r.gatewayRegistry.GetDeviceGateway(deviceID)
    if err != nil {
        return ErrDeviceNotConnected
    }
    return r.gatewayClient.SendToDevice(gw.InternalURL, deviceID, event)
}
```

`EventRouter` 新增两个依赖字段：
```go
type EventRouter struct {
    manager         *ConnectionManager
    gatewayRegistry *gateway.GatewayRegistry // 新增
    gatewayClient   *gateway.Client           // 新增
    mu              sync.Mutex
    batchQueue      map[string][]Event
    staleDeltas     map[string]struct{}
}
```

### cloud/connection_manager.go 改动

移除以下内容：
- `deviceConnections map[string]string` 字段
- `RegisterDeviceConnection()` 方法
- `FindUserConnsByDevice()` 方法（依赖 deviceConnections）
- `GetDeviceConn()` 方法

`ConnTypeDevice` 和 `SSEConnection.DeviceID` 字段同步移除（设备连接不再由 server 管理）。

### main.go 改动

```go
// 新增 Gateway 模块初始化
gatewayRegistry := gateway.NewGatewayRegistry()
gatewayClient := gateway.NewClient()

// 内部路由（不经过 RequireAuth，仅内网可访问）
internalGroup := r.Group("/internal")
gateway.RegisterInternalRoutes(internalGroup, gatewayRegistry, deviceSvc)

// cloud 模块传入 gateway 依赖
cloudModule := cloud.New(gatewayRegistry, gatewayClient)
cloudGroup := r.Group("/cloud")
cloudGroup.Use(middleware.RequireAuth(casdoorEndpoint))
cloudModule.RegisterRoutes(cloudGroup, deviceSvc, casdoorEndpoint)

// 设备申请 Gateway 地址（暂不校验认证，打通链路后再加）
r.POST("/cloud/device/gateway-assign", gateway.GatewayAssignHandler(gatewayRegistry))
```

---

## API 设计

### 端点汇总

#### Server 对外（设备调用）

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| `POST` | `/cloud/device/gateway-assign` | 暂无 | 设备申请 Gateway 地址 |

#### Server 对外（Console App / 不变）

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| `GET` | `/cloud/workspace/:workspaceID/event` | RequireAuth | Console App SSE 订阅 |
| `POST` | `/cloud/session/:sessionID/subscribe` | RequireAuth | 订阅会话 |
| `POST` | `/cloud/session/:sessionID/unsubscribe` | RequireAuth | 取消订阅 |
| `POST` | `/cloud/event` | RequireAuth | 设备推送执行事件 |
| `POST` | `/cloud/command` | RequireAuth | 用户发送控制指令 |
| `GET` | `/cloud/stats` | RequireAuth | 连接统计 |

#### Server 内部（Gateway → Server，仅内网）

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/internal/gateway/register` | Gateway 注册 |
| `POST` | `/internal/gateway/:gatewayID/heartbeat` | Gateway 心跳 |
| `POST` | `/internal/gateway/device/online` | 设备上线回调 |
| `POST` | `/internal/gateway/device/offline` | 设备下线回调 |

#### Gateway 内部接口（Server → Gateway，仅内网）

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/device/:deviceID/event` | 设备 SSE 连接端点 |
| `POST` | `/internal/device/:deviceID/send` | server 向设备投递指令 |

### 请求/响应格式

#### POST /cloud/device/gateway-assign

```json
// Request（设备 → server）
{
  "deviceID": "device-uuid-xxx",
  "region":   "cn-shanghai"
}

// Response 200
{
  "gatewayID":  "gw-01",
  "gatewayURL": "https://gw-01.example.com"
}

// Response 503（无可用 Gateway）
{
  "error": "no gateway available"
}
```

#### POST /internal/gateway/register

```json
// Request（Gateway → server，Gateway 启动时调用）
{
  "gatewayID":   "gw-01",
  "endpoint":    "https://gw-01.example.com",
  "internalURL": "http://gw-01.internal:8080",
  "region":      "cn-shanghai",
  "capacity":    1000
}

// Response 200
{
  "success":           true,
  "heartbeatInterval": 30
}
```

#### POST /internal/gateway/:gatewayID/heartbeat

```json
// Request（Gateway → server，每 30s）
{
  "currentConns": 42
}

// Response 200
{ "success": true }
```

#### POST /internal/gateway/device/online

```json
// Request（Gateway → server，设备连接 Gateway 后立即回调）
{
  "deviceID":  "device-uuid-xxx",
  "gatewayID": "gw-01"
}

// Response 200
{ "success": true }
```

#### POST /internal/gateway/device/offline

```json
// Request（Gateway → server，设备断开后立即回调）
{
  "deviceID":  "device-uuid-xxx",
  "gatewayID": "gw-01"
}

// Response 200
{ "success": true }
```

#### GET /device/:deviceID/event（Gateway 端点，设备连接）

SSE 长连接，响应头：
```
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
X-Accel-Buffering: no
```

连接建立后立即推送确认事件：
```
event: message
data: {"type":"device.connected","properties":{"deviceID":"device-uuid-xxx","gatewayID":"gw-01"}}
```

之后持续推送控制指令和心跳：
```
event: message
data: {"type":"heartbeat","properties":{"timestamp":1741600000000}}

event: message
data: {"type":"session.abort","properties":{"sessionID":"sess-xxx"}}
```

#### POST /internal/device/:deviceID/send（Gateway 端点，server 调用）

```json
// Request（server → Gateway）
{
  "event": {
    "type": "session.abort",
    "properties": { "sessionID": "sess-xxx" }
  }
}

// Response 200
{ "success": true }

// Response 404（设备未连接此 Gateway）
{ "error": "device not connected" }
```

---

## 数据流设计

### 流程 1：设备启动并连接 Gateway

```
opencode CLI              costrict-web server           Device Gateway
    │                            │                             │
    │ POST /cloud/device/        │                             │
    │   gateway-assign           │                             │
    │ { deviceID, region }       │                             │
    │───────────────────────────>│                             │
    │                            │ GatewayRegistry.Allocate()  │
    │ { gatewayURL: "https://    │                             │
    │   gw-01.example.com" }     │                             │
    │<───────────────────────────│                             │
    │                            │                             │
    │ GET {gatewayURL}/device/   │                             │
    │   :deviceID/event（SSE）   │                             │
    │────────────────────────────────────────────────────────>│
    │                            │                             │ 建立连接
    │                            │ POST /internal/gateway/     │
    │                            │   device/online             │
    │                            │<────────────────────────────│
    │                            │ BindDevice()                │
    │                            │ DeviceService.SetOnline()   │
    │ ← SSE: device.connected    │                             │
    │<───────────────────────────────────────────────────────>│
    │                            │                             │
    │ (长连接保持，直到主动退出) │                             │
```

### 流程 2：设备推送执行事件给用户（路径不变）

```
opencode CLI              costrict-web server           Console App
    │                            │                             │
    │ POST /cloud/event          │                             │
    │ { deviceID, sessionID,     │                             │
    │   event: message.part.delta}                            │
    │───────────────────────────>│                             │
    │                            │ EventRouter.RouteDeviceEvent│
    │                            │ FindUserConnsBySession()    │
    │                            │ 加入 batchQueue（16ms批发） │
    │ { success: true }          │                             │
    │<───────────────────────────│                             │
    │                            │ SSE batch 事件              │
    │                            │────────────────────────────>│
```

### 流程 3：用户指令下发给设备（经 Gateway）

```
Console App           costrict-web server       Device Gateway    opencode CLI
    │                        │                        │                 │
    │ POST /cloud/command     │                        │                 │
    │ { deviceID, sessionID,  │                        │                 │
    │   event: session.abort }│                        │                 │
    │───────────────────────>│                        │                 │
    │                        │ RouteUserCommand()      │                 │
    │                        │ GetDeviceGateway(devID) │                 │
    │                        │ → GatewayInfo{gw-01}    │                 │
    │                        │                        │                 │
    │                        │ POST /internal/device/  │                 │
    │                        │   :deviceID/send        │                 │
    │                        │───────────────────────>│                 │
    │                        │                        │ 写入设备SSE连接 │
    │                        │                        │────────────────>│
    │                        │ { success: true }       │                 │
    │                        │<───────────────────────│                 │
    │ { success: true }      │                        │                 │
    │<───────────────────────│                        │                 │
```

### 流程 4：设备主动退出

```
opencode CLI              Device Gateway        costrict-web server
    │                          │                        │
    │ 进程退出/主动断开 SSE    │                        │
    │──────────────────────X  │                        │
    │                          │ 感知连接关闭           │
    │                          │ POST /internal/gateway/│
    │                          │   device/offline       │
    │                          │───────────────────────>│
    │                          │                        │ UnbindDevice()
    │                          │                        │ DeviceService.SetOffline()
    │                          │ { success: true }      │
    │                          │<───────────────────────│
```

---

## 与现有代码的关系

### 现有代码改动范围

| 文件 | 改动类型 | 具体内容 |
|------|---------|---------|
| `cloud/connection_manager.go` | 删减 | 移除 `deviceConnections`、`RegisterDeviceConnection()`、`FindUserConnsByDevice()`、`GetDeviceConn()` |
| `cloud/event_router.go` | 修改 | `RouteUserCommand` 改为调用 `gateway.Client.SendToDevice()`；`EventRouter` 新增 `gatewayRegistry`、`gatewayClient` 字段 |
| `cloud/handlers.go` | 删减 | 移除 `DeviceSSEHandler` |
| `cloud/types.go` | 删减 | 移除 `ConnTypeDevice`、`ErrDeviceNotConnected` |
| `cloud/cloud.go` | 修改 | `New()` 接收 gateway 依赖；`RegisterRoutes` 移除设备SSE路由 |
| `cmd/api/main.go` | 新增 | 初始化 `GatewayRegistry`、`gateway.Client`；注册 `/internal` 路由组；注册 `/cloud/device/gateway-assign` |

### 不变的部分

- `cloud/connection_manager.go` 用户连接管理逻辑全部保留
- `cloud/event_router.go` `RouteDeviceEvent()` 及批处理逻辑不变
- `cloud/handlers.go` 所有用户侧 Handler 不变
- `services/DeviceService` 全部复用（`SetOnline`/`SetOffline` 由 Gateway 回调触发）
- `models.Device` 不变
- 用户 SSE 链路完全不变

### 错误处理

`RouteUserCommand` 调用 Gateway 失败时的处理：

| 场景 | HTTP 响应 |
|------|---------|
| `GetDeviceGateway` 返回错误（设备未分配 Gateway） | 404 `device not connected` |
| Gateway HTTP 调用超时或失败 | 502 `gateway unreachable` |
| Gateway 返回 404（设备未连接此 Gateway） | 404 `device not connected` |

---

## 私有化部署

企业可在内网自建 Device Gateway 并注册到 costrict-web server，实现设备连接完全不出内网：

```
企业内网                                │  公网
                                        │
opencode CLI                            │
    │ 连接企业内网 Gateway               │
    ▼                                   │
企业自建 Device Gateway                 │
    │ POST /internal/gateway/register ──┤──> costrict-web server
    │ POST /internal/gateway/device/online/offline 回调
    │ 接收 server 指令投递（入站，内网） │
    ▼                                   │
opencode CLI 收到指令                   │
```

server 通过注册时上报的 `internalURL` 调用企业 Gateway，企业 Gateway 的 `internalURL` 可以是公网地址（走专线/VPN）或 server 侧可达的内网地址。

---

## Redis 水平扩展方案

### 问题场景

阶段一的 `GatewayRegistry` 使用进程内 `sync.RWMutex + map` 存储，当 costrict-web server 多实例部署时：

```
实例 A                    实例 B
GatewayRegistry           GatewayRegistry
  gateways: {gw-01}         gateways: {gw-01}   ← 各自独立，无法共享
  deviceGateway:            deviceGateway:
    dev-1 → gw-01             dev-2 → gw-01      ← 各自维护不同设备的映射

Console App → 实例A → POST /cloud/command { deviceID: dev-2 }
实例A查本地 deviceGateway → 找不到 dev-2 → 404  ← 路由失败！
```

**核心问题：`deviceGateway`（设备→Gateway 映射）和 `gateways`（Gateway 注册表）必须跨实例共享。**

### Redis 数据结构设计

```
# Gateway 注册表（Hash，存储 GatewayInfo 序列化数据）
HSET gateway:registry gw-01 '{"id":"gw-01","endpoint":"...","internalURL":"...","region":"cn-shanghai","capacity":1000,"currentConns":42}'
HSET gateway:registry gw-02 '{"id":"gw-02",...}'

# Gateway 心跳时间戳（独立 Hash，便于超时检测）
HSET gateway:heartbeat gw-01 1741600000000   # Unix 毫秒
HSET gateway:heartbeat gw-02 1741600001000

# 设备→Gateway 映射（Hash）
HSET gateway:device gw-01 dev-1              # 注意：反向存储，gatewayID → deviceID 不适合查询
# 实际使用正向映射：
HSET device:gateway dev-1 gw-01
HSET device:gateway dev-2 gw-01
HSET device:gateway dev-3 gw-02
```

### 抽象接口设计

将 `GatewayRegistry` 抽象为接口，阶段一内存实现，阶段三 Redis 实现，上层调用方无感知切换：

```go
// gateway/store.go

// Store 是 GatewayRegistry 的存储后端接口
type Store interface {
    // Gateway 管理
    RegisterGateway(info *GatewayInfo) error
    HeartbeatGateway(gatewayID string, currentConns int) error
    ListGateways() ([]*GatewayInfo, error)          // 返回所有 Gateway（含心跳时间）
    RemoveGateway(gatewayID string) error

    // 设备→Gateway 映射
    BindDevice(deviceID, gatewayID string) error
    UnbindDevice(deviceID string) error
    GetDeviceGateway(deviceID string) (string, error) // 返回 gatewayID
}

// MemoryStore 单机内存实现（阶段一）
type MemoryStore struct {
    mu            sync.RWMutex
    gateways      map[string]*GatewayInfo
    heartbeats    map[string]int64
    deviceGateway map[string]string
}

func NewMemoryStore() Store

// RedisStore Redis 实现（阶段三）
type RedisStore struct {
    client *redis.Client
}

func NewRedisStore(client *redis.Client) Store
```

`GatewayRegistry` 依赖 `Store` 接口，不直接持有 map：

```go
type GatewayRegistry struct {
    store  Store           // 可替换的存储后端
    client *Client         // 调用 Gateway 的 HTTP 客户端
}

func NewGatewayRegistry(store Store) *GatewayRegistry
```

`main.go` 按配置选择实现：

```go
var store gateway.Store
if cfg.RedisURL != "" {
    rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisURL})
    store = gateway.NewRedisStore(rdb)
} else {
    store = gateway.NewMemoryStore()
}
gatewayRegistry := gateway.NewGatewayRegistry(store)
```

### RedisStore 实现细节

```go
// gateway/store_redis.go
package gateway

import (
    "context"
    "encoding/json"
    "time"

    "github.com/redis/go-redis/v9"
)

const (
    redisKeyGatewayRegistry  = "gateway:registry"   // Hash: gatewayID → GatewayInfo JSON
    redisKeyGatewayHeartbeat = "gateway:heartbeat"   // Hash: gatewayID → Unix毫秒时间戳
    redisKeyDeviceGateway    = "device:gateway"      // Hash: deviceID → gatewayID
)

type RedisStore struct {
    client *redis.Client
}

func (s *RedisStore) RegisterGateway(info *GatewayInfo) error
    // HSET gateway:registry {gatewayID} {json}
    // HSET gateway:heartbeat {gatewayID} {now}

func (s *RedisStore) HeartbeatGateway(gatewayID string, currentConns int) error
    // 1. HGET gateway:registry {gatewayID} → 反序列化
    // 2. 更新 CurrentConns
    // 3. HSET gateway:registry {gatewayID} {json}
    // 4. HSET gateway:heartbeat {gatewayID} {now}

func (s *RedisStore) ListGateways() ([]*GatewayInfo, error)
    // HGETALL gateway:registry → 反序列化所有 GatewayInfo
    // HGETALL gateway:heartbeat → 合并 LastHeartbeat 字段

func (s *RedisStore) RemoveGateway(gatewayID string) error
    // HDEL gateway:registry {gatewayID}
    // HDEL gateway:heartbeat {gatewayID}

func (s *RedisStore) BindDevice(deviceID, gatewayID string) error
    // HSET device:gateway {deviceID} {gatewayID}

func (s *RedisStore) UnbindDevice(deviceID string) error
    // HDEL device:gateway {deviceID}

func (s *RedisStore) GetDeviceGateway(deviceID string) (string, error)
    // HGET device:gateway {deviceID}
```

### 超时清理的跨实例协调

多实例下，每个 server 实例都运行 `startCleanup()` goroutine，会重复清理同一个超时 Gateway。使用 Redis 分布式锁避免重复操作：

```go
func (r *GatewayRegistry) startCleanup() {
    ticker := time.NewTicker(GatewayCleanupIntervalMs * time.Millisecond)
    for range ticker.C {
        // 尝试获取分布式锁，获取失败则跳过本轮（其他实例在处理）
        ok, _ := r.store.TryLock("gateway:cleanup:lock", 15*time.Second)
        if !ok {
            continue
        }
        r.doCleanup()
    }
}
```

`MemoryStore` 的 `TryLock` 始终返回 true（单机无需锁），`RedisStore` 使用 `SET NX EX` 实现：

```go
// RedisStore.TryLock
func (s *RedisStore) TryLock(key string, ttl time.Duration) (bool, error) {
    return s.client.SetNX(context.Background(), key, "1", ttl).Result()
}
```

### 分配策略在多实例下的一致性

`Allocate()` 每次调用 `store.ListGateways()` 从 Redis 读取最新数据，天然保证多实例看到相同的 Gateway 列表和连接数，无需额外同步。

`CurrentConns` 的更新通过 `HeartbeatGateway` 写入 Redis，各实例读取时均为最新值，分配决策基于全局视图。

### 新增依赖

```
# go.mod 阶段三新增
github.com/redis/go-redis/v9
```

### Server 实例横向扩展的完整性分析

Gateway 层通过 Redis 解决了指令路由问题，但 server 层自身还存在一个跨实例缺口：**用户 SSE 连接的 `sessionSubscriptions` 仍是进程内 map**。

#### 问题场景

```
设备 → 实例A → POST /cloud/event { sessionID: sess-1 }
实例A 查进程内 sessionSubscriptions → 空（用户 SSE 连接在实例B）
→ 事件丢失，用户收不到
```

#### 解法：session 订阅关系写 Redis + Pub/Sub 广播

**新增 Redis 数据结构：**

```
# 会话→workspaceID 映射（订阅时写入，用于确定广播频道）
HSET session:workspace sess-1 workspace-A
HSET session:workspace sess-2 workspace-A

# Pub/Sub 频道（运行时，无持久化）
PUBLISH workspace:{workspaceID} <event JSON>
```

**`Store` 接口新增方法：**

```go
type Store interface {
    // ... 原有 Gateway 和设备映射方法 ...

    // 会话→workspaceID 映射（server 用户 SSE 跨实例路由）
    SetSessionWorkspace(sessionID, workspaceID string) error  // HSET session:workspace
    GetSessionWorkspace(sessionID string) (string, error)     // HGET session:workspace
    DelSessionWorkspace(sessionID string) error               // HDEL session:workspace（取消订阅时清理）

    // Pub/Sub（仅 RedisStore 实现，MemoryStore 直接本地路由）
    Publish(channel string, payload []byte) error
    Subscribe(channel string, handler func(payload []byte)) error
}
```

**`RouteDeviceEvent` 改造：**

```go
func (r *EventRouter) RouteDeviceEvent(deviceID, sessionID string, event Event) {
    // 原有批处理逻辑不变，仅改查找目标连接的方式

    // 单机模式（MemoryStore）：直接查本地 sessionSubscriptions
    // Redis 模式（RedisStore）：
    //   1. store.GetSessionWorkspace(sessionID) → workspaceID
    //   2. store.Publish("workspace:"+workspaceID, eventJSON)
    //      → 所有实例（含本实例）收到后查本地 userConnections 推送
}
```

**`SubscribeHandler` 改造：**

```go
// 订阅会话时同步写 Redis
func SubscribeHandler(...) {
    manager.SubscribeToSession(sessionID, connID)
    store.SetSessionWorkspace(sessionID, conn.WorkspaceID)  // 新增
}

// 取消订阅时清理
func UnsubscribeHandler(...) {
    manager.UnsubscribeFromSession(sessionID, connID)
    store.DelSessionWorkspace(sessionID)  // 新增
}
```

**每个 server 实例启动时订阅自身 workspace 频道：**

```go
// cloud/cloud.go，Redis 模式下启动
store.Subscribe("workspace:*", func(payload []byte) {
    var event Event
    json.Unmarshal(payload, &event)
    workspaceID := extractWorkspaceFromChannel(channel)
    // 查本地 userConnections，找到该 workspace 下的用户连接推送
    manager.RouteToWorkspace(workspaceID, event)
})
```

### 完整 Redis Key 全景

```
# Gateway 层
gateway:registry    Hash   gatewayID → GatewayInfo JSON
gateway:heartbeat   Hash   gatewayID → 心跳时间戳（Unix 毫秒）
device:gateway      Hash   deviceID  → gatewayID

# Server 用户 SSE 层
session:workspace   Hash   sessionID → workspaceID

# 分布式锁
gateway:cleanup:lock  String  SET NX EX（防重复清理）

# Pub/Sub 频道（运行时，无持久化）
workspace:{workspaceID}   设备事件广播给该 workspace 下所有实例的用户连接
```

### 多实例完整部署架构图

```
Console App A          Console App B
    │                       │
    ▼                       ▼
costrict-web 实例A    costrict-web 实例B
  用户连接: u1,u2       用户连接: u3,u4
  GatewayRegistry       GatewayRegistry
  store: RedisStore ────────────────────> Redis
  SUB workspace:ws-1    SUB workspace:ws-1  gateway:registry
                                            gateway:heartbeat
                                            device:gateway
                                            session:workspace
    │                       │
    ▼                       ▼
Device Gateway A      Device Gateway B
    │                       │
    ▼                       ▼
设备群 A               设备群 B
```

**跨实例完整路由示例：**

```
# 场景：设备在 Gateway B，用户 SSE 连接在实例A

设备 → 实例B → POST /cloud/event { sessionID: sess-1, event: message.part.delta }
实例B: store.GetSessionWorkspace("sess-1") → Redis HGET → "ws-1"
实例B: store.Publish("workspace:ws-1", eventJSON)

实例A 订阅了 workspace:ws-1，收到消息
实例A: manager.RouteToWorkspace("ws-1") → 找到本地用户连接 u1,u2
实例A: 推送事件到用户 SSE 连接 ✅

# 场景：Console App 发指令，设备在 Gateway B

Console App → 实例A → POST /cloud/command { deviceID: dev-3 }
实例A: store.GetDeviceGateway("dev-3") → Redis HGET → "gw-02"（Gateway B）
实例A: store.ListGateways() → 找到 gw-02.internalURL
实例A: POST gw-02/internal/device/dev-3/send ✅
```

两条链路均通过 Redis 完成跨实例路由，server 实例完全无状态，可任意横向扩展。

---

## 实施计划

### 阶段一：打通链路（当前目标，暂不校验认证）

**server 侧（`internal/gateway/` 新模块）：**

1. `gateway/types.go` — 定义 `GatewayInfo`、`DeviceAllocation` 类型和常量
2. `gateway/registry.go` — 实现 `GatewayRegistry`（注册、心跳、分配、设备绑定、超时清理）
3. `gateway/client.go` — 实现调用 Gateway 的 HTTP 客户端（`SendToDevice`）
4. `gateway/handlers.go` — 实现 5 个 Handler（Gateway注册/心跳、设备上下线回调、设备分配）
5. `cloud/event_router.go` — `RouteUserCommand` 改为调用 `gateway.Client`
6. `cloud/connection_manager.go` — 移除设备连接相关逻辑
7. `cloud/handlers.go` — 移除 `DeviceSSEHandler`
8. `cloud/cloud.go` — 更新 `New()` 签名和 `RegisterRoutes`
9. `cmd/api/main.go` — 注册新路由

**Device Gateway（独立服务）：**

10. 实现设备 SSE 端点 `GET /device/:deviceID/event`
11. 实现内部投递端点 `POST /internal/device/:deviceID/send`
12. 启动时向 server 注册，定期心跳
13. 设备上下线时回调 server

### 阶段二：稳定性增强

- Gateway 分配接口加认证（复用 `DeviceService.VerifyDeviceToken`）
- Gateway 故障转移：设备检测到 Gateway 断开后，重新向 server 申请分配
- Gateway 优雅下线：通知 server 停止分配新连接
- 单元测试：`GatewayRegistry` 分配策略、并发安全
- 集成测试：设备上下线回调、指令投递链路

### 阶段三：Redis 水平扩展

- `go.mod` 新增 `github.com/redis/go-redis/v9`
- 实现 `gateway/store_redis.go`（`RedisStore`），覆盖 Gateway 层全部 key
- `Store` 接口新增 `TryLock`、`SetSessionWorkspace`、`GetSessionWorkspace`、`DelSessionWorkspace`、`Publish`、`Subscribe` 方法
- `main.go` 按 `cfg.RedisURL` 是否配置选择 `MemoryStore` 或 `RedisStore`
- `startCleanup` 加分布式锁（`TryLock`）防多实例重复清理
- `RouteDeviceEvent` 改为 Redis Pub/Sub 广播，`SubscribeHandler`/`UnsubscribeHandler` 同步写 `session:workspace`
- 每个 server 实例启动时订阅 `workspace:*` 频道，收到消息后路由到本地用户连接
- Gateway 多实例部署
- 监控指标：Gateway 连接数、指令投递成功率、分配延迟

---

**文档版本：** 1.3.0
**创建日期：** 2026-03-13
**维护者：** CoStrict Team
