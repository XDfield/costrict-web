> **实现状态：待开发**
>
> - 状态：🟡 待开发
> - 目标模块：`server/internal/channel/`（核心模块）+ `server/cmd/channel-worker/`（独立进程）
> - 依赖模块：`server/internal/notification/`（复用 sender 注册表模式）
> - 前置条件：通知渠道模块已实现

---

# Cloud Channel 技术提案

## 目录

- [概述](#概述)
- [术语与概念](#术语与概念)
- [设计原则](#设计原则)
- [架构设计](#架构设计)
- [数据模型](#数据模型)
- [模块设计](#模块设计)
- [企微机器人 Channel 实现](#企微机器人-channel-实现)
- [API 设计](#api-设计)
- [消息流程](#消息流程)
- [与通知模块的关系](#与通知模块的关系)
- [安全考量](#安全考量)
- [错误处理](#错误处理)
- [一期范围与后续规划](#一期范围与后续规划)

---

## 概述

### 背景与动机

costrict-web 已有**通知渠道**（Notification Channel）模块，支持企微群机器人 Webhook、通用 Webhook 等单向推送渠道，用于将设备事件（会话完成、失败等）推送给用户。

后续计划引入**云端 AI**能力，让用户通过配置的 Channel 与云端 AI 进行双向交流（如提问、获取建议、触发任务等）。为此需要建立一套**双向 Channel 基础设施**：

1. **接收消息**：从外部平台（如企微）接收用户消息
2. **处理消息**：路由到云端 AI 进行处理（AI 模块后续实现，本期预留接口）
3. **回复消息**：将 AI 回复通过同一 Channel 返回给用户

一期只支持企微机器人（WeChat Work Bot），实现 Channel 基础架构和企微适配器。

### 参考

- openclaw 的 Channel 插件体系：`ChannelPlugin` 接口定义了 `outbound`（发送）、`gateway`（生命周期）、`security`（安全策略）、`inbound`（消息接收）等适配器模式
- costrict-web 已有的通知 `ChannelSender` 接口：`sender/sender.go` 中的注册表模式
- 企微群机器人 API：支持 Webhook 单向推送；企微自建应用 API：支持双向消息

---

## 术语与概念

| 术语 | 含义 |
|------|------|
| **Channel** | 双向消息通道，连接 costrict-web 与外部消息平台（企微、Slack 等） |
| **Notification Channel** | 已有的单向推送渠道（通知模块），仅 outbound |
| **Channel Adapter** | Channel 的平台适配器，实现特定平台的消息收发逻辑 |
| **Conversation** | 一次 Channel 上的会话，绑定用户、Channel 实例和外部聊天 ID |
| **Bot Handler** | 接收外部平台消息的 Webhook/回调处理器 |
| **Cloud AI** | 后续实现的云端 AI 服务，Channel 模块将消息路由给它 |

---

## 设计原则

1. **复用已有基础设施**：复用 `notification/sender` 的注册表模式，复用已有的 sender（如企微 Webhook 推送能力用于回复）
2. **单向通知 vs 双向 Channel 分离**：通知模块专注事件推送，Channel 模块专注双向对话。两者通过独立的模块和路由共存
3. **AI 无关设计**：本期不实现 AI 处理逻辑，定义 `MessageHandler` 接口作为 AI 接入点，默认使用 echo 回复
4. **可扩展性**：新增 Channel 平台只需实现 `ChannelAdapter` 接口并注册
5. **与 openclaw 模式对齐**：参考 openclaw 的 `ChannelOutboundAdapter` / `ChannelGatewayAdapter` 模式，但不照搬其复杂度，按 costrict-web 实际需要裁剪

---

## 架构设计

### 进程架构

Channel 模块涉及两种运行模式，分别由不同进程承载：

| 进程 | 入口 | 职责 | 部署形态 |
|------|------|------|---------|
| **API Server** | `cmd/api/main.go` | Webhook 回调接收、Channel 配置管理 API、消息记录查询 | 可水平扩展 |
| **Channel Worker** | `cmd/channel-worker/main.go` | 运行长轮询等有状态 Channel、消息处理与回复 | 单实例部署 |

```
                    ┌─────────────────────────────────────────────┐
                    │  API Server（可多实例）                       │
                    │                                             │
  企微 Webhook ────►│  POST /api/channels/wecom/webhook           │
                    │    → WeComAdapter.ParseInbound()            │
                    │    → MessageHandler.Handle()                │
                    │    → WeComAdapter.Reply()                   │
                    │    → 同步处理，直接回复                       │
                    │                                             │
  用户浏览器 ──────►│  Channel 配置 CRUD API                       │
                    │  Conversation 查询 API                      │
                    └─────────────────────────────────────────────┘

                    ┌─────────────────────────────────────────────┐
                    │  Channel Worker（单实例）                     │
                    │                                             │
                    │  Long Polling Channel（微信个人号等）          │
                    │     WeChatAdapter.Start() → Poller goroutine │
                    │     → MessageHandler.Handle()               │
                    │     → WeChatAdapter.Reply()                 │
                    │     → 直接处理，不经过 DB                     │
                    └─────────────────────────────────────────────┘
```

**设计要点：**

- API Server 完全无状态，可水平扩展，同步处理 Webhook 消息并直接回复
- Channel Worker 是有状态的（持有长轮询连接），单实例部署，直接处理长轮询消息
- 两者共享 `channel_configs` 和 `channel_conversations` 表，用于配置管理和会话绑定
- **不做 DB 消息队列**：消息日志留给后续云端 AI 的 session 会话模块

### 与通知模块的共存关系

```
                    ┌──────────────────────┐
                    │  notification/       │  ← 单向推送（已有）
                    │  TriggerNotifications│
                    │  → WeComSender.Send()│
                    └──────────────────────┘
Device Event ───────►│
                    └──────────────────────┘

                    ┌──────────────────────┐
  User (企微) ─────►│  API Server          │  ← Webhook 同步处理
  Webhook Callback  │  channel/handlers.go │
                    │  → MessageHandler    │
                    │  → Adapter.Reply()   │
                    └──────────────────────┘

  User (微信) ─────►│  Channel Worker       │  ← Long Polling 直接处理
  (无 Webhook)      │  WeChat Poller        │
                    │  → MessageHandler    │
                    │  → WeChat.Reply()    │
                    └──────────────────────┘
```

### 目录结构

```
server/
├── cmd/
│   ├── api/main.go               # API Server（已有）
│   ├── worker/main.go            # Sync/Scan Worker（已有）
│   └── channel-worker/main.go    # Channel Worker（新增，单实例）
├── internal/channel/
│   ├── channel.go                # Module 定义、RegisterRoutes（API Server 用）
│   ├── types.go                  # 核心类型定义
│   ├── adapter.go                # ChannelAdapter 接口 + 注册表
│   ├── handler.go                # MessageHandler 接口 + Echo 实现
│   ├── service.go                # ChannelService（配置 CRUD、Webhook 同步处理）
│   ├── worker.go                 # ChannelWorker（Worker 进程核心，启停长轮询）
│   ├── handlers.go               # Gin HTTP Handler（API Server 用）
│   └── adapters/
│       ├── wecom/
│       │   ├── adapter.go
│       │   ├── types.go
│       │   ├── verify.go
│       │   └── crypto.go
│       ├── wechat/
│       │   ├── adapter.go        # WeChatAdapter（StartableChannel）
│       │   ├── types.go
│       │   ├── polling.go        # Long Polling 管理
│       │   └── client.go         # iLink Bot API 客户端
│       └── adapter.go            # 注册入口
├── Dockerfile.channel-worker     # Channel Worker 容器镜像（新增）
```

---

## 数据模型

### ChannelConfig 表（渠道配置）

每个用户可以配置一个或多个 Channel 实例，每个实例对应一个外部平台的接入点。

```sql
CREATE TABLE channel_configs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         TEXT        NOT NULL REFERENCES users(user_id),
    channel_type    TEXT        NOT NULL,                    -- "wecom"
    name            TEXT        NOT NULL,                    -- 用户自定义名称
    enabled         BOOLEAN     NOT NULL DEFAULT true,
    
    -- 平台配置（JSONB，各 Channel 类型不同）
    config          JSONB       NOT NULL DEFAULT '{}',
    
    -- Webhook 验证状态
    webhook_verified BOOLEAN    NOT NULL DEFAULT false,
    webhook_token   TEXT,                                    -- Webhook 验证 token（如企微回调 Token）
    
    -- 状态
    last_active_at  TIMESTAMPTZ,
    last_error      TEXT,
    
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ               -- 软删除
);

CREATE INDEX idx_channel_configs_user_id ON channel_configs(user_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_channel_configs_type    ON channel_configs(channel_type) WHERE deleted_at IS NULL;
```

**企微 Channel 的 config 示例：**

```json
{
  "corpId": "ww1234567890",
  "agentId": "1000002",
  "secret": "xxx",
  "token": "callback-token",
  "encodingAesKey": "callback-aes-key"
}
```

### ChannelConversation 表（会话绑定）

> **不实现。** 会话与对话上下文的映射关系留给后续云端 AI 的 session 会话模块定义。Channel 模块只负责消息的收发和路由。

---

## 模块设计

### ChannelAdapter 接口

参考 openclaw 的 `ChannelOutboundAdapter` 和 `ChannelGatewayAdapter`，定义适合 costrict-web 的简化接口：

```go
// ChannelAdapter Channel 平台适配器接口
type ChannelAdapter interface {
    // Type 返回适配器类型标识（如 "wecom"）
    Type() string
    
    // Capabilities 返回该 Channel 支持的能力
    Capabilities() ChannelCapabilities
    
    // ValidateConfig 校验用户提供的平台配置
    ValidateConfig(config json.RawMessage) error
    
    // ConfigSchema 返回配置字段的描述（供前端渲染表单）
    ConfigSchema() []ConfigField
    
    // ParseInbound 解析外部平台的 Webhook 请求为标准化的 InboundMessage
    // 返回 nil 表示该请求不是消息（如验证请求，由 HandleVerification 处理）
    ParseInbound(r *http.Request) (*InboundMessage, error)
    
    // HandleVerification 处理外部平台的 Webhook URL 验证请求
    // 返回 response body 和是否已处理的标志
    HandleVerification(r *http.Request, config json.RawMessage) (body string, handled bool, err error)
    
    // Reply 通过该 Channel 发送回复消息
    Reply(ctx context.Context, config json.RawMessage, target ReplyTarget, message OutboundMessage) error
    
    // ParseEvent 解析非消息类事件（如群成员变化、机器人被 @ 等）
    // 返回 nil 表示无特殊事件或不需要处理
    ParseEvent(r *http.Request) (*ChannelEvent, error)
}
```

### 核心类型定义

```go
// ChannelCapabilities Channel 支持的能力
type ChannelCapabilities struct {
    InboundMessages  bool     `json:"inboundMessages"`  // 是否支持接收消息
    OutboundMessages bool     `json:"outboundMessages"` // 是否支持发送消息
    DirectChat       bool     `json:"directChat"`       // 是否支持私聊
    GroupChat        bool     `json:"groupChat"`        // 是否支持群聊
    Markdown         bool     `json:"markdown"`         // 是否支持 Markdown
    Media            bool     `json:"media"`            // 是否支持图片/文件
    MentionRequired  bool     `json:"mentionRequired"`  // 群聊中是否需要 @机器人
    ContentTypes     []string `json:"contentTypes"`     // 支持的内容类型
}

// InboundMessage 接收到的消息（标准化）
type InboundMessage struct {
    ExternalChatID    string            `json:"externalChatId"`
    ExternalChatType  string            `json:"externalChatType"`  // "direct" | "group"
    ExternalUserID    string            `json:"externalUserId"`
    ExternalMessageID string            `json:"externalMessageId"`
    Content           string            `json:"content"`
    ContentType       string            `json:"contentType"`       // "text" | "image" | "file"
    Metadata          map[string]any    `json:"metadata,omitempty"`
}

// OutboundMessage 发送的消息
type OutboundMessage struct {
    ContentType string `json:"contentType"` // "text" | "markdown"
    Content     string `json:"content"`
}

// ReplyTarget 回复目标
type ReplyTarget struct {
    ExternalChatID string `json:"externalChatId"`
    ExternalUserID string `json:"externalUserId,omitempty"`
}

// ChannelEvent Channel 事件
type ChannelEvent struct {
    EventType string         `json:"eventType"`
    ChatID    string         `json:"chatId"`
    UserID    string         `json:"userId"`
    Data      map[string]any `json:"data,omitempty"`
}

// ConfigField 配置字段（复用 notification/sender 的定义）
type ConfigField = sender.ConfigField
```

### MessageHandler 接口（AI 接入点）

```go
// MessageHandler 处理接收到的 Channel 消息
// 后续接入云端 AI 时，替换 EchoMessageHandler 为 AI 实现
type MessageHandler interface {
    Handle(ctx context.Context, msg *InboundMessage, conversation *ChannelConversation) (*OutboundMessage, error)
}
```

**一期默认实现 - EchoMessageHandler：**

```go
type EchoMessageHandler struct{}

func (h *EchoMessageHandler) Handle(ctx context.Context, msg *InboundMessage, conv *ChannelConversation) (*OutboundMessage, error) {
    return &OutboundMessage{
        ContentType: "text",
        Content:     fmt.Sprintf("[Echo] %s", msg.Content),
    }, nil
}
```

### ChannelService（API Server 侧）

负责 Webhook 接收与同步处理、配置管理。运行在 API Server 进程中。

```go
type ChannelService struct {
    db              *gorm.DB
    adapters        map[string]ChannelAdapter   // type → adapter
    messageHandler  MessageHandler
}

func NewChannelService(db *gorm.DB, handler MessageHandler) *ChannelService

// Webhook 处理（API Server 调用，同步处理并回复）
func (s *ChannelService) HandleWebhook(channelType string, r *http.Request) (body string, statusCode int, err error)
// 内部流程：ParseInbound → MessageHandler.Handle → Adapter.Reply

// 配置管理
func (s *ChannelService) CreateConfig(userID string, req CreateChannelConfigRequest) (*ChannelConfig, error)
func (s *ChannelService) UpdateConfig(userID, configID string, req UpdateChannelConfigRequest) (*ChannelConfig, error)
func (s *ChannelService) DeleteConfig(userID, configID string) error
func (s *ChannelService) GetConfig(userID, configID string) (*ChannelConfig, error)
func (s *ChannelService) ListConfigs(userID string) ([]ChannelConfig, error)

// 测试
func (s *ChannelService) SendTestMessage(userID, configID string) error
```

### ChannelWorker（独立进程）

运行在 `cmd/channel-worker/main.go`，单实例部署。只负责运行长轮询 Channel。

```go
type ChannelWorker struct {
    db              *gorm.DB
    adapters        map[string]ChannelAdapter
    messageHandler  MessageHandler
    pollers         map[string]context.CancelFunc  // configID → cancel
    mu              sync.Mutex
}

func NewChannelWorker(db *gorm.DB, handler MessageHandler) *ChannelWorker

// 启动 Worker
func (w *ChannelWorker) Run(ctx context.Context) error
// 内部逻辑：
//   1. StartPollers()      — 扫描所有 enabled=true 的 StartableChannel 配置，启动长轮询
//   2. WatchConfigChanges() — 定期检查 ChannelConfig 变更，动态启停长轮询 goroutine

// 长轮询生命周期
func (w *ChannelWorker) startPoller(configID string, config json.RawMessage)
func (w *ChannelWorker) stopPoller(configID string)
func (w *ChannelWorker) refreshPollers()  — 对比 DB 配置与运行中的 pollers，增删
```

### ChannelWorker（独立进程）

运行在 `cmd/channel-worker/main.go`，单实例部署。负责两件事：

1. **处理 Webhook 消息**：轮询 `channel_messages` 表中 `status=pending` 的 inbound 消息，调用 MessageHandler 后回复
2. **运行长轮询 Channel**：启动时为每个 `enabled=true` 且实现 `StartableChannel` 的 ChannelConfig 启动长轮询 goroutine

```go
type ChannelWorker struct {
    db              *gorm.DB
    adapters        map[string]ChannelAdapter
    messageHandler  MessageHandler
    pollers         map[string]context.CancelFunc  // configID → cancel
    mu              sync.Mutex
}

func NewChannelWorker(db *gorm.DB, handler MessageHandler) *ChannelWorker

// 启动 Worker
func (w *ChannelWorker) Run(ctx context.Context) error
// 内部逻辑：
//   1. StartPollers()      — 扫描所有 enabled=true 的 StartableChannel 配置，启动长轮询
//   2. ProcessPendingLoop() — 循环轮询 channel_messages (pending)，处理并回复
//   3. WatchConfigChanges() — 定期检查 ChannelConfig 变更，动态启停长轮询 goroutine

// 长轮询生命周期
func (w *ChannelWorker) startPoller(configID string, config json.RawMessage)
func (w *ChannelWorker) stopPoller(configID string)
func (w *ChannelWorker) refreshPollers()  — 对比 DB 配置与运行中的 pollers，增删
```

**Pending 消息处理流程：**

```
1. SELECT * FROM channel_messages WHERE status='pending' AND direction='inbound'
   ORDER BY created_at ASC LIMIT 50
   FOR UPDATE SKIP LOCKED

2. 对每条消息：
   a. UPDATE status='processing'
   b. 查找 ChannelConfig → 获取 adapter
   c. messageHandler.Handle(msg, conv) → OutboundMessage
   d. adapter.Reply(ctx, config, target, response)
   e. 记录 outbound ChannelMessage
   f. UPDATE status='delivered'
   g. 若失败 → UPDATE status='failed', error='...'

3. SLEEP(poll_interval) → 回到步骤 1
```

### cmd/channel-worker/main.go

参照现有 `cmd/worker/main.go` 模式：

```go
func main() {
    logger.Init(logger.Config{
        Dir: "./logs", FilePrefix: "channel-worker",
        MaxAgeDays: 7, Console: true, ConsoleLevel: "warn",
    })

    cfg := config.Load()
    db := database.Initialize(cfg.DatabaseURL)
    // AutoMigrate channel models...

    worker := channel.NewChannelWorker(db, &channel.EchoMessageHandler{})

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    log.Fatal(worker.Run(ctx))
}
```

### 部署配置

```yaml
# docker-compose.yml
channel-worker:
    build:
        context: ./server
        dockerfile: Dockerfile.channel-worker
    environment:
        - DATABASE_URL=postgres://...
    deploy:
        replicas: 1    # 必须单实例
```

```json
// package.json scripts 新增
"dev:channel-worker": "cd server && go run ./cmd/channel-worker",
"build:channel-worker": "cd server && go build -o bin/channel-worker ./cmd/channel-worker"
```

---

## 企微 Channel 实现（wecom）

### 平台选择

企微有两种机器人接入方式：

| 方式 | 能力 | 适用场景 |
|------|------|---------|
| **群机器人 Webhook** | 仅单向推送（outbound），无法接收消息 | 通知模块（已实现） |
| **自建应用** | 双向消息（inbound + outbound），支持私聊和群聊 | Channel 模块（本期实现） |

一期采用**企微自建应用**方式，通过回调 URL 接收用户消息。

### 企微自建应用接入流程

1. 管理员在企微管理后台创建自建应用
2. 配置应用的接收消息服务器 URL：`https://<costrict-host>/api/channels/wecom/webhook`
3. 设置 Token 和 EncodingAESKey
4. 用户在 costrict-web 控制台填入 CorpID、AgentID、Secret、Token、EncodingAESKey
5. costrict-web 验证配置并保存
6. 用户在企微中找到应用，发送消息即可与云端 AI 对话

### WeComAdapter 实现要点

```go
type WeComAdapter struct {
    client *http.Client
}

type WeComConfig struct {
    CorpID         string `json:"corpId"`
    AgentID        int    `json:"agentId"`
    Secret         string `json:"secret"`
    Token          string `json:"token"`          // 回调 Token
    EncodingAESKey string `json:"encodingAesKey"` // 回调加密 Key
}
```

**关键实现：**

1. **URL 验证**：企微配置回调 URL 时会发送验证请求（`GET /webhook?msg_signature=...&echostr=...`），需要用 Token + EncodingAESKey 解密 echostr 并原样返回
2. **消息解密**：接收到的消息体是加密的 XML，需要用 AES 解密
3. **消息解析**：解析 XML 获取 `Content`、`FromUserName`、`MsgType` 等
4. **消息发送**：调用企微 API `POST https://qyapi.weixin.qq.com/cgi-bin/message/send` 发送文本/Markdown 消息
5. **Access Token 管理**：通过 `corpId + secret` 获取 access_token，需要缓存和自动刷新（有效期 7200s）

### Webhook 路由设计

```
POST /api/channels/wecom/webhook
    ?msg_signature=xxx&timestamp=xxx&nonce=xxx
    
Body: <XML encrypted message>

处理流程（API Server 同步处理）：
1. 从 URL 查询参数获取 msg_signature, timestamp, nonce
2. 查找所有 wecom 类型的 ChannelConfig（遍历匹配 Token）
3. 用匹配的 config 解密消息
4. 解析为 InboundMessage
5. 查找或创建 Conversation
6. MessageHandler.Handle() → OutboundMessage
7. adapter.Reply() → 发送回复
8. 返回 "success" 给企微服务器
```

---

## 微信个人号 Channel 实现（wechat）

### 背景

参考腾讯官方发布的 [`Tencent/openclaw-weixin`](https://github.com/Tencent/openclaw-weixin) 插件（156 stars）以及社区版 [`freestylefly/openclaw-wechat`](https://github.com/freestylefly/openclaw-wechat)（1615 stars），两者均通过 iLink Bot 协议接入微信个人号，支持 QR 码扫码登录、私聊和群聊。

与企微自建应用的**被动接收 Webhook 回调**不同，微信个人号采用**主动长轮询（Long Polling）**模式——costrict-web server 作为客户端，持续向微信 iLink Bot 后端拉取新消息。

### 架构对比

| 维度 | 企微（wecom） | 微信个人号（wechat） |
|------|-------------|-------------------|
| 消息接收 | 被动：Webhook 回调 | 主动：Long Polling 拉取 |
| 消息发送 | REST API 调用 | REST API 调用 |
| 鉴权 | corpId + secret → access_token | Bearer token（登录后获取） |
| 加密 | AES-CBC（回调消息加解密） | AES-128-ECB（CDN 媒体传输） |
| 登录 | 不需要（配置即用） | QR 码扫码登录 |
| Gateway 模式 | stateless（无状态） | stateful（需维持长轮询连接） |

### iLink Bot API 协议

基于 `Tencent/openclaw-weixin` README 中公开的 Backend API Protocol：

**通用请求头：**

| Header | 值 |
|--------|---|
| `Content-Type` | `application/json` |
| `AuthorizationType` | `ilink_bot_token` |
| `Authorization` | `Bearer <token>` |
| `X-WECHAT-UIN` | Base64 编码的随机 uint32 |

**核心端点：**

| 端点 | 路径 | 说明 |
|------|------|------|
| `getUpdates` | `getupdates` | 长轮询获取新消息 |
| `sendMessage` | `sendmessage` | 发送消息（文本/图片/视频/文件） |
| `getUploadUrl` | `getuploadurl` | 获取 CDN 上传预签名 URL |
| `getConfig` | `getconfig` | 获取账号配置（typing ticket 等） |
| `sendTyping` | `sendtyping` | 发送/取消输入状态 |

**消息结构（WeixinMessage）：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `from_user_id` | string | 发送者 ID |
| `to_user_id` | string | 接收者 ID |
| `message_type` | number | 1=USER, 2=BOT |
| `item_list` | MessageItem[] | 消息内容（文本/图片/语音/文件/视频） |
| `context_token` | string | 会话上下文 token，回复时需传回 |

### WeChatAdapter 实现要点

```go
type WeChatAdapter struct {
    client *http.Client
}

type WeChatConfig struct {
    APIServer   string `json:"apiServer"`   // iLink Bot 后端地址
    Token       string `json:"token"`        // Bearer token（扫码登录后获取）
}
```

**关键实现：**

1. **长轮询消息拉取**：启动后台 goroutine，持续调用 `getUpdates`（带 `get_updates_buf` 游标），获取新消息后交给 MessageHandler 处理
2. **消息发送**：调用 `sendMessage` 端点，将 OutboundMessage 转换为 iLink Bot 格式（`item_list` + `context_token`）
3. **输入状态**：处理消息前可调用 `sendTyping` 提示用户"正在输入"
4. **会话上下文**：通过 `context_token` 维护对话上下文
5. **生命周期管理**：Channel 启用后开始长轮询，禁用/删除后停止（通过 `context.Context` 取消）

### 目录结构补充

```
server/internal/channel/adapters/
    ├── wecom/                    # 企微自建应用
    │   ├── adapter.go
    │   ├── types.go
    │   ├── verify.go
    │   └── crypto.go
    └── wechat/                   # 微信个人号（iLink Bot）
        ├── adapter.go            # WeChatAdapter 实现
        ├── types.go              # iLink Bot 消息类型定义
        ├── polling.go            # Long Polling 长轮询管理
        └── client.go             # iLink Bot API 客户端封装
```

### Gateway 模式适配

微信个人号的长轮询模式需要扩展 `ChannelAdapter` 接口或 ChannelService 的启动逻辑：

```go
// StartableChannel 可选接口，用于需要后台运行的 Channel
// 未实现此接口的 Channel（如 wecom）仅通过 Webhook 被动接收
type StartableChannel interface {
    ChannelAdapter
    // Start 启动后台消息拉取，ctx 取消时停止
    Start(ctx context.Context, config json.RawMessage, handler InboundMessageHandler) error
}
```

**ChannelService 启动逻辑：**

```
ChannelService.Start() {
    for each ChannelConfig where enabled=true:
        adapter := registry.Get(config.ChannelType)
        if startable, ok := adapter.(StartableChannel); ok {
            go startable.Start(ctx, config, messageHandler)
        }
}
```

- wecom：不实现 `StartableChannel`，被动等待 Webhook
- wechat：实现 `StartableChannel`，启动时自动开始长轮询

---

## API 设计

### 端点汇总

| 方法 | 路径 | 权限 | 说明 |
|------|------|------|------|
| `POST` | `/api/channels/wecom/webhook` | 无（外部回调） | 企微消息回调入口 |
| — | *(微信个人号无 Webhook，通过长轮询主动拉取)* | — | — |
| `GET` | `/api/channels/available` | 用户 | 列出可用 Channel 类型及配置 Schema |
| `GET` | `/api/channels` | 用户 | 列出用户已配置的 Channel |
| `POST` | `/api/channels` | 用户 | 创建 Channel 配置 |
| `GET` | `/api/channels/:id` | 用户 | 获取 Channel 配置详情 |
| `PUT` | `/api/channels/:id` | 用户 | 更新 Channel 配置 |
| `DELETE` | `/api/channels/:id` | 用户 | 删除 Channel 配置 |
| `POST` | `/api/channels/:id/test` | 用户 | 发送测试消息 |

### 关键请求/响应

#### GET /api/channels/available

```json
{
  "channelTypes": [
    {
      "type": "wecom",
      "name": "企业微信（自建应用）",
      "capabilities": {
        "inboundMessages": true,
        "outboundMessages": true,
        "directChat": true,
        "groupChat": true,
        "markdown": true,
        "media": false,
        "mentionRequired": true,
        "contentTypes": ["text", "image"]
      },
      "schema": [
        {
          "key": "corpId",
          "label": "企业 ID (CorpID)",
          "type": "text",
          "required": true,
          "placeholder": "ww1234567890abcdef",
          "helpText": "在企业微信管理后台 → 我的企业 中获取"
        },
        {
          "key": "agentId",
          "label": "应用 AgentID",
          "type": "text",
          "required": true,
          "placeholder": "1000002"
        },
        {
          "key": "secret",
          "label": "应用 Secret",
          "type": "password",
          "required": true,
          "helpText": "在企业微信管理后台 → 应用管理 中获取"
        },
        {
          "key": "token",
          "label": "回调 Token",
          "type": "password",
          "required": true,
          "helpText": "配置接收消息服务器时自定义的 Token"
        },
        {
          "key": "encodingAesKey",
          "label": "回调 EncodingAESKey",
          "type": "password",
          "required": true,
          "helpText": "配置接收消息服务器时自定义的 EncodingAESKey"
        }
      ]
    }
  ]
}
```

#### POST /api/channels

```json
// Request
{
  "channelType": "wecom",
  "name": "我的企微 AI 助手",
  "config": {
    "corpId": "ww1234567890abcdef",
    "agentId": 1000002,
    "secret": "xxx",
    "token": "my-token",
    "encodingAesKey": "my-aes-key"
  }
}

// Response 201
{
  "channel": {
    "id": "uuid-xxx",
    "channelType": "wecom",
    "name": "我的企微 AI 助手",
    "enabled": true,
    "webhookVerified": false,
    "config": {
      "corpId": "ww1234567890abcdef",
      "agentId": 1000002,
      "secret": "***",
      "token": "***",
      "encodingAesKey": "***"
    },
    "webhookUrl": "https://<host>/api/channels/wecom/webhook",
    "createdAt": "2026-04-11T00:00:00Z"
  }
}
```

**注意**：config 中的敏感字段（secret、token、encodingAesKey）在响应中应脱敏为 `***`。

---

## 消息流程

### 路径 A：Webhook Channel（企微）— API Server 同步处理

```
1. 企微服务器 POST /api/channels/wecom/webhook
   │
   ▼ [API Server]
2. WeComAdapter.HandleVerification()
   │  是验证请求 → 返回解密后的 echostr → 结束
   │
   │  不是验证请求 ↓
   │
3. WeComAdapter.ParseInbound(r)
   │  → 解密 XML → 解析为 InboundMessage
   │
4. 查找匹配的 ChannelConfig
   │
5. MessageHandler.Handle(ctx, msg, conv)
   │  → 一期: EchoMessageHandler
   │  → 后续: AI 处理器
   │
7. WeComAdapter.Reply(ctx, config, target, response)
   │  → 获取 access_token → 调用企微消息发送 API
   │
8. 返回 "success" 给企微服务器

### 路径 B：Long Polling Channel（微信个人号）— Worker 直接处理

```
[Channel Worker 启动时]
1. 扫描所有 enabled=true 的 wechat 类型 ChannelConfig
2. 对每个 config 启动 Poller goroutine
   │
   ▼ [Poller goroutine]
3. WeChatClient.GetUpdates(ctx, cursor)
   │  → 长轮询等待（最多 35s）
   │  → 返回 msgs[]
   │
4. 对每条消息：
   │  a. 转换为 InboundMessage
   │  b. MessageHandler.Handle() → OutboundMessage
   │  c. WeChatClient.SendMessage() 发送回复
   │  d. 更新 cursor → 继续下一轮 GetUpdates
   │
   ▼ [config 变更时]
5. Worker 定期 refreshPollers()
   → 新增 enabled 的 config → 启动新 Poller
   → disabled/删除 的 config → 停止对应 Poller
```

### Access Token 缓存策略

```
首次调用 → 请求 access_token → 缓存（key: corpId:agentId, TTL: 7000s）
后续调用 → 检查缓存 → 有效则直接使用
                  → 过期则刷新
```

使用 `sync.Map` 或内存 cache 实现，无需 Redis。

---

## 与通知模块的关系

### 职责边界

| 维度 | Notification（已有） | Channel（本期新增） |
|------|---------------------|-------------------|
| 方向 | 单向（outbound only） | 双向（inbound + outbound） |
| 触发 | 系统事件（会话完成等） | 用户主动发消息 |
| 配置 | UserNotificationChannel | ChannelConfig |
| 消息格式 | 事件通知（标题+摘要） | 自由对话 |
| AI 集成 | 无 | 通过 MessageHandler 接入 |
| 发送能力 | WeComSender（群机器人 Webhook） | WeComAdapter（自建应用 API） |

### 不复用 sender 的原因

- 通知模块的 `WeComSender` 使用群机器人 Webhook（单向，只需 URL）
- Channel 模块需要自建应用 API（双向，需要 CorpID + Secret + AgentID）
- 两者的鉴权方式、API 调用方式完全不同
- Channel 需要消息接收（inbound）能力，sender 没有这个接口

但 **注册表模式**（`Register` / `Get` / `All`）和 **ConfigField 模式**可以复用。

---

## 安全考量

1. **Webhook 鉴权**：通过企微的 msg_signature 机制验证请求合法性（SHA1(sort(token, timestamp, nonce, encrypt))）
2. **敏感配置加密**：ChannelConfig 中的 secret、token 等字段考虑加密存储（至少在 API 响应中脱敏）
3. **用户隔离**：每个 ChannelConfig 绑定 user_id，消息处理时验证归属
4. **消息去重**：通过 external_message_id 去重，避免企微重试导致重复处理
5. **速率限制**：Webhook 入口加 rate limiter，防止消息洪泛
6. **输入校验**：对用户消息内容做长度限制和基础校验

---

## 错误处理

| 场景 | 处理方式 |
|------|---------|
| Webhook 验证失败 | 返回空字符串，不暴露内部信息 |
| 消息解密失败 | 记录日志，返回 200（避免企微重试） |
| access_token 获取失败 | 记录 ChannelMessage 为 failed，返回 200 |
| 消息发送失败 | 记录 ChannelMessage 为 failed + error 信息 |
| AI 处理超时 | 设置超时（如 30s），超时后返回默认消息 |
| 未找到匹配的 ChannelConfig | 返回 200，记录 warning 日志 |
| 重复消息（同 external_message_id） | 跳过处理，返回 200 |

**关键原则**：对所有 inbound 请求返回 HTTP 200，避免企微服务器重试。错误通过 ChannelMessage 的 status 和 error 字段记录。

---

## 一期范围与后续规划

### 一期（本次实现）

- [x] Channel 模块基础架构（ChannelAdapter 接口、注册表）
- [x] `StartableChannel` 可选接口（支持长轮询模式）
- [x] ChannelConfig 数据模型（会话/消息日志留给 AI session 模块）
- [x] **独立 Channel Worker 进程**（`cmd/channel-worker/main.go`）
  - Pending 消息轮询与处理
  - 长轮询 Channel 启停管理
  - 配置变更动态感知
- [x] **API Server 侧 Channel 模块**（Webhook 接收 + 配置管理 API）
- [x] 企微自建应用适配器（WeComAdapter）— Webhook 回调模式
  - 回调 URL 验证
  - 消息接收（文本消息）
  - 消息发送（文本/Markdown）
  - Access Token 缓存
- [x] 微信个人号适配器（WeChatAdapter）— iLink Bot 长轮询模式
  - iLink Bot API 客户端
  - Long Polling 消息拉取
  - 消息发送（文本）
  - 输入状态提示（typing）
- [x] Channel 管理 API（CRUD + 测试）
- [x] EchoMessageHandler（默认 AI 回复）
- [x] 消息记录和查询 API
- [x] Dockerfile.channel-worker + package.json scripts

### 二期（AI 接入）

- [ ] 实现 AIMessageHandler，对接云端 AI 服务
- [ ] 支持流式回复（如适用）
- [ ] 对话上下文管理（历史消息作为 AI 上下文）
- [ ] 支持 @机器人触发（群聊场景）

### 三期（扩展）

- [ ] 飞书（Feishu）Channel 适配器
- [ ] Slack Channel 适配器
- [ ] Discord Channel 适配器
- [ ] 富媒体消息支持（图片、文件）
- [ ] Channel 健康检查与监控
- [ ] Channel 统计面板（消息量、响应时间等）

---

**文档版本：** 2.2.0
**创建日期：** 2026-04-11
**更新日期：** 2026-04-11
**维护者：** CoStrict Team
