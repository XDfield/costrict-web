# 通知渠道模块实施进度

基于 `docs/NOTIFICATION_CHANNEL_DESIGN.md` v4.0.0，任务跟踪。

---

## 一、数据模型（`server/internal/models/models.go`）

- [ ] 追加 `NotificationChannel` 模型
  ```go
  type NotificationChannel struct {
      ID            string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
      Name          string         `gorm:"not null"                                       json:"name"`
      Scope         string         `gorm:"not null;index;default:'personal'"               json:"scope"`
      CreatorID     string         `gorm:"not null;index"                                 json:"creatorId"`
      WorkspaceID   string         `gorm:"index"                                          json:"workspaceId"`
      Enabled       bool           `gorm:"not null;default:true"                          json:"enabled"`
      Config        datatypes.JSON `gorm:"not null"                                       json:"config"`
      TriggerEvents pq.StringArray `gorm:"type:text[]"                                    json:"triggerEvents,omitempty"`
      LastUsedAt    *time.Time     `                                                      json:"lastUsedAt,omitempty"`
      LastError     string         `                                                      json:"lastError,omitempty"`
      CreatedAt     time.Time      `                                                      json:"createdAt"`
      UpdatedAt     time.Time      `                                                      json:"updatedAt"`
      DeletedAt     gorm.DeletedAt `gorm:"index"                                          json:"-"`
  }
  ```
- [ ] 追加 `UserConfig` 模型
  ```go
  type UserConfig struct {
      ID        string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
      UserID    string         `gorm:"not null;uniqueIndex:idx_user_config_key"       json:"userId"`
      Key       string         `gorm:"not null;uniqueIndex:idx_user_config_key"       json:"key"`
      Value     datatypes.JSON `gorm:"not null"                                       json:"value"`
      UpdatedAt time.Time      `                                                      json:"updatedAt"`
  }
  ```
- [ ] 追加 `NotificationLog` 模型
  ```go
  type NotificationLog struct {
      ID        string     `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
      ChannelID string     `gorm:"not null;index"                                 json:"channelId"`
      UserID    string     `gorm:"not null;index"                                 json:"userId"`
      EventType string     `gorm:"not null"                                       json:"eventType"`
      SessionID string     `gorm:"index"                                          json:"sessionId,omitempty"`
      DeviceID  string     `gorm:"index"                                          json:"deviceId,omitempty"`
      Status    string     `gorm:"not null"                                       json:"status"`
      Error     string     `                                                      json:"error,omitempty"`
      SentAt    *time.Time `                                                      json:"sentAt,omitempty"`
      CreatedAt time.Time  `                                                      json:"createdAt"`
  }
  ```
- [ ] `AutoMigrate` 追加三个新模型
- [ ] `go.mod` 新增 `github.com/lib/pq v1.10.x`（pq.StringArray）

---

## 二、通知模块（`server/internal/notification/`）

### 2.1 types.go

- [ ] `SenderConfig` 结构体（`URL`、`Secret`）
- [ ] `NotificationMessage` 结构体（`Title`、`Body`、`EventType`、`SessionID`、`DeviceID`、`Metadata`）
- [ ] `NotificationSubscriptionItem` 结构体（`ChannelID`、`Enabled`、`TriggerEvents`）
- [ ] `NotificationSubscriptionsConfig` 结构体（`Subscriptions []NotificationSubscriptionItem`）

### 2.2 service.go

- [ ] `NotificationService` 结构体（`db`、`httpClient`）
- [ ] `NewNotificationService(db *gorm.DB) *NotificationService`
- [ ] `TriggerNotifications(userID, eventType, sessionID, deviceID string)` — goroutine 异步
- [ ] `send()` — 查询 personal 渠道 + 读取 UserConfig 订阅，逐一发送
- [ ] `postToSender()` — HTTP POST 统一格式，可选 HMAC-SHA256 签名
- [ ] `SendTest(channelID, userID string) error`
- [ ] `ListLogs(channelID, userID string, limit int)`
- [ ] `buildMessage(eventType, sessionID, deviceID string) NotificationMessage`

### 2.3 handlers.go

**Personal 渠道管理：**
- [ ] `GET /api/notification-channels` — 列出当前用户 personal 渠道
- [ ] `POST /api/notification-channels` — 创建 personal 渠道
- [ ] `GET /api/notification-channels/:id` — 获取渠道详情（含 config）
- [ ] `PUT /api/notification-channels/:id` — 更新 personal 渠道
- [ ] `DELETE /api/notification-channels/:id` — 删除 personal 渠道（软删除）
- [ ] `POST /api/notification-channels/:id/test` — 发送测试通知
- [ ] `GET /api/notification-channels/:id/logs` — 查看发送记录（?limit=20）

**Global 渠道管理（管理员）：**
- [ ] `GET /api/workspaces/:workspaceID/notification-channels` — 列出 global 渠道
- [ ] `POST /api/workspaces/:workspaceID/notification-channels` — 创建 global 渠道
- [ ] `PUT /api/workspaces/:workspaceID/notification-channels/:id` — 更新 global 渠道
- [ ] `DELETE /api/workspaces/:workspaceID/notification-channels/:id` — 删除 global 渠道

**用户订阅管理（读写 UserConfig）：**
- [ ] `GET /api/workspaces/:workspaceID/notification-channels/available` — 可订阅渠道列表（含订阅状态）
- [ ] `GET /api/notification-channels/subscriptions` — 获取当前订阅配置
- [ ] `PUT /api/notification-channels/subscriptions` — 保存订阅配置（整体覆盖）

### 2.4 notification.go

- [ ] `Module` 结构体 + `New(db *gorm.DB) *Module`
- [ ] `RegisterRoutes(apiGroup *gin.RouterGroup)` — 注册所有端点

---

## 三、路由注册（`server/cmd/api/main.go`）

- [ ] 初始化 `notificationModule := notification.New(database.GetDB())`
- [ ] 调用 `notificationModule.RegisterRoutes(apiGroup)`

---

## 四、触发集成（`server/internal/cloud/handlers.go`）

当前 `DeviceNotifyHandler` 只做 SSE 推送，需在此处补充通知触发：

- [ ] `DeviceNotifyHandler` 注入 `*notification.NotificationService`
- [ ] 在 SSE 路由完成后调用 `notificationSvc.TriggerNotifications(device.UserID, eventType, sessionID, deviceID)`
- [ ] 扩展事件类型映射（当前只有 `EventInterventionRequired`，补充 `session.completed`、`session.failed`、`session.aborted`）

---

## 五、企微 Sender 服务（`sender/wecom/`）

- [ ] `sender/wecom/main.go` — HTTP 服务入口，读取环境变量（`WECOM_WEBHOOK_URL`、`LISTEN_ADDR`、`NOTIFICATION_SECRET`）
- [ ] `sender/wecom/handler.go` — 接收 `NotificationMessage`，可选验签，转换为企微 Markdown 发送
- [ ] `sender/wecom/Dockerfile` — 容器化支持
- [ ] `sender/README.md` — Sender 服务规范说明 + 自定义实现指南

---

## 进度概览

| 阶段 | 内容 | 状态 |
|------|------|------|
| 一 | 数据模型 | 未开始 |
| 二 | 通知模块（server） | 未开始 |
| 三 | 路由注册 | 未开始 |
| 四 | 触发集成 | 未开始 |
| 五 | 企微 Sender 服务 | 未开始 |
