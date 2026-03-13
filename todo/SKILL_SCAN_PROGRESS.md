# 能力项安全扫描实施进度

参考方案：`docs/SKILL_SCAN_DESIGN.md`

## 总体进度

- **P0 阶段**（后端核心能力）：✅ 完成
- **P1 阶段**（前端展示）：✅ 完成

---

## P0 任务清单（后端，优先实现）

| # | 任务 | 状态 | 说明 |
|---|------|------|------|
| 1 | 数据模型：`capability_items` 扫描字段（`security_status` / `last_scan_id`） | ✅ 完成 | 已有字段复用，`SecurityScan` 模型扩展（`ScanModel` / `DurationMs` / `Summary` / `Recommendations` / `RawOutput`） |
| 2 | 数据模型：`SecurityScan` 扩展为完整扫描结果表 | ✅ 完成 | `internal/models/models.go` |
| 3 | 数据模型：新建 `ScanJob` 表 | ✅ 完成 | `internal/models/models.go`，AutoMigrate 已注册（api + worker 两个入口） |
| 4 | `ScanJobService` 实现 | ✅ 完成 | `internal/services/scan_job_service.go`，Enqueue / Cancel / 去重检查 |
| 5 | LLM 调用复用现有 `llm.Client` | ✅ 完成 | 直接复用 `internal/llm/client.go` 的 `ChatSimple`，无需新增封装 |
| 6 | System Prompt 定义 | ✅ 完成 | 内嵌于 `internal/services/scan_service.go`，继承 `SCAN_SKILL.md` 红线行为和风险分级 |
| 7 | `ScanService.ScanItem()` 实现 | ✅ 完成 | `internal/services/scan_service.go`，Prompt 构造 + LLM 调用 + JSON 解析 + 失败重试 + 结果写入 |
| 8 | `ScanWorkerPool` 实现 | ✅ 完成 | `internal/worker/scan_worker.go`，SKIP LOCKED 拉取 + 执行 + 指数退避重试，集成到 `cmd/worker/main.go` |
| 9 | 触发点接入：CreateItem / CreateItemDirect / UpdateItem | ✅ 完成 | `internal/handlers/capability_item.go`，写入后异步 goroutine 入队 |
| 10 | 触发点接入：SyncService | ✅ 完成 | `SyncService.enqueueScan()` 在 create/update 路径各调用一次 |
| 11 | 扫描 API Handler | ✅ 完成 | `internal/handlers/scan.go`，5 个接口；路由注册于 `cmd/api/main.go` |
| 12 | 环境变量配置 | ✅ 完成 | `SCAN_ENABLED` / `SCAN_LLM_MODEL` / `SCAN_LLM_TIMEOUT_SECONDS` / `SCAN_LLM_MAX_INPUT_TOKENS` / `SCAN_WORKER_CONCURRENCY`，`.env.example` 已补充 |

---

## P1 任务清单（前端展示，暂缓）

| # | 任务 | 状态 | 说明 |
|---|------|------|------|
| 13 | 能力项列表：扫描状态徽章 | ✅ 完成 | `components/scan-badge.tsx`，卡片右上角图标徽章（仅非 unscanned 时展示） |
| 14 | 能力项详情：安全扫描区块 | ✅ 完成 | `components/scan-section.tsx`，含状态/摘要/红线/权限/建议/历史折叠，自动轮询 scanning 状态 |
| 15 | 安装确认弹窗 | ✅ 完成 | `components/scan-install-guard.tsx`，caution → 警告确认；reject → 仅 Org Admin 可继续 |

---

## 文件变更记录

### 新增文件
- `server/internal/services/scan_job_service.go` — 扫描任务入队/查询/取消
- `server/internal/services/scan_service.go` — 扫描核心逻辑（Prompt + LLM + 解析 + 写入）
- `server/internal/worker/scan_worker.go` — 扫描 Worker Pool
- `web/web-ui/components/scan-badge.tsx` — 扫描状态徽章组件
- `web/web-ui/components/scan-section.tsx` — 详情页安全扫描区块（含轮询/历史/权限展示）
- `web/web-ui/components/scan-install-guard.tsx` — 安装确认弹窗（caution/reject 拦截）

### 修改文件
- `server/internal/models/models.go` — 扩展 `SecurityScan`，新增 `ScanJob` 模型，AutoMigrate 注册
- `server/internal/handlers/scan.go` — 新增扫描相关 API Handler（5 个接口）
- `server/internal/handlers/sync.go` — 新增 `ScanJobService` 包级变量
- `server/internal/handlers/capability_item.go` — CreateItem / CreateItemDirect / UpdateItem 写入后异步入队
- `server/internal/services/sync_service.go` — `SyncService` 新增 `ScanJobService` 字段和 `enqueueScan()` 方法
- `server/cmd/api/main.go` — AutoMigrate 注册 `ScanJob`，初始化 `ScanJobService`，注册 5 条扫描路由
- `server/cmd/worker/main.go` — AutoMigrate 注册 `SecurityScan`/`ScanJob`，启动 `ScanWorkerPool`
- `server/.env.example` — 补充扫描相关环境变量
- `web/web-ui/lib/api-client.ts` — 新增 `ScanStatus`/`SecurityScan`/`ScanStatusResponse` 类型，新增 `scanApi`，`CapabilityItem` 补充 `securityStatus`/`lastScanId`，`CapabilityRegistry` 补充 `orgId`
- `web/web-ui/components/capability-item-card.tsx` — 卡片右上角集成 `ScanBadge`
- `web/web-ui/app/items/[id]/page.tsx` — 详情页集成 `ScanSection` + `ScanInstallGuard`

---

## 关键设计决策

1. **独立队列表**：`capability_scan_jobs` 独立于 `sync_jobs`，避免扫描任务阻塞同步队列
2. **同 Worker 进程**：`ScanWorkerPool` 与 `WorkerPool` 并列运行在同一 `cmd/worker` 进程，共享 DB 连接
3. **去重策略**：同一 `item_id` 不允许同时存在多条 `pending/running` 任务；`create/update/sync` 触发时静默跳过，`manual` 触发时返回冲突提示
4. **低重试次数**：`max_attempts=2`，LLM 调用失败重试1次后终态 `failed`，避免无效消耗
5. **总开关**：`SCAN_ENABLED=false` 时所有 `Enqueue` 调用直接返回，适配无 LLM 的私有化部署
6. **非阻塞触发**：所有触发点均在主流程写入完成后异步入队，不影响 API 响应时间
