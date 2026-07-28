# 角色与权限矩阵

> 系列文档之五，总览见 [`GIT_UX_JOURNEYS_OVERVIEW.md`](./GIT_UX_JOURNEYS_OVERVIEW.md)。
> 角色跨越所有旅程，独立成篇。本篇先厘清现状四层权限模型，再走查 Git 方案下每层的映射。
> 判定图例：✅ 天然满足 · 🔧 补足后满足 · 📌 需决策

## 来源标注约定

`[现状]` 代码已实现 · `[V4 §x]` V4 原文写了 · `[推导]` V4 未写但属架构必然要求 · **`[新增建议]`** ⚠️ 本文提议的改进（汇总在文末，可独立取舍）

> **本篇性质说明**：§3 的 `business_admin` 精简是**独立的权限模型优化建议**，与 Git 方案迁移无依赖关系——放在本系列只因角色模型需要一并厘清。评审 Git 迁移时可跳过 §3。

---

## 1. 现状：四层权限，来源各不相同

关键认识：**这四层不是一套角色表，而是四种独立的授权来源**。它们各自解决不同问题，混在一起看容易误解。

| 层 | 角色 | 存储 | 授予方式 |
|---|---|---|---|
| **① 系统级** | `platform_admin` / `business_admin` | `user_system_roles` 表（DB CHECK 约束只允许这两个值，`20260406100000` 迁移） | 平台管理员显式授予 |
| **② 组织级** | 部门主管 | **不存储**——由组织树拓扑实时推导 | 自动：非叶子部门成员 ⇒ 管辖该子树 |
| **③ 资源级** | repo `owner` / `admin` / `member` | `repo_members` 表 | 仓库所有者邀请 |
| **④ 内容级** | 下发接收方（`readonly` / `dismissible`） | `item_distributions` + receipts | 下发者投放 |

### 1.1 系统级：两个角色，继承关系简单

```
platform_admin  >  business_admin
```

| | `platform_admin` | `business_admin`（可以考虑去除） |
|---|---|---|
| 授予/撤销系统角色 | ✅ | ❌ |
| 平台配置、通知渠道、公告 | ✅ | ❌ |
| 大客户品牌配置 | ✅ | ❌ |
| 审计日志查询 | ✅ | ❌ |
| 全平台下发记录与回执查看 | ✅ | ❌ |
| 成员管理后台 | ✅ | ❌ |
| **看板全局可见（跨所有部门）** | ✅ | ✅ |
| **菜单入口可见性** | ✅ | ✅ |
| **下发能力** | ✅（无限范围） | ❌ |

**`business_admin` 目前实际只有两个作用**：

1. **看板全局 scope** —— `hasAllAccess`（`authz/scope.go:149-157`）：拥有该角色即可见所有部门数据，且不经过 dept-sync（dept-sync 挂了也稳）
2. **菜单可见性** —— `ResourceRegistry` 按角色决定菜单项是否渲染（`authz/registry.go`）

**中间件挂载现状**：`RequirePlatformAdmin` 挂在 8 处（admin 路由组、enterprise、notification、settings、systemrole 等）；`RequireBusinessAdminOrAbove` **在 main.go 的路由注册里没有挂载点**——它只通过 `hasAllAccess` 的角色展开间接生效。

### 1.2 组织级：不是角色，是拓扑

部门主管**没有对应的角色记录**，完全由组织树形状推导（`ledDepartmentsFor`，`distribution_service.go:194-229`）：

> 用户所属部门若是**非叶子节点**（有子部门）⇒ 管辖该部门整棵子树；叶子部门成员 ⇒ 不管辖任何范围。

代码注释明确："This needs no leader_id / position / 工号 — only the operator's department memberships and the tree shape."

**收益**：组织架构调整后授权自动跟随，无需维护主管名单。详见下发者篇 D4.5。

### 1.2.1 同一棵组织树上有两套判定规则

盘查 kanban 页面时发现：**看板可见范围**与**下发管辖范围**都建立在 dept-sync 的组织树上，但判定规则完全不同。

| | 看板可见范围 | 下发管辖范围 |
|---|---|---|
| 判定方式 | **显式授权** | **纯拓扑推导** |
| 规则 | 自己所属部门 + `kanban.scope.dept` / `kanban.scope.all` grant 授予的额外前缀 | 非叶子部门成员 ⇒ 管辖该子树，**无需任何 grant** |
| 实现 | `authz/scope.go:89` `ResolveUserScope()`；额外前缀 `:180` `extraVisibleDeptPrefixes()`，`pathHasPrefix` 前缀下沉 | `distribution_service.go:194` `ledDepartmentsFor()` |
| 授权 UI | `admin/pages/permissions.tsx:35-36`（`SCOPE_ALL_CODE` / `SCOPE_DEPT_CODE`） | 无（自动推导） |
| 数据源 | dept-sync 部门树 | dept-sync 部门树（**同一份短 TTL 缓存**，`distribution_service.go:190-191` 注释明确） |

**含义**：一个人可能「能在看板上看到 A 部门数据」却「不能向 A 部门下发」，反之亦然——两者是独立的授权面。这是有意设计（见 §1.3），但**两套规则并存增加了理解成本**，且组织调整时两边的生效时机都依赖同一缓存刷新。

`[推导]` Git 方案对这两套规则均无影响——它们完全不碰能力项内容（不读 items/versions/content），耦合点仅在 dept-sync 缓存层。

### 1.3 一条关键的隔离设计

`distribution_service.go:120` 的注释值得单独拎出来：

> `unlimited` is intentionally **NOT** widened to `business_admin` / kanban "see all" — **viewing scope must not bleed into push power**.

即：**看得见 ≠ 推得动**。`business_admin` 有全局查看权，但没有任何下发权。这是有意的权限隔离，任何角色精简都必须保住这条边界。

---

## 2. Git 方案下的映射

| 层 | Git 方案下如何承载 | 判定 |
|---|---|---|
| **① 系统级** | 保持在 costrict-web DB。V4 §2.2 不重写业务字段，角色表不受影响 | ✅ |
| **② 组织级** | 保持在 costrict-web + dept-sync。不经过 Gitea。`[V4 §8.1]` 原文明确「dept-sync \| 部门树 + 成员关系**真相源**」——V4 已确认其地位，无需另行决策 | ✅ |
| **③ 资源级** | **迁移点**：`repo_members` 的语义与 Gitea collaborator 高度重合，V4 §4.7 的内容访问层直接查 Gitea permission。两套成员表需要合并或建立同步 | 🔧 |
| **④ 内容级** | 下发状态机保持在 costrict-web；内容授权如何传给 Gitea 见下发者篇 D9（📌 P-1） | 🔧 |

### 2.1 ③ 资源级的合并选项

现状 `repo_members{owner/admin/member}` 与 Gitea 的 collaborator 权限（read/write/admin）语义近似但不等价。三种处理：

| 选项 | 做法 | 代价 |
|---|---|---|
| **A 单向同步** | costrict-web 保持 `repo_members` 为真相源，变更时同步到 Gitea collaborator | 两套数据需对账，但业务语义可控 |
| **B Gitea 为准** | 废弃 `repo_members`，成员管理 UI 直接读写 Gitea collaborator API | 语义受限于 Gitea 模型；邀请流程要重做 |
| **C 双轨** | 平台内的可见性用 `repo_members`，Git 操作权限用 collaborator | 用户会看到两套权限，易困惑 |

三个选项均需修改 V4 §4.7「纯 Gitea visibility 透传」的既有决策（选项 A/C 引入平台侧 ACL；选项 B 则要求现有邀请流程全部重做），**因此本文不预设倾向，列为总览 P-1 的一部分交由 V4 作者定夺**。

---

## 3. 关于精简 `business_admin`（`[新增建议]`，与 Git 迁移无关）

现状它只承担"看板全局可见 + 菜单可见性"，且没有独立的路由中间件挂载点。评估如下：

### 3.1 可以去掉的理由

- 职责单薄：两个作用都可由**直接权限授予**替代——`hasAllAccess` 已经支持 `ScopeAllPermission` 的 per-user 直接授予路径（`scope.go:159-168`），不依赖角色
- 认知成本：`platform_admin` 与 `business_admin` 的边界需要额外解释，而实际差异只在"能不能看全部部门"
- 角色继承逻辑（`systemrole/types.go` 的角色展开）可一并简化

### 3.2 去掉前必须处理的

| # | 事项 | 说明 |
|---|---|---|
| 1 | **存量用户迁移** | 现有 `business_admin` 用户需转为 `ScopeAllPermission` 的直接授予，否则会突然失去看板全局视野 |
| 2 | **DB 约束与展开逻辑** | `20260406100000` 的 CHECK 约束、`systemrole/types.go` 的角色展开、`authz/registry.go` 的菜单注册表都要同步改 |
| 3 | **菜单可见性替代方案** | `ResourceRegistry` 目前按角色配菜单，去掉后需改为按权限码 |
| 4 | **保住隔离边界** | §1.3 那条"看得见 ≠ 推得动"必须继续成立——直接授予 `ScopeAllPermission` 同样不能带来下发权 |
| 5 | **`RequireBusinessAdminOrAbove` 中间件** | 目前无挂载点，可直接删除；但需确认没有其他仓库/分支在用 |

### 3.3 建议

**精简为「平台管理员 + 部门主管」两层是可行的**，且与 Git 方案无冲突（角色层完全落在 costrict-web）。建议做法：

```
platform_admin        保留（系统治理）
business_admin        废弃 → 迁移为 ScopeAllPermission 直接授予
部门主管               保留（拓扑推导，无需改动）
repo owner/admin/member   见 §2.1，与 Gitea collaborator 合并
下发接收方             保留（内容级，与角色层正交）
```

这样系统级只剩一个角色，"全局可见"退化为一个可授予的权限点——概念更少，且更灵活（可以给任意人开全局视野而不必给管理员头衔）。

---

## 4. 补足项清单

| # | 补足项 | 来源 | 组件 | 出处 |
|---|---|---|---|---|
| R-a | 📌 `repo_members` 与 Gitea collaborator 的关系（三选一，见 §2.1；属总览 P-1 范围） | `[推导]` | web / gitea / db | §2.1 |
| R-b | 权限矩阵回归用例：重点覆盖「看得见 ≠ 推得动」边界 | `[推导]` | web | §1.3 |

> ~~部门真相源确认~~ 已移除：`[V4 §8.1]` 原文「dept-sync \| 部门树 + 成员关系**真相源**」已明确，本文不应重开。

---

## 5. 可选改进（`[新增建议]`，与 Git 迁移无关）

| # | 改进 | 动机 | 组件 |
|---|---|---|---|
| **RE-1** | `business_admin` 精简：存量用户转 `ScopeAllPermission` 直接授予、DB CHECK 约束调整、角色展开简化、菜单注册表改按权限码 | 该角色实际只承担"看板全局可见 + 菜单可见性"，且 `hasAllAccess` 已支持 per-user 直接授予路径（`scope.go:159-168`） | web / db |
| **RE-2** | 删除无挂载点的 `RequireBusinessAdminOrAbove` 中间件 | 该中间件在路由注册中无挂载点，仅通过角色展开间接生效 | web |

> 两项均为独立的权限模型优化，**不在 Git registry 迁移范围内**，建议单独立项。执行前须处理 §3.2 列出的 5 项前置事项，尤其是保住 §1.3 的「看得见 ≠ 推得动」边界。
