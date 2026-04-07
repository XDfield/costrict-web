# 4. 核心组件设计

## 4.1 AgentRuntime — 运行时入口

AgentRuntime 是整个 Agent Runtime Layer 的胶水层，负责：
- 管理 trpc-agent-go Runner 实例
- 协调 TaskRegistry 和 SubagentRegistry
- 消费 Runner event channel 并广播到 EventBus
- 驱动子任务完成后的 announce 回调

```go
// server/internal/agentrt/runtime.go

type AgentRuntime struct {
    runner       runner.ManagedRunner   // trpc-agent-go Runner
    tasks        *TaskRegistry
    subagents    *SubagentRegistry
    eventBus     *EventBus
    db           *gorm.DB
    llmConfig    *config.LLMConfig     // 复用现有 LLM 配置
}

// 初始化
func NewAgentRuntime(db *gorm.DB, llmCfg *config.LLMConfig) (*AgentRuntime, error)

// 核心方法
func (rt *AgentRuntime) SubmitTask(ctx context.Context, params SubmitTaskParams) (*TaskRecord, error)
func (rt *AgentRuntime) GetTaskStatus(taskID string) (*TaskRecord, error)
func (rt *AgentRuntime) CancelTask(taskID string) error
func (rt *AgentRuntime) ListTasks(userID string, opts ListTasksOptions) ([]*TaskRecord, error)
func (rt *AgentRuntime) SubscribeEvents(taskID string) (<-chan RuntimeEvent, func())

// 内部方法
func (rt *AgentRuntime) consumeEvents(eventCh <-chan *event.Event, taskID string)
func (rt *AgentRuntime) announceToParent(childTask *TaskRecord)
func (rt *AgentRuntime) recoverLostTasks()  // 启动时恢复丢失任务
```

## 4.2 TaskRegistry — 任务注册表

对标 openclaw `src/tasks/task-registry.ts`，负责任务 CRUD 和状态流转。

```go
// server/internal/agentrt/task_registry.go

type TaskRegistry struct {
    mu    sync.RWMutex
    db    *gorm.DB
    hooks []TaskHookFunc  // 状态变更钩子
}

// 生命周期操作
func (r *TaskRegistry) Create(params CreateTaskParams) (*TaskRecord, error)
func (r *TaskRegistry) MarkRunning(taskID, requestID string) error
func (r *TaskRegistry) UpdateProgress(taskID, summary string) error
func (r *TaskRegistry) Complete(taskID string, summary string) error
func (r *TaskRegistry) Fail(taskID string, errMsg string) error
func (r *TaskRegistry) Cancel(taskID string) error
func (r *TaskRegistry) MarkLost(taskID string) error

// 交付管理
func (r *TaskRegistry) MarkDelivered(taskID string) error
func (r *TaskRegistry) MarkDeliveryFailed(taskID string) error

// 查询
func (r *TaskRegistry) Get(taskID string) (*TaskRecord, error)
func (r *TaskRegistry) ListByUser(userID string, opts ListTasksOptions) ([]*TaskRecord, error)
func (r *TaskRegistry) ListByParent(parentTaskID string) ([]*TaskRecord, error)
func (r *TaskRegistry) FindByRequestID(requestID string) (*TaskRecord, error)

// 钩子
type TaskHookFunc func(task *TaskRecord, oldStatus, newStatus TaskStatus)
func (r *TaskRegistry) OnStateChange(hook TaskHookFunc)
```

## 4.3 SubagentRegistry — 子代理注册表

对标 openclaw `src/agents/subagent-registry.ts`，管理 subagent 运行记录和层级关系。

```go
// server/internal/agentrt/subagent_registry.go

type SubagentRegistry struct {
    mu   sync.RWMutex
    runs map[string]*SubagentRunRecord  // runID -> record（内存索引）
    db   *gorm.DB
}

// 生命周期
func (r *SubagentRegistry) Register(params RegisterSubagentParams) (*SubagentRunRecord, error)
func (r *SubagentRegistry) MarkStarted(runID string) error
func (r *SubagentRegistry) MarkEnded(runID string, outcome SubagentOutcome, reason SubagentEndReason, result *string) error
func (r *SubagentRegistry) MarkAnnounced(runID string) error

// 查询
func (r *SubagentRegistry) Get(runID string) (*SubagentRunRecord, error)
func (r *SubagentRegistry) GetByChildSession(childSessionKey string) (*SubagentRunRecord, error)
func (r *SubagentRegistry) ListByRequester(requesterSessionKey string) ([]*SubagentRunRecord, error)
func (r *SubagentRegistry) CountActiveByRequester(requesterSessionKey string) int

// 恢复
func (r *SubagentRegistry) LoadFromDB() error                      // 启动时从 DB 加载到内存
func (r *SubagentRegistry) PendingAnnouncements() []*SubagentRunRecord  // 需要重试通知的记录
```

## 4.4 EventBus — 事件总线

用于将 trpc-agent-go event channel 中的事件扇出到多个订阅者（SSE/WebSocket 连接）。

```go
// server/internal/agentrt/event_bus.go

type RuntimeEventType string

const (
    EventTaskCreated    RuntimeEventType = "task.created"
    EventTaskRunning    RuntimeEventType = "task.running"
    EventTaskProgress   RuntimeEventType = "task.progress"
    EventTaskCompleted  RuntimeEventType = "task.completed"
    EventTaskFailed     RuntimeEventType = "task.failed"
    EventTaskCancelled  RuntimeEventType = "task.cancelled"
    EventToolCall       RuntimeEventType = "tool.call"
    EventToolResult     RuntimeEventType = "tool.result"
    EventAgentResponse  RuntimeEventType = "agent.response"
    EventSubagentSpawn  RuntimeEventType = "subagent.spawn"
    EventSubagentDone   RuntimeEventType = "subagent.done"
)

type RuntimeEvent struct {
    Type      RuntimeEventType       `json:"type"`
    TaskID    string                 `json:"taskId"`
    Timestamp time.Time              `json:"timestamp"`
    Data      map[string]interface{} `json:"data,omitempty"`
}

type EventBus struct {
    mu          sync.RWMutex
    subscribers map[string]map[string]chan RuntimeEvent  // taskID -> subID -> channel
}

func NewEventBus() *EventBus
func (eb *EventBus) Subscribe(taskID string) (ch <-chan RuntimeEvent, unsubscribe func())
func (eb *EventBus) Publish(taskID string, event RuntimeEvent)
func (eb *EventBus) Close(taskID string)  // 关闭某任务的所有订阅
```
