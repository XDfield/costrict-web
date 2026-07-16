# Item Distribution（技能下发/推送）功能设计文档

**Status**: Proposal  
**Author**: Claude  
**Date**: 2026-05-21  

---

## 1. 背景与目标

### 1.1 背景

当前 costrict-web 的 Item（Skill / MCP / Plugin）仅通过 Repository 的 `public/private` 可见性控制访问范围，缺乏**主动下发**机制。企业场景中，主管/管理员需要将 curated AI 技能批量推送给团队，并控制其生命周期（只读、可修改、可收回）。接收方应在收藏列表中自动看到这些下发的技能。

### 1.2 目标

1. 管理员/主管可将 Item 主动下发给指定用户或组织
2. 下发技能自动出现在接收方的收藏列表中
3. 支持权限模式：只读、可 Fork、可编辑
4. 支持生命周期控制：下发、暂停、收回
5. 与通知系统集成，接收方实时收到推送
6. 兼容现有组织架构（Casdoor Organization），可扩展部门树

### 1.3 非目标

- 实时协同编辑（超出范围，保持与现有 Item 更新机制一致）
- 复杂的工作流审批（简化为主管一键下发）
- 跨租户/跨组织推送（组织之间保持隔离）

---

## 2. 术语定义

| 术语 | 定义 |
|------|------|
| **下发 (Distribute)** | 主动将某个 Item 推送给目标用户或组织 |
| **下发者 (Distributor)** | 执行下发操作的用户 |
| **接收者 (Recipient)** | 被下发目标覆盖到的用户 |
| **回执 (Receipt)** | 接收者对某条下发的状态记录 |
| **权限模式 (Permission Mode)** | 接收者对该 Item 的操作权限 |
| **生命周期 (Lifecycle)** | 下发的当前状态：active / paused / revoked |

---

## 3. 数据模型设计

### 3.1 ItemDistribution（下发记录主表）

存储每一次"下发"动作的核心信息。

```go
type ItemDistribution struct {
    ID              string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
    ItemID          string    `gorm:"type:uuid;not null;index" json:"itemId"`
    DistributorID   string    `gorm:"type:text;not null;index" json:"distributorId"`
    PermissionMode  string    `gorm:"type:varchar(32);default:'readonly'" json:"permissionMode"`
    // readonly | forkable | editable
    Status          string    `gorm:"type:varchar(32);default:'active'" json:"status"`
    // active | paused | revoked
    ScopeType       string    `gorm:"type:varchar(32);default:'user'" json:"scopeType"`
    // user | organization | department（预留）| role（预留）
    TargetID        string    `gorm:"type:text;not null;index" json:"targetId"`
    Message         string    `gorm:"type:text" json:"message,omitempty"`
    RevokedAt       *time.Time `json:"revokedAt,omitempty"`
    ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
    CreatedAt       time.Time  `json:"createdAt"`
    UpdatedAt       time.Time  `json:"updatedAt"`

    Item *CapabilityItem `gorm:"foreignKey:ItemID" json:"item,omitempty"`
}
```

**索引设计：**

| 索引名 | 字段 | 用途 |
|--------|------|------|
| `idx_dist_item_status` | `(item_id, status)` | 查询某 Item 的所有有效下发 |
| `idx_dist_target` | `(scope_type, target_id, status)` | 查询某目标收到的有效下发 |
| `idx_dist_distributor` | `(distributor_id, status)` | 查询我下发的记录 |

**唯一约束：**
- 同 Item 同目标允许重新下发（生成新记录），不做唯一限制。历史版本通过 `status=revoked` 保留。

### 3.2 ItemDistributionReceipt（下发回执表）

用户视角的"我收到了什么"。支持用户单独忽略某条下发。

```go
type ItemDistributionReceipt struct {
    ID              string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
    DistributionID  string    `gorm:"type:uuid;not null;index" json:"distributionId"`
    UserID          string    `gorm:"type:text;not null;index" json:"userId"`
    ReceiptStatus   string    `gorm:"type:varchar(32);default:'unread'" json:"receiptStatus"`
    // unread | read | dismissed | accepted
    ForkedItemID    *string   `gorm:"type:uuid" json:"forkedItemId,omitempty"`
    CreatedAt       time.Time `json:"createdAt"`
    UpdatedAt       time.Time `json:"updatedAt"`
}
```

**唯一索引：** `idx_dist_receipt_user_dist` on `(distribution_id, user_id)`

**设计 rationale：** 下发给组织时可能有 1000+ 用户，采用**预创建模式**（下发时批量插入 receipts）。查询时直接 JOIN 该表，性能可控。

### 3.3 Organization（组织架构基础表）

当前系统仅有 Casdoor 同步的 `User.Organization` 字符串字段。为支持"下发给组织"，需要规范化组织实体，并预留部门树扩展。

```go
type Organization struct {
    ID          string     `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
    Name        string     `gorm:"type:varchar(191);not null;uniqueIndex" json:"name"`
    // 与 Casdoor owner / User.Organization 对齐
    DisplayName string     `gorm:"type:varchar(255)" json:"displayName"`
    Description string     `gorm:"type:text" json:"description"`
    ParentID    *string    `gorm:"type:uuid;index" json:"parentId,omitempty"`
    OrgType     string     `gorm:"type:varchar(32);default:'company'" json:"orgType"`
    // company | department | team
    CreatedAt   time.Time  `json:"createdAt"`
    UpdatedAt   time.Time  `json:"updatedAt"`
}
```

**迁移策略：**
1. 首次 AutoMigrate 后，运行 backfill：从 `users.organization` 提取唯一非空值，批量插入 `organizations`
2. 后续用户同步时，如 `Organization` 不存在则自动创建

---

## 4. 权限模式详解

| 模式 | 接收方能力 | 收藏列表表现 | 修改原 Item |
|------|-----------|-------------|------------|
| **readonly** | 查看、阅读、安装使用 | 出现在收藏，标为"下发" | 不可 |
| **forkable** | 可 Fork 到自己的 Registry | 出现在收藏，标为"下发"；Fork 后收藏自己的副本 | 不可（改副本） |
| **editable** | 直接编辑原 Item | 出现在收藏，标为"下发" | 可以 |

### 4.1 Fork 行为

用户首次 Fork 时，如不存在个人 Registry，则**自动创建**一个 internal 类型 Registry：
- Name: `user-{subject_id}-personal`
- RepoID: 关联到一个自动创建的 private Repository

Fork 后的物品与原物品完全独立，原物品更新不会同步。

### 4.2 editable 权限与 RepoMember 的协调

非 Repo 成员的接收者，若 receipt 为 editable，则在 `canMutateItem` 检查中**额外放行**。仅对该 Item 有效，不提升其 `RepoMember` 角色。

这意味着：
- 接收者可以调用 `PUT /items/:id` 修改该 Item
- 修改记录中 `UpdatedBy` 为该接收者
- 该接收者仍无法修改同 Repo 中的其他 Item

---

## 5. 生命周期状态机

```
          distribute
              |
              v
    +------------------+
    |     active       |<------------------+
    +------------------+                   |
         |          |                      |
    pause|          | resume               |
         v          |                      |
    +------------------+                   |
    |     paused       |                   |
    +------------------+                   |
         |                                 |
    revoke|        revoke                  |
         v          |                      |
    +------------------+                   |
    |     revoked      |                   |
    +------------------+                   |
         |                               update
         |                                 |
         +---------------------------------+
              （revoked 后同 Item 同目标可重新下发，
               生成新 Distribution 记录）
```

| 状态 | 收藏列表可见性 | ReceiptStatus | 说明 |
|------|---------------|---------------|------|
| **active** | 可见 | unread / read / accepted | 正常生效 |
| **paused** | 隐藏（但仍存在）| 不变 | 临时停用，可随时恢复 |
| **revoked** | 移除 | dismissed | 正式收回，收藏列表删除 favorite 记录 |

**与"用户离开 org"的差异说明**：上表描述的是**下发者主动收回**（distribution.status → revoked）。还有一种被动场景是**用户离开组织**（distribution 仍 active，但用户不再属于 target org）——此时该用户的 receipts 置为 `dismissed`，但**保留** `receiptStatus='accepted'` 或已有 `ForkedItemID` 的记录（用户已"拿走"该能力项，回收等于惩罚）。详见 §9.3 OrgMembershipSyncWorker。

---

## 6. API 设计

### 6.1 下发者视角

| Method | Path | 说明 |
|--------|------|------|
| POST | `/items/:id/distribute` | 下发 Item |
| GET | `/items/:id/distributions` | 查询某 Item 的所有下发记录 |
| PUT | `/distributions/:id` | 更新下发（暂停/恢复/修改权限/附言） |
| DELETE | `/distributions/:id` | 收回下发（状态置 revoked） |
| GET | `/distributions/my/sent` | 我下发的所有记录 |

**POST /items/:id/distribute Request Body:**

```json
{
  "targets": [
    {"scopeType": "user", "targetId": "user-subject-id-1"},
    {"scopeType": "organization", "targetId": "costrict-ai"}
  ],
  "permissionMode": "readonly",
  "message": "推荐团队统一使用这个代码审查 Skill",
  "expiresAt": "2026-12-31T00:00:00Z"
}
```

**Response:**

```json
{
  "distributions": [
    {
      "id": "dist-uuid-1",
      "itemId": "item-uuid",
      "scopeType": "user",
      "targetId": "user-subject-id-1",
      "permissionMode": "readonly",
      "status": "active",
      "recipientCount": 1,
      "createdAt": "2026-05-21T10:00:00Z"
    },
    {
      "id": "dist-uuid-2",
      "itemId": "item-uuid",
      "scopeType": "organization",
      "targetId": "costrict-ai",
      "permissionMode": "readonly",
      "status": "active",
      "recipientCount": 42,
      "createdAt": "2026-05-21T10:00:00Z"
    }
  ]
}
```

### 6.2 接收者视角

| Method | Path | 说明 |
|--------|------|------|
| GET | `/distributions/my/received` | 我收到的下发列表 |
| POST | `/distributions/:id/dismiss` | 忽略某条下发 |
| POST | `/distributions/:id/fork` | Fork 该物品（仅 forkable 模式） |
| POST | `/distributions/:id/read` | 标记为已读 |

### 6.3 与收藏列表集成

修改现有 `GET /items?favorited=true` 查询逻辑：

**当前逻辑：** 仅查询 `item_favorites` 表  
**新逻辑：** UNION 以下两部分：
1. `item_favorites` — 用户主动收藏
2. `item_distribution_receipts` — 有效下发且未 dismiss，关联的 `ItemDistribution.status = 'active'`

返回字段增加：

```json
{
  "source": "favorite" | "distributed",
  "distribution": {
    "id": "dist-uuid",
    "permissionMode": "readonly",
    "distributorId": "user-subject-id",
    "message": "..."
  }
}
```

---

## 7. 通知集成

### 7.1 新增事件类型

```go
const (
    EventItemDistributed = "item.distributed"
    EventItemRevoked     = "item.revoked"
    EventItemPaused      = "item.paused"
)
```

### 7.2 触发时机

| 事件 | 触发时机 | 接收方 |
|------|---------|--------|
| `item.distributed` | 下发成功后异步触发 | 所有目标用户 |
| `item.revoked` | 收回下发后异步触发 | 受影响的用户 |
| `item.paused` | 暂停下发后异步触发 | 受影响的用户 |

### 7.3 消息模板示例（WeCom / 系统通知）

> 【技能下发】**林凯** 向你下发了技能 **「代码审查助手」**（权限：只读）  
> 附言：推荐团队统一使用这个代码审查 Skill  
> [点击查看]

---

## 8. 权限检查策略

### 8.1 谁能下发？

仅拥有 `platform_admin` 系统角色的用户可以下发 Item。创建者和 Repo 管理员均不具备下发权限。

### 8.2 谁能修改/收回下发？

满足以下任一条件：
- `DistributorID`（下发者本人）
- `platform_admin`

### 8.3 谁能接收？

| ScopeType | 匹配规则 |
|-----------|---------|
| `user` | `target_id == 当前用户 subject_id` |
| `organization` | `当前用户 User.Organization == target_id` |
| `department` | （预留）`当前用户 department_id == target_id` |

---

## 9. 与现有架构的兼容性

### 9.1 复用机制清单

| 现有机制 | 复用方式 |
|---------|---------|
| `ItemFavorite` | 下发 active 时自动创建 favorite；revoke 时自动删除 |
| `notification` 模块 | 新增事件类型，复用 `TriggerMessage` |
| `CapabilityItem` + `CapabilityVersion` | editable 直接更新原 Item；forkable 复用 `persistNewItem` 创建副本 |
| `Repository` + `RepoMember` | 权限检查复用 `getCallerRepoRole` / `isRepoAdmin` |
| `middleware` | 接入 `RequireAuth` 认证 |
| `GORM AutoMigrate` | 新模型加入 AutoMigrate 列表 |

### 9.2 组织架构兼容

当前系统没有独立的组织架构模块，仅有 Casdoor 同步的 `User.Organization` 字符串。本次设计：
- 引入 `Organization` 实体表，将字符串升级为规范化实体
- `User.Organization` 字段保持现有 Casdoor 同步逻辑不变
- 通过 backfill 将历史用户自动归类到 Organization
- 预留 `ParentID` 和 `OrgType`，未来可平滑升级为部门树

### 9.3 组织成员变更级联（OrgMembershipSyncWorker）

ADR-1 的"预创建 receipts"策略只覆盖**下发那一刻**在 org 内的用户。事后加入 / 离开 org 的用户需要由 worker 主动维护 receipts。该 worker 同时承担 Gitea team 同步职责（详见 ROADMAP Phase 2.5），是同一事件的双消费者之一。

**事件源**：`user.organization_changed`，由 dept-sync 服务 webhook 推送（与 ROADMAP §3.5 dept-sync 集成链路一致）。事件载荷：`{user_id, old_org_id, new_org_id, changed_at}`。

**加入 org（new_org_id 非空）流程**：

```
1. OrgGiteaSyncWorker 先调 Gitea admin API 把用户加入 new_org_id 对应的 team（fire-and-forget + 重试）
2. Gitea team 成功后，OrgMembershipSyncWorker 执行：
   SELECT id, permission_mode FROM item_distributions
   WHERE scope_type='organization'
     AND target_id=$new_org_id
     AND status='active'
   → 对每条 distribution INSERT item_distribution_receipts
     (distribution_id, user_id, receipt_status='unread')
     ON CONFLICT (distribution_id, user_id) DO NOTHING
3. 异步触发 item.distributed 通知（复用 §7.2 模板）
```

**离开 org（old_org_id 非空）流程**：

```
1. OrgGiteaSyncWorker 调 Gitea admin API 从 old_org_id team 移除用户
2. Gitea team 成功后，OrgMembershipSyncWorker 执行：
   UPDATE item_distribution_receipts
   SET receipt_status='dismissed', updated_at=NOW()
   WHERE user_id=$user_id
     AND receipt_status IN ('unread', 'read')
     AND distribution_id IN (
       SELECT id FROM item_distributions
       WHERE scope_type='organization' AND target_id=$old_org_id
     )
   -- 已 accepted 或已 fork（ForkedItemID 非空）的 receipt 保留
```

**重试与一致性**：

- worker **无状态**：不维护 `sync_status` 列，依赖退避重试 + 审计日志兜底
- 复用 ROADMAP §2.4 通用 webhook 广播 worker 的 6 次指数退避队列（1s / 5s / 30s / 2min / 10min / 1h）+ 死信队列
- Gitea team API 与 receipts 维护**不在同一事务**（跨库）：先 Gitea team 成功再写 receipts，避免"Gitea 已加入但 receipts 未补"导致用户看不到下发；逆序失败由日 cron 全量校对兜底（对比 org 成员 vs receipts 集合）
- 每次写 `gitea_admin_audit_log`（actor=系统，endpoint=`/admin/teams/{id}/members`）

**为何不加 `user_gitea_team_binding` 表**：

V4 §6.6 第 751 行明文：fork 中间件**不校验** team 成员关系（Gitea 自己查 `team_user` 表）。因此：

- 中间件不需要本地 team 映射 → 不需要中间表
- worker 失败由重试 + 审计覆盖 → 不需要 `sync_status` 状态机
- "用户在哪些 team" 的查询走 Gitea API（低频管理操作，可接受延迟）→ 不需要本地索引

---

## 10. 数据库迁移计划

### 10.1 新表创建

通过 GORM AutoMigrate 自动创建：
- `item_distributions`
- `item_distribution_receipts`
- `organizations`

### 10.2 Backfill 脚本

在 `server/cmd/migrate/main.go` 中新增：

```go
func backfillOrganizations(db *gorm.DB) error {
    // 从 users.organization 提取唯一非空值
    // INSERT INTO organizations (name, display_name) VALUES ... ON CONFLICT DO NOTHING
}
```

### 10.3 索引创建

通过 Goose SQL 迁移或 GORM 标签自动创建：
- `idx_dist_item_status`
- `idx_dist_target`
- `idx_dist_distributor`
- `idx_dist_receipt_user_dist`

---

## 11. 扩展性预留

### 11.1 ScopeType 扩展

当前：`user | organization`  
预留：`department | role | team`

- `department`：未来引入 Department 树表后，`target_id` 解释为部门 ID
- `role`：可复用 `SystemRole` 或 `RepoMember.Role`，下发给某角色的所有用户

### 11.2 PermissionMode 扩展

当前：`readonly | forkable | editable`  
预留：`suggestable | scheduled`

- `suggestable`：接收方可建议修改，需审批后合并
- `scheduled`：定时自动激活/过期

### 11.3 Organization 扩展

- `ParentID` 已预留，未来可支持部门树查询
- `OrgType` 区分公司/部门/小组，支持混合组织形态

---

## 12. 关键实现文件

| 文件 | 用途 |
|------|------|
| `server/internal/models/models.go` | 新增 `ItemDistribution`, `ItemDistributionReceipt`, `Organization` |
| `server/internal/services/distribution_service.go` | 业务逻辑（下发、收回、查询、权限检查、Fork） |
| `server/internal/handlers/distribution.go` | REST API handlers |
| `server/internal/notification/types.go` | 新增事件类型常量 |
| `server/cmd/migrate/main.go` | AutoMigrate 新表 + backfill `organizations` |
| `server/internal/handlers/recommend.go` | 修改 favorite 列表查询，集成下发记录 |
| `server/cmd/api/main.go` | 注册新路由 |

---

## 13. 验证方案

### 13.1 单元测试

- `distribution_service_test.go`
  - 下发给单个用户
  - 下发给组织（批量 receipt 创建）
  - 权限检查（仅 platform_admin 可下发，创建者/Repo 管理员禁止下发）
  - 收回后收藏列表不可见
  - Fork 后新 Item 独立存在
- `org_membership_sync_test.go`（§9.3 OrgMembershipSyncWorker）
  - 用户加入 org → 自动收到该 org 所有 active distribution 的 unread receipts
  - 用户加入 org → 已存在的 receipt（ON CONFLICT）不重复创建
  - 用户加入 org → 触发 `item.distributed` 通知投递
  - 用户离开 org → `unread` / `read` receipts 置为 `dismissed`
  - 用户离开 org → `accepted` receipt **保留**（不惩罚已接纳下发的用户）
  - 用户离开 org → 已 fork（`ForkedItemID` 非空）的 receipt **保留**
  - Gitea team API 失败 → receipts **不写**（保证 Gitea 拒绝时用户看不到下发）
  - Gitea team API 成功但 receipts 写入失败 → 退避重试 + 日 cron 校对兜底

### 13.2 集成测试

完整流程：
1. 创建 Item
2. 下发给组织
3. 组织内用户登录
4. `GET /items?favorited=true` 验证下发物品出现
5. 收回下发
6. 验证收藏列表消失
7. 验证 `NotificationLog` 有下发通知记录

### 13.3 手动验证

- Swagger UI 或 curl 测试 `POST /items/:id/distribute`
- 以接收用户身份验证收藏列表
- 测试 dismiss / fork / read 操作

---

## 14. 风险评估

| 风险 | 影响 | 缓解措施 |
|------|------|---------|
| 大组织下发性能（1000+人）| 下发接口响应慢 | 批量 INSERT receipts，使用 GORM 事务；如仍慢可改为异步队列 |
| editable 权限边界模糊 | 非 Repo 成员修改 Item 引发困惑 | 修改记录中明确标记 `UpdatedBy`；UI 中显示"由 XX 下发，可编辑" |
| Organization backfill 数据量大 | 迁移耗时 | 使用分批 backfill，每次 1000 条 |
| 与现有 favorite 计数冲突 | favorite_count 不准确 | receipt 创建/删除时不修改 favorite_count，仅通过 UNION 查询合并 |

---

## 附录：设计决策记录 (ADR)

### ADR-1：Receipt 预创建 vs 懒创建

**决策**：采用预创建模式（下发时批量插入 receipts）  
**理由**：
- 与现有 GORM 事务模式一致，代码简单
- 查询收藏列表时直接 JOIN，无需实时解析组织成员关系
- 支持用户单独 dismiss（需要持久化记录）

### ADR-2：Fork 目标 Registry

**决策**：自动创建用户个人 internal Registry  
**理由**：
- 无需前端交互，用户体验流畅
- 与现有 Repo/Registry 模型兼容
- 隔离用户 Fork 的内容与公共仓库

### ADR-3：editable 权限实现方式

**决策**：临时授予编辑权限（不提升 RepoMember 角色）  
**理由**：
- 最小权限原则，仅对下发 Item 生效
- 不污染 Repo 成员关系
- 实现简单：在 `canMutateItem` 中增加 receipt 检查即可

### ADR-4：组织成员事后变更的 receipt 维护策略

**决策**：用 `OrgMembershipSyncWorker` 监听 `user.organization_changed` 事件，**事后**维护 `item_distribution_receipts`，无需新增 `user_gitea_team_binding` 表。

**背景**：ADR-1 的"预创建 receipts"策略只覆盖**下发那一刻**在 org 内的用户。事后加入 org 的用户永远不会自动收到 receipt——这是 ADR-1 的盲区。

**方案**：
- 用户加入 org X：扫描 `scope_type='organization' AND target_id=X AND status='active'` 的所有 `item_distributions`，批量 INSERT receipts（`receiptStatus='unread'`），触发 `item.distributed` 通知
- 用户离开 org X：将 receipts 置为 `dismissed`，但**保留** `receiptStatus='accepted'` 和已有 `ForkedItemID` 的记录（用户已"拿走"该能力项，回收等于惩罚）
- 失败重试与 ROADMAP Phase 2.5 `OrgGiteaSyncWorker` 共用退避队列；事务保证 receipts 与 Gitea team 同步语义上原子（先 Gitea team 成功再写 receipts）

**理由**：
- 不引入新表：复用 `item_distributions` + `item_distribution_receipts` + `gitea_admin_audit_log`
- worker 无状态：fire-and-forget + 退避重试即可，无需 `sync_status` 状态机
- 接受最终一致性：用户加入 org 后 1-5 秒内 receipt 出现（与 Gitea team 同步窗口对齐），用户重试或刷新即可

**被否决方案**：
- 新增 `user_gitea_team_binding(user_id, org_id, team_id, sync_status)` 表：fork 中间件不校验 team 成员关系（V4 §6.6 明文不在中间件做 team 校验），状态机是过度设计
