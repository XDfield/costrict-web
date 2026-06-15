# 6. 渠道接入

## 6.1 设计目标

复用 costrict-web 现有的 channel 适配器框架，**不需要新建渠道**。ClawAgent 实现 `channel.MessageHandler` 接口，接收所有渠道的入站消息并回复。

## 6.2 Channel Handler 接口

costrict-web 已定义标准的消息处理接口（`server/internal/channel/adapter.go`）：

```go
type MessageHandler interface {
    Handle(ctx context.Context, msg *InboundMessage, sender Sender) error
}

type Sender interface {
    Send(ctx context.Context, content string) error
    SendMessage(ctx context.Context, msg OutboundMessage) error
    ReplyContext() ReplyContext
}
```

当前 `main.go` 传入的是 `&channel.NoopMessageHandler{}`（消息被丢弃）。本方案替换为 `ClawAgentRuntime`。

## 6.3 ClawAgent Handler 实现

```go
// server/internal/clawagent/handler.go

func (rt *ClawAgentRuntime) Handle(
    ctx context.Context,
    msg *channel.InboundMessage,
    sender channel.Sender,
) error {
    // 1. 获取 userID（从 channel 绑定的用户）
    userID := rt.resolveUserID(msg)
    if userID == "" {
        return sender.Send(ctx, "抱歉，无法识别您的身份。")
    }

    // 2. 构造 sessionID（渠道维度隔离）
    sessionID := fmt.Sprintf("%s:%s:%s",
        msg.ChannelType, msg.ExternalChatID, msg.ExternalUserID)

    // 3. 调用 Runner
    userMessage := model.NewUserMessage(msg.Content)
    eventCh, err := rt.runner.Run(ctx, userID, sessionID, userMessage)
    if err != nil {
        return fmt.Errorf("agent run failed: %w", err)
    }

    // 4. 异步消费事件并回复
    go rt.streamResponse(ctx, eventCh, sender)
    return nil
}

func (rt *ClawAgentRuntime) streamResponse(
    ctx context.Context,
    eventCh <-chan *event.Event,
    sender channel.Sender,
) {
    var buf strings.Builder
    var lastFlush time.Time

    flush := func() {
        if buf.Len() > 0 {
            sender.Send(ctx, buf.String())
            buf.Reset()
            lastFlush = time.Now()
        }
    }

    for evt := range eventCh {
        if evt.Error != nil {
            sender.Send(ctx, fmt.Sprintf("⚠️ %s", evt.Error.Message))
            continue
        }
        if evt.IsFinalResponse() {
            flush()
            break
        }
        // 累积文本，按句号/换行刷新
        if evt.Response != nil && len(evt.Response.Choices) > 0 {
            buf.WriteString(evt.Response.Choices[0].Delta.Content)
            if buf.Len() > 500 || time.Since(lastFlush) > 2*time.Second {
                flush()
            }
        }
    }
}
```

## 6.4 企微长连接机器人

costrict-web 已有完整的企微长连接机器人支持：

```
wecom-bot-proxy (独立进程)
    ↕ WebSocket
企微服务器 (qyapi.weixin.qq.com)
    ↕ HTTP API
costrict-web server
    → channel/adapters/wecom-bot/
    → ClawAgentRuntime.Handle()
```

**无需额外开发**：wecom-bot-proxy 已处理 WebSocket 连接、消息去重、路由表。channel adapter 已处理消息解析和回复。

ClawAgent 只需实现 `MessageHandler` 接口即可接入企微消息流。

### 企微消息路由

| 消息来源 | ExternalChatType | sessionID 构造 | 说明 |
|---------|-----------------|---------------|------|
| 单聊 | "single" | `wecom-bot:{chatID}:{userID}` | 一对一私聊 |
| 群聊 | "group" | `wecom-bot:{chatID}:group` | 群内共享 session |

### 企微通知场景的 AI 化处理（核心特性）

现有的企微通知场景（权限请求、问卷）由 `server/internal/dispatcher/` 通过按钮卡片处理，本提案将这部分**升级为 AI 驱动的自然语言交互**，作为 ClawAgent 的核心特性：

| 通知场景 | 原实现（按钮卡片） | AI 驱动实现（自然语言） |
|---------|------------------|----------------------|
| 权限请求 | `sendApprovalCard` 发送批准/拒绝/自批准按钮 | AI 描述权限请求，用户用自然语言回复批准/拒绝 |
| 问卷 | `sendVoteCards` 发送投票选项按钮 | AI 转述问题，用户用自然语言回答 |
| 权限批处理 | `sendSessionNoticeCard` 跳转链接 | AI 汇总权限列表，批量自然语言处理 |

**关键设计**：
- `Dispatcher` 新增 `EventForwarder`，将事件转发到 ClawAgent Runtime
- ClawAgent Runtime 新增 `EventHandler`，构造自然语言描述并注入 AI 对话
- AI 识别用户意图后，通过 `DeviceProxyClient` 调用 `/api/v1/permissions/{id}/reply` 或 `/api/v1/questions/{id}/reply`
- 保留传统按钮卡片作为降级方案（AI 不可用或用户禁用时回退）

详细设计见 [ai-driven-notification-handling.md](./ai-driven-notification-handling.md)。

## 6.5 Web Chat 渠道

通过新增 REST API + SSE 接口支持浏览器直接对话（非 IM 渠道）：

```
POST /api/clawagent/chat       → 提交消息，返回流式 SSE
GET  /api/clawagent/sessions   → 列出会话
GET  /api/clawagent/history/:id → 获取会话历史
```

详细 API 见 [08-api.md](./08-api.md)。

## 6.6 OpenAI 兼容 API

trpc-agent-go 自带 OpenAI 兼容 Server（`server/openai/`），可直接暴露：

```go
// server/internal/clawagent/openai_server.go

func (rt *ClawAgentRuntime) SetupOpenAIServer() http.Handler {
    srv, _ := openai.New(
        openai.WithRunner(rt.runner),
        openai.WithSessionService(rt.sessionSvc),
        openai.WithModelName("clawagent"),
    )
    return srv.Handler()
}
```

第三方客户端（如 ChatBox、OpenCat）可直接对接 `POST /v1/chat/completions`。

认证方式：Bearer token（用户的 Casdoor JWT 或专用 API Key）。

## 6.7 渠道对比

| 渠道 | 接入方式 | 当前状态 | 工作量 |
|------|---------|---------|--------|
| 企微应用 (wecom) | webhook 适配器 | ✅ 已有 | 零 |
| 企微长连接机器人 (wecom-bot) | proxy + 适配器 | ✅ 已有 | 零 |
| 微信个人号 (wechat) | polling 适配器 | ✅ 已有 | 零 |
| Web Chat | REST + SSE | ❌ 需新增 | 小 |
| OpenAI 兼容 | trpc-agent-go server | ❌ 需封装 | 小 |
