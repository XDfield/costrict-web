# AI 驱动的企微通知处理设计方案（核心特性）

> **状态**：ClawAgent 核心特性（P5 阶段，不可省略）
>
> **对应阶段**：见 [11-roadmap.md](./11-roadmap.md) P5
>
> **依赖**：P4 Workspace 委托 + DeviceProxyClient

## 概述

将现有的企微通知场景（权限请求、问卷）从传统的按钮卡片交互转换为 AI 驱动的自然语言交互，让用户能够通过自然语言与系统沟通，由 AI 自动处理相应的权限和问卷事件。

**核心价值**：体现 ClawAgent 从"被动响应消息"升级为"主动理解意图"的能力跃迁，是本提案区别于传统 IM bot 的关键差异化能力。

## 当前实现分析

### 现有交互流程

```
设备端事件 → Dispatcher → 企微卡片 → 用户点击按钮 → ActionCallback → 设备端API
```

### 现有事件类型

1. **权限请求** (`permission`)
   - 卡片：批准 / 拒绝 / 自批准 按钮
   - 回调：`/api/v1/permissions/{id}/reply`

2. **问卷** (`question`) 
   - 卡片：选项按钮（单题）或跳转链接（多题）
   - 回调：`/api/v1/questions/{id}/reply`

### 现有局限性

1. **交互僵化**：用户只能选择预设选项，无法表达复杂意图
2. **多轮处理差**：无法处理需要澄清或补充信息的场景
3. **上下文缺失**：每次交互独立，无法利用历史对话理解用户意图
4. **批量处理低效**：多个权限/问卷需要多次操作

## AI 驱动的交互设计

### 目标交互流程

```
设备端事件 → Dispatcher → AI Runtime → AI生成自然语言问题 
→ 用户自然语言回复 → AI理解意图 → 自动调用设备端API
```

### 核心优势

1. **自然交互**：用户可用自然语言表达复杂意图
2. **智能理解**：AI 能理解模糊表达，主动澄清
3. **批量处理**：可一次性处理多个相关事件
4. **上下文记忆**：利用历史对话理解用户偏好
5. **个性化响应**：基于用户画像提供定制化建议

## 架构设计

### 系统架构

```
┌─ 设备端 (cs-cloud) ──────────────────────────────────────┐
│  发送事件: permission / question                            │
└────────────┬───────────────────────────────────────────────┘
             │ 现有 SSE 事件流
             ↓
┌───────────────────────────────────────────────────────────┐
│  Dispatcher (现有扩展)                                     │
│  ├── 新增: AI交互模式检测                                   │
│  └── 扩展: 事件转发到 AI Runtime                           │
└────────────┬───────────────────────────────────────────────┘
             │ 新增: 事件转发接口
             ↓
┌───────────────────────────────────────────────────────────┐
│  ClawAgent Runtime (AI Runtime)                            │
│  ├── 事件理解模块 (Event Comprehension)                     │
│  ├── 意图识别模块 (Intent Recognition)                      │
│  ├── 对话管理模块 (Conversation Management)                 │
│  └── 设备代理模块 (Device Proxy)                            │
└────────────┬───────────────────────────────────────────────┘
             │ 自然语言交互
             ↓
┌───────────────────────────────────────────────────────────┐
│  企微渠道 (现有适配器)                                      │
│  └── 接收用户自然语言回复                                    │
└────────────┬───────────────────────────────────────────────┘
             │ 用户回复
             ↓
┌───────────────────────────────────────────────────────────┐
│  AI Runtime 处理                                           │
│  ├── 理解用户意图                                          │
│  ├── 必要时澄清问题                                         │
│  ├── 调用设备端 API                                         │
│  └── 反馈处理结果                                          │
└───────────────────────────────────────────────────────────┘
```

### 关键组件

#### 1. 事件转发器 (Event Forwarder)

在 `server/internal/dispatcher/dispatcher.go` 中扩展：

```go
type EventForwarder struct {
    aiRuntimeClient *AIRuntimeClient
}

func (f *EventForwarder) ShouldUseAIInteraction(userID string, eventType string) bool {
    // 检查用户是否启用了 AI 交互模式
    // 优先级: 用户设置 > 系统默认
}

func (f *EventForwarder) ForwardToAI(input DispatchInput) error {
    // 将事件转发到 AI Runtime
    aiRequest := AIEventRequest{
        UserID:      input.UserID,
        EventType:   input.EventType,
        SessionID:   input.SessionID,
        DeviceID:    input.DeviceID,
        ActionData:  input.ActionData,
        Path:        input.Path,
    }
    return f.aiRuntimeClient.SendEvent(aiRequest)
}
```

#### 2. AI Runtime 事件接口

在 `server/internal/clawagent/` 中新增事件处理：

```go
// server/internal/clawagent/event_handler.go

type EventHandler struct {
    runtime *ClawAgentRuntime
}

func (h *EventHandler) HandleAIEvent(ctx context.Context, req AIEventRequest) error {
    // 1. 构造事件描述
    eventDesc := h.describeEvent(req)
    
    // 2. 注入到 AI 对话
    message := fmt.Sprintf("[系统事件] %s\n请根据用户偏好和上下文，用自然语言向用户说明此情况，并询问如何处理。", eventDesc)
    
    // 3. 调用 AI 生成回复
    sessionID := fmt.Sprintf("event:%s:%s", req.EventType, req.SessionID)
    userMessage := model.NewUserMessage(message)
    
    eventCh, err := h.runtime.runner.Run(ctx, req.UserID, sessionID, userMessage)
    // ... 流式回复到企微
    
    return nil
}

func (h *EventHandler) describeEvent(req AIEventRequest) string {
    switch req.EventType {
    case "permission":
        return h.describePermission(req)
    case "question":
        return h.describeQuestion(req)
    default:
        return fmt.Sprintf("未知事件类型: %s", req.EventType)
    }
}

func (h *EventHandler) describePermission(req AIEventRequest) string {
    // 解析权限详情
    permType := req.ActionData["permission"].(string)
    cmd := req.ActionData["command"].(string)
    return fmt.Sprintf("设备端请求执行 %s 权限: %s", permType, cmd)
}

func (h *EventHandler) describeQuestion(req AIEventRequest) string {
    // 解析问题详情
    question := req.ActionData["question"].(string)
    options := req.ActionData["options"].([]string)
    return fmt.Sprintf("设备端提问: %s\n选项: %v", question, options)
}
```

#### 3. 意图识别与设备调用

```go
// server/internal/clawagent/intent_handler.go

type IntentHandler struct {
    deviceProxy *DeviceProxyClient
}

func (h *IntentHandler) HandleUserResponse(ctx context.Context, userID string, response string, eventContext *EventContext) error {
    // 1. 让 AI 分析用户意图
    intent := h.parseUserIntent(response, eventContext)
    
    // 2. 根据意图调用相应设备 API
    switch intent.Type {
    case "approve_permission":
        return h.approvePermission(ctx, intent, eventContext)
    case "reject_permission": 
        return h.rejectPermission(ctx, intent, eventContext)
    case "answer_question":
        return h.answerQuestion(ctx, intent, eventContext)
    case "ask_clarification":
        // AI 需要澄清，继续对话
        return h.continueConversation(ctx, intent.Question)
    case "batch_approve":
        return h.batchApprove(ctx, intent, eventContext)
    default:
        return fmt.Errorf("未知意图: %s", intent.Type)
    }
}

type UserIntent struct {
    Type           string            // approve_permission, reject_permission, etc.
    Confidence     float64           // 置信度
    DeviceID       string            // 目标设备
    SessionID      string            // 会话ID  
    PermissionID   string            // 权限ID
    QuestionID     string            // 问题ID
    Answers        map[string]any    // 答案
    Question       string            // 澄清问题
    Reasoning      string            // 推理过程
}
```

#### 4. 设备代理集成

```go
// server/internal/clawagent/device_integration.go

func (h *IntentHandler) approvePermission(ctx context.Context, intent *UserIntent, context *EventContext) error {
    // 调用现有的设备代理接口
    proxyPath := fmt.Sprintf("/api/v1/permissions/%s/reply", intent.PermissionID)
    requestBody := map[string]any{"approved": true}
    
    return h.deviceProxy.ProxyToDevice(ctx, intent.DeviceID, proxyPath, requestBody)
}

func (h *IntentHandler) answerQuestion(ctx context.Context, intent *UserIntent, context *EventContext) error {
    proxyPath := fmt.Sprintf("/api/v1/questions/%s/reply", intent.QuestionID)
    requestBody := map[string]any{"answers": intent.Answers}
    
    return h.deviceProxy.ProxyToDevice(ctx, intent.DeviceID, proxyPath, requestBody)
}
```

## 实施计划

### Phase 1: 基础集成 (2天)

**目标**: 建立 AI Runtime 与现有通知系统的基础连接

- [ ] 在 Dispatcher 中添加事件转发器
- [ ] 在 ClawAgent 中添加事件处理器
- [ ] 实现基础的事件描述和 AI 对话注入
- [ ] 添加用户设置：AI 交互模式开关

**验收**: 权限事件能触发 AI 对话，用户收到自然语言描述

### Phase 2: 意图识别 (2天)

**目标**: AI 能理解用户回复并正确调用设备 API

- [ ] 实现意图识别模块
- [ ] 集成设备代理调用
- [ ] 添加权限批准/拒绝的意图处理
- [ ] 添加问卷回答的意图处理

**验收**: 用户能用自然语言批准权限、回答问卷，AI 正确调用设备 API

### Phase 3: 高级特性 (2天)

**目标**: 支持批量处理、澄清、个性化等高级特性

- [ ] 批量权限处理
- [ ] 澄清问题生成
- [ ] 基于用户历史的个性化建议
- [ ] 错误处理和重试机制

**验收**: AI 能智能处理复杂场景，提供个性化建议

### Phase 4: 优化与监控 (1天)

- [ ] 性能优化
- [ ] 监控和日志
- [ ] 用户反馈收集
- [ ] 文档完善

**总计**: 7天开发时间

## 技术挑战与解决方案

### 1. 意图理解准确性

**挑战**: 用户表达模糊，AI 可能误解意图

**解决方案**:
- 使用高置信度阈值，低置信度时主动澄清
- 提供确认机制："您是要批准执行 'rm -rf /tmp' 吗？"
- 学习用户历史表达模式

### 2. 实时性要求

**挑战**: 权限请求需要及时响应

**解决方案**:
- 设置超时机制，超时后回退到传统卡片交互
- 优先级队列，权限请求优先处理
- 异步处理 + 事件通知

### 3. 错误处理

**挑战**: API 调用失败，AI 如何处理

**解决方案**:
- 详细错误信息反馈给 AI
- AI 生成用户友好的错误说明
- 提供重试或替代方案

### 4. 用户学习成本

**挑战**: 用户不熟悉 AI 交互方式

**解决方案**:
- 引导式首次使用
- 渐进式功能开放
- 保留传统卡片方式作为备选

## 数据库扩展

### 新增表

```sql
-- AI 交互偏好设置
CREATE TABLE ai_interaction_preferences (
    user_id VARCHAR(255) PRIMARY KEY,
    ai_enabled BOOLEAN DEFAULT true,
    confidence_threshold DECIMAL(3,2) DEFAULT 0.7,
    auto_approve_patterns TEXT[], -- 自动批准的权限模式
    language_style VARCHAR(50) DEFAULT 'professional', -- professional/casual/friendly
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- AI 对话历史（与 clawagent_sessions 配合）
CREATE TABLE ai_event_conversations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id VARCHAR(255) NOT NULL,
    event_type VARCHAR(50) NOT NULL,
    session_id VARCHAR(255) NOT NULL,
    device_id VARCHAR(255),
    event_data JSONB,
    conversation_started_at TIMESTAMPTZ DEFAULT NOW(),
    conversation_completed_at TIMESTAMPTZ,
    resolution VARCHAR(50), -- approved/rejected/clarified/timeout
    ai_confidence DECIMAL(3,2),
    user_messages JSONB -- 存储用户消息历史
);

CREATE INDEX idx_ai_event_conversations_user_events
    ON ai_event_conversations(user_id, event_type, created_at DESC);
```

## API 设计

### 用户设置 API

```go
// GET /api/ai-interaction/preferences
// PUT /api/ai-interaction/preferences
type AIPreferences struct {
    AIEnabled           bool     `json:"ai_enabled"`
    ConfidenceThreshold float64  `json:"confidence_threshold"`
    AutoApprovePatterns []string `json:"auto_approve_patterns"`
    LanguageStyle       string   `json:"language_style"`
}
```

### AI 交互统计 API

```go
// GET /api/ai-interaction/stats
type AIInteractionStats struct {
    TotalEvents          int     `json:"total_events"`
    AIHandledEvents      int     `json:"ai_handled_events"`
    AverageConfidence    float64 `json:"average_confidence"`
    ResolutionTypes      map[string]int `json:"resolution_types"`
    UserSatisfactionRate float64 `json:"user_satisfaction_rate"`
}
```

## 监控指标

1. **处理成功率**: AI 正确理解和处理事件的比例
2. **平均置信度**: AI 意图识别的平均置信度
3. **用户满意度**: 用户对 AI 处理结果的反馈
4. **响应时间**: 从事件到处理的平均时间
5. **回退率**: 回退到传统卡片交互的比例

## 安全考虑

1. **权限验证**: AI 仍然遵守现有的权限验证机制
2. **审计日志**: 所有 AI 处理的事件都需要详细日志
3. **敏感操作**: 高风险操作需要二次确认
4. **数据隔离**: 不同用户的对话数据严格隔离

## 总结

这个设计方案将现有的按钮式交互升级为智能的自然语言交互，在保持系统安全性和可靠性的同时，显著提升用户体验。通过分阶段实施，可以逐步验证和优化各个功能模块。

**预期收益**:
- 用户体验提升：更自然的交互方式
- 操作效率提升：批量处理能力
- 系统智能化：基于上下文的个性化服务
- 技术架构升级：为未来更复杂的 AI 交互打基础