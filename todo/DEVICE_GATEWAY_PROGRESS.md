# Device Gateway 实施进度

基于 `docs/DEVICE_GATEWAY_DESIGN.md` v1.3.0，阶段一（打通链路）任务跟踪。

---

## 一、基础设施：Redis（docker-compose）

- [ ] `docker-compose.yml` — 新增 Redis 服务
  ```yaml
  redis:
    image: redis:7-alpine
    container_name: costrict-redis
    ports:
      - "6379:6379"
    volumes:
      - redis_data:/data
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 10s
      timeout: 5s
      retries: 5
  volumes:
    redis_data:
  ```
- [ ] `.env.example` — 新增 `REDIS_URL=redis://localhost:6379`
- [ ] `server/.env.example` — 新增 `REDIS_URL=redis://localhost:6379`

---

## 二、Device Gateway 服务（`gateway/` 与 `server/` 同级）

新建独立 Go 服务：`D:\DEV\costrict-web\gateway\`

### 2.1 项目初始化

- [ ] `gateway/go.mod` — `module github.com/costrict/costrict-web/gateway`，依赖 `gin`、`google/uuid`
- [ ] `gateway/cmd/main.go` — 入口，读取环境变量，启动 HTTP 服务

### 2.2 类型定义（`gateway/internal/types.go`）

- [ ] `DeviceConnection` 结构体
  ```go
  type DeviceConnection struct {
      DeviceID     string
      Send         chan []byte   // SSE 原始数据
      Done         chan struct{}
      LastActivity int64
  }
  ```
- [ ] 常量：`HeartbeatInterval = 30s`、`SendChannelCapacity = 64`

### 2.3 连接管理（`gateway/internal/manager.go`）

- [ ] `ConnectionManager` 结构体：`mu sync.RWMutex`、`connections map[deviceID]*DeviceConnection`
- [ ] `Register(deviceID string) *DeviceConnection` — 注册设备连接（已有旧连接先关闭）
- [ ] `Close(deviceID string)` — 关闭连接，幂等
- [ ] `Send(deviceID string, data []byte) error` — 非阻塞写入 Send 通道，通道满时丢弃
- [ ] `startHeartbeat()` — goroutine，每 30s 向所有连接推送 heartbeat 事件
- [ ] `Count() int` — 返回当前连接数（供心跳上报使用）

### 2.4 HTTP Handler（`gateway/internal/handlers.go`）

- [ ] `DeviceSSEHandler` — `GET /device/:deviceID/event`
  - 调用 `manager.Register(deviceID)`
  - 设置 SSE 响应头（`Content-Type: text/event-stream`、`X-Accel-Buffering: no` 等）
  - 推送 `device.connected` 确认事件
  - 循环监听 `conn.Send` / `conn.Done` / `ctx.Done()`
  - 连接断开时调用 `manager.Close(deviceID)` + 回调 server `POST /internal/gateway/device/offline`
- [ ] `SendToDeviceHandler` — `POST /internal/device/:deviceID/send`
  - 解析 `{ "event": Event }` 请求体
  - 调用 `manager.Send(deviceID, data)`
  - 设备不在线返回 404

### 2.5 Server 注册与心跳（`gateway/internal/registration.go`）

- [ ] `Register(serverURL, gatewayID, endpoint, internalURL, region string, capacity int) error`
  - 启动时调用，`POST {serverURL}/internal/gateway/register`
- [ ] `startHeartbeat(serverURL, gatewayID string, manager *ConnectionManager)`
  - goroutine，每 30s `POST {serverURL}/internal/gateway/:gatewayID/heartbeat`
  - Body：`{ "currentConns": manager.Count() }`
- [ ] `NotifyOnline(serverURL, gatewayID, deviceID string) error`
  - `POST {serverURL}/internal/gateway/device/online`
- [ ] `NotifyOffline(serverURL, gatewayID, deviceID string) error`
  - `POST {serverURL}/internal/gateway/device/offline`

### 2.6 路由注册（`gateway/internal/router.go`）

- [ ] `SetupRouter(manager *ConnectionManager, cfg *Config) *gin.Engine`
  - `GET  /device/:deviceID/event` → `DeviceSSEHandler`
  - `POST /internal/device/:deviceID/send` → `SendToDeviceHandler`

### 2.7 配置（`gateway/internal/config.go`）

- [ ] `Config` 结构体，从环境变量读取：
  - `GATEWAY_ID`（默认 `gw-01`）
  - `GATEWAY_PORT`（默认 `8081`）
  - `GATEWAY_ENDPOINT`（设备连接用公网地址）
  - `GATEWAY_INTERNAL_URL`（server 调用用内部地址）
  - `GATEWAY_REGION`（默认 `default`）
  - `GATEWAY_CAPACITY`（默认 `1000`）
  - `SERVER_URL`（costrict-web server 地址）

---

## 三、设备端改造（`D:\DEV\opencode\packages\opencode`）

**现有基础：**
- `src/cli/cmd/cloud.ts` — `cs cloud` 命令，已实现阻塞式运行（`await new Promise(() => {})`），注册设备后启动内部 server 并调用 `connect()`
- `src/costrict/device/client.ts` — 设备注册（`register()`）、本地缓存（`~/.costrict/share/device.json`）、token 轮换
- `src/costrict/device/sse.ts` — SSE 连接管理，当前连接目标为 `{base_url}/cloud/device/:deviceID/event`（直连 server），含指数退避重连（1s→最大 60s）、`control` 事件转发到本地 server

**需要改造的内容：**

### 3.1 Gateway 分配（`src/costrict/device/gateway.ts`，新建）

- [ ] `assignGateway(device: DeviceInfo): Promise<string>` — 向 server 申请 Gateway 地址
  - `POST {device.base_url}/cloud/device/gateway-assign`，body：`{ deviceID: device.device_id }`
  - 返回 `gatewayURL` 字符串
  - 进程内缓存分配结果，避免重复请求

### 3.2 SSE 连接目标改造（`src/costrict/device/sse.ts`）

- [ ] `connect(localPort: number)` 改造：连接前先调用 `assignGateway()` 获取 `gatewayURL`
- [ ] SSE 连接目标由 `{base_url}/cloud/device/:deviceID/event` 改为 `{gatewayURL}/device/:deviceID/event`
- [ ] 认证头由 `Bearer {device_token}` 改为暂不携带（打通链路阶段）
- [ ] 新增事件类型处理：`device.connected`（打印日志确认连接）；原有 `heartbeat`、`control` 逻辑保持不变
- [ ] 断线重连时重新调用 `assignGateway()`（Gateway 可能已切换），清除进程内缓存后重新分配

### 3.3 启动流程（`src/cli/cmd/cloud.ts`）

当前已满足阻塞式运行要求，无需改动：
```ts
// 现有流程（已正确）：
const device = await register()     // 注册/复用设备
const server = Server.listen(...)   // 启动内部 server
connect(server.port!).catch(() => {}) // 连接 Gateway SSE（含重连循环）
await new Promise(() => {})         // 阻塞保持进程
```

---

## 四、Server 改造（`D:\DEV\costrict-web\server`）

### 4.1 Gateway 模块（`server/internal/gateway/`）

- [ ] `types.go` — `GatewayInfo`、`DeviceAllocation` 结构体，常量
- [ ] `store.go` — `Store` 接口定义 + `MemoryStore` 实现
  - Gateway 管理：`RegisterGateway`、`HeartbeatGateway`、`ListGateways`、`RemoveGateway`
  - 设备映射：`BindDevice`、`UnbindDevice`、`GetDeviceGateway`
  - 锁：`TryLock`（MemoryStore 始终返回 true）
- [ ] `registry.go` — `GatewayRegistry`，依赖 `Store` 接口
  - `Allocate(region string)` — 过滤超时 → 优先同 region → 最少连接数
  - `startCleanup()` — 每 10s 清理心跳超时（> 60s）的 Gateway
- [ ] `client.go` — `Client`，HTTP 调用 Gateway 内部接口
  - `SendToDevice(internalURL, deviceID string, event cloud.Event) error`
- [ ] `handlers.go` — 5 个 Handler
  - `GatewayRegisterHandler` — `POST /internal/gateway/register`
  - `GatewayHeartbeatHandler` — `POST /internal/gateway/:gatewayID/heartbeat`
  - `DeviceOnlineHandler` — `POST /internal/gateway/device/online`（调用 `BindDevice` + `DeviceService.SetOnline`）
  - `DeviceOfflineHandler` — `POST /internal/gateway/device/offline`（调用 `UnbindDevice` + `DeviceService.SetOffline`）
  - `GatewayAssignHandler` — `POST /cloud/device/gateway-assign`（暂不校验认证）

### 4.2 Cloud 模块改造（`server/internal/cloud/`）

- [ ] `types.go` — 移除 `ConnTypeDevice`、`ErrDeviceNotConnected`
- [ ] `connection_manager.go` — 移除 `deviceConnections` 字段及相关方法
  - 删除：`RegisterDeviceConnection()`、`FindUserConnsByDevice()`、`GetDeviceConn()`
- [ ] `event_router.go` — `RouteUserCommand` 改为调用 `gateway.Client.SendToDevice()`
  - `EventRouter` 新增 `gatewayRegistry *gateway.GatewayRegistry`、`gatewayClient *gateway.Client` 字段
- [ ] `handlers.go` — 移除 `DeviceSSEHandler`
- [ ] `cloud.go` — `New()` 接收 gateway 依赖，`RegisterRoutes` 移除设备 SSE 路由

### 4.3 路由注册（`server/cmd/api/main.go`）

- [ ] 初始化 `gateway.NewMemoryStore()` 和 `gateway.NewGatewayRegistry(store)`
- [ ] 初始化 `gateway.NewClient()`
- [ ] 注册 `/internal` 路由组（不经过 `RequireAuth`）
- [ ] 注册 `POST /cloud/device/gateway-assign`（不经过 `RequireAuth`）
- [ ] `cloud.New()` 传入 gateway 依赖

---

## 进度记录

| 日期 | 内容 |
|------|------|
| 2026-03-13 | 创建进度文档，确认实施范围（阶段一：打通链路，暂不校验认证） |
| 2026-03-13 | 完成阶段一全部 server 侧和 gateway 服务实现，编译通过 |

## 完成状态（阶段一）

### 一、基础设施
- [x] `docker-compose.yml` — Redis 服务（已存在）
- [x] `.env.example` — 新增 `REDIS_URL=redis://localhost:6379`

### 二、Device Gateway 服务（`gateway/`）
- [x] `gateway/go.mod`
- [x] `gateway/cmd/main.go`
- [x] `gateway/internal/types.go`
- [x] `gateway/internal/config.go`
- [x] `gateway/internal/manager.go`
- [x] `gateway/internal/handlers.go`
- [x] `gateway/internal/registration.go`
- [x] `gateway/internal/router.go`

### 三、设备端改造（`opencode`）
- [x] `src/costrict/device/gateway.ts`（新建）— `assignGateway()` 向 server 申请 Gateway 地址，进程内缓存
- [x] `src/costrict/device/sse.ts` — 断线重连时清除缓存并重新 `assignGateway()`；SSE 目标改为 `{gatewayURL}/device/:deviceID/event`；移除认证头；新增 `device.connected` / `session.abort` / `session.message` 事件处理

### 四、Server 改造
- [x] `server/internal/gateway/types.go`
- [x] `server/internal/gateway/store.go`（Store 接口 + MemoryStore）
- [x] `server/internal/gateway/registry.go`
- [x] `server/internal/gateway/client.go`
- [x] `server/internal/gateway/handlers.go`（5 个 Handler + RegisterInternalRoutes）
- [x] `server/internal/cloud/types.go`（移除 ConnTypeDevice、ErrDeviceNotConnected）
- [x] `server/internal/cloud/connection_manager.go`（移除设备连接相关逻辑）
- [x] `server/internal/cloud/event_router.go`（RouteUserCommand 改为调用 gateway.Client）
- [x] `server/internal/cloud/handlers.go`（移除 DeviceSSEHandler）
- [x] `server/internal/cloud/cloud.go`（New() 接收 gateway 依赖）
- [x] `server/cmd/api/main.go`（注册 /internal 路由组和 /cloud/device/gateway-assign）
