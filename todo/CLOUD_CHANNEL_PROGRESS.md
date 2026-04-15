# Cloud Channel 模块实施进度

基于 `docs/proposals/CLOUD_CHANNEL_DESIGN.md` v2.2.0，任务跟踪。

> **实现状态：✅ 一期骨架完成（含第二轮 review 修复）**
> 核心模块、两个适配器、Worker 进程、API 路由均已实现并编译通过。
> 企微适配器的消息解密解析仍需真实环境联调验证。

---

## Review 修复记录

### 第一轮 review
- ✅ auth 中间件缺失
- ✅ 消息未解密
- ✅ config 匹配错误
- ✅ 死代码清理
- ✅ `interface{}` → `context.Context`（部分）

### 第二轮 review
- ✅ `ReplyTarget` 增加 `ContextToken` 字段，微信 `Reply()` 传入 `contextToken`
- ✅ `InboundMessageHandler` 签名从 `interface{}` 改为 `context.Context`
- ✅ `ChannelConfig` 加入 `cmd/migrate/main.go` AutoMigrate 列表
- ✅ `errcode=-14` 会话超时：Poller 停止并返回提示，不做指数退避
- ✅ Worker `refreshPollers` 检测 config 内容变更（SHA256 哈希对比），自动重启 poller
- ✅ API 响应返回 `webhookUrl` 字段（`{cloudBaseURL}/api/channels/{type}/webhook`）
- ✅ `WeChatClient.doRequest` 传入 `context.Context`，不再硬编码 `context.Background()`
- ✅ Poller 移除未使用的 `cancel` 字段

### 待后续版本
- [ ] 企微消息去重（MsgId 缓存）
- [ ] 微信 typing 状态发送

---

## 一、数据模型（`server/internal/models/`）

- [x] 追加 `ChannelConfig` 模型
  ```go
  type ChannelConfig struct {
      ID              string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
      UserID          string         `gorm:"not null;index"                                 json:"userId"`
      ChannelType     string         `gorm:"not null;index"                                 json:"channelType"`
      Name            string         `gorm:"not null"                                       json:"name"`
      Enabled         bool           `gorm:"not null;default:true"                          json:"enabled"`
      Config          datatypes.JSON `gorm:"type:jsonb;default:'{}'"                        json:"config"`
      WebhookVerified bool           `gorm:"not null;default:false"                         json:"webhookVerified"`
      LastActiveAt    *time.Time     `                                                      json:"lastActiveAt,omitempty"`
      LastError       string         `                                                      json:"lastError,omitempty"`
      CreatedAt       time.Time      `                                                      json:"createdAt"`
      UpdatedAt       time.Time      `                                                      json:"updatedAt"`
      DeletedAt       gorm.DeletedAt `gorm:"index"                                          json:"-"`
  }
  ```
- [x] `AutoMigrate` 追加新模型
- [x] 创建 migration SQL 文件（`server/migrations/`）

> 注：只建 `channel_configs` 一张表。会话绑定和消息日志均留给后续云端 AI 的 session 会话模块。

---

## 二、Channel 核心模块（`server/internal/channel/`）

### 2.1 types.go — 核心类型定义

- [x] `ChannelCapabilities` 结构体
- [x] `InboundMessage` 结构体
- [x] `OutboundMessage` 结构体
- [x] `ReplyTarget` 结构体（含 `ContextToken` 字段）
- [x] `ChannelEvent` 结构体

### 2.2 adapter.go — ChannelAdapter 接口 + 注册表

- [x] `ChannelAdapter` 接口（Type / Capabilities / ValidateConfig / ConfigSchema / ParseInbound / HandleVerification / Reply / ParseEvent）
- [x] `StartableChannel` 可选接口（Start 方法，长轮询模式）
- [x] 适配器注册表（`Register` / `Get` / `All`）

### 2.3 handler.go — MessageHandler 接口

- [x] `MessageHandler` 接口定义
- [x] `EchoMessageHandler` 默认实现

### 2.4 service.go — ChannelService（API Server 侧）

- [x] `ChannelService` 结构体（db / adapters / messageHandler）
- [x] `NewChannelService(db, handler, cloudBaseURL) *ChannelService`
- [x] `HandleWebhook(channelType, r)` — 解析 → 同步处理 → 回复
- [x] `CreateConfig / UpdateConfig / DeleteConfig / GetConfig / ListConfigs` — 配置 CRUD
- [x] `SendTestMessage(userID, configID)` — 测试发送
- [x] `GetWebhookURL(channelType)` — 返回 webhook URL

### 2.5 worker.go — ChannelWorker（独立进程核心）

- [x] `ChannelWorker` 结构体（db / adapters / messageHandler / pollers / configHashes / mu）
- [x] `NewChannelWorker(db, handler) *ChannelWorker`
- [x] `Run(ctx) error` — StartPollers + WatchConfigChanges
- [x] `startPoller(configID, config)` — 启动长轮询 goroutine
- [x] `stopPoller(configID)` — 停止长轮询
- [x] `refreshPollers()` — 定期检查配置变更（SHA256 对比 config 内容），动态启停

### 2.6 handlers.go — Gin HTTP Handler

- [x] `POST /api/channels/wecom/webhook` — 企微回调（无需鉴权，同步处理）
- [x] `GET /api/channels/available` — 列出可用 Channel 类型
- [x] `GET /api/channels` — 列出用户配置
- [x] `POST /api/channels` — 创建配置
- [x] `GET /api/channels/:id` — 获取详情
- [x] `PUT /api/channels/:id` — 更新配置
- [x] `DELETE /api/channels/:id` — 删除配置
- [x] `POST /api/channels/:id/test` — 测试发送
- [x] Swagger 注释

### 2.7 channel.go — Module 定义

- [x] `Module` 结构体 + `New(db, handler) *Module`
- [x] `RegisterRoutes(apiGroup *gin.RouterGroup)`

---

## 三、企微适配器（`server/internal/channel/adapters/wecom/`）

### 3.1 types.go

- [x] `WeComConfig` 结构体（CorpID / AgentID / Secret / Token / EncodingAESKey）
- [x] 企微回调 XML 结构体
- [x] 企微消息发送请求/响应结构体

### 3.2 crypto.go — 消息加解密

- [x] AES CBC 解密（PKCS7 unpad）
- [x] AES CBC 加密（PKCS7 pad）
- [x] 签名生成与验证

### 3.3 verify.go — 回调 URL 验证

- [x] `HandleVerification(r, config)` — 解密 echostr 返回明文

### 3.4 adapter.go — WeComAdapter

- [x] `Type()` → "wecom"
- [x] `Capabilities()` / `ValidateConfig()` / `ConfigSchema()`
- [x] `ParseInbound(r)` — 解密 XML → InboundMessage
- [x] `HandleVerification(r, config)`
- [x] `Reply(ctx, config, target, message)` — access_token 缓存 + 消息发送
- [x] Access Token 缓存（sync.Map，TTL 7000s）

### 3.5 注册入口

- [x] `adapter.Register(wecom.NewWeComAdapter())`

---

## 四、微信个人号适配器（`server/internal/channel/adapters/wechat/`）

> 参考 `Tencent/openclaw-weixin` 的 iLink Bot API 协议。

### 4.1 types.go

- [x] `WeChatConfig`（APIServer / Token）
- [x] `WeixinMessage` / `MessageItem` / 请求响应结构体

### 4.2 client.go — iLink Bot API 客户端

- [x] `WeChatClient` + `NewWeChatClient(config)`
- [x] `GetUpdates(ctx, getUpdatesBuf)`
- [x] `SendMessage(ctx, toUserID, contextToken, itemList)`
- [x] `GetConfig(ctx, ilinkUserID)` / `SendTyping(ctx, ...)`
- [x] 通用请求方法（Auth 头）

### 4.3 polling.go — Long Polling

- [x] `Poller` 结构体
- [x] `Start(ctx)` — 循环 + 游标 + 自动重连
- [x] `Stop()`
- [x] 退避重试

### 4.4 adapter.go — WeChatAdapter（StartableChannel）

- [x] `Type()` → "wechat"
- [x] `Capabilities()` / `ValidateConfig()` / `ConfigSchema()`
- [x] `Reply(ctx, config, target, message)` — SendMessage API
- [x] `Start(ctx, config, handler)` — 创建 Poller

### 4.5 注册入口

- [x] `adapter.Register(wechat.NewWeChatAdapter())`

---

## 五、Channel Worker 进程（`server/cmd/channel-worker/main.go`）

- [x] 入口函数（参照 `cmd/worker/main.go`）
- [x] logger（FilePrefix: "channel-worker"）
- [x] DB 初始化 + AutoMigrate
- [x] 注册适配器 + 创建 ChannelWorker
- [x] SIGINT/SIGTERM 优雅关闭

### 5.1 Dockerfile.channel-worker

- [x] 参照 `Dockerfile.worker`

### 5.2 package.json scripts

- [x] `"dev:channel-worker"` / `"build:channel-worker"`
- [x] 更新 `"dev:all"` 和 `"build"`

---

## 六、API Server 路由注册（`server/cmd/api/main.go`）

- [x] `channelModule := channel.New(database.GetDB(), channel.NewEchoMessageHandler())`
- [x] `channelModule.RegisterRoutes(apiGroup)`

---

## 七、配置敏感字段处理

- [x] API 响应脱敏（wecom: secret/token/encodingAesKey → `***`，wechat: token → `***`）
- [x] API 响应包含 `webhookUrl` 字段

---

## 进度概览

| 阶段 | 内容 | 状态 |
|------|------|------|
| 一 | 数据模型（channel_configs 1 张表） | ✅ 已完成 |
| 二 | Channel 核心模块 | ✅ 已完成 |
| 三 | 企微适配器（wecom） | ✅ 已完成 |
| 四 | 微信个人号适配器（wechat） | ✅ 已完成 |
| 五 | Channel Worker 进程 | ✅ 已完成 |
| 六 | API Server 路由注册 | ✅ 已完成 |
| 七 | 配置敏感字段处理 | ✅ 已完成 |
| 八 | 第二轮 review 修复 | ✅ 已完成 |
