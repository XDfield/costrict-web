# Costrict-Cloud 系统架构总览

本文档整合 server / gateway / cs-bridge / csc-cli 四大组件的完整架构关系与职责划分，作为跨文档统一参考。

> 相关文档：
> - [HTTP 隧道设计](./proposals/HTTP_TUNNEL_DESIGN.md) — server ↔ gateway ↔ cs-bridge 隧道链路细节
> - [系统设计](./SYSTEM_DESIGN.md) — server 业务模块划分
> - [用户中心设计](./identity-tenant/CS_USER_SERVICE_DESIGN.md) — cs-user 与下游契约
> - [Gateway 部署](./deployment/gateway-statefulset-static.md) — Gateway StatefulSet 拓扑
> - [术语表](./GLOSSARY.md)

---

## 1. 自顶向下分层视图（纵向职责链）

```
                            ┌──────────────────────────────┐
                            │       最终用户 / 终端设备      │
                            └──────────────┬───────────────┘
                                           │
=========== ▼ ① 用户接入层 (Clients) =================================================
                            ┌──────────────┴───────────────┐
                            │  Portal 浏览器 / csc-cli      │
                            │  (团队/仓库/capability 入口)   │
                            └──────────────┬───────────────┘
                                           │ HTTPS
=========== ▼ ② 边缘网关层 (Edge / Ingress) ==========================================
                            ┌──────────────┴───────────────┐
                            │  APISIX → nginx-router        │
                            │  (TLS 终止 + 路由 + chash)     │
                            └──────────────┬───────────────┘
                                           │
                                            ├── /api/* ────────────────┐
                                            └── /device/* ──────┐      │
                                                                 ▼      ▼
=========== ▼ ③ 业务核心层 (costrict-web server) =====================================
                            ┌──────────────────────────────────────┐
                            │  用户面 API + 内部 API + TunnelProxy  │
                            │  + Worker 池（Catalog/Scan/Build）    │
                            └──┬───────────────┬───────────────┬───┘
                               │               │               │
=========== ▼ ④ 隧道层 (Device Gateway) ▼ ⑤ 设备端 (cs-bridge) =====================
                            ┌──┴───────┐   ┌───┴──────────┐   ┌─┴────────────┐
                            │ Gateway  │◄─►│ cs-bridge    │   │ 支撑服务      │
                            │ (yamux)  │WS │ tunnel agent │   │ (cs-user /    │
                            └──────────┘   │ + cs serve   │   │  Casdoor /    │
                                           └──────────────┘   │  Gitea / DB)  │
                                                              └───────────────┘
```

> 纵向看：每一层只与相邻层通信，下层不感知上层业务语义；横向看：隧道链路（Gateway ↔ cs-bridge）与业务链路（server ↔ 支撑服务）在 server 内汇合。

---

## 2. 完整架构图（ASCII 总览）

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                          ① 用户接入层 (Clients)                              │
│                                                                              │
│   浏览器 (Portal)             csc-cli                                       │
│   Next.js + React 19         命令行 + AI Agent                               │
│        │                          │                                          │
│        │ HTTPS                     │ HTTPS + Bearer Token                    │
└────────┼──────────────────────────┼─────────────────────────────────────────┘
         │                          │
         ▼                          ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│                        ② 边缘网关层 (Edge / Ingress)                          │
│                                                                              │
│   ┌─────────────────────────────┐    ┌─────────────────────────────────────┐  │
│   │      APISIX                 │    │       nginx-router (OpenResty)       │  │
│   │  • TLS 终止                  │───▶│  • 从 /device/{id}/ 提取 deviceID    │  │
│   │  • /api/* → server           │    │  • chash 一致性哈希到 Gateway Pod   │  │
│   │  • /device/* → nginx-router  │    │  • 自动跟踪 Pod IP 漂移              │  │
│   │  • WebSocket 透传            │    │                                     │  │
│   └─────────────────────────────┘    └─────────────────────────────────────┘  │
└────────────────────────────────────────────────────────────────────────────────┘
         │                                     │
         │ /api/*                               │ /device/*
         ▼                                     ▼
┌──────────────────────────────────────┐  ┌──────────────────────────────────────┐
│   ③ costrict-web server (Go/Gin)     │  │   ④ Device Gateway                   │
│                                      │  │                                      │
│ ┌──────────────────────────────────┐ │  │  • WebSocket 隧道入口                │
│ │ 用户面 API (/api/*)               │ │  │  • yamux 多路复用（一连接多 stream） │
│ │  • Auth / Org / Repo              │ │  │  • HTTP over yamux stream 反向代理  │
│ │  • Capability (skill/agent/       │ │  │  • 设备上下线事件（替代旧 SSE）      │
│ │    command/mcp/plugin)            │ │  │  • 多 Pod 水平扩展 (StatefulSet)    │
│ │  • Marketplace                     │ │  │  • GatewayRegistry 同步到 Redis     │
│ │  • Device Management              │  └──────────────────────────────────────┘
│ │  • ClawAgent (trpc-agent-go)      │ │                  ▲
│ └──────────────────────────────────┘ │                  │ yamux over WebSocket
│ ┌──────────────────────────────────┐ │                  │ (单条长连接)
│ │ 内部 API (/api/internal/*)        │ │                  │
│ │  • 被 cs-user / cs-bridge 调用   │ │                  │
│ └──────────────────────────────────┘ │                  │
│ ┌──────────────────────────────────┐ │                  │
│ │ 隧道代理 TunnelProxy              │ ───── HTTP 请求 ────┘
│ │  • 识别 /cloud/device/:id/proxy/* │ │  (按 deviceID 查 GatewayRegistry)
│ │  • 转发给对应 Gateway Pod          │ │
│ └──────────────────────────────────┘ │
│ ┌──────────────────────────────────┐ │
│ │ Worker 池                         │ │
│ │  • Catalog Ingest (capability     │ │
│ │    内容同步 + 安全扫描)            │ │
│ │  • Plugin build pipeline          │ │
│ │  • 异步任务                        │ │
│ └──────────────────────────────────┘ │
└──────────────────────────────────────┘
         │                                     │
         │ SQL / 内部 HTTP                      │ HTTP Proxy
         ▼                                     ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│                        ⑤ 设备端 (cs-bridge)                                   │
│                                                                              │
│   ┌──────────────────────────────┐    ┌───────────────────────────────────┐  │
│   │ cs-bridge tunnel agent       │    │ cs serve (本地 HTTP API)          │  │
│   │ • 向 server 注册设备          │    │ • /api/v1/conversations          │  │
│   │ • 申请 Gateway 地址            │    │ • /api/v1/events (SSE)           │  │
│   │ • 建立 WebSocket 隧道          │    │ • workspace runtime (文件/VCS)   │  │
│   │ • Accept yamux stream          │    │ • permissions (请求/回复)        │  │
│   │ • 转发到本地 cs serve          │    │ • AI Agent runtime               │  │
│   └──────────────────────────────┘    └───────────────────────────────────┘  │
└────────────────────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────────────────┐
│                          ⑥ 支撑服务层 (Supporting Services)                  │
│                                                                              │
│   ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐    │
│   │ cs-user      │  │ Casdoor      │  │ Gitea fork   │  │ dept-sync    │    │
│   │ (新, 用户中心) │  │ (旧 IAM, 灰度)│  │ (Git Server) │  │ (组织树同步)  │    │
│   │ • 验 JWT     │  │ • OAuth/OIDC │  │ • team/user  │  │              │    │
│   │ • UserInfo   │  │ • RBAC       │  │   仓库托管    │  │              │    │
│   └──────────────┘  └──────────────┘  └──────────────┘  └──────────────┘    │
│                                                                              │
│   ┌────────────────────────────┐    ┌──────────────────────────────────┐     │
│   │ PostgreSQL                 │    │ Redis                            │     │
│   │ • organization/user/role   │    │ • GatewayStore (device→Pod 映射) │     │
│   │ • capability_registry      │    │ • Session 缓存                   │     │
│   │ • clawagent_sessions       │    │                                  │     │
│   └────────────────────────────┘    └──────────────────────────────────┘     │
└────────────────────────────────────────────────────────────────────────────────┘
```

## 3. Mermaid 版本（可直接渲染）

```mermaid
flowchart TB
    subgraph CLIENT["① 用户接入层"]
        BROWSER[浏览器 Portal]
        CSC[csc-cli<br/>命令行 + AI Agent]
    end

    subgraph EDGE["② 边缘网关层"]
        APISIX[APISIX<br/>TLS 终止 + WS 透传]
        NGINX[nginx-router<br/>chash 哈希到 Gateway]
        APISIX --> NGINX
    end

    subgraph SERVER["③ costrict-web server (Go/Gin)"]
        USER_API[用户面 API<br/>Auth/Org/Repo/Capability<br/>Marketplace/ClawAgent]
        INTERNAL_API[内部 API<br/>/api/internal/*]
        TUNNEL[TunnelProxy<br/>/cloud/device/:id/proxy/*]
        WORKER[Worker 池<br/>Catalog/Scan/Plugin build]
    end

    subgraph GW["④ Device Gateway"]
        GATEWAY[Gateway StatefulSet<br/>yamux over WebSocket<br/>设备上下线事件]
    end

    subgraph DEVICE["⑤ 设备端 cs-bridge"]
        TUNNEL_AGENT[cs-bridge tunnel agent<br/>注册 + WebSocket 隧道]
        CS_SERVE[cs serve<br/>本地 HTTP API + AI Agent runtime]
        TUNNEL_AGENT -.本地.-> CS_SERVE
    end

    subgraph SUPPORT["⑥ 支撑服务层"]
        CS_USER[cs-user<br/>用户中心]
        CASDOOR[Casdoor<br/>旧 IAM]
        GITEA[Gitea fork<br/>Git Server]
        PG[(PostgreSQL)]
        REDIS[(Redis<br/>GatewayStore)]
    end

    BROWSER & CSC -->|HTTPS| APISIX
    APISIX -->|/api/*| USER_API
    APISIX -->|/device/*| NGINX
    NGINX --> GATEWAY
    USER_API --> TUNNEL
    TUNNEL -->|HTTP via yamux| GATEWAY
    GATEWAY <-->|WebSocket 隧道| TUNNEL_AGENT
    TUNNEL_AGENT <-.Accept stream.- CS_SERVE
    USER_API --> PG
    USER_API --> REDIS
    GATEWAY -.注册.- REDIS
    USER_API --> CS_USER
    CS_USER --> CASDOOR
    USER_API --> GITEA
    CS_USER --> PG
    CSC -.PAT.-> GITEA
    CSC -.OAuth.-> CASDOOR
```

---

## 4. 各组件职责一览

| 层 | 组件 | 核心职责 | 不做什么 |
|---|---|---|---|
| **① 接入** | 浏览器 Portal | 团队/仓库/capability 管理 UI、ClawAgent 对话 | 不直连设备 |
| | csc-cli | 命令行工具：capability 安装/同步、登录、KB/WF 子命令、AI Agent 流程 | 不承载业务逻辑 |
| **② 边缘** | APISIX | TLS 终止、路由分发、WebSocket 透传 | 不做粘滞、不解析业务 |
| | nginx-router | chash 一致性哈希、按 deviceID 路由到 Gateway Pod | 不终止 TLS |
| **③ Server** | 用户面 API | 业务核心：认证/组织/仓库/capability/marketplace/agent | 不直接连设备 |
| | 内部 API | 给 cs-user / cs-bridge / app-ai-native 的服务间调用 | 不暴露公网 |
| | TunnelProxy | 识别 `/cloud/device/:id/proxy/*`，按 deviceID 查 GatewayRegistry 后转给对应 Pod | 不持有长连接 |
| | Worker 池 | Catalog 内容同步 + 安全扫描、plugin build、异步任务 | 不在请求路径上 |
| **④ Gateway** | Gateway Pod | 承载 yamux 多路复用会话、反向代理 HTTP、上下线事件 | 不写业务 DB、不解析业务报文 |
| **⑤ 设备** | cs-bridge tunnel agent | 注册设备、申请 Gateway、维护 WebSocket 隧道、转发 stream | 不实现业务 API |
| | cs serve | 本地 conversations/events/workspace/permissions API + AI Agent runtime | 不感知云端 |
| **⑥ 支撑** | cs-user | 验 JWT、UserInfo 4 层契约、企业身份字段映射 | 不写业务数据 |
| | Casdoor | OAuth/OIDC、旧 RBAC（灰度退出中） | — |
| | Gitea fork | team-level + user-level Git 仓库托管 | 不参与隧道 |
| | PostgreSQL | 业务 + 认证数据 | — |
| | Redis | GatewayStore（device→Gateway Pod 映射）、session 缓存 | 不做主存储 |

---

## 5. 三条核心数据流

### 5.1 业务请求（同步）

```
Client → APISIX → server (/api/*) → PostgreSQL / Gitea / cs-user
```

适用于 Portal / csc-cli 的所有 CRUD：组织管理、capability 浏览、marketplace、登录态校验等。

### 5.2 设备代理（透明隧道）

```
Browser / csc-cli
   └─► APISIX
        └─► server (/cloud/device/:id/proxy/*)
             └─► 查 Redis GatewayStore 找到 deviceID 所在 Gateway Pod
                  └─► Gateway 开 yamux stream
                       └─► cs-bridge tunnel agent Accept stream
                            └─► 本地转发到 cs serve
                                 └─► 响应沿原路返回
```

Portal / csc-cli 像访问本地 cs serve 一样访问远端设备，链路完全透明。

### 5.3 设备接入（建立隧道）

```
cs-bridge 启动
   ├─► POST /api/devices/register        （向 server 注册设备）
   ├─► POST /cloud/device/gateway-assign  （申请 Gateway 地址）
   └─► wss://api.example.com/device/{deviceID}/tunnel
          └─► APISIX (TLS 终止 + WS 透传)
               └─► nginx-router (按 deviceID chash 选 Pod)
                    └─► Gateway Pod
                         └─► Accept WebSocket → 建立 yamux Session
                              └─► 上报 Redis GatewayStore
```

设备与 Gateway 之间只维护**一条** WebSocket 连接，承载所有 yamux stream。

---

## 6. 跨层契约与约束

| 契约 | 提供方 | 消费方 | 备注 |
|---|---|---|---|
| UserInfo 4 层 schema | cs-user | server / cs-bridge / csc-cli / app-ai-native | 详见 `identity-tenant/CS_USER_SERVICE_DESIGN.md` |
| GatewayStore device→Pod 映射 | Gateway | server / nginx-router | Redis 共享 |
| HTTP over yamux stream | cs serve | server TunnelProxy / Gateway | `/cloud/device/:id/proxy/*` |
| capability 内容 schema | server Catalog | csc-cli / Portal / app-ai-native | 5 类 `item_type` |
| `/api/internal/*` | server | cs-user / cs-bridge / app-ai-native | 仅集群内可达 |
| JWT (Casdoor → cs-user 灰度) | cs-user / Casdoor | 所有下游服务 | 30 天 dual-trust 窗口 |

---

## 7. 部署形态速查

| 部署模式 | 文档 | 适用场景 |
|---|---|---|
| StatefulSet + 静态 FQDN | `deployment/gateway-statefulset-static.md` | 容器云 DNS 不一致 |
| DaemonSet + APISIX chash | `deployment/gateway-daemonset-apisix-chash.md` | 标准 K8s + headless Service |
| docker-compose | `deployment/gateway-docker-compose.md` | 开发/测试 |
