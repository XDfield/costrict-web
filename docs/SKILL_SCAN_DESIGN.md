# 能力项安全扫描技术方案

## 一、背景与目标

### 1.1 背景

技能中心（Skill Center）允许用户创建、同步、分享各类能力项（`capability_items`），包括 Skill、Agent、Command、MCP 等。这些内容最终会被 AI Agent（如 Claude Code）加载执行，其内容质量和安全性直接影响用户系统安全。

参考 `SCAN_SKILL.md` 中描述的人工审查协议，当前平台缺少对能力项内容的自动化安全审查机制，存在以下风险：

- 恶意 Skill 通过公网同步进入平台（`source_type=external`）
- 内部用户创建包含危险指令的 Skill（如窃取凭证、外传数据）
- 外部同步的 MCP Server 命令存在供应链风险

### 1.2 目标

在能力项**创建或同步写入**时，异步触发 AI Agent 驱动的安全扫描，对内容进行风险分级标注，并在前端展示扫描结果，辅助用户和管理员决策。

**核心约束**：

- 扫描**异步执行**，不阻塞能力项的创建/同步写入流程
- 扫描结果作为**参考标注**，不自动拦截或删除内容（第一版）
- 扫描逻辑由 **AI Agent（LLM）** 驱动，而非规则引擎

---

## 二、整体架构

```
能力项创建 / 同步写入
        ↓
  capability_items 写入完成
        ↓ 异步触发（不阻塞响应）
  ScanJob 入队（PostgreSQL）
        ↓  SELECT FOR UPDATE SKIP LOCKED
  ScanWorker Pool
        ↓
  AI Agent（LLM）执行扫描
  ├─ 分析 content 字段（Markdown + frontmatter）
  ├─ 分析 metadata 字段（命令、URL、环境变量等）
  └─ 输出结构化扫描报告
        ↓
  写入 capability_scan_results 表
  更新 capability_items.scan_status
        ↓
  前端展示扫描徽章 / 详情
```

复用 `SKILL_SYNC_DESIGN.md` 中已有的 **PostgreSQL 队列 + Worker Pool** 模式（`SELECT FOR UPDATE SKIP LOCKED`），扫描任务与同步任务共享同一套基础设施，但队列表独立。

---

## 三、数据模型

### 3.1 扩展 `capability_items` 表

新增扫描状态字段，不修改现有字段：

```sql
ALTER TABLE capability_items ADD COLUMN scan_status VARCHAR(32) DEFAULT 'pending';
-- pending   : 等待扫描（刚创建/刚更新）
-- scanning  : 扫描中
-- clean     : 无风险
-- low       : 低风险
-- medium    : 中风险
-- high      : 高风险
-- extreme   : 极高风险（对应 SCAN_SKILL.md 中的 EXTREME）
-- error     : 扫描失败
-- skipped   : 跳过（内容过短或无法解析）

ALTER TABLE capability_items ADD COLUMN last_scanned_at TIMESTAMP;
ALTER TABLE capability_items ADD COLUMN scan_result_id UUID;
```

### 3.2 新增 `capability_scan_results` 表

```sql
CREATE TABLE capability_scan_results (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  item_id         UUID NOT NULL REFERENCES capability_items(id) ON DELETE CASCADE,
  item_version    INTEGER NOT NULL,           -- 对应 capability_versions.version，确保结果与版本绑定

  -- 扫描元信息
  scan_model      VARCHAR(128) NOT NULL,      -- 执行扫描的 LLM 模型标识，如 claude-3-5-sonnet
  scan_duration_ms INTEGER,
  triggered_by    VARCHAR(32) NOT NULL,       -- 'create' | 'update' | 'sync' | 'manual'

  -- 风险评级
  risk_level      VARCHAR(32) NOT NULL,       -- clean | low | medium | high | extreme
  verdict         VARCHAR(32) NOT NULL,       -- safe | caution | reject
  -- safe    : 可正常展示和安装
  -- caution : 展示警告，用户需确认后安装
  -- reject  : 建议管理员介入（不自动删除）

  -- 结构化扫描报告（AI Agent 输出的 JSON）
  report          JSONB NOT NULL DEFAULT '{}',
  -- {
  --   "red_flags": ["curl to unknown URL in step 3", ...],
  --   "permissions": {
  --     "files": ["~/.ssh", "~/.aws"],
  --     "network": ["api.example.com"],
  --     "commands": ["curl", "base64"]
  --   },
  --   "summary": "该 Skill 在步骤3尝试向外部服务器发送数据...",
  --   "recommendations": ["移除第3步的 curl 调用", ...]
  -- }

  -- 原始 AI 输出（调试用）
  raw_output      TEXT,

  -- 审计
  created_at      TIMESTAMP DEFAULT NOW()
);

CREATE INDEX idx_scan_results_item_id ON capability_scan_results(item_id);
CREATE INDEX idx_scan_results_risk_level ON capability_scan_results(risk_level);
```

### 3.3 新增 `capability_scan_jobs` 表（扫描任务队列）

独立于同步任务队列（`sync_jobs`），避免相互干扰：

```sql
CREATE TABLE capability_scan_jobs (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  item_id       UUID NOT NULL REFERENCES capability_items(id) ON DELETE CASCADE,
  item_version  INTEGER NOT NULL,

  trigger_type  VARCHAR(32) NOT NULL,   -- create | update | sync | manual
  trigger_user  VARCHAR(191),           -- 手动触发时的用户 ID

  status        VARCHAR(32) NOT NULL DEFAULT 'pending',
  -- pending | running | success | failed | cancelled

  priority      INTEGER NOT NULL DEFAULT 5,
  -- 1=高（manual），5=普通（create/update/sync）

  retry_count   INTEGER DEFAULT 0,
  max_attempts  INTEGER DEFAULT 2,      -- 扫描失败最多重试1次，避免无效消耗
  last_error    TEXT,

  scheduled_at  TIMESTAMP NOT NULL DEFAULT NOW(),
  started_at    TIMESTAMP,
  finished_at   TIMESTAMP,
  scan_result_id UUID,                  -- 完成后关联 capability_scan_results.id

  created_at    TIMESTAMP DEFAULT NOW()
);

CREATE INDEX idx_scan_jobs_status ON capability_scan_jobs(status, scheduled_at);
CREATE INDEX idx_scan_jobs_item_id ON capability_scan_jobs(item_id);
```

**去重约束**：同一 `item_id` 不允许同时存在多条 `pending` 或 `running` 任务。后续触发时，若已有活跃任务，则静默跳过（`create/update/sync` 触发）或返回冲突提示（`manual` 触发）。

---

## 四、触发时机

### 4.1 内部创建（`source_type=internal`）

```
POST /api/registries/:id/items   →  capability_items 写入完成
                                          ↓
                                  ScanJobService.Enqueue(itemID, "create")
                                  （异步，不影响 201 响应）
```

### 4.2 内容更新

```
PUT /api/items/:id   →  capability_items 更新完成，capability_versions 新增记录
                                ↓
                        ScanJobService.Enqueue(itemID, "update")
                        （仅当 content 或 metadata 字段发生变化时入队）
```

### 4.3 同步写入（`source_type=external`）

在 `SyncService.SyncRegistry()` 的增量对比循环中，每次 `Create` 或 `Save` 一条 `SkillItem` 后，追加入队：

```
SyncService 写入 SkillItem（新增或更新）
        ↓
ScanJobService.Enqueue(itemID, "sync")
```

批量同步场景下，多条入队操作在同一事务外异步执行，不影响同步事务的原子性。

### 4.4 手动触发

管理员或能力项 Owner 可在前端手动触发重新扫描：

```
POST /api/items/:id/scan   →  ScanJobService.Enqueue(itemID, "manual", userID, priority=1)
```

---

## 五、AI Agent 扫描设计

### 5.1 扫描 Agent 定位

扫描 Agent 是一个**无状态的单次调用 LLM**，每次扫描独立执行，不维护上下文。输入为能力项内容，输出为结构化 JSON 报告。

不引入额外的 Agent 框架，直接调用 LLM API（与平台现有 LLM 调用方式保持一致）。

### 5.2 输入构造

```
扫描输入 = 能力项元信息 + 完整内容

元信息：
  - item_type（skill / agent / command / mcp）
  - name / description
  - source_type（internal / external）
  - metadata（hosting_type / command / args / url / environment_variables 等）

内容：
  - content 字段（完整 Markdown + frontmatter）
  - 若 item_type=mcp，额外附加 metadata 中的命令和参数
```

### 5.3 System Prompt

```
你是一个专业的 AI 能力项安全审查员。
你需要对用户提交的 AI Agent Skill / MCP Server 配置进行安全审查。

## 审查维度

### 红线行为（出现任意一条 → risk_level=extreme）
- 向未知外部 URL 发送数据（curl/wget/fetch 到非文档域名）
- 读取凭证文件（~/.ssh、~/.aws、~/.config、.env 等）
- 请求用户提供 API Key / Token / 密码并外传
- 使用 base64/eval/exec 处理外部输入
- 修改系统文件（/etc、/usr、注册表等）
- 访问浏览器 Cookie 或会话数据
- 使用混淆/压缩/编码的代码片段

### 高风险行为（risk_level=high）
- 网络请求到 IP 地址而非域名
- 安装未列明的软件包
- 请求 sudo / 管理员权限
- 读取工作区外的文件系统路径

### 中风险行为（risk_level=medium）
- 需要网络访问但目标域名可信
- 需要读写本地文件（工作区内）
- 使用环境变量传递敏感配置（本身合理，需确认是否外传）

### 低风险行为（risk_level=low）
- 纯文本处理、格式化、注释生成
- 访问公开 API（天气、汇率等）
- 本地计算，无网络无文件操作

## 输出格式

严格输出以下 JSON，不要添加任何额外文字：

{
  "risk_level": "clean | low | medium | high | extreme",
  "verdict": "safe | caution | reject",
  "red_flags": ["具体描述发现的红线行为，引用原文", ...],
  "permissions": {
    "files": ["列出需要访问的文件路径"],
    "network": ["列出需要访问的域名或 IP"],
    "commands": ["列出执行的系统命令"]
  },
  "summary": "100字以内的风险摘要，中文",
  "recommendations": ["具体的修改建议", ...]
}

verdict 规则：
- risk_level=clean/low → verdict=safe
- risk_level=medium    → verdict=caution
- risk_level=high/extreme → verdict=reject
```

### 5.4 输出解析与容错

```
AI 输出
  ↓
尝试 JSON 解析
  ├─ 成功 → 校验字段完整性 → 写入 capability_scan_results
  └─ 失败（非 JSON / 格式错误）
       ↓
     重试一次（附加提示："请只输出 JSON，不要有其他内容"）
       ├─ 成功 → 写入结果
       └─ 失败 → scan_status=error，记录 raw_output 供调试
```

---

## 六、服务层设计

### 6.1 目录结构扩展

在 `SKILL_SYNC_DESIGN.md` 已有目录结构基础上新增：

```
server/
├── internal/
│   ├── services/
│   │   ├── scan_service.go      # 扫描核心逻辑（Worker 调用）
│   │   ├── scan_job_service.go  # 扫描任务入队 / 查询 / 取消
│   │   └── llm_client.go        # LLM API 调用封装（复用或新增）
│   ├── worker/
│   │   ├── worker.go            # 现有同步 Worker（不变）
│   │   └── scan_worker.go       # 扫描 Worker Pool（新增）
│   └── handlers/
│       └── scan.go              # 扫描相关 API Handler（新增）
```

### 6.2 ScanJobService

```go
type ScanJobService struct {
    DB *gorm.DB
}

// Enqueue 入队扫描任务
// 同一 item_id 已有 pending/running 任务时：
//   - triggerType=create/update/sync → 静默跳过
//   - triggerType=manual → 返回 ErrJobAlreadyQueued
func (s *ScanJobService) Enqueue(itemID string, itemVersion int, triggerType, triggerUser string, priority int) (*models.ScanJob, error)

// Cancel 取消 pending 状态的任务
func (s *ScanJobService) Cancel(jobID, operatorID string) error
```

### 6.3 ScanService（核心）

```go
type ScanService struct {
    DB        *gorm.DB
    LLMClient *LLMClient
}

type ScanResult struct {
    RiskLevel       string
    Verdict         string
    RedFlags        []string
    Permissions     ScanPermissions
    Summary         string
    Recommendations []string
    RawOutput       string
    DurationMs      int64
}

// ScanItem 对单个能力项执行一次完整扫描，由 ScanWorker 调用
func (s *ScanService) ScanItem(ctx context.Context, itemID string, itemVersion int, triggerType string) (*ScanResult, error)
```

**ScanItem 执行步骤**

```
1. 查询 capability_items（content + metadata + item_type）
2. 构造扫描输入（元信息 + 完整内容）
3. 更新 capability_items.scan_status = "scanning"
4. 调用 LLMClient.Complete(systemPrompt, userInput)
5. 解析 JSON 输出（失败则重试一次）
6. 写入 capability_scan_results
7. 更新 capability_items.scan_status = risk_level
8. 更新 capability_items.last_scanned_at / scan_result_id
```

### 6.4 ScanWorker Pool

与 `SKILL_SYNC_DESIGN.md` 中 `WorkerPool` 结构完全对称，消费 `capability_scan_jobs` 队列：

```go
type ScanWorkerPool struct {
    DB           *gorm.DB
    ScanService  *ScanService
    Concurrency  int           // 默认 2（LLM 调用有并发限制）
    PollInterval time.Duration // 默认 3s
    stopCh       chan struct{}
}

func (p *ScanWorkerPool) Start()
func (p *ScanWorkerPool) Stop()
```

**拉取 SQL**（与同步 Worker 相同模式）

```sql
SELECT * FROM capability_scan_jobs
WHERE status = 'pending'
  AND scheduled_at <= NOW()
ORDER BY priority ASC, scheduled_at ASC
LIMIT 1
FOR UPDATE SKIP LOCKED
```

**失败重试退避**

| 第 N 次重试 | 等待时间 |
|------------|---------|
| 第 1 次 | 2 分钟 |
| 超过 max_attempts | 终态 failed，scan_status=error |

扫描失败重试次数（`max_attempts=2`）低于同步任务，原因是 LLM 调用失败通常是模型或配额问题，多次重试意义有限。

---

## 七、API 设计

```
POST   /api/items/:id/scan              # 手动触发扫描
GET    /api/items/:id/scan-status       # 查询当前扫描状态和最新结果摘要
GET    /api/items/:id/scan-results      # 查询历史扫描结果列表（分页）
GET    /api/scan-results/:id            # 查询单条扫描结果详情
POST   /api/scan-jobs/:id/cancel        # 取消 pending 的扫描任务
GET    /api/admin/scan-jobs             # 管理员查看全局扫描队列状态
```

### 7.1 接口说明

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/api/items/:id/scan` | 手动触发重新扫描，返回 `{ jobId, status }` |
| `GET` | `/api/items/:id/scan-status` | 返回 `scan_status`、`last_scanned_at`、最新结果摘要（risk_level + verdict + summary） |
| `GET` | `/api/items/:id/scan-results` | 历史扫描记录，含每次的 risk_level / verdict / triggered_by / created_at |
| `GET` | `/api/scan-results/:id` | 完整报告，含 red_flags / permissions / recommendations |

### 7.2 scan-status 响应示例

```json
{
  "scan_status": "high",
  "last_scanned_at": "2026-03-13T10:00:00Z",
  "latest_result": {
    "id": "uuid-xxx",
    "risk_level": "high",
    "verdict": "reject",
    "summary": "该 Skill 在步骤3尝试通过 curl 向外部服务器发送工作区文件内容。",
    "red_flags_count": 2,
    "scan_model": "claude-3-5-sonnet"
  }
}
```

---

## 八、前端展示

### 8.1 能力项列表页

在每个能力项卡片上展示扫描徽章：

```
[能力项名称]                           [🟢 安全] / [🟡 注意] / [🔴 高风险] / [扫描中...] / [待扫描]
```

徽章颜色映射：

| scan_status | 显示文案 | 颜色 |
|-------------|---------|------|
| pending / scanning | 扫描中... | 灰色（动画） |
| clean / low | 安全 | 绿色 |
| medium | 注意 | 黄色 |
| high | 高风险 | 橙色 |
| extreme | 极高风险 | 红色 |
| error | 扫描失败 | 灰色 |
| skipped | 已跳过 | 灰色 |

### 8.2 能力项详情页

在详情页新增"安全扫描"区块：

```
安全扫描                                              [重新扫描]
─────────────────────────────────────────────────────
风险等级：🔴 高风险          扫描时间：2026-03-13 10:00
建议：⚠️ 建议管理员介入后再安装

发现的问题（2项）：
  • 步骤3中包含 curl 命令，向 api.unknown.com 发送数据
  • 读取了 ~/.aws/credentials 文件

权限需求：
  文件：~/.aws/credentials
  网络：api.unknown.com
  命令：curl, base64

修改建议：
  1. 移除步骤3的 curl 调用
  2. 不应在 Skill 中读取凭证文件

[查看历史扫描记录]
```

### 8.3 安装确认弹窗

当用户点击"安装"一个 `verdict=caution` 或 `verdict=reject` 的能力项时，弹出确认框：

```
⚠️ 安全警告

该能力项的扫描结果为「中风险」，请确认后再安装：

  该 Skill 需要访问本地文件系统（工作区内），
  并向 weather-api.com 发起网络请求。

[取消]                              [我已了解风险，继续安装]
```

`verdict=reject` 时，"继续安装"按钮仅对 Org Admin 可见，普通用户只能取消。

---

## 九、LLM 配置

### 9.1 模型选择

扫描 Agent 使用平台已配置的 LLM 服务，通过环境变量指定：

```bash
SCAN_LLM_MODEL=claude-3-5-sonnet-20241022   # 扫描使用的模型
SCAN_LLM_TIMEOUT_SECONDS=30                  # 单次调用超时
SCAN_LLM_MAX_INPUT_TOKENS=8000               # 输入截断上限（超出则截断 content 末尾）
SCAN_ENABLED=true                            # 总开关，false 时所有入队操作跳过
```

### 9.2 成本控制

- `SCAN_ENABLED=false` 可全局关闭扫描（私有化部署无 LLM 的场景）
- 内容超过 `SCAN_LLM_MAX_INPUT_TOKENS` 时，截断 `content` 末尾部分，保留 frontmatter 和前 N 行，并在 report 中注明"内容已截断，扫描结果仅供参考"
- `max_attempts=2` 限制重试次数，避免 LLM 异常时无限消耗

---

## 十、数据流全景图

```
┌──────────────────────────────────────────────────────────────┐
│  触发层                                                        │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐        │
│  │  创建/更新    │  │  同步写入     │  │  手动触发     │        │
│  │  API Handler │  │  SyncService │  │  API Handler │        │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘        │
│         └─────────────────┴─────────────────┘                 │
│                   ScanJobService.Enqueue()                     │
└───────────────────────────┬──────────────────────────────────┘
                            ↓  写入
        ┌───────────────────────────────────────┐
        │  capability_scan_jobs（PostgreSQL）    │
        │  pending → running → success / failed  │
        └───────────────────────────────────────┘
                            ↑  SELECT FOR UPDATE SKIP LOCKED
                            │  每 3 秒轮询，priority ASC
┌───────────────────────────┴──────────────────────────────────┐
│  ScanWorker Pool（cmd/worker 进程内，与 SyncWorker 并列）       │
│                                                               │
│  ScanWorker-1       ScanWorker-2                              │
│       ↓                   ↓                                   │
│  ① 竞争拉取任务（SKIP LOCKED），status → running              │
│  ② ScanService.ScanItem()                                     │
│     ├─ 查询 capability_items（content + metadata）            │
│     ├─ 构造 Prompt（System + User）                           │
│     ├─ 调用 LLM API（claude-3-5-sonnet）                      │
│     ├─ 解析 JSON 输出（失败重试一次）                          │
│     ├─ 写入 capability_scan_results                           │
│     └─ 更新 capability_items.scan_status / last_scanned_at   │
│  ③ 更新 ScanJob（success/failed，关联 scan_result_id）        │
└──────────────────────────────────────────────────────────────┘
                            ↓
        ┌───────────────────────────────────────┐
        │  capability_scan_results              │
        │  capability_items.scan_status 更新    │
        └───────────────────────────────────────┘
                            ↓
        前端轮询 /api/items/:id/scan-status
        展示扫描徽章 / 详情 / 安装确认弹窗
```

---

## 十一、与现有设计的关系

| 现有设计 | 本方案复用点 |
|---------|------------|
| `SKILL_SYNC_DESIGN.md` Worker Pool | 复用 `SELECT FOR UPDATE SKIP LOCKED` 队列消费模式，`ScanWorkerPool` 与 `WorkerPool` 并列运行在同一 `cmd/worker` 进程中 |
| `SKILL_SYNC_DESIGN.md` SyncService | `SyncService.SyncRegistry()` 在写入 `SkillItem` 后调用 `ScanJobService.Enqueue()`，无侵入式扩展 |
| `SKILL_DATA_DESIGN.md` capability_items | 仅新增 `scan_status` / `last_scanned_at` / `scan_result_id` 三个字段，不修改现有字段 |
| `SCAN_SKILL.md` 审查协议 | System Prompt 完整继承其红线行为、风险分级、输出格式定义 |

---

## 十二、实施路线图

| 阶段 | 内容 | 预估时间 |
|------|------|---------|
| **P0** | 数据模型：`capability_items` 新增扫描字段，新建 `capability_scan_results` / `capability_scan_jobs` 表 | 0.5天 |
| **P0** | `ScanJobService`：Enqueue / Cancel / 去重检查 | 0.5天 |
| **P0** | `LLMClient` 封装（复用现有或新增），System Prompt 定义 | 1天 |
| **P0** | `ScanService.ScanItem()`：Prompt 构造 + LLM 调用 + JSON 解析 + 结果写入 | 1.5天 |
| **P0** | `ScanWorkerPool`：SKIP LOCKED 拉取 + 执行 + 退避重试，集成到 `cmd/worker` | 1天 |
| **P0** | 触发点接入：`CreateItem` / `UpdateItem` API Handler 写入后入队 | 0.5天 |
| **P0** | 触发点接入：`SyncService` 写入 SkillItem 后入队 | 0.5天 |
| **P0** | 扫描 API：`POST /scan` / `GET /scan-status` / `GET /scan-results/:id` | 0.5天 |
| **P1** | 前端：能力项列表扫描徽章 | 0.5天 |
| **P1** | 前端：能力项详情页安全扫描区块 | 1天 |
| **P1** | 前端：安装确认弹窗（caution / reject 场景） | 0.5天 |
| **P2** | 管理员视图：全局扫描队列状态 / 高风险条目汇总 | 1天 |
| **P2** | 自动归档策略：`extreme` 风险条目自动设为 `draft` 并通知管理员 | 1天 |

**P0 阶段（约 6 天）** 即可上线后台扫描能力，前端暂不展示；**P1 阶段（约 2 天）** 完成用户可见的扫描结果展示。
