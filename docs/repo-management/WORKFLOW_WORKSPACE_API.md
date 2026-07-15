# Team API（@server 内部接口）

| 字段 | 内容 |
|---|---|
| 状态 | Draft · 评审中 |
| 作者 | CoStrict 团队 |
| 创建日期 | 2026-07-15 |
| 评审范围 | @server / workflow 编排器 / workspace 服务 / Gitea |
| 暴露范围 | **仅内网**——网关（gateway / api-gateway）不对 `/api/internal/*` 路径放行；只允许同 VPC / 服务网格内的可信服务通过 service token 调用 |
| 关联文档 | [`CSC_WF_SUBCOMMAND_CONTRACT.md`](./CSC_WF_SUBCOMMAND_CONTRACT.md)（csc 客户端契约）、[`WORKFLOW_REPO_PATH_ALGORITHM.md`](./WORKFLOW_REPO_PATH_ALGORITHM.md)（repo path 算法）、[`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) v2.16 §17、[`TEAM_ORG_UNIFICATION.md`](../identity-tenant/TEAM_ORG_UNIFICATION.md) ADR-3 v3 |

> 本文档定义 @server 暴露给 **workflow 编排器** 与 **workspace 服务** 的 2 个**内部** HTTP 接口（路径前缀 `/api/internal/`），用于 **team**（workflow 业务侧的团队概念，每个 team 对应一个 Gitea team，授权该 team 下所有 workflow 实例 repo）成员与 workflow 实例 repo 权限的联动管理。所有 Gitea API 调用由 @server 持 admin token 执行，调用方不感知 Gitea 细节。**接口不做用户鉴权，仅做服务鉴权。**
>
> **命名说明**：本文中「team」统一指 workflow 业务侧的团队（由 workspace 服务管理其生命周期与成员），与 @server 自身的 `workspace` 概念（业务工作区 / 工作空间）不同；workspace 服务是 team 的外部所有者与权威数据源。

---

## 1. 背景与设计原则

### 1.1 team 模型

- 每个 **team** ≈ 一个工作流团队，存在成员进出逻辑（由 workspace 服务管理）
- 每个 team 下可启动**多个 workflow 实例**
- 每个 workflow 实例对应一个独立的 Gitea repo（路径由 [WORKFLOW_REPO_PATH_ALGORITHM.md](./WORKFLOW_REPO_PATH_ALGORITHM.md) 计算）

### 1.2 team 粒度决策

**每个 team 对应一个 Gitea team**（1:1 映射）：

- Gitea team 授权该 team 下**所有** workflow 实例 repo
- team 成员变更只需改**一个** Gitea team（自动生效到所有实例）
- 实例间权限不隔离（同 team 内所有成员可见所有实例）

> 若未来需要实例级权限隔离，可扩展为 team + instance team 双层；当前阶段不引入。

### 1.3 接口清单

| # | 接口 | 调用方 | 时机 | 职责 |
|---|---|---|---|---|
| 1 | `POST /api/internal/workflow/init` | workflow 编排器 | workflow 实例启动 | 创建实例 repo + 创建/复用 team 对应的 Gitea team + 授权 |
| 2 | `POST /api/internal/teams/:team_id/members:sync` | workspace 服务 | team 成员变更 | 增量或全量同步 Gitea team 成员 |

### 1.4 服务鉴权与租户识别

> **本接口不做用户鉴权**——没有 JWT、没有用户身份校验。仅做**服务鉴权**：调用方（workspace 服务 / workflow 编排器）使用与 @server 双方预先约定的 service token 发起请求。

- HTTP Header：
  - `X-Internal-Service-Token: <service_token>`（必填，**双方预先约定的共享密钥**；建议通过 mTLS + sidecar secret 注入，不走配置明文）
  - `X-Tenant-Id: <tenant_id>`（**可选**；未传时 @server 使用配置项 `DEFAULT_TENANT_ID` 作为默认租户）
  - `X-Request-Id: <uuid>`（推荐，用于幂等 + 链路追踪）
- **不做跨租户越权保护**：调用方为可信内部服务，自行保证传入的 `tenant_id` / `team_id` 合法。@server 不反查 team 归属 tenant，不做 JWT/tenant 比对
- **token 校验**：@server 启动时从 secret manager 拉取约定的 service token，每个请求对比 `X-Internal-Service-Token`，不一致返回 `401 UNAUTHORIZED_SERVICE`
- @server 持 Gitea admin token 执行实际 Gitea API 调用；调用方 PAT 不参与

---

## 2. 接口 1：POST /api/internal/workflow/init

### 2.1 用途

workflow 实例启动时由编排器调用。原子地完成：

1. 计算 `wf_repo_path`（按 WORKFLOW_REPO_PATH_ALGORITHM.md）= `<gitea_host>/<org>/<def_slug>__<instance_short_id>.git`，返回给调用方时附带完整地址（含 scheme + host）
2. 在 Gitea **创建空的 workflow 实例 repo**（仅创建容器，不写入任何内容；后续 workflow 内容由 `csc wf node push` 等流程填充）
3. **get-or-create team 对应的 Gitea team**（同 team 第一个实例 init 时创建 Gitea team；后续 init 复用）
4. 把该 Gitea team 关联到当前实例 repo（write 权限）
5. **首次创建** Gitea team 时，把 `initial_members` 加入 team；后续 init 忽略 `initial_members`（成员变更走接口 2）

### 2.2 请求

```http
POST /api/internal/workflow/init
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Tenant-Id: <tenant_id>（可选，缺省走默认租户）
Content-Type: application/json
X-Request-Id: <uuid>（推荐，用于幂等）

{
  "workflow_def_slug": "bug-fix-flow",
  "instance_id": "f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab",
  "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
  "initial_members": [
    {"user_id": "u-alice-uuid", "role": "owner"},
    {"user_id": "u-bob-uuid",   "role": "write"},
    {"enterprise_uid": "EMP-CAROL-001", "role": "read"}
  ],
  "force": false
}
```

#### 2.2.1 字段说明

请求 header：

| Header | 必填 | 说明 |
|---|---|---|
| `X-Internal-Service-Token` | ✓ | 双方预先约定的 service token；@server 对比失败返回 `401 UNAUTHORIZED_SERVICE` |
| `X-Tenant-Id` |  | 租户 ID；未传时使用 `DEFAULT_TENANT_ID` 配置项作为默认租户 |
| `X-Request-Id` |  | 推荐传，用于幂等 + 链路追踪 |

请求 body：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `workflow_def_slug` | string (`[a-z0-9-]+`) | ✓ | workflow 定义 slug |
| `instance_id` | string (UUID v4/v7) | ✓ | 编排器分配的实例 ID |
| `team_id` | string (UUID) | ✓ | 所属 team ID（@server 不反查 tenant 归属，由调用方自行保证合法） |
| `initial_members[].user_id` | string (UUID) | ✓† | cs-user 颁发的平台 user_id（与 `enterprise_uid` 二选一）；对齐 [USER_CENTER_DESIGN.md](../identity-tenant/USER_CENTER_DESIGN.md) §6 base 层 |
| `initial_members[].enterprise_uid` | string | ✓† | 用户企业标识（`enterprise_identities.enterprise_uid`，跨登录源稳定）；当 workspace 服务只有企业工号、未拿到 user_id 时使用，对齐 [MULTI_TENANCY_DESIGN.md](../identity-tenant/MULTI_TENANCY_DESIGN.md) §6.5.1 |
| `initial_members[].role` | enum | ✓* | `owner`（仅 1 个）/ `write` / `read` |
| `force` | bool |  | 跳过交互式确认（默认 false） |

> `†` 每个 `initial_members[]` 项必须**恰好二选一**传 `user_id` 或 `enterprise_uid`；同时传或都不传返回 `400 INVALID_REQUEST`。
>
> `*` `initial_members` 仅在**首次创建 team 对应的 Gitea team** 时使用；后续 init 调用可省略此字段，传了也会被忽略。
>
> **本接口不接收 workflow 定义/审计配置等内容字段**；repo 创建后保持空容器，workflow 内容（定义快照、节点产物等）由后续 `csc wf node push` 等流程填充。
>
> **cs-user 职责边界**：本接口调用链路中，cs-user **既做 user_id / enterprise_uid 合法性校验，也做 → `gitea_username` 的解析**（通过 `user_gitea_binding` 表 + `enterprise_identities` 表）；不校验 team 归属。调用方无需感知 Gitea 账号体系。

### 2.3 响应

#### 2.3.1 成功（首次创建 Gitea team + 实例 repo）— HTTP 200

```json
{
  "created": {
    "repo": true,
    "team": true
  },
  "wf_repo_path": "https://gitea.costrict.local/costrict-workflow/bug-fix-flow__f3a8b2c1.git",
  "team": {
    "name": "t-7f3c9a1e",
    "id": 42,
    "is_new": true,
    "members_applied": 3,
    "members_skipped": []
  },
  "role": "owner",
  "algorithm_version": "v1"
}
```

#### 2.3.2 成功（Gitea team 已存在，仅创建实例 repo 并授权）— HTTP 200

```json
{
  "created": {
    "repo": true,
    "team": false
  },
  "wf_repo_path": "https://gitea.costrict.local/costrict-workflow/bug-fix-flow__f3a8b2c1.git",
  "team": {
    "name": "t-7f3c9a1e",
    "id": 42,
    "is_new": false,
    "members_applied": 0,
    "members_skipped": [],
    "note": "team already exists; initial_members ignored; authorize new repo to existing team"
  },
  "role": "owner",
  "algorithm_version": "v1"
}
```

#### 2.3.3 幂等重入（同 instance_id 重复 init）— HTTP 200

```json
{
  "created": {"repo": false, "team": false},
  "wf_repo_path": "https://gitea.costrict.local/costrict-workflow/bug-fix-flow__f3a8b2c1.git",
  "team": {
    "name": "t-7f3c9a1e",
    "id": 42,
    "is_new": false,
    "members_applied": 0,
    "members_skipped": []
  },
  "role": "owner",
  "algorithm_version": "v1"
}
```

> 幂等规则：相同 `(workflow_def_slug, instance_id)` 二次调用视为成功（no-op），不重复创建 repo / 不重复授权 team。

### 2.4 错误码

| HTTP | error_code | 说明 | 建议 |
|---|---|---|---|
| 400 | `INVALID_REQUEST` | 必填字段缺失 / UUID 格式错误 / role 取值非法 / owner 数量 ≠ 1（首次创建 team 时）/ `user_id` 与 `enterprise_uid` 同时传或都不传 | 修正请求 |
| 401 | `UNAUTHORIZED_SERVICE` | `X-Internal-Service-Token` 缺失或与约定值不一致 | 检查 service token 配置 |
| 409 | `REPO_EXISTS_WITH_DIFFERENT_OWNER` | 同一 instance_id 对应 repo 已存在但不属于当前 team | 新建 instance_id 重启或联系 admin |
| 422 | `USER_ID_INVALID` | `user_id` 在 cs-user 中不存在 / 已禁用 | 调用方修正 user_id 或先在 cs-user 完成开户 |
| 422 | `ENTERPRISE_UID_NOT_FOUND` | `enterprise_uid` 在 cs-user `enterprise_identities` 中找不到 / 跨 tenant 不匹配 | 调用方核实 enterprise_uid；确认 X-Tenant-Id 是否正确 |
| 422 | `GITEA_USERNAME_UNRESOLVED` | cs-user 解析 user_id / enterprise_uid → gitea_username 失败（用户从未登录 Gitea，无 `user_gitea_binding`） | 让用户先登录 Gitea 触发自动开户（cs-user GiteaUserSyncWorker 会回填 binding） |
| 500 | `GITEA_API_FAILURE` | Gitea API 调用失败（@server 内部已重试 3 次） | 重试；持续失败联系 admin |
| 503 | `CS_USER_UNAVAILABLE` | cs-user SDK 调用失败（无法校验标识 / 解析 gitea_username） | 稍后重试 |

### 2.5 @server 内部流程

```
1. 校验 request schema + X-Internal-Service-Token（与约定值不一致 → 401 UNAUTHORIZED_SERVICE）
2. tenant_id = header.X-Tenant-Id ?? DEFAULT_TENANT_ID（不再做 team 归属反查 / JWT 比对）
3. 对每个 initial_member 调 cs-user SDK ResolveGiteaUsername(identifier, tenant_id)：
   - identifier = user_id  → 查 users + user_gitea_binding
   - identifier = enterprise_uid → 先查 enterprise_identities → user_id，再查 user_gitea_binding
   - 失败码：
     · user_id 不存在/已禁用 → 422 USER_ID_INVALID
     · enterprise_uid 不匹配 → 422 ENTERPRISE_UID_NOT_FOUND
     · 无 user_gitea_binding → 422 GITEA_USERNAME_UNRESOLVED
4. 计算 repo 相对路径 `relative_path = "<org>/<def_slug>__<instance_short_id>"`（WORKFLOW_REPO_PATH_ALGORITHM.md）；同时拼装响应用的完整 URL：`wf_repo_path = "<gitea_host>/<org>/<def_slug>__<instance_short_id>.git"`（含 scheme + host）
5. Gitea API（admin token，URL 中只使用 relative_path，不含 host）：
   a. GET /repos/<relative_path>
      ├─ 不存在 → POST /repos 创建空 repo（默认分支 main + branch protection，不写入任何文件）
      └─ 存在 → 视为幂等成功（不重复创建，不修改内容）
   b. 计算 gitea_team_name（§4.1 规则）
   c. GET /orgs/costrict-workflow/teams/<gitea_team_name>
      ├─ 不存在 → POST /orgs/costrict-workflow/teams 创建
      │           └─ 对每个 initial_member（使用步骤 3 解析出的 gitea_username）：
      │               ├─ PUT /teams/<team_id>/members/<gitea_username>
      │               └─ 设 member role
      └─ 存在 → 跳过成员操作（initial_members 忽略）
   d. PUT /teams/<team_id>/repos/<relative_path>（授权 team 对该 repo 的 write 权限）
6. 返回响应（`wf_repo_path` 字段值为完整 URL，含 scheme + host + `.git` 后缀）
```

---

## 3. 接口 2：POST /api/internal/teams/:team_id/members:sync

### 3.1 用途

team 成员变更时调用（成员加入 / 离开 / 角色变更）。@server 更新对应的 Gitea team 成员。**由于 team 与 Gitea team 1:1**，一次 sync 只改一个 Gitea team，自动生效到该 team 下所有实例 repo。

### 3.2 请求

#### 3.2.1 模式 A：delta 增量

```http
POST /api/internal/teams/7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a/members:sync
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Tenant-Id: <tenant_id>（可选，缺省走默认租户）
Content-Type: application/json
X-Request-Id: <uuid>

{
  "change_type": "delta",
  "changes": [
    {"user_id": "u-charlie-uuid",       "action": "add",         "role": "write"},
    {"user_id": "u-bob-uuid",           "action": "remove"},
    {"enterprise_uid": "EMP-ALICE-001", "action": "role_change", "role": "read"}
  ]
}
```

#### 3.2.2 模式 B：full_sync 全量 reconcile

```http
POST /api/internal/teams/7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a/members:sync
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Tenant-Id: <tenant_id>（可选，缺省走默认租户）
Content-Type: application/json
X-Request-Id: <uuid>

{
  "change_type": "full_sync",
  "expected_members": [
    {"user_id": "u-alice-uuid",         "role": "read"},
    {"user_id": "u-charlie-uuid",       "role": "write"},
    {"enterprise_uid": "EMP-DAVE-042",  "role": "owner"}
  ]
}
```

@server 内部对 `expected_members` 与当前 Gitea team 成员做 diff，自动 add/remove/role_change 到期望态。

#### 3.2.3 字段说明

请求 header：同 §2.2.1（`X-Internal-Service-Token` / `X-Tenant-Id` / `X-Request-Id`）。

请求 body：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `change_type` | enum | ✓ | `delta` / `full_sync` |
| `changes[]` | array | change_type=delta 时必填 | 增量变更项 |
| `changes[].user_id` | string (UUID) | ✓† | 平台 user_id（与 `enterprise_uid` 二选一） |
| `changes[].enterprise_uid` | string | ✓† | 用户企业标识（与 `user_id` 二选一） |
| `changes[].action` | enum | ✓ | `add` / `remove` / `role_change` |
| `changes[].role` | enum | action=add/role_change 必填 | `owner` / `write` / `read` |
| `expected_members[]` | array | change_type=full_sync 时必填 | 期望成员完整列表 |
| `expected_members[].user_id` | string (UUID) | ✓† | 平台 user_id（与 `enterprise_uid` 二选一） |
| `expected_members[].enterprise_uid` | string | ✓† | 用户企业标识（与 `user_id` 二选一） |
| `expected_members[].role` | enum | ✓ | 必须恰好 1 个 owner |

> `†` 每个 member 项必须**恰好二选一**传 `user_id` 或 `enterprise_uid`。
>
> **owner 唯一性约束**：full_sync 模式要求 `expected_members` 恰好含 1 个 owner；delta 模式下若 action=role_change 且 target_role=owner，@server 会先把原 owner 降级为 write 再升级新 owner（事务性保证）。
>
> **cs-user 职责边界**：同 §2.2.1，cs-user 既校验 user_id / enterprise_uid 合法性，也做 → gitea_username 解析；不校验 team 归属。

### 3.3 响应

#### 3.3.1 成功（同步完成）— HTTP 200

```json
{
  "team": {
    "name": "t-7f3c9a1e",
    "id": 42
  },
  "summary": {
    "added": 1,
    "removed": 1,
    "role_changed": 1,
    "skipped": [
      {"user_id": "u-bob-uuid", "reason": "GITEA_USERNAME_UNRESOLVED"}
    ]
  },
  "affected_instances": 12
}
```

> `affected_instances` 表示该 team 当前授权的 workflow 实例 repo 数量（仅供参考，由于 team 共享，所有实例自动生效）。

#### 3.3.2 异步任务受理（成员数 > 100 时）— HTTP 202

```json
{
  "task_id": "task-9e8a7b6c-...",
  "status": "accepted",
  "estimated_duration_seconds": 30
}
```

> 触发条件：`changes.length > 100` 或 `expected_members.length > 100`。可通过 `GET /api/internal/workflow/tasks/:task_id` 查任务状态。

### 3.4 错误码

| HTTP | error_code | 说明 | 建议 |
|---|---|---|---|
| 400 | `INVALID_REQUEST` | changes/expected_members 为空 / action 取值非法 / full_sync 时 owner 数量 ≠ 1 / `user_id` 与 `enterprise_uid` 同时传或都不传 | 修正请求 |
| 401 | `UNAUTHORIZED_SERVICE` | `X-Internal-Service-Token` 缺失或与约定值不一致 | 检查 service token 配置 |
| 404 | `TEAM_NOT_FOUND` | team 对应的 Gitea team 不存在（init 接口尚未调用过） | 先调 init |
| 409 | `OWNER_TRANSFER_CONFLICT` | delta 模式 role_change 目标=owner，但新 owner 标识无法解析到 gitea_username | 让目标用户先登录 Gitea 完成开户 |
| 422 | `USER_ID_INVALID` | `user_id` 在 cs-user 中不存在 / 已禁用 | 调用方修正 user_id 或先在 cs-user 完成开户 |
| 422 | `ENTERPRISE_UID_NOT_FOUND` | `enterprise_uid` 在 cs-user `enterprise_identities` 中找不到 | 调用方核实 enterprise_uid；确认 X-Tenant-Id 是否正确 |
| 422 | `GITEA_USERNAME_UNRESOLVED` | cs-user 解析标识 → gitea_username 失败（用户从未登录 Gitea） | 让用户先登录 Gitea 触发自动开户 |
| 500 | `GITEA_API_FAILURE` | Gitea API 调用失败（@server 内部已重试 3 次） | 重试；持续失败联系 admin |
| 503 | `CS_USER_UNAVAILABLE` | cs-user SDK 调用失败 | 稍后重试 |

### 3.5 @server 内部流程

#### delta 模式

```
1. 校验 request schema + X-Internal-Service-Token（不一致 → 401 UNAUTHORIZED_SERVICE）
2. tenant_id = header.X-Tenant-Id ?? DEFAULT_TENANT_ID
3. 对每个 change 调 cs-user SDK ResolveGiteaUsername(identifier, tenant_id)（identifier = user_id 或 enterprise_uid）：
   - USER_ID_INVALID / ENTERPRISE_UID_NOT_FOUND / GITEA_USERNAME_UNRESOLVED → 加入 skipped
4. GET team 对应的 Gitea team（不存在 → 404 TEAM_NOT_FOUND）
5. 对每个 change（使用步骤 3 解析出的 gitea_username）：
   a. switch action:
      - add → PUT /teams/<id>/members/<gitea_username> + 设 role
      - remove → DELETE /teams/<id>/members/<gitea_username>（Gitea 404 视为 no-op，不进 skipped）
      - role_change → 若 target=owner：先把当前 owner 降级为 write，再升级新 owner（事务性，失败回滚）
                       否则：直接更新 member role
6. 累计 summary 返回
```

#### full_sync 模式

```
1. 校验 request schema + X-Internal-Service-Token + owner 唯一性
2. tenant_id = header.X-Tenant-Id ?? DEFAULT_TENANT_ID
3. 对每个 expected_member 调 cs-user SDK ResolveGiteaUsername(identifier, tenant_id)（identifier = user_id 或 enterprise_uid）：
   - 任一失败 → 422（USER_ID_INVALID / ENTERPRISE_UID_NOT_FOUND / GITEA_USERNAME_UNRESOLVED），整笔拒绝
4. GET team 对应的 Gitea team + GET 当前 team members（Gitea API）
5. 计算 diff（按 gitea_username 匹配）：
   - 期望集合 = expected_members（gitea_username → role，由步骤 3 解析得到）
   - 当前集合 = team members（gitea_username → role）
   - to_add = 期望 - 当前
   - to_remove = 当前 - 期望
   - to_role_change = 交集但 role 不同
6. 复用 delta 模式步骤 5 执行变更
7. 返回 summary（含 added / removed / role_changed / skipped）
```

---

## 4. 命名规则与 Gitea namespace 布局

### 4.1 命名规则

| 实体 | 命名规则 | 示例 |
|---|---|---|
| workflow 实例 repo（相对路径，Gitea API 用） | `<org>/<def_slug>__<instance_short_id>` | `costrict-workflow/bug-fix-flow__f3a8b2c1` |
| workflow 实例 repo（完整 URL，对外响应用） | `<scheme>://<gitea_host>/<org>/<def_slug>__<instance_short_id>.git` | `https://gitea.costrict.local/costrict-workflow/bug-fix-flow__f3a8b2c1.git` |
| team 对应的 Gitea team | `t-<team_short_id>` | `t-7f3c9a1e` |
| workflow namespace（Gitea org） | `costrict-workflow`（全局唯一） | — |

> `team_short_id` = `team_id` 去掉连字符后取前 8 位；`instance_short_id` 同理。完整 `team_id` 在 @server 内部表中保留以避免碰撞。
>
> **`<gitea_host>` 来源**：@server 启动时由配置项 `GITEA_PUBLIC_URL`（如 `https://gitea.costrict.local`）注入；多租户/多 region 场景下可按 tenant_id 路由到不同 host。响应中始终返回可直接用于 `git clone` 的完整 URL。

### 4.2 Gitea namespace 布局示例

```
gitea.costrict.local/
└── costrict-workflow/                         ← workflow namespace（Gitea org，全局唯一）
    │
    ├── bug-fix-flow__f3a8b2c1/                ← team A 的实例 1 repo
    ├── bug-fix-flow__a9e7d4f2/                ← team A 的实例 2 repo
    ├── release-pipeline__c1d2e3f4/            ← team B 的实例 1 repo
    │
    └── teams/                                 ← Gitea team（隐式，per-team）
        ├── t-7f3c9a1e                        ← team A 的 Gitea team
        │   ├── members: [alice(owner), bob(write), charlie(write)]
        │   └── authorized_repos:
        │       ├── bug-fix-flow__f3a8b2c1 (write)
        │       └── bug-fix-flow__a9e7d4f2 (write)
        └── t-99aa88bb                        ← team B 的 Gitea team
            └── ...
```

### 4.3 team 角色 → Gitea team 权限映射

| team 角色 | Gitea team 内权限 | 能力 |
|---|---|---|
| `owner` | `admin` | push / merge / 加减成员 / 改 team 配置 |
| `write` | `write` | push 节点分支 / 开 PR / merge PR |
| `read` | `read` | 仅查看（评审人 / 观察者） |

---

## 5. 调用方对接指南

### 5.1 workflow 编排器

| 时机 | 调用 |
|---|---|
| workflow 实例启动 | `POST /api/internal/workflow/init`，header 带 `X-Internal-Service-Token`，body 传 team_id + instance_id + initial_members（每个成员含 `user_id` 或 `enterprise_uid` + `role`，同 team 第 1 次必传，之后可省略）；本接口仅创建空 repo + 授权，不接收 workflow 定义内容 |
| 实例结束 / archive | 走既有 `csc wf archive`（不在本 API 范围） |

### 5.2 workspace 服务

> workspace 服务是 team 的外部所有者，负责 team 的生命周期与成员管理；@server 仅承接 team 对应的 Gitea team 联动。

| 时机 | 调用 |
|---|---|
| 用户加入 team | `POST /api/internal/teams/:id/members:sync`，change_type=delta，changes=[{user_id 或 enterprise_uid, action: add, role: write}] |
| 用户离开 team | 同上，changes=[{..., action: remove}] |
| 用户角色变更 | 同上，changes=[{..., action: role_change, role: <new_role>}] |
| team 解散 | 全量 remove + archive（建议依次调用 sync + archive） |
| 灾难恢复 / 数据修复 | change_type=full_sync，传当前 team 完整期望成员列表（每项含 user_id 或 enterprise_uid + role） |

### 5.3 幂等性

- 接口 1：基于 `(workflow_def_slug, instance_id)` 幂等；X-Request-Id 推荐用于追踪
- 接口 2：基于 `(team_id, identifier, action)` 业务幂等（identifier = user_id 或 enterprise_uid）；重复 add 同一用户不会出错，重复 remove 视为成功（Gitea API 返回 404 时 @server 转换为 no-op）

---

## 6. 与现有 CSC_WF_SUBCOMMAND_CONTRACT 的关系

| CSC 命令 | 与本 API 关系 |
|---|---|
| `csc wf init` | **不再透传**：本 API 已改为内部接口（service token 鉴权），csc 客户端无 token 不能直接调用。csc 端的 `wf init` 应改为由 workflow 编排器代为调用本 API；csc 客户端只走编排器暴露的对外接口 |
| `csc wf authorize <username>` / `revoke <username>` | **保持不变**。csc 命令是**单实例级别的 ad-hoc 授权**（owner 主动加人）；本 API 接口 2 是**team 级别的批量同步**（team 成员变更联动）。两者通过同一 Gitea team 写入，互不冲突（最终一致） |
| `csc wf node push / approve / merge` | 不受影响 |
| `csc wf transfer-owner <username>` | **冲突告警**：csc 转让 owner 会修改 `.workflow/instance.json.owner`，但本 API 模型下 team owner 由 team 角色（owner）决定。建议禁用 `csc wf transfer-owner`，owner 转移走 workspace 服务的角色变更接口（接口 2 的 role_change=owner）|

> **重要**：本 API 上线后，建议在 csc 客户端文档中加注："team 成员变更应走 workspace 服务 → 接口 2，而非 csc wf authorize/revoke；后者保留给单实例临时授权场景"。

---

## 7. 开放问题

| # | 问题 | 阻塞实施 |
|---|---|---|
| **W-1** | team 解散时 Gitea team 是否删除？建议 archive team 后保留 Gitea team 30 天（审计需要），TTL 过期后由后台 worker 清理 | 设计阶段 |
| **W-2** | archived workflow 实例 repo 是否保留 team 授权？建议 archive 时同步从 team 的 authorized_repos 移除，避免遗留权限 | 设计阶段 |
| **W-3** | @server 维护 `team_members` 表 vs 每次实时查 Gitea？前者支持 full_sync diff 高效计算；后者实现简单但慢。建议前者（与 [TEAM_ORG_UNIFICATION.md](../identity-tenant/TEAM_ORG_UNIFICATION.md) `gitsync_failed_events` 死信表配套） | 设计阶段 |
| **W-4** | 跨 team 用户移动（A team 离开 → B team 加入）：是否要事务性保证？建议两端各自调用接口 2，最终一致即可（短暂窗口期内用户在两个 team 都有/都没有权限可接受） | 设计阶段 |
| **W-5** | team 角色 `owner` 转移时是否要求新 owner 标识能解析到有效 gitea_username？建议是（避免新 owner 接管后无法操作 repo）；当前接口 2 已通过 cs-user SDK 解析失败 → `GITEA_USERNAME_UNRESOLVED` 兜底 | 设计阶段 |

---

## 修订记录

| 版本 | 日期 | 修订内容 |
|---|---|---|
| v1.0 | 2026-07-15 | 首次发布：定义 workflow init + workspace 成员变更 2 个接口；team 粒度为 per-workspace；initial_members 调用方显式传；delta + full_sync 双模式；Gitea namespace / team 命名规则；与 CSC_WF_SUBCOMMAND_CONTRACT 既有命令的关系明确；异步任务模型（>100 成员） |
| v1.1 | 2026-07-15 | init 接口职责简化：workflow 实例 repo 仅创建空容器，不再写入 `.workflow/definition.snapshot.yaml` / `.workflow/instance.json`；移除 `definition_snapshot` / `audit_config` 请求字段；移除 `DEFINITION_DRIFT` 错误码（替换为 `REPO_EXISTS_WITH_DIFFERENT_OWNER`，覆盖跨 workspace 误用场景）；幂等规则简化为按 `(workflow_def_slug, instance_id)` no-op；@server 内部流程同步去除 commit 步骤 |
| v1.2 | 2026-07-15 | 明确 `tenant_id` 唯一来源为 JWT claim：§1.4 重写为「鉴权与租户识别」，禁止调用方在 request body 传 tenant_id；§2.5 / §3.5 内部流程显式标注「从 JWT 取 tenant_id」并补「反查 workspace_id → 所属 tenant_id 比对」步骤 |
| v1.3 | 2026-07-15 | 响应字段 `wf_repo_path` 取值改为完整 URL（含 scheme + host + `.git` 后缀），便于调用方直接 `git clone`；§4.1 命名规则表区分「相对路径（Gitea API 用）」与「完整 URL（对外响应用）」；新增 `<gitea_host>` 来源说明（配置项 `GITEA_PUBLIC_URL`） |
| v1.4 | 2026-07-15 | **接口调整为内部接口**：路径前缀 `/api/internal/`（网关不放行）；去除 JWT 用户鉴权，改为 service token 鉴权（header `X-Internal-Service-Token`，双方约定）；`tenant_id` 从 JWT claim 改为可选 header `X-Tenant-Id`，未传时走 `DEFAULT_TENANT_ID`；移除 workspace 归属反查与 `TENANT_MISMATCH` 错误码；cs-user 职责收窄为**仅做 user_id 合法性校验**（不再查 `user_gitea_binding`、不校验 workspace）；调用方在 body 中直传 `gitea_username`（替代旧的 `username` 字段）；错误码表重整：移除 `NOT_AUTHENTICATED` / `WORKSPACE_FORBIDDEN` / `USER_GITEA_BINDING_MISSING`，新增 `UNAUTHORIZED_SERVICE` / `USER_ID_INVALID` / `GITEA_USER_NOT_FOUND`；§6 标注 `csc wf init` 不再透传本 API（csc 客户端无 service token） |
| v1.5 | 2026-07-15 | **成员标识对齐 identity-tenant 体系**：调用方不再传 `gitea_username`（workspace 服务无此信息）；改为支持 `user_id`（平台 UUID，对齐 USER_CENTER §6 base 层）或 `enterprise_uid`（企业标识，对齐 MULTI_TENANCY §6.5.1 `enterprise_identities.enterprise_uid`），二选一；@server 调 cs-user SDK `ResolveGiteaUsername(identifier, tenant_id)` 解析 → gitea_username（cs-user 查 `user_gitea_binding` / `enterprise_identities`）；错误码调整：移除 `GITEA_USER_NOT_FOUND`，新增 `ENTERPRISE_UID_NOT_FOUND` / `GITEA_USERNAME_UNRESOLVED`（用户从未登录 Gitea 场景） |
| v1.6 | 2026-07-15 | **概念重命名：`workspace` → `workflow_team`**（避免与 @server 自身的 workspace 业务工作区概念混淆）：`workspace_id` → `workflow_team_id`；`workspace_team`（JSON 字段）→ `team`；Gitea team 命名前缀 `ws-` → `wt-`；endpoint 路径 `/api/internal/workspaces/:workspace_id/...` → `/api/internal/workflow-teams/:workflow_team_id/...`；错误码 `WORKSPACE_TEAM_NOT_FOUND` → `WORKFLOW_TEAM_NOT_FOUND`；内部表 `workspace_team_members` → `workflow_team_members`；评审范围与「workspace 服务」（外部调用方服务名）保持不变，仅澄清其为 workflow_team 的外部所有者；v1.0~v1.5 修订记录保留原文字（历史快照） |
| v1.7 | 2026-07-15 | **概念进一步简化：`workflow_team` → `team`**（"workflow_" 前缀剥离，全程使用 team）：`workflow_team_id` → `team_id`；`workflow_team_short_id` → `team_short_id`；endpoint `/api/internal/workflow-teams/:workflow_team_id/...` → `/api/internal/teams/:team_id/...`；Gitea team 命名前缀 `wt-` → `t-`；错误码 `WORKFLOW_TEAM_NOT_FOUND` → `TEAM_NOT_FOUND`；内部表 `workflow_team_members` → `team_members`；标题 `Workflow Team API` → `Team API`；文档头「命名说明」重写为「team = workflow 业务侧的团队概念」 |