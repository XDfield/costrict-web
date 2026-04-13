# Cloud Team Agent 系统设计文档

> 版本：v0.1.0 · 日期：2026-04-11

---

## 一、背景与目标

### 1.1 现有系统局限

| 系统 | 协作范围 | 通信机制 | 状态管理 |
|------|----------|----------|----------|
| OMX Team | 单机多 tmux pane | 本地文件系统 mailbox | `.omx/state/` 本地目录 |
| Claude Code Swarm | 单机多进程/pane | 本地文件系统 mailbox | `~/.local/share/claude-code/teams/` |

两者均依赖本地文件系统通信，无法跨越机器边界。当任务规模需要多台设备并行时，缺乏统一的协调层。

### 1.2 设计目标

1. **跨机器协作**：多台设备上的 Teammate 可以协同执行同一个任务
2. **任务拆解在客户端**：Leader 在本地完成任务分解，降低云端依赖
3. **云端统一管控**：进度跟踪、审批、session 生命周期由云端负责
4. **仓库亲和性调度**：同一仓库的修改任务优先分配给同一台设备，避免版本冲突
5. **Teammate 间通过云端通信**：消息总线替代本地 mailbox

---

## 二、整体架构

### 2.1 系统分层

```
┌─────────────────────────────────────────────────────────────────────┐
│                           用户 / 管理员                              │
│                    Web Dashboard / CLI Monitor                       │
└──────────────────────────────┬──────────────────────────────────────┘
                               │ HTTPS / WebSocket
┌──────────────────────────────▼──────────────────────────────────────┐
│                         Cloud Control Plane                          │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────────────┐   │
│  │ Session Mgr  │  │  Task Store  │  │     Approval Queue       │   │
│  │（会话注册/   │  │（任务状态机）│  │（权限审批/人工干预）     │   │
│  │  生命周期）  │  │              │  │                          │   │
│  └──────────────┘  └──────────────┘  └──────────────────────────┘   │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────────────┐   │
│  │ Message Bus  │  │Progress Track│  │   Repo Affinity Registry │   │
│  │（消息路由/  │  │（实时进度）  │  │（Teammate→Repo 映射表）  │   │
│  │  广播）      │  │              │  │                          │   │
│  └──────────────┘  └──────────────┘  └──────────────────────────┘   │
└──────────────────────────────┬──────────────────────────────────────┘
                               │ WebSocket 长连接
          ┌────────────────────┼────────────────────┐
          │                    │                    │
┌─────────▼──────┐   ┌─────────▼──────┐   ┌────────▼───────┐
│  Leader Client │   │Teammate Client │   │Teammate Client │
│  (Machine A)   │   │  (Machine B)   │   │  (Machine C)   │
│                │   │                │   │                │
│ ┌────────────┐ │   │ ┌────────────┐ │   │ ┌────────────┐ │
│ │Task Planner│ │   │ │Task Runner │ │   │ │Task Runner │ │
│ │(本地LLM拆解│ │   │ │(本地执行)  │ │   │ │(本地执行)  │ │
│ └────────────┘ │   │ └────────────┘ │   │ └────────────┘ │
│ ┌────────────┐ │   │ ┌────────────┐ │   │ ┌────────────┐ │
│ │Cloud Client│ │   │ │Cloud Client│ │   │ │Cloud Client│ │
│ │(WS连接管理)│ │   │ │(WS连接管理)│ │   │ │(WS连接管理)│ │
│ └────────────┘ │   │ └────────────┘ │   │ └────────────┘ │
│ ┌────────────┐ │   │ ┌────────────┐ │   │ ┌────────────┐ │
│ │Local Repos │ │   │ │Local Repos │ │   │ │Local Repos │ │
│ │(repo-A,B)  │ │   │ │(repo-C,D)  │ │   │ │(repo-A,E)  │ │
│ └────────────┘ │   │ └────────────┘ │   │ └────────────┘ │
└────────────────┘   └────────────────┘   └────────────────┘
```

### 2.2 角色定义

| 角色 | 运行位置 | 核心职责 |
|------|----------|----------|
| **Leader** | 客户端（发起方机器） | 任务拆解、调度决策、审批响应、结果汇总 |
| **Teammate** | 客户端（任意机器） | 任务执行、进度上报、权限申请、跨机通信 |
| **Cloud Control Plane** | 云端服务 | Session 管理、消息路由、进度追踪、审批队列 |

---

## 三、客户端设计

### 3.1 Leader 客户端

Leader 是任务的发起者和协调者，**所有任务拆解逻辑在本地执行**。

```
Leader Client
├── Task Planner（任务规划器）
│   ├── 接收用户目标
│   ├── 调用本地 LLM 进行任务拆解
│   ├── 分析每个子任务的仓库依赖（repo affinity analysis）
│   └── 生成 TaskPlan（含仓库亲和性标注）
│
├── Scheduler（调度器）
│   ├── 查询云端 Repo Affinity Registry
│   ├── 按亲和性规则分配任务给 Teammate
│   ├── 处理 Teammate 不可用时的重新调度
│   └── 提交 TaskPlan 到云端 Task Store
│
├── Approval Handler（审批处理器）
│   ├── 订阅云端 Approval Queue
│   ├── 在本地 UI 展示审批请求（带 Teammate 标识）
│   └── 发送审批结果回云端
│
└── Cloud Client（云端连接）
    ├── WebSocket 长连接管理
    ├── 断线重连与状态恢复
    └── 事件订阅（进度更新、审批请求、Teammate 状态变化）
```

#### 3.1.1 任务拆解流程

Leader 机器上**不要求持有目标仓库代码**。拆解前通过**远程代码探查**（Remote Explore）向已有该仓库的 Teammate 发起只读查询，获取足够的代码上下文后再做任务拆解。

```
用户输入目标（涉及若干仓库）
    │
    ▼
Leader 查询云端 Repo Affinity Registry
确定每个仓库由哪些 Teammate 持有
    │
    ▼
Remote Explore 阶段（并行，每个仓库选一个 Teammate）
Leader 向云端发送 explore.request
    │
    ▼
目标 Teammate 在本地执行只读探查
（文件树、关键符号、模块依赖、git log 等）
结果通过云端 Message Bus 返回给 Leader
    │
    ▼
Task Planner（本地 LLM）基于探查结果拆解任务
每个子任务包含：
  - taskId
  - description
  - repoAffinity: string[]   ← 涉及的仓库（已由探查确认）
  - fileHints: string[]      ← 具体文件路径（来自探查结果）
  - dependencies: taskId[]
    │
    ▼
Scheduler 按亲和性规则分配 assignedTeammate
    │
    ▼
提交 TaskPlan 到云端
```

#### 3.1.2 远程代码探查（Remote Explore）

Leader 通过云端向 Teammate 发起只读探查请求，Teammate 在**受限沙箱**中执行（仅允许 `rg`、`grep`、`ls`、`find`、`git log` 等只读命令，与 OMX `omx-explore` 的 allowlist 机制一致），防止探查操作意外触发写入。

```typescript
interface ExploreRequest {
  requestId: string
  sessionId: string
  fromLeaderId: string
  targetTeammateId: string     // 持有目标仓库的 Teammate
  repoRemoteUrl: string
  queries: ExploreQuery[]
}

interface ExploreQuery {
  type: 'file_tree' | 'symbol_search' | 'content_search' | 'git_log' | 'dependency_graph'
  params: Record<string, unknown>
}

interface ExploreResult {
  requestId: string
  queryResults: Array<{
    type: string
    output: string
    truncated: boolean
  }>
}
```

**探查流程：**

```
Leader                    云端 Message Bus              Teammate
  │                              │                          │
  │── explore.request ──────────→│                          │
  │                              │── explore.request ──────→│
  │                              │                          │ 在 allowlist 沙箱执行
  │                              │                          │ rg / ls / git log ...
  │                              │←── explore.result ───────│
  │←── explore.result ──────────│                          │
  │                              │                          │
  │ 基于结果做任务拆解            │                          │
```

**超时与降级：**
- 探查请求超时（默认 30s）→ Leader 基于用户描述做粗粒度拆解，`fileHints` 留空
- Teammate 离线 → 从 Registry 选择同仓库的其他 Teammate；无可用 Teammate → 降级为粗粒度拆解

#### 3.1.3 仓库亲和性分析

Leader 在任务拆解阶段，结合远程探查结果精确识别每个子任务涉及的仓库和文件，并在 `repoAffinity` 与 `fileHints` 字段中标注。

```typescript
interface SubTask {
  taskId: string
  description: string
  repoAffinity: string[]       // ["git@github.com:org/repo-a.git"]
  fileHints: string[]          // 预期修改的文件路径（辅助调度）
  dependencies: string[]
  assignedTeammateId?: string  // 由 Scheduler 填充
}
```

### 3.2 Teammate 客户端

Teammate 是任务的执行者，运行在任意一台已注册的机器上。

```
Teammate Client
├── Task Runner（任务执行器）
│   ├── 订阅云端分配给自己的任务
│   ├── 本地执行（调用 Claude Code / Codex）
│   ├── 实时上报进度到云端
│   └── 执行完成后上报结果
│
├── Permission Requester（权限申请器）
│   ├── 拦截需要审批的工具调用
│   ├── 向云端发送 ApprovalRequest
│   ├── 等待云端推送 ApprovalResponse
│   └── 根据结果继续或中止执行
│
├── Messenger（消息收发器）
│   ├── 向云端发送消息（目标：指定 Teammate 或 Leader）
│   └── 接收云端推送的消息并注入执行上下文
│
├── Explore Handler（远程探查处理器）
│   ├── 订阅云端推送的 explore.request
│   ├── 在 allowlist 沙箱中执行只读命令（rg/ls/git log 等）
│   ├── 防止探查操作触发写入（与 omx-explore 机制一致）
│   └── 将探查结果通过云端返回给 Leader
│
├── Repo Registry（仓库注册器）
│   ├── 启动时扫描本地已有仓库
│   ├── 上报 Repo → Machine 映射到云端
│   └── 仓库状态变化时同步更新
│
└── Cloud Client（云端连接）
    ├── WebSocket 长连接管理
    └── 断线重连（重连后恢复未完成任务）
```

---

## 四、云端设计

### 4.1 核心模块

#### 4.1.1 Session Manager（会话管理器）

管理团队协作 Session 的完整生命周期。

```typescript
interface TeamSession {
  sessionId: string
  name: string
  createdAt: number
  status: 'active' | 'paused' | 'completed' | 'failed'
  leaderId: string
  leaderMachineId: string
  teammates: TeammateRegistration[]
  taskPlanId?: string
}

interface TeammateRegistration {
  teammateId: string
  machineId: string
  machineName: string
  status: 'online' | 'offline' | 'busy'
  repos: RepoInfo[]          // 该机器已有的仓库列表
  currentTaskId?: string
  connectedAt: number
  lastHeartbeat: number
}
```

**职责：**
- Leader 创建 Session，获取 `sessionId`
- Teammate 通过 `sessionId` 加入 Session
- 维护心跳检测，标记离线 Teammate
- Session 状态持久化，支持 Leader 重连后恢复

#### 4.1.2 Task Store（任务状态机）

存储并管理所有子任务的状态流转。

```
任务状态机：

pending ──→ assigned ──→ claimed ──→ running ──→ completed
   │            │            │           │
   │            ▼            ▼           ▼
   │         reassign     timeout      failed
   │            │                        │
   └────────────┴────────────────────────┘
                        ↓
                    retry / abandon
```

```typescript
interface Task {
  taskId: string
  sessionId: string
  description: string
  repoAffinity: string[]
  fileHints: string[]
  dependencies: string[]
  assignedTeammateId: string
  status: TaskStatus
  createdAt: number
  claimedAt?: number
  startedAt?: number
  completedAt?: number
  result?: TaskResult
  retryCount: number
}

type TaskStatus = 'pending' | 'assigned' | 'claimed' | 'running' | 'completed' | 'failed'
```

#### 4.1.3 Repo Affinity Registry（仓库亲和性注册表）

维护 Teammate → Repo 的映射，为 Leader 调度提供数据支撑。

```typescript
interface RepoAffinityEntry {
  repoRemoteUrl: string        // "git@github.com:org/repo.git"
  repoLocalPath: string        // 本地路径（仅供参考）
  machineId: string
  teammateId: string
  lastSyncedAt: number
  currentBranch: string
  hasUncommittedChanges: boolean
}
```

**查询接口：**
```
GET /registry/repos?remoteUrl=<url>
→ 返回拥有该仓库的所有 Teammate 列表，按 lastSyncedAt 排序
```

Leader 调度时优先选择：
1. 已有目标仓库 **且** 无未提交变更的 Teammate
2. 已有目标仓库但有未提交变更（需先 commit/stash）
3. 无该仓库的 Teammate（需先 clone）

#### 4.1.4 Message Bus（消息总线）

替代本地文件系统 mailbox，实现跨机器消息路由。

```typescript
interface CloudMessage {
  messageId: string
  sessionId: string
  from: string              // teammateId 或 'leader'
  to: string                // teammateId 或 'leader' 或 'broadcast'
  type: MessageType
  payload: unknown
  timestamp: number
}

type MessageType =
  | 'task_message'          // Teammate 间协作消息
  | 'progress_update'       // 进度上报
  | 'approval_request'      // 权限申请
  | 'approval_response'     // 审批结果
  | 'task_complete'         // 任务完成通知
  | 'teammate_idle'         // Teammate 空闲通知
  | 'session_event'         // Session 级别事件
```

消息路由规则：
- `to: 'leader'` → 推送到 Leader 的 WebSocket 连接
- `to: '<teammateId>'` → 推送到对应 Teammate 的 WebSocket 连接
- `to: 'broadcast'` → 推送到 Session 内所有成员
- 目标离线时消息持久化到队列，重连后推送

#### 4.1.5 Approval Queue（审批队列）

集中管理所有 Teammate 发起的权限审批请求。

```typescript
interface ApprovalRequest {
  approvalId: string
  sessionId: string
  requesterId: string        // 发起审批的 Teammate ID
  requesterName: string
  toolName: string           // 需要审批的工具（Bash/Edit/Write 等）
  toolInput: Record<string, unknown>
  description: string        // 人类可读的操作描述
  riskLevel: 'low' | 'medium' | 'high'
  status: 'pending' | 'approved' | 'rejected'
  feedback?: string
  permissionUpdates?: unknown[]
  createdAt: number
  resolvedAt?: number
}
```

**审批流程：**
```
Teammate 发起审批请求
    │
    ▼
云端 Approval Queue 入队
    │
    ▼
推送给 Leader（WebSocket 事件：approval_request）
    │
    ▼
Leader 本地 UI 显示审批对话框
（含 Teammate 标识、工具名称、操作描述、风险级别）
    │
    ▼
Leader 批准 / 拒绝 / 修改权限范围
    │
    ▼
云端更新 ApprovalRequest 状态
    │
    ▼
推送 approval_response 给对应 Teammate
    │
    ▼
Teammate 继续执行 / 中止
```

#### 4.1.6 Progress Tracker（进度追踪器）

聚合所有 Teammate 的实时进度，提供全局视图。

```typescript
interface SessionProgress {
  sessionId: string
  totalTasks: number
  completedTasks: number
  failedTasks: number
  runningTasks: number
  pendingTasks: number
  teammates: TeammateProgress[]
  timeline: ProgressEvent[]
}

interface TeammateProgress {
  teammateId: string
  name: string
  machineId: string
  currentTask?: TaskSummary
  completedCount: number
  failedCount: number
  lastActivity: number
}
```

---

## 五、通信协议

### 5.1 连接建立

```
客户端                                    云端
   │                                        │
   │── WebSocket Connect ──────────────────→│
   │   Authorization: Bearer <token>        │
   │   X-Session-Id: <sessionId>            │
   │   X-Role: leader | teammate            │
   │   X-Machine-Id: <machineId>            │
   │                                        │
   │←── Connected + Session State ─────────│
   │    (包含当前 session 快照)              │
```

### 5.2 事件格式

所有 WebSocket 消息采用统一 envelope 格式：

```typescript
interface CloudEvent {
  eventId: string
  type: string
  sessionId: string
  timestamp: number
  payload: unknown
}
```

### 5.3 关键事件类型

| 方向 | 事件类型 | 说明 |
|------|----------|------|
| Client → Cloud | `session.create` | Leader 创建 Session |
| Client → Cloud | `session.join` | Teammate 加入 Session |
| Client → Cloud | `task.plan.submit` | Leader 提交任务计划 |
| Client → Cloud | `task.claim` | Teammate 认领任务 |
| Client → Cloud | `task.progress` | Teammate 上报进度 |
| Client → Cloud | `task.complete` | Teammate 完成任务 |
| Client → Cloud | `approval.request` | Teammate 申请权限 |
| Client → Cloud | `approval.respond` | Leader 响应审批 |
| Client → Cloud | `message.send` | 发送消息给指定成员 |
| Client → Cloud | `repo.register` | Teammate 注册本地仓库 |
| Client → Cloud | `explore.request` | Leader 向 Teammate 发起只读代码探查 |
| Client → Cloud | `explore.result` | Teammate 返回探查结果给 Leader |
| Cloud → Client | `task.assigned` | 云端通知 Teammate 有新任务 |
| Cloud → Client | `approval.request` | 云端推送审批请求给 Leader |
| Cloud → Client | `approval.response` | 云端推送审批结果给 Teammate |
| Cloud → Client | `message.receive` | 推送消息给目标成员 |
| Cloud → Client | `session.updated` | Session 状态变化通知 |
| Cloud → Client | `teammate.status` | Teammate 上线/离线通知 |

### 5.4 断线重连

```
Teammate 断线
    │
    ▼
云端标记 Teammate 为 offline
正在执行的任务状态变为 interrupted
    │
    ▼
Teammate 重连（携带 sessionId + lastEventId）
    │
    ▼
云端推送断线期间的积压消息
恢复 interrupted 任务状态为 running
    │
    ▼
Teammate 继续执行
```

---

## 六、调度策略

### 6.1 仓库亲和性调度算法

Leader 在提交 TaskPlan 前，对每个子任务执行以下调度逻辑：

```
对于每个 SubTask（含 repoAffinity 列表）：

1. 查询云端 Repo Affinity Registry
   → 获取拥有所有目标仓库的 Teammate 列表

2. 优先级排序：
   P1: 已有全部目标仓库 + 无未提交变更 + 当前空闲
   P2: 已有全部目标仓库 + 有未提交变更（需先处理）+ 当前空闲
   P3: 已有部分目标仓库（缺失的需 clone）+ 当前空闲
   P4: 无目标仓库（全部需 clone）+ 当前空闲
   P5: 任意有空闲 Teammate（最后兜底）

3. 若存在 P1/P2 级别 Teammate，优先分配
   → 避免同一仓库被多台机器并发修改

4. 若同一仓库已有任务在某 Teammate 上运行中：
   → 将该仓库的后续任务排队到同一 Teammate（串行化）
   → 除非任务间无文件冲突（由 fileHints 判断）
```

### 6.2 依赖任务调度

```
任务 A（repo-x）──→ 任务 B（repo-x）──→ 任务 C（repo-x, repo-y）
                                              │
                                              ▼
                                    C 等待 A、B 完成后才分配
                                    优先分配给已有 repo-x 的机器
```

### 6.3 负载均衡

在亲和性满足的前提下，次要排序因素：
- Teammate 当前任务队列长度（越短越优先）
- Teammate 历史任务成功率
- 机器性能指标（可选，由 Teammate 上报）

---

## 七、数据流示例

### 7.1 完整工作流

```
用户在 Leader 机器输入："重构认证模块，并更新相关测试"
    │
    ▼
Leader Task Planner（本地 LLM）拆解任务：
  Task-1: 分析当前认证实现（只读，repo: auth-service）
  Task-2: 重构 src/auth/validate.ts（写，repo: auth-service）
  Task-3: 更新 tests/auth/*.test.ts（写，repo: auth-service）
  Task-4: 更新 API 文档（写，repo: docs）
  依赖关系：Task-2 依赖 Task-1，Task-3 依赖 Task-2
    │
    ▼
Leader Scheduler 查询云端 Repo Affinity Registry：
  auth-service → Machine-B (Teammate-1), Machine-C (Teammate-2)
  docs         → Machine-C (Teammate-2)
    │
    ▼
调度决策：
  Task-1 → Teammate-1（Machine-B，有 auth-service，空闲）
  Task-2 → Teammate-1（同仓库，等 Task-1 完成）
  Task-3 → Teammate-1（同仓库，等 Task-2 完成）
  Task-4 → Teammate-2（Machine-C，有 docs）
    │
    ▼
Leader 提交 TaskPlan 到云端 Task Store
    │
    ▼
云端推送 task.assigned 给 Teammate-1（Task-1）
云端推送 task.assigned 给 Teammate-2（Task-4）
    │
    ▼
Teammate-1 执行 Task-1（分析，只读）
    │
    ├── 进度上报 → 云端 Progress Tracker → 推送给 Leader
    │
    ▼
Task-1 完成 → 云端解锁 Task-2 → 推送给 Teammate-1
    │
    ▼
Teammate-1 执行 Task-2（修改文件）
    │
    ├── 需要写入敏感文件 → 发送 approval.request 到云端
    │   云端推送给 Leader → Leader 审批 → 云端推送结果给 Teammate-1
    │
    ▼
Task-2 完成 → 云端解锁 Task-3 → 推送给 Teammate-1
    │
    ▼
Teammate-1 执行 Task-3，Teammate-2 执行 Task-4（并行）
    │
    ▼
所有任务完成 → 云端更新 Session 状态为 completed
Leader 收到 session.updated 事件，汇总结果
```

---

## 八、云端 API 设计

### 8.1 REST API

```
# Session 管理
POST   /api/sessions                    创建 Session
GET    /api/sessions/:id                获取 Session 详情
PATCH  /api/sessions/:id                更新 Session 状态
DELETE /api/sessions/:id                关闭 Session

# 成员管理
POST   /api/sessions/:id/members        Teammate 加入 Session
DELETE /api/sessions/:id/members/:mid   Teammate 离开 Session

# 任务管理
POST   /api/sessions/:id/tasks          提交 TaskPlan（批量）
GET    /api/sessions/:id/tasks          获取任务列表
GET    /api/tasks/:taskId               获取单个任务
PATCH  /api/tasks/:taskId               更新任务状态/结果

# 审批管理
GET    /api/sessions/:id/approvals      获取待审批列表
PATCH  /api/approvals/:approvalId       响应审批

# 仓库注册
POST   /api/registry/repos              注册/更新仓库信息
GET    /api/registry/repos              查询仓库亲和性

# 进度查询
GET    /api/sessions/:id/progress       获取 Session 进度快照

# 远程探查
POST   /api/sessions/:id/explore        Leader 发起探查请求（同步等待结果）

# 选举
POST   /api/sessions/:id/leader/elect   客户端发起选举（携带 capabilities）
GET    /api/sessions/:id/leader         查询当前 Leader 信息
POST   /api/sessions/:id/leader/heartbeat  Leader 心跳续约
```

### 8.2 WebSocket 端点

```
WS /ws/sessions/:id
  Query params:
    token=<auth_token>
    machineId=<machine_id>
    lastEventId=<id>    （断线重连时携带，用于消息补发）
```

注：连接时不再携带 `role=leader|teammate`，角色由云端选举决定。

---

## 九、Leader 选举机制

所有客户端以**对等身份**连接云端，角色（Leader / Teammate）由云端通过分布式锁选举产生，客户端无需预先指定。

### 9.1 选举流程

```
所有客户端启动，连接云端 WebSocket
    │
    ▼
每个客户端发送 leader.elect 请求
携带：machineId, capabilities（本地仓库列表、性能指标）
    │
    ▼
云端原子抢占分布式锁（Redis SET NX + TTL=30s）
    │
    ├── 抢到锁 ──→ 成为 Leader
    │               云端广播 leader.elected（含 leaderId）
    │               Leader 从云端拉取 Session 状态
    │               ├── 全新 Session → 开始任务拆解
    │               └── 恢复 Session → 接管未完成任务调度
    │
    └── 未抢到 ──→ 成为 Teammate
                    等待 leader.elected 事件
                    上报本地仓库信息到 Repo Affinity Registry
                    等待云端推送任务分配
```

### 9.2 候选人适合性评分

当多个客户端同时发起 `leader.elect` 时，云端在锁竞争前对候选人打分，**评分最高者优先参与抢锁**（评分相同则先到先得）：

| 评分因素 | 权重 | 说明 |
|----------|------|------|
| 本地仓库覆盖度 | 40% | 拥有更多目标仓库的机器，Leader 调度时信息更完整 |
| 历史心跳稳定性 | 30% | 过去连接的心跳成功率，稳定性高的机器更适合做 Leader |
| 机器性能指标 | 20% | CPU / 内存余量，任务拆解需要本地 LLM 推理资源 |
| 连接时延 | 10% | 与云端的网络时延，时延低的响应更及时 |

```typescript
interface ElectRequest {
  machineId: string
  repos: RepoInfo[]           // 本地已有仓库列表
  heartbeatSuccessRate: number // 历史心跳成功率 0~1
  cpuIdlePercent: number
  memoryFreeMB: number
  rttMs: number               // 与云端的往返时延
}
```

### 9.3 心跳与锁续约

Leader 当选后需持续续约，防止因进程假死导致锁长期占用：

```
Leader                          云端
  │                               │
  │── heartbeat（每 10s）────────→│
  │   payload: { leaderId, ts }   │ 刷新锁 TTL → 30s
  │                               │
  │   （超过 30s 无心跳）         │
  │                               │ 锁自动过期
  │                               │ 广播 leader.expired 事件
  │                               │
  │          所有 Teammate        │
  │←── leader.expired ───────────│
  │                               │
  │  重新发起 leader.elect 抢占   │
```

### 9.4 Leader 宕机恢复

```
原 Leader 宕机
    │
    ▼
云端锁 TTL 到期（最长 30s）
    │
    ▼
云端广播 leader.expired
    │
    ▼
存活的 Teammate 重新发起 leader.elect
    │
    ▼
新 Leader 当选
    │
    ▼
新 Leader 从云端 Task Store 拉取 Session 快照：
  - 已完成任务 → 跳过
  - 运行中任务 → 确认对应 Teammate 是否仍在线
    ├── 在线 → 继续执行，无需重新分配
    └── 离线 → 重置为 pending，重新调度
  - 待分配任务 → 继续按亲和性分配
    │
    ▼
新 Leader 接管调度，Session 无感恢复
```

### 9.5 脑裂防护

极端网络分区场景下，可能出现两个客户端都认为自己是 Leader（脑裂）。防护措施：

- **锁版本号（Fencing Token）**：每次选举锁版本号单调递增，云端拒绝旧版本号的写操作
- **Leader 写操作携带 fencingToken**：Task Store、Approval Queue 的写入均需校验 token，旧 Leader 的写请求被拒绝后感知自己已失效，主动降级为 Teammate
- **Teammate 忽略旧 Leader 消息**：收到 `leader.elected` 后，记录当前 `leaderId`，忽略来自其他 machineId 的调度指令

```typescript
interface LeaderWriteRequest {
  fencingToken: number   // 选举轮次，单调递增
  leaderId: string
  payload: unknown
}
// 云端校验：fencingToken < currentToken → 拒绝，返回 409 Stale Leader
```

---

## 十、关键设计决策

| 决策 | 说明 | 对比原有方案 |
|------|------|-------------|
| **云端自动选举 Leader** | 所有客户端对等接入，通过分布式锁抢占成为 Leader，无需用户指定角色 | 原有方案由用户手动启动 Leader 进程 |
| **远程代码探查** | Leader 无需持有仓库代码，通过云端向 Teammate 发起只读探查获取上下文后再拆解任务 | 原有单机方案 Leader 直接访问本地代码 |
| **任务拆解在客户端** | Leader 本地 LLM 执行，减少云端 LLM 调用，响应更快 | OMX/Swarm 均在本地拆解，保持一致 |
| **仓库亲和性调度** | Leader 查询云端 Registry，优先将同仓库任务分配给同一机器 | 原有方案单机无此问题，多机新增 |
| **WebSocket 替代文件 Mailbox** | 跨机器实时消息推送，无需轮询 | 原有方案依赖本地文件系统 |
| **云端集中审批** | 所有 Teammate 的权限请求路由到云端，Leader 统一审批 | 原有 LeaderPermissionBridge 仅限同机 |
| **Session 持久化** | 云端存储完整 Session 状态，Leader/Teammate 重连后可恢复 | 原有方案重启后状态丢失 |
| **串行化同仓库写任务** | 同仓库的写操作任务默认串行分配给同一 Teammate，避免冲突 | 原有单机方案通过 Authority Lease 解决 |
| **Repo Affinity Registry** | Teammate 启动时上报本地仓库列表，云端维护映射表 | 原有方案无跨机仓库感知 |
| **Fencing Token 防脑裂** | 选举轮次单调递增，云端拒绝旧 Leader 的写操作，旧 Leader 主动降级 | 原有单机方案无此问题 |

---

## 十一、扩展点

### 11.1 人工干预

除权限审批外，云端支持以下人工干预能力：
- **暂停/恢复任务**：Leader 可暂停特定 Teammate 的执行
- **重新分配任务**：将某 Teammate 的任务转移给另一台机器
- **注入消息**：Leader 向正在执行的 Teammate 发送补充指令
- **强制终止**：终止特定任务或整个 Session

### 11.2 多 Leader 支持（未来）

当前设计为单 Leader。未来可扩展为：
- 多 Leader 共同监控同一 Session（只读 Leader）
- Leader 转移（原 Leader 离线后指定新 Leader）

### 11.3 任务模板

Leader 可将成功的 TaskPlan 保存为模板，下次直接复用调度策略。

---

## 十二、与现有系统的对比

| 维度 | OMX Team（单机） | Claude Code Swarm（单机） | Cloud Team（本文档） |
|------|-----------------|--------------------------|---------------------|
| 协作范围 | 单机多 pane | 单机多进程/pane | 多机多进程 |
| Leader 产生 | 用户手动启动 | 用户手动启动 | 云端自动选举 |
| Leader 代码依赖 | 需持有代码 | 需持有代码 | 通过远程探查获取上下文 |
| 任务拆解 | Leader 本地 | Leader 本地 | Leader 本地（探查后拆解） |
| 通信机制 | 本地文件 mailbox | 本地文件 mailbox | 云端 WebSocket |
| 状态管理 | `.omx/state/` | `~/.local/share/` | 云端 Task Store |
| 审批路由 | 本地 UI 直接显示 | LeaderPermissionBridge | 云端 Approval Queue |
| 仓库感知 | 无需（单机） | 无需（单机） | Repo Affinity Registry |
| 断线恢复 | 无 | 无 | 支持（lastEventId 补发） |
| Leader 容灾 | 无 | 无 | 自动重选举 + Session 恢复 |
| 进度可视化 | HUD（本地） | Leader UI | Web Dashboard + CLI |

---

*文档版本：v0.1.0 | 基于 OMX architecture.md v0.12.4 + Claude Code SWARM_ARCHITECTURE.md 设计*
