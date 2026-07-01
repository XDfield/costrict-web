# Admin Bundle Import（管理后台 catalog 包导入）功能设计文档

**Status**: Implemented
**Author**: Claude
**Date**: 2026-07-01

---

## 1. 背景与目标

### 1.1 背景

catalog 数据落地 `capability_items` 的既有链路是纯运维手动操作（见 [`CATALOG_INGEST.md`](../CATALOG_INGEST.md)）：SSH 到节点 → `kubectl cp` 把 `catalog-bundle.tar.gz` 拷进 api pod → 在 pod 里跑 `migrate ingest-upstream --source=...`，靠 `scripts/run-ingest.sh` 编排 dry-run → 人工确认 → 真实导入。

这个流程只有能 SSH + kubectl 的人能做，且没有可视化的预览/确认/历史，`failed>0` 的硬性关卡靠脚本约束。

### 1.2 目标

把这条手动流程搬进**管理后台 UI**，让平台管理员在浏览器里完成 catalog 包导入：

1. 仅平台管理员可用（复用现有 `/admin` 路由组 + `RequirePlatformAdmin`）。
2. 通过 UI 上传/指定 bundle（选文件或拖拽，或粘贴 URL）。
3. 按类型查看当前存量。
4. 在内容管理页筛选"缺少安全评估/评分"的项并导出。
5. **不破坏用户自己上传的内容**。

### 1.3 非目标

- 不改上游 bundle 构造流程（`costrict-skills-repo` 侧不变）。
- 不改用户侧 `/plugins/upload` 单文件上传路径。
- 不改上游 `ai-resource-eval` 评分计算（本仓库只摄入结果）。
- 不做导入任务的分布式锁/多副本强一致（应用层 + leader election 已足够，见 §4）。

### 1.4 核心原则：复用而非重造

导入执行**直接复用** `services.CatalogIngestService.Ingest()`——就是 `migrate ingest-upstream` 背后调用的同一个 service。这带来两个天然收益：

- **不破坏用户上传**：`CatalogIngestService` 已有的 slug 冲突重试（`-2..-10` 后缀，不劫持已存在的行）+ `CatalogEntryDir` 匹配（只更新 catalog 摄入的行，用户上传行该字段永远为空）机制直接生效，无需新写防覆盖逻辑。
- **评分/健康度/安全扫描**摄入行为与 CLI 路径完全一致。

---

## 2. 术语

| 术语 | 含义 |
|---|---|
| bundle | 上游 `everything-ai-coding` 产出的 `catalog-bundle.tar.gz`；解压后根目录 = `manifest.json` + `index.json`（条目清单）+ `catalog-download/`（各条目文件），一次含 skill/mcp/plugin/rule 等上千条 |
| dry-run | 只算 diff、不写库的预览阶段（`IngestOptions.DryRun=true`） |
| 来源 A / URL | 粘贴可重取的 bundle URL（首选） |
| 来源 B / upload | 浏览器上传 bundle 文件（备选） |
| import runner | api 进程内、leader 选举出的单实例后台执行器（§4） |

---

## 3. 数据模型

### 3.1 `capability_import_jobs`（导入任务表 + 历史/审计）

`server/internal/models/capability_import_job.go` + goose 迁移 `migrations/20260630000000_create_capability_import_jobs.sql`。字段仿 `SyncJob`/`ScanJob` 的队列习惯：

| 列 | 说明 |
|---|---|
| `id` | uuid |
| `source_kind` | `url` \| `upload` |
| `source_url` | 来源 A 时填：可重取 URL |
| `filename` | 展示用（上传原文件名 / URL 末段） |
| `storage_key` | 来源 B 时填：上传的 tar.gz 在 StorageBackend 里的 key |
| `status` | `pending` \| `running` \| `previewed` \| `success` \| `failed` \| `expired` |
| `dry_run` | true=当前待执行/执行中的是 dry-run 预览；false=真实导入 |
| `reparse` | 是否强制重新解析所有条目（透传 `IngestOptions.Reparse`） |
| `trigger_user` | 操作导入的平台管理员 subject_id（审计追溯用，**不落到 item 归属**，见 §6） |
| `result` | 序列化的 `ImportResult`（camelCase 投影，含 added/updated/…/manifestSha256/generatedAt/errors/incompleteErrors） |
| `error_message`, `retry_count`, `max_attempts`, `scheduled_at`, `started_at`, `finished_at` | 队列/重试/终态字段 |

> 这张表本身就是结构化的导入历史；前端"最近导入记录"直接读它。真实导入的成功/失败还额外调 `audit.Record(triggerUser, "item.bundle_import", "capability_import_job", jobID, result)`，让操作出现在 M5 运营页的审计视图。

---

## 4. 执行模型（方案 D：DB 队列 + leader-elected poller + reaper）

### 4.1 为什么不是"请求内 goroutine"

导入 3000+ 条目要几十秒到几分钟，必须异步。最初考虑请求内 goroutine，但对抗性评审指出它有真实生产缺陷：pod 重启/滚动发布会让任务永久卡死；两个管理员并发确认有竞态；且丢掉了仓库里 `ScanJob`/`SyncJob` 那套"DB 队列 + worker 领取 + retry + 终态"的可靠性内核。

### 4.2 为什么不是"复用现有独立 worker 进程"

现有 `worker` 进程（`cmd/worker`）**没有挂载 artifacts PVC、没有初始化 StorageBackend**（`deploy/charts/worker/` 无 volume），因此读不到来源 B 上传的 bundle 文件。而 api 进程有该 PVC（现有 artifact 上传就靠它）。

### 4.3 采用方案 D

- `POST` 只负责"保存来源（URL 字符串或上传到 StorageBackend）+ 写 `pending` job"，**不在请求内执行**。
- 复用 api 进程现有的 `leader.NewElection`（`cmd/api/main.go`，已用于 `costrict-scheduler`）新增 `costrict-import-runner` 选举。**只有 leader 副本**跑 import poller，`FOR UPDATE SKIP LOCKED` 串行领取 `pending` job（仿 `internal/worker/worker.go`），跑 `Ingest` 后 finalize（复刻 worker 的 retry/终态）。
- **单 leader 串行执行**天然堵死并发竞态：两个管理员同时确认，只是把两个 job 排进队列，poller 逐个执行，绝不并发写库。
- poller 在 api 进程 → 能读 artifacts PVC（来源 B）也能下载 URL（来源 A）。

### 4.4 状态机

```
提交(URL/上传) ──► pending(dryRun=true)
                      │  leader poller FOR UPDATE SKIP LOCKED 领取
                      ▼
                   running ──► Ingest(DryRun=true) ──► previewed(写 Result)
                                                          │ 管理员确认
                                                          │ (failed==0 且 deleted 占比 gate 通过)
                                                          ▼
                                          pending(dryRun=false) ──► running
                                                          ▼
                                              Ingest(DryRun=false) ──► success / failed
```

- **dry-run 失败**（下载/解压/ingest 崩）→ 可 retry（只读幂等，backoff 仿 worker），超 `MaxAttempts` 转 `failed`。
- **真实导入失败/异常** → `failed`，**不自动 retry**（非幂等，可能已部分写库，需人工核对后重新发起）。涵盖三种：Ingest 报错（下载/解压崩）；`result.Failed>0`（部分条目写库失败——`CatalogIngestService` 此时返回 nil error，但导入并未完全成功，故不能记 success）；URL job 的真实导入 manifest sha 与预览值不一致（内容在预览后变化，绕过了对旧内容评估的 failed/large-delete gate）。三者都标 `failed` + 明确 error_message。
- **reaper**：`running` 且 `started_at` 超阈值（30min）→ dry-run 重排为 `pending`（bump retry_count，超限转 failed）；真实导入标 `failed` 并提示人工核对。解决 leader 崩溃/重启导致的卡死。
- **previewed TTL**（2h）未确认 → 懒惰置 `expired` 并清理 StorageBackend 里的 bundle。
- **guarded CAS**：expire/reaper 清理 bundle 前先 `UPDATE ... WHERE id AND status=<期望>` 且仅当 `RowsAffected>0` 才删 bundle——避免与并发 `ConfirmJob`（已把 previewed 翻成 pending、bundle 仍需要）竞态误删。

### 4.5 并发保护

`Start/Stop` 用 `sync.Mutex` + `running bool` 防 leader flap 期间双启第二个 poller loop；`loop` 接收自己的局部 `stopCh` 以便 `Stop` 安全重置字段；`wg.Wait()` 在锁外，避免死锁。`Start` 建一个可取消的 context 传给 `Ingest`（不再是 `context.Background()`），`Stop` 在失去 leadership 时 `cancel()` 它——让正在跑的下载/导入能被中断而非继续写库；配合 reaper 兜底 leader 崩溃场景。

---

## 5. 两种 bundle 来源 + 两步关卡

### 5.1 来源 A — URL（首选）

管理员粘贴可重取 URL（如 GitHub Release 的 `catalog-bundle.tar.gz`）。dry-run 与 confirm 各自用 `IngestSource{URL}` 让 `materialize` 自行下载，**不落 StorageBackend**。优点：绕开浏览器上传大文件（不受 ingress 体积限制、不怕中断），且天然免除跨副本保留文件的复杂度。

### 5.2 来源 B — 文件上传（备选）

本地现场构建的自定义包无 URL 时使用。上传阶段 multipart 流式写入 `StorageBackend.Put("import-jobs/{jobId}/bundle.tar.gz")`；dry-run/confirm 各自 `Get` 回一份临时文件喂给 `IngestSource{Tarball}`；终态后删除存储对象。跨副本前提：依赖 RWX PVC（与现有 artifact 上传前提一致，非本功能新增假设）。

### 5.3 两步关卡

镜像运维脚本 `run-ingest.sh` 的安全性：**dry-run 预览**（展示 added/updated/deleted/failed/incomplete 计数 + `manifestSha256`/`generatedAt` 供核对包身份）→ `failed>0` 禁止确认 → `deleted` 占当前存量比例超阈值（20%）时需二次勾选确认 → 才 confirm 真实导入。

---

## 6. 归属：catalog 导入项保持公共（system）

导入执行时 `Ingest` 的 `IngestOptions.TriggerUser` 传 `"system"`（常量 `adminimport.catalogImportTriggerUser`），与 CLI `ingest-upstream` 完全一致——catalog 导入的 item `created_by`/`updated_by` 保持 `system`（公共/无归属），**不会**变成操作管理员的"我的上传"。谁触发的导入通过 `capability_import_jobs.trigger_user` + `audit.Record` 追溯，绝不落到 item 归属上。

---

## 7. 内容管理筛选 + CSV 导出

在既有 M6 内容管理（`adminitem`）上扩展：

- **缺少安全评估**：`security_status = 'unscanned'`（精确"从未评估"，**不含** pending/scanning/error/skipped——那些是"扫过/正在扫"，语义不同，因此是独立筛选而非复用 `securityStatusGroups["unknown"]`）。
- **缺少评分**：`experience_score <= 0`（上游 `final_score` 缺失/为 0）。
- 两个筛选与现有 type/status/security 筛选 AND 组合。
- **CSV 导出**：复用同一套筛选构造，不分页流式吐出 `name,slug,item_type,category,source,security_status,experience_score,created_at`。带 UTF-8 BOM（Excel 中文），且 `name/slug/category/source` 经 `csvSafe()` 中和公式注入（CWE-1236：`= + - @ tab CR` 开头前缀单引号）。

---

## 8. API 设计（挂在 `/admin` 组，平台管理员）

| 方法 + 路径 | 作用 |
|---|---|
| `POST /admin/import-jobs` | 二选一：JSON `{sourceUrl, reparse}`（首选）或 multipart 文件（field `file` + form `reparse`）。写 `pending` job，返回 `202 {jobId, status}`；leader poller 异步跑 dry-run |
| `GET /admin/import-jobs/:id` | 轮询状态 + `result`（含 manifestSha256/generatedAt）+ 懒惰过期 |
| `POST /admin/import-jobs/:id/confirm` | 对 `previewed` 且 `failed==0` 的 job，`deleted` 占比超阈值需带 `{confirmLargeDelete:true}`；翻转为 `pending, dryRun=false` |
| `GET /admin/import-jobs/:id/errors.log` | 下载 `errors`+`incompleteErrors` 纯文本 |
| `GET /admin/import-jobs` | 分页历史 |
| `GET /admin/import-stats` | 当前存量按 `item_type` group by |
| `GET /admin/items/export.csv` | 内容管理筛选结果导出 CSV（`adminitem`） |
| `GET /admin/items?missingSecurityEval=&missingScore=` | 内容管理列表新增两筛选参数（`adminitem`） |

---

## 9. 前端设计

`portal/opencode-display-revamp/packages/app-ai-native/src/pages/admin/`。

- **新页面 `pages/import.tsx`** + 菜单项 `admin.import`（`menu-registry.ts`）+ 路由 `/admin/import`：存量统计卡片 + URL/上传两个 tab（共用 reparse 勾选）+ dry-run 预览（计数 + manifest + `failed>0` 禁确认 + `deleted` 过大红色警告二次勾选）+ 确认 + 最近导入记录表。
- **M6 `pages/content.tsx`**：筛选栏加"缺少安全评估/缺少评分"两个 toggle + "导出 CSV"按钮。

### 9.1 关键 UX 决策

- **切页恢复（`restoreActiveJob`）**：导入 job 在后端持续跑，但预览面板原本只用页面 local state，切到别的菜单再回来会丢失进行中/待确认状态。`onMount` 加载历史后，若最近 job 处于 `pending/running/previewed` 则恢复到预览面板并继续轮询。
- **文件选择框**：file `<input>` 必须放在可点击 dropzone `div` **外面**——嵌在里面时 `fileInput.click()` 的合成 click 会冒泡回 `div.onClick`，循环触发多个文件选择框。
- **下载（errors.log / CSV）走 `downloadViaFetch`**（`api.ts`）：`fetch` 拿 blob + `credentials:include` + `a.download`，而非 `<a href>` 直接导航——后者对 `/api/*` 的顶层导航会被 dev 的 Vite SPA fallback 变成空白 `index.html` 页。

---

## 10. 兼容性 / 部署注意事项

1. `cmd/api/main.go` 新增一份 `CatalogIngestService` 构造（字段同 `cmd/migrate/main.go`：`DB/Parser/TagSvc/CategorySvc/ScanJobService`）——api 进程此前从未引入过这个 service。
2. 上传体积上限单独定义 `adminimport.MaxCatalogBundleUploadSize = 200MB`（区别于给单个 zip 用的 `MaxArchiveUploadSize=50MB`）。
3. **客户端上传 body size 限制在集群 ingress 层（repo 外）**：`deploy/charts/` 下**没有** api 的 ingress/前置 nginx（gateway 的 nginx-router 只 proxy `/device/*`，不在 api 上传路径）。文件上传（来源 B）需部署时确认 api ingress `proxy-body-size`/`client_max_body_size` ≥ 210M；**来源 A（URL）不经此链路，为首选路径**。
4. 新表走标准 AutoMigrate + goose 迁移（`20260630000000`），不改现有表结构。
5. 部署顺序：后端 PR 须先于前端部署（新端点先在线）。

---

## 11. 验证

本地全栈（`costrict-local-stack`）真实导入上游最新 bundle（`catalog-bundle-manual-877d899`，14370 条目）端到端通过：

- URL 提交 → dry-run 预览（failed=0）→ confirm → success（added/updated 与预览一致）。
- 7 项验证：存量统计、历史、审计日志、缺评估/缺评分筛选、CSV 导出、errors.log 下载、**用户 seed item 的 `created_by`/`source_type`/`status` 全未变**。
- 前端 Playwright（auth 全栈真登录 portal 连真后端）：页面渲染、URL/文件上传两来源、切页恢复、筛选、下载均验证。
- 后端 `go build`/`vet`/`test`/`test -race` + 前端 `tsgo` typecheck 全绿。

---

## 12. 非目标 / Follow-up

- 单个 skill/mcp zip 的管理员强制导入（现有 `/plugins/upload` 已覆盖用户场景）。
- 导入回滚到上一次状态（catalog ingest 的 archive 是软删，重新导入含该条目的包即恢复；全量快照成本远超收益）。
- Follow-up（低优先）：`runner`/`worker`/`scan_worker` 三处 poller-queue-finalizer 模板可抽公共 `jobqueue`；`ImportResult` DTO 与 `services.IngestResult` 双维护可考虑给后者加 json tag。

---

## 13. 相关代码与文档

- ingest 底层链路：[`docs/CATALOG_INGEST.md`](../CATALOG_INGEST.md)
- 后端模块：`server/internal/adminimport/`（service / runner / handlers / module）
- 复用的 ingest service：`server/internal/services/catalog_ingest_service.go`
- 内容管理扩展：`server/internal/adminitem/`
- 装配：`server/cmd/api/main.go`（CatalogIngestService + leader import runner）
- 前端：`portal/opencode-display-revamp/packages/app-ai-native/src/pages/admin/pages/import.tsx` + `content.tsx`
