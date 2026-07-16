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
| Phase 0 | cs-user 服务抽离（user 数据 ownership + read-through RPC） | 72 | 38 | 53% | 🟡 进行中（P0-1 + P0-2 + P0-3 + P0-4 完成，P0-5 部分） |
| Phase A | JWT 自签 + 雇佣上下文最小集 | ~40 | 0 | 0% | ⏳ 待启动 |
| Phase B | tenant 维度落地（数据隔离） | ~28 | 0 | 0% | ⏳ 待启动 |
| Phase C | 三级权限 + admin API | ~16 | 0 | 0% | ⏳ 待启动 |
| Phase E | 身份联邦扩展（多 IdP + Gitea + webhook） | ~20 | 0 | 0% | ⏳ 按需 |

> **Phase 0 大任务颗粒度**：8 个 P0-X 子任务 + 验收清单，当前完成 P0-1（骨架）/ P0-2（Postgres + 迁移）/ P0-3（models + read CRUD）/ P0-4（认证中间件）四个完整大任务 + P0-5（Helm chart）部分（缺 secret.yaml + chart 测试）。下一步推进 P0-6（ETL 脚本）或 P0-7（read-through RPC client）。

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

- [x] **实现**：`deploy/charts/cs-user/Chart.yaml`
- [x] **实现**：`deploy/charts/cs-user/values.yaml`（image / replicas / env / networkPolicy.enabled）
- [x] **实现**：`templates/deployment.yaml` + `templates/service.yaml` + `templates/networkpolicy.yaml`（限同 namespace 流量）
- [ ] **补全**：`templates/secret.yaml`（K8s Secret 注入 `CS_USER_INTERNAL_TOKEN` + PG 凭据；ADR §3.2 列出但当前缺失）
- [ ] **测试覆盖**：`deploy/charts/cs-user/tests/` 跑 `helm lint` + `helm template` 渲染断言（参考 `test-lint-charts.yaml` CI）
- [ ] **测试覆盖**：`helm template` 输出 fixture 文件，断言关键 path（labels / env vars / networkPolicy selectors）
- [ ] **swagger 注解**：无（chart 不暴露 endpoint）

### P0-6：ETL 脚本（dry-run + idempotent UPSERT）🔜

- [ ] **实现**：`cs-user/cmd/etl/main.go`，支持 `--dry-run` / `--source-dsn` / `--target-dsn` / `--batch-size` flags
- [ ] **实现**：`cs-user/internal/etl/export.go`（从 costrict-web DB 读 `users` + `user_auth_identities` 流式批读）
- [ ] **实现**：`cs-user/internal/etl/import.go`（基于 `subject_id` UPSERT 到 cs-user DB，`ON CONFLICT (subject_id) DO UPDATE`）
- [ ] **实现**：`cs-user/internal/etl/diff.go`（dry-run 模式产出字段级 diff 报告）
- [ ] **验证**：行数对齐 + 抽样字段对比 + `casdoor_universal_id` 唯一性检查
- [ ] **测试覆盖**：`etl/export_test.go` + `etl/import_test.go`（用 testcontainers 双 PG 实例，跑 ETL 后断言行数 + 字段一致性）
- [ ] **测试覆盖**：`etl/idempotent_test.go`（连续跑两次，第二次 0 写入）
- [ ] **测试覆盖**：`etl/dry_run_test.go`（dry-run 模式目标 DB 0 变化）
- [ ] **swagger 注解**：无（ETL 是离线脚本，不是 HTTP endpoint）

### P0-7：read-through RPC client in costrict-web 🔜

- [ ] **实现**：`server/internal/user/rpc_client.go`（实现现有 `UserService` 接口，内部走 HTTP 调 cs-user `/api/internal/users/*`）
- [ ] **实现**：`server/internal/user/cached_rpc.go`（包装 RPC client + 复用现有 `CachedUserService` LRU + TTL）
- [ ] **实现**：失败降级策略——缓存命中返 stale + log warning；缓存未命中返 503
- [ ] **实现**：`server/internal/config` 增加 cs-user endpoint + `X-Internal-Token` 配置项
- [ ] **接线**：`server/cmd/api/main.go` 用 `cachedRPC` 替换 `cachedService`（环境门控：`USER_SERVICE_BACKEND=rpc|local`）
- [ ] **测试覆盖**：`user/rpc_client_test.go`（用 `httptest` mock cs-user，覆盖 happy / 404 / 5xx / timeout）
- [ ] **测试覆盖**：`user/cached_rpc_test.go`（缓存命中 / 失效 / stale 兜底 / 503 兜底）
- [ ] **集成测试**：`server/internal/user/integration_test.go`（同时跑 server + cs-user，端到端验证 `GET /api/users/:id` 走 RPC）
- [ ] **swagger 注解**：server 端 endpoint（`/api/users/:id` 等）注解保持不变（接口签名未变，只是内部实现切换）

### P0-8：costrict-web users 表 READONLY cutover 🔜

- [ ] **实现**：应用层 gate——server 内所有 user 写入路径（grep `models.User` 的 INSERT/UPDATE/DELETE）路由到 RPC，本地 DB 只读
- [ ] **实现**：DB trigger 兜底——costrict-web `users` 表加 `BEFORE INSERT/UPDATE/DELETE` trigger 拒写（除 cutover 期间白名单）
- [ ] **验证**：`grep -r "models.User" server/ | grep -E "(Create|Update|Delete)"` 输出清零（除 RPC client 自身）
- [ ] **验证**：cutover 后连续 1 小时压测，`CachedUserService` 命中率 > 90%
- [ ] **验证**：cs-user DB 独立备份 + 恢复测试通过
- [ ] **测试覆盖**：`server/internal/user/readonly_test.go`（尝试写本地 user 表应失败 / 抛 trigger 错误）
- [ ] **回滚预案**：保留 costrict-web DB 备份 30 天；提供 `USER_SERVICE_BACKEND=local` 一键回滚开关
- [ ] **swagger 注解**：无（cutover 是部署操作，不改 endpoint）

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

### A1：employment_identities 表迁移

- [ ] **实现**：`cs-user/migrations/202607XX_create_employment_identities.sql`（MULTI_TENANCY §6.5.1, §8）
- [ ] **实现**：`cs-user/internal/models/employment_identity.go`
- [ ] **测试覆盖**：迁移 up/down 重入测试（testcontainers）
- [ ] **swagger 注解**：无（数据库迁移）

### A2：tenant_configs 表（最小 schema）

- [ ] **实现**：`cs-user/migrations/202607XX_create_tenant_configs.sql`（`tenant_id` + `yaml` text 列）
- [ ] **实现**：`cs-user/internal/models/tenant_config.go`
- [ ] **测试覆盖**：yaml 列读写测试
- [ ] **swagger 注解**：无

### A3：JWT 自签（RS256 + JWKS）

- [ ] **实现**：`cs-user/internal/auth/jwt_signer.go`（RS256 签名 + kid 管理）
- [ ] **实现**：`cs-user/internal/auth/jwks_handler.go`（`GET /.well-known/jwks.json` endpoint）
- [ ] **实现**：保留 Casdoor JWT 30 天兼容窗口（feature flag `jwt_self_sign_enabled`）
- [ ] **测试覆盖**：`jwt_signer_test.go`（签名/验签往返 + claim 字段完整性）
- [ ] **测试覆盖**：`jwks_handler_test.go`（公开 key 格式 + 缓存头）
- [ ] **swagger 注解**：`/.well-known/jwks.json` 加 `@Tags well-known` + 完整注解
- [ ] **swagger 注解**：`make swagger` 重新生成

### A4：OAuth callback 加 ApplyEnterpriseMapping

- [ ] **实现**：`cs-user/internal/user/service.go` 增加 `ApplyEnterpriseMapping` 步骤（按 `employment_providers.enabled` 门控）
- [ ] **测试覆盖**：service 测试增加 employment provider 启用/禁用两种场景
- [ ] **swagger 注解**：无（service 层逻辑）

### A5：JWT claims 扩展

- [ ] **实现**：保留 `universal_id` / `sub` / `provider` / `preferred_username` / `email` / `phone` / `exp`
- [ ] **实现**：新增 `enterprise` Map + `primary_provider` + `tenant_id` 字段（追加，不可替换）
- [ ] **测试覆盖**：claim 字段 fixture 测试（覆盖 §9.6 双格式 reader 的 5 种场景）
- [ ] **swagger 注解**：JWKS endpoint 的 `@Response` 引用 claims DTO

### A6：default tenant 引导脚本

- [ ] **实现**：`cs-user/migrations/202607XX_bootstrap_default_tenant.sql`
- [ ] **测试覆盖**：脚本幂等性测试
- [ ] **swagger 注解**：无

### A7：接管 Casdoor OAuth `/oidc-auth/api/v1/plugin/login` 端点

- [ ] **决策**：策略 (a) costrict-web 直接当 OP vs (b) 保留 Casdoor 前端 + 重签（推荐 b）
- [ ] **实现**：`cs-user/internal/handlers/oidc_auth.go`（新或改）
- [ ] **关键风险测试**：csc 真实 login 流程集成测试，验证 `profile.account.{uuid, email, display_name, created_at}` + `profile.organization.*` 字段完整
- [ ] **swagger 注解**：`/oidc-auth/api/v1/plugin/login` + `/token` 加完整注解（与 Casdoor 原契约一致）

### A8：灰度发布

- [ ] **实现**：feature flag `jwt_self_sign_enabled`（off → 双签 → 单签）
- [ ] **测试覆盖**：feature flag 三态行为测试
- [ ] **swagger 注解**：无（部署配置）

### Phase A 验收（ROADMAP §9.5 必过）

- [ ] 用新 JWT 调 cs-cloud，`UserID()` 与旧 JWT 一致
- [ ] `provider` 路由正确（github/email/phone/idtrust 各试一次）
- [ ] csc login → poll → store → API call 全流程通过
- [ ] csc 登录后能看到 `accountInfo.email` / `organization` 字段
- [ ] assistant-ui SSE/WebSocket 连接正常
- [ ] quota-manager `/quota-manager/api/v1/*` 调用 `AuthUser.ID` 解析成功
- [ ] cs-cloud + costrict-web 双格式 reader 已落地（§9.6）
- [ ] 30 天灰度结束关闭 Casdoor JWT 签名 24 小时无异常

---

## 阶段 B：tenant 维度落地（数据隔离）

> Phase 0 完成后启动；单 tenant 模式（`tenant_id=default`）也能跑，不阻塞 Phase A。

- [ ] **B1**：`tenants` + `tenant_admins` 表迁移（`cs-user/migrations/`）
- [ ] **B2**：给 `users` / `user_auth_identities` / `user_profile` / `employment_identities` 加 `tenant_id` 列 + 索引
- [ ] **B3**：tenant resolution（subdomain → email domain → 显式选择）
- [ ] **B4**：中间件从 JWT 提取 `tenant_id` 注入 request context
- [ ] **B5**：应用层所有 query 经 `tenantScope(ctx)` helper
- [ ] **B6**：PostgreSQL RLS Policy 兜底（`CREATE POLICY tenant_isolation ON ...`）
- [ ] **B7**：`(tenant_id, username)` 联合唯一索引，`email` 保持全局唯一

**每个任务的测试 + swagger 子项同 Phase 0/A 模板**：迁移配 testcontainers up/down；query 改动配 sqlmock 注入测试；新 admin endpoint 配完整 swagger 注解 + InternalToken security。

---

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
| 共享密钥泄露 | 中 | K8s secret 注入 + 定期轮换 + networkPolicy 限同 namespace | 🟡 networkPolicy 已就绪，secret.yaml 待补 |
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
| 待定 | Phase 2 启动前：JWT 密钥存储方式（文件 / KMS） | ⏳ Phase A3 触发前 |
| 待定 | Phase 3 启动前：employment resolution_strategy 默认值（first_wins / last_wins） | ⏳ Phase A4 触发前 |
| 待定 | cutover 执行窗口（停机 / 灰度比例序列） | ⏳ Phase 0 P0-8 触发前 |
| 待定 | OAuth callback 接管策略 (a) vs (b) | ⏳ Phase A7 触发前 |

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
