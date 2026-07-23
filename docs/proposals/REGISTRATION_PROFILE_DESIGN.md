# 首次注册自定义 Profile 设计稿

## 1. 背景与目标

`f9177f7` 提交后,server 不再在登录/绑定时覆写用户自有 profile 字段(`DisplayName` / `Email` / `Phone` / `AvatarURL` / `Organization` / `Username`)。但 **CREATE 分支**(`server/internal/user/service.go:805`)仍直接用 JWT claims 填充这些字段的初始值,导致:

- `phone` provider 注册的用户,`display_name` = 电话号码
- `username` = `phone_<number>` 之类的程序化字符串,用户无法自定义
- 企业身份信息(`employment_identities`)与 basic profile 已分离,但 basic profile 缺少"用户自主编辑"入口

本提案目标:

1. **首次注册时,允许用户自定义 `username` 和 `display_name`**
2. **提供配置开关**,允许指定 provider 走自动映射跳过表单(如 `idtrust` 已返回可信 display name)
3. **未完成注册的用户被 gate 拦截**,直到填完 username 才能使用系统
4. **后台管理员可手动修改 username**(用户侧终身不可改)

## 2. 非目标

1. 用户侧不支持后续修改 `username`(一次性定终身)
2. 不做存量用户回填(已存在的用户**视为已完成注册**)
3. 不做 provider 字段映射的可视化配置(Phase 1 硬编码在 cs-user)
4. 不做跨 tenant 维度的平台级配置(策略是 platform-level)
5. 不引入 nickname / bio 等额外 profile 字段(只动 `username` + `display_name`)

## 3. 已锁定的关键决策

| 议题 | 决策 |
|---|---|
| Username 后续可改? | **否**(用户侧);**是**(后台管理员) |
| Username 唯一性范围 | **tenant 内唯一**(`(tenant_id, username)` 复合唯一索引) |
| 未填表单是否放行 | **不放行**;gate 中间件拦截 |
| 存量用户处理 | 视为已完成,不做回填 |
| Basic profile vs Enterprise identity | **分开**(沿用现有 `employment_identities` 与 `users.display_name` 双层模型) |
| 前端仓库 | `D:\DEV\opencode\packages\app-ai-native` |

## 4. 数据模型变更

### 4.1 `users` 表 schema 调整

| 字段 | 变更 | 说明 |
|---|---|---|
| `username` | UNIQUE 约束从全局 → `(tenant_id, username)` 复合唯一 | 允许跨 tenant 重名 |
| `profile_completed_at` | 新增 `*time.Time`,默认 NULL | NULL = 未完成注册,触发 gate |
| `username_source` | **不引入** | 存量视为已完成,无需追溯来源 |

### 4.2 Migration 步骤(R0)

1. **Pre-migration check**:`SELECT tenant_id, username, COUNT(*) FROM users GROUP BY tenant_id, username HAVING COUNT(*) > 1` —— 检测存量跨 tenant 重名,人工合并
2. `DROP INDEX idx_users_username ON users`(全局 unique)
3. `CREATE UNIQUE INDEX idx_users_tenant_username ON users(tenant_id, username)`
4. `ALTER TABLE users ADD COLUMN profile_completed_at TIMESTAMP NULL DEFAULT NULL`
5. **存量回填**:`UPDATE users SET profile_completed_at = created_at WHERE profile_completed_at IS NULL`(全部视为已完成)
6. **回滚**:保留旧 unique 索引 SQL,如出现问题重建全局 unique(需要先处理跨 tenant 重名)

### 4.3 全仓影响扫描

迁移前必须 grep 全仓,确认没有把 `username` 当 stable 标识符(应该都用 `subject_id`):

```bash
# 期望:查询都用 subject_id,username 仅用于展示和登录
grep -rn 'Where("username' --include="*.go" server/ cs-user/
grep -rn '"username"' --include="*.go" server/internal/handlers/
```

## 5. 配置项

环境变量,加载到 server config:

| 变量 | 默认 | 含义 |
|---|---|---|
| `REGISTRATION_NAME_STRATEGY` | `manual` | `manual` = 弹表单;`auto` = provider 在白名单则跳过 |
| `REGISTRATION_AUTO_NAME_PROVIDERS` | `idtrust,github` | auto 模式下,可信到直接跳过表单的 provider |
| `REGISTRATION_FORCE_FORM_PROVIDERS` | `phone` | 无论策略如何,这些 provider 强制弹表单(因通常缺真名) |

策略组合示例:

| Provider | 策略 | 行为 |
|---|---|---|
| `idtrust` | auto | 跳过表单,`display_name` = idtrust `Employment.DisplayName`,`username` = `idtrust_<employee_number>` 自动生成 |
| `github` | auto | 跳过表单,`display_name` = github `name`,`username` = github `login` |
| `phone` | manual | 强制弹表单(避免 display_name = 电话号) |
| `wecom` | manual | 弹表单 |

## 6. API 设计

### 6.1 检查 username 可用性(前端实时校验,去抖动)

```
GET /api/users/me/username-available?username=<candidate>
Authorization: Standard
Response 200: { "available": true }
Response 200: { "available": false, "reason": "taken" | "invalid_format" | "reserved" }
```

校验规则:
- 长度 3-32
- 字符集 `[a-zA-Z0-9_-]`
- 保留词黑名单:`admin`、`root`、`system`、`me`、`api`、`auth`、`register` 等路由冲突词
- tenant 内唯一(查询 `users WHERE tenant_id = ? AND username = ?`)

### 6.2 完成注册(一次性,仅未完成用户可调)

```
POST /api/users/me/complete-registration
Authorization: Standard + ProfileIncompleteUserAllowed
Request:  { "username": "<choice>", "display_name": "<choice>" }
Response 200: { "user": <updated UserDTO> }
Response 409: { "error": "username_taken" }
Response 400: { "error": "invalid_username" | "invalid_display_name" }
```

行为:
- 置 `users.username` + `users.display_name`
- 置 `users.profile_completed_at = NOW()`
- 通过 cs-user RPC 双写
- 不发新 JWT(username 不进 JWT claim,只影响 users 表)

### 6.3 自助修改 display_name(注册后)

```
PATCH /api/users/me/profile
Authorization: Standard
Request:  { "display_name": "<new value>" }
Response 200: { "user": <updated UserDTO> }
```

`username` 不在 patch 范围内(用户侧终身不可改)。

### 6.4 后台管理员修改 username(用户侧不可达)

```
POST /api/admin/users/:subject_id/profile
Authorization: RequireTenantAdmin
Request:  { "username": "<new value>" }   // 可选 display_name
Response 200: { "user": <updated UserDTO> }
Response 409: { "error": "username_taken" }
```

走现有 `rpc_client_admin_user.go` 的 admin surface,扩展 `SetUserProfile` RPC。审计日志记录变更前后值。

## 7. 流程

### 7.1 注册拦截流程

```
OAuth Callback (handlers.go:432)
   ↓
GetOrCreateUser → returns (user, is_new_user, err)
   ↓                                    ↓
existing user                     new user
   ↓                                    ↓
profile_completed_at != NULL?     profile_completed_at = NULL
   ↓                                    ↓ NO
正常跳转                          设置 cookie flag `reg_pending=1`
                                   ↓
                                   前端读取 flag → 跳 `/register/complete`
```

### 7.2 Gate 中间件 `RequireProfileComplete`

注册在 `OptionalAuth` 之后、业务路由之前:

```go
r.Use(middleware.RequireProfileComplete())
```

逻辑:
- 取 `AuthClaims.Sub` → 查 `users.profile_completed_at`
- NULL → 除白名单外所有请求返回 `409 profile_incomplete`
- 白名单:
  - `POST /api/users/me/complete-registration`
  - `GET /api/users/me/username-available`
  - `POST /api/auth/logout`
  - `GET /api/users/me`(只读自己的状态)
  - `/api/health`、`/swagger/*`

性能:与现有 `StatusChecker` 同模式,加 30s TTL cache。

### 7.3 cs-user RPC 变更

`cs-user/internal/handlers/users.go:226` 的 `GetOrCreate`:
- Response 新增 `is_new_user: bool`
- 走 `GetOrCreateUserEx`,返回创建标志
- server 端 `user.Service.GetOrCreateUser` 透传该标志

cs-user 新增 RPC:
- `POST /api/internal/users/:subject_id/complete-registration`(对应 server 6.2)
- `POST /api/internal/users/:subject_id/profile`(对应 server 6.3 / 6.4 共用,admin 调用走 internal secret)

## 8. Provider 字段映射(Phase 1 硬编码)

cs-user 内新增 `provider_mapping.go`,中心化 provider → (username, display_name) 映射规则:

```go
// 伪码
func MapProviderToProfile(provider string, claims *JWTClaims) (username, displayName string, ok bool) {
    switch provider {
    case "idtrust":
        // Employment.DisplayName 已在 enterprise claims 里
        return "idtrust_" + claims.ProviderUserID, claims.ExternalClaims["display_name"].(string), true
    case "github":
        return claims.PreferredUsername, claims.Name, true
    case "phone", "wecom":
        return "", "", false // 强制走表单
    }
    return "", "", false
}
```

Phase 2 可配置化(从 env / tenant config 读 mapping table)。

## 9. 前端(`app-ai-native`)

### 9.1 新增路由

- `/register/complete` —— 注册完成页
  - username 输入(实时校验 + 去抖动调 `/username-available`)
  - display_name 输入
  - "此后不可修改"提示
  - 提交 → `/complete-registration` → 成功后清 `reg_pending` cookie,跳工作台

### 9.2 全局 gate

- 顶层 layout 读 user state,`profile_completed_at == null` 时所有路由(除 `/register/complete`、`/logout`)重定向到 `/register/complete`
- 与后端 gate 形成双保险

### 9.3 后台 admin UI

- 租户成员管理页 → 成员详情 → "修改用户名"操作(仅 tenant admin 可见)

## 10. 风险与回滚

| 风险 | 缓解 |
|---|---|
| `(tenant_id, username)` 复合 unique 与现有跨 tenant 重名冲突 | Pre-migration 检测 SQL + 人工合并 |
| Gate 中间件误拦现有用户 | 存量 migration 步骤 5 全量回填 `profile_completed_at` |
| cs-user / server 双写不一致 | complete-registration 走 transaction,失败回滚 server 本地表 |
| 配置误配(`auto` 模式但 provider 不在白名单) | 默认值保守(`manual`);provider 不在 `AUTO_NAME_PROVIDERS` 时强制走表单 |
| 前端拦截页未上线就启 gate | 部署顺序:先后端 + 前端 ready → 再 enable env(`REGISTRATION_NAME_STRATEGY=manual` 默认即生效,但 gate 通过 feature flag 控制) |

### 10.1 Feature flag

引入 `PROFILE_GATE_ENABLED=true|false`(默认 false),允许 gate 中间件在故障时立即关闭,不影响现有用户。上线后观察 24h 再固化。

### 10.2 回滚 SQL

```sql
-- 1. 关 gate:PROFILE_GATE_ENABLED=false 重启
-- 2. 全量回填,避免新用户被卡:
UPDATE users SET profile_completed_at = COALESCE(profile_completed_at, NOW());
-- 3. (可选)重建全局 unique,前提是无跨 tenant 重名
```

## 11. 实施阶段

| Phase | 范围 | 仓库 |
|---|---|---|
| **R0** | Schema migration + 全仓 username 使用扫描 | `costrict-web/server` |
| **R1** | cs-user `GetOrCreate` 返 `is_new_user`;server `GetOrCreateUser` 透传 | `costrict-web/server` + `cs-user` |
| **R2** | Username 校验 + `GET /username-available` + `POST /complete-registration` + `PATCH /me/profile` | `costrict-web/server` + `cs-user` |
| **R3** | Gate 中间件 + `PROFILE_GATE_ENABLED` feature flag | `costrict-web/server` |
| **R4** | Provider mapping 配置 + cs-user `provider_mapping.go` | `costrict-web/cs-user` |
| **R5** | Admin 改 username RPC + 前端入口 | `costrict-web/server` + `cs-user` + `app-ai-native` |
| **R6** | 前端 `/register/complete` 拦截页 + 全局 gate | `opencode/packages/app-ai-native` |

R6 必须在 R3 启用 flag **之前** 上线 / ready,否则新用户登录即卡死。

## 12. 待评审澄清

1. `users` 表是否已经按 `tenant_id` 分域?`tenants` 表与 `users.tenant_id` 关联语义?(影响复合 unique 的 tenant_id 来源)
2. `username` 保留词黑名单清单需要业务方确认(路由冲突词 + 品牌词 + 系统词)
3. Admin 改 username 是否需要审计日志单独存储,还是用现有 audit log 通道?
4. `auto` 模式下,provider 返回的 username 与现有 tenant 内用户冲突时,fallback 策略?(建议:fallback 到 manual 表单,而非自动加随机后缀)
