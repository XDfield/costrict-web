> **实现状态：待实现**
>
> - 状态：📋 设计阶段
> - 涉及仓库：`costrict-web`（服务端）、`opencode`（客户端）
> - 前置依赖：opencode 侧 session usage statistics 提案（`opencode/docs/session-usage-statistics.md`）

---

# Session Usage Statistics 上报与活跃度统计技术提案

## 目录

- [概述](#概述)
- [背景与动机](#背景与动机)
- [整体架构](#整体架构)
- [数据模型](#数据模型)
- [API 设计](#api-设计)
- [客户端实现（opencode 侧）](#客户端实现opencode-侧)
- [服务端实现（costrict-web 侧）](#服务端实现costrict-web-侧)
- [活跃度计算详解](#活跃度计算详解)
- [扩展考虑](#扩展考虑)
- [实施计划](#实施计划)

---

## 概述

将 opencode CLI 本地 SQLite 中聚合的会话用量数据上报到 costrict-web server，并在服务端实现按 `git_repo_url` + 用户维度的对话活跃度聚合查询，支持 7 天折线图展示。

### 核心需求

| # | 需求 | 说明 |
|---|------|------|
| 1 | 数据上报 | opencode CLI 定期将 session usage 汇总数据推送到 costrict-web |
| 2 | 服务端存储 | costrict-web 使用独立 SQLite 文件持久化上报数据（不侵入业务 PostgreSQL） |
| 3 | 活跃度查询 | 提供 API 查询指定 `git_repo_url` 下各用户近 7 天的对话次数折线图数据 |

### 参考来源

| 来源 | 路径 | 说明 |
|------|------|------|
| session usage 原始提案 | `opencode/docs/session-usage-statistics.md` | 客户端数据聚合 SQL 与字段定义 |
| Learning Pusher | `opencode/src/learning/pusher.ts` | 已有的认证上报模式 |
| Stats 命令 | `opencode/src/cli/cmd/stats.ts` | 已有的 session 聚合实现 |
| CoStrict 认证 | `opencode/src/costrict/provider/` | OAuth + JWT 认证体系 |

---

## 背景与动机

opencode CLI 在本地 SQLite 中记录了完整的会话与消息数据（session、message、project 表），并可通过 `stats` 命令聚合展示 token 用量和费用统计。但这些数据仅存在于各用户本地设备，存在以下问题：

1. **无法跨设备汇总**：同一用户在多台设备上的使用情况无法统一查看
2. **无法团队级观测**：团队管理者无法了解成员在各仓库上的 AI 编码活跃情况
3. **数据孤岛**：本地数据随设备丢失、重装而消失，缺乏持久备份
4. **无法驱动运营决策**：缺乏聚合数据支撑仓库维度的 AI 工具使用分析

本提案将本地聚合数据上报到 costrict-web server，建立中心化的用量数据视图。

---

## 整体架构

```
┌─────────────────────────────────────────────┐
│  opencode CLI (client)                      │
│                                             │
│  SQLite ──► aggregateSessionStats()         │
│             + git remote get-url origin     │
│                      │                      │
│            UsageReporter (new module)        │
│                      │                      │
│         POST /cloud-api/api/usage/report    │
│         (Bearer token, CoStrict auth)       │
└──────────────────────┬──────────────────────┘
                       │ HTTPS
                       ▼
┌─────────────────────────────────────────────┐
│  costrict-web server (Go + Gin)             │
│                                             │
│  POST /api/usage/report   ← 接收上报       │
│  GET  /api/usage/activity ← 7d 折线图查询  │
│                                             │
│  ┌───────────────────────────┐              │
│  │ session_usage_reports     │ (独立 SQLite)│
│  │ 每个 session+model 一行   │              │
│  │ data/usage_stats.db       │              │
│  └───────────────────────────┘              │
│                                             │
│  ※ 统计数据与业务 PostgreSQL 完全隔离       │
│  ※ 后续对接 ES 统计指标服务后可直接迁移     │
└─────────────────────────────────────────────┘
```

---

## 数据模型

### 上报数据结构（客户端 → 服务端）

```jsonc
// POST /api/usage/report
{
  "reports": [
    {
      "session_id": "abc-123",
      "date": "2026-03-30",
      "updated": "2026-03-30",
      "model_id": "claude-sonnet-4-20250514",
      "provider_id": "anthropic",
      "input_tokens": 12340,
      "output_tokens": 5670,
      "reasoning_tokens": 890,
      "cache_read_tokens": 1000,
      "cache_write_tokens": 500,
      "cost": 0.05,
      "rounds": 3,
      "git_repo_url": "https://github.com/zgsm-ai/opencode",
      "git_worktree": "/home/user/projects/opencode"
    }
  ],
  "device_id": "device-uuid",
  "reported_at": "2026-03-30T23:59:00Z"
}
```

字段对应关系来自 `opencode/docs/session-usage-statistics.md` 中 Target Output Fields 的定义。

### 服务端数据库表（独立 SQLite）

> **存储选型说明**：用量统计数据是典型的时序/统计类数据（写多读少、追加为主、按时间范围聚合），
> 后续将对接 ES 统计指标服务。当前阶段使用独立 SQLite 文件存储，有以下优势：
>
> 1. **不侵入业务 PG**：统计数据与业务数据完全隔离，不增加业务库负担
> 2. **迁移 ES 路径最短**：SQLite 全量 `SELECT *` → ES Bulk API 即可完成迁移
> 3. **部署零依赖**：单文件存储，无需额外中间件
> 4. **项目已有 SQLite 驱动依赖**：`gorm.io/driver/sqlite v1.6.0` 已在 go.mod 中

```sql
-- 初始化建表（由 GORM AutoMigrate 或手动执行）

CREATE TABLE IF NOT EXISTS session_usage_reports (
    id                 TEXT PRIMARY KEY,
    user_id            TEXT    NOT NULL,
    device_id          TEXT    DEFAULT '',
    session_id         TEXT    NOT NULL,
    date               TEXT    NOT NULL,        -- ISO date 'YYYY-MM-DD'
    updated            TEXT    NOT NULL,        -- ISO date 'YYYY-MM-DD'
    model_id           TEXT    NOT NULL,
    provider_id        TEXT    NOT NULL DEFAULT '',
    input_tokens       INTEGER NOT NULL DEFAULT 0,
    output_tokens      INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens   INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    cost               REAL    NOT NULL DEFAULT 0,
    rounds             INTEGER NOT NULL DEFAULT 0,
    git_repo_url       TEXT    NOT NULL DEFAULT '',
    git_worktree       TEXT    NOT NULL DEFAULT '',
    reported_at        TEXT    NOT NULL DEFAULT (datetime('now')),
    created_at         TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT    NOT NULL DEFAULT (datetime('now'))
);

-- 幂等约束：同一 user + session + model 只允许一条记录
CREATE UNIQUE INDEX IF NOT EXISTS uq_usage_session_model
    ON session_usage_reports (user_id, session_id, model_id);

-- 核心查询索引
CREATE INDEX IF NOT EXISTS idx_usage_repo_user_date
    ON session_usage_reports (git_repo_url, user_id, date);
CREATE INDEX IF NOT EXISTS idx_usage_user_date
    ON session_usage_reports (user_id, date);
CREATE INDEX IF NOT EXISTS idx_usage_repo_date
    ON session_usage_reports (git_repo_url, date);
CREATE INDEX IF NOT EXISTS idx_usage_device
    ON session_usage_reports (device_id);
```

### GORM Model

```go
// server/internal/models/usage_report.go
package models

import "time"

// SessionUsageReport 存储在独立 SQLite 数据库中（非业务 PostgreSQL）。
// 字段类型使用 SQLite 兼容的通用类型，ID 由应用层生成 UUID。
type SessionUsageReport struct {
    ID               string    `gorm:"primaryKey" json:"id"`
    UserID           string    `gorm:"not null;uniqueIndex:uq_usage_session_model" json:"userId"`
    DeviceID         string    `gorm:"index:idx_usage_device" json:"deviceId"`
    SessionID        string    `gorm:"not null;uniqueIndex:uq_usage_session_model" json:"sessionId"`
    Date             string    `gorm:"not null" json:"date"`                  // ISO date "YYYY-MM-DD"
    Updated          string    `gorm:"not null" json:"updated"`               // ISO date "YYYY-MM-DD"
    ModelID          string    `gorm:"not null;uniqueIndex:uq_usage_session_model" json:"modelId"`
    ProviderID       string    `gorm:"not null;default:''" json:"providerId"`
    InputTokens      int64     `gorm:"not null;default:0" json:"inputTokens"`
    OutputTokens     int64     `gorm:"not null;default:0" json:"outputTokens"`
    ReasoningTokens  int64     `gorm:"not null;default:0" json:"reasoningTokens"`
    CacheReadTokens  int64     `gorm:"not null;default:0" json:"cacheReadTokens"`
    CacheWriteTokens int64     `gorm:"not null;default:0" json:"cacheWriteTokens"`
    Cost             float64   `gorm:"not null;default:0" json:"cost"`
    Rounds           int       `gorm:"not null;default:0" json:"rounds"`
    GitRepoURL       string    `gorm:"not null;default:'';index:idx_usage_repo_user_date;index:idx_usage_repo_date" json:"gitRepoUrl"`
    GitWorktree      string    `gorm:"not null;default:''" json:"gitWorktree"`
    ReportedAt       time.Time `gorm:"not null" json:"reportedAt"`
    CreatedAt        time.Time `json:"createdAt"`
    UpdatedAt        time.Time `json:"updatedAt"`
}

func (SessionUsageReport) TableName() string {
    return "session_usage_reports"
}
```

> **与 PG 版本的关键差异**：
> - `ID` 不使用 `gen_random_uuid()`，改由应用层 `uuid.New().String()` 生成
> - `Date`/`Updated` 使用 `string` 类型存储 ISO date，SQLite 无原生 DATE 类型
> - 去掉所有 PG 特有的 `type:varchar(N)`、`type:uuid`、`type:numeric(12,6)` 标注
> - 索引由 GORM AutoMigrate 自动创建

### 设计决策

| 决策点 | 选择 | 理由 |
|--------|------|------|
| **存储引擎** | 独立 SQLite 文件 (`data/usage_stats.db`) | 不侵入业务 PG；项目已有 SQLite 驱动依赖；后续迁移 ES 路径最短 |
| **存储粒度** | 每 session+model 一行 | 与原提案 SQL `GROUP BY s.id, modelID` 对齐；同一 session 切换 model 产生多行 |
| **幂等机制** | `UNIQUE (user_id, session_id, model_id)` + UPSERT | 客户端可能重复上报（重启、网络重试），`INSERT OR REPLACE` / `ON CONFLICT DO UPDATE` 保证幂等 |
| **用户标识** | JWT `userId`（Casdoor） | 复用现有 RequireAuth 中间件，服务端从 token 提取，不可伪造 |
| **git_repo_url** | 客户端上报前规范化 | 去 `.git` 后缀、SSH → HTTPS、统一小写，避免同仓库因 URL 差异导致数据分散 |
| **时间粒度** | `date` 列按日存储 (TEXT `YYYY-MM-DD`) | 7 天折线图按 `date` GROUP BY，SQLite 无原生 DATE 类型，TEXT 存 ISO date 即可 |
| **ID 生成** | 应用层 `uuid.New().String()` | SQLite 无 `gen_random_uuid()` 函数，由 Go 代码生成 |

---

## API 设计

### 数据上报

```
POST /api/usage/report
Authorization: Bearer <token>
Content-Type: application/json
```

**Request Body:**

```jsonc
{
  "reports": [
    {
      "session_id": "string",     // required
      "date": "2026-03-30",       // required, ISO date
      "updated": "2026-03-30",    // required, ISO date
      "model_id": "string",       // required
      "provider_id": "string",
      "input_tokens": 0,
      "output_tokens": 0,
      "reasoning_tokens": 0,
      "cache_read_tokens": 0,
      "cache_write_tokens": 0,
      "cost": 0.0,
      "rounds": 1,                // required, >= 1
      "git_repo_url": "string",   // required
      "git_worktree": "string"    // optional
    }
  ],
  "device_id": "string"           // optional
}
```

**Response 200:**

```jsonc
{
  "accepted": 5,
  "skipped": 0,
  "errors": []
}
```

**服务端处理逻辑：**

1. 从 JWT 提取 `userId`
2. 逐条校验 `reports`：`session_id`、`model_id`、`git_repo_url` 非空；`rounds >= 1`
3. 批量 UPSERT：`INSERT ... ON CONFLICT (user_id, session_id, model_id) DO UPDATE SET ...`（SQLite 3.24+ 原生支持）
4. 仅在 `reported_at` 新于已有记录时覆盖 token/cost/rounds 字段，防止旧数据覆盖新数据

### 活跃度查询

```
GET /api/usage/activity?git_repo_url=<url>&days=7
Authorization: Bearer <token>
```

**Query Parameters:**

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `git_repo_url` | string | Y | - | git 仓库 URL（URL encode） |
| `days` | int | N | 7 | 时间范围天数（1-90） |

**Response 200:**

```jsonc
{
  "git_repo_url": "https://github.com/zgsm-ai/opencode",
  "range": {
    "from": "2026-03-24",
    "to": "2026-03-30"
  },
  "users": [
    {
      "user_id": "casdoor-user-id-1",
      "username": "alice",
      "daily": [
        { "date": "2026-03-24", "sessions": 3 },
        { "date": "2026-03-25", "sessions": 5 },
        { "date": "2026-03-26", "sessions": 0 },
        { "date": "2026-03-27", "sessions": 2 },
        { "date": "2026-03-28", "sessions": 7 },
        { "date": "2026-03-29", "sessions": 1 },
        { "date": "2026-03-30", "sessions": 4 }
      ],
      "total_sessions": 22
    },
    {
      "user_id": "casdoor-user-id-2",
      "username": "bob",
      "daily": [
        { "date": "2026-03-24", "sessions": 1 },
        { "date": "2026-03-25", "sessions": 0 },
        { "date": "2026-03-26", "sessions": 3 },
        { "date": "2026-03-27", "sessions": 0 },
        { "date": "2026-03-28", "sessions": 2 },
        { "date": "2026-03-29", "sessions": 0 },
        { "date": "2026-03-30", "sessions": 1 }
      ],
      "total_sessions": 7
    }
  ]
}
```

---

## 客户端实现（opencode 侧）

### 新增模块

`packages/opencode/src/usage/reporter.ts` — 复用 `learning/pusher.ts` 的认证上报模式。

### 核心流程

```
Session 结束 / Bus event
       │
       ▼
  aggregateForSession(sessionId)    ← 复用 stats.ts 中的聚合逻辑
       │
       ▼
  resolveGitRepoUrl(worktree)       ← git -C <worktree> remote get-url origin
       │
       ▼
  normalize(url)                    ← SSH → HTTPS, 去 .git 后缀, 统一小写
       │
       ▼
  enqueue(report)                   ← 写入内存队列
       │
       ▼
  flush() [debounce 5s]             ← 批量 POST /api/usage/report
```

### 触发时机

| 时机 | 方式 | 说明 |
|------|------|------|
| Session 结束/切换 | `Bus.subscribe(Session.Event.Updated)` | 实时上报，debounce 合并 |
| CLI 退出 | `process.on("beforeExit")` | flush 未发送的队列 |
| 定期兜底 | `setInterval(300_000)` | 每 5 分钟 flush，防止长会话丢数据 |

### git_repo_url 规范化

```typescript
function normalize(url: string): string {
  return url
    .replace(/\.git$/, "")
    .replace(/^git@([^:]+):/, "https://$1/")
    .replace(/\/$/, "")
    .toLowerCase()
}
```

### 本地队列与重试

- 内存队列，flush 失败时保留，下次重试
- 最多保留 500 条未上报记录
- 3 次重试后丢弃，避免永久堆积
- 复用 `createAuthenticatedFetch()` 处理 token 过期刷新

### 隐私控制

- `git_worktree`（本地路径）标记为可选字段，默认不上传
- 用户可通过配置 `usage.report = false` 关闭上报功能
- 首次上报前在 TUI 中显示提示

---

## 服务端实现（costrict-web 侧）

### 文件结构

```
server/
├── internal/
│   ├── models/
│   │   └── usage_report.go          # GORM model（SQLite 兼容）
│   ├── database/
│   │   ├── database.go              # 业务 PostgreSQL 连接（已有）
│   │   └── usage_db.go              # 独立 SQLite 连接（新增）
│   ├── handlers/
│   │   └── usage.go                 # HTTP handlers (Report, Activity)
│   ├── services/
│   │   └── usage.go                 # 业务逻辑（BatchUpsert, GetActivity）
│   └── ...
├── data/
│   └── usage_stats.db               # SQLite 数据文件（运行时自动创建）
└── cmd/api/main.go                  # 注册路由 + 初始化 SQLite
```

### 独立 SQLite 连接管理

```go
// server/internal/database/usage_db.go
package database

import (
    "fmt"
    "log"
    "path/filepath"

    "github.com/costrict/costrict-web/server/internal/models"
    "gorm.io/driver/sqlite"
    "gorm.io/gorm"
)

var UsageDB *gorm.DB

// InitializeUsageDB 初始化独立的 SQLite 数据库用于用量统计。
// dataDir 为数据存放目录（如 "data/"），文件名固定为 "usage_stats.db"。
func InitializeUsageDB(dataDir string) (*gorm.DB, error) {
    dbPath := filepath.Join(dataDir, "usage_stats.db")

    db, err := gorm.Open(sqlite.Open(dbPath+"?_journal_mode=WAL&_busy_timeout=5000"), &gorm.Config{
        TranslateError: true,
    })
    if err != nil {
        return nil, fmt.Errorf("failed to open usage SQLite: %w", err)
    }

    // WAL 模式 + 合理的 busy_timeout 支持并发读写
    sqlDB, _ := db.DB()
    sqlDB.SetMaxOpenConns(1)  // SQLite 写串行，限制为 1 个写连接
    sqlDB.SetMaxIdleConns(1)

    // AutoMigrate 建表 + 索引
    if err := db.AutoMigrate(&models.SessionUsageReport{}); err != nil {
        return nil, fmt.Errorf("failed to migrate usage tables: %w", err)
    }

    log.Printf("Usage SQLite initialized at %s", dbPath)
    UsageDB = db
    return db, nil
}

func GetUsageDB() *gorm.DB {
    return UsageDB
}
```

### 路由注册

```go
// cmd/api/main.go — 在 main() 中初始化独立 SQLite + 注册路由

// 初始化用量统计专用 SQLite（与业务 PG 完全隔离）
usageDB, err := database.InitializeUsageDB("data")
if err != nil {
    log.Fatalf("Failed to init usage DB: %v", err)
}

usageSvc := &services.UsageService{DB: usageDB}
usageHandler := handlers.NewUsageHandler(usageSvc)

// authed 分组下新增
usage := authed.Group("/usage")
{
    usage.POST("/report", usageHandler.Report)
    usage.GET("/activity", usageHandler.Activity)
}
```

### BatchUpsert 实现

> **SQLite UPSERT**：SQLite 3.24+ 支持 `INSERT ... ON CONFLICT DO UPDATE`，GORM 的
> `clause.OnConflict` 会自动适配 SQLite 方言。由于 SQLite 写串行化，批量写入在
> 同一事务中执行以减少锁竞争。

```go
func (s *UsageService) BatchUpsert(userID string, items []ReportItem) (int, error) {
    reports := make([]models.SessionUsageReport, 0, len(items))
    for _, item := range items {
        reports = append(reports, models.SessionUsageReport{
            ID:        uuid.New().String(), // 应用层生成 UUID
            UserID:    userID,
            SessionID: item.SessionID,
            ModelID:   item.ModelID,
            // ... map remaining fields
        })
    }

    // 在单个事务中批量 UPSERT，减少 SQLite 写锁开销
    err := s.DB.Transaction(func(tx *gorm.DB) error {
        result := tx.Clauses(clause.OnConflict{
            Columns: []clause.Column{
                {Name: "user_id"},
                {Name: "session_id"},
                {Name: "model_id"},
            },
            DoUpdates: clause.AssignmentColumns([]string{
                "input_tokens", "output_tokens", "reasoning_tokens",
                "cache_read_tokens", "cache_write_tokens",
                "cost", "rounds", "reported_at", "updated_at", "updated",
            }),
        }).CreateInBatches(reports, 100)
        return result.Error
    })

    if err != nil {
        return 0, err
    }
    return len(reports), nil
}
```

### Activity 查询实现

> **SQLite 日期处理**：SQLite 使用 `date()` 函数做日期运算，无 PG 的 `CURRENT_DATE - INTERVAL` 语法。
> `date` 列存储为 TEXT `YYYY-MM-DD` 格式，字符串比较等价于日期比较。

```go
func (s *UsageService) GetActivity(gitRepoURL string, days int) ([]DailyActivity, error) {
    var rows []struct {
        UserID   string
        Date     string
        Sessions int64
    }

    // SQLite: date('now', '-6 days') 计算 6 天前的日期
    s.DB.Model(&models.SessionUsageReport{}).
        Select("user_id, date, COUNT(DISTINCT session_id) AS sessions").
        Where("git_repo_url = ? AND date >= date('now', ?)", gitRepoURL, fmt.Sprintf("-%d days", days-1)).
        Group("user_id, date").
        Order("user_id, date").
        Scan(&rows)

    // Application layer: fill date gaps, resolve usernames
    // ...
}
```

核心 SQL（SQLite 方言）：

```sql
SELECT
    user_id,
    date,
    COUNT(DISTINCT session_id) AS sessions
FROM session_usage_reports
WHERE git_repo_url = ?1
  AND date >= date('now', '-6 days')
GROUP BY user_id, date
ORDER BY user_id, date;
```

应用层后处理：

1. 生成完整日期序列 `[from..to]`
2. 对每个 user_id 左填充缺失日期为 `sessions: 0`
3. 通过 Casdoor API 批量解析 `user_id → username`（复用 `handlers.GetUserNames` 逻辑）

---

## 活跃度计算详解

### "对话次数"定义

**一次对话 = 一个唯一 session_id**。同一 session 内切换 model 会产生多条 `session_usage_reports` 记录，但 `COUNT(DISTINCT session_id)` 确保该 session 只计为 1 次对话。

### 查询流程

```
1. 确定日期范围: [today - (days-1), today]

2. SQL 聚合 (SQLite):
   SELECT user_id, date, COUNT(DISTINCT session_id) AS sessions
   FROM session_usage_reports
   WHERE git_repo_url = ? AND date >= date('now', '-6 days')
   GROUP BY user_id, date

3. 应用层组装:
   - 构建完整日期数组 date_range = [from..to]
   - 对每个 user_id:
     - SQL 结果映射为 map[date]sessions
     - 遍历 date_range，缺失日补 0
     - 计算 total_sessions = SUM(daily.sessions)
   - 批量解析 user_id → username

4. 返回 JSON
```

### 性能分析

| 数据规模估算 | 值 |
|---|---|
| 活跃用户/仓库 | ~50 人 |
| 每人每天 sessions | ~10 |
| 7 天数据量 | ~3,500 行 |
| 90 天数据量 | ~45,000 行 |
| 180 天数据量（保留上限） | ~90,000 行 |

SQLite 在此量级下完全胜任：

- **读性能**：索引 `idx_usage_repo_user_date` 覆盖核心查询，万级数据秒回。SQLite 的 B-tree 索引对此规模极为高效
- **写性能**：WAL 模式下写入不阻塞读取。上报频率低（每 session 一次），单次批量 UPSERT 通常 <100 条，事务耗时 <10ms
- **并发**：SQLite 写串行化，但本场景写入频率极低、单次耗时极短，不构成瓶颈。读操作在 WAL 模式下完全并发
- **文件大小**：90,000 行 × ~500 字节/行 ≈ 45MB，远在 SQLite 舒适区内（SQLite 理论上限 281TB）

> **何时需要迁移**：当活跃用户超过 500 人或数据量超过百万行时，应考虑迁移至 ES。
> 当前方案设计为过渡方案，预留了清晰的 ES 迁移路径。

---

## 数据流时序图

```
Client (opencode)                     Server (costrict-web)
     │                                       │
     │  ──── Session ends ────►               │
     │                                        │
     │  aggregateSessionStats()               │
     │  git remote get-url origin             │
     │  normalize(url)                        │
     │                                        │
     │  ─── POST /api/usage/report ──────►    │
     │      { reports: [...], device_id }     │
     │      Authorization: Bearer <jwt>       │
     │                                        │
     │                                   Validate JWT
     │                                   Extract userId
     │                                   BatchUpsert
     │                                        │
     │  ◄── 200 { accepted: N } ─────────     │
     │                                        │
     │                                        │
     │  Web Dashboard / API Consumer          │
     │                                        │
     │  ─── GET /api/usage/activity ──────►   │
     │      ?git_repo_url=...&days=7          │
     │                                        │
     │                                   SQL aggregate
     │                                   Fill date gaps
     │                                   Resolve usernames
     │                                        │
     │  ◄── 200 { users: [...] } ────────     │
```

---

## 扩展考虑

### 未来可扩展查询维度

基于同一张 `session_usage_reports` 表，后续可扩展以下统计视图，无需额外建表：

| 扩展 | 实现方式 |
|------|----------|
| Token 消耗折线图 | `SUM(input_tokens + output_tokens)` GROUP BY date |
| 费用趋势 | `SUM(cost)` GROUP BY date |
| Model 分布饼图 | `COUNT(*)` GROUP BY model_id |
| 跨仓库对比 | 去掉 `WHERE git_repo_url = ?`，改为 `GROUP BY git_repo_url` |
| 团队排行榜 | `ORDER BY total_sessions DESC LIMIT N` |

### 数据生命周期

- **保留策略**: 默认保留 180 天数据
- **清理方式**: gocron 定时任务 `DELETE FROM session_usage_reports WHERE date < date('now', '-180 days')`
- 在 `server/internal/scheduler/` 中注册，复用现有 gocron 基础设施
- 清理后执行 `VACUUM` 回收 SQLite 磁盘空间（可选，低频执行如每周一次）

### ES 迁移路径

当后续 ES 统计指标服务就绪后，迁移方案如下：

| 阶段 | 操作 | 说明 |
|------|------|------|
| **历史数据导入** | `SELECT * FROM session_usage_reports` → ES Bulk API | SQLite 全表扫描 + 批量写入 ES，一次性迁移 |
| **双写过渡** | UsageService 同时写 SQLite + ES | 新上报数据双写，验证 ES 数据一致性 |
| **切读** | Activity 查询切换至 ES | 确认 ES 聚合结果与 SQLite 一致后切换 |
| **下线 SQLite** | 删除 SQLite 写入逻辑 + 归档 db 文件 | 完全切换至 ES |

> **迁移友好性**：SQLite 的全量 `SELECT *` 输出可直接映射为 ES document，
> 字段结构与 ES 的 flat JSON document 模型天然匹配，无需复杂的 ETL 转换。

### 安全与权限

| 场景 | 策略 |
|------|------|
| 上报权限 | RequireAuth — 任何已登录用户可上报自己的数据 |
| 查询权限 | RequireAuth — 已登录用户可查询（后续可加仓库级 RBAC） |
| 数据真实性 | `userId` 来自 JWT 服务端提取，客户端无法伪造 |
| 限流 | 建议对 `/api/usage/report` 加 rate limit（如 10 req/min per user） |

---

## 实施计划

| 阶段 | 任务 | 涉及仓库 | 预计工时 |
|------|------|----------|----------|
| Phase 1 | SQLite 连接管理 (`usage_db.go`) + GORM model + AutoMigrate + UPSERT service | costrict-web | 0.5d |
| Phase 2 | Report handler + Activity handler + 路由注册 + Swagger 注释 | costrict-web | 1d |
| Phase 3 | UsageReporter 模块 + CoStrict auth 复用 + Bus 集成 | opencode | 1d |
| Phase 4 | git_repo_url normalize + 内存队列 + 重试逻辑 | opencode | 0.5d |
| Phase 5 | E2E 联调测试 | both | 0.5d |
| **总计** | | | **~3.5d** |

---

## 技术选型总结

| 方面 | 选择 | 理由 |
|------|------|------|
| 上报协议 | HTTPS REST (JSON) | 与 learning/pusher 模式一致 |
| 认证 | CoStrict OAuth (Bearer JWT) | 复用 `createAuthenticatedFetch()` |
| 存储 | 独立 SQLite (`data/usage_stats.db`) | 不侵入业务 PG；项目已有 SQLite 驱动；迁移 ES 路径最短 |
| ORM | GORM (SQLite dialect) | 复用项目 GORM 生态，AutoMigrate 自动建表/索引 |
| 幂等 | UPSERT on `(user_id, session_id, model_id)` | 防止重复上报导致数据膨胀 |
| 并发控制 | WAL 模式 + 单写连接 + 事务批量写 | SQLite 最佳实践，读写不互斥 |
| 活跃度指标 | `COUNT(DISTINCT session_id)` per day | "一个 session = 一次对话"，直觉且准确 |
| 触发机制 | Event-driven + debounce + periodic flush | 平衡实时性与网络开销 |
| 客户端队列 | 内存队列 + 有限重试 | 简单可靠，无需额外依赖 |
| 后续演进 | SQLite → ES 统计指标服务 | 过渡方案，数据结构与 ES document 天然匹配 |
