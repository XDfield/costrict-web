# 3. 核心数据模型

## 3.1 TaskRecord — 任务记录

对标 openclaw `src/tasks/task-registry.types.ts` 中的 `TaskRecord`。

```go
// server/internal/agentrt/task_models.go

type TaskStatus string

const (
    TaskQueued    TaskStatus = "queued"     // 已入队，等待执行
    TaskRunning   TaskStatus = "running"    // 正在执行
    TaskSucceeded TaskStatus = "succeeded"  // 执行成功
    TaskFailed    TaskStatus = "failed"     // 执行失败
    TaskTimedOut  TaskStatus = "timed_out"  // 执行超时
    TaskCancelled TaskStatus = "cancelled"  // 用户取消
    TaskLost      TaskStatus = "lost"       // 异常丢失（进程崩溃等）
)

type TaskDeliveryStatus string

const (
    DeliveryPending       TaskDeliveryStatus = "pending"        // 结果待交付
    DeliveryDelivered     TaskDeliveryStatus = "delivered"       // 已交付给请求方
    DeliverySessionQueued TaskDeliveryStatus = "session_queued"  // 已排入 session 队列
    DeliveryFailed        TaskDeliveryStatus = "failed"          // 交付失败
    DeliveryNotApplicable TaskDeliveryStatus = "not_applicable"  // 无需交付（顶层任务）
)

type TaskRuntime string

const (
    RuntimeSubagent TaskRuntime = "subagent"  // 由 subagent 执行
    RuntimeUser     TaskRuntime = "user"      // 用户直接下发
    RuntimeCron     TaskRuntime = "cron"      // 定时任务触发
)

type TaskRecord struct {
    ID               uint               `gorm:"primaryKey"`
    TaskID           string             `gorm:"uniqueIndex;size:64"`
    Runtime          TaskRuntime        `gorm:"size:20;index"`
    RequesterSession string             `gorm:"size:128;index"`
    ChildSession     *string            `gorm:"size:128"`
    ParentTaskID     *string            `gorm:"size:64;index"`
    AgentID          string             `gorm:"size:64"`
    RequestID        string             `gorm:"size:64;index"` // trpc-agent-go ManagedRunner requestID
    UserID           string             `gorm:"size:64;index"`
    Task             string             `gorm:"type:text"`     // 任务描述/prompt
    Status           TaskStatus         `gorm:"size:20;index"`
    DeliveryStatus   TaskDeliveryStatus `gorm:"size:20"`
    ProgressSummary  *string            `gorm:"type:text"`     // 中间进度摘要
    TerminalSummary  *string            `gorm:"type:text"`     // 最终结果摘要
    Error            *string            `gorm:"type:text"`
    CreatedAt        time.Time          `gorm:"index"`
    StartedAt        *time.Time
    EndedAt          *time.Time
    LastEventAt      *time.Time
}
```

## 3.2 SubagentRunRecord — 子代理运行记录

对标 openclaw `src/agents/subagent-registry.types.ts` 中的 `SubagentRunRecord`。

```go
// server/internal/agentrt/subagent_models.go

type SubagentOutcome string

const (
    OutcomeOK      SubagentOutcome = "ok"
    OutcomeError   SubagentOutcome = "error"
    OutcomeTimeout SubagentOutcome = "timeout"
    OutcomeKilled  SubagentOutcome = "killed"
)

type SubagentEndReason string

const (
    EndReasonComplete     SubagentEndReason = "complete"
    EndReasonError        SubagentEndReason = "error"
    EndReasonKilled       SubagentEndReason = "killed"
    EndReasonTimeout      SubagentEndReason = "timeout"
    EndReasonParentCancel SubagentEndReason = "parent_cancel"
)

type SubagentRunRecord struct {
    ID                  uint            `gorm:"primaryKey"`
    RunID               string          `gorm:"uniqueIndex;size:64"`
    ChildSessionKey     string          `gorm:"size:128;index"`
    RequesterSessionKey string          `gorm:"size:128;index"`
    TaskID              string          `gorm:"size:64;index"` // 关联 TaskRecord.TaskID
    RequestID           string          `gorm:"size:64"`       // 关联 trpc-agent-go requestID
    AgentName           string          `gorm:"size:64"`       // 执行的 agent 名称
    Task                string          `gorm:"type:text"`     // 委托的任务描述
    Outcome             *SubagentOutcome   `gorm:"size:20"`
    EndReason           *SubagentEndReason `gorm:"size:20"`
    FrozenResultText    *string         `gorm:"type:text"`     // 冻结的最终输出
    AnnounceRetryCount  int             `gorm:"default:0"`
    CreatedAt           time.Time
    StartedAt           *time.Time
    EndedAt             *time.Time
    AnnouncedAt         *time.Time                             // 结果已通知主 agent 的时间
}
```

## 3.3 状态机定义

### 任务状态机

```
                  ┌─────────────────────────┐
                  │                         ▼
  ──► queued ──► running ──► succeeded
                  │   │
                  │   ├──► failed
                  │   │
                  │   └──► timed_out
                  │
                  └──────────► cancelled
                  
  任何非终态 ──────────────► lost (进程崩溃恢复时标记)
```

终态判定：

```go
func IsTerminalStatus(s TaskStatus) bool {
    return s == TaskSucceeded || s == TaskFailed ||
           s == TaskTimedOut || s == TaskCancelled || s == TaskLost
}
```

### 结果交付状态机

```
  pending ──► delivered        (直接交付成功)
          ├─► session_queued   (排入 session 等待主 agent 消费)
          ├─► failed           (交付失败，超过重试上限)
          └─► not_applicable   (顶层任务，无需交付)
```

### Subagent 生命周期

```
  registered ──► started ──► ended ──► announced ──► cleaned_up
                               │
                               └─► announce_retry (最多 5 次，指数退避)
                                       │
                                       └─► give_up (超时或超限)
```
