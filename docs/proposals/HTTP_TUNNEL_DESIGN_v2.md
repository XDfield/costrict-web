# HTTP 隧道设计文档（yamux over WebSocket）— 部署架构与协议说明

> **面向读者**：其他业务组 / 跨团队协作者
> **目的**：说明 cs-bridge 设备如何与云端建立透明 HTTP 隧道、各组件部署拓扑、以及隧道协议契约，便于评估接入成本与边界。
> **命名约定**：设备端二进制 `cs-bridge`；云端消费方 `Portal / csc-cli`；隧道承载于 `Gateway`。

---

## 1. 背景与目标

cs-bridge 在用户设备上启动 `cs serve`（本地 HTTP API），提供会话、消息、工作区、权限等能力。云端 Portal / csc-cli 希望像访问本地服务一样访问远端设备的这些 API，链路对调用方完全透明。

为达成该目标，引入基于 **yamux over WebSocket** 的反向 HTTP 隧道：

- 设备主动出站建立一条 WebSocket 长连接到云端 Gateway；
- 在该连接上承载 yamux 多路复用，每个 HTTP 请求复用一个独立 stream；
- 完全弃用旧的 SSE 控制指令通道，所有控制信令统一走 HTTP API。

```
Portal / csc-cli ──HTTP──▶ [黑盒隧道] ──HTTP──▶ cs serve（设备本地，由 cs-bridge 拉起）
```

---

## 2. 部署架构

### 2.1 整体拓扑

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                          ① 客户端 (Consumers)                                 │
│                                                                              │
│   Portal 浏览器                       csc-cli                                │
│   (团队/会话/工作区 UI)               (命令行 + AI Agent)                     │
└────────────────┬─────────────────────────────┬────────────────────────────────┘
                 │ HTTPS                       │ HTTPS + Bearer Token
                 ▼                             ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│                        ② 边缘网关层 (Edge / Ingress)                          │
│                                                                              │
│   ┌─────────────────────────────┐    ┌─────────────────────────────────────┐  │
│   │      APISIX                 │    │       nginx-router (OpenResty)       │  │
│   │  • TLS 终止                  │───▶│  • 从 /device/{id}/ 提取 deviceID    │  │
│   │  • /api/*      → server      │    │  • chash 一致性哈希到 Gateway Pod   │  │
│   │  • /device/*   → nginx-router│    │  • 自动跟踪 Pod IP 漂移              │  │
│   │  • WebSocket 透传            │    │                                     │  │
│   └─────────────────────────────┘    └─────────────────────────────────────┘  │
└────────────────────────────────────────────────────────────────────────────────┘
                 │                                     │
                 │ /api/* +                             │ /device/*
                 │ /cloud/device/:id/proxy/*            │
                 ▼                                     ▼
┌──────────────────────────────────────┐  ┌──────────────────────────────────────┐
│   ③ costrict-web server (Go/Gin)     │  │   ④ Device Gateway                   │
│                                      │  │                                      │
│  • 用户面 API（业务 CRUD）            │  │  • StatefulSet（Pod 名稳定）         │
│  • TunnelProxy 代理入口               │  │  • 承载 yamux session per device     │
│  • GatewayRegistry（device→Pod）     │  │  • 设备上下线事件回调                 │
│  • 鉴权 (RequireAuth)                 │  │  • 多副本水平扩展                     │
└──────────────────┬───────────────────┘  └─────────────────▲────────────────────┘
                   │                          HTTP 代理请求  │
                   └────────────────────────────────────────┘
                                                              │
                                                  yamux over WebSocket
                                                  (单条长连接, 设备出站)
                                                              │
┌──────────────────────────────────────────────────────────────────────────────┐
│                          ⑤ 设备端 (cs-bridge)                                 │
│                                                                              │
│   ┌──────────────────────────────┐    ┌───────────────────────────────────┐  │
│   │ tunnel agent                 │    │ cs serve (本地 HTTP API)          │  │
│   │ • 出站 WebSocket 隧道         │    │ • conversations / events (SSE)    │  │
│   │ • yamux Client session       │    │ • workspace runtime (文件/VCS)    │  │
│   │ • Accept stream → 本地转发    │───▶│ • permissions (请求/回复)          │  │
│   └──────────────────────────────┘    └───────────────────────────────────┘  │
└────────────────────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────────────────┐
│                          ⑥ 支撑服务层                                         │
│   PostgreSQL │ Redis (GatewayStore: deviceID → Gateway Pod 映射)              │
└──────────────────────────────────────────────────────────────────────────────┘
```

### 2.2 部署形态对照

| 部署模式 | 适用环境 | Gateway 形态 | 路由方式 |
|---|---|---|---|
| StatefulSet + 静态 FQDN | 容器云 DNS 不一致 / 跨集群 | Pod 名稳定 `costrict-web-gateway-0/1/...` | nginx-router 按 `replicaCount` 自动生成 Pod FQDN |
| DaemonSet + APISIX chash | 标准 K8s + headless Service | 每节点一个 Pod | APISIX `chash` 按 deviceID 哈希 |
| docker-compose | 开发 / 测试 | 单容器 | 直连 |

### 2.3 端口与网络分段

| 流向 | 端口 / 协议 | 备注 |
|---|---|---|
| Client → APISIX | 443 / HTTPS + WSS | 公网入口，TLS 终止 |
| APISIX → nginx-router | 80 / HTTP | 集群内 |
| nginx-router → Gateway Pod | 8080 / HTTP | 集群内，按 deviceID chash |
| server → Gateway Pod | 8080 / HTTP | 集群内，`/cloud/device/:id/proxy/*` 反向代理 |
| cs-bridge → APISIX | 443 / WSS（出站） | 设备主动出站，单一长连接 |
| Gateway ↔ Redis | 6379 / TCP | 写 GatewayStore |
| server ↔ PostgreSQL | 5432 / TCP | 业务 + 认证数据 |

### 2.4 设备路由原理

**为什么需要 nginx-router chash？**

Gateway 是 StatefulSet / DaemonSet 多副本部署，单台设备必须**稳定地路由到同一个 Gateway Pod**，因为该 Pod 持有与设备的 yamux session。

- APISIX 将 `/device/{deviceID}/...` 转给 nginx-router；
- nginx-router 从 URI 提取 `deviceID`，作为 chash key 一致性哈希到某个 Gateway Pod；
- Pod 扩缩容时，仅受影响区间的 deviceID 迁移，其余设备无感知；
- Pod IP 漂移由 nginx-router 周期解析跟踪，运维只维护 `replicaCount`。

### 2.5 高可用与扩缩容

| 维度 | 策略 |
|---|---|
| Gateway 水平扩展 | StatefulSet 增减 `replicaCount` + nginx-router 自动发现 |
| 设备重连 | cs-bridge 检测 yamux session 关闭后，指数退避重连；新连接自动覆盖旧 session |
| 单 Pod 故障 | nginx-router 健康检查剔除，相关设备触发重连到新 Pod |
| 隧道流量隔离 | 业务 API 流量（/api/*）与隧道流量（/device/*、/cloud/device/:id/proxy/*）在 APISIX 分流 |

---

## 3. 协议说明

### 3.1 协议栈

```
┌────────────────────────────────────────────────────┐
│  应用层：HTTP/1.1（cs serve API：REST + SSE）       │
├────────────────────────────────────────────────────┤
│  多路复用层：yamux（每个 HTTP 请求 = 一个 stream）   │
├────────────────────────────────────────────────────┤
│  传输层：WebSocket（Binary Frame，全双工）           │
├────────────────────────────────────────────────────┤
│  安全层：TLS（APISIX 终止；集群内明文）              │
├────────────────────────────────────────────────────┤
│  网络层：TCP                                        │
└────────────────────────────────────────────────────┘
```

### 3.2 yamux 多路复用

[hashicorp/yamux](https://github.com/hashicorp/yamux) 在任意 `io.ReadWriteCloser` 上建立多路复用会话。每个 HTTP 请求对应一个独立 stream，互不阻塞。

```
单条 WebSocket 连接（设备 ↔ Gateway Pod）
  └─ yamux Session
       ├─ Stream 1：GET /session/           （客户端请求 A）
       ├─ Stream 2：POST /session/:id/chat  （客户端请求 B）
       ├─ Stream 3：GET /event              （SSE 长连接流）
       └─ Stream N：...                     （并发上限内任意扩展）
```

**关键特性**：
- 一个设备**全生命周期只维护一条 WebSocket**；
- 多个 HTTP 请求在该连接上并发，无需队头阻塞；
- stream 关闭不影响 session；
- 客户端 / 服务端任意一方均可 `Open()` 发起 stream。

### 3.3 WebSocket 升级握手

设备出站连接端点：`GET /device/:deviceID/tunnel`

```http
GET /device/{deviceID}/tunnel HTTP/1.1
Host: api.example.com
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Key: {random}
Sec-WebSocket-Version: 13
```

- APISIX 透传 WebSocket 升级，不做粘滞；
- nginx-router 按 `deviceID` chash 选 Pod，转发升级请求；
- Gateway Pod 完成 `Upgrader.Upgrade()`，建立 yamux Server session；
- 连接建立 / 断开时，Gateway 回调 server 标记设备 online / offline。

### 3.4 HTTP over yamux stream

每个 yamux stream 是一个标准 `io.ReadWriteCloser`，直接用 `net/http` 的 `http.ReadRequest` / `http.ReadResponse` 读写 HTTP 报文，**无需自定义应用协议**。

请求方向（Gateway → 设备）：

```
Gateway.Open(stream)
   └─ 写入完整 HTTP 请求报文（Request-Line + Headers + Body）
        └─ cs-bridge Accept(stream)
             └─ http.ReadRequest() 解析
                  └─ 转发到本地 cs serve（127.0.0.1:{localPort}）
                       └─ resp.Write(stream) 写回 HTTP 响应
```

### 3.5 路径映射规则

| 客户端调用 | server 内部转发 | Gateway stream 写入 |
|---|---|---|
| `ANY /cloud/device/:deviceID/proxy/session/` | `ANY /device/:deviceID/proxy/session/` | `ANY /session/` |
| `ANY /cloud/device/:deviceID/proxy/session/:id/chat` | `ANY /device/:deviceID/proxy/session/:id/chat` | `ANY /session/:id/chat` |
| `ANY /cloud/device/:deviceID/proxy/event` | `ANY /device/:deviceID/proxy/event` | `ANY /event`（SSE 流式） |

**前缀剥离规则**：`/cloud/device/:deviceID/proxy` 与 `/device/:deviceID/proxy` 两层前缀在链路中逐层剥离，最终到达 cs serve 的路径即客户端语义路径。

### 3.6 SSE 流式响应处理

`GET /event` 这类 SSE 长连接响应通过 yamux stream 持续 `io.Copy`，直到任一方关闭 stream 或底层 session。客户端侧表现为标准 SSE，无感知隧道存在。

### 3.7 设备上下线事件

- **Online**：Gateway 的 `/device/:id/tunnel` WebSocket 握手成功 + yamux Server 建立后，回调 server；
- **Offline**：yamux session 关闭（`CloseChan` 触发）后，回调 server；
- 旧 SSE 通道的上下线语义**完全由隧道连接/断开事件取代**。

### 3.8 鉴权与安全

| 资源 | 鉴权方式 |
|---|---|
| `/cloud/device/:deviceID/proxy/*`（客户端调用） | `RequireAuth`（Casdoor/cs-user JWT） |
| `/device/:deviceID/tunnel`（设备出站） | `device_token` 校验（复用 `DeviceService.VerifyDeviceToken`） |
| Gateway ↔ server 内部通信 | 集群内 mTLS（推荐）或 namespace 隔离 |
| WebSocket 帧 | Binary Message；不含敏感凭据（凭据在 HTTP Header 内） |

### 3.9 流控与稳定性边界

| 维度 | 默认 / 建议 |
|---|---|
| yamux stream 读写超时 | 30s（stream 级） |
| 单设备并发 stream 上限 | 按设备负载评估（防止客户端并发请求压垮设备） |
| 重连退避 | 指数退避（建议初始 1s，上限 30s） |
| 监控指标 | 活跃 yamux session 数、stream 并发数、请求延迟、重连次数 |

---

## 4. 端点清单（参考）

### 4.1 设备 → 云端（cs-bridge 出站）

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/api/devices/register` | 设备注册（保留） |
| `POST` | `/cloud/device/gateway-assign` | 申请 Gateway 地址（保留） |
| `GET`（WS 升级） | `/device/:deviceID/tunnel` | 建立 yamux 隧道（**新增**） |

### 4.2 客户端 → 云端（Portal / csc-cli）

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| `ANY` | `/cloud/device/:deviceID/proxy/*path` | `RequireAuth` | 透传到设备 cs serve（**新增**） |
| `GET` | `/cloud/workspace/:id/event` | `RequireAuth` | 客户端 SSE 订阅（保留） |

### 4.3 弃用端点

| 端点 | 状态 |
|---|---|
| `GET /device/:deviceID/event` | **弃用**（旧 Gateway SSE 控制指令） |
| `POST /internal/device/:deviceID/send` | **弃用**（旧 Gateway 内部投递） |

---

## 5. 数据流（典型场景）

### 5.1 设备建立隧道

```
cs-bridge                       APISIX → nginx-router → Gateway Pod
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

### 5.2 客户端请求透传

```
Portal/csc-cli  server    Gateway    cs-bridge   cs serve
    │              │          │           │           │
    │ GET /cloud/device     │           │           │
    │  /:id/proxy/session/  │           │           │
    │────────────>│          │           │           │
    │             │ GetDeviceGateway     │           │
    │             │ ProxyRequest()       │           │
    │             │──────────>│           │           │
    │             │           │ Open()+Write HTTP    │
    │             │           │──────────>│           │
    │             │           │           │ handleStream()
    │             │           │           │ GET /session/
    │             │           │           │──────────>│
    │             │           │           │ 200 OK    │
    │             │           │           │<──────────│
    │             │           │           │ resp.Write(stream)
    │             │           │<──────────│           │
    │             │ ReadResponse()       │           │
    │             │<──────────│           │           │
    │ 200 OK      │          │           │           │
    │<────────────│          │           │           │
```

### 5.3 SSE 事件流透传

```
Portal/csc-cli  server    Gateway    cs-bridge   cs serve
    │              │          │           │           │
    │ GET /cloud/device/:id/proxy/event  │           │
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
    │<────────────│          │           │           │
    │ (持续推送, 单 stream)   │           │           │
```

### 5.4 设备重连

```
cs-bridge 重启
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

## 6. 接入边界与约束

| 项目 | 边界 |
|---|---|
| 客户端无感知 | Portal / csc-cli 只需把 `baseUrl` 指向 server，对设备 API 的调用语义零变化 |
| 设备单连接 | 一台 cs-bridge 全生命周期仅维护一条出站 WebSocket；不主动开放入站端口 |
| 路径前缀 | 客户端必须使用 `/cloud/device/:deviceID/proxy/*`，不能直连 `/device/*` |
| 透明 HTTP | 请求 / 响应（含 Header / Body / SSE）按 HTTP 语义透传，链路不解析业务报文 |
| 并发上限 | 单设备 stream 并发受 yamux 与设备负载双重约束，超大并发需评估 |
| 离线设备 | 设备离线时隧道不可达，客户端请求返回 `503 device tunnel not connected` |

---

## 7. 命名映射（v1 → v2）

| v1 旧称 | v2 新称 | 说明 |
|---|---|---|
| `cs cloud`（命令/二进制） | `cs-bridge` | 设备端二进制，承载 tunnel agent + cs serve |
| Console App | Portal / csc-cli | 云端消费方（浏览器 UI / 命令行） |
| `packages/app` | Portal 前端 | 同上，UI 实现侧 |
| `cmd/cloud/main.go` | `cmd/cs-bridge/main.go` | 二进制入口路径 |
| cs serve | cs serve | 不变（仍由 cs-bridge 本地拉起） |

---

**文档版本：** 2.1.0（部署架构与协议说明版）
**创建日期：** 2026-03-13
**更新日期：** 2026-07-28
**维护者：** CoStrict Team
