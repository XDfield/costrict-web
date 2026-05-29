> **实现状态：🔧 实施中**
>
> - 状态：🔧 Phase 1 实施中
> - 关联提案：`NOTIFICATION_CHANNEL_DESIGN.md`（已完成）
> - 涉及模块：`notification`、`channel`、`cloud`、`dispatcher`、`cs-cloud`
> - 进度跟踪：`todo/SYSTEM_NOTIFICATION_INTERACTIVE_PROGRESS.md`

---

# 系统通知交互闭环技术提案

## 目录

- [概述](#概述)
- [架构设计](#架构设计)
- [数据模型](#数据模型)
- [模块设计](#模块设计)
- [统一分发层](#统一分发层)
- [问卷复杂度判断](#问卷复杂度判断)
- [企微交互卡片](#企微交互卡片)
- [操作回传链路](#操作回传链路)
- [错误处理](#错误处理)
- [文件变更清单](#文件变更清单)
- [实施顺序](#实施顺序)

---

## 概述

### 背景与动机

现有通知渠道模块（`NOTIFICATION_CHANNEL_DESIGN.md`）实现了从 Device 事件到外部渠道的**单向推送**。用户收到通知后，仍需回到 CoStrict App 内完成操作（权限审批、问卷回答等）。

本提案在此基础上增加**交互闭环能力**：

1. **统一分发**：根据用户配置的渠道类型和事件类型，自动选择合适的分发渠道
2. **交互卡片**：用户在企微卡片上直接点击按钮完成审批/单选问卷回答，无需跳转
3. **智能降级**：多选/复杂问卷场景引导到移动端会话页面，卡片中携带移动端会话 URL

### 设计原则

- **统一分发层**：所有通知分发通过 `Dispatcher`，不分散在 Handler 中
- **职责分离**：CSC（AI agent）仅通过 SSE 上报标准事件，不感知 cloud server；cs-cloud 全权负责事件识别与 server 对接
- **基于用户配置**：查询用户启用的 Channel 和 UserNotificationChannel，自动选择
- **渠道能力匹配**：根据事件类型需求（交互/单向）选择匹配的渠道
- **优先级降级**：交互渠道优先，无交互渠道时降级到单向通知
- **复杂度自适应**：根据问卷复杂度自动选择企微卡片按钮或引导到移动端
- **始终携带会话 URL**：所有消息中携带移动端会话访问地址，作为兜底入口
- **无前端路由暴露**：通知记录仅内部使用，不暴露查询/操作 API
- **DB 持久化 Token**：操作令牌持久化到 `system_notifications` 表

---

## 架构设计

### 事件上报链路

**职责划分**：

| 层级 | 仓库 | 职责 |
|------|------|------|
| AI Agent | `csc` | 通过 SSE 上报标准事件（`permission.asked`、`question.asked`、`session.idle`），不感知 cloud server |
| 桥接层 | `cs-cloud` | 监听 CSC SSE 事件，识别事件类型，判断运行模式（local/cloud），在非 local 模式下转发到 server |
| 后端 | `server`（本提案） | 接收通知请求，Dispatcher 分发到渠道，管理 token 和回调 |

**事件流**：

```
CSC Agent
    │  SSE 标准事件
    │  permission.asked / question.asked / session.idle
    ▼
cs-cloud（桥接层）
    │
    │  事件识别 + 模式判断
    │
    ├─ local 模式 → 不上报，仅本地处理
    │
    └─ cloud 模式 → POST /cloud/device/notify
         │  { type, sessionID, path, data: {...} }
         │  Authorization: Bearer {cloudAPIToken}
         ▼
    costrict-web Server
```

**cs-cloud 已有统一事件架构**：

cs-cloud 内部已有 `EventBus`（`runtime/eventbus.go`）作为统一事件总线，与底层 AI agent runtime 无关：

```
Agent（任意 runtime: cs / csc）
    │  emit(agent.Event{Type, Data})
    ▼
EventBus（runtime/eventbus.go）
    │  发布/订阅模式，支持 filter
    │
    ├─ Subscribe → localServer SSE（现有，转发给浏览器客户端）
    ├─ Subscribe → fileWatcher / gitWatcher（现有）
    └─ Subscribe → cloud notify forwarder（新增，本提案）
```

- **`Agent.SetEventEmitter()`**：每个 agent 实例启动时注册 emitter，所有事件统一发到 EventBus
- **`EventBus`**：发布/订阅模式，支持 Backend filter，所有 runtime 事件汇聚到此处
- **`agent.Event`**：标准化事件结构 `{Type, ConversationID, MessageID, Backend, Data}`

**cs-cloud 需新增的变更**：

仅需在 EventBus 上新增一个订阅者 `cloud notify forwarder`，无需修改任何 agent runtime 代码：

```go
// 新增文件：internal/cloud/notify_forwarder.go
type NotifyForwarder struct {
    eventBus   *runtime.EventBus
    cloudClient *cloud.Client
    deviceID   string
    deviceToken string
    localMode  bool
}

func (f *NotifyForwarder) Start(ctx context.Context) {
    ch := f.eventBus.Subscribe(nil)
    go func() {
        for {
            select {
            case <-ctx.Done():
                f.eventBus.Unsubscribe(ch)
                return
            case event, ok := <-ch:
                if !ok {
                    return
                }
                f.handleEvent(event)
            }
        }
    }()
}

func (f *NotifyForwarder) handleEvent(event agent.Event) {
    if f.localMode {
        return  // local 模式不上报
    }

    // 识别响应事件 → 标记 system_notifications 已处理
    if isResponseEvent(event.Type) {
        f.handleResponseEvent(event)
        return
    }

    // 识别可通知事件
    var notifyType string
    var notifyData any

    switch event.Type {
    case "permission.asked":
        notifyType = "permission"
        notifyData = buildPermissionData(event.Data)
    case "question.asked":
        notifyType = "question"
        notifyData = buildQuestionData(event.Data)
    case "session.idle":
        notifyType = "idle"
        notifyData = map[string]any{"timestamp": time.Now().UnixMilli()}
    default:
        return  // 其他事件不转发
    }

    // 调用 server 的 /cloud/device/notify
    payload := map[string]any{
        "deviceID":  f.deviceID,
        "type":      notifyType,
        "sessionID": event.ConversationID,
        "path":      extractPath(event),
        "data":      notifyData,
    }

    body, _ := json.Marshal(payload)
    req, _ := http.NewRequest("POST", f.cloudClient.URL("/cloud/device/notify", ""), bytes.NewReader(body))
    f.cloudClient.SetDeviceAuthHeaders(req, f.deviceToken)
    resp, err := f.cloudClient.HTTPClient().Do(req)
    // ... 错误处理 ...
}

// 响应事件处理：标记 system_notifications 已处理
func isResponseEvent(eventType string) bool {
    return eventType == "permission.responded" ||
        eventType == "question.responded"
}

func (f *NotifyForwarder) handleResponseEvent(event agent.Event) {
    sessionID := event.ConversationID

    // 调用 server API 标记该 session 的通知为已处理
    payload := map[string]any{
        "sessionID": sessionID,
        "type":      strings.Split(event.Type, ".")[0], // "permission" or "question"
    }

    body, _ := json.Marshal(payload)
    req, _ := http.NewRequest("POST", f.cloudClient.URL("/cloud/device/notify/responded", ""), bytes.NewReader(body))
    f.cloudClient.SetDeviceAuthHeaders(req, f.deviceToken)
    resp, err := f.cloudClient.HTTPClient().Do(req)
    // ... 错误处理（失败仅日志，不影响主流程）...
    _ = resp
    _ = err
}
```

**优势**：
- 与底层 AI agent runtime 完全解耦，cs 和 csc 的事件都能被捕获
- 仅需新增一个 EventBus 订阅者，不修改现有 agent 代码
- local 模式自动跳过，不影响本地开发体验

**cs-cloud 转发时构造的请求体格式**：

```json
{
  "deviceID": "device-123",
  "type": "permission",
  "sessionID": "session-abc",
  "path": "/path/to/workspace",
  "data": {
    "permissionID": "...",
    "permissionType": "bash.execute",
    "patterns": ["rm -rf /tmp/*"],
    "always": [],
    "tool": { "messageID": "...", "callID": "..." },
    "metadata": {}
  }
}
```

```json
{
  "deviceID": "device-123",
  "type": "question",
  "sessionID": "session-abc",
  "path": "/path/to/workspace",
  "data": {
    "requestID": "...",
    "questions": [
      {
        "question": "请选择部署环境",
        "header": "部署环境",
        "options": [
          { "label": "开发环境", "description": "..." },
          { "label": "生产环境", "description": "..." }
        ],
        "multiple": false,
        "custom": true
      }
    ],
    "tool": { "messageID": "...", "callID": "..." }
  }
}
```

### Server 端处理流程

```
POST /cloud/device/notify          POST /cloud/device/notify/responded
    │  （cs-cloud 转发事件）            │  （cs-cloud 识别响应事件后调用）
    ▼                                   ▼
DeviceNotifyHandler               NotifyRespondedHandler
    │                                   │
    ├────► SSE 实时推送                   └─ UPDATE system_notifications
    │                                         SET status='acted', acted_at=NOW()
    └─ Dispatcher.Dispatch()                      WHERE session_id=? AND status='pending'
         │
         ├─ 交互事件 → 缓冲 60s
         │   ├─ 60s 内收到 responded 标记 → isInterventionHandled() → 跳过 IM
         │   ├─ 60s 内 IM 卡片响应 → OnInterventionResponse() → 取消计时器
         │   └─ 60s 超时 → 发送 IM 通知
         │
         └─ 非交互事件 → 立即分发
```

**web 端响应的完整链路**：

```
用户在 web 端操作（审批/回答）
    → Device 收到响应
    → Agent emit("permission.responded" / "question.responded")
    → EventBus → NotifyForwarder.handleResponseEvent()
    → POST /cloud/device/notify/responded {sessionID, type}
    → server: UPDATE system_notifications SET status='acted' WHERE session_id=?
```

#### NotifyRespondedHandler（server 端新增）

```go
// cloud/handlers.go
func NotifyRespondedHandler(store *notification.Store) gin.HandlerFunc {
    return func(c *gin.Context) {
        // 设备认证（复用现有中间件）

        var body struct {
            SessionID string `json:"sessionID" binding:"required"`
            Type      string `json:"type"`
        }
        if err := c.ShouldBindJSON(&body); err != nil {
            c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
            return
        }

        err := store.MarkRespondedBySession(body.SessionID)
        if err != nil {
            slog.Error("mark responded failed", "sessionID", body.SessionID, "error", err)
        }

        c.JSON(http.StatusOK, gin.H{"success": true})
    }
}
```

### 目录结构（变更部分）

```
server/internal/
├── dispatcher/                       # 新增
│   ├── dispatcher.go                 # Dispatcher（统一分发逻辑）
│   └── selector.go                   # ChannelSelector（渠道选择策略）
├── channel/
│   ├── types.go                      # 修改：OutboundMessage 增加 Action 字段
│   ├── service.go                    # 修改：增加 ActionHandler + 回调处理
│   └── adapters/wecom/
│       ├── types.go                  # 修改：增加 InteractiveCard 等类型
│       ├── adapter.go                # 修改：新增 SendInteractiveCard
│       └── verify.go                 # 修改：ParseInbound 增加 template_card_event
├── notification/
│   ├── service.go                    # 复用：现有 TriggerNotifications
│   └── store.go                      # Store（CRUD + token + 缓冲兜底扫描）
├── cloud/
│   ├── cloud.go                      # 修改：Module 增加 Dispatcher 依赖
│   └── handlers.go                   # 修改：DeviceNotifyHandler + NotifyRespondedHandler
├── models/
│   └── models.go                     # 已修改：SystemNotification 模型
└── cmd/
    ├── api/main.go                   # 修改：初始化 Dispatcher
    └── worker/main.go                # 修改：启动 StartSweep

cs-cloud/internal/                    # ← cs-cloud 仓库变更（另仓）
├── cloud/
│   └── notify_forwarder.go           # 新增：事件转发器
└── ...
```

---

## 数据模型

### system_notifications 表

复用已有的迁移文件 `20260526000000_create_system_notifications.sql`。本提案中该表**仅作内部 token 持久化 + 审计日志**使用，不暴露给前端查询。

```sql
CREATE TABLE IF NOT EXISTS system_notifications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id VARCHAR(191) NOT NULL,
    type VARCHAR(64) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'pending',
    title TEXT NOT NULL,
    content TEXT,
    session_id VARCHAR(255),
    device_id VARCHAR(255),
    workspace_id UUID,
    action_type VARCHAR(64),
    action_data JSONB DEFAULT '{}',
    action_token VARCHAR(128) UNIQUE,
    action_result JSONB,
    acted_at TIMESTAMP,
    expires_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    read_at TIMESTAMP,
    deleted_at TIMESTAMP
);
```

**关键字段说明：**

| 字段 | 用途 |
|------|------|
| `action_token` | 64 hex 字符，一次性令牌，嵌入企微卡片按钮 key 中 |
| `action_data` | JSONB，存储操作所需的完整上下文（含问题索引），回传 Device 时使用 |
| `action_result` | JSONB，用户操作结果（approve/reject/选项索引） |
| `status` | `pending` → `acted`（用户操作后）/ `expired`（超时） |
| `expires_at` | 默认 30 分钟，Store 后台定期清理 |

---

## 模块设计

### Store（`notification/store.go`）

操作令牌存储，底层使用 `system_notifications` 表。提供三层过期保障：

```go
type Store struct {
    db *gorm.DB
}

func (s *Store) Create(input CreateNotificationInput) (*models.SystemNotification, error)
func (s *Store) ExecuteAction(token string, result map[string]any) (*models.SystemNotification, error)
func (s *Store) MarkRespondedBySession(sessionID string) error
func (s *Store) MarkExpired()
func (s *Store) SweepStaleNotifications(staleThreshold time.Duration) ([]models.SystemNotification, error)
func (s *Store) StartSweep(ctx context.Context, disp *Dispatcher)
```

#### Token 过期机制（三层保障）

| 层级 | 机制 | 触发方式 | 目的 |
|------|------|----------|------|
| 1 | 被动过期检查 | `ExecuteAction()` 内 | 用户操作时兜底检查，拒绝已过期 token |
| 2 | 主动批量清理 | `MarkExpired()` 定时调用 | 清理 DB 中堆积的过期记录 |
| 3 | TTL 设置 | `Create()` 时写入 `expires_at` | 创建时确定过期时间，默认 30 分钟 |

主动批量清理通过现有 worker 进程调度：

```go
// notification/store.go
func (s *Store) MarkExpired() {
    threshold := time.Now()
    s.db.Model(&models.SystemNotification{}).
        Where("status = 'pending' AND expires_at IS NOT NULL AND expires_at < ?", threshold).
        Update("status", "expired")
}

func (s *Store) SweepStaleNotifications(staleThreshold time.Duration) ([]models.SystemNotification, error) {
    cutoff := time.Now().Add(-staleThreshold)
    var stale []models.SystemNotification
    if err := s.db.Where(
        "status = 'pending' AND created_at < ? AND deleted_at IS NULL", cutoff,
    ).Find(&stale).Error; err != nil {
        return nil, err
    }
    return stale, nil
}

func (s *Store) StartSweep(ctx context.Context, disp *Dispatcher) {
    ticker := time.NewTicker(5 * time.Minute)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            s.MarkExpired()
            stale, err := s.SweepStaleNotifications(120 * time.Second)
            if err != nil {
                continue
            }
            for _, n := range stale {
                disp.dispatchStaleNotification(n)
            }
        }
    }
}
```

#### 缓冲机制（方案 C：混合方案）

**主路径（server 内存 goroutine）**：交互事件到达时，Dispatcher 启动 60s `time.AfterFunc`，超时后查 DB 判断是否已处理。

**兜底路径（worker 5 分钟轮询）**：扫描 `created_at` 距今超过 120s 且 `status='pending'` 的记录，触发补发。120s 阈值确保不会与 server 内存计时器产生竞争。

```
时间线：
  0s     事件到达，server 启动 60s timer
  60s    server timer 到期，查 DB → 已处理 → 跳过 / 未处理 → 发 IM
  120s   worker 扫描阈值（只处理超过 120s 的记录）
  300s   worker 下一轮扫描（5 分钟间隔）

如果 server 在 0-60s 间重启：
  worker 在 300s 时扫描到 created_at > 120s 的 pending 记录 → 补发 IM
```

---

## 统一分发层

### Dispatcher（`dispatcher/dispatcher.go`）

```go
type Dispatcher struct {
    db              *gorm.DB
    store           *notification.Store
    notificationSvc *notification.NotificationService
    channelSvc      *channel.ChannelService
    cloudBaseURL    string
    bufferPeriod    time.Duration
    pendingMap      sync.Map
}

type DispatchInput struct {
    UserID      string
    WorkspaceID string
    EventType   string
    SessionID   string
    DeviceID    string
    Path        string
    SessionURL  string
    ActionData  map[string]any
}
```

**分发逻辑**：

```go
func (d *Dispatcher) Dispatch(input DispatchInput) {
    channels, err := d.selectChannels(input.UserID, input.EventType)
    if err != nil {
        slog.Error("dispatcher select channels failed", "error", err)
        return
    }

    if needsInteraction(input.EventType) {
        d.bufferDispatch(input, channels)
        return
    }

    d.doDispatch(input, channels)
}

func needsInteraction(eventType string) bool {
    return eventType == "permission" || eventType == "question"
}
```

#### 缓冲逻辑

```go
func (d *Dispatcher) bufferDispatch(input DispatchInput, channels *SelectedChannels) {
    if _, exists := d.pendingMap.Load(input.SessionID); exists {
        return
    }

    pending := &pendingNotification{
        Input:     input,
        Channels:  channels,
        CreatedAt: time.Now(),
    }

    d.pendingMap.Store(input.SessionID, pending)

    pending.Timer = time.AfterFunc(d.bufferPeriod, func() {
        d.handleBufferTimeout(input.SessionID)
    })
}

func (d *Dispatcher) handleBufferTimeout(sessionID string) {
    val, ok := d.pendingMap.LoadAndDelete(sessionID)
    if !ok {
        return
    }

    pending := val.(*pendingNotification)

    if d.isInterventionHandled(sessionID) {
        return
    }

    d.doDispatch(pending.Input, pending.Channels)
}

func (d *Dispatcher) isInterventionHandled(sessionID string) bool {
    var count int64
    d.db.Model(&models.SystemNotification{}).
        Where("session_id = ? AND status = ?", sessionID, "acted").
        Count(&count)
    return count > 0
}

func (d *Dispatcher) OnInterventionResponse(sessionID string) {
    val, ok := d.pendingMap.LoadAndDelete(sessionID)
    if !ok {
        return
    }
    pending := val.(*pendingNotification)
    pending.Timer.Stop()
}

func (d *Dispatcher) doDispatch(input DispatchInput, channels *SelectedChannels) {
    if needsInteraction(input.EventType) && len(channels.Interactive) > 0 {
        d.dispatchInteractive(input, channels)
        return
    }
    d.dispatchNotification(input, channels)
}
```

#### 缓冲机制时序

```
事件到达（permission / question）
    │
    ├── SSE 推送 → web 端（立即，不变）
    │
    └── Dispatcher.Dispatch()
         │
         ├── 非交互事件 → 立即发送 IM 通知
         │
         └── 交互事件 → 启动 60s 计时器
              │
              ├── 60s 内用户在 web 端响应
              │    → handleBufferTimeout() 查询 status='acted' → 跳过 IM 通知
              │
              ├── 60s 内用户通过 IM 卡片响应
              │    → ActionHandler → OnInterventionResponse() → 取消计时器
              │
              └── 60s 超时无人响应
                   → 发送 IM 通知
```

### ChannelSelector（`dispatcher/selector.go`）

```go
type SelectedChannels struct {
    Interactive []models.ChannelConfig
    OneWay      []models.UserNotificationChannel
}

type ChannelSelector struct {
    db *gorm.DB
}

func (s *ChannelSelector) Select(userID, eventType string) (*SelectedChannels, error) {
    result := &SelectedChannels{}

    s.db.Where(
        "user_id = ? AND channel_type IN ('wecom') AND enabled = true AND deleted_at IS NULL",
        userID,
    ).Find(&result.Interactive)

    s.db.Where(
        "user_id = ? AND enabled = true AND ? = ANY(trigger_events) AND deleted_at IS NULL",
        userID, eventType,
    ).Find(&result.OneWay)

    return result, nil
}
```

### 交互事件分发

```go
func (d *Dispatcher) dispatchInteractive(input DispatchInput, channels *SelectedChannels) {
    for _, ch := range channels.Interactive {
        if ch.ChannelType == "wecom" {
            d.sendWeComInteractive(input, ch)
            return
        }
    }
    d.dispatchNotification(input, channels)
}

func (d *Dispatcher) sendWeComInteractive(input DispatchInput, ch models.ChannelConfig) {
    complexity := analyzeQuestionComplexity(input.EventType, input.ActionData)

    switch complexity {
    case "simple_approval":
        d.sendApprovalCard(input, ch)
    case "simple_single_select":
        d.sendSingleSelectCard(input, ch)
    case "multiple_questions":
        d.sendMultipleQuestionCards(input, ch)
    case "complex_with_multiselect":
        d.sendGuidanceCard(input, ch, true)
    default:
        d.sendGuidanceCard(input, ch, false)
    }
}
```

---

## 问卷复杂度判断

### SDK 数据结构（`@opencode-ai/sdk`）

```typescript
type QuestionRequest = {
    id: string
    sessionID: string
    questions: Array<QuestionInfo>
    tool?: { messageID: string; callID: string }
}

type QuestionInfo = {
    question: string
    header: string
    options: Array<QuestionOption>
    multiple?: boolean
    custom?: boolean
}

type QuestionOption = {
    label: string
    description: string
}
```

### 判断规则

```
analyzeQuestionComplexity(eventType, actionData)
    │
    ├─ type == "permission" ─────────────→ simple_approval
    │
    ├─ type == "question"
    │   ├─ 任一问题 multiple==true ──────→ complex_with_multiselect
    │   ├─ 任一问题 custom==true ────────→ complex
    │   ├─ 任一问题 options > 4 ─────────→ complex
    │   ├─ questions.length == 1 ────────→ simple_single_select
    │   ├─ questions.length <= 4 ────────→ multiple_questions
    │   └─ questions.length > 4 ─────────→ complex
    │
    └─ 其他类型 ─────────────────────────→ complex
```

**核心规则**：**只要这批问题中存在任何多选问题，就直接引导到移动端页面**，并在卡片中说明"由于企业微信控件限制，无法在卡片中完成多选操作"。

### 判断实现

```go
func analyzeQuestionComplexity(eventType string, actionData map[string]any) string {
    if eventType == "permission" {
        return "simple_approval"
    }

    if eventType != "question" {
        return "complex"
    }

    questionsVal, ok := actionData["questions"]
    if !ok {
        return "complex"
    }

    questions, ok := questionsVal.([]any)
    if !ok || len(questions) == 0 {
        return "complex"
    }

    for _, q := range questions {
        questionMap, ok := q.(map[string]any)
        if !ok {
            return "complex"
        }
        if multiple, ok := questionMap["multiple"].(bool); ok && multiple {
            return "complex_with_multiselect"
        }
        if custom, ok := questionMap["custom"].(bool); ok && custom {
            return "complex"
        }
        if optionsVal, ok := questionMap["options"]; ok {
            if options, ok := optionsVal.([]any); ok && len(options) > 4 {
                return "complex"
            }
        }
    }

    switch {
    case len(questions) == 1:
        return "simple_single_select"
    case len(questions) <= 4:
        return "multiple_questions"
    default:
        return "complex"
    }
}
```

### 各场景发送实现

#### 权限审批卡片

```go
func (d *Dispatcher) sendApprovalCard(input DispatchInput, ch models.ChannelConfig) {
    token, err := d.store.Create(notification.CreateNotificationInput{
        UserID:     input.UserID,
        Type:       input.EventType,
        Title:      "权限审批请求",
        SessionID:  input.SessionID,
        DeviceID:   input.DeviceID,
        ActionType: "permission_approval",
        ActionData: jsonMarshal(input.ActionData),
    })
    if err != nil {
        slog.Error("create notification failed", "error", err)
        return
    }

    card := wecom.InteractiveCard{
        Title:       "权限审批请求",
        Description: buildPermissionDescription(input.ActionData),
        URL:         input.SessionURL,
        Buttons: []wecom.CardButton{
            {Text: "批准", Key: fmt.Sprintf("approve:%s", token.ActionToken), Style: 1},
            {Text: "拒绝", Key: fmt.Sprintf("reject:%s", token.ActionToken), Style: 2},
        },
    }

    if err := d.channelSvc.SendInteractiveCard(ctx, input.UserID, "wecom", card); err != nil {
        slog.Error("send approval card failed", "error", err)
    }
}
```

**企微卡片示例**：

```json
{
  "msgtype": "template_card",
  "template_card": {
    "card_type": "button_interaction",
    "main_title": { "title": "权限审批请求" },
    "sub_title_text": "工具: Bash\n命令: rm -rf /tmp/test\n\n<a href=\"SESSION_URL\">在会话中查看详情</a>",
    "button_list": [
      { "text": "批准", "style": 1, "key": "approve:TOKEN" },
      { "text": "拒绝", "style": 2, "key": "reject:TOKEN" }
    ]
  }
}
```

#### 单选问题卡片

```go
func (d *Dispatcher) sendSingleSelectCard(input DispatchInput, ch models.ChannelConfig) {
    questionMap := input.ActionData["questions"].([]any)[0].(map[string]any)
    options := questionMap["options"].([]any)

    token, err := d.store.Create(notification.CreateNotificationInput{
        UserID:     input.UserID,
        Type:       input.EventType,
        Title:      questionMap["header"].(string),
        SessionID:  input.SessionID,
        DeviceID:   input.DeviceID,
        ActionType: "question_select",
        ActionData: jsonMarshal(input.ActionData),
    })
    if err != nil {
        slog.Error("create notification failed", "error", err)
        return
    }

    buttons := make([]wecom.CardButton, 0, len(options))
    for i, opt := range options {
        o := opt.(map[string]any)
        buttons = append(buttons, wecom.CardButton{
            Text:  o["label"].(string),
            Key:   fmt.Sprintf("select:%s:%d", token.ActionToken, i),
            Style: 1,
        })
    }

    card := wecom.InteractiveCard{
        Title:       questionMap["header"].(string),
        Description: questionMap["question"].(string),
        URL:         input.SessionURL,
        Buttons:     buttons,
    }

    if err := d.channelSvc.SendInteractiveCard(ctx, input.UserID, "wecom", card); err != nil {
        slog.Error("send select card failed", "error", err)
    }
}
```

#### 多问题多条卡片（2-4 个单选问题）

```go
func (d *Dispatcher) sendMultipleQuestionCards(input DispatchInput, ch models.ChannelConfig) {
    questions := input.ActionData["questions"].([]any)

    for i, q := range questions {
        questionMap := q.(map[string]any)
        options := questionMap["options"].([]any)

        token, err := d.store.Create(notification.CreateNotificationInput{
            UserID:     input.UserID,
            Type:       input.EventType,
            Title:      questionMap["header"].(string),
            SessionID:  input.SessionID,
            DeviceID:   input.DeviceID,
            ActionType: "question_select",
            ActionData: jsonMarshal(map[string]any{
                "questionIndex": i,
                "question":      questionMap,
            }),
        })
        if err != nil {
            slog.Error("create notification for question failed", "index", i, "error", err)
            continue
        }

        buttons := make([]wecom.CardButton, 0, len(options))
        for j, opt := range options {
            o := opt.(map[string]any)
            buttons = append(buttons, wecom.CardButton{
                Text:  o["label"].(string),
                Key:   fmt.Sprintf("select:%s:%d", token.ActionToken, j),
                Style: 1,
            })
        }

        card := wecom.InteractiveCard{
            Title:       fmt.Sprintf("问题 %d/%d：", i+1, len(questions)) + questionMap["header"].(string),
            Description: questionMap["question"].(string),
            URL:         input.SessionURL,
            Buttons:     buttons,
        }

        if err := d.channelSvc.SendInteractiveCard(ctx, input.UserID, "wecom", card); err != nil {
            slog.Error("send question card failed", "index", i, "error", err)
        }
    }
}
```

#### 引导卡片（复杂场景 → 移动端）

```go
func (d *Dispatcher) sendGuidanceCard(input DispatchInput, ch models.ChannelConfig, isMultiSelect bool) {
    var description string

    if isMultiSelect {
        questions := input.ActionData["questions"].([]any)
        description = fmt.Sprintf(
            "有 %d 个问题需要回答，其中包含多选题\n\n由于企业微信控件限制，无法在卡片中完成多选操作，请前往移动端或网页端处理",
            len(questions),
        )
    } else if input.EventType == "question" {
        questions := input.ActionData["questions"].([]any)
        description = fmt.Sprintf(
            "有 %d 个问题需要回答\n\n请在移动端或网页端查看详情并回答",
            len(questions),
        )
    } else {
        description = "需要在会话中完成操作\n\n请在移动端或网页端查看详情"
    }

    card := wecom.InteractiveCard{
        Title:       "需要您的操作",
        Description: description,
        URL:         input.SessionURL,
        Buttons: []wecom.CardButton{
            {Text: "前往处理", Key: fmt.Sprintf("navigate:%s", input.SessionURL), Style: 1},
        },
    }

    if err := d.channelSvc.SendInteractiveCard(ctx, input.UserID, "wecom", card); err != nil {
        slog.Error("send guidance card failed", "error", err)
    }
}
```

---

## 企微交互卡片

### 发送侧

```go
// types.go 新增
type InteractiveCard struct {
    Title       string
    Description string
    URL         string
    Buttons     []CardButton
}

type CardButton struct {
    Text  string
    Key   string
    Style int
}

type WeComTemplateCard struct {
    CardType     string            `json:"card_type"`
    MainTitle    *WeComTitle       `json:"main_title,omitempty"`
    SubTitleText string            `json:"sub_title_text,omitempty"`
    ButtonList   []WeComCardButton `json:"button_list,omitempty"`
}
```

```go
// adapter.go 新增
func (a *WeComAdapter) SendInteractiveCard(ctx context.Context, userID string, card InteractiveCard) error {
    accessToken, err := getAccessToken(&a.sysConfig, a.client, cache)
    if err != nil {
        return err
    }

    subTitle := card.Description
    if card.URL != "" {
        subTitle += fmt.Sprintf("\n\n<a href=\"%s\">在会话中查看</a>", card.URL)
    }

    body, _ := json.Marshal(WeComSendRequest{
        ToUser:  userID,
        MsgType: "template_card",
        AgentID: a.sysConfig.AgentID,
        TemplateCard: &WeComTemplateCard{
            CardType:     "button_interaction",
            MainTitle:    &WeComTitle{Title: card.Title},
            SubTitleText: subTitle,
            ButtonList:   convertButtons(card.Buttons),
        },
    })

    url := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/message/send?access_token=%s", accessToken)
    resp, err := a.client.Post(url, "application/json", bytes.NewReader(body))
    // ... 错误处理 ...
}
```

### 回调侧

**ParseInbound 增强**（`verify.go`）：

```go
func parseEventKey(key string) (action string, token string) {
    parts := strings.SplitN(key, ":", 3)
    if len(parts) < 2 {
        return key, ""
    }
    if parts[0] == "select" && len(parts) == 3 {
        return "select:" + parts[2], parts[1]
    }
    return parts[0], parts[1]
}
```

### 卡片状态更新

```go
func (a *WeComAdapter) UpdateCardStatus(responseCode, statusText string) error {
    accessToken, err := getAccessToken(&a.sysConfig, a.client, cache)
    if err != nil {
        return err
    }

    body, _ := json.Marshal(map[string]any{
        "response_code": responseCode,
        "template_card": map[string]any{
            "card_type":      "button_interaction",
            "sub_title_text": statusText,
            "button_list": []map[string]any{
                {"text": statusText, "style": 1, "key": "done"},
            },
        },
    })

    url := fmt.Sprintf(
        "https://qyapi.weixin.qq.com/cgi-bin/message/update_template_card?access_token=%s",
        accessToken,
    )
    resp, err := a.client.Post(url, "application/json", bytes.NewReader(body))
    // ... 错误处理 ...
}
```

---

## 操作回传链路

### ActionHandler（`main.go` 中构造）

```go
actionHandler := func(ctx context.Context, action string, token string) error {
    pending, err := store.ExecuteAction(token, map[string]any{"action": action})
    if err != nil {
        return fmt.Errorf("token invalid or expired: %w", err)
    }

    dispatcher.OnInterventionResponse(pending.SessionID)

    result := map[string]any{
        "sessionID": pending.SessionID,
        "type":      pending.Type,
        "action":    action,
        "token":     token,
    }

    return cloudModule.Router.RouteUserCommand(pending.DeviceID, cloud.Event{
        Type:       "intervention.response",
        Properties: result,
    })
}
```

### 完整链路

```
用户点击"批准" → 企微回调 POST /api/webhooks/channels/wecom
    → ChannelService.HandleWebhook()
    → ParseInbound() → {ContentType: "action_callback", Content: "approve", Metadata: {"actionToken": "TOKEN"}}
    → ActionHandler("approve", "TOKEN")
    → Store.ExecuteAction("TOKEN")
        → UPDATE system_notifications SET status='acted', acted_at=NOW()
          WHERE action_token='TOKEN' AND status='pending'
    → Dispatcher.OnInterventionResponse(sessionID) → 取消缓冲计时器
    → EventRouter.RouteUserCommand(deviceID, Event{Type: "intervention.response", ...})
    → Gateway → Device
    → UpdateCardStatus(responseCode, "已批准 ✓")
```

### CSC Agent 端配合

Agent 需要支持接收 `intervention.response` 事件：

```json
{
  "type": "intervention.response",
  "properties": {
    "sessionID": "...",
    "type": "permission",
    "action": "approve",
    "token": "abc123"
  }
}
```

Agent 收到后：
- `permission` + `approve`：恢复执行被挂起的工具调用
- `permission` + `reject`：中止当前操作
- `question` + `select:N`：将选项 N 注入到会话流程中（N 为选项索引）
- 超时未收到（30 分钟）：Agent 自行决定默认行为

---

## 错误处理

| 场景 | 处理方式 |
|------|---------|
| Token 不存在或已使用 | 返回 `"success"` 给企微（避免重试），日志记录 |
| Token 已过期 | Store 自动标记 `expired`，同上 |
| Device 不在线 | `RouteUserCommand` 返回错误，日志记录 |
| 无任何渠道配置 | 日志记录，SSE 推送仍然有效 |
| 企微 API 调用失败 | 日志记录，不影响 SSE 推送 |
| 卡片状态更新失败 | 日志记录，不影响操作回传 |
| SessionURL 为空 | 卡片中省略链接，纯按钮交互仍可用 |

---

## 文件变更清单

### server 仓库（`costrict-web/server`）

| 文件 | 操作 | 说明 |
|------|------|------|
| `migrations/20260526000000_create_system_notifications.sql` | 已创建 | system_notifications 表 |
| `models/models.go` | 已修改 | SystemNotification 模型 |
| `notification/store.go` | 已创建 | Store（CRUD + token 过期 + 缓冲兜底扫描 + StartSweep） |
| `dispatcher/dispatcher.go` | **新建** | Dispatcher（统一分发 + 复杂度判断 + 各场景发送 + 缓冲机制） |
| `dispatcher/selector.go` | **新建** | ChannelSelector（渠道查询） |
| `channel/types.go` | 修改 | OutboundMessage 扩展 |
| `channel/service.go` | 修改 | 增加 SendInteractiveCard + SetActionHandler + HandleWebhook 回调分支 |
| `channel/adapters/wecom/types.go` | 修改 | 增加 InteractiveCard、WeComTemplateCard 等类型 |
| `channel/adapters/wecom/adapter.go` | 修改 | 新增 SendInteractiveCard、UpdateCardStatus |
| `channel/adapters/wecom/verify.go` | 修改 | ParseInbound 增加 template_card_event + parseEventKey |
| `cloud/types.go` | 修改 | 新增 EventInterventionResponse |
| `cloud/cloud.go` | 修改 | Module 增加 Dispatcher 依赖 + 注册新路由 |
| `cloud/handlers.go` | 修改 | DeviceNotifyHandler 调用 Dispatcher + 新增 NotifyRespondedHandler |
| `cmd/api/main.go` | 修改 | 初始化 Store、Dispatcher、ActionHandler |
| `cmd/worker/main.go` | 修改 | 启动 notificationStore.StartSweep() |

### cs-cloud 仓库（`cs-cloud`）

| 文件 | 操作 | 说明 |
|------|------|------|
| `cloud/notify_forwarder.go` | **新建** | EventBus 订阅者：识别标准事件 + 响应事件 + 模式判断 + 调用 server API |
| `cli/serve.go` 或 `localserver/server.go` | 修改 | 初始化并启动 NotifyForwarder |

> 无需修改任何 agent runtime 代码（`agent/csc/`、`agent/cs/`）。NotifyForwarder 仅订阅 EventBus，与底层 runtime 解耦。

---

## 实施顺序

**策略：先跑通主流程（事件到达 → 通知发出），再补充交互卡片和定时清理。**

### Phase 1: 基础设施 + 主流程（server）

```
Step 1: Store（notification/store.go）
        依赖：system_notifications 表 + SystemNotification 模型（已有）
        内容：Create / ExecuteAction / MarkRespondedBySession / MarkExpired
        验证：单元测试 CRUD + token 过期检查

Step 2: Dispatcher（dispatcher/dispatcher.go + selector.go）
        内容：Dispatch 入口 + ChannelSelector 渠道查询
              + 复杂度判断 analyzeQuestionComplexity
              + 缓冲机制（60s timer + pendingMap + isInterventionHandled）
              + OnInterventionResponse + dispatchStaleNotification（worker 兜底）
        验证：单元测试分发路由 + 缓冲超时 + 取消逻辑
        注意：此步骤各场景发送方法（sendApprovalCard 等）先写骨架，
              实际企微卡片发送在 Step 5 中实现

Step 3: Server 端接入（cloud/handlers.go + cloud.go + main.go）
        内容：
        - DeviceNotifyHandler 改造：调用 Dispatcher.Dispatch()
        - NotifyRespondedHandler 新增：POST /cloud/device/notify/responded
        - cloud.go 模块改造：注册 Dispatcher + 新路由
        - main.go 依赖注入：Store → Dispatcher → ActionHandler 闭包
        验证：
        - curl 模拟 POST /cloud/device/notify → DB 写入 pending 记录
        - curl 模拟 POST /cloud/device/notify/responded → status 更新为 acted
        - 确认 60s 后日志输出"buffer timeout, sending IM notification"
```

### Phase 2: 事件源接入（cs-cloud）

```
Step 4: NotifyForwarder（cs-cloud/cloud/notify_forwarder.go）
        内容：
        - EventBus 订阅 → 识别 permission.asked / question.asked / session.idle
        - cloud 模式判断 → POST /cloud/device/notify
        - 识别 permission.responded / question.responded
          → POST /cloud/device/notify/responded
        - 初始化集成（入口文件启动 NotifyForwarder）
        验证：
        - 本地 cs-cloud 启动，触发权限请求
        - 观察 server 日志收到 POST /cloud/device/notify
        - 观察 DB 写入 system_notifications 记录
        - web 端响应后，观察 status 更新为 acted
```

**里程碑：此时主流程已跑通**

### Phase 3: 企微交互卡片

```
Step 5: WeComAdapter 交互卡片
        内容：
        5a. types.go — InteractiveCard / CardButton / WeComTemplateCard 等类型
        5b. adapter.go — SendInteractiveCard + UpdateCardStatus
        5c. verify.go — ParseInbound 增强（template_card_event + parseEventKey）
        5d. service.go — SendInteractiveCard 方法 + SetActionHandler + HandleWebhook 回调分支
        5e. dispatcher.go — 填充各场景发送方法
        验证：
        - 权限请求 → 企微收到批准/拒绝卡片
        - 点击批准 → 回调 → Device 收到 intervention.response
        - 单选问卷 → 企微收到选项按钮 → 点击 → 回调 → Device
        - 多问题 → 多张卡片 → 逐个回调
        - 多选/复杂 → 引导卡片 + 会话 URL
```

### Phase 4: Worker 定时清理

```
Step 6: Worker 集成（cmd/worker/main.go）
        内容：
        - 初始化 notification.Store
        - go notificationStore.StartSweep(ctx, dispatcher)
        - 合并执行：过期 token 清理（MarkExpired）+ 缓冲兜底补发（120s 阈值）
        验证：
        - 模拟 server 重启，pending 记录超过 120s
        - worker 扫描到 → 补发 IM 通知
        - pending 记录超过 30 分钟 → MarkExpired 标记为 expired
```

### Phase 5: 端到端验证

```
Step 7: 全链路验证
        7a. 权限审批：事件 → 缓冲 60s → 企微卡片 → 批准/拒绝 → Device
        7b. 单选问卷：事件 → 缓冲 60s → 选项按钮 → 选择 → Device
        7c. 多问题：事件 → 多张卡片 → 逐个回调 → Device
        7d. 多选/复杂：事件 → 引导卡片 + 会话 URL
        7e. Web 端响应：用户 web 端操作 → cs-cloud 标记 → 缓冲跳过 IM
        7f. IM 卡片响应：点击企微卡片 → 取消计时器 → Device
        7g. Worker 兜底：模拟 server 重启 → 120s 后补发 IM
        7h. Token 过期：超过 30 分钟 → MarkExpired → ExecuteAction 拒绝
```

---

**文档版本：** 3.0.0
**创建日期：** 2026-05-26
**更新日期：** 2026-05-28
**维护者：** CoStrict Team
