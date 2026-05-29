# 通知渠道模块实施进度

基于 `docs/NOTIFICATION_CHANNEL_DESIGN.md` v4.0.0，任务跟踪。

> **实现状态：✅ 已完成**
> 数据模型、通知服务、Sender 实现、API 路由、触发集成均已实现。

---

## 一、数据模型（`server/internal/models/models.go`）

- [x] `SystemNotificationChannel` — 系统级通知渠道（管理员管理）
- [x] `UserNotificationChannel` — 用户通知渠道配置（含 ChannelType、UserConfig、TriggerEvents）
- [x] `UserConfig` — 通用用户 KV 配置存储
- [x] `NotificationLog` — 通知发送记录（含状态、错误信息、发送时间）
- [x] `AutoMigrate` 已追加所有新模型
- [x] `github.com/lib/pq` 依赖已引入（pq.StringArray）
- [x] Migration 种子数据（`server/migrations/20260326000000_seed_system_notification_channels.sql`）

---

## 二、通知模块（`server/internal/notification/`）

### 2.1 types.go — 事件常量与消息类型

- [x] 12 种触发事件常量（session/permission/question/idle/project-invitation/item/system）
- [x] `ProjectInvitationMessage` 结构体
- [x] `SystemNotificationMessage` 结构体

### 2.2 sender/ — 发送器接口与实现

**sender.go：**
- [x] `NotificationMessage` 结构体（Title/Body/EventType/SessionID/DeviceID/Metadata）
- [x] `ChannelSender` 接口（Type/Send/ValidateUserConfig/UserConfigSchema）
- [x] `ConfigField` 结构体（前端表单渲染用）
- [x] 发送器注册表（Register/Get/All）

**sender/wecom.go：**
- [x] `WeComSender` — 企微群机器人 Webhook 发送
- [x] Markdown 格式消息（含事件图标 + 详情链接）
- [x] 配置校验（webhookUrl 必填）
- [x] 配置 Schema（前端表单描述）

**sender/webhook.go：**
- [x] `WebhookSender` — 通用 Webhook 发送
- [x] 可选 HMAC-SHA256 签名（X-Notification-Signature 请求头）
- [x] 配置校验 + Schema

### 2.3 service.go — NotificationService

- [x] `NewNotificationService(db, cloudBaseURL)` — 初始化并注册 Sender
- [x] `TriggerNotifications()` — 异步触发（goroutine），查询匹配渠道并发送
- [x] `TriggerMessage()` — 直接发送自定义消息
- [x] `send()` — 查询用户已启用渠道 → 遍历发送 → 记录日志（成功/失败）
- [x] `SendTest()` — 测试发送
- [x] `ListLogs()` — 查询发送日志
- [x] `GetAvailableChannelTypes()` — 列出可用渠道类型（含 Schema）
- [x] `GetSupportedTriggerEvents()` / `IsSupportedTriggerEvent()`
- [x] `buildMessage()` — 构建通知消息（含会话详情 URL）
- [x] `getWorkspaceID()` — 通过 deviceID + path 查询 workspaceID

### 2.4 handlers.go — Gin HTTP Handler

**管理员端（需平台管理员权限）：**
- [x] `GET /admin/notification-channels` — 列出系统渠道
- [x] `POST /admin/notification-channels` — 创建系统渠道
- [x] `PUT /admin/notification-channels/:id` — 更新系统渠道
- [x] `DELETE /admin/notification-channels/:id` — 删除系统渠道

**用户端：**
- [x] `GET /notification-channels/available` — 可用渠道类型 + 支持的事件列表
- [x] `GET /notification-channels` — 列出用户自己的渠道
- [x] `POST /notification-channels` — 创建用户渠道（含事件校验）
- [x] `GET /notification-channels/:id` — 获取渠道详情
- [x] `PUT /notification-channels/:id` — 更新渠道
- [x] `DELETE /notification-channels/:id` — 删除渠道（软删除）
- [x] `POST /notification-channels/:id/test` — 测试发送
- [x] `GET /notification-channels/:id/logs` — 查看发送日志

### 2.5 notification.go — Module 定义

- [x] `Module` 结构体 + `New(db, cloudBaseURL)`
- [x] `RegisterRoutes(apiGroup)` — 注册所有端点（管理员 + 用户）

---

## 三、路由注册（`server/cmd/api/main.go`）

- [x] `notificationModule := notification.New(db, cfg.CloudBaseURL)`
- [x] `notificationModule.RegisterRoutes(authed)` — 挂载到认证路由组

---

## 四、触发集成

### 4.1 Cloud 设备通知（`server/internal/cloud/handlers.go`）

- [x] `DeviceNotifyHandler` 注入 `*notification.NotificationService`
- [x] SSE 推送后调用 `notificationSvc.TriggerNotifications()`
- [x] `isNotifiableEvent()` 事件过滤

### 4.2 项目邀请通知（`server/internal/project/service.go`）

- [x] `ProjectService` 注入 `notificationSvc`
- [x] 邀请创建时发送 `project.invitation.created` + `system.notification` 通知

### 4.3 技能下发通知（`server/internal/services/distribution_service.go`）

- [x] `DistributionService` 注入 `notificationSvc`（通过 `SetNotificationService()`）
- [x] 技能下发时发送 `item.distributed` 通知
- [x] 技能收回/暂停时发送 `item.revoked` / `item.paused` 通知

---

## 五、Sender 服务规范

> 注：最终实现采用内嵌 Sender 模式（直接在 server 进程内注册发送器），
> 而非原设计文档中的独立微服务。Webhook Sender 支持外部自定义服务对接。

---

## 进度概览

| 阶段 | 内容 | 状态 |
|------|------|------|
| 一 | 数据模型（4 张表） | ✅ 已完成 |
| 二 | 通知模块（service + handlers + sender） | ✅ 已完成 |
| 三 | 路由注册 | ✅ 已完成 |
| 四 | 触发集成（cloud + project + distribution） | ✅ 已完成 |
| 五 | Sender 实现（wecom + webhook） | ✅ 已完成 |
