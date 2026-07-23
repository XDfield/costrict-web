# 术语表与命名规范（costrict-web 跨文档通用）

> **定位**：本文档统一定义 `docs/repo-management/` 与 `docs/identity-tenant/` 共用的核心术语与 ID 命名约定。任一文档新增术语前先在此登记，避免含义漂移。
>
> **维护原则**：单点真相——某个 ID 的格式 / 类型 / 真相源以本文档为准，子文档只允许引用，不允许重新定义。

---

## 1. 术语定义

| 术语 | 英文 / 代码符号 | 含义 | 真相源 |
|---|---|---|---|
| 用户 | user | cs-user 内部用户实体（已开户、有 `u_` 前缀 ID） | cs-user 服务 `users` 表 |
| 身份 | identity | 用户在某个 IdP（GitHub / Feishu / LDAP / IdTrust / AAD 等）上的可认证凭据；一个 user 可绑定多个 identity | cs-user `user_identities` 表 |
| 团队 | team | 平台级组织单元，跨 capability 共享（不归属 workflow / KB 任意单一业务） | 外部 `org-team-service` 模块 |
| 部门 | department / org | 树形组织结构；与 team 在 v2 决策后合并为同一概念（外部模块管理） | 外部 `org-team-service` 模块 |
| 租户 | tenant | 多租户隔离边界；一个客户企业 = 一个 tenant | cs-user `tenants` 表（[MULTI_TENANCY_DESIGN §7](./identity-tenant/MULTI_TENANCY_DESIGN.md)）；Stage D 抽离前物理位于 `costrict-web/server/migrations/` |
| 团队命名空间 | team namespace / team ns | per-team 的 Gitea org `t-<team_short_id>`，承载 KB / workflow repo | @server 镜像于 Gitea，由 `members:sync` lazy 创建 |
| KB 仓库 | kb repo | 某个 (team, code_repo) 对应的知识库 git repo | @server 推导路径，无绑定表 |
| Workflow 类型仓库 | wf type repo | 某个 (team, def_slug) 对应的工作流定义仓库；main 存 def canonical | @server 推导路径，无绑定表 |
| Workflow 实例 | wf instance | workflow 类型仓库上的一个 `inst-<inst_short>` 分支 | @server 在 `workflow/init` 时创建 |
| 平台管理员 | platform_admin | CoStrict 内部超级管理员，跨 tenant | `users.system_role = 'admin'` |
| 租户管理员 | tenant_admin | 客户企业管理员，限本 tenant | `tenant_admins` 表 |
| 普通成员 | tenant_member | tenant 内普通用户 | — |

---

## 2. ID 命名规范

### 2.1 用户标识

| 字段名 | 格式 | 出现位置 | 说明 |
|---|---|---|---|
| `user_id` | `u_[a-f0-9]{12}`（cs-user 内部 12 hex） | cs-user API / webhook payload `subject` / 业务表 `created_by` FK | 业务侧主键 |
| `universal_id` | Casdoor JWT 内的 `universal_id`（brokered IdP），缺失时 fallback 到 `sub` | JWT claim（costrict 历史字段） | cs-cloud / quota-manager 主用，零侵入兼容；保留 Casdoor 原值用于跨系统对齐 |
| `sub` | `usr_<uuid>`（cs-user 内部 Subject） | JWT claim（OIDC 标准） | 通用兼容，cs-user 内部主键 |
| `user.id`（嵌套） | 同 `sub`（Phase B 才发出） | JWT claim `user` Map 的 `id` 字段 | 完整用户快照字段 |
| `jwt_user_id` | — | gin context key（middleware 注入） | SDK 取值优先级：`universal_id` → `sub` → `user.id` |

> **取值约定（2026-07-23 修订）**：原"四字段同值约定"（`user_id` ≡ `universal_id` ≡ `sub` ≡ `user.id`）已废弃。当前规则：
> - `sub` 始终是 cs-user 内部 Subject（`usr_<uuid>`）
> - `universal_id` 优先取 Casdoor 原始 JWT 的 `universal_id`（brokered IdP 场景下与 Casdoor 生态对齐），缺失时 fallback 到 `sub`
> - `user.id` 嵌套快照与 `sub` 同值（Phase B 才发出）
> - 下游业务侧仍通过 SDK 统一读 `jwt_user_id`，SDK fallback 顺序 `universal_id` → `sub` → `user.id` 不变
>
> 详见 [MULTI_TENANCY_DESIGN §12.7](./identity-tenant/MULTI_TENANCY_DESIGN.md) "universal_id 取值约定"。

### 2.2 团队 / 租户标识

| 字段名 | 格式 | 类型 | 说明 |
|---|---|---|---|
| `team_id` | UUIDv4 | string | 外部 `org-team-service` 主键；team ns 推导输入 |
| `team_short_id` | UUIDv4 去连字符后前 8 hex 小写（如 `7f3c9a1e`） | string | team ns Gitea org 名的短后缀；纯函数 `teamShortId(team_id)` 推导（[KB_REPO_PATH_ALGORITHM §3.0](./repo-management/KB_REPO_PATH_ALGORITHM.md)）|
| `team_short` | 等同 `team_short_id` | string | **别名**，建议统一使用 `team_short_id`；现存文档两者并存时视为同义 |
| `tenant_id` | UUID（建议 UUIDv4） | string | cs-user `tenants.id` 主键（业务表通过 FK 引用） |
| `tenant_slug` | `[a-z][a-z0-9-]{2,30}`（如 `acme`） | string | URL 友好的 tenant 别名 |

> **禁止**：用 `team_short_id` 直接做业务表 FK——它是 8 hex 派生值，碰撞概率非零；业务 FK 必须用完整 `team_id` UUID。

### 2.3 仓库与分支命名

| 名称 | 格式 | 用途 |
|---|---|---|
| team ns org | `t-<team_short_id>` | per-team Gitea org（如 `t-7f3c9a1e`） |
| KB repo | `kb-<host>__<escaped_segments>` | team ns 内的 KB 仓库 |
| Workflow type repo | `wf-<def_slug>` | team ns 内的工作流定义仓库 |
| Workflow instance branch | `inst-<inst_short>` | `inst_short` = instance UUIDv4 前 8 hex；分支落在 type repo 上 |
| Node PR branch | `node/<seq>-<slug>` | 节点执行器推送的临时分支；PR base = `inst-<inst_short>`（不是 main） |
| Def evolution PR branch | `feat/<change-summary>` 或类似；PR base = `main` | def canonical 演进；详见 [CSC_WF_SUBCOMMAND_CONTRACT §4.6](./repo-management/CSC_WF_SUBCOMMAND_CONTRACT.md) |

---

## 3. 真相源矩阵

| 数据 | 真相源 | 业务访问方式 |
|---|---|---|
| 用户 profile（邮箱 / 姓名 / 头像） | cs-user `users` 表 | REST API / JWT `user` Map 快照 |
| 用户身份绑定（identity） | cs-user `user_identities` 表 | cs-user API |
| 用户归属哪些 team | 外部 `org-team-service` | `OrgService.ListUserTeams()`（in-process 缓存） |
| Team / department 结构 | 外部 `org-team-service` | `OrgService.GetDepartmentTree()` |
| Team 成员列表 | 外部 `org-team-service`（canonical）→ @server 镜像到 Gitea team ns org | OrgService 读外部；Gitea API 读镜像 |
| Tenant 列表 / 配置 | cs-user `tenants` 表 | cs-user `tenants` API（Stage D 抽离前由 `costrict-web/server` 内 cs-user 模块提供） |
| 用户 Gitea 账号 | Gitea `users` 表 | Gitea admin API（由 cs-user 自动开户） |
| KB / WF repo 路径 | 纯函数推导（无持久化） | [`KB_REPO_PATH_ALGORITHM`](./repo-management/KB_REPO_PATH_ALGORITHM.md) / [`WORKFLOW_REPO_PATH_ALGORITHM`](./repo-management/WORKFLOW_REPO_PATH_ALGORITHM.md) |
| Workflow def canonical | type repo `main` 分支的 `definition.yaml` | git pull / Gitea API |

---

## 4. 命名规范统一原则

1. **单一规范字段名**：每个概念只允许一个规范字段名；其它写法视为别名并标注 `(alias)`。
2. **缩写避免歧义**：`team_short` 与 `team_short_id` 在 v2.0 后视为同义，**新文档统一用 `team_short_id`**；`team_short` 仅在已落地的算法描述中保留。
3. **大小写**：所有 hex 派生 ID（`team_short_id` / `inst_short`）统一小写。
4. **版本号**：spec 文档主版本（如 `v2.17`）与卫星文档（如 `KB_REPO_PATH_ALGORITHM v2.0`）独立演进，spec 引用卫星文档时**必须显式标版本号**。
5. **路径前缀**：
   - team ns：`t-`（如 `t-7f3c9a1e`）
   - KB repo：`kb-`
   - Workflow type repo：`wf-`
   - 实例分支：`inst-`
   - 节点分支：`node/`
6. **内部接口前缀**：所有内部服务间调用统一 `/api/internal/*`；用户面接口（csc / portal）保持 `/api/*`。

---

## 5. 跨文档引用索引

| 主题 | 文档 |
|---|---|
| Repo 管理主规范 | [`repo-management/REPOSITORY_MANAGEMENT_SPEC.md`](./repo-management/REPOSITORY_MANAGEMENT_SPEC.md) |
| Team ns + KB ensure + workflow init 内部接口 | [`repo-management/TEAM_NAMESPACE_API.md`](./repo-management/TEAM_NAMESPACE_API.md) |
| org-team-service webhook 事件类型 + payload | [`identity-tenant/TEAM_ORG_UNIFICATION.md`](./identity-tenant/TEAM_ORG_UNIFICATION.md) §4 |
| 多租户设计 | [`identity-tenant/MULTI_TENANCY_DESIGN.md`](./identity-tenant/MULTI_TENANCY_DESIGN.md) |
| 用户中心 + JWT 签发 | [`identity-tenant/USER_CENTER_DESIGN.md`](./identity-tenant/USER_CENTER_DESIGN.md) |
| 身份架构路线图 | [`identity-tenant/IDENTITY_ARCHITECTURE_ROADMAP.md`](./identity-tenant/IDENTITY_ARCHITECTURE_ROADMAP.md) |
| 联邦身份决策 | [`identity-tenant/IDENTITY_FEDERATION_DECISION.md`](./identity-tenant/IDENTITY_FEDERATION_DECISION.md) |
| cs-user 服务设计 | [`identity-tenant/CS_USER_SERVICE_DESIGN.md`](./identity-tenant/CS_USER_SERVICE_DESIGN.md) |
| csc KB 子命令 | [`repo-management/CSC_KB_SUBCOMMAND_CONTRACT.md`](./repo-management/CSC_KB_SUBCOMMAND_CONTRACT.md) |
| csc WF 子命令 | [`repo-management/CSC_WF_SUBCOMMAND_CONTRACT.md`](./repo-management/CSC_WF_SUBCOMMAND_CONTRACT.md) |
