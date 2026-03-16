# 通知渠道模块技术提案

## 目录

- [概述](#概述)
- [架构设计](#架构设计)
- [数据模型](#数据模型)
- [模块设计](#模块设计)
- [API 设计](#api-设计)
- [触发集成](#触发集成)
- [错误处理](#错误处理)

---

## 概述

### 背景与动机

opencode CLI 执行任务时，用户可能不在 Console App 前关注进度。执行事件（任务完成、失败、需要确认）需要一种**主动触达**机制，确保用户能及时感知。

通知渠道模块是独立的主动推送模块，Server 内部直接实现各平台的通知发送逻辑，不依赖外部 Sender 服务。

### 设计原则

- **Server 内置实现**：各平台通知逻辑（企微、Webhook 等）直接在 Server 内部实现，通过统一的 `ChannelSender` 接口扩展
- **双层配置**：管理员配置系统渠道（开关 + 系统参数），用户在启用的渠道基础上填写自己的配置项
- **用户自主**：用户可独立配置 Webhook 渠道，无需管理员介入
- **可扩展**：新增平台只需实现 `ChannelSender` 接口并注册

---

## 架构设计

### 整体定位

```
opencode CLI
    │  POST /cloud/device/notify（上报执行事件）
    ▼
DeviceNotifyHandler
    │
    └──────────────────────────────────► NotificationService.TriggerNotifications()
                                                  │
                                     查询 UserNotificationChannel
                                     （user_id=userID, enabled=true, eventType 匹配）
                                                  │
                                     ┌────────────┴──────────────────┐
                                ChannelSender                  ChannelSender
                                （WeComSender）                （WebhookSender）
                                     │                              │
                                企微群机器人                   自定义 Webhook
```

### 目录结构

```
server/internal/notification/
├── notification.go          # 模块初始化、RegisterRoutes
├── types.go                 # 类型别名（复用 sender 包类型）
├── service.go               # NotificationService（触发、日志、渠道查询）
├── handlers.go              # Gin HTTP Handler
└── sender/
    ├── sender.go            # ChannelSender 接口 + 注册表
    ├── wecom.go             # 企微群机器人实现
    └── webhook.go           # 通用 Webhook 实现
```

---

## 数据模型

### SystemNotificationChannel 表

管理员配置的系统渠道，控制哪些渠道类型对用户可用。

```go
type SystemNotificationChannel struct {
    ID           string         // uuid PK
    Type         string         // "wecom" | "webhook" 等
    Name         string         // 显示名，如"企业微信"
    WorkspaceID  string         // 空=全局
    Enabled      bool           // 管理员开关
    SystemConfig datatypes.JSON // 系统级配置（各渠道不同，可为空）
    CreatedBy    string
    CreatedAt, UpdatedAt time.Time
    DeletedAt    gorm.DeletedAt
}
```

### UserNotificationChannel 表

用户在系统渠道基础上的个人配置。

```go
type UserNotificationChannel struct {
    ID              string         // uuid PK
    UserID          string         // 用户 ID
    SystemChannelID string         // 关联系统渠道（webhook 类型可为空）
    ChannelType     string         // "wecom" | "webhook"
    Name            string         // 用户自定义名称
    Enabled         bool
    UserConfig      datatypes.JSON // 用户配置（各渠道不同）
    TriggerEvents   pq.StringArray // text[]
    LastUsedAt      *time.Time
    LastError       string
    CreatedAt, UpdatedAt time.Time
    DeletedAt       gorm.DeletedAt
}
```

**用户配置示例（企微）：**
```json
{"webhookUrl": "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx"}
```

**用户配置示例（Webhook）：**
```json
{"url": "https://my-server.com/notify", "secret": "optional-secret"}
```

### NotificationLog 表

```go
type NotificationLog struct {
    ID            string     // uuid PK
    UserChannelID string     // 关联 UserNotificationChannel
    UserID        string
    ChannelType   string
    EventType     string
    SessionID     string
    DeviceID      string
    Status        string     // "success" | "failed"
    Error         string
    SentAt        *time.Time
    CreatedAt     time.Time
}
```

### UserConfig 表

通用用户配置 KV 存储，供其他模块使用。

```go
type UserConfig struct {
    ID        string         // uuid PK
    UserID    string         // UNIQUE(user_id, key)
    Key       string         // UNIQUE(user_id, key)
    Value     datatypes.JSON
    UpdatedAt time.Time
}
```

### 触发事件常量

| 值 | 触发时机 |
|----|---------|
| `session.completed` | 会话执行完成 |
| `session.failed` | 会话执行失败 |
| `session.aborted` | 会话被用户中止 |
| `device.offline` | 设备意外断线 |

---

## 模块设计

### ChannelSender 接口

```go
type ChannelSender interface {
    Type() string
    Send(userConfig json.RawMessage, msg NotificationMessage) error
    ValidateUserConfig(userConfig json.RawMessage) error
    UserConfigSchema() []ConfigField  // 供前端渲染配置表单
}

type ConfigField struct {
    Key         string `json:"key"`
    Label       string `json:"label"`
    Type        string `json:"type"`     // "text" | "password" | "url"
    Required    bool   `json:"required"`
    Placeholder string `json:"placeholder,omitempty"`
    HelpText    string `json:"helpText,omitempty"`
}
```

### 内置 Sender 实现

**WeComSender（企微群机器人）：**
- `UserConfig`: `{"webhookUrl": "..."}`
- 发送 Markdown 格式消息到企微群机器人 Webhook

**WebhookSender（通用 Webhook）：**
- `UserConfig`: `{"url": "...", "secret": "..."}`
- POST `NotificationMessage` JSON 到指定 URL
- 配置 secret 时附加 `X-Notification-Signature: sha256=<HMAC-SHA256(body, secret)>`

### NotificationService

```go
func (s *NotificationService) TriggerNotifications(userID, eventType, sessionID, deviceID string)
func (s *NotificationService) SendTest(userChannelID, userID string) error
func (s *NotificationService) ListLogs(userChannelID, userID string, limit int) ([]models.NotificationLog, error)
func (s *NotificationService) GetAvailableChannelTypes() []map[string]any
```

---

## API 设计

### 端点汇总

| 方法 | 路径 | 权限 | 说明 |
|------|------|------|------|
| `GET` | `/api/admin/notification-channels` | 管理员 | 列出所有系统渠道 |
| `POST` | `/api/admin/notification-channels` | 管理员 | 创建系统渠道 |
| `PUT` | `/api/admin/notification-channels/:id` | 管理员 | 更新系统渠道（含开关） |
| `DELETE` | `/api/admin/notification-channels/:id` | 管理员 | 删除系统渠道 |
| `GET` | `/api/notification-channels/available` | 用户 | 列出可用渠道类型（含配置 schema） |
| `GET` | `/api/notification-channels` | 用户 | 列出用户自己的渠道配置 |
| `POST` | `/api/notification-channels` | 用户 | 创建用户渠道配置 |
| `GET` | `/api/notification-channels/:id` | 用户 | 获取渠道详情 |
| `PUT` | `/api/notification-channels/:id` | 用户 | 更新渠道配置 |
| `DELETE` | `/api/notification-channels/:id` | 用户 | 删除渠道配置 |
| `POST` | `/api/notification-channels/:id/test` | 用户 | 发送测试通知 |
| `GET` | `/api/notification-channels/:id/logs` | 用户 | 查看通知发送记录 |

### 关键请求/响应

#### GET /api/notification-channels/available

```json
{
  "channelTypes": [
    {
      "systemChannelId": "uuid-xxx",
      "type": "wecom",
      "name": "企业微信",
      "schema": [
        {
          "key": "webhookUrl",
          "label": "企微群机器人 Webhook URL",
          "type": "url",
          "required": true,
          "placeholder": "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx"
        }
      ]
    },
    {
      "systemChannelId": "",
      "type": "webhook",
      "name": "自定义 Webhook",
      "schema": [...]
    }
  ]
}
```

#### POST /api/notification-channels

```json
// Request
{
  "systemChannelId": "uuid-xxx",
  "channelType": "wecom",
  "name": "我的企微通知",
  "userConfig": {
    "webhookUrl": "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx"
  },
  "triggerEvents": ["session.completed", "session.failed"]
}

// Response 201
{
  "channel": {
    "id": "uuid-yyy",
    "userId": "user-id",
    "channelType": "wecom",
    "name": "我的企微通知",
    "enabled": true,
    "userConfig": {"webhookUrl": "..."},
    "triggerEvents": ["session.completed", "session.failed"],
    "createdAt": "2026-03-16T00:00:00Z"
  }
}
```

---

## 触发集成

`POST /cloud/device/notify` 收到事件后：

```go
if notificationSvc != nil && isNotifiableEvent(body.Type) {
    notificationSvc.TriggerNotifications(device.UserID, body.Type, body.SessionID, device.DeviceID)
}

func isNotifiableEvent(eventType string) bool {
    switch eventType {
    case "session.completed", "session.failed", "session.aborted":
        return true
    }
    return false
}
```

---

## 错误处理

| 场景 | HTTP 状态码 | 响应体 |
|------|------------|--------|
| 未携带 token | 401 | `{"error": "Authentication required"}` |
| 渠道不存在或无权访问 | 404 | `{"error": "notification channel not found"}` |
| 不支持的渠道类型 | 400 | `{"error": "unsupported channel type: xxx"}` |
| 用户配置校验失败 | 400 | `{"error": "<具体错误>"}` |
| 测试发送失败 | 500 | `{"success": false, "error": "<具体错误>"}` |

发送失败时：
- 写入 `NotificationLog`（status=failed）
- 更新 `UserNotificationChannel.LastError`
- **不重试**，不影响其他渠道

---

## 扩展新渠道

1. 在 `server/internal/notification/sender/` 下新建文件（如 `feishu.go`）
2. 实现 `ChannelSender` 接口
3. 在 `NewNotificationService()` 中调用 `sender.Register(sender.NewFeishuSender())`
4. 管理员在后台创建对应 type 的 `SystemNotificationChannel` 记录

---

**文档版本：** 5.0.0
**创建日期：** 2026-03-12
**更新日期：** 2026-03-16
**维护者：** CoStrict Team
