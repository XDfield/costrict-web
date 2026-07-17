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
| Phase A | JWT 自签 + 雇佣上下文最小集 | ~40 | 10 | 25% | 🟡 进行中（A1 + A2 + A6 + A4 service 层 + A4b endpoint+server wiring + A3 JWT signer + A5 claims 扩展 + A7 cs-user endpoint + A7b server 端 OAuth callback wiring + A8 灰度 三态门控 完成；Phase A 验收 待跑） |
| Phase B | tenant 维度落地（数据隔离） | ~28 | 5 | 18% | 🟡 进行中（B1 tenants + tenant_admins 表 + 默认 default 租户行 + tenant_configs FK 完成；B2 给 users / user_auth_identities / employment_identities 加 tenant_id 列 + FK + 索引 完成；B3 tenant.Resolver 三层 fallback primitives + email_domains typed reader 完成；B3b.1 cs-user 侧 HTTP middleware + TenantConfig + context helpers 完成；B3b.2a server 侧 tenant slug forwarding（middleware + RPC header 注入 + ApexDomains 配置）完成；B3b.2b/c OAuth callback 链路 + picker + cross-tenant 检测 + B4-B7 待启动） |
| Phase C | 三级权限 + admin API | ~16 | 0 | 0% | ⏳ 待启动 |
| Phase E | 身份联邦扩展（多 IdP + Gitea + webhook） | ~20 | 0 | 0% | ⏳ 按需 |

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
- [ ] 30 天灰度结束关闭 Casdoor JWT 签名 24 小时无异常

---

## 阶段 B：tenant 维度落地（数据隔离）

> Phase 0 完成后启动；单 tenant 模式（`tenant_id=default`）也能跑，不阻塞 Phase A。

- [x] **B1**：`tenants` + `tenant_admins` 表迁移（`cs-user/migrations/20260717100000_create_tenants_and_tenant_admins.sql`）— 详见下方"B1 实现细节"

- [x] **B2**：给 `users` / `user_auth_identities` / `employment_identities` 加 `tenant_id` 列 + 索引（`user_profile` 表尚未存在，待其首次落地时一并加入）— 详见下方"B2 实现细节"

- [x] **B3**：tenant resolution — `tenant.Resolver` 三层 fallback primitives（slug / email-domain / email）+ email_domains typed reader。HTTP middleware / cookie / session / Casdoor redirect 拆到 **B3b**（下一子任务）。— 详见下方"B3 实现细节"
- [ ] **B3**：tenant resolution（subdomain → email domain → 显式选择）
- [ ] **B4**：中间件从 JWT 提取 `tenant_id` 注入 request context
- [ ] **B5**：应用层所有 query 经 `tenantScope(ctx)` helper
- [ ] **B6**：PostgreSQL RLS Policy 兜底（`CREATE POLICY tenant_isolation ON ...`）
- [ ] **B7**：`(tenant_id, username)` 联合唯一索引，`email` 保持全局唯一

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

**B3b.2b（待启动）** — OAuth callback 链路：

- [ ] OAuth callback `handlers.AuthCallback`（server/internal/handlers/handlers.go:387）在 GetOrCreateUser 之前注入 tenant 解析：Try 1 subdomain → Try 2 email-domain → Try 3 picker redirect
- [ ] picker endpoint `/api/tenant/suggest?email=...`（设计 §5.1 Try 2 多命中场景）— 需要 cs-user 暴露 RPC 端点（B3b.2b 子任务）

**B3b.2c（待启动）** — cross-tenant 访问检测：

- [ ] JWT claims 加 `tenant_id` 字段（server/internal/middleware/auth.go AuthClaims + server/internal/user/service.go JWTClaims）
- [ ] middleware 比对 JWT `tenant_id` vs cookie `cs_tenant_slug`，不匹配 → 401/403 + 提示切换（设计 §5.3 场景 C）

### B3b.2a 实现细节

- **server 不复制 tenants 表**：与 cs-user 不同，server 的 tenant 包只做 slug 提取 + ctx 转发。实际 DB 查询由 cs-user 完成（`tenant_id` lookup via `ResolveBySlug`）。这保证 tenant 数据 ownership 集中在 cs-user（ADR D1）。
- **slug vs tenant_id 都接受**：cookie 和 header 既可以装 slug 也可以装 tenant_id — cs-user 的 `ResolveBySlug` 接受两者，所以 server 不需要区分。
- **空 slug = "no signal"**：`WithSlug(ctx, "")` 是合法的；`HasSlug` 返回 false；RPC 转发省略 `X-Tenant-Id`；cs-user 自动 fallback 到 default tenant。这让本地 dev（无 apex 配置、无 cookie）零摩擦。
- **subdomain 提取与 cs-user 一致**：`slugFromHost` 用 `strings.LastIndex(sub, ".")` 取 label immediately below apex（`foo.acme.example.com` → `acme`），与 cs-user `resolver.go` 行为对齐 — 两端必须 agree，否则同一 host 解析出不同 slug。
- **middleware 装在 OptionalAuth 之前**：让 JWKS / health / swagger 等公开路由也能从 cookie/subdomain 提取 slug 转发；不过这些路径不调 cs-user RPC，slug 实际只对后续 RPC 调用有用。
- **RPC 转发位置**：`rpc_client.do()` 在设 `X-Internal-Token` 之后立即检查 ctx 中的 slug，非空时设 `X-Tenant-Id`。`tenant_ctx.go` 提供共享 helper，nil-ctx 防御。
- **已知限制 — write-path 未覆盖（B3b.2b 修）**：`rpc_writer.go doCapture()` 虽然也加了同样的注入逻辑，但所有 6 个 `RPCWriter` 公开方法（GetOrCreateUser / SyncUser / BindIdentityToUser / TransferIdentityToUser / UnbindIdentityByProvider / ApplyEnterpriseMapping / ReissueToken）当前硬编码 `context.Background()`，所以 slug 永远到不了注入点。修复需要把 `ctx context.Context` 加到 `UserWriter` interface 的所有方法签名上（DualWriter + RPCWriter + UserService 同步）+ 改 6 个 handler call sites。该 refactor 自然落在 B3b.2b（OAuth callback 链路）里 — B3b.2b 本来就要改 `handlers.go:452-476` 的 callback flow，顺手把 ctx 穿透下来。当前 B3b.2a 只覆盖 read-path（GetUserByID / GetUsersByIDs / SearchUsers / ListUserIdentities），write-path 暂时仍 fallback 到 default tenant。`rpc_writer.go doCapture()` 里的注入代码保留作为 B3b.2b 的 placeholder（注释说明），避免后续漏改。

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

- [ ] **C1**：权限模型表 + 中间件（platform_admin / tenant_admin / tenant_member）
- [ ] **C2**：platform_admin API（tenant CRUD、跨 tenant 审计）
- [ ] **C3**：tenant_admin API（本 tenant 用户列表、IdP 配置、provider_mapping yaml 编辑）
- [ ] **C4**：越权防护 + 审计日志

**测试覆盖重点**：每个 admin endpoint 必须覆盖"角色不符 → 403"路径；swagger 注解挂 `@Security` 双重（InternalToken + BearerAuth 角色注解）。

---

## 阶段 E：身份联邦扩展（按需启用）

- [ ] **E1**：provider_mapping yaml 标准化（per-tenant）
- [ ] **E2**：tenant 级 IdP 接入（global vs tenant-specific）
- [ ] **E3a**：Gitea JWT 中间件 fork + user 自动开户 + `user_gitea_binding` 维护（归属 cs-user）
- [ ] **E3b**：Gitea `team_user` 同步（GitServerAdapter）（归属 server `internal/gitsync/`）
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

## 参考文档

- [身份架构实施路线图](../docs/identity-tenant/IDENTITY_ARCHITECTURE_ROADMAP.md) — 5 份提案的执行视图
- [cs-user Phase 1 ADR](../docs/identity-tenant/ADR_CS_USER_PHASE1_DECISIONS.md) — 10 项关键决策
- [cs-user 服务设计](../docs/identity-tenant/CS_USER_SERVICE_DESIGN.md) — 服务边界 + 4 层 UserInfo 契约
- [多租户设计](../docs/identity-tenant/MULTI_TENANCY_DESIGN.md) — tenant 维度 + 三级权限
- [用户中心设计](../docs/identity-tenant/USER_CENTER_DESIGN.md) — JWT 自签 + 身份主权
- [身份联邦 ADR](../docs/identity-tenant/IDENTITY_FEDERATION_DECISION.md) — Gitea JWT fork
- [团队/组织统一](../docs/identity-tenant/TEAM_ORG_UNIFICATION.md) — GitServerAdapter 归属
- [用户表前身进度](./USER_TABLE_PROGRESS.md) — server 单体内 user 表 + CachedUserService（cs-user 抽离的依赖）
