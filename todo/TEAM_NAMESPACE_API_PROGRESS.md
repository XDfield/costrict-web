# Team Namespace API v1.1 — 实现进度

| 版本 | v1.1 |
|---|---|
| 创建日期 | 2026-07-21 |
| 依据 | [`docs/repo-management/TEAM_NAMESPACE_API_REFERENCE.md`](../docs/repo-management/TEAM_NAMESPACE_API_REFERENCE.md) |
| 计划文件 | `~/.costrict/plans/buzzing-nibbling-kernighan.md` |

## 已落地（Stage 1-11）

### cs-user 改动

- **Stage 1**：`SearchUsersByEmployeeNumber(ctx, employeeNumber, limit)` service 方法 — JOIN `employment_identities` 反查 `users`，按 `last_synced_at DESC` 取最近一行，tenant 范围 via `tenant.IDFromContext`。
- **Stage 1**：migration `20260722000000_add_employment_employee_number_index.sql` 加速反查（部分索引 `WHERE deleted_at IS NULL AND employee_number IS NOT NULL`）。
- **Stage 2**：`SearchUsers` handler 接受 `employee_number` query；与 `keyword` 互斥（同时传 → 400）。

### @server 改动

- **Stage 3**：migration `20260722000000_create_team_ns_and_bot_credentials.sql` 落 `team_ns` + `team_bot_credentials` 两表；`token_encrypted` TEXT / `token_sha256` CHAR(64)。
- **Stage 3**：`models/team_ns.go` — gorm 模型，`TokenEncrypted` 和 `TokenSHA256` 标 `json:"-"`。
- **Stage 4**：`crypto/aes_gcm.go` — AES-256-GCM 助手（NewAESGCM / Seal / Open / SHA256Hex / DecodeBase64Key）+ 8 项 round-trip / wrong-key / tamper / short / wrong-length 测试。
- **Stage 5**：`gitsync/client_extensions.go` — Gitea API 扩展（CreateUser / GetUserByName / CreateUserToken / DeleteUserToken / CreateOrg / GetOrgByName / ListOrgTeams）+ `dispatch` httptest 助手 + 8 项单测。
- **Stage 5**：`gitsync/client_extensions.go` 后续扩展：UpdateOrg / ListOrgMembers / AddOrgMember / RemoveOrgMember（用于 teamns 编排层）。
- **Stage 6**：`gitsync/bot_account.go` — `ProvisionBot` / `RevokeBot` / `RotateBot` 三件套；幂等 revoke（404 → nil），RotateBot 先 mint 后 revoke（最小 credential gap）；7 项单测。
- **Stage 6**：`gitsync/org_ops.go` — `EnsureOrg`（get-or-create，容忍 409）/ `UpdateOrgDescription` / `ListOrgMembers` / `AddOrgMember` / `RemoveOrgMember`（org 级 member 操作）/ `RemoveAllMembers`（dissolve 用）。
- **Stage 7**：`user/userref.go` — `UserRefResolver` 实现 UserRef（`user_id` XOR `employee_number`）→ `gitea_username` 解析；4 类 sentinel 错误（`ErrInvalidUserRef` / `ErrUserNotFound` / `ErrUserNotGiteaReady` / `ErrRPCUnavailable`）；10 项单测。
- **Stage 7**：`user/rpc_client.go` 扩展 — `GetGiteaBinding(ctx, subjectID)` 调 `GET /api/internal/users/:subject_id/gitea-binding`；`SearchByEmployeeNumber` / `SearchByEmployeeNumberN` 走 cs-user 的 employee_number 反查路径。
- **Stage 8**：`teamns/service.go` — 编排层，封装 7 个公开方法（CreateTeam / GetTeam / ListTeams / PatchTeam / DissolveTeam / SyncTeamMembers / RotateBotToken / DecryptBotToken / LookupTeamNS / LookupBotMeta / ResolveGiteaBaseURL）；idempotent create、tenant-scoped、full_sync 模式、dissolve retention 90 天。
- **Stage 8**：15 项 `teamns` 单测覆盖 validate / GetTeam / ListTeams 拒绝 tenant_id query / 分页 / archived 拒写 / dissolve 幂等 / rotate 边界 / decrypt 往返。
- **Stage 9**：`handlers/team_internal.go` — 7 个薄 handler（CreateTeam / GetTeam / ListTeams / PatchTeam / SyncTeamMembers / DissolveTeam / RotateBotToken），全部带 Swagger 注释，错误码映射 doc §1-§7。
- **Stage 9**：`handlers/team_internal_test.go` — 14 项测试覆盖 503 disabled / 400 bad JSON / 400 validation / 404 not-found / 200 happy / ListTeams 分页 / DissolveTeam idempotent。
- **Stage 10**：`workflow/paths.go` — 纯函数实现 `WORKFLOW_REPO_PATH_ALGORITHM.md` v2.0：`TeamShort` / `InstanceShort` / `EscapeDefSlug` / `WfRepoPath` / `WfBranchName`。
- **Stage 10**：`handlers/workflow_init.go` — `WorkflowInit` handler 验证 + 路径计算 + team_ns 存在性检查 + bot 凭据解密 + URL 组合；5 项测试覆盖 503 / 400 / 412 / 200 happy path。
- **Stage 11**：`cmd/api/main.go` — 在 cs-user RPC 已配置的分支下 wire `teamns.Service`（注入 db / gitsync / userref / AES-GCM）；新建 `/api/internal/*` 路由组（`middleware.InternalAuth`）注册 8 条路由；新增 `loadBotTokenKey()` 从 `CS_BOT_TOKEN_KEY` 读 base64-32byte key，缺失/格式错误均不阻断启动（handler 层降级为 503）。

## 测试覆盖状态

| 包 | 状态 |
|---|---|
| `cs-user/internal/user` | ✅ |
| `cs-user/internal/handlers` | ✅ |
| `server/internal/crypto` | ✅ |
| `server/internal/gitsync` | ✅ |
| `server/internal/user` | ✅ |
| `server/internal/teamns` | ✅ |
| `server/internal/workflow` | ✅ |
| `server/internal/handlers` | ⚠️ 新增测试全通过；`marketplace_test.go` 中 `TestLogBehavior_FeedbackDedupSupersedes` / `TestGetItemStats_RatingSurvivesTextOnlyEdit` 在全量运行时存在**预先存在**的污染（与本期改动无关，单独运行均通过） |
| `server/cmd/api` | ✅ `go build` 通过 |

## 已知限制（与计划文件一致）

- **`employment_identities(tenant_id, employee_number)` 唯一索引**：Phase B 待落地；本期 SearchUsers 命中多行时按 `last_synced_at DESC` 取一条。
- **AES-GCM key 管理**：env 注入（`CS_BOT_TOKEN_KEY` base64-32byte），KMS 集成推迟。
- **cs-user giteasync 懒创建**：UserRef 解析时若 `gitea_username` 为空，本期返回 `ErrUserNotGiteaReady`（→ 404），不触发自动懒创建；上游需先调 cs-user `apply-enterprise-mapping`。
- **workflow/init Gitea 侧操作**：type repo 创建 + branch protection + instance branch 创建**未 wire**（workflow 编排器集成尚未排期）；handler 已落地合规契约响应，Created 标志位返回 false。后续 slice 通过新增 `teamns.EnsureWorkflowRepo` 接入。
- **bot username 抢占**：cs-user 没有 `bot-t-*` 预留规则；失败时返回 409 `BOT_USERNAME_TAKEN`，ops 干预。

## 不在本期范围

- workflow/init 之外的其他 workflow 接口（cancel / status / complete）
- `team_ns` 物理删除 runbook（90 天保留窗口截止后的 ops 流程）
- bot 凭据 KMS / vault 集成
- 跨租户 team 迁移（更换 git_server）

## 验证命令

```bash
# cs-user
cd cs-user && go build ./... && go test ./internal/user/ ./internal/handlers/

# server
cd server && go build ./...
cd server && go test ./internal/crypto/ ./internal/gitsync/ ./internal/user/ ./internal/teamns/ ./internal/workflow/

# 格式 + 静态检查
cd server && gofmt -l internal/ cmd/
cd server && go vet ./internal/... ./cmd/...
```
