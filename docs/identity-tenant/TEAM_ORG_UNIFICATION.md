# Team / Org 概念统一提案（Layer 2）

| 字段 | 内容 |
|---|---|
| 状态 | Draft · 评审中（v3，team 同步职责反转） |
| 作者 | CoStrict 团队 |
| 创建日期 | 2026-07-14 |
| 最近修订 | 2026-07-15（v3：ADR-3 反转——team 同步从 cs-user 改归 @server；cs-user 仅保留 user-level Gitea 开户） |
| 评审范围 | costrict-web（server / gateway）/ 外部 org-team-service 模块（其他同事负责，提供 webhook + 查询 API）/ cs-user（user-level Gitea 协作）/ Casdoor / Gitea（只读） / admin 后台 |
| 关联文档 | [`ITEM_DISTRIBUTION_DESIGN.md`](../proposals/ITEM_DISTRIBUTION_DESIGN.md)（§3.3 / §3.1 / §6.1 / §8.3 / §9.3 待对齐）、[`MULTI_TENANCY_DESIGN.md`](./MULTI_TENANCY_DESIGN.md)、[`CS_USER_SERVICE_DESIGN.md`](./CS_USER_SERVICE_DESIGN.md) |

> **架构方向调整（2026-07-15 v2）**：经评审确认，组织架构与团队机制由其他同事负责的**外部独立模块 `org-team-service`** 统一管理。本提案**不再**在 costrict-web 内引入 `teams` / `team_members` 持久化表，改为通过 **OrgService 接口 + Webhook 事件** 对接外部模块。原 v1 设计（本地表 + dept-sync 客户端 DB 持久化）撤销，详见 ADR-1 / ADR-7 / ADR-8 / ADR-9 的"撤销"说明。

> **ADR-3 反转（2026-07-15 v3）**：Gitea team 同步职责从原计划的 cs-user **反转到 @server**。cs-user 仅保留 user-level 工作（Gitea user 自动开户 + `user_gitea_binding` 维护），team-level `team_user` 同步统一归集到 @server。理由：职责内聚（@server 已承担业务侧 Gitea 协作）+ GitServerAdapter 复用 + cs-user 保持纯粹 + 故障隔离。详见 ADR-3。

---

## TL;DR

**五个核心决策**：

1. **业务侧统一通过 OrgService 接口对接外部 `org-team-service` 模块**。组织架构 + 团队成员的真相源是外部模块，costrict-web **不本地持久化**——所有业务方（distribution / admin / login）通过 in-process OrgService 接口实时查询，由 OrgService 内部封装 HTTP 调用、缓存、降级。

2. **`teams` / `team_members` 持久化表撤销**。原 v1 设计（引入两张本地表作为唯一真相源）整体撤销。`teams.team_type` 共表方案、`role` 字段简化、`tenant_id` 预留讨论全部失效——这些都是本地持久化才需要决定的设计点。

3. **Casdoor 降级为纯登录认证源**（不变）。登录时只 upsert `users` 表的 profile 字段（邮箱 / 姓名 / 头像），**完全不读不写** team / org 归属。Casdoor 的 `organization` 字段被永久忽略——multi-tenant 边界从此与 Casdoor 解耦。

4. **Webhook 事件用于缓存失效 + 下游推送，不持久化**。外部模块推送组织 / 团队变更事件，costrict-web 收到后：(a) 失效 OrgService 内的 in-process 缓存；(b) 触发必要的下游通知（如 metrics）。**不写本地表**。

5. **Distribution scope 永久废弃 `organization`，统一为 `user | department`**（不变）。这**对齐代码现状**——`distribution_service.go:317-330` 的 switch 只有 `case "department"`，`test:626` 主动断言 organization 返回 `ErrUnsupportedScope`。`department` scope 的数据源从 dept-sync 客户端切换为 OrgService 接口。

**一句话价值**：业务侧不再面对"该读哪一套 team 数据"的困惑；组织架构 / 团队机制的复杂度（CRUD、生命周期、跨租户）由专门的外部模块负责，costrict-web 只消费；distribution / admin / login 等业务侧统一通过 OrgService 接口解耦。

---

## 目录

```
Part I：动机与目标
  1. 背景：4 套 team/org 概念并存
  2. 目标与非目标

Part II：对接架构（消费外部模块）
  3. OrgService 接口设计
  4. Webhook 事件处理
  5. In-process 缓存策略
  6. 真相源结论

Part III：与现有系统的关系
  7. Casdoor（降级为登录认证源）
  8. dept-sync 客户端（废弃，被 OrgService 替代）
  9. Gitea team（不接入）
  10. ITEM_DISTRIBUTION_DESIGN §3.3 Organization 表（不在本地实现）

Part IV：Distribution scope 升级
  11. 当前问题（文档 vs 代码不一致）
  12. Layer 2 改动
  13. 文档对齐清单

Part V：迁移路径（3 阶段）
  14. Stage A：OrgService 接口 + Webhook endpoint
  15. Stage B：业务切到 OrgService
  16. Stage C：登录流程的游离用户处理 + admin unassigned 视图

Part VI：影响面
  17. 代码改动清单
  18. 新增文件
  19. 不变项

Part VII：决策记录（ADR）
  ADR-1：~~teams 表 team_type 共表~~（v2 撤销）
  ADR-2：Casdoor 角色（纯认证源）
  ADR-3：本提案不直接接入 Gitea team；Gitea team 同步由 cs-user 负责
  ADR-4：multi-tenant 限制解除（边界由外部模块决定）
  ADR-5：organization scope 永久废弃
  ADR-6：新用户"无 org 状态"处理（依赖外部模块）
  ADR-7：~~dept-sync 真实架构~~（v2 撤销，保留作历史背景）
  ADR-8：~~team_members.role 简化~~（v2 撤销）
  ADR-9：~~teams 表不预留 tenant_id~~（v2 撤销）
  ADR-10：消费外部 org-team-service 模块（不本地持久化）
  ADR-11：OrgService 接口设计（in-process 抽象）
  ADR-12：Webhook 事件处理（仅失效缓存，不持久化）
  ADR-13：In-process 缓存策略

Part VIII：消费方需求清单（A 节已闭环）+ 实现侧开放问题（B 节待 API 定稿）
```

---

# Part I：动机与目标

## 1. 背景：4 套 team/org 概念并存

### 1.1 现状盘点

costrict-web 平台当前对"团队 / 组织"概念有 **4 套并存的数据表达**，互不通气：

| # | 概念 | 真相源 | 表达方式 | 实际用途 | 维护方 |
|---|---|---|---|---|---|
| 1 | Casdoor organization | Casdoor `owner` 字段 | `users.organization`（单值字符串） | admin 后台筛选 / 报表元数据（本提案后废弃，详见 ADR-2） | Casdoor 同步 |
| 2 | Department | 外部 `costrict-dept-info` 服务（详见 ADR-7 历史） | dept-sync 客户端 60 秒**内存缓存** + 部门树 path | distribution scope + admin 树展示 | dept-sync 客户端（`server/internal/deptsync/`）按需查询 |
| 3 | Gitea team | Gitea `team_user` 表 | Gitea 内部表 | fork repo 权限 | 当前手动管理，未来由 **@server** 通过 GitServerAdapter 同步（见 ADR-3；`server/internal/gitsync/`） |
| 4 | Organization 实体 | [`ITEM_DISTRIBUTION_DESIGN.md`](../proposals/ITEM_DISTRIBUTION_DESIGN.md) §3.3 规划 | `organizations` 表（**未实施**） | 文档预留抽象 | 无（仅设计稿） |

### 1.2 痛点

| 痛点 | 具体表现 |
|---|---|
| **同一问题多套答案** | "用户 U 属于哪些团队 / 组织"——业务方不知道该读 Casdoor / dept-sync 客户端 / Gitea 哪一个 |
| **真相源不可靠** | Casdoor `users.organization` 是字符串（不可靠）；dept-sync 是按需 HTTP 查询（无持久化，外部服务故障即查询失败）；Gitea `team_user` 是外部系统（不可本地 JOIN） |
| **业务侧耦合外部系统** | `distribution_service.go:317-330` 的 `case "department":` 通过 dept-sync 客户端实时调用 `costrict-dept-info`；外部服务故障 = distribution 故障 |
| **文档与代码漂移** | [`ITEM_DISTRIBUTION_DESIGN.md`](../proposals/ITEM_DISTRIBUTION_DESIGN.md) §3.1 说支持 `organization` scope，但代码 `distribution_service.go:87` 的错误信息明文 `scope must be user or department`，`test:626` 主动断言 organization 拒绝——文档未同步 |
| **未实施的设计成为障碍** | §3.3 的 `Organization` 表设计存在但没建表；新业务想引用 organization 实体时无表可用 |

### 1.3 触发本提案的核心驱动

业务方提出"平台需要有这么一个团队、组织概念机制"的诉求——不只是 distribution 用、不只是 fork 权限用、不只是 admin 报表用，而是**平台整体**需要一个清晰的 team/org 抽象。

**架构调整后的解法（v2）**：组织架构 + 团队机制的复杂度（CRUD、生命周期、跨租户、权限边界）由**专门的外部独立模块 `org-team-service`**（其他同事负责）承担。costrict-web 不再自己维护本地表，而是通过 OrgService 接口 + Webhook 事件对接。这样：

- costrict-web 专注于业务逻辑，不重复实现组织/团队管理
- 外部模块由专门团队演进，可独立部署 / 升级
- multi-tenancy 等复杂机制在外部模块内闭环，本提案不直接处理

---

## 2. 目标与非目标

### 2.1 目标

| 编号 | 目标 | 完成标准 |
|---|---|---|
| G1 | **业务侧统一通过 OrgService 接口** | distribution / admin / login 等业务方不再直接调用 dept-sync 客户端 / Casdoor org 字段，统一通过 `OrgService` 接口 |
| G2 | **Casdoor 降级为纯认证源** | 登录流程只刷新 `users` 表 profile 字段；不读不写 team / org 归属 |
| G3 | **接入外部模块 webhook** | Webhook endpoint 收到事件后失效 in-process 缓存；不持久化 |
| G4 | **distribution scope 对齐代码现状** | `ITEM_DISTRIBUTION_DESIGN.md` §3.1 / §6.1 / §8.3 / §11.1 文档更新，organization scope 永久废弃 |
| G5 | **现有 dept-sync 客户端废弃** | `server/internal/deptsync/` 包整体下线（或保留作兼容期 fallback） |

### 2.2 非目标

| 编号 | 非目标 | 理由 |
|---|---|---|
| NG1 | 在 costrict-web 内引入 teams / team_members 持久化表 | 由外部 `org-team-service` 模块负责 |
| NG2 | 实现组织 / 团队的 CRUD 接口 | 同上，由外部模块提供 |
| NG3 | 处理跨租户的 team 边界 | 由外部模块 + [`MULTI_TENANCY_DESIGN.md`](./MULTI_TENANCY_DESIGN.md) 闭环 |
| NG4 | Gitea team 反向同步（Gitea → @server） | 维持现状（ADR-3 仅定义正向 @server → Gitea） |
| NG5 | 接入 Casdoor webhook | Casdoor 降级为纯认证源（ADR-2） |

---

# Part II：对接架构（消费外部模块）

## 3. OrgService 接口设计

### 3.1 设计目标

`OrgService` 是 costrict-web 内的 in-process 抽象层，封装对外部 `org-team-service` 模块的调用。所有业务方通过此接口获取组织 / 团队数据，**不直接**调用 HTTP。

### 3.2 接口定义（Go 伪代码）

```go
// server/internal/orgservice/service.go

type Service interface {
    // 部门树（admin 后台组织树展示）
    GetDepartmentTree(ctx context.Context) (*DeptTree, error)

    // 部门成员（distribution scope='department'）
    ListDepartmentMembers(ctx context.Context, deptID string) ([]Member, error)

    // 用户所属部门 / 团队（登录触发 + admin 用户详情）
    ListUserTeams(ctx context.Context, userID string) ([]TeamMembership, error)

    // 子树成员（distribution scope='department' 带 include_children）
    ListSubtreeMembers(ctx context.Context, rootDeptID string) ([]Member, error)

    // 游离用户列表（admin 后台"未归队用户"视图）
    ListUnassignedUsers(ctx context.Context, pagination Params) ([]User, Total, error)
}

type Member struct {
    UserID    string
    Role      string  // 来自外部模块的 role 语义（"leader" / "member" 等）
    IsLeader  bool    // 拓扑 leader（运行时基于 path 计算，由外部模块提供或本地计算）
}

type TeamMembership struct {
    TeamID    string
    TeamType  string  // "organization" / "department" / "team"
    TeamPath  string
    Role      string
}
```

### 3.3 实现层

| 模块 | 职责 |
|---|---|
| `orgservice.HttpClient` | HTTP 调用 `org-team-service`（认证 / 重试 / 超时） |
| `orgservice.Cache` | in-process 缓存（短 TTL，详见 §5） |
| `orgservice.ServiceImpl` | 业务逻辑组合（缓存 → HTTP → 降级） |

### 3.4 降级策略（外部模块故障时）

| 场景 | 策略 |
|---|---|
| 外部模块 5xx 错误 | 返回 stale cached data（如有）+ log warning；如无缓存返回 503 |
| 外部模块超时（>3s） | 同上 |
| 外部模块部署维护期 | 主动返回 503 + Retry-After header |
| 网络分区 | 同 5xx 错误 |

业务侧需要容忍 `ErrOrgServiceUnavailable`——distribution scope='department' 在降级时**跳过该次下发**（不影响 user scope），admin 后台展示错误占位符。

---

## 4. Webhook 事件处理

### 4.1 Endpoint

```
POST /api/webhooks/org-team/events
Headers:
  X-OrgTeam-Signature: <hmac-sha256>
  X-OrgTeam-Event-Id: <uuid>
  X-OrgTeam-Event-Type: <type>
Body: JSON payload
```

### 4.2 事件类型（v2 草案，待外部模块确认）

| Type | Payload 示例 | 处理动作 |
|---|---|---|
| `team.created` | `{team_id, team_type, parent_id, path, name}` | 失效部门树缓存 |
| `team.updated` | `{team_id, changes: {...}}` | 失效部门树缓存 |
| `team.deleted` | `{team_id}` | 失效部门树缓存 |
| `member.added` | `{team_id, user_id, role}` | 失效该 team 成员缓存 + 该 user 的 team 列表缓存 |
| `member.removed` | `{team_id, user_id}` | 同上 |
| `member.role_changed` | `{team_id, user_id, old_role, new_role}` | 同上 |

### 4.3 处理逻辑

```go
func (h *WebhookHandler) HandleEvent(ctx context.Context, event Event) error {
    // 1. 验签（HMAC-SHA256）
    if err := h.verifySignature(event); err != nil {
        return err
    }

    // 2. 幂等检查（基于 event_id，内存去重 LRU 1 小时）
    if h.deduper.Seen(event.ID) {
        return nil  // 重复事件，忽略
    }

    // 3. 失效缓存（不持久化）
    h.cache.InvalidateByEvent(event)

    // 4. 触发下游通知（metrics / 可选 log）
    h.metrics.IncEventProcessed(event.Type)

    return nil
}
```

### 4.4 错过事件的补偿

由于本提案不持久化，"错过 webhook"对本地无状态影响。但如果 in-process 缓存有 stale 数据，需要补偿：

- **方案**：OrgService 客户端维护 `last_event_id` watermark，定期（每 5 分钟）调用外部模块的"事件回放"接口，补全缺失事件
- **替代方案**：依赖缓存的短 TTL（30 秒）自然回收——简单但容忍 staleness
- 待外部模块接口契约确认后选定（详见 Part VIII 开放问题）

### 4.5 webhook 事件类型（按消费方分组）

不同消费方对事件粒度的需求不同——业务侧（OrgService）关注**缓存失效**，@server 关注**Gitea 侧同步**。下表用消费方分组重列事件类型，补充 §4.2 草案缺失的 3 个 bulk / 生命周期事件。

| Type | 消费方 | 用途 | 转发的 @server 内部接口 |
|---|---|---|---|
| `team.created` | OrgService（缓存失效） | 部门树变更 | — |
| `team.updated` | OrgService | 部门树变更 | — |
| `team.deleted` | OrgService | 部门树变更 | — |
| `team.members.changed` | **@server** | team 成员批量变更（delta 模式） | `POST /api/internal/teams/:team_id/members:sync` `mode=delta` |
| `team.members.reconcile` | **@server** | 全量对账（周期性 / 重启后 / 异常恢复） | `POST /api/internal/teams/:team_id/members:sync` `mode=full_sync` |
| `team.dissolved` | **@server** | team 解散 → archive team ns | `POST /api/internal/teams/:team_id/dissolve` |
| `member.added` | OrgService | 单成员加入 → 缓存失效（@server 不直接消费，由 `team.members.changed` 覆盖） | — |
| `member.removed` | OrgService | 单成员移除 → 缓存失效（同上） | — |
| `member.role_changed` | OrgService | 单成员角色变更 → 缓存失效 | — |

> **粒度约定**：单成员事件（`member.*`）由外部模块按需推送，OrgService 消费做缓存失效；批量同步事件（`team.members.changed` / `team.members.reconcile`）由外部模块在 batch 提交 / 全量对账时推送，@server 消费做 Gitea 侧同步。**@server 不监听 `member.*` 事件**，避免与 bulk 事件重复触发 Gitea API。

### 4.6 webhook payload schema（v2 草案）

#### 4.6.1 信封结构（所有事件通用）

```json
{
  "event_id": "evt_<uuid>",
  "event_type": "team.members.changed",
  "occurred_at": "2026-07-15T10:23:45Z",
  "version": "1.0",
  "source": "org-team-service",
  "tenant_id": "t_acme",
  "subject": { /* 事件特定字段，见 §4.6.2-4.6.4 */ },
  "data": { /* 事件特定字段 */ },
  "signature": "<HMAC-SHA256 over (event_id + occurred_at + subject + data), key = webhook_secret_ref 解出的共享密钥>"
}
```

签名头：

```
X-OrgTeam-Signature: <hex(hmac-sha256)>
X-OrgTeam-Event-Id: evt_<uuid>
X-OrgTeam-Event-Type: team.members.changed
```

#### 4.6.2 `team.members.changed`（delta 批量变更）

```json
{
  "event_type": "team.members.changed",
  "subject": {
    "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
    "team_display_name": "Platform Team"
  },
  "data": {
    "add_members": [
      { "user_id": "u_abc123def456", "gitea_username": "alice" },
      { "user_id": "u_def789abc012", "gitea_username": "bob" }
    ],
    "remove_members": [
      { "user_id": "u_aaa111bbb222", "gitea_username": "charlie" }
    ]
  }
}
```

@server 收到后原样转换 body 为 `POST /api/internal/teams/:team_id/members:sync` (`mode=delta`)。

#### 4.6.3 `team.members.reconcile`（全量对账）

```json
{
  "event_type": "team.members.reconcile",
  "subject": {
    "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
    "team_display_name": "Platform Team"
  },
  "data": {
    "members": [
      { "user_id": "u_abc123def456", "gitea_username": "alice" },
      { "user_id": "u_def789abc012", "gitea_username": "bob" }
    ],
    "trigger": "scheduled"   // "scheduled" | "manual" | "post_outage"
  }
}
```

@server 收到后调 `POST /api/internal/teams/:team_id/members:sync` (`mode=full_sync`)，把 `members[]` 转成 `add_members`，`remove_members` 留空——内部接口在 full_sync 模式下自动 diff 当前 Gitea org 成员，把不在 `members[]` 中的全部 remove。

#### 4.6.4 `team.dissolved`

```json
{
  "event_type": "team.dissolved",
  "subject": {
    "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a"
  },
  "data": {
    "dissolved_at": "2026-07-15T10:23:45Z",
    "reason": "merged_into:7f3c9a1e-...",   // 可选：合并到另一个 team / 业务下线 / admin 手工
    "retention_days": 90                     // Gitea org archived 后保留时长（@server 默认 90）
  }
}
```

@server 收到后调 `POST /api/internal/teams/:team_id/dissolve`，body 透传 `retention_days` 与 `reason`。详见 [`TEAM_NAMESPACE_API.md`](../repo-management/TEAM_NAMESPACE_API.md) §3。

### 4.7 消费方契约对齐

| 消费方 | 监听事件 | 操作 |
|---|---|---|
| **OrgService**（costrict-web 业务侧） | `team.*` / `member.*` | 失效 in-process 缓存（不持久化） |
| **@server**（Gitea 侧镜像） | `team.members.changed` / `team.members.reconcile` / `team.dissolved` | 转调 [`TEAM_NAMESPACE_API.md`](../repo-management/TEAM_NAMESPACE_API.md) §2 / §3 的内部接口；team ns lazy 创建 + 成员同步 + archive |
| **distribution worker** | `team.*` / `member.*` | 失效 distribution scope 缓存（依赖 OrgService，不直接订阅） |

@server 监听的 3 个事件**不依赖 OrgService 缓存**——直接消费 webhook 后转调 Gitea，确保 Gitea 侧状态与 org-team-service 一致；OrgService 与 @server 是 org-team-service 的两个独立消费方，互不依赖。

---

## 5. In-process 缓存策略

### 5.1 缓存层级

| 缓存层 | TTL | 失效触发 | 用途 |
|---|---|---|---|
| L1：goroutine 本地（sync.Map） | 5 秒 | TTL 过期 | 高频读（distribution scope 解析） |
| L2：进程级（sync.Map 或 LRU） | 30 秒 | TTL / Webhook 主动失效 | 一般读（admin 后台组织树） |

### 5.2 缓存键

```
orgservice:dept_tree                     # 整棵部门树
orgservice:dept_members:{dept_id}        # 某部门成员列表
orgservice:subtree_members:{dept_id}     # 子树成员列表
orgservice:user_teams:{user_id}          # 用户所属团队
orgservice:unassigned_users:{page_hash}  # 游离用户列表
```

### 5.3 缓存失效规则（基于 webhook 事件）

| 事件 | 失效缓存 |
|---|---|
| `team.created` / `team.updated` / `team.deleted` | `dept_tree`（整树失效，简化处理） |
| `member.added` / `member.removed` / `member.role_changed` | `dept_members:{team_id}` + `subtree_members:{team_id}` + 所有祖先部门的 `subtree_members` + `user_teams:{user_id}` |

为简化实现，`team.*` 事件触发**全树失效**（实际场景变更频率低，可接受）。

---

## 6. 真相源结论

本提案实施完成后，平台各类数据的真相源如下表所示。**业务侧通过 OrgService 接口获取**，由 OrgService 内部决定走缓存还是 HTTP。

| 数据类别 | 真相源 | 业务侧访问方式 | 备注 |
|---|---|---|---|
| User profile（邮箱 / 姓名 / 头像） | `users` 表 | 本地 GORM | Casdoor 登录时刷新 |
| User 归属哪些 team / department | 外部 `org-team-service` 模块 | `OrgService.ListUserTeams()` | Casdoor 不再参与 |
| Team / department 结构本身 | 外部 `org-team-service` 模块 | `OrgService.GetDepartmentTree()` | §3.3 Organization 表合并到外部模块实现 |
| Distribution scope='department' 解析 | 外部 `org-team-service` 模块 | `OrgService.ListDepartmentMembers()` | 不再读 dept-sync 客户端 |
| Fork repo 权限 | Gitea `team_user` 表 | Gitea API | `team_user` 写入由 **@server** 负责（ADR-3；fork middleware 仍直接读 Gitea API） |
| Admin 后台组织树展示 | 外部 `org-team-service` 模块 | `OrgService.GetDepartmentTree()` | 不再读 dept-sync 客户端 |

---

# Part III：与现有系统的关系

## 7. Casdoor（降级为登录认证源）

### 7.1 当前角色

- **认证**：OIDC / OAuth 登录入口
- **元数据**：`users.organization` 字段（单值字符串），被 admin 后台 / 报表读取

### 7.2 Layer 2 改动

- **保留**：OIDC / OAuth 认证（不变）
- **保留**：登录时 upsert `users` 表的 profile 字段（邮箱 / 姓名 / 头像）
- **删除**：业务侧对 `users.organization` 字符串的依赖（admin 后台 / 报表改读 OrgService）

### 7.3 影响

Casdoor 的 `organization` 字段在本提案后变成"历史遗留字段"——继续存在但不被任何业务读取。

详见 **ADR-2**。

---

## 8. dept-sync 客户端（废弃）

### 8.1 当前形态

`server/internal/deptsync/` 是 HTTP 客户端，按需调用外部 `costrict-dept-info` 服务（60 秒内存缓存，无 Redis、无定时同步）。详见历史版本 ADR-7 的调研记录。

### 8.2 Layer 2 改动

**整体废弃**——dept-sync 客户端的所有职责被 OrgService 接口替代：

| 原 dept-sync 客户端职责 | 新归属 |
|---|---|
| 部门树查询 | `OrgService.GetDepartmentTree()` |
| 部门成员查询 | `OrgService.ListDepartmentMembers()` |
| 用户部门查询 | `OrgService.ListUserTeams()` |
| 60 秒内存缓存 | OrgService 内的 L1 + L2 缓存（详见 §5） |

### 8.3 迁移路径

| Stage | dept-sync 客户端状态 |
|---|---|
| A | 保留运行（业务侧仍依赖） |
| B | 业务侧切到 OrgService；dept-sync 客户端不再被调用（保留代码作为 fallback） |
| C | 删除 `server/internal/deptsync/` 包 + 配置项（`DEPT_SYNC_*`） |

### 8.4 注：`costrict-dept-info` 外部服务的命运

`costrict-dept-info` 服务由其他团队维护。本提案**不直接决定**其命运——可能是 `org-team-service` 模块的前身 / 一部分 / 被替代。costrict-web 只关心：废弃 dept-sync 客户端后，所有数据查询走 OrgService → 外部 `org-team-service`。

---

## 9. Gitea team（不接入）

### 9.1 当前角色

Gitea 内部 `team_user` 表用于 fork repo 权限。当前手动管理；[`ITEM_DISTRIBUTION_DESIGN.md`](../proposals/ITEM_DISTRIBUTION_DESIGN.md) §9.3 设想 OrgGiteaSyncWorker 通过 Gitea admin API 维护 team 成员——**未实施**。

### 9.2 Layer 2 决策（v2 调整）

**本提案不直接接入 Gitea team**；Gitea team 同步由 **@server 模块**负责（详见 ADR-3；实施位置 `server/internal/gitsync/`）。cs-user 仅保留 user-level 工作（自动开户 + `user_gitea_binding` 维护），不掺入 team-level 同步。

@server 作为 org-team-service 的另一个消费方：
- 收到 `member.added` 事件 → 通过 GitServerAdapter 调用 Gitea admin API 添加该用户到对应 team
- 收到 `member.removed` 事件 → 同步删除
- 实时事件驱动，@server 是真相源（单向写入 Gitea）

### 9.3 fork 权限维持现状

- fork 中间件继续通过 Gitea 查 `team_user` 表（**不变**）
- Gitea `team_user` 表的**写入权**由 @server 持有（通过 GitServerAdapter）
- "用户在哪些 Gitea team" 查询走 Gitea API（低频管理操作，可接受延迟）

---

## 10. ITEM_DISTRIBUTION_DESIGN §3.3 `Organization` 表（不在本地实现）

### 10.1 现状

[`ITEM_DISTRIBUTION_DESIGN.md`](../proposals/ITEM_DISTRIBUTION_DESIGN.md) §3.3 规划了 `Organization` 表，但**未实施**（`server/cmd/migrate/main.go` 未执行 backfill，AutoMigrate 未包含）。

### 10.2 Layer 2 处理

- §3.3 的 `Organization` 表设计**整体转移到外部 `org-team-service` 模块**实现
- costrict-web 内**不**创建该表（既不在本地建，也不在 OrgService 内建——OrgService 只消费）
- [`ITEM_DISTRIBUTION_DESIGN.md`](../proposals/ITEM_DISTRIBUTION_DESIGN.md) §3.3、§9.2、§10.1、§10.2、§11.3 章节**全部重写**或删除（详见 §13 文档对齐清单）

### 10.3 不需要数据迁移

由于 §3.3 表从未实施，且本提案不在本地建表，**不需要**任何数据迁移。

---

# Part IV：Distribution scope 升级

## 11. 当前问题（文档 vs 代码不一致）

[`ITEM_DISTRIBUTION_DESIGN.md`](../proposals/ITEM_DISTRIBUTION_DESIGN.md) 与代码在 organization scope 上的矛盾：

| 来源 | 内容 | 行为 |
|---|---|---|
| §3.1 数据模型 | `ScopeType: user \| organization \| department（预留）\| role（预留）` | 文档说支持 organization |
| §6.1 API 示例 | `{"scopeType": "organization", "targetId": "costrict-ai"}` | 文档示例使用 organization |
| §8.3 匹配规则 | `organization: 当前用户 User.Organization == target_id` | 文档说按 Casdoor organization 匹配 |
| §11.1 扩展性预留 | 当前 `user \| organization`；预留 `department \| role \| team` | 文档把 department 列为预留 |
| `distribution_service.go:64` | 字段注释 `// user \| department` | 代码注释只列 user + department |
| `distribution_service.go:87` | `ErrUnsupportedScope = "distribution target scope must be user or department"` | 错误信息明文只支持 user + department |
| `distribution_service.go:317-330` | switch 只 `case "department"`，default 返回 `ErrUnsupportedScope` | organization 落到 default 被拒 |
| `distribution_service_test.go:626` | 主动断言 organization 返回 `ErrUnsupportedScope` | 测试**保证** organization 被拒 |

**结论**：实现时已经**主动废弃** organization scope（统一走 department），但文档没同步更新。本提案把这个事实正式化。

---

## 12. Layer 2 改动

### 12.1 Distribution scope 最终语义

| ScopeType | 行为 | 真相源 | 数据访问 |
|---|---|---|---|
| `user` | 单用户：`target_id == 当前用户 subject_id` | `users` 表 | 本地 GORM |
| `department` | 团队：`target_id` 解释为外部模块的 team_id，查 OrgService 解析成员 | 外部 `org-team-service` | `OrgService.ListDepartmentMembers()` |
| ~~`organization`~~ | **永久废弃**（保留 test:626 断言） | — | — |
| ~~`role`~~ | 仍预留 | — | — |

### 12.2 `distribution_service.go` 改动

**唯一改动**：`case "department":` 数据源从 dept-sync 客户端切换到 OrgService 接口。

```go
// 当前（Layer 2 之前）
switch t.ScopeType {
case "department":
    members, err := s.deptSyncClient.LookupDepartmentMembers(ctx, t.TargetID)
    // 通过 HTTP 调用 costrict-dept-info，可能走 60 秒内存缓存

// Layer 2 之后
switch t.ScopeType {
case "department":
    members, err := s.OrgService.ListDepartmentMembers(ctx, t.TargetID)
    // 通过 OrgService 接口（L1/L2 缓存 → HTTP 调用 org-team-service → 降级兜底）
    // 注意：distribution 的 is_leader 概念由 teams.path 拓扑计算（外部模块提供 IsLeader 字段）
default:
    return ErrUnsupportedScope
}
```

**保留**：`default` 分支返回 `ErrUnsupportedScope`，确保 organization 永久被拒（test:626 继续通过）。

---

## 13. 文档对齐清单

[`ITEM_DISTRIBUTION_DESIGN.md`](../proposals/ITEM_DISTRIBUTION_DESIGN.md) 的以下章节需要在本提案 Stage A 同步修改：

| 章节 | 当前内容 | 改为 |
|---|---|---|
| §3.1 line 60-61 | `ScopeType: user \| organization \| department（预留）\| role（预留）` | `ScopeType: user \| department`（移除 organization；role 仍预留） |
| §3.3 line 105-127 | `Organization` 表设计 | 整节重写：声明 Organization 实体由外部 `org-team-service` 模块实现，本仓库通过 OrgService 接口消费 |
| §6.1 line 213 / line 238 | API 示例 `scopeType: organization` | 改为 `scopeType: department`，targetId 改为外部模块的 team_id |
| §8.3 line 324-331 | 匹配规则表格含 organization / department（预留） | 仅保留 `user` / `department`（移除 organization；department 改为正式） |
| §9.2 line 347-353 | "组织架构兼容" 描述 Casdoor + backfill | 重写：链接到本提案 Part III，声明 Casdoor 解耦，组织数据由外部模块提供 |
| §9.3 line 355-406 | OrgMembershipSyncWorker 设计 | 重写：worker 撤销（外部模块自身维护）；webhook 接入由 OrgService 内部处理 |
| §10.1 line 411-416 | 新表创建：`organizations` | 移除（不在本地建表） |
| §10.2 line 418-427 | `backfillOrganizations` 脚本 | 移除（不需要 backfill） |
| §11.1 line 441-448 | 扩展性预留：当前 `user \| organization`；预留 `department` | 改为：当前 `user \| department`；预留 `role` |
| §11.3 line 457-460 | Organization 扩展（`ParentID` / `OrgType`） | 移除（已合并到外部模块实现） |

---

# Part V：迁移路径（3 阶段）

每个 Stage 之间有兜底机制，单 Stage 失败可独立回滚。

## 14. Stage A：OrgService 接口 + Webhook endpoint

**目标**：建立对接外部模块的基础设施，但**不改变业务侧行为**。

**前置条件（阻塞项）**：外部 `org-team-service` 模块的 API **已经定稿**——具体包括 Part VIII-B 的 I-1 ~ I-4 / I-7：
- I-1：API 路径前缀
- I-2：API 响应 schema
- I-3：webhook 推送契约
- I-4：全量重同步 API
- I-7：部署形态（网络可达性 + 认证）

**当前状态（2026-07-15）**：外部模块 API **尚未定稿**。本提案 Part VIII-A（R-Q1 ~ R-D3）已作为消费方需求输入提交给外部模块团队。Stage A 启动等 API 设计完成。

**未阻塞的并行工作**：可与外部模块 API 设计并行进行的工作：
- 文档对齐（§13 清单，修改 [`ITEM_DISTRIBUTION_DESIGN.md`](../proposals/ITEM_DISTRIBUTION_DESIGN.md)）
- ADR 评审 / 团队对齐
- OrgService 接口定义的 review（不依赖具体 API 形态）

**任务**：
1. 新增 `server/internal/orgservice/` 包，实现 `Service` 接口（§3.2）+ HttpClient + Cache
2. 新增 Webhook endpoint `POST /api/webhooks/org-team/events`（§4.1）+ HMAC 签名验证
3. 实现缓存失效逻辑（§5）
4. 修改 [`ITEM_DISTRIBUTION_DESIGN.md`](../proposals/ITEM_DISTRIBUTION_DESIGN.md) §3.1 / §3.3 / §6.1 / §8.3 / §9.2 / §9.3 / §10 / §11（对齐清单见 §13）
5. 业务侧**不动**（distribution / admin 仍走 dept-sync 客户端）

**验证**：
- OrgService 接口的集成测试（mock 外部模块）
- Webhook endpoint 接收测试事件，验证签名 + 缓存失效
- 业务侧无感知（distribution 仍读 dept-sync 客户端）

**回滚**：删除 `orgservice/` 包 + webhook endpoint 路由。dept-sync 客户端仍在运行。

---

## 15. Stage B：业务切到 OrgService

**目标**：业务侧从 dept-sync 客户端切到 OrgService 接口。

**任务**：
1. `distribution_service.go:317-330` 的 `case "department":` 改读 `OrgService.ListDepartmentMembers()`
2. admin 后台组织树 API（`GET /admin/organizations`）改读 `OrgService.GetDepartmentTree()`
3. admin 后台用户查询 API 的组织字段改读 `OrgService.ListUserTeams()`
4. dept-sync 客户端代码暂时保留（不删除，作为 fallback）

**验证**（1-2 周）：
- 监控 distribution 成功率（不应下降）
- 监控 admin 后台组织树展示准确性
- 监控 OrgService 的 HTTP 调用错误率 / 延迟
- 如果 OrgService 故障，可以临时切回 dept-sync 客户端（保留代码）

**回滚**：distribution / admin 改回 dept-sync 客户端。

---

## 16. Stage C：登录流程的游离用户处理 + admin 后台"未归队用户"视图 + dept-sync 客户端下线

**目标**：解决新用户的"无 org 状态"问题（ADR-6）；admin 后台支持游离用户管理；彻底废弃 dept-sync 客户端。

**任务**：
1. **登录流程改造**（Casdoor callback handler）：
   - 用户登录时除了更新 `users` 表 profile，**异步触发** `OrgService.ListUserTeams(userID)` 查询该用户的团队归属
   - 结果**不持久化团队数据**到本地（仅作为 OrgService in-process 缓存预热）
   - 如果该用户在外部模块也无团队数据，**仅作为短 TTL 的会话级业务状态**（如 Redis 5 分钟标记，**不写入 `users` 表 schema**）——便于 admin 后台实时筛选"近期登录的游离用户"，不构成对团队数据的持久化，与 ADR-10 完全兼容
2. **admin 后台"未归队用户"视图**：
   - 新增 `GET /admin/users?teamStatus=unassigned` 过滤参数
   - 实现：调用 `OrgService.ListUnassignedUsers()`（外部模块提供）+ 本地 `users` 表 LEFT JOIN（或外部模块直接返回 user_id 列表，本地补 profile）
3. **dept-sync 客户端下线**：
   - 删除 `server/internal/deptsync/` 包
   - 删除配置项 `DEPT_SYNC_*`
   - 删除相关测试 / mock
4. **移除业务侧对 `users.organization` 字段的依赖**（admin 后台筛选 / 报表）

**验证**：
- 新用户登录后，OrgService 缓存有该用户的团队数据（或确认游离）
- admin 后台可以查到所有游离用户
- dept-sync 客户端相关代码彻底清理，无残留引用
- distribution / admin 业务正常运行

**回滚**：恢复 dept-sync 客户端代码（从 git 历史拉回）；保留登录触发 OrgService 调用（无害）。

---

# Part VI：影响面

## 17. 代码改动清单

| 文件 | 改动类型 | Stage |
|---|---|---|
| `server/internal/services/distribution_service.go:317-330` | `case "department":` 数据源从 dept-sync 客户端切到 `OrgService` | B |
| `server/internal/services/distribution_service_test.go:626` | 保留 organization 拒绝断言；新增 OrgService mock 测试 | B |
| `server/internal/adminuser/handlers.go` | `GET /admin/organizations` 数据源切到 `OrgService.GetDepartmentTree()` | B |
| `server/internal/adminuser/handlers.go` | 新增 `GET /admin/users?teamStatus=unassigned`（调用 `OrgService.ListUnassignedUsers()`） | C |
| `server/internal/deptsync/`（整个包，约 1,468 行） | **删除** | C |
| 登录流程（Casdoor callback handler，具体文件待 Stage C 确认） | 异步触发 `OrgService.ListUserTeams(userID)`，结果不持久化 | C |
| [`ITEM_DISTRIBUTION_DESIGN.md`](../proposals/ITEM_DISTRIBUTION_DESIGN.md) §3.1 / §3.3 / §6.1 / §8.3 / §9.2 / §9.3 / §10 / §11 | 文档对齐（清单见 §13） | A |

## 18. 新增文件

| 文件 | 职责 |
|---|---|
| `server/internal/orgservice/service.go` | `Service` 接口定义 + DTO（§3.2） |
| `server/internal/orgservice/http_client.go` | 外部模块 HTTP 调用（认证 / 重试 / 超时） |
| `server/internal/orgservice/cache.go` | L1 + L2 缓存实现（§5） |
| `server/internal/orgservice/impl.go` | ServiceImpl 业务逻辑组合 |
| `server/internal/orgservice/types.go` | Member / TeamMembership / Event 等 DTO |
| `server/internal/handlers/orgteam_webhook.go` | Webhook endpoint + HMAC 验证 + 幂等 |
| `server/internal/orgservice/mocks/` | mock 实现（测试用） |

## 19. 不变项

| 项 | 理由 |
|---|---|
| Gitea `team_user` 表 schema | Gitea 自管 schema；写入权转移到 @server（详见 ADR-3） |
| ~~OrgGiteaSyncWorker~~ | **删除设想**——该 worker 从未实施；team 同步逻辑由 @server GitServerAdapter 实现 |
| Distribution 的 `user` scope | 完全不动 |
| Casdoor OIDC / OAuth 认证链路 | 不变 |
| `users` 表 schema | 不变（`organization` 字段保留为遗留字段，未来由 [`CS_USER_SERVICE_DESIGN.md`](./CS_USER_SERVICE_DESIGN.md) 决定） |
| fork 权限中间件 | 维持现状（Gitea 自己查 `team_user`）；@server 保证 `team_user` 数据正确 |

---

# Part VII：决策记录（ADR）

## ADR-1：~~teams 表 team_type 共表 vs 拆分~~（**v2 撤销**）

**v2 决策**：本 ADR 整体**撤销**——costrict-web 不在本地建 `teams` 表，组织 / 团队结构由外部 `org-team-service` 模块管理。

**撤销理由**：架构方向调整为消费外部模块（详见 ADR-10）。"共表 vs 拆分"是本地持久化才需要决定的设计点，现在不在本提案范围。

**历史背景（v1）**：曾决策用单 `teams` 表 + `team_type` 字段统一 organization / department / team 三种语义。已撤销。

---

## ADR-2：Casdoor 角色（纯认证源）

**决策**：Casdoor 降级为纯登录认证源。登录时只 upsert `users` 表 profile 字段，**完全不读不写** team / org 归属。Casdoor 的 `organization` 字段被永久忽略。

**理由**：
- Casdoor `users.organization` 是单值字符串，不可靠（不可 JOIN / 易脏）
- Casdoor 的单 organization 限制不应约束本地的 multi-tenant 模型
- 维护 Casdoor → 本地的持续同步链路（webhook）成本高，收益低
- 外部 `org-team-service` 模块是更可靠的 org/department 真相源

**影响**：
- multi-tenant 边界完全由外部 `org-team-service` 模块决定（与 Casdoor 解耦）
- 新用户登录后处于"无 org 状态"直到外部模块覆盖该用户（ADR-6）

**替代方案（被否决）**：
- Casdoor webhook 接入 + 持续同步——增加同步链路复杂度
- Casdoor 登录返回的 org 信息首次快照写入——Casdoor org 字段不可靠，可能写入脏数据

---

## ADR-3：本提案不直接接入 Gitea team；Gitea team 同步由 @server 负责（v3 决策反转）

> **v3 变更（2026-07-15）**：team 同步职责从 cs-user **反转**到 @server。cs-user 仅保留 user-level 工作（自动开户 + `user_gitea_binding` 维护）；team-level `team_user` 同步统一归集到 @server。详见下文理由。

**决策**：本提案（TEAM_ORG_UNIFICATION）**不直接**对接 Gitea team——但**不再**是"完全不接入"。Gitea team 成员授权的同步职责由 **@server 模块** 承担（实施位置 `server/internal/gitsync/`，定义 `GitServerAdapter` 抽象 + Gitea 实现）：

- @server 消费 org-team-service 的 webhook 事件（团队 / 成员变更）
- @server 通过 **GitServerAdapter** 接口（in-process 抽象，首期实现 GiteaAdapter）实时 push 到 Gitea `team_user` 表
- **同步方向**：单向 @server → Gitea（@server 是真相源，Gitea 是被写入方）
- **触发模式**：实时（事件驱动）

**职责边界**：

| 模块 | 职责 |
|---|---|
| TEAM_ORG_UNIFICATION（本提案） | 消费 org-team-service（HR 组织 / 团队结构）；不感知 Gitea team |
| **@server** | 同时承担：①业务侧 Gitea 协作（workflow init / KB init / capability init / repo CRUD）；②team-level 同步（消费 org-team-service webhook → 写入 Gitea `team_user`，fork repo 权限） |
| **cs-user** | 仅 user-level Gitea 协作：自动开户（user provisioning）+ `user_gitea_binding` 维护 + fork Gitea middleware 回调（`/api/internal/users/:id/gitea-binding`）；**不参与 team 同步** |
| Gitea | `team_user` 表的拥有者；fork 中间件继续内部查询（不变） |

**理由（v3 反转）**：
- **职责内聚**：@server 已承担 workflow init / KB init / capability init 等业务侧 Gitea 协作，team_user 同步是同类型工作（写入 Gitea 元数据），归集到一处
- **GitServerAdapter 复用**：@server 已需要 Gitea client（业务侧 repo 操作），扩展 team-level 操作是自然延伸
- **cs-user 保持纯粹**：cs-user 只负责"用户身份"维度（开户 + binding），不掺入"团队 / 权限"维度，职责边界更清晰
- **故障隔离**：team 同步故障不影响 user 开户链路（登录链路），降级范围更小

**影响**：
- 本提案实施时**不动**Gitea 客户端代码（如有）；Gitea team 同步是 @server 的独立工作流（`server/internal/gitsync/`）
- fork 权限中间件维持现状（继续内部查 `team_user`）—— @server 负责保证 `team_user` 表数据正确
- 如果 @server 故障，fork 权限可能短暂不一致（已写入 Gitea 的数据仍生效，新变更延迟）
- cs-user 与 Gitea 的关系收缩为：仅 user-level（auto-provision + binding），不再持 team-level admin 操作权

---

## ADR-4：multi-tenant 限制解除（边界由外部模块决定）

**决策**：multi-tenant 边界完全由外部 `org-team-service` 模块决定，**不再**受 Casdoor 单 organization 假设约束。

**理由**：
- ADR-2 已经把 Casdoor 降级——它的单 organization 限制自然失效
- 外部 `org-team-service` 模块由专门团队设计，自然支持 multi-tenant
- [`MULTI_TENANCY_DESIGN.md`](./MULTI_TENANCY_DESIGN.md) 的 row-level multi-tenancy 设计与外部模块的 tenant 概念对接

**影响**：
- 一个 user 可以同时属于多个 organization（外部模块决定）
- [`MULTI_TENANCY_DESIGN.md`](./MULTI_TENANCY_DESIGN.md) 落地时，需要与外部模块团队协调 tenant_id 的语义对齐
- 本提案的 OrgService 接口不感知 tenant（透传）——tenant 隔离由外部模块保证

**v1 → v2 调整**：v1 设想本地 `team_members` 表承担 multi-tenant 边界；v2 改为外部模块负责。

---

## ADR-5：organization scope 永久废弃

**决策**：distribution scope 永久废弃 `organization`，统一为 `user | department`。这**对齐代码现状**（`distribution_service.go:87` 错误信息 + `test:626` 主动断言）。

**理由**：
- 代码已经主动废弃 organization scope（统一走 department），有测试覆盖
- distribution 的语义是"下发到团队"，团队由外部模块的 team 实体表达——organization scope 是冗余概念
- 文档与代码漂移是 staleness 问题，应正式化代码行为

**影响**：
- [`ITEM_DISTRIBUTION_DESIGN.md`](../proposals/ITEM_DISTRIBUTION_DESIGN.md) §3.1 / §6.1 / §8.3 / §11.1 修改（清单见 §13）
- `distribution_service.go` 的 `default` 分支保留 `ErrUnsupportedScope`（不删，作为永久防御）

**替代方案（被否决）**：
- 文档对齐代码（恢复 organization scope）——需要回答"organization scope 与 department scope 语义如何区分"，目前无答案

---

## ADR-6：新用户"无 org 状态"处理（依赖外部模块）

**决策**：双层处理游离用户问题：
1. **基础层**：新用户首次 Casdoor 登录后，`users` 表有记录但**外部模块暂无团队数据**，处于"游离"状态
2. **主动层**：登录流程**异步触发** `OrgService.ListUserTeams(userID)`，把结果缓存到 in-process L2（不持久化）——这把游离窗口从"未知"压缩到"OrgService 缓存内可见"

如果该用户在外部模块中确实无团队数据，由 admin 通过"未归队用户"视图（数据来自 `OrgService.ListUnassignedUsers()`）手动归队（在外部模块操作，不在 costrict-web 内）。

**理由**：
- costrict-web 不持久化团队数据，"游离"本质是外部模块的状态——本提案只能查询，不能改写
- **登录触发 OrgService 查询**是 A1 的自然补全：登录是用户最高频的"主动时刻"，正好触发查询
- 异步 goroutine 不阻塞登录流程，用户体验无影响
- 如果该用户在外部模块中确实无团队数据，admin 手动归队需要在外部模块操作（不在 costrict-web 范围）

**影响**：
- 新用户登录后 30 秒内（异步 goroutine 完成），OrgService 缓存有该用户的团队数据（或确认游离）
- admin 后台"未归队用户"视图调用 `OrgService.ListUnassignedUsers()`
- 不存在"提高同步频率"问题——外部模块自身维护数据，costrict-web 实时查询

**替代方案（被否决）**：
- 在本地 `users` 表加 `is_unassigned` 标记——违反"不本地持久化"原则（ADR-10）
- 同步阻塞登录等 OrgService 返回——登录性能下降不可接受
- 在本地 fallback 到 dept-sync 客户端——dept-sync 客户端 Stage C 已废弃

---

## ADR-7：~~dept-sync 真实架构~~（**v2 撤销，保留作历史背景**）

**v2 决策**：本 ADR 的结论整体**撤销**——dept-sync 客户端在 Stage C 整体下线。本 ADR 保留作为历史背景，用于理解 dept-sync 客户端废弃前的真实形态。

**历史调研发现**（仍有效）：
- dept-sync 是 `server/internal/deptsync/` 内的 HTTP **客户端**（约 1,468 行 Go 代码），不是独立服务
- 它调用外部 `costrict-dept-info` 服务，60 秒内存缓存（`DEPT_SYNC_CACHE_TTL_SECONDS`）
- 没有定时同步（按需查询），没有 webhook 推送，没有 Redis
- 稳定性高（零 TODO/FIXME，测试完善，优雅降级）

**v2 调整理由**：
- 外部模块 `org-team-service` 由其他同事负责，统一管理组织 / 团队——dept-sync 客户端的职责被 OrgService 接口完全替代
- `costrict-dept-info` 服务的命运由其维护团队决定（可能演进为 `org-team-service`，或被替代）
- costrict-web 不再需要 dept-sync 客户端

**迁移**：详见 Part V Stage B / Stage C。

---

## ADR-8：~~team_members.role 简化~~（**v2 撤销**）

**v2 决策**：本 ADR 整体**撤销**——costrict-web 不在本地建 `team_members` 表，`role` 字段语义由外部 `org-team-service` 模块定义。

**撤销理由**：架构方向调整为消费外部模块（详见 ADR-10）。`role` 字段是本地持久化才需要决定的设计点。

**历史背景（v1）**：曾决策 `team_members.role` 仅保留 `member | leader` 两个值（删除 admin）。已撤销——OrgService 的 Member DTO 透传外部模块的 role 语义。

**注**：distribution 的 `is_leader` 概念（运行时基于 path 拓扑计算）仍由外部模块提供（OrgService.Member.IsLeader 字段）。

---

## ADR-9：~~teams 表不预留 tenant_id 列~~（**v2 撤销**）

**v2 决策**：本 ADR 整体**撤销**——costrict-web 不在本地建 `teams` 表，不存在"是否预留 tenant_id"的问题。

**撤销理由**：架构方向调整为消费外部模块（详见 ADR-10）。tenant_id 由外部模块管理，[`MULTI_TENANCY_DESIGN.md`](./MULTI_TENANCY_DESIGN.md) 落地时与外部模块团队协调。

---

## ADR-10：消费外部 org-team-service 模块（不本地持久化）

**决策**：组织架构 + 团队机制的真相源是外部独立模块 `org-team-service`（其他同事负责）。costrict-web 通过 OrgService 接口 + Webhook 事件对接，**不本地持久化**任何组织 / 团队数据。

**理由**：
- 组织 / 团队的复杂度（CRUD、生命周期、跨租户、权限边界、领导关系）应由专门模块承担
- costrict-web 是业务系统，不应重复实现组织 / 团队管理逻辑
- 外部模块由专门团队演进，可独立部署 / 升级 / 扩容
- multi-tenancy 等复杂机制在外部模块内闭环
- 不持久化 = 不需要数据迁移 / 同步窗口 / 冲突处理

**影响**：
- 本提案 v1 设计的 `teams` / `team_members` 表整体撤销（ADR-1 / ADR-8 / ADR-9）
- 业务侧通过 OrgService 接口获取数据（distribution / admin / login 等）
- Webhook 事件仅用于缓存失效 + 下游通知，不持久化
- 失败补偿依赖外部模块的全量重同步 API

**替代方案（被否决）**：
- v1 的本地持久化方案（teams + team_members 表）——重复实现外部模块职责，增加同步复杂度
- 直接调用 `costrict-dept-info`（绕过 OrgService 抽象）——丢失降级 / 缓存策略，且接口契约分散
- 本地短 TTL 缓存表——实际等于"半持久化"，需要处理 schema 演进，违背"不持久化"原则

---

## ADR-11：OrgService 接口设计（in-process 抽象）

**决策**：所有业务方通过 `server/internal/orgservice/` 内的 `Service` 接口访问组织 / 团队数据。OrgService 内部封装 HTTP 调用、缓存、降级。

**接口设计原则**：
- **业务方零感知**：调用方不关心数据来自缓存还是 HTTP
- **降级友好**：返回值附 stale 标记，业务方可选择是否接受
- **测试友好**：提供 mock 实现，单元测试不依赖外部模块
- **DTO 稳定**：接口契约稳定，外部模块 API 变化只影响 HttpClient 层

**接口方法**（详见 §3.2）：
- `GetDepartmentTree()` / `ListDepartmentMembers()` / `ListSubtreeMembers()`
- `ListUserTeams()` / `ListUnassignedUsers()`

**替代方案（被否决）**：
- 业务方各自调用 HTTP——重复实现缓存 / 降级，难以统一治理
- 用 GORM 直连外部模块的 DB——跨服务 DB 共享是反模式，破坏外部模块封装

---

## ADR-12：Webhook 事件处理（仅失效缓存，不持久化）

**决策**：Webhook endpoint 收到事件后，**仅失效 in-process 缓存** + 触发 metrics，**不持久化任何数据**。

**理由**：
- costrict-web 不持久化组织 / 团队数据（ADR-10），webhook 不需要写表
- in-process 缓存的失效是 webhook 的主要价值——避免业务侧读到 stale 数据
- 不持久化 = 不需要处理事件顺序 / 幂等 / 重放等复杂问题（除内存 LRU 去重）

**事件类型**（详见 §4.2）：
- `team.created` / `team.updated` / `team.deleted`
- `member.added` / `member.removed` / `member.role_changed`

**幂等保证**：内存 LRU 缓存（1 小时窗口）记录已处理的 `event_id`，重复事件忽略。

**替代方案（被否决）**：
- 持久化事件流（如 Kafka）——过度设计，costrict-web 不需要审计 / 重放
- 不接 webhook，仅靠缓存 TTL 自然回收——staleness 窗口可达 30 秒（L2 TTL），distribution 场景可能影响业务
- webhook 触发本地表 upsert（v1 思路）——违背"不持久化"原则

---

## ADR-13：In-process 缓存策略

**决策**：OrgService 内置两级缓存（L1 5 秒 + L2 30 秒），webhook 主动失效。

**缓存层级**（详见 §5）：
- L1（goroutine 本地 sync.Map）：5 秒 TTL，高频读
- L2（进程级 sync.Map 或 LRU）：30 秒 TTL，一般读

**失效规则**：
- `team.*` 事件 → 全树失效（简化处理，team 变更频率低可接受）
- `member.*` 事件 → 精确失效（该 team + 该 user + 祖先部门的子树）

**降级策略**：
- 外部模块 5xx / 超时 → 返回 stale L2 数据 + log warning
- 无缓存数据 → 返回 503，业务侧处理 `ErrOrgServiceUnavailable`

**替代方案（被否决）**：
- 无缓存，每次直连外部模块——distribution 性能不可接受（每次 scope 解析都走 HTTP）
- Redis 缓存（跨进程共享）——过度工程，本提案 30 秒 TTL + webhook 失效已经够用；且 Redis cache 在 v1 调研中已经证明不是必需
- 长 TTL（5 分钟）——staleness 风险高，webhook 失效跟不上

---

# Part VIII：消费方需求清单 + 实现侧开放问题

> **2026-07-15 更新**：经与外部模块团队沟通确认，外部 `org-team-service` 模块的 API **尚未定稿**。本部分相应调整为：
> - **A 节**：costrict-web 作为消费方的需求清单——现已收敛，作为给外部模块团队的**需求输入**
> - **B 节**：实现侧开放问题——依赖外部模块 API 具体形态，待 API 定稿后回答

## A. 消费方需求清单（向外部模块提出，已闭环）

本提案确认 costrict-web 业务侧需要的查询能力、事件、SLA。这是给外部 `org-team-service` 模块团队的**需求输入**，不依赖具体 API 形态。

### A.1 查询能力需求

| # | 业务场景 | 需要的查询 | 频率 / SLA 要求 |
|---|---|---|---|
| **R-Q1** | distribution scope='department'（单部门） | 给定 dept_id，查成员列表 | 高频（每次下发）；P99 < 100ms（缓存命中）/ < 500ms（缓存未命中） |
| **R-Q2** | distribution scope='department'（子树） | 给定 dept_id，查子树所有成员（去重） | 高频；P99 同 R-Q1 |
| **R-Q3** | admin 后台组织树展示 | 查整棵部门树（含嵌套） | 中频（页面加载）；P99 < 500ms |
| **R-Q4** | 登录触发 + 用户详情页 | 给定 user_id，查该用户所属所有团队 | 中频（登录时）；P99 < 200ms |
| **R-Q5** | admin 后台"未归队用户"视图 | 查所有游离用户（无团队归属的用户列表，分页） | 低频（admin 操作）；P99 < 1s |

**注**：R-Q1 / R-Q2 / R-Q3 / R-Q4 已经在现有 `costrict-dept-info` 服务中提供（被 `server/internal/deptsync/client.go:202 / 250 / 274` 调用），可参考其 API 形态作为基线。R-Q5 是否扩展、是否新增端点，由外部模块团队决定。

### A.2 Webhook 事件需求

| # | 事件类型 | costrict-web 的处理 |
|---|---|---|
| **R-E1** | 团队创建 / 修改 / 删除 | 失效部门树缓存 |
| **R-E2** | 成员加入 / 离开 / 角色变更 | 失效相关团队 + 用户缓存 |

**注**：costrict-web **不需要**事件持久化、重放、有序交付——只要能失效缓存即可。最简单的事件 schema 即可（type + id + payload）。

### A.3 数据语义需求

| # | 语义 | 说明 |
|---|---|---|
| **R-S1** | 部门 / 团队 materialized path | 每个节点携带 path（如 `/root/engineering/backend`），用于子树查询 + leader 拓扑计算 |
| **R-S2** | 成员 leader 标记 | 是否为 leader 由 path 拓扑决定（用户属于非叶子部门即为其子树的拓扑 leader）；外部模块**返回 IsLeader 字段**或由消费方本地计算——两者择一，建议外部模块返回（减少消费方重复实现） |
| **R-S3** | 角色 | 成员角色由外部模块定义；costrict-web 透传，不二次解释 |

### A.4 失败补偿需求

| # | 场景 | 需求 |
|---|---|---|
| **R-F1** | costrict-web 重启后缓存丢失 | 提供全量重同步 API（一次性拉所有团队 / 成员）补预热缓存，或允许遍历调用查询接口 |
| **R-F2** | 外部模块故障 | 返回明确的 5xx 让消费方降级；可选支持 stale-while-revalidate（Cache-Control: stale-while-revalidate） |

### A.5 部署 / 认证需求

| # | 需求 | 说明 |
|---|---|---|
| **R-D1** | 网络可达 | 外部模块部署后 costrict-web 可直连（同 VPC / VPN / 公开） |
| **R-D2** | 认证 | **建议沿用** `X-Query-Key` header（query_key 表）以减少迁移成本；如换新机制需提供过渡期 |
| **R-D3** | 服务发现 | 提供 DNS 名称或 service mesh 配置 |

---

## B. 实现侧开放问题（等外部模块 API 定稿）

以下问题依赖外部模块 API 的具体形态。当前**外部模块 API 尚未定稿**，本提案 Stage A 实施前需要外部模块团队先完成 API 设计。

| # | 问题 | 阻塞 Stage |
|---|---|---|
| **I-1** | API 路径前缀（沿用 `/costrict-dept-info/api/v1`？新 `/org-team-service/api/v1`？） | A |
| **I-2** | API 响应 schema（字段命名、嵌套结构、错误码） | A |
| **I-3** | webhook 推送的 endpoint URL（costrict-web 提供）/ 签名算法 / payload schema / 重试策略 | A |
| **I-4** | 全量重同步 API 的路径 / 参数（last_event_id？since timestamp？）/ 响应格式（流式 / 分页） | A |
| **I-5** | `ListUnassignedUsers` API 是否提供（R-Q5） | C |
| **I-6** | 性能 SLA（P99 / QPS 上限） | B |
| **I-7** | 部署形态（独立服务 / 集群 / 与现有 `costrict-dept-info` 合并） | A |

---

## C. v1 → v2 已关闭问题

| 原 v1 问题 | v2 处理 |
|---|---|
| O1（dept-sync 服务稳定性 + Stage D Redis 退役） | 撤销——dept-sync 客户端 Stage C 整体下线（ADR-7） |
| O2（dept-sync 同步频率 / HR 负载） | 撤销——dept-sync 客户端废弃，不存在该问题 |
| O3（admin 后台"未归队用户"视图责任方） | 仍由本提案实现——数据源改为 `OrgService.ListUnassignedUsers()`（Stage C） |
| O4（teams 表 tenant_id 预留） | 撤销——本地不建表（ADR-9） |
| O5（team_members.role='admin' 权限边界） | 撤销——本地不建表，role 语义由外部模块定义（ADR-8） |

---

## D. 提案状态总结

| 维度 | 状态 |
|---|---|
| **方案层面**（消费外部模块 / 不本地持久化 / OrgService 接口） | **已闭环** |
| **需求层面**（A 节 R-Q1 ~ R-D3 作为外部模块 API 设计的输入） | **已闭环** |
| **实施层面**（Stage A 阻塞于外部模块 API 定稿，B 节 I-1 ~ I-7） | **待外部团队** |

**下一步**：将 A 节作为"消费方需求文档"提交给外部 `org-team-service` 模块团队；待其完成 API 设计后启动 Stage A 实施。
