# 12. Session 设计与刷新机制

> **状态**：补充设计（基于 openclaw session 模型对比）
>
> **参考**：`D:\DEV\openclaw\src\routing\session-key.ts`、`D:\DEV\openclaw\src\config\sessions\reset.ts`、`D:\DEV\openclaw\src\config\sessions\store-maintenance.ts`
>
> **对应阶段**：影响 P0 / P4，新增 P4.5

## 12.1 三个 session 层级

ClawAgent 实际涉及三层 "session"，本章聚焦第 1 层（AgentSession）：

| 层级 | 存储 | 说明 |
|------|------|------|
| **AgentSession** | `clawagent_sessions`（trpc-agent-go） | Agent 与用户的对话上下文 |
| **DeviceConversation** | cs-cloud conversations API | 委托到设备端的任务执行会话 |
| **ChannelRoute** | channel adapter（已有） | IM 渠道发送目标的 last route 元数据 |

## 12.2 现状 gap（对比 openclaw）

参照 openclaw 的成熟 session 模型，提案当前在以下维度存在缺失：

| 维度 | openclaw 实现 | 当前提案 | gap 影响 |
|------|--------------|---------|---------|
| sessionID 命名 | `agent:{agentId}:{rest}` 可正反解析 | `{channelType}:{chatID}:{userID}` | 无法在 key 内编码 thread / type / version |
| freshness 判定 | `evaluateSessionFreshness` 双模式 | 无 | session 永久 fresh，长期对话 token 失控 |
| reset 机制 | daily（默认 4am）/ idle（默认 30min）双模式 + per-type 配置 | 无 | 上下文无限累积，跨日话题混淆 |
| reset 归档 | archive 旧 session，按 retention 保留 | 无 | - |
| prune | 30 天自动清理归档（`DEFAULT_SESSION_PRUNE_AFTER_MS`） | 无 | DB 无限增长 |
| compaction | `rotateBytes=10MB` 触发文件 rotate | 无 | `session_data` JSONB 撑爆 LLM context |
| 容量上限 | `maxEntries=500` 按 updatedAt 删最旧 | 无 | 单用户 session 数失控 |
| per-type 策略 | direct/group/thread 独立 reset 配置 | 无 | 群聊活跃 session 被单聊策略误伤 |
| thread 支持 | `{base}:thread:{threadId}` | 无 | IM 群 thread 污染主群上下文 |
| identity link | `resolveLinkedPeerId` 跨渠道合并 | 无 | 同一用户跨 IM 渠道是不同 session（可接受） |

## 12.3 推荐设计

### 12.3.1 sessionID 命名规范

参考 openclaw 的可解析结构，加 `agent:clawagent:` 前缀，支持版本后缀：

```
agent:clawagent:{channelType}:{chatID}:{userID}                 # 单聊
agent:clawagent:{channelType}:{chatID}:group                     # 群聊
agent:clawagent:{channelType}:{chatID}:group:thread:{threadId}   # 群内 thread
agent:clawagent:event:{eventType}:{eventID}                      # 通知专用（P5）
agent:clawagent:task:{taskID}                                    # 委托任务回传专用（P4）
```

**好处**：
- 与未来其他 agent runtime 共存（app_name 维度隔离）
- 正则可解析 type / chatID / userID，便于审计与调试
- 与 trpc-agent-go `session/postgres` 后端的 `(app_name, session_id)` 二元主键兼容
- 末尾可加 `:v{N}` 版本号实现 reset（见 12.3.3）

### 12.3.2 业务元数据表 `agent_session_meta`

trpc-agent-go 的 `clawagent_sessions` 只存 session_data。业务元数据（freshness / version / archive 状态）自建：

```sql
CREATE TABLE agent_session_meta (
    session_id      VARCHAR(255) PRIMARY KEY,   -- 含 :v{N} 后缀
    user_id         VARCHAR(255) NOT NULL,
    base_key        VARCHAR(255) NOT NULL,       -- 不含版本号的部分
    version         INTEGER NOT NULL DEFAULT 1,
    reset_type      VARCHAR(20) NOT NULL,        -- direct/group/thread/event/task
    last_message_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    message_count   INTEGER NOT NULL DEFAULT 0,
    token_estimate  INTEGER NOT NULL DEFAULT 0,
    is_archived     BOOLEAN NOT NULL DEFAULT FALSE,
    archived_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(user_id, base_key, version)
);

CREATE INDEX idx_session_meta_user_lastmsg ON agent_session_meta(user_id, last_message_at DESC);
CREATE INDEX idx_session_meta_base_active  ON agent_session_meta(base_key) WHERE is_archived = FALSE;
```

### 12.3.3 freshness 与版本化 reset

Handler 入口检查 freshness，stale 时 bump 版本号（参照 openclaw `evaluateSessionFreshness`）：

```go
func (rt *ClawAgentRuntime) resolveActiveSession(
    userID, baseKey string,
) (string, error) {
    meta, err := rt.sessionMeta.Active(userID, baseKey)
    if err == ErrNotFound {
        // 首次：创建 v1
        sid := fmt.Sprintf("%s:v1", baseKey)
        rt.sessionMeta.Create(userID, baseKey, 1, resetTypeOf(baseKey))
        return sid, nil
    }
    if rt.isStale(meta, time.Now()) {
        // bump version：旧 session 归档，新建 v(N+1)
        rt.sessionMeta.Archive(meta.SessionID)
        newVer := meta.Version + 1
        sid := fmt.Sprintf("%s:v%d", baseKey, newVer)
        rt.sessionMeta.Create(userID, baseKey, newVer, meta.ResetType)
        return sid, nil
    }
    return meta.SessionID, nil
}

func (rt *ClawAgentRuntime) isStale(meta *SessionMeta, now time.Time) bool {
    switch meta.ResetType {
    case "direct":
        // daily reset（默认 4am 当地时间）
        return meta.LastMessageAt.Before(dailyResetAt(now, rt.cfg.Session.DailyResetHour))
    case "group", "thread":
        // idle reset（默认 30min）
        return now.Sub(meta.LastMessageAt) > rt.cfg.Session.GroupIdleMinutes*time.Minute
    case "event":
        // idle reset（默认 60min）
        return now.Sub(meta.LastMessageAt) > rt.cfg.Session.EventIdleMinutes*time.Minute
    case "task":
        // idle reset（默认 120min）
        return now.Sub(meta.LastMessageAt) > rt.cfg.Session.TaskIdleMinutes*time.Minute
    }
    return false
}
```

**关键**：stale 时**不删除** session_data，只归档（保留以便审计），物理删除交给 prune 阶段。

**每轮对话后的元数据更新**：每次 `runner.Run()` 完成一轮对话（final response 后），handler 在 goroutine 池中异步更新：

```go
func (rt *ClawAgentRuntime) updateSessionMeta(ctx context.Context, sid string) {
    // message_count +1
    rt.sessionMeta.IncrementMessageCount(sid)
    // 用简单启发式更新 token_estimate
    data := rt.sessionSvc.Get(ctx, sid)
    est := estimateTokens(data)     // len(json) / 4，不需精确
    rt.sessionMeta.UpdateTokenEstimate(sid, est)
}
```

`estimateTokens()` 不做精确 tokenize（避免 hot path 开销），用 `len(json_bytes) / 4` 粗估，足够触发 compaction 阈值判断。

### 12.3.4 session_data 压缩（compaction）

trpc-agent-go 的 session_data JSONB 会无限增长。在 token 阈值触发 LLM 总结。

#### 12.3.4.1 触发时机

每轮 final response 后，与 memory refresh 并行（同一 goroutine 池），调用 `maybeCompact()`。只在 `TokenEstimate >= MaxSessionTokens` 时实际执行。

#### 12.3.4.2 Token 估算

**不在 hot path 上做精确 tokenize**。每轮对话后 handler 调 `updateSessionMeta()` 用 `len(json_bytes) / 4` 做粗略估算（对英文为主的对话误差 < 30%，足够触发判定）。如需更精确可在配置中切换为 tiktoken-go。

估算值写入 `agent_session_meta.token_estimate`，compaction 后重新估算并更新。

#### 12.3.4.3 LLM 总结范围

`SummarizeSession()` **只压缩对话历史（session_data JSONB），不碰 system prompt**。system prompt 由 `runner.Run()` 在每次调用时通过 `BuildInstruction(persona, memory)` 动态生成，与持久化的 session_data 无关。

**"KeepLastNMessages" 的定义**：N = 最近 N 轮完整的 user + assistant 对话回合（turn pair）。一轮 turn pair 包括 user 输入、中间 tool_call → tool_result 循环、最终 assistant 回复，视为一个消息单元。默认 `compaction_keep_recent_messages: 10`，即保留最近 10 轮对话原文，之前的内容 LLM 总结。

#### 12.3.4.4 并发安全

compaction goroutine 与下一轮用户消息可能存在竞态。采用**乐观锁**：

```go
func (rt *ClawAgentRuntime) maybeCompact(ctx context.Context, sid string) (bool, error) {
    meta, err := rt.sessionMeta.Get(sid)
    if err != nil || meta.TokenEstimate < rt.cfg.Session.MaxSessionTokens {
        return false, nil  // 无需压缩
    }

    // 1. 快照当前 message_count
    beforeMsgCount := meta.MessageCount

    // 2. 读取 session_data
    data := rt.sessionSvc.Get(ctx, sid)

    // 3. 二次检查：如果此时已有新消息写入，放弃本次压缩
    currentMeta, _ := rt.sessionMeta.Get(sid)
    if currentMeta.MessageCount != beforeMsgCount {
        return false, nil  // 有新消息到达，下轮 final response 再试
    }

    // 4. LLM 总结（保留最近 N 轮 turn pair）
    summary, err := rt.llm.SummarizeSession(ctx, data,
        KeepTurns(rt.cfg.Session.CompactionKeepRecent),
    )
    if err != nil {
        return false, fmt.Errorf("llm summarize: %w", err)
    }

    // 5. 原子替换
    rt.sessionSvc.Replace(ctx, sid, summary)
    rt.sessionMeta.UpdateTokenEstimate(sid, estimateTokens(summary))
    return true, nil
}
```

**竞态窗口**：仅在步骤 2~3 之间（一次 DB 读）。用户消息在步骤 2 之后写入 → `message_count` 变化 → 放弃本次压缩 → 下轮 final response 重试。无需 per-session 锁。

#### 12.3.4.5 压缩后的数据格式

`session_data` JSONB 在 trpc-agent-go 中是一个事件数组 `[]event.Event`。压缩后输出**同结构**的事件数组，LLM 将旧事件浓缩为一条 `type: "session_summary"` 的事件：

```json
[
  {
    "id": "evt_summary_xxx",
    "type": "session_summary",
    "timestamp": "2026-06-16T10:00:00Z",
    "payload": {
      "summary": "用户询问了项目部署方案，讨论了 Docker 和 k8s 的选择，最终决定使用 Docker Compose。用户偏好 PostgreSQL 15，要求配置自动备份。",
      "original_events": 24,
      "compacted_at": "2026-06-16T10:00:00Z"
    }
  },
  {
    "id": "evt_026",
    "type": "user",
    "timestamp": "2026-06-16T10:01:00Z",
    "payload": { "content": "帮我检查下 docker-compose.yml" }
  },
  // ... 后续 KeepTurns 条事件原文保留
]
```

trpc-agent-go runner 对 `session_summary` 类型的 event 不做特殊处理（不抛出、不回放），仅作为上下文透传给 LLM。LLM 看到总结内容后可据此理解前文，无需完整历史。

**退路**：如果 LLM 输出的 JSON 结构不符合预期，保留原数据、记一条 warning 日志、下轮重试。

#### 12.3.4.6 失败容错

| 失败场景 | 行为 |
|----------|------|
| LLM 总结超时或返回非 JSON | 保留原数据，记 warning，等下一轮 final response 重试 |
| 并发写入检测到 message_count 变化 | 静默放弃，等下一轮 |
| `Replace()` 写入失败 | 保留旧数据，meta 表 token_estimate 不变，下轮重试 |
| `estimateTokens()` 在 compaction 后估算出错 | 不阻塞，下轮重新估算后可能触发再次压缩 |

**最重要的是：compaction 失败永不导致对话中断或数据丢失**。

#### 12.3.4.7 压缩后上下文大小

一次典型的 compaction：

| 指标 | 压缩前 | 压缩后 |
|------|--------|--------|
| session_data token 数 | ~8,000 | ~1,200 ~ ~2,500（10 条原文 + 总结） |
| 事件数 | ~40~60 | ~12~15（1 条 summary + 10 条原文 + 边界） |
| LLM 调用代价 | — | 1 次 SummarizeSession（输入 ~8k / 输出 ~1k） |

#### 12.3.4.8 后续可选项（P5+）

参考 openclaw 的 `/compact` 命令和 Pi runtime overflow 回压：

- 手动 `/compact` 命令：用户主动触发压缩（携带自定义指令，例如 "重点保留架构决策"）
- context overflow 自动回压：如果 trpc-agent-go runner 检测到上下文溢出错误，自动触发紧急压缩并重试当前轮次（而非直接失败）
- compaction 前 memory flush：压缩前将当前关键上下文写入 workspace 持久化文件（替代 LLM summary 可能丢失的细节）

### 12.3.5 prune 与容量上限

后台 goroutine（每小时跑一次）：

```sql
-- 1. 删 30 天前的归档 session
DELETE FROM clawagent_sessions cs
USING agent_session_meta m
WHERE cs.session_id = m.session_id
  AND m.is_archived = TRUE
  AND m.last_message_at < NOW() - INTERVAL '30 days';

DELETE FROM agent_session_meta
WHERE is_archived = TRUE
  AND last_message_at < NOW() - INTERVAL '30 days';

-- 2. per-user 容量上限（默认 200）：超出则删最旧的归档
WITH ranked AS (
    SELECT session_id, user_id,
           ROW_NUMBER() OVER (PARTITION BY user_id ORDER BY last_message_at DESC) AS rn
    FROM agent_session_meta
    WHERE is_archived = TRUE
)
DELETE FROM agent_session_meta WHERE session_id IN (
    SELECT session_id FROM ranked WHERE rn > $1
);
```

参照 openclaw 默认值：prune=30 天，maxEntries=500（我们降到 200，因为用户量预期更大）。

### 12.3.6 announce 与 session 的协调（关键修复）

**这是当前提案最大的 gap**：`runner.Run(ctx, task.UserID, task.AgentSessionBaseKey, callbackMsg)` 在以下场景会失败或行为异常：

| 场景 | 当前提案行为 | 实际后果 |
|------|-------------|---------|
| session 已 archived（reset 后） | callback 注入到旧 archived session | 用户在新版本 session 里看不到结果 |
| session 被 prune | runner.Run 自动新建空 session | 上下文丢失，用户不知所云 |
| 用户已开新版本 session 时 announce 到达 | 注入到旧版本 | 同上 |

**修复方案**：baseKey + activeSessionID 解耦

1. 任务表字段 `agent_session_base_key`（详见 [10-database.md](./10-database.md#agent_workspace_tasks-表)，DDL 已采用此命名）：存 base_key（不含 `:v{N}` 后缀），announce 时动态解析当前 active 版本。

2. announce 时动态解析：

```go
func (rt *ClawAgentRuntime) announceToAgent(task *DelegationTask) {
    activeSID, err := rt.sessionMeta.ResolveActive(task.UserID, task.AgentSessionBaseKey)
    switch err {
    case nil:
        // 正常回传到当前 active session
    case ErrSessionArchived:
        // 旧版本已归档，但仍有新版本 active — 走 activeSID
        // 在 callbackMsg 里加任务上下文摘要，帮用户建立关联
    case ErrSessionPruned:
        // session 已彻底清理，无法回传
        rt.tasks.MarkDeliveryFailed(task.TaskID, "session pruned")
        return
    }
    // ... 用 activeSID 调 runner.Run
}
```

3. 前端展示：任务详情里同时展示 base_key 和当前 active session_id，让用户能追溯。

### 12.3.7 群聊的 Persona/Memory 决策

提案当前 `wecom-bot:{chatID}:group` 共享 session，但 Persona 和 Memory 是 per-userID 的——这块**没明确归属**。三个选项：

| 选项 | Persona 来源 | Memory 写入 | 优点 | 缺点 |
|------|-------------|------------|------|------|
| A 共享默认 | 平台 default | 不写入 | 简单 | 失去个性化 |
| **B 混合（推荐）** | default soulContent + 发送者 userContext | 按发送者 userID 单独写 | 个性化 + 共享上下文 | AgentFactory 需 sender 注入 |
| C 每 user 独立 | 每个发送者自己的 | 按发送者自己的 | 完全个性化 | 群内 @ 不同人 Agent 上下文断裂 |

推荐 **B**：群 sessionID 共享上下文，但每条 inbound 标注 `senderUserID`。AgentFactory 构造时：

```go
// 群聊场景
persona := personaMgr.LoadGroupDefault(ctx)  // 平台 default soulContent
senderUserCtx := personaMgr.LoadUserContext(ctx, senderUserID)
persona.SoulContent = groupDefaultPersona.SoulContent
persona.UserContext = senderUserCtx  // 仅替换 userContext，不替换 soulContent

// memory refresh 按 senderUserID 写
go rt.memoryMgr.Refresh(bgCtx, senderUserID, userMsg, assistantReply)
```

### 12.3.8 推荐配置项

```yaml
clawagent:
  session:
    daily_reset_hour: 4              # 单聊每天 4am reset
    group_idle_minutes: 30           # 群聊 30 分钟无活动 reset
    event_idle_minutes: 60           # 通知 session 60 分钟 reset
    task_idle_minutes: 120           # 任务回传 session 2 小时 reset
    prune_after_days: 30             # 30 天归档 session 清理
    max_sessions_per_user: 200       # 单用户 session 上限
    max_session_tokens: 8000         # 触发压缩的 token 上限
    compaction_keep_recent_messages: 10  # 压缩时保留最近 10 条
```

### 12.3.9 与 trpc-agent-go session 后端的边界

trpc-agent-go `session/postgres` 后端**只提供**：CRUD（按 session_id 存取 session_data JSONB API：`GetSession` / `CreateSession` / `AppendEvent`）。

**我们扩展的接口**（自建 wrapper）：

| 方法 | 用途 | 实现 |
|------|------|------|
| `Get(ctx, sid)` → JSONB | 读取完整 session_data | 直接映射到 `GetSession` |
| `Replace(ctx, sid, data)` | 原子替换 session_data（compaction 用） | `UPDATE clawagent_sessions SET session_data=$1, updated_at=NOW() WHERE session_id=$2` |
| `AppendEventWithEstimate(ctx, sess, evt)` | 追加事件 + 更新 token_estimate | 调用 `AppendEvent` 后异步 `updateSessionMeta()` |

**不提供**（我们自建在 `agent_session_meta` 表 + 业务逻辑）：
- freshness / reset 判定
- 版本管理
- compaction
- prune
- per-user 容量上限

## 12.4 roadmap 影响

| 阶段 | 增量 | 工时 |
|------|------|------|
| P0 基础骨架 | sessionID 命名规范（带前缀）+ `agent_session_meta` 表 + Handler 入口 freshness 检查 | +0.5 天 |
| P4 Workspace 委托 | 任务表字段语义化为 `agent_session_base_key`（DDL 已采用）+ announce 动态解析 active session | +0.5 天 |
| **新增 P4.5 Session 维护** | compaction goroutine + prune goroutine + 容量上限 + 配置项 + 监控指标 | +1 天 |

**总工期 21.5 → 23.5 天**

P4.5 详细任务：

- [ ] 创建 `agent_session_meta` 表 + migration
- [ ] 实现 `SessionMetaManager`（Create / Active / Archive / ResolveActive / Get）
- [ ] 实现 `resolveActiveSession()` + freshness 检查（daily / idle 双模式）
- [ ] Handler / EventHandler 入口接入 resolveActiveSession
- [ ] compaction goroutine（监听 final response → 估算 tokens → LLM 总结）
- [ ] prune goroutine（每小时扫描归档过期 session + per-user 容量）
- [ ] 群聊 Persona/Memory 混合模式实现（选项 B）
- [ ] 配置项 + 启动校验
- [ ] 监控指标（session 数 / 平均 tokens / reset 频率 / prune 量）

**验收标准**：
1. 单聊对话跨日（>4am）后，新消息自动开 v2 session，旧 v1 归档
2. 群聊 30 分钟无活动后 reset，通知 60 分钟无活动后 reset，任务回传 120 分钟无活动后 reset
3. session_data 超 8000 tokens 自动压缩，输出 `type: "session_summary"` 事件 + 保留最近 10 轮原文
4. 压缩期间新消息到达时，静默放弃本次压缩，下轮重试（不丢失数据）
5. 30 天归档 session 物理删除
6. 委托任务 announce 时，即使原 session 已 reset，仍能定位到当前 active session 回传结果
