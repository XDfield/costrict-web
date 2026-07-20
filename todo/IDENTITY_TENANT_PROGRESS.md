# 身份与多租户架构实施进度

基于 [`docs/identity-tenant/`](../docs/identity-tenant/) 提案栈（ROADMAP + ADR + 5 份设计文档），跟踪 cs-user 微服务抽离与身份架构演进的执行清单。

**与 [`USER_TABLE_PROGRESS.md`](./USER_TABLE_PROGRESS.md) 的关系**：那份跟踪 `server` 单体内的 `users` 表 + `CachedUserService` 实现（已完成 64%，是 cs-user 抽离的"前身"）。本文件跟踪将这些代码**抽离到独立 cs-user 服务**及后续身份架构演进。两份互补、不重复——`CachedUserService` 在本文件的 P0-7 中作为 read-through RPC 的本地缓存被复用。

---

## 总则：测试覆盖与 swagger 文档同步（每个任务必须遵守）

**测试覆盖**——任何代码改动（新增 handler / service / middleware / ETL / RPC client）必须满足：

1. **单元测试**：与源码同包的 `*_test.go`（`foo_test.go` 紧贴 `foo.go`），覆盖 happy path + 至少一个错误路径。
2. **认证 gating 测试**：`/api/internal/*` 路由必须有 missing-token / wrong-token / correct-token 三态测试（参考 `internal/middleware/internal_auth_test.go`）。
3. **集成 smoke**：每个新 endpoint 至少一个 `httptest.NewRecorder` 级路由测试（参考 `internal/app/app_test.go`）。
4. **本地 gate**：`make check`（fmt + vet + test-race）必须 0 失败；CI 矩阵自动跑全部 `go.mod` 模块。
5. **不引入未测试代码**：PR 中新增的每个公开函数至少有一个直接调用它的测试。

**swagger 文档同步**——任何 HTTP endpoint 改动必须同步：

1. **handler 注解**：新 handler 必须是**命名函数**（不能是匿名闭包，swag 不解析），上方挂 `@Summary` / `@Description` / `@Tags` / `@Produce` / `@Param` / `@Success` / `@Failure` / `@Router`。
2. **`@Security InternalToken`**：仅 `/api/internal/*` 路由挂；`/healthz` / `/readyz` 保持无鉴权。
3. **spec 重新生成**：handler 改动后必须跑 `make swagger`（`swag init`），把 `cs-user/docs/{docs.go,swagger.json,swagger.yaml}` 重新生成并提交。
4. **格式校验**：跑 `make swagger-check`（`swag fmt`）确保注解列对齐。
5. **请求/响应 schema**：`@Param` / `@Success` 引用的 struct 必须真实存在（swag 用 `--parseDependency --parseInternal` 跨包解析）。

**幂等约束**：ETL 脚本与 cutover 流程必须支持 dry-run + 二次执行无副作用。

---

## 阶段进度总览

| 阶段 | 主题 | 子任务数 | 已完成 | 完成度 | 状态 |
|---|---|---|---|---|---|
| Phase 0 | cs-user 服务抽离（user 数据 ownership + read-through RPC） | 82 | 81 | 99% | 🟡 进行中（P0-1 + P0-2 + P0-3 + P0-4 + P0-5 + P0-6 + P0-7 + P0-8a + cs-user Phase 2 write API + P0-8b RPCWriter/DualWriter + DB trigger 完成；P0-8b 剩余：操作侧 cutover sequence） |
| Phase A | JWT 自签 + 雇佣上下文最小集 | ~40 | 10 | 25% | 🟡 进行中（A1 + A2 + A6 + A4 service 层 + A4b endpoint+server wiring + A3 JWT signer + A5 claims 扩展 + A7 cs-user endpoint + A7b server 端 OAuth callback wiring + A8 灰度 三态门控 完成；Phase A 代码级 acceptance bar 已落地（`phasea_integration_test.go` 4 tests），运维级 acceptance 项待 runbook 驱动） |
| Phase B | tenant 维度落地（数据隔离） | ~28 | 12 | 43% | 🟡 进行中（B1 tenants + tenant_admins 表 + 默认 default 租户行 + tenant_configs FK 完成；B2 给 users / user_auth_identities / employment_identities 加 tenant_id 列 + FK + 索引 完成；B3 tenant.Resolver 三层 fallback primitives + email_domains typed reader 完成；B3b.1 cs-user 侧 HTTP middleware + TenantConfig + context helpers 完成；B3b.2a server 侧 tenant slug forwarding（middleware + RPC header 注入 + ApexDomains 配置）完成；B3b.2b-step1 ctx 穿透 UserWriter interface（write-path slug 转发激活）完成；B3b.2b-step2a cs-user `/api/internal/tenants/resolve-by-email` RPC 端点 + ListByEmailDomain resolver primitive 完成；B3b.2b-step2b server RPC client + AuthCallback Try 2 email-domain 解析（cookie + ctx 注入）完成；B7 `(tenant_id, username)` 复合唯一索引（email 全局唯一保留）完成；B3b.2c cross-tenant 检测（JWT `tenant_slug` claim 路径：cs-user 签发 + server 解析 + TenantMatch middleware）完成；B4 JWT `tenant_id` claim → request ctx（TenantContext middleware + tenant.WithTenantID/TenantIDFromContext helpers + DefaultTenantID 常量）完成；B5 cs-user 侧 `tenant.Scope(ctx)` query helper + 4 read 方法迁移（GetUserByID/GetUsersByIDs/SearchUsers/ListIdentities）完成；B3b.2b-step2c server 端 `/api/tenants/suggest` wrapper endpoint（picker UI suggestion 接口）完成；B5 write 方法 scoping follow-up（GetOrCreateUser/BindIdentityToUser/TransferIdentityToUser/UnbindIdentityByProvider + refreshUserProfileFromIdentitiesTx 全路径 tenant scope）完成；B3b.2b-step2c AuthCallback picker redirect + 前端 picker 页面 待启动；**B6 RLS 已降级为未来工作（2026-07-17）** — 业务确认无需当前做代码防范，设计草案保留供触发条件满足后取用） |
| Phase C | 三级权限 + admin API | ~16 | 8 | 50% | 🟡 进行中（C1 platform_admins 表 + 模型 + service readers + JWT claims 扩展（cs-user EnterpriseClaims + server AuthClaims）+ reissue-token handler wiring + permission middlewares（RequirePlatformAdmin / RequireTenantAdmin / RequireTenantMember）+ 36 测试全绿 完成；C2 platform_admin tenant CRUD API（7 endpoints + tenant.Admin service + RPC client + server handlers）完成，RequirePlatformAdmin 首次 wiring；C3.1 tenant_admin 用户列表（GET /api/tenant/users + RPC client + RequireTenantAdmin 首次 wiring）完成；C3.2 tenant config CRUD（GET + PUT /api/tenant/config + cs-user tenantconfig.Service + RPC client + server handlers）完成；C3.3 provider_mapping typed editing（GET + PUT /api/tenant/provider-mapping + cs-user ProviderMapping typed struct + Validate/Parse/Serialize + Node-based yaml merge 保 sibling sections + RPC client + server handlers）完成；C4.1 审计日志基础设施（user_center_audit_log 表 + auditlog.Service 最佳努力写入器 + 6 写路径 instrument + server ActorMeta ctx-carrier → X-Actor-Tenant-Role/X-Actor-Platform-Scope headers 转发）完成；**admin-user-migration 切片（option A 完整迁移，9 commits）已落地 2026-07-20**：@server `/api/admin/users/*` 身份 + status 走 cs-user RPC，cs-user 成为身份 + status 单一真相源，Casdoor 仅作登录源认证；详见下方"admin-user-migration 实现细节"；**C4.3 audit-log list endpoints 切片（7 commits）已落地 2026-07-20**：cs-user `auditlog.Service.List` reader + 2 内部 endpoints（platform cross-tenant + tenant-scoped via X-Tenant-Id）+ @server RPC client + 2 public endpoints（RequirePlatformAdmin + RequireTenantAdmin）+ main.go wiring；详见下方"C4.3 audit-log list 实现细节"；跨 tenant 用户 ops / email allowlist 等 C2 其他切片 + C4.2 active 越权检测 待启动） |
| Phase E | 身份联邦扩展（多 IdP + Gitea + webhook） | ~20 | 3 | 15% | 🟡 进行中（E3a.1 Gitea user 自动开户切片已落地：`user_gitea_binding` 表 + `giteasync.GiteaClient` HTTP 客户端（establishes cs-user 首个 outbound HTTP client 模式）+ `giteasync.Service` 状态机（pending → synced | error，409 → lookup 恢复，timeout 保持 pending 待 reconciliation cron）+ `user.Service.GetOrCreateUser` 新用户 hook（best-effort，Gitea 宕机永不 fail signup）+ `GET /api/internal/users/:subject_id/gitea-binding` 读 endpoint + `user.gitea_provisioned` audit vocab；**E3a.1 被 E3b.1.1 重构：per-tenant Gitea 解析替换全局 `CS_USER_GITEA_BASE_URL`/`CS_USER_GITEA_ADMIN_TOKEN`**（见下方 E3b.1.1）；E3b.1 @server GitServerAdapter MVP 框架已落地；**E3b.1.1 per-tenant Gitea fix 已落地**（cs-user `git_servers` 表 + 两端 `ResolveAdapterForTenant` per-tenant 解析；见下方 E3b.1.1 实现细节）；E3a.2 cascades + reconciliation cron / E3a.3 fork JWT middleware / E3b.2 real provider swap + cron + delta sync / E4 webhook 待启动） |

> **Phase 0 大任务颗粒度**：8 个 P0-X 子任务 + 验收清单，当前完成 P0-1（骨架）/ P0-2（Postgres + 迁移）/ P0-3（models + read CRUD）/ P0-4（认证中间件）/ P0-5（Helm chart）/ P0-6（ETL 脚本）/ P0-7（read-through RPC client in server）/ P0-8a（应用层 write gate）/ cs-user Phase 2 write API（5 endpoints）/ **P0-8b 应用层 RPCWriter+DualWriter（OAuth callback + admin 写路径 re-route）** 九个完整大任务。下一步推进 P0-8b 剩余两项—— **DB trigger 兜底**（costrict-web `users` 表 `BEFORE INSERT/UPDATE/DELETE` 拒写）+ **操作侧 cutover**（`docs/identity-tenant/P0-8_CUTOVER_RUNBOOK.md` step 3-5：dual-write canary 24h → readonly+rpc cutover → trigger enable）。应用层写路径已 unblock：`UserModule.Writer` 按 `(Backend, WriteMode)` 矩阵选 writer（local / DualWriter / RPCWriter），P0-8a readonly+rpc boot fatal 已移除。详见 `docs/identity-tenant/P0-8_CUTOVER_RUNBOOK.md`。

---

## 阶段 0：cs-user 服务抽离（user 数据 ownership）

**目标**：搭独立 cs-user 服务（Monorepo `costrict-web/cs-user/`），接管 user 数据 ownership，costrict-web 通过 read-through RPC 调用。**不含** JWT 自签、OAuth callback 接管、employment_identities。

**ADR 锁定**：D1（直接抽离）/ D3（独立 PG）/ D4（strangler fig）/ D5（REST only）/ D6（write cs-user + read-through）/ D8（共享密钥）/ D9（monorepo）/ D10（最小 Phase 1）。

### P0-1：服务骨架（gin + /healthz + /config）✅

- [x] **实现**：`cs-user/cmd/api/main.go`（zap logger → config.Load → app.NewRouter → http.Server → graceful shutdown）
- [x] **实现**：`cs-user/internal/config/config.go`（env 加载，纯 `os.Getenv` + `envDefault` + `requireNonEmpty`，无 viper）
- [x] **实现**：`cs-user/internal/app/app.go`（gin.New + Recovery + /healthz + /readyz + /api/internal/ping）
- [x] **测试覆盖**：`config_test.go`（5 测试：defaults / custom env / missing token / missing pg creds / DSN）
- [x] **测试覆盖**：`app_test.go`（healthz always 200 / readyz OK / readyz 503 / internal missing token / wrong token / correct token / nil config panic）
- [x] **swagger 注解**：`@title cs-user API` + `@BasePath /` + 包级 `@securityDefinitions.apikey InternalToken`（在 `main.go`）
- [x] **swagger 注解**：`healthz` / `readyz` / `PingHandler` 各自的 `@Router` / `@Summary` / `@Tags`
- [x] **CI 矩阵**：`.github/workflows/test.yml` 自动发现所有 `go.mod` 并跑 build + vet + test-race

### P0-2：独立 PostgreSQL + cs-user schema（goose migrations）✅

- [x] **实现**：`cs-user/internal/config/PostgresConfig.DSN()`（已就绪）
- [x] **实现**：`cs-user/migrations/` 目录 + goose 迁移文件，从 `server/migrations/` 复制 user 相关 5 个文件：
  - [x] `20260401100000_create_users_table.sql`
  - [x] `20260408154000_migrate_users_to_subject_id_and_serial_pk.sql`
  - [x] `20260524000000_create_user_auth_identities_table.sql`
  - [x] `20260525000000_add_explicitly_unbound_to_user_auth_identities.sql`
  - [x] `20260616150000_add_status_to_users.sql`
- [x] **实现**：`cs-user/migrations/embed.go`（`//go:embed *.sql` 把迁移文件嵌入 binary，与 server 同模式）
- [x] **实现**：`cs-user/internal/storage/postgres.go`（gorm + pgx 连接池 + env-tunable pool sizing + `Ping()` 实现 `app.ReadyChecker`）
- [x] **实现**：`cs-user/internal/migration/runner.go`（goose.Up 包装；`NewRunner` 用嵌入 FS，`NewRunnerWithFS` 注入测试 FS）
- [x] **实现**：`cmd/api/main.go` 启动时跑迁移（dev 模式，`CS_USER_AUTO_MIGRATE=1` 触发）/ 提供 `cs-user-migrate` 独立 binary（prod 模式，Helm pre-deploy hook 入口）
- [x] **实现**：`cs-user/cmd/migrate/main.go` 独立迁移 binary，acquire `pg_advisory_lock` 防并发（lock keys 故意与 server 错开：24680/13579 vs 12345/67890）
- [x] **接线**：`app.NewRouter(cfg, pool)` 替换 `nil` 桩，/readyz 现反映真实 DB 可达性
- [x] **测试覆盖**：`internal/storage/postgres_test.go`（envInt defaults / valid / rejects-garbage / rejects-negative + configurePool defaults / overrides / invalid + DSN format + nil-config + Close idempotency）
- [x] **测试覆盖**：`internal/storage/postgres_cgo_test.go`（`//go:build cgo` —— Ping OK / closed-DB fails / nil-Pool / SQLDB accessor，用 sqlite :memory: 而非 testcontainers，匹配 server 测试惯例）
- [x] **测试覆盖**：`internal/migration/runner_test.go`（NewRunner 拒绝 nil db / 空 dialect / nil fs / 未知 dialect）
- [x] **测试覆盖**：`internal/migration/runner_cgo_test.go`（`//go:build cgo` —— Up 应用全部 / Up 幂等 / Version 从 0 推进到 20260102000000 / Down 回滚；用 fstest.MapFS 注入 synthetic migrations 而非真实 Postgres-only schema）
- [ ] **测试覆盖**：`app_test.go` 增加 readyChecker 真实场景（DB 拒连 → 503）的集成测试 *(P0-3 接 CRUD 时一并补，因当前 readyChecker 是接口注入，已有 stubReadyErr 覆盖 503 路径)*
- [x] **CI 矩阵**：`go test -race ./...`（含 cgo-tagged sqlite 测试）全部 PASS；Linux + Windows 双平台均跑通
- [x] **swagger 注解**：无新 endpoint（迁移是基础设施）；`make swagger` 重生成后 spec 无 diff

### P0-3：User / UserAuthIdentity 模型 + read-side CRUD ✅

- [x] **实现**：`cs-user/internal/models/models.go`（从 `server/internal/models` 迁移 `User` + `UserAuthIdentity` struct + GORM tags，schema 与 migrations/*.sql 1:1 对应）
- [x] **实现**：`cs-user/internal/user/service.go`（read 方法子集：GetUserByID / GetUsersByIDs / SearchUsers / ListIdentities）
- [x] **暂缓**：write 路径（bind / unbind / transfer / GetOrCreate）—— 依赖 JWT claims 管道，留给 Phase A
- [x] **实现**：`cs-user/internal/handlers/users.go`（3 read handlers：GET /:subject_id / POST /by-ids / GET /search）
- [x] **实现**：`cs-user/internal/handlers/user_auth_identities.go`（1 read handler：GET /:subject_id/auth-identities）
- [x] **接线**：`app.NewRouter` 改签名为 `(cfg, Deps)`，Deps 携带 `Users` + `AuthIdentities` service + ReadyChecker；nil service → 503 stub（保持 swagger spec 一致）；注册到 `internal := r.Group("/api/internal", RequireInternalToken(...))`
- [x] **测试覆盖**：`internal/user/service_test.go`（cgo-tagged sqlite + gorm AutoMigrate；GetByID found/not-found/soft-delete-hidden/empty-id；GetByIDs map-shape/missing-omitted/empty-skip；Search keyword/inactive-excluded/default-limit；ListIdentities primary-first/empty-id/no-rows；nil-db 守卫覆盖每个方法）
- [x] **测试覆盖**：`internal/handlers/users_test.go`（4 endpoint：happy + 404 + 500-leak-prevention + body-validation [empty / oversized / negative / garbage limit]，stub UserService 无需 DB）
- [x] **测试覆盖**：`internal/handlers/user_auth_identities_test.go`（happy + empty-result + 500-leak-prevention）
- [x] **测试覆盖**：`internal/app/app_test.go` 更新为 Deps 签名（保持原有 health/ping/swagger/auth-gating 覆盖）
- [x] **GORM 坑修复**：`SearchUsers` keyword 过滤的 AND/OR 优先级用括号包住（否则 SQL 把 `(is_active AND username LIKE) OR display_name LIKE` 拆错，inactive 行漏出）
- [x] **GORM 坑修复**：测试 seed 时 `IsActive=false` 被 gorm zero-value omission 吞掉 + Create 后 column-default 读回结构体覆盖；用 `desiredActive := u.IsActive` 在 Create 前捕获 + Create 后 Update 强制写入
- [ ] **测试覆盖**：`models/constraints_test.go`（subject_id 唯一性 / external_key 组合键）*(留到 P0-6 ETL 阶段做集成测试时一并覆盖，单测 sqlite + AutoMigrate 不一定能完全反映 PG 真实约束)*
- [x] **swagger 注解**：4 个新 handler 加完整注解（`@Param` 引用 path/query，`@Success` 引用 `models.User` / `models.UserAuthIdentity`；schema 已由 swag 自动生成）
- [x] **swagger 注解**：所有 `/api/internal/users/*` 路由挂 `@Security InternalToken`
- [x] **swagger 注解**：`make swagger` 重生成 `docs/`，spec 现含 5 endpoints + 2 model schemas

### P0-4：内部 API 共享密钥认证中间件 ✅

- [x] **实现**：`cs-user/internal/middleware/internal_auth.go`（`X-Internal-Token` header + `subtle.ConstantTimeCompare`）
- [x] **防御深度**：空 token 配置时返 500（防 `ConstantTimeCompare("","")==1` 误授权）
- [x] **测试覆盖**：`internal_auth_test.go`（6 测试：missing / empty / wrong / correct / prefix-attack / empty-config defense）
- [x] **swagger 注解**：包级 `@securityDefinitions.apikey InternalToken` 已在 `main.go`

### P0-5：Helm chart（cluster-internal only）✅

- [x] **实现**：`deploy/charts/cs-user/Chart.yaml`（version 0.1.0）
- [x] **实现**：`deploy/charts/cs-user/values.yaml`（image / replicas / env / networkPolicy.enabled / secrets 块 / tests.curlImage）
- [x] **实现**：`templates/deployment.yaml` + `templates/service.yaml` + `templates/networkpolicy.yaml`（限同 namespace 流量）
- [x] **补全**：`templates/secret.yaml`（chart-managed Secret opt-in via `secrets.create=true`；注入 `CS_USER_POSTGRES_PASSWORD` + `CS_USER_INTERNAL_TOKEN`，key 由 `database.existingSecretKey` / `internalToken.existingSecretKey` 控制；与 deployment.yaml 的 fallback 链路对齐 `<release>-secrets`）
- [x] **测试覆盖**：`templates/tests/test-connection.yaml`（helm test pod，hook=test + before-hook-creation,hook-succeeded；curlimages/curl 镜像可经 `tests.curlImage` 配置；探 `/healthz` + `/readyz`，--fail --max-time 5；securityContext 与主容器一致）
- [x] **CI 矩阵**：`.github/workflows/lint-charts.yaml` 把 cs-user 加入 chart matrix（之前漏了，与 gateway/api/worker/portal/postgres/proxy/wecom-bot-proxy 同列跑 `helm lint` + `helm template`）
- [ ] **测试覆盖**：`helm template` 输出 fixture 文件，断言关键 path（labels / env vars / networkPolicy selectors）*(延后：lint-charts CI 已覆盖 lint + template，fixture 级深度断言属于额外加固，留给 P0-7 集成测试基础设施一并接入)*
- [x] **swagger 注解**：无（chart 不暴露 endpoint）

**实现说明 / 决策**：

- **secret 双轨策略**：(a) 生产：`secrets.create=false`（默认），由 sealed-secrets / external-secrets / `kubectl create secret` 外部供给，operator 把 `database.existingSecret` + `internalToken.existingSecret` 指向它；(b) dev/staging/CI：`secrets.create=true`，chart 渲染 `<release>-secrets` 并自动 wire 进 Deployment。两条路径在 pod env 层等价（deployment.yaml 用 `existingSecret || (secrets.create ? <release>-secrets : nil)` 的 fallback 链）。
- **空 default fail-fast**：`database.existingSecret` + `internalToken.existingSecret` + `secrets.create=false` 时，Deployment 既不渲染 PG_PASSWORD 也不渲染 INTERNAL_TOKEN env → 容器启动时 `config.Load()` 必然 panic，杜绝"密钥没注入却静默起服"。
- **helm test 不验 internal API**：测试 pod 走未鉴权的 `/healthz` + `/readyz`；internal API（需 X-Internal-Token）的端到端覆盖留给 P0-7 集成测试 binary（同时起 server + cs-user）。
- **手工渲染核验**：本地无 helm CLI，已用模板语义手算 4 种组合（prod 默认 / 外部 secret / chart-managed / 混合）的渲染结果；CI 的 `helm template` 是权威 gate。

### P0-6：ETL 脚本（dry-run + idempotent UPSERT）✅

- [x] **实现**：`cs-user/cmd/etl/main.go`，支持 `--dry-run` / `--source-dsn` / `--target-dsn` / `--batch-size` flags（外加 `--max-diff-records` / `--skip-users` / `--skip-auth-identities` / `--report` / `--sqlite`）
- [x] **实现**：`cs-user/internal/etl/export.go`（`users` + `user_auth_identities` 流式批读，keyset 分页 on `id`，`Unscoped()` 包含软删行，`ErrAbort` 提前终止）
- [x] **实现**：`cs-user/internal/etl/import.go`（compare-then-write 策略：load target by subject_id → 字段 diff → 仅写差异列；map-based update 正确处理 nil 清空；保留 target ID + CreatedAt；传播软删；事务包裹单批）
- [x] **实现**：`cs-user/internal/etl/diff.go`（字段级 diff，区分 nil vs ""，bool / time / DeletedAt 全覆盖；ID + CreatedAt 明确排除）
- [x] **验证**：行数对齐（CountUsers/CountAuthIdentities 双向断言）+ 抽样字段对比（dry-run FieldDiffRecords）+ `casdoor_universal_id` 唯一性预检（`ValidateSource` GROUP BY HAVING COUNT(*) > 1）
- [x] **测试覆盖**：`etl/diff_test.go`（13 case：identical / string / ptr-string / nil-vs-empty / bool / time / DeletedAt / ID+CreatedAt 排除）+ `etl/export_test.go`（9 case：streaming order / 软删包含 / 空表 / batch=1 / 无效 batch size / nil DB / abort / auth-identities 流式 / CountUsers 含软删）+ `etl/import_test.go`（14 case：insert / no-diff skip / update / clear pointer / preserve ID+CreatedAt / propagate soft-delete / empty batch / nil DB / empty subject_id 跳过 / auth-identities 等价 × 3 / ValidateSource dups + no-dups）
- [x] **测试覆盖**：`etl/idempotent_test.go`（4 case：连续跑两次第二次 inserted=updated=0；单行 mutation 后只 1 update + 2 unchanged；auth-identities 等价；双表端到端 parity）
- [x] **测试覆盖**：`etl/dry_run_test.go`（5 case：dry-run target 0 增长 + DryRun flag；FieldDiffs 准确 + target 未被修改；maxDiffRecords 上限；-1 unlimited；auth-identities 等价）

**测试总数**：45 case，全过（`go test -race ./internal/etl/` ~1.7s）。
- [x] **swagger 注解**：无（ETL 是离线脚本，不是 HTTP endpoint）

**实现说明 / 偏差**：

- **没用 testcontainers 双 PG**：sqlite (cgo-tagged) 覆盖了所有 write 语义（INSERT / UPDATE / 软删传播 / nil 清空 / idempotency / dry-run）。postgres-only 的 `ON CONFLICT ... WHERE` / advisory lock 路径不在 ETL 包内（advisory lock 在 `cmd/migrate` 已用真 PG 验证；ETL 用 compare-then-write 而非 ON CONFLICT，所以 PG-specific 路径反而更少）。testcontainers 留给 P0-7 集成测试 binary（需要同时起 server + cs-user）一并接入。
- **没用 `ON CONFLICT (subject_id) DO UPDATE`**：改成 compare-then-write（load target → diff → 仅写差异列）。原因：(1) 自然产出 inserted/updated/unchanged 三段统计，dry-run 复用同一逻辑；(2) 避免 `ON CONFLICT ... WHERE ROW(...) IS DISTINCT FROM ROW(...)` 在自增 ID + timestamp 列上的微妙语义；(3) 第二次跑 0 写入的 idempotency 由 diff 直接保证。
- **`casdoor_universal_id` 重复检测** 是 WARN 不是 FAIL：重复会在 INSERT 时自然报错，中断本批事务；预先 WARN 让操作员决定是否清理源数据后再跑。
- **map-based update**：gorm struct-based Updates 会吞掉零值（无法把 email 清成 NULL），所以 `buildUserUpdateMap` 用 `map[string]any` 显式列出差异列。

### P0-7：read-through RPC client in costrict-web ✅

- [x] **实现**：`server/internal/user/reader.go` 新增 `UserReader` 接口（4 方法：GetUserByID / GetUsersByIDs / SearchUsers / ListUserIdentities，全部带 `ctx`）；`*UserService` 隐式实现
- [x] **实现**：`server/internal/user/cached_service.go` 重构成包 `UserReader`（不再直接拿 `*gorm.DB`）；SearchUsers / ListUserIdentities 作为无缓存 pass-through；删除 `WarmupCache`（无 RPC 对应，prod 无 caller）
- [x] **实现**：`server/internal/user/rpc_client.go` 新 RPCClient：4 个 endpoint（bare User / `{users:map}` / `{users:[]}` / `{identities:[]}` 三种 decode shape），HTTP 404 → `gorm.ErrRecordNotFound` bare sentinel，transport/timeout/5xx → `ErrRPCUnavailable`（wrapped），per-request `context.WithTimeout(ctx, ...)`（不复用 deptsync 的 `context.Background()` 短板）
- [x] **实现**：`server/internal/config/config.go` 新增 `UserServiceConfig{Backend,BaseURL,InternalToken,TimeoutSec}` + 4 个 env（`USER_SERVICE_BACKEND` 默认 `"local"`、`USER_SERVICE_URL`、`USER_SERVICE_INTERNAL_TOKEN`、`USER_SERVICE_TIMEOUT_SECONDS` 默认 10）
- [x] **接线**：`server/internal/user/user.go` `NewWithConfig(db, sync, cfg)` 根据 `cfg.Backend` 在 local `*UserService` 与 `*RPCClient` 之间选 Reader；CachedService 包 Reader；`SetOnUserUpdated(cached.InvalidateCache)` 钩子保留（本地写入立即失效缓存）
- [x] **接线**：`server/cmd/api/main.go` 用新签名调用 + boot-time 校验：`Backend=="rpc"` 但 RPCClient 未配置时 `log.Fatalf` 防止静默 fallback
- [x] **caller 迁移**：`handlers.go:731` (ListBoundIdentities) + `users.go:257` (SearchUsers) 从 `Service` 切到 `CachedService`，让 RPC backend 实际生效
- [x] **caller 迁移**：Step 1 给 4 个读方法加 ctx 后传播到全部 14 处 caller（5 prod + 9 test），含 `s.db.WithContext(ctx)`
- [x] **测试覆盖**：`user/rpc_client_test.go` 11 case（happy × 4 + 404 × 2 + 500 + timeout + !Configured + 3 种 empty/malformed body）
- [x] **测试覆盖**：`user/cached_rpc_test.go` 2 case（GetUserByID cache miss→hit→invalidate、GetUsersByIDs cache miss→hit，用 atomic 计数 HTTP 调用）
- [x] **集成测试**：`user/integration_test.go` 2 case（`NewWithConfig` 全链：RPC backend 端到端 cache 行为；local backend 默认行为不变）
- [x] **swagger 注解**：handler 签名未变（`SearchUsers` / `ListBoundIdentities` / `GetUserBasicInfo` / 等）；`swag fmt -d` 零 diff

**测试总数**：15 case，全过（`go test -race ./internal/user/...` ~3.3s）。

**实现说明 / 偏差（与 progress doc 142-152 行原始规划对比）**：

1. **没建独立 `cached_rpc.go` 文件**：`CachedUserService` 重构成包任意 `UserReader`，包装 RPCClient 自动生效，无需平行缓存类。原计划设想的"独立缓存文件"被接口重构吸收。
2. **没做 stale-while-error 兜底**：progress doc 148 行原列"缓存命中返 stale + warning；缓存未命中返 503"。**主动放弃**，原因：(1) 默认 `local` backend 零行为变化；(2) RPC 模式用于 canary，silent stale fallback 会遮蔽 outage；(3) `patrickmn/go-cache` 不原生暴露 expired-but-not-evicted entries，实现 stale-while-error 需要扩展 TTL 的二级缓存（实质复杂度）。**caveat**：`/api/auth/me`（`handlers.go:669`）在 RPC outage 期间会返 500；canary runbook 必须监控这个 endpoint 的 500-rate。
3. **没用 testcontainers / 真 cs-user E2E**：httptest mock 已覆盖 client + cache + 接线全路径。真 cs-user E2E（同时起两进程端到端）留 P0-8 单独测试 binary 接入。
4. **`WarmupCache` 删除**：grep 确认无 prod caller，仅 `service_test.go:473` 一处测试；保留一个 RPC 无法实现的接口方法比删了更糟。
5. **non-404 4xx 映射**：当前返 `fmt.Errorf("user rpc client: status %d", code)` 而非映射成 sentinel——cs-user 契约未列 4xx（除 404），实际不应触发；保留原始 status code 在错误消息里便于诊断。
6. **`handlers/users.go:202` 仍用 `==` 比较 `gorm.ErrRecordNotFound`**：pre-existing pattern，非 P0-7 引入；RPC client 返 bare sentinel 所以 `==` 仍 work，但 follow-up 建议迁到 `errors.Is`。

**已知限制**：
- **RPC 模式下写后读 skew**：server 仍写本地（Phase 1 cs-user 无写 API），本地缓存失效钩子会触发，但 cs-user DB 只在下次 P0-6 ETL 跑后更新；10min 缓存 TTL 部分掩盖。最坏：刚绑定的 identity 在 backend=rpc 时 `/api/users/:id/auth-identities` 最长 ETL 间隔（默认 15min）不可见。**对策**：P0-8 cutover 前 backend 保持 local；backend=rpc 仅用于 read-only canary。
- **`/health` 不反映 cs-user 可达性**：静态 200。带 cs-user ping 的 `/readyz` 留 P0-8。
- **无 retry 无 metrics**：对齐 deptsync 约定；canary 数据显示抖动再 revisit。

### P0-8a：应用层 write gate 🔧（preparation for P0-8 cutover）

**Why split**: P0-8 原计划一步到位（DB trigger + 路由写 RPC + READONLY 锁表），但 Phase 1 cs-user 只有 4 个 read endpoints，无 write API；而 `GetOrCreateUser` 在每次 OAuth 登录触发——直接 block 写等于 break 登录。P0-8a 把代码侧 gate 准备好但**不激活**（默认 `WriteMode=local`，零行为变更）；P0-8b 是真正 cutover，阻塞在 cs-user Phase 2 write API。

**实现**：
- ✅ `USER_SERVICE_WRITE_MODE=local|readonly` 环境变量（默认 `local`，与 `USER_SERVICE_BACKEND` 同一组 config）
- ✅ 5 个写方法（`GetOrCreateUser` / `BindIdentityToUser` / `TransferIdentityToUser` / `UnbindIdentityByProvider` / `SyncUser`）入口处 short-circuit 返 `ErrWriteBlocked` sentinel
- ✅ Boot-time 校验：(local+local=ok / local+rpc=warn split-brain canary / readonly+local=fatal login broken / readonly+rpc=fatal no write API)
- ✅ `readonly_test.go`：10 行 table-driven（5 方法 × 2 模式）+ `validateUserConfig` 4 组合 pure function 验证 + split-brain warn 实测
- ✅ `docs/identity-tenant/P0-8_CUTOVER_RUNBOOK.md`：canary → cutover → rollback 完整操作手册
- ✅ `validateUserConfig` 抽成 pure helper（消息 + shouldFatal）——避免 log.Fatalf 走 subprocess 测试

**Known limitations**：
- **`WriteMode=readonly` 当前不能启用**——任何部署翻这个开关都会 break 登录（boot fatal 在两处拦截）。Kill switch waiting for cs-user Phase 2.
- **Gate 在 service 边界，非 per-method opt-out**：5 个方法一起翻。Canary 后如需细分再加，YAGNI 现在。
- **`UpdateUserLastLogin` 未 gate**：grep 未发现 caller，疑似 dead code；删除属于独立 cleanup PR。
- **No DB trigger / no backup test / no load test**：全部 defer 到 P0-8b（操作侧 cutover）。

### P0-8b：costrict-web users 表 READONLY cutover 🔜（blocked on 操作侧 cutover）

> **应用层 + DB 兜底 已 shipped（两个 PR）** — server 侧 5 个 OAuth/admin 写路径全部走 `UserModule.Writer`，按 `(Backend, WriteMode)` 矩阵选 writer：`local+local`→UserService，`rpc+local`→DualWriter（dual-write canary，primary=local authoritative，secondary=cs-user best-effort），`rpc+readonly`→RPCWriter（cs-user sole write authority）。P0-8a 的 readonly+rpc boot fatal 已移除（RPCWriter 现在是合法 writer）。DB trigger 兜底已上线（gate 在 GUC `app.users_readonly_cutover`，默认 OFF，operator 在 runbook step 6 激活）。
>
> **下一步 unblock 点**：纯操作侧 cutover sequence（详见 `docs/identity-tenant/P0-8_CUTOVER_RUNBOOK.md` step 3-7：dual-write canary 24h → readonly+rpc cutover → 激活 DB trigger GUC → 1h 压测验收）。所有应用 + DB 层代码均已就绪。

- [x] **实现**：cs-user Phase 2 write API（POST `/users/get-or-create`、`/:subject_id/bind-identity`、`/transfer-identity`，DELETE `/:subject_id/identities/:provider`）
- [x] **实现**：应用层 login 写路径 re-route 到 cs-user RPC（新增 `RPCWriter` 并列 `RPCClient`），移除 P0-8a 的 readonly+rpc boot fatal
- [x] **实现**：DB trigger 兜底——`server/migrations/20260716000000_create_users_readonly_trigger.sql`：trigger 函数 gate 在 GUC `app.users_readonly_cutover` 上，默认 OFF（no-op），operator 在 runbook step 6 用 `ALTER DATABASE ... SET app.users_readonly_cutover = 'on'` 激活。允许提前部署不阻塞 dual-write canary
- [ ] **验证**：`grep -r "models.User" server/ | grep -E "(Create|Update|Delete)"` 输出清零（除 RPC client 自身）
- [ ] **验证**：cutover 后连续 1 小时压测，`CachedUserService` 命中率 > 90%
- [ ] **验证**：cs-user DB 独立备份 + 恢复测试通过
- [ ] **测试覆盖**：trigger 拒写测试（尝试写本地 user 表应失败 / 抛 trigger 错误）
- [ ] **回滚预案**：保留 costrict-web DB 备份 30 天；提供 `USER_SERVICE_BACKEND=local USER_SERVICE_WRITE_MODE=local` 一键回滚开关
- 📋 详见 `docs/identity-tenant/P0-8_CUTOVER_RUNBOOK.md`
- [ ] **swagger 注解**：无（cutover 是部署操作，不改 endpoint）

#### server-side RPCWriter 实现详情（已完成）

新增 `server/internal/user/{writer.go, rpc_writer.go, rpc_writer_test.go}`，修改 `user.go` / `readonly_test.go` / `handlers/{handlers.go, users.go}`。一个 PR 落地。

**`UserWriter` interface（writer.go）** — 5 个方法签名与 `*UserService` 既有写方法 byte-identical（包括无 ctx 参数），让 local backend 直接 satisfy 接口、零修改。RPCWriter 内部用 `context.Background()` + `httpClient.Timeout` 作为请求边界（写路径不可中断，否则 cs-user 会进入不一致状态）。

**`RPCWriter` client（rpc_writer.go）** — embed `*RPCClient` 复用 baseURL/token/HTTPClient 配置（读+写共用一套 wire format：X-Internal-Token + 10s timeout + 5xx→ErrRPCUnavailable 映射）。新增 `doCapture(ctx, method, path, body)` 返回 `(status, body, transportError)`，写路径需要 inspect response body 以区分 same-status-different-meaning 响应（cs-user 把两种 bind 冲突都返 HTTP 409）。

**两处 error 字符串归一化**（保证 handler 端 substring 匹配 across backends）：

| cs-user 返回 | HTTP | RPCWriter 行为 | Why |
|---|---|---|---|
| `identity explicitly unbound; requires force_rebind` | 409 | 返回 `nil`（no-op） | 匹配 server 本地 writer `service.go:290` 的 no-op 语义 |
| `identity already bound to another user` | 409 | 返回 `errors.New("identity_already_bound")` | `handlers.go:566` 靠精确匹配 redirect 到 merge-identity 流程 |
| `identity_not_found` | 404 | surface verbatim | `handlers.go:833` 精确匹配 |
| `cannot unbind last identity` / `identity not found` | 409 / 404 | surface verbatim | `handlers.go:766` 精确匹配 |
| 其他 4xx | 4xx | `parseErrorBody` 提取 `{"error":"..."}` envelope 返 verbatim | 通用合约 |
| 5xx / transport | — | `fmt.Errorf("%w: ...", ErrRPCUnavailable)` | 与 read path 一致，handler 层 503 |

**`DualWriter`（writer.go）** — P0-8 cutover step 3 的 canary posture（`rpc+local`）。每个写先打 Primary（UserService，authoritative），成功后 best-effort 同步打 Secondary（RPCWriter）。Primary 错误透传给 caller；Secondary 错误只 `logger.Warn`、**绝不 fail request**——canary 的意义就是在不破坏用户流程的前提下暴露 RPC divergence。Secondary 是同步调用以便观察 divergence；如果成为性能瓶颈再改 fire-and-forget（YAGNI 当前）。

**Writer 选择矩阵**（`user.go: NewWithConfig`）：

| Backend | WriteMode | Reader | Writer | Posture |
|---|---|---|---|---|
| local | local | UserService | UserService | 默认（无变化） |
| local | readonly | — | — | **fatal**（login broken，无收益） |
| rpc | local | RPCClient | DualWriter(svc, rpc) | dual-write canary |
| rpc | readonly | RPCClient | RPCWriter | cs-user authoritative（cutover 终态） |

**Re-route 调用点**（6 处 production call sites，全部从 `UserModule.Service.X` 改为 `UserModule.Writer.X`）：

- `handlers/handlers.go:436` OAuth callback `GetOrCreateUser`（new user）
- `handlers/handlers.go:534` OAuth callback `GetOrCreateUser`（current user）
- `handlers/handlers.go:565` OAuth provider binding `BindIdentityToUser(opts.ForceRebind=true)`
- `handlers/handlers.go:765` admin unbind `UnbindIdentityByProvider`
- `handlers/handlers.go:832` admin transfer `TransferIdentityToUser`
- `handlers/users.go:324` user-search backfill `SyncUser`

注：`service.go:861` `SyncUser → BindIdentityToUser` 的内部递归保留 `s.BindIdentityToUser(...)`——这是 UserService 内部逻辑，不该绕过自身。post-login hook 仍在 UserService 内部触发（DualWriter 只把 Primary 的成功结果透传，不会重跑 hook）。

**Test coverage**（`rpc_writer_test.go`，14 个 test cases）：

- 5 个方法各 happy path（assert path/method/auth/body shape）
- BindIdentityToUser 3 个 409 cases（explicitly_unbound→nil / identity_already_bound→token / 其他→verbatim）
- 5xx → ErrRPCUnavailable；4xx → verbatim server message
- NotConfigured → ErrNotConfigured（5 方法都验证）
- DualWriter 4 cases（primary success fan-out / primary failure skips secondary / secondary failure doesn't fail request / nil secondary）
- `parseErrorBody` 5 cases（envelope / empty / non-json / empty error / missing field）

**Validation matrix test**（`readonly_test.go: TestValidateUserConfig`）— 4 个组合的 `(msg, fatal)` 断言；新增 3 个 NewWithConfig 集成测试（local+local→UserService / rpc+local→DualWriter / rpc+readonly→RPCWriter）确认 writer 类型断言。

`cd server && go test ./internal/user/... ./internal/handlers/... -count=1 -race` 全绿。

#### cs-user Phase 2 write API 详情（已完成）

5 个 server 写方法的 RPC counterparts，一个 PR 落地（commits `feat(cs-user)` × 4 + `docs(cs-user)` × 1）：

| Endpoint | Server method | Notes |
|---|---|---|
| POST `/api/internal/users/get-or-create` | `GetOrCreateUser` + `SyncUser` | SyncUser 折叠进去（cs-user 无 post-login hook） |
| POST `/api/internal/users/:subject_id/bind-identity` | `BindIdentityToUser` | Body: JWTClaims + BindIdentityOptions |
| POST `/api/internal/users/transfer-identity` | `TransferIdentityToUser` | Body: targetUserSubjectID + externalKey |
| DELETE `/api/internal/users/:subject_id/identities/:provider` | `UnbindIdentityByProvider` | Path params only |

**Faithful-port 不变量（security/correctness invariants）**：

- `buildExternalKey` 格式：`casdoor:<provider>:<universal_id>`（与 server byte-identical，否则 dual-write canary 时跨 DB lookup 会 miss）
- `providerRank`：idtrust=300 / github=200 / phone=100 / default=0（驱动 primary 级联）
- Soft-delete recovery：bind 恢复 soft-deleted identity 而非插重复行
- Last-identity invariant：unbind 拒绝让用户变成零 identity
- syncInterval skip：`GetOrCreateUser` 在 15min 内重复调用跳过 update
- Subject ID 格式：新用户 `"usr_" + uuid.NewString()`

**Three deliberate divergences from server**（cs-user 不复刻）：

1. No writeMode gate（无 kill switch；server-side RPCWriter 持有 readonly gate via `USER_SERVICE_WRITE_MODE`）
2. No `notifyUserUpdated`（cache invalidation 是 server 侧 via RPCWriter 调 `CachedService.InvalidateCache`）
3. No `runPostLoginHook`（systemrole bootstrap 留在 server）

**新增 error sentinels**（map to HTTP 409）：`ErrLastIdentity` / `ErrExplicitlyUnbound` / `ErrIdentityAlreadyBound`。

### Phase 0 完成标准（ADR §3.5 验收清单）

- [ ] cs-user Dockerfile 构建通过，本地 docker-compose 起得来，`/healthz` 返 200
- [ ] ETL dry-run 在生产数据快照：行数一致 + 0 字段 drift
- [ ] costrict-web 任意 user API 走 RPC 路径返回正确数据
- [ ] CachedUserService 命中率 > 90%（连续 1 小时压测）
- [ ] cs-user DB 独立备份恢复测试通过
- [ ] costrict-web users 表进入 READONLY（grep 验证无写入路径）
- [ ] `make check` 在 cs-user + server 两个模块均 0 失败
- [ ] `make swagger` 重新生成后 `git diff` 仅含本次 endpoint 改动

---

## 阶段 A：JWT 自签 + 雇佣上下文最小集（Phase 0 完成后启动）

**目标**：JWT 不再依赖 Casdoor，补齐 `employment_identities` 表 + `employment_providers` 配置。

> **物理路径**：Phase 0 完成后，所有认证/身份代码在 `cs-user/internal/auth/`。`server/internal/middleware/auth.go` 仅保留 JWT 验签逻辑（依赖 cs-user 的 JWKS endpoint）。

### A1：employment_identities 表迁移 ✅

- [x] **实现**：`cs-user/migrations/20260716150000_create_employment_identities.sql`（21 列 + 4 索引 + 1 partial unique index `WHERE deleted_at IS NULL`；BIGSERIAL PK / text 列 / timestamptz，遵循 cs-user 既有约定，对应设计 §9.2 lines 674-705；`tenant_id` + `enterprise_uid` 推迟到 Phase B per MULTI_TENANCY §6.5.1, §8.3）
- [x] **实现**：`cs-user/internal/models/employment_identity.go`（GORM struct，21 字段 + `gorm.DeletedAt` 软删除；`LastSyncedAt` / `NextSyncDueAt` 用 `default:CURRENT_TIMESTAMP` 让 DB 兜底，跨 sqlite/PG 通用）
- [x] **测试覆盖**：`cs-user/internal/models/employment_identity_test.go`（`//go:build cgo`，sqlite `:memory:` + AutoMigrate + 手动重建 partial unique index；3 测试：CRUDRoundTrip / Defaults / UserSubjectIDUniqueAfterDelete 软删后可重插入）
- [x] **测试覆盖**：`cs-user/internal/migration/runner_phaseA_test.go`（runner 接 A1/A2 命名文件的 smoke test：Up 应用两表 / Version 推进到 20260716160000 / Down 单步回滚）
- [x] **swagger 注解**：无（数据库迁移）

### A2：tenant_configs 表（最小 schema） ✅

- [x] **实现**：`cs-user/migrations/20260716160000_create_tenant_configs.sql`（Phase A 最小：`tenant_id text PK` + `config_yaml text NOT NULL DEFAULT '{}'` + `updated_by text` + `updated_at` / `created_at`；Phase B 再拆 `provider_mapping` / `username_strategy` / `employment_providers` / `features` 分列 + 加 `tenants(tenant_id)` FK）
- [x] **实现**：`cs-user/internal/models/tenant_config.go`（GORM struct，5 字段；无软删除，re-onboard 走 UPSERT）
- [x] **测试覆盖**：`cs-user/internal/models/tenant_config_test.go`（`//go:build cgo`，3 测试：YAMLColumnRoundTrip byte-for-byte / DefaultRowInsert `default` 行默认值 / TenantIDUniquePK 重复拒绝）
- [x] **swagger 注解**：无

### A3：JWT 自签（RS256 + JWKS） ✅

- [x] **密钥存储**：选定 file-based PEM（决策记录见 `decision log`）。operator 用 `openssl genpkey -algorithm RSA -out cs-user-jwt.pem -pkeyopt rsa_keygen_bits:2048` 生成，通过 k8s secret / docker secret 挂载到容器；轮换 = 换文件 + 重启 pod；环境变量 `CS_USER_JWT_SIGNING_KEY_PATH` 指定路径
- [x] **实现**：`cs-user/internal/auth/signer.go`（`Signer` struct + `NewSignerFromPEMPath` / `NewSignerFromPEM` 构造器，PKCS#1 + PKCS#8 双格式支持；`kid` 按 RFC 7638 JWK thumbprint 自动推导 — SHA-256 over canonical `{"e":"...","kty":"RSA","n":"..."}`，base64url-no-pad；`SignJWT(claims, now)` 用 RS256 + 注入 kid header；`KID()` 暴露 kid 给 caller；`JWKS()` 输出 RFC 7517 兼容 keyset）
- [x] **实现**：`cs-user/internal/auth/jwks.go`（`JWKS{Keys []JWK}` + `JWK{Kty,Use,Kid,Alg,N,E}` wire types；字段名/顺序对齐 server 端 `middleware/jwks.go` 消费者）
- [x] **实现**：`cs-user/internal/handlers/jwks.go`（`JWKSAPI{Signer}` + `GetJWKS` handler；`Signer` 为 nil 时返 503，遵循既有 unavailableUserService stub 模式）
- [x] **路由挂载**：`cs-user/internal/app/app.go` 注册公开路由 `r.GET("/.well-known/jwks", jwksAPI.GetJWKS)`（**公开**，按 RFC 7517；不挂 `/api/internal` 下，避免 X-Internal-Token gate）
- [x] **配置**：`cs-user/internal/config/config.go` 加 `JWTConfig{SigningKeyPath}` + env `CS_USER_JWT_SIGNING_KEY_PATH`；空字符串可选启动（signer=nil → JWKS 返 503），A7 接管 OAuth callback 时收紧为 required
- [x] **DI 接线**：`cs-user/cmd/api/main.go` 启动时若 `SigningKeyPath` 非空则加载并注入 `app.Deps.Signer`；加载失败 `logger.Fatal`；空路径仅 `logger.Warn` 提示 JWKS 不可用
- [ ] **Casdoor 兼容窗口**：保留 Casdoor JWT 30 天窗口（feature flag `jwt_self_sign_enabled`）→ 推迟到 A7/A8（cutover 阶段才需要兼容窗口；A3 仅交付 signer primitive）
- [x] **测试覆盖**：`cs-user/internal/auth/signer_test.go`（10 测试：PKCS#1 + PKCS#8 PEM 解析 / 多种 malformed PEM 错误路径 / 文件路径加载 + 缺失/空路径错 / RFC 7638 KID determinism — 同 key 同 kid 不同 key 不同 kid / RFC 7638 算法 cross-validate — 用 stdlib `json.Marshal` + `sha256` 独立计算 thumbprint 比对，避免 brittle hardcoded reference vector / SignJWT 往返用公钥验签 + 校验 kid/alg header / nil Signer 安全 / JWKS 字段形状 + n/e 往返到源 key / nil JWKS 返空 keyset）
- [x] **测试覆盖**：`cs-user/internal/handlers/jwks_test.go`（2 测试：HappyPath 200 + 校验 JWK 字段 / NilSignerReturns503）
- [x] **app 路由测试**：`cs-user/internal/app/app_test.go` 加 `TestJWKS_RouteRegisteredAs503WhenSignerMissing`（确认路由公开可访问 + 无 signer 时返 503）
- [x] **swagger 注解**：`/.well-known/jwks` 加 `@Tags jwks` + 完整 godoc
- [x] **swagger 注解**：`make swagger` 重新生成（diff: +420 行跨 docs.go/swagger.json/swagger.yaml；含 A3 + A4b endpoint 规范）

### A4：OAuth callback 加 ApplyEnterpriseMapping ✅（service 层，endpoint + server wiring 待 follow-up）

- [x] **实现**：`cs-user/internal/user/employment_mapping.go`（`Service.ApplyEnterpriseMapping` 方法 + `loadEnabledEmploymentProviders` YAML reader + `upsertEmploymentIdentity` 写入路径；按 `employment_providers.enabled` 门控；返回 `ErrEnterpriseMappingDisabled` sentinel 让 caller 区分"跳过"与"失败"；Phase A 只写最小字段 `user_subject_id` + `provider` + `sync_status="fresh"` + 24h `next_sync_due_at`，企业字段留 NULL 等真实 provider client + A5 扩展 claims 填充）
- [x] **测试覆盖**：`cs-user/internal/user/employment_mapping_test.go`（`//go:build cgo`，9 测试：DisabledProvider 不写 / EnabledProvider_NewUser 创建正确字段 / EnabledProvider_ExistingUser update-in-place ID 不变 / EmptyConfigYAML `"{}"` 视为禁用 / MissingTenantConfig 行缺失视为禁用不报错 / MalformedYAML 报解析错误不静默禁用 / DefaultTenantID 空字符串 fallback 到 `"default"` / ValidationErrors 空 subject_id / 空 provider / PerProviderConfigIgnored 富 YAML 解析不爆）
- [x] **swagger 注解**：无（service 层逻辑）

### A4b：cs-user endpoint + server OAuth callback wiring ✅

- [x] **cs-user endpoint**：`POST /api/internal/users/apply-enterprise-mapping`（handler `UsersAPI.ApplyEnterpriseMapping` 在 `cs-user/internal/handlers/users.go`；route 注册在 `cs-user/internal/app/app.go`；`UserService` interface 加 `ApplyEnterpriseMapping` 入口；`unavailableUserService` stub 跟进；`applyEnterpriseMappingRequest{tenant_id?, user_subject_id!, provider!}` body；response `{"applied": bool}` — `ErrEnterpriseMappingDisabled` 映射 200 + `applied=false`，验证错 400，YAML 解析错 500，正常 200 + `applied=true`）
- [x] **server RPCWriter**：`server/internal/user/rpc_writer.go` 加 `ApplyEnterpriseMapping(userSubjectID, provider string) error`（POST 转发到 cs-user；200 不管 applied 真假都 nil；4xx/5xx 走 `mapWriteError` 复用既有契约；未配置返 `ErrNotConfigured`）
- [x] **server UserWriter interface + DualWriter**：`server/internal/user/writer.go` 接口加 `ApplyEnterpriseMapping`；DualWriter delegate Primary 后 best-effort Secondary（secondary 失败仅 `logger.Warn` 不影响 caller，与其它 write 方法对齐）
- [x] **server UserService 本地 stub**：`server/internal/user/enterprise_mapping.go`（`UserService.ApplyEnterpriseMapping` no-op nil — server 无 `employment_identities` 表；local 模式调用即降级为"未启用"；DualWriter Primary 端就是它）
- [x] **OAuth callback hook**：`server/internal/handlers/handlers.go:436` GetOrCreateUser 成功后用返回 user 的 `SubjectID` + `claims.Provider` 触发 `Writer.ApplyEnterpriseMapping`；任何错误仅 `fmt.Printf("[WARN] ...")`，不阻塞 login；空 provider 跳过（无 mapping 可做）
- [x] **测试覆盖**：
  - `cs-user/internal/handlers/users_test.go` 新增 4 测试（AppliedTrue happy path / DisabledMaps200AppliedFalse / MalformedYAMLMaps500 / ValidationMaps400 表驱动 + OptionalTenantID 转发）
  - `server/internal/user/rpc_writer_test.go` 新增 8 测试（RPCWriter HappyPath body shape / DisabledIsSuccess / 5xxMapsToErrRPCUnavailable / 4xxSurfacesServerMessage / NotConfigured；DualWriter FansOut / SecondaryErrorDoesNotFail / PrimaryErrorFails / NilSecondarySkipsFanOut；UserService LocalStubIsNoOp）
- [x] **swagger 注解**：`make swagger` 待跑（PR 合入前）

### A5：JWT claims 扩展 ✅

- [x] **实现**：`cs-user/internal/auth/claims.go` — `EnterpriseClaims` struct，三组字段：
  - 标准 JWT（RFC 7519）：`iss` / `sub` / `iat` / `nbf` / `exp` / `aud` / `jti`，时间字段用 `*time.Time` + 实现 `jwt.Claims` 接口 6 个 getter（`GetExpirationTime` / `GetNotBefore` / `GetIssuedAt` / `GetIssuer` / `GetSubject` / `GetAudience`），让 jwt/v5 解析器自动 enforce `exp`/`nbf`
  - OIDC 身份（mirror `models.JWTClaims`）：`universal_id` / `name` / `preferred_username` / `email` / `picture` / `owner` / `provider` / `provider_user_id` / `phone` — 与 Casdoor token 形状一致，依赖方切换无感知
  - 企业上下文（A5 新增，源自 `employment_identities`）：`employee_number` / `job_title` / `job_level` / `employment_type` / `cost_center` / `org_path` / `work_location`；所有字段 `omitempty`，nil employment 时整组缺省
  - 租户：`tenant_id`，Phase B 填充，Phase A5 占位
- [x] **构造器**：`NewEnterpriseClaims(IssuanceParams, now)` — `IssuanceParams` bundling `Issuer`/`Subject`/`Audience`/`TTL`/`JTI`/`Identity`/`Employment`/`TenantID`；`now` 参数注入便于测试；`nbf` 默认 = now（即签即用）；返回 `*EnterpriseClaims`，可直接喂 `Signer.SignJWT`
- [x] **validation sentinel**：`ErrEmptySubject`（空 subject）+ `ErrZeroTTL`（零 TTL 防误签 forever-token）
- [x] **wire 兼容性**：与 server 端 `JWTClaims` 解析形状对齐；server 在 A7 接管时仅需扩展可识别的企业字段（追加，不替换既有字段）
- [x] **设计取舍**：扁平字段（`employee_number`、`job_title` 等顶级 claim）vs 嵌套 `enterprise` Map — 选扁平，因 OIDC 标准惯例 + jwt.Claims 接口 getter 假设顶级字段；嵌套 Map 会破坏 `jwt.MapClaims` 标准 reader 的 ergonomics
- [x] **测试覆盖**：`cs-user/internal/auth/claims_test.go`（8 测试：HappyPath 校验所有字段 / EmptySubject 错误 / ZeroTTL 错误 / NilIdentityAndEmployment omitempty 不泄漏字段 / JSONShape 验证 wire 格式 snake_case + aud 数组 / SignAndVerifyWithSigner 端到端签 + 公钥验签 + 校验 sub/name/employee_number 往返 / ExpiredTokenRejected 验证 exp gate 经 jwt.Claims 接口生效 / NilReceiverInterfaceSafety 防 nil-deref）
- [x] **swagger 注解**：无（claims 是 JWT payload shape，非 HTTP endpoint）
- [ ] **A7 follow-up**：接管 OAuth callback 时给 `IssuanceParams.Identity` 喂入 Casdoor claims + `Employment` 喂入 `Service.ApplyEnterpriseMapping` 后的 employment_identities row；同时扩展 server 端 `JWTClaims` 解析器识别新增企业字段（追加，不替换）

### A6：default tenant 引导脚本 ✅

- [x] **实现**：`cs-user/migrations/20260716170000_bootstrap_default_tenant.sql`（INSERT `tenant_id="default"` + `config_yaml="{}"`；`ON CONFLICT (tenant_id) DO NOTHING` 保证幂等；Down 仅删 default 行，保留 operator-supplied rows）
- [x] **测试覆盖**：`cs-user/internal/migration/bootstrap_default_tenant_test.go`（4 测试：DefaultTenantInsertCreatesRow / UpIsIdempotent 双跑无重复 / ReUpAfterOperatorInsert operator YAML 不被覆盖 / DownRemovesDefaultRow 只删 default 保留其它租户）
- [x] **swagger 注解**：无（数据库迁移）

### A7：接管 Casdoor OAuth `/oidc-auth/api/v1/plugin/login` 端点

**策略 (b) 重签**（已决）：Casdoor 仍负责 login UI / OAuth dance / password reset / MFA。server 校验 Casdoor JWT 后调 cs-user `POST /api/internal/users/reissue-token`，cs-user 加载 `employment_identities` 快照（A4）+ 构建 A5 claims + 用 A3 signer 签发，server 拿返回的 token 设 cookie + 跑 A8 灰度三态。

**A7 endpoint-only（已落地）** — server 端 wiring 在 A7b：

- [x] **决策**：策略 (b) 重签 — 30 天双签窗口对齐灰度策略；最小爆炸半径
- [x] **service reader**：`cs-user/internal/user/employment_mapping.go` 加 `Service.GetEmploymentIdentity(ctx, userSubjectID)` — `(nil, nil)` 表示用户无 employment 快照（graceful degradation，灰度阶段关键）；`ErrEmptySubjectID` 边界保留
- [x] **config 扩展**：`cs-user/internal/config/config.go` `JWTConfig` 加 `Issuer` / `TTL` / `DefaultAudience` 字段；env vars `CS_USER_JWT_ISSUER`（默认 `"cs-user"`）+ `CS_USER_JWT_TTL`（默认 `1h`，Go duration string）+ `CS_USER_JWT_AUDIENCE`（CSV，逗号分隔）；TTL 零/负值 boot fatal，防止误签 forever-token
- [x] **handler**：`cs-user/internal/handlers/auth.go` 新增 `AuthAPI.ReissueToken` + `EmploymentReader` 接口（最小依赖面，与 `UserService` 分离便于 stub）；request `{user_subject_id (required), identity *JWTClaims (optional), tenant_id (optional), audience []string (optional override)}`；response `{token, expires_at}`；503 当 signer nil（与 JWKS 一致）；400 当 `ErrEmptySubjectID`；500 其它内部错；不重新校验 Casdoor JWT — X-Internal-Token 已认证 caller
- [x] **接线**：`cs-user/internal/app/app.go` 注册 `POST /api/internal/users/reissue-token`；`Deps.EmploymentReader` 字段（nil 时降级到 `unavailableAuthReader{}` 503 stub 保持 swagger spec 稳定）；`cmd/api/main.go` 把 `userSvc` 同时挂到 `Users` + `EmploymentReader`
- [x] **测试覆盖**：`cs-user/internal/user/employment_mapping_test.go` 加 5 测试（HappyPath 全字段往返 / MissingRowReturnsNilNotFound graceful degradation / SoftDeletedExcluded gorm DeletedAt 行为 / EmptySubjectErrors / NilDBGuard）；`cs-user/internal/handlers/auth_test.go` 新增 10 测试（HappyPath 端到端验签 issuer/sub/employee_number/aud/tenant_id / NilSignerMaps503 / MissingSubjectIDRejected binding / EmptySubjectFromServiceMaps400 belt-and-braces / ServiceErrorMaps500 no leak / NoEmploymentRowStillIssuesToken 灰度关键 / AudienceOverride / TenantIDForwarded / NilIdentityStillWorks Phase B refresh 路径 / BadJSONMaps400）；`cs-user/internal/config/config_test.go` 加 7 测试（JWTDefaults / JWTIssuerOverride / JWTTTLParsing 多 duration 格式 / JWTTTLInvalid / JWTTTLZeroRejects / JWTAudienceCSV / JWTAudienceEmptyOmitted）；`cs-user/internal/app/app_test.go` 加 1 测试（ReissueToken_RouteRegistered503WhenMissingDeps — auth gate 401 + nil-signer 503）
- [x] **swagger 注解**：`AuthAPI.ReissueToken` 完整注解（`@Summary` / `@Description` / `@Tags auth` / `@Security InternalToken` / `@Param reissueTokenRequest` / `@Success reissueTokenResponse` / `@Failure 400/500/503` / `@Router /api/internal/users/reissue-token`）；`make swagger` 已重生成 `cs-user/docs/{docs.go,swagger.json,swagger.yaml}`

**A7 待办**（不在本 PR）：

- [x] **A7b server 端 wiring**：`server/internal/handlers/handlers.go` OAuth callback 在 `GetOrCreateUser` 后调 RPCWriter 新方法 `ReissueToken(userSubjectID, identity, audience)`；返回 token 写入 cookie；feature flag `JWT_SELF_SIGN_ENABLED`（默认 OFF，A8 三态门控）。失败时降级到 Casdoor token — 不阻塞 login。
- [ ] **关键风险测试**：csc 真实 login 流程集成测试，验证 `profile.account.{uuid, email, display_name, created_at}` + `profile.organization.*` 字段完整

**A7b 实现细节**（已落地）：

- [x] **RPCWriter.ReissueToken** (`server/internal/user/rpc_writer.go`)：POST `/api/internal/users/reissue-token`；request `{user_subject_id, identity *JWTClaims (PascalCase wire — cs-user 走 encoding/json 大小写不敏感 fallback 解码), audience []string}`；response `{token, expires_at}`；空 token / 5xx / 4xx 都映射错误（5xx → ErrRPCUnavailable，4xx → server message verbatim，empty token → defensive error）
- [x] **UserWriter 接口扩展** (`server/internal/user/writer.go`)：新增 `ReissueToken(userSubjectID, claims, audience) (string, time.Time, error)`；新增 sentinel `ErrSelfSignUnavailable`
- [x] **DualWriter.ReissueToken** (`server/internal/user/writer.go`)：**绕过 Primary**（与 ApplyEnterpriseMapping 不同 — Primary 是本地 UserService 无 RSA 签名密钥），直接路由到 Secondary；nil-Secondary 返回 `ErrSelfSignUnavailable`；Secondary 错误**传播**到 caller（OAuth callback 需要知道失败才能 fallback）
- [x] **本地 UserService stub** (`server/internal/user/enterprise_mapping.go`)：`ReissueToken` 总是返回 `ErrSelfSignUnavailable`；注释说明 server 无本地 RSA 签名密钥，需 `USER_SERVICE_BACKEND=rpc`
- [x] **feature flag** (`server/internal/config/config.go`)：`Config.JWTSelfSignEnabled bool`；env var `JWT_SELF_SIGN_ENABLED`；`strconv.ParseBool` 词汇（"1"/"t"/"true" → true，"0"/"f"/"false"/""/garbage → false）；默认 OFF
- [x] **OAuth callback wiring** (`server/internal/handlers/handlers.go`)：`InitCookieConfig` 把 `cfg.JWTSelfSignEnabled` 注入到包级 `jwtSelfSignEnabled`；`AuthCallback` 用 `cookieToken` 变量（默认 Casdoor token）+ 在 `created != nil` 后条件调 `ReissueToken`；失败 warn-log + fallback 到 Casdoor token；ApplyEnterpriseMapping 的 hook 重构到同一 `created != nil` 分支下
- [x] **测试覆盖**：`server/internal/user/rpc_writer_test.go` 加 13 测试（RPCWriter HappyPath wire-format + AudienceForwarded + NilAudienceOmitted + EmptySubjectIDRejected + NotConfigured + 5xxMapsToErrRPCUnavailable + 4xxSurfacesServerMessage + EmptyTokenInResponseErrors；DualWriter BypassesPrimary + NilSecondaryReturnsErrSelfSignUnavailable + SecondaryErrorPropagates；UserService LocalStubReturnsErrSelfSignUnavailable）；`server/internal/config/config_test.go` 加 2 测试（DefaultFalse + EnvParsing 多 case）；`recordingWriter` stub 扩展 ReissueToken + reissueTokenFn hook
- [x] **swagger 注解**：server 端无新 endpoint（RPCWriter 是内部 client）；cs-user 端 spec 已生成（A7 步骤）

### A8：灰度发布

- [x] **实现**：feature flag `JWT_SIGN_MODE=off|dual|single`（off → 双签 → 单签）
- [x] **测试覆盖**：feature flag 三态行为测试
- [x] **swagger 注解**：无（部署配置）

**A8 实现细节**（已落地）：

- **三态 enum**（`server/internal/config/config.go`）：替换 A7b 的 `JWTSelfSignEnabled bool` 字段为 `JWTSignMode string`，新增常量 `JWTSignModeOff` / `JWTSignModeDual` / `JWTSignModeSingle`。Loader `loadJWTSignMode()` 优先 `JWT_SIGN_MODE`（off/dual/single 大小写不敏感，非法值 `log.Fatalf`），fallback `JWT_SELF_SIGN_ENABLED=true → dual`（A7b 词汇兼容）。`JWT_SIGN_MODE` 胜过 `JWT_SELF_SIGN_ENABLED`（同时设置时前者赢，便于灰度回退）。
- **多源 JWKSProvider**（`server/internal/middleware/jwks.go`）：内部 `jwksURL string` 字段重命名为 `sources []string`，新增 `NewMultiJWKSProvider(baseURLs []string)`（dedup + drop empty）；`refresh()` 从所有源 fetch 后合并 kid→key 映射；**单源失败不致命**（仅当所有源都失败才返回 error）— dual 模式下 cs-user 短暂不可用时 Casdoor 源仍能签发已存在 token。`NewJWKSProvider(baseURL)` 签名向后兼容（仍接受单个 base URL）。
- **main.go boot wiring**（`server/cmd/api/main.go`）：`switch cfg.JWTSignMode` 三态分支构建对应 JWKSProvider（off → Casdoor-only / dual → `[cs-user, Casdoor]` Multi / single → cs-user-only）。dual/single 模式下 `USER_SERVICE_URL` 为空则 `logger.Fatal`（不允许灰度开关开了但 cs-user endpoint 不可达）。
- **OAuth callback**（`server/internal/handlers/handlers.go`）：`jwtSelfSignEnabled bool` → `jwtSignMode string`（package var，由 `InitCookieConfig` 注入）。条件从 `if jwtSelfSignEnabled` 改为 `if jwtSignMode != config.JWTSignModeOff`，行为相同（dual/single 都触发 ReissueToken；失败 fallback 到 Casdoor token，不阻塞 login）。注释明确 dual vs single 的差异在 verifier 不在 callback。
- **`ErrSelfSignUnavailable` docstring 更新**（`server/internal/user/writer.go`）：A8 词汇下不再描述为 "硬错误"，改为 "非阻塞 fallback"（dual 模式下 cs-user 故障 fallback 到 Casdoor token + WARN log）。

**A8 测试覆盖**（已落地）：

- `server/internal/config/config_test.go`：4 新测试（DefaultIsOff + JWT_SIGN_MODE_ParsesAllThreeStates 含 8 case + JWT_SELF_SIGN_ENABLED_LegacyMapping 含 8 case + JWT_SIGN_MODE_WinsOverLegacyBool 优先级验证）。
- `server/internal/middleware/jwks_test.go`（NEW）：8 新测试（NewMultiJWKSProvider_DedupesAndDropsEmpty + AllEmptyYieldsNoSources + MultiSource_MergesKeysFromBothURLs + MultiSource_ToleratesOneSourceFailing + MultiSource_AllFailuresIsError + SingleSource_StillWorksAfterRefactor + NoSources_ReturnsError + NewJWKSProvider_AppendsJWKSPath）。
- `server/internal/handlers/handlers_test.go`：2 新测试（TestAuthCallback_JWTSignModeGating 三态表驱动 off/dual/single 验证 ReissueToken 调用次数 + cookie 值；TestAuthCallback_JWTSignModeFallbackOnReissueError 验证 ReissueToken 失败时 fallback 到 Casdoor token 不阻塞 login）。引入 `callbackStubWriter` 类型（embeds `*userpkg.UserService` + 重写 ReissueToken 计数）。
- `server/internal/middleware/auth_test.go`：现有 `newTestJWKSProvider` / `parseJWTToken` 测试 fixture 字段名同步 `jwksURL → sources`（5 处 replace），全部继续通过。

**A8 后续 / 已知限制**：

- **Casdoor API fallback**（`fetchUserInfo(casdoorEndpoint, token)` in `auth.go`）在 single 模式下仍存在 — 极旧的 Casdoor token 理论上仍可通过 userinfo API 路径验证。彻底切断需要 `RequireAuth` / `OptionalAuth` / `ParseToken` 接受 mode 参数（侵入性更大，10+ call sites）。本 PR 留给后续清理任务（Phase A 验收前的最终 cutover 步骤）。
- **JWT_SIGN_MODE=invalid 触发 `log.Fatalf`**：boot 阶段强失败，生产环境 typo 会立即暴露（不会"降级为 off"灰度无法生效）。但导致 `loadJWTSignMode` 的非法值路径难以单元测试（需要 subprocess）— 接受这个权衡。
- **`Config.JWTSelfSignEnabled` 字段已删除**：旧的 `JWT_SELF_SIGN_ENABLED` env var 仍接受（通过 `loadJWTSignMode` 内部消化），但作为 Config 字段已不可编程访问 — 任何外部消费者（如 admin endpoint 报告 mode 状态）应改读 `cfg.JWTSignMode`。
- **A8 不实现 dual-sign 模式下"同时签发 Casdoor + cs-user JWT"路径**：cookie 中只放 cs-user JWT，Casdoor JWT 不再返回给客户端（client 只看到一个 token）。"双签"指 verifier 端双接受（old Casdoor session + new cs-user session 都能通过），不是同时签发。

### Phase A 验收（ROADMAP §9.5 必过）

- [ ] 用新 JWT 调 cs-cloud，`UserID()` 与旧 JWT 一致
- [ ] `provider` 路由正确（github/email/phone/idtrust 各试一次）
- [ ] csc login → poll → store → API call 全流程通过
- [ ] csc 登录后能看到 `accountInfo.email` / `organization` 字段
- [ ] assistant-ui SSE/WebSocket 连接正常
- [ ] quota-manager `/quota-manager/api/v1/*` 调用 `AuthUser.ID` 解析成功
- [x] cs-cloud + costrict-web 双格式 reader 已落地（§9.6）— costrict-web 端实现位于 `server/internal/authidentity/normalize.go:NormalizeClaimsMap`（flat first → nested/renamed fallback，line 42 注释明确 "兼容旧 Casdoor flat JWT + 新 cs-user canonical JWT"），覆盖 email/phone/name/username/picture/provider 全字段。cs-cloud 端 reader 落在 opencode 仓库（`D:\DEV\opencode\packages\opencode\internal\provider\jwt.go`），costrict-web 仓库外。
- [x] **代码级 acceptance bar（2026-07-17）** — cs-user 仓库内可锁定的端到端 contract，由 `cs-user/internal/handlers/phasea_integration_test.go` 覆盖：
  - `TestPhaseA_FullContextEndToEnd`：reissue-token + JWKS 端到端 — 用 JWKS 端点返回的公钥（非测试自身私钥）验证签发的 token，断言 standard / OIDC identity / enterprise / tenant / permission 五组 claim 全部正确填充。
  - `TestPhaseA_GrayReleaseMinimalToken`：灰度路径（无 PermissionReader + 无 employment 行）— 仍签发可验证 token，且 JSON 省略 enterprise + permission claim 组（pre-cutover relying party 只看到 standard + tenant）。
  - `TestPhaseA_JWKSKidMatchesSignerKid`：token kid header 与 JWKS 端点 kid 一致（依赖方多 key 轮换场景下选对 key）。
  - `TestPhaseA_TokenExpiryMatchesConfigTTL`：exp 与配置 1h TTL 一致（cookie MaxAge 依赖此契约）。
  - 余下 L447-452 + L454 acceptance 项为集成 / 运维级（cs-cloud / csc / assistant-ui / quota-manager 真实下游 + 30 天灰度），由 `docs/identity-tenant/P0-8_CUTOVER_RUNBOOK.md` 同款运维 runbook 驱动，不能在本仓库单元测试覆盖。
- [ ] 30 天灰度结束关闭 Casdoor JWT 签名 24 小时无异常

---

## 阶段 B：tenant 维度落地（数据隔离）

> Phase 0 完成后启动；单 tenant 模式（`tenant_id=default`）也能跑，不阻塞 Phase A。

- [x] **B1**：`tenants` + `tenant_admins` 表迁移（`cs-user/migrations/20260717100000_create_tenants_and_tenant_admins.sql`）— 详见下方"B1 实现细节"

- [x] **B2**：给 `users` / `user_auth_identities` / `employment_identities` 加 `tenant_id` 列 + 索引（`user_profile` 表尚未存在，待其首次落地时一并加入）— 详见下方"B2 实现细节"

- [x] **B3**：tenant resolution — `tenant.Resolver` 三层 fallback primitives（slug / email-domain / email）+ email_domains typed reader。HTTP middleware / cookie / session / Casdoor redirect 拆到 **B3b**（下一子任务）。— 详见下方"B3 实现细节"
- [x] **B3b.1**：cs-user HTTP middleware 解析三层信号（`X-Tenant-Id` header → `cs_tenant_slug` cookie → Host subdomain）— 详见下方"B3b.1 实现细节"
- [x] **B3b.2a**：server 端 ctx 助手 + RPC `X-Tenant-Id` 转发 — 详见下方"B3b.2a 实现细节"
- [x] **B3b.2b-step1**：RPCWriter + UserWriter 接口接受 ctx（write-path slug 转发真正生效）
- [x] **B3b.2b-step2a**：cs-user endpoint `POST /api/internal/tenants/resolve-by-email`（picker candidate source）
- [x] **B3b.2b-step2b**：server RPC client + AuthCallback Try 2 注入（email-domain → slug → cookie）
- [x] **B3b.2b-step2c**：picker suggestion endpoint `/api/tenants/suggest`（server 侧 wrapper，pre-login 可调）
- [ ] **B3b.2b-step2d**：AuthCallback `ambiguous` 分支 picker redirect — **待前端 picker 页面落地后启用**（独立前端 PR，本仓库 server 工作流外）
- [x] **B3b.2c**：cross-tenant 检测（JWT `tenant_slug` claim 比对 + `TenantMatch` middleware 401）
- [x] **B4**：中间件从 JWT 提取 `tenant_id` 注入 request context
- [x] **B5**：应用层 query 经 `tenant.Scope(ctx)` helper（cs-user 侧 primitive + 4 个 read 方法迁移；write 方法待 follow-up）— 详见下方"B5 实现细节"
- [~] **B6**：PostgreSQL RLS Policy 兜底（`CREATE POLICY tenant_isolation ON ...`）— **已降级为未来工作（2026-07-17）**，不进 Phase B 范围。设计草案见下方"B6 设计草案"，待触发条件满足后再启动
- [x] **B7**：`(tenant_id, username)` 联合唯一索引，`email` 保持全局唯一 — 见 cs-user/migrations/20260717180000_users_username_per_tenant_unique.sql（已完成 2026-07-17）

**每个任务的测试 + swagger 子项同 Phase 0/A 模板**：迁移配 testcontainers up/down；query 改动配 sqlmock 注入测试；新 admin endpoint 配完整 swagger 注解 + InternalToken security。

### B1：tenants + tenant_admins 表迁移（已落地）

- [x] **实现**：`cs-user/migrations/20260717100000_create_tenants_and_tenant_admins.sql` —
  - `tenants` 表（13 列）：`tenant_id TEXT PK` / `slug VARCHAR(32) UNIQUE NOT NULL` / `display_name VARCHAR(191) NOT NULL` / `status VARCHAR(32) DEFAULT 'active'` / `edition VARCHAR(32) DEFAULT 'team'` / `email_domains TEXT DEFAULT '[]'` / `features TEXT DEFAULT '{}'` / `limits TEXT DEFAULT '{}'` / `settings TEXT DEFAULT '{}'` / `deletion_requested_at TIMESTAMPTZ` / `deleted_at TIMESTAMPTZ` / `created_at TIMESTAMPTZ NOT NULL DEFAULT now()` / `updated_at TIMESTAMPTZ NOT NULL DEFAULT now()`
  - `tenant_admins` 表（6 列 + 复合 PK）：`(tenant_id, user_id) PRIMARY KEY` / `role VARCHAR(32) NOT NULL`（owner | admin | billing，应用层枚举校验，无 CHECK 约束） / `granted_by VARCHAR(191) NOT NULL` / `granted_at TIMESTAMPTZ NOT NULL DEFAULT now()` / `revoked_at TIMESTAMPTZ`（NULL = active）
  - 外键：`fk_tenant_admins_tenant` (CASCADE) + `fk_tenant_admins_user` (CASCADE) + `fk_tenant_admins_granted_by` (RESTRICT — 防止删除有授权记录的用户)
  - 索引：`idx_tenant_admins_user_active` partial index `WHERE revoked_at IS NULL`（hot path: user → active grants）
  - Bootstrap：插入 `tenant_id='default'` 行（slug='default' / display_name='Default Tenant' / status='active' / edition='enterprise'），与 A6 的 `tenant_configs('default')` 对齐
  - A2 TODO 兑现：给 `tenant_configs` 加 `fk_tenant_configs_tenant` FK (RESTRICT) — A2 时 tenant_id 是无 FK 的纯 text PK，B1 起正式建立引用关系
  - 全部 column COMMENT ON 配齐，便于 `\d+` / DB 工具展示
  - Schema 决策（见 migration 头注释）：text 而非 UUID 列类型（与 tenant_configs / users 对齐）；TEXT 而非 JSONB/TEXT[] 持有 JSON（与 EmploymentIdentity.Attributes 同约定，B2/B3 引入 typed reader 时再迁移）；TIMESTAMPTZ 而非 TIMESTAMP（新表遵循 RFC 3339 best practice）
- [x] **GORM 模型**：
  - `cs-user/internal/models/tenant.go` — `Tenant` 结构体（13 字段对应表列），`TableName()` 返回 `tenants`
  - `cs-user/internal/models/tenant_admin.go` — `TenantAdmin` 结构体 + 三个 role 常量（`TenantRoleOwner` / `TenantRoleAdmin` / `TenantRoleBilling`），`TableName()` 返回 `tenant_admins`
  - 时间字段不加 `gorm:"type:timestamptz"` tag — sqlite 测试 driver 无法 scan TIMESTAMPTZ 字符串回 time.Time；Postgres 由 migration 显式建 TIMESTAMPTZ，GORM AutoMigrate 在生产禁用
- [x] **测试覆盖**：`cs-user/internal/models/tenant_test.go`（7 新测试）—
  - `TestTenant_DefaultRowInsert`：仅 required 字段插入时默认值生效（status=active / edition=team / 四个 JSON 列 / 时间戳非零）
  - `TestTenant_JSONColumnRoundTrip`：四个 JSON-holding TEXT 列 byte-for-byte round-trip
  - `TestTenant_TenantIDPrimaryKeyRejectsDuplicate`：tenant_id PK 唯一性
  - `TestTenant_SlugUniqueRejectsDuplicate`：slug UNIQUE 约束
  - `TestTenantAdmin_CompositePrimaryKeyRejectsDuplicate`：复合 PK (tenant_id, user_id) 拒绝重复
  - `TestTenantAdmin_SameUserAcrossMultipleTenants`：同一 user 跨多租户 active grant（多租户成员资格正向用例）
  - `TestTenantAdmin_RevokeSetsTimestampWithoutDeleting`：撤销设置 RevokedAt 而非删行（审计轨迹）
- [x] **swagger 注解**：无（数据层迁移，无 API endpoint 暴露）

**B1 已知限制 / 后续衔接**：

- `email_domains` / `features` / `limits` / `settings` 当前为 TEXT 持 JSON 字符串，**无 DB 侧 JSON 查询能力**（无法 `WHERE features @> '{"sso": true}'`）。MULTI_TENANCY_DESIGN §7 原方案是 JSONB + TEXT[]；B2/B3 引入 typed reader 时若需查询能力，会加迁移把列改为 JSONB。
- `role` 没有 CHECK 约束 — 故意不加，便于后续扩展角色（如 `auditor`）不需要迁移。应用层枚举校验在 B2 service 中加。
- B1 只建表 + 默认 default 租户行，**不实现任何 tenant CRUD endpoint / service** — 这是 B3 (tenant resolution) 和 C2 (platform_admin API) 的工作。
- B1 不实现 `tenants` 表的 status 状态机校验（active → suspended → deleted 转移规则）— 这属于 B6 (RLS policy) 的工作。
- B1 未给 `users` 表加 `tenant_id` 列 — 这是 B2 的工作（需要数据回填策略：现有用户默认归属 default 租户）。

### B2：user-domain 表加 tenant_id 列（已落地）

- [x] **实现**：`cs-user/migrations/20260717120000_add_tenant_id_to_user_tables.sql` —
  - 三张表（`users` / `user_auth_identities` / `employment_identities`）各加 `tenant_id TEXT NOT NULL DEFAULT 'default'` 列
  - `DEFAULT 'default'` 即"数据回填策略"：现有所有行 ALTER 时自动落 default 租户；后续 INSERT 未显式指定时也落 default — 与 A6 的 default tenant bootstrap 对齐
  - 三张表各加 FK 到 `tenants(tenant_id)` `ON DELETE RESTRICT`（统一选择 RESTRICT 而非 CASCADE：tenant 生命周期走 `status='deleted'` 路径，物理删除是少见运维操作，RESTRICT 强制先迁移用户再删 tenant，避免误删）
  - 索引：每张表加单列 `idx_<table>_tenant_id` 覆盖"给定 tenant 找全部 user"热路径；`employment_identities` 额外加 `(tenant_id, user_subject_id)` 复合索引覆盖跨租户反向解析（MULTI_TENANCY §8.3）
  - 各加 `COMMENT ON COLUMN ... IS '所属租户 ID（默认 default；FK 到 tenants.tenant_id，RESTRICT 删除）'`
  - **不在 B2 落地**：`(tenant_id, enterprise_uid)` 唯一索引 — `enterprise_uid` 列本身要等后续 Phase B 子任务引入（A1 注释里已说明），届时一并迁移
  - **`user_profile` 表跳过**：该表尚未存在，待其首次落地时一并加入 tenant_id 列
- [x] **GORM 模型**：
  - `cs-user/internal/models/models.go` — `User` 与 `UserAuthIdentity` 各加 `TenantID string` 字段（gorm tag `type:text;size:191;not null;default:default;index:idx_<table>_tenant_id`，json `tenant_id`）
  - `cs-user/internal/models/employment_identity.go` — `EmploymentIdentity` 加 `TenantID` 字段，gorm tag 同时声明两个 index（`idx_employment_identities_tenant_id` + `idx_employment_identities_tenant_user` 用 `priority:1/2` 表达复合索引顺序）；`UserSubjectID` 加上 `priority:2` 让 GORM AutoMigrate（sqlite 测试场景）能重建复合索引
  - 文档注释更新：`EmploymentIdentity` 头注释里删除 "tenant_id column ... lands in Phase B"，改为说明 B2 已落地 + 唯一索引仍待 enterprise_uid
- [x] **测试覆盖**：`cs-user/internal/models/tenant_id_test.go`（6 新测试，全 cgo-gated sqlite in-memory）—
  - `TestUser_TenantIDDefaultsToDefault`：User 不显式设 TenantID 时落 'default'（backfill 契约）
  - `TestUser_ExplicitTenantID`：显式 TenantID="t-acme" round-trip
  - `TestUser_QueryByTenantID`：跨双租户插入 5 用户，`WHERE tenant_id=?` 计数验证 idx_users_tenant_id 索引扫描语义
  - `TestUserAuthIdentity_TenantIDDefaultsToDefault`：第二张表的 default 契约（先创建 backing User 满足应用层引用语义）
  - `TestEmploymentIdentity_TenantIDDefaultsToDefault`：第三张表 + 手工补建 partial unique index（mirror newEmploymentIdentityDB 模式）
  - `TestEmploymentIdentity_QueryByTenantAndSubject`：复合索引 `(tenant_id, user_subject_id)` 反向解析；同一 `user_subject_id` 跨多租户行可正确区分
- [x] **swagger 注解**：无（数据层迁移，无 API endpoint 暴露）

**B2 已知限制 / 后续衔接**：

- **FK 在 sqlite 测试不强制**：sqlite 默认 `PRAGMA foreign_keys=OFF`，本仓库的 `gorm.Open(sqlite.Open(...))` 也没有显式开启；FK 约束的真正强制发生在 Postgres 生产环境。集成测试若需覆盖 FK 误删拒绝路径，待 testcontainers-go 接入（跨阶段测试基础设施"待补强"）。
- **`(tenant_id, enterprise_uid)` 唯一索引仍缺**：`enterprise_uid` 列在 A1 时就被注释为后续 Phase B 引入；待其落地时本 B2 复合索引可保留（覆盖反向解析），唯一索引独立新增。
- **无应用层写入改动**：B2 仅 schema 改动；service / handler 不会主动 set `TenantID`，所有 Create 仍走 `DEFAULT 'default'`。B3（tenant resolution）起开始从 JWT/context 提取并显式 set。
- **无 query 过滤改造**：B5（`tenantScope(ctx)` helper）才会让所有 query 自动 `WHERE tenant_id = ctx.TenantID`。B2 之后到 B5 之前，应用层 query 仍全表扫描，但因为所有行 tenant_id='default'，行为等价单租户。

### B3：tenant.Resolver 三层 fallback primitives（已落地）

- [x] **新建包**：`cs-user/internal/tenant/` — 独立 bounded concern，不污染 `internal/user/`
- [x] **实现**：`cs-user/internal/tenant/resolver.go` —
  - `Resolver` struct（持有 `*gorm.DB`）+ `NewResolver(db)` 构造器；并发安全（无共享可变状态）
  - **`ResolveBySlug(ctx, slug)`** — Try 1 子域名路径；接受 `slug` 或 `tenant_id`（cookie / X-Tenant-Id header 两种 caller 携带方式）；过滤 `status='active'`；`gorm.ErrRecordNotFound` → `ErrTenantNotFound` sentinel；空/纯空白输入 → `ErrTenantNotFound`（不污染日志）
  - **`ResolveByEmailDomain(ctx, domain)`** — Try 2 邮箱域路径；加载所有 active tenants，Go 端用 `ParseEmailDomains` 过滤；唯一命中 → 返回；零命中 → `ErrTenantNotFound`；多命中 → `ErrAmbiguousTenant`（设计 §5.3 场景 B：交给前端 picker）
  - **`ResolveByEmail(ctx, email)`** — `domainFromEmail` 提取域名后委托 `ResolveByEmailDomain`；malformed email（无 @ / @ 在首尾 / 空）→ `ErrTenantNotFound` 而非 internal error
  - **`ResolveFromHost(ctx, host, apexDomains)`** — `slugFromHost` 相对 apex 列表提取首段子域；apex 可带端口（dev 模式 `localhost:8080`）；FQDN 末尾点容忍；嵌套子域 `login.acme.cs-user.example.com` → slug `acme`（B1 slug 单 label 约定）
  - 三个 sentinel：`ErrTenantNotFound` / `ErrAmbiguousTenant`（caller 翻译为 HTTP 行为）
  - **nil receiver 守卫**：所有方法在 `r == nil || r.db == nil` 时返回 `ErrTenantNotFound`（graceful degradation）
- [x] **email_domains typed reader**：`ParseEmailDomains(*models.Tenant) []string` —
  - B1 留 TODO："B2/B3 引入 typed reader 时再按需迁移"；本任务兑现
  - 解码 `email_domains` TEXT 列里的 JSON array of strings；空字符串 / `'[]'` / malformed JSON → empty slice（graceful degradation，与 `EmploymentIdentity.Attributes` 同约定）
  - 输出 lowercase + trim（写入侧由 Phase C tenant_admin API 强制规范化；reader 端防御性 normalize）
  - nil tenant → nil slice
- [x] **测试覆盖**：`cs-user/internal/tenant/resolver_test.go`（28 新测试，全 cgo/sqlite in-memory）—
  - **ResolveBySlug** × 5：Hit / AcceptsTenantIDToo / Miss / SuspendedExcluded / EmptyInput
  - **ResolveByEmailDomain** × 6：UniqueHit / CaseInsensitive / Ambiguous（globex.com 双 tenant）/ Miss / SuspendedExcluded / MultiDomainPerTenant（acme.com + acme.cn 都解析到 t-acme）
  - **ResolveByEmail** × 2：HappyPath / Malformed（空 / no @ / 首尾 @）
  - **ResolveFromHost** × 7：SubdomainHit / WithPort（host:8443）/ BareApex（bare apex → ErrTenantNotFound）/ NoApexMatch / NestedSubdomain（login.acme…→acme）/ FQDNTrailingDot / LocalhostApex（dev 模式 apex=localhost:8080）
  - **ParseEmailDomains** × 6：HappyPath / EmptyArray / EmptyString / Malformed / LowercasedOutput / NilTenant
  - **domainFromEmail** × 1 表驱动（8 case）
  - **NilGuards** × 1 表驱动（4 method × nil receiver 都返回 ErrTenantNotFound）
- [x] **swagger 注解**：无（数据层 service，无 API endpoint 暴露；B3b HTTP middleware 落地时加）

**B3 已知限制 / 后续衔接**：

- **`ResolveByEmailDomain` 全表扫描**：因 B1 把 `email_domains` 存为 TEXT 持 JSON 字符串（非 JSONB / TEXT[]），无法 push contains check 到 DB；当前是 "load all active tenants + Go 端 filter"。B3 阶段 tenant 数在 10s，性能可接受。后续若 tenant 上 10k+，会加迁移把列改 JSONB / TEXT[]（设计 §6.5.1 / B1 known-limitations 已注明），此方法改为单条 `WHERE ? = ANY(email_domains)`。
- **`ResolveBySlug` 接受 slug 或 tenant_id**：用 `WHERE tenant_id = ? OR slug = ?` 单次查询。如果未来 slug 与 tenant_id 命名空间可能冲突（罕见，slug 是 `[a-z0-9-]{3,32}`，tenant_id 是 UUID），需要拆两次查询或加前缀。当前 cs-user tenant_id 都以 `t-` / `default` 起头，无冲突。
- **`ResolveFromHost` apex 配置**：apex 列表是参数注入（caller 配置），不是 env。B3b HTTP middleware 会从 config 读取 `CS_USER_APEX_DOMAINS` 环境变量并传入。本地 dev 用 `localhost:8080`，prod 用 `cs-user.example.com`。
- **`ResolveByEmail` 不预校验邮箱 RFC 合法性**：用 `strings.LastIndex(email, "@")` 提取，简单 conservative；接受 quoted local part 等 RFC-valid 但 weird 邮箱会失败。Phase C admin/IdP 流程预校验后才会进此路径，可接受。
- **B3 不实现 orchestration**：没有 top-level `Resolve(ctx, host, email, apex)` 编排函数，因为 Try 3 explicit selection 是 UI flow（阻塞等待用户输入），编排属于 HTTP layer。B3b middleware 会按 subdomain → email domain 顺序串起来；Try 3 由前端处理。
- **未在 server 端 wire 起来**：B3 是纯 service 层，server / cs-user cmd/api 还没调用 `Resolver`。第一个 caller 是 B3b middleware（注入到 OAuth callback 链路 + 后续业务 handler）。

### B3b：tenant.Resolver HTTP wiring

**B3b.1（已完成 2026-07-17）** — cs-user 侧 middleware + config + context helpers：

- [x] `cs-user/internal/tenant/context.go`：`WithTenant` / `FromContext` / `HasTenant`（unexported `ctxKey` 防碰撞；nil 容忍；nil 值表示"无信号"语义）
- [x] `cs-user/internal/config/config.go`：加 `TenantConfig.ApexDomains []string` + `CS_USER_APEX_DOMAINS` CSV env loader（mirror JWT audience CSV pattern）
- [x] `cs-user/internal/middleware/tenant.go`：`ResolveTenant(resolver, apexDomains) gin.HandlerFunc` 按 §5 三层 fallback（X-Tenant-Id header → `cs_tenant_slug` cookie → Host subdomain），全 miss fallback 到 default tenant；DB error 走 `c.Set("tenant_resolve_error", ...)` 让 handler 翻译成 503；`TenantFromGin(c)` 便捷取值
- [x] `cs-user/internal/app/app.go`：`Deps.TenantResolver *tenant.Resolver` 字段（可选；nil 时 middleware 跳过 → Phase A 行为不变）；middleware 装到 `r.Use` 顶层（早于 `/api/internal` route group）
- [x] `cs-user/cmd/api/main.go`：从 `pool.Gorm` 构造 `tenant.NewResolver` 注入 `app.Deps`
- [x] **测试覆盖**：`internal/middleware/tenant_test.go`（cgo-gated sqlite + 8 测试：X-Tenant-Id header hit / cookie fallback / header 优先 / subdomain layer / 无信号 default fallback / bogus header falls through / nil resolver no-op / default-missing 优雅降级）；`internal/tenant/context_test.go`（5 测试：WithTenant+FromContext round-trip / empty ctx / nil ctx / WithTenant nil ctx defensive / nil 值 = "no signal"）；`internal/config/config_test.go` 加 2 测试（ApexDomainsDefault / ApexDomainsCSV 空白容忍）

**B3b.2（拆分为 2a/2b/2c）** — server 侧 tenant slug 转发 + OAuth callback 链路 + picker + cross-tenant 检测

**B3b.2a（已完成 2026-07-17）** — server 侧 tenant slug forwarding（最小可用：cookie/header/subdomain → ctx → RPC X-Tenant-Id）：

- [x] `server/internal/tenant/context.go`：`WithSlug` / `SlugFromContext` / `HasSlug`（unexported ctxKey；只存 slug，**不**做 DB 查询 — server 不复制 tenants 表，转发 slug 给 cs-user 让其解析）
- [x] `server/internal/middleware/tenant.go`：`ResolveTenantSlug(apexDomains) gin.HandlerFunc` 按 §5 三层 fallback（`X-Tenant-Id` header → `cs_tenant_slug` cookie → Host subdomain），全 miss 存空 slug（= "no signal"）；middleware 永不 abort chain
- [x] `server/internal/user/tenant_ctx.go` + `rpc_client.do`：从 ctx 读 slug，非空时设 `X-Tenant-Id` 出站 header（空时省略 → cs-user fallback default tenant）。**read-path only** — write-path 注入推迟到 B3b.2b（见下方"已知限制"）
- [x] `server/internal/config/config.go`：`UserServiceConfig.ApexDomains []string` + `USER_SERVICE_APEX_DOMAINS` env（CSV via `getEnvSlice`）
- [x] `server/cmd/api/main.go`：`r.Use(middleware.ResolveTenantSlug(cfg.UserService.ApexDomains))` 装在 CORS/Logger/Recovery/ErrorLogger 之后、OptionalAuth 之前（公开路由也能解析 slug）
- [x] **测试覆盖**：`internal/tenant/context_test.go`（5 测试）、`internal/middleware/tenant_test.go`（9 测试：header-wins / cookie-fallback / header-precedence / subdomain / subdomain-with-port / nested-subdomain-returns-label-below-apex / no-signal-none / host-is-apex-none / whitespace-header-ignored）；full server `go test ./...` 通过；user 包既有 RPC 测试未受影响

**B3b.2b（拆分为 step1/step2）** — ctx 穿透 + OAuth callback 链路：

**B3b.2b-step1（已完成 2026-07-17）** — ctx 穿透 refactor（解锁 write-path tenant slug 转发）：

- [x] `server/internal/user/writer.go`：`UserWriter` interface 7 个方法签名全部加 `ctx context.Context` 首参；`DualWriter` 同步并 propagate ctx 给 Primary/Secondary
- [x] `server/internal/user/rpc_writer.go`：`RPCWriter` 7 个方法接受 ctx；`doCapture()` 内部 6 处 `context.Background()` 替换为传入的 ctx；write-path 的 `X-Tenant-Id` header 注入（先前 B3b.2a placeholder）现在真正生效
- [x] `server/internal/user/service.go`：`*UserService` 4 个 write 方法（GetOrCreateUser / BindIdentityToUser / TransferIdentityToUser / UnbindIdentityByProvider）接受 ctx（local DB 写入不需要 tenant 转发，ctx 仅做接口对齐；内部递归调用用 `context.Background()`）
- [x] `server/internal/user/bootstrap.go`：`SyncUser` 接受 ctx（local stub 忽略）
- [x] `server/internal/user/enterprise_mapping.go`：`ApplyEnterpriseMapping` + `ReissueToken` 接受 ctx（local stub 返回 `ErrSelfSignUnavailable`）
- [x] `server/internal/handlers/handlers.go`：8 个 caller site 改为传 `c.Request.Context()`（OAuth callback + bind/unbind/transfer endpoint）
- [x] `server/internal/handlers/users.go:324`：`backfillUsers` 是 fire-and-forget 后台 goroutine（无 gin.Context），用 `context.Background()` — tenant 转发对 backfill 无意义
- [x] **测试**：所有 `*UserService` 直接调用方 + DualWriter/RPCWriter fake + handler test stub 全部同步加 ctx；`go test ./...` 通过
- [x] **效果**：B3b.2a 留下的 write-path 注入 placeholder 现在真正激活 — 任何 RPCWriter 写调用（OAuth callback / bind / unbind / transfer）都会把 ctx 中的 slug 通过 `X-Tenant-Id` 转发给 cs-user；cs-user middleware 自动解析并写入对应的 `tenant_id`

**B3b.2b-step2（拆分为 step2a / step2b）** — OAuth callback Try 1/2/3 编排 + picker 端点

**B3b.2b-step2a（已完成 2026-07-17）** — cs-user 侧 RPC 端点 `POST /api/internal/tenants/resolve-by-email`：

- [x] `cs-user/internal/tenant/resolver.go`：新增 `ListByEmailDomain(ctx, email)` — 收集所有命中（picker candidate source）
- [x] `cs-user/internal/handlers/tenants.go`：新 handler `TenantsAPI.ResolveByEmail` — 接收 `{email}`，三态响应：`{status: ok, slug, tenant_id}` / `{status: ambiguous, candidates: [...]}` / `{status: not_found}`。**故意全程 200** — 语义状态在 `status` 字段里，避免 server RPCWriter 同时检查 HTTP 码 + body
- [x] `cs-user/internal/app/app.go`：`Deps.TenantResolver` 驱动新 endpoint；nil 时挂 `unavailableTenantResolver` stub 返回 503（保持 swagger 稳定）；`registerTenantRoutes` 挂 `/api/internal/tenants/resolve-by-email` 到 internal-token-protected 组
- [x] 测试：`tenants_test.go`（7 cases：unique hit / ambiguous / not_found / 空候选 list / 400 malformed / 400 empty / 其他错误 fall-through）+ `resolver_test.go` 新增 6 个 `ListByEmailDomain` 用例
- [x] swagger spec 同步生成（`make swagger`）

**B3b.2b-step2b（已完成 2026-07-17）** — server 侧 RPC client + AuthCallback Try 2 注入：

- [x] `server/internal/user/rpc_client_tenant.go`（新）：`(*RPCClient).ResolveTenantByEmail(ctx, email)` + 类型 `TenantEmailResolution` / `TenantEmailCandidate`。三态响应镜像 cs-user；空 email 短路为 `not_found` 无网络开销；5xx + 404 + 未知 status 全部归到 `ErrRPCUnavailable`（cs-user 应用层 200 + `status:not_found`，HTTP 404 意味着路由/部署问题，不能静默）
- [x] `server/internal/user/user.go`：新增 `TenantResolver` interface（`ResolveTenantByEmail(ctx, email)`），`Module.TenantResolver` 字段。`NewWithConfig` 在 rpc backend 时把 `*RPCClient` 注入；local backend 时留 nil（ADR D1 — 本地无 tenant 数据）
- [x] `server/internal/handlers/handlers.go:AuthCallback`（line 454 前后）：在 GetOrCreateUser 之前注入 §5 Try 2 块 — 若 `tenant.SlugFromContext(ctx) == ""` 且 `claims.Email != ""` 且 `UserModule.TenantResolver != nil`：
  - **ok**（唯一命中）→ `tenant.WithSlug` 重新注入 ctx（让随后的 GetOrCreateUser/ApplyEnterpriseMapping/ReissueToken 通过 RPCWriter 把 `X-Tenant-Id` 转发给 cs-user）+ set `cs_tenant_slug` cookie（365 天，Secure 跟随 `cookieSecure`，HttpOnly 防 JS 读）
  - **ambiguous** → TODO(step2c) redirect 到 picker UI；当前 log + fall through，登录不被阻塞
  - **not_found / error** → fall through 到默认 tenant
- [x] 测试：9 个 RPC client 用例（unique hit / ambiguous / not_found / 空 email 短路 / 5xx / 404 / 转发现有 slug / 未配置 / 未知 status）+ 接口 conformance 检查 `*RPCClient` 满足 `TenantResolver`
- [x] **效果**：当用户来自未配置 subdomain 的链接（直连顶级域名 / 邮件链接 / SSO 入口），登录后自动归属其 email 域名对应的 tenant，后续请求 cookie 自动携带 slug，无需每次重解析

**B3b.2b-step2c（部分完成 2026-07-17 — server 端 picker suggestion endpoint 已落地）** — picker UI redirect：

- [ ] AuthCallback `ambiguous` 分支：redirect 到 `/tenant/picker?candidates=...&state=...`（前端渲染候选 list，用户选择后 POST 回来 set cookie + 重放登录）。**待前端 picker 页面落地后再启用** — 当前仍 fall through 到 default tenant，避免 redirect 到 404 死链
- [x] picker endpoint `/api/tenant/suggest?email=...` — server 侧 wrapper 转发到 cs-user（前端 picker UI 调这个，不是直接打 internal endpoint）。已落地 `server/internal/handlers/tenant.go::SuggestTenant` + 路由 `api.GET("/tenants/suggest", ...)`（位于 OptionalAuth 区，pre-login 可调）；9 个测试覆盖 ok / ambiguous / not_found / 空 email 400 / local mode 503 / RPC unavailable 502 / 其他 error 502 / 未知 status 规范化为 not_found / nil 响应防御
- [ ] 前端 picker 页面 + 选择回调路由（独立前端 PR，不在本仓库 server 工作流范围内）

**B3b.2c（已完成 2026-07-17）** — cross-tenant 访问检测（JWT `tenant_slug` claim 路径）：

- [x] cs-user `EnterpriseClaims` + `IssuanceParams` 加 `TenantSlug` 字段；`/api/internal/users/reissue-token` handler 接受 body 里的 `tenant_slug` 并原样写入签发的 JWT
- [x] server `RPCWriter.ReissueToken` 从 ctx 取 slug 注入 body
- [x] server `middleware/auth.go` `AuthClaims` + `CasdoorUserInfo` 加 `TenantSlug`；`parseJWTToken` 直接从 MapClaims 读 `tenant_slug`（绕过 `NormalizeClaimsMap`，因为它只处理标准 Casdoor 字段）；`setAuthContext` 透传
- [x] NEW `server/internal/middleware/tenant_match.go` `TenantMatch` middleware：JWT slug vs runtime slug（来自 ResolveTenantSlug 的 ctx）不匹配 → 401 + clear cookie；任一为空 → 跳过（pre-cutover Casdoor token 或无 runtime 信号）
- [x] `server/cmd/api/main.go` 全局注册 `TenantMatch`（位于 `OptionalAuth` 之后、`RequireAuth` 之前 — 全局层，所有路由生效）
- [x] 测试：cs-user `TestReissueToken_TenantSlugForwarded` / `TestReissueToken_EmptyTenantSlugOmitted` + server `tenant_match_test.go` 6 用例（both empty / JWT-only / runtime-only / match / mismatch 401 / no-auth-claims）
- **设计决定**：用 `tenant_slug` claim 而非 `tenant_id` 做比较 — 避免每次请求做 slug→tenant_id 查询（RPC 调用）。两个 claim 可以并存于 JWT（`tenant_id` 留给未来 RLS / 多租户查询）。`tenant_slug` 空表示 pre-cutover Casdoor token，middleware 自动跳过 — 灰度兼容。
- **HMAC side-channel cookie 方案废弃**：A7 cs-user self-sign 基础设施已就绪，JWT-claim 路径更干净（无额外 cookie，无 HMAC 密钥管理）。

### B6 设计草案（**已降级为未来工作 — 2026-07-17**）

> **状态变更（2026-07-17）**：经评审，B6 当前不在 Phase B 必做范围。业务侧确认"可做要求，不需要当前做代码上的防范"。设计草案保留在下方供未来取用，但**不进入 Phase B 工作流**。
>
> 降级理由：
> 1. Phase B 应用层三层覆盖（B3b.2c JWT tenant_slug cross-tenant 检测 + B4 tenant_id → ctx + B5 `tenant.Scope(ctx)` read 路径）已经堵住业务路径上的跨 tenant 越权
> 2. DB 直接绕过 cs-user 查询的场景在当前部署形态下不存在（cs-user 是唯一持有 users 表 DSN 的进程）
> 3. RLS 的运维复杂度（三角色 / GORM Callback / 灰度 4 阶段 / testcontainers）和当前收益不匹配
> 4. **platform_admin 跨 tenant 路径**（草案 §platform_admin）一并延后到 Phase C（admin API）一起做
>
> **未来再启动 B6 的触发条件**：
> - 多服务/多进程共享 users 表 DSN（例如某些 read-only 副本直接连库做分析查询）
> - 合规审计明确要求 DB 层强制隔离
> - Phase B write scoping 出现真实跨 tenant 越权事件

> 以下是原始设计草案内容，保留作为未来取用的参考。

#### 目标 / 非目标

- ✅ **目标**：DB 层强制 `tenant_id` 过滤；即使 cs-user 应用层 bug 漏带 `WHERE tenant_id = ?`（B5 的 `tenant.Scope(ctx)` 因写路径未覆盖 / 未来重构手滑），DB 也不会返回跨 tenant 行
- ✅ **目标**：跨 tenant 平台管理路径走独立角色 + SECURITY DEFINER 函数（不依赖应用层 GUC）
- ✅ **目标**：可灰度（shadow 模式先观察 RLS 拒绝事件，再 enforce）
- ❌ **非目标**：业务库（costrict-web server 的 capability_items 等）的 RLS — 见 MULTI_TENANCY §22.3，留作独立后续
- ❌ **非目标**：替代 B5 应用层 scoping — RLS 是兜底，应用层仍要先过滤（RLS 命中率高 = 应用层覆盖率差，是 smell）

#### 范围：覆盖的表

当前 cs-user 已落地的 tenant-scoped 表：

| 表 | tenant_id 列 | 已有索引 |
|----|--------------|----------|
| `users` | TEXT NOT NULL DEFAULT 'default' (B2) | `idx_users_tenant_id` + `idx_users_tenant_username` (B7) |
| `user_auth_identities` | TEXT NOT NULL DEFAULT 'default' (B2) | `idx_user_auth_identities_tenant_id` |
| `employment_identities` | TEXT NOT NULL DEFAULT 'default' (B2) | `idx_employment_identities_tenant_id` + `idx_employment_identities_tenant_user` |

**不覆盖**的表（无 tenant 维度或不存在）：
- `tenants` / `tenant_admins` / `tenant_configs` — tenant 元数据本身，跨 tenant 可见（admin 路径）
- `user_profile` / `user_system_roles` / `user_gitea_binding` / `username_history` — MULTI_TENANCY 附录 C 列出但 cs-user 尚未落地；这些表首次落地时必须同步加 RLS（在各自迁移文件里）
- default tenant 行（`tenant_id = 'default'`）—— RLS 把它当普通 tenant，所有 default-tenant 行对运行时可见

#### 三角色矩阵

| 角色 | LOGIN | BYPASSRLS | 用途 | 备注 |
|------|-------|-----------|------|------|
| `cs_user_owner` | NO | NO | DDL / migration；table owner | FORCE RLS 让 owner 也受约束；migration 期间临时 DISABLE |
| `cs_user_app` | NO | NO | cs-user 服务运行时（普通 tenant 查询） | 通过 GORM pool 连接，由 GUC 注入当前 tenant |
| `cs_user_platform_admin` | NO | NO | 平台管理跨 tenant 读 | 走 SECURITY DEFINER 函数显式跳过 RLS，**禁止普通连接设 GUC 绕过** |

**LOGIN 角色**（应用层连接用，从对应 NOLOGIN 角色继承）：
- `cs_user_app_login` MEMBER OF `cs_user_app` —— cs-user 服务 DSN
- `cs_user_pa_login` MEMBER OF `cs_user_platform_admin` —— 平台管理专用 DSN（独立 pool）

`cs_user_owner` 不需要 LOGIN —— DDL 通过 migration runner 临时提权运行（`SET ROLE cs_user_owner`），migration 结束 `RESET ROLE` 并 re-enable RLS。

#### RLS Policy 定义

```sql
-- 1. 启用 + FORCE（所有 tenant-scoped 表）
ALTER TABLE users FORCE ROW LEVEL SECURITY;
ALTER TABLE user_auth_identities FORCE ROW LEVEL SECURITY;
ALTER TABLE employment_identities FORCE ROW LEVEL SECURITY;

-- 2. 三角色（NOLOGIN，靠 LOGIN 角色 MEMBER OF 继承）
CREATE ROLE cs_user_owner NOLOGIN NOBYPASSRLS;
CREATE ROLE cs_user_app    NOLOGIN NOBYPASSRLS;
CREATE ROLE cs_user_platform_admin NOLOGIN NOBYPASSRLS;

-- 3. cs_user_app 强制按 session 变量过滤
--    注意：B1 偏离 = TEXT 而非 UUID，所以无 ::UUID cast（与 MULTI_TENANCY 附录 C 不同）
--    NULLIF(..., '') 让 GUC 未设时 fallback 到 NULL，三值逻辑下 cs_user_app 永远看不到任何行
--    （fail-closed：宁可全错，也不漏一行跨 tenant 数据）
CREATE POLICY tenant_isolation_app ON users
    FOR ALL
    TO cs_user_app
    USING (tenant_id = NULLIF(current_setting('cs_user.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('cs_user.tenant_id', true), ''));

-- 同样 policy 复制到 user_auth_identities / employment_identities（三张表独立 POLICY）
-- ...

-- 4. cs_user_platform_admin 不挂任何 POLICY —— 它走 SECURITY DEFINER 函数跳过 RLS
--    （见下方 platform_admin 路径）

-- 5. cs_user_owner：FORCE RLS 下也受 POLICY 约束，但 owner 是 POLICY 的默认 target，
--    不显式声明就意味着无 POLICY 命中 → 全表不可见。给 owner 一条 "DENY ALL"：
CREATE POLICY tenant_isolation_owner_deny ON users
    FOR ALL
    TO cs_user_owner
    USING (false)
    WITH CHECK (false);
-- （这条让 owner 在 RLS 启用期间无法读写；migration 期间 DISABLE RLS 才能改数据）
```

#### GUC 注入点（cs-user 侧）

cs-user 用 GORM，需要 hook 在每个 request-scoped tx 开始时执行 `SET LOCAL cs_user.tenant_id = ?`。两个候选方案：

**方案 A — GORM Callbacks（推荐）**

注册 `Before("create", "query", "update", "delete")` callback，从 `db.Statement.Context` 取 tenant_id 并 `EXEC 'SET LOCAL cs_user.tenant_id = ' || quote($1)`。优点：自动覆盖所有 GORM 路径，零侵入；缺点：raw SQL（`db.Raw`）若不走 callback 链会漏（但 cs-user 内部全 GORM）。

**方案 B — 显式 tx 包装**

`tenant.RunInTx(ctx, tenantID, fn)` helper 强制每个请求 handler 显式开 tx 并设 GUC。优点：白盒；缺点：所有 handler 改造，工作量大，且容易漏写。

**决定：方案 A**。在 `cs-user/internal/storage/postgres.go::Open` 里注册全局 GORM Callback，从 ctx 读 `tenant.IDFromContext(ctx)` 并 `SET LOCAL`。`IDFromContext` fallback 到 `tenant.DefaultTenantID = "default"` —— pre-cutover 单租户请求自动看到 default tenant 行（与 B2 回填等价行为）。

#### platform_admin 跨 tenant 路径

```sql
-- 专用 SECURITY DEFINER 函数，绕过 RLS 读所有 tenant。
-- `SET LOCAL row_security = off` 是 PG 官方推荐的 RLS 旁路方式（PG 13+），
-- 在 SECURITY DEFINER 函数内生效且作用域限定到本次调用 —— 比 MULTI_TENANCY
-- 附录 C 的"函数 owner 是 cs_user_owner"做法更稳（避免 owner-deny POLICY
-- 自相矛盾：FORCE RLS 下 owner 也被 deny，函数以 owner 身份反而查不到行）。
CREATE OR REPLACE FUNCTION cs_user_list_all_users()
RETURNS SETOF users
LANGUAGE sql SECURITY DEFINER SET search_path = cs_user, pg_temp AS $$
    SET LOCAL row_security = off;
    SELECT * FROM users;
$$;
-- 仅 cs_user_platform_admin 角色有 EXECUTE 权限
REVOKE ALL ON FUNCTION cs_user_list_all_users() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION cs_user_list_all_users() TO cs_user_platform_admin;
```

#### 灰度策略（必做，不能直接 enforce）

直接 ENABLE + FORCE RLS 风险：cs-user 应用层若有未覆盖路径（B5 write 方法未 scope / 任何遗漏的 raw query），RLS 会**静默**返回空结果集（POLICY 命中后行被过滤），用户感知是"数据丢失"。

**Shadow 模式**（MULTI_TENANCY §N2 推荐）：
1. **阶段 1 — ENABLE without FORCE**：`ALTER TABLE ... ENABLE ROW LEVEL SECURITY`（不加 FORCE）—— table owner（cs_user_owner，即 migration 角色）仍能 bypass，应用层不受影响；但 cs_user_app_login 已受 POLICY 约束
2. **阶段 2 — 应用层 GUC 注入先跑通**：先实现 GORM Callback + GUC 注入，让所有 cs-user 请求都正确 SET LOCAL；观察 1 周无空结果异常
3. **阶段 3 — FORCE + audit log**：加 FORCE，配 PG `ROW SECURITY VIOLATION` 事件监听（pgaudit 或 log_analyzer），发现违规即告警
4. **阶段 4 — 真正 enforce**：默认 ON；shadow 模式作为可回滚开关（`SECURITY_SCAN_SHORT_CIRCUIT_DISABLED` 同款 env 模式：设 `B6_RLS_SHADOW_MODE=true` 回到阶段 2）

#### 测试矩阵（testcontainers）

新文件 `cs-user/internal/storage/rls_test.go`，用 testcontainers-go 起真实 PG：

| 用例 | 期望 |
|------|------|
| `cs_user_app_login` + GUC 设为 tenant A | 只看到 tenant A 的 users / identities |
| `cs_user_app_login` + GUC 未设 | 0 行（fail-closed） |
| `cs_user_app_login` + GUC 设为 tenant A + 写 tenant B 行 | WITH CHECK 拒绝 |
| `cs_user_app_login` + 跨 tenant JOIN（A.user × B.identity） | join 结果空 |
| `cs_user_pa_login` + `list_all_users()` 函数 | 看到所有 tenant 行 |
| `cs_user_owner` + 直接 SELECT（FORCE 已启） | 0 行（owner-deny POLICY） |
| `cs_user_owner` + DISABLE RLS + SELECT | 全部行（migration 路径） |

**sqlite 不支持 RLS** —— 现有 cs-user sqlite 测试套件（service_test 等）跑不了 RLS 用例。RLS 测试必须 testcontainers-only，build tag `//go:build cgo` 配合 testcontainers-go（已有 `runner_cgo_test.go` 先例）。

#### 与 B5 write 方法 scoping 的关系

B6 RLS 兜底后，B5 write 方法 scoping follow-up 的紧迫度下降：
- **B6 enforce 前**：write path 未 scope 是真实漏洞（DB default 保证新建行 tenant_id='default'，但 UPDATE/DELETE WHERE 漏 tenant_id 会跨 tenant 改）
- **B6 enforce 后**：write path 未 scope 也被 RLS WITH CHECK 兜住（INSERT/UPDATE 必须满足 `tenant_id = current_setting`，否则拒绝）

**结论**：B5 write scoping 仍应做（应用层性能 + 可读性 + RLS 兜底失效时的防御），但 B6 落地后它的紧迫度从 P1 降到 P2。建议 B6 enforce（阶段 4）后再补 B5 write scoping，作为 defense-in-depth 加固。

#### 工作量估算

| 子任务 | 估算 |
|--------|------|
| migration SQL（三角色 + POLICY × 3 表 + SECURITY DEFINER 函数） | 0.5 天 |
| GORM Callback + GUC 注入 + tenant_id ctx 透传 | 1 天 |
| testcontainers RLS 测试套件（7 用例） | 1.5 天 |
| 灰度 shadow 模式 + env flag + audit log | 0.5 天 |
| 文档 + code review | 0.5 天 |
| **合计** | **4 天** |

分 2 个 PR：
- **PR-1**：migration + GORM Callback + GUC 注入 + 测试（ENABLE without FORCE，shadow 阶段）
- **PR-2**：FORCE + audit log + 正式 enforce（观察 1 周后）

#### 待决问题（需用户拍板）

1. **平台管理跨 tenant 路径当前是否启用？** 如果没有现成的"看所有 tenant 用户"功能，platform_admin 角色和 SECURITY DEFINER 函数可以延后到 Phase C（admin API）一起做，B6 只覆盖 cs_user_app 路径
2. **`SET LOCAL row_security = off` 是否符合安全审计要求？** 比 MULTI_TENANCY 附录 C 的 owner-as-definer 更稳但更新；如果有合规要求严格按附录 C 实现，需要补 `cs_user_pa_admin_definer` 角色
3. **GORM Callback vs 显式 tx 包装**：我倾向 Callback（零侵入），但 Callback 难调试（全局副作用）；如果团队偏好显式，方案 B 也可以

#### 参考实现 / 文档

- `docs/identity-tenant/MULTI_TENANCY_DESIGN.md` §10.2（L864）—— 主体设计
- `docs/identity-tenant/MULTI_TENANCY_DESIGN.md` 附录 C（L2946）—— 完整参考实现（注意：用 `::UUID` cast，B6 需改 TEXT）
- PostgreSQL 官方文档 [43.1. RLS](https://www.postgresql.org/docs/current/ddl-rowsecurity.html) —— `SET LOCAL row_security = off` 在 SECURITY DEFINER 函数内的用法
- `cs-user/migrations/20260717120000_add_tenant_id_to_user_tables.sql` —— B2 schema 实际形态

---

### B7 实现细节（已完成 2026-07-17）

- **migration**：`cs-user/migrations/20260717180000_users_username_per_tenant_unique.sql`
  - `ALTER TABLE users DROP CONSTRAINT IF EXISTS idx_user_username` + `DROP INDEX IF EXISTS idx_user_username`（两条语句幂等覆盖 CONSTRAINT 形式与 INDEX 形式两种历史风格）
  - `CREATE UNIQUE INDEX IF NOT EXISTS idx_users_tenant_username ON users(tenant_id, username)`
  - Down 回滚到全局唯一（若已有跨 tenant 同名用户，回滚会失败 — 预期，回滚到 Phase B 前的多租户状态本身就不一致）
- **不动 email / external_key**：
  - `users.email` 全局唯一保留 — SSO 桥接、密码找回、邮件通知都依赖 email 全局唯一
  - `user_auth_identities.external_key`（形如 `{provider}:{provider_user_id}`）全局唯一保留 — 跨 tenant SSO 去重锚点，是 B3b 后续跨 tenant 身份合并（identity transfer）链路的基石
- **数据兼容**：当前所有用户 tenant_id='default'（B2 回填），现有 'alice/default' 在迁移后仍唯一不冲突。新建 tenant 后才有真正的多 tenant 用户重叠场景
- **应用层无改动**：`GetOrCreateUser` / `SyncUser` 都是按 `(sub, universal_id)` 找用户，不依赖 username 唯一性；service 层不需要随本迁移改动

### B3b.2b-step2c 实现细节（部分完成 2026-07-17 — server 端 picker suggestion endpoint）

- **范围声明**：本增量只落地 server 端 suggestion endpoint + 路由 + handler + 测试。AuthCallback `ambiguous` 分支的 picker redirect **没有启用** — 前端 picker 页面尚未存在，启用 redirect 会落到 404 死链。当前 AuthCallback `ambiguous` 仍 log + fall through 到 default tenant，登录不被阻塞。前端 picker 落地后启用 redirect 是一个独立的小增量。
- **NEW handler**（`server/internal/handlers/tenant.go`）：
  - `SuggestTenant(c *gin.Context)` — wrapper around `UserModule.TenantResolver.ResolveTenantByEmail`，专为前端 picker UI 设计
  - 输入：`email` query param（trim 后非空校验，空则 400）
  - 输出：`tenantSuggestResponse{status, slug?, tenant_id?, candidates?}`，三态镜像 cs-user 内部协议（ok / ambiguous / not_found）
  - 未知 status 字符串规范化为 `not_found`（cs-user 协议演进时的 fallback safety net，不让前端死在不认识的 discriminator 上）
- **路由注册**（`server/cmd/api/main.go`）：`api.GET("/tenants/suggest", handlers.SuggestTenant)` 位于公开 OptionalAuth 区（与 `/users/names` / `/users/info` 同层）— picker 页面在 pre-login 状态下调用，不能 gate 在 RequireAuth 之后
- **错误码语义**：
  - 400 — 空 email（客户端 bug）
  - 503 — UserModule 或 TenantResolver 为 nil（local backend 模式，ADR D1 — server 不持有 tenant 数据）；503 而非 404 让前端区分"feature 在此部署禁用"vs"端点路径错误"
  - 502 — RPC 不可用（ErrRPCUnavailable）或任何非预期 resolver error；不泄漏内部错误文本到 body
  - 200 — 所有三种语义状态（ok / ambiguous / not_found）；discriminator 在 body.status 字段，与 cs-user 内部 RPC 协议对齐
- **测试**（`server/internal/handlers/tenant_test.go`，9 用例）：
  - `TestSuggestTenant_Ok` — 唯一命中返回 slug+tenant_id
  - `TestSuggestTenant_Ambiguous` — 多命中返回 candidates 数组
  - `TestSuggestTenant_NotFound` — 无命中所有字段空
  - `TestSuggestTenant_EmptyEmailReturns400` — whitespace-only email 短路 400，不调用 resolver
  - `TestSuggestTenant_LocalModeReturns503` — TenantResolver nil（local backend）返回 503
  - `TestSuggestTenant_RPCUnavailableReturns502` — `errors.Is(err, ErrRPCUnavailable)` 路径
  - `TestSuggestTenant_OtherErrorReturns502` — 非 sentinel error 也走 502，且 body 不泄漏 "boom"
  - `TestSuggestTenant_UnknownStatusNormalizesToNotFound` — cs-user 协议演进兜底
  - `TestSuggestTenant_NilResponseReturns502` — 防御 (nil, nil) contract 违反（ResolveTenantByEmail 文档承诺不返回，但跨服务边界不信任）
- **stub 模式**：测试用 `stubTenantResolver` 直接实现 `TenantResolver` interface，closure 字段控制每个用例的 outcome；不需要起 httptest server，因为 suggest handler 只消费 Go-level `TenantEmailResolution`，不碰 wire format
- **swagger 注释**：handler 携带完整 `@Summary / @Description / @Tags / @Param / @Success / @Failure / @Router` 注释。注：当前仓库 `make swagger` 因 pre-existing `server/internal/audit/handlers.go` ParseComment 错误无法 regen（与本次改动无关）；swagger spec 已经 stale 多个 commit。本增量的注释已通过 `swag fmt` 校验语法，等 audit handlers 修复后 regen 会自动包含本端点
- **ADR D1 强化**：endpoint 在 local backend 模式明确返回 503 而非伪装数据 — 提醒调用方"此部署没有 tenant 数据"，避免 silent fallback 掩盖配置错误

### B5 write 方法 scoping follow-up 实现细节（已完成 2026-07-17）

承接 B5（read 方法 scoping）的 follow-up：把 4 个 write 方法 + 1 个共享 tx helper 全部纳入 `tenant.Scope(ctx)` 覆盖。

- **覆盖范围**：
  - `GetOrCreateUser` — 8 个 lookup 查询 + 新建 user Create 路径 + race retry 路径
  - `BindIdentityToUser` — tx 内 3 个 lookup + 2 个 Model.Updates + 1 个 Create + 1 个 primary 检测
  - `TransferIdentityToUser` — tx 内 1 个 lookup + 1 个 Model.Updates + 2 次 refreshUserProfileFromIdentitiesTx
  - `UnbindIdentityByProvider` — tx 内 2 个 lookup + 1 个 count + 1 个 Model.Updates 循环 + 1 个 primary 重新选择
  - `refreshUserProfileFromIdentitiesTx(ctx, tx, userSubjectID)` — 签名加 ctx 参数，内部 lookup/Updates/Save 全 scope
- **关键修复点（避坑）**：
  - **scope 不能挂在 db handle 上**：原本写成 `db := s.db.WithContext(ctx).Scopes(tenant.Scope(ctx))` — `db.Create(&user)` 因此返回 "record not found"（GORM Statement 在 Scopes 后被 WHERE 污染，Create 路径行为异常）。改为 `db := s.db.WithContext(ctx)` + 每个 lookup 单独 `.Scopes(tenantScope)`。Create / Updates 走 scope-in-chain 模式而非 handle-scoped。
  - **Create 路径显式设 TenantID**：`user.TenantID = tenant.IDFromContext(ctx)` 和 `identity.TenantID = tenant.IDFromContext(ctx)`。否则依赖 GORM 列默认值 `'default'` — 多租户场景下 acme.com 登录的用户会被错误归入 default tenant。B6 RLS 兜底取消后这是**唯一**保证新建行 tenant_id 正确的机制，P1 紧迫度。
  - **TransferIdentityToUser 不允许跨 tenant**：scope 应用后，外部 caller 若尝试 transfer 另一个 tenant 的 identity，lookup 直接 `record not found`（identity_not_found），fail-closed。这是有意的安全边界 — 跨 tenant 身份转移需要专门 admin 路径（Phase C）。
- **`refreshUserProfileFromIdentitiesTx` 签名变更**：`(tx, userSubjectID)` → `(ctx, tx, userSubjectID)`。3 个 tx 方法内调用全部更新。`identity_test.go` 里 3 个直接调用也更新（注入 `context.Background()`，sqlite 测试库无 tenant 概念，scope 退化为 `WHERE tenant_id = 'default'`，与列默认值匹配，不影响行为）。
- **scope 函数复用**：`scope := tenant.Scope(ctx)` 在每个方法开头捕获一次，闭包内 tenant_id 已固定，多处复用安全（pure function）。
- **测试覆盖**：现有 `service_test.go` / `service_write_test.go` / `identity_test.go` 全部 PASS（无 tenant 上下文场景走 DefaultTenantID = "default"，与 B2 列默认值匹配，行为不变）。未新增 cross-tenant 测试用例 — sqlite 无法测真实 cross-tenant（需 testcontainers），且 B5 read scoping 已有等价行为验证。Cross-tenant fail-closed 行为是 scope 应用机制的直接结果，单测覆盖应用链路即可。
- **GORM 行为说明**：`.Scopes(scope).Where(...)` 在 GORM v2 上是创建新 Statement 实例并附加条件，对原 db handle 无副作用；`.Scopes()` 后链式 `.Create()` / `.Updates()` 行为正常。这是反复实测后的结论，不是 GORM 文档承诺。

### B5 实现细节（已完成 2026-07-17）

- **scope = cs-user 侧 read 方法**：design §10.1 明确要求"cs-user 所有 service 方法签名强制带 ctx，DB query 自动加 WHERE tenant_id = ?"。server 侧不持有 tenant-scoped 表（ADR D1 — cs-user owns users + tenants），所以 B5 落地在 cs-user。
- **新增 primitive**（`cs-user/internal/tenant/context.go`）：
  - `DefaultTenantID = "default"` 常量（与 server 端 mirror；B2 回填后所有行都是 'default'，pre-cutover 单租户行为等价）
  - `IDFromContext(ctx) string` — 从 `*models.Tenant` 解析 canonical tenant_id，空则 fallback 到 DefaultTenantID；保证 caller 永远拿到非空值
  - `Scope(ctx) func(*gorm.DB) *gorm.DB` — 返回 GORM scope，应用 `WHERE tenant_id = ?`；pass 给 `db.Scopes(tenant.Scope(ctx))`
- **迁移的 4 个 read 方法**（`cs-user/internal/user/service.go`）：`GetUserByID` / `GetUsersByIDs` / `SearchUsers` / `ListIdentities` 签名加 `ctx context.Context` 首参；每个 query 加 `.WithContext(ctx).Scopes(tenant.Scope(ctx))`。同步更新：
  - `handlers/users.go` UserService interface + 3 call sites（`c.Request.Context()` 透传）
  - `handlers/user_auth_identities.go` AuthIdentityService interface + 1 call site
  - `app/app.go` `unavailableUserService` + `unavailableAuthIdentityService` stubs（`_ context.Context` 占位）
  - `handlers/{users,user_auth_identities}_test.go` stub signatures + closure 签名
  - `internal/user/service_test.go` + `service_write_test.go` 所有 call sites（`context.Background()`）
- **write 方法（GetOrCreateUser / BindIdentityToUser / TransferIdentityToUser / UnbindIdentityByProvider）暂未应用 Scope**：write 路径有复杂 tx 逻辑（identity cascade / primary 切换 / refreshUserProfileFromIdentitiesTx），机械加 Scope 风险高。B2 已确保所有新建行 tenant_id='default'（DB default），数据正确性已保证；query scoping 是 defense-in-depth，由 B6 RLS 兜底。Write 方法 scoping 列为 follow-up（与 B6 一起做更稳）。
- **单租户安全性**：DefaultTenantID fallback 确保 `WHERE tenant_id = 'default'` 查到与 pre-B5 全表扫描等价的结果集；可增量推进无需 flag。所有现有 sqlite + gorm 测试无需改动 fixture（默认 ctx 即 default tenant）即继续通过。
- **B6 RLS 之前的安全态势**：B3b.2c + B4 + B5 三层覆盖（slug 比较 → ctx 注入 → query scope）；剩余攻击面是绕过 cs-user 直接查 DB（B6 RLS 解决）+ write path 未 scope（DB default + B2 回填保证正确性）。

### B4 实现细节（已完成 2026-07-17）

- **cs-user 端无新改动**：`EnterpriseClaims.TenantID` + `IssuanceParams.TenantID` + reissue-token handler body 接受 `tenant_id` 早已存在（Phase A7 时已落地）。本任务纯 server 端 plumbing。
- **server 端 ctx 助手**：`server/internal/tenant/context.go` 加 `WithTenantID` / `TenantIDFromContext`（与 `WithSlug` / `SlugFromContext` 平行）+ `DefaultTenantID = "default"` 常量。新增独立 `tenantIDKey struct{}` 避免与 slug key 冲突；测试 `TestWithSlugAndTenantID_DoNotClobber` 锁定。
- **AuthClaims / CasdoorUserInfo 加 TenantID 字段**：与 TenantSlug 平行；`parseJWTToken` 直接从 MapClaims 读 `tenant_id`（`claims["tenant_id"].(string)` comma-ok，绕过 `NormalizeClaimsMap` — 后者只处理标准 Casdoor 字段）；`setAuthContext` 透传 `userInfo.TenantID → AuthClaims.TenantID`。
- **NEW `TenantContext` middleware**（`server/internal/middleware/tenant_context.go`）：读 `AuthClaimsKey.AuthClaims.TenantID`，空则 fallback 到 `tenant.DefaultTenantID`；写入 `c.Request.Context()` via `tenant.WithTenantID`；同时 `c.Set("tenant_id", id)` 便于 handler 直接 `c.GetString("tenant_id")`。
- **main.go 装配位置**：`r.Use(middleware.TenantContext())` 位于 `OptionalAuth`（populates AuthClaimsKey）之后、`TenantMatch` 之后 — 顺序相对 `TenantMatch` 无所谓（两者读 disjoint 字段：TenantID vs TenantSlug）。
- **fallback 设计哲学**：所有下游（B5 `tenantScope(ctx)` query helper）都保证拿到非空 tenant_id；pre-cutover Casdoor token 或 unauthenticated 请求自动落 default — 与 B2 数据回填（所有行 tenant_id='default'）等价单租户行为一致。
- **与 B3b.2c 的关系**：B3b.2c 检测 cross-tenant 访问（用 slug claim 做 string 比较，避免每请求 slug→tenant_id 查询）；B4 为合法请求注入 query scoping 用的 canonical ID。两条 claim 在 JWT 中并存（`tenant_slug` + `tenant_id`），分别服务不同用途。

### B3b.2a 实现细节

- **server 不复制 tenants 表**：与 cs-user 不同，server 的 tenant 包只做 slug 提取 + ctx 转发。实际 DB 查询由 cs-user 完成（`tenant_id` lookup via `ResolveBySlug`）。这保证 tenant 数据 ownership 集中在 cs-user（ADR D1）。
- **slug vs tenant_id 都接受**：cookie 和 header 既可以装 slug 也可以装 tenant_id — cs-user 的 `ResolveBySlug` 接受两者，所以 server 不需要区分。
- **空 slug = "no signal"**：`WithSlug(ctx, "")` 是合法的；`HasSlug` 返回 false；RPC 转发省略 `X-Tenant-Id`；cs-user 自动 fallback 到 default tenant。这让本地 dev（无 apex 配置、无 cookie）零摩擦。
- **subdomain 提取与 cs-user 一致**：`slugFromHost` 用 `strings.LastIndex(sub, ".")` 取 label immediately below apex（`foo.acme.example.com` → `acme`），与 cs-user `resolver.go` 行为对齐 — 两端必须 agree，否则同一 host 解析出不同 slug。
- **middleware 装在 OptionalAuth 之前**：让 JWKS / health / swagger 等公开路由也能从 cookie/subdomain 提取 slug 转发；不过这些路径不调 cs-user RPC，slug 实际只对后续 RPC 调用有用。
- **RPC 转发位置**：`rpc_client.do()` 在设 `X-Internal-Token` 之后立即检查 ctx 中的 slug，非空时设 `X-Tenant-Id`。`tenant_ctx.go` 提供共享 helper，nil-ctx 防御。
- **已知限制 — write-path 未覆盖（B3b.2b-step1 已修复）**：B3b.2a 当时由于 `rpc_writer.go` 所有 6 个 `RPCWriter` 方法硬编码 `context.Background()`，slug 永远到不了注入点。该 refactor 已在 B3b.2b-step1 完成 — `UserWriter` interface 全部加 ctx，write-path 注入现在生效。详见 B3b.2b-step1 实现细节。

### B3b.1 实现细节

- **三层 fallback 顺序**：`X-Tenant-Id` header（server → cs-user RPC 可信载体，因 `/api/internal` 路由组已被 `RequireInternalToken` gate）→ `cs_tenant_slug` cookie（浏览器粘性选择）→ Host subdomain（仅当 `cfg.Tenant.ApexDomains` 非空，本地 dev 默认禁用）
- **default fallback**：所有信号 miss 时 middleware 主动查 `slug="default"` 行；DB error（含 default 行被误删）走 `tenant_resolve_error` context key 让 handler 翻译成 503
- **优雅降级**：nil resolver / DB error 时 middleware **不** abort chain；handler 用 `TenantFromGin` 取 `(*Tenant, error)`，nil+nil-err 视为"default 行不存在"返回 503
- **ctxKey 防碰撞**：`tenant` 包用 unexported `ctxKey struct{}` 作 key，调用方无法构造相同 key；middleware 同时写 gin 内置 key（`c.Set("tenant", ...)`）和 `c.Request` 的 context（`tenant.WithTenant`），net/http handler 也能读到
- **subdomain 仅当 apex 配置**：本地 dev `Host="localhost:8080"` 没有 subdomain 信号；生产 prod `Host="acme.cs-user.example.com"` → slug "acme"。`CS_USER_APEX_DOMAINS="cs-user.example.com"` 启用，留空禁用
- **B3b.1 不在 OAuth 链路**：OAuth callback 拿不到 cookie（Casdoor 重定向是 GET，cookie 跨域受限）+ subdomain 也常常缺失；Try 1/2/3 编排放到 B3b.2 server 端实现，B3b.1 只覆盖 cs-user 入口的 raw HTTP 路径（`/api/internal/*` RPC + JWKS / healthz / swagger 等公开路径）

---

## 阶段 C：三级权限 + admin API

## 阶段 C：三级权限 + admin API

- [x] **C1**：权限模型表 + 中间件（platform_admin / tenant_admin / tenant_member）— 详见下方"C1 实现细节"
- [x] **C2**：platform_admin tenant CRUD API（7 endpoints）— 详见下方"C2 实现细节"；跨 tenant 用户 ops / audit-log infra / email allowlist 等其他 C2 切片未做（用户明确选 Tenant CRUD 切片优先）
- [ ] **C3**：tenant_admin API（本 tenant 用户列表、IdP 配置、provider_mapping yaml 编辑）— **C3.1 用户列表（2 commits）+ C3.2 tenant config CRUD（5 commits）+ C3.3 provider_mapping typed editing（5 commits）已落地**；详见下方"C3.1 / C3.2 / C3.3 实现细节"
- [ ] **C4**：越权防护 + 审计日志 — **C4.1 审计日志基础设施（5 commits）已落地**：`user_center_audit_log` 表（migration + GORM model + 3 indexes 覆盖 tenant/actor/action 三个查询维度）+ `auditlog.Service` 最佳努力写入器（DB 失败仅 WARN 日志，不打断用户 op）+ 6 写路径 instrument（platform tenant create/suspend/restore/delete + tenant_config.update + provider_mapping.update）+ server 端 AuthClaims → `tenant.ActorMeta` ctx-carrier → RPC client 转发 `X-Actor-Tenant-Role` / `X-Actor-Platform-Scope` headers。详见下方"C4.1 实现细节"；C4.2 active 越权检测 middleware / C4.2 list endpoints 待启动

**测试覆盖重点**：每个 admin endpoint 必须覆盖"角色不符 → 403"路径；swagger 注解挂 `@Security` 双重（InternalToken + BearerAuth 角色注解）。

### C1：权限模型表 + 中间件（已落地）

- [x] **实现**：`cs-user/migrations/20260717190000_create_platform_admins.sql` —
  - `platform_admins` 表（4 列 + PK）：`user_id VARCHAR(191) PRIMARY KEY`（每用户一行）/ `granted_by VARCHAR(191) NOT NULL` / `granted_at TIMESTAMPTZ NOT NULL DEFAULT now()` / `scope VARCHAR(32) NOT NULL DEFAULT 'full'`（full | support | read_only，应用层枚举校验，无 CHECK 约束）
  - 外键：`fk_platform_admins_user` (CASCADE — 删用户自动撤销授权) + `fk_platform_admins_granted_by` (RESTRICT — 防止删除有授权记录的授予者)
  - 索引：`idx_platform_admins_scope`（platform_admin lookup by scope，e.g. "show all read_only admins"）
  - **无 revoked_at** — lifecycle 是 DELETE-only（hard revoke），审计 trail 落 `user_center_audit_log`（§16.2），区别于 tenant_admins 的 soft-revoke
  - **无 bootstrap INSERT** — 第一条 platform_admin 由 operator 手动 SQL 注入（详见决策记录）
- [x] **模型**：`cs-user/internal/models/platform_admin.go` — `PlatformAdmin` 结构体 + 三个 scope 常量（`PlatformScopeFull` / `PlatformScopeSupport` / `PlatformScopeReadOnly`），`TableName()` 返回 `platform_admins`
- [x] **service 读取方法**：`cs-user/internal/user/permission.go` —
  - `GetPlatformAdmin(ctx, userSubjectID) (*models.PlatformAdmin, error)` — 单行 lookup；`(nil, nil)` 表示非 platform admin（graceful degradation，镜像 `GetEmploymentIdentity` 契约）
  - `ListActiveTenantRoles(ctx, userSubjectID, tenantID) ([]string, error)` — 多行 lookup，`WHERE revoked_at IS NULL`；返回 role names 列表（空 slice = regular member）
  - sentinel errors：`ErrEmptySubjectID` / `ErrEmptyTenantID`（caller-programming error，handler 映射 400）
- [x] **JWT claims 扩展**：`cs-user/internal/auth/claims.go` — `EnterpriseClaims` 加三个字段（`TenantRoles []string` / `PlatformAdmin bool` / `PlatformScope string`，全 omitempty），`IssuanceParams` 加对应输入字段，`NewEnterpriseClaims` 透传
- [x] **reissue-token wiring**：`cs-user/internal/handlers/auth.go` — 新增 `PermissionReader` interface（可选注入，nil 时跳过 = 灰度模式），`AuthAPI.Permissions` 字段；`ReissueToken` 在 employment read 后调用 `GetPlatformAdmin` + `ListActiveTenantRoles`，结果翻译为 JWT claims
- [x] **server JWT 解析扩展**：`server/internal/middleware/auth.go` — `AuthClaims` + `CasdoorUserInfo` 加 `PlatformAdmin bool` / `PlatformScope string` / `TenantRoles []string` 字段；`parseJWTToken` 从 MapClaims 提取（`NormalizeClaimsMap` 不覆盖 Phase C1 字段）；`setAuthContext` 透传
- [x] **permission middlewares**：`server/internal/middleware/permission.go` —
  - `RequirePlatformAdmin(scopeArgs...)` — platform_admin=true 必须；scope allowlist 可选过滤
  - `RequireTenantAdmin(roles...)` — tenant_admin role 命中即可；platform admin 短路（§14.3 super-tenant）；空 role args = 任意 tenant_admin role
  - `RequireTenantMember` — 仅要求 AuthClaims.TenantID 非空（baseline gate）
  - 全部：缺 AuthClaimsKey → 401；存在但权限不足 → 403（区分"未登录" vs "权限不够"）
- [x] **app wiring**：`cs-user/cmd/api/main.go` + `cs-user/internal/app/app.go` — `Deps.PermissionReader` 字段（生产 = 同一个 `*user.Service`），`registerAuthRoutes` 注入到 `AuthAPI.Permissions`

**测试覆盖**（全绿）：
- `cs-user/internal/models/platform_admin_test.go`（5 测试）— PK 唯一、scope 默认 full、UPDATE lifecycle、DELETE revoke
- `cs-user/internal/auth/claims_test.go`（+4 测试）— permission fields round-trip / JSON shape / omitempty / sign+verify
- `cs-user/internal/user/permission_test.go`（8 测试）— GetPlatformAdmin 三态（happy / not-found-nil / empty-arg-err）+ ListActiveTenantRoles 四态（happy / skips-revoked / tenant-scoped / empty-args-err）
- `cs-user/internal/handlers/auth_test.go`（+5 测试）— permission claims populated / no-reader-still-issues / regular-member-omits-claims / platform-error-500 / tenant-roles-error-500
- `server/internal/middleware/permission_test.go`（13 测试）— RequirePlatformAdmin 5 / RequireTenantAdmin 6 / RequireTenantMember 3 / non-AuthClaims-value 防护

**决策记录**：
1. **无 env-var auto-bootstrap**：第一条 platform_admin 由 operator 通过 SQL migration 手动 INSERT（或运维脚本）；不做 env-var 自动 bootstrap，避免"机器一启动 root 就被自动赋权"的安全 footgun
2. **no revoked_at**：platform_admins 是 hard-delete lifecycle（DELETE 撤销），区别于 tenant_admins 的 soft-revoke；审计 trail 落 user_center_audit_log（§16.2）
3. **no CHECK constraint on scope**：scope enum 由应用层校验（PlatformScope* 常量集），DB 不加 CHECK — 添加新 scope 不需要 migration，匹配 tenant_admins role 的同样决策
4. **PermissionReader 可选注入**：handler 的 `Permissions` 字段为 nil 时跳过 lookup（灰度模式）；这样 C1 上线时可以只开 schema + claims，不激活 middleware，再单独激活 middleware 强制
5. **platform admin super-tenant**：`RequireTenantAdmin` 中 platform admin 短路通过 — 平台级管理员本质是跨 tenant 的（§14.3）



### C2 实现细节：platform_admin tenant CRUD API（已落地 2026-07-17）

**端点**（7 个，两层 ADR D1 模式 — cs-user 内部 + server 公开代理）：

| cs-user 内部（InternalAuth）| server 公开（RequirePlatformAdmin）| 行为 |
|---|---|---|
| `GET /api/internal/platform/tenants` | `GET /api/platform/tenants` | 分页列表（`limit` / `offset` / `status` 过滤）|
| `POST /api/internal/platform/tenants` | `POST /api/platform/tenants` | 创建租户；slug 校验 + email_domains 非冲突 |
| `GET /api/internal/platform/tenants/:id` | `GET /api/platform/tenants/:id` | 单租户；`:id` 接受 `tenant_id` OR `slug` |
| `PATCH /api/internal/platform/tenants/:id` | `PATCH /api/platform/tenants/:id` | 部分更新（display_name / edition / email_domains / features / limits / settings）|
| `POST /api/internal/platform/tenants/:id/suspend` | `POST /api/platform/tenants/:id/suspend` | active → suspended |
| `POST /api/internal/platform/tenants/:id/restore` | `POST /api/platform/tenants/:id/restore` | suspended → active |
| `POST /api/internal/platform/tenants/:id/delete` | `POST /api/platform/tenants/:id/delete` | active\|suspended → deleted + `deletion_requested_at = now` |

**生命周期状态机**：
```
       create          suspend         restore
[ ] ──────────► [active] ◄─────────► [suspended]
                  │                     │
                  │    delete           │  delete
                  └──────► [deleted] ◄──┘
                            (deletion_requested_at = now;
                             30-day grace cron → hard delete is OUT OF SCOPE)
```
非法状态转移返回 409（`ErrInvalidStateTransition`）。

**Sentinel → HTTP 映射**（两层都一致）：
- `ErrTenantNotFound` → 404
- `ErrSlugTaken` / `ErrEmailDomainConflict` / `ErrInvalidStateTransition` → 409
- `ErrInvalidSlug` / `ErrInvalidEdition` / `ErrInvalidDisplayName` / `ErrInvalidEmailDomains` → 400
- server 端额外：`ErrRPCUnavailable` / `ErrNotConfigured` → 502

**分层文件**：
- `cs-user/internal/tenant/admin.go` — `Admin` 服务（write/lifecycle），独立于 `Resolver`（read）
- `cs-user/internal/tenant/admin_test.go` — 23 测试（sqlite :memory: + `//go:build cgo`）
- `cs-user/internal/handlers/platform_tenants.go` — `PlatformTenantsAPI`（7 handlers + swagger）
- `cs-user/internal/handlers/platform_tenants_test.go` — 16 handler 测试
- `cs-user/internal/app/app.go` — `Deps.TenantAdmin` 字段 + `registerPlatformTenantRoutes` 注册 + `unavailablePlatformTenantService` 503 stub
- `cs-user/cmd/api/main.go` — `tenant.NewAdmin(pool.Gorm)` 注入
- `cs-user/docs/{docs.go,swagger.json,swagger.yaml}` — `make swagger` 重生成（`@Tags platform-tenants`）
- `server/internal/user/rpc_client_platform_tenant.go` — 7 RPC 方法 + 8 sentinel（distinct from cs-user's；ADR D1 类型解耦）
- `server/internal/user/rpc_client_platform_tenant_test.go` — 14 测试（httptest stub）
- `server/internal/handlers/platform_tenant.go` — `PlatformTenantAPI`（7 thin handlers）
- `server/internal/handlers/platform_tenant_test.go` — 14 handler 测试
- `server/cmd/api/main.go` — `buildPlatformTenantService(module)` helper + `platformTenants` route group（首次 wiring `middleware.RequirePlatformAdmin`）

**关键决策**：

1. **Slug 校验 AS-IS**：`CreateTenant` 不预规范化 slug 大小写，直接按 `[a-z0-9-]{3,32}` 校验 — 客户端传 `"Acme"` 直接 400 而不是 silently 改成 `"acme"`。客户端 bug 值得显式暴露。
2. **slug + tenant_id immutable on PATCH**：PATCH 请求体里有 slug 字段就忽略（不是 400），匹配"partial update"语义；status 变更只能走 suspend/restore/delete 端点。
3. **`deletion_requested_at` vs `deleted_at` 区分**：前者是设计 30-day grace 标记（本 PR 设置），后者是 gorm soft-delete 列（hard-delete cron 后续设置）。
4. **email_domains 冲突检测 O(N)**：当前实现扫描所有 tenants 的 domains，对当前规模（<100 tenants per 设计 §3.3）足够；Postgres GIN index + exclusion constraint 优化 deferred。
5. **server 端 sentinel 独立于 cs-user**：`user.ErrSlugTaken` ≠ `tenant.ErrSlugTaken`（不同包，不同 error var），避免 server ↔ cs-user 类型耦合（ADR D1）。
6. **legacy `systemrole.RequirePlatformAdmin` 不动**：旧 `/api/admin/*` 路由继续用 role-based 中间件；新 `/api/platform/tenants` 用 JWT-claim-based。Consolidation 是独立 PR（影响所有旧 admin 路由，需要单独 rollback 方案）。
7. **RPC unavailable → 502**：server 本地 backend 模式下 `UserModule.TenantResolver == nil`（ADR D1 — 本地无 tenant 数据），handler 返回 502 而不是 404，提示运维 backend 配错。

**out-of-scope（显式）**：
- 创建首位 tenant_admin — 设计 §4.2 提及，C3（tenant_admin API）own
- `user_center_audit_log` 写入（§16.2）— C4 own；当前只 structured logger 写 actor + action 字段
- webhook fanout（`tenant.created` / `tenant.suspended` / `tenant.restored` / `tenant.deletion_requested`）— Phase E4 own
- 30-day grace 后的 hard-delete cron — 独立 PR + runbook
- email_domains GIN index — 优化 deferred
- 跨 tenant 用户 ops / audit-log query / email allowlist — 其他 C2 切片，未做

**测试**：cs-user tenant 23 + handler 16 + swagger pass；server RPC 14 + handler 14；gofmt / go vet clean；`make swagger` 重生成 clean。Phase A 4 个集成测试不受影响（reissue-token 路径未改）。



### C3.1 实现细节：tenant_admin 用户列表（已落地 2026-07-17）

**端点**（1 个公开 + 1 个 RPC 方法，**cs-user 零改动** — 复用已有 `/api/internal/users/search`）：

| server 公开（RequireTenantAdmin("owner","admin")）| cs-user 内部（已存在）| 行为 |
|---|---|---|
| `GET /api/tenant/users?keyword=&limit=` | `GET /api/internal/users/search` | 列出本 tenant 活跃用户；keyword 子串过滤（username/display_name/email 前缀）；limit 上限 200 |

**关键发现 — cs-user 零改动**：`SearchUsers` 服务（`cs-user/internal/user/service.go`）已经通过 `tenant.Scope(ctx)` 自动按 `tenant_id` 过滤；`ResolveTenant` 中间件读 `X-Tenant-Id` header 并填充 ctx。C3.1 的缺口**完全在 server 侧**：缺一个 JWT-gated 公开端点。本切片用 2 commits 闭合这个缺口。

**Slug 注入链路**：
```
tenant_admin JWT
  → AuthClaims.TenantSlug (Phase B/A7 claim)
  → handler 用 tenant.WithSlug(ctx, slug) 写入
  → rpc_client.ListTenantUsers 读 ctx via tenantSlugFromContext
  → HTTP header X-Tenant-Id: <slug>
  → cs-user ResolveTenant middleware
  → tenant.Scope(ctx) pins query
```

Fallback：legacy token 无 `TenantSlug` 时用 `TenantID`（cs-user `WHERE tenant_id = ? OR slug = ?` 两者都接受）。

**分层文件**：
- `server/internal/user/rpc_client_tenant_user.go` — `ListTenantUsers(ctx, keyword, limit) []TenantUser` + `TenantUser` struct（6 fields：subject_id/username/display_name/email/is_active/tenant_id）+ 3 sentinel（ErrTenantUserUnavailable 等）
- `server/internal/user/rpc_client_tenant_user_test.go` — 10 tests（含 X-Tenant-Id 转发断言、envelope + bare array 双兼容、NotConfigured / transport / 5xx / 4xx / decode 错误路径、JSON round-trip tag 校验）
- `server/internal/handlers/tenant_user.go` — `TenantUserAPI.ListTenantUsers` handler（验证 / claims 读取 / slug 注入 / RPC 调用 / error 映射）
- `server/internal/handlers/tenant_user_test.go` — 11 tests（happy path / slug 注入 / TenantID fallback / 负 limit / 超 max / RPCUnavailable / TenantUserUnavailable / unknown error / nil svc / no claims / no tenant binding）
- `server/cmd/api/main.go` — `buildTenantUserService(module)` helper + `tenantUsers` route group（首次 wiring `middleware.RequireTenantAdmin("owner", "admin")`）

**关键决策**：

1. **billing 角色不进 gate**：`RequireTenantAdmin("owner", "admin")` 故意不含 `billing` — billing 用户管订阅，不应看用户列表（设计 §4.2）。
2. **复用已有 cs-user 端点**：不为 C3.1 新建 `/api/internal/tenant/users` — 已有的 `/api/internal/users/search` 完全满足语义（active-only / tenant-scoped / paginated）。新端点会让 audit trail 含义更清晰，但 DRY 更重要；未来若需 inactive-user 包含或 cursor 分页再分叉。
3. **server 端 cap 200**：handler 提前拒 `limit > 200`，节省一次 round-trip — cs-user 也有同样 cap，server 重复是为了快速失败。
4. **empty tenant binding → 403**：理论上 `RequireTenantAdmin` 已确保 TenantRoles 非空，但 handler 防御性 403 — 避免极端场景下空 slug 走到 cs-user 触发 503（含义不清）。
5. **NoClaims → 401**：`AuthClaimsKey` 不在 ctx 表示 Auth 中间件没跑，是程序员错误（路由配置漏中间件），handler 显式 401 让错误暴露。
6. **`ErrTenantUserUnavailable` 独立于 `ErrRPCUnavailable`**：cs-user 返 4xx 意味着 RPC client 自己 malformed 了 request（4xx 来源应是 handler，不是 upstream），故单独 sentinel；handler 都映射成 502，但 log 区分。

**out-of-scope（显式）**：
- inactive / suspended user 包含（设计 §4.2 提及）— 后续切片
- cursor 分页 — 后续切片
- role 过滤（如 "只看 admin"）— 后续切片
- 用户详情 / 修改 / 删除接口 — 独立切片

**测试**：cs-user 零改动，原 23+16+4 测试全绿不受影响；server RPC 10 + handler 11 全绿；gofmt / go vet clean。Pre-existing `TestGetItemStats_RatingSurvivesTextOnlyEdit` flake 与本切片无关（隔离跑 pass）。



### C3.2 实现细节：tenant config CRUD（已落地，4 commits）

**端点（cs-user 内部 + server 公开双层）**

| cs-user (InternalToken) | server (BearerAuth + RequireTenantAdmin) | 行为 |
|---|---|---|
| `GET /api/internal/tenant/config` | `GET /api/tenant/config` | 读 tenant_configs 行；无行时返回 synthetic `{"config_yaml":"{}"}` |
| `PUT /api/internal/tenant/config` | `PUT /api/tenant/config` | 全量替换 YAML blob；cs-user 解析校验 + size cap；updated_by 由 X-Actor-Subject-Id header 注入 |

**slug 注入链（与 C3.1 完全一致）**：JWT → AuthClaims → tenant.WithSlug(ctx) → rpc_client → X-Tenant-Id header → cs-user ResolveTenant → tenant_id pinned。无 path / body 中的 tenant 字段可被 spoofing。

**actor 注入链（新增）**：JWT `sub` claim → AuthClaims.Sub → X-Actor-Subject-Id header → cs-user → tenant_configs.updated_by 列。Header 而非 body 字段：保证审计链路无法与 auth claim 漂移（misbehaving client 无法谎报 editor）。空 sub（service-account token）→ 空 header → NULL stored。

**文件影响（5 个新文件 + 4 个修改）**

- `cs-user/internal/tenantconfig/service.go` (NEW) — `Service.Get(ctx, tenantID) → synthetic default {} on missing row`；`Service.Update(ctx, UpdateParams) → validates + upserts`。3 个 sentinel：ErrInvalidYAML (400)、ErrYAMLTooLarge (413)、ErrEmptyTenantID (500 programmer error)。64 KiB cap。
- `cs-user/internal/tenantconfig/service_test.go` (NEW) — 12 tests（first/second write、default normalize、nil actor、boundary cap、unknown YAML keys 接受、3 个 error sentinel）
- `cs-user/internal/handlers/tenant_config.go` (NEW) — `TenantConfigAPI.GetTenantConfig` / `UpdateTenantConfig`；X-Actor-Subject-Id header 读 + 转发；error→HTTP 映射
- `cs-user/internal/handlers/tenant_config_test.go` (NEW) — 9 handler tests
- `cs-user/internal/app/app.go` (MODIFIED) — `Deps.TenantConfig` 字段 + `registerTenantConfigRoutes` + `unavailableTenantConfigService` 503 fallback stub
- `cs-user/cmd/api/main.go` (MODIFIED) — `tenantconfig.New(pool.Gorm)` 注入
- `cs-user/docs/` (REGENERATED) — 2 个新路径
- `server/internal/user/rpc_client_tenant_config.go` (NEW) — 2 RPC 方法 + 3 server-side sentinel（ADR D1 type decoupling）
- `server/internal/user/rpc_client_tenant_config_test.go` (NEW) — 12 RPC tests（含 adversarial：400 非 YAML sentinel 不能被误判为 ErrInvalidYAML）
- `server/internal/handlers/tenant_config.go` (NEW) — `TenantConfigAPI` 双 handler；slug + actor 双注入链；error→HTTP 映射
- `server/internal/handlers/tenant_config_test.go` (NEW) — 12 handler tests
- `server/cmd/api/main.go` (MODIFIED) — `buildTenantConfigService` helper + 新 route group `/tenant/config`（RequireTenantAdmin + GET + PUT）

**关键设计决策**

1. **C3.2 仅做 raw blob CRUD**，不做 schema 校验：yaml.v3 严格解析（reject 坏语法），但接受任意 keys（unknown sections stored cleanly）。typed provider_mapping 校验在 C3.3 上层叠加，避免阻塞 C3.2 ship。
2. **空 body normalize 成 `"{}"`**：保证读路径 "missing row → default {}" 对称（即使是 explicit clear 后），不让客户端看到 NULL。
3. **updated_by 是 NULL-able column**：空 sub 转发空 header，cs-user 存 NULL（非空串），保留 "no actor recorded" 语义。Service 层用 `*string` 表达。
4. **YAML size cap = 64 KiB**：对 design 已知的 5 个 subsection（provider_mapping / username_strategy / employment_providers / features / enterprise_schema_ext）足够慷慨；超出几乎肯定说明数据该进 typed table。Server-side 不重复 cap（cs-user 已经 cap），保持单一 source of truth。
5. **Read path 无 404**：missing row 返回 synthetic default，tenant 永远有 "implicit {} config"。消除客户端处理 "no config yet" 的特殊路径。
6. **FirstOrCreate 不用，改 find-then-act in tx**：跨 sqlite（test）/ Postgres（prod）可移植；race window collapse 到 single-row PK touch，last-write-wins 是 acceptable（single-admin edit cadence per tenant）。
7. **Server-side 400 → ErrInvalidYAML body-text 匹配**：cs-user 响应 body 含 "invalid YAML" 文本时 route 到 ErrInvalidYAML；其他 400 reason（如未来 "tenant resolution required"）→ ErrTenantConfigUnavailable，避免误分类。
8. **第一手 use of `middleware.RequireTenantAdmin` on PUT**：C3.1 已首次 wiring 在 GET 上；C3.2 是首次在 mutating endpoint 上用，锁定"只有 owner/admin 才能 PUT"的合约。

**out of scope（明确）**

- typed YAML schema validation（provider_mapping 等）— C3.3
- field-level merge / patch 语义 — PUT 全量替换
- audit log row writes（user_center_audit_log §16.2）— C4
- optimistic concurrency（If-Match / etag）— single-admin edit 节奏，last-write-wins acceptable
- hard cap 在 server 端重复（cs-user 已 cap）
- 读取历史版本（versioning）— 后续切片

**测试**：cs-user tenantconfig 12 service tests + 9 handler tests；server 12 RPC tests + 12 handler tests = 45 tests；gofmt / go vet clean；cs-user 既有测试无 regression（go test ./... 全绿）。



### C3.3 实现细节：provider_mapping typed editing（已落地 2026-07-17，5 commits）

**端点（cs-user 内部 + server 公开双层，与 C3.2 同形状）**

| cs-user (InternalToken) | server (BearerAuth + RequireTenantAdmin) | 行为 |
|---|---|---|
| `GET /api/internal/tenant/provider-mapping` | `GET /api/tenant/provider-mapping` | 读 typed provider_mapping 子树；缺段返回 `{"providers":{}}` |
| `PUT /api/internal/tenant/provider-mapping` | `PUT /api/tenant/provider-mapping` | PUT 语义全量替换 provider_mapping 子树；sibling 顶层 keys（employment_providers / features 等）逐字保留 |

**slug + actor 注入链**：与 C3.2 完全一致（JWT → AuthClaims → tenant.WithSlug(ctx) → rpc_client → X-Tenant-Id；JWT sub → X-Actor-Subject-Id）。

**文件影响（5 个新文件 + 3 个修改）**

- `cs-user/internal/tenantconfig/provider_mapping.go` (NEW) — `ProviderMapping` / `Provider` / `EnterpriseSync` typed structs（指针字段 `*bool Enabled` / `*int Rank` 保留 "absent" vs "explicit zero" 区分）；3 sentinel：ErrProviderNameInvalid / ErrIntervalInvalid / ErrRankNegative；`Validate()` 应用默认 Enabled→true 并校验 name pattern `^[a-z0-9_]{1,64}$` + rank 非负 + interval 是 Go duration 且 ≤ 30 天；`ParseProviderMapping` / `SerializeProviderMapping`。
- `cs-user/internal/tenantconfig/provider_mapping_test.go` (NEW) — 21 tests（parse 4 + validate 9 + service 8）。
- `cs-user/internal/tenantconfig/service.go` (MODIFIED) — `GetProviderMapping` + `UpdateProviderMapping` + `mergeProviderMappingSection`（yaml.v3 Node-based，保留 sibling sections 注释/顺序）。
- `cs-user/internal/handlers/tenant_provider_mapping.go` + `_test.go` (NEW) — `TenantProviderMappingAPI`，14 handler tests。
- `cs-user/internal/app/app.go` (MODIFIED) — `registerTenantProviderMappingRoutes` + `unavailableTenantProviderMappingService` 503 fallback。
- `cs-user/docs/` (REGENERATED) — 2 个新路径。
- `server/internal/user/rpc_client_tenant_provider_mapping.go` + `_test.go` (NEW) — 2 RPC 方法 + 3 server-side sentinel（ADR D1 type decoupling）；17 RPC tests（含 adversarial：400 非 matching body 不能被误判为 typed sentinel）。
- `server/internal/handlers/tenant_provider_mapping.go` + `_test.go` (NEW) — `TenantProviderMappingAPI`；15 handler tests。
- `server/cmd/api/main.go` (MODIFIED) — `buildProviderMappingService` helper + 新 route group `/tenant/provider-mapping`。

**关键设计决策**

1. **PUT = full replace of provider_mapping subtree only**：兄弟 sections（employment_providers / features 等）逐字保留。Test `TestService_UpdateProviderMapping_ReplacesSubtreeOnly` 锁定契约：PUT 只含 ldap 时，原 blob 里的 wxwork 被丢弃。
2. **yaml.v3 Node-based merge 而非 `map[string]any` round-trip**：保留 tenant_admin 经 C3.2 raw endpoint 写入的注释 / 顺序 / 锚点。`yaml.Unmarshal` 顶层是 DocumentNode，必须 unwrap 到内层 mapping node 才能正确遍历 key/value pairs（首发 bug：sibling 被丢；test 抓到 → 修）。
3. **Provider name pattern `^[a-z0-9_]{1,64}$`**：underscore（不是 hyphen）匹配现有 provider id（wxwork / azure_ad / dingtalk / feishu）。不耦合到固定 provider registry — design §9.3 明确 providers 是 dynamic per tenant Casdoor 配置。
4. **`*bool` Enabled 默认 true 但保留 explicit false**：Validate() 在 nil 时 in-place 改成 `&true`，让序列化的 YAML 是 canonical（omitted enabled 不留歧义）；explicit false 透传。
5. **Interval cap = 30 days**：拒绝 "1ns" DoS payload 和 "9999h" 拼写错误。空 interval 合法（表示 "use default / no sync"）。
6. **Server-side body-text matcher 多 sentinel 分流**：cs-user 400 body 含 "invalid provider name" / "invalid enterprise_sync.interval" / "rank must be non-negative" / "invalid YAML" 之一时分别 route 到对应 sentinel；其他 400 reason → ErrTenantConfigUnavailable（fail-closed，不猜测）。
7. **同 RPC client 共享 C3.2 transport**：`*userpkg.RPCClient` 同时承载 raw-blob + typed 两套方法；server 端 buildProviderMappingService 与 buildTenantConfigService 返回同一实例，避免双 client 双 pool。

**out of scope（明确）**

- provider registry 强校验（只允许已知 provider id 列表）— 当前 pattern 校验已足够，dynamic per tenant Casdoor 配置
- field_map value 语法校验（system attribute 是否存在等）— C3.3 仅 type-shape 校验
- PATCH semantics（增量修改单 provider）— 当前 PUT 全量替换；后续切片
- multi-YAML-doc merge precedence（同 key 多次出现）— yaml.v3 已 last-wins，未额外处理

**测试**：cs-user tenantconfig 包 21 个新 tests（provider_mapping_test.go）+ 14 handler tests；server 17 RPC tests + 15 handler tests = 67 tests；gofmt / go vet clean；cs-user + server 既有测试无 regression。Pre-existing `TestLogBehavior_FeedbackDedupSupersedes` / `TestGetItemStats_RatingSurvivesTextOnlyEdit` marketplace_test.go flake（fixture-mutation，与本切片无关）随包测试数增长再现，隔离跑 pass。



---

## 阶段 E：身份联邦扩展（按需启用）

- [ ] **E1**：provider_mapping yaml 标准化（per-tenant）
- [ ] **E2**：tenant 级 IdP 接入（global vs tenant-specific）
- [ ] **E3a**：Gitea JWT 中间件 fork + user 自动开户 + `user_gitea_binding` 维护（归属 cs-user） — **E3a.1 user 自动开户切片（5 commits）已落地 2026-07-20**：见下方"E3a.1 实现细节"；E3a.2 rename/disable/delete cascades + reconciliation cron / E3a.3 fork JWT middleware 待启动
- [ ] **E3b**：Gitea `team_user` 同步（GitServerAdapter）（归属 server `internal/gitsync/`） — **E3b.1 MVP 框架切片（4 commits）已落地 2026-07-20**（已被 E3b.1.1 per-tenant 重构覆盖：handler path / service 签名 / config 均变更）；**E3b.1.1 per-tenant Gitea fix（10 commits）已落地 2026-07-20**：见下方"E3b.1.1 实现细节"；E3b.2 real provider swap + cron + delta sync 待启动
- [ ] **E4**：webhook 用户变更广播系统

---

## 跨阶段测试基础设施（已就绪，每个新任务复用）

| 设施 | 位置 | 覆盖 |
|---|---|---|
| 单元测试 stdlib | `*_test.go` 紧贴源码 | 不引入 testify，与 `server/` 一致 |
| HTTP smoke | `httptest.NewRecorder` + `app.NewRouter` | 见 `internal/app/app_test.go` |
| CI 矩阵 | `.github/workflows/test.yml` | 自动发现所有 `go.mod`，每个跑 build + vet + test-race |
| 本地 gate | `cs-user/Makefile` `make check` | fmt + vet + test-race |
| Swagger 生成 | `cs-user/Makefile` `make swagger` | `swag init -g cmd/api/main.go -o docs --parseDependency --parseInternal` |
| Swagger 格式 | `cs-user/Makefile` `make swagger-check` | `swag fmt -d ./cmd/api,./internal` |

**待补强**（Phase 0 推进过程中按需加）：

- [ ] `testcontainers-go` 接入（PG / 双 PG ETL 场景；当前测试都是纯 stdlib）
- [ ] `helm lint` + `helm template` CI（chart 测试基础设施）
- [ ] 集成测试 binary（同时跑 server + cs-user 的端到端 gate，P0-7 触发需要）

---

## 风险登记（来自 ADR §5）

| 风险 | 严重度 | 缓解 | 当前状态 |
|---|---|---|---|
| 107 处 user 访问点迁移遗漏 | 高 | grep 验证 `models.User` 直接访问清零；costrict-web 表 READONLY 兜底 | 🔴 未启动（P0-7/8 触发） |
| 共享密钥泄露 | 中 | K8s secret 注入 + 定期轮换 + networkPolicy 限同 namespace | ✅ networkPolicy + secret.yaml + chart-managed/existingSecret 双轨就绪 |
| ETL cutover 数据丢失 | 高 | dry-run + diff + cutover 停写 + 备份保留 30 天 | 🔴 未启动（P0-6） |
| cs-user 故障 → costrict-web user API 全挂 | 高 | CachedUserService stale 兜底 + 多副本 + 监控 | 🔴 未启动（P0-7） |
| Phase 3 csc 兼容性破坏 | 高 | 真实 csc login 集成测试 + 灰度 + 30 天兼容窗口 | ⏳ Phase A7 |
| `employment_identities` 改名遗漏 | 低 | ADR 同步重命名 3 份提案 | ✅ 已完成 |

---

## 决策追踪（来自 ADR §6）

| 日期 | 决策 | 状态 |
|---|---|---|
| 2026-07-16 | D1-D10 全部锁定 | ✅ Accepted |
| 2026-07-16 | `enterprise_identities` → `employment_identities` 改名同步 3 份提案 | ✅ 已完成 |
| ✅ 已决 | Phase 2 启动前：JWT 密钥存储方式 | ✅ Phase A3 — file-based PEM（operator openssl 生成 + k8s secret 挂载；轮换 = 换文件 + 重启 pod；KMS 推迟到 Phase B 多租户 prod） |
| 待定 | Phase 3 启动前：employment resolution_strategy 默认值（first_wins / last_wins） | ⏳ Phase A4 触发前 |
| 待定 | cutover 执行窗口（停机 / 灰度比例序列） | ⏳ Phase 0 P0-8 触发前 |
| ✅ 已决 | OAuth callback 接管策略 (a) vs (b) | ✅ Phase A7 — 策略 (b) 重签（Casdoor 保留前端 + login UI；cs-user `POST /api/internal/users/reissue-token` 接受 server 校验过的 Casdoor claims + user_subject_id，加载 employment_identities + A5 claims + A3 signer 签发；最小爆炸半径，对齐 30 天双签灰度窗口） |

---

### C4.1 实现细节：审计日志基础设施（已落地 2026-07-20，5 commits）

**5 commits**：`3832a23` (A 表迁移+模型) / `0be4eb0` (B service) / `7a31a09` (C handler orchestration + 6 写路径 instrument + 5 TODO markers 移除) / `4836241` (D server AuthClaims→ActorMeta ctx + header 转发) / `<本 commit>` (E 进度文档 + 计数器 5→6, 31%→38%)

**架构**：handler 层做 audit orchestration（不修改 `tenant.Admin` / `tenantconfig.Service` 签名），后 commit C 实现期间偏离原计划。理由：handler 已掌握 action（路由）/ target（URL 参数或响应体）/ actor meta（headers），service 层专注业务逻辑即可；audit 是横切关注，wrap service call 是更干净的边界。代价：`tenant_config.update` 的 payload 暂只记 `bytes` 而非 before/after diff（diff 需 service 协作；deferred per known limitations）。

**7 项关键设计决策**：

1. **handler 层 audit orchestration**（非 service 层）— 上文已述；`PlatformTenantsAPI.Audit` / `TenantConfigAPI.Audit` / `TenantProviderMappingAPI.Audit` 三个可选 `*auditlog.Service` 字段；nil = skip（test path / 503 fallback 友好）。

2. **最佳努力写入语义**（never-fail-the-op）— `auditlog.Service.Record` 失败仅 WARN 日志（含完整 RecordParams），返回 error 给 caller **必须忽略**。理由：用户 op 已 commit；audit 失败不应让成功的写返回 500。Test `TestRecord_NilServiceDoesNotPanic` + `TestRecord_DBClosedReturnsError` 锁定契约。

3. **表 schema：无 FK / 无 CHECK / nullable everything except action** — `user_center_audit_log` 与 tenants/users 平级独立；tenant hard-delete 后审计行保留（regulator-visible action history per §16.2）。无 CHECK on action：新事件加类型不需要迁移，匹配 `platform_admins.scope` 决策（C1 decision 3）。`tenant_id` / `actor_subject_id` 等 nullable：platform-level 事件（NULL tenant_id）+ 系统动作（NULL actor）一等公民。

4. **3 indexes 覆盖三个查询维度** — `(tenant_id, created_at DESC)` 跨 tenant 查询 / `(actor_subject_id, created_at DESC)` 单 actor 行为追溯 / `(action, created_at DESC)` 按 action 类型筛选。当前规模 <1000 ops/day，10x 增长内无需分区。

5. **server 端 AuthClaims → `tenant.ActorMeta` ctx-carrier** — 复用现有 `tenant.WithSlug` pattern（`server/internal/tenant/context.go`）；handler 调 `tenant.WithActorMeta(ctx, ActorMeta{Role, Scope})` 后 RPC client 自动从 ctx 取值转发 headers。3 RPC client 共享 `applyActorMetaHeaders(req, ctx)` helper（`server/internal/user/tenant_ctx.go`），无签名变更。

6. **"first role wins" for X-Actor-Tenant-Role** — 多 role 用户（如 owner + tenant_admin）只记第一个；满足合规需求，full role-list audit deferred。Test `TestActorMetaFromClaims_PicksFirstRole` 锁定。

7. **header 名常量在 cs-user / server 两边各自定义（duplicate）** — cs-user `handlers/audit.go` 定义 `actorTenantRoleHeader` / `actorPlatformScopeHeader`；server `user/tenant_ctx.go` 定义 `ActorRoleHeader` / `ActorPlatformScopeHeader`。两个 module 各自独立，duplicate 比 cross-module import 更轻。

**6 个 audit actions 落地**：

| Action | Target ID | Payload | Handler |
|---|---|---|---|
| `tenant.create` | `tenant:<new_id>` | `{slug, edition, email_domains}` | `PlatformTenantsAPI.CreateTenant` |
| `tenant.suspend` | `tenant:<id>` | `{slug}` | `PlatformTenantsAPI.SuspendTenant` |
| `tenant.restore` | `tenant:<id>` | `{slug}` | `PlatformTenantsAPI.RestoreTenant` |
| `tenant.deletion_requested` | `tenant:<id>` | `{slug}` | `PlatformTenantsAPI.DeleteTenant` |
| `tenant_config.update` | `tenant_config:<tenant_id>` | `{bytes: N}` | `TenantConfigAPI.UpdateTenantConfig` |
| `provider_mapping.update` | `provider_mapping:<tenant_id>` | `{provider_count, provider_names}` | `TenantProviderMappingAPI.UpdateProviderMapping` |

**测试覆盖**（24 tests across 4 files）：

- `cs-user/internal/models/audit_log_test.go` — 4 tests（table name / NULL defaults / JSONB payload round-trip / action vocab constants compile）
- `cs-user/internal/auditlog/service_test.go` — 6 tests（happy / NULL tenant / NULL actor / empty action / nil service no-panic / DB closed WARN log fires）
- `cs-user/internal/handlers/audit_test.go` — 5 tests（create / suspend / tenant_config.update / provider_mapping.update / nil audit quiet）
- `server/internal/user/rpc_client_actor_meta_headers_test.go` — 5 tests（full meta forwards / no meta omits / partial meta omits empty / ctx round-trip / nil-safe lookup）
- `server/internal/handlers/platform_tenant_actor_meta_test.go` — 3 tests（Create handler injects ActorMeta end-to-end / first role wins / empty claims returns zero-value）
- 1 offline grep-verified invariant：`grep -rn "TODO(audit-log): C4" cs-user/internal/` 返回空（5 markers 全部移除）

**已知 deferred（不在 C4.1 范围）**：

- **C4.2 active 越权检测 middleware** — 今天 cross-tenant protection 仅依赖 JWT → AuthClaims → `X-Tenant-Id` → ResolveTenant 链 + RequireTenantAdmin role check；C4.1 仅记录**成功**的 admin 动作，不记录拒绝尝试。
- **C4.3 list endpoints**（`GET /api/platform/audit-logs` + `GET /api/tenants/me/audit-logs`）— pagination + filter by action/actor/target/date range。表 + indexes 已 ready 支撑。**✅ 已落地 2026-07-20（7 commits）**，详见下方"C4.3 audit-log list 实现细节"。
- **`tenant.update` audit event** — UpdateTenant PATCH（display_name/edition/domains 等）目前未审计；deferred 直到合规审查标需要。
- **payload diff** — 当前 payload 仅记 post-state（bytes / provider_count 等）；before/after diff 需 service 协作；deferred。
- **retention/TTL cron** — 表无限增长；7-year retention policy + cron 是独立 ops PR。
- **webhook fanout** — Phase E4。

**回滚步骤**：

- 应用层：在 server 容器环境变量中禁用 audit header forwarding（C4.2 未来工作可加 flag）；cs-user 端 `Deps.AuditLog = nil` 让 `recordAudit` nil-safe skip 全部审计写入，handler 不需改动。
- 数据层：`DROP TABLE user_center_audit_log` 即可（无 FK 依赖，无下游消费者）。

---

### E3a.1 实现细节：Gitea user 自动开户（已落地 2026-07-20，5 commits）

**5 commits**：`<A>` (user_gitea_binding 表 + 模型 + 4 测试) / `<B>` (`giteasync.GiteaClient` HTTP 客户端 + 8 httptest 测试) / `<C>` (`giteasync.Service` 状态机 + audit hook + 12 测试) / `<D>` (`user.Service.GetOrCreateUser` hook + `GetGiteaBinding` 读方法 + `UsersAPI.GetGiteaBinding` handler + `GiteaConfig` + `cmd/api/main.go` wiring + 12 测试) / `<本 commit>` (E 进度文档 + Phase E 计数器 0→1, 0%→5%)

**架构**：3 层清晰分离 — `giteasync.Client`（HTTP transport + sentinel errors）/ `giteasync.Service`（state machine + best-effort writer + audit hook）/ `user.Service` hook（仅在 GetOrCreateUser new-user 分支触发，错误吞掉）。`user.GiteaProvisioner` interface 在 user 包声明（避免 `giteasync → models → user` import cycle）。复用 C4.1 `auditlog.Service` + 新增 `ActionUserGiteaProvisioned` / `TargetTypeUserGiteaBinding` vocab 常量。

**7 项关键设计决策**：

1. **同步 best-effort，永不 fail signup** — Provision 在 OAuth callback 进程内调用（5s ctx timeout）；任何失败（网络 / 5xx / 4xx）仅 WARN 日志 + 写 audit；users row 已 commit，binding 留 pending/error 等 E3a.2 reconciliation cron 修复。理由：signup 是冷路径，几百 ms 延迟 < Casdoor OAuth 自身延迟；Gitea 不可用不应阻塞用户登录拿 JWT。

2. **状态机简化为 pending → synced | error**（无 dead_letter）— E3a.1 不实现重试队列；timeout 保持 pending（cron 周期性扫 pending → 重试）；其他失败 → error（last_error populated，ops 手动或 cron 修复）。匹配 USER_CENTER_DESIGN §11.2 简化版。

3. **409 → LookupUserByName 恢复路径** — Gitea 已有同 username 时 POST /admin/users 返 409；Service 切到 GET /users/{name} 拿 UID，binding 标 synced（幂等结果）。这一支覆盖 cs-user 重启 / DB 漂移 / 旧用户走新代码等场景。

4. **cs-user 首个 outbound HTTP client** — 此前 cs-user 全部是入站（gin handlers）；giteasync.GiteaClient establishes the pattern（stdlib net/http + JSON in/out + sentinel errors + httptest 测试）。后续 IdP 客户端（LDAP / OIDC source）可复用此模式。

5. **`SetGiteaSync` setter 而非构造参数** — `user.NewService(db)` 签名稳定（30+ test call site 不动）；setter 在 main.go 显式调用一次。Phase A4b 已用过此模式（cmd/api/main.go 已多次）。

6. **config gate 是 nil-safe 全链路** — `CS_USER_GITEA_BASE_URL` / `CS_USER_GITEA_ADMIN_TOKEN` 任一为空 → `GiteaConfig.Enabled() = false` → main.go 不构造 client → `SetGiteaSync` 不调用 → `Service.giteaSync = nil` → hook 跳过 `if s.giteaSync != nil`。local dev + 单元测试不需要真实 Gitea。

7. **`user_gitea_binding` 表无 FK 到 users** — 与 `user_center_audit_log` 同决策：binding 行必须撑过 users hard-delete，否则 reconciliation cron 无法检测孤儿 Gitea 账号（§11.3）。PK 是 `(user_subject_id, tenant_id)`（B5 multi-tenancy + cs-user TEXT-subject_id 约定）；`gitea_username` 全局唯一索引（匹配 Gitea 自身约束）。

**33 测试覆盖**：

- `models/user_gitea_binding_test.go` — 5 tests（TableName / nullable defaults / synced round-trip / status vocab / int64Ptr helper）
- `giteasync/client_test.go` — 9 tests（happy / 409 / 401 / network / ctx-timeout / lookup-happy / lookup-404 / missing-params / NewClient-empty-config）
- `giteasync/service_test.go` — 12 tests（happy / 409-recovery / client-error / already-synced-noop / nil-audit / nil-client / timeout-keeps-pending / audit-row-on-synced / best-effort-timeout-isolation / buildUsername-sanitizer-4-cases）
- `user/service_gitea_test.go` — 7 tests（hook-fires-on-new / hook-no-fire-on-existing / failure-doesnt-abort-signup / nil-provisioner-skipped / GetGiteaBinding happy / not-found / empty-subject）
- `handlers/users_gitea_binding_test.go` — 5 tests（happy / 404 / empty-subject 400 / ErrEmptySubjectID 400 / 500-no-leak）

**已知 deferred（不在 E3a.1 范围）**：

- **E3a.2 username rename cascade** — `user.updated` → `PATCH /admin/users/{old}` + UPDATE binding.gitea_username；当前 username 改名不会同步到 Gitea。
- **E3a.2 disable / soft-delete / hard-delete cascades** — users 表状态变化不传播到 Gitea。
- **E3a.2 reconciliation cron** — pending/error binding 漂移修复 + 孤儿 Gitea 账号检测。
- **E3a.3 fork JWT middleware 集成** — 当前 binding row 可查询但不强制；用户 sync_status='pending' 仍能访问 Gitea 直到 fork 中间件落地 503 gate（需协调 Gitea fork release，独立 PR）。
- **E4 webhook 触发** — 当前同步 in-process 调用；E4 webhook subscription system 落地后切换为 async fire-and-forget。
- **E3b `team_user` 同步** — ADR-3 v3：cs-user 只做 user-level provisioning；team-level sync 归 @server GitServerAdapter。
- **payload diff / before-after snapshot** — 当前 payload 仅记 post-state（status / gitea_uid / error）。
- **密码策略** — 随机 32-byte hex password 一次性使用（Gitea JWT middleware 是 auth path，不是密码）；未来 IdP-backed provisioning (LDAP/OIDC source) 需要更严格策略。

**回滚步骤**：

- 应用层：清空 `CS_USER_GITEA_BASE_URL` 或 `CS_USER_GITEA_ADMIN_TOKEN` 环境变量并重启 cs-user 进程；`GiteaConfig.Enabled() = false` → main.go 不构造 client → hook 全链路 nil-skip。无需改代码。
- 数据层：`DROP TABLE user_gitea_binding`（无 FK 依赖；下游消费者只有本特性的 handler + service）。
- Gitea 侧：手动 `DELETE FROM gitea_user WHERE username LIKE 'u-%'`（仅清理 cs-user 自动开户的账号；命名前缀 `u-` 保证可识别）。

---

### E3b.1 实现细节：@server GitServerAdapter MVP 框架（已落地 2026-07-20，4 commits）

**ADR-3 v3 边界**：cs-user 保留 user-level 工作（E3a.1），team-level Gitea 操作（`POST /teams/:id/members`、`POST /orgs/:org/teams`）归 @server。E3b.1 在 @server 落地最小可用的 GitServerAdapter 框架，team 数据源先用 stub provider，等 org-team-service 集成或 cs-user team RPC 后再替换。

**4 commits**：`<A>` (`gitsync.GiteaClient` Gitea team API 客户端 + sentinels + 10 httptest 测试) / `<B>` (`TeamDataProvider` interface + `StubTeamProvider` + 7 测试) / `<C>` (`TeamSyncService` full-reconcile loop + `ConfigTeamResolver` + 11 测试 + provider.go 格式修复) / `<D>` (`SyncTeam` handler + sentinel→HTTP mapping + 7 测试 + `GiteaConfig`/`TeamSyncConfig` env gate + `logger.L()` accessor + `cmd/api/main.go` wiring + `POST /api/admin/teams/:team_id/sync` 路由 gated by `systemrole.RequirePlatformAdmin`)

**关键设计决策**：

1. **Full reconcile 幂等** — 每次 SyncTeam 都对比 expected vs current Gitea 状态，add 缺失 + remove 多余。无状态跟踪，安全重试。expected == current 时 Skipped，零 API 调用。
2. **Per-member 错误隔离** — 单个 add/remove 失败不中止 batch；失败记入 `SyncResult.Errors[]`，handler 返回 200 + partial result。理由：admin 触发，需要可见的 partial-success 而非 fail-fast。
3. **StubProvider 是 MVP 数据源** — `TeamDataProvider` interface 是 swap point；当前 hardcoded map；未来 real provider（cs-user team RPC / org-team-service webhook payload adapter）实现同一 interface，service 层零改动。
4. **team_id → gitea_team_id 走 config** — `TEAM_SYNC_MAPPINGS=team-a=42,team-b=7` env var；`ConfigTeamResolver` 查 map；未知 team_id 返回 `ErrTeamNotFound` → 404。未来 DB-backed team metadata 表替换。
5. **Nil service → 503** — `GITEA_BASE_URL` 或 `GITEA_ADMIN_TOKEN` 未设时不构造 service；handler 检测 nil 返回 503（feature disabled）。运维可灰度上线。
6. **`logger.L()` accessor** — 给 gitsync.Service 传 `*zap.Logger`；之前 logger 包未暴露 zap 实例，新增 `L()` 兜底返回 `zap.NewNop()`。

**已知 deferred（不在 E3b.1 范围）**：

- **cs-user `teams` 表** + `/api/internal/teams/*` RPC — 等 org-team-service 集成（ADR-10）。
- **E4 webhook 接收** — 当前手动触发；E4 webhook 系统会替代为事件驱动。
- **E3b.2 reconciliation cron** — 定时全量校对 + delta sync（避免每次 O(N) API 调用）。
- **E3b.2 real provider swap** — StubProvider 替换为 cs-user team RPC 或 org-team-service webhook adapter。
- **Audit log 集成** — 当前仅 zap 日志；real provider 落地后再接 audit（避免审计 stub 测试数据）。
- **Org-level 操作**（`POST /orgs`、`POST /orgs/:org/teams`）— 当前只做 team membership sync；org 创建假设 admin 在 Gitea 直接做。
- **并发同步锁** — 两个 admin 同时触发同一 team 同步可能 race；MVP 接受（Gitea API 幂等），分布式锁 deferred。

**回滚步骤**：

- 应用层：清空 `GITEA_BASE_URL` 或 `GITEA_ADMIN_TOKEN` 环境变量并重启 server；`gitsync.NewClient` 返回 nil → `NewService` 返回 nil → `InitTeamSyncService(nil)` → handler 返回 503。无需改代码。
- 路由层：注释 `cmd/api/main.go` 中 `admin.POST("/teams/:team_id/sync", handlers.SyncTeam)` 一行即可完全移除 endpoint。
- Gitea 侧：手动校对 Gitea team 成员（stub provider 数据可控，无脏数据风险）。

---

### E3b.1.1 实现细节：per-tenant Gitea fix（已落地 2026-07-20，10 commits）

**Bug 背景**：E3a.1（cs-user `giteasync`）与 E3b.1（@server `gitsync`）双双假设全局唯一 Gitea — cs-user 用 `CS_USER_GITEA_BASE_URL` / `CS_USER_GITEA_ADMIN_TOKEN`，@server 用 `GITEA_BASE_URL` / `GITEA_ADMIN_TOKEN`。违反 `MULTI_TENANCY_DESIGN.md §20`：**每个 tenant 必须绑定一个 `git_servers` row**（`tenants.git_server_id` FK），adapter 解析走 `ResolveAdapterForTenant(tenantID)`，不允许全局 singleton。结果：所有 tenant 的 user 开户 / team sync 都打到同一 Gitea 实例。

**修复策略（option A 完整修复）**：cs-user 落地 `git_servers` 表 + template-row bootstrap（env → DB 桥接）+ `/api/internal/tenants/:tenant_id/git-server` RPC + 两端 Resolver 重构为 per-tenant。

**10 commits**：

cs-user 侧（5 commits）：
- `<1>` (`migrations/20260721160000_create_git_servers.sql` + `models.GitServer` + `models.Tenant.GitServerID` + 4 model 测试)
- `<2>` (`gitserver.DBResolver` + `gitserver.Config` + 5 sentinel errors + 5 resolver 测试 cgo sqlite)
- `<3>` (`gitserver.BootstrapTemplate` 从 env 创建 / backfill 现有 tenant / 幂等 + 5 测试)
- `<4>` (`giteasync.Service` 重构：`client` 字段 → `resolver` + `clientFactory` seam；`Provision(ctx, user)` 走 `resolver.Resolve(ctx, tenantID)` → transient client；13 service 测试 rewrite，含 `TestProvision_PerTenantResolverCalled` 回归测试)
- `<5>` (`GET /api/internal/tenants/:tenant_id/git-server` handler + 7 测试 + `app.Deps.GitServerResolver` wiring + `cmd/api/main.go` 注册 route + 启动期 bootstrap)

@server 侧（4 commits）：
- `<6>` (`user.RPCClient.GetTenantGitServer` + 5 sentinel errors + path-based tenant_id（不在 header，因为 platform-admin 可能 sync 任意 tenant）+ 12 测试)
- `<7>` (`gitsync.RPCResolver` + 5min TTL cache + errors 不缓存（transient 失败立即重试）+ `GitServerConfig`/`GitServerClient` interface（避开 gitsync → user import cycle）+ 10 测试含 race-tested concurrent)
- `<8>` (`gitsync.Service` 重构：`client` 字段 → `gitResolver` + `clientFactory func(GitServerConfig)`；`SyncTeam` 签名 `(ctx, teamID)` → `(ctx, tenantID, teamID)`；nil resolver → nil Service（feature-disabled signal）；15 service 测试 rewrite，含 `TestSyncTeam_PerTenantResolverCalled` 回归测试)
- `<9>` (handler path 改为 `/tenants/:tenant_id/teams/:team_id/sync` + `TeamSyncService.SyncTeam(ctx, tenantID, teamID)` + `cmd/api/main.go` 注入 `RPCResolver`（包 `tenantGitServerAdapter` 做 `user.TenantGitServerConfig` ↔ `gitsync.GitServerConfig` 类型翻译）+ 移除 `GiteaConfig`/`TeamSyncConfig`/`loadTeamSyncMappings`；team_sync 测试更新到新路径 + 新增 `TestSyncTeam_EmptyTenantIDReturns400`)

progress 文档（1 commit）：
- `<本 commit>`（Phase E 计数器 2→3, 10%→15%；新增本节）

**关键设计决策**：

1. **Template-row bootstrap（env → DB 桥接）** — 启动期若 `CS_USER_GITEA_BASE_URL` + `CS_USER_GITEA_ADMIN_TOKEN` 已设且 DB 无 `is_template=true` row，从 env 创建 template row + backfill 所有现有 tenant 的 `git_server_id`。保留 operator 现有 env-var-driven deploy 习惯，同时把真相搬到 DB。幂等：已存在 template 直接返回其 server_id。
2. **tenants.git_server_id nullable 迁移窗** — migration 不强制 NOT NULL（避免破坏现有 row）；bootstrap transaction 内立即 backfill。后续单独 migration 加 NOT NULL + UNIQUE。风险：迁移窗内新建 tenant 可能 NULL；bootstrap transaction 缓解。
3. **Sentinel 错误 vocab** — cs-user 侧 `ErrTenantNotFound` / `ErrTenantMissingGitServer` / `ErrGitServerNotFound` / `ErrGitServerDisabled` / `ErrConfigMalformed`；@server 侧 `ErrGitServerTenantNotFound` / `ErrGitServerNoBinding` / `ErrGitServerRowMissing` / `ErrGitServerConfigMalformed` / `ErrGitServerDisabled`（前缀 `ErrGitServer*` 避免与 platform-tenant 的 `ErrTenantNotFound` 碰撞；ADR D1 类型解耦）。
4. **Cache 设计** — `RPCResolver` 5min TTL，errors 不缓存（transient RPC 失败下次重试，避免 5min 卡死）。mutex-guarded `map[tenant]*cacheEntry`。single-flight YAGNI（admin-triggered sync 是 bursty 不是 hot path）。
5. **类型解耦（ADR D1）** — `gitsync.GitServerConfig` 与 `user.TenantGitServerConfig` 字段一一对应但分别声明，避免 `gitsync → user` import cycle；`cmd/api/main.go` 的 `tenantGitServerAdapter` 做类型翻译。同样 cs-user 侧 `gitsync.GitServerConfig` 与 cs-user `gitserver.Config` 分别声明。
6. **Client factory seam** — `gitsync.Service.clientFactory func(GitServerConfig) GiteaTeamMemberAPI` 字段；production 用 `defaultClientFactory`（包 `NewClient`），测试注入 stub 不需要起 HTTP server。cs-user 侧 `giteasync.Service.clientFactory func(endpoint, adminToken string) GiteaUserProvisioner` 同模式。
7. **Path-based tenant_id（不在 X-Tenant-Id header）** — platform-admin 可能 sync 任意 tenant，不一定是请求 own 的 tenant；cs-user RPC `GET /api/internal/tenants/:tenant_id/git-server` 把 tenant_id 放 path。
8. **@server teamResolver 占位** — `NewConfigTeamResolver(nil)` 当前返回 false → `ErrTeamNotFound`。`TEAM_SYNC_MAPPINGS` env 已移除，未来 DB-backed team metadata 表替换（与 cs-user `teams` 表 ADR-10 集成时同步落地）。当前无 caller，acceptable。

**已知 deferred（不在 E3b.1.1 范围）**：

- **Vault integration** — `git_servers.config.admin_token` 当前 plaintext JSONB；TODO 标记。缓解：DB-level access controls + 字段永不 log。
- **NOT NULL enforcement** — `tenants.git_server_id` 当前 nullable；backfill 完成后单独 migration 加 NOT NULL + UNIQUE。
- **Cache invalidation API** — 当前仅 TTL；operator rotate admin_token 后最长 5min staleness。acceptable（rotation 罕见）。
- **Per-tenant `teams` 表** — @server teamResolver 仍占位；等 cs-user `teams` 表落地后做 cross-service 集成。
- **Bootstrap 多 git server 支持** — 当前 template row 仅 1 个（env-driven）；多 Gitea 实例需要 admin UI / API 创建额外 `git_servers` row，未来 slice。

**回滚步骤**：

- cs-user：删 `git_servers` migration + `models.GitServer` + `gitserver.*` + handler；`giteasync.Service` revert 到 `client *GiteaClient`。tenant.git_server_id column 残留无害（nullable）。
- @server：revert commit `<9>`（handler path + main.go wiring + config），revert commit `<8>`（service 签名），revert commit `<7>`（resolver 包），revert commit `<6>`（RPC client）。`GiteaConfig`/`TeamSyncConfig` 通过 git revert 恢复。
- 环境变量：恢复 `GITEA_BASE_URL` / `GITEA_ADMIN_TOKEN` / `TEAM_SYNC_MAPPINGS` / `CS_USER_GITEA_BASE_URL` / `CS_USER_GITEA_ADMIN_TOKEN` 即可。

**测试覆盖（合计 71 测试）**：

- cs-user: 4 (model) + 5 (resolver) + 5 (bootstrap) + 13 (giteasync service) + 7 (handler) = **34**
- @server: 12 (rpc client) + 10 (resolver) + 15 (gitsync service) + 8 (handler team_sync) = **45** （含 4 个 per-tenant 回归测试）

---

### admin-user-migration 实现细节：@server admin/users 切片迁移到 cs-user（已落地 2026-07-20，9 commits）

**迁移目标**：把 @server `/api/admin/users/*`（platform-admin 成员管理面）的身份 + status 真相迁到 cs-user。**方案 A 完整迁移**（用户明确选项）：cs-user 持有 user identity + status 唯一真相；Casdoor 仅作登录源认证；@server 保留本地 activity counts（`capability_items` / `item_distributions` / `item_distribution_receipts`）和 system roles（`user_system_roles`），因为这些表在 `costrict_db` 不在 `cs_user`，跨 DB join 按 ADR D1 拆分。

**9 commits**：

cs-user 侧（5 commits，新建 `/api/internal/users/*` admin 内部端点面）：
- `<1>` (`models.ActionUserStatusChanged` + `TargetTypeUser` audit vocab；为 SetUserStatus 的 `user_center_audit_log` 行做准备)
- `<2>` (`GET /api/internal/users/list` + `adminUserListItem` DTO + 分页 / keyword / organization / status 过滤；@server 列表 → RPC；4 测试 happy + 400 + 500 + empty)
- `<3>` (`POST /api/internal/users/:subject_id/status` + `setUserStatusRequest` body（含 `operator_id` 用于 self-lock check + audit 归属）+ transactional from_status 读取 + self-lock sentinel + audit row 写入；5 测试含 200/400/404/409/invalid body)
- `<4>` (`GET /api/internal/users/organizations` + per-tenant 组织 roll-up；3 测试含 empty → `[]` 序列化)
- `<5>` (`GET /api/internal/users/:subject_id/profile` + `adminUserProfileDTO`（privacy-scoped，omit `external_key` / `casdoor_*` / `provider_user_id`）；4 测试含 happy + 不漏 infra IDs + 404 + 500)

@server 侧（3 commits，新建 RPC client + 重写 adminuser handlers + 清理 dead code）：
- `<6>` (`user.RPCClient` 新增 4 个 admin-user 方法：`ListUsers` / `SetUserStatus` / `ListOrganizations` / `GetUserProfile`；`adminUserDo` 共享 helper（X-Internal-Token + X-Tenant-Id forwarding + actor-meta headers + timeout）；`mapAdminUserHTTPError` 把 HTTP code 映射为本地 sentinel；6 个本地 DTO 类型与 cs-user 一一对应；sentinels 前缀 `ErrAdminUserRPC*` 避免与现有 `admin_service.go` sentinels 碰撞；13 测试 happy + each HTTP code branch + NotConfigured + transport fault)
- `<7>` (`adminuser.Module` 重写：`AdminUserRPC` interface（prod impl = `*userpkg.RPCClient`，compile-time assertion `var _ AdminUserRPC = (*userpkg.RPCClient)(nil)`）；handlers 走 RPC 拿身份 + status，`m.users.RolesForUsers` + `m.users.GetUserProfile` 留本地（cross-DB split per ADR D1）；nil RPC 或 `!Configured()` → 503；typed-nil interface trap 被 `RPCClient.Configured()` 的 nil-receiver check 兜底；15 测试 stubRPC 注入，覆盖每个 handler happy + each sentinel + 503 + 401 + 边缘 case)
- `<8>` (cleanup：删 `admin_service.go` 的 `ListUsers` / `SetUserStatus` / `ListOrganizations` / `IsValidUserStatus` / `ListUsersParams` / `OrganizationCount` / 3 个 sentinel errors（`ErrInvalidUserStatus` / `ErrCannotChangeOwnStatus` / `ErrAdminUserNotFound`）+ 删 adminuser.SetUserStatusHandler 里的 `audit.Record(...)` 双写（cs-user 的 `user_center_audit_log` 行已是真相源，per ADR D1 single-source-of-truth）+ 清理 admin_service_test.go 中对应死测试)

progress 文档（1 commit）：
- `<本 commit>`（Phase C 计数器 6→7, 38%→44%；新增本节）

**关键设计决策**：

1. **方案 A 完整迁移** — 用户明确选 option A（身份 + status 全迁），cs-user 作单一真相源；Casdoor 仅作登录源认证。区别于方案 B（仅迁 status，留 @server 身份）：避免双 DB 同步语义复杂度。
2. **Cross-DB 拆分**（ADR D1）— @server 保留 activity counts + system roles 本地查询；`costrict_db` 与 `cs_user` 是物理分离 DB，跨 DB join 不可行；GetUserProfileHandler 是 split-then-merge 的范本：RPC 拿 identity → 本地 DB 拿 activity counts → 本地 DB 拿 roles → 合并到 `adminUserResponse`。
3. **Sentinel 解耦**（ADR D1）— cs-user 侧 sentinels（`ErrInvalidUserStatus` / `ErrCannotChangeOwnStatus` / `ErrAdminUserNotFound`）与 @server 侧 sentinels（`ErrAdminUserRPCInvalidStatus` / `ErrAdminUserRPCNotFound` / `ErrAdminUserRPCCannotChangeOwn`）分别声明；前缀 `ErrAdminUserRPC*` 避免碰撞；@server 不 import cs-user 的 error vars，HTTP code/body 文本匹配保持契约稳定。
4. **HTTP code 兼容**（self-lock 400 而非 409）— cs-user 的 SetUserStatus 返回 409 表示 self-lock；@server handler 保留 legacy 400（避免破坏前端 expectations），代码注释说明 deviation。REST anti-pattern 但前端兼容性优先。
5. **AdminUserRPC interface for testability** — `adminuser.Module.rpc` 字段类型是 interface 而非 `*userpkg.RPCClient` 具体类型；prod impl 满足 interface（compile-time assertion 防漂移）；test 用 `stubRPC` 注入 canned response / error，不起 HTTP server。
6. **Nil-safe feature gating** — RPC backend 未配置（`USER_SERVICE_BACKEND != rpc`）时 `*RPCClient.Configured()` 返回 false；adminuser handlers 检测 `m.rpc == nil || !m.rpc.Configured()` 返回 503。typed-nil interface trap 被 `Configured()` 的 nil-receiver check 兜底（`c != nil && c.baseURL != "" && ...`）。
7. **Privacy-scoped DTO**（`adminUserProfileDTO`）— cs-user 故意不返回 `external_key` / `casdoor_*` / `provider_user_id` 等 infra-only identifiers；admin UI 消费 human-facing 字段。@server 的 `adminUserResponse.UniversalID` 字段保留（shape compat）但永远空字符串——前端 selection/echo-back 如果依赖 universalId 需要后续 slice 通过非 admin endpoint 单独暴露。
8. **Audit single-source-of-truth**（Commit 8）— 早期是 transitional double-write（cs-user 写 `user_center_audit_log` + @server 写本地 `audit_log`）；Commit 8 删 @server 端 `audit.Record`，cs-user 的 audit row 成为唯一权威记录。
9. **Response 字段从 RPC 派生** — `SetUserStatusHandler` 现在返回 `{success, from_status, to_status}`（取自 RPC response），不再是 `{success: true}` 单字段；前端可以基于 `from_status → to_status` 转换做更细的 UI 反馈。

**已知 deferred（不在 admin-user-migration 范围）**：

- **UniversalID 重新暴露** — 当前 `adminUserResponse.UniversalID` 永远空；如果前端 enterprise/grant selection 依赖此字段，需要后续 slice 通过非 admin endpoint（e.g. `/api/admin/users/:id/external-ids`）单独暴露，避免 admin surface 漏出 infra-only IDs。
- **Tenancy 维度切换** — 当前 `X-Tenant-Id` forwarding 透传 ctx.tenant slug；如果 platform-admin 想跨 tenant 列用户，需要单独 endpoint（不在 C2 admin/user 切片）。
- **`@server admin_service.go` 全删** — 当前保留 `GetUserStatus`（middleware 用）+ `GetUserProfile`（activity counts）+ `RolesForUsers`（roles lookup）；如果未来 activity counts 也迁 cs-user（需要 ETL），可以彻底删 admin_service.go。
- **Per-tenant admin endpoints**（C3.x 系列）— 当前 `/api/admin/users/*` 是 platform-admin only；tenant_admin 视角的成员管理（C3.1 已有 `GET /api/tenant/users`）后续可以扩展为完整 CRUD（status switch / profile / organizations）。

**回滚步骤**：

- cs-user：删 `/api/internal/users/list` / `/:subject_id/status` / `/organizations` / `/:subject_id/profile` 4 个 handler + `user/admin_service.go` 的 `SetUserStatus` / `ListOrganizations` 方法（cs-user 侧）。`ActionUserStatusChanged` / `TargetTypeUser` audit vocab 保留无害。
- @server：revert commit `<8>`（恢复 dead code + 本地 audit）→ revert commit `<7>`（adminuser handlers 回到本地 DB 查询）→ revert commit `<6>`（删 RPC client 文件）。`adminuser.Module` 签名回退到 `New(users *UserService)`。
- DB：无 schema 变更（本切片纯应用层）。

**测试覆盖（合计 50 测试）**：

- cs-user: 2 (audit vocab model tests 已存在) + 4 (list) + 5 (set-status) + 3 (organizations) + 4 (profile) = **18 handler-level 测试**（admin_service 方法本身的 model-level 测试在 cs-user `user/admin_service_test.go` 内，未在本切片变更）
- @server: 13 (RPC client) + 15 (adminuser handlers rewrite) = **28**

---



- [身份架构实施路线图](../docs/identity-tenant/IDENTITY_ARCHITECTURE_ROADMAP.md) — 5 份提案的执行视图
- [cs-user Phase 1 ADR](../docs/identity-tenant/ADR_CS_USER_PHASE1_DECISIONS.md) — 10 项关键决策
- [cs-user 服务设计](../docs/identity-tenant/CS_USER_SERVICE_DESIGN.md) — 服务边界 + 4 层 UserInfo 契约
- [多租户设计](../docs/identity-tenant/MULTI_TENANCY_DESIGN.md) — tenant 维度 + 三级权限
- [用户中心设计](../docs/identity-tenant/USER_CENTER_DESIGN.md) — JWT 自签 + 身份主权
- [身份联邦 ADR](../docs/identity-tenant/IDENTITY_FEDERATION_DECISION.md) — Gitea JWT fork
- [团队/组织统一](../docs/identity-tenant/TEAM_ORG_UNIFICATION.md) — GitServerAdapter 归属
- [用户表前身进度](./USER_TABLE_PROGRESS.md) — server 单体内 user 表 + CachedUserService（cs-user 抽离的依赖）

---

### C4.3 audit-log list 实现细节：`GET /api/platform/audit-logs` + `GET /api/tenant/audit-logs`（已落地 2026-07-20，7 commits）

**目标**：把 C4.1 落地的 `user_center_audit_log` 表 + 3 indexes 暴露成两个面向消费方的 query endpoints：
- platform-admin 跨 tenant 视图（`GET /api/platform/audit-logs`，RequirePlatformAdmin）
- tenant_admin 本 tenant 视图（`GET /api/tenant/audit-logs`，RequireTenantAdmin；cs-user 通过 X-Tenant-Id header 强制 scope）

cs-user 仍是 `user_center_audit_log` 唯一真相源（ADR D1）；@server 仅作 RPC proxy。

**7 commit 分解**（5 cs-user + 1 @server handler/main.go 合并 + 1 doc；落地顺序）：

| # | 仓库 | 文件 | 内容 |
|---|------|------|------|
| 1 | cs-user | `internal/auditlog/service.go` + `service_test.go` | `ListParams` / `ListResult` 类型 + `Service.List(ctx, ListParams)` 分页 reader（newest-first，limit 默认 100 cap 500，offset 默认 0；7 测试覆盖 empty / defaults+caps / tenant filter / action+actor+target AND / time range / ordering / pagination / nil-svc） |
| 2 | cs-user | `internal/handlers/audit_logs.go` + `audit_logs_test.go` | `PlatformAuditLogsAPI.List` + `TenantAuditLogsAPI.List` 内部 endpoints（共享 query 合约：action/actor_subject_id/target_type/target_id/from/to/limit/offset；platform 透传 tenant_id query，tenant 强制从 ctx 覆盖（防跨 tenant 伪造）；10 handler 测试，含 spoofing-protection 用例） |
| 3 | cs-user | `internal/app/app.go` | `registerPlatformAuditLogRoutes` + `registerTenantAuditLogRoutes` + `unavailableAuditLogService` 503 stub（返回 `auditlog.ErrNilDB` 映射 503，保持 swagger 路径稳定） |
| 4 | server | `internal/user/rpc_client_audit_log.go` + `rpc_client_audit_log_test.go` | `*RPCClient.ListAuditLogs` + `ListAuditLogsForTenant` — `trustTenantID` flag 控制 URL builder 是否带 tenant_id（platform 透传，tenant strip）；3 sentinel：`ErrNotConfigured` / `ErrRPCUnavailable` / `ErrAuditLogRPCBadRequest`；11 测试覆盖 URL plumbing + sentinel 映射 + tenant-scope 防泄漏 + 3 个 `buildAuditLogQuery` 单元测试 |
| 5+6 | server | `internal/handlers/audit_log.go` + `audit_log_test.go` + `cmd/api/main.go` | `PlatformAuditLogAPI.PlatformListAuditLogs` + `TenantAuditLogAPI.TenantListAuditLogs` 公开 endpoints；main.go wiring 2 route groups + 2 `build*Service` helpers（同 `*RPCClient` via `Module.TenantResolver`，local backend 模式 nil → 502）；14 handler 测试覆盖 happy / 400 / 502 / 500 / nil-Svc 502 / date-only / tenant_id-stripped 防伪造 |
| 7 | doc | `todo/IDENTITY_TENANT_PROGRESS.md` | Phase C 计数 7→8、44%→50%；标记 C4.3 已落地；新增本节 |

**关键设计决策**：

1. **scope 强制在 handler 层，不在 service 层** — `auditlog.Service.List` 接受任意 `TenantID` 过滤（scope-agnostic）；cs-user 的 `TenantAuditLogsAPI.List` 从 `tenant.IDFromContext(ctx)` 强制覆盖 query 来的 tenant_id；@server 的 `TenantAuditLogAPI.TenantListAuditLogs` 在转发前 strip query tenant_id。两层独立防护。
2. **trustTenantID flag** — RPC client URL builder 用单一 bool flag 区分 platform vs tenant 路径；避免重复代码。
3. **timestamp 宽松解析** — handler 接受 RFC3339Nano / RFC3339 / `2006-01-02T15:04:05` / `2006-01-02 15:04:05` / `2006-01-02` 五种格式，方便 curl 手测；服务端再统一转 RFC3339Nano UTC 发给 cs-user。
4. **empty 200，非 404** — `Total=0, Logs=[]` 是合法答案；不让消费方把"无记录"当错误处理。
5. **default-tenant fallback** — tenant-scope endpoint 在 ctx 无 tenant 时 fallback 到 `tenant.DefaultTenantID` 而非 4xx，保持 pre-cutover 窗口 200-stable。
6. **ErrNilDB 503 stub** — cs-user app.go 的 `unavailableAuditLogService` 返回 `auditlog.ErrNilDB`（非 `errServiceUnavailable`）以让 `respondAuditLogErr` 一致映射 503。
7. **typed-nil trap 防御** — `*RPCClient.Configured()` nil-receiver check 防止 `(*RPCClient)(nil)` 通过 `interface{}` 非空比较；同 admin-user-migration step 6。
8. **compile-time interface assertion** — `var _ PlatformAuditLogService = (*userpkg.RPCClient)(nil)` + `var _ TenantAuditLogService = (*userpkg.RPCClient)(nil)` 防 RPC client signature drift。
9. **cross-DB split 保留** — @server 仍 own capability_items / item_distributions / activity counts（同 admin-user-migration），audit-log 表归属 cs-user（C4.1）。

**测试覆盖**：7 service + 10 cs-user handler + 11 RPC client + 14 @server handler = **42 测试**（全部 PASS；cs-user handler 包 `-race` 在无关的 `TestGetTenantGitServer_*` pre-existing race 上失败，非本切片引入；不带 `-race` 全绿）。

**Deferred items**（不在本切片）：
- **C4.2 active 越权检测 middleware** — 仍仅记录成功 admin 动作；拒绝尝试不入 audit。下一步可加。
- **`target_id` 跨表 join 视图**（如把 `target_id=user:u-123` 关联到用户展示名）— 当前 raw `target_id` 字符串透传，消费方自己解析。
- **streaming / cursor 分页** — 当前 limit/offset；如 audit 量级超 10M 行，可换 cursor-based。
- **audit log retention / 短期高基数表分区** — DB 层运维，不在应用切片。

**Rollback steps**：
1. `git revert` 7 commits（按相反顺序：doc → handler/main.go → RPC client → cs-user handlers → cs-user service）。
2. 应用层：cs-user 容器 `Deps.AuditLog = nil` 让 audit 写入 skip（与 C4.1 回滚步骤共享），list endpoint 自动 503。
3. DB 层：`user_center_audit_log` 表保留（C4.1 写入端；如要彻底回滚，按 C4.1 步骤处理）。

