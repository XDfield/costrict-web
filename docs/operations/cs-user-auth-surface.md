# cs-user 认证表面与 API 文档参考

> 适用范围：cs-user（身份 / 租户 / 审计中心）当前阶段（Phase A/B/C/E3）已落地的认证模型、JWT 结构、内部 RPC 请求头约定，以及对外暴露的 Swagger 文档位置。
>
> 阅读对象：接入 @server 的开发者、运维、安全审计人员。
>
> 最近更新：2026-07-21（C3.4 + JWT 完成切片 + Swagger 重新生成之后）。

---

## 1. Swagger 文档（OpenAPI 3.0）

### 1.1 文件位置

cs-user 内部业务端点全部走 `/api/internal/*`，OpenAPI 规范由 [`swag`](https://github.com/swaggo/swag) 从 handler 上的 godoc 注释自动生成，输出物位于 `cs-user/docs/`：

| 文件 | 用途 |
|---|---|
| `cs-user/docs/swagger.json` | OpenAPI 3.0 JSON 规范 |
| `cs-user/docs/swagger.yaml` | 同上 YAML 形态（人工 diff 友好） |
| `cs-user/docs/docs.go` | 嵌入二进制的 Go 常量，`gin-swagger` 在运行时通过 `/swagger/*` 路径对外暴露 |

### 1.2 重新生成命令

```bash
cd cs-user
make swagger
# 等价于：
#   swag init -g cmd/api/main.go -o docs --parseDependency --parseInternal
```

`swag` 静态分析需要 annotation 里的类型与文件实际 import 别名严格匹配；不匹配会直接 abort。

### 1.3 已覆盖的 27 个端点（截至 2026-07-21）

所有 `/api/internal/*` 端点均要求 `X-Internal-Token` 头（详见第 3 节），这是 cs-user 与 @server 之间的内部 RPC 边界，不对外公开。

| 分组 | 端点 | 说明 |
|---|---|---|
| healthz | `GET  /api/internal/ping` | 内部健康检查 |
| **platform admin**（@server 通过 platform_admin 表面转发）|||
| | `GET    /api/internal/platform/tenants` | 租户列表 |
| | `POST   /api/internal/platform/tenants` | 创建租户 |
| | `GET    /api/internal/platform/tenants/{tenant_id}` | 租户详情 |
| | `PUT    /api/internal/platform/tenants/{tenant_id}` | 更新租户 |
| | `POST   /api/internal/platform/tenants/{tenant_id}/suspend` | 停用 |
| | `POST   /api/internal/platform/tenants/{tenant_id}/restore` | 恢复 |
| | `DELETE /api/internal/platform/tenants/{tenant_id}` | 硬删除 |
| | `GET    /api/internal/platform/audit-logs` | 平台级审计日志（跨租户） |
| **tenant admin**（@server 通过 tenant_admin 表面转发，带 `X-Tenant-Id`）|||
| | `GET    /api/internal/tenant/config` | 读取本租户配置 |
| | `PUT    /api/internal/tenant/config` | 更新本租户配置 |
| | `GET    /api/internal/tenant/provider-mapping` | 读取 IdP 映射 |
| | `PUT    /api/internal/tenant/provider-mapping` | 更新 IdP 映射 |
| | `GET    /api/internal/tenants/{tenant_id}/git-server` | 本租户 Gitea endpoint |
| | `GET    /api/internal/tenants/audit-logs` | 本租户审计日志 |
| | `POST   /api/internal/tenants/resolve-by-email` | 邮箱反查租户 |
| **用户 ops**（@server 通过 admin-users / reissue-token 表面转发）|||
| | `GET    /api/internal/users/list` | 用户列表 |
| | `GET    /api/internal/users/search` | 用户搜索 |
| | `GET    /api/internal/users/by-ids` | 批量按 ID 查 |
| | `POST   /api/internal/users/get-or-create` | 幂等创建 |
| | `GET    /api/internal/users/organizations` | 组织聚合 |
| | `PUT    /api/internal/users/{subject_id}/status` | 启停 / 封禁 |
| | `PUT    /api/internal/users/{subject_id}/profile` | 资料更新 |
| | `POST   /api/internal/users/apply-enterprise-mapping` | 应用企业身份映射 |
| | `POST   /api/internal/users/reissue-token` | 重签 JWT（切换租户 / 续期） |
| | `GET    /api/internal/users/{subject_id}/gitea-binding` | 用户 Gitea 绑定关系 |
| | `GET    /api/internal/users/{subject_id}/auth-identities` | 用户全部 IdP 绑定 |
| | `POST   /api/internal/users/{subject_id}/bind-identity` | 绑定 IdP 身份 |
| | `DELETE /api/internal/users/{subject_id}/identities/{provider}` | 解绑 IdP |
| | `POST   /api/internal/users/transfer-identity` | 身份迁移 |

---

## 2. JWT 结构（`cs-user/internal/auth/claims.go::EnterpriseClaims`）

### 2.1 签名 / 密钥分发

- **算法**：RS256。
- **Issuer**：cs-user（Phase A 切换完成，cs-user 是身份单一真相源）。
- **`kid` 派生**：cs-user 私有 RSA 公钥部分经 RFC 7638 JWK thumbprint 计算（SHA-256，base64url），作为 JWT 头 `kid`。
- **JWKS endpoint**：`GET /.well-known/jwks`（公开、无认证），@server 启动时拉取并按 cache TTL 刷新。轮换流程见 [`docs/operations/jwt-key-rotation.md`](./jwt-key-rotation.md)。

### 2.2 Payload 结构（28 字段，4 组）

所有字段 `omitempty`，Casdoor 旧 token 缺 Phase B/C 字段不会破坏解析。

#### Group 1 — 标准 JWT (RFC 7519)，7 字段

经 `jwt.Claims` interface 接到 `jwt/v5` parser，`exp` / `nbf` 由 parser 强制校验。

| 字段 | JSON key | 说明 |
|---|---|---|
| `Issuer` | `iss` | cs-user |
| `Subject` | `sub` | 用户 subject_id（即 user_center.id），全系统唯一 |
| `Audience` | `aud` | 受众列表（当前为 `["costrict-cloud"]`） |
| `Expiry` | `exp` | 过期时间戳（短 TTL，默认 1h，可配） |
| `NotBefore` | `nbf` | 生效时间 |
| `IssuedAt` | `iat` | 签发时间 |
| `JTI` | `jti` | token 唯一 ID（用于撤销追踪） |

#### Group 2 — OIDC identity（与 Casdoor 旧 token 1:1 兼容），9 字段

@server 既有 `JWTClaims` parser 已覆盖这组；切换 issuer 后无需改前端。

| 字段 | JSON key | 说明 |
|---|---|---|
| `UniversalID` | `universal_id` | 全局唯一 ID |
| `Name` | `name` | 显示名 |
| `PreferredUsername` | `preferred_username` | 用户名 |
| `Email` | `email` | 邮箱 |
| `Picture` | `picture` | 头像 URL |
| `Owner` | `owner` | 所属（保留字段） |
| `Provider` | `provider` | 登录源 IdP（casdoor / oidc / saml ...） |
| `ProviderUserID` | `provider_user_id` | IdP 侧 user id |
| `Phone` | `phone` | 手机号 |

#### Group 3 — 企业上下文（Phase A5，源自 `employment_identities`），7 字段

| 字段 | JSON key | 说明 |
|---|---|---|
| `EmployeeNumber` | `employee_number` | 工号 |
| `JobTitle` | `job_title` | 职位 |
| `JobLevel` | `job_level` | 职级 |
| `EmploymentType` | `employment_type` | 用工类型 |
| `CostCenter` | `cost_center` | 成本中心 |
| `OrgPath` | `org_path` | 组织路径（`/root/dept/team`） |
| `WorkLocation` | `work_location` | 工作地点 |

#### Group 4 — 租户 + 权限（Phase B / C1），5 字段

| 字段 | JSON key | 说明 |
|---|---|---|
| `TenantID` | `tenant_id` | 当前租户 ID |
| `TenantSlug` | `tenant_slug` | URL-friendly 租户键；@server 用它和 cookie/subdomain 比对做跨租户检测（B3b.2c）。Casdoor 旧 token 此字段空，比对跳过 |
| `TenantRoles` | `tenant_roles` | 本租户激活角色数组，e.g. `["owner","admin"]`、`["tenant_admin"]`。普通成员为空 |
| `PlatformAdmin` | `platform_admin` | bool，平台级管理员标志 |
| `PlatformScope` | `platform_scope` | `full` / `support` / `read_only`；仅 `platform_admin=true` 时生效 |

### 2.3 反射锁定（已固化）

`cs-user/internal/auth/claims_test.go::TestEnterpriseClaims_JSONTagVocabularyLock` 通过反射扫描全部 28 个 JSON tag，确保：

- 新增字段不会意外复用既有 tag
- 重命名 tag 会立刻 fail 测试（保护所有下游消费方：@server middleware、运维脚本、前端解析器）

任何修改 `EnterpriseClaims` 字段 / JSON tag 的 PR 必须同步更新该测试的期望集合。

---

## 3. 业务模块收到的合法认证请求头

cs-user `/api/internal/*` 端点的"合法请求"由两层约束定义：

1. **网络边界**：仅 @server RPC client 直连（生产环境通过 docker network / 127.0.0.1；不暴露公网）。
2. **共享密钥**：所有请求必带 `X-Internal-Token`。

信任边界在 `X-Internal-Token`，cs-user **不重新验 JWT**——它信任 @server 已经验过，并把 JWT 关键 claim 翻译成下面 5 个 `X-*` header。

### 3.1 Header 清单

| Header | 必 / 选 | 含义 | cs-user 消费方 |
|---|---|---|---|
| **`X-Internal-Token`** | **必带** | 内部 RPC 共享密钥。`middleware.RequireInternalToken` 校验，缺失或不匹配 → 401。整个 `/api/internal/*` group gate。 | `middleware/internal_auth.go` |
| `X-Tenant-Id` | 选（租户场景必带） | 已解析的 tenant_id（**trusted carrier**）。`ResolveTenant` middleware 把它落到 ctx，给 `tenant.Scope(ctx)` 做行级过滤（gorm 查询自动加 `WHERE tenant_id = ?`）。缺失时回落到 cookie / subdomain / default 租户解析。 | `middleware/tenant.go` |
| `X-Actor-Subject-Id` | 选（写操作审计必带） | 操作者 JWT `sub`。`captureAuditMeta` 写进 `auditlog.RecordParams::ActorSubjectID`，缺失 → audit 行该字段 NULL。 | `handlers/audit.go::captureAuditMeta` |
| `X-Actor-Tenant-Role` | 选 | 操作者在本次操作的租户角色（`tenant_admin` / `owner` / `admin` 等）。 | 同上 |
| `X-Actor-Platform-Scope` | 选 | 操作者的平台权限粒度（`full` / `support` / `read_only`）。仅 `platform_admin=true` 时填。 | 同上 |

常量声明位置：
- `cs-user/internal/middleware/internal_auth.go:15` — `InternalTokenHeader = "X-Internal-Token"`
- `cs-user/internal/handlers/tenant_config.go:72` — `actorSubjectIDHeader = "X-Actor-Subject-Id"`（其余 actor header 同文件声明）

### 3.2 端到端调用链

```
浏览器
  │
  │  Authorization: Bearer <cs-user JWT>     ← 公开边界
  ▼
@server middleware.Auth
  │  - JWKS 拉取 kid → 验签
  │  - 解出 EnterpriseClaims（28 字段）
  │  - c.Set(AuthClaimsKey, claims)
  ▼
@server 业务 handler（platform_admin / tenant_admin / 用户自助）
  │  - 调对应 RPC client 方法
  ▼
@server RPC client
  │  Headers:
  │    X-Internal-Token:        <共享密钥>              ← 必带
  │    X-Tenant-Id:             <claims.TenantID>        ← 租户场景
  │    X-Actor-Subject-Id:      <claims.Sub>             ← 写操作
  │    X-Actor-Tenant-Role:     <claims.TenantRoles[0]>  ← 写操作
  │    X-Actor-Platform-Scope:  <claims.PlatformScope>   ← platform_admin 时
  ▼
cs-user /api/internal/* handler
  │  - RequireInternalToken（gate）
  │  - ResolveTenant（落 X-Tenant-Id 到 ctx）
  │  - captureAuditMeta（落 actor header 到 audit meta）
  │  - tenant.Scope(ctx) 强制行级过滤
  ▼
DB（user_center / tenants / audit_logs ...）
```

### 3.3 审计语义（缺失容错）

`captureAuditMeta` 对 4 个 actor header 的缺失都做 best-effort NULL 落库，**不报错**。设计动机：

- **系统调用**（无 actor）：例如定时同步任务、内部 cron，audit 行的 actor 字段为 NULL 是正确的。
- **未认证调用**：不应到达内部 endpoint（被 `X-Internal-Token` 401 挡住）；到达则 audit NULL + 安全告警。
- **兼容历史**：早期 RPC client 不转发 actor header，cs-user 端 audit 行字段为 NULL，跨版本查询时需要容忍。

### 3.4 跨租户隔离保证（load-bearing）

`X-Tenant-Id` 经 `ResolveTenant` 落到 ctx 后，**所有 cs-user 内部 service 调用 `gorm` 查询都必须经 `tenant.Scope(ctx)`** —— 这是一个 gorm scope，自动追加 `WHERE tenant_id = ?`。

- 行级：`cs-user/internal/user/admin_service.go::SetUserStatus` 第 113 行（读）+ 124 行（写）—— read & write 双向 scope。
- 一个 tenant X 的 admin 通过 @server 表面 `/api/tenant/users/:id/status` targeting Y 的用户 → cs-user 的 `tenant.Scope(ctx)` 让该行不可见 → `gorm.ErrRecordNotFound` → cs-user 边缘返回 HTTP 404 → @server RPC client 翻译成 `ErrAdminUserRPCNotFound` → @server handler 返回 404 给前端。

**fail-closed**：跨租户访问在 DB 层就过滤掉，不依赖应用层 if-else 校验。

---

## 4. 关键文件索引

| 主题 | 路径 |
|---|---|
| JWT payload 结构 | `cs-user/internal/auth/claims.go` |
| JWT 反射锁定测试 | `cs-user/internal/auth/claims_test.go::TestEnterpriseClaims_JSONTagVocabularyLock` |
| JWT 签发器（key 管理、kid 派生）| `cs-user/internal/auth/jwt.go` |
| JWKS endpoint | `cs-user/internal/handlers/jwks.go`（或同包内） |
| 内部 token gate | `cs-user/internal/middleware/internal_auth.go` |
| 租户解析 middleware | `cs-user/internal/middleware/tenant.go` |
| 审计 header 捕获 | `cs-user/internal/handlers/audit.go::captureAuditMeta` |
| Actor header 常量 | `cs-user/internal/handlers/tenant_config.go:69-72` |
| Swagger 输出 | `cs-user/docs/swagger.{json,yaml}` + `docs.go` |
| 密钥轮换 runbook | `docs/operations/jwt-key-rotation.md` |
| 三级权限模型 | `todo/IDENTITY_TENANT_PROGRESS.md` Phase C |

---

## 5. 变更历史

| 日期 | 切片 | 影响 |
|---|---|---|
| 2026-07-20 | JWT 完成切片（4 commits） | cs-user 正式成为 JWT issuer；@server middleware 增加 `provider_user_id` 顶层 claim 读取；轮换 runbook + helper 脚本落地 |
| 2026-07-20 | admin-user-migration（9 commits） | @server `/api/admin/users/*` 身份 + status 走 cs-user RPC；cs-user 成为身份单一真相源 |
| 2026-07-20 | C3.4 tenant_admin 用户状态 | `PUT /api/tenant/users/:id/status` 镜像 platform_admin；cs-user 已 tenant-scoped，无 cs-user 改动 |
| 2026-07-21 | Swagger 重新生成 + 2 annotation 修复 | 27 endpoints 重新同步到当前代码状态 |
