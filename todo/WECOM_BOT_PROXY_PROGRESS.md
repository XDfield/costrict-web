# WeCom Bot Proxy 实施进度

> **部分已废弃**：企微模板卡片（template_card）相关进度项已于 2026-07-15 移除。
> Proxy 现仅支持 text/markdown 消息转发；卡片发送（`SendInteractiveCard` /
> `SendVoteCard` / `SendTextNoticeCard`）、卡片更新（`/api/bot/card/update`）、
> 卡片回调监听（`OnEventTemplateCardEvent`）相关条目保留作历史记录。

基于 `docs/proposals/WECOM_BOT_PROXY_DESIGN.md`，按依赖关系排序的开发任务列表。

---

## 一、Proxy 项目脚手架（无依赖）

- [ ] 仓库初始化 — `wecom-bot-proxy` 独立 Go module
  - `go.mod` — `module github.com/costrict/wecom-bot-proxy`
  - 依赖：`github.com/gin-gonic/gin`、`nhooyr.io/websocket`（或 `github.com/coder/websocket`）、`github.com/goccy/go-yaml`
- [ ] `cmd/proxy/main.go` — 入口：加载配置、初始化组件、启动 HTTP server、优雅关闭
- [ ] `internal/config/config.go` — YAML 配置加载 + 环境变量插值（`${VAR}` 语法）
- [ ] `config.yaml` — 配置文件模板
- [ ] `Dockerfile` — 多阶段构建
- [ ] `docker-compose.yaml` — 本地开发编排

---

## 二、WebSocket 连接管理（依赖：一）

- [ ] `internal/ws/protocol.go` — 企微长连接协议类型定义
  - 所有 cmd 请求/响应结构体（`aibot_subscribe`、`ping`、`aibot_msg_callback`、`aibot_event_callback`、`aibot_respond_msg`、`aibot_respond_welcome_msg`、`aibot_respond_update_msg`、`aibot_send_msg`）
  - 统一的 WS frame 封装（`cmd` + `headers` + `body`）
- [ ] `internal/ws/conn.go` — WS 连接生命周期状态机
  - 状态：Disconnected → Connecting → Connected
  - 连接 `wss://openws.work.weixin.qq.com` + 发送 `aibot_subscribe`
  - 监听 `disconnected_event` 触发重连
  - 统一 write loop goroutine（避免并发写）
  - 消息分发：收到 S→C 消息后回调上层 handler
- [ ] `internal/ws/heartbeat.go` — 心跳管理器
  - 30s 间隔发送 `ping`
  - 连续 3 次未收到 pong → 判定连接失效 → 触发重连
  - 仅在 Connected 状态发送心跳
- [ ] `internal/ws/conn_test.go` — 连接管理单测（mock WS server）

---

## 三、路由引擎（依赖：一）

- [ ] `internal/router/table.go` — Task Route 路由表
  - `task_routes`：LRU Cache，`task_id → backend`，TTL 24h
  - `default_backend`：兜底路由目标
  - `Register(taskID, backend)` — 写入路由
  - `Route(msg) → backend` — 路由查找（卡片事件走 task_id，其他走 default）
- [ ] `internal/router/table_test.go` — 路由表单测
  - 注册 + 查找、TTL 过期、未命中回退 default、LRU 淘汰

---

## 四、协议翻译层（依赖：二）

- [ ] `internal/api/translator.go` — 业务格式 ↔ 企微 WS 协议翻译
  - `TranslateSend(req)` — 业务 outbound → `aibot_send_msg`（处理 `chat_type` 映射、`msg_type` 映射、`card` 类型反序列化）
  - `TranslateReply(req)` — 业务 reply → `aibot_respond_msg`
  - `TranslateStreamReply(req)` — 业务 stream reply → `aibot_respond_msg`（流式）
  - `TranslateWelcome(req)` — 业务 welcome → `aibot_respond_welcome_msg`
  - `TranslateCardUpdate(req)` — 业务 card update → `aibot_respond_update_msg`
  - `TranslateInbound(wsMsg)` — 企微 WS 推送 → 标准化 InboundMessage 格式

---

## 五、后端客户端（依赖：一）

- [ ] `internal/backend/client.go` — HTTP 客户端
  - `Forward(backend, inboundMsg)` — 向后端转发 inbound 消息
  - HMAC-SHA256 签名（`X-Bot-Proxy-Signature` + `X-Bot-Proxy-Timestamp`）
  - 重试逻辑（按 backend 配置）
  - 后端健康状态追踪（`healthy` + `last_success`）
- [ ] `internal/backend/client_test.go` — 后端客户端单测

---

## 六、消息去重（依赖：一）

- [ ] `internal/dedup/dedup.go` — 基于 msgid 的 LRU 去重
  - 可配置大小（默认 10000）+ TTL（默认 5min）
  - `Check(msgID) → bool`（存在则跳过）

---

## 七、HTTP API Server（依赖：二 ~ 六）

- [ ] `internal/api/server.go` — Gin HTTP server
  - 中间件：per-backend token 鉴权 + 速率限制
  - 路由注册
- [ ] `internal/api/handlers.go` — REST handlers
  - `POST /api/bot/send` — 发送消息（显式 `task_id` 注册路由 + translator 翻译 + WS 发送）
  - `POST /api/bot/reply` — 回复消息
  - `POST /api/bot/reply/stream` — 流式消息回复
  - `POST /api/bot/welcome` — 回复欢迎语
  - `POST /api/bot/card/update` — 更新模板卡片（不做重试）
  - `GET /api/bot/health` — 健康检查（连接状态 + 后端状态）

---

## 八、消息分发管道（依赖：二、三、四、五、六）

- [ ] WS 推送 → Inbound 分发集成
  - WS conn 收到 `aibot_msg_callback` → translator 翻译 → dedup 检查 → route 查找 → backend forward
  - WS conn 收到 `aibot_event_callback` → 判断事件类型 → translator 翻译 → route 查找 → backend forward
  - 卡片事件路由：提取 `task_id` → task_routes 查找 → 精确路由 / 回退 default
  - 进入会话事件路由：直接走 default backend

---

## 九、costrict-web 适配器（依赖：七，可与八并行）

- [ ] `server/internal/channel/adapters/wecom-bot/types.go` — 类型定义
  - `BotProxyClient` 配置结构
  - proxy API 请求/响应类型
- [ ] `server/internal/channel/adapters/wecom-bot/client.go` — proxy HTTP API 客户端
  - `Send(ctx, userID, chatType, msgType, content, taskID)` — 调用 `POST /api/bot/send`
  - `Reply(ctx, reqID, msgType, content)` — 调用 `POST /api/bot/reply`
  - `UpdateCard(ctx, reqID, cardUpdate)` — 调用 `POST /api/bot/card/update`
  - `Welcome(ctx, reqID, msgType, content)` — 调用 `POST /api/bot/welcome`
  - Bearer token 鉴权
- [ ] `server/internal/channel/adapters/wecom-bot/adapter.go` — `WeComBotAdapter`
  - 实现 `channel.ChannelAdapter` 接口
  - `Type() → "wecom-bot"`
  - `ParseInbound()` — 直接反序列化 proxy 转发的 InboundMessage
  - `HandleVerification()` — 空实现（长连接模式无需验证）
  - `Reply()` — 通过 proxy `/api/bot/send` 发送
  - `ConfigSchema()` — proxy URL + auth token 配置项
- [ ] 卡片方法实现
  - `SendInteractiveCard(userID, card, taskID)` — 复用 `InteractiveCard` 结构
  - `SendVoteCard(userID, card, taskID)` — 复用 `VoteCard` 结构
  - `SendTextNoticeCard(userID, card, taskID)` — 复用 `TextNoticeCard` 结构
- [ ] 卡片更新实现
  - 实现 `channel.CardStatusUpdater` 接口
  - `UpdateCardStatus()` — 通过 proxy `/api/bot/card/update` 更新
- [ ] 降级策略实现
  - `HandleActionCallback()` — 封装 3s 超时降级逻辑（超时返回"处理中"）
- [ ] `ChannelService` 集成
  - `NewChannelService` 新增 `weComBotEnabled` 参数
  - adapter registry 注册 `wecom-bot`
- [ ] Webhook 路由 — 复用 `/api/webhooks/channels/wecom-bot`

---

## 十、集成测试（依赖：八、九）

- [ ] Proxy 单元测试覆盖
  - 路由表：注册 + 查找 + TTL + LRU
  - 协议翻译：业务格式 ↔ 企微 WS 协议双向正确性
  - 后端客户端：重试 + 签名 + 超时
  - 消息去重：LRU + TTL
- [ ] 端到端集成测试
  - WS 消息接收 → 路由 → 后端转发 → 回复 → WS 发送（完整闭环）
  - 卡片回调：发送卡片 → 用户点击 → 精确路由 → 卡片更新
  - 自由消息：用户发消息 → 路由到 default → 回复
  - 多后端并行：同一用户多后端卡片互不干扰
  - 降级：卡片更新超时 → "处理中" → 异步推送结果
- [ ] Docker Compose 集成
  - proxy + costrict-web 联合启动
  - 健康检查验证

---

## 依赖关系图

```
一（脚手架）
├── 二（WS 连接管理）──┐
├── 三（路由引擎）─────┤
├── 五（后端客户端）───┤
├── 六（消息去重）─────┤
│                      │
│                  四（协议翻译，依赖二）
│                      │
├── 二~六 ──────► 七（HTTP API Server）
│                      │
│                  八（消息分发管道，依赖二三四五六）
│                      │
└── 七 ───────► 九（costrict-web 适配器，可与八并行）
                       │
                   八 + 九 ──► 十（集成测试）
```
