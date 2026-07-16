# Team Namespace API（@server 内部接口集）

| 字段 | 内容 |
|---|---|
| 版本 | v2.0.1 |
| 状态 | Draft · 评审中 |
| 创建日期 | 2026-07-15 |
| 最近更新 | 2026-07-16（v2.0.1：响应体补全 `kb_clone_url` / `kb_web_url` / `wf_clone_url` / `wf_web_url` 完整 URL 字段，落实 §10.3.1 SoT 约束） |
| 暴露范围 | **仅内网**——网关（gateway / api-gateway）不对 `/api/internal/*` 路径放行；只允许同 VPC / 服务网格内的可信服务通过 service token 调用 |
| 关联文档 | [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) v2.17 §16 / §17 / §18、[`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) v2.0、[`WORKFLOW_REPO_PATH_ALGORITHM.md`](./WORKFLOW_REPO_PATH_ALGORITHM.md) v2.0、[`CSC_KB_SUBCOMMAND_CONTRACT.md`](./CSC_KB_SUBCOMMAND_CONTRACT.md)、[`CSC_WF_SUBCOMMAND_CONTRACT.md`](./CSC_WF_SUBCOMMAND_CONTRACT.md)、[`TEAM_ORG_UNIFICATION.md`](../identity-tenant/TEAM_ORG_UNIFICATION.md) ADR-3 v3 |

> 本文档定义 @server 暴露给可信内部服务的**一组内部 HTTP 接口**（路径前缀 `/api/internal/`），用于 **team namespace（team ns）生命周期**及其承载的 KB repo / workflow 类型 repo 管理。team 是**平台级概念**（不是 workflow 业务专属），共享给 KB / workflow / 未来任何 team-scoped 业务数据 repo 使用。所有 Gitea API 调用由 @server 持 admin token 执行，调用方不感知 Gitea 细节。**接口不做用户鉴权，仅做服务鉴权。**
>
> **truth source 边界**：team 业务定义 / 成员关系的 truth source 是外部 [`org-team-service`](../identity-tenant/TEAM_ORG_UNIFICATION.md)；@server 仅镜像 Gitea 侧的 team ns org 状态，**不维护 team 表**。本文档接口由 `org-team-service` webhook 转发 / 编排器主动触发 / csc 通过编排器代调 三种路径发起。

---

## 0. 版本演进说明

| 版本 | 日期 | 文件名 | 接口范围 | 触发变更 |
|---|---|---|---|---|
| v1.0–v1.7 | 2026-07-15 早段 | `WORKFLOW_WORKSPACE_API.md` | 仅 2 个 workflow 专属接口（`workflow/init` + `teams/:id/members:sync`）；team = workflow 业务概念 | v2.16 workflow 引入 |
| **v2.0** | 2026-07-15 | **`TEAM_NAMESPACE_API.md`**（重命名 + 重构） | 4 个平台级接口：team ns 生命周期 + KB ensure + workflow init | v2.17 架构反转：team 升为平台概念，KB 也落 team ns |

**v1.x → v2.0 兼容性**：v1.x 仅文档定稿，未实际部署；v2.0 直接覆盖 v1.x。原 `WORKFLOW_WORKSPACE_API.md` 文件**删除**，所有引用切换到 `TEAM_NAMESPACE_API.md`。

---

## 1. 背景与设计原则

### 1.1 team ns 模型

- **team namespace（team ns）** = 一个 per-team 的 Gitea org `t-<team_short_id>`，承载 KB repo（§16）/ workflow 类型 repo（§17）/ 未来扩展
- 每个 team 对应**唯一** team ns；team ns 的 owner 名由 [`teamShortId` 算法](./KB_REPO_PATH_ALGORITHM.md#30-step-0-team_short-推导) 推导（UUID 前 8 hex）
- team ns org 成员 = team 成员；成员关系变更**同步**到 org（不再 per-repo 加 collaborator）

### 1.2 平台共享决策

**team 是平台级概念，不是 workflow 业务专属**：

- 同一 team 同时为 KB / workflow / 未来扩展提供命名空间与权限边界
- team ns 由首次 `members:sync` lazy 创建，与具体业务（KB / workflow）解耦
- @server 不感知 team 业务语义（team 是工程团队？产品团队？部门？）—— 仅做 Gitea 侧镜像

### 1.3 接口清单

| # | 接口 | 调用方 | 时机 | 职责 |
|---|---|---|---|---|
| 1 | `POST /api/internal/teams/:team_id/members:sync` | `org-team-service` webhook / 编排器 | team 成员变更 / 首次部署 | get-or-create team ns Gitea org + 同步成员 |
| 2 | `POST /api/internal/teams/:team_id/dissolve` | `org-team-service` webhook | team 解散 | archive team ns org + 移除成员 |
| 3 | `POST /api/internal/kb/ensure` | 编排器（代 csc） | `csc kb push` 前置 | get-or-create KB repo（落 team ns） |
| 4 | `POST /api/internal/workflow/init` | workflow 编排器 | workflow 实例启动 | get-or-create 类型 repo + 实例 branch |

### 1.4 服务鉴权与租户识别

> **本接口集不做用户鉴权**——没有 JWT、没有用户身份校验。仅做**服务鉴权**：调用方使用与 @server 双方预先约定的 service token 发起请求。

- HTTP Header：
  - `X-Internal-Service-Token: <service_token>`（必填，**双方预先约定的共享密钥**；建议通过 mTLS + sidecar secret 注入，不走配置明文）
  - `X-Tenant-Id: <tenant_id>`（**可选**；未传时 @server 使用配置项 `DEFAULT_TENANT_ID` 作为默认租户）
  - `X-Request-Id: <uuid>`（推荐，用于幂等 + 链路追踪）
- **不做跨租户越权保护**：调用方为可信内部服务，自行保证传入的 `tenant_id` / `team_id` 合法。@server 不反查 team 归属 tenant，不做 JWT/tenant 比对
- **token 校验**：@server 启动时从 secret manager 拉取约定的 service token，每个请求对比 `X-Internal-Service-Token`，不一致返回 `401 UNAUTHORIZED_SERVICE`
- @server 持 Gitea admin token 执行实际 Gitea API 调用；调用方 PAT 不参与

### 1.5 错误响应统一格式

```json
{
  "error": "<error_code>",
  "detail": "<human-readable message>",
  "hint": "<suggested next step>",
  "request_id": "<uuid from X-Request-Id or generated>"
}
```

| HTTP | error_code | 通用触发条件 |
|---|---|---|
| 400 | `INVALID_REQUEST` | 字段缺失 / 类型错误 / 不合约束 |
| 401 | `UNAUTHORIZED_SERVICE` | service token 缺失或不一致 |
| 404 | `TEAM_NOT_FOUND` | team_id 在 `org-team-service` 不存在（server 先调外部校验） |
| 412 | `TEAM_NS_NOT_INITIALIZED` | team ns Gitea org 不存在（KB ensure / workflow init 专用） |
| 500 | `GITEA_API_FAILURE` | admin PAT 调 Gitea 失败 |
| 502 | `UPSTREAM_ERROR` | `org-team-service` 调用失败 |

---

## 2. 接口 1：POST /api/internal/teams/:team_id/members:sync

### 2.1 用途

team ns 的**生命周期入口**。原子地完成：

1. **get-or-create team ns Gitea org** `t-<team_short>`（首次 sync 时创建，后续 sync 复用）
2. **同步成员**（delta 增量 或 full_sync 全量对账）
3. 首次创建时配置 org（`members_can_create_repos=false` + private + 默认权限 member=write）

### 2.2 请求

```http
POST /api/internal/teams/7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a/members:sync HTTP/1.1
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Tenant-Id: <tenant_id>（可选，缺省走默认租户）
X-Request-Id: <uuid>（推荐）
Content-Type: application/json

{
  "mode": "delta",                          // "delta" | "full_sync"
  "team_display_name": "Platform Team",     // 可选；首次创建时用作 org description
  "add_members": [
    { "user_id": "u-alice-uuid",  "gitea_username": "alice"  },
    { "user_id": "u-bob-uuid",    "gitea_username": "bob"    }
  ],
  "remove_members": [
    { "user_id": "u-charlie-uuid", "gitea_username": "charlie" }
  ]
}
```

#### 2.2.1 字段说明

请求 path：

| 字段 | 必填 | 说明 |
|---|---|---|
| `team_id` | ✓ | UUIDv4；server 先调 `org-team-service` 校验存在性，不存在返回 404 `TEAM_NOT_FOUND` |

请求 body：

| 字段 | 必填 | 类型 | 说明 |
|---|---|---|---|
| `mode` | ✓ | enum `"delta"` / `"full_sync"` | `delta`：仅按 add/remove 变更；`full_sync`：add/remove 之外的现存成员**全部 remove**（用于周期性对账） |
| `team_display_name` | | string | 首次创建 org 时用作 description 一部分；后续 sync 忽略 |
| `add_members` | | array | 添加成员列表（delta 模式必填其一） |
| `add_members[].user_id` | ✓ | string | @server 内部用户 ID（用于审计） |
| `add_members[].gitea_username` | ✓ | string | Gitea 侧用户名（用于实际邀请） |
| `remove_members` | | array | 移除成员列表（同结构） |

### 2.3 响应

```json
{
  "team_ns_org": "t-7f3c9a1e",
  "team_ns_exists": true,
  "created": false,
  "members_changed": {
    "added": ["alice", "bob"],
    "removed": ["charlie"]
  },
  "skipped": [
    { "gitea_username": "dave", "reason": "user not provisioned in Gitea yet" }
  ],
  "current_members_count": 12
}
```

### 2.4 行为分支

| 场景 | server 行为 | created | team_ns_exists |
|---|---|---|---|
| **team ns 不存在**（首次 sync） | ① 调 `POST /admin/users` 创建 org `t-<team_short>` ② 配置 `members_can_create_repos=false` + private + member=write ③ 写 description（含 full team_id + display_name + source） ④ 邀请 add_members 加入 org | true | true |
| **team ns 已存在 + delta 模式** | ① add_members 调 `PUT /orgs/<org>/members/<user>` ② remove_members 调 `DELETE /orgs/<org>/members/<user>` | false | true |
| **team ns 已存在 + full_sync 模式** | ① `GET /orgs/<org>/members` 取当前成员 ② 计算 diff（add / remove） ③ 按差异调用同上 API | false | true |
| **用户尚未在 Gitea 创建** | 该用户加入被跳过，记入 `skipped` 字段并附 reason | — | true |

### 2.5 错误码（接口专属）

| HTTP | error_code | 触发条件 | 响应体 |
|---|---|---|---|
| 400 | `INVALID_REQUEST` | `team_id` 非 UUID / mode 不在 enum / `gitea_username` 缺失 | `{ "error": "invalid_request", "detail": "..." }` |
| 401 | `UNAUTHORIZED_SERVICE` | service token 缺失或不一致 | `{ "error": "unauthorized_service" }` |
| 404 | `TEAM_NOT_FOUND` | team_id 在 `org-team-service` 不存在 | `{ "error": "team_not_found", "detail": "team_id not in org-team-service" }` |
| 409 | `TEAM_NS_CONFLICT` | `team_short_id` 已被另一 team 占用（UUID 前 8 hex 碰撞，理论概率 < 10^-6/1000 teams） | `{ "error": "team_ns_conflict", "hint": "admin must rename or migrate; see ops runbook" }` |
| 500 | `GITEA_API_FAILURE` | admin PAT 调 Gitea 失败 | `{ "error": "gitea_api_failed", "detail": "..." }` |
| 502 | `UPSTREAM_ERROR` | `org-team-service` 调用失败 | `{ "error": "upstream_error", "detail": "org-team-service returned 5xx" }` |

### 2.6 幂等保证

- 相同 `team_id` + 相同成员集重复 `members:sync`：进入 delta 空操作分支，返回 `members_changed.added=[]/removed=[]`
- `mode=full_sync` 多次调用同一目标集：第二次 diff 为空，no-op

---

## 3. 接口 2：POST /api/internal/teams/:team_id/dissolve

### 3.1 用途

team 解散时由 `org-team-service` 推送 `team:dissolved` webhook，转调本接口。原子地完成：

1. **archive team ns org**（保留审计窗口，不立即删除）
2. **移除所有成员**（保留 repo 内容）
3. 写入解散元数据（reason + 时间戳 + actor）

### 3.2 请求

```http
POST /api/internal/teams/7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a/dissolve HTTP/1.1
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Tenant-Id: <tenant_id>（可选）
X-Request-Id: <uuid>（推荐）
Content-Type: application/json

{
  "reason": "team merged into another",
  "actor": { "user_id": "u-admin-uuid", "gitea_username": "admin" }
}
```

### 3.3 响应

```json
{
  "team_ns_org": "t-7f3c9a1e",
  "archived": true,
  "members_removed_count": 12,
  "retention_until": "2026-10-13T00:00:00Z",
  "audit_log_id": "audit-9c8d7e6f-..."
}
```

### 3.4 行为分支

| 场景 | server 行为 | archived |
|---|---|---|
| **team ns 存在** | ① archive org ② `GET /orgs/<org>/members` 全员 remove ③ 写审计 | true |
| **team ns 不存在**（未创建） | 视为已解散，no-op；记审计 | false |
| **team ns 已 archived**（重复 dissolve） | 幂等成功；不二次操作 | true |

### 3.5 retention

- 默认 90 天保留期；过期后由 ops runbook 决定是否物理删除
- `retention_until` 字段返回保留期截止时间（UTC ISO 8601）

### 3.6 错误码（接口专属）

| HTTP | error_code | 触发条件 |
|---|---|---|
| 400 | `INVALID_REQUEST` | `team_id` 非 UUID / `reason` 缺失 |
| 401 | `UNAUTHORIZED_SERVICE` | service token 不一致 |
| 404 | `TEAM_NOT_FOUND` | team_id 在 `org-team-service` 不存在 |
| 500 | `GITEA_API_FAILURE` | Gitea archive / member remove 失败 |

---

## 4. 接口 3：POST /api/internal/kb/ensure

### 4.1 用途

KB repo 的**唯一入口**。原子地完成：

1. **前置校验 team ns 存在**（不存在返回 412 `TEAM_NS_NOT_INITIALIZED`）
2. 计算 `kb_repo_path`（按 [`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) v2.0）
3. **get-or-create KB repo**（首次 ensure 时创建 + 配置 main branch protection；后续 ensure 复用）

> 权限不再 per-repo 显式表达——team ns org 成员自动有 read/write；接口不返回 `role` 字段。

### 4.2 请求

```http
POST /api/internal/kb/ensure HTTP/1.1
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Tenant-Id: <tenant_id>（可选）
X-Request-Id: <uuid>（推荐）
Content-Type: application/json

{
  "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
  "code_repo_url": "https://github.com/ownerA/proj.git"
}
```

#### 4.2.1 字段说明

| 字段 | 必填 | 类型 | 说明 |
|---|---|---|---|
| `team_id` | ✓ | UUID | 必须先经 `members:sync` 创建 team ns；否则 412 |
| `code_repo_url` | ✓ | string | 必须 http(s) scheme；ssh / git scheme 由 csc 归一化（详见算法 spec §3.6） |

### 4.3 响应（统一 schema）

```json
{
  "kb_repo_path": "t-7f3c9a1e/kb-github.com__ownera__proj",
  "kb_clone_url": "https://gitea.costrict.local/t-7f3c9a1e/kb-github.com__ownera__proj.git",
  "kb_web_url": "https://gitea.costrict.local/t-7f3c9a1e/kb-github.com__ownera__proj",
  "team_ns_exists": true,
  "created": false,
  "algorithm_version": "v2"
}
```

#### 4.3.1 字段说明

| 字段 | 必返回 | 说明 |
|---|---|---|
| `kb_repo_path` | ✓ | team ns 内的相对路径（`<org>/<repo>`）；用于审计 / 日志 / URL 拼接调试 |
| `kb_clone_url` | ✓ | **调用方 git clone / push / pull 必须直接使用此字段**——server 已拼接 `<tenant_gitea_base_url>/<kb_repo_path>.git`；调用方禁止自行拼接（详见 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) §10.3.1 SoT 约束） |
| `kb_web_url` | ✓ | 浏览器访问入口（不带 `.git` 后缀）；用于 portal UI 链接跳转 |
| `team_ns_exists` | ✓ | 通常为 true（false 时直接 412 不进入本响应） |
| `created` | ✓ | 本次 ensure 是否实际创建了 repo（首次为 true，后续幂等为 false） |
| `algorithm_version` | ✓ | 路径算法版本，与 [`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) 同步 |

> `<tenant_gitea_base_url>` 来源：@server 从 `tenant_configs.<JWT.tenant_id or X-Tenant-Id>.git.base_url` 解析（与 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) §1.5 协同）。

### 4.4 行为分支

| 场景 | server 行为 | created | team_ns_exists |
|---|---|---|---|
| **team ns 不存在** | 直接 412 `TEAM_NS_NOT_INITIALIZED` + hint "先调 members:sync" | — | false |
| **KB repo 不存在**（首次 ensure） | ① 校验 team ns 存在 ② `POST /admin/users/t-<team_short>/repos` 建 repo（private） ③ `POST /repos/.../branch_protections` 配置 main 保护 | true | true |
| **KB repo 已存在** | 视为幂等成功（no-op） | false | true |

### 4.5 错误码（接口专属）

| HTTP | error_code | 触发条件 | 响应体 |
|---|---|---|---|
| 400 | `INVALID_REQUEST` | `code_repo_url` 缺失 / 非 http(s) / `team_id` 非 UUID / 解析失败 | `{ "error": "invalid_request", "detail": "..." }` |
| 401 | `UNAUTHORIZED_SERVICE` | service token 不一致 | `{ "error": "unauthorized_service" }` |
| 412 | `TEAM_NS_NOT_INITIALIZED` | team ns Gitea org 不存在 | `{ "error": "team_ns_not_initialized", "hint": "call POST /api/internal/teams/:team_id/members:sync first" }` |
| 500 | `GITEA_API_FAILURE` | admin PAT 调 Gitea 失败 | `{ "error": "gitea_api_failed", "detail": "..." }` |

### 4.6 幂等保证

- 同一调用者重复 ensure 同一 `(code_repo_url, team_id)`：第二次进入"已存在"分支，返回 `created=false`
- 不同 team 对同一 code_repo_url：互不感知，各自 ensure 创建独立 KB repo

---

## 5. 接口 4：POST /api/internal/workflow/init

### 5.1 用途

workflow 类型 repo + 实例 branch 的**唯一入口**。原子地完成：

1. **前置校验 team ns 存在**（不存在返回 412）
2. 计算 `wf_repo_path` + `instance_branch`（按 [`WORKFLOW_REPO_PATH_ALGORITHM.md`](./WORKFLOW_REPO_PATH_ALGORITHM.md) v2.0 §A / §B）
3. **get-or-create 类型 repo**（首次 init 该 def 时创建 + 把 `definition_snapshot` 写入 main + 配置 main + `inst-*` 通配 branch protection）
4. **def drift 校验**（类型 repo 已存在时，从 main HEAD 读 def，与传入 `definition_snapshot` 对比，不一致返回 409）
5. **get-or-create 实例 branch**（base = main HEAD）

### 5.2 请求

```http
POST /api/internal/workflow/init HTTP/1.1
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Tenant-Id: <tenant_id>（可选）
X-Request-Id: <uuid>（推荐）
Content-Type: application/json

{
  "workflow_def_slug": "bug-fix-flow",
  "instance_id": "f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab",
  "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
  "definition_snapshot": "<yaml 内容：节点定义 / DAG / audit_level 配置>"
}
```

> **定义来源**：v2.17 下，定义的 canonical 存储是 `wf-<def>` repo 的 `main` 分支。`definition_snapshot` 字段仅在**类型 repo 首次创建**时使用——把 def 写入 main；后续 init 应从 main HEAD 读取定义（而非每次重传）。`definition_snapshot` 与 main 现存版本不一致时返回 409 `DEFINITION_DRIFT`，提示调用方先在 main 上 PR 更新 def。

#### 5.2.1 字段说明

| 字段 | 必填 | 类型 | 说明 |
|---|---|---|---|
| `workflow_def_slug` | ✓ | string | 团队自定义的 def 标识符；转义规则见算法 spec §3.A.5 |
| `instance_id` | ✓ | UUID | 由 workflow 编排器分配；推导 `inst-<short>` 见算法 spec §B |
| `team_id` | ✓ | UUID | 必须先经 `members:sync` 创建 team ns |
| `definition_snapshot` | 类型 repo 首次创建时必填 | string | workflow def 的 yaml 内容；类型 repo 已存在时仅用于 drift 校验 |

### 5.3 响应（统一 schema）

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
  "algorithm_version": "v2"
}
```

#### 5.3.1 字段说明

| 字段 | 必返回 | 说明 |
|---|---|---|
| `wf_repo_path` | ✓ | team ns 内的相对路径（`<org>/<repo>`）；用于审计 / 日志 / URL 拼接调试 |
| `wf_clone_url` | ✓ | **调用方 git clone / fetch / push 必须直接使用此字段**——server 已拼接 `<tenant_gitea_base_url>/<wf_repo_path>.git`；调用方禁止自行拼接（详见 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) §10.3.1 SoT 约束） |
| `wf_web_url` | ✓ | 浏览器访问入口（不带 `.git` 后缀）；用于 portal UI 链接跳转、PR 查看等 |
| `instance_branch` | ✓ | 本次实例的工作 branch 名（`inst-<short>`）；调用方需 `git fetch <wf_clone_url> <instance_branch>` 后切到该 branch |
| `created.type_repo` | ✓ | 是否新建了类型 repo（首次 init 该 def 为 true） |
| `created.instance_branch` | ✓ | 是否新建了实例 branch（幂等重入为 false） |
| `team_ns_exists` | ✓ | 通常为 true（false 时直接 412 不进入本响应） |
| `algorithm_version` | ✓ | 路径算法版本，与 [`WORKFLOW_REPO_PATH_ALGORITHM.md`](./WORKFLOW_REPO_PATH_ALGORITHM.md) 同步 |

> `<tenant_gitea_base_url>` 来源：@server 从 `tenant_configs.<JWT.tenant_id or X-Tenant-Id>.git.base_url` 解析（与 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) §1.5 协同）。

### 5.4 行为分支

| 场景 | server 行为 | created.type_repo | created.instance_branch | team_ns_exists |
|---|---|---|---|---|
| **team ns 不存在** | 412 `TEAM_NS_NOT_INITIALIZED` + hint | — | — | false |
| **类型 repo 不存在**（首次 init 该 def） | ① `POST /admin/users/t-<team_short>/repos` 建 type repo ② 把 `definition_snapshot` 写入 main（首次 canonical 存储） ③ 配置 main + `inst-*` 通配 branch protection ④ 从 main 创建 `inst-<short>` branch | true | true | true |
| **类型 repo 已存在 + 实例 branch 不存在** | ① 从 main HEAD 读 def 校验与 `definition_snapshot` 一致（不一致 → 409 `DEFINITION_DRIFT`） ② 从 main 创建 `inst-<short>` branch | false | true | true |
| **实例 branch 已存在**（幂等重入） | 视为成功（no-op） | false | false | true |
| **实例 branch 碰撞**（同 team 内 inst-<8hex> 已被占用） | 409 `INSTANCE_BRANCH_CONFLICT` + hint "重新分配 instance_id" | false | false | true |

### 5.5 错误码（接口专属）

| HTTP | error_code | 触发条件 | 响应体 |
|---|---|---|---|
| 400 | `INVALID_REQUEST` | `workflow_def_slug` 空 / `instance_id` / `team_id` 非 UUID / `definition_snapshot` 在类型 repo 首次创建时缺失 | `{ "error": "invalid_request", "detail": "..." }` |
| 401 | `UNAUTHORIZED_SERVICE` | service token 不一致 | `{ "error": "unauthorized_service" }` |
| 412 | `TEAM_NS_NOT_INITIALIZED` | team ns Gitea org 不存在 | `{ "error": "team_ns_not_initialized", "hint": "call members:sync first" }` |
| 409 | `DEFINITION_DRIFT` | `definition_snapshot` 与 main 现存 def hash 不一致 | `{ "error": "definition_drift", "detail": "existing=<hash> incoming=<hash>, open PR on main to update def" }` |
| 409 | `INSTANCE_BRANCH_CONFLICT` | `inst-<short>` 在该类型 repo 已被占用（碰撞或 UUID 重复） | `{ "error": "instance_branch_conflict", "hint": "regenerate instance_id" }` |
| 500 | `GITEA_API_FAILURE` | admin PAT 调 Gitea 失败 | `{ "error": "gitea_api_failed", "detail": "..." }` |

### 5.6 幂等保证

- 相同 `(workflow_def_slug, instance_id, team_id)` 二次调用：进入"实例 branch 已存在"分支，no-op 返回成功
- 类型 repo 已存在但实例 branch 新建：返回 `created.type_repo=false / instance_branch=true`

---

## 6. 接口调用模式

### 6.1 `org-team-service` webhook 转发

```
org-team-service            @server                           Gitea
       │                       │                                │
       │── team:members_changed webhook ──▶│                     │
       │   { team_id, add, remove, mode }  │                     │
       │                                  │── POST /api/internal │
       │                                  │   /teams/:id/members:sync
       │                                  │── admin PAT ─────────▶│ 创建/同步 org
       │                                  │◀──────── ack ─────────│
       │◀──────── 200 ────────────────────│                     │
```

### 6.2 编排器触发 KB ensure

```
csc (用户侧)        编排器                 @server                Gitea
   │                   │                       │                    │
   │── csc kb push ───▶│                       │                    │
   │   (含 team_id     │                       │                    │
   │    code_repo_url) │                       │                    │
   │                   │── POST /api/internal/kb/ensure ──▶│         │
   │                   │   X-Internal-Service-Token        │         │
   │                   │                                   │── admin PAT ─▶│ 创建/复用 repo
   │                   │◀── kb_repo_path + kb_clone_url ───│         │
   │                   │   + kb_web_url                    │         │
   │◀── kb_clone_url ──│                       │            │         │
   │   (csc 直接用)    │                       │            │         │
   │                   │                                   │            │
   │── git push <kb_clone_url> main:main ─────────────────────────────▶│
   │   (用调用者本人 PAT)                                              │
```

### 6.3 编排器触发 workflow init

```
workflow 编排器            @server                          Gitea
       │                       │                               │
       │── 实例启动 ─────────▶│                                │
       │   (def_slug,         │                                │
       │    instance_id,      │                                │
       │    team_id, def)     │                                │
       │── POST /api/internal/workflow/init ─▶│                  │
       │                       │── admin PAT ──────────────────▶│ get-or-create type repo
       │                       │── admin PAT ──────────────────▶│ create instance branch
       │◀── wf_repo_path ──────│                                │
       │   + wf_clone_url      │                                │
       │   + wf_web_url        │                                │
       │   + instance_branch   │                                │
       │                       │                                │
   后续节点执行器:                                              │
   git fetch <wf_clone_url> <instance_branch>                   │
   切 node/<seq>-<slug> branch, base=inst-<short>               │
   开 PR, reviewer 审计, merge 入 inst-<short>                  │
```

### 6.4 csc 与内部接口的关系

**csc 不直接调内部接口**——csc 客户端无 service token，由**编排器代为鉴权**调用：

| csc 命令 | 编排器代调接口 |
|---|---|
| `csc kb push`（[KB 契约](./CSC_KB_SUBCOMMAND_CONTRACT.md)） | `POST /api/internal/kb/ensure` |
| `csc wf init`（[WF 契约](./CSC_WF_SUBCOMMAND_CONTRACT.md)） | `POST /api/internal/workflow/init` |
| `csc wf node push` | init 已先调用，直接走 git/Gitea 原生 |
| `csc wf def update` | 直接走 git/Gitea 原生（main PR） |

team ns 生命周期（`members:sync` / `dissolve`）**不在 csc 范围内**——由 portal UI（admin 操作）或 `org-team-service` webhook 自动转发。

---

## 7. 安全考虑

### 7.1 service token 管理

- token 通过 secret manager 注入，不在配置明文出现
- token 轮换周期：建议 90 天；轮换时双 token 共存窗口（@server 接受新旧两个 token）
- token 泄露应急：立即在 secret manager 替换，@server 滚动重启

### 7.2 调用方白名单

| 接口 | 允许的调用方 |
|---|---|
| `members:sync` / `dissolve` | `org-team-service`、`platform-orchestrator` |
| `kb/ensure` | `csc-orchestrator`（代 csc）、`kb-generation-service` |
| `workflow/init` | `workflow-orchestrator` |

@server 可选实现调用方白名单（基于 mTLS 客户端证书 / sidecar 注入的 caller-id header），不在本规范强制要求。

### 7.3 审计

- 所有内部接口调用落入 @server audit log（含 `X-Request-Id` / caller / `team_id` / 关键参数 hash）
- Gitea 原生 audit log 记录 org / member / repo 级操作
- 双向对账：@server 与 `org-team-service` 可周期对账 team 成员一致性

### 7.4 速率限制

- 单调用方对单 `team_id` 的 `members:sync` 速率：建议 ≤ 10 req/min（防 webhook 风暴）
- `kb/ensure` / `workflow/init`：≤ 60 req/min（编排器批量部署时放宽）
- 超限返回 429 `RATE_LIMITED`

---

## 8. 多租户隔离

- `X-Tenant-Id` header 决定所有 Gitea 操作的 base_url（与 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) §1.5 协同）
- 调用方应保证 `team_id` 归属传入的 `tenant_id`；@server 不反查（信任内部服务）
- 跨 tenant 的 team 共享 / 合并：**不支持**（与 §1.5.5 一致）

---

## 9. 与原 `WORKFLOW_WORKSPACE_API.md` 的差异

| 维度 | v1.x（原） | v2.0（本文件） |
|---|---|---|
| 文件名 | `WORKFLOW_WORKSPACE_API.md` | `TEAM_NAMESPACE_API.md` |
| team 语义 | workflow 业务专属 | **平台级**（KB / workflow 共享） |
| team Gitea 实体 | Gitea team（不属于任何 org 或属于固定 org） | Gitea org `t-<team_short>`（per-team） |
| 接口数 | 2 | **4** |
| 接口 1 | `workflow/init` 创建实例 repo + 加 team | `teams/:id/members:sync` 创建 team ns org + 同步成员 |
| 接口 2 | `teams/:id/members:sync` 同步 Gitea team | `teams/:id/dissolve` archive team ns org |
| 接口 3 | — | `kb/ensure` KB repo 落 team ns |
| 接口 4 | — | `workflow/init` 类型 repo + 实例 branch 落 team ns |
| workflow repo 模型 | 每实例一 repo | **类型 repo + 实例 branch** |
| workflow PR base | main | **`inst-<short>`** |
| KB repo | 不涉及 | `t-<short>/kb-<host>__<segs>` |
| team_id 概念 | workflow 业务 team | 平台 team（外部 `org-team-service` 为 truth source） |

---

## 修订记录

| 版本 | 日期 | 修订内容 |
|---|---|---|
| v1.0–v1.7 | 2026-07-15 早段 | `WORKFLOW_WORKSPACE_API.md`：从最初"workflow workspace"概念逐步演进到 team = workflow 业务 team；定义 2 个内部接口（`workflow/init` + `teams/:id/members:sync`）；Gitea team（非 org）；workflow = 每实例一 repo |
| v2.0 | 2026-07-15 | **重构为 TEAM_NAMESPACE_API.md（平台级 team ns 接口集）**：①team 升为**平台级概念**（不再 workflow 专属）；②team Gitea 实体从 team 改为 **per-team org** `t-<team_short>`（lazy 创建于首次 `members:sync`）；③接口扩展为 **4 个**：(a) `teams/:id/members:sync` team ns 生命周期入口 + 成员同步 / (b) `teams/:id/dissolve` team 解散归档 / (c) `kb/ensure` KB repo 落 team ns / (d) `workflow/init` workflow 类型 repo + 实例 branch 落 team ns；④workflow 模型从「每实例一 repo」改为「类型 repo + 实例 branch」（base = main）；⑤workflow PR base 从 main 改为 `inst-<short>`；⑥新增 §0 版本演进说明、§6 接口调用模式（含 4 张序列图）、§7 安全考虑、§8 多租户隔离、§9 与原文件的差异对照；⑦依据 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) v2.17 §16 / §17 / §18 与 [`TEAM_ORG_UNIFICATION.md`](../identity-tenant/TEAM_ORG_UNIFICATION.md) ADR-3 v3 |
| v2.0.1 | 2026-07-16 | **响应体补全完整 URL 字段（落实 §10.3.1 SoT 约束）**：①§4.3 `kb/ensure` 响应新增 `kb_clone_url` / `kb_web_url` 字段 + 字段说明（调用方禁止自行拼接 base_url）；②§5.3 `workflow/init` 响应新增 `wf_clone_url` / `wf_web_url` 字段 + 字段说明；③§6.2 / §6.3 序列图更新——server 返回 path + clone_url + web_url，调用方 git 操作直接用 `*_clone_url`；④依据 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) §10.3.1（单一 source of truth：server 返回 ready-to-use 绝对 URL，csc / 编排器禁止自行拼接 `gitea_base_url + repo_path`） |
