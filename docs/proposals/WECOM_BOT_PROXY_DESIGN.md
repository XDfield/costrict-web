> **实现状态：待开发**
>
> - 状态：🟡 待开发
> - 目标：新建独立仓库 `wecom-bot-proxy`，costrict-web 侧新增 `wecom-bot` channel adapter
> - 依赖：企微智能机器人长连接 API（WebSocket 模式）
> - 参考：https://developer.work.weixin.qq.com/document/path/101463

---

# 企微智能机器人长连接代理服务技术提案

## 目录

- [概述](#概述)
- [术语与概念](#术语与概念)
- [背景与动机](#背景与动机)
- [与现有架构对比](#与现有架构对比)
- [架构设计](#架构设计)
- [模块设计](#模块设计)
- [协议层设计](#协议层设计)
- [路由引擎设计](#路由引擎设计)
- [API 设计](#api-design)
- [配置设计](#配置设计)
- [高可用设计](#高可用设计)
- [costrict-web 侧适配器](#costrict-web-侧适配器)
- [消息流程](#消息流程)
- [交互卡片流程映射](#交互卡片流程映射)
- [降级策略](#降级策略)
- [错误处理](#错误处理)
- [安全考量](#安全考量)
- [实施计划](#实施计划)

---

## 概述

本文提案设计一个独立的代理服务 `wecom-bot-proxy`，通过企微智能机器人的 **WebSocket 长连接** 模式对接一个企微机器人，并将消息分发到不同的 costrict-web 后端实例。路由策略为：交互卡片回调通过 `task_id → backend` 映射精确路由到创建方后端，自由消息统一路由到默认后端。

### 核心价值

1. **无需公网 IP**：长连接模式不需要暴露公网回调 URL，服务部署在内网即可
2. **无需加解密**：与 Webhook 模式不同，长连接模式不涉及消息加解密，降低复杂度
3. **多实例路由**：一个企微机器人按任务维度（task_id）将卡片回调精确路由到创建该卡片的后端
4. **协议解耦**：costrict-web 通过业务导向的 HTTP 接口与 proxy 交互，不感知企微 WS 协议细节；proxy 负责 HTTP ↔ WS 协议翻译

---

## 术语与概念

| 术语 | 含义 |
|------|------|
| **wecom-bot-proxy** | 本提案设计的独立代理服务，维护企微 WS 长连接并提供路由 |
| **智能机器人** | 企微平台提供的 AI 机器人能力，区别于自建应用（Agent） |
| **长连接模式** | 通过 WebSocket 与企微服务器保持持久连接，无需公网回调 |
| **Backend** | 被代理的后端服务（costrict-web 实例），通过 HTTP 接收转发消息 |
| **自建应用** | 现有 costrict-web 已对接的模式（CorpID + AgentID + CGI API） |

---

## 背景与动机

### 当前状况

costrict-web 已有企微对接能力（`channel/adapters/wecom/`），基于**自建应用 Webhook 模式**：

- 需要公网可达的回调 URL（`/api/webhooks/channels/wecom`）
- 需要处理 AES-CBC 加解密和 SHA1 签名验证
- 消息发送走 CGI API（`qyapi.weixin.qq.com/cgi-bin/message/send`）
- 交互卡片通过独立的 CGI API 更新（`update_template_card`）

### 新需求

1. **内网部署**：costrict-web 实例部署在无公网 IP 的环境时，无法配置 Webhook 回调
2. **多实例路由**：多个 costrict-web 实例共享一个企微机器人，按任务维度（task_id）将交互卡片回调精确路由到创建卡片的后端，自由消息走默认后端
3. **更低延迟**：长连接模式消息延迟更低，适合实时交互场景
4. **流式消息**：长连接模式支持流式消息回复，未来可用于 AI 对话的逐字输出

---

## 与现有架构对比

| 维度 | 现有模式（自建应用 Webhook） | 长连接模式（智能机器人） |
|------|------|------|
| 协议 | HTTP 回调 + XML + AES 加解密 | WebSocket 长连接，明文 JSON |
| 公网要求 | 需要公网可达 URL | 无需公网 IP |
| 认证方式 | CorpID + AgentID + Secret + Token | BotID + Secret |
| 消息发送 | CGI API（`qyapi.weixin.qq.com`） | 通过 WS 连接直接发送 |
| 交互卡片 | `template_card` via CGI `message/send` | `aibot_send_msg` via WS |
| 卡片更新 | `update_template_card` via CGI（异步） | `aibot_respond_update_msg` via WS（5s 内同步回复） |
| 流式消息 | 不支持 | 支持 stream 机制 |
| 心跳 | 无 | 30s ping |
| 连接限制 | 无（无状态） | 每个机器人仅一个活跃连接 |
| 适用场景 | 有公网 IP 的正式部署 | 无公网 IP / 多实例共享 |

**两者是独立的企微 API，互不冲突**。现有自建应用模式继续用于需要 CGI API 的场景；长连接模式用于无公网 IP 和多实例共享的场景。

---

## 架构设计

### 整体架构图

```
┌──────────────────────────────────────────────────────────────┐
│                                                              │
│   ┌─────────────────────┐    wss://openws.work.weixin.qq.com │
│   │   wecom-bot-proxy   │◄══════════════════════════════     │
│   │                     │      (WebSocket 长连接)            │
│   │  ┌───────────────┐  │                                   │
│   │  │ WS Conn Mgr   │  │  subscribe / heartbeat / recv    │
│   │  └───────┬───────┘  │                                   │
│   │          │          │                                   │
│   │  ┌───────▼───────┐  │                                   │
│   │  │  Route Table  │  │  task_id → backend 关联路由       │
│   │  │ (task_route + │  │  + default backend 兜底          │
│   │  │  default)     │  │                                   │
│   │  └──┬─────┬──┬───┘  │                                   │
│   │     │     │  │      │                                   │
│   │  ┌──▼──┐┌─▼┐┌▼───┐  │                                   │
│   │  │ B.A ││B.B││B.C │  │  Backend Clients (平等实例)      │
│   │  └──┬──┘└┬─┘└┬───┘  │                                   │
│   └─────┼─────┼────┼────┘                                   │
│         │     │    │                                        │
│    ┌────▼──┐┌──▼──┐┌▼─────┐                                 │
│    │costrict││costr││costr │                                 │
│    │-web A  ││ict B││ict C │                                 │
│    └────────┘└─────┘└──────┘                                 │
│                                                              │
│   HTTP POST /api/bot/inbound     (proxy → backend)          │
│   HTTP POST /api/bot/send        (backend → proxy, 注册路由) │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

### 进程架构

```
wecom-bot-proxy (单进程)
├── WebSocket Connection Manager (goroutine)
│   ├── read loop: 接收企微推送
│   ├── write loop: 发送命令到企微
│   └── heartbeat ticker: 30s ping
├── Route Table
│   ├── task_routes: task_id → backend (TTL-based, 从 outbound 注册)
│   └── default_backend: 非卡片回调消息的默认路由目标
├── HTTP API Server (gin)
│   ├── POST /api/bot/send         (costrict-web → proxy → WS)
│   ├── POST /api/bot/reply        (costrict-web → proxy → WS)
│   ├── POST /api/bot/card/update  (costrict-web → proxy → WS)
│   ├── POST /api/bot/welcome      (costrict-web → proxy → WS)
│   └── GET  /api/bot/health       (连接状态检查)
└── Message Dedup (msgid → struct{} LRU cache)
```

---

## 模块设计

### 项目结构

```
wecom-bot-proxy/
├── cmd/proxy/main.go              # 入口
├── internal/
│   ├── ws/
│   │   ├── conn.go                # WS 连接生命周期管理
│   │   ├── protocol.go            # 企微长连接协议类型定义
│   │   └── heartbeat.go           # 心跳管理器
│   ├── router/
│   │   ├── table.go               # 任务关联路由表
│   │   └── config.go              # 路由配置加载
│   ├── api/
│   │   ├── server.go              # HTTP API server
│   │   ├── handlers.go            # REST handlers
│   │   └── translator.go          # 业务格式 → 企微 WS 协议翻译
│   ├── backend/
│   │   └── client.go              # 向后端转发 HTTP 请求
│   └── config/
│       └── config.go              # 配置加载与校验
├── config.yaml                    # 配置文件模板
├── Dockerfile
├── docker-compose.yaml
└── go.mod
```

### 模块职责

#### ws/ — WebSocket 连接管理

**conn.go**：连接生命周期状态机

```
[Disconnected] ──connect()──► [Connecting]
      ▲                            │
      │                     subscribe 成功
      │                            │
      │                            ▼
      │                       [Connected] ◄─── heartbeat
      │                            │
   disconnected_event         连接异常 / 超时
   或 write error                   │
      └────────────────────────────┘
               (exponential backoff reconnect)
```

核心逻辑：

- 连接 `wss://openws.work.weixin.qq.com` 后立即发送 `aibot_subscribe`
- 订阅成功后进入 `Connected` 状态
- 监听 `disconnected_event` 触发重连
- 连接异常时指数退避重连（5s → 10s → 20s → ... → 60s cap）
- 维护 `sendCh` channel，所有 WS 写操作统一通过 write loop goroutine 执行（避免并发写）

**protocol.go**：企微长连接协议类型

定义所有 cmd 相关的请求/响应结构体：

| cmd | 方向 | 说明 |
|-----|------|------|
| `aibot_subscribe` | C→S | 订阅/身份校验 |
| `ping` | C→S | 心跳 |
| `aibot_msg_callback` | S→C | 消息回调 |
| `aibot_event_callback` | S→C | 事件回调（进入会话/卡片点击/连接断开） |
| `aibot_respond_msg` | C→S | 回复消息（含流式） |
| `aibot_respond_welcome_msg` | C→S | 回复欢迎语 |
| `aibot_respond_update_msg` | C→S | 更新模板卡片 |
| `aibot_send_msg` | C→S | 主动推送消息 |
| `aibot_upload_media_init` | C→S | 上传初始化 |
| `aibot_upload_media_chunk` | C→S | 上传分片 |
| `aibot_upload_media_finish` | C→S | 上传完成 |

**heartbeat.go**：心跳管理

- 30 秒间隔发送 `ping`
- 连续 3 次未收到 `pong` 响应则判定连接失效，触发重连
- 心跳与连接状态绑定，非 `Connected` 状态不发送心跳

#### router/ — 路由引擎

维护任务关联路由表（详见 [路由引擎设计](#路由引擎设计)）：

- **Task Route**：追踪 outbound 请求中显式声明的 `task_id`，卡片回调时精确路由到创建方
- **Default Backend**：所有非卡片回调消息的兜底路由目标

#### api/ — HTTP API Server

对 costrict-web 暴露 REST API。**translator.go** 负责将业务导向的 HTTP 请求翻译为企微 WS 协议命令。costrict-web 侧不需要了解企微 WS 协议的任何细节。

#### backend/ — 后端客户端

负责向后端 costrict-web 实例发送 HTTP 请求（inbound 转发）。

---

## 协议层设计

### 设计原则

**costrict-web 与 proxy 之间的协议面向业务设计，与企微 WS 协议细节完全解耦。**

- proxy 的 inbound 格式（proxy → costrict-web）对齐 costrict-web 现有的 `channel.InboundMessage` 结构
- proxy 的 outbound API（costrict-web → proxy）使用业务语义（发送消息、回复消息、更新卡片），由 proxy 内部的 `translator.go` 负责翻译为企微 WS 命令
- `task_id` 由调用方在请求顶层显式声明，proxy 直接用于路由注册，不解析消息体内部结构

### Inbound 消息格式（proxy → costrict-web）

Inbound 格式对齐 costrict-web 现有 `channel.InboundMessage` 结构，使 adapter 的 `ParseInbound` 实现简洁：

```json
{
  "externalChatId": "CHATID",
  "externalChatType": "single",
  "externalUserId": "USERID",
  "externalMessageId": "MSGID",
  "contentType": "text",
  "content": "hello",
  "metadata": {
    "reqId": "REQUEST_ID",
    "botId": "BOTID",
    "chatId": "CHATID",
    "chatType": "single",
    "msgType": "text",
    "timestamp": 1700000000
  }
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `externalChatId` | string | 会话 ID（单聊为 userid，群聊为 chatid） |
| `externalChatType` | string | `"single"` 单聊 / `"group"` 群聊 |
| `externalUserId` | string | 消息发送者 userid |
| `externalMessageId` | string | 消息唯一 ID（用于幂等去重） |
| `contentType` | string | `"text"` / `"markdown"` / `"action_callback"` / `"image"` / `"file"` / `"voice"` / `"video"` / `"mixed"` |
| `content` | string | 消息文本内容（文本消息为原文，卡片回调为 action key） |
| `metadata` | object | 扩展字段，包含企微回调的原始信息 |

**文本消息示例**：

```json
{
  "externalChatId": "zhangsan",
  "externalChatType": "single",
  "externalUserId": "zhangsan",
  "externalMessageId": "MSGID",
  "contentType": "text",
  "content": "帮我看看会话状态",
  "metadata": {
    "reqId": "REQUEST_ID",
    "botId": "BOTID",
    "chatType": "single",
    "msgType": "text",
    "originalContent": { "text": { "content": "帮我看看会话状态" } },
    "timestamp": 1700000000
  }
}
```

**交互卡片回调示例**（用户点击模板卡片）：

```json
{
  "externalChatId": "zhangsan",
  "externalChatType": "single",
  "externalUserId": "zhangsan",
  "externalMessageId": "MSGID",
  "contentType": "action_callback",
  "content": "approve",
  "metadata": {
    "reqId": "REQUEST_ID",
    "botId": "BOTID",
    "eventType": "template_card_event",
    "taskId": "perm_abc123",
    "responseCode": "RESPONSE_CODE",
    "timestamp": 1700000000
  }
}
```

**进入会话事件示例**：

```json
{
  "externalChatId": "zhangsan",
  "externalChatType": "single",
  "externalUserId": "zhangsan",
  "externalMessageId": "MSGID",
  "contentType": "event",
  "content": "enter_chat",
  "metadata": {
    "reqId": "REQUEST_ID",
    "botId": "BOTID",
    "eventType": "enter_chat",
    "timestamp": 1700000000
  }
}
```

### Outbound 请求格式（costrict-web → proxy）

详见 [API 设计](#api-design) 章节。所有 outbound API 使用业务语义的请求体，`task_id` 作为顶层显式字段，由 proxy 翻译为企微 WS 协议。

---

## 路由引擎设计

### 设计原则

路由的核心挑战：**一个企微机器人服务多个 costrict-web 后端，卡片回调消息必须准确路由到创建该卡片的后端**。

关键约束：用户是**全局共享**的，同一用户在多个 costrict-web 实例上都有任务。因此不能按用户/会话维度路由，只能按**任务维度**路由。

路由策略：

1. **任务关联（task_id）**：costrict-web 在发送卡片时在请求顶层显式声明 `task_id`，proxy 直接注册路由，卡片点击回调时精确路由到创建方
2. **默认后端**：非卡片回调的消息（自由消息、进入会话等）统一路由到默认后端。这是设计意图：自由消息没有任务维度可路由，由 default backend 统一处理

### 路由表设计

```
┌─────────────────────────────────────────────────────────┐
│ Task Route: task_id → backend                            │
│                                                          │
│ 写入时机: backend 调用 /api/bot/send 时，从请求顶层     │
│           task_id 字段直接读取并注册（不解析 content）    │
│ 读取时机: 收到 template_card_event 回调时查找             │
│ 生命周期: 注册时写入，TTL 过期自动清理（默认 24h）        │
├─────────────────────────────────────────────────────────┤
│ Default Backend                                          │
│ 所有非卡片回调消息的兜底路由目标                          │
└─────────────────────────────────────────────────────────┘
```

### 路由查找算法

```
收到 inbound 消息 M:
  │
  ├─ M 是 template_card_event 且包含 task_id?
  │    ├─ 查找 task_routes[task_id] → 命中? → 路由到该 backend
  │    └─ 未命中 → 路由到 default backend（并记录警告日志）
  │
  └─ 其他消息（自由消息 / 进入会话 / 用户反馈 等）
       └─ 路由到 default backend
```

### Outbound 注册（路由表写入）

costrict-web 调用 proxy 的 outbound API 时，在请求顶层显式声明 `task_id`。proxy 直接读取并注册路由，无需解析消息体内部结构：

```go
func (p *Proxy) handleSend(req SendRequest, callerBackend string) {
    // 从请求顶层直接读取 task_id，不解析 content
    if req.TaskID != "" {
        p.routeTable.Register(req.TaskID, callerBackend)
    }
    // translator 将业务格式翻译为企微 WS 命令并发送
    p.wsConn.Send(translator.TranslateSend(req))
}
```

### 路由表实现

```go
type RouteTable struct {
    mu sync.RWMutex

    // task_id → backend (TTL-based LRU)
    taskRoutes *lru.Cache[string, routeEntry]

    // 所有非卡片回调消息的目标
    defaultBackend string
}

type routeEntry struct {
    Backend   string
    ExpiresAt time.Time
}

func (rt *RouteTable) Route(msg InboundMessage) string {
    rt.mu.RLock()
    defer rt.mu.RUnlock()

    if msg.EventType == "event" && msg.MsgType == "template_card_event" {
        if taskID := extractTaskIDFromEvent(msg.Content); taskID != "" {
            if entry, ok := rt.taskRoutes.Get(taskID); ok && time.Now().Before(entry.ExpiresAt) {
                return entry.Backend
            }
        }
    }

    return rt.defaultBackend
}
```

### 配置示例

```yaml
backends:
  backend-a:
    url: "http://costrict-a:8080/api/bot/inbound"
    auth_token: "${BACKEND_A_TOKEN}"
    timeout: 10s
    retry: 3
  backend-b:
    url: "http://costrict-b:8080/api/bot/inbound"
    auth_token: "${BACKEND_B_TOKEN}"
    timeout: 10s
    retry: 3
  backend-c:
    url: "http://costrict-c:8080/api/bot/inbound"
    auth_token: "${BACKEND_C_TOKEN}"
    timeout: 10s
    retry: 3

routing:
  default_backend: backend-a
  task_route_ttl: 24h
```

| 配置项 | 说明 | 默认值 |
|--------|------|--------|
| `backends.<name>.url` | 后端 inbound endpoint 地址 | 必填 |
| `backends.<name>.auth_token` | 后端独立鉴权 token（同时用于识别调用方身份） | 必填 |
| `backends.<name>.timeout` | HTTP 请求超时 | `10s` |
| `backends.<name>.retry` | 失败重试次数 | `3` |
| `routing.default_backend` | 默认后端名称（所有非卡片回调消息的目标） | 必填 |
| `routing.task_route_ttl` | task_id 路由条目有效期 | `24h` |

### 路由场景示例

**场景 1：通知卡片回调（核心场景）**

```
1. backend-b 的 costrict-web 产生权限请求
   → 调用 proxy POST /api/bot/send (task_id="perm_abc123", msg_type="card")
   → proxy 注册: task_routes["perm_abc123"] = "backend-b"
   → proxy 翻译为 aibot_send_msg → 通过 WS 发送 → 企微展示审批卡片

2. 用户点击"批准"
   → 企微推送 template_card_event (task_id="perm_abc123")
   → proxy 查找: task_routes["perm_abc123"] → "backend-b" ✓
   → 路由到 backend-b 的 costrict-web
   → backend-b 处理审批，同步返回卡片更新
   → proxy 翻译为 aibot_respond_update_msg → 通过 WS 发送 → 企微更新为"已批准"
```

**场景 2：同一用户，多后端卡片并行**

```
同一用户在多个 costrict-web 实例上各有任务：

1. backend-a 发送审批卡片 (task_id="perm_a_001")
   → task_routes["perm_a_001"] = "backend-a"

2. backend-b 发送通知卡片 (task_id="notice_b_001")
   → task_routes["notice_b_001"] = "backend-b"

3. 用户点击 backend-a 的审批卡片
   → task_id="perm_a_001" → "backend-a" ✓

4. 用户点击 backend-b 的通知卡片
   → task_id="notice_b_001" → "backend-b" ✓

→ 同一用户的卡片互不干扰，各自精确路由
```

**场景 3：自由消息（走默认后端）**

```
用户发送 "帮我看看会话状态"
   → 企微推送 aibot_msg_callback (from=zhangsan)
   → 非卡片事件 → 路由到 default_backend (backend-a)
   → backend-a 的 costrict-web 处理
```

---

## API 设计

### 鉴权

所有 Outbound API 请求需在 Header 中携带后端专属 token：

| Header | 说明 |
|--------|------|
| `Authorization` | `Bearer {backends.<name>.auth_token}` |

proxy 通过 token 匹配 `backends` 配置项来**同时完成鉴权和调用方身份识别**。每个 backend 配置独立的 `auth_token`，proxy 收到请求后遍历 backends 配置匹配 token，找到匹配项即为该 backend 的身份。

### Inbound Endpoint（proxy → costrict-web）

`POST {backend.url}` — proxy 向匹配的后端转发消息

- 请求体：对齐 `channel.InboundMessage` 的标准化格式（见 [协议层设计](#协议层设计)）
- 请求头：

| Header | 说明 |
|--------|------|
| `X-Bot-Proxy-Signature` | HMAC-SHA256 签名（使用 `backends.<name>.hmac_secret`） |
| `X-Bot-Proxy-Timestamp` | 请求时间戳 |
| `X-Bot-Proxy-Msg-ID` | 消息 ID（用于幂等） |

costrict-web 的 inbound handler 处理完成后，可同步返回即时回复：

```json
{
  "reply": {
    "msg_type": "text",
    "content": "收到，处理中..."
  }
}
```

如果后端返回了 `reply` 字段，proxy 会立即翻译为 WS 回复命令发送。

### Outbound API（costrict-web → proxy）

Outbound API 使用**业务导向**的请求格式。costrict-web 不需要了解企微 WS 协议细节（如 `aibot_send_msg`、`chat_type` 数值编码等），由 proxy 内部的 `translator.go` 负责翻译。

#### 发送消息

```
POST /api/bot/send
```

```json
{
  "user_id": "zhangsan",
  "chat_type": "single",
  "msg_type": "markdown",
  "content": "**告警通知**\nCPU > 90%",
  "task_id": "perm_abc123"
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `user_id` | string | 是 | 接收者 userid（单聊）或 chatid（群聊） |
| `chat_type` | string | 是 | `"single"` 单聊 / `"group"` 群聊 |
| `msg_type` | string | 是 | `"text"` / `"markdown"` / `"card"` |
| `content` | string | 是 | 消息内容。`text` 为纯文本，`markdown` 为 Markdown 格式，`card` 为 JSON 序列化的卡片结构 |
| `task_id` | string | 否 | 任务 ID。填写时 proxy 自动注册 `task_id → caller backend` 路由。卡片回调时据此路由到本后端 |

proxy 内部翻译逻辑：
- `chat_type: "single"` → 企微 `chat_type: 1`，`group` → `chat_type: 2`
- `msg_type: "text"` → 企微 `aibot_send_msg` + `msgtype: "text"`
- `msg_type: "markdown"` → 企微 `aibot_send_msg` + `msgtype: "markdown"`
- `msg_type: "card"` → 企微 `aibot_send_msg` + `msgtype: "template_card"`，`content` 反序列化为卡片结构放入 `template_card` 字段

响应：

```json
{ "success": true }
```

#### 回复消息

```
POST /api/bot/reply
```

用于回复用户消息回调。`req_id` 来自 inbound 消息的 `metadata.reqId`。

```json
{
  "req_id": "ORIGINAL_REQ_ID",
  "msg_type": "text",
  "content": "回复内容"
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `req_id` | string | 是 | 原始回调的 req_id |
| `msg_type` | string | 是 | `"text"` / `"markdown"` |
| `content` | string | 是 | 回复内容 |

#### 回复流式消息

```
POST /api/bot/reply/stream
```

```json
{
  "req_id": "ORIGINAL_REQ_ID",
  "stream_id": "STREAM_ID",
  "finish": false,
  "content": "正在为您查询..."
}
```

| 字段 | 说明 |
|------|------|
| `stream_id` | 流式消息标识，同一 ID 的多次推送更新同一条消息 |
| `finish` | 设为 `true` 结束流式消息 |
| `content` | 本段流式内容 |

约束：从首次发送开始需在 **10 分钟内** 完成（设 `finish=true`）。

#### 回复欢迎语

```
POST /api/bot/welcome
```

```json
{
  "req_id": "ORIGINAL_REQ_ID",
  "msg_type": "text",
  "content": "您好！有什么可以帮您的？"
}
```

约束：需在收到 `enter_chat` 事件后 **5 秒内** 发送。

#### 更新模板卡片

```
POST /api/bot/card/update
```

```json
{
  "req_id": "ORIGINAL_REQ_ID",
  "card_type": "button_interaction",
  "content": "{\"main_title\":{\"title\":\"已批准\"},\"button_list\":[{\"text\":\"已处理\",\"style\":1,\"key\":\"done\"}]}",
  "task_id": "TASK_ID"
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `req_id` | string | 是 | 原始卡片事件回调的 req_id |
| `card_type` | string | 否 | 卡片类型，如 `"button_interaction"`、`"text_notice"` |
| `content` | string | 是 | JSON 序列化的卡片更新内容 |
| `task_id` | string | 否 | 任务 ID，用于卡片标识 |

约束：需在收到 `template_card_event` 后 **5 秒内** 发送。详见 [降级策略](#降级策略)。

#### 健康检查

```
GET /api/bot/health
```

响应：

```json
{
  "status": "connected",
  "bot_id": "BOTID",
  "connected_at": "2026-06-11T10:00:00Z",
  "last_heartbeat": "2026-06-11T10:05:30Z",
  "backends": {
    "backend-a": { "healthy": true, "last_success": "2026-06-11T10:04:00Z" },
    "backend-b": { "healthy": true, "last_success": "2026-06-11T10:03:00Z" }
  }
}
```

`status` 可选值：`connected` / `connecting` / `disconnected`

---

## 配置设计

### 完整配置示例

```yaml
bot:
  bot_id: "${WECOM_BOT_ID}"
  secret: "${WECOM_BOT_SECRET}"
  ws_url: "wss://openws.work.weixin.qq.com"
  heartbeat_interval: 30s
  reconnect_initial_backoff: 5s
  reconnect_max_backoff: 60s

server:
  listen: ":9090"

backends:
  backend-a:
    url: "http://costrict-a:8080/api/bot/inbound"
    auth_token: "${BACKEND_A_TOKEN}"
    hmac_secret: "${BACKEND_A_HMAC_SECRET}"
    timeout: 10s
    retry: 3
  backend-b:
    url: "http://costrict-b:8080/api/bot/inbound"
    auth_token: "${BACKEND_B_TOKEN}"
    hmac_secret: "${BACKEND_B_HMAC_SECRET}"
    timeout: 10s
    retry: 3

routing:
  default_backend: backend-a
  task_route_ttl: 24h

dedup:
  enabled: true
  max_entries: 10000
  ttl: 5m

logging:
  level: info
  format: json
```

所有敏感字段支持环境变量插值（`${VAR_NAME}` 语法）。

---

## 高可用设计

### 问题

企微限制每个智能机器人**同一时间只能有一个活跃连接**。新连接会踢掉旧连接。因此 WS 连接无法水平扩展。

### 方案：Active-Passive 故障转移

```
┌─────────────────────────────────┐
│         Redis / etcd            │
│    (分布式锁 + leader 选举)      │
└──────────┬──────────────────────┘
           │ acquire lock
    ┌──────▼──────┐     ┌──────────────┐
    │  Active     │     │  Standby     │
    │  Proxy #1   │     │  Proxy #2    │
    │  (持锁+WS)  │     │  (等锁+探活) │
    └──────┬──────┘     └──────┬───────┘
           │                   │
    ┌──────▼──────┐     ┌──────▼───────┐
    │ LB (VIP)    │◄────┘              │
    │ :9090       │                    │
    └─────────────┘                    │
         │                             │
    costrict-web 调用 outbound API     │
```

**故障切换流程**：

1. Active 实例持有 Redis 分布式锁（TTL 30s，每 10s 续期）
2. Active 维护 WS 连接，处理所有消息路由
3. Standby 定期尝试获取锁（每 15s）
4. Active 宕机 → 锁超时释放 → Standby 获取锁 → 建立 WS 连接 → 接管流量
5. HTTP API 通过 LB（VIP 或 DNS）暴露，两个实例均可接受 outbound 请求
6. 非 Active 实例收到 outbound 请求时，通过内部通道转发给 Active 实例

### 简化方案（一期）

一期可不引入 Redis，采用简单的单实例部署 + systemd 自动重启。通过 LB 健康检查实现快速恢复：

- proxy 暴露 `/api/bot/health` endpoint
- LB 对 `/api/bot/health` 做健康检查
- 进程崩溃时 systemd 自动重启
- 重启后自动重连 WS 并恢复订阅

---

## costrict-web 侧适配器

### 新增 adapter：`wecom-bot`

在 `server/internal/channel/adapters/wecom-bot/` 下新增适配器，通过 HTTP 与 proxy 交互：

```
server/internal/channel/adapters/wecom-bot/
├── adapter.go        # WeComBotAdapter 实现 ChannelAdapter 接口
├── types.go          # 类型定义
└── client.go         # 调用 proxy HTTP API 的客户端
```

### 现有 ChannelAdapter 接口

适配器需实现 `channel.ChannelAdapter` 接口（定义于 `server/internal/channel/adapter.go`）：

```go
type ChannelAdapter interface {
    Type() string
    Capabilities() ChannelCapabilities
    ValidateConfig(config json.RawMessage) error
    ConfigSchema() []ConfigField
    ParseInbound(r *http.Request, config json.RawMessage) (*InboundMessage, error)
    HandleVerification(r *http.Request, config json.RawMessage) (body string, handled bool, err error)
    Reply(ctx context.Context, config json.RawMessage, target ReplyTarget, message OutboundMessage) error
}
```

### 适配器设计

```go
type WeComBotAdapter struct {
    client *BotProxyClient  // 调用 proxy HTTP API
}

func (a *WeComBotAdapter) Type() string { return "wecom-bot" }

func (a *WeComBotAdapter) Capabilities() channel.ChannelCapabilities {
    return channel.ChannelCapabilities{
        InboundMessages:  true,
        OutboundMessages: true,
        DirectChat:       true,
        GroupChat:        true,
        Markdown:         true,
        Media:            false,
        MentionRequired:  false,  // 智能机器人不需要 @mention
        ContentTypes:     []string{"text", "markdown", "card"},
    }
}

func (a *WeComBotAdapter) ParseInbound(r *http.Request, _ json.RawMessage) (*channel.InboundMessage, error) {
    // proxy 转发来的消息已对齐 InboundMessage 结构，直接反序列化
    var msg channel.InboundMessage
    if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
        return nil, err
    }
    return &msg, nil
}

func (a *WeComBotAdapter) HandleVerification(r *http.Request, _ json.RawMessage) (string, bool, error) {
    // 长连接模式不需要验证回调，直接跳过
    return "", false, nil
}

func (a *WeComBotAdapter) Reply(ctx context.Context, _ json.RawMessage, target channel.ReplyTarget, message channel.OutboundMessage) error {
    // 调用 proxy 的 POST /api/bot/send
    return a.client.Send(ctx, target.ExternalChatID, "single", message.ContentType, message.Content, "")
}
```

### 卡片发送方法

适配器额外提供卡片发送方法，复用现有的卡片结构类型（`InteractiveCard`、`VoteCard`、`TextNoticeCard`），通过 `POST /api/bot/send` + `msg_type: "card"` 发送：

```go
func (a *WeComBotAdapter) SendInteractiveCard(ctx context.Context, userID string, card InteractiveCard, taskID string) error {
    cardJSON, _ := json.Marshal(card)
    return a.client.Send(ctx, userID, "single", "card", string(cardJSON), taskID)
}
```

### 卡片更新实现

实现 `channel.CardStatusUpdater` 接口，用于交互卡片回调后的状态更新：

```go
func (a *WeComBotAdapter) UpdateCardStatus(responseCode, statusText, action string, cardData []byte, externalUserID string) error {
    // 从 session store 获取 req_id（通过 response_code 映射）
    reqID := a.getReqIDByResponseCode(responseCode)
    cardUpdate := buildCardUpdate(statusText, action, cardData)
    return a.client.UpdateCard(context.Background(), reqID, cardUpdate)
}
```

**关键差异**：与现有 wecom adapter 的异步 CGI 更新不同，wecom-bot adapter 的卡片更新需要通过 proxy 的 `/api/bot/card/update` 在 5s 内完成。详见 [降级策略](#降级策略)。

### Webhook 路由注册

在 `server/internal/channel/channel.go` 中注册 proxy inbound 回调路由：

```go
// 现有路由（通过 :type 参数匹配 adapter）
publicGroup.POST("/webhooks/channels/:type", WebhookHandler(m.Service))

// Proxy inbound callback (called by wecom-bot-proxy)
// 走标准 WebhookHandler，:type = "wecom-bot"
```

`wecom-bot` 作为新的 channel type 注册到 adapter registry，通过标准 webhook 路径 `/api/webhooks/channels/wecom-bot` 接收 proxy 转发的消息。

### ChannelService 集成

`NewChannelService` 需要新增 `weComBotEnabled` 参数，用于控制 `wecom-bot` adapter 的注册：

```go
func NewChannelService(db *gorm.DB, handler MessageHandler, cloudBaseURL string,
    enabledTypes []string, weComEnabled, weComWebhookEnabled, weChatEnabled, weComBotEnabled bool) *Module
```

在 adapter 过滤逻辑中增加：

```go
case "wecom-bot":
    if !weComBotEnabled {
        continue
    }
```

### 配置

costrict-web 侧新增配置项：

```env
# wecom-bot-proxy 配置（长连接模式）
WECOM_BOT_PROXY_URL=http://wecom-bot-proxy:9090
WECOM_BOT_PROXY_AUTH_TOKEN=backend-a-secret-token
WECOM_BOT_PROXY_ENABLED=true
```

### 与 Dispatcher 的集成

`wecom-bot` adapter 的 `Reply` 方法接口与现有 `wecom.WeComAdapter` 一致（都是 `ChannelAdapter.Reply`），Dispatcher 可通过 adapter 接口统一调用，无需修改业务逻辑。

卡片发送方法（`SendInteractiveCard`、`SendVoteCard`、`SendTextNoticeCard`）签名与现有 wecom adapter 一致，Dispatcher 可通过同一接口调用。

关键差异点（均在 adapter 内部处理，对 Dispatcher 透明）：

| 操作 | 自建应用 adapter (`wecom`) | 长连接 adapter (`wecom-bot`) |
|------|--------------------------|----------------------------|
| 发送交互卡片 | CGI `message/send` + `template_card` | `POST /api/bot/send` + `msg_type: "card"` + `task_id` |
| 回复消息 | CGI `message/send` | `POST /api/bot/reply` |
| 更新卡片 | CGI `update_template_card`（异步，无时间限制） | `POST /api/bot/card/update`（5s 内，见降级策略） |
| 获取 userID | IDTrust 绑定中的 `ProviderUserID` | inbound 消息的 `externalUserId`（直接可用） |
| 发送目标 | `touser` + `agentid` | `user_id` + `chat_type` |

---

## 消息流程

### 流程 1：通知卡片回调（核心场景）

```
backend-b 的 costrict-web 产生权限请求
    │  WeComBotAdapter.SendInteractiveCard(userID, card, taskID="perm_abc123")
    │  → POST /api/bot/send (user_id="zhangsan", msg_type="card", task_id="perm_abc123")
    │
    ▼
wecom-bot-proxy 收到 outbound 请求
    │  token 匹配 → 识别调用方为 "backend-b"
    │  注册路由: task_routes["perm_abc123"] = "backend-b" (TTL 24h)
    │  translator 翻译为 aibot_send_msg → 通过 WS 发送
    │
    ▼
企微展示审批卡片
    │
    ▼
用户点击"批准"
    │
    ▼
企微推送 aibot_event_callback (template_card_event)
    │  body.event.task_id = "perm_abc123"
    │
    ▼
proxy 路由查找
    │  task_routes["perm_abc123"] → "backend-b" ✓
    │  翻译为 InboundMessage 格式 (contentType="action_callback", content="approve")
    │
    ▼
BackendClient.Forward("backend-b", inboundMsg)
    │  POST http://costrict-b:8080/api/webhooks/channels/wecom-bot
    │
    ▼
backend-b 的 ChannelService.HandleWebhook
    │  WeComBotAdapter.ParseInbound → InboundMessage
    │  actionHandler 处理审批逻辑
    │  WeComBotAdapter.UpdateCardStatus → POST /api/bot/card/update
    │
    ▼
proxy 翻译为 aibot_respond_update_msg → 通过 WS 发送 → 企微更新为"已批准"
```

### 流程 2：自由消息（走默认后端）

```
用户发送 "帮我看看会话状态"
    │
    ▼
企微推送 aibot_msg_callback (from=zhangsan)
    │
    ▼
proxy 路由查找
    │  非卡片事件 → default_backend = "backend-a"
    │  翻译为 InboundMessage 格式 (contentType="text", content="帮我看看会话状态")
    │
    ▼
路由到 backend-a 的 costrict-web
    │  POST http://costrict-a:8080/api/webhooks/channels/wecom-bot
    │
    ▼
backend-a 处理 → 可选同步返回即时回复
    │  { "reply": { "msg_type": "text", "content": "会话运行中" } }
    │
    ▼
proxy 翻译为 aibot_respond_msg → 通过 WS 发送 → 企微展示给用户
```

### 流程 3：后端主动推送消息

```
costrict-web Dispatcher 触发通知
    │  WeComBotAdapter.Reply() 或 SendInteractiveCard()
    │
    ▼
POST http://wecom-bot-proxy:9090/api/bot/send
    │  Authorization: Bearer {backend-a专属token}
    │  Body: { user_id, chat_type, msg_type, content, task_id? }
    │
    ▼
wecom-bot-proxy token 匹配 → 识别调用方为 "backend-a"
    │  （如果 task_id 非空，自动注册路由）
    │  translator 翻译为 aibot_send_msg → 通过 WS 发送
    │
    ▼
企微展示给用户
```

### 流程 4：同一用户多后端卡片并行

```
同一用户在多个 costrict-web 实例上各有进行中的任务：

1. backend-a 发送审批卡片 (task_id="perm_a_001")
   → task_routes["perm_a_001"] = "backend-a"

2. backend-b 发送通知卡片 (task_id="notice_b_001")
   → task_routes["notice_b_001"] = "backend-b"

3. 用户点击 backend-a 的审批卡片 → task_id="perm_a_001" → "backend-a" ✓
4. 用户点击 backend-b 的通知卡片 → task_id="notice_b_001" → "backend-b" ✓

→ 同一用户的卡片互不干扰，各自精确路由到创建方
```

---

## 交互卡片流程映射

### 现有 Dispatcher 卡片类型 → 长连接模式

| 现有操作 | 现有实现 (wecom adapter) | 长连接实现 (wecom-bot adapter) |
|---------|---------|-------------|
| `sendApprovalCard` | `WeComAdapter.SendInteractiveCard()` → CGI `message/send` | `WeComBotAdapter.SendInteractiveCard()` → `POST /api/bot/send` |
| `sendVoteCards` / `sendSingleVoteCard` | `WeComAdapter.SendVoteCard()` → CGI `message/send` | `WeComBotAdapter.SendVoteCard()` → `POST /api/bot/send` |
| `sendSessionNoticeCard` | `WeComAdapter.SendTextNoticeCard()` → CGI `message/send` | `WeComBotAdapter.SendTextNoticeCard()` → `POST /api/bot/send` |
| `sendGuidanceCard` | `WeComAdapter.SendInteractiveCard()` → CGI `message/send` | `WeComBotAdapter.SendInteractiveCard()` → `POST /api/bot/send` |
| `UpdateCardStatus` | `WeComAdapter.UpdateCardStatus()` → CGI `update_template_card`（异步） | `WeComBotAdapter.UpdateCardStatus()` → `POST /api/bot/card/update`（5s 内同步） |

### 卡片更新的时序差异

**现有模式（异步）**：
```
用户点击 → 企微回调 XML → costrict-web 处理 → 异步调用 CGI update_template_card（无时间限制）
```

**长连接模式（同步）**：
```
用户点击 → 企微 WS 推送 → proxy 转发 → costrict-web 处理 → 同步返回卡片更新 → proxy 通过 WS 回复（5s 内）
```

---

## 降级策略

### 问题

长连接模式下，卡片更新必须在收到 `template_card_event` 后 **5 秒内** 通过 WS 回复。如果 costrict-web 的业务逻辑（权限审批、投票统计等）涉及外部 API 调用或耗时操作，可能无法在 5s 内完成。

### 降级方案：先应答后异步更新

```
用户点击卡片按钮
    │
    ▼
proxy 转发 → costrict-web actionHandler
    │
    ├─ 业务逻辑可在 5s 内完成?
    │    └─ 是 → UpdateCardStatus → proxy 同步回复卡片更新 → 完成
    │
    └─ 业务逻辑可能超时?
         └─ 立即返回 "处理中" 卡片状态（UpdateCardStatus + "处理中"）
            → 后续业务完成后，通过 SendInteractiveCard 主动推送最终结果
```

具体策略：

1. **快速应答**：actionHandler 收到卡片回调后，如果判断处理时间可能超过 3s，立即调用 `UpdateCardStatus` 返回 "处理中" 状态（将按钮替换为 "处理中..." 文案）
2. **异步通知**：业务完成后，通过 `POST /api/bot/send` 主动推送 markdown 消息或新卡片通知用户最终结果
3. **超时兜底**：如果 actionHandler 在 5s 内既没有完成也没有返回 "处理中"，proxy 发送默认的 "已收到" 回复（避免企微侧超时无响应）

### adapter 层实现建议

在 `WeComBotAdapter` 中封装降级逻辑：

```go
func (a *WeComBotAdapter) HandleActionCallback(ctx context.Context, action, responseCode, reqID string, handler ActionFunc) {
    done := make(chan struct{})
    go func() {
        handler(ctx, action)
        close(done)
    }()

    select {
    case <-done:
        // handler 在合理时间内完成，卡片更新已在 handler 内调用
    case <-time.After(3 * time.Second):
        // 超时降级：立即返回 "处理中" 状态
        a.UpdateCardStatus(responseCode, "处理中...", action, nil, "")
    }
}
```

### 对 retry 的影响

卡片更新请求因有 5s 时效限制，proxy 对 `/api/bot/card/update` 的请求不应做重试（重试会加剧超时）。其他 outbound API（`/api/bot/send`、`/api/bot/reply`）可正常重试。

---

## 错误处理

### WS 连接层面

| 场景 | 处理策略 |
|------|---------|
| 连接断开 | 指数退避重连（5s → 10s → 20s → 40s → 60s cap） |
| 订阅失败 | 等待 backoff 后重试，记录错误日志 |
| 心跳超时（3 次未收到 pong） | 主动断开连接并重连 |
| 收到 `disconnected_event` | 立即重连 |

### 后端转发层面

| 场景 | 处理策略 |
|------|---------|
| 后端不可达 | 按 `retry` 配置重试（间隔 1s），全部失败后记录 dead letter 日志 |
| 后端超时 | 记录警告日志，不重试（避免企微侧超时） |
| 后端返回非 2xx | 记录错误日志，不重试 |
| 所有后端不可用 | 返回 503，proxy 本身不缓存消息（未来可加 Redis 队列） |

### 消息去重

- 基于 `externalMessageId` 的内存 LRU 缓存（可配置大小，默认 10000 条，TTL 5 分钟）
- 重复消息直接丢弃，避免后端重复处理

### 速率限制

- 企微限制：每个会话 30 条/分钟，1000 条/小时
- proxy 侧实现令牌桶限流，超出限制时排队或丢弃
- 对后端的 HTTP 调用也做并发限制（默认最大 100 并发）

---

## 安全考量

1. **Per-backend Token 鉴权**：每个 backend 配置独立的 `auth_token`，proxy 通过 token 同时完成鉴权和调用方身份识别
2. **HMAC 签名**：proxy → 后端的 inbound 请求使用 HMAC-SHA256 签名（每个 backend 独立 `hmac_secret`），后端可验证请求来源
3. **Bot Secret 保护**：BotID 和 Secret 通过环境变量注入，不写入配置文件
4. **HTTPS**：生产环境 proxy 的 HTTP API 应通过 TLS 暴露（或前置 Nginx/Caddy）
5. **网络隔离**：proxy 部署在可同时访问企微 WS 和后端服务的网络位置

---

## 实施计划

### 一期（MVP）

- [ ] **P0：WS 连接管理** — 连接/订阅/心跳/断线重连
- [ ] **P0：路由表** — task_id → backend 关联路由 + default backend 兜底
- [ ] **P0：Inbound 路由** — 卡片回调走 task_id 查找，其他走 default
- [ ] **P0：Outbound API** — `/api/bot/send` 基本发送能力（text + markdown），显式 `task_id` 注册路由
- [ ] **P0：消息去重** — msgid LRU 缓存
- [ ] **P0：配置管理** — YAML 配置 + 环境变量插值
- [ ] **P1：costrict-web adapter** — `wecom-bot` channel adapter + `ParseInbound` + `Reply`

### 二期

- [ ] **模板卡片发送** — `/api/bot/send` 支持 `msg_type: "card"`，适配器实现 `SendInteractiveCard` / `SendVoteCard` / `SendTextNoticeCard`
- [ ] **卡片交互回调** — `/api/bot/card/update` 端点，处理 `template_card_event`（5s 同步回复 + 降级策略）
- [ ] **欢迎语** — `/api/bot/welcome` 端点，处理 `enter_chat` 事件
- [ ] **流式消息** — `/api/bot/reply/stream` 端点
- [ ] **健康检查** — `/api/bot/health` 端点
- [ ] **路由状态 API** — 查看 task_routes 表、路由命中/未命中统计

### 三期

- [ ] **高可用** — Redis 分布式锁 + Active-Passive 故障转移（路由表通过 Redis 持久化共享）
- [ ] **Dead Letter** — 后端不可达时的 Redis 消息队列
- [ ] **Prometheus 指标** — 消息吞吐、路由命中率、后端延迟
- [ ] **文件上传** — 临时素材上传（init → chunk → finish）

---

## 附录

### 技术栈推荐

| 组件 | 选择 | 理由 |
|------|------|------|
| 语言 | Go | 与 costrict-web 技术栈一致 |
| WebSocket | `nhooyr.io/websocket` 或 `github.com/coder/websocket` | 官方维护，API 现代 |
| HTTP | `github.com/gin-gonic/gin` | 与 costrict-web 一致 |
| 配置 | `github.com/goccy/go-yaml` + 环境变量 | 轻量 YAML 解析 |
| 日志 | `log/slog` | Go 标准库，结构化日志 |

### 企微长连接协议速查

| cmd | 方向 | 触发场景 |
|-----|------|---------|
| `aibot_subscribe` | C→S | 连接建立后立即发送 |
| `ping` | C→S | 每 30s 一次 |
| `aibot_msg_callback` | S→C | 用户发消息（单聊或 @bot 群聊） |
| `aibot_event_callback` | S→C | 进入会话 / 卡片点击 / 连接断开 / 用户反馈 |
| `aibot_respond_msg` | C→S | 回复消息回调（支持流式） |
| `aibot_respond_welcome_msg` | C→S | 回复进入会话事件（5s 内） |
| `aibot_respond_update_msg` | C→S | 更新模板卡片（5s 内） |
| `aibot_send_msg` | C→S | 主动推送消息 |
| `aibot_upload_media_init` | C→S | 上传初始化 |
| `aibot_upload_media_chunk` | C→S | 上传分片（≤512KB，≤100片） |
| `aibot_upload_media_finish` | C→S | 上传完成 |

### 速率限制参考

| 维度 | 限制 |
|------|------|
| 每个会话 | 30 条/分钟，1000 条/小时 |
| 回复时效 | 消息回调后 24h 内可回复 |
| 欢迎语时效 | 进入会话事件后 5s 内 |
| 卡片更新时效 | 模板卡片事件后 5s 内 |
| 流式消息时效 | 首次发送后 10min 内完成 |
| 心跳间隔 | 建议 30s |
| 上传频率 | 30 次/分钟，1000 次/小时 |
| 临时素材有效期 | 3 天 |
