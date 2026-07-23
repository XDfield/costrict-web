# 身份与多租户架构实施路线图

**状态**：Active · 2026-07-13
**目的**：把现有的 5 份身份相关提案整合成**单一执行视图**，明确「要做的事 / 已做的事 / 可推迟的事」，消除提案文档过多带来的迷茫感。
**读者**：实施者、评审者、决策者。

> 本文档**不替代**下列任何一份提案，而是它们的**索引 + 执行清单 + 阶段切分**。所有设计细节以原提案为准。

---

## 1. 五份提案的真正关系（不是重复，是依赖栈）

```
┌─────────────────────────────────────────────────────────────┐
│  MULTI_TENANCY_DESIGN.md（顶层：tenant 维度）              │
│  ─────────────────────────────────────────────────────       │
│  + tenant_id 列、RLS、tenant_configs、tenant_admin         │
│  + employment_providers（雇佣上下文与 IdP 解耦）            │
│  + 三级权限（platform / tenant / member）                   │
├─────────────────────────────────────────────────────────────┤
│  CS_USER_SERVICE_DESIGN.md（中层：服务边界）                │
│  ─────────────────────────────────────────────────────       │
│  + 从 costrict-web 单体抽离 cs-user 微服务                  │
│  + 4 层 UserInfo 契约（base / identities / profile / ent）  │
│  + provider_mapping yaml                                    │
├─────────────────────────────────────────────────────────────┤
│  USER_CENTER_DESIGN.md（底层：身份主权）                    │
│  ─────────────────────────────────────────────────────       │
│  + 把用户身份主权从 Casdoor 收回到 costrict-web             │
│  + JWT 自签（不再依赖 Casdoor JWT）                          │
│  + webhook 广播用户变更到下游                                │
├─────────────────────────────────────────────────────────────┤
│  IDENTITY_FEDERATION_DECISION.md（正交 ADR）                │
│  ─────────────────────────────────────────────────────       │
│  + Gitea JWT 中间件 fork（~250 行）                          │
│  + Gitea user 自动开户 + user_gitea_binding 维护 [cs-user]   │
│  + Gitea team_user 同步（GitServerAdapter）[@server, v3]     │
├─────────────────────────────────────────────────────────────┤
│  MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md（已合并）        │
│  ─────────────────────────────────────────────────────       │
│  - UserAuthIdentity 表 + bind/unbind 流程                    │
│  - 已被 MULTI_TENANCY_DESIGN §11.4 吸收，原文档可归档       │
└─────────────────────────────────────────────────────────────┘
```

**结论**：这 5 份**没有重复**，是上层依赖下层的递进关系。但 286KB 总量确实让人难以看清「我下周该做什么」——这正是本文要解决的。

---

## 2. 当前代码实现现状（与提案对照）

| 领域 | 状态 | 关键文件 / 证据 |
|---|---|---|
| `users` 表（`BIGSERIAL` PK + `subject_id VARCHAR(191)` UNIQUE 业务标识） | ✅ 已实现 | `server/migrations/20260401100000_create_users_table.sql`、`20260408154000_migrate_users_to_subject_id_and_serial_pk.sql` |
| `user_auth_identities` 表（多 IdP 绑定） | ✅ 已实现 | `server/migrations/20260524000000_create_user_auth_identities_table.sql` + `20260525000000_add_explicitly_unbound` |
| Casdoor OAuth/OIDC 客户端 | ✅ 已实现 | `server/internal/casdoor/client.go` |
| JWKS JWT 验证 + fallback 到 userinfo | ✅ 已实现 | `server/internal/middleware/auth.go` |
| 多 provider 绑定 + `external_key` 组合键 | ✅ 已实现 | `server/internal/user/service.go:buildExternalKey`、`server/internal/handlers/users.go` |
| JWT claims（`sub` / `universal_id` / `provider` / `email` / `phone`） | 🟡 部分 | `server/internal/middleware/auth.go:27-37`、`server/internal/user/service.go:19-31` |
| `tenants` 表 / `tenant_id` 列 | ❌ 未开始 | grep 无任何 `tenant_id` 列 |
| `tenant_configs` 表 + provider mapping yaml | ❌ 未开始 | — |
| `employment_identities` 表 / `employment_providers` | ✅ Slice 1 + 1.5 完成（2026-07-23） | `cs-user/migrations/20260716150000_create_employment_identities.sql` + `20260722400000_add_employment_enterprise_uid.sql`；模型 `cs-user/internal/models/employment_identity.go`；门控 + 字段映射 + 运行时消费 `cs-user/internal/user/employment_mapping.go`（详见 §2.1 / §2.2） |
| RLS（PostgreSQL Row Security Policy） | ❌ 未开始 | — |
| 三级权限（platform / tenant_admin / member） | ❌ 未开始 | — |
| `primary_provider` claim / 雇佣上下文 JWT claim | ❌ 未开始 | — |
| cs-user 微服务抽离 | ❌ 未开始 | 仍在 `costrict-web` 单体内 |
| JWT 自签（脱离 Casdoor JWT） | ❌ 未开始 | 仍用 Casdoor JWT |
| webhook 用户变更广播 | ❌ 未开始 | — |
| Gitea JWT 中间件 fork | ❌ 未开始 | 见 `IDENTITY_FEDERATION_DECISION.md` |
| Gitea user 自动开户 + `user_gitea_binding` 维护 | ❌ 未开始 | 归属 cs-user；见 `CS_USER_SERVICE_DESIGN.md` §11 |
| Gitea `team_user` 同步（GitServerAdapter） | ❌ 未开始 | 归属 @server（`server/internal/gitsync/`）；见 `TEAM_ORG_UNIFICATION.md` ADR-3 v3 |

**核心判断**：当前代码停在「**多 IdP 单租户**」阶段，距离「多租户 + 雇佣上下文 + 服务抽离」的终态大约还有 70% 工作量。

### 2.1 Slice 1（field_map 建模 + enterprise_uid 索引）落地范围

**目标**：让"配置指定身份来源作为企业身份 + 字段映射"这条线进入可配置阶段，为 Slice 2 接入真实 provider client 铺路。

**配置 schema**（`tenant_configs.config_yaml` 内）：
```yaml
employment_providers:
  enabled: [wxwork]                # 哪些 provider 视为"企业身份来源"
  field_map:                       # Slice 1 新增
    wxwork:
      enterprise_uid: "UserId"     # internal column ← external IdP field
      employee_number: "JobNumber"
      cost_center: "Department"
      org_path: "FullPath"
      hire_date: "JoinTime"
```

**Go 类型**（`cs-user/internal/user/employment_mapping.go`）：
```go
type employmentProvidersConfig struct {
    Enabled  []string                  `yaml:"enabled"`
    FieldMap map[string]FieldMapConfig `yaml:"field_map,omitempty"`
}
type FieldMapConfig map[string]string  // YAML key=internal column, value=external field
```

**Whitelist 校验**：`allowedEmploymentColumns` 锁定 12 个允许的 internal column（`enterprise_uid` / `employee_number` / `cost_center` / `org_path` / `direct_manager_subject_id` / `direct_manager_external_ref` / `job_title` / `job_level` / `employment_type` / `hire_date` / `regular_date` / `work_location`）。配置时 unknown internal column 立即报 parse error，避免运行时静默 no-op。

**Migration**（`cs-user/migrations/20260722400000_add_employment_enterprise_uid.sql`）：
- `ALTER TABLE employment_identities ADD COLUMN enterprise_uid VARCHAR(191)`
- `CREATE UNIQUE INDEX uq_employment_identities_tenant_enterprise_uid ON employment_identities (tenant_id, enterprise_uid) WHERE enterprise_uid IS NOT NULL` —— partial unique，stub write path 阶段 enterprise_uid 为 NULL，多行可共存；Slice 2 填字段后自动启用 per-tenant 唯一性。

**Slice 1 不做的事**（留给 Slice 2）：
- 真实 provider client（idtrust API / Azure AD Graph / 企微）接入
- OAuth callback 里 `ApplyEnterpriseMapping` 的实际触发串通
- A5 扩展 JWT claims 直接带 enterprise 字段

### 2.2 Slice 1.5（field_map 运行时消费）落地范围

**目标**：让 field_map 真正驱动 employment_identities 写入，不必等 Slice 2 的真实 provider client。OAuth callback 解码 IdP userinfo 后，把外部 claims 作为 `map[string]any` 传进来即可生效。

**API 扩展**（`cs-user/internal/user/employment_mapping.go`）：
```go
type EmploymentMappingParams struct {
    TenantID       string
    UserSubjectID  string
    Provider       string
    ExternalClaims map[string]any  // Slice 1.5 新增：caller 负责填充
}
```

**运行时映射**：`applyFieldMap(fieldMap, claims) map[string]any` 按 field_map 把 external claims 转成 internal_column → typed value：
- 日期列（`hire_date` / `regular_date`）走 `parseClaimDate`，支持 RFC 3339 字符串、int64/int/float64 Unix 秒、`time.Time`；解析失败静默跳过（不 500 登录）
- 字符串列走 `fmt.Sprint`（数字/布尔自动 stringify）
- 缺失 / nil 的 external field → 该列保持 NULL

**Write path 串通**：
- Create 路径：`applyMappedToRow` 把 mapped map 按 column-name 分派到 EmploymentIdentity 字段
- Update 路径：mapped 合并进 `Updates` map，刷新存量行时同步覆盖 enterprise 字段

**Slice 1.5 不做的事**（仍留给 Slice 2）：
- 真实 IdP API 调用（OAuth callback 仍未串通 ApplyEnterpriseMapping）
- field_map 内 `interval` / `on_login: refresh_if_stale` 配置建模（slice 2 配合真实 provider client 落地）
- A5 扩展 JWT claims 直接带 enterprise 字段

**Partial unique index 验证**（2026-07-23）：手工 probe（SQLite 支持 partial index，与 PG 同语义）确认：
- `enterprise_uid` NULL 多行可共存（stub 阶段安全）
- 同 tenant 内同 `enterprise_uid` 拒绝写入
- 跨 tenant 同 `enterprise_uid` 允许（per-tenant 隔离正确）

### 2.3 Slice 2（OAuth callback 串通 + ExternalClaims 数据源）落地范围

**目标**：让 field_map 真正在登录时跑起来 — 不再需要 Slice 2 之外的手动触发。Multi-IdP OAuth callback 拿到的 IdP userinfo 自动流到 cs-user 的 `ApplyEnterpriseMapping`，按 tenant 的 field_map 写 employment_identities。

**链路**（4 处改动）：

1. **server `user.JWTClaims`** + **cs-user `models.JWTClaims`** 同步加 `ExternalClaims map[string]any` 字段（`json:"external_claims,omitempty"`）。两边的 wire contract 由 `cs-user/internal/models/jwt_claims_test.go` 的 round-trip test 锁定。

2. **server `auth_multi_idp.go` callback**（`runMultiIdPCallback`）：构造 JWTClaims 时把 `profile.Raw`（OAuth client 已经 hold 的完整 IdP userinfo map）塞进 `ExternalClaims`。无需新增 IdP client — `generic_client.go:63` 的 `Raw` 字段早就 hold 了这个数据，只是之前没传出去。

3. **cs-user `Service.GetOrCreateUser`**：两个 success path（新建用户 + 更新已有用户）return 之前自动调 `applyEnterpriseMappingOnLogin(ctx, subjectID, claims)` helper。helper 是 best-effort swallow：所有错误（feature 未启用、tenant_configs 缺失、YAML malformed、DB 错误）都不阻塞登录 — 企业身份映射是 bonus feature。`claims.Provider == ""` 短路（legacy Casdoor path 不触发）。

4. **server callback 显式调用 `writer.ApplyEnterpriseMapping` 删除**（`auth_multi_idp.go` + `handlers.go`）：之前 Phase A4b 在 server 端显式调一次，但没传 ExternalClaims（只传 subjectID+provider），是降级 stub。Slice 2 后 cs-user 内部 GetOrCreateUser 自动触发完整版，server 那次冗余 RPC 删除。`UserWriter.ApplyEnterpriseMapping` 接口保留供未来运维手动触发。

**端到端示例**（wxwork 登录）：
```
用户 → Casdoor (wxwork provider) → server callback
  ↓
FetchUserInfo → profile.Raw = {"UserId":"wx_001","JobNumber":"E-42","Department":"R&D",...}
  ↓
JWTClaims{Provider:"wxwork", ExternalClaims: profile.Raw, ...}
  ↓ RPC POST /api/internal/users/get-or-create
cs-user GetOrCreateUser
  ↓ 创建/更新 users 行
  ↓ applyEnterpriseMappingOnLogin:
  ↓   loadEmploymentProvidersConfig → enabled=[wxwork], field_map set
  ↓   applyFieldMap(claims.ExternalClaims) → {enterprise_uid:"wx_001", employee_number:"E-42", ...}
  ↓   upsert employment_identities row（映射字段写入）
  ↓ return user
```

**Slice 2 不做的事**（留给 Slice 3+）：
- A5 扩展 JWT claims 直接带 enterprise 字段（让 access_token 自带企业身份，不必每次回查 employment_identities）
- `interval` / `on_login: refresh_if_stale` 配置建模（避免每次登录都跑映射，按 TTL 短路）
- 多 IdP `external_claims` 字段名冲突的 tenant 级配置（当前假设各 provider 的 IdP 字段名 tenant 全局通用）

---

## 3. 终态架构（一图概览）

```
┌────────────────────────────────────────────────────────────────────┐
│                     costrict-web（业务层）                         │
│  ┌─────────────┐  ┌─────────────┐  ┌──────────────────────────┐   │
│  │ 业务 API    │  │ tenant_admin│  │ platform_admin API       │   │
│  │ (含 tenant  │  │ API         │  │ (跨 tenant)              │   │
│  │  上下文)    │  │             │  │                          │   │
│  └──────┬──────┘  └──────┬──────┘  └───────────┬──────────────┘   │
│         │                │                     │                   │
│         └────────────────┼─────────────────────┘                   │
│                          ▼                                          │
│  ┌──────────────────────────────────────────────────────────────┐  │
│  │  cs-user 微服务（独立部署）                                  │  │
│  │  ────────────────────────────────────────────────            │  │
│  │  • 4 层 UserInfo：base / identities / profile / enterprise   │  │
│  │  • JWT 自签（RS256 / JWKS）                                  │  │
│  │  • employment_providers 门控雇佣上下文                       │  │
│  │  • webhook 广播用户变更                                      │  │
│  └────────────────────────┬─────────────────────────────────────┘  │
└──────────────────────────┼─────────────────────────────────────────┘
                           │ OAuth / OIDC
                           ▼
┌────────────────────────────────────────────────────────────────────┐
│  Casdoor（降级为内部 IdP 之一，不再是身份主权源）                 │
│  ── 与 idtrust / azure_ad / ldap / github / feishu 同位           │
└────────────────────────────────────────────────────────────────────┘
                           │
                           ▼ (per-tenant provider_mapping yaml)
┌────────────────────────────────────────────────────────────────────┐
│  PostgreSQL（共享库 + 行级隔离）                                  │
│  ────────────────────────────────────────────────                 │
│  • 所有用户表带 tenant_id 列                                      │
│  • RLS Policy 作为安全网                                          │
│  • tenant_configs yaml 存 provider_mapping / employment_providers │
└────────────────────────────────────────────────────────────────────┘
```

---

## 4. 实施路线图（6 个阶段，每阶段独立可上线）

> **关键原则**：每个阶段都是**完整的、可上线的、有用户价值的**。任何一个阶段卡住都不阻塞前一阶段的产出。
>
> **路径基线变更（2026-07-16 ADR）**：原 ROADMAP 把 cs-user 服务抽离放在 Stage D（可无限期推迟）。经 [`ADR_CS_USER_PHASE1_DECISIONS.md`](./ADR_CS_USER_PHASE1_DECISIONS.md) 决策，**提前到 Phase 0**——先搭 cs-user 服务壳 + 接管 user 数据 ownership（user CRUD only），再做 Phase A 的 JWT 自签。Phase A 起所有认证 / 身份相关代码物理路径在 `cs-user/...`。

### 阶段 0：cs-user 服务抽离（user 数据 ownership，**先于 Phase A**）

**目标**：搭独立 cs-user 服务（Monorepo `costrict-web/cs-user/`），接管 user 数据 ownership（users / user_auth_identities 表 CRUD），costrict-web 通过 read-through RPC 调用。**不含** JWT 自签、OAuth callback 接管、employment_identities——这些留到 Phase A 及之后。

**任务清单**：

| # | 任务 | 涉及文件 | 来源 |
|---|---|---|---|
| P0-1 | cs-user 服务骨架（gin + /healthz + 配置加载） | `cs-user/cmd/api/main.go`、`cs-user/internal/config/` | ADR D1, D9 |
| P0-2 | 独立 PostgreSQL 实例 + cs-user schema | `cs-user/migrations/`（从 server/migrations 复制 user 相关） | ADR D3 |
| P0-3 | User / UserAuthIdentity 模型 + CRUD service 迁移 | `cs-user/internal/models/`、`cs-user/internal/user/`、`cs-user/internal/handlers/` | ADR D1 |
| P0-4 | 内部 API 共享密钥认证中间件（`X-Internal-Token` header） | `cs-user/internal/middleware/internal_auth.go` | ADR D8 |
| P0-5 | Helm chart（cluster-internal only，network policy 限制） | `deploy/charts/cs-user/` | ADR D9 |
| P0-6 | ETL 脚本（dry-run + idempotent UPSERT by subject_id） | `cs-user/cmd/etl/main.go` | ADR D6 |
| P0-7 | read-through RPC client：`server/internal/user/rpc_client.go`，复用 CachedUserService | `server/internal/user/` | ADR D4, D5, D6 |
| P0-8 | costrict-web users 表进入 READONLY（写入路由到 cs-user） | 应用层 gate + DB trigger 兜底 | ADR D6 |

**完成标准**：
- cs-user Dockerfile 构建通过，本地 docker-compose 起得来，`/healthz` 返 200
- ETL dry-run 在生产数据快照：行数一致 + 0 字段 drift
- costrict-web 任意 user API（如 `GET /api/users/:id`）走 RPC 路径返回正确数据
- CachedUserService 命中率 > 90%（连续 1 小时压测）
- cs-user DB 独立备份恢复测试通过
- costrict-web users 表进入 READONLY（grep 验证无写入路径）

**不在本阶段**：JWT 自签、OAuth callback 接管、employment_identities、tenant_configs、tenant_id 列、RLS、webhook。

**协议**：REST only（HTTP/JSON），不引入 gRPC（见 ADR D5）。

---

### 阶段 A：JWT 自签 + 雇佣上下文最小集（**MVP，最高优先级**）

**目标**：让 JWT 不再依赖 Casdoor，并补齐 `employment_identities` 表 + `employment_providers` 配置。这一步是后续所有阶段的基础。

**任务清单**：

> **物理路径说明**：Phase 0 完成后，本阶段所有认证 / 身份相关代码物理路径在 `cs-user/...`（独立服务）。原 ROADMAP 描述的 `server/internal/auth/` 等路径已迁移到 `cs-user/internal/auth/`。`server/internal/middleware/auth.go` 仅保留 JWT 验签逻辑（依赖 cs-user 的 JWKS endpoint）。

| # | 任务 | 涉及文件 | 来源提案 |
|---|---|---|---|
| A1 | 新建 `employment_identities` 表迁移 | `server/migrations/202607XX_create_employment_identities.sql`（cs-user 范围，Stage D 前物理在 server/） | MULTI_TENANCY §6.5.1, §8 |
| A2 | 新建 `tenant_configs` 表（最小 schema：`tenant_id` + `yaml` 列） | `server/migrations/202607XX_create_tenant_configs.sql` | MULTI_TENANCY §9.2 |
| A3 | 实现 JWT 自签（RS256 + JWKS endpoint），保留 Casdoor JWT 30 天兼容窗口 | `server/internal/auth/jwt_signer.go`（新）、`server/internal/middleware/auth.go`（改） | USER_CENTER Part II、MULTI_TENANCY §12、§9 下游兼容矩阵 |
| A4 | OAuth callback 中加 `ApplyEnterpriseMapping` 步骤，按 `employment_providers.enabled` 门控 | `server/internal/user/service.go`、`server/internal/handlers/users.go` | MULTI_TENANCY §11.1[5]、§11.4.2 |
| A5 | JWT claims **保留** `universal_id` / `sub` / `provider` / `preferred_username` / `email` / `phone` / `exp`，**新增** `enterprise` Map + `primary_provider` + `tenant_id` 字段（详见 §9 下游兼容矩阵） | `server/internal/user/service.go:19-31` | MULTI_TENANCY §12.1 |
| A6 | 默认 tenant 引导脚本：把现有用户全部归入 `default` tenant | `server/migrations/202607XX_bootstrap_default_tenant.sql` | MULTI_TENANCY §26 |
| A7 | **接管 Casdoor 的 `/oidc-auth/api/v1/plugin/login` OAuth 端点**（cs-cloud / csc 的登录入口）。两种策略二选一：(a) costrict-web 实现 OP 端点直接签 JWT；(b) 保留 Casdoor OAuth 前端，callback 后用 costrict-web 私钥重签 JWT（替换 Casdoor 签名） | `server/internal/handlers/oidc_auth.go`（新或改） | IDENTITY_FEDERATION_DECISION v2、§9 |
| A8 | 灰度发布：先双签（Casdoor + costrict-web 同时签），下游服务逐步切换；30 天后停 Casdoor 签 | 部署配置 + feature flag `jwt_self_sign_enabled` | §9 |

**完成标准**：
- 用户登录后 JWT 中能看到 `enterprise.employee_id` / `enterprise.department`
- 用 GitHub 登录（非 employment provider）不污染 enterprise 字段
- cs-cloud / csc / assistant-ui 三个下游服务**无需发版**仍能正常工作（详见 §9）
- 关掉 Casdoor JWT 验证 30 天后系统仍正常工作

**不在本阶段**：tenant_id 列、RLS、tenant_admin 角色、cs-user 服务抽离、webhook。

---

### 阶段 B：tenant 维度落地（数据隔离）

**目标**：所有用户相关表加 `tenant_id` 列，应用层 query 一律 scope by tenant，PostgreSQL RLS 作为安全网。

**任务清单**：

| # | 任务 | 来源提案 |
|---|---|---|
| B1 | `tenants` + `tenant_admins` 表迁移 | MULTI_TENANCY §7 |
| B2 | 给 `users` / `user_auth_identities` / `user_profile` / `employment_identities` 加 `tenant_id` 列 + 索引 | MULTI_TENANCY §8 |
| B3 | tenant resolution：subdomain → email domain → 显式选择 | MULTI_TENANCY §5 |
| B4 | 中间件从 JWT 提取 `tenant_id` 注入 request context | MULTI_TENANCY §15、§22 |
| B5 | 应用层所有 query 经 `tenantScope(ctx)` helper | MULTI_TENANCY §10 |
| B6 | PostgreSQL RLS Policy 作为兜底（`CREATE POLICY tenant_isolation ON ...`） | MULTI_TENANCY §10 |
| B7 | `(tenant_id, username)` 联合唯一索引，`email` 保持全局唯一（防钓鱼） | MULTI_TENANCY §6 |

**完成标准**：
- 两个 tenant 同名用户 `alice` 互不冲突
- 跨 tenant 访问被应用层 + RLS 双重拦截

---

### 阶段 C：三级权限 + 管理 API

**目标**：platform_admin / tenant_admin / tenant_member 三级角色 + 对应管理 API。

**任务清单**：

| # | 任务 | 来源提案 |
|---|---|---|
| C1 | 权限模型表 + 中间件 | MULTI_TENANCY §14 |
| C2 | platform_admin API：tenant CRUD、跨 tenant 审计 | MULTI_TENANCY §24 |
| C3 | tenant_admin API：本 tenant 用户列表、IdP 配置、provider_mapping yaml 编辑 | MULTI_TENANCY §25 |
| C4 | 越权防护 + 审计日志 | MULTI_TENANCY §16 |

**完成标准**：tenant_admin 能在 web UI 里改本 tenant 的 provider mapping，看不到其他 tenant 数据。

---

### 阶段 D：cs-user 微服务抽离（**已提前到 Phase 0，本节保留作历史参考**）

> **2026-07-16 ADR 反转**：本阶段原设计为「可无限期推迟，团队 < 10 人 / QPS < 100 不做」。经 [`ADR_CS_USER_PHASE1_DECISIONS.md`](./ADR_CS_USER_PHASE1_DECISIONS.md) 决策，**已提前到 Phase 0 实施**（先做 user 数据 ownership + read-through RPC，不含 JWT 自签）。本节描述保留作历史参考，实际执行以 Phase 0 任务清单为准。

**原目标**：把用户身份相关代码从 `costrict-web` 单体剥离为独立 `cs-user` 服务。

**原判断**：单体内代码（阶段 A/B/C 的产出）已经能用，**抽离是组织扩展需求**，不是功能需求。

**任务清单**：见 `CS_USER_SERVICE_DESIGN.md` Part II-VI，本文不重复。

---

### 阶段 E：身份联邦扩展（**按需启用**）

**目标**：把 Casdoor 降级为「内部 IdP 之一」，集成 idtrust / azure_ad / ldap / github / feishu 等多个外部 IdP。

**判断**：每接入一个新 IdP 是**独立增量**，不需要一次性做完。

**任务清单**：

| # | 任务 | 来源提案 |
|---|---|---|
| E1 | provider_mapping yaml 标准化（per-tenant） | MULTI_TENANCY §17、CS_USER_SERVICE Part IV |
| E2 | tenant 级 IdP 接入（global vs tenant-specific） | MULTI_TENANCY §19 |
| E3a | Gitea JWT 中间件 fork + user 自动开户 + `user_gitea_binding` 维护 | IDENTITY_FEDERATION_DECISION（独立 ADR）+ CS_USER_SERVICE §11；归属 **cs-user** |
| E3b | Gitea `team_user` 同步（GitServerAdapter + GiteaAdapter） | TEAM_ORG_UNIFICATION ADR-3 v3 + CS_USER_SERVICE Part VII（设计参考）；归属 **@server**（`server/internal/gitsync/`） |
| E4 | webhook 用户变更广播系统 | USER_CENTER Part IV、MULTI_TENANCY §21 |

---

## 5. 推荐执行顺序与最小可上线切片

```
[现在] ── 阶段 A（MVP，~2 周）──▶ 可上线：JWT 自签 + 雇佣上下文
                                  │
                                  ▼
        阶段 B（tenant_id，~2 周）──▶ 可上线：多 tenant 数据隔离
                                  │
                                  ▼
        阶段 C（权限 + admin API，~2 周）──▶ 可上线：tenant 自治
                                  │
                                  ▼
        阶段 E1-E2（多 IdP 接入，按需）──▶ 每接一个 IdP 独立上线
                                  │
                                  ▼
        阶段 D（cs-user 抽离，可推迟）──▶ 团队规模触发时再做
                                  │
                                  ▼
        阶段 E3a-E3b-E4（Gitea + webhook，按需）
            E3a：cs-user Gitea user 开户 + binding
            E3b：@server Gitea team_user 同步（v3 新归集）
```

**如果你只能做一件事**：做阶段 A。它独立可上线，且是所有后续阶段的前置条件。

**如果暂时不需要多租户**：只做阶段 A 即可，跳过 B/C。`tenant_id` 全部默认填 `default`，单 tenant 模式跑得很好。

---

## 6. 提案文档归档建议

| 文档 | 当前状态 | 建议动作 |
|---|---|---|
| `MULTI_TENANCY_DESIGN.md` | 评审中，156KB | **保留为权威设计**。本文是它的执行视图 |
| `CS_USER_SERVICE_DESIGN.md` | 评审中，67KB | **保留**。阶段 D 启动时为主输入 |
| `USER_CENTER_DESIGN.md` | 评审中，63KB | **保留**。阶段 A（JWT 自签）和阶段 D 的基础理论 |
| `IDENTITY_FEDERATION_DECISION.md` | Accepted ADR | **保留**。阶段 E3 的输入 |
| `MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md` | 已被吸收 | **可归档**。内容已并入 MULTI_TENANCY §11.4 + 已实现的 `user_auth_identities` 表 |

**不需要删任何文档**——它们是不同抽象层次的资产。但**新加入的同学只需要先读本文 + MULTI_TENANCY_DESIGN 即可上手**，其他作为深入参考。

---

## 7. 开放问题（需要决策）

| # | 问题 | 默认建议 | 决策时机 |
|---|---|---|---|
| Q1 | 是否在阶段 A 就引入 `tenant_id` 列（即使是单 tenant 模式）？ | 是，避免阶段 B 时大表迁移 | 阶段 A 启动前 |
| Q2 | JWT 自签的密钥存储：KMS / Vault / 文件？ | 文件 + 启动时加载，阶段 C 后迁 KMS | 阶段 A 启动前 |
| Q3 | `default` tenant 的命名：`default` / `platform` / `legacy`？ | `default` | 阶段 A 启动前 |
| Q4 | 雇佣上下文 resolution_strategy 默认值：`first_wins` / `last_wins`？ | `first_wins`（更安全） | 阶段 A 启动前 |
| Q5 | RLS 启用时机：阶段 B 一开始就开 / 阶段 B 末尾开？ | 阶段 B 末尾，先让应用层稳定 | 阶段 B 中期 |
| Q6 | cs-user 抽离触发条件（团队规模 / QPS / 部署需求）？ | 团队 > 10 或 QPS > 100 时评估 | 不阻塞 |

---

## 8. 与现有提案的差异点（避免实施时混淆）

本路线图**不修改**任何已有提案的设计决策，仅做执行编排。如果本文与原提案冲突，**以原提案为准**，本文需修正。

唯一例外：**`employment_providers` 雇佣上下文与 IdP 解耦**这一概念（MULTI_TENANCY §11.4.4 重写）是本文编写期间刚定稿的设计，已同步到 MULTI_TENANCY_DESIGN。其他提案未提及此概念，实施时**以 MULTI_TENANCY §11.4 为准**。

---

## 9. 下游服务兼容性矩阵（JWT 自签必须满足的约束）

JWT 自签的最大风险**不是签名算法变更**（三个下游都不验签），而是**claim 名称变更**和**OAuth 登录端点迁移**。下表是阶段 A 实施前必须确认的兼容性约束。

### 9.1 下游服务 JWT 消费行为

| 服务 | 路径 | 签名校验？ | JWT 读取的 claims | **额外用户信息获取** | Casdoor 直接耦合 | 风险等级 |
|---|---|---|---|---|---|---|
| **cs-cloud** | `D:\DEV\cs-cloud` Go 服务 | ❌ 不校验（仅 base64 解 payload） | `universal_id`（首选）/ `sub`（fallback）、`preferred_username`、`displayName` / `name`、`email`、`provider`、`phone` / `phone_number`、`properties`、`exp` | 不另外调 API 取用户信息（claim 即全部） | 间接：通过 `/oidc-auth/api/v1/plugin/login` 端点 OAuth | 🟡 中（claim 依赖最多） |
| **csc** | `D:\dev\csc` CLI 工具 | ❌ 不校验 | 仅 `exp`（`token.ts:28-35`） | **会取**：OAuth token endpoint 返回 JSON body 的 `profile.account.{uuid, email, display_name, created_at}` + `profile.organization.{uuid, has_extra_usage_enabled, billing_type, subscription_created_at}`（`cli/handlers/auth.ts:73-83`）；fallback 调 `${BASE_API_URL}/api/oauth/profile`（`services/oauth/getOauthProfile.ts:40`）和 `ROLES_URL`（`client.ts:279`） | 间接：同上 OAuth 端点 | 🟡 中（依赖 OAuth response shape） |
| **assistant-ui** | `D:\DEV\assistant-ui` TS 前端 | ❌ 不校验（JWT 视为 opaque bearer） | 仅 `exp`（`AssistantCloudAuthStrategy.ts:7-35` 用于缓存过期） | **不取**：`packages/cloud/` 主路径不调任何用户信息 API；唯一例外 `templates/cloud-clerk/`（Clerk OAuth demo）由 Clerk 自管用户信息，不依赖 costrict-web | 无 | 🟢 极低 |
| **quota-manager** | `D:\DEV\quota-manager` Go 服务 | ❌ 不校验（仅 base64 解 payload） | 仅 `universal_id`（`internal/models/models.go:56`，缺失返 error）；解析但未消费 `name` / `staffID` / `github` / `phone` | 不另外调 API 取用户信息（claim 即全部） | 无 | 🟢 低（仅依赖 `universal_id` 单字段，已在 §12.1 canonical） |

### 9.2 兼容性硬约束（不可违反）

实施阶段 A 时，新签的 JWT **必须**满足：

1. **保留所有现有 claim 名称**：`universal_id` / `sub` / `preferred_username` / `name` / `displayName` / `email` / `phone` / `provider` / `properties` / `exp` / `iat`。新增 `enterprise` / `primary_provider` / `tenant_id` 字段为**追加**，不可替换现有字段。**注**：cs-cloud 与 costrict-web 已落地双格式 reader（§9.6），即使 issuer 只发嵌套 canonical 也能正确解析；但遵循此约束可降低其他未升级服务的兼容风险。
2. **`universal_id` 不可降级**：cs-cloud 把它作为主 UserID（`internal/provider/jwt.go:27-32` 优先取 `universal_id`，缺失才 fallback 到 `sub`）；quota-manager 也把它作为**唯一**用户标识（`internal/models/models.go:56`，缺失直接返 error，无 fallback）。新 JWT 必须继续填 `universal_id`，不能只填 `sub`。
3. **`provider` claim 保留语义**：cs-cloud 用它路由（`github` / `email` / `phone` / `idtrust` 等）。值集合**不可变更**，否则 cs-cloud 路由会断。
4. **`exp` claim 必填**：三个服务都依赖它做过期判断。
5. **JWT 头部 `alg` 字段对消费者透明**：因为都不验签，从 HS256 换 RS256 不影响下游；但 `kid`（key id）建议保留以便未来启用验签时平滑过渡。
6. **OAuth token endpoint JSON body 必须保留 `profile` 字段**（csc 依赖）：`POST /oidc-auth/api/v1/plugin/login/token` 返回的 JSON 中，`profile.account.{uuid, email, display_name, created_at}` 和 `profile.organization.{uuid, has_extra_usage_enabled, billing_type, subscription_created_at}` 字段集合**不可移除**（csc `cli/handlers/auth.ts:73-83` 直接读这些字段）。新增字段允许，删字段会导致 csc 看不到用户邮箱/组织信息。Fallback `${BASE_API_URL}/api/oauth/profile` 端点同样需要保留兼容（阶段 A 实施时确认此 endpoint 由 Casdoor 还是 costrict-web 提供）。

### 9.3 OAuth 登录端点迁移（最大风险点）

cs-cloud 和 csc 都通过 `GET /oidc-auth/api/v1/plugin/login` 发起 OAuth 登录，poll `GET /oidc-auth/api/v1/plugin/login/token` 换 token。这是 **Casdoor 插件端点**。

JWT 自签后，这个端点的处理逻辑必须迁移到 costrict-web（或保留 Casdoor 作为 OAuth 前端，costrict-web 在 callback 后用私钥**重签** JWT）。两种策略：

| 策略 | 描述 | 优点 | 缺点 |
|---|---|---|---|
| **(a) costrict-web 直接当 OP** | 实现 `/oidc-auth/api/v1/plugin/login` + `/token` 端点，Casdoor 退化为内部 IdP 之一 | 架构最干净，与 USER_CENTER 终态一致 | 工作量大；要兼容旧 OAuth 参数（如 `provider=casdoor`） |
| **(b) 保留 Casdoor OAuth 前端 + costrict-web 重签**（推荐） | Casdoor 完成 OAuth 后，callback 到 costrict-web，costrict-web 用私钥重新签 JWT 返回给客户端 | 改动最小，下游零感知 | Casdoor 仍需保留；架构未完全干净 |

**默认推荐 (b)**：阶段 A 用 (b)，阶段 D 启动 cs-user 服务抽离时再切到 (a)。

### 9.4 灰度发布序列

```
第 0 天：上线阶段 A 代码 + feature flag `jwt_self_sign_enabled=false`
        ── 行为不变，Casdoor 仍签 JWT
第 1 天：打开 `jwt_self_sign_enabled=true`（双签模式）
        ── JWT 同时含 Casdoor 签名 + costrict-web 签名（两个 token）
        ── cs-cloud / csc / assistant-ui 仍用旧 token，零感知
第 7 天：开始让下游切换到新 token（按 tenant 灰度）
第 30 天：停 Casdoor 签名，只保留 costrict-web 自签
```

### 9.5 兼容性验证检查表（阶段 A 验收必过）

- [ ] 用新 JWT 调 cs-cloud 任意 API，`UserID()` 返回值与旧 JWT 一致
- [ ] 用新 JWT 调 cs-cloud，`provider` 路由正确（github/email/phone/idtrust 各试一次）
- [ ] csc 用新 JWT 完成 login → poll → store → API call 全流程
- [ ] **csc 登录后能看到 `accountInfo.email` / `organization` 等字段**（验证 OAuth token endpoint JSON body 的 `profile.account.*` / `profile.organization.*` 字段完整）
- [ ] assistant-ui 加载页面后 SSE/WebSocket 连接正常（Bearer token 透传）
- [ ] 用新 JWT 调 quota-manager 任意 `/quota-manager/api/v1/*` API，`AuthUser.ID` 解析成功（验证 `universal_id` 字段存在）
- [ ] cs-cloud + costrict-web/server 已落地双格式 reader（见 §9.6），quota-manager / csc / assistant-ui 零改动
- [ ] 30 天灰度结束后，关闭 Casdoor JWT 签名 24 小时观察无异常

### 9.6 下游服务兼容读取策略（双格式 reader）

§9.2 硬约束 #1 要求"保留现有 claim 名称，新增字段为**追加**不可替换"。但 MULTI_TENANCY §12.1 的 canonical claims 把用户基本身份搬进嵌套 `user` Map、把 `provider` 改名 `primary_provider`——若 cs-cloud / costrict-web 直接按 canonical 解析，会失去对旧 Casdoor flat JWT 的兼容。

**解决策略：双格式 reader**——cs-cloud 与 costrict-web 在 JWT 解析层同时支持新旧两种结构，读取规则统一为 **flat first → nested/renamed fallback**：

| 场景 | 行为 |
|---|---|
| 旧 Casdoor JWT（仅 flat） | flat hit，与现状一致 |
| 新 strict canonical JWT（仅 nested `user` Map） | flat miss → nested hit |
| 新 compat JWT（flat + nested 同值） | flat hit |
| 新 compat JWT（flat + nested 异值） | flat 优先，保旧兼容 |

**字段映射表**（每个字段的 fallback 路径）：

| 现有 flat 字段 | 新路径 | 处理 |
|---|---|---|
| `universal_id` | top-level `universal_id`（§12.1 已保留） | 无需 fallback |
| `sub` | top-level `sub`（已保留） | 无需 fallback |
| `id` | `user.id` | flat → nested |
| `preferred_username` | `user.username` | flat → nested |
| `name` | `user.display_name` | flat → nested |
| `displayName` | `user.display_name` | flat → nested |
| `email` | `user.email` | flat → nested |
| `phone` / `phone_number` | `user.phone` | flat → nested |
| `provider` | `primary_provider`（改名，仍顶层） | flat → renamed |
| `picture` / `avatar_url` | `user.avatar_url` | flat → nested |
| `properties` | §12.1 无对应（`enterprise.custom_fields` 语义不同） | **flat-only**，新 token 不带则 properties 解析为空（不影响核心身份） |

**实现锚点**：

| 服务 | 文件 | 改动 |
|---|---|---|
| **cs-cloud** | `D:/DEV/cs-cloud/internal/provider/jwt.go` | `JWTPayload` struct 加 `PrimaryProvider` + `User *JWTUser` 字段；新增 `JWTUser` struct；新增 `emailOrFallback` / `nameOrFallback` / `displayNameOrFallback` / `preferredUsernameOrFallback` / `phoneOrFallback` / `providerOrFallback` helper；`ResolveProvider` + `ResolveDisplayName` 改用 helper |
| **costrict-web/server** | `d:/dev/costrict-web/server/internal/authidentity/normalize.go` | 新增 `lookupNested(claims, "user.email")` + `strNested(claims, paths...)` helper；`NormalizeClaimsMap` 在每个字段读取点的 firstNonEmpty 链尾追加 nested fallback |
| quota-manager | 无需改动 | 仅依赖 `universal_id`（§12.1 保留） |
| csc / assistant-ui | 无需改动 | 仅依赖 `exp`（§12.1 保留） |

**单元测试覆盖**（两服务均落地 4+1 fixture）：
1. 旧 Casdoor flat-only token
2. 新 strict canonical nested-only token
3. 新 compat 模式（flat + nested 同值）
4. 新 compat 模式（flat + nested 异值 → 验证 flat 优先）
5. 部分字段混合（仅 flat / 仅 nested 共存）

测试文件：
- `D:/DEV/cs-cloud/internal/provider/jwt_test.go`
- `d:/dev/costrict-web/server/internal/authidentity/normalize_test.go`

**与 §9.4 灰度序列的关系**：双格式 reader 是灰度期间（Day 0~30）保证业务连续性的关键——Casdoor 旧 JWT 与 cs-user 新 JWT 并存时两个服务都能正确解析，无需协调发版切换。Day 30 停掉 Casdoor 签名后，flat 路径自然 dead，可在后续版本清理。

---

## 10. TL;DR

- 5 份提案**不重复**，是依赖栈；**不要删任何一份**。
- 当前代码实现到「多 IdP 单 tenant」阶段（阶段 0 起点之前）。
- **第一件事是阶段 0**：cs-user 服务抽离（user 数据 ownership + read-through RPC），见 [`ADR_CS_USER_PHASE1_DECISIONS.md`](./ADR_CS_USER_PHASE1_DECISIONS.md)。Phase 0 完成后做阶段 A（JWT 自签 + 雇佣上下文最小集）。
- 多 tenant 不是必需：单 tenant 模式（`tenant_id=default`）也能跑。
- cs-user 微服务抽离**已提前到 Phase 0**（原 ROADMAP 把它放在 Stage D 可推迟，2026-07-16 ADR 反转）。
- 详细设计查原提案，本文是**执行清单 + 阶段切分 + 下游兼容矩阵**。
- JWT 自签对 cs-cloud / costrict-web 是**双格式 reader 改动**（§9.6 已落地，旧/新 JWT 都能解析）；对 csc / assistant-ui / quota-manager **零侵入**——只需保留现有 claim 名称（`universal_id` / `provider` / `exp` 等）+ 维持 `/oidc-auth/api/v1/plugin/login` 端点。
