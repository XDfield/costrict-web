# Git Ownership 重构提案：从 cs-user 迁移到 server

| 字段 | 内容 |
|---|---|
| 状态 | Accepted · Implementing（Phase 1-3 完成；Phase 4 additive 完成，destructive 待 e2e；Phase 5 观察期） |
| 作者 | DoSun |
| 创建日期 | 2026-07-22 |
| 评审范围 | server / cs-user / gitea fork / ops |
| 关联文档 | [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md)、[`CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md`](./CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md)、[`../identity-tenant/USER_CENTER_DESIGN.md`](../identity-tenant/USER_CENTER_DESIGN.md)、[`../identity-tenant/CS_USER_SERVICE_DESIGN.md`](../identity-tenant/CS_USER_SERVICE_DESIGN.md)、[`../DATABASE_DESIGN.md`](../DATABASE_DESIGN.md) |
| 实施进度 | [`../../todo/GIT_OWNERSHIP_REFACTOR_PROGRESS.md`](../../todo/GIT_OWNERSHIP_REFACTOR_PROGRESS.md) |

---

## TL;DR

把当前散落在 **cs-user** 的 git 相关能力（`git_servers` 配置、`user_gitea_binding` 绑定、用户级 Gitea 开户状态机）**整体迁移到 server**。cs-user 只保留身份与认证职责（users / user_auth_identities / IdP / JWT 签发），通过 outbox 事件通知 server "用户已创建"，server 消费事件后调用本地 Gitea adapter 完成开户。

**核心动机**：消除两份重复的 Gitea HTTP client（`cs-user/internal/giteasync/client.go` + `server/internal/gitsync/client.go`）、解耦 OAuth 回调关键路径、把所有 git 适配逻辑收拢到 server 一个域。

**实施路径**：分 5 个 phase 推进，每 phase 可独立 ship + 可回滚，预估 4–5 周（单工程师全职）。

---

## 1. 背景与痛点

### 1.1 当前状态（事实清单）

| 维度 | cs-user | server |
|---|---|---|
| `git_servers` 表 | ✅ 拥有 | ❌ 无（team_ns.git_server_id 跨服务 FK） |
| `user_gitea_binding` 表 | ✅ 拥有 | ❌ |
| Gitea HTTP client | `cs-user/internal/giteasync/client.go`（user admin API） | `server/internal/gitsync/client.go`（team-member API） |
| 用户级开户 | ✅（giteasync.Provision，同步触发于 GetOrCreateUser） | ❌ |
| Team / Org / Bot / KB repo 开户 | ❌ | ✅（gitsync + teamns） |
| Tenant → git_server 解析 | 拥有（`gitserver.DBResolver`） | RPC 调用 cs-user（`gitsync/resolver.go:117` `GetTenantGitServer`） |
| User → gitea_username 解析 | 拥有（DB 本地查询） | RPC 调用 cs-user（`user/userref.go:96` `GetGiteaBinding`） |

### 1.2 痛点

1. **职责越界**：cs-user 的领域是"身份"，但它现在承担了"git 开户"，违反 SRP
2. **重复适配层**：两份独立 Gitea HTTP client，协议升级 / fork 中间件改动需要双边同步，漂移风险高
3. **OAuth callback 耦合**：`cs-user/internal/user/service.go:567` 在 `GetOrCreateUser` 内部**同步**调 `giteasync.Provision`，Gitea 慢或挂会直接阻塞登录链路
4. **跨服务 FK**：`server/internal/teamns` 的 `team_ns.git_server_id` 引用 cs-user 的 `git_servers` 表，靠 RPC 解析（多一次网络往返 + 失败重试路径）
5. **配置 locality 不一致**：team namespace / KB repo / bot account 的 Gitea 操作全在 server，唯独 user 开户在 cs-user

### 1.3 已发生的演进

- 2026-07-22：env-based `CS_USER_GITEA_BASE_URL` / `CS_USER_GITEA_ADMIN_TOKEN` 模板 bootstrap 已被移除（commit pending），改为 `git_servers` 表纯 DB 驱动 + tenant 维度绑定。本次提案是该演进的**自然延伸**：不仅配置来自 DB，连表本身都应归属 server。

---

## 2. 目标与非目标

### 2.1 目标

- **G1**：cs-user 不再持有任何 git 概念（无 `git_servers` / `user_gitea_binding` / `tenants.git_server_id`）
- **G2**：server 成为唯一 git adapter，所有 Gitea HTTP 调用集中在 `server/internal/gitsync/`
- **G3**：cs-user 在用户创建时通过 outbox 事件通知 server，**异步**完成 git 开户
- **G4**：OAuth 登录链路的延迟与 Gitea 健康状态解耦（Gitea 故障不影响 auth）
- **G5**：保留 5 阶段灰度能力，每阶段可独立 ship + 可回滚

### 2.2 非目标

- **N1**：不重写 team_ns / KB repo / bot account 的现有 Gitea 流程（已在 server，保持原样）
- **N2**：不引入消息队列基础设施（Kafka / RabbitMQ）— 用 cs-user 库内 outbox 表 + worker 即可
- **N3**：不重构 Casdoor ↔ cs-user ↔ server 三方 JWT 签发链路（属于 identity-tenant 域）
- **N4**：不动 Gitea fork 的 JWT 中间件（属于 capability-registry 域，已稳定）

---

## 3. 静态架构（迁移后）

### 3.1 服务职责边界

```
┌─────────────────────────────────────────────────────────────────┐
│                            cs-user                              │
│  Identity only                                                  │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │ users  ·  user_auth_identities  ·  idp_sources          │    │
│  │ tenants (no git_server_id)  ·  JWT signer               │    │
│  │ user_events (outbox)  ·  outbox worker                  │    │
│  └─────────────────────────────────────────────────────────┘    │
└──────────────────────────┬──────────────────────────────────────┘
                           │ HTTP push (at-least-once, idempotent)
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│                            server                               │
│  Business + git adapter                                         │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │ git_servers  ·  user_gitea_binding  ·  tenants_mirror   │    │
│  │ gitsync/ (user + team + org + bot + KB repo)            │    │
│  │ user_event_consumer (receives cs-user events)           │    │
│  └─────────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────┘
```

### 3.2 数据表归属（迁移后）

| 表 | 归属 | 备注 |
|---|---|---|
| `users` | cs-user | 不变 |
| `user_auth_identities` | cs-user | 不变 |
| `idp_sources` | cs-user | 不变 |
| `tenants` | cs-user | 删除 `git_server_id` 列；其余不变 |
| `git_servers` | **server**（迁移自 cs-user） | |
| `user_gitea_binding` | **server**（迁移自 cs-user） | |
| `user_events`（新） | cs-user | outbox 表，worker 投递后归档 |

### 3.3 RPC 接口变化

| 接口 | 当前 | 迁移后 |
|---|---|---|
| `cs-user → server: POST /api/internal/users/created` | — | **新增**（事件投递端点） |
| `server → cs-user: GetTenantGitServer` | RPC | **删除**（server 本地查询） |
| `server → cs-user: GetGiteaBinding` | RPC | **删除**（server 本地查询） |
| `server → cs-user: GetOrCreateUser / ReissueToken / BindIdentity` | RPC | 不变（identity 域） |

---

## 4. 事件契约

### 4.1 Outbox 表（cs-user 库）

```sql
CREATE TABLE user_events (
    event_id        UUID PRIMARY KEY,
    user_subject_id VARCHAR(191) NOT NULL,
    tenant_id       VARCHAR(64)  NOT NULL,
    event_type      VARCHAR(32)  NOT NULL,  -- user.created | user.updated | user.deleted
    payload         JSONB        NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    delivered_at    TIMESTAMPTZ  NULL,
    delivery_count  INT          NOT NULL DEFAULT 0,
    last_error      TEXT         NULL,
    next_retry_at   TIMESTAMPTZ  NULL
);

CREATE INDEX idx_user_events_undelivered
    ON user_events (next_retry_at)
    WHERE delivered_at IS NULL;
```

**写入语义**：cs-user 在 `GetOrCreateUser` 同事务内 INSERT 一行 `user.created`（事务一致性硬保证）。

### 4.2 投递契约

- **传输**：HTTP POST → `server: /api/internal/users/created`
- **认证**：`X-Internal-Token`（与现有 cs-user ↔ server RPC 一致）
- **语义**：**at-least-once**；server 端必须幂等
- **重试**：exponential backoff（1s, 5s, 30s, 2m, 10m, 30m, 1h, 6h, 24h），24h 后告警
- **ordering**：同一 `user_subject_id` 的多个事件**按 `created_at` 串行投递**（worker 单线程 + 按 user 分区）

### 4.3 Payload 形状（user.created）

```json
{
  "event_id": "uuid-v4",
  "event_type": "user.created",
  "user": {
    "subject_id": "usr_abc123",
    "username": "alice",
    "email": "alice@example.com",
    "display_name": "Alice",
    "tenant_id": "default"
  },
  "occurred_at": "2026-07-22T10:00:00Z"
}
```

### 4.4 Server 端幂等

- 入口：`POST /api/internal/users/created`
- 处理：按 `payload.user.subject_id` 查 `user_gitea_binding`
  - 已存在 `synced` → 直接 ACK，不重复创建
  - 已存在 `pending` → 继续 provision（可恢复中断）
  - 不存在 → INSERT pending + 调用 gitsync.ProvisionUser → 状态机推进

---

## 5. 详细实施计划

### Phase 1 — 基础设施准备（1 周）

**目标**：server 加 git_servers + user_gitea_binding 表与 CRUD；吸收 giteasync 逻辑；**不接事件**，先手动调用验证。

**任务清单**：

| # | 任务 | 文件 |
|---|---|---|
| 1.1 | server 新增 `git_servers` 迁移 + GORM 模型 | `server/migrations/20260722150000_create_git_servers.sql`、`server/internal/models/git_server.go` |
| 1.2 | server 新增 `user_gitea_binding` 迁移 + GORM 模型 | `server/migrations/20260722150010_create_user_gitea_binding.sql`、`server/internal/models/user_gitea_binding.go` |
| 1.3 | server 吸收 giteasync 状态机（迁到 `server/internal/gitsync/user_provision.go`） | `server/internal/gitsync/user_provision.go` |
| 1.4 | server 加 `POST/GET/PUT/DELETE /api/internal/git-servers` CRUD | `server/internal/handlers/git_servers.go` |
| 1.5 | server 加 `PUT /api/internal/tenants/:id/git-server`（绑定） | `server/internal/handlers/tenant_git_server.go` |
| 1.6 | server 启动期 git_servers seed（保留之前的 BootstrapTemplate 函数，作为一次性 CLI / 启动可选） | `server/cmd/api/main.go` |
| 1.7 | 单元测试：giteasync 状态机 + resolver | `server/internal/gitsync/user_provision_test.go` |

**完成标准**：server 单独可通过 RPC 调用 `gitsync.ProvisionUser(ctx, userSubjectID, tenantID)` 完成 Gitea 开户。

### Phase 2 — cs-user 加事件出口（1 周）

**目标**：cs-user 加 outbox 表 + worker；在 `GetOrCreateUser` 事务里写事件；worker 推到 server `/api/internal/users/created`。

**任务清单**：

| # | 任务 | 文件 |
|---|---|---|
| 2.1 | cs-user 新增 `user_events` 迁移 + GORM 模型 | `cs-user/migrations/20260722200000_create_user_events.sql` |
| 2.2 | cs-user 加 `internal/eventbus/outbox.go`（worker loop + 投递器） | `cs-user/internal/eventbus/outbox.go` |
| 2.3 | cs-user 在 `user/service.go::GetOrCreateUser` 同事务写 `user.created` | `cs-user/internal/user/service.go` |
| 2.4 | cs-user 加 server 投递目标配置（`CS_USER_EVENT_TARGET_URL` + token） | `cs-user/internal/config/config.go` |
| 2.5 | server 加 `/api/internal/users/created` 消费端点（先只记日志，不做 provision） | `server/internal/handlers/user_events.go` |
| 2.6 | 端到端测试：cs-user 创建用户 → outbox → server 收到 ACK | `cs-user/internal/eventbus/outbox_test.go` |

**完成标准**：cs-user 创建用户后，server 日志出现 `received user.created subject_id=usr_xxx`。**giteasync hook 仍开启**（双写过渡）。

### Phase 3 — 消费侧接入（0.5 周）

**目标**：server 收到事件后真正调用 `gitsync.ProvisionUser`；cs-user 关掉原 giteasync hook（保留代码做灰度回滚）。

**任务清单**：

| # | 任务 | 文件 |
|---|---|---|
| 3.1 | server `/api/internal/users/created` 端点改为真正调 `gitsync.ProvisionUser` | `server/internal/handlers/user_events.go` |
| 3.2 | cs-user 加 feature flag `CS_USER_GITEA_SYNC_LEGACY=true/false`（默认 true 灰度，验证后切 false） | `cs-user/internal/config/config.go` |
| 3.3 | cs-user `main.go::SetGiteaSync` 改为按 flag 决定是否挂 | `cs-user/cmd/api/main.go` |
| 3.4 | 端到端测试：登录 → server 收到事件 → server 完成 Gitea 开户 → `user_gitea_binding` 写入 server 库 | 手动验证 |

**完成标准**：登录后 server 库 `user_gitea_binding` 出现新行；cs-user 库 `user_gitea_binding` 不再增长（flag=false 时）。

### Phase 4 — 数据迁移与清理（1 周）

**目标**：把 cs-user 库存量搬到 server 库；删 cs-user 端的 git 代码与表。

**任务清单**：

| # | 任务 | 文件 |
|---|---|---|
| 4.1 | 写一次性迁移脚本（dump cs-user.git_servers → server.git_servers；dump cs-user.user_gitea_binding → server.user_gitea_binding） | `server/cmd/migrate-git-from-cs-user/main.go` |
| 4.2 | server 的 `teamns.git_server_id` FK 改为引用本地 `git_servers` 表 | `server/migrations/20260722210000_alter_teamns_git_server_fk.sql` |
| 4.3 | server 的 `gitsync/resolver.go::Resolve` 从 RPC 改为本地查询 | `server/internal/gitsync/resolver.go` |
| 4.4 | server 的 `user/userref.go::Resolve` 中 gitea_username 改本地查询 | `server/internal/user/userref.go` |
| 4.5 | 删 cs-user 端：`internal/giteasync/`、`internal/gitserver/`、`internal/handlers/users_gitea_binding.go`、`internal/handlers/tenant_git_server.go` | 整目录删除 |
| 4.6 | 删 cs-user 端：`models/git_server.go`、`models/user_gitea_binding.go` | 文件删除 |
| 4.7 | cs-user `tenants` 表 DROP `git_server_id` 列 | `cs-user/migrations/20260722220000_drop_git_server_id_from_tenants.sql` |
| 4.8 | 删 cs-user 端 RPC：`GetTenantGitServer`、`GetGiteaBinding` | `cs-user/internal/handlers/`、`server/internal/user/rpc_reader.go` |
| 4.9 | 验证：所有 git 操作在 server 单边完成；cs-user 路径无残留 | 手动 e2e |

**完成标准**：cs-user 代码库 grep `gitea` / `git_server` 应只剩注释或历史 migration 文件。

### Phase 5 — 稳定性观察（1 周）

**目标**：双写窗口观察，确认无 dangling 状态后删灰度开关。

**任务清单**：

| # | 任务 | 文件 |
|---|---|---|
| 5.1 | 观察 server 端 `user_gitea_binding.sync_status` 分布、事件投递失败率、登录延迟 | — |
| 5.2 | 删 cs-user `CS_USER_GITEA_SYNC_LEGACY` flag + 相关代码路径 | `cs-user/internal/config/config.go`、`cs-user/cmd/api/main.go` |
| 5.3 | 更新 `REPOSITORY_MANAGEMENT_SPEC.md` 反映新架构 | `docs/repo-management/REPOSITORY_MANAGEMENT_SPEC.md` |
| 5.4 | 更新 `CS_USER_SERVICE_DESIGN.md` 移除 git 章节 | `docs/identity-tenant/CS_USER_SERVICE_DESIGN.md` |

**完成标准**：稳定运行 7 天无回滚事件，提案 status 从 Draft → Accepted · Implemented。

---

## 6. 关键决策记录

### D1：事件出口机制选 Outbox 而非 MQ

**选**：cs-user 库内 outbox 表 + worker

**理由**：
- 不引入新基础设施（无 Kafka / RabbitMQ 依赖）
- 事务一致性硬保证（user INSERT 与 outbox INSERT 同事务）
- 本 dev 环境友好（无需起 broker）
- 已有等价模式可参考（server 的 audit_log + leader-based cleanup worker）

**代价**：worker 逻辑自实现（重试、ordering、idempotency），但代码量 < 400 LOC 可控。

### D2：`git_servers` 表完全搬到 server，不复制

**选**：完全迁移到 server，cs-user 不保留

**理由**：
- 避免双源真理
- `tenants.git_server_id` 列从 cs-user 删除（cs-user 不再需要知道 tenant 用哪个 git）
- server 端需要新建 tenant 镜像表（`tenants_mirror`）承载绑定关系

### D3：`tenants` 表归属不动

**选**：`tenants` 表主体仍在 cs-user（用户身份与租户归属紧密），仅删除 `git_server_id` 列

**替代方案**（未采纳）：把 `tenants` 整表搬到 server — 代价过大（cs-user 大量代码引用 tenants），收益不明确。

### D4：事件 at-least-once + server 端 idempotent

**选**：at-least-once 投递 + server 按 `subject_id` 幂等

**理由**：
- exactly-once 需要分布式事务或两阶段提交，复杂度过高
- server 端 giteasync 状态机本身就是幂等的（pending → synced，重复触发不会创建第二个 Gitea 账号）

### D5：5 phase 推进，每 phase 可独立 ship

**选**：渐进式灰度，避免大爆炸

**理由**：
- cs-user ↔ server 是关键链路，一次大爆炸失败回滚成本不可接受
- 每 phase 留下灰度开关，验证后再切下一 phase

---

## 7. 风险与缓解

| # | 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|---|
| R1 | outbox worker 在 cs-user 单点故障期间不投递，server 拿不到事件 | 中 | 中（新用户无法 git 操作） | worker 多副本 + leader election（参考 server 已有 leader 模式）；监控 `user_events.delivered_at IS NULL AND created_at < NOW() - 5min` |
| R2 | 事件 ordering 错乱（user.deleted 早于 user.created 投递） | 低 | 中 | worker 按 `user_subject_id` hash 分区串行投递；server 端按 event_id 去重 |
| R3 | 迁移期 cs-user 和 server 两边 `user_gitea_binding` 都写入，产生数据不一致 | 中 | 低 | P3 用 flag 控制；P4 迁移前先停 cs-user hook；迁移脚本做 `subject_id` 比对，不一致告警 |
| R4 | cs-user 删 `tenants.git_server_id` 后，遗留 cs-user 代码引用该字段编译失败 | 低 | 低 | P4.7 之前完成所有引用清理；编译验证 + 测试覆盖 |
| R5 | Gitea fork 的 JWT 中间件依赖 `user_gitea_binding` 在 cs-user 库 | 低 | 高（capability-registry 链路断） | P4 之前确认 fork 中间件走的是 server 的 binding 表（已是 server 库的本地查询） |
| R6 | dev 环境缺 Gitea，难以验证端到端 provision 流程 | 中 | 低 | P1 起引入 `docker-compose.yml` 起 Gitea；CI 加 Gitea sidecar |

---

## 8. 成功指标

迁移完成后 4 周内观察：

| 指标 | 目标 |
|---|---|
| cs-user OAuth callback P99 延迟 | 较迁移前下降 ≥ 30%（giteasync 同步调用摘除） |
| `user.created` 事件投递成功率 | ≥ 99.95% |
| server 端 `user_gitea_binding` 从事件到 synced 的中位延迟 | ≤ 2s |
| cs-user 代码库 grep `gitea` / `git_server` 命中 | 仅历史 migration 与注释 |
| git 相关 RPC 调用数（server → cs-user） | 降至 0 |

---

## 9. 替代方案（已考虑未采纳）

### A1：保留 cs-user 的 giteasync，只把 git_servers 表搬到 server

**否决理由**：跨服务表查询需要 RPC，反而增加耦合；不解决 OAuth callback 同步阻塞问题。

### A2：cs-user 和 server 都保留 git 适配层，但用 gRPC 双向流替代 HTTP RPC

**否决理由**：技术换汤不换药，复杂度提升明显但收益模糊。

### A3：把 gitea fork 的 user 开户也合并到 capability-registry 同步链路

**否决理由**：capability-registry 是单向（Gitea → server webhook），user 开户是反向（cs-user → Gitea），合并语义混乱。

---

## 10. 参考文献

- [Outbox Pattern](https://microservices.io/patterns/data/transactional-outbox.html)
- [`docs/identity-tenant/USER_CENTER_DESIGN.md`](../identity-tenant/USER_CENTER_DESIGN.md) §11.2（原 giteasync eager 设计出处）
- [`docs/identity-tenant/CS_USER_SERVICE_DESIGN.md`](../identity-tenant/CS_USER_SERVICE_DESIGN.md) §170（GiteaUserSyncWorker 设计）
- [`docs/repo-management/REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md)
- [`docs/repo-management/CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md`](./CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md)
