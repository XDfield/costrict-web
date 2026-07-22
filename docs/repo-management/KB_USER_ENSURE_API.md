# KB Ensure 用户侧接口契约

| 版本 | v1.0 |
|---|---|
| 创建日期 | 2026-07-22 |
| 依据 | [`TEAM_NAMESPACE_API_REFERENCE.md`](./TEAM_NAMESPACE_API_REFERENCE.md) §接口 9（internal 版本） / [`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) v2.0 / [`CSC_KB_SUBCOMMAND_CONTRACT.md`](./CSC_KB_SUBCOMMAND_CONTRACT.md) v1.0 |
| 算法实现唯一归属 | **costrict-web server**（csc 不内置副本） |

> 本规范定义 `POST /api/kb/ensure`——给 **csc 客户端**直调的 KB repo ensure 入口。行为与 internal `POST /api/internal/kb/ensure`（见 [`TEAM_NAMESPACE_API_REFERENCE.md`](./TEAM_NAMESPACE_API_REFERENCE.md) §接口 9）等价，差异仅在**鉴权模型**与**team_id 解析策略**：
>
> - internal 版本：`X-Internal-Service-Token` 鉴权，调用方（编排器）显式传 `team_id`
> - 用户侧版本（本文档）：**用户 JWT** 鉴权，server 根据 JWT 反查当前用户所属 team 自动派生 `team_id`；多团队场景下要求用户显式选择

---

## 0. 与 internal 版本的关系

| 维度 | `POST /api/internal/kb/ensure`（§接口 9） | `POST /api/kb/ensure`（本文档） |
|---|---|---|
| 鉴权 | `X-Internal-Service-Token`（共享密钥） | 用户 JWT（Casdoor 签发） |
| 调用方 | 编排器 / workflow 引擎 | csc 客户端（用户机器） |
| `team_id` 入参 | 必填，调用方显式传 | **可选**；缺省时由 server 根据 JWT 反查用户所属 team 自动派生；多团队场景必填 |
| 多团队处理 | 不存在（编排器自己知道 team） | 见 §3.2 — 0/1/多三类分支 |
| 响应 shape | 见 §接口 9.3 | 一致（§4.3） |
| 路径算法 | KB_REPO_PATH_ALGORITHM v2.0 | 同 |
| bot 凭据返回 | ✓ | ✓（同；token 仅在响应中透传一次，禁止写日志） |

> **设计动机**：internal 接口已覆盖"编排器代调"路径（csc → 编排器 → @server）。用户侧接口面向**无编排器**场景——csc 直接 @server，省一跳；同时把"团队选择"决策点放回用户侧，避免编排器成为单点瓶颈。

---

## 1. 接口契约

```
POST /api/kb/ensure HTTP/1.1
Host: server.costrict.local
Authorization: Bearer <用户 JWT>
X-Request-Id: <uuid>           （推荐；缺失则 @server 生成）
Content-Type: application/json

{
  "code_repo_url": "https://github.com/ownerA/proj.git",
  "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a"   // 可选
}
```

### 1.1 请求字段

| 字段 | 必填 | 类型 | 说明 |
|---|---|---|---|
| `code_repo_url` | ✓ | string | 必须 http(s):// 起头；ssh / git scheme 由 csc 端归一为 https（见 [`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) §3.6） |
| `team_id` | 否 | UUID | 多团队用户必填（用于显式选择）；单团队用户可省（server 自动派生）；零团队用户传也无效（直接 403） |

### 1.2 鉴权

| Header | 必填 | 说明 |
|---|---|---|
| `Authorization: Bearer <jwt>` | ✓ | Casdoor 签发的用户 JWT；走 `middleware.RequireAuth`，校验通过后将 `subject_id`（用户 UUID）写入 `UserIDKey` |

> 不需要 `X-Internal-Service-Token`——这是用户侧接口，与 §接口 1-9 的 internal 鉴权模型完全隔离。

---

## 2. team_id 解析策略

server 收到请求后，按以下顺序解析 `team_id`：

### 2.1 显式 `team_id`（请求体已传）

1. 校验 `team_id` 是合法 UUID，否则 400 `INVALID_REQUEST`
2. **校验当前用户是否属于该 team**——调用 `ResolveCurrentUserTeams(ctx, subject_id)` 拿到用户所属 team 列表，检查传入 `team_id` 是否在列表中
3. 不属于 → 403 `TEAM_MEMBERSHIP_REQUIRED`
4. 属于 → 用该 `team_id` 继续 §3 主流程

### 2.2 隐式派生（请求体未传 `team_id`）

调用 `ResolveCurrentUserTeams(ctx, subject_id)` 反查用户所属 team 列表：

| 列表长度 | 分支 | 响应 |
|---|---|---|
| **0** | 零团队 | 403 `NO_TEAM_MEMBERSHIP`（§3.4）——提示用户先加入团队 |
| **1** | 单团队 | 自动派生 `team_id = teams[0].team_id`，继续 §3 主流程 |
| **>1** | 多团队 | 409 `TEAM_DISAMBIGUATION_REQUIRED`（§3.3）——返回 team 列表，要求用户重选 |

### 2.3 `ResolveCurrentUserTeams` 的实现归属

**真相源**：外部 `org-team-service`（详见 [`TEAM_ORG_UNIFICATION.md`](../identity-tenant/TEAM_ORG_UNIFICATION.md)）。本接口通过 cs-user RPC 反查；csc 端不参与 team 归属解析。

```
server (POST /api/kb/ensure)
  └─ cs-user RPC: GET /api/internal/users/:subject_id/teams
       └─ cs-user 查 org-team-service（或本地缓存镜像）
            → 返回 [{team_id, display_name, role}, ...]
```

> **降级策略**：org-team-service 不可用时，server 返回 503 `ORG_TEAM_SERVICE_UNAVAILABLE`，**不**回落到 Gitea org membership 反查——避免双源真相漂移。

---

## 3. 行为分支

### 3.1 主流程（team_id 已确定）

复用 [`TEAM_NAMESPACE_API_REFERENCE.md`](./TEAM_NAMESPACE_API_REFERENCE.md) §接口 9.4 的全部行为：

1. 前置校验 team ns 存在（不存在 → 412 `TEAM_NS_NOT_INITIALIZED`）
2. 计算 `kb_repo_path`（按 [`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) v2.0）
3. get-or-create kb repo（首次 ensure 该 `(code_repo_url, team_id)` 时建 private repo + main branch protection）
4. **附带 bot 凭据返回**

### 3.2 team_id 解析矩阵

| `team_id` 入参 | 用户实际 team 数 | server 行为 |
|---|---|---|
| 未传 | 0 | 403 `NO_TEAM_MEMBERSHIP` |
| 未传 | 1 | 自动派生，进 §3.1 主流程 |
| 未传 | ≥2 | 409 `TEAM_DISAMBIGUATION_REQUIRED` + team 列表 |
| 已传 | 0（用户无任何 team）| 403 `NO_TEAM_MEMBERSHIP`（即便传了 team_id，成员校验也过不了） |
| 已传 | ≥1 且 `team_id` 在列表内 | 进 §3.1 主流程 |
| 已传 | ≥1 且 `team_id` 不在列表内 | 403 `TEAM_MEMBERSHIP_REQUIRED` |

### 3.3 多团队分支详细响应

```http
HTTP/1.1 409 Conflict
Content-Type: application/json

{
  "error_code": "TEAM_DISAMBIGUATION_REQUIRED",
  "message": "current user belongs to multiple teams; specify team_id explicitly",
  "teams": [
    {
      "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
      "display_name": "Platform Team",
      "role": "owner"
    },
    {
      "team_id": "9b8c7d6e-1234-...",
      "display_name": "Mobile Team",
      "role": "member"
    }
  ],
  "hint": "re-call POST /api/kb/ensure with team_id field set to one of the above"
}
```

csc 端行为：**不直接 exit**；交互式场景下提示用户选择，非交互式场景（CI）exit ≠ 0 并打印 hint。

### 3.4 零团队分支详细响应

```http
HTTP/1.1 403 Forbidden
Content-Type: application/json

{
  "error_code": "NO_TEAM_MEMBERSHIP",
  "message": "current user does not belong to any team; join a team before initializing kb",
  "hint": "ask your platform admin to add you to a team, or check your org-team-service membership"
}
```

### 3.5 成员校验失败分支

```http
HTTP/1.1 403 Forbidden
Content-Type: application/json

{
  "error_code": "TEAM_MEMBERSHIP_REQUIRED",
  "message": "current user is not a member of the specified team",
  "team_id": "<requested team_id>",
  "hint": "ask the team owner to add you, or pick a team you belong to"
}
```

---

## 4. 响应

### 4.1 成功响应（200）

shape 与 [`TEAM_NAMESPACE_API_REFERENCE.md`](./TEAM_NAMESPACE_API_REFERENCE.md) §接口 9.3 完全一致：

```json
{
  "kb_repo_path": "t-7f3c9a1e/kb-github.com__ownera__proj",
  "kb_clone_url": "https://gitea.costrict.local/t-7f3c9a1e/kb-github.com__ownera__proj.git",
  "kb_web_url": "https://gitea.costrict.local/t-7f3c9a1e/kb-github.com__ownera__proj",
  "created": { "kb_repo": true },
  "team_ns_exists": true,
  "algorithm_version": "v2",
  "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
  "team_resolution": "implicit_single",
  "bot_credentials": {
    "gitea_username": "bot-t-7f3c9a1e",
    "gitea_user_id": 42,
    "token": "cs-bot-1a2b3c4d5e6f...",
    "clone_url_with_token": "https://bot-t-7f3c9a1e:cs-bot-1a2b3c4d5e6f...@gitea.costrict.local/t-7f3c9a1e/kb-github.com__ownera__proj.git"
  }
}
```

### 4.2 较 internal 版本新增字段

| 字段 | 必返回 | 说明 |
|---|---|---|
| `team_id` | ✓ | 实际使用的 team_id（无论隐式派生还是显式传入，都回显，便于客户端日志/审计）|
| `team_resolution` | ✓ | 派生路径标签：`implicit_single`（用户单团队自动派生）/ `explicit`（用户显式传入并校验通过）|

> 内部字段（`kb_repo_path` / `kb_clone_url` / `kb_web_url` / `created` / `team_ns_exists` / `algorithm_version` / `bot_credentials.*`）的语义与 internal 版本完全一致，不在此复述。

### 4.3 token 安全约定

`bot_credentials.token` 与 `clone_url_with_token` 的处理与 internal 版本一致：
- 仅在响应体内返回一次
- **server 禁止写日志**（含 access log / error log / metric label）
- csc 端建议落进程内存，进程退出即失效；如需持久化，落用户本地 secret store（macOS Keychain / Windows Credential Manager / Linux libsecret）

---

## 5. 错误码总表

| HTTP | error_code | 触发条件 | 是否可重试 |
|---|---|---|---|
| 400 | `INVALID_REQUEST` | `code_repo_url` 缺 scheme / 非 http(s) / 裸 host 无 path；`team_id`（如传）非 UUID | 修正后重试 |
| 401 | `UNAUTHORIZED` | JWT 缺失 / 过期 / 签名失败（来自 `RequireAuth` 中间件） | 刷新 token 后重试 |
| 403 | `NO_TEAM_MEMBERSHIP` | 用户零团队（§3.4） | 加入团队后重试 |
| 403 | `TEAM_MEMBERSHIP_REQUIRED` | 显式传入的 `team_id` 用户不属于（§3.5） | 改选自己所属 team 后重试 |
| 409 | `TEAM_DISAMBIGUATION_REQUIRED` | 用户多团队且未显式传 `team_id`（§3.3） | 显式传 `team_id` 后重试 |
| 412 | `TEAM_NS_NOT_INITIALIZED` | team ns 未通过 `POST /api/internal/teams` 创建 | 先调 internal create team |
| 502 | `KB_REPO_PROVISIONING_FAILED` | 上游 Gitea 5xx / 网络（与 internal 版本一致） | 退避后重试 |
| 503 | `ORG_TEAM_SERVICE_UNAVAILABLE` | cs-user / org-team-service RPC 失败（§2.3 降级策略） | 退避后重试 |

---

## 6. 与 csc 端契约（[`CSC_KB_SUBCOMMAND_CONTRACT.md`](./CSC_KB_SUBCOMMAND_CONTRACT.md) 联动）

csc 端 `kb push` / `kb pull` / `kb status` 等子命令的 ensure 调用约定：

| csc 行为 | 合规要求 |
|---|---|
| JWT 携带 | 从 csc config（`~/.costrict/config.toml` 或 env）读，**禁止**写入 git repo / 共享文件 |
| `team_id` 默认行为 | 单团队场景不传；多团队场景 csc 应**交互式询问**用户后传显式 `team_id`；CI 模式（非交互）应直接 fail-fast，不静默选择 |
| 多团队响应处理 | 收到 409 `TEAM_DISAMBIGUATION_REQUIRED` 时，把 `teams` 列表渲染给用户；用户选择后用所选 `team_id` 重发 |
| 零团队响应处理 | 收到 403 `NO_TEAM_MEMBERSHIP` 时，**仅打印 hint**，不自动加入任何 team（避免误操作）|
| URL 归一化责任 | ssh / git scheme → https，**csc 端做**（与 internal 版本约定一致）|
| team_id 来源（首选） | csc config / `.costrict/kb.yaml` / 环境变量；csc **不反查** team 归属——把派生责任交给 server（除非 csc 自己缓存了用户 team 列表做交互式 prompt） |

---

## 7. 示例：完整调用链

### 7.1 单团队用户（隐式派生）

```
用户 alice（subject_id=usr-alice-uuid）属于 1 个 team（7f3c9a1e-...）
在本地仓库 https://github.com/ownerA/proj.git 执行 csc kb push

1. csc 解析 code_repo_url（从 git remote origin），归一化为 https://github.com/ownerA/proj.git
2. csc 调 POST /api/kb/ensure
   Authorization: Bearer <alice JWT>
   Body: { "code_repo_url": "https://github.com/ownerA/proj.git" }
   （未传 team_id，交由 server 派生）

3. server 内部流程:
   a. RequireAuth 中间件验 JWT，写入 UserIDKey=usr-alice-uuid
   b. 调 ResolveCurrentUserTeams(ctx, usr-alice-uuid)
      → [{team_id: "7f3c9a1e-...", display_name: "Platform Team", role: "owner"}]
      → 长度 1，自动派生 team_id=7f3c9a1e-...
   c. 进 §3.1 主流程（与 internal §接口 9.4 完全一致）:
      - 前置 team ns 校验 → ok
      - 算 kb_repo_path = t-7f3c9a1e/kb-github.com__ownera__proj
      - get-or-create kb repo + main branch protection
      - 解密 bot token 明文

4. server 返回:
   {
     "kb_repo_path": "t-7f3c9a1e/kb-github.com__ownera__proj",
     "kb_clone_url": "https://gitea.costrict.local/t-7f3c9a1e/kb-github.com__ownera__proj.git",
     ...
     "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
     "team_resolution": "implicit_single",
     "bot_credentials": { ... }
   }

5. csc 行为:
   - 用 bot_credentials.clone_url_with_token 执行 git push origin main
```

### 7.2 多团队用户（显式选择）

```
用户 bob 属于 2 个 team（Platform + Mobile），首次未传 team_id

1. csc 调 POST /api/kb/ensure
   Body: { "code_repo_url": "https://github.com/o/p.git" }

2. server 返回 409:
   {
     "error_code": "TEAM_DISAMBIGUATION_REQUIRED",
     "teams": [
       {"team_id":"7f3c9a1e-...","display_name":"Platform Team","role":"owner"},
       {"team_id":"9b8c7d6e-...","display_name":"Mobile Team","role":"member"}
     ],
     "hint": "re-call POST /api/kb/ensure with team_id field set to one of the above"
   }

3. csc 渲染菜单（交互式）/ exit ≠ 0（CI 模式）
4. 用户选 Platform → csc 重发:
   Body: { "code_repo_url": "https://github.com/o/p.git", "team_id": "7f3c9a1e-..." }
5. server 进 §3.1 主流程，team_resolution="explicit"
```

### 7.3 零团队用户

```
用户 carol 不属于任何 team

1. csc 调 POST /api/kb/ensure
   Body: { "code_repo_url": "https://github.com/o/p.git" }

2. server 返回 403:
   {
     "error_code": "NO_TEAM_MEMBERSHIP",
     "message": "current user does not belong to any team; join a team before initializing kb",
     "hint": "ask your platform admin to add you to a team..."
   }

3. csc 打印 hint，exit ≠ 0
```

---

## 8. 开放问题（暂不解决）

| 问题 | 现状 | 后续 |
|---|---|---|
| `ResolveCurrentUserTeams` 实现位置 | 文档约定走 cs-user RPC → org-team-service；具体 RPC 路径待 cs-user 侧落地（`GET /api/internal/users/:subject_id/teams`） | 与 cs-user team-membership 模型同步排期 |
| 多团队时的"默认 team"概念 | 当前无——用户每次必须选 | 后续考虑在用户偏好里设 default team，省一次选择；不在本期 |
| archived team 是否出现在多团队列表 | 不出现（archived team 等同解散，成员已清）| 与 §接口 6 dissolve 语义一致 |
| 用户加入 team 后多团队列表缓存 | 当前每次请求实时查 cs-user | 高 QPS 场景考虑 server 短 TTL 缓存（≤5s），不影响一致性 |
| 跨 tenant 的 team 归属 | 当前 JWT 自带 tenant_id，仅查同 tenant 内 team | 多 tenant 用户场景后续展开 |

---

## 修订记录

| 版本 | 日期 | 修订内容 |
|---|---|---|
| v1.0 | 2026-07-22 | 首次发布：定义 `POST /api/kb/ensure`（用户侧 csc 直调入口）；JWT 鉴权；team_id 0/1/多三类分支；多团队 409 + team 列表 + hint；零团队 403；显式 team_id 成员校验 403；新增 `team_id` / `team_resolution` 响应字段；错误码表覆盖 400/401/403/409/412/502/503；与 [`TEAM_NAMESPACE_API_REFERENCE.md`](./TEAM_NAMESPACE_API_REFERENCE.md) §接口 9（internal 版本）行为对齐，差异仅在鉴权与 team_id 解析策略 |
