# 系统通知交互闭环实施进度

> **已废弃**：企微模板卡片（template_card）交互链路已于 2026-07-15 移除，
> 项目现在只保留纯文本/markdown AI 回复链路。本进度文档保留作为历史记录，
> 不再驱动后续开发。设计文档 `docs/proposals/SYSTEM_NOTIFICATION_INTERACTIVE.md`
> 同样仅作历史参考。

基于 `docs/proposals/SYSTEM_NOTIFICATION_INTERACTIVE.md` v3.0.0，任务跟踪。

> **实现状态：🔧 实施中**
> Phase 1 Step 1 部分完成，Step 2 待开始。

---

## Phase 1: 基础设施 + 主流程（server）

### Step 1: Store（notification/store.go）

- [x] `Store` 结构体 + `NewStore()`
- [x] `CreateNotificationInput` 结构体（UserID / Type / Title / Content / SessionID / DeviceID / ActionType / ActionData / CardData）
- [x] `Create()` — 创建通知记录 + 生成 action_token
- [x] `ExecuteAction()` — 原子消费 token，更新 status='acted'
- [x] `MarkRespondedBySession()` — web 端响应标记（按 sessionID 批量更新）
- [x] `GetByToken()` — 按 token 查询
- [x] `GetPendingByUser()` — 查询用户待处理通知
- [x] `MarkRead()` — 标记已读
- [x] `generateActionToken()` — 64 hex 字符
- [ ] `SweepStaleNotifications()` — 扫描超过 120s 的 pending 记录（worker 兜底用）
- [ ] `MarkExpired()` — 批量过期清理
- [ ] `StartSweep()` — 定时清理入口（合并过期清理 + 缓冲兜底）
- [ ] `List()` — 分页查询
- [ ] `CountUnread()` — 未读计数
- [ ] `GetByID()` — 按 ID 查询
- [ ] 单元测试

### Step 2: Dispatcher（dispatcher/dispatcher.go + selector.go）

- [ ] `DispatchInput` 结构体（UserID / WorkspaceID / EventType / SessionID / DeviceID / Path / SessionURL / ActionData）
- [ ] `SelectedChannels` 结构体（Interactive / OneWay）
- [ ] `ChannelSelector.Select()` — 查询 Channel（交互）+ UserNotificationChannel（单向）
- [ ] `Dispatcher` 结构体（db / notificationSvc / store / cloudBaseURL / bufferPeriod / pendingMap）
- [ ] `NewDispatcher()` — 构造函数
- [ ] `Dispatch()` — 主入口：渠道查询 → 交互事件缓冲 → 非交互事件立即分发
- [ ] `needsInteraction()` — 判断是否交互事件
- [ ] `analyzeQuestionComplexity()` — 复杂度判断（simple_approval / simple_single_select / multiple_questions / complex_with_multiselect / complex）
- [ ] 缓冲机制：
  - [ ] `bufferDispatch()` — 60s `time.AfterFunc`
  - [ ] `handleBufferTimeout()` — 超时后查 DB 判断是否已处理
  - [ ] `isInterventionHandled()` — 查询 status='acted'
  - [ ] `OnInterventionResponse()` — 外部调用取消计时器
  - [ ] `cancelBufferedNotification()` — 停止 timer + 清理 pendingMap
  - [ ] `dispatchStaleNotification()` — worker 兜底调用
- [ ] 分发路由：
  - [ ] `dispatchInteractive()` — 交互渠道优先
  - [ ] `dispatchNotification()` — 单向通知降级
  - [ ] `doDispatch()` — 统一分发执行
- [ ] 各场景发送方法（骨架，Phase 3 填充）：
  - [ ] `sendWeComInteractive()` — 复杂度路由
  - [ ] `sendApprovalCard()`
  - [ ] `sendSingleSelectCard()`
  - [ ] `sendMultipleQuestionCards()`
  - [ ] `sendGuidanceCard()`
- [ ] 单元测试

### Step 3: Server 端接入

**cloud/handlers.go：**
- [ ] `DeviceNotifyHandler` 改造 — 调用 `Dispatcher.Dispatch()`
- [ ] `NotifyRespondedHandler` 新增 — POST `/cloud/device/notify/responded`
- [ ] `buildSessionURL()` — 构建移动端会话地址

**cloud/cloud.go：**
- [ ] `Module` 增加 Dispatcher 依赖
- [ ] 注册新路由 `/cloud/device/notify/responded`

**cloud/types.go：**
- [ ] `EventInterventionResponse` 事件类型

**cmd/api/main.go：**
- [ ] 初始化 notification.Store
- [ ] 初始化 Dispatcher
- [ ] 构造 ActionHandler 闭包（ExecuteAction → OnInterventionResponse → RouteUserCommand）
- [ ] 注入到 Cloud Module

**验证：**
- [ ] curl 模拟 POST /cloud/device/notify → DB 写入 pending 记录
- [ ] curl 模拟 POST /cloud/device/notify/responded → status 更新为 acted
- [ ] 确认 60s 后日志输出 "buffer timeout"

---

## Phase 2: 事件源接入（cs-cloud）

### Step 4: NotifyForwarder

- [ ] `NotifyForwarder` 结构体（eventBus / cloudClient / deviceID / deviceToken / localMode）
- [ ] `Start(ctx)` — 订阅 EventBus
- [ ] `handleEvent()` — 事件分发：
  - [ ] `permission.asked` → POST /cloud/device/notify
  - [ ] `question.asked` → POST /cloud/device/notify
  - [ ] `session.idle` → POST /cloud/device/notify
  - [ ] `permission.responded` → POST /cloud/device/notify/responded
  - [ ] `question.responded` → POST /cloud/device/notify/responded
- [ ] `isResponseEvent()` — 识别响应事件
- [ ] `handleResponseEvent()` — 调用 server 标记 API
- [ ] `buildPermissionData()` / `buildQuestionData()` — 构造事件数据
- [ ] 初始化集成（入口文件启动 NotifyForwarder）

**验证：**
- [ ] cs-cloud 启动，触发权限请求 → server 收到 POST
- [ ] DB 写入 system_notifications 记录
- [ ] web 端响应后 status 更新为 acted

---

## Phase 3: 企微交互卡片

### Step 5: WeComAdapter 交互卡片

**channel/adapters/wecom/types.go：**
- [ ] `InteractiveCard` / `CardButton` 结构体
- [ ] `WeComTemplateCard` / `WeComTitle` / `WeComCardButton` 结构体
- [ ] `WeComSendRequest` 扩展 `TemplateCard` 字段

**channel/adapters/wecom/adapter.go：**
- [ ] `SendInteractiveCard()` — 构造 template_card JSON → 企微消息发送 API
- [ ] `UpdateCardStatus()` — 通过 response_code 更新卡片为已处理

**channel/adapters/wecom/verify.go：**
- [ ] `ParseInbound` 增强 — 解析 `template_card_event` 回调 XML
- [ ] `parseEventKey()` — 解析 EventKey（approve/reject/select:N/navigate）
- [ ] `WeComCallbackMessage` 扩展 TaskId / ResponseCode / EventKey 字段

**channel/service.go：**
- [ ] `SendInteractiveCard()` 方法
- [ ] `SetActionHandler()` — 注册 ActionHandler 回调
- [ ] `HandleWebhook` 增加 `action_callback` 分支

**channel/types.go：**
- [ ] `OutboundMessage` 扩展

**dispatcher/dispatcher.go — 填充各场景发送方法：**
- [ ] `sendApprovalCard()` — 批准/拒绝卡片
- [ ] `sendSingleSelectCard()` — 单选选项按钮
- [ ] `sendMultipleQuestionCards()` — 多问题多卡片
- [ ] `sendGuidanceCard()` — 复杂场景引导

**验证：**
- [ ] 权限请求 → 企微收到批准/拒绝卡片 → 点击 → Device 收到响应
- [ ] 单选问卷 → 企微收到选项按钮 → 点击 → Device 收到响应
- [ ] 多问题 → 多张卡片 → 逐个回调
- [ ] 多选/复杂 → 引导卡片 + 会话 URL

---

## Phase 4: Worker 定时清理

### Step 6: Worker 集成

**cmd/worker/main.go：**
- [x] 引入 dispatcher / notification 包
- [x] 初始化 notification.Store
- [x] 初始化 Dispatcher（`dispatcher.NewDispatcher(db, notificationSvc, notificationStore, cfg.AppURL, nil)`）
- [x] 启动 `go notificationStore.StartSweep(ctx, disp)`
- [x] 日志输出 "Notification sweep started"
- [ ] `StartSweep` 方法实现（store.go 中待添加）
- [ ] `SweepStaleNotifications` 方法实现
- [ ] `MarkExpired` 方法实现

**验证：**
- [ ] 模拟 server 重启，pending 记录超过 120s → worker 补发 IM
- [ ] pending 记录超过 30 分钟 → MarkExpired 标记为 expired

---

## Phase 5: 端到端验证

### Step 7: 全链路验证

- [ ] 7a. 权限审批：事件 → 缓冲 60s → 企微卡片 → 批准/拒绝 → Device
- [ ] 7b. 单选问卷：事件 → 缓冲 60s → 选项按钮 → 选择 → Device
- [ ] 7c. 多问题：事件 → 多张卡片 → 逐个回调 → Device
- [ ] 7d. 多选/复杂：事件 → 引导卡片 + 会话 URL
- [ ] 7e. Web 端响应：用户 web 端操作 → cs-cloud 标记 → 缓冲跳过 IM
- [ ] 7f. IM 卡片响应：点击企微卡片 → 取消计时器 → Device
- [ ] 7g. Worker 兜底：模拟 server 重启 → 120s 后补发 IM
- [ ] 7h. Token 过期：超过 30 分钟 → MarkExpired → ExecuteAction 拒绝

---

## 数据模型

- [x] 迁移文件 `server/migrations/20260526000000_create_system_notifications.sql`
- [x] `SystemNotification` 模型（models/models.go）
