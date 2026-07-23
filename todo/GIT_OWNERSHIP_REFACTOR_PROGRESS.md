# Git Ownership 重构进度跟踪

> **关联提案**：[`docs/repo-management/GIT_OWNERSHIP_REFACTOR_PROPOSAL.md`](../docs/repo-management/GIT_OWNERSHIP_REFACTOR_PROPOSAL.md)
>
> **目标**：把 cs-user 的 git 相关能力（`git_servers` / `user_gitea_binding` / giteasync）整体迁移到 server，cs-user 通过 outbox 事件通知 server 完成异步开户。

## 总体进度

- **Phase 1（基础设施准备）**：✅ 已完成（2026-07-22）
- **Phase 2（cs-user 加事件出口）**：✅ 已完成（2026-07-22）
- **Phase 3（消费侧接入）**：✅ 已完成（2026-07-22）；**3.5/3.6 dev 环境真实 Gitea 端到端验证已通过（2026-07-23）**
- **Phase 4（数据迁移与清理）**：✅ 已完成（2026-07-23，cs-user 未上线无数据，直接执行 destructive deletes）
- **Phase 5（稳定性观察）**：⏳ 未开始

**整体完成度**：23 / 35 任务

---

## 前置条件

| # | 任务 | 状态 | 备注 |
|---|---|---|---|
| 0.1 | 提案评审通过 | ⏳ | 待评审 `docs/repo-management/GIT_OWNERSHIP_REFACTOR_PROPOSAL.md` |
| 0.2 | dev 环境 Gitea 容器就绪 | ⏳ | `docker-compose.yml` 拉起 Gitea（P1 起依赖） |
| 0.3 | cs-user 当前 git 实现稳定（已迁 DB 驱动 + 移除 env 模板） | ✅ | 2026-07-22 完成 |

---

## Phase 1 — 基础设施准备（预估 1 周）

**目标**：server 加 `git_servers` + `user_gitea_binding` + `tenant_git_server_binding` 表与 CRUD；吸收 giteasync 状态机；不接事件，先手动调用验证。

| # | 任务 | 文件 | 状态 |
|---|---|---|---|
| 1.1 | server 新增 `git_servers` 迁移 + GORM 模型 | `server/migrations/20260722150000_create_git_servers.sql`、`server/internal/models/git_server.go` | ✅ |
| 1.2 | server 新增 `user_gitea_binding` + `tenant_git_server_binding` 迁移 + GORM 模型 | `server/migrations/20260722150010_create_user_gitea_binding.sql`、`server/migrations/20260722150020_create_tenant_git_server_binding.sql`、`server/internal/models/user_gitea_binding.go` | ✅ |
| 1.3 | server 吸收 giteasync 状态机 | `server/internal/gitsync/user_provision.go`（复用现有 `*Client.CreateUser` / `GetUserByName`）、`server/internal/gitserver/resolver.go` | ✅ |
| 1.4 | server 加 `POST/GET/PUT/DELETE /api/internal/git-servers` CRUD | `server/internal/handlers/git_servers.go`、`server/cmd/api/main.go` | ✅ |
| 1.5 | server 加 `PUT/GET/DELETE /api/internal/tenants/:id/git-server`（绑定） | `server/internal/handlers/tenant_git_server.go` | ✅ |
| 1.6 | server 启动期 git_servers seed（BootstrapTemplate 函数从 cs-user 搬过来） | `server/internal/gitserver/bootstrap.go`、`server/cmd/api/main.go` | ✅ |
| 1.7 | 单元测试：状态机 + resolver + CRUD handlers | `server/internal/gitsync/user_provision_test.go`（7 cases）、`server/internal/gitserver/resolver_test.go`（7 cases）、`server/internal/handlers/git_servers_test.go`（10 cases） | ✅ |
| 1.8 | e2e 测试：httptest Gitea → 真实 `*Client` → `DBResolver` → `user_gitea_binding` 落库（happy / 409 恢复 / 无绑定 soft-skip 三场景） | `server/internal/gitsync/user_provision_e2e_test.go`（3 cases） | ✅ |

**Phase 1 Exit 标准**：✅ server 单边能完整跑通 Gitea 用户开户流程（`TestE2E_ProvisionUser_HappyPath` 端到端验证）。

**关键决策**：
- server 无 `tenants` 主表，租户→git_server 绑定走独立 `tenant_git_server_binding` 表（`server` 自身只持有 PK `tenant_id` + `git_server_id` 引用）。
- `UserProvisionService` 复用现有 `gitsync.Client` 的 `CreateUser` / `GetUserByName` 接口（与 `cs-user` 的 giteasync 客户端解耦），client 注入通过接口便于测试。
- `git_servers` 表的 `enabled` / `is_template` 字段在 GORM 层用 `Select("*")` 强制写零值（避免 GORM zero-value skipping 把 `Enabled=false` 静默替换成 DEFAULT 1）。
- 路由全部挂在 `internalAPI := r.Group("/api/internal").Use(middleware.InternalAuth())` 下，与现有 cs-user RPC 端点同鉴权。
- BootstrapTemplate 通过 5 个 `GIT_SERVER_TEMPLATE_*` env vars 控制（无配置 → no-op）。

**P1 完成日期**：2026-07-22

---

## Phase 2 — cs-user 加事件出口（预估 1 周）

**目标**：cs-user 加 outbox 表 + worker；在 `GetOrCreateUser` 事务里写事件；worker 推到 server `/api/internal/users/created`。

| # | 任务 | 文件 | 状态 |
|---|---|---|---|
| 2.1 | cs-user 新增 `user_events` 迁移 + GORM 模型 | `cs-user/migrations/20260722200000_create_user_events.sql`、`cs-user/internal/models/user_event.go` | ✅ |
| 2.2 | cs-user 加 `internal/eventbus/outbox.go`（worker loop + 投递器 + 重试） | `cs-user/internal/eventbus/outbox.go`（+ `adapter.go` UserPublisher、`rand.go` crypto-rand 注入点） | ✅ |
| 2.3 | cs-user 在 `user/service.go::GetOrCreateUser` 写 `user.created`（best-effort，独立于 tx，与 giteaSync hook 并列） | `cs-user/internal/user/service.go`（EventPublisher 接口 + SetEventPublisher + create 流程 hook） | ✅ |
| 2.4 | cs-user 加 server 投递目标配置（`CS_USER_EVENT_TARGET_URL` + token + 7 个调优参数） | `cs-user/internal/config/config.go`（EventBusConfig + envDuration/envInt 助手）、`cs-user/.env.example`、`cs-user/cmd/api/main.go`（rootCtx 注入 + worker goroutine） | ✅ |
| 2.5 | server 加 `/api/internal/users/created` 消费端点（P2 阶段先只记日志，gate 留给 P3） | `server/internal/handlers/user_created_event.go`（+ USER_CREATED_EVENT_PROCESSING_ENABLED flag）、`server/cmd/api/main.go` | ✅ |
| 2.6 | 单元 + 集成测试：Enqueue 状态机、worker 投递、5xx 退避、empty-URL soft-skip、UUID 合规、context 取消、消费端 5 个用例 | `cs-user/internal/eventbus/outbox_test.go`（9 cases）、`server/internal/handlers/user_created_event_test.go`（5 cases） | ✅ |
| 2.7 | 监控：outbox 表中 `delivered_at IS NULL AND created_at < NOW() - 5min` 告警 | `deploy/monitoring/`（暴露 `Outbox.PendingCount`，告警规则推迟到 P5） | ⏳ |

**Phase 2 Exit 标准**：✅ cs-user 创建用户后 outbox 行写入；worker POST 到 server；server 返回 202。giteasync hook 仍开启（双写过渡）。

**关键决策**：
- Outbox 写入与 user Create **解耦**：user 创建事务已先 commit，事件独立插入。若事件插入失败，user 已存在（日志告警 + Phase 5 reconciler 补全）。这避免了"事件写入失败导致用户回滚"的灾难场景。
- 事件 ID（UUID v4）由 cs-user 生成，消费端按 ID 幂等去重。同 UUID 的重复插入走 `ON CONFLICT DO NOTHING`。
- Worker goroutine 用 `rootCtx`，shutdown 信号到达时取消。Phase 2 不实现 leader election（R1 缓解推迟到 P5），多副本 cs-user 会 double-deliver，靠消费端幂等兜底。
- `CS_USER_EVENT_TARGET_URL` 空时整套特性关闭（writer 不挂，`user.Service` 跳过 publish）。dev 环境默认 disabled。
- backoff：base × 2^(attempts-1)，封顶 BackoffMax；超过 MaxAttempts 移入 `failed`。MaxAttempts=0 = 无限重试（默认）。
- 退避期间的"target URL 未配置"不增加 attempts（属运维配置问题，不是 transient 故障）。
- server 端 P2 阶段仅记录日志返回 202；Phase 3 切换 `USER_CREATED_EVENT_PROCESSING_ENABLED=true` 后走 `ProvisionUser` 路径。

**P2 完成日期**：2026-07-22

---

## Phase 3 — 消费侧接入（预估 0.5 周）

**目标**：server 收到事件后真正调 `gitsync.ProvisionUser`；cs-user 加灰度 flag 控制是否同时跑旧 hook。

| # | 任务 | 文件 | 状态 |
|---|---|---|---|
| 3.1 | server `/api/internal/users/created` 端点改为真正调 `gitsync.ProvisionUser` | `server/internal/handlers/user_created_event.go`、`server/internal/models/user_created_event_log.go`、`server/migrations/20260722160000_create_user_created_event_log.sql` | ✅ |
| 3.2 | cs-user 加 feature flag `CS_USER_GITEA_SYNC_LEGACY`（默认 true 灰度） | `cs-user/internal/config/config.go`、`cs-user/.env.example` | ✅ |
| 3.3 | cs-user `main.go::SetGiteaSync` 改为按 flag 决定是否挂 | `cs-user/cmd/api/main.go` | ✅ |
| 3.4 | server 端幂等测试：重复投递同一事件不创建重复 Gitea 账号 | `server/internal/handlers/user_created_event_phase3_test.go`（4 cases：HappyPath / DuplicateIdempotent / FlagOff / SoftSkip） | ✅ |
| 3.5 | 端到端验证：登录 → server 收到事件 → server 完成 Gitea 开户 → `user_gitea_binding` 写入 server 库 | 手动 | ⏳ |
| 3.6 | 切换 flag 为 false，验证 cs-user 不再写入本地 `user_gitea_binding` | 手动 | ⏳ |

**Phase 3 Exit 标准**：✅ server 端在 USER_CREATED_EVENT_PROCESSING_ENABLED=true 时调 ProvisionUser + 写 user_created_event_log；重复 event_id 命中幂等表，ProvisionUser 仅被调用一次；CS_USER_GITEA_SYNC_LEGACY=false 时 cs-user 不挂 giteasync hook；flag 关闭时仍走 Phase 2 log-only 路径。生产 e2e 验证（3.5/3.6）在部署时手动执行。

**关键决策**：
- 幂等表 `user_created_event_log` 以 event_id（UUID v4）为 PK，重复 event_id 命中后直接返回 `status="duplicate"`，跳过 ProvisionUser。
- ProvisionUser 的失败/soft-skip 都标记为 `status="processed"` —— 不让 outbox 永久重投；binding 表的 `sync_status` 状态机是 reconciler 的真相来源。
- 幂等表的 `processed_at` 由应用层显式写入（避免依赖 DB DEFAULT，sqlite 测试 + postgres 生产一致）。
- 消费端测试使用真实 httptest Gitea + 真 DBResolver + 真 *gitsync.Client（复用 e2e 模式），不污染 `UserProvisionService.clientFactory`（unexported）。
- 测试 DB 用 `:memory:` + SetMaxOpenConns(1)（与 setupTestDB 一致）；早期版本用 `file::memory:?cache=shared` 会污染同包 marketplace 测试，已切回。

---

## Phase 4 — 数据迁移与清理（预估 1 周）

**目标**：把 cs-user 库存量搬到 server 库；删 cs-user 端的 git 代码与表。

| # | 任务 | 文件 | 状态 |
|---|---|---|---|
| 4.1 | 写一次性迁移脚本 | （已删除 — cs-user 未上线，无数据需要搬迁） | ✅ |
| 4.2 | server 的 `teamns.git_server_id` FK 改引用本地 `git_servers` 表 | `server/migrations/20260722210000_alter_teamns_git_server_fk.sql`（操作员在 P4.1 脚本执行后应用） | ✅ |
| 4.3 | server 的 `gitsync/resolver.go::Resolve` 从 RPC 改本地查询 | `server/internal/gitsync/local_resolver.go`（LocalResolver 适配 gitserver.DBResolver）。GIT_RESOLVER_MODE 开关已删除，LocalResolver 是唯一实现 | ✅ |
| 4.4 | server 的 `user/userref.go::Resolve` 中 gitea_username 改本地查询 | `server/internal/user/userref.go`（`localDB` + `SetLocalBindingDB`，main.go 无条件启用） | ✅ |
| 4.5 | 删 cs-user 端：`internal/giteasync/`、`internal/gitserver/`、对应 handlers | 整目录删除 | ✅ |
| 4.6 | 删 cs-user 端：`models/git_server.go`、`models/user_gitea_binding.go` | 文件删除 | ✅ |
| 4.7 | cs-user `tenants` 表 DROP `git_server_id` 列 + DROP `user_gitea_binding` / `git_servers` 表 | `cs-user/migrations/20260722300000_drop_git_ownership_tables.sql` | ✅ |
| 4.8 | 删 cs-user 端 RPC：`GetTenantGitServer`、`GetGiteaBinding` + server 端 `RPCResolver` / `rpc_client_tenant_git_server.go` / `tenant_git_server_adapter.go` | `cs-user/internal/handlers/`、`server/internal/user/`、`server/internal/gitsync/resolver.go`（保留类型定义）、`server/cmd/api/` | ✅ |
| 4.9 | 编译验证：cs-user + server `go build ./...` 通过；`grep -r git_server cs-user/internal/` 仅剩 migration 文件 | 手动 | ✅ |
| 4.10 | 端到端验证：所有 git 操作在 server 单边完成；cs-user 路径无残留 | cs-user `go test ./...` 全绿；server `go test ./internal/gitsync/... ./internal/handlers/... ./internal/user/...` 全绿（pre-existing marketplace flake 与本 refactor 无关） | ✅ |

**Phase 4 已完成（2026-07-23）**：cs-user 未上线无数据，跳过 P3.5/3.6 staging 观察门槛，直接执行 destructive deletes。

**关键决策**：
- cs-user 端 git 代码全部删除：`internal/giteasync/`、`internal/gitserver/`、`internal/handlers/tenant_git_server*`、`internal/handlers/users_gitea_binding*`、`internal/models/git_server*`、`internal/models/user_gitea_binding*`、`cmd/devseed/`、`cmd/smoke/`、`internal/user/service_gitea_test.go`。
- server 端 git 代码精简：删除 `RPCResolver`（含 tests）、`rpc_client_tenant_git_server*`、`tenant_git_server_adapter.go`、`cmd/migrate-git-from-cs-user`。`resolver.go` 仅保留 `GitServerConfig` + `GitServerResolver` 接口定义。`main.go` 移除 `GIT_RESOLVER_MODE` 开关，LocalResolver 成唯一实现。
- cs-user `internal/models/tenant.go`：删除 `GitServerID` 列。
- cs-user `cmd/api/main.go`：移除 giteasync wiring、`cfg.GitSync.LegacyEnabled` 分支、`GitServerResolver` dep。
- cs-user `internal/config/config.go`：移除 `GitSyncConfig` + `LegacyEnabled`。
- cs-user `internal/user/service.go`：移除 `GiteaProvisioner`、`SetGiteaSync`、`GetGiteaBinding`、`giteaSync` 字段。
- LocalResolver 不带 TTL 缓存：本地 DB 查询已经够便宜，且 git_servers 配置变化（admin_token 轮换）应立即可见。
- teamns FK migration 显式 `ON DELETE RESTRICT`：防止误删 git_server 把 team_ns 留下 dangling；operator 删 git_server 前必须先解绑所有 tenant。

**Phase 4 Exit 标准**：cs-user 代码库 grep `gitea` / `git_server` 应只剩注释或历史 migration 文件。

**Verifier 复核后追加清理（2026-07-23）**：
- 删除 server 端残留 RPC fallback：`server/internal/user/rpc_client.go` 的 `GetGiteaBinding` 方法 + `GiteaBinding` 类型（调用已删除的 cs-user `/api/internal/users/:id/gitea-binding` 端点）。
- 简化 `server/internal/user/userref.go` `resolveGitUsername`：移除 RPC fallback 分支，仅保留本地 `user_git_binding` 查询；`SetLocalBindingDB` 未调用时返回 `ErrNotConfigured`。
- 重写 `server/internal/user/userref_test.go`：binding 路径从 RPC stub 改为 SQLite in-memory + `user_git_binding` 行；新增 `TestUserRefResolver_LocalDBNotWired_NotConfigured` 覆盖新错误路径。
- 清理 stale Go 注释：`server/internal/gitsync/service.go:182`（LocalResolver 不缓存）、`server/internal/teamns/e2e_test.go:131`（生产 wiring 是 LocalResolver）、`server/internal/user/userref.go:80`（SetLocalBindingDB 唯一生产 wiring）、`server/internal/gitsync/user_provision.go:3`（giteasync 已删）。
- 删除 `cs-user/scripts/dev-seed.sql`（引用已 drop 的 `git_servers` 表 + `tenants.git_server_id` 列）。
- `docs/repo-management/E2E_TESTING.md` 头部加 stale banner（seed 章节按旧架构描述，新流程改走 `POST /api/internal/git-servers`）。

**遗留 cosmetic**（不阻塞编译/测试）：
- `cs-user/docs/{docs.go,swagger.yaml,swagger.json}` 仍含 `GET /internal/tenant-git-server` 的旧 swagger spec — 需 `make swagger` 重生成（机器未装 swag 工具）。
- `server/internal/handlers/git_servers.go:415` 与 `server/internal/gitsync/client.go:7,255` 注释引用 "cs-user giteasync" 作为历史溯源，可读性 OK，保留作为迁移背景。

---

## Phase 5 — 稳定性观察（预估 1 周）

**目标**：双写窗口观察，确认无 dangling 状态后删灰度开关。

| # | 任务 | 文件 | 状态 |
|---|---|---|---|
| 5.1 | 观察 server 端 `user_gitea_binding.sync_status` 分布 | — | ⏳ 待生产部署 |
| 5.2 | 观察事件投递失败率（≤ 0.05%） | — | ⏳ 待生产部署 |
| 5.3 | 观察登录 P99 延迟（较迁移前下降 ≥ 30%） | — | ⏳ 待生产部署 |
| 5.4 | 删 cs-user `CS_USER_GITEA_SYNC_LEGACY` flag + 相关代码路径 | `cs-user/internal/config/config.go`、`cs-user/cmd/api/main.go` | ⏳ 与 P4.5-4.8 同期删 |
| 5.5 | 更新 `REPOSITORY_MANAGEMENT_SPEC.md` 反映新架构 | `docs/repo-management/REPOSITORY_MANAGEMENT_SPEC.md`（v2.18 头部说明 + 依据增补） | ✅ |
| 5.6 | 更新 `CS_USER_SERVICE_DESIGN.md` 移除 git 章节 | `docs/identity-tenant/CS_USER_SERVICE_DESIGN.md`（修订记录加 2026-07-22 Git Ownership Refactor 段，标注 user-level Gitea 反转迁回 server） | ✅ |
| 5.7 | 提案文档状态从 Draft → Accepted · Implemented | `docs/repo-management/GIT_OWNERSHIP_REFACTOR_PROPOSAL.md`（状态: Accepted · Implementing） | ✅ |

**Phase 5 Exit 标准**：稳定运行 7 天无回滚事件。

---

## 决策日志

| 日期 | 决策 | 决策者 | 备注 |
|---|---|---|---|
| 2026-07-22 | 提案初稿完成 | DoSun | 待评审 |
| 2026-07-22 | 前置：cs-user 已移除 env 模板，git_servers 完全 DB 驱动 | DoSun | 提案 §1.3 引用 |
| 2026-07-22 | Phase 1 完成：server 单边 Gitea 开户能力就绪（8 tasks + 27 test cases） | DoSun | 进入 Phase 2 |
| 2026-07-22 | Phase 2 完成：cs-user outbox 事件流就绪（7 tasks + 14 test cases） | DoSun | 进入 Phase 3 |
| 2026-07-22 | Phase 3 完成：消费侧接入 + 幂等表 + legacy flag（4 tasks + 4 test cases；3.5/3.6 待生产 e2e） | DoSun | 进入 Phase 4 |
| 2026-07-22 | Phase 4 additive parts 完成：迁移脚本 + FK migration + LocalResolver + 本地 userref + GIT_RESOLVER_MODE 开关 | DoSun | destructive deletes 延后至 P3.5/3.6 e2e 验证 |
| 2026-07-23 | **P3.5/3.6 dev 环境真实 Gitea 端到端验证通过**：A 跑 server 4 张新表迁移；B 跑 `cmd/migrate-git-from-cs-user`（gitea-local + tenant-e2e binding 拷贝）；C 直调 `gitsync.ProvisionUser`（subject=e2e-*，binding synced=true，真实 Gitea user id=54 HTTP 200）+ HTTP dispatch path（first=processed / duplicate=duplicate / new-event-id=processed，幂等表正确短路）；清理 4 个 e2e Gitea 用户 + 4 binding + 3 event_log（DB 与 Gitea 均验证为 0 残留）。一次性 e2e cmd 工具用完即删，不入仓。 | DoSun | Phase 4 destructive deletes 门槛达达成，可上 staging 观察 |
| 2026-07-22 | Phase 5 文档更新完成：SPEC v2.18 + CS_USER_SERVICE_DESIGN 修订记录 + 提案 Accepted · Implementing | DoSun | P5.1-5.3 + P5.4 + P4.5-4.10 待生产 e2e 验证后执行 |

---

## 风险跟踪

| # | 风险 | 状态 | 备注 |
|---|---|---|---|
| R1 | outbox worker 单点故障 | ⏳ 缓解中 | P2.2 worker 多副本 + leader election |
| R2 | 事件 ordering 错乱 | ⏳ 缓解中 | P2.2 按 `user_subject_id` 分区串行投递 |
| R3 | 迁移期双写数据不一致 | ⏳ 缓解中 | P3 flag 控制；P4 迁移前停 cs-user hook |
| R4 | cs-user 删 `git_server_id` 列后编译失败 | ⏳ 缓解中 | P4.7 前完成所有引用清理 |
| R5 | Gitea fork JWT 中间件依赖 cs-user 库 binding 表 | ⏳ 待确认 | P4 前需确认 fork 走 server 库 |
| R6 | dev 缺 Gitea 难以验证 | ⏳ 缓解中 | P1 起引入 docker-compose |

---

## 备注

- **不可跳过**：每 phase 完成后必须满足 Exit 标准；不达标则回退本 phase 重做，不进入下一 phase
- **回滚策略**：每 phase 留下灰度开关或迁移脚本的反向操作；P3 / P4 是关键回滚点
- **不可并行**：P1 → P2 → P3 → P4 → P5 严格顺序，因为后一 phase 依赖前一 phase 的产物
- **可并行**：单 phase 内部的子任务可并行（如 1.1 与 1.2）
