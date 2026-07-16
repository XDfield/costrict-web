# 用户中心设计提案（costrict-web User Center）

| 字段 | 内容 |
|---|---|
| 状态 | Draft · 评审中 |
| 作者 | CoStrict 团队 |
| 创建日期 | 2026-07-09 |
| 评审范围 | server / casdoor / gitea fork / app-ai-native / csc / gateway |
| 关联文档 | [`CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md`](../repo-management/CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md)（§6 用户中心 + JWT 中间件基线）、[`IDENTITY_FEDERATION_DECISION.md`](./IDENTITY_FEDERATION_DECISION.md)（v2 已决策：II + 方式 3）、[`USER_TABLE_DESIGN.md`](../proposals/USER_TABLE_DESIGN.md)（现有 `users` 表）、[`MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md`](../proposals/MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md)（多 provider 绑定方案 B）、[`CLOUD_TEAM_ARCHITECTURE.md`](../proposals/CLOUD_TEAM_ARCHITECTURE.md)（团队 / 成员关系）、[`CAPABILITY_PORTAL_DECISION.md`](../repo-management/CAPABILITY_PORTAL_DECISION.md)（portal 部署形态） |

> 本文件是 V4 §6 与 `IDENTITY_FEDERATION_DECISION.md` v2 决策的**实施级细化**——把"用户中心主权归 costrict-web"这句话拆成可落地的数据模型、API、生命周期管理、安全约束与迁移路径。所有架构方向（II + 方式 3）严格继承自既有决策，不重新讨论方案选型；只补充决策尚未覆盖的实施细节（如 JWT claims schema、profile 字段聚合规则、跨服务一致性兜底等）。

---

## TL;DR

把"用户是谁"的真相源从 Casdoor 迁到 **costrict-web server**：username / email / 业务字段（业务线 / 部门 / 角色 / 偏好 / 配额）全部自主管理，costrict-web 用 RS256 私钥自签 JWT 并暴露 JWKS endpoint；Casdoor 退化为多登录源 UI 提供者（GitHub OAuth / 短信 / LDAP），仅承担"登录方式选择器"职责；Gitea fork 通过 JWT 中间件（验证 JWKS + 校验 `user_gitea_binding` 状态）认可 costrict-web 签发的身份，账号创建由 sync worker 在 `user.created` 时 eager 完成（详见 §11）。

跨服务引用统一使用不可变 `user_id`（UUID），username 可改但仅用于显示与 URL；用户身份变更通过通用 webhook 系统广播给所有订阅方（Gitea sync / cs-cloud / csc / app-ai-native），6 次指数退避 + 死信队列 + 日级全量校对兜底最终一致性。

**用户中心三层模型**：

| 层 | 真相源 | 内容 |
|---|---|---|
| **Identity 层**（认证凭据） | 多 provider（Casdoor / GitHub / LDAP / 短信） | OAuth sub / 手机号 / LDAP DN 等原始登录凭据；`user_auth_identities` 表关联 |
| **Account 层**（统一账号） | **costrict-web** | `users` 表：不可变 `user_id` + 可变 `username` / `email` / `display_name` / `avatar_url`；自签 JWT |
| **Profile 层**（业务属性） | **costrict-web** | `user_profile` 表：业务线 / 部门 / 角色 / 偏好 / 配额；与 Gitea user 模型完全解耦 |

---

## 目录

```
Part I：动机与目标
  1. 背景与痛点
  2. 目标与非目标

Part II：架构（Static View）
  3. 整体架构（角色分工 + 拓扑）
  4. 三层数据模型（Identity / Account / Profile）
  5. JWT 自签发与 JWKS

Part III：身份与认证（Who & How to Login）
  6. 注册与登录链路
  7. 多 Provider 绑定与主身份升级
  8. username 全生命周期
  9. 用户禁用 / 注销 / 软删除

Part IV：状态同步（Dynamic View）
  10. webhook 多目标广播系统
  11. Gitea binding 同步
  12. Casdoor 退化模式与现有调用链适配

Part V：API 与管理面
  13. 用户中心 REST API
  14. 管理员后台与审计
  15. 用户自助服务（self-service）

Part VI：实施与风险
  16. 数据迁移与切换路径
  17. 风险与对策
  18. 已决策项与开放问题
  附录 A：JWT claims schema 完整定义
  附录 B：webhook event payload 规范
  附录 C：现有 Casdoor 调用点影响盘点
```

---

# Part I：动机与目标

## 1. 背景与痛点

### 1.1 现状（Casdoor 为唯一 IdP）

```
用户 ──► Casdoor (IdP, 多登录源: LDAP/GitHub/短信/密码)
            ├─► costrict-web (验 JWKS, 应用层 UserAuthIdentity)
            │     └─► device gateway / proxy / wecom-bot-proxy / app-ai-native / server
            └─► Gitea (OAuth2 Source = Casdoor OIDC)
```

业务数据库（如 `devices.user_id` / `repositories.owner_id`）只存 Casdoor `sub` 引用，每次需要展示用户名 / 邮箱都依赖 Casdoor Admin API 或 JWT 解析。

### 1.2 痛点

| 痛点 | 表现 | 根因 |
|---|---|---|
| **多源身份不统一** | 同一自然用户用 GitHub / LDAP / 短信登录拿到 3 个不同 sub，下游无法识别为同一人 | Casdoor 默认把每个 source 当独立 user；应用层 `UserAuthIdentity` 合并只在 costrict-web 内有效，Gitea 等下游感知不到 |
| **业务字段无处放** | 业务线 / 部门 / 角色 / 偏好 / 配额无法存进 Casdoor user 模型 | Casdoor user 字段固定，扩展需 fork Casdoor |
| **username 主权缺失** | 改名要走 Casdoor Admin API + 同步 Gitea，链路长且不可控 | username 真相源在 Casdoor，costrict-web 只是被动同步方 |
| **Gitea 与 costrict-web 身份割裂** | Gitea 用 Casdoor OIDC Source，但 user 模型与 costrict-web `users` 表无强约束 | 双方都是 Casdoor 的下游，无统一主权方 |
| **Casdoor 单点故障** | Casdoor 挂 → 全生态登录瘫痪 | Casdoor 同时承担 IdP + 用户管理 + 多登录源 UI 三重职责 |
| **业务查询性能差** | 列表展示用户名 / 邮箱需频繁调 Casdoor API | 业务库无本地 users 表，每次反查走 HTTP |
| **审计归因困难** | 用户改名后 Casdoor 旧 sub 还在但 username 已变；commit author 难以反查到当前 user | 跨服务无统一 `user_id`，依赖可变 username 做关联 |

### 1.3 已有资产（不推翻重做）

- `users` 表（`USER_TABLE_DESIGN.md`）：本地 users 表已落地，含 `casdoor_id` / `casdoor_sub` / `organization` 等 Casdoor 引用字段
- `user_auth_identities` 表（`MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md`）：显式绑定（方案 B）已实施，支持一 user 多 identity
- `UserAuthIdentity` 服务接口：`ResolveOrCreateUserByIdentity` / `BindIdentityToUser` / `ListUserIdentities` / `UnbindIdentity` 已存在
- JWT 解析中间件（`server/internal/middleware/auth.go`）：已支持 JWKS 验签 fallback Casdoor API
- Gitea fork（`CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md` §6.6）：JWT 中间件范围已圈定（~250 行）

---

## 2. 目标与非目标

### 2.1 目标

1. **用户中心主权归 costrict-web**：username / email / 业务字段（业务线 / 部门 / 角色 / 偏好 / 配额）全部由 costrict-web 主权管理，Casdoor 退化为多登录源 UI 提供者
2. **自签 JWT (RS256 + JWKS)**：costrict-web 暴露 `/.well-known/jwks.json`，Gitea fork 与生态内所有服务信任 costrict-web 签发的 JWT
3. **不可变 `user_id` 跨服务引用**：所有业务表 `user_id` 字段统一为 UUID，username 仅用于显示与 URL（可改）
4. **多 provider 显式绑定**：保留 `MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md` 方案 B，用户在已登录态下显式绑定新 identity，按 provider 优先级自动升级主身份
5. **username 全生命周期管理**：注册 / 改名 / 禁用 / 注销 4 个动作有明确状态机，每次变更通过 webhook 广播给下游订阅方
6. **用户中心 API 化**：注册 / 登录 / 改资料 / 绑定 / 解绑 / 禁用 / 注销全部走 costrict-web REST API，对外契约清晰
7. **Gitea binding 自动同步**：用户注册时 sync worker eager 创建 Gitea 账号（`user.created` webhook 触发 `POST /admin/users` + 写 `user_gitea_binding`）；username 改名 / 禁用 / 注销通过 webhook 触发 sync worker 调 Gitea admin API 级联
8. **Casdoor 单点故障隔离**：Casdoor 挂只影响"无法用对应源登录"，已签发 JWT 的用户继续可用；登录后的所有业务请求不再依赖 Casdoor

### 2.2 非目标

- **不重新决策方案选型**：方案 II + 方式 3 已在 `IDENTITY_FEDERATION_DECISION.md` v2 定稿，本文档不重新讨论
- **不替换 Casdoor**：Casdoor 继续作为多登录源 UI 提供者（GitHub OAuth / 短信 / LDAP / 密码），仅退化其 IdP 角色
- **不实现标准 OIDC OP 端点**：`/oauth2/authorize` / `/oauth2/token` / `/oauth2/userinfo` 不在范围（Gitea 通过 fork JWT 中间件而非 OAuth2 Source 信任 costrict-web）
- **不做账号自动归并**：两个已存在本地用户的自动合并不支持（`MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md` §2 已声明非目标）
- **不做 IdP 多租户**：单 tenant 模型，不引入 organization 概念在 user center 层（业务线层级在 §4.3 `user_profile.business_line_id` 表达）
- **不做用户密码自管**：密码登录仍由 Casdoor（或对应 IdP）承担，costrict-web 不存密码哈希；密码相关查询透传给 Casdoor
- **不引入 Keycloak / Authentik 等替代品**：保留 Casdoor 现状，仅重新定位

---

# Part II：架构（Static View）

## 3. 整体架构

### 3.1 角色分工

| 组件 | 职责 | 主权范围 |
|---|---|---|
| **Casdoor** | 多登录源 UI 提供者（GitHub OAuth / 短信 / LDAP / 密码），处理 OAuth/OIDC 协议细节 | 仅承担"用户选哪种方式登录"的 UI 与协议层；不签发下游认可的 JWT |
| **costrict-web server** | **用户中心 OP**：自签 JWT、维护 `users` / `user_profile` / `user_auth_identities` / `user_gitea_binding`，发布 webhook | 全生态用户身份真相源；业务字段（业务线 / 部门 / 角色 / 偏好 / 配额）主权 |
| **costrict-web gateway** | HTTP 反向代理，附加 cookie 解析 / JWT 注入 / X-Forwarded-* header 透传 | 仅做转发，不持业务状态 |
| **Gitea（fork）** | 通过 JWT 中间件（§6.6 V4）验证 costrict-web JWKS + 校验 `user_gitea_binding.sync_status='synced'`（非 synced 返回 503）；账号创建由 sync worker 在 `user.created` 时 eager 完成 | 仅存 Gitea 内部 user 表（identity 用），业务字段从 costrict-web JWT claims 读取 |
| **cs-cloud / csc / app-ai-native** | webhook 订阅方；按 `user_id` 缓存本地用户摘要；JWT 验签信任 costrict-web JWKS | 各自本地缓存层，真相源始终是 costrict-web |
| **`costrict-system` 服务账号** | 单一 site-level admin PAT，仅用于 §7.2 V4 列出的 2 个跨用户场景 | 严禁用于代理用户操作 |

### 3.2 部署拓扑

```
┌────────────────────── 用户 / AI agent ──────────────────────┐
│                                                              │
│  浏览器 / csc / SDK           AI agent（PAT）                │
│   │                            │                             │
└───┼────────────────────────────┼─────────────────────────────┘
    │                            │
    ▼                            ▼
┌──────────────────────────────────────────────────────────────┐
│              Casdoor（多登录源 UI 提供者）                    │
│   GitHub OAuth / 短信 / LDAP / 密码                           │
│   [登录 UI / OAuth 协议层] —— 不签发下游认可的 JWT            │
└──────────────┬───────────────────────────────────────────────┘
               │ OAuth callback (code)
               ▼
┌──────────────────────────────────────────────────────────────┐
│           costrict-web server（用户中心 OP）                  │
│                                                              │
│   POST /api/auth/login            交换 code → 颁发 JWT        │
│   POST /api/auth/refresh           刷新 JWT                  │
│   POST /api/auth/logout            撤销 session              │
│   GET  /api/auth/resolve           gateway 子请求验 cookie   │
│   GET  /api/users/me               当前用户 profile          │
│   PATCH /api/users/me              改 username / avatar      │
│   POST  /api/users/me/identities   绑定新 provider           │
│   DELETE /api/users/me/identities/:id  解绑                  │
│   GET   /.well-known/jwks.json     公钥分发                  │
│                                                              │
│   users / user_profile / user_auth_identities / user_gitea_binding│
│   webhook_subscriptions / webhook_deliveries                 │
│                                                              │
│   JWT 签名密钥：RS256 私钥（secret store 注入）              │
└──────┬────────────────────────────────────────┬──────────────┘
       │ JWT (Authorization: Bearer / cookie)   │ webhook 广播
       │                                        │ (user.updated/disabled/deleted)
       ▼                                        ▼
┌──────────────────────────┐       ┌──────────────────────────────┐
│  Gitea (fork JWT 中间件) │       │  订阅方：                     │
│   验 JWKS                │       │  - gitea-sync-worker          │
│   校验 binding 状态      │       │  - cs-cloud                   │
│   注入 session           │       │  - csc-notify                 │
│   (业务字段从 claims 读) │       │  - app-ai-native              │
└──────────────────────────┘       └──────────────────────────────┘
```

### 3.3 数据流方向（关键约束）

```
登录态流转（单向）：
  Casdoor 多源登录 → costrict-web 交换 + 自签 JWT → 下游服务验 JWKS

用户属性流转（双向）：
  costrict-web users 表 ◄──► Casdoor（仅 login/profile 拉取，不再写）
  costrict-web users 表 ──► Gitea user 表（webhook 触发单向同步）

业务字段流转（单向）：
  costrict-web user_profile ──► 下游只读缓存
  禁止下游写业务字段（业务线 / 部门 / 配额由 admin 在 costrict-web 维护）
```

**核心原则**：costrict-web 是用户身份与业务字段的**唯一写入方**，下游所有服务（含 Gitea）都是只读消费者。

---

## 4. 三层数据模型（Identity / Account / Profile）

### 4.1 Identity 层：`user_auth_identities`（已存在，沿用）

> 已在 `MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md` §4.2 实施，本节仅补充字段约束。

```sql
CREATE TABLE user_auth_identities (
    id               BIGSERIAL PRIMARY KEY,
    user_id          UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider         VARCHAR(64) NOT NULL,           -- casdoor | github | ldap | phone | wechat | ...
    issuer           VARCHAR(255),                   -- https://casdoor.example.com | https://github.com | ...
    external_key     VARCHAR(255) NOT NULL,          -- 稳定身份键（issuer + provider_user_id 归一化）
    external_subject VARCHAR(191),                   -- OAuth sub 原值（可能为 issuer/name 格式）
    external_user_id VARCHAR(191),                   -- provider 内部 user id（如 GitHub numeric id）
    provider_user_id VARCHAR(191),                   -- provider 维度的稳定 id（手机号 / LDAP DN / GitHub login）

    display_name     VARCHAR(191),
    email            VARCHAR(191),
    phone            VARCHAR(64),
    avatar_url       TEXT,
    organization     VARCHAR(191),

    is_primary       BOOLEAN NOT NULL DEFAULT false,
    last_login_at    TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_user_auth_identities_external_key
  ON user_auth_identities (external_key);
CREATE INDEX idx_user_auth_identities_user
  ON user_auth_identities (user_id);
CREATE UNIQUE INDEX uq_user_auth_identities_user_primary
  ON user_auth_identities (user_id) WHERE is_primary = true;
```

**业务约束**：

1. 一个 `external_key` 全局只能绑定一个 `user_id`（防止两个本地 user 绑同一外部身份）
2. 一个 `user_id` 可有多条 identity 记录（多 provider 绑定）
3. 同一 `user_id` 任意时刻只能有一条 `is_primary = true`（部分唯一索引保证）
4. `provider + external_user_id` 组合唯一（防止同 provider 同账号绑两次）

### 4.2 Account 层：`users`（基于现有表微调）

> 现有 `users` 表（`USER_TABLE_DESIGN.md`）保留主结构，做以下调整以适配主权迁移：

```sql
-- 新增字段（保留 casdoor_* 字段做迁移期兼容，迁移完成后清理）
ALTER TABLE users
  ADD COLUMN status VARCHAR(32) NOT NULL DEFAULT 'active',
    -- active | disabled | suspended | deleted（软删除）
  ADD COLUMN username_lower VARCHAR(191) GENERATED ALWAYS AS (lower(username)) STORED,
    -- 大小写不敏感唯一约束辅助
  ADD COLUMN primary_provider VARCHAR(64),
    -- 当前 primary identity 的 provider，便于列表展示
  ADD COLUMN password_changed_at TIMESTAMPTZ,
    -- 仅记录"用户在 Casdoor 改过密码的时间"（透传 webhook），不存密码
  ADD COLUMN muted_until TIMESTAMPTZ,
    -- 软禁用到期时间（admin 临时静默用户）
  ADD COLUMN deletion_requested_at TIMESTAMPTZ,
    -- 注销申请时间（30 天 grace period 后真删）
  ADD COLUMN deleted_at TIMESTAMPTZ;
    -- 软删除时间戳

-- 调整约束
CREATE UNIQUE INDEX uq_users_username_lower
  ON users (lower(username)) WHERE deleted_at IS NULL;
  -- username 大小写不敏感唯一；软删除后释放占用

CREATE INDEX idx_users_status
  ON users (status) WHERE deleted_at IS NULL;
CREATE INDEX idx_users_email
  ON users (email) WHERE deleted_at IS NULL AND email IS NOT NULL;
```

**关键字段语义**：

| 字段 | 可变性 | 说明 |
|---|---|---|
| `id` (UUID) | **不可变** | 跨服务引用唯一键；注销后永不复用 |
| `username` | 可改（受约束） | 显示与 URL 用；改名触发 webhook |
| `email` | 可改 | 一个 email 不能同时绑多个 active user |
| `display_name` / `avatar_url` | 可改 | 用户自助编辑 |
| `status` | admin 改 | active / disabled / suspended / deleted 四态 |
| `primary_provider` | 系统自动 | 跟随 `user_auth_identities.is_primary` 切换 |
| `casdoor_*` 字段 | 兼容期保留 | 迁移完成后删除（详见 §16） |

**`username` 命名约束**（Gitea namespace 兼容，详见 `CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md` §6.5）：

- **长度 3-38 字符**（上限受 Gitea user name max=40 约束：Gitea 实际用户名 `u-<username>` 需 ≤ 40，减去 `u-` 前缀 2 字符 → costrict username ≤ 38）
- **字符集**：`[a-z0-9_]`（小写字母 + 数字 + **下划线，禁止短横线 `-`**），不区分大小写存储。**禁止 `-` 是为了避免与 `u-` namespace 前缀产生视觉/解析混淆**
- **首尾字符**：必须字母或数字开头 / 结尾（不允许 `_` 开头结尾）
- **不允许连续下划线**：禁止 `__` 连续出现
- **保留词黑名单**：`admin` / `system` / `costrict` / `bot` / `root` / `support` / `me` / `self` / `new` / `official` 等（详见附录 D）。注：因为 Gitea user 名带 `u-` 前缀，Gitea 默认 reserved（`admin` / `api` / `user` / `owner` / `explore` 等 ~30 个）**自动隔离**，无需在 costrict 层重复维护
- **改名冷却期**：90 天内最多改 1 次（防滥用）

### 4.3 Profile 层：`user_profile`（业务字段独立表）

> 沿用 V4 §14.4 设计，所有业务字段在 costrict-web，Gitea user 模型只存 identity。

```sql
CREATE TABLE user_profile (
    user_id          UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    business_line_id UUID REFERENCES dept_tree(id),
    dept_id          UUID REFERENCES dept_tree(id),
    role             VARCHAR(64),                  -- 业务角色：admin | approver | auditor | member
    role_updated_by  UUID REFERENCES users(id),
    role_updated_at  TIMESTAMPTZ,

    preferences      JSONB NOT NULL DEFAULT '{}',  -- UI 主题 / 通知设置 / 语言 / 时区
    quota            JSONB NOT NULL DEFAULT '{}',  -- 配额：max_repos / max_private_repos / max_pat_count
    tags             JSONB NOT NULL DEFAULT '[]',  -- 自由标签（admin 标注，如 vip / pilot_user）

    hired_at         DATE,
    left_at          DATE,
    employee_id      VARCHAR(64),                  -- HR 工号（与 dept-sync 关联）

    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_user_profile_business_line
  ON user_profile (business_line_id) WHERE left_at IS NULL;
CREATE INDEX idx_user_profile_dept
  ON user_profile (dept_id) WHERE left_at IS NULL;
CREATE INDEX idx_user_profile_role
  ON user_profile (role) WHERE left_at IS NULL;
```

**字段聚合规则**（沿用 `MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md` §3.3 + 业务字段扩展）：

| 字段 | 规则 |
|---|---|
| `users.display_name` | 优先 `user_profile` 自定义名 → primary identity `display_name` |
| `users.avatar_url` | 优先 primary identity；空则 fallback GitHub avatar |
| `users.email` | 仅合法邮箱；优先 primary identity；fallback 其他 identity |
| `users.phone` | 优先 phone provider identity；fallback primary identity |
| `users.organization` | 优先 primary identity，尤其 idtrust / LDAP |
| `users.username` | **稳定，不随 provider 切换**；仅用户主动改名时变 |
| `user_profile.business_line_id` / `dept_id` | 仅 admin 可改（来自 dept-sync webhook） |
| `user_profile.role` | 仅 admin 可改 |
| `user_profile.preferences` | 用户自助编辑 |
| `user_profile.quota` | 仅 admin 可改 |

### 4.4 Gitea 绑定：`user_gitea_binding`（V4 §14.4 沿用）

```sql
CREATE TABLE user_gitea_binding (
    user_id          UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    gitea_uid        INT       UNIQUE,    -- pending 期间为 NULL，sync worker 调 POST /admin/users 成功后回填（对齐 V4 §14.4）
    gitea_username   VARCHAR(64) NOT NULL,
    sync_status      VARCHAR(32) NOT NULL DEFAULT 'pending',
    last_synced_at   TIMESTAMPTZ,
    last_error       TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_user_gitea_binding_gitea_username
  ON user_gitea_binding (gitea_username);
```

**状态机**：

```
pending ──POST /admin/users 成功──► synced
   │                                  │
   │                                  ├──改名 webhook──► pending ──patch成功──► synced
   │                                  │
   └──POST /admin/users 失败─────────►error ──重试6次指数退避──► dead_letter（admin 手工）
```

### 4.5 Webhook 系统：`webhook_subscriptions` / `webhook_deliveries`（V4 §14.4 沿用）

详见 V4 §14.4 与 §6.5，本提案不重复定义。用户中心产生的 `user.updated` / `user.disabled` / `user.deleted` / `user.created` / `user.profile_changed` / `user.identity_bound` / `user.identity_unbound` 共 7 类事件，全部通过该通用系统广播。

---

## 5. JWT 自签发与 JWKS

### 5.1 签名算法与密钥管理

| 项 | 值 |
|---|---|
| 算法 | **RS256**（非对称，便于多服务验签） |
| 私钥 | RSA 2048-bit，存 secret store（Vault / AWS Secrets Manager / k8s secret），不进 git |
| 公钥 | 通过 JWKS endpoint 分发，下游 5min cache |
| 密钥轮换 | `kid` 标识，新旧 key 共存 7 天过渡；旧 key 退役后从 JWKS 移除 |
| 紧急撤销 | admin 一键发布新 `kid` + 推送 webhook 通知下游立即刷 cache |

### 5.2 JWKS endpoint

```http
GET /.well-known/jwks.json
→ 200 {
  "keys": [
    {
      "kty": "RSA",
      "kid": "costrict-2026-07",
      "use": "sig",
      "alg": "RS256",
      "n": "<base64url-modulus>",
      "e": "AQAB"
    }
  ],
  "cache-control": "public, max-age=300"
}
```

**Cache-Control**：5 分钟（300s），与下游 cache TTL 对齐；轮换时通过 `kid` 失效。

### 5.3 JWT claims schema（详见附录 A）

```json
{
  "iss": "https://costrict-web.example.com",
  "sub": "u_abc123def456",                       // 不可变 user_id
  "aud": ["costrict-web", "gitea", "cs-cloud"],
  "exp": 1735689600,
  "iat": 1735686000,
  "nbf": 1735686000,
  "jti": "<uuid>",                               // 一次性 token id（refresh 用）
  "preferred_username": "alice_wonderland",      // 可变，仅展示
  "email": "alice@example.com",
  "email_verified": true,
  "name": "Alice Wonderland",
  "picture": "https://...",
  "locale": "zh-CN",
  "groups": ["costrict:member", "audit-team"],  // 业务角色 + 业务线 group
  "primary_provider": "github",
  "session_id": "<uuid>",
  "auth_time": 1735686000
}
```

**关键字段说明**：

| claim | 来源 | 用途 |
|---|---|---|
| `sub` | `users.id` | 跨服务引用键；下游必须用此值做权限判定 |
| `preferred_username` | `users.username` | 展示与 URL；下游不应基于此做权限判定 |
| `groups` | `user_profile.role` + 业务线聚合 | Gitea team mapping / 业务权限 |
| `primary_provider` | `users.primary_provider` | 标识本次登录用的 provider |
| `auth_time` | 登录时间 | 强制 re-auth 场景（如改密 / 改 2FA） |

### 5.4 Token 生命周期

| Token 类型 | TTL | 用途 |
|---|---|---|
| Access token | **15 分钟** | API 请求；放 Authorization header |
| Refresh token | **30 天**（滑动续期） | 换 access token；存 HttpOnly cookie |
| ID token | 与 access 同 | OIDC 兼容客户端 |
| Session cookie | 与 refresh 同 | 浏览器登录态 |

**强制重新登录场景**（`auth_time` 早于阈值时拒绝）：

- 改 username / email
- 绑定 / 解绑 identity
- 生成 / 撤销 PAT
- 修改 2FA 设置
- admin 强制 logout

### 5.5 Refresh token 轮换

每次 refresh 同时**撤销旧 refresh token** + 签发新对（防 token 重放）：

```
POST /api/auth/refresh
  Cookie: costrict_refresh=<old-refresh>
→ 200 { access_token, ... }
  Set-Cookie: costrict_refresh=<new-refresh>; HttpOnly; Secure; SameSite=Strict
```

旧 refresh token 加入黑名单（Redis，TTL = 原 token 剩余有效期），重放时返回 `invalid_grant` 并记录可疑事件。

---

# Part III：身份与认证（Who & How to Login）

## 6. 注册与登录链路

### 6.1 首次注册（lazy，通过首次登录完成）

> 用户中心不提供独立"注册"端点——所有新用户都通过"首次多源登录"自动创建。

```
用户访问 costrict-web ──► 点击"用 GitHub 登录"
   └─► 跳 Casdoor GitHub OAuth 入口
       └─► Casdoor 处理 OAuth 协议（仅 UI / 协议层）
           └─► 回调 costrict-web /api/auth/login?code=...
               └─► costrict-web 后端：
                   1. 用 code 调 Casdoor token endpoint 换 access_token
                   2. 用 access_token 调 Casdoor userinfo 拿 provider 原始资料
                   3. 归一化 → 生成 external_key
                   4. 查 user_auth_identities：
                      ├─ 命中 → 加载 user，更新 last_login_at
                      └─ 未命中 → 校验 username 唯一 → 创建 user + identity（is_primary=true）
                   5. 自签 JWT (sub = user.id)
                   6. Set-Cookie costrict_session + costrict_refresh
                   7. 异步触发 user.created webhook（仅首次创建）
```

### 6.2 后续登录（已存在用户）

```
用户用任意已绑定 provider 登录
   └─► Casdoor 处理多源 OAuth
       └─► 回调 costrict-web
           └─► 同 §6.1 步骤 1-3
               └─► 查 user_auth_identities：
                   └─ 命中 → 加载 user
                       ├─ status=active → 签 JWT，正常登录
                       ├─ status=disabled → 拒绝登录，提示"账号已禁用"
                       ├─ status=suspended → 检查 muted_until，未到期拒绝
                       └─ status=deleted → 拒绝登录，提示"账号已注销"
```

### 6.3 登录失败兜底

| 场景 | 行为 |
|---|---|
| Casdoor 不可达 | 提示"登录服务暂不可用"；不降级（避免绕过多源校验） |
| 用户已被禁用但仍持有效 JWT | 后续 API 请求时校验 `users.status`，401 撤销 cookie |
| JWT 已过期但 refresh 还在 | 自动 refresh；refresh 也过期则重定向登录 |
| 用户被 webhook 标记删除但仍有活跃 session | 下次 API 请求强制 logout + 撤销所有 token |

---

## 7. 多 Provider 绑定与主身份升级

> 严格继承 `MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md` 方案 B：显式绑定 + 自动主身份升级。本节仅补充与用户中心的集成点。

### 7.1 绑定流程

```
已登录用户访问 /settings/identities ──► 点"绑定 GitHub"
   └─► 跳 Casdoor GitHub OAuth（state 含 user_id + nonce）
       └─► 回调 costrict-web /api/auth/bind?code=...&state=...
           └─► 校验 state.nonce（防 CSRF）
               └─► 同 §6.1 步骤 1-3 归一化 identity
                   └─► BindIdentityToUser(user_id, identity)：
                       1. 检查 external_key 未被其他 user 绑定
                       2. INSERT user_auth_identities (is_primary=false)
                       3. 按 provider 优先级规则决定是否升级 primary
                       4. 触发 user.identity_bound webhook
                       5. 若 primary 切换 → 触发 user.profile_changed webhook（display_name / avatar 可能变）
```

### 7.2 主身份升级规则（沿用既有设计）

```
provider_rank: idtrust(300) > github(200) > ldap(150) > phone(100) > casdoor(50)

新 identity.rank > 当前 primary.rank  →  自动切换 primary
新 identity.rank <= 当前 primary.rank →  保持原 primary
```

### 7.3 解绑流程

```
已登录用户点"解绑 LDAP identity"
   └─► DELETE /api/users/me/identities/:id
       └─► UnbindIdentity(user_id, identity_id)：
           1. 至少保留 1 条 identity（不允许全解绑成孤儿账号）
           2. 解绑的是 primary → 按规则重新选 primary（剩余中 rank 最高）
           3. DELETE user_auth_identities WHERE id = ...
           4. 触发 user.identity_unbound webhook
           5. 若 primary 切换 → 同 §7.1 步骤 5
```

### 7.4 admin 强制解绑

admin 后台可强制解绑任意用户的任意 identity（如发现 identity 被盗用）：

```
admin POST /api/admin/users/:id/identities/:identityId/unbind?reason=security_incident
   └─► 同 §7.3 流程 + 审计日志记录 admin 操作 + 通知用户（邮件 / IM）
```

强制解绑会触发用户**下次请求强制 re-auth**（撤销当前 session）。

---

## 8. username 全生命周期

> 核心约束：`user_id` 永不可变；`username` 可改但走严格状态机 + webhook 广播。

### 8.1 状态机

```
[不存在]
   └─首次登录─► [active]
                  ├─用户改名─► [active, username=t2] ──webhook user.updated──► 下游级联
                  ├─admin 禁用─► [disabled] ──webhook user.disabled──► Gitea login_prohibited
                  │                └─admin 恢复─► [active] ──webhook user.updated──► Gitea 恢复
                  ├─admin 静默─► [suspended, muted_until=T] ──webhook user.disabled──► 同上
                  │                └─到期/手工恢复─► [active]
                  └─用户申请注销─► [deletion_requested] ──30天 grace period──► [deleted]
                                          ├─期间用户撤销─► [active]
                                          └─期间 admin 强制─► [deleted] 立即生效
```

### 8.2 username 改名规则

| 规则 | 内容 |
|---|---|
| 用户主动改名 | 90 天内最多 1 次；冷却期内拒绝 |
| admin 改名 | 不限次数；记录审计 |
| 唯一性 | 大小写不敏感全局唯一（`username_lower` 索引） |
| 保留字 | 黑名单拒绝（详见附录 D） |
| 字符集 | `[a-z0-9_]`（**禁止短横线 `-`**），**首尾必须字母/数字**，**禁止 `__` 连续** |
| 长度 | **3-38 字符**（Gitea `u-<username>` ≤ 40 强约束，见 §4.2） |
| 历史占用 | 旧 username 释放后 1 年内不可被其他 user 注册（防冒充） |

### 8.3 改名级联效应

```
用户提交 PATCH /api/users/me { username: "alice_wonderland" }
   └─► costrict-web：
       1. 校验新 username（唯一 / 保留字 / 冷却期 / 字符集）
       2. 强制 re-auth（auth_time 早于阈值则 401 要求重登）
       3. 事务更新 users.username + users.updated_at
       4. INSERT username_history (user_id, old_username, new_username, changed_at, changed_by)
       5. 触发 user.updated webhook (changed_fields: ["username"])
       6. 同步等待 gitea-sync-worker 确认（30s 超时则降级异步）

gitea-sync-worker 收到 webhook：
   └─► 调 Gitea admin API: PATCH /admin/users/{old_name} { login_name: new, source: 0 }
       └─► Gitea 自动级联：
           - repo ownership: u-alice/* → u-alice_wonderland/*（自动 redirect）
           - commit author: 旧 commit 显示旧 username（git immutable，不可改）
           - PR / issue 提及：保留（Gitea redirect 兜底）

cs-cloud / csc 收到 webhook：
   └─► 失效本地 user cache；下次请求按新 username 加载
```

### 8.4 username 历史表

```sql
CREATE TABLE username_history (
    id           BIGSERIAL PRIMARY KEY,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    old_username VARCHAR(64) NOT NULL,
    new_username VARCHAR(64) NOT NULL,
    changed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    changed_by   UUID NOT NULL REFERENCES users(id),  -- self 或 admin
    change_type  VARCHAR(32) NOT NULL                 -- user_initiated | admin_initiated
);

CREATE INDEX idx_username_history_user
  ON username_history (user_id, changed_at DESC);
CREATE INDEX idx_username_history_old
  ON username_history (old_username, changed_at DESC);
  -- 用于"旧 username 1 年保护期"查询
```

---

## 9. 用户禁用 / 注销 / 软删除

### 9.1 禁用（disable）

admin 主动操作，可逆。

```
admin POST /api/admin/users/:id/disable { reason: "policy_violation" }
   └─► users.status = 'disabled' + muted_until = NULL
       └─► webhook user.disabled：
           - gitea-sync-worker: Gitea PATCH /admin/users/{name} { login_prohibited: true }
           - cs-cloud: 拒绝该用户的新 session；现有 session 5min 内失效
           - csc-notify: 推送用户通知（IM / 邮件）
           - app-ai-native: 终止该用户的 AI agent 任务
```

### 9.2 静默（suspend）

带过期时间的临时禁用。

```
admin POST /api/admin/users/:id/suspend { duration: "7d", reason: "minor_violation" }
   └─► users.status = 'suspended' + muted_until = now() + 7d
       └─► 同 §9.1 webhook
           └─► cs-cloud 启动 cron job：muted_until 到期自动恢复 status=active
```

### 9.3 注销（soft delete with grace period）

用户主动 / admin 强制，30 天 grace period。

```
用户 POST /api/users/me/delete { confirm: "DELETE" }
   └─► 强制 re-auth
       └─► users.status = 'deleted' + deletion_requested_at = now()
           └─► webhook user.deleted (soft)：
               - 所有下游：立即拒绝新请求，撤销 session
               - gitea-sync-worker: Gitea login_prohibited=true，**不删 Gitea user**（保留 30 天审计）
               - cs-cloud: 设备列表保留（admin 可重新分配）

30 天 cron job：
   └─► 扫描 deletion_requested_at < now() - 30天 AND status='deleted' AND deleted_at IS NULL
       └─► 真删除（事务内执行，**显式 DELETE 不依赖 FK CASCADE，因 users 行走软删除保留）：
           - UPDATE users SET deleted_at = now(), email = NULL, username = 'deleted_' || left(user_id::text, 8)
             WHERE id = ?（释放 email / username 占用，保留行做审计）
           - DELETE FROM user_auth_identities WHERE user_id = ?（显式删， CASCADE 不会触发因 users 行未删）
           - DELETE FROM user_gitea_binding WHERE user_id = ?（同上）
           - DELETE FROM user_profile WHERE user_id = ?（profile 一并清理）
           - 调 Gitea admin API: Gitea user ownership 转给 costrict-system + DELETE 原 Gitea user（V4 §7.2.4）
           - webhook user.deleted (hard)：下游彻底清理本地状态
```

### 9.4 grace period 内撤销

```
用户在 30 天内重新登录（用原绑定的 provider）
   └─► 检测 status='deleted' AND deletion_requested_at < now() - 30天 不成立
       └─► 提示"账号正在注销中，是否撤销？"
           └─► 用户确认撤销 → status='active' + deletion_requested_at=null
               └─► webhook user.updated (changed_fields: ["status"])
```

---

# Part IV：状态同步（Dynamic View）

## 10. webhook 多目标广播系统

### 10.1 事件类型清单

| event_type | 触发 | subject 关键字段 | 订阅方 |
|---|---|---|---|
| `user.created` | 首次登录创建 user | `user_id` / `username` / `primary_provider` | gitea-sync / cs-cloud / csc-notify |
| `user.updated` | username / email / display_name / avatar 变更 | `user_id` / `old_username` / `new_username` / `changed_fields` | 全部 |
| `user.profile_changed` | business_line / dept / role / preferences 变更 | `user_id` / `changed_fields` | cs-cloud / app-ai-native |
| `user.disabled` | admin disable / suspend | `user_id` / `reason` / `muted_until` | 全部 |
| `user.enabled` | admin 恢复 disabled/suspended 用户 | `user_id` | 全部 |
| `user.deleted` (soft) | 用户申请注销 / admin 强制 | `user_id` / `deletion_requested_at` | 全部 |
| `user.deleted` (hard) | 30 天 grace period 到期 | `user_id` | 全部 |
| `user.identity_bound` | 绑定新 provider | `user_id` / `identity_id` / `provider` | gitea-sync（按需更新 Gitea OAuth binding） |
| `user.identity_unbound` | 解绑 provider | `user_id` / `identity_id` / `provider` | gitea-sync |
| `user.primary_identity_changed` | 主身份切换 | `user_id` / `old_provider` / `new_provider` | cs-cloud（更新展示） |

### 10.2 投递保证

- **At-least-once**：每个事件至少投递一次；订阅方按 `event_id` 幂等
- **顺序保证**：同一 `user_id` 的事件按 `occurred_at` 顺序投递（partition by user_id）
- **重试策略**：6 次指数退避（1s / 5s / 30s / 2min / 10min / 1h）→ 死信队列 → admin 手工处理
- **全量校对**：每日 cron job，每个订阅方拉取昨日事件清单与本地消费记录比对，发现丢失主动补投

### 10.3 事件 payload schema（详见附录 B）

```json
{
  "event_id": "evt_<uuid>",
  "event_type": "user.updated",
  "occurred_at": "2026-07-09T10:00:00Z",
  "version": "1.0",
  "source": "costrict-web/user-center",
  "subject": {
    "user_id": "u_abc123def456",
    "old_username": "alice",
    "new_username": "alice_wonderland"
  },
  "data": {
    "changed_fields": ["username"],
    "current_state": {
      "username": "alice_wonderland",
      "email": "alice@example.com",
      "primary_provider": "github"
    }
  },
  "signature": "<HMAC-SHA256 over (event_id + occurred_at + subject + data)>"
}
```

---

## 11. Gitea binding 同步

### 11.1 sync worker 职责

`GiteaUserSyncWorker` 订阅 `user.*` 事件，按事件类型调 Gitea admin API（**eager 模式：注册即创建 Gitea 账号**）：

| 事件 | Gitea API | 失败重试 |
|---|---|---|
| `user.created` | `POST /admin/users` 创建 Gitea 账号 + 写 binding（pending → synced） | 6 次指数退避后入死信 |
| `user.updated` (username) | `PATCH /admin/users/{old}` 改名 | 6 次后入死信 |
| `user.disabled` | `PATCH /admin/users/{name}` 设 `login_prohibited=true` | 6 次后入死信 |
| `user.enabled` | `PATCH /admin/users/{name}` 设 `login_prohibited=false` | 同上 |
| `user.deleted` (soft) | `PATCH /admin/users/{name}` 设 `login_prohibited=true`（保留账号） | 同上 |
| `user.deleted` (hard) | `DELETE /admin/users/{name}` + repo ownership 转给 `costrict-system`（V4 §7.2.4） | 同上 |
| `user.identity_bound` | 无 Gitea API 调用（仅更新本地 cache） | N/A |

> **fork JWT 中间件职责变更**：中间件不再 auto-provisioning，仅做 JWT 验证 + 查 `user_gitea_binding.sync_status`，非 `synced` 一律返回 `503 Service Unavailable` + `Retry-After: 5`（严格一致模式，详见 V4 §6.6）。

### 11.2 状态机与 user_gitea_binding

```
[初始] user.created 事件
   └─► sync worker 收到 webhook
       ├─► INSERT user_gitea_binding (sync_status='pending', gitea_username='u-<username>', gitea_uid=NULL)
       └─► POST /admin/users 创建 Gitea 账号
           ├─ 成功 → UPDATE SET sync_status='synced', gitea_uid=<返回>, last_synced_at=now()
           └─ 失败 → UPDATE SET sync_status='error', last_error=... → 6 次指数退避 → dead_letter

[用户访问 Gitea]
   └─► fork JWT 中间件验证 JWT → 查 user_gitea_binding
       ├─ sync_status='synced' → 放行，转 Gitea internal session
       ├─ sync_status='pending' / 'error' / 行不存在 → 503 + Retry-After: 5
       └─ 用户侧：客户端收到 503 后退避重试（worker 通常 100ms~1s 内完成）

[username 改名]
   └─► user.updated 事件触发 worker
       └─► PATCH Gitea user + UPDATE user_gitea_binding SET gitea_username=new
           ├─ 成功：sync_status='synced'
           └─ 失败：sync_status='error', last_error 记录；6 次后 dead_letter

[用户注销]
   └─► user.deleted (hard) 事件
       └─► DELETE Gitea user + DELETE user_gitea_binding（保留 users.deleted_at 软删）
```

### 11.3 一致性兜底

| 风险 | 兜底 |
|---|---|
| webhook 丢失 / worker 宕导致 user_gitea_binding 落后 | 每日 cron 双向比对：正向 `users` vs `binding.gitea_username` 发现漂移修复；反向 `binding` vs `users` 发现孤儿账号标记告警 |
| Gitea user 被外力删除（admin 手工操作） | 每小时探活 `GET /users/{username}`，发现缺失触发 sync worker 重建（调 `POST /admin/users` + UPDATE binding） |
| Gitea 改名 API 失败但实际成功（网络抖动） | 双向校验：改后 GET 确认；用户访问时若 binding 仍 `error`，由每日 cron 兜底修复（不再依赖 fork 中间件 fallback） |
| 存量用户无 Gitea 账号（切换到 eager 模式前注册的） | 部署新版本时启动一次性 migration：扫描 `users` 表所有无 `user_gitea_binding` 的活跃用户，分批调 `POST /admin/users` 补建（限速 10 QPS，避免 Gitea 创建 API 压力） |

---

## 12. Casdoor 退化模式与现有调用链适配

### 12.1 Casdoor 配置调整

```ini
# 关闭 Casdoor 自身作为 IdP 的下游认可
[oauth2]
# Casdoor 自身继续支持 OAuth2 Client（用于接收 GitHub / LDAP 等 source 回调）
ENABLE = true

# 但 Casdoor 颁发的 JWT 不再被下游信任（costrict-web 不再透传 Casdoor JWT）
# 下游服务（含 Gitea）改为信任 costrict-web 自签 JWT
```

### 12.2 现有调用链影响（详见附录 C）

| 调用点 | 现状 | 迁移后 |
|---|---|---|
| `server/internal/middleware/auth.go:parseJWTToken` | 验 Casdoor JWKS | 改为验 **costrict-web JWKS** |
| `server/internal/casdoor/client.go:CasdoorUser` | 业务代码反查 Casdoor user | 改为查 `users` + `user_auth_identities` 本地表 |
| `GET /api/users/:id`（业务方调用） | 转发 Casdoor Admin API | 直查本地 `users` 表 |
| Gitea OAuth2 Source = Casdoor OIDC | Casdoor 颁发 JWT 给 Gitea | 改为 fork JWT 中间件验 costrict-web JWKS |
| cs-cloud / csc 拿用户信息 | 调 Casdoor userinfo | 调 costrict-web `/api/users/:id` |

### 12.3 兼容期（dual-trust）

迁移期 30 天内，下游服务同时验 Casdoor JWKS 和 costrict-web JWKS：

```
auth middleware:
  1. 解析 JWT iss
  2. iss = "casdoor" → 验 Casdoor JWKS（兼容旧 token）
  3. iss = "costrict-web" → 验 costrict-web JWKS（新 token）
  4. 都失败 → 401
```

30 天后下线 Casdoor JWKS 验证。

---

# Part V：API 与管理面

## 13. 用户中心 REST API

### 13.1 公开端点（无需 auth）

```http
POST   /api/auth/login              多源登录入口（Casdoor code → JWT）
POST   /api/auth/refresh            刷新 access token
POST   /api/auth/logout             撤销 session
GET    /.well-known/jwks.json       JWKS 公钥分发
GET    /api/health                  健康检查
```

### 13.2 用户自助端点（require auth）

```http
GET    /api/users/me                获取当前用户 profile
PATCH  /api/users/me                改 display_name / avatar / preferences
POST   /api/users/me/username       改 username（90 天冷却）
POST   /api/users/me/delete         申请注销（30 天 grace）
POST   /api/users/me/delete/cancel  撤销注销申请

GET    /api/users/me/identities     列出已绑定 identity
POST   /api/users/me/identities     绑定新 provider（OAuth 流程入口）
DELETE /api/users/me/identities/:id 解绑 identity

> 注：不提供"手工切换 primary identity"端点。primary 切换完全由 provider rank 规则
> 自动驱动（绑定时升级、解绑时重选），与 `MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md` §2
> 非目标 #2 一致。用户若想切换 primary，应解绑当前 primary 后按所需 provider 顺序重新绑定。

GET    /api/users/me/sessions       列出活跃 session
DELETE /api/users/me/sessions/:id   撤销指定 session
DELETE /api/users/me/sessions       撤销所有 session（除当前）

GET    /api/users/me/pats           列出 PAT
POST   /api/users/me/pats           生成 PAT（V4 §7.3.3）
DELETE /api/users/me/pats/:id       撤销 PAT

GET    /api/users/me/gitea-binding  查 Gitea 同步状态
```

### 13.3 admin 端点（require admin role）

```http
GET    /api/admin/users             列出用户（含 disabled / suspended / deleted）
GET    /api/admin/users/:id         获取任意用户详情
PATCH  /api/admin/users/:id         改任意用户资料
PATCH  /api/admin/users/:id/username 改任意用户 username（不受冷却期约束）
POST   /api/admin/users/:id/disable 禁用用户
POST   /api/admin/users/:id/suspend 静默用户（带过期）
POST   /api/admin/users/:id/enable  恢复用户
POST   /api/admin/users/:id/delete  强制注销（跳过 grace period）

PATCH  /api/admin/users/:id/profile 改 business_line / dept / role / quota / tags
GET    /api/admin/users/:id/identities  列出用户 identity
DELETE /api/admin/users/:id/identities/:identityId  强制解绑 identity

GET    /api/admin/audit-logs        审计日志查询
GET    /api/admin/webhook-deliveries  webhook 投递状态
POST   /api/admin/webhook-deliveries/:id/replay  手工重投死信
```

### 13.4 内部端点（仅 fork Gitea / 内部服务调用）

```http
GET    /api/internal/users/by-username/:username  Gitea sync worker 反查（binding 状态 / user_id）
POST   /api/internal/webhooks/replay          死信手工重投触发
```

> 注：eager 模式下 sync worker 自己写 `user_gitea_binding`，不再需要 fork 中间件回调 `POST /api/internal/users/:id/gitea-binding`（已移除）。

### 13.5 错误响应统一格式

```json
{
  "error": {
    "code": "USERNAME_COOLDOWN",
    "message": "Username can only be changed once every 90 days",
    "details": {
      "last_changed_at": "2026-05-01T...",
      "available_at": "2026-07-30T..."
    },
    "request_id": "<uuid>"
  }
}
```

**标准错误码**：

| code | HTTP | 说明 |
|---|---|---|
| `UNAUTHORIZED` | 401 | 未登录 / token 失效 |
| `FORBIDDEN` | 403 | 权限不足 |
| `RE_AUTH_REQUIRED` | 401 | 需要 recent auth（auth_time 过旧） |
| `USERNAME_TAKEN` | 409 | username 已被占用 |
| `USERNAME_RESERVED` | 400 | username 在保留字黑名单 |
| `USERNAME_COOLDOWN` | 429 | 改名冷却期内 |
| `EMAIL_TAKEN` | 409 | email 已被其他 user 占用 |
| `IDENTITY_ALREADY_BOUND` | 409 | identity 已绑给其他 user |
| `LAST_IDENTITY` | 409 | 不允许解绑最后一个 identity |
| `USER_DISABLED` | 403 | 用户已被禁用 |
| `USER_DELETION_REQUESTED` | 403 | 用户在注销流程中 |

---

## 14. 管理员后台与审计

### 14.1 admin 后台功能

| 模块 | 功能 |
|---|---|
| 用户列表 | 按 status / business_line / dept / role 筛选；批量禁用 / 恢复 |
| 用户详情 | 基本 + identity + profile + gitea-binding + 最近登录 + 审计 |
| Identity 管理 | 强制解绑 / 重置 primary |
| Profile 编辑 | 业务线 / 部门 / 角色 / 配额 / 标签 |
| 注销审批 | 强制注销流程；批量清理 |
| Webhook 监控 | 投递状态 + 死信重投 |
| 审计日志 | 按 actor / event / time 范围查询 |

### 14.2 审计日志表

```sql
CREATE TABLE user_center_audit_log (
    id              BIGSERIAL PRIMARY KEY,
    actor_user_id   UUID,                            -- 操作者（self / admin / system）
    actor_type      VARCHAR(32) NOT NULL,            -- user | admin | system | fork_gitea
    action          VARCHAR(64) NOT NULL,            -- user.update_username | user.disable | identity.bind | ...
    target_user_id  UUID,                            -- 被操作的用户
    payload_hash    VARCHAR(64) NOT NULL,            -- SHA256 of payload（不存原文）
    request_id      UUID,
    source_ip       INET,
    user_agent      TEXT,
    result          VARCHAR(16) NOT NULL,            -- success | failure
    error_code      VARCHAR(64),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_user_center_audit_log_actor
  ON user_center_audit_log (actor_user_id, created_at DESC);
CREATE INDEX idx_user_center_audit_log_target
  ON user_center_audit_log (target_user_id, created_at DESC);
CREATE INDEX idx_user_center_audit_log_action_time
  ON user_center_audit_log (action, created_at DESC);
```

### 14.3 关键审计事件

- 用户改名 / 改 email / 改 display_name
- identity 绑定 / 解绑 / primary 切换
- admin 禁用 / 静默 / 恢复 / 强制注销
- PAT 生成 / 撤销
- JWT 签发 / refresh / 撤销（仅记录 jti + exp，不记 token 原文）
- 登录成功 / 失败
- 强制 re-auth 触发

---

## 15. 用户自助服务（self-service）

### 15.1 自助功能清单

| 功能 | 端点 | 强制 re-auth |
|---|---|---|
| 改 display_name / avatar | `PATCH /api/users/me` | 否 |
| 改 preferences（主题 / 语言 / 通知） | `PATCH /api/users/me` | 否 |
| 改 username | `POST /api/users/me/username` | 是 |
| 改 email（首次设置） | `PATCH /api/users/me` | 是 |
| 改 email（已有值） | `PATCH /api/users/me` + 验证码确认 | 是 |
| 绑定 / 解绑 identity | `POST/DELETE /api/users/me/identities/*` | 是 |
| 生成 / 撤销 PAT | `POST/DELETE /api/users/me/pats/*` | 是 |
| 撤销其他 session | `DELETE /api/users/me/sessions/:id` | 是 |
| 申请注销 | `POST /api/users/me/delete` | 是 |

### 15.2 强制 re-auth 实现

```go
func RequireRecentAuth(maxAge time.Duration) gin.HandlerFunc {
    return func(c *gin.Context) {
        authTime := c.GetTime("auth_time")
        if time.Since(authTime) > maxAge {
            c.AbortWithStatusJSON(401, gin.H{
                "error": gin.H{
                    "code": "RE_AUTH_REQUIRED",
                    "message": "Recent authentication required",
                    "details": gin.H{
                        "max_age_seconds": maxAge.Seconds(),
                        "current_age_seconds": time.Since(authTime).Seconds(),
                    },
                },
            })
            return
        }
        c.Next()
    }
}
```

默认 `maxAge = 5min`；高敏感操作（改 username / 注销）可设 `maxAge = 1min`。

---

# Part VI：实施与风险

## 16. 数据迁移与切换路径

### 16.1 迁移阶段

| 阶段 | 周期 | 关键产出 |
|---|---|---|
| **M0：基础设施** | 1 周 | RS256 密钥对生成 + JWKS endpoint 上线 + `users` / `user_profile` / `user_gitea_binding` 表结构更新 |
| **M1：JWT 自签** | 1 周 | `/api/auth/login` 切换为自签 JWT；下游服务进入 dual-trust 兼容期 |
| **M2：Casdoor 退化** | 1 周 | Casdoor 关闭下游认可；Gitea OAuth2 Source 关闭；fork JWT 中间件上线 |
| **M3：webhook 系统** | 1 周 | `webhook_subscriptions` / `webhook_deliveries` 上线；`user.*` 事件接通；订阅方接入 |
| **M4：Gitea binding sync** | 1 周 | `GiteaUserSyncWorker` 上线；username 改名级联测试 |
| **M5：admin 后台** | 2 周 | 用户管理 / identity 管理 / 注销流程 / 审计查询 |
| **M6：兼容期下线** | M1+30 天 | 关闭 Casdoor JWKS 验证；删除 `users.casdoor_*` 字段；下线 Casdoor Admin API 调用 |
| **M7：稳定运行** | M6+2 周 | 无 P0 故障；下线 V2 fallback；迁移完成 |

### 16.2 现有数据迁移

```sql
-- M0 阶段：users 表新增字段（保留 casdoor_* 兼容）
ALTER TABLE users
  ADD COLUMN status VARCHAR(32) NOT NULL DEFAULT 'active',
  ADD COLUMN primary_provider VARCHAR(64),
  ADD COLUMN deleted_at TIMESTAMPTZ,
  ...;

-- M1 阶段：回填 primary_provider
UPDATE users u
SET primary_provider = (
    SELECT provider FROM user_auth_identities
    WHERE user_id = u.id AND is_primary = true
    LIMIT 1
)
WHERE primary_provider IS NULL;

-- M3 阶段：建立 user_profile（默认值）
INSERT INTO user_profile (user_id, preferences, quota)
SELECT id, '{}'::jsonb, '{}'::jsonb
FROM users
WHERE NOT EXISTS (SELECT 1 FROM user_profile WHERE user_id = users.id);

-- M6 阶段：删除 Casdoor 兼容字段
ALTER TABLE users
  DROP COLUMN casdoor_id,
  DROP COLUMN casdoor_universal_id,
  DROP COLUMN casdoor_sub,
  DROP COLUMN organization;
```

### 16.3 灰度策略

| 灰度维度 | 方式 |
|---|---|
| 用户范围 | 先内部 dogfood → 10% 用户 → 50% → 100%（按 user_id hash 分桶） |
| 服务范围 | costrict-web 先切 → cs-cloud → app-ai-native → csc → Gitea fork |
| 兼容期 | 30 天 dual-trust，发现问题可一键回滚到 Casdoor JWKS |
| 回滚开关 | env `USER_CENTER_JWT_ISSUER=costrict-web|casdoor`（默认 costrict-web） |

---

## 17. 风险与对策

| 风险 | 严重度 | 对策 |
|---|---|---|
| **costrict-web 单点故障 → 全生态登录瘫痪** | 高 | costrict-web 多副本部署 + Redis 共享 session；JWKS CDN 缓存兜底（即使 server 挂，下游仍能验已签 token 直到 exp） |
| **RS256 私钥泄漏** | 高 | 私钥仅存 secret store，进程启动时注入；轮换预案（新 kid + 推送 webhook）；监控异常 JWT 签发模式 |
| **username 改名级联失败**（Gitea 改名 API 失败） | 中 | sync worker 重试 6 次指数退避 → 死信；admin 手工处理；每日 cron 双向比对兜底修复（不再依赖 fork 中间件 fallback） |
| **webhook 投递延迟** | 中 | 6 次退避 + 死信告警；每日 cron 全量校对；订阅方按 event_id 幂等 |
| **Casdoor 不可达**（迁移期） | 中 | 兼容期内 dual-trust；用户已签 JWT 的可继续用；新登录失败提示明确 |
| **identity 被盗用绑定** | 高 | 强制 re-auth；绑定流程加验证码（敏感操作）；admin 强制解绑 + 通知用户 |
| **JWT 被重放** | 中 | access token TTL=15min；refresh token 一次性轮换；JTI 黑名单；IP/UA 异常检测 |
| **审计日志膨胀** | 低 | 90 天热数据 PostgreSQL；超过归档 S3；按月分表 |
| **username 历史占用 1 年保护期被绕过** | 低 | DB 约束 + 应用层双重校验；admin override 需审计记录 |
| **fork Gitea 中间件 rebase 冲突** | 中 | fork 范围最小化（~250 行）；upstream 同步前先跑回归测试；保留 fork 分支独立 |
| **多 provider 绑定 race condition**（同一 identity 被两 user 同时绑） | 中 | `external_key` 唯一约束 + 事务隔离；冲突时拒绝后者并提示 |
| **用户在注销 grace period 内被恶意恢复** | 低 | 撤销注销强制 re-auth；admin override 单独审计 |

---

## 18. 已决策项与开放问题

### 18.1 已决策项（继承既有决策）

| # | 决策点 | 决议 | 来源 |
|---|---|---|---|
| 1 | 用户中心主权方 | **costrict-web**（自签 JWT + 业务字段） | `IDENTITY_FEDERATION_DECISION.md` v2 |
| 2 | Gitea 信任方式 | **fork JWT 中间件**（验 JWKS + 校验 `user_gitea_binding` 状态，账号由 sync worker eager 创建） | 同上 |
| 3 | Casdoor 角色 | **退化为多登录源 UI 提供者** | 同上 |
| 4 | 多 provider 绑定 | **显式绑定（方案 B）** + 自动 primary 升级 | `MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md` |
| 5 | 跨服务引用键 | **不可变 `user_id`（UUID）**；username 仅展示 | V4 §6.3 |
| 6 | username 改名级联 | webhook → Gitea admin API；commit author 不改 | V4 §6.4 |
| 7 | 用户注销 repo 接管 | 转给 `costrict-system` 账号，保留历史 | V4 §7.2.4 |
| 8 | webhook 投递保证 | 6 次指数退避 + 死信 + 日级全量校对 | V4 §6.5 |

### 18.2 本提案新增决策

| # | 决策点 | 决议 | 理由 |
|---|---|---|---|
| 9 | JWT 算法 | **RS256**（非对称） | 便于多服务验签 + JWKS 分发 |
| 10 | Access token TTL | **15 分钟** | 平衡安全与性能；refresh 续期 |
| 11 | Refresh token TTL | **30 天**（滑动续期 + 一次性轮换） | 防重放；用户体验好 |
| 12 | username 改名冷却期 | **90 天 1 次**（用户主动）；admin 不限 | 防滥用；admin 应急通道 |
| 13 | username 历史占用保护 | **1 年** | 防冒充 |
| 14 | 注销 grace period | **30 天** | 用户后悔窗口 + 审计需求 |
| 15 | 强制 re-auth 阈值 | **5min**（默认）；1min（高敏感） | 平衡安全与体验 |
| 16 | 兼容期 | **30 天 dual-trust** | 平滑迁移 + 回滚能力 |
| 17 | 业务字段存储 | **`user_profile` 独立表**，与 `users` 解耦 | 业务字段高频变更，不污染 account 表 |

### 18.3 开放问题（待评审确认）

| # | 问题 | 推荐 | 备注 |
|---|---|---|---|
| 1 | 是否提供标准 OIDC OP 端点（`/oauth2/authorize` 等）给第三方系统？ | 暂不提供 | V3 生态内不需要；未来按需评估 |
| 2 | 用户密码是否要 costrict-web 自管（脱离 Casdoor）？ | 否（继续透传 Casdoor） | 密码管理非用户中心主目标 |
| 3 | 是否支持 SSO 跨域（多域名共享登录态）？ | 支持（同父域 cookie + JWKS 共享） | 详见附录 C |
| 4 | 2FA / MFA 是否在 costrict-web 实现？ | 第一阶段不在；第二阶段评估 | Casdoor 已有 2FA，沿用 |
| 5 | PAT 生成 / 撤销是否走用户中心 API？ | 是（V4 §7.3.3 已圈定） | 与 Gitea PAT 体系打通 |
| 6 | 是否提供用户导入（CSV / LDAP 批量预创建）？ | 第一阶段不在 | 后续 admin 工具 |
| 7 | 跨 tenant 场景（多组织）是否预留？ | 不预留（单 tenant） | 未来需要时重构 |
| 8 | webhook 订阅方如何注册（admin 手工 / API 自助）？ | admin 手工 + 内部 API 双通道 | 防止任意服务订阅敏感事件 |

---

# 附录

## 附录 A：JWT claims schema 完整定义

```json
{
  "iss": "https://costrict-web.example.com",
  "sub": "u_abc123def456789",
  "aud": ["costrict-web", "gitea", "cs-cloud", "app-ai-native"],
  "exp": 1735689600,
  "iat": 1735686000,
  "nbf": 1735686000,
  "jti": "5f1e9b8c-...",
  "preferred_username": "alice_wonderland",
  "email": "alice@example.com",
  "email_verified": true,
  "name": "Alice Wonderland",
  "picture": "https://avatars.example.com/u/abc123",
  "locale": "zh-CN",
  "timezone": "Asia/Shanghai",
  "groups": ["costrict:member", "audit-team", "dept:engineering"],
  "primary_provider": "github",
  "primary_provider_rank": 200,
  "session_id": "sess_<uuid>",
  "auth_time": 1735686000,
  "auth_methods": ["github"],
  "acr": "urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport",
  "amr": ["pwd", "otp"]
}
```

**字段分类**：

| 类别 | 字段 |
|---|---|
| 标准 OIDC claims | iss / sub / aud / exp / iat / nbf / jti |
| 标准 userinfo claims | preferred_username / email / email_verified / name / picture / locale / zoneinfo |
| 业务扩展 | groups / primary_provider / primary_provider_rank |
| session 管理 | session_id / auth_time / auth_methods |
| 安全元数据 | acr / amr |

## 附录 B：webhook event payload 规范

### B.1 公共 envelope

```json
{
  "event_id": "evt_<uuid>",
  "event_type": "<string>",
  "occurred_at": "<RFC3339>",
  "version": "1.0",
  "source": "costrict-web/user-center",
  "subject": { ... },
  "data": { ... },
  "signature": "<HMAC-SHA256 over (event_id|occurred_at|JSON.stringify(subject)|JSON.stringify(data))>"
}
```

### B.2 各事件类型 schema

#### `user.created`

```json
{
  "event_type": "user.created",
  "subject": {
    "user_id": "u_abc123",
    "username": "alice",
    "primary_provider": "github"
  },
  "data": {
    "email": "alice@example.com",
    "display_name": "Alice",
    "primary_identity_external_key": "github:12345"
  }
}
```

#### `user.updated`（username 变更）

```json
{
  "event_type": "user.updated",
  "subject": {
    "user_id": "u_abc123",
    "old_username": "alice",
    "new_username": "alice_wonderland"
  },
  "data": {
    "changed_fields": ["username"],
    "current_state": {
      "username": "alice_wonderland",
      "updated_at": "2026-07-09T10:00:00Z"
    },
    "change_reason": "user_initiated"
  }
}
```

#### `user.disabled`

```json
{
  "event_type": "user.disabled",
  "subject": {
    "user_id": "u_abc123",
    "username": "alice_wonderland"
  },
  "data": {
    "reason": "policy_violation",
    "muted_until": null,
    "actor_user_id": "u_admin01",
    "disabled_at": "2026-07-09T10:00:00Z"
  }
}
```

#### `user.deleted` (hard)

```json
{
  "event_type": "user.deleted",
  "subject": {
    "user_id": "u_abc123",
    "former_username": "alice_wonderland"
  },
  "data": {
    "deletion_type": "hard",
    "deletion_completed_at": "2026-08-08T10:00:00Z",
    "grace_period_started_at": "2026-07-09T10:00:00Z",
    "gitea_repo_ownership_transferred_to": "costrict-system"
  }
}
```

#### `user.identity_bound`

```json
{
  "event_type": "user.identity_bound",
  "subject": {
    "user_id": "u_abc123",
    "identity_id": 42,
    "provider": "ldap"
  },
  "data": {
    "external_key": "ldap:cn=alice,dc=example,dc=com",
    "is_primary": false,
    "primary_changed": false
  }
}
```

### B.3 订阅方验签

```go
func VerifySignature(payload []byte, signature string, secret []byte) error {
    event := parseEnvelope(payload)
    mac := hmac.New(sha256.New, secret)
    mac.Write([]byte(event.EventID + "|" + event.OccurredAt + "|" + jsonMarshal(event.Subject) + "|" + jsonMarshal(event.Data)))
    expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
    if !hmac.Equal([]byte(signature), []byte(expected)) {
        return ErrInvalidSignature
    }
    return nil
}
```

## 附录 C：现有 Casdoor 调用点影响盘点

| 调用点 | 文件路径 | 现状 | 迁移动作 |
|---|---|---|---|
| JWT 解析 | `server/internal/middleware/auth.go:parseJWTToken` | 验 Casdoor JWKS | 切到 costrict-web JWKS |
| Casdoor user 反查 | `server/internal/casdoor/client.go:CasdoorUser` | 业务代码调 Casdoor Admin API | 改为查 `users` + `user_auth_identities` |
| OAuth 回调 | `server/internal/handlers/auth.go:AuthCallback` | Casdoor code → 透传 JWT | 改为 Casdoor code → 自签 JWT |
| Gitea OAuth Source | Gitea `app.ini` `[oauth2]` | ENABLE=true（信任 Casdoor） | ENABLE=false；启用 fork JWT 中间件 |
| cs-cloud 用户信息 | 调 Casdoor userinfo | HTTP 反查 | 改为调 `/api/users/:id` |
| csc 用户信息 | 同上 | 同上 | 同上 |
| app-ai-native | JWT 验签（Casdoor JWKS） | 直接信任 | 改为验 costrict-web JWKS |
| wecom-bot-proxy | 用户身份透传 | JWT 解析 | 改 JWKS endpoint |

## 附录 D：username 保留字黑名单

```
admin, administrator, root, superuser, sudo, sysadmin,
system, costrict, cs, csc, cloud, support, help, info,
bot, robot, agent, ai, official, staff, team, group,
org, organization, public, private, internal, external,
gitea, git, github, gitlab, casdoor, oauth, oidc, sso,
api, sdk, cli, portal, app, web, mail, email, sms,
test, testing, dev, development, staging, prod, production,
null, undefined, none, anonymous, guest, user, users,
me, self, profile, settings, config, configuration,
sales, billing, finance, hr, legal, security, abuse
```

**保留字规则**：

- 大小写不敏感
- 不允许作为 username 的全部 / 前缀（如 `admin-alice` 也拒绝）
- 长度 ≥ 4 的保留字才进黑名单（避免短词误伤）
- admin 可 override（审计记录）

---

> 本提案与 `CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md` §6 严格对齐，与 `IDENTITY_FEDERATION_DECISION.md` v2 决策一致；与 `USER_TABLE_DESIGN.md` / `MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md` 既有设计互补（保留 `users` / `user_auth_identities` 主结构，仅扩展）。评审通过后，可作为 Stage 0（用户中心 + fork Gitea JWT 中间件）的实施基线。
