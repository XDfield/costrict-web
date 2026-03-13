# HTTP 隧道设计文档（yamux over WebSocket）

## 目录

- [背景与目标](#背景与目标)
- [架构概览](#架构概览)
- [核心原理](#核心原理)
- [组件改动范围](#组件改动范围)
- [详细设计](#详细设计)
- [API 设计](#api-设计)
- [数据流设计](#数据流设计)
- [依赖引入](#依赖引入)
- [与现有架构的关系](#与现有架构的关系)
- [实施计划](#实施计划)

---

## 背景与目标

### 现有链路的局限

当前 `cs cloud` 命令已实现设备侧的云端接入：

```
cs cloud 启动
  ├─ 向 server 注册设备（POST /api/devices/register）
  ├─ 申请 Gateway 地址（POST /cloud/device/gateway-assign）
  ├─ 本地启动 cs serve（127.0.0.1:随机端口）
  └─ 连接 Gateway SSE（GET /device/:id/event）接收控制指令
```

SSE 连接是单向的（server → device），仅能下发 `session.abort`、`session.message` 等预定义控制指令。`packages/app`（Console App）无法通过云端访问设备的完整 cs serve API（session 列表、消息历史、文件读取、LSP 等），导致云端模式下 UI 功能严重受限。

### 目标

在不改动 `packages/app` 任何代码的前提下，让 Console App 像访问本地 cs serve 一样访问远端设备，中间的 `server → gateway → device` 链路对 app 完全透明。

所有控制指令（原 SSE 下发的 `session.abort` 等）统一改为通过 HTTP proxy 隧道调用设备 cs serve API，**完全弃用 Gateway SSE 连接**，设备只维护一条 WebSocket 隧道连接。

```
app ──HTTP──▶ [黑盒隧道] ──HTTP──▶ cs serve（设备本地）
```

---

## 架构概览

```
Console App（浏览器）
    │ 普通 HTTP 请求（baseUrl = costrict-web server）
    ▼
costrict-web server
    │ 识别 /cloud/device/:deviceID/proxy/* 请求
    │ 查 GatewayRegistry 找到设备所在 Gateway
    │ 转发给 Gateway（普通 HTTP）
    ▼
Device Gateway
    │ 通过 yamux session 开一个新 stream
    │ 把 HTTP 请求写入 stream
    ▼
cs cloud（设备侧 Go tunnel agent）
    │ Accept() 拿到 stream
    │ 本地 HTTP 转发给 cs serve
    │ 把响应写回 stream
    ▼
Device Gateway（收到响应）
    │ 把 stream 数据组装成 HTTP 响应
    ▼
costrict-web server → Console App
```

设备与 Gateway 之间只维护**一条连接**：

- WebSocket 隧道：承载 yamux 多路复用，用于 HTTP 请求代理
- ~~原有 SSE 连接~~：**完全弃用**，设备上下线通知改由 WebSocket 隧道的连接/断开事件触发

---

## 核心原理

### yamux 多路复用

[hashicorp/yamux](https://github.com/hashicorp/yamux)（⭐ 2.5k，MIT）在任意 `io.ReadWriteCloser` 上建立多路复用会话，每个 HTTP 请求对应一个独立的 yamux stream，互不阻塞。

```
WebSocket 连接（全双工）
  └─ yamux Session
       ├─ Stream 1：GET /session/           （app 请求 A）
       ├─ Stream 2：POST /session/:id/chat  （app 请求 B）
       └─ Stream 3：GET /event             （SSE 事件流）
```

### WebSocket 作为传输层

yamux 需要 `net.Conn` 接口（`Read/Write/Close`）。WebSocket 连接通过适配器包装成 `net.Conn`，即可直接传入 yamux。

```go
// WebSocket → net.Conn 适配（约 30 行）
type wsConn struct{ *websocket.Conn }
func (c *wsConn) Read(b []byte) (int, error)  { ... }
func (c *wsConn) Write(b []byte) (int, error) { ... }
```

### HTTP over yamux stream

每个 stream 是一个标准的 `io.ReadWriteCloser`，可以直接用 `net/http` 的 `http.ReadRequest` / `http.ReadResponse` 读写 HTTP 报文，无需自定义协议。

---

## 组件改动范围

| 组件 | 改动类型 | 具体内容 |
|------|---------|---------|
| `gateway/internal/tunnel.go` | **新增** | `wsConn` 适配器 |
| `gateway/internal/tunnel_handler.go` | **新增** | `DeviceTunnelHandler`：`/device/:id/tunnel` WebSocket 升级 + yamux server session 管理；连接建立/断开时通知 server 设备上下线 |
| `gateway/internal/proxy_handler.go` | **新增** | `DeviceProxyHandler`：`/device/:id/proxy/*` HTTP 代理端点 |
| `gateway/internal/manager.go` | **重写** | 弃用 SSE `ConnectionManager`，改为 `TunnelManager`：`yamuxSessions map[string]*yamux.Session` |
| `gateway/internal/handlers.go` | **删除** | 弃用 `DeviceSSEHandler`、`SendToDeviceHandler` |
| `gateway/internal/router.go` | **重写** | 移除 `/device/:id/event` 和 `/internal/device/:id/send`，注册 `/device/:id/tunnel` 和 `/device/:id/proxy/*` |
| `gateway/internal/types.go` | **精简** | 移除 `DeviceConnection`、`Event`、`SendChannelCapacity` |
| `gateway/go.mod` | **新增依赖** | `github.com/hashicorp/yamux`、`github.com/gorilla/websocket` |
| `cs cloud`（Go 重写） | **新增** | `tunnel/client.go`：连接 `/device/:id/tunnel`，yamux client session，`Accept()` 循环转发；移除原有 SSE 连接逻辑 |
| `server/internal/gateway/client.go` | **重写** | 移除 `SendToDevice`，新增 `ProxyRequest(gatewayInternalURL, deviceID, req, resp)` |
| `server/internal/gateway/handlers.go` | **新增** | `DeviceProxyHandler`：`/cloud/device/:deviceID/proxy/*` 鉴权后转发 |
| `server/internal/cloud/event_router.go` | **修改** | `RouteUserCommand` 改为通过 `ProxyRequest` 调用设备 cs serve API，而非 `SendToDevice` |
| `server/internal/cloud/cloud.go` | **扩展** | `RegisterRoutes` 注册代理路由 |
| `packages/app`（Console App） | **零改动** | `baseUrl` 指向 costrict-web server 即可 |

---

## 详细设计

### gateway/internal/tunnel.go（新增）

```go
package internal

import (
    "io"
    "net"
    "sync"
    "time"

    "github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool { return true },
}

type wsConn struct {
    *websocket.Conn
    reader io.Reader
    mu     sync.Mutex
}

func (c *wsConn) Read(b []byte) (int, error) {
    for {
        if c.reader != nil {
            n, err := c.reader.Read(b)
            if err == io.EOF {
                c.reader = nil
                continue
            }
            return n, err
        }
        _, r, err := c.Conn.NextReader()
        if err != nil {
            return 0, err
        }
        c.reader = r
    }
}

func (c *wsConn) Write(b []byte) (int, error) {
    c.mu.Lock()
    defer c.mu.Unlock()
    err := c.Conn.WriteMessage(websocket.BinaryMessage, b)
    if err != nil {
        return 0, err
    }
    return len(b), nil
}

func (c *wsConn) SetDeadline(t time.Time) error      { return c.Conn.SetReadDeadline(t) }
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.Conn.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.Conn.SetWriteDeadline(t) }
func (c *wsConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (c *wsConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
```

### gateway/internal/manager.go（重写）

原有 SSE `ConnectionManager` 完全替换为 `TunnelManager`：

```go
type TunnelManager struct {
    mu       sync.RWMutex
    sessions map[string]*yamux.Session
}

func NewTunnelManager() *TunnelManager {
    return &TunnelManager{sessions: make(map[string]*yamux.Session)}
}

func (m *TunnelManager) Register(deviceID string, session *yamux.Session) {
    m.mu.Lock()
    defer m.mu.Unlock()
    if old, ok := m.sessions[deviceID]; ok {
        old.Close()
    }
    m.sessions[deviceID] = session
}

func (m *TunnelManager) Get(deviceID string) (*yamux.Session, bool) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    s, ok := m.sessions[deviceID]
    return s, ok
}

func (m *TunnelManager) Close(deviceID string) {
    m.mu.Lock()
    defer m.mu.Unlock()
    if s, ok := m.sessions[deviceID]; ok {
        s.Close()
        delete(m.sessions, deviceID)
    }
}

func (m *TunnelManager) Count() int {
    m.mu.RLock()
    defer m.mu.RUnlock()
    return len(m.sessions)
}
```

### gateway/internal/tunnel_handler.go（新增）

```go
// DeviceTunnelHandler 处理设备建立 yamux 隧道
// GET /device/:deviceID/tunnel（WebSocket 升级）
func DeviceTunnelHandler(manager *TunnelManager, cfg *Config) gin.HandlerFunc {
    return func(c *gin.Context) {
        deviceID := c.Param("deviceID")

        ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
        if err != nil {
            return
        }

        conn := &wsConn{Conn: ws}
        session, err := yamux.Server(conn, yamux.DefaultConfig())
        if err != nil {
            ws.Close()
            return
        }

        manager.Register(deviceID, session)

        go func() {
            if err := NotifyOnline(cfg.ServerURL, cfg.GatewayID, deviceID); err != nil {
                log.Printf("[Gateway] notify online failed for device %s: %v", deviceID, err)
            }
        }()

        // 阻塞直到 session 关闭
        <-session.CloseChan()

        manager.Close(deviceID)
        go func() {
            if err := NotifyOffline(cfg.ServerURL, cfg.GatewayID, deviceID); err != nil {
                log.Printf("[Gateway] notify offline failed for device %s: %v", deviceID, err)
            }
        }()
    }
}
```

### gateway/internal/proxy_handler.go（新增）

```go
// DeviceProxyHandler 将 HTTP 请求通过 yamux stream 转发给设备
// POST/GET /device/:deviceID/proxy/*path
func DeviceProxyHandler(manager *TunnelManager) gin.HandlerFunc {
    return func(c *gin.Context) {
        deviceID := c.Param("deviceID")

        session, ok := manager.Get(deviceID)
        if !ok {
            c.JSON(http.StatusServiceUnavailable, gin.H{"error": "device tunnel not connected"})
            return
        }

        stream, err := session.Open()
        if err != nil {
            c.JSON(http.StatusServiceUnavailable, gin.H{"error": "failed to open tunnel stream"})
            return
        }
        defer stream.Close()

        // 重写请求路径：去掉 /device/:id/proxy 前缀
        path := c.Param("path")
        c.Request.URL.Path = path
        c.Request.RequestURI = path

        if err := c.Request.Write(stream); err != nil {
            c.JSON(http.StatusBadGateway, gin.H{"error": "failed to write request to tunnel"})
            return
        }

        resp, err := http.ReadResponse(bufio.NewReader(stream), c.Request)
        if err != nil {
            c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read response from tunnel"})
            return
        }
        defer resp.Body.Close()

        for k, vs := range resp.Header {
            for _, v := range vs {
                c.Header(k, v)
            }
        }
        c.Status(resp.StatusCode)
        io.Copy(c.Writer, resp.Body)
    }
}
```

### gateway/internal/router.go（重写）

```go
func SetupRouter(manager *TunnelManager, cfg *Config) *gin.Engine {
    r := gin.Default()

    r.GET("/device/:deviceID/tunnel", DeviceTunnelHandler(manager, cfg))
    r.Any("/device/:deviceID/proxy/*path", DeviceProxyHandler(manager))

    return r
}
```

### cs cloud tunnel client（Go，新增）

```go
// tunnel/client.go
package tunnel

import (
    "bufio"
    "fmt"
    "net"
    "net/http"

    "github.com/gorilla/websocket"
    "github.com/hashicorp/yamux"
)

func Connect(gatewayURL, deviceID string, localPort int) error {
    wsURL := fmt.Sprintf("%s/device/%s/tunnel", gatewayURL, deviceID)
    ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
    if err != nil {
        return fmt.Errorf("tunnel dial failed: %w", err)
    }

    conn := &wsConn{Conn: ws}
    session, err := yamux.Client(conn, yamux.DefaultConfig())
    if err != nil {
        return fmt.Errorf("yamux client failed: %w", err)
    }
    defer session.Close()

    for {
        stream, err := session.Accept()
        if err != nil {
            return fmt.Errorf("session closed: %w", err)
        }
        go handleStream(stream, localPort)
    }
}

func handleStream(stream net.Conn, localPort int) {
    defer stream.Close()

    req, err := http.ReadRequest(bufio.NewReader(stream))
    if err != nil {
        return
    }

    req.URL.Scheme = "http"
    req.URL.Host = fmt.Sprintf("127.0.0.1:%d", localPort)
    req.RequestURI = ""

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        http.Error(&responseWriter{stream}, "upstream error", 502)
        return
    }
    defer resp.Body.Close()

    resp.Write(stream)
}
```

`cmd/cloud/main.go` 中移除原有 SSE 连接逻辑，改为只启动隧道：

```go
// 原有：
// go connectSSE(gatewayURL, deviceID)
// go connectTunnel(gatewayURL, deviceID, localPort)

// 新：
tunnel.Connect(gatewayURL, deviceID, localPort)
```

### server/internal/gateway/client.go（重写）

移除 `SendToDevice`，新增 `ProxyRequest`：

```go
// ProxyRequest 将 app 的 HTTP 请求通过 Gateway 代理给设备
func (c *Client) ProxyRequest(gatewayInternalURL, deviceID string, r *http.Request, w http.ResponseWriter) error {
    target := fmt.Sprintf("%s/device/%s/proxy%s", gatewayInternalURL, deviceID, r.URL.Path)
    if r.URL.RawQuery != "" {
        target += "?" + r.URL.RawQuery
    }

    proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
    if err != nil {
        return err
    }
    proxyReq.Header = r.Header.Clone()

    resp, err := c.httpClient.Do(proxyReq)
    if err != nil {
        return fmt.Errorf("gateway unreachable: %w", err)
    }
    defer resp.Body.Close()

    for k, vs := range resp.Header {
        for _, v := range vs {
            w.Header().Add(k, v)
        }
    }
    w.WriteHeader(resp.StatusCode)
    io.Copy(w, resp.Body)
    return nil
}
```

### server/internal/gateway/handlers.go（新增）

```go
// DeviceProxyHandler 鉴权后将请求转发给 Gateway 代理
// ANY /cloud/device/:deviceID/proxy/*path
func DeviceProxyHandler(registry *GatewayRegistry, client *Client) gin.HandlerFunc {
    return func(c *gin.Context) {
        deviceID := c.Param("deviceID")

        gw, err := registry.GetDeviceGateway(deviceID)
        if err != nil {
            c.JSON(http.StatusNotFound, gin.H{"error": "device not connected"})
            return
        }

        if err := client.ProxyRequest(gw.InternalURL, deviceID, c.Request, c.Writer); err != nil {
            c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
        }
    }
}
```

### server/internal/cloud/event_router.go（修改）

`RouteUserCommand` 不再调用 `gatewayClient.SendToDevice`，改为通过 `ProxyRequest` 调用设备 cs serve 对应 API：

```go
func (r *EventRouter) RouteUserCommand(deviceID string, event Event) error {
    gw, err := r.gatewayRegistry.GetDeviceGateway(deviceID)
    if err != nil {
        return fmt.Errorf("device not connected")
    }

    body, _ := json.Marshal(event.Properties)
    path := eventTypeToPath(event.Type)
    req, _ := http.NewRequest(http.MethodPost, "http://placeholder"+path, bytes.NewReader(body))
    req.Header.Set("Content-Type", "application/json")

    var buf bytes.Buffer
    rw := &bufResponseWriter{buf: &buf}
    return r.gatewayClient.ProxyRequest(gw.InternalURL, deviceID, req, rw)
}

// eventTypeToPath 将控制指令类型映射到 cs serve API 路径
// 例如：session.abort → /session/:id/abort
func eventTypeToPath(eventType string) string {
    // 根据实际 cs serve API 路径补充
    ...
}
```

---

## API 设计

### 新增端点汇总

#### Gateway 端点（设备连接）

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET`（WebSocket 升级） | `/device/:deviceID/tunnel` | 设备建立 yamux 隧道；连接建立/断开时通知 server 上下线 |
| `ANY` | `/device/:deviceID/proxy/*path` | Gateway 收到后通过 yamux stream 转发给设备 |

#### Server 端点（Console App 调用）

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| `ANY` | `/cloud/device/:deviceID/proxy/*path` | RequireAuth | 鉴权后转发给 Gateway |

#### 弃用端点

| 端点 | 原用途 | 状态 |
|------|--------|------|
| `GET /device/:deviceID/event` | Gateway SSE 控制指令 | **弃用** |
| `POST /internal/device/:deviceID/send` | Gateway 内部投递 | **弃用** |

#### 不变的端点

| 端点 | 说明 |
|------|------|
| `POST /cloud/device/gateway-assign` | 设备申请 Gateway（保留） |
| `GET /cloud/workspace/:id/event` | Console App SSE 订阅（保留，server → app 推送） |
| `POST /cloud/event` | 设备事件上报（保留） |
| `POST /cloud/command` | 用户指令下发（保留，内部改为 proxy 转发） |

---

## 数据流设计

### 流程 1：设备建立隧道

```
cs cloud（Go）                  Device Gateway
    │                                 │
    │ GET /device/:id/tunnel          │
    │ Upgrade: websocket              │
    │────────────────────────────────>│
    │                                 │ upgrader.Upgrade()
    │                                 │ yamux.Server(wsConn)
    │                                 │ NotifyOnline → server
    │ yamux.Client(wsConn)            │
    │<────────────────────────────────│
    │                                 │
    │ (yamux session 建立，等待 Accept) │
```

### 流程 2：Console App 请求透传

```
Console App    costrict-web server    Device Gateway    cs cloud    cs serve
    │                  │                    │               │           │
    │ GET /cloud/device │                   │               │           │
    │  /:id/proxy/      │                   │               │           │
    │  session/         │                   │               │           │
    │──────────────────>│                   │               │           │
    │                   │ GetDeviceGateway  │               │           │
    │                   │ ProxyRequest()    │               │           │
    │                   │ GET /device/:id/  │               │           │
    │                   │   proxy/session/  │               │           │
    │                   │──────────────────>│               │           │
    │                   │                   │ session.Open()│           │
    │                   │                   │ 写入 HTTP 请求 │           │
    │                   │                   │──────────────>│           │
    │                   │                   │               │ handleStream()
    │                   │                   │               │ GET /session/
    │                   │                   │               │──────────>│
    │                   │                   │               │ 200 OK    │
    │                   │                   │               │<──────────│
    │                   │                   │               │ resp.Write(stream)
    │                   │                   │<──────────────│           │
    │                   │                   │ ReadResponse()│           │
    │                   │ 200 OK            │               │           │
    │                   │<──────────────────│               │           │
    │ 200 OK            │                   │               │           │
    │<──────────────────│                   │               │           │
```

### 流程 3：SSE 事件流透传（`GET /event`）

SSE 是长连接流式响应，`DeviceProxyHandler` 中 `io.Copy` 会持续转发，直到连接断开，无需特殊处理。

```
Console App    server    Gateway    cs cloud    cs serve
    │            │          │           │           │
    │ GET /cloud/device     │           │           │
    │  /:id/proxy/event     │           │           │
    │────────────>│          │           │           │
    │             │──────────>│           │           │
    │             │           │ Open()   │           │
    │             │           │──────────>│           │
    │             │           │           │ GET /event│
    │             │           │           │──────────>│
    │             │           │           │ SSE stream│
    │             │           │ io.Copy   │<──────────│
    │             │ io.Copy   │<──────────│           │
    │ SSE stream  │<──────────│           │           │
    │<────────────│           │           │           │
    │ (持续推送)  │           │           │           │
```

### 流程 4：设备重连

```
cs cloud 重启
  │
  │ 重新申请 Gateway（assignGateway，有缓存则跳过）
  │ 重新连接 Tunnel（GET /device/:id/tunnel）
  │   └─ 连接建立时 Gateway 自动 NotifyOnline
  │
Gateway 感知旧 yamux session 关闭
  │ TunnelManager.Close(deviceID) + NotifyOffline
  │ TunnelManager.Register(deviceID, newSession)  ← 新连接覆盖旧连接
```

---

## 依赖引入

### gateway/go.mod

```
github.com/hashicorp/yamux    v0.1.2
github.com/gorilla/websocket  v1.5.3
```

### cs cloud（Go 模块，新建或复用现有）

```
github.com/hashicorp/yamux    v0.1.2
github.com/gorilla/websocket  v1.5.3
```

---

## 与现有架构的关系

### 现有代码改动范围

| 文件 | 改动类型 | 说明 |
|------|---------|------|
| `gateway/internal/manager.go` | **重写** | 弃用 SSE `ConnectionManager`，改为 `TunnelManager` |
| `gateway/internal/handlers.go` | **删除** | 弃用 `DeviceSSEHandler`、`SendToDeviceHandler` |
| `gateway/internal/router.go` | **重写** | 移除旧路由，注册隧道路由 |
| `gateway/internal/types.go` | **精简** | 移除 SSE 相关类型 |
| `gateway/internal/tunnel.go` | **新增** | `wsConn` 适配器 |
| `gateway/internal/tunnel_handler.go` | **新增** | `DeviceTunnelHandler` |
| `gateway/internal/proxy_handler.go` | **新增** | `DeviceProxyHandler` |
| `server/internal/gateway/client.go` | **重写** | 移除 `SendToDevice`，新增 `ProxyRequest` |
| `server/internal/gateway/handlers.go` | **新增** | `DeviceProxyHandler` |
| `server/internal/cloud/event_router.go` | **修改** | `RouteUserCommand` 改为 proxy 转发 |
| `server/internal/cloud/cloud.go` | **扩展** | 注册代理路由 |
| `server/internal/gateway/registry.go` | 不变 | — |
| `server/internal/gateway/store.go` | 不变 | — |
| `server/internal/gateway/types.go` | 不变 | — |

### 与 DEVICE_GATEWAY_DESIGN.md 的关系

本方案是 DEVICE_GATEWAY_DESIGN.md 的**能力扩展与简化**：

- Gateway 注册、心跳机制**完整保留**
- 设备上下线回调（`NotifyOnline`/`NotifyOffline`）**保留**，触发时机从 SSE 连接改为 WebSocket 隧道连接/断开
- SSE 控制指令链路（`/cloud/command` → `SendToDevice` → 设备）**完全弃用**，改为通过 proxy 隧道调用 cs serve API
- 设备 `device_id` 路由逻辑复用现有 `GatewayRegistry.GetDeviceGateway()`

### 与方案 C（数据中台）的关系

本方案（方案 B）是**过渡方案**，优先解决 Console App 访问设备 API 的问题：

- **方案 B 适用**：实时性要求高的读写操作（session 操作、文件读取、LSP 等）
- **方案 C 适用**：历史数据查询、多设备聚合、离线设备数据访问

两者不互斥，可以逐步将高频只读数据（session 列表、消息历史）迁移到方案 C，降低隧道压力。

---

## 实施计划

### 阶段一：隧道基础设施（当前目标）

**gateway 侧：**

1. `go.mod` 新增 `hashicorp/yamux`、`gorilla/websocket`
2. `gateway/internal/tunnel.go` — `wsConn` 适配器
3. `gateway/internal/manager.go` — 重写为 `TunnelManager`
4. `gateway/internal/handlers.go` — 删除（弃用 SSE handler）
5. `gateway/internal/tunnel_handler.go` — `DeviceTunnelHandler`（含上下线通知）
6. `gateway/internal/proxy_handler.go` — `DeviceProxyHandler`
7. `gateway/internal/router.go` — 重写路由
8. `gateway/internal/types.go` — 移除 SSE 相关类型

**cs cloud 侧（Go 重写）：**

9. `tunnel/wsc.go` — `wsConn` 适配器
10. `tunnel/client.go` — yamux client，`Accept()` 循环 + `handleStream()`
11. `cmd/cloud/main.go` — 移除 SSE 连接，只启动 `tunnel.Connect()`

**server 侧：**

12. `server/internal/gateway/client.go` — 移除 `SendToDevice`，新增 `ProxyRequest`
13. `server/internal/gateway/handlers.go` — 新增 `DeviceProxyHandler`
14. `server/internal/cloud/event_router.go` — `RouteUserCommand` 改为 proxy 转发
15. `server/internal/cloud/cloud.go` — 注册 `/cloud/device/:deviceID/proxy/*path`

### 阶段二：稳定性增强

- 隧道认证：`/device/:id/tunnel` 端点校验 `device_token`（复用现有 `DeviceService.VerifyDeviceToken`）
- 超时控制：yamux stream 级别的读写超时（默认 30s）
- 重连机制：cs cloud 检测 yamux session 关闭后自动重连，指数退避
- 流量限制：单设备并发 stream 数上限（防止 app 并发请求压垮设备）
- 监控指标：活跃 yamux session 数、stream 并发数、请求延迟

### 阶段三：与方案 C 协同

- 高频只读接口（`GET /session/`、`GET /session/:id/message/`）迁移到 server 侧数据中台
- 隧道专注于写操作和实时性要求高的接口
- 逐步缩小隧道代理的路径范围，降低架构复杂度

---

**文档版本：** 1.1.0
**创建日期：** 2026-03-13
**更新日期：** 2026-03-13
**维护者：** CoStrict Team
