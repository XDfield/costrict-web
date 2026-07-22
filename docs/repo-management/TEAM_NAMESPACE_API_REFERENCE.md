# Team Namespace API · 接口参考

> 本文是 @server 暴露给内部可信服务的 team CRUD + team ns + workflow init 接口的**实现级参考**。
> 设计背景 / ADR / 演进历程见 [`TEAM_NAMESPACE_API.md`](./TEAM_NAMESPACE_API.md) v2.0.1。
> 路径算法见 [`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) / [`WORKFLOW_REPO_PATH_ALGORITHM.md`](./WORKFLOW_REPO_PATH_ALGORITHM.md)。
> 租户 Gitea 解析（每租户一个 endpoint）见 [`MULTI_TENANCY_DESIGN.md`](../identity-tenant/MULTI_TENANCY_DESIGN.md) §20。

| 字段 | 内容 |
|---|---|
| 版本 | v1.1（参考稿；与 TEAM_NAMESPACE_API.md v2.0.1 同步；v1.1 新增 team 显式 CRUD + bot 账号模型）|
| 适用范围 | 仅 `/api/internal/*` —— 网关不放行，同 VPC 内可信服务通过 service token 调用 |
| 鉴权 | 服务鉴权（`X-Internal-Service-Token`），**无用户 JWT** |
| 实现状态 | @server 已落地 admin 同步入口（`POST /api/admin/tenants/:tenant_id/teams/:team_id/sync`，仅手动触发）；下表 `/api/internal/*` 接口为待实现契约 |

---

## 接口清单

| # | 方法 + 路径 | 用途 |
|---|---|---|
| 1 | `POST /api/internal/teams` | 创建 team — 唯一触发 Gitea ns + bot 账号 + bot token 创建的入口 |
| 2 | `GET /api/internal/teams/:team_id` | 查询单个 team（元信息 + bot 账号 + 成员计数；**不返回 bot 明文 token**）|
| 3 | `GET /api/internal/teams` | 分页列表，可按 `tenant_id` / 关键字过滤 |
| 4 | `PATCH /api/internal/teams/:team_id` | 更新 team 元信息（display name / description），同步镜像到 Gitea org description |
| 5 | `POST /api/internal/teams/:team_id/members:sync` | 成员同步 — `delta` 增量 或 `full_sync` 全量对账（**不再承担 team ns 创建**，改由接口 1 显式承担）|
| 6 | `POST /api/internal/teams/:team_id/dissolve` | team 解散 — archive org + 全员移除 + 撤销 bot token |
| 7 | `POST /api/internal/teams/:team_id/bot-token:rotate` | 轮换 bot token（旧 token 立即失效）|
| 8 | `POST /api/internal/workflow/init` | workflow 类型 repo + 实例 branch get-or-create；响应附 bot token 给上游 |
| 9 | `POST /api/internal/kb/ensure` | kb repo get-or-create（仅 main，无实例 branch 概念）；响应附 bot token 给上游 csc / 编排器 |

> team 业务定义 / 成员关系的真相源仍是外部 `org-team-service`；@server 仅镜像 Gitea 侧 ns / 成员 / bot 状态，不做业务规则决策。`team_id` 由 org-team-service 在调用前置分配（UUID），@server 视其为幂等键。

---

## 公共约定

### 请求头

所有接口共用：

| Header | 必填 | 说明 |
|---|---|---|
| `X-Internal-Service-Token` | ✓ | 双方预先约定的共享密钥；建议通过 mTLS + sidecar secret 注入，不走配置明文 |
| `X-Tenant-Id` | 否 | **所有接口统一可选**；未传时 @server 使用 `DEFAULT_TENANT_ID`（与 cs-user `ResolveTenant` 中间件三层 resolver → `default` 兜底同语义）。多租户启用前，全部流量均落到 default 租户的 git_server，由 `tenants(default).git_server_id` 解析。**前置约束**：`default` 租户必须已绑定 `git_server` 行（启动期 bootstrap 保证） |
| `X-Request-Id` | 推荐 | 透传到审计日志 + Gitea API trace，缺失则 @server 生成 |
| `Content-Type: application/json` | ✓ | |

### 通用错误码

| HTTP | error_code | 含义 |
|---|---|---|
| 401 | `UNAUTHORIZED_SERVICE` | service token 不一致 |
| 503 | `GITEA_API_FAILURE` | Gitea API 调用失败（已重试 N 次后）|

各接口另有专属错误码，见对应章节。

---

## 机器人账号模型

> v1.1 引入。每个 team 在其所属租户的 Gitea 上拥有一个**专属 bot 账号**，用于：
> - workflow 编排器以团队身份 clone / push workflow 类型 repo
> - 后续团队级数据操作（kb repo 同步、artifact 推送等）
>
> bot 账号是 @server 在 team 创建时**原子产出**的产物之一，与 Gitea ns / 默认权限配置一并落地。

### 命名约定

| 项 | 规则 | 例 |
|---|---|---|
| Gitea username | `bot-t-<team_short_id>` | `bot-t-7f3c9a1e` |
| Gitea user email | `bot+<team_short_id>@costrict.internal` | `bot+7f3c9a1e@costrict.internal` |
| Token name | `costrict-team-bot-<tenant_short>` | `costrict-team-bot-default` |
| Token scope | `write:repository` + `read:user`（最小权限）| — |
| Org 内 team 归属 | `Owners`（Gitea 默认 owner team，保证 bot 可写所有 repo）| — |

> bot username 与人类用户命名空间隔离（`bot-t-*` 前缀），cs-user 在 Gitea username 唯一性校验时**预留** `bot-t-*` 模式，避免人类用户抢占。

### 存储

bot token 是敏感凭据，存储在 @server 的 `team_bot_credentials` 表（每 team 至多一行）：

```sql
CREATE TABLE team_bot_credentials (
    team_id          VARCHAR(191) NOT NULL PRIMARY KEY,
    tenant_id        TEXT         NOT NULL,
    git_server_id    VARCHAR(64)  NOT NULL,         -- 创建时锁定的 Gitea 实例
    gitea_username   VARCHAR(191) NOT NULL,         -- bot-t-<team_short>
    gitea_user_id    BIGINT       NOT NULL,         -- Gitea 侧 user id（用于 API 调用）
    gitea_token_id   BIGINT       NOT NULL,         -- Gitea 侧 token id（用于 rotate / revoke）
    token_encrypted  TEXT         NOT NULL,         -- AES-GCM 密文；KMS-managed key，明文不入库
    token_sha256     CHAR(64)     NOT NULL,         -- 用于在不解密的情况下校验传入 token
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    rotated_at       TIMESTAMPTZ,
    revoked_at       TIMESTAMPTZ,                    -- dissolve 时设置；保留审计窗口

    CONSTRAINT fk_team_bot_credentials_team FOREIGN KEY (team_id) ...
);
```

> **明文 token 的生命周期**：仅存在于 @server 内存（响应给上游业务服务时）+ 上游业务服务自己的密钥存储（推荐 K8s Secret / Vault）。@server 不向任何 GET 接口返回明文 token；**仅**在 `POST /teams`（创建）、`POST /teams/:id/bot-token:rotate`、`POST /workflow/init` 三处响应中返回明文一次。

### 与每租户 Gitea 的关系

bot 账号与 Gitea ns 都归属于 team 的 `tenant_id` 解析出的 git_server（[`MULTI_TENANCY_DESIGN.md`](../identity-tenant/MULTI_TENANCY_DESIGN.md) §20.4）。team 创建时 @server 通过 `ResolveAdapterForTenant(tenant_id)` 解析 Gitea endpoint，并在该 Gitea 上完成：
1. 创建 / 复用 team ns org `t-<team_short>`
2. 创建 bot 用户 `bot-t-<team_short>`
3. 将 bot 加入 org `Owners` team
4. 为 bot 创建 PAT（personal access token）

> 租户级迁移（更换 git_server）超出本文范围；现有 team 锁定在创建时的 git_server_id，迁移走专门 runbook。

---

## 接口 1：POST /api/internal/teams

team 的**唯一创建入口**。原子完成：

1. **前置校验** — `team_id` 是 UUID；`tenant_id` 缺省时解析为 `default`（与公共约定一致）
2. **解析租户 Gitea** — `ResolveAdapterForTenant(tenant_id)`；default 租户的 `git_server_id` 在启动期 bootstrap 时绑定，理论上不会 412；只有显式传了一个未绑定 git_server 的非 default 租户才会 412 `TENANT_GIT_SERVER_UNRESOLVED`
3. **get-or-create team ns Gitea org** `t-<team_short_id>` + 配置 org（`members_can_create_repos=false` + private + 默认 member=write）
4. **get-or-create bot 账号** `bot-t-<team_short_id>`（首次创建时建用户 + 加 org Owners team + 建 PAT）
5. **可选种子成员同步** — `initial_members` 非空时按 `delta` 模式应用
6. 写入 `team_ns` + `team_bot_credentials` 表

幂等：相同 `team_id` 重入 → 返回已存在的 ns + bot 信息（明文 token **不返回**，仅在首次创建时返回）。

### 1.1 请求

```http
POST /api/internal/teams HTTP/1.1
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Tenant-Id: <tenant_id>
X-Request-Id: <uuid>
Content-Type: application/json

{
  "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
  "team_display_name": "Platform Team",
  "creator": { "employee_number": "E-1000" },
  "initial_members": [
    { "user_id": "u-alice-uuid" },
    { "employee_number": "E-1001" }
  ]
}
```

### 1.2 字段

| 字段 | 必填 | 类型 | 说明 |
|---|---|---|---|
| `team_id` | ✓ | UUID | org-team-service 前置分配；@server 视其为幂等键 |
| `team_display_name` | ✓ | string | 用作 org description；非空 |
| `creator` | ✓ | UserRef | 创建者，审计用；不需要预先在 Gitea org 内（首次 sync 时会被加为普通成员）|
| `initial_members` | 否 | array<UserRef> | 种子成员；非空时按 `delta` 模式应用 |

> **UserRef（用户引用模型）**：与接口 5 `members:sync` 一致 —— `user_id` 或 `employee_number` **二选一**，**不接受 `gitea_username`**。同条引用同时传两者或都不传 → 400 `INVALID_REQUEST`。@server 通过 cs-user RPC 解析出 `gitea_username`（必要时触发 giteasync 懒创建），完整解析路径与失败语义见 [接口 5.2 UserRef 模型](#52-字段)。

### 1.3 响应

```json
{
  "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
  "team_ns_org": "t-7f3c9a1e",
  "team_display_name": "Platform Team",
  "git_server_id": "gs-template-...",
  "gitea_base_url": "https://gitea.costrict.local",
  "created": {
    "team_ns": true,
    "bot_account": true,
    "bot_token": true
  },
  "members_added_count": 2,
  "bot": {
    "gitea_username": "bot-t-7f3c9a1e",
    "gitea_user_id": 42,
    "token_id": 17,
    "token": "cs-bot-1a2b3c4d5e6f...",
    "token_sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
  },
  "audit_log_id": "audit-4b5c6d7e-..."
}
```

| 字段 | 必返回 | 说明 |
|---|---|---|
| `team_ns_org` | ✓ | Gitea org 名 `t-<team_short_id>`（UUID 前 8 hex）|
| `git_server_id` | ✓ | 创建时锁定的 git_server id；后续所有操作通过此 id 解析 endpoint |
| `gitea_base_url` | ✓ | 创建时锁定的 Gitea base URL；上游拼接 clone url 用 |
| `created.team_ns` / `bot_account` / `bot_token` | ✓ | 三个子操作是否本次新建；幂等重入为 false |
| `members_added_count` | ✓ | 实际生效的种子成员数（已存在的会被去重）|
| `bot.gitea_username` / `gitea_user_id` / `token_id` | ✓ | bot 账号元信息 |
| `bot.token` | **仅本次响应** | 明文 token；**仅创建时返回一次**，幂等重入不返回（null）。上游必须立即落盘到自己的密钥存储 |
| `bot.token_sha256` | ✓ | 永久返回；用于上游在不持有明文时校验自己缓存的 token 是否仍有效 |

### 1.4 行为分支

| 场景 | server 行为 | `created.team_ns` / `bot_account` / `bot_token` |
|---|---|---|
| team_id 全新 | ① 创建 org ② 配置 org ③ 创建 bot 用户 + 加 Owners team + 建 PAT ④ 应用种子成员 | true / true / true |
| team_id 已存在（幂等重入）| 返回已存在 ns + bot 元信息；**不返回明文 token** | false / false / false |
| `X-Tenant-Id` 缺省 | 解析为 `default` 租户，沿用 default 的 git_server | true / true / true |
| 显式传入一个不存在 / 非 active 的 tenant | 412 `TENANT_NOT_FOUND`（default 不应触发）| — |
| 显式传入一个未绑定 git_server 或 server disabled 的 tenant | 412 `TENANT_GIT_SERVER_UNRESOLVED`（default 不应触发，bootstrap 保证）| — |
| bot username 已被人类用户抢占 | 409 `BOT_USERNAME_TAKEN` | — |

### 1.5 专属错误码

| HTTP | error_code | 触发条件 |
|---|---|---|
| 400 | `INVALID_REQUEST` | `team_id` 非 UUID / `team_display_name` 缺失 / `initial_members` 与 `creator` 重叠 / `creator` 或任一 `initial_members` 不是合法 UserRef |
| 404 | `MEMBER_USER_NOT_FOUND` | `creator` 或任一 `initial_members` 解析失败（cs-user 侧 `users.subject_id` / `employment_identities.employee_number` 均无匹配；先经 cs-user `giteasync` 推用户）|
| 409 | `BOT_USERNAME_TAKEN` | `bot-t-<team_short>` 在 Gitea 已被人类用户占用（应通过 cs-user 预留规则避免）|
| 412 | `TENANT_NOT_FOUND` | `tenant_id` 在 `tenants` 表不存在或非 active |
| 412 | `TENANT_GIT_SERVER_UNRESOLVED` | tenant 未绑定 git_server 或对应 server disabled |

---

## 接口 2：GET /api/internal/teams/:team_id

查询单个 team 状态。**响应不含明文 bot token**；上游需要 token 时应通过 `bot-token:rotate`（强制轮换）或 `workflow/init`（带实例上下文）获取。

### 2.1 请求

```http
GET /api/internal/teams/7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a HTTP/1.1
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Request-Id: <uuid>
```

### 2.2 响应

```json
{
  "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
  "team_ns_org": "t-7f3c9a1e",
  "team_display_name": "Platform Team",
  "git_server_id": "gs-template-...",
  "gitea_base_url": "https://gitea.costrict.local",
  "status": "active",
  "member_count": 12,
  "bot": {
    "gitea_username": "bot-t-7f3c9a1e",
    "gitea_user_id": 42,
    "token_id": 17,
    "token_sha256": "9f86d081...",
    "created_at": "2026-07-21T10:00:00Z",
    "rotated_at": "2026-07-21T10:00:00Z"
  },
  "created_at": "2026-07-21T10:00:00Z",
  "updated_at": "2026-07-21T10:00:00Z"
}
```

### 2.3 行为分支

| 场景 | server 行为 |
|---|---|
| team 存在 + active | 200 + 完整字段 |
| team 已 archived（dissolved）| 200 + `status=archived` + `bot.rotated_at` 为 null + `bot` 字段保留元信息但明文已撤销 |
| team_id 从未创建过 | 404 `TEAM_NOT_FOUND` |

### 2.4 专属错误码

| HTTP | error_code | 触发条件 |
|---|---|---|
| 400 | `INVALID_REQUEST` | `team_id` 非 UUID |
| 404 | `TEAM_NOT_FOUND` | `team_id` 从未通过 `POST /teams` 创建 |

---

## 接口 3：GET /api/internal/teams

分页列表，主要供平台管理后台 / `org-team-service` 对账使用。

> **不做跨租户查询**：本接口**始终**按单一 tenant 过滤，租户来源严格遵循公共约定（`X-Tenant-Id` 缺省 → `default`）。**不接受** `tenant_id` 作为 query 参数；平台级聚合需求（如统计全租户 team 总数）走专门的 admin / metrics 接口，不在此契约内。理由：跨租户查询会绕过 `tenant.Scope(ctx)` 行级过滤模型，破坏 MULTI_TENANCY_DESIGN.md §20 的隔离边界。

### 3.1 请求

```http
GET /api/internal/teams?page=1&page_size=50&q=platform HTTP/1.1
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Tenant-Id: <tenant_id>   # 可选；缺省 = default
```

| Query 参数 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `page` | 否 | 1 | 1-based |
| `page_size` | 否 | 50 | 上限 200 |
| `q` | 否 | — | 模糊匹配 `team_display_name`（LIKE %q%）|
| `status` | 否 | — | `active` / `archived`；不传则全部 |

> ~~`tenant_id` query 参数~~ 已移除。租户范围**只**通过 `X-Tenant-Id` header 注入，由 `tenant.Scope(ctx)` 在 SQL 层强制过滤，调用方无法绕过。

### 3.2 响应

```json
{
  "teams": [
    {
      "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
      "team_ns_org": "t-7f3c9a1e",
      "team_display_name": "Platform Team",
      "status": "active",
      "member_count": 12,
      "created_at": "2026-07-21T10:00:00Z"
    }
  ],
  "page": 1,
  "page_size": 50,
  "total": 1
}
```

> 响应**不**回显 `tenant_id` 字段 —— 单一租户上下文已经隐含在请求 header 中，回显反而暗示"可能是别的租户"。如需租户自省，调用方读自己发的 `X-Tenant-Id` 即可。

### 3.3 专属错误码

| HTTP | error_code | 触发条件 |
|---|---|---|
| 400 | `INVALID_REQUEST` | `page` / `page_size` 非法 / `status` 非法枚举 / 调用方误传了 `tenant_id` query 参数（明确拒绝以避免误用） |

---

## 接口 4：PATCH /api/internal/teams/:team_id

更新 team 元信息。当前仅支持 `team_display_name` 与 `description`，并镜像到 Gitea org description。

### 4.1 请求

```http
PATCH /api/internal/teams/7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a HTTP/1.1
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Request-Id: <uuid>
Content-Type: application/json

{
  "team_display_name": "Platform Team (renamed)",
  "description": "Owns platform infra"
}
```

### 4.2 字段

| 字段 | 必填 | 类型 | 说明 |
|---|---|---|---|
| `team_display_name` | 否 | string | 缺省时不动 |
| `description` | 否 | string | 缺省时不动；非空时镜像到 Gitea org description |

> 不通过本接口修改 tenant 归属、bot 账号、ns 名（这些走专门的 dissolve + recreate runbook）。

### 4.3 响应

204 No Content。

### 4.4 专属错误码

| HTTP | error_code | 触发条件 |
|---|---|---|
| 400 | `INVALID_REQUEST` | body 既无 `team_display_name` 也无 `description` |
| 404 | `TEAM_NOT_FOUND` | `team_id` 从未创建 |
| 410 | `TEAM_ARCHIVED` | team ns org 处于 archived 状态，禁止修改 |

---

## 接口 5：POST /api/internal/teams/:team_id/members:sync

成员同步入口（**不再创建 team ns**，ns 由接口 1 显式创建）。原子完成：

1. 校验 team ns 存在 + 非 archived
2. 同步成员（`delta` 增量 或 `full_sync` 全量对账）

### 5.1 请求

```http
POST /api/internal/teams/7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a/members:sync HTTP/1.1
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Tenant-Id: <tenant_id>
X-Request-Id: <uuid>
Content-Type: application/json

{
  "mode": "delta",
  "add_members": [
    { "user_id": "u-alice-uuid" },
    { "employee_number": "E-1001" }
  ],
  "remove_members": [
    { "employee_number": "E-1002" }
  ]
}
```

### 5.2 字段

| 字段 | 必填 | 类型 | 说明 |
|---|---|---|---|
| `mode` | ✓ | enum: `delta` / `full_sync` | `delta`：仅应用 add/remove；`full_sync`：以 `add_members` 为期望全量，diff 出需要 remove 的成员 |
| `add_members` | 否 | array | 加成员列表；可空 |
| `add_members[].user_id` | **二选一** | string | `org-team-service` 内部用户 ID（cs-user `users.subject_id`）|
| `add_members[].employee_number` | **二选一** | string | 企业身份 id —— **工号**。对应 cs-user `employment_identities.employee_number` 与 JWT 中 `employee_number` claim（[`EnterpriseClaims`](../../cs-user/internal/auth/claims.go)）同字段名 |
| `remove_members` | 否 | array | 同 schema；`full_sync` 模式下可留空 |

> **UserRef（用户引用模型，全文档统一）**：本文档所有"用户声明"——包括本接口的 `add_members` / `remove_members`、[接口 1](#接口-1post-apiinternalteams) 的 `creator` / `initial_members`、[接口 6](#接口-6post-apiinternalteamsteam_iddissolve) / [接口 7](#接口-7post-apiinternalteamsteam_idbot-tokenrotate) 的 `actor`——均遵循同一 schema：`user_id` 或 `employee_number` **二选一**，**不接受 `gitea_username`**。Gitea 平台细节不泄漏到业务层。@server 收到后：
>
> - `user_id` 路径 → 调 cs-user `GET /api/internal/users/:subject_id` 拿 `users.gitea_username`
> - `employee_number` 路径 → 调 cs-user `GET /api/internal/users/search?employee_number=...`（新增查询键）→ 在 `employment_identities` 表内按 `(tenant_id, employee_number)` 反查 `user_subject_id` → 再解析 `gitea_username`
>
> 必要时触发 cs-user 侧 giteasync 懒创建用户，再调 Gitea API 完成 org 成员增删。**调用方传 `gitea_username` 字段会被忽略**（避免上游对 Gitea 内部命名产生耦合）。
>
> **二选一约束**：同一条引用同时传 `user_id` 和 `employee_number` → 400 `INVALID_REQUEST`；两个都不传 → 400 `INVALID_REQUEST`。
>
> **唯一性约束**：`employment_identities(tenant_id, employee_number)` 唯一索引待 Phase B 落地（见 `employment_identity.go:13-17` 注释）；当前期间若同一 tenant 内同一工号命中多行（极少见 —— 通常源于 provider 切换的过渡期），@server 返回该成员引用的 `reason="ambiguous"` 走 `members_unresolved`，由 ops 人工裁决，不擅自选定。

> **bot 账号不在同步范围内** — `members:sync` 永远不动 `bot-t-<team_short>`；bot 只随 team 创建而创建、随 dissolve 而撤销。`full_sync` diff 时也跳过 bot username 前缀。

### 5.3 响应

```json
{
  "team_ns_org": "t-7f3c9a1e",
  "members_added_count": 2,
  "members_removed_count": 1,
  "members_unresolved": [
    { "employee_number": "E-1002", "reason": "not_found" }
  ],
  "audit_log_id": "audit-4b5c6d7e-..."
}
```

| 字段 | 必返回 | 说明 |
|---|---|---|
| `members_added_count` / `members_removed_count` | ✓ | 实际生效的成员变更数（已解析 + Gitea API 成功的）|
| `members_unresolved` | ✓ | 解析失败的成员引用列表（`reason` ∈ `not_found` / `giteasync_pending` / `ambiguous`）；空数组表示全部成功。**默认不因 unresolved 报错** —— 调用方按需重试或人工跟进；只有当**所有**成员都 unresolved 时才返回 404 `MEMBER_USER_NOT_FOUND` |

### 5.4 行为分支

| 场景 | server 行为 |
|---|---|
| team ns 存在 + `delta` | 处理 add/remove，不动 org 配置 |
| team ns 存在 + `full_sync` | ① 拉取当前 org 成员（排除 `bot-t-*`）② diff 出不在 `add_members` 中的全员 remove ③ 应用 add |
| team ns 不存在 | 412 `TEAM_NS_NOT_INITIALIZED`（提示先调 `POST /teams`）|
| team ns 已 archived | 410 `TEAM_ARCHIVED` |

### 5.5 专属错误码

| HTTP | error_code | 触发条件 |
|---|---|---|
| 400 | `INVALID_REQUEST` | `team_id` 非 UUID / `mode` 非法 / add+remove 有重叠 / 某条成员引用同时传 `user_id` 和 `employee_number` 或两者都缺 |
| 404 | `MEMBER_USER_NOT_FOUND` | **所有**成员引用都解析失败（部分失败走响应体 `members_unresolved`，不报错）|
| 410 | `TEAM_ARCHIVED` | team ns org 处于 archived 状态 |
| 412 | `TEAM_NS_NOT_INITIALIZED` | team ns 未通过 `POST /teams` 创建 |

---

## 接口 6：POST /api/internal/teams/:team_id/dissolve

team 解散时由 `org-team-service` 推送 `team:dissolved` webhook，转调本接口。原子完成：

1. **archive team ns org**（保留审计窗口，不立即删除）
2. **移除所有成员**（保留 repo 内容）
3. **撤销 bot token**（Gitea API 删 token + 本地 `revoked_at` 置位；`team_bot_credentials` 行保留供审计）
4. 写入解散元数据

### 6.1 请求

```http
POST /api/internal/teams/7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a/dissolve HTTP/1.1
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Tenant-Id: <tenant_id>
X-Request-Id: <uuid>
Content-Type: application/json

{
  "reason": "team merged into another",
  "actor": { "employee_number": "E-1000" }
}
```

### 6.2 字段

| 字段 | 必填 | 类型 | 说明 |
|---|---|---|---|
| `reason` | ✓ | string | 解散原因（审计可见）|
| `actor` | ✓ | UserRef | 触发解散的操作者（审计可见）；UserRef schema 见 [接口 5.2](#52-字段) |

### 6.3 响应

```json
{
  "team_ns_org": "t-7f3c9a1e",
  "archived": true,
  "members_removed_count": 12,
  "bot_token_revoked": true,
  "retention_until": "2026-10-13T00:00:00Z",
  "audit_log_id": "audit-9c8d7e6f-..."
}
```

| 字段 | 必返回 | 说明 |
|---|---|---|
| `archived` | ✓ | 本次是否执行了 archive（已 archived 时幂等返回 true，no-op）|
| `members_removed_count` | ✓ | 实际移除的成员数 |
| `bot_token_revoked` | ✓ | 本次是否执行了 token 撤销；幂等重入为 false |
| `retention_until` | ✓ | 保留期截止时间（UTC ISO 8601）；默认 90 天，过期后由 ops runbook 决定物理删除 |

### 6.4 行为分支

| 场景 | server 行为 | `archived` | `bot_token_revoked` |
|---|---|---|---|
| team ns 存在 | ① archive org ② 全员 remove ③ 撤销 bot token ④ 写审计 | true | true |
| team ns 不存在（从未创建过）| 视为已解散，no-op；记审计 | false | false |
| team ns 已 archived（重复 dissolve）| 幂等成功，不二次操作 | true | false |

### 6.5 专属错误码

| HTTP | error_code | 触发条件 |
|---|---|---|
| 400 | `INVALID_REQUEST` | `team_id` 非 UUID / `reason` 缺失 / `actor` 不是合法 UserRef |
| 404 | `TEAM_NOT_FOUND` | `team_id` 在 `org-team-service` 不存在 |

---

## 接口 7：POST /api/internal/teams/:team_id/bot-token:rotate

轮换 bot token。旧 token 在本接口返回成功后**立即失效**；上游业务服务必须先拿到新 token 再丢弃旧 token 缓存。

### 7.1 请求

```http
POST /api/internal/teams/7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a/bot-token:rotate HTTP/1.1
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Request-Id: <uuid>
Content-Type: application/json

{
  "reason": "suspected leak via CI logs",
  "actor": { "employee_number": "E-1000" }
}
```

### 7.2 字段

| 字段 | 必填 | 类型 | 说明 |
|---|---|---|---|
| `reason` | ✓ | string | 轮换原因（审计可见）|
| `actor` | ✓ | UserRef | 触发轮换的操作者（审计可见）；UserRef schema 见 [接口 5.2](#52-字段) |

### 7.3 响应

```json
{
  "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
  "bot": {
    "gitea_username": "bot-t-7f3c9a1e",
    "gitea_user_id": 42,
    "token_id": 18,
    "token": "cs-bot-1a2b3c4d5e6f...",
    "token_sha256": "8b1a9953c4611296a827abf8c47804d7285f97e9a13e5c4c1f4b3d8e0c9a7b6f"
  },
  "previous_token_revoked": true,
  "audit_log_id": "audit-9d8e7f6a-..."
}
```

### 7.4 行为分支

| 场景 | server 行为 |
|---|---|
| team 存在 + active | ① Gitea API 删旧 token ② Gitea API 建新 token ③ 更新本地 `token_encrypted` / `token_sha256` / `gitea_token_id` / `rotated_at` ④ 返回新明文 token 一次 |
| team 已 archived | 410 `TEAM_ARCHIVED`（archived 后不允许 rotate；如需复活走专门 runbook）|
| team_id 从未创建 | 404 `TEAM_NOT_FOUND` |

### 7.5 专属错误码

| HTTP | error_code | 触发条件 |
|---|---|---|
| 400 | `INVALID_REQUEST` | `reason` 缺失 / `actor` 不是合法 UserRef |
| 404 | `TEAM_NOT_FOUND` | `team_id` 从未创建 |
| 410 | `TEAM_ARCHIVED` | team ns org 处于 archived 状态 |

---

## 接口 8：POST /api/internal/workflow/init

workflow 类型 repo + 实例 branch 的**唯一入口**。原子完成：

1. **前置校验 team ns 存在**（不存在 → 412）
2. 计算 `wf_repo_path` + `instance_branch`（按 [`WORKFLOW_REPO_PATH_ALGORITHM.md`](./WORKFLOW_REPO_PATH_ALGORITHM.md) v2.0 §A / §B）
3. **get-or-create 类型 repo**（首次 init 该 def 时创建 + `definition_snapshot` 写入 main + 配置 main + `inst-*` 通配 branch protection）
4. **def drift 校验**（类型 repo 已存在时，从 main HEAD 读 def，与传入 `definition_snapshot` 对比，不一致返回 409）
5. **get-or-create 实例 branch**（base = main HEAD）
6. **附带 bot 凭据返回**（上游业务服务用 bot 身份 clone/push 该实例 branch）

### 8.1 请求

```http
POST /api/internal/workflow/init HTTP/1.1
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Tenant-Id: <tenant_id>
X-Request-Id: <uuid>
Content-Type: application/json

{
  "workflow_def_slug": "bug-fix-flow",
  "instance_id": "f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab",
  "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
  "definition_snapshot": "<yaml 内容：节点定义 / DAG / audit_level 配置>"
}
```

### 8.2 字段

| 字段 | 必填 | 类型 | 说明 |
|---|---|---|---|
| `workflow_def_slug` | ✓ | string | 团队自定义的 def 标识符；转义规则见算法 spec §3.A.5 |
| `instance_id` | ✓ | UUID | workflow 编排器分配；推导 `inst-<short>` 见算法 spec §B |
| `team_id` | ✓ | UUID | 必须先经 `POST /teams` 创建 team ns + bot |
| `definition_snapshot` | 类型 repo 首次创建时必填 | string | workflow def 的 yaml 内容；类型 repo 已存在时仅用于 drift 校验 |

> **定义来源**：v2.17 后，定义的 canonical 存储是 `wf-<def>` repo 的 `main` 分支。`definition_snapshot` 字段仅在**类型 repo 首次创建**时使用——把 def 写入 main；后续 init 应从 main HEAD 读取定义，不应每次重传。`definition_snapshot` 与 main 现存版本不一致时返回 409 `DEFINITION_DRIFT`。

### 8.3 响应

```json
{
  "wf_repo_path": "t-7f3c9a1e/wf-bug-fix-flow",
  "wf_clone_url": "https://gitea.costrict.local/t-7f3c9a1e/wf-bug-fix-flow.git",
  "wf_web_url": "https://gitea.costrict.local/t-7f3c9a1e/wf-bug-fix-flow",
  "instance_branch": "inst-f3a8b2c1",
  "created": {
    "type_repo": false,
    "instance_branch": true
  },
  "team_ns_exists": true,
  "algorithm_version": "v2",
  "bot_credentials": {
    "gitea_username": "bot-t-7f3c9a1e",
    "gitea_user_id": 42,
    "token": "cs-bot-1a2b3c4d5e6f...",
    "clone_url_with_token": "https://bot-t-7f3c9a1e:cs-bot-1a2b3c4d5e6f...@gitea.costrict.local/t-7f3c9a1e/wf-bug-fix-flow.git"
  }
}
```

| 字段 | 必返回 | 说明 |
|---|---|---|
| `wf_repo_path` | ✓ | team ns 内的相对路径（`<org>/<repo>`），用于审计 / 日志 |
| `wf_clone_url` | ✓ | 不含凭据的 clone URL，仅用于人类展示 / 日志 |
| `wf_web_url` | ✓ | 浏览器访问入口（不带 `.git` 后缀）；用于 portal UI 链接 |
| `instance_branch` | ✓ | 本次实例的工作 branch 名（`inst-<short>`）|
| `created.type_repo` | ✓ | 是否新建了类型 repo（首次 init 该 def 为 true）|
| `created.instance_branch` | ✓ | 是否新建了实例 branch（幂等重入为 false）|
| `team_ns_exists` | ✓ | 通常为 true（false 时直接 412 不进入本响应）|
| `algorithm_version` | ✓ | 路径算法版本，与 `WORKFLOW_REPO_PATH_ALGORITHM.md` 同步 |
| `bot_credentials.gitea_username` / `gitea_user_id` | ✓ | bot 账号元信息；每次 init 都返回 |
| `bot_credentials.token` | ✓ | bot 明文 token；上游业务服务用此 token 包装 git https basic auth 推/拉 wf repo |
| `bot_credentials.clone_url_with_token` | ✓ | 已嵌入凭据的 clone URL；上游可直接 `git clone <url>` 使用。**仅用于内存透传给编排器，禁止写日志** |

> **token 复发说明**：`bot_credentials.token` 每次调用都返回**同一个**当前活跃 token（与接口 1 / 7 不同 —— 这俩只在创建/轮换时返回一次）。原因：workflow init 的上游是短生命周期的编排器进程，无持久化密钥存储；每次 init 拿一次是更安全的折中。如果上游需要长期缓存 token，应自行调 `GET /teams/:team_id` 取 `token_sha256` 比对。
>
> **archived team 的 token 行为**：team 进入 archived 状态后，token 已撤销，本接口会因 412 `TEAM_NS_NOT_INITIALIZED`（team ns 不存在）或 Gitea API 鉴权失败而不可用。

### 8.4 行为分支

| 场景 | server 行为 | `created.type_repo` | `created.instance_branch` | `bot_credentials` |
|---|---|---|---|---|
| team ns 不存在 | 412 `TEAM_NS_NOT_INITIALIZED` + hint | — | — | — |
| 类型 repo 不存在（首次 init 该 def）| ① 建类型 repo ② `definition_snapshot` 写入 main ③ 配置 main + `inst-*` 通配 branch protection ④ 从 main 创建 `inst-<short>` branch ⑤ 返回 bot 凭据 | true | true | ✓ |
| 类型 repo 已存在 + def 一致 + branch 不存在 | 从 main HEAD 创建 `inst-<short>` branch + 返回 bot 凭据 | false | true | ✓ |
| 类型 repo 已存在 + def 一致 + branch 已存在 | 幂等 no-op + 返回 bot 凭据 | false | false | ✓ |
| 类型 repo 已存在 + def 不一致 | 409 `DEFINITION_DRIFT` | — | — | — |

### 8.5 专属错误码

| HTTP | error_code | 触发条件 |
|---|---|---|
| 400 | `INVALID_REQUEST` | `instance_id` / `team_id` 非 UUID / `workflow_def_slug` 含非法字符 |
| 409 | `DEFINITION_DRIFT` | 类型 repo main HEAD 上的 def 与传入 `definition_snapshot` 不一致 |
| 412 | `TEAM_NS_NOT_INITIALIZED` | team ns 未通过 `POST /teams` 创建 |

---

## 接口 9：POST /api/internal/kb/ensure

kb repo 的**唯一入口**。行为与接口 8 (`workflow/init`) 对齐——同样的"前置校验 → 算法算路径 → get-or-create repo + branch protection → 附 bot 凭据返回"骨架，差异只在 kb 没有"实例 branch / def drift"这两层。

原子完成：

1. **前置校验 team ns 存在**（不存在 → 412）
2. 计算 `kb_repo_path`（按 [`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) v2.0）
3. **get-or-create kb repo**（首次 ensure 该 `(code_repo_url, team_id)` 时创建 private repo + 配置 main branch protection）
4. **附带 bot 凭据返回**（上游 csc / 编排器用 bot 身份 push / pull kb repo）

> **与 workflow/init 的语义差异**：workflow repo 的 `main` 存 server-side canonical 内容（`definition.yaml`），首创建时由 server 写入，后续 init 校验 drift；kb repo 的 `main` 完全由**用户侧 `csc kb push`** 写入，server 不写任何 canonical 文件，因此**没有 drift 校验**——只要 repo 存在即视为已就绪，幂等返回 `created=false`。
>
> **无实例 branch**：workflow 有 `(team, def) → 类型 repo` + `(instance) → inst-* branch` 两层；kb 只有 `(code_repo_url, team) → kb repo` 一层，所有内容直接落 `main`。

### 9.1 请求

```http
POST /api/internal/kb/ensure HTTP/1.1
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Tenant-Id: <tenant_id>
X-Request-Id: <uuid>
Content-Type: application/json

{
  "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
  "code_repo_url": "https://github.com/ownerA/proj.git"
}
```

### 9.2 字段

| 字段 | 必填 | 类型 | 说明 |
|---|---|---|---|
| `team_id` | ✓ | UUID | 必须先经 `POST /teams` 创建 team ns + bot |
| `code_repo_url` | ✓ | string | 必须 http(s):// 起头；ssh / git scheme 由 csc 端归一为 https（见算法 spec §3.6）；server 不做归一化只校验 scheme |

> **URL 归一化责任划分**：csc 端负责把 `git@github.com:o/p.git` / `git://github.com/o/p` 归一为 `https://github.com/o/p.git` 再发请求；server 只接受 http(s):// 起头，其它 scheme 直接 400。这与算法 spec §3.6 + [`CSC_KB_SUBCOMMAND_CONTRACT.md`](./CSC_KB_SUBCOMMAND_CONTRACT.md) §2.3 一致，避免双端规则漂移。

### 9.3 响应

```json
{
  "kb_repo_path": "t-7f3c9a1e/kb-github.com__ownera__proj",
  "kb_clone_url": "https://gitea.costrict.local/t-7f3c9a1e/kb-github.com__ownera__proj.git",
  "kb_web_url": "https://gitea.costrict.local/t-7f3c9a1e/kb-github.com__ownera__proj",
  "created": {
    "kb_repo": true
  },
  "team_ns_exists": true,
  "algorithm_version": "v2",
  "bot_credentials": {
    "gitea_username": "bot-t-7f3c9a1e",
    "gitea_user_id": 42,
    "token": "cs-bot-1a2b3c4d5e6f...",
    "clone_url_with_token": "https://bot-t-7f3c9a1e:cs-bot-1a2b3c4d5e6f...@gitea.costrict.local/t-7f3c9a1e/kb-github.com__ownera__proj.git"
  }
}
```

| 字段 | 必返回 | 说明 |
|---|---|---|
| `kb_repo_path` | ✓ | team ns 内的相对路径（`<org>/<repo>`），用于审计 / 日志 |
| `kb_clone_url` | ✓ | 不含凭据的 clone URL，仅用于人类展示 / 日志 |
| `kb_web_url` | ✓ | 浏览器访问入口（不带 `.git` 后缀）；用于 portal UI 链接 |
| `created.kb_repo` | ✓ | 是否新建了 kb repo（首次 ensure 该 `(code_repo_url, team_id)` 为 true；幂等重入为 false）|
| `team_ns_exists` | ✓ | 通常为 true（false 时直接 412 不进入本响应）|
| `algorithm_version` | ✓ | 路径算法版本，与 `KB_REPO_PATH_ALGORITHM.md` 同步 |
| `bot_credentials.gitea_username` / `gitea_user_id` | ✓ | bot 账号元信息；每次 ensure 都返回 |
| `bot_credentials.token` | ✓ | bot 明文 token；上游 csc / 编排器用此 token 包装 git https basic auth 推/拉 kb repo |
| `bot_credentials.clone_url_with_token` | ✓ | 已嵌入凭据的 clone URL；上游可直接 `git clone <url>` 使用。**仅用于内存透传，禁止写日志** |

> **token 复发说明**：与接口 8 一致——`bot_credentials.token` 每次调用都返回**同一个**当前活跃 token。原因相同：kb ensure 的上游是短生命周期的 csc 进程或编排器，无持久化密钥存储；每次 ensure 拿一次是更安全的折中。如需长期缓存，应自行调 `GET /teams/:team_id` 取 `token_sha256` 比对。
>
> **archived team 的 token 行为**：team 进入 archived 状态后，token 已撤销，本接口会因 412 `TEAM_NS_NOT_INITIALIZED`（team ns 不存在）或 Gitea API 鉴权失败而不可用。

### 9.4 行为分支

| 场景 | server 行为 | `created.kb_repo` | `bot_credentials` |
|---|---|---|---|
| team ns 不存在 | 412 `TEAM_NS_NOT_INITIALIZED` + hint | — | — |
| kb repo 不存在（首次 ensure 该 `(code_repo_url, team_id)`）| ① 建 kb repo（private）② 配置 main branch protection ③ 返回 bot 凭据 | true | ✓ |
| kb repo 已存在 | 幂等 no-op（不读 / 校验任何文件内容）+ 返回 bot 凭据 | false | ✓ |
| team 已 archived | 与"team ns 不存在"等价：412 `TEAM_NS_NOT_INITIALIZED` | — | — |

> **不校验内容**：与 workflow 的 409 `DEFINITION_DRIFT` 分支不同，kb ensure 不读 main HEAD 任何文件——kb 的内容由用户侧 `csc kb push` 全权管理，server 视其为黑盒。

### 9.5 专属错误码

| HTTP | error_code | 触发条件 |
|---|---|---|
| 400 | `INVALID_REQUEST` | `team_id` 非 UUID / `code_repo_url` 缺 scheme / 非 http(s) scheme / 裸 host 无 path |
| 412 | `TEAM_NS_NOT_INITIALIZED` | team ns 未通过 `POST /teams` 创建 |

> **无 409 / 502 特化错误码**：kb ensure 没有 drift 分支（无 409）；上游 Gitea 5xx / 网络故障统一回落到通用 502（见 §通用错误码），不暴露后端拓扑。

---

## 触发链路

接口实际由谁调用？三种典型路径（详见 [`TEAM_ORG_UNIFICATION.md`](../identity-tenant/TEAM_ORG_UNIFICATION.md)）：

```
1. org-team-service webhook → @server
   team.created             → POST /teams                     （创建 ns + bot）
   team.updated             → PATCH /teams/:id                （镜像 display_name）
   team.members.changed     → members:sync (mode=delta)
   team.members.reconcile   → members:sync (mode=full_sync)
   team.dissolved           → dissolve                        （archive + 撤销 bot）

2. workflow 编排器 → @server
   workflow instance start  → workflow/init                   （编排器收 bot_credentials）

3. csc 客户端 → 编排器 → @server（代调）
   csc workflow start       → workflow/init（编排器代调，不直接到 @server）
   csc kb push / pull       → kb/ensure（编排器代调，收 bot_credentials 后 git push/pull）

4. 平台管理员（应急）→ @server admin 入口
   bot 怀疑泄露            → POST /teams/:id/bot-token:rotate （手动轮换）
   排查 / 重放成员同步     → POST /api/admin/tenants/:tenant_id/teams/:team_id/sync
```

**粒度约定**：单成员事件（`member.added` / `member.removed`）由外部模块按需推送，**OrgService 消费做缓存失效**；批量同步事件（`team.members.changed` / `team.members.reconcile`）由外部模块在 batch 提交 / 全量对账时推送，**@server 消费做 Gitea 侧同步**。**@server 不监听 `member.*` 事件**，避免与 bulk 事件重复触发 Gitea API。

---

## 已实现的 admin 入口（实现状态参考）

@server 已落地一个**手动触发**的 admin 同步入口，仅供平台管理员排查 / 重放时使用，**不替代** webhook 路径：

| 方法 + 路径 | 鉴权 | 用途 |
|---|---|---|
| `POST /api/admin/tenants/:tenant_id/teams/:team_id/sync` | `RequirePlatformAdmin` | 手动触发 team ns Gitea org + 成员同步；返回 `SyncResult`；不接 `mode` 参数，固定 `full_sync` 语义 |

`/api/internal/*` 九接口尚未实现；本文档为前置接口契约，落地实现时以此为权威 schema。

---

## 变更历史

| 版本 | 日期 | 变更 |
|---|---|---|
| v1.0 | 2026-07-21 | 初稿；3 接口（members:sync / dissolve / workflow init），lazy 创建模型 |
| v1.1 | 2026-07-21 | 引入 team 显式 CRUD（接口 1-4）+ bot 账号模型（命名约定 / 存储表 / token 生命周期）+ bot-token:rotate（接口 7）；workflow init 响应附 `bot_credentials`；members:sync 不再承担 ns 创建职责（改由接口 1）；**`X-Tenant-Id` 统一改为可选** —— 全部接口缺省时落到 `default` 租户；**UserRef 全局统一** —— 所有用户声明（`creator` / `initial_members` / `add_members` / `remove_members` / `actor`）均改为 `user_id` 或 `employee_number` 二选一，**不接受 `gitea_username`**（与 JWT `employee_number` claim 对齐；Gitea 平台细节不泄漏到业务层） |
| v1.2 | 2026-07-22 | 新增接口 9 `POST /api/internal/kb/ensure` —— kb repo get-or-create 入口；行为骨架对齐接口 8（前置 team ns 校验 → 算法算路径 → get-or-create repo + main branch protection → 附 `bot_credentials` 返回），差异：① 无实例 branch 概念（kb 只有一层 `(code_repo_url, team) → kb repo`）；② 无 drift 校验（kb repo `main` 内容由用户侧 `csc kb push` 全权管理，server 不写 canonical 文件）；③ 错误码收敛到 400 / 412 两类，无 409 分支。算法实现按 [`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) v2.0；触发链路补 csc kb push/pull 走编排器代调路径 |
