> **实现状态：已完成**
>
> - 状态：✅ 已完成
> - 实现位置：`server/internal/services/`（`git_service.go`, `parser_service.go`, `sync_service.go`, `job_service.go`）；`server/internal/worker/worker.go`；`server/internal/scheduler/`；`server/internal/models/models.go`（`SyncJob` 模型）；`cmd/api/` 和 `cmd/worker/` 入口
> - 说明：Skill 仓库数据同步模块已完整实现，包括 Git 拉取、解析、同步任务管理、定时调度器和独立 Worker 进程。

---

# Skill 仓库数据同步技术方案

## 一、现状分析

### 1.1 已有基础（可直接复用）

| 字段 | 所在模型 | 用途 |
|------|---------|------|
| `ExternalURL` / `ExternalBranch` | `SkillRegistry` | 外部 Git 仓库地址和分支 |
| `SyncEnabled` / `SyncInterval` | `SkillRegistry` | 同步开关和间隔（秒） |
| `LastSyncedAt` / `LastSyncSHA` | `SkillRegistry` | 上次同步时间和 commit SHA |
| `SourcePath` / `SourceSHA` | `SkillItem` | 来源文件路径和内容哈希 |
| `SkillVersion` | 独立表 | 完整版本历史 |
| `Organization` | 独立表 | 组织实体，含 Visibility / OwnerID / Members |

### 1.2 核心设计：同步类型组织

引入**同步类型组织（Sync Organization）**作为同步场景的入口：

- `Organization` 新增 `OrgType` 字段，取值 `normal`（默认）或 `sync`
- `sync` 类型的组织在创建时必须关联一个 `SkillRegistry`，并在该 Registry 上配置 Git 同步源
- 同步配置（ExternalURL、SyncInterval 等）保留在 `SkillRegistry` 层，Org 仅做关联
- 同步类型组织的 Registry 内容完全由 Git 仓库驱动，不允许手动增删 SkillItem

```
Git 仓库
  └── 同步类型组织（OrgType=sync）
        └── SkillRegistry（持有同步配置，SourceType=external）
              └── SkillItem（由同步引擎写入，只读）
```

### 1.3 需要新增的部分

- `Organization.OrgType` 字段
- Git 操作服务层（clone / fetch / diff）
- SKILL.md / plugin.json 文件解析器
- 同步执行引擎（增量对比、冲突处理）
- gocron 定时调度器（生产者，只负责入队）
- `SyncJob` 任务队列表（基于 PostgreSQL）
- `SyncLog` 同步日志表
- Worker Pool（消费者，从队列拉取并执行）
- `SkillRegistry.SyncStatus` / `SyncConfig` / `LastSyncLogID` 扩展字段
- 同步相关 API 及前端界面

---

## 二、新增依赖

```go
// server/go.mod 新增
require (
    github.com/go-git/go-git/v5   v5.11.0  // Git 操作（纯 Go 实现，无需系统 git）
    github.com/go-co-op/gocron/v2 v2.19.1  // 定时调度（生产者，只负责入队）
    // gopkg.in/yaml.v3 已存在，用于 YAML 解析
)
```

**选型说明**

- **go-git/go-git**：纯 Go 实现，无需系统安装 git，支持浅克隆（`Depth:1`），适合服务端场景
- **go-co-op/gocron v2**：仅作为定时触发器，到期后向 `SyncJob` 表写入一条 pending 记录，不直接执行同步逻辑
- **队列存储选型**：基于已有 PostgreSQL 实现，利用 `SELECT ... FOR UPDATE SKIP LOCKED` 保证多 Worker 并发拉取安全，无需引入 Redis 等额外基础设施

---

## 三、数据模型扩展

### 3.1 扩展 `Organization` 表

在现有结构体中新增 `OrgType` 字段：

```go
// 新增字段
OrgType string `gorm:"type:varchar(32);default:'normal'" json:"orgType"`
// normal | sync
```

**业务约束**

- `OrgType=sync` 时，创建组织需同时创建或关联一个配置了 Git 同步源的 `SkillRegistry`
- `OrgType=normal` 的组织保持原有行为不变

### 3.2 扩展 `SkillRegistry` 表

同步配置保留在 Registry 层，新增以下字段：

```go
// 新增字段
SyncStatus    string         `gorm:"default:'idle'" json:"syncStatus"`
// idle | syncing | error | paused

SyncConfig    datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"syncConfig" swaggertype:"object"`
// {
//   "includePatterns":   ["**/*.md", "skills/**/SKILL.md"],
//   "excludePatterns":   ["node_modules/**"],
//   "conflictStrategy":  "keep_remote",  // keep_local | keep_remote
//   "webhookSecret":     "..."
// }

LastSyncLogID *string `json:"lastSyncLogId"`
```

> `ExternalURL` / `ExternalBranch` / `SyncEnabled` / `SyncInterval` 已存在，无需新增。

### 3.3 新增 `SyncJob` 表（任务队列）

```go
// server/internal/models/models.go

type SyncJob struct {
    ID           string     `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
    RegistryID   string     `gorm:"not null;index" json:"registryId"`
    TriggerType  string     `gorm:"not null" json:"triggerType"` // scheduled | manual | webhook
    TriggerUser  string     `json:"triggerUser"`
    Priority     int        `gorm:"not null;default:5" json:"priority"`
    // 优先级：1=最高（manual/webhook）5=普通（scheduled），数字越小越优先
    Status       string     `gorm:"not null;default:'pending';index" json:"status"`
    // pending | running | success | failed | cancelled
    Payload      datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"payload"`
    // { "dryRun": false, "reason": "..." }
    RetryCount   int        `gorm:"default:0" json:"retryCount"`
    MaxAttempts  int        `gorm:"default:3" json:"maxAttempts"`
    LastError    string     `gorm:"type:text" json:"lastError"`
    ScheduledAt  time.Time  `gorm:"not null;index" json:"scheduledAt"` // 最早可执行时间
    StartedAt    *time.Time `json:"startedAt"`
    FinishedAt   *time.Time `json:"finishedAt"`
    SyncLogID    *string    `json:"syncLogId"` // 执行完成后关联 SyncLog
    CreatedAt    time.Time  `json:"createdAt"`

    Registry *SkillRegistry `gorm:"foreignKey:RegistryID" json:"registry,omitempty"`
}
```

**状态机**

```
pending ──→ running ──→ success
               │
               └──→ failed ──→ pending（RetryCount < MaxAttempts，退避重试）
                        │
                        └──→ failed（RetryCount >= MaxAttempts，终态）

pending ──→ cancelled（手动取消或 Registry 被禁用时）
```

**去重约束**

同一 Registry 不允许同时存在多条 `pending` 或 `running` 的任务，入队前检查：

```sql
SELECT COUNT(*) FROM sync_jobs
WHERE registry_id = $1 AND status IN ('pending', 'running')
```

有记录则跳过入队（定时触发）或返回冲突错误（手动触发提示用户）。

### 3.4 新增 `SyncLog` 表

```go
// server/internal/models/models.go

type SyncLog struct {
    ID           string     `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
    RegistryID   string     `gorm:"not null;index" json:"registryId"`
    TriggerType  string     `gorm:"not null" json:"triggerType"`    // scheduled | manual | webhook
    TriggerUser  string     `json:"triggerUser"`
    Status       string     `gorm:"not null;default:'running'" json:"status"` // running | success | failed | cancelled
    CommitSHA    string     `json:"commitSha"`
    PreviousSHA  string     `json:"previousSha"`
    TotalItems   int        `gorm:"default:0" json:"totalItems"`
    AddedItems   int        `gorm:"default:0" json:"addedItems"`
    UpdatedItems int        `gorm:"default:0" json:"updatedItems"`
    DeletedItems int        `gorm:"default:0" json:"deletedItems"`
    SkippedItems int        `gorm:"default:0" json:"skippedItems"`
    FailedItems  int        `gorm:"default:0" json:"failedItems"`
    ErrorMessage string     `gorm:"type:text" json:"errorMessage"`
    DurationMs   int64      `json:"durationMs"`
    StartedAt    time.Time  `gorm:"not null" json:"startedAt"`
    FinishedAt   *time.Time `json:"finishedAt"`
    CreatedAt    time.Time  `json:"createdAt"`

    Registry *SkillRegistry `gorm:"foreignKey:RegistryID" json:"registry,omitempty"`
}
```

### 3.5 AutoMigrate 注册

```go
// server/main.go 的 AutoMigrate 调用中新增：
&models.SyncJob{},
&models.SyncLog{},
```

---

## 四、目录结构

执行层（Worker）作为独立二进制，与主服务（API Server）共享 `internal/` 下的模型和服务代码，通过同一个 PostgreSQL 数据库通信。

```
server/
├── cmd/
│   ├── api/
│   │   └── main.go            # API Server 入口（现有 main.go 迁移至此）
│   └── worker/
│       └── main.go            # Worker 独立入口（可单独构建/部署/扩容）
├── internal/
│   ├── models/
│   │   └── models.go          # 扩展 Organization（OrgType），扩展 SkillRegistry，新增 SyncJob / SyncLog
│   ├── services/              # 新增目录，api 和 worker 共用
│   │   ├── git_service.go     # Git 操作封装
│   │   ├── parser_service.go  # 文件内容解析
│   │   ├── sync_service.go    # 同步核心逻辑（Worker 调用）
│   │   └── job_service.go     # 任务入队 / 查询 / 取消（API 调用）
│   ├── worker/                # 新增目录
│   │   └── worker.go          # Worker Pool，消费 SyncJob 队列
│   ├── scheduler/             # 新增目录
│   │   └── scheduler.go       # gocron 调度器（只负责定时入队，运行在 API Server 侧）
│   └── handlers/
│       ├── organization.go    # 扩展：CreateOrg 支持 sync 类型
│       ├── sync.go            # 新增同步相关 API Handler
│       └── ...（现有文件不变）
└── go.mod
```

**两个二进制的职责划分**

| | `cmd/api` | `cmd/worker` |
|---|---|---|
| 启动内容 | Gin HTTP Server + gocron Scheduler | Worker Pool only |
| 依赖 | DB + JobService + Scheduler + Handlers | DB + SyncService + WorkerPool |
| 可横向扩展 | 否（Scheduler 需单实例，防重复入队） | **是**，多实例靠 SKIP LOCKED 竞争任务 |
| 部署方式 | 单副本 | 按负载水平扩容副本数 |

---

## 五、服务层设计

### 5.1 GitService

**职责**：封装所有 Git 操作，对上层屏蔽 go-git 细节。

```go
// server/internal/services/git_service.go

package services

type GitService struct {
    TempBaseDir string // 临时克隆目录，如 /tmp/costrict-sync
}

type CloneResult struct {
    LocalPath string
    CommitSHA string
}

// Clone 浅克隆仓库到临时目录，返回本地路径和最新 commit SHA
func (s *GitService) Clone(repoURL, branch string) (*CloneResult, error)

// Fetch 在已有本地仓库上拉取最新变更，返回新 commit SHA
func (s *GitService) Fetch(localPath, branch string) (newSHA string, err error)

// GetHeadSHA 获取当前 HEAD 的 commit SHA
func (s *GitService) GetHeadSHA(localPath string) (string, error)

// ListFiles 按 glob 模式列出匹配的文件路径列表
func (s *GitService) ListFiles(localPath string, includes, excludes []string) ([]string, error)

// ReadFile 读取文件内容
func (s *GitService) ReadFile(localPath, relPath string) ([]byte, error)

// ContentHash 计算文件内容的 SHA-256 哈希
func (s *GitService) ContentHash(content []byte) string

// Cleanup 删除临时目录
func (s *GitService) Cleanup(localPath string) error
```

**关键实现细节**

- 使用 `Depth: 1` 浅克隆，节省带宽和磁盘
- 临时目录命名规则：`{TempBaseDir}/{registryID}-{timestamp}`
- 每次同步完成后（无论成功失败）都在 `defer` 中调用 `Cleanup`

### 5.2 ParserService

**职责**：将 Git 仓库中的文件解析为 `SkillItem` 数据结构。

```go
// server/internal/services/parser_service.go

package services

type ParsedItem struct {
    Slug        string
    ItemType    string // skill | agent | command | mcp | hook
    Name        string
    Description string
    Category    string
    Version     string
    Content     string         // 原始文件内容
    Metadata    map[string]any
    SourcePath  string
    ContentHash string
}

// ParseSKILLMD 解析 SKILL.md（YAML frontmatter + Markdown body）
func (p *ParserService) ParseSKILLMD(content []byte, sourcePath string) (*ParsedItem, error)

// ParsePluginJSON 解析 .claude-plugin/plugin.json
func (p *ParserService) ParsePluginJSON(content []byte) (*ParsedItem, error)

// ParseAgentsMD 解析 AGENTS.md，提取技能描述段落
func (p *ParserService) ParseAgentsMD(content []byte, sourcePath string) ([]*ParsedItem, error)

// InferItemType 根据文件路径推断 item 类型
// skills/** → skill / agents/** → agent / commands/** → command / hooks/** → hook
func (p *ParserService) InferItemType(filePath string) string

// InferSlug 从文件路径生成 slug
// skills/dev-tools/browser.md → dev-tools-browser
func (p *ParserService) InferSlug(filePath string) string
```

**SKILL.md frontmatter 格式约定**

```yaml
---
name: Browser Automation
description: Control browser with persistent state
type: skill          # skill | agent | command | mcp | hook
category: automation
version: 1.2.0
author: SawyerHood
---
# Browser Automation

正文内容...
```

### 5.3 SyncService（核心）

**职责**：由 Worker 调用，执行单次同步的完整逻辑。不再负责并发控制（由队列去重保证）。

```go
// server/internal/services/sync_service.go

package services

type SyncService struct {
    DB      *gorm.DB
    Git     *GitService
    Parser  *ParserService
}

type SyncOptions struct {
    TriggerType string // scheduled | manual | webhook
    TriggerUser string
    DryRun      bool   // 仅预览变更，不写入数据库
}

type SyncResult struct {
    LogID       string
    CommitSHA   string
    PreviousSHA string
    Status      string
    Added       int
    Updated     int
    Deleted     int
    Skipped     int
    Failed      int
    Errors      []string
    Duration    time.Duration
}

// SyncRegistry 对单个 Registry 执行一次完整同步，由 Worker 调用
func (s *SyncService) SyncRegistry(ctx context.Context, registryID string, opts SyncOptions) (*SyncResult, error)
```

**SyncRegistry 执行步骤**

```
1. 查询 Registry 配置（ExternalURL / ExternalBranch / SyncConfig）
2. 创建 SyncLog（status=running）
3. 更新 Registry.SyncStatus = "syncing"
4. defer：无论成功失败都更新 SyncLog 和 Registry 状态
5. GitService.Clone() → 临时目录
6. 对比 CommitSHA == Registry.LastSyncSHA → 相同则跳过
7. GitService.ListFiles()（按 includePatterns / excludePatterns 过滤）
8. 遍历文件 → ParserService.Parse()
   ├─ 新文件          → db.Create(SkillItem + SkillVersion)
   ├─ ContentHash 变化 → db.Save(SkillItem) + db.Create(SkillVersion)
   ├─ 数据库有/仓库无  → db.Update("status", "archived")
   └─ ContentHash 相同 → 跳过
9. 更新 Registry.LastSyncSHA / LastSyncedAt / SyncStatus
10. 更新 SyncLog（status=success/failed，统计数据）
11. GitService.Cleanup()
```

**增量对比逻辑**

```
SkillItem.SourceSHA  ←→  ContentHash(远程文件)
  相同 → Skipped++
  不同 → Updated++（保存新内容，插入 SkillVersion）
  本地无 → Added++
  远程无 → Deleted++（status 改为 archived，不物理删除）
```

---

## 六、队列与调度设计

### 6.1 整体分层

```
┌──────────────────────────────────────────────────────┐
│  生产者层                                              │
│  ┌─────────────────┐   ┌──────────────┐              │
│  │ Scheduler        │   │  API Handler │              │
│  │ (gocron 定时触发) │   │ (手动/Webhook)│              │
│  └────────┬────────┘   └──────┬───────┘              │
│           │  enqueue          │  enqueue              │
└───────────┼───────────────────┼──────────────────────┘
            ↓                   ↓
┌──────────────────────────────────────────────────────┐
│  队列层：SyncJob 表（PostgreSQL）                      │
│  pending → running → success / failed                 │
└──────────────────────────┬───────────────────────────┘
                           ↓  SELECT FOR UPDATE SKIP LOCKED
┌──────────────────────────────────────────────────────┐
│  消费者层：Worker Pool                                 │
│  Worker-1  Worker-2  Worker-N                         │
│       └──────────┴──── SyncService.SyncRegistry()    │
└──────────────────────────────────────────────────────┘
```

### 6.2 JobService（入队逻辑）

```go
// server/internal/services/job_service.go

package services

type JobService struct {
    DB *gorm.DB
}

// Enqueue 向队列写入一条同步任务
// 若该 Registry 已有 pending/running 任务：
//   - triggerType=scheduled → 静默跳过
//   - triggerType=manual/webhook → 返回 ErrJobAlreadyQueued
func (j *JobService) Enqueue(registryID, triggerType, triggerUser string, opts EnqueueOptions) (*models.SyncJob, error)

type EnqueueOptions struct {
    Priority    int       // 1=高（manual/webhook），5=普通（scheduled）
    ScheduledAt time.Time // 默认 time.Now()，可设置延迟执行
    DryRun      bool
    MaxAttempts int       // 默认 3
}

// Cancel 取消 pending 状态的任务
func (j *JobService) Cancel(jobID, operatorID string) error

// CancelByRegistry 取消某 Registry 所有 pending 任务（Registry 被禁用时调用）
func (j *JobService) CancelByRegistry(registryID string) error

// GetPendingCount 获取队列中 pending 任务数（用于 API 状态展示）
func (j *JobService) GetPendingCount(registryID string) (int64, error)
```

### 6.3 Scheduler（定时生产者）

gocron 职责缩减为**定时入队**，不再直接执行同步。

```go
// server/internal/scheduler/scheduler.go

package scheduler

type Scheduler struct {
    cron       gocron.Scheduler
    jobService *services.JobService
    db         *gorm.DB
    jobMap     map[string]uuid.UUID // registryID → gocron job UUID
    mu         sync.RWMutex
}

// Start 启动调度器，加载所有 SyncEnabled=true 的 Registry 并注册定时任务
func (s *Scheduler) Start() error

// Stop 优雅停止
func (s *Scheduler) Stop()

// RegisterRegistry 为单个 Registry 注册/更新定时入队任务
func (s *Scheduler) RegisterRegistry(registry *models.SkillRegistry) error

// UnregisterRegistry 移除 Registry 的定时任务
func (s *Scheduler) UnregisterRegistry(registryID string)
```

**RegisterRegistry 核心逻辑**

```go
job, err := s.cron.NewJob(
    gocron.DurationJob(time.Duration(registry.SyncInterval) * time.Second),
    gocron.NewTask(func() {
        // 只入队，不执行；已有任务则静默跳过
        s.jobService.Enqueue(registry.ID, "scheduled", "", services.EnqueueOptions{
            Priority: 5,
        })
    }),
    gocron.WithSingletonMode(gocron.LimitModeReschedule),
)
s.jobMap[registry.ID] = job.ID()
```

### 6.4 Worker Pool（消费者）

```go
// server/internal/worker/worker.go

package worker

type WorkerPool struct {
    db          *gorm.DB
    syncService *services.SyncService
    concurrency int           // 并发 Worker 数，默认 3
    pollInterval time.Duration // 轮询间隔，默认 5s
    stopCh      chan struct{}
}

// Start 启动 Worker Pool
func (p *WorkerPool) Start()

// Stop 优雅停止（等待当前运行中的任务完成）
func (p *WorkerPool) Stop()
```

**单个 Worker 执行循环**

```go
func (p *WorkerPool) runWorker() {
    ticker := time.NewTicker(p.pollInterval)
    for {
        select {
        case <-p.stopCh:
            return
        case <-ticker.C:
            p.processOne()
        }
    }
}

func (p *WorkerPool) processOne() {
    var job models.SyncJob

    // SKIP LOCKED：多 Worker 并发拉取时互不阻塞
    err := p.db.Transaction(func(tx *gorm.DB) error {
        result := tx.Raw(`
            SELECT * FROM sync_jobs
            WHERE status = 'pending'
              AND scheduled_at <= NOW()
            ORDER BY priority ASC, scheduled_at ASC
            LIMIT 1
            FOR UPDATE SKIP LOCKED
        `).Scan(&job)
        if result.RowsAffected == 0 {
            return ErrNoJob
        }
        return tx.Model(&job).Updates(map[string]any{
            "status":     "running",
            "started_at": time.Now(),
        }).Error
    })

    if errors.Is(err, ErrNoJob) {
        return
    }

    // 执行同步
    var opts services.SyncOptions
    json.Unmarshal(job.Payload, &opts)
    opts.TriggerType = job.TriggerType
    opts.TriggerUser = job.TriggerUser

    result, syncErr := p.syncService.SyncRegistry(context.Background(), job.RegistryID, opts)

    // 更新任务状态
    p.finalizeJob(&job, result, syncErr)
}
```

**失败重试退避策略**

```go
func (p *WorkerPool) finalizeJob(job *models.SyncJob, result *services.SyncResult, err error) {
    updates := map[string]any{"finished_at": time.Now()}

    if err != nil && job.RetryCount+1 < job.MaxAttempts {
        // 指数退避：30s, 5min, 30min
        backoff := time.Duration(math.Pow(10, float64(job.RetryCount+1))) * 3 * time.Second
        updates["status"]       = "pending"
        updates["retry_count"]  = job.RetryCount + 1
        updates["scheduled_at"] = time.Now().Add(backoff)
        updates["last_error"]   = err.Error()
    } else if err != nil {
        updates["status"]     = "failed"
        updates["last_error"] = err.Error()
    } else {
        updates["status"]    = "success"
        updates["sync_log_id"] = result.LogID
    }

    p.db.Model(job).Updates(updates)
}
```

**退避时间表**

| 第 N 次重试 | 等待时间 |
|------------|---------|
| 第 1 次 | 30 秒 |
| 第 2 次 | 5 分钟 |
| 第 3 次 | 终态 failed |

### 6.5 各进程入口

**`cmd/api/main.go`**（API Server，单副本）

```go
db := database.Connect()

jobSvc := &services.JobService{DB: db}

// 启动调度器（生产者，只入队）
sched := &scheduler.Scheduler{
    JobService: jobSvc,
    DB:         db,
}
if err := sched.Start(); err != nil {
    log.Fatalf("Failed to start scheduler: %v", err)
}
defer sched.Stop()

handlers.JobService = jobSvc
handlers.SyncScheduler = sched

// 启动 HTTP Server
r := setupRouter()
r.Run(":8080")
```

**`cmd/worker/main.go`**（Worker，可多副本横向扩展）

```go
db := database.Connect()

syncSvc := &services.SyncService{
    DB:     db,
    Git:    &services.GitService{TempBaseDir: os.Getenv("SYNC_TMP_DIR")},
    Parser: &services.ParserService{},
}

// Worker 数量通过环境变量配置，默认 3
concurrency, _ := strconv.Atoi(os.Getenv("WORKER_CONCURRENCY"))
if concurrency <= 0 {
    concurrency = 3
}

pool := &worker.WorkerPool{
    DB:           db,
    SyncService:  syncSvc,
    Concurrency:  concurrency,
    PollInterval: 5 * time.Second,
}

// 优雅退出
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer stop()

pool.Start()
<-ctx.Done()
pool.Stop()
```

---

## 七、API 设计

### 7.1 组织创建扩展

```go
// POST /api/organizations
type CreateOrgRequest struct {
    Name        string `json:"name" binding:"required"`
    DisplayName string `json:"displayName"`
    Description string `json:"description"`
    Visibility  string `json:"visibility"`
    OrgType     string `json:"orgType"` // normal（默认）| sync

    // OrgType=sync 时必填，用于初始化关联的 SkillRegistry 同步配置
    SyncRegistry *CreateSyncRegistryInput `json:"syncRegistry"`
}

type CreateSyncRegistryInput struct {
    ExternalURL      string   `json:"externalUrl" binding:"required"`
    ExternalBranch   string   `json:"externalBranch"`   // 默认 main
    SyncInterval     int      `json:"syncInterval"`     // 默认 3600
    SyncEnabled      bool     `json:"syncEnabled"`
    IncludePatterns  []string `json:"includePatterns"`
    ExcludePatterns  []string `json:"excludePatterns"`
    ConflictStrategy string   `json:"conflictStrategy"` // keep_remote（默认）| keep_local
    WebhookSecret    string   `json:"webhookSecret"`
}
```

**CreateOrg 处理逻辑扩展**

```
POST /api/organizations
  1. 校验请求体
  2. 若 OrgType=sync，校验 SyncRegistry.ExternalURL 非空
  3. 创建 Organization（写入 OrgType）
  4. 若 OrgType=sync：
     a. 自动创建同名 SkillRegistry（SourceType=external，写入同步配置字段）
     b. 调用 SyncScheduler.RegisterRegistry() 注册定时任务
     c. 可选：立即触发一次初始同步
  5. 返回 Organization + 自动创建的 Registry 信息
```

### 7.2 同步操作接口

所有触发同步的接口统一通过 `JobService.Enqueue()` 入队，不直接执行：

```go
// main.go 路由新增
api.POST("/organizations/:id/sync",        handlers.TriggerOrgSync)     // 转发到 Org 关联的 Registry
api.POST("/organizations/:id/sync/cancel", handlers.CancelOrgSync)
api.GET("/organizations/:id/sync-status",  handlers.GetOrgSyncStatus)
api.GET("/organizations/:id/sync-logs",    handlers.ListOrgSyncLogs)
api.GET("/organizations/:id/sync-jobs",    handlers.ListOrgSyncJobs)
api.POST("/registries/:id/sync",           handlers.TriggerRegistrySync) // 直接操作 Registry
api.POST("/registries/:id/sync/cancel",    handlers.CancelRegistrySync)
api.GET("/registries/:id/sync-status",     handlers.GetRegistrySyncStatus)
api.GET("/registries/:id/sync-logs",       handlers.ListRegistrySyncLogs)
api.GET("/registries/:id/sync-jobs",       handlers.ListRegistrySyncJobs)
api.GET("/sync-logs/:id",                  handlers.GetSyncLogDetail)
api.GET("/sync-jobs/:id",                  handlers.GetSyncJobDetail)
api.POST("/webhooks/github",               handlers.HandleGitHubWebhook)
```

### 7.3 接口说明

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/organizations` | 创建组织，`orgType=sync` 时自动创建并配置关联 Registry |
| `POST` | `/organizations/:id/sync` | 手动入队同步任务，`?dryRun=true` 为预览模式 |
| `POST` | `/organizations/:id/sync/cancel` | 取消 pending 状态的任务 |
| `GET` | `/organizations/:id/sync-status` | 当前同步状态 + 队列中任务数 + 最近日志摘要 |
| `GET` | `/organizations/:id/sync-logs` | 分页查询同步执行日志 |
| `GET` | `/organizations/:id/sync-jobs` | 分页查询任务队列（含 pending/running/failed） |
| `POST` | `/registries/:id/sync` | 直接为 Registry 入队同步任务 |
| `GET` | `/registries/:id/sync-status` | 获取 Registry 同步状态 |
| `GET` | `/registries/:id/sync-logs` | 查询 Registry 同步日志 |
| `GET` | `/registries/:id/sync-jobs` | 查询 Registry 任务队列 |
| `GET` | `/sync-logs/:id` | 查询单条执行日志详情 |
| `GET` | `/sync-jobs/:id` | 查询单条任务详情 |
| `POST` | `/webhooks/github` | 接收 GitHub push 事件，入队对应 Registry 同步任务 |

**TriggerSync Handler 示例**

```go
func TriggerRegistrySync(c *gin.Context) {
    registryID := c.Param("id")
    dryRun := c.Query("dryRun") == "true"
    userID := c.GetString("userID")

    job, err := handlers.JobService.Enqueue(registryID, "manual", userID, services.EnqueueOptions{
        Priority: 1,
        DryRun:   dryRun,
    })
    if errors.Is(err, services.ErrJobAlreadyQueued) {
        c.JSON(http.StatusConflict, gin.H{"message": "已有同步任务在队列中，请稍后再试"})
        return
    }
    c.JSON(http.StatusAccepted, gin.H{"jobId": job.ID, "status": job.Status})
}
```

### 7.4 UpdateRegistry 联动调度器

```go
// 在现有 UpdateRegistry handler 末尾新增：
// 如果修改了 SyncEnabled 或 SyncInterval，同步更新调度器
if req.SyncEnabled != nil || req.SyncInterval != 0 {
    handlers.SyncScheduler.RegisterRegistry(&registry)
}
// 如果禁用同步，取消队列中所有 pending 任务
if req.SyncEnabled != nil && !*req.SyncEnabled {
    handlers.JobService.CancelByRegistry(registry.ID)
}
```

### 7.5 GitHub Webhook 处理逻辑

```
POST /api/webhooks/github
  1. 验证 X-Hub-Signature-256 HMAC 签名（使用 SyncConfig.webhookSecret）
  2. 解析 push event，提取 repository.html_url
  3. 查询 ExternalURL 匹配该 URL 的 SkillRegistry
  4. 调用 JobService.Enqueue(registryID, "webhook", "", EnqueueOptions{Priority: 1})
```

---

## 八、数据流全景图

```
┌─────────────────────────────────────────────────────────────────┐
│  cmd/api  进程（单副本）                                          │
│                                                                   │
│  用户创建同步类型组织 → CreateOrg API                             │
│    └─ 自动创建 SkillRegistry（写入同步配置）                      │
│    └─ SyncScheduler.RegisterRegistry()                           │
│                                                                   │
│  ┌─────────────────────┐   ┌──────────────────────────┐         │
│  │  gocron Scheduler    │   │  API Handler             │         │
│  │  定时触发，priority=5 │   │  手动/Webhook，priority=1 │         │
│  └──────────┬──────────┘   └─────────────┬────────────┘         │
│             └──────────────┬─────────────┘                       │
│                  JobService.Enqueue()                             │
└──────────────────────────┬──────────────────────────────────────┘
                           ↓  写入
          ┌────────────────────────────────────┐
          │     SyncJob 表（PostgreSQL）         │
          │  pending → running → success/failed  │
          │  同一 Registry 只允许一条活跃任务     │
          └────────────────────────────────────┘
                           ↑  SELECT FOR UPDATE SKIP LOCKED
                           │  每 5 秒轮询，priority ASC + scheduled_at ASC
┌──────────────────────────┴──────────────────────────────────────┐
│  cmd/worker 进程（可多副本横向扩展）                               │
│                                                                   │
│  Worker-1        Worker-2        Worker-N                         │
│     ↓                ↓                ↓                          │
│  ① 竞争拉取任务（SKIP LOCKED 保证互斥），status → running         │
│  ② SyncService.SyncRegistry()                                    │
│     ├─ 创建 SyncLog（running）                                    │
│     ├─ GitService.Clone() → /tmp/{registryID}-{ts}/              │
│     ├─ CommitSHA 未变 → skipped，结束                            │
│     ├─ ListFiles() → ParserService.Parse()                       │
│     │   ├─ 新增 → Create(SkillItem + SkillVersion)               │
│     │   ├─ 变更 → Save(SkillItem) + Create(SkillVersion)         │
│     │   ├─ 删除 → Update(status="archived")                      │
│     │   └─ 未变 → 跳过                                           │
│     ├─ 更新 Registry（LastSyncSHA / SyncStatus="idle"）           │
│     └─ 更新 SyncLog（success/failed）                            │
│  ③ 更新 SyncJob（success/failed，关联 SyncLogID）                │
│  ④ GitService.Cleanup()                                          │
│  ⑤ [失败且可重试] SyncJob → pending，ScheduledAt += 退避时间     │
└─────────────────────────────────────────────────────────────────┘
```

**横向扩展说明**

- 新增 Worker 副本只需在 docker-compose 中调整 `replicas` 或另起容器，无需任何配置变更
- 多个 Worker 通过 `SKIP LOCKED` 自动分摊任务，不会重复执行同一个 Job
- API Server 保持单副本（Scheduler 防止重复入队），Worker 副本数按同步任务量弹性调整

---

## 九、前端界面设计

### 9.1 创建组织弹窗扩展

在现有"创建组织"表单中新增类型选择：

```
创建组织
├── 组织名称（Input）
├── 显示名称（Input）
├── 描述（Textarea）
├── 可见性（Select：公开 / 私有）
├── 组织类型（Radio）
│   ├── ● 普通组织（默认）
│   └── ○ 同步组织（从 Git 仓库同步技能）
└── [当选择"同步组织"时展开]
    ├── Git 仓库 URL（Input，必填）
    ├── 分支（Input，默认 main）
    ├── 启用自动同步（Switch）
    ├── 同步间隔（Select：每小时 / 每6小时 / 每天 / 自定义秒数）
    ├── 包含模式（Textarea，每行一个 glob 表达式）
    ├── 排除模式（Textarea）
    └── 冲突策略（Select：保留远程 / 保留本地）
```

### 9.2 同步类型组织详情页

同步类型组织的详情页在现有 Tab 基础上调整：

```
同步组织详情页
├── 概览 Tab（现有，新增同步状态卡片）
│   └── 状态卡片：当前状态 | 上次同步时间 | 上次 CommitSHA | Git 仓库地址
├── 技能列表 Tab（现有，只读，不显示手动新增入口）
└── 同步管理 Tab（新增，实为关联 Registry 的同步配置透传）
    ├── 配置区（直接编辑 Registry 的同步字段）
    │   ├── Git 仓库 URL（Input）
    │   ├── 分支（Input）
    │   ├── 启用自动同步（Switch）
    │   ├── 同步间隔（Select）
    │   ├── 包含模式（Textarea）
    │   ├── 排除模式（Textarea）
    │   └── 冲突策略（Select）
    ├── 操作区
    │   ├── [保存配置]
    │   ├── [立即同步]（触发后禁用，显示同步中状态）
    │   └── [预览变更]（dryRun=true，展示将要变更的条目）
    └── 日志列表
        └── 每条：时间 | 触发方式 | 状态徽章 | 新增/更新/删除数量
```

---

## 十、实施路线图

| 阶段 | 内容 | 预估时间 |
|------|------|---------|
| **P0** | 工程结构调整（main.go 拆分为 cmd/api + cmd/worker，共享 internal/） | 0.5天 |
| **P0** | 数据模型扩展（Organization 新增 OrgType，SkillRegistry 新增 SyncStatus/SyncConfig，新增 SyncJob / SyncLog 表） | 0.5天 |
| **P0** | CreateOrg API 扩展（sync 类型校验 + 自动创建 Registry + 注册调度器） | 1天 |
| **P0** | GitService（clone / fetch / listFiles / readFile / hash / cleanup） | 2天 |
| **P0** | ParserService（SKILL.md frontmatter 解析 + ItemType/Slug 推断） | 1天 |
| **P0** | SyncService 核心（SyncRegistry + 全量同步 + SyncLog 记录） | 1.5天 |
| **P0** | JobService（Enqueue / Cancel / 去重检查） | 0.5天 |
| **P0** | Worker Pool（SKIP LOCKED 拉取 + 执行 + 退避重试）+ cmd/worker 入口 | 1天 |
| **P0** | 调度器集成（gocron 定时入队）+ cmd/api 入口 | 0.5天 |
| **P0** | 同步 API（触发入队 / 取消 / 状态 / 日志 / 任务列表，Org 和 Registry 双入口） | 1天 |
| **P0** | docker-compose 新增 worker 服务（共享 DB，独立容器） | 0.5天 |
| **P1** | 增量同步优化（ContentHash 对比，跳过未变文件） | 1天 |
| **P1** | 冲突策略实现（keep_local / keep_remote） | 0.5天 |
| **P1** | 前端：创建组织弹窗扩展 + 同步管理 Tab + 任务队列列表 + 日志列表 | 2.5天 |
| **P2** | GitHub Webhook 接收（push 事件入队 + HMAC 验证） | 1天 |
| **P2** | DryRun 预览模式 | 1天 |
| **P2** | plugin.json / AGENTS.md 解析支持 | 1天 |

**总计约 17 个工作日**，P0 阶段（10天）即可上线基础同步能力（含独立 Worker 部署）。
