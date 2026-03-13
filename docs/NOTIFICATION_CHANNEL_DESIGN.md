# 通知渠道模块技术提案

## 目录

- [概述](#概述)
- [背景与动机](#背景与动机)
- [可行性分析](#可行性分析)
- [架构设计](#架构设计)
- [数据模型](#数据模型)
- [模块设计](#模块设计)
- [渠道发送器](#渠道发送器)
- [API 设计](#api-设计)
- [与 SSE 模块集成](#与-sse-模块集成)
- [错误处理](#错误处理)
- [双向扩展预留](#双向扩展预留)
- [实施计划](#实施计划)

---

## 概述

### 背景与动机

`CLOUD_SSE_SERVER_DESIGN.md` 中的 SSE 模块实现了 opencode CLI 与 Console App 之间的实时事件推送。但 SSE 是**在线推送**，当用户未打开 Console App 时，执行事件（任务完成、失败、需要确认）将无法触达用户。

通知渠道模块作为 SSE 实时推送的**离线补偿**，在用户不在线时通过 IM 机器人或 Webhook 主动推送通知。

### 参考来源

参考 openclaw 项目的渠道架构设计（`src/channels/plugins/`、`src/gateway/server-channels.ts`），openclaw 的渠道是**双向 IM 接入**（用户通过 Telegram/Slack 发消息给 AI 执行命令）。本模块取其插件式渠道架构和状态追踪设计，定位调整为**单向推送通知**：

| 特性 | openclaw 渠道 | 本模块 v1 | 本模块 v2（预留） |
|------|-------------|-----------|------------|
| 方向 | 双向（接收用户消息 + 发送回复） | 单向（主动推送通知） | 双向（Webhook 回调接收指令） |
| 渠道类型 | Telegram、WhatsApp、Discord、Slack、Signal 等 | Slack、钉钉、企业微信、飞书、通用 Webhook | 同左，新增 Webhook 接收端点 |
| 配置存储 | YAML 配置文件 | PostgreSQL（与现有模型一致） | 同左，新增 `inbound_config` 字段 |
| 多账户 | 每渠道支持多账户 | 每条记录为独立渠道配置（用户可创建多个） | 同左 |
| 插件架构 | `ChannelPlugin` 接口 + 适配器模式 | `ChannelSender` 接口（单向） | 新增 `ChannelReceiver` 接口（双向） |
| 重连策略 | 指数退避，最多 10 次 | 无需（HTTP 请求，无持久连接） | 无需（Webhook 回调，平台主动推送） |

---

## 可行性分析

### 技术栈匹配度

| 需求 | 现有条件 | 结论 |
|------|---------|------|
| 数据库存储 | GORM + PostgreSQL | **直接扩展** |
| JSON 字段 | `gorm.io/datatypes` | **直接使用** |
| 用户身份 | `middleware.UserIDKey` | **直接复用** |
| 工作空间关联 | `Organization.ID` | **直接映射** |
| HTTP 客户端 | Go 标准库 `net/http` | **直接使用** |
| 异步执行 | Go goroutine | **直接使用** |
| HMAC 签名 | Go 标准库 `crypto/hmac` | **直接使用** |

### 需要新增的内容

| 内容 | 工作量 | 说明 |
|------|--------|------|
| `NotificationChannel`、`NotificationLog` 模型 | 小 | 追加到 `models/models.go` |
| `internal/notification/` 模块 | 中 | service + handlers + senders |
| `/api/notification-channels` 路由注册 | 小 | `main.go` 追加约 15 行 |
| SSE EventRouter 集成点 | 小 | `event_router.go` 注入 `NotificationService` |
| `pq.StringArray` 依赖 | 小 | `github.com/lib/pq`（PostgreSQL text[] 数组） |

---

## 架构设计

### 整体定位

```
opencode CLI
    │  POST /cloud/event（推送执行事件）
    ▼
EventRouter.RouteDeviceEvent()
    │
    ├─────────────────────────────────────► Console App（SSE 实时推送）
    │                                            │
    │                                      用户在线 → 实时看到
    │
    └─────────────────────────────────────► NotificationService.TriggerNotifications()
                                                 │
                                           检查用户 SSE 连接状态
                                                 │
                                           用户离线 → 查询匹配的通知渠道
                                                 │
                                    ┌────────────┼────────────┬────────────┐
                                  Slack      DingTalk      WeCom       Webhook
                                    │            │            │            │
                                 HTTP POST    HTTP POST    HTTP POST    HTTP POST
                                    │            │            │            │
                                 写入 NotificationLog（记录发送结果）
```

### 新增目录结构

```
server/internal/
├── notification/
│   ├── notification.go     # 模块初始化、RegisterRoutes
│   ├── handlers.go         # Gin HTTP Handler（8 个端点）
│   ├── service.go          # NotificationService（触发逻辑、日志记录）
│   ├── types.go            # 公共类型（ChannelSender 接口、各渠道 Config 结构）
│   └── senders/
│       ├── slack.go        # Slack Incoming Webhook
│       ├── dingtalk.go     # 钉钉自定义机器人（支持加签）
│       ├── wecom.go        # 企业微信群机器人
│       ├── feishu.go       # 飞书自定义机器人（支持签名）
│       └── webhook.go      # 通用 Webhook（支持 HMAC-SHA256）
├── cloud/                  # SSE 模块（已有，需注入 NotificationService）
├── models/
│   └── models.go           # 追加 NotificationChannel、NotificationLog 模型
└── ...
```

---

## 数据模型

### NotificationChannel 表

```go
// NotificationChannel 通知渠道配置
type NotificationChannel struct {
    ID            string              `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
    Name          string              `gorm:"not null"                                       json:"name"`
    ChannelType   string              `gorm:"not null;index"                                 json:"channelType"`
    UserID        string              `gorm:"not null;index"                                 json:"userId"`
    WorkspaceID   string              `gorm:"index"                                          json:"workspaceId"`
    Enabled       bool                `gorm:"not null;default:true"                          json:"enabled"`
    Config        datatypes.JSON      `gorm:"not null"                                       json:"config"`
    TriggerEvents pq.StringArray      `gorm:"type:text[]"                                    json:"triggerEvents"`
    LastUsedAt    *time.Time          `                                                      json:"lastUsedAt,omitempty"`
    LastError     string              `                                                      json:"lastError,omitempty"`

    // --- v2 双向扩展预留字段（当前值均为零值，不影响 v1 逻辑）---
    InboundEnabled bool           `gorm:"not null;default:false"  json:"inboundEnabled"` // 是否启用 Webhook 回调接收
    InboundConfig  datatypes.JSON `                               json:"inboundConfig,omitempty"` // 回调接收配置（验签密钥等）
    WebhookSecret  string         `gorm:"not null;default:''"     json:"-"`              // 平台回调验签密钥（不对外暴露）
    WebhookToken   string         `gorm:"uniqueIndex;not null;default:gen_random_uuid()" json:"webhookToken"` // 回调 URL 中的唯一 token，用于路由到本渠道

    CreatedAt     time.Time           `                                                      json:"createdAt"`
    UpdatedAt     time.Time           `                                                      json:"updatedAt"`
    DeletedAt     gorm.DeletedAt      `gorm:"index"                                          json:"-"`
}
```

### NotificationLog 表

```go
// NotificationLog 通知发送记录
type NotificationLog struct {
    ID        string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
    ChannelID string         `gorm:"not null;index"                                 json:"channelId"`
    EventType string         `gorm:"not null"                                       json:"eventType"`
    SessionID string         `gorm:"index"                                          json:"sessionId,omitempty"`
    DeviceID  string         `gorm:"index"                                          json:"deviceId,omitempty"`
    Status    string         `gorm:"not null"                                       json:"status"`
    Payload   datatypes.JSON `                                                      json:"payload,omitempty"`
    Error     string         `                                                      json:"error,omitempty"`
    SentAt    *time.Time     `                                                      json:"sentAt,omitempty"`
    CreatedAt time.Time      `                                                      json:"createdAt"`
}
```

### 字段说明

#### NotificationChannel

| 字段 | 类型 | 约束 | 说明 |
|------|------|------|------|
| `id` | uuid | PK | 渠道唯一标识 |
| `name` | varchar | NOT NULL | 用户自定义名称，如 "工作群钉钉机器人" |
| `channel_type` | varchar | NOT NULL INDEX | 渠道类型，见下方常量 |
| `user_id` | varchar | NOT NULL INDEX | 归属用户（Casdoor sub） |
| `workspace_id` | varchar | INDEX | 归属工作空间，空表示个人渠道 |
| `enabled` | bool | NOT NULL DEFAULT true | 是否启用 |
| `config` | jsonb | NOT NULL | 渠道配置（含 URL、密钥等） |
| `trigger_events` | text[] | | 触发事件列表，空表示不自动触发 |
| `last_used_at` | timestamp | | 最后一次成功发送时间 |
| `last_error` | text | | 最后一次发送失败的错误信息 |
| `inbound_enabled` | bool | DEFAULT false | **[v2 预留]** 是否启用 Webhook 回调接收 |
| `inbound_config` | jsonb | | **[v2 预留]** 回调接收配置（如消息过滤规则） |
| `webhook_secret` | varchar | DEFAULT '' | **[v2 预留]** 平台回调验签密钥，不对外暴露 |
| `webhook_token` | varchar | UNIQUE NOT NULL | **[v2 预留]** 回调 URL 唯一 token，建表时自动生成，用于区分不同渠道的回调端点 |

**渠道类型常量：**

| 值 | 说明 |
|----|------|
| `slack` | Slack Incoming Webhook |
| `dingtalk` | 钉钉自定义机器人 |
| `wecom` | 企业微信群机器人 |
| `feishu` | 飞书自定义机器人 |
| `webhook` | 通用 HTTP Webhook |

**触发事件常量：**

| 值 | 触发时机 |
|----|---------|
| `session.completed` | 会话执行完成 |
| `session.failed` | 会话执行失败 |
| `session.aborted` | 会话被用户中止 |
| `device.offline` | 设备意外断线 |

#### NotificationLog

| 字段 | 类型 | 约束 | 说明 |
|------|------|------|------|
| `id` | uuid | PK | 日志唯一标识 |
| `channel_id` | uuid | NOT NULL INDEX | 关联渠道 |
| `event_type` | varchar | NOT NULL | 触发的事件类型 |
| `session_id` | varchar | INDEX | 关联会话（可为空） |
| `device_id` | varchar | INDEX | 关联设备（可为空） |
| `status` | varchar | NOT NULL | `success` \| `failed` |
| `payload` | jsonb | | 发送的消息内容快照 |
| `error` | text | | 失败时的错误信息 |
| `sent_at` | timestamp | | 实际发送时间 |

---

## 模块设计

### types.go — 公共类型

```go
package notification

// ChannelSender 渠道发送器接口（插件式架构，参考 openclaw ChannelPlugin）
// v1 实现此接口即可上线
type ChannelSender interface {
    // Type 返回渠道类型标识，如 "slack"
    Type() string
    // Send 发送通知，config 为该渠道的 JSON 配置
    Send(config json.RawMessage, msg NotificationMessage) error
    // ValidateConfig 校验配置合法性（创建/更新时调用）
    ValidateConfig(config json.RawMessage) error
}

// ChannelReceiver 渠道接收器接口（v2 双向扩展，当前不实现）
//
// v2 实现此接口后，渠道即支持接收用户从 IM 发来的指令并路由到 opencode CLI。
// 接口设计遵循 openclaw ChannelOutboundAdapter 的对称原则。
//
// 实现路径：
//   1. 各平台发送器同时实现 ChannelReceiver（如 SlackSender → SlackChannel）
//   2. 注册到 NotificationService.receivers map
//   3. WebhookReceiveHandler 根据 webhook_token 找到渠道，调用 receiver.ParseInbound()
//   4. 解析结果路由到 cloud.EventRouter.RouteUserCommand()
type ChannelReceiver interface {
    // VerifySignature 验证平台回调请求的签名合法性
    // rawBody 为原始请求体，headers 为请求头，secret 为 webhook_secret 字段值
    VerifySignature(rawBody []byte, headers map[string]string, secret string) error
    // ParseInbound 将平台回调的原始 payload 解析为标准化的入站消息
    ParseInbound(rawBody []byte) (*InboundMessage, error)
}

// InboundMessage 从 IM 渠道接收到的标准化入站消息（v2 预留）
type InboundMessage struct {
    ChannelUserID string         // 平台侧的用户标识（如 Slack user_id、钉钉 senderStaffId）
    ChatID        string         // 平台侧的会话标识（用于回复）
    Text          string         // 用户发送的原始文本
    ReplyToMsgID  string         // 若为回复消息，关联的原始消息 ID
    RawPayload    map[string]any // 原始 payload（供扩展使用）
}

// NotificationMessage 通知消息结构
type NotificationMessage struct {
    Title     string         `json:"title"`
    Body      string         `json:"body"`
    EventType string         `json:"eventType"`
    SessionID string         `json:"sessionId,omitempty"`
    DeviceID  string         `json:"deviceId,omitempty"`
    Metadata  map[string]any `json:"metadata,omitempty"`
}

// 各渠道 Config 结构（存入 NotificationChannel.Config JSON 字段）

type SlackConfig struct {
    WebhookURL string `json:"webhookUrl"`
}

type DingTalkConfig struct {
    WebhookURL string `json:"webhookUrl"`
    Secret     string `json:"secret,omitempty"`  // 加签密钥（可选）
}

type WeComConfig struct {
    WebhookURL string `json:"webhookUrl"`
}

type FeishuConfig struct {
    WebhookURL string `json:"webhookUrl"`
    Secret     string `json:"secret,omitempty"`  // 签名密钥（可选）
}

type WebhookConfig struct {
    URL     string            `json:"url"`
    Method  string            `json:"method,omitempty"`   // 默认 "POST"
    Headers map[string]string `json:"headers,omitempty"`
    Secret  string            `json:"secret,omitempty"`   // HMAC-SHA256 签名密钥（可选）
}
```

### service.go — 业务逻辑层

```go
package notification

type NotificationService struct {
    db      *gorm.DB
    senders map[string]ChannelSender  // channelType → sender 实例
}

func NewNotificationService(db *gorm.DB) *NotificationService

// TriggerNotifications 根据事件触发匹配的通知渠道（异步执行，不阻塞调用方）
//
// 参数：
//   - userID: 设备归属用户 ID
//   - event: 来自 SSE EventRouter 的事件
//   - sessionID / deviceID: 事件关联上下文
//   - onlyWhenOffline: true 时仅在用户无活跃 SSE 连接时发送
//   - manager: SSE ConnectionManager，用于检查用户在线状态
func (s *NotificationService) TriggerNotifications(
    userID string,
    event cloud.Event,
    sessionID string,
    deviceID string,
    onlyWhenOffline bool,
    manager *cloud.ConnectionManager,
)

// send 内部方法：查询匹配渠道并逐一发送，写入 NotificationLog
func (s *NotificationService) send(
    userID string,
    eventType string,
    msg NotificationMessage,
)

// SendTest 向指定渠道发送测试通知（校验归属）
func (s *NotificationService) SendTest(channelID, userID string) error

// ListLogs 查询渠道的通知发送记录（最近 N 条）
func (s *NotificationService) ListLogs(channelID, userID string, limit int) ([]models.NotificationLog, error)

// buildMessage 根据事件类型构建 NotificationMessage
func (s *NotificationService) buildMessage(event cloud.Event, sessionID, deviceID string) NotificationMessage
```

**TriggerNotifications 核心逻辑：**

```go
func (s *NotificationService) TriggerNotifications(...) {
    go func() {
        // 1. 检查用户在线状态（可选）
        if onlyWhenOffline {
            userConns := manager.FindUserConnsByUser(userID)
            if len(userConns) > 0 {
                return  // 用户在线，跳过通知
            }
        }

        // 2. 构建消息
        msg := s.buildMessage(event, sessionID, deviceID)

        // 3. 触发发送
        s.send(userID, event.Type, msg)
    }()
}

func (s *NotificationService) send(userID, eventType string, msg NotificationMessage) {
    // 查询该用户下 enabled=true 且 trigger_events 包含 eventType 的渠道
    var channels []models.NotificationChannel
    s.db.Where(
        "user_id = ? AND enabled = true AND ? = ANY(trigger_events)",
        userID, eventType,
    ).Find(&channels)

    for _, ch := range channels {
        sender, ok := s.senders[ch.ChannelType]
        if !ok {
            continue
        }

        sentAt := time.Now()
        err := sender.Send(ch.Config, msg)

        log := models.NotificationLog{
            ChannelID: ch.ID,
            EventType: eventType,
            SessionID: msg.SessionID,
            DeviceID:  msg.DeviceID,
            SentAt:    &sentAt,
        }

        if err != nil {
            log.Status = "failed"
            log.Error = err.Error()
            s.db.Model(&ch).Update("last_error", err.Error())
        } else {
            log.Status = "success"
            s.db.Model(&ch).Updates(map[string]any{
                "last_used_at": sentAt,
                "last_error":   "",
            })
        }

        s.db.Create(&log)
    }
}
```

### handlers.go — HTTP Handler 层

```go
package notification

// ListHandler 列出当前用户的通知渠道
// GET /api/notification-channels
// 认证：RequireAuth
func ListHandler(svc *NotificationService) gin.HandlerFunc

// CreateHandler 创建通知渠道
// POST /api/notification-channels
// 认证：RequireAuth
func CreateHandler(svc *NotificationService) gin.HandlerFunc

// GetHandler 获取渠道详情
// GET /api/notification-channels/:id
// 认证：RequireAuth
func GetHandler(svc *NotificationService) gin.HandlerFunc

// UpdateHandler 更新渠道配置
// PUT /api/notification-channels/:id
// 认证：RequireAuth
func UpdateHandler(svc *NotificationService) gin.HandlerFunc

// DeleteHandler 删除渠道（软删除）
// DELETE /api/notification-channels/:id
// 认证：RequireAuth
func DeleteHandler(svc *NotificationService) gin.HandlerFunc

// TestHandler 发送测试通知
// POST /api/notification-channels/:id/test
// 认证：RequireAuth
func TestHandler(svc *NotificationService) gin.HandlerFunc

// ListLogsHandler 查看通知发送记录
// GET /api/notification-channels/:id/logs
// 认证：RequireAuth
func ListLogsHandler(svc *NotificationService) gin.HandlerFunc

// ListWorkspaceChannelsHandler 列出工作空间的通知渠道
// GET /api/workspaces/:workspaceID/notification-channels
// 认证：RequireAuth
func ListWorkspaceChannelsHandler(svc *NotificationService) gin.HandlerFunc
```

### notification.go — 模块初始化

```go
package notification

type Module struct {
    Service *NotificationService
}

func New(db *gorm.DB) *Module {
    svc := NewNotificationService(db)
    // 注册所有内置发送器
    svc.RegisterSender(&senders.SlackSender{})
    svc.RegisterSender(&senders.DingTalkSender{})
    svc.RegisterSender(&senders.WeComSender{})
    svc.RegisterSender(&senders.FeishuSender{})
    svc.RegisterSender(&senders.WebhookSender{})
    return &Module{Service: svc}
}

func (m *Module) RegisterRoutes(apiGroup *gin.RouterGroup)
```

---

## 渠道发送器

### Slack（senders/slack.go）

```go
// 发送格式：Slack Block Kit
// 文档：https://api.slack.com/messaging/webhooks

type SlackSender struct{}

func (s *SlackSender) Send(config json.RawMessage, msg NotificationMessage) error {
    var cfg SlackConfig
    // 解析配置
    // 构建 Slack Block Kit payload
    payload := map[string]any{
        "blocks": []map[string]any{
            {"type": "header", "text": map[string]any{"type": "plain_text", "text": msg.Title}},
            {"type": "section", "text": map[string]any{"type": "mrkdwn", "text": msg.Body}},
        },
    }
    // POST to cfg.WebhookURL
}
```

### 钉钉（senders/dingtalk.go）

```go
// 发送格式：钉钉自定义机器人 Markdown 消息
// 文档：https://open.dingtalk.com/document/robots/custom-robot-access

type DingTalkSender struct{}

// 加签算法（当 cfg.Secret 非空时）：
// timestamp = 当前毫秒时间戳
// sign = base64(HMAC-SHA256("${timestamp}\n${secret}", secret))
// 请求 URL 追加 ?timestamp=xxx&sign=xxx
```

### 企业微信（senders/wecom.go）

```go
// 发送格式：企业微信群机器人 Markdown 消息
// 文档：https://developer.work.weixin.qq.com/document/path/91770

type WeComSender struct{}
```

### 飞书（senders/feishu.go）

```go
// 发送格式：飞书自定义机器人富文本消息
// 文档：https://open.feishu.cn/document/client-docs/bot-v3/add-custom-bot

type FeishuSender struct{}

// 签名算法（当 cfg.Secret 非空时）：
// timestamp = 当前秒级时间戳
// sign = base64(HMAC-SHA256("${timestamp}\n${secret}", secret))
```

### 通用 Webhook（senders/webhook.go）

```go
// 发送格式：JSON POST，支持自定义 Headers 和 HMAC-SHA256 签名

type WebhookSender struct{}

// 签名算法（当 cfg.Secret 非空时）：
// signature = hex(HMAC-SHA256(request_body, secret))
// 请求头追加：X-Webhook-Signature: sha256=<signature>
```

---

## API 设计

### 端点汇总

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| `GET` | `/api/notification-channels` | RequireAuth | 列出当前用户的通知渠道 |
| `POST` | `/api/notification-channels` | RequireAuth | 创建通知渠道 |
| `GET` | `/api/notification-channels/:id` | RequireAuth | 获取渠道详情 |
| `PUT` | `/api/notification-channels/:id` | RequireAuth | 更新渠道配置 |
| `DELETE` | `/api/notification-channels/:id` | RequireAuth | 删除渠道（软删除） |
| `POST` | `/api/notification-channels/:id/test` | RequireAuth | 发送测试通知 |
| `GET` | `/api/notification-channels/:id/logs` | RequireAuth | 查看通知发送记录 |
| `GET` | `/api/workspaces/:workspaceID/notification-channels` | RequireAuth | 列出工作空间的通知渠道 |

### 请求/响应格式

#### POST /api/notification-channels

```json
// Request
{
  "name": "工作群钉钉机器人",
  "channelType": "dingtalk",
  "workspaceId": "org-uuid-xxx",
  "enabled": true,
  "config": {
    "webhookUrl": "https://oapi.dingtalk.com/robot/send?access_token=xxx",
    "secret": "SEC..."
  },
  "triggerEvents": ["session.completed", "session.failed"]
}

// Response 201
{
  "channel": {
    "id": "uuid-xxx",
    "name": "工作群钉钉机器人",
    "channelType": "dingtalk",
    "userId": "casdoor-user-id",
    "workspaceId": "org-uuid-xxx",
    "enabled": true,
    "config": {
      "webhookUrl": "https://oapi.dingtalk.com/robot/send?access_token=xxx",
      "secret": "SEC..."
    },
    "triggerEvents": ["session.completed", "session.failed"],
    "createdAt": "2026-03-12T00:00:00Z",
    "updatedAt": "2026-03-12T00:00:00Z"
  }
}

// Response 400（config 校验失败）
{
  "error": "invalid config: webhookUrl is required"
}
```

#### PUT /api/notification-channels/:id

```json
// Request（字段均可选）
{
  "name": "工作群钉钉机器人（已更新）",
  "enabled": false,
  "triggerEvents": ["session.completed"]
}

// Response 200
{
  "channel": { ... }
}
```

#### POST /api/notification-channels/:id/test

```json
// Response 200
{
  "success": true,
  "message": "测试通知已发送"
}

// Response 500（发送失败）
{
  "success": false,
  "error": "webhook request failed: 400 Bad Request"
}
```

#### GET /api/notification-channels/:id/logs

```json
// Query: ?limit=20（默认 20，最大 100）

// Response 200
{
  "logs": [
    {
      "id": "log-uuid-xxx",
      "channelId": "channel-uuid-xxx",
      "eventType": "session.completed",
      "sessionId": "session-uuid-xxx",
      "deviceId": "device-uuid-xxx",
      "status": "success",
      "sentAt": "2026-03-12T10:00:00Z",
      "createdAt": "2026-03-12T10:00:00Z"
    },
    {
      "id": "log-uuid-yyy",
      "channelId": "channel-uuid-xxx",
      "eventType": "session.failed",
      "status": "failed",
      "error": "webhook request failed: 401 Unauthorized",
      "sentAt": "2026-03-12T09:00:00Z",
      "createdAt": "2026-03-12T09:00:00Z"
    }
  ]
}
```

#### GET /api/notification-channels

```json
// Response 200
{
  "channels": [
    {
      "id": "uuid-xxx",
      "name": "工作群钉钉机器人",
      "channelType": "dingtalk",
      "enabled": true,
      "triggerEvents": ["session.completed", "session.failed"],
      "lastUsedAt": "2026-03-12T10:00:00Z",
      "lastError": "",
      "createdAt": "2026-03-12T00:00:00Z"
    }
  ]
}
```

> **注意**：列表接口不返回 `config` 字段（含密钥），详情接口才返回完整 config。

---

## 与 SSE 模块集成

### EventRouter 集成点

在 `cloud/event_router.go` 中注入 `NotificationService`：

```go
// cloud/event_router.go

type EventRouter struct {
    manager         *ConnectionManager
    notificationSvc NotificationServiceInterface  // 接口，避免循环依赖
    batchQueue      map[string][]Event
    staleDeltas     map[string]struct{}
    mu              sync.Mutex
}

// NotificationServiceInterface 避免 cloud 包直接依赖 notification 包
type NotificationServiceInterface interface {
    TriggerNotifications(
        userID string,
        event Event,
        sessionID string,
        deviceID string,
        onlyWhenOffline bool,
        manager *ConnectionManager,
    )
}
```

在 `RouteDeviceEvent` 中触发通知：

```go
func (r *EventRouter) RouteDeviceEvent(deviceID string, event Event) {
    // ... 现有路由逻辑（查找订阅连接、加入 batchQueue）...

    // 触发通知（对特定事件类型）
    if r.notificationSvc != nil && isNotifiableEvent(event) {
        sessionID, _ := event.Properties["sessionID"].(string)
        ownerUserID := r.manager.GetDeviceOwnerUserID(deviceID)
        r.notificationSvc.TriggerNotifications(
            ownerUserID, event, sessionID, deviceID,
            true,  // onlyWhenOffline：用户在线时不重复通知
            r.manager,
        )
    }
}

func isNotifiableEvent(event Event) bool {
    switch event.Type {
    case EventSessionStatus:
        // 仅在 status 为 completed/failed/aborted 时触发
        status, _ := event.Properties["status"].(string)
        return status == "completed" || status == "failed" || status == "aborted"
    }
    return false
}
```

### ConnectionManager 新增方法

在 `cloud/connection_manager.go` 中新增：

```go
// GetDeviceOwnerUserID 通过 deviceID 查找设备归属用户 ID
// 用于通知服务确定通知目标用户
func (m *ConnectionManager) GetDeviceOwnerUserID(deviceID string) string {
    m.mu.RLock()
    defer m.mu.RUnlock()
    connID, ok := m.deviceConnections[deviceID]
    if !ok {
        return ""
    }
    conn, ok := m.connections[connID]
    if !ok {
        return ""
    }
    return conn.UserID
}

// FindUserConnsByUser 查找用户的所有活跃 SSE 连接
// 用于判断用户是否在线
func (m *ConnectionManager) FindUserConnsByUser(userID string) []string {
    m.mu.RLock()
    defer m.mu.RUnlock()
    connIDs, ok := m.userConnections[userID]
    if !ok {
        return nil
    }
    result := make([]string, 0, len(connIDs))
    for id := range connIDs {
        result = append(result, id)
    }
    return result
}
```

### 数据流

```
opencode CLI
    │  POST /cloud/event { type: "session.status", properties: { status: "completed", sessionID: "xxx" } }
    ▼
EventRouter.RouteDeviceEvent("device-uuid", event)
    │
    ├──────────────────────────────────────────────────────────────────────────────► batchQueue
    │                                                                                     │
    │                                                                              16ms 后批量推送
    │                                                                                     ▼
    │                                                                            Console App（SSE）
    │
    └──────────────────────────────────────────────────────────────────────────► go TriggerNotifications()
                                                                                          │
                                                                                    检查用户 SSE 连接
                                                                                          │
                                                                                    用户离线
                                                                                          │
                                                                                  查询 notification_channels
                                                                                  WHERE user_id = 'xxx'
                                                                                  AND enabled = true
                                                                                  AND 'session.completed' = ANY(trigger_events)
                                                                                          │
                                                                                  ┌───────┴───────┐
                                                                                DingTalk      Webhook
                                                                                  │              │
                                                                                HTTP POST    HTTP POST
                                                                                  │              │
                                                                                写入 notification_logs
```

---

## 错误处理

### HTTP 错误响应规范

| 场景 | HTTP 状态码 | 响应体 |
|------|------------|--------|
| 未携带 token | 401 | `{"error": "Authentication required"}` |
| 渠道不存在或不属于当前用户 | 404 | `{"error": "notification channel not found"}` |
| config 校验失败 | 400 | `{"error": "invalid config: <具体原因>"}` |
| 不支持的渠道类型 | 400 | `{"error": "unsupported channel type: xxx"}` |
| 请求体解析失败 | 400 | `{"error": "invalid request body"}` |
| 测试发送失败 | 500 | `{"success": false, "error": "<具体错误>"}` |
| 非工作空间成员 | 403 | `{"error": "not a member of this workspace"}` |

### 发送失败处理

发送器（`ChannelSender.Send()`）失败时：
- 写入 `NotificationLog`（status=failed，error=错误信息）
- 更新 `NotificationChannel.LastError`
- **不重试**（HTTP Webhook 为即时推送，失败即失败，用户可通过日志查看）
- 不影响其他渠道的发送（逐一独立发送）

---

## 双向扩展预留

本节描述 v1 中已埋入的扩展点，以及 v2 实现双向 Webhook 回调所需的完整路径。v1 不实现任何双向逻辑，但所有设计决策均已为 v2 留出空间。

### v1 已预留的扩展点

| 扩展点 | 位置 | v1 状态 | v2 用途 |
|--------|------|---------|---------|
| `inbound_enabled` 字段 | `NotificationChannel` 表 | 默认 false，忽略 | 控制是否接受该渠道的回调 |
| `inbound_config` 字段 | `NotificationChannel` 表 | 空，忽略 | 存储消息过滤规则、自动回复模板等 |
| `webhook_secret` 字段 | `NotificationChannel` 表 | 空，忽略 | 验证平台回调签名 |
| `webhook_token` 字段 | `NotificationChannel` 表 | 建表时自动生成 UUID | 构成回调 URL 的唯一路径段 |
| `ChannelReceiver` 接口 | `types.go` | 已定义，无实现 | 各渠道实现后即可接入 |
| `InboundMessage` 结构 | `types.go` | 已定义 | 标准化入站消息载体 |

### v2 回调 URL 设计

每个渠道创建时自动生成 `webhook_token`，对应的回调 URL 格式为：

```
POST /api/notification-channels/webhook/:webhookToken
```

用户在平台侧（Slack、钉钉等）配置此 URL 为 Outgoing Webhook 地址。服务端通过 `webhook_token` 定位到具体渠道记录，再调用对应的 `ChannelReceiver.VerifySignature()` 验证签名。

**各平台回调验签方式（v2 实现时参考）：**

| 渠道 | 验签方式 | 密钥来源 |
|------|---------|---------|
| Slack | `X-Slack-Signature: v0=HMAC-SHA256(body)` | Slack App 的 Signing Secret |
| 钉钉 | URL 参数 `timestamp` + `sign`，HMAC-SHA256 | 机器人加签密钥 |
| 企业微信 | 消息体 AES 加解密 + `msg_signature` | `EncodingAESKey` + `Token` |
| 飞书 | 请求头 `X-Lark-Signature`，HMAC-SHA256 | 飞书应用的 Verification Token |
| 通用 Webhook | `X-Webhook-Signature: sha256=HMAC-SHA256(body)` | 用户自定义 `secret` |

### v2 入站消息路由流程

```
平台（Slack/钉钉等）
    │  POST /api/notification-channels/webhook/:webhookToken
    ▼
WebhookReceiveHandler（v2 新增）
    │
    ├─ 1. 按 webhook_token 查询 NotificationChannel
    ├─ 2. 检查 inbound_enabled = true
    ├─ 3. 调用 ChannelReceiver.VerifySignature()（验签失败返回 401）
    ├─ 4. 调用 ChannelReceiver.ParseInbound() → InboundMessage
    ├─ 5. 查询 ChannelBinding（IM chatID ↔ sessionID 映射表，v2 新增）
    │
    ├─ 找到绑定 → cloud.EventRouter.RouteUserCommand(sessionID, event)
    │               └─ SSE 下发 session.message 给 opencode CLI
    │
    └─ 未找到绑定 → 忽略 或 回复"请先绑定会话"
```

### v2 需要新增的内容

在 v1 基础上，v2 仅需新增以下内容，**不需要修改现有表结构**（预留字段已在 v1 建表时创建）：

1. **`ChannelBinding` 表**（新增）：记录 IM chatID ↔ sessionID 的绑定关系

    ```go
    type ChannelBinding struct {
        ID            string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
        ChannelID     string    `gorm:"not null;index"`   // 关联 NotificationChannel
        PlatformChatID string   `gorm:"not null"`         // 平台侧会话 ID（如 Slack channel_id）
        SessionID     string    `gorm:"not null;index"`   // 关联 opencode 会话
        DeviceID      string    `gorm:"not null"`         // 关联设备
        CreatedAt     time.Time
        ExpiresAt     *time.Time                          // 绑定过期时间（可选）
    }
    ```

2. **`ChannelReceiver` 实现**：各渠道发送器扩展实现接收器接口（如 `SlackSender` → `SlackChannel`）

3. **`WebhookReceiveHandler`**：新增 `POST /api/notification-channels/webhook/:webhookToken` 端点

4. **`NotificationService.receivers`**：在现有 `senders` map 旁新增 `receivers map[string]ChannelReceiver`

---

## 实施计划

### 阶段一：基础渠道（P0）

优先实现使用最广泛的渠道：

1. **`models/models.go`** — 追加 `NotificationChannel`、`NotificationLog` 模型，更新 `AutoMigrate`
2. **`internal/notification/types.go`** — 定义 `ChannelSender` 接口和各渠道 Config 结构
3. **`internal/notification/senders/webhook.go`** — 通用 Webhook（最基础，可验证架构）
4. **`internal/notification/senders/slack.go`** — Slack（国际用户常用）
5. **`internal/notification/senders/dingtalk.go`** — 钉钉（国内用户常用）
6. **`internal/notification/service.go`** — `NotificationService`
7. **`internal/notification/handlers.go`** — 8 个 HTTP Handler
8. **`internal/notification/notification.go`** — 模块初始化
9. **`cmd/api/main.go`** — 注册路由（约 10 行）

### 阶段二：SSE 集成与完整渠道（P1）

10. **`cloud/connection_manager.go`** — 新增 `GetDeviceOwnerUserID`、`FindUserConnsByUser`
11. **`cloud/event_router.go`** — 注入 `NotificationServiceInterface`，在 `RouteDeviceEvent` 中触发通知
12. **`internal/notification/senders/wecom.go`** — 企业微信
13. **`internal/notification/senders/feishu.go`** — 飞书

### 阶段三：增强功能（P2）

- 通知模板自定义（用户可配置消息标题/内容模板）
- 工作空间级渠道共享（workspace 管理员配置，成员共用）
- Config 敏感字段加密存储（AES-256-GCM）
- 通知发送限流（同一渠道每分钟最多发送 N 条，防止事件风暴）
- 通知日志自动清理（保留最近 30 天）

### 阶段四：双向 Webhook 回调（v2，独立立项）

> 前提：v1 单向通知稳定上线，且有明确的双向需求。

1. **`ChannelBinding` 表**：新建，记录 IM chatID ↔ sessionID 绑定关系
2. **`ChannelReceiver` 实现**：优先实现 Slack 和钉钉（使用最广泛，验签机制相对简单）
3. **`WebhookReceiveHandler`**：新增 `POST /api/notification-channels/webhook/:webhookToken` 端点
4. **`NotificationService` 扩展**：注册 `receivers`，处理入站消息路由
5. **`cloud/event_router.go` 扩展**：`RouteUserCommand` 支持来自 IM 渠道的指令（与 Console App 指令同路径）
6. **激活预留字段**：`inbound_enabled`、`inbound_config`、`webhook_secret` 开始对外暴露和使用

**v2 不需要修改 `NotificationChannel` 表结构**，所有预留字段已在 v1 建表时创建。

### main.go 修改点（最小侵入）

```go
// 在现有路由注册之后追加

// Notification channel module
notificationModule := notification.New(database.GetDB())
notificationModule.RegisterRoutes(apiGroup)

// 工作空间通知渠道列表
apiGroup.GET("/workspaces/:workspaceID/notification-channels",
    notificationHandlers.ListWorkspaceChannelsHandler(notificationModule.Service))
```

### 需要新增的 go.mod 依赖

```
# 用于 PostgreSQL text[] 数组类型（pq.StringArray）
github.com/lib/pq v1.10.x
```

---

## 附录：各渠道消息格式示例

### Slack Block Kit

```json
{
  "blocks": [
    {
      "type": "header",
      "text": { "type": "plain_text", "text": "✅ 会话执行完成" }
    },
    {
      "type": "section",
      "text": {
        "type": "mrkdwn",
        "text": "*设备*: MacBook Pro - 工作\n*会话*: session-uuid-xxx\n*状态*: completed"
      }
    }
  ]
}
```

### 钉钉 Markdown

```json
{
  "msgtype": "markdown",
  "markdown": {
    "title": "会话执行完成",
    "text": "## ✅ 会话执行完成\n\n**设备**: MacBook Pro - 工作\n\n**会话**: session-uuid-xxx\n\n**状态**: completed"
  }
}
```

### 企业微信 Markdown

```json
{
  "msgtype": "markdown",
  "markdown": {
    "content": "## ✅ 会话执行完成\n> **设备**: MacBook Pro - 工作\n> **会话**: session-uuid-xxx\n> **状态**: completed"
  }
}
```

### 通用 Webhook

```json
{
  "title": "会话执行完成",
  "body": "设备 MacBook Pro - 工作 的会话 session-uuid-xxx 已完成",
  "eventType": "session.completed",
  "sessionId": "session-uuid-xxx",
  "deviceId": "device-uuid-xxx",
  "metadata": {
    "status": "completed"
  }
}
```

---

**文档版本：** 1.0.0
**创建日期：** 2026-03-12
**维护者：** CoStrict Team
