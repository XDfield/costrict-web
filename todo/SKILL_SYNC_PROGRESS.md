# Skill 仓库数据同步实施进度

参考方案：`docs/SKILL_SYNC_DESIGN.md`

## 总体进度

- **P0 阶段**（基础同步能力）：✅ 完成
- **P1 阶段**（增量优化 + 前端）：✅ 完成
- **P2 阶段**（Webhook + DryRun + 扩展解析）：✅ 完成

**全部完成**，`go build ./...`、`go vet ./...`、`tsc --noEmit` 均通过。

---

## P0 任务清单

| # | 任务 | 状态 | 说明 |
|---|------|------|------|
| 1 | 工程结构调整（cmd/api + cmd/worker） | ✅ 完成 | main.go 迁移至 cmd/api/main.go，新建 cmd/worker/main.go |
| 2 | 数据模型扩展 | ✅ 完成 | Organization.OrgType, SkillRegistry.SyncStatus/SyncConfig/LastSyncLogID, 新增 SyncJob/SyncLog |
| 3 | 新增依赖（go-git, gocron） | ✅ 完成 | go.mod 已添加 |
| 4 | GitService 实现 | ✅ 完成 | internal/services/git_service.go |
| 5 | ParserService 实现 | ✅ 完成 | internal/services/parser_service.go |
| 6 | JobService 实现 | ✅ 完成 | internal/services/job_service.go |
| 7 | SyncService 核心实现 | ✅ 完成 | internal/services/sync_service.go |
| 8 | Worker Pool 实现 | ✅ 完成 | internal/worker/worker.go + cmd/worker/main.go |
| 9 | Scheduler 集成 | ✅ 完成 | internal/scheduler/scheduler.go + cmd/api/main.go |
| 10 | 同步 API Handler | ✅ 完成 | internal/handlers/sync.go |
| 11 | CreateOrg API 扩展 | ✅ 完成 | handlers/handlers.go 扩展 sync 类型 |
| 12 | docker-compose 新增 worker 服务 | ✅ 完成 | docker-compose.yml |

## P1 任务清单

| # | 任务 | 状态 | 说明 |
|---|------|------|------|
| 13 | 增量同步优化（ContentHash 对比） | ✅ 完成 | SyncService 中基于 SourceSHA 跳过未变文件 |
| 14 | 冲突策略实现（keep_local/keep_remote） | ✅ 完成 | sync_service.go 中检测本地手动修改后跳过 |
| 15 | 前端：创建组织弹窗扩展 | ✅ 完成 | create-org-dialog.tsx 支持 sync 类型 |
| 16 | 前端：同步管理 Tab + 日志列表 | ✅ 完成 | org-sync-tab.tsx + dashboard 集成 |

## P2 任务清单

| # | 任务 | 状态 | 说明 |
|---|------|------|------|
| 17 | GitHub Webhook 接收（push 事件 + HMAC 验证） | ✅ 完成 | sync.go HandleGitHubWebhook + verifyGitHubSignature |
| 18 | DryRun 预览模式 | ✅ 完成 | SyncOptions.DryRun 贯穿 SyncService + API ?dryRun=true |
| 19 | plugin.json / AGENTS.md 解析支持 | ✅ 完成 | parser_service.go ParsePluginJSON + sync_service.go parseFile |

---

## 文件变更记录

### 新增文件
- `server/cmd/api/main.go` — API Server 入口（含 Scheduler）
- `server/cmd/worker/main.go` — Worker 独立入口
- `server/internal/services/git_service.go` — Git 操作封装
- `server/internal/services/parser_service.go` — SKILL.md / plugin.json / AGENTS.md 解析
- `server/internal/services/sync_service.go` — 同步核心逻辑（增量对比 + 冲突策略 + DryRun）
- `server/internal/services/job_service.go` — 任务入队/查询/取消
- `server/internal/worker/worker.go` — Worker Pool
- `server/internal/scheduler/scheduler.go` — gocron 调度器
- `server/internal/handlers/sync.go` — 同步相关 API Handler（含 GitHub Webhook）
- `server/Dockerfile.worker` — Worker 容器镜像
- `web/web-ui/components/org-sync-tab.tsx` — 同步管理 Tab 组件

### 修改文件
- `server/internal/models/models.go` — 扩展 Organization/SkillRegistry，新增 SyncJob/SyncLog
- `server/internal/handlers/handlers.go` — CreateOrganization 支持 sync 类型
- `server/internal/handlers/skill_registry.go` — UpdateRegistry 联动调度器
- `server/main.go` — 保留（兼容），核心逻辑迁移至 cmd/api/main.go
- `docker-compose.yml` — 新增 worker 服务

---

## 关键设计决策

1. **双二进制架构**：`cmd/api`（API Server + Scheduler，单副本）和 `cmd/worker`（Worker Pool，可横向扩展）
2. **PostgreSQL 队列**：使用 `SELECT FOR UPDATE SKIP LOCKED` 实现多 Worker 并发安全拉取
3. **去重机制**：同一 Registry 不允许同时存在多条 pending/running 任务
4. **退避重试**：失败后指数退避（30s → 5min → 终态 failed）
5. **增量对比**：基于 `SourceSHA`（文件内容哈希）跳过未变文件
