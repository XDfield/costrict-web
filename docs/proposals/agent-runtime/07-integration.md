# 7. 与 trpc-agent-go 的集成点

## 7.1 Runner 集成

trpc-agent-go 的 `Runner` 是核心执行器，负责 Agent 编排、Session 管理和 Event 流转。

```go
// server/internal/agentrt/setup.go

func buildRunner(llmCfg *config.LLMConfig) (runner.ManagedRunner, error) {
    // 1. 创建 LLM Model（复用现有配置）
    modelInstance := openai.New(llmCfg.Model,
        openai.WithAPIKey(llmCfg.APIKey),
        openai.WithBaseURL(llmCfg.BaseURL),
    )

    // 2. 创建主 Agent + 子 Agents
    researcher := llmagent.New("researcher",
        llmagent.WithModel(modelInstance),
        llmagent.WithInstruction("你是一个专业的信息检索助手..."),
        llmagent.WithTools([]tool.Tool{webSearchTool, readFileTool}),
    )

    coder := llmagent.New("coder",
        llmagent.WithModel(modelInstance),
        llmagent.WithInstruction("你是一个代码编写助手..."),
        llmagent.WithTools([]tool.Tool{execTool, writeFileTool}),
    )

    // 3. 创建编排 Agent（主 Agent 可自主选择委托）
    orchestrator := llmagent.New("orchestrator",
        llmagent.WithModel(modelInstance),
        llmagent.WithInstruction("你是一个任务编排器，根据用户需求将任务分配给合适的专业 Agent..."),
        llmagent.WithSubAgents([]agent.Agent{researcher, coder}),
    )

    // 4. 创建 Runner
    r := runner.NewRunner("costrict-agent", orchestrator,
        runner.WithSessionService(session.NewInMemoryService()),
    )

    return r.(runner.ManagedRunner), nil
}
```

关键对接点：

| trpc-agent-go 接口 | 用途 |
|---------------------|------|
| `runner.Run(ctx, userID, sessionID, msg, opts...)` | 提交任务，返回 event channel |
| `runner.Cancel(requestID)` | 取消正在执行的任务 |
| `runner.RunStatus(requestID)` | 查询运行状态 |
| `agent.WithRequestID(id)` | 绑定 taskID 作为 requestID，便于追踪 |

## 7.2 Callback 集成

利用 trpc-agent-go 的 `BeforeAgentCallback` / `AfterAgentCallback` 拦截 Agent 执行过程：

```go
// server/internal/agentrt/callbacks.go

func registerCallbacks(ag *llmagent.LLMAgent, rt *AgentRuntime) {
    // 子 Agent 执行前：注册 subagent run
    ag.SetBeforeAgentCallback(func(ctx context.Context, args *agent.BeforeAgentArgs) (*agent.BeforeAgentResult, error) {
        agentName := args.Agent.Info().Name
        taskID := getTaskIDFromContext(ctx)

        rt.subagents.Register(RegisterSubagentParams{
            RequesterSessionKey: args.Invocation.SessionID(),
            AgentName:           agentName,
            TaskID:              taskID,
        })

        rt.eventBus.Publish(taskID, RuntimeEvent{
            Type:   EventSubagentSpawn,
            TaskID: taskID,
            Data:   map[string]interface{}{"agentName": agentName},
        })

        return nil, nil  // 不修改执行流
    })

    // 子 Agent 执行后：记录结果，触发 announce
    ag.SetAfterAgentCallback(func(ctx context.Context, args *agent.AfterAgentArgs) (*agent.AfterAgentResult, error) {
        agentName := args.Agent.Info().Name
        taskID := getTaskIDFromContext(ctx)
        result := args.Event.Content()

        rt.subagents.MarkEnded(runID, OutcomeOK, EndReasonComplete, &result)
        rt.tasks.Complete(childTaskID, result)

        go rt.announceToParent(childTask)

        return nil, nil
    })
}
```

## 7.3 Session 集成

trpc-agent-go 的 Session 管理对话历史和状态：

```go
// 开发阶段：使用内存 session
runner.WithSessionService(session.NewInMemoryService())

// 生产阶段：可实现 DB 持久化 session
type DBSessionService struct {
    db *gorm.DB
}

func (s *DBSessionService) GetSession(ctx context.Context, appName, userID, sessionID string) (*session.Session, error)
func (s *DBSessionService) CreateSession(ctx context.Context, appName, userID string) (*session.Session, error)
func (s *DBSessionService) AppendEvent(ctx context.Context, session *session.Session, event *event.Event) error
```

Session 与 TaskRecord 的关系：

- 每个 TaskRecord 通过 `RequesterSession` 关联到一个 trpc-agent-go Session
- 子任务通过 `ChildSession` 创建独立的 Session，隔离上下文
- 子任务完成后通过 announce 机制将结果注入主 Session

## 7.4 多 Agent 编排集成

根据业务场景选择不同的编排模式：

```go
// 链式编排：分析 → 生成 → 审核
pipeline := chainagent.New("review-pipeline",
    chainagent.WithSubAgents([]agent.Agent{analyzer, generator, reviewer}),
)

// 并行编排：同时搜索多个数据源
parallel := parallelagent.New("multi-search",
    parallelagent.WithSubAgents([]agent.Agent{webSearcher, dbSearcher, docSearcher}),
)

// 图式编排：条件分支 + 循环
sg := graph.NewStateGraph(schema)
sg.AddNode("classifier", classifyTask)
sg.AddNode("simple_agent", simpleAgent)
sg.AddNode("complex_agent", complexAgent)
sg.AddConditionalEdges("classifier",
    func(ctx context.Context, s graph.State) (string, error) {
        if s["complexity"].(string) == "high" {
            return "complex", nil
        }
        return "simple", nil
    },
    map[string]string{"simple": "simple_agent", "complex": "complex_agent"},
)

// 动态 Agent 创建（根据请求参数动态构建）
r := runner.NewRunnerWithAgentFactory("costrict-agent", "orchestrator",
    func(ctx context.Context, ro agent.RunOptions) (agent.Agent, error) {
        return buildAgentForRequest(ro)
    },
)
```
