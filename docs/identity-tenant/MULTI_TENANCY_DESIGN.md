# 多租户机制设计提案（cs-user Multi-Tenancy）

| 字段 | 内容 |
|---|---|
| 状态 | Draft · 评审中（v2，2026-07-15 v3 决策同步：team 同步归 @server） |
| 作者 | CoStrict 团队 |
| 创建日期 | 2026-07-10 |
| 最近修订 | 2026-07-15（v3 决策同步：team-level GitServerAdapter 工作归集到 @server；cs-user 仅保留 user-level Git server 操作。详见 [`TEAM_ORG_UNIFICATION.md`](./TEAM_ORG_UNIFICATION.md) ADR-3 v3） |
| 评审范围 | cs-user（新）/ @server（业务服务，承担 team-level GitServerAdapter + 业务侧 repo 操作）/ costrict-web / casdoor / app-ai-native / csc / cs-cloud |
| 关联文档 | [`CS_USER_SERVICE_DESIGN.md`](./CS_USER_SERVICE_DESIGN.md)（cs-user 服务化基线，本提案对其扩展）、[`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md)（用户中心 4 层模型）、[`MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md`](../proposals/MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md)（多 provider 绑定方案 B）、[`IDENTITY_FEDERATION_DECISION.md`](./IDENTITY_FEDERATION_DECISION.md)（身份联邦 v3 决策）、[`CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md`](../repo-management/CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md)（V4 架构基线）、[`TEAM_ORG_UNIFICATION.md`](./TEAM_ORG_UNIFICATION.md)（ADR-3 v3：team 同步归属） |

> 本提案在 [`CS_USER_SERVICE_DESIGN.md`](./CS_USER_SERVICE_DESIGN.md) 已决策的"cs-user 服务化 + 4 层 UserInfo + 企业身份可配置映射"基础上引入**多租户能力**——支持不同企业作为独立 tenant 共用一套 cs-user 服务，数据逻辑隔离、身份认证隔离、企业身份映射按租户定制。所有未提及的设计点严格继承自 cs-user / user-center 既有决策；本提案只补充 tenant 维度的扩展。

---

## TL;DR

**三个核心决策**：

1. **共享基础设施 + 行级隔离（row-level multi-tenancy）**：所有 cs-user 表新增 `tenant_id` 列 + 索引；不拆分实例、不拆分 DB。单个 cs-user 服务承载所有 tenant；PostgreSQL Row Security Policy（RLS）做兜底防数据泄漏。规模化后允许个别大客户升级为独立实例（silos 模式）。

2. **租户识别（tenant resolution）三层 fallback**：① **子域名**（`acme.cs-user.example.com` → tenant slug `acme`）→ ② **登录邮箱域名**（`alice@acme.com` → tenant by `email_domain=acme.com`）→ ③ **显式选择**（用户从 tenant 选择器选）。三层组合覆盖企业 SSO、邮箱邀请、混合场景。

3. **租户作用域的用户唯一性 + 全局不可变 user_id**：`users.id` 保持 UUID 全局唯一不变；`username` 唯一性从全局放宽到 `(tenant_id, username)`——同一 username 可在不同 tenant 注册。`external_key` 同样从全局放宽到 `(tenant_id, external_key)`，但跨 tenant 默认禁止复用（防同名攻击）。每 tenant 独立 provider mapping yaml，企业身份字段按 tenant 定制。

4. **新用户 onboarding（username / display_name 解耦）**：首次登录后 `username` 走严格确认链路——支持 tenant 级 `auto`（IdP claim 自动映射，企业场景免填写）/ `user_input`（UI 填写）/ `hybrid`（默认，先 auto 失败 fallback 到 UI）三种模式，一经确认 v1 不可变。`display_name` 走宽松链路，用户随时可改。业务 JWT 在 onboarding 未完成时注入 `onboarding_required` 字段供前端识别（§11.3）。

5. **多 IdP 绑定的不覆写原则 + 雇佣上下文与 IdP 解耦**：用户基本身份字段（display_name / avatar_url / locale / email 等）首次登录时由当时绑定的 IdP 一次性填充，后续绑新 IdP **不覆写**（append-only + first-write-wins）。雇佣上下文（enterprise_identities.*）**归属到 user**，由 tenant 在 `employment_providers` 配置中显式声明哪些 IdP 可作为雇佣上下文提供方；非 employment provider（如 GitHub / 飞书）登录 / 绑定 / 切换都不影响雇佣字段，避免社交 IdP 污染企业组织数据（§11.4）。

**一句话价值**：一套 cs-user 服务支持 B2B SaaS（多企业客户）+ 私有化（多业务线分离）+ 客户独立 IdP 接入；不增加部署复杂度；用户身份与企业身份天然隔离。

---

## 目录

> 本文件 3114 行 / 158KB，**按用途快速跳转**：

| 你想找什么 | 直接看 |
|---|---|
| tenant 是什么 / 怎么识别 | §3-5（L169-332） |
| 数据库表结构 | §7-9（L573-827）+ 附录 A（L2801） |
| 数据隔离怎么做（RLS / 应用层） | §10（L828）+ 附录 C（L2908） |
| 登录链路 / onboarding / 多 IdP 绑定 | §11（L925） |
| JWT claims 字段表 | §12（L1377，重点 L1838 context key 表） |
| 权限模型（platform_admin / tenant_admin） | §14-16（L1898-2024） |
| 企业身份字段映射 yaml | §17-19（L2027-2251）+ 附录 B（L2851） |
| webhook 事件 | §21（L2468）+ 附录 D（L2987） |
| API（platform admin / tenant admin） | §23-25（L2528-2665） |
| 迁移路径 / 风险 | §26-28（L2668-2758） |
| 已决策 vs 开放问题 | §29-30（L2761-2800） |

---

### 完整章节索引（含行号）

**Part I：动机与目标**（L95）
- §1. 背景与痛点（L97）
- §2. 目标与非目标（L141）

**Part II：租户模型**（L169）
- §3. 共享 vs 独立 vs 混合部署（L171）
- §4. tenant 实体定义与生命周期（L205）
- §5. tenant resolution（子域 / 邮箱域 / 显式选择）（L263）
- §6. 跨租户约束（L333）

**Part III：数据模型扩展**（L573）
- §7. tenants 表 + tenant_admins 表（L575）
- §8. 租户作用域表 schema 扩展（L653）
- §9. tenant 级配置：provider mapping + features（L754）
- §10. 数据隔离机制（应用层 + RLS 兜底）（L828）

**Part IV：认证流程扩展**（L923）
- §11. 租户识别登录链路（L925）— 含 §11.3 onboarding / §11.4 多 IdP 不覆写原则
- §12. JWT 机制：双层契约（Casdoor JWT / 业务 JWT）+ claims + 选型（L1377）
- §13. 跨租户 SSO 与身份迁移（L1868）

**Part V：权限模型**（L1896）
- §14. platform_admin / tenant_admin / tenant_member 三级（L1898）
- §15. API 鉴权与 tenant 隔离规则（L1930）
- §16. 越权防护与审计（L2002）

**Part VI：企业身份与租户配置**（L2025）
- §17. tenant 级 provider mapping yaml（L2027）
- §18. tenant 级 enterprise schema 扩展字段（L2067）
- §19. tenant 级 IdP 接入（L2118）

**Part VII：跨服务边界**（L2252）
- §20. 通用 Git Server 适配层 + tenant 独立实例（L2254）
- §21. webhook：tenant-scoped 订阅与事件（L2468）
- §22. costrict-web 业务侧 tenant 上下文传递（L2499）

**Part VIII：API 变更**（L2526）
- §23. API tenant 上下文传递（L2528）
- §24. platform admin API（L2603）
- §25. tenant admin API（L2634）

**Part IX：实施与迁移**（L2666）
- §26. 默认 tenant 引导（brownfield 迁移）（L2668）
- §27. 分阶段切换路径（L2709）
- §28. 风险与对策（L2739）

**Part X：已决策项与开放问题**（L2759）
- §29. 已决策项（L2761）
- §30. 开放问题（L2780）

**附录**（L2801）
- A. tenants 表完整 schema（L2801）
- B. tenant-scoped provider mapping yaml 示例（L2851）
- C. RLS（Row Security Policy）配置参考（L2908）
- D. tenant.* webhook 事件 payload schema（L2987）
- E. 现有调用点改造清单（L3102）

---

### 外部引用本文件的位置

> 拆分 / 重排时务必同步更新以下引用——锚点失效会破坏跨文档导航。

| 引用方 | 引用章节 | 用途 |
|---|---|---|
| [`IDENTITY_ARCHITECTURE_ROADMAP.md`](./IDENTITY_ARCHITECTURE_ROADMAP.md) | §5 / §6 / §6.5.1 / §7 / §8 / §9.2 / §10 / §11.1 / §11.4 / §11.4.2 / §11.4.4 / §12 / §12.1 / §14 / §15 / §16 / §17 / §19 / §21 / §22 / §24 / §25 / §26 | 实施路线图 A1-E4 任务 ↔ 设计章节回链 |
| [`GLOSSARY.md`](../GLOSSARY.md) | §7 | tenant 概念真相源 |
| [`TEAM_ORG_UNIFICATION.md`](./TEAM_ORG_UNIFICATION.md) | ADR-3 v3 | team 同步职责反转记录 |

---

# Part I：动机与目标

## 1. 背景与痛点

### 1.1 现状：cs-user 默认单租户

[`CS_USER_SERVICE_DESIGN.md`](./CS_USER_SERVICE_DESIGN.md) 与 [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md) 设计的 cs-user 服务默认**单租户模型**：

- `users.username` 全局唯一
- `user_auth_identities.external_key` 全局唯一
- `provider-mapping.yaml` 单份全局配置
- Casdoor 单实例，所有 source 共享
- Gitea 单实例，所有 org 共享
- JWT claims 不含 tenant 标识

**问题场景**：

| 场景 | 单租户下的痛点 |
|---|---|
| **B2B SaaS**：服务多家企业客户 | 不同客户的员工同名（两家人力都有"张伟"）；不同客户用不同企业 IdP（A 用 idtrust、B 用 Azure AD）；不同客户字段命名差异 |
| **私有化部署多业务线**：集团内多个子公司共用一套 costrict | 子公司 A 与子公司 B 都希望独立管理用户、独立 IdP；但共享同一套底层能力（capability registry / device） |
| **客户独立 IdP 接入**：每客户自带 IdP | 当前 provider-mapping.yaml 全局唯一，新增客户 IdP 字段会污染其他客户配置 |
| **数据合规与隔离**：客户要求"我们的用户数据别人看不到" | 单租户模型无法保证逻辑隔离；只能靠业务层权限校验，缺少 DB 层兜底 |
| **跨租户审计**：审计日志混在一起 | 无法按客户分维度出报表 |
| **计费 / 配额**：按客户计量 | 单租户模型下无 tenant_id 维度 |

### 1.2 触发引入多租户的核心驱动

1. **业务方向**：costrict-cloud 从内部研发工具演进为 B2B SaaS，需要服务多家企业客户
2. **客户独立 IdP**：每家客户自带企业 SSO（idtrust / Azure AD / 飞书 / 钉钉），需要按客户配置
3. **数据合规**：客户合同条款要求"用户数据与其他客户物理/逻辑隔离"
4. **私有化部署灵活性**：单一客户集团内部也可能有"多业务线独立运营"诉求
5. **未来演进**：可能推出"个人版"（tenant = 个人）+ "企业版"（tenant = 公司）分层

### 1.3 已有资产（不推翻重做）

- [`CS_USER_SERVICE_DESIGN.md`](./CS_USER_SERVICE_DESIGN.md)：cs-user 服务化骨架 + 4 层 UserInfo（base / identities / profile / enterprise）
- [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md)：4 层模型（Identity / Account / Profile / Gitea-binding）+ JWT 自签 + webhook 广播
- [`MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md`](../proposals/MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md)：多 provider 绑定 + provider rank 自动升级
- 企业身份可配置映射：`provider-mapping.yaml` + 10 内置 transformer
- 现有 `users` / `user_auth_identities` / `user_profile` 表结构

多租户是这些设计的**正交扩展**——只增加 tenant 维度，不改变既有契约。

---

## 2. 目标与非目标

### 2.1 目标

1. **支持多 tenant 共享一套 cs-user**：单 cs-user 实例承载 N 个企业客户，数据行级隔离
2. **tenant 三层识别**：子域名 / 邮箱域 / 显式选择，覆盖所有常见登录入口
3. **租户作用域用户唯一性**：`(tenant_id, username)` 唯一、`(tenant_id, external_key)` 唯一；允许同名跨租户；禁止 external_key 跨租户复用（防钓鱼）
4. **租户级配置**：每 tenant 独立 `provider-mapping.yaml` 覆盖 + enterprise schema 扩展字段
5. **三级权限模型**：`platform_admin`（CoStrict 内部）/ `tenant_admin`（客户管理员）/ `tenant_member`（普通用户）
6. **DB 层隔离兜底**：PostgreSQL Row Security Policy（RLS）强制 tenant_id 过滤，防止应用层 bug 导致数据泄漏
7. **JWT 携带 tenant 上下文**：`tenant_id` / `tenant_slug` 进 claims，下游业务服务从 JWT 直接拿 tenant
8. **跨服务 tenant 上下文传递**：Gitea org 命名空间 / webhook 事件 / costrict-web 业务请求统一带 tenant_id
9. **tenant 生命周期管理**：创建 / 配置 / 暂停 / 注销 / 数据导出
10. **brownfield 兼容**：现有单租户数据迁移到默认 tenant，零感知切换

### 2.2 非目标

- **不做物理隔离（per-tenant 独立 DB）**：v1 用共享 DB + 行级隔离；个别大客户可走"独立实例"模式（特殊部署形态，不在本提案范围）
- **不引入 organization / team 层级**：tenant 是顶层隔离边界；tenant 内部不再有组织树（业务线 / 部门仍由 `dept-sync` + `user_profile.dept_id` 表达）
- **不做用户跨租户迁移**：一个用户账号（`users.id`）创建后归属固定 tenant，不支持"搬家"；如需切换，新 tenant 重新创建账号
- **不支持用户同时属于多个 tenant**（v1）：v1 严格 1 用户 1 tenant；未来如需多 tenant 成员关系（用户切换 tenant 登录）走独立提案
- **不做 tenant 级定制 UI**：UI 主题、品牌 LOGO 等定制暂不在范围
- **不做 tenant 级密码策略 / 2FA 差异化**：所有 tenant 共用 cs-user 安全策略
- **不替换 Casdoor，但弱依赖 Casdoor**：Casdoor 仅作**多登录源协议适配器**（OAuth 2.0 / OIDC / SAML 转发），cs-user **不消费 Casdoor 的 organization / user / RBAC 模型**。所有 tenant 共享单个 Casdoor organization，tenant 上下文通过 OAuth `state.tenant_id` 由 cs-user 自管（详见 §11.2 / §12.3 双层契约）。multi-organization 模式已否决（依赖过深）。
- **v1 仅实现 gitea_adapter，不绑死 Gitea**：cs-user 通过 `GitServerAdapter` 接口抽象 Git server（详见 §20.2），v1 默认实现 gitea_adapter（含 fork JWT 中间件 + pre-receive hook），GitLab / Forgejo / Gerrit / 裸 Git HTTP 等 adapter 在 v2 实现。**严格 1 tenant : 1 git server 实例绑定**（`tenants.git_server_id` NOT NULL + UNIQUE，详见 §20.3 / §20.4），不再支持「多 tenant 共享一个 Git server 实例 + namespace 隔离」模式；物理共享形态下 namespace 前缀仅作额外防护（§20.5）

---

# Part II：租户模型

## 3. 共享 vs 独立 vs 混合部署

### 3.1 三种模型对比

| 模型 | 描述 | 数据隔离 | 运维成本 | 多租户效率 | 适用场景 |
|---|---|---|---|---|---|
| **Silo（独立基础设施）** | 每个 tenant 独立 cs-user + 独立 DB | 物理 | 高（N 套部署） | 低 | 强合规客户 / 超大客户 |
| **Shared（共享基础设施 + 行级隔离）** | 单 cs-user + 单 DB + `tenant_id` 列 | 逻辑 | 低（1 套部署） | 高 | 主流 B2B SaaS / 私有化 |
| **Hybrid（共享 + 大客户 silo）** | 默认 shared；个别大客户走 silo | 混合 | 中 | 高 | 长期演进 |

### 3.2 推荐：**Shared + 行级隔离（v1）**

**理由**：

1. **运维简单**：1 套 cs-user 服务，监控 / 升级 / 备份统一
2. **资源利用率高**：小客户共享资源池，无需为每客户预留容量
3. **跨租户功能易实现**：platform admin 总览 / 全局审计 / 跨租户搜索
4. **RLS 兜底足够安全**：PostgreSQL Row Security Policy 强制 tenant_id 过滤，DB 层防泄漏
5. **保留升级路径**：未来如需 silo 模式，可平滑迁移单个 tenant（导出 + 独立部署）

**Hybrid 升级路径**（未来）：当某 tenant 用户数 > 10 万 / 月活 / 合规要求物理隔离时，启动 silo 迁移：
- 该 tenant 数据从共享 DB 导出（按 `tenant_id` 过滤）
- 部署独立 cs-user 实例 + 独立 DB
- DNS 切换：`big-customer.cs-user.example.com` → 独立实例
- 共享实例保留 redirect 提示一段时间

### 3.3 v1 不做 silo / hybrid 的理由

- 当前客户规模 < 100 家，shared 模式容量充足
- silo 模式会带来跨实例查询、统一监控、版本同步等工程负担
- 等 ≥ 1 家客户提出硬性合规要求时再启动 hybrid

---

## 4. tenant 实体定义与生命周期

### 4.1 tenant 实体属性

| 字段 | 类型 | 说明 |
|---|---|---|
| `tenant_id` | UUID | 不可变主键 |
| `slug` | VARCHAR(32) | URL-safe slug，用于子域名 / 路径；全局唯一 |
| `display_name` | VARCHAR(191) | 展示名（如 "Acme Corporation"） |
| `status` | VARCHAR(32) | `active` / `suspended` / `deleted` |
| `edition` | VARCHAR(32) | `free` / `team` / `enterprise` / `on_premise` |
| `email_domains` | TEXT[] | 邮箱域名白名单（如 `{acme.com, acme.cn}`），用于邮箱域识别 |
| `features` | JSONB | 功能开关（如 `{ai_agent: true, gitea: true}`） |
| `limits` | JSONB | 配额（如 `{max_users: 500, max_admins: 10}`） |
| `created_at` / `updated_at` / `deleted_at` | TIMESTAMPTZ | 时间戳 |

### 4.2 tenant 生命周期

```
[创建] platform_admin POST /api/platform/tenants
   └─► 校验 slug 唯一 + email_domains 不冲突（不能与已有 tenant 重叠）
       └─► 创建 tenant 行 + 默认 provider mapping（继承全局）
           └─► 指派首位 tenant_admin
               └─► webhook tenant.created（通知 costrict-web 创建对应业务资源）

[配置] tenant_admin POST /api/tenants/:slug/config
   └─► 改 provider mapping / features / limits
       └─► webhook tenant.config_changed

[暂停] platform_admin POST /api/platform/tenants/:id/suspend
   └─► status=suspended → 所有该 tenant 用户登录被拒（30 天 grace）
       └─► webhook tenant.suspended → 下游清理该 tenant 活跃 session

[恢复] platform_admin POST /api/platform/tenants/:id/restore
   └─► status=active → 用户可正常登录
       └─► webhook tenant.restored

[注销] platform_admin POST /api/platform/tenants/:id/delete
   └─► status=deleted + deletion_requested_at（30 天 grace period）
       └─► webhook tenant.deletion_requested
           ├─► 30 天内 tenant_admin 可撤销
           └─► 30 天后 cron 真删：tenant 内所有用户 + identity + profile + enterprise 一并清理
               └─► webhook tenant.deleted (hard)
```

### 4.3 tenant 配额

| 配额项 | 默认值（按 edition） | 触发拦截 |
|---|---|---|
| `max_users` | free=10 / team=100 / enterprise=10000 | 新用户首次登录时拒绝 |
| `max_admins` | free=1 / team=5 / enterprise=50 | admin 角色授权时拒绝 |
| `max_identity_providers` | free=1 / team=3 / enterprise=20 | tenant 配置 IdP 时拒绝 |
| `max_gitea_repos` | free=10 / team=100 / enterprise=10000 | Gitea 创建 repo 时拒绝（业务侧） |

配额超限返回 HTTP 429 + 错误码 `TENANT_QUOTA_EXCEEDED`。

---

## 5. tenant resolution（子域 / 邮箱域 / 显式选择）

### 5.1 三层 fallback 链路

```
用户访问 cs-user 登录页
   │
   ▼
[Try 1] 子域名识别
   URL：https://acme.cs-user.example.com/login
   解析 host 第 1 段：slug = "acme"
   查 tenants WHERE slug = 'acme' AND status = 'active'
   ├─ 命中 → tenant 上下文确定，登录页带上 tenant 品牌
   └─ 未命中 → 继续 Try 2
   │
   ▼
[Try 2] 邮箱域识别（仅在用户输入邮箱后触发）
   用户输入 alice@acme.com
   查 tenants WHERE 'acme.com' = ANY(email_domains)
   ├─ 唯一命中 → tenant 上下文确定
   ├─ 多命中（罕见，多 tenant 重叠域名）→ 跳 Try 3 让用户选
   └─ 未命中 → 继续 Try 3
   │
   ▼
[Try 3] 显式选择
   展示 tenant 选择器（搜索 + 列表）
   用户选 tenant → tenant 上下文确定
   │
   ▼
tenant 上下文写入 cookie + session：
   Set-Cookie: cs_tenant_slug=acme; Domain=.cs-user.example.com; HttpOnly
   跳转该 tenant 的 Casdoor OAuth 入口（多源 UI 仅展示该 tenant 配置的 source）
```

### 5.2 tenant 上下文存储

| 存储 | 范围 | 用途 |
|---|---|---|
| Cookie `cs_tenant_slug` | 浏览器 session | 前端识别当前 tenant |
| Session metadata（Redis） | 服务端 | 后端识别当前 tenant |
| JWT claim `tenant_id` + `tenant_slug` | 跨服务 | 下游业务识别 tenant |

### 5.3 特殊场景

**场景 A：用户首次登录尚未确定 tenant**

- 跳到通用登录页 `https://cs-user.example.com/login`（无子域）
- 输入邮箱 → Try 2 邮箱域识别
- 命中 → 写 cookie + 重定向到 `https://acme.cs-user.example.com/login/callback?...`

**场景 B：tenant 邮箱域冲突**

- `acme.com` 被 Acme Inc 与 Acme Subsidiary 都声明（异常配置）
- Try 2 多命中 → 弹"请选择您所属组织"列表
- 用户选 → 写 cookie

**场景 C：跨 tenant 访问**

- 用户在 tenant A 登录后访问 tenant B 的 URL
- 检测：JWT `tenant_id` 与 cookie `cs_tenant_slug` 对应 tenant 不匹配
- 行为：提示"您当前登录 tenant A，是否切换到 tenant B？"
- 用户确认 → 走 tenant B 登录流程（A 的 session 保留或撤销，取决于配置）

**场景 D：platform_admin 跨租户操作**

- platform_admin 不属于任何特定 tenant
- 通过 `X-Tenant-Id` header 显式指定操作目标 tenant

---

## 6. 跨租户约束

### 6.1 用户唯一性

| 字段 | 单租户模型（旧） | 多租户模型（新） |
|---|---|---|
| `users.id` (UUID) | 全局唯一 | **全局唯一（不变）** |
| `users.username` | 全局唯一 | **(tenant_id, username) 唯一**（仅 `username_confirmed_at IS NOT NULL` 行；onboarding 窗口期允许 NULL，详见 §11.3）|
| `users.email` | 全局唯一 | **全局唯一（不变；防钓鱼）**，例外走 `tenant_email_allowlist` |
| `user_auth_identities.external_key` | 全局唯一 | **全局唯一（不变；禁止跨 tenant 复用）** |

> **决策摘要**：`external_key` 全局唯一 + `username` tenant 级唯一 + `email` 全局唯一（带 allowlist 例外）。三者差异化策略，分别对应"身份不可重复"、"名字可重复"、"邮箱防钓鱼"。详见 §6.2 / §8.1 / §8.2 实现细节。

**email 跨 tenant 策略**：

- **默认禁止**：同一 email 不能在多个 tenant 注册（防钓鱼；Alice@acme.com 不能在 tenant B 冒充）
- **例外**：tenant_admin 可在后台显式 allowlist 某 email 跨 tenant（如集团内部子公司共用员工）
- **实现**：`users.email` 仍加全局 unique 索引（保持单租户行为）+ `tenant_email_allowlist` 表存储例外

```sql
-- 默认全局 unique（防钓鱼）
CREATE UNIQUE INDEX uq_users_email_global
  ON cs_user.users (email) WHERE deleted_at IS NULL AND email IS NOT NULL;

-- 例外表
CREATE TABLE cs_user.tenant_email_allowlist (
    email          VARCHAR(191) NOT NULL,
    tenant_ids     UUID[] NOT NULL,    -- 允许多 tenant
    reason         TEXT,
    created_by     UUID NOT NULL,       -- platform_admin
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (email)
);
```

### 6.2 external_key 跨 tenant

**external_key 角色澄清**（避免与 user_id 混淆）：

`external_key` 属于 `user_auth_identities` 表（不是 `users` 表），是**登录源维度**的稳定身份键，由 `issuer + provider_user_id` 归一化生成（详见 [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md) §4.1）。它**故意耦合登录源**——每条登录源身份对应一行 identity + 一个 external_key，一个 cs-user 用户绑 N 个登录源就有 N 个 external_key，但只对应 1 个 `user_id`。

| 字段 | 所属表 | 维度 | 跨登录源稳定性 | 全局唯一 | 出现在 JWT | 业务表 FK |
|---|---|---|---|---|---|---|
| `external_key` | user_auth_identities | 登录源身份 | ✗（每条 identity 一个）| ✓（§6.2）| ✗ | ✗ |
| `user_id` (UUID) | users | cs-user 用户 | ✓（跨登录源不变）| ✓ | ✓（`sub`）| ✓ |
| `users.username` | users | cs-user 用户 | ✓ | 仅 tenant 内（§6.1）| ✓（`preferred_username`）| ✗ |
| `users.email` | users | cs-user 用户 | ✓ | ✓（防钓鱼，§6.1）| ✓ | ✗ |

**external_key 生成规则**（cs-user 在 OAuth callback 后归一化生成，统一格式 `{issuer}|{provider_user_id}`）：

| 登录源 | external_key 示例 |
|---|---|
| GitHub | `https://github.com\|alice` |
| Azure AD | `https://login.microsoftonline.com/{tenant}\|alice@acme.com` |
| idtrust | `https://idtrust.acme.com\|alice` |
| Casdoor password | `https://casdoor.example.com\|alice` |
| LDAP | `ldap://dc.acme.com\|uid=alice,ou=staff,dc=acme,dc=com` |

**关系图**：

```
users (1 行)
  user_id = u_abc123  ← 用户级稳定标识（UUID），跨登录源不变
   │
   ├─ user_auth_identities (行 1)  ← GitHub 登录
   │    external_key = "https://github.com|alice"
   │    provider = "github"
   │
   ├─ user_auth_identities (行 2)  ← Azure AD 登录
   │    external_key = "https://login.microsoftonline.com/{tenant}|alice@acme.com"
   │    provider = "azure_ad"
   │
   └─ user_auth_identities (行 3)  ← 飞书登录
        external_key = "https://open.feishu.cn|alice"
        provider = "feishu"
```

**跨 tenant 复用禁止**：

- **绝对禁止**：同一 `external_key` 跨 tenant 复用（如 GitHub 账号 `https://github.com|alice` 不能在 tenant A 和 tenant B 各绑一个 user）
- **DB 约束**：`user_auth_identities` 加全局 unique 索引在 `external_key` 上（保持单租户行为）
- **理由**：同一个 GitHub 账号不能在多 tenant 各开一个账号——容易导致身份混淆与钓鱼攻击

```sql
-- external_key 仍全局 unique（不变）
CREATE UNIQUE INDEX uq_user_auth_identities_external_key
  ON cs_user.user_auth_identities (external_key);
```

> **关键决策**：`external_key` 全局唯一 + `username` tenant 级唯一 + `email` 全局唯一（带 allowlist 例外）。三者差异化策略，分别对应"身份不可重复"、"名字可重复"、"邮箱防钓鱼"。

### 6.3 跨 tenant 引用键

- 跨服务引用仍用**全局 `user_id`（UUID）**
- 业务表（devices / capability_items）外键 user_id 不变；但新增 `tenant_id` 列用于业务侧隔离
- JWT claims 同时带 `user_id` + `tenant_id`，下游业务按需消费

### 6.4 username_history 1 年保护的 tenant 作用域

[`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md) §8.4 规定 username 注销后 1 年内不可被复用（防冒名）。多租户化后：

- **保护范围调整为 tenant 级**：旧 username 在 tenant A 注销后，1 年内仅在 tenant A 内不可复用；tenant B 可立即复用同一 username（不会引发混淆——因为是独立 user 行）。
- **DB 实现**：`username_history` 表加 `tenant_id` 列（继承 USER_CENTER §8.4 表结构的 `old_username` / `new_username` / `changed_at` 字段命名）+ `(tenant_id, lower(old_username), changed_at)` 唯一索引；查询时带 tenant_id 过滤 + `changed_at > now() - INTERVAL '1 year'`。
- **90 天改名冷却**同理改为 tenant 级——同一用户在 tenant A 内 90 天内不能再次改名。

### 6.5 tenant 与 enterprise 关系设计

**概念区分**：tenant 与 enterprise 属于不同抽象层，不要混淆。

| 维度 | Tenant（租户）| Enterprise（企业）|
|---|---|---|
| **抽象层** | 商业 / 隔离层 | 身份元数据层 |
| **是什么** | cs-cloud 与客户签约的商业主体 | 用户的雇佣关系 / 组织归属 |
| **决定的事** | 数据隔离（RLS）、计费、配额、资源（Git server、IdP source）、JWT `tenant_id` | 用户的 department / title / 工号 / 上级 |
| **粒度** | 粗（一个客户一个）| 细（一个用户一个，且可空）|
| **是否独立表** | ✓ `tenants` 表 | ✗ 是 `enterprise_identities` 表的属性（§6 4 层 UserInfo）|
| **典型例子** | acme 与 cs-cloud 签约 → acme 是 tenant | alice 是 acme 员工，工号 EMP001、P7、Platform Team |

**核心洞察**：**tenant 是隔离边界，enterprise 是用户属性**。tenant 决定数据/资源/计费，enterprise 决定人的雇佣元数据。

#### 三种典型关系模式

**模式 A：1 tenant = 1 enterprise**（默认推荐，v1）

一个 tenant 代表一个企业客户；所有用户的企业身份都属于同一企业实体。适合 B2B SaaS 默认场景。

**模式 B：1 tenant = N enterprises**（集团 + 子公司）

```
tenant "acme-group"  ←──────────── 集团统一签约
  ├─ alice  (enterprise: acme-hq,         dept=Platform)    ← 总部 IdP
  ├─ bob    (enterprise: acme-subsidiary, dept=Sales)        ← 子公司 IdP
  └─ carol  (enterprise: acme-research,   dept=AI)           ← 研究院 IdP
```

集团与 cs-cloud 签约（tenant 是合同主体），但内部有多个企业实体（子公司、研究院）。不同用户通过不同 IdP source 登录，映射出不同 enterprise 字段值（`enterprise.uid` 区分）。**v1 通过单 tenant + 多 IdP source + enterprise_uid 区分**实现，不引入多 enterprise 实体表。

**模式 C：N tenants = 1 enterprise**（少见，特殊场景）

同一企业拆多个 tenant 做环境隔离（dev / staging / prod）。v1 通过「同企业客户开多个 tenant」实现，用户在每个 tenant 各有账号（不支持跨 tenant membership，§13.3 v2 演进）。

#### v1 推荐设计：模式 A 为主，模式 B 兼容

**决策 1：不引入 `enterprises` 独立表**

enterprise 仍是 user 的属性（`enterprise_identities` 表，§6 4 层 UserInfo 中的 enterprise 层）。一个 tenant 内默认所有用户共享同一企业身份（`tenant.enterprise_config` 描述 tenant 级元信息），不同 enterprise 通过 user 的 `enterprise_identities.enterprise_uid` 区分（详见 §6.5.1）。

**决策 2：`tenants.enterprise_config` JSONB 承载 tenant 级企业元信息**

```jsonc
// tenants.enterprise_config 示例
{
  "legal_name": "Acme Inc.",                  // 法律主体名
  "display_name": "Acme",                      // 展示名（登录页 / 邮件签名）
  "logo_url": "https://.../acme-logo.png",
  "brand_color": "#FF5733",                    // 登录页主题色
  "industry": "technology",
  "size_band": "1000-5000"
}
```

用于登录页品牌、邮件签名、合同主体展示。**不是企业实体表，只是 tenant 的企业身份描述**。schema 详见附录 A。

**决策 3：enterprise 字段不做隔离 key**

业务表 RLS 用 `tenant_id`，**不**用 enterprise。模式 B 下子公司员工与总部员工都在同一 tenant 内，数据按 tenant_id 隔离（同 tenant 内可见）。跨 enterprise 访问控制由业务侧用 `jwt_enterprise.uid` / `department` claim 实现（可选）。

**决策 4：跨 enterprise / 跨 tenant 关系留给 v2**

- 跨 tenant（用户同时属多个 tenant）：v2 走 `user_tenant_memberships` 多对多表（§13.3）
- 跨 enterprise（用户同时属多个企业）：v2 走 `user_enterprise_memberships` 多对多表
- v1 严格 1 user : 1 tenant : 1 enterprise identity（由 tenant 级 IdP 决定）

### 6.5.1 enterprise_uid：企业身份唯一标识

为支持**企业身份在下发、传递过程中的稳定定位**（JWT 下发、跨系统用户关联、部门映射定位、组织树关联），引入 `enterprise_uid` 字段作为用户在 tenant 内的企业身份唯一键。

**字段定义**：

```sql
-- enterprise_identities 表加 enterprise_uid 列
ALTER TABLE cs_user.enterprise_identities
  ADD COLUMN enterprise_uid VARCHAR(128);
-- 默认由 IdP 的 employeeNumber / employeeId 字段映射，可由 tenant 级配置覆盖

-- tenant 内唯一约束（同一 tenant 内 enterprise_uid 不可重复）
CREATE UNIQUE INDEX uq_enterprise_identities_tenant_uid
  ON cs_user.enterprise_identities (tenant_id, enterprise_uid)
  WHERE enterprise_uid IS NOT NULL AND deleted_at IS NULL;
```

**关键属性**：

| 属性 | 约束 |
|---|---|
| 唯一性 | `(tenant_id, enterprise_uid)` 唯一；跨 tenant 可重复 |
| 可空 | 个人版 tenant / 自由职业者用户 `enterprise_uid` 可为 NULL |
| 稳定性 | 一旦写入不可变（员工工号不会改）；如确需变更，走「注销旧 identity + 1 年保护 + 创建新 identity」流程 |
| 默认来源 | `${provider.employeeNumber}` 或 `${provider.employeeId}`（OIDC / LDAP / SCIM 通用字段）|
| Tenant 级覆盖 | `tenant_configs[<t_id>].enterprise_field_mapping.uid.source` 可配置（§12.1.1）|

**用途**：

| 场景 | 用法 |
|---|---|
| JWT `enterprise.uid` claim（§12.1）| 下发到下游业务，作为企业身份稳定 key |
| 跨系统用户关联 | cs-cloud / app-ai-native 业务侧按 `enterprise.uid` 与 HR 系统 / 邮件系统对齐 |
| 部门映射定位 | `dept_code_to_name` transformer 按 `enterprise.uid` 查找用户当前部门（§17.3）|
| 组织树关联 | 同步 HR 组织树时，按 `enterprise.uid` 关联到 cs-user 的 user 行 |
| 审计 | 审计日志记录 `enterprise_uid`，便于企业合规审计（按工号追溯）|

**与 external_key / user_id 的区分**：

| 字段 | 表 | 维度 | 跨登录源稳定性 | 跨企业稳定性 | 出现在 JWT | 用途 |
|---|---|---|---|---|---|---|
| `external_key` | user_auth_identities | 登录源身份 | ✗（每条 identity 一个）| ✗ | ✗ | 防钓鱼（§6.2）|
| `enterprise_uid` | enterprise_identities | 企业身份 | ✓（跨登录源不变，由 IdP 工号决定）| ✗（换企业时变）| ✓（`enterprise.uid`）| 企业身份定位、部门映射 |
| `user_id` | users | cs-user 用户 | ✓ | ✓ | ✓（`sub`）| 业务表 FK、跨系统稳定标识 |
| `users.username` | users | cs-user 用户 | ✓ | ✓ | ✓（`user.username`）| tenant 内可读标识 |

**多 IdP 合并时的 uid 求值**：

用户绑多个 IdP（如 idtrust + AAD）时，`enterprise_uid` 按 `priority_providers` 顺序求值——第一个非空的 IdP 提供者胜出（§12.1.1）。

> **求值范围限定**：`priority_providers` 仅在 `tenant_configs.employment_providers.enabled` 列表内的 IdP 之间生效（§11.4.4）。非 employment provider（如 github / feishu 等）即使被绑也不会贡献 uid / enterprise 字段，避免社交 IdP 污染雇佣上下文。`priority_providers` 中若列出非 employment provider，cs-user 在配置加载时静默忽略该项并打 warn 日志。

例如 acme 集团模式下（employment_providers.enabled = [idtrust, azure_ad]）：

```
priority_providers: [idtrust, azure_ad]    # github / feishu 即使绑了也不参与 uid 求值
alice 通过 idtrust 登录 → enterprise.uid = idtrust.employeeNumber = "EMP001"
bob   通过 azure_ad 登录 → enterprise.uid = azure_ad.employeeId = "A-12345"
```

两个用户的 `enterprise_uid` 都在 tenant `acme-group` 内唯一，互不冲突。

---

---

# Part III：数据模型扩展

## 7. tenants 表 + tenant_admins 表

> **服务归属**：`tenants` / `tenant_admins` / `tenant_configs` 三表归 **cs-user** 维护——认证 / 身份 / 租户隔离的真相源都在 cs-user；costrict-web 业务表（`devices` / `capability_items` 等）通过 `tenant_id` FK 引用 `cs_user.tenants(tenant_id)`，**不本地维护 tenant 元数据**（详见 §22.1）。
>
> **物理路径说明**：Stage D 抽离前，cs-user 代码物理位于 `costrict-web/server/`（迁移文件、`internal/auth/`、`internal/user/` 等）；Stage D 剥离为独立服务后路径变 `cs-user/...`，本节 schema 定义不变。详见 [`IDENTITY_ARCHITECTURE_ROADMAP`](./IDENTITY_ARCHITECTURE_ROADMAP.md) Stage A/D。

### 7.1 `tenants` 表

```sql
CREATE TABLE cs_user.tenants (
    tenant_id        UUID PRIMARY KEY,
    slug             VARCHAR(32) NOT NULL,         -- 全局唯一 URL-safe [a-z0-9-]{3,32}
    display_name     VARCHAR(191) NOT NULL,
    status           VARCHAR(32) NOT NULL DEFAULT 'active',  -- active | suspended | deleted
    edition          VARCHAR(32) NOT NULL DEFAULT 'team',
    email_domains    TEXT[] NOT NULL DEFAULT '{}',  -- 邮箱域白名单
    features         JSONB NOT NULL DEFAULT '{}',   -- 功能开关
    limits           JSONB NOT NULL DEFAULT '{}',   -- 配额
    settings         JSONB NOT NULL DEFAULT '{}',   -- 通用设置（语言/时区默认）
    deletion_requested_at TIMESTAMPTZ,
    deleted_at       TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_tenants_slug
  ON cs_user.tenants (slug) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX uq_tenants_email_domain
  ON cs_user.tenants (unnest(email_domains)) WHERE deleted_at IS NULL;
  -- 防止两 tenant 声明同一邮箱域（导致 Try 2 多命中）
```

### 7.2 `tenant_admins` 表

> 决策：tenant_admin 关系独立表，不混入 `user_system_roles`——避免 platform_admin / tenant_admin / tenant_member 三级混杂难管理。

```sql
CREATE TABLE cs_user.tenant_admins (
    tenant_id        UUID NOT NULL REFERENCES cs_user.tenants(tenant_id) ON DELETE CASCADE,
    user_id          UUID NOT NULL REFERENCES cs_user.users(id) ON DELETE CASCADE,
    role             VARCHAR(32) NOT NULL,    -- owner | admin | billing
    granted_by       UUID NOT NULL REFERENCES cs_user.users(id),
    granted_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at       TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, user_id)
);

CREATE INDEX idx_tenant_admins_user
  ON cs_user.tenant_admins (user_id) WHERE revoked_at IS NULL;
```

**tenant_admin 三级**：

| role | 权限 |
|---|---|
| `owner` | 全部 tenant 操作 + 指派 / 撤销其他 admin + 删除 tenant（grace） |
| `admin` | 用户管理 + provider mapping 配置 + 业务功能开关 |
| `billing` | 仅计费 / 配额查看，无用户管理权 |

### 7.3 platform_admins 表

```sql
CREATE TABLE cs_user.platform_admins (
    user_id          UUID PRIMARY KEY REFERENCES cs_user.users(id) ON DELETE CASCADE,
    granted_by       UUID NOT NULL REFERENCES cs_user.users(id),
    granted_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    scope            VARCHAR(32) NOT NULL DEFAULT 'full',  -- full | support | read_only
);

CREATE INDEX idx_platform_admins_scope
  ON cs_user.platform_admins (scope);
```

platform_admin 的 `users` 行 `tenant_id` 可为 NULL（platform 维度账号）；普通用户 `tenant_id` 必填。

> **platform_admin NULL tenant_id 处理**（详见 §10.2 与 附录 C）：
> - **DB 层**：RLS 策略 `tenant_id = current_setting('cs_user.tenant_id', true)::UUID` 对 NULL tenant_id 行永远返回 NULL（SQL 三值逻辑），即普通查询（cs_user_app 角色）永远看不到 platform_admin 行。
> - **访问路径**：platform_admin 行通过**独立登录角色**（cs_user_pa_login）+ **SECURITY DEFINER 函数**（如 `cs_user.list_all_users`）访问，函数显式跳过 RLS。
> - **唯一性约束**：platform_admin 行不受 `uq_users_tenant_username` 约束（predicate 排除 NULL tenant_id），改由 `uq_users_platform_admin_username` 单独保证 platform_admin 之间 username 全局唯一。

---

## 8. 租户作用域表 schema 扩展

### 8.1 `users` 表加 `tenant_id`

```sql
ALTER TABLE cs_user.users
  ADD COLUMN tenant_id UUID REFERENCES cs_user.tenants(tenant_id),
    -- platform_admin 可为 NULL；普通用户必填
  ADD COLUMN tenant_id_set_at TIMESTAMPTZ NOT NULL DEFAULT now();
    -- tenant_id 一旦设置不可变（防用户搬家）

-- onboarding 窗口期：username 允许 NULL，确认后冻结（详见 §11.3）
ALTER TABLE cs_user.users
  ALTER COLUMN username DROP NOT NULL,
  ADD COLUMN username_confirmed_at TIMESTAMPTZ;   -- NULL = onboarding 未完成

-- 调整唯一约束（替换原 username_lower / email 全局唯一）
DROP INDEX uq_users_username_lower;
DROP INDEX uq_users_email;

CREATE UNIQUE INDEX uq_users_tenant_username
  ON cs_user.users (tenant_id, lower(username))
  WHERE deleted_at IS NULL
    AND tenant_id IS NOT NULL
    AND username IS NOT NULL;       -- 仅确认后的行参与唯一性

-- platform_admin 行的 tenant_id 为 NULL，不参与上面的 tenant-scoped 唯一性
-- 单独约束：platform_admin 之间 username 全局唯一
CREATE UNIQUE INDEX uq_users_platform_admin_username
  ON cs_user.users (lower(username))
  WHERE deleted_at IS NULL
    AND tenant_id IS NULL
    AND username IS NOT NULL;

CREATE UNIQUE INDEX uq_users_email_global
  ON cs_user.users (email)
  WHERE deleted_at IS NULL AND email IS NOT NULL;
  -- 保持全局唯一防钓鱼；例外走 tenant_email_allowlist

CREATE INDEX idx_users_tenant
  ON cs_user.users (tenant_id) WHERE deleted_at IS NULL;

CREATE INDEX idx_users_pending_onboarding
  ON cs_user.users (tenant_id)
  WHERE username_confirmed_at IS NULL AND deleted_at IS NULL;
  -- tenant_admin 查询"未完成 onboarding 的用户"
```

> **tenant_id 不可变约束**：一旦用户首次登录 tenant_id 写入，后续不可改（防止用户跨 tenant 搬家导致历史数据混乱）。如确需切换 tenant，新 tenant 重新创建账号 + 旧账号归档。

### 8.2 `user_auth_identities` 加 `tenant_id`（冗余，便于查询）

> **multi-step 迁移**（N0 阶段执行）：直接 `ADD COLUMN ... NOT NULL` 对已有数据的表会失败（PostgreSQL 报 `column contains null values`）。必须分三步：① 加 nullable 列；② backfill（JOIN users 拿 tenant_id）；③ 设 NOT NULL。

```sql
-- Step 1: 加 nullable 列（N0 阶段；不阻塞写）
ALTER TABLE cs_user.user_auth_identities
  ADD COLUMN tenant_id UUID REFERENCES cs_user.tenants(tenant_id) ON DELETE CASCADE;

-- Step 2: backfill（N0 阶段；按 user_id JOIN）
UPDATE cs_user.user_auth_identities uai
SET tenant_id = u.tenant_id
FROM cs_user.users u
WHERE uai.user_id = u.id AND uai.tenant_id IS NULL;

-- Step 3: 设 NOT NULL（N0 阶段；Step 2 全表回填完成后执行）
ALTER TABLE cs_user.user_auth_identities
  ALTER COLUMN tenant_id SET NOT NULL;

-- external_key 仍保持全局唯一（防钓鱼，§6.2）
-- 不再加 (tenant_id, external_key) 复合唯一——external_key 全局唯一已足够

CREATE INDEX idx_user_auth_identities_tenant
  ON cs_user.user_auth_identities (tenant_id);
```

> 注：`tenant_id` 在此为冗余字段（可从 `users.tenant_id` JOIN 得到），但加索引便于"列出 tenant 内所有 identity"等查询。同理适用于 `user_profile` / `enterprise_identities` / `user_gitea_binding`——三张表均在 N0 阶段按 Step 1 → 2 → 3 顺序迁移。

### 8.3 `user_profile` / `user_system_roles` / `enterprise_identities` 加 `tenant_id`

每张租户作用域表都加 `tenant_id`，便于按 tenant 聚合查询、批量导出、级联清理。

### 8.4 `user_gitea_binding` 加 `tenant_id`

> 同 §8.2 multi-step 迁移：① 加 nullable 列；② backfill；③ SET NOT NULL。下面只列最终态。

```sql
-- 最终态（N0 三步迁移后）
-- ALTER TABLE cs_user.user_gitea_binding
--   ADD COLUMN tenant_id UUID REFERENCES cs_user.tenants(tenant_id) ON DELETE CASCADE;
-- UPDATE ... backfill ...
-- ALTER TABLE cs_user.user_gitea_binding ALTER COLUMN tenant_id SET NOT NULL;

CREATE INDEX idx_user_gitea_binding_tenant
  ON cs_user.user_gitea_binding (tenant_id);
```

Gitea org namespace 按 tenant 隔离（详见 §20）：`g-<tenant_slug>/u-<username>/`。

---

## 9. tenant 级配置：provider mapping + features

### 9.1 配置层级（覆盖优先级）

```
global default (cs-user/config/provider-mapping.yaml)
   │
   ▼ 被覆盖
tenant 级 (cs_user.tenant_configs 表存 yaml 内容)
   │
   ▼ 被覆盖
（未来）provider 级临时 override（应急用）
```

### 9.2 `tenant_configs` 表

```sql
CREATE TABLE cs_user.tenant_configs (
    tenant_id        UUID PRIMARY KEY REFERENCES cs_user.tenants(tenant_id) ON DELETE CASCADE,
    provider_mapping YAML NOT NULL,    -- 覆盖 / 扩展全局 provider-mapping.yaml
    username_strategy YAML NOT NULL DEFAULT '{}',
      -- onboarding 期间 username 求值规则（详见 §11.3.3），空 yaml 走全局默认 hybrid
    display_name_strategy YAML NOT NULL DEFAULT '{}',
      -- display_name 初始化优先级（详见 §11.3.5），空 yaml 走全局默认
    employment_providers YAML NOT NULL DEFAULT '{}',
      -- 雇佣上下文提供方 IdP 列表与刷新策略（详见 §11.4.4）。
      -- 空 yaml = enabled 列表为空 = 该 tenant 不启用雇佣上下文（所有用户 enterprise_identities 为空）；
      -- tenant_admin 必须显式声明哪些 IdP 是 employment provider 才会写入/刷新雇佣字段
    features         JSONB NOT NULL DEFAULT '{}',
    enterprise_schema_ext JSONB NOT NULL DEFAULT '{}',
      -- tenant 自定义 enterprise 字段（写入 enterprise_identities.attributes）
    updated_by       UUID NOT NULL REFERENCES cs_user.users(id),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### 9.3 配置合并语义

```yaml
# 全局 config/provider-mapping.yaml
providers:
  ldap:
    rank: 150
    field_map:
      employee_number: "employeeNumber"   # 通用默认

# tenant_configs[tenant_id=acme].provider_mapping
providers:
  ldap:
    enabled: true                          # 覆盖
    rank: 200                              # 覆盖（acme 把 ldap 提到比 aad 高）
    field_map:
      employee_number: "emp_id"            # 覆盖（acme 的 LDAP 字段命名不同）
      cost_center: "cost_ctr"             # 新增
    enterprise_sync:
      interval: "6h"                       # 覆盖（acme 要更频繁同步）

# 求值结果（合并后）
providers:
  ldap:
    enabled: true
    rank: 200
    field_map:
      employee_number: "emp_id"            # tenant 覆盖
      cost_center: "cost_ctr"             # tenant 新增
      # 其他默认字段继承全局
    enterprise_sync:
      interval: "6h"
```

合并规则：**深合并 + tenant 覆盖全局**；数组整体替换（不元素合并）。

---

## 10. 数据隔离机制（应用层 + RLS 兜底）

### 10.1 应用层：每个查询带 `tenant_id`

cs-user 所有 service 方法签名强制带 `tenant_id`：

```go
// 错误（无 tenant 上下文）
func (s *UserService) Get(userID string) (*User, error)

// 正确（必带 tenant 上下文）
func (s *UserService) Get(ctx context.Context, userID string) (*User, error)
// 内部从 ctx 拿 tenant_id；DB 查询自动加 WHERE tenant_id = ?

// 跨 tenant 查询（仅 platform_admin）
func (s *UserService) GetCrossTenant(ctx context.Context, userID string) (*User, error)
// 要求 ctx 中有 platform_admin scope；否则 403
```

**强制 lint 规则**：所有 DB query builder 调用必须带 `.Where("tenant_id = ?", tenantID)`，CI 检查（如禁止 `db.Raw` / `db.Find` 不带 WHERE）。

### 10.2 DB 层：PostgreSQL Row Security Policy（RLS）兜底

> 即使应用层 bug 漏掉 tenant_id 过滤，RLS 在 DB 层强制隔离。

```sql
-- 1. 启用 RLS（FORCE 强制对 table owner 也生效）
ALTER TABLE cs_user.users ENABLE ROW LEVEL SECURITY;
ALTER TABLE cs_user.users FORCE ROW LEVEL SECURITY;
ALTER TABLE cs_user.user_auth_identities ENABLE ROW LEVEL SECURITY;
ALTER TABLE cs_user.user_auth_identities FORCE ROW LEVEL SECURITY;
ALTER TABLE cs_user.user_profile ENABLE ROW LEVEL SECURITY;
ALTER TABLE cs_user.user_profile FORCE ROW LEVEL SECURITY;
ALTER TABLE cs_user.enterprise_identities ENABLE ROW LEVEL SECURITY;
ALTER TABLE cs_user.enterprise_identities FORCE ROW LEVEL SECURITY;
-- 其他租户作用域表同理

-- 2. 角色 / Schema 分离
--    - cs_user_owner: DDL / migration 角色持有 table owner，但 NOBYPASSRLS
--      （只在 migration 时连接；运行时不连接）
--    - cs_user_app: 运行时角色（NOBYPASSRLS，受 RLS 约束）
--    - cs_user_platform_admin: 跨 tenant 查询角色（NOBYPASSRLS，专用 SECURITY DEFINER
--      函数显式跳过 RLS，禁止普通连接设置 GUC 绕过）
CREATE ROLE cs_user_owner NOLOGIN NOBYPASSRLS;
CREATE ROLE cs_user_app    NOLOGIN NOBYPASSRLS;
CREATE ROLE cs_user_platform_admin NOLOGIN NOBYPASSRLS;
GRANT cs_user_app TO cs_user_app_login;          -- 应用登录角色
GRANT cs_user_platform_admin TO cs_user_pa_login; -- platform_admin 专用登录角色

-- 3. RLS 策略：cs_user_app 强制按 session 变量过滤
--    session variable cs_user.tenant_id 由应用层在获取连接时 SET（仅 cs_user_app 角色）
CREATE POLICY tenant_isolation_users ON cs_user.users
  FOR ALL TO cs_user_app
  USING (tenant_id = current_setting('cs_user.tenant_id', true)::UUID);

-- 4. platform_admin 路径走独立连接（cs_user_pa_login）+ SECURITY DEFINER 函数
--    函数显式跳过 RLS（owner 执行），且只允许跨 tenant 读，不允许跨 tenant 写
CREATE FUNCTION cs_user.get_user_cross_tenant(p_user_id UUID)
RETURNS cs_user.users
LANGUAGE sql SECURITY DEFINER SET search_path = cs_user, pg_temp AS $$
  SELECT * FROM cs_user.users WHERE id = p_user_id;
$$;
REVOKE EXECUTE ON FUNCTION cs_user.get_user_cross_tenant FROM PUBLIC;
GRANT  EXECUTE ON FUNCTION cs_user.get_user_cross_tenant TO cs_user_platform_admin;

-- 5. 应用层（cs_user_app 角色）每次获取连接时 SET session 变量
--    Go 伪代码（用 pgx 连接池 BeforeAcquire hook，确保连接重置）
func setupSession(conn *pgx.Conn, tenantID string) error {
    conn.Exec("SET cs_user.tenant_id = $1", tenantID)  // 仅 cs_user_app 角色；
                                                       // GUC 由 RLS 直接消费，
                                                       // 不再有 is_platform_admin GUC
    return nil
}
```

> **关键设计点**：
> - **`FORCE ROW LEVEL SECURITY`**：table owner（cs_user_owner）也受 RLS 约束，防止 migration 角色被误用做运行时连接绕过隔离。
> - **三角色分离**：DDL 角色（cs_user_owner）与运行时角色（cs_user_app）严格分离；migration 期间禁用 RLS（`ALTER TABLE ... DISABLE ROW LEVEL SECURITY`），migration 结束重新 ENABLE + FORCE。
> - **平台管理员路径**：platform_admin 通过**专用登录角色**（cs_user_pa_login）+ **SECURITY DEFINER 函数**显式跳过 RLS，不依赖应用层 GUC，杜绝连接复用导致跨 tenant 越权。
> - **platform_admin 的 users 行（`tenant_id = NULL`）**：通过 `cs_user.get_user_cross_tenant` 函数读取；不参与 `cs_user_app` 角色的 tenant-scoped 查询，避免 RLS 三值逻辑下 NULL 行被滤除。

**RLS 注意事项**：

- 启用 + `FORCE` RLS 后，所有查询（含 table owner）自动加 filter；DB 层防泄漏
- 平台管理员查询走**专用角色（`cs_user_pa_login`）+ SECURITY DEFINER 函数**显式跳过 RLS（不再依赖应用层 GUC）
- 性能影响 < 5%（PostgreSQL RLS 优化良好）
- 详细配置参考附录 C

### 10.3 备份与导出按 tenant 隔离

- 备份工具支持 `--tenant-id=<id>` 仅备份单 tenant 数据
- tenant 注销时 `pg_dump --tenant-id=<id>` 导出归档后真删

---

# Part IV：认证流程扩展

## 11. 租户识别登录链路

### 11.1 完整登录流程

```
[1] 用户访问 cs-user
   ├─ 子域：https://acme.cs-user.example.com → slug="acme"
   └─ 无子域：https://cs-user.example.com → 跳通用登录页（Try 2 邮箱识别）
   │
[2] tenant resolution（§5 三层 fallback）
   ├─ 命中 tenant → 写 cookie cs_tenant_slug=acme
   └─ 未命中 → tenant 选择器
   │
[3] 跳 Casdoor OAuth 入口（单 organization 多 source 模式，详见 §11.2）
   ├─ 所有 tenant 共享单个 Casdoor organization（costrict-default）
   ├─ OAuth state 参数编码 tenant_id（HMAC 签名防篡改）
   └─ Casdoor 不感知 tenant 概念，tenant 上下文由 cs-user 自管
   │
[4] Casdoor 处理 OAuth（仅展示该 tenant 配置的 source）
   ├─ Acme tenant 的 source：idtrust + azure_ad + password
   └─ 其他 tenant 的 source 不展示
   │
[5] OAuth callback → cs-user
   ├─ 校验 state.tenant_id 与 cookie.cs_tenant_slug 一致
   ├─ 拉 Casdoor userinfo + raw_claims
   ├─ NormalizeJWTClaims（带 tenant 上下文）
   ├─ ResolveOrCreateUserByIdentity（带 tenant_id）：
   │   ├─ 命中 → 加载 user，校验 user.tenant_id == 当前 tenant
   │   │         （不匹配则 401 "identity belongs to another tenant"；
   │   │          §6.2 external_key 全局唯一约束保证命中只能是同一 tenant）
   │   │         └─ 若 external_key 已被本 user 绑定 → 走【登录路径】，按 §11.4.4 切换 primary_provider
   │   │         └─ 若是新 source（绑了 idtrust，现在用未绑的 aad 登录）→ 走【绑定路径】§11.4.3
   │   └─ 未命中 → 校验 tenant 配额 max_users → 创建 user（写 tenant_id）→ ApplyInitialProfileFromIdP（§11.4.2）
   ├─ 分支判断（详见 §11.4）：
   │   ├─ 新用户首次落库 / 已有用户走登录路径（含 primary_provider 切换）
   │   │   → 调用 ApplyEnterpriseMapping **仅当 source ∈ tenant_configs.employment_providers.enabled**
   │   │     （非 employment provider 如 github 登录 → 跳过，enterprise_identities 保持现状或留空）
   │   │     求值规则详见 §11.4.4
   │   └─ 已有用户走"绑定新 source"路径（§11.4.3）
   │       → **短路**：跳过 ApplyEnterpriseMapping / ApplyInitialProfileFromIdP，仅 INSERT user_auth_identities
   ├─ 自签 JWT（含 tenant_id / tenant_slug claims）
   └─ Set-Cookie cs_session + cs_tenant_slug
   │
[6] 后续 API 请求
   └─ JWT 携带 tenant_id；应用层 + RLS 双层过滤
```

### 11.2 Casdoor 接入：单 organization + state.tenant_id（唯一方案）

Casdoor 在本架构中定位为**多登录源协议适配器**（详见 §12.3 双层契约），cs-user **不消费 Casdoor 的 organization / user / RBAC 模型**。所有 tenant 共享单个 Casdoor organization，tenant 上下文完全由 cs-user 自管。

**接入方式**：

- Casdoor 单 organization（如 `costrict-default`），所有 source 共存（idtrust / AAD / LDAP / password / 飞书 / 钉钉 等）
- OAuth `state` 参数编码 `tenant_id`（HMAC 签名防篡改，绑定 `redirect_uri` 与 issued_at 防 CSRF / 重放）
- callback 时 cs-user 反查 `state.tenant_id` 拿到 tenant 上下文
- Casdoor 端不感知 tenant 概念；source 列表是否按 tenant 过滤见 §19.2

**为什么不走 Casdoor multi-organization**：

| 维度 | multi-organization（已否决） | 单 organization + state.tenant_id ✓ |
|---|---|---|
| Casdoor 依赖深度 | cs-user 必须读 Casdoor organization / user 模型 | cs-user 只把 Casdoor 当 OAuth 协议层（仅消费 ID Token）|
| 替换 IdP 成本 | 强绑 Casdoor 抽象，难替换 | 通过标准 OAuth/OIDC 解耦，未来替换 Casdoor 只改 `client_id` / `issuer` / JWKS URL |
| 数据一致性 | Casdoor 与 cs-user 双 source of truth | cs-user 数据库是唯一 source of truth，Casdoor 端 user 数据视为缓存（JIT provisioning 时建）|
| 用户模型 | cs-user 必须镜像 Casdoor user | cs-user 自管 user 行，Casdoor JWT 仅用于首次身份确认 |

**已否决方案**：multi-organization（清晰隔离但依赖过深，违反「Casdoor 仅作 IdP 协议层」核心约束）。

**IdP 切换契约**（未来替换 Casdoor 时的最小改动面）：

cs-user 对 IdP 的依赖仅限于以下标准接口，替换 Casdoor 时只需调整这些配置：

| 依赖面 | 配置项 | 切换时改什么 |
|---|---|---|
| OAuth 入口 | `casdoor.authorize_url` / `casdoor.client_id` | 改为新 IdP 的 authorize endpoint 与 client_id |
| Token 端点 | `casdoor.token_url` | 改为新 IdP 的 token endpoint |
| JWKS 验证 | `casdoor.jwks_url` | 改为新 IdP 的 `/.well-known/jwks.json` |
| Callback 路由 | `GET /api/auth/callback/:source` | 路由不变，仅 ID Token 解析适配 |
| ID Token claims | `sub` / `email` / `source_claims` | 若新 IdP claims 命名不同，改 NormalizeJWTClaims 适配 |

cs-user 内部业务逻辑（用户表、tenant 模型、4 层 UserInfo、企业身份映射）与 IdP 解耦，零改动。

### 11.3 新用户登录后 onboarding（username 确认 + display_name 初始化）

新用户首次 OAuth callback 落库后，**username 与 display_name 解耦处理**：username 走严格确认链路（v1 不支持后续变更），display_name 走宽松可变链路（用户随时可改）。

#### 11.3.1 username 三种解析模式

| 模式 | 触发条件 | 数据来源 | 适用场景 |
|---|---|---|---|
| **auto-mapping**（自动映射）| tenant 配置 `username_strategy.source != "user_input"` | IdP claim / 企业身份字段 | 企业内部 IdP（idtrust / AAD / LDAP），claim 已含规范化工号或邮箱前缀 |
| **user-input**（UI 填写）| auto-mapping 求值为空、或求值结果在 tenant 内冲突 | 用户在 onboarding 页手动填写 | 公开 IdP（GitHub / Google）首次登录、企业 IdP 未返回可用字段 |
| **hybrid**（混合，默认）| tenant 未显式关闭 fallback | 先 auto-mapping，失败则 fallback 到 user-input | 大多数 tenant 默认形态 |

> **关键决策（v1）**：username 一经确认即**冻结**，写入 `users.username_confirmed_at`，后续不允许变更（`UPDATE users SET username = ...` 在 repository 层拒绝；详见 §11.3.5）。变更需求留待 v2（走 platform_admin 审批流，本提案不实现）。

#### 11.3.2 username 求值流程

```
[1] OAuth callback 落库（ResolveOrCreateUserByIdentity）
    └─ user 行 INSERT，username = NULL，username_confirmed_at = NULL

[2] ApplyUsernameStrategy（按 tenant_configs.username_strategy 求值）
    ├─ mode = "auto" / "hybrid"
    │   ├─ 按 source 求值候选值：
    │   │   - source = "idp_claim:preferred_username"  → raw_claims["preferred_username"]
    │   │   - source = "idp_claim:sub"                 → raw_claims["sub"]
    │   │   - source = "enterprise_uid"                → enterprise_identities.uid（§6.5.1）
    │   │   - source = "employee_id"                   → enterprise_identities.employee_id
    │   │   - source = "email_local_part"              → split(users.email, "@")[0]
    │   │   - source = "transformer:{name}"            → 调用内置 transformer（§17.3）
    │   ├─ 候选值经 NormalizeUsername 规整（小写 / 去空格 / 字符白名单 `a-z0-9._-`）
    │   ├─ 校验 tenant 内唯一（§6.1：uq_users_tenant_username）
    │   └─ 通过 → UPDATE users SET username=$1, username_confirmed_at=now()
    │       失败（空 / 冲突）→ mode=hybrid 继续 fallback 到 [3]
    └─ mode = "user_input"
        └─ 不求值，跳到 [3]

[3] UI 引导填写（username_confirmed_at 仍为 NULL）
    ├─ 业务 JWT 注入 onboarding_required=["username"]（详见 §12.1）
    ├─ 前端识别后跳 /onboarding/username 页
    ├─ 用户填写 → POST /api/me/username（带候选值实时校验）
    └─ 通过 → UPDATE users SET username=$1, username_confirmed_at=now()
              → 重新签发 JWT（清空 onboarding_required）

[4] 后续登录（已确认）
    └─ 跳过整个流程，username 直接从 DB 读取
```

**NormalizeUsername 规则**（v1）：

- 字符白名单 `[a-z0-9._-]`，长度 3-32，首尾非 `.`/`-`/`_`
- 自动小写化（`Alice` → `alice`）
- 保留 IdP 原值在 `user_auth_identities.raw_claims`，便于审计

#### 11.3.3 username_strategy yaml 配置（tenant 级）

```yaml
# tenant_configs[acme].username_strategy（v1 默认 hybrid）
mode: hybrid                    # auto | user_input | hybrid
source: employee_id             # mode=auto/hybrid 时生效
transformer: null               # 可选：source=transformer:xxx 时指定
fallback_to_user_input: true    # mode=hybrid 时，auto 失败是否走 UI
reserve_patterns:               # 黑名单（保留字 / 保留前缀）
  - "admin*"
  - "root"
  - "support"
  - "^(cs|costrict)-.*"         # 平台保留前缀
normalize:
  case: lower
  trim: true
  charset: "[a-z0-9._-]"
  min_length: 3
  max_length: 32
```

**典型 tenant 配置示例**：

| Tenant 类型 | mode | source | 说明 |
|---|---|---|---|
| 企业内部（idtrust / AAD）| `auto` | `employee_id` 或 `idp_claim:preferred_username` | 免填写，工号即用户名 |
| 中小企业（密码 / LDAP）| `hybrid` | `email_local_part` | 邮箱前缀兜底，冲突时 UI 填写 |
| 公开 SaaS（GitHub / Google）| `user_input` | — | 强制用户手填（不能直接用 GitHub login，避免暴露社交身份）|

#### 11.3.4 API：username 确认与 display_name 修改

```
POST /api/me/username                 # 仅 username_confirmed_at IS NULL 可调
  Body: { "username": "alice" }
  Resp: 200 { username, username_confirmed_at } / 409 (tenant 内冲突) / 422 (格式非法)

GET  /api/me/username/availability?username=alice
  Resp: 200 { available: true } / 200 { available: false, suggestion: ["alice1","alice_dev"] }

GET  /api/me/profile                  # 当前 profile 快照
PATCH /api/me/profile                 # display_name 可改；username 字段忽略
  Body: { "display_name": "Alice Zhang" }
  Resp: 200 { display_name, updated_at }
```

**约束**：

- `POST /api/me/username` 仅当 `username_confirmed_at IS NULL` 时允许（已确认返回 409 `username_immutable`）
- `PATCH /api/me/profile` 接受 `display_name` 单字段，不接受 `username`（避免误改）
- display_name 长度 1-64，UTF-8（允许中文 / Emoji），不做唯一性校验
- display_name 修改不影响 username，也不重写 username_history

#### 11.3.5 display_name 初始化与变更

| 阶段 | 行为 |
|---|---|
| 首次落库 | 按 `display_name_strategy` 求值（默认优先级：`idp_claim:name` → `idp_claim:given_name+family_name` → `username` → `"User-"+user_id[:8]`）|
| onboarding 页 | UI 允许用户在确认 username 时一并设置 display_name（默认填充 IdP 求值结果）|
| 后续变更 | 用户随时 `PATCH /api/me/profile`；写入 `user_profile.display_name` + 审计日志（§16）|
| 审计 | display_name 变更走 `audit_logs` 标准事件 `user.profile.updated`（不进 username_history）|

```yaml
# tenant_configs[acme].display_name_strategy（v1 默认值如下）
source_priority:                   # 首个非空胜出
  - "idp_claim:name"
  - "idp_claim:nickname"
  - "compose:given_name+family_name"
  - "username"                     # 兜底用 username
fallback_format: "User-{user_id_short}"
allow_user_override: true          # 必须为 true（display_name 设计上用户可改）
max_length: 64
```

#### 11.3.6 onboarding_required JWT claim（前端识别）

业务 JWT 在用户尚未完成 onboarding 时，注入 `onboarding_required` 字段（详见 §12.1 claims 结构）：

```json
{
  "sub": "u_abc123",
  "tenant_id": "t_acme",
  "onboarding_required": ["username"],   // 空数组或字段缺失 = 已完成
  "onboarding_url": "/onboarding/username"
}
```

- 业务侧中间件识别 `onboarding_required` 非空时，对**业务端点**返回 `428 Precondition Required` + `Location: /onboarding/username`
- 白名单端点（不受 onboarding 拦截）：`POST /api/me/username` / `GET /api/me/username/availability` / `GET /api/me` / `POST /api/auth/logout`
- onboarding 完成后（username_confirmed_at 写入），cs-user 重新签发 JWT，`onboarding_required=[]`

#### 11.3.7 关键决策与边界

| 决策项 | v1 选择 | 理由 |
|---|---|---|
| username 后续变更 | **不支持** | username 是业务表 `created_by` 等字段的可读来源 + tenant 内品牌标识；频繁变更导致审计断裂 |
| username 变更审批流 | v2 提供（platform_admin 介入）| 高价值场景（员工改名、合并账号），但需审批与 username_history 协调 |
| username 允许 NULL | 仅 onboarding 窗口期；完成 onboarding 后 NOT NULL | DB 层：`username VARCHAR(64) NULL` + partial unique index `WHERE username IS NOT NULL` |
| display_name 唯一性 | 不校验 | 展示字段，多个用户可重名 |
| 第三方系统（Gitea 等）username 同步 | 在 onboarding 完成时刻同步一次（创建账号），后续不变更 | 与 §20 GitServerAdapter 对齐；变更需求 v2 处理 |
| IdP 返回 username 与现有用户冲突 | 不强制覆盖；fallback 到 UI 填写 | 防止误合并账号 |

### 11.4 用户信息字段填充策略（首次登录 vs 后续 IdP 绑定）

新用户首次登录时由当时绑定的 IdP 走**初始化填充逻辑**；用户后续再绑新 IdP 时遵循**不覆写原则**（first-write-wins / append-only），已落库的 users / user_profile / enterprise_identities 字段不被新 IdP 覆写。雇佣上下文（`enterprise_identities.*`）归属到**平台 user info**（`(user_id, tenant_id)` 维度），由 tenant 显式声明的 **employment provider** 写入/刷新，与 `primary_provider` 解耦（详见 §11.4.4）。这两条共同保证身份合并（identity merge）的安全性。

#### 11.4.1 字段分类（按可变性）

| 类别 | 例子 | 首次填充源 | 后续变更渠道 | 二次绑 IdP 时 |
|---|---|---|---|---|
| **A. 身份键冻结** | `users.id` / `users.username`（onboarding 后）/ `users.tenant_id` / `external_key` | cs-user 自生成 / onboarding 确认 / 首次登录写入 | v1 不可（v2 审批流）| **绝不覆写** |
| **B. IdP 一次性填充（用户可改）** | `display_name` / `avatar_url` / `locale` / `timezone` / `user_profile.*` | 首个 IdP claim 求值（按 `display_name_strategy`，§11.3.5）| `PATCH /api/me/profile` | **不覆写**（first-write-wins）|
| **C. 雇佣上下文**（绑定到 user，由 employment provider 提供，详见 §11.4.4）| `enterprise_identities.*`（uid / employee_id / department / title / cost_center / ...）| 首次用 `employment_providers.enabled` 中的 IdP 登录时求值写入（非 employment provider 登录留空）| 用户用 employment provider 重新登录时按 `refresh` 策略刷新（员工调岗同步）；用户不能手改 | **不覆写**；非 employment provider 绑定完全不触碰此行；employment provider 绑定按 `resolution_strategy` 处理 |
| **D. 用户绑定类（需验证）** | `users.email` / `users.phone` | 首个 IdP claim | 走验证流程（邮件 OTP / 短信码） | **不覆写**（新 IdP 即使返回不同 email/phone 也不写入；仅记录在 `user_auth_identities.raw_claims`）|
| **E. 系统审计** | `created_at` / `created_by_idp` / `last_login_at` / `auth_time` | 系统自动 | 系统自动 | 自动更新 `last_login_at`；其余不动 |

#### 11.4.2 首次登录填充流程

OAuth callback 落库时（§11.1 [5] `ResolveOrCreateUserByIdentity`），cs-user 调用 `ApplyInitialProfileFromIdP(user, rawClaims, provider)`：

```go
// 伪代码：仅当字段为 NULL / 空时写入；已有值不覆写
func ApplyInitialProfileFromIdP(user *User, rawClaims map[string]any, provider string) {
    // 类 B：IdP 一次性填充（用户可改）
    if user.DisplayName == "" {
        user.DisplayName = resolveDisplayName(rawClaims, tenantConfig.DisplayNameStrategy)
    }
    if user.AvatarURL == "" {
        user.AvatarURL = rawClaims["picture"]   // OIDC standard
    }
    if user.Locale == "" {
        user.Locale = rawClaims["locale"]       // OIDC standard
    }
    // 类 D：email/phone 仅首次写入
    if user.Email == "" {
        user.Email = rawClaims["email"]
        user.EmailVerified = castToBool(rawClaims["email_verified"])
    }
    // 类 E：审计字段
    user.CreatedByIdp = provider
    user.LastLoginAt = now()
    // 类 A：走 ApplyUsernameStrategy（§11.3.2）
    ApplyUsernameStrategy(user, rawClaims, tenantConfig.UsernameStrategy)
    // 类 C：雇佣上下文 —— 仅当 provider 是 employment provider 时才求值
    if sliceContains(tenantConfig.EmploymentProviders.Enabled, provider) {
        ApplyEnterpriseMapping(user, rawClaims, provider, tenantConfig)
        // → 首次写入 enterprise_identities (user_id, tenant_id, uid, employee_id, ...)
        // → resolution_strategy=first_wins 时首次写入即占位，后续 employment provider 登录按 refresh 策略
    }
    // 非 employment provider（如 GitHub）登录 → enterprise_identities 留空
    //   用户后续绑 employment provider 时才写入；详见 §11.4.4
}
```

**关键约束**：

- 所有写入用 `WHERE field IS NULL` 或 `COALESCE` 守卫（DB 层防并发覆写）
- `created_by_idp` 记录用户**首次** 落库的 IdP（永久不变；与 `primary_provider` 不同）
- 整个流程只在 `ResolveOrCreateUserByIdentity` 命中新用户分支时调用；命中已有用户分支不调用
- 类 C 雇佣上下文受 `employment_providers.enabled` 门控——非 employment provider IdP 不会产生雇佣快照行（这与"用户身份完整性"无关，只是雇佣信息缺失，业务侧可降级处理）

#### 11.4.3 后续 IdP 绑定流程（append-only，不覆写）

用户在 `/me/identities` 页面绑定新登录源时（GitHub 用户再绑 AAD、idtrust 用户再绑飞书 等），走独立路径 `ConnectIdentityOnly`：

```
[1] 用户已登录（业务 JWT 携带 primary_provider=idtrust）
[2] GET /api/me/identities/connect/:source → OAuth 入口
[3] OAuth callback → cs-user（与首次登录共用 callback 端点，分支判断）
   ├─ 校验 state.user_id == session.user_id
   ├─ 拉 IdP userinfo + raw_claims
   ├─ 校验 external_key 未被其他 user 占用（§6.2 全局唯一）
   │     └─ 已被占用 → 409 "identity already bound to another user"
   └─ INSERT user_auth_identities (user_id, provider, external_key, raw_claims, bound_at=now())
      ← 仅这一行写入；users / user_profile / enterprise_identities 不动
[4] 绑定完成 → 用户可在 /me/identities 列表看到新 source
[5] 返回 200 { identity_id, provider, bound_at }，业务 JWT 不变（primary_provider 不变）
```

**显式禁止的副作用**（绑定流程中不得调用）：

- ❌ `ApplyInitialProfileFromIdP` —— 不重写 display_name / avatar_url / locale
- ❌ `ApplyUsernameStrategy` —— username 已确认，绝不再求值
- ❌ `ApplyEnterpriseMapping` —— 雇佣上下文绑定到 user，由 `employment_providers` 门控（§11.4.4），绑定路径不参与
- ❌ 任何 `UPDATE users SET email/phone` —— 防止新 IdP 的 email 顶掉原邮箱

#### 11.4.4 雇佣上下文与 IdP 解耦（employment providers）

**核心设计修正**：雇佣上下文（`enterprise_identities.*`）归属到 **平台 user info**（`(user_id, tenant_id)` 维度），**不与 `primary_provider` 耦合**。tenant 通过 `employment_providers` 配置显式声明哪些 IdP 可作为雇佣上下文提供方；其余 IdP（如 GitHub / 飞书 / 钉钉等社交或协作型 IdP）即使成为 primary_provider，也不影响雇佣上下文。

**`employment_providers` tenant 级配置**（写入 `tenant_configs.employment_providers` yaml，详见 §9.2）：

```yaml
# tenant_configs[acme].employment_providers
enabled:                          # 该 tenant 认可作为雇佣上下文来源的 IdP 列表
  - idtrust                       # 主选（rank 最高）
  - aad                           # 备选
  - ldap                          # 备选
  # github / feishu / dingtalk 不在列表 = 仅用于登录，不写雇佣上下文
primary_source: idtrust            # enabled 多个时的优先求值源（用于首次写入）
resolution_strategy: first_wins    # 多个 employment provider claims 冲突时
                                  # first_wins：保留首次写入（默认）
                                  # last_wins：用最新登录的 employment provider 覆盖
                                  # reject_on_conflict：冲突拒绝登录，要求 admin 介入
refresh: on_login                  # on_login：用户每次用 employment provider 登录刷新雇佣字段（员工调岗同步）
                                  # manual_only：仅 admin 触发同步任务刷新
uid_immutability: enforce          # enforce：uid 不一致 → 403（默认，§6.5.1）
                                  # allow_change_with_audit：允许变更，写审计日志（仅离职/重入职场景）
```

**关键不变量**：

- `enterprise_identities` 行 = `(user_id, tenant_id)` 维度的雇佣快照（**绑定到 user**），不属于任何 `user_auth_identities` 行
- 仅当用户**当前 session 的 primary_provider ∈ employment_providers.enabled** 时，登录流程才触发 `ApplyEnterpriseMapping`；否则跳过
- 非 employment provider 的 IdP（GitHub 等）登录 / 绑定 / 切换 → `enterprise_identities` 一律不动
- `primary_provider` claim（§12.1）仅描述本次登录方式，不暗示雇佣刷新

**行为矩阵**（用户绑了 3 个 IdP：idtrust / aad / github，employment_providers=[idtrust, aad]）：

| 场景 | 当前登录 IdP | 是 employment provider？ | enterprise_identities 行为 |
|---|---|---|---|
| 首次登录（idtrust）| idtrust | ✓ | **首次写入**（按 resolution_strategy 占位）|
| 首次登录（github，无 idtrust）| github | ✗ | 留空；待用户后续绑 employment provider 时再写入 |
| 绑第二个 IdP（aad）| aad（绑定）| ✓ | 按 `resolution_strategy`：first_wins→不动；last_wins→覆写 |
| 绑第二个 IdP（github）| github（绑定）| ✗ | 不动（§11.4.3）|
| 切换 primary_provider（idtrust → github）| github | ✗ | 不动；JWT 中 enterprise 仍展示 idtrust 时期快照 |
| 切换 primary_provider（idtrust → aad）| aad | ✓ | 按 `refresh`：on_login→重新求值（uid 一致性校验通过则刷新非 uid 字段）|
| 解绑 employment provider（解 idtrust，仍保留 aad）| aad 下次登录 | ✓ | 不动；aad 是 employment provider，下次登录可继续刷新 |
| 解绑所有 employment provider（只剩 github）| github | ✗ | 保留现有 enterprise_identities 行作为历史雇佣快照，不再刷新 |

**切换 primary_provider 到 employment provider 的刷新流程**：

```
[1] OAuth callback → cs-user 命中已存在 user
[2] primary_provider := aad（session-scoped，仅影响当前 JWT）
[3] 检查 aad ∈ employment_providers.enabled
   ├─ ✗ 不在列表 → 跳过 ApplyEnterpriseMapping，签发 JWT（enterprise claim 取现有快照）
   └─ ✓ 在列表 → 继续 [4]
[4] ApplyEnterpriseMapping 重新求值：
   ├─ source_claims 来自 aad 的 raw_claims
   ├─ enterprise_uid 校验（§6.5.1 immutability，按 uid_immutability 配置）：
   │   ├─ 新 uid == 现有 uid → 正常刷新 employee_id / department / title / ... 非_uid 字段
   │   ├─ 新 uid != 现有 uid + enforce → 403 "enterprise_uid mismatch"
   │   ├─ 新 uid != 现有 uid + allow_change_with_audit → 覆盖 uid + 写审计日志
   │   └─ aad 无 uid 信息 → 保留现有 uid + 其他字段视配置可空或保留快照
   └─ UPDATE enterprise_identities SET ... WHERE user_id AND tenant_id
[5] 类 B / D 字段（display_name / email / phone / avatar_url）不重写
[6] 签发新 JWT，primary_provider=aad，enterprise claim = 刷新后快照
```

> **设计要点**：雇佣上下文是**用户属性**，由 tenant 信任的 employment provider 提供，与"用户当前用什么 IdP 登录"解耦。这避免了"用户用 GitHub 登录后雇佣身份消失"的诡异行为，也让 tenant 能严格指定哪些 IdP 是权威雇佣源（防止社交 IdP 污染企业组织架构数据）。

#### 11.4.5 绑定解绑与边界

```
DELETE /api/me/identities/:identity_id
  ├─ 校验：identity.user_id == session.user_id
  ├─ 校验：解绑后用户至少保留 1 个有效登录源（防账户失联）
  │     └─ 仅剩 1 个 → 409 "cannot unbind last identity"
  ├─ 软删除 user_auth_identities 行（deleted_at = now()）
  └─ 不影响任何 users / user_profile 字段（绑定时没写，解绑时也不删）

GET /api/me/identities
  └─ 返回当前用户所有绑定的 IdP 列表（含 bound_at / last_used_at / is_primary）
```

**特殊情况**：

| 场景 | 处理 |
|---|---|
| 用户绑了 GitHub + idtrust，先解绑 idtrust | 允许；下次只能用 GitHub 登录；enterprise_identities 行保留（被视为历史雇佣快照，不再刷新）|
| 用户改 email 后再绑新 IdP 返回旧 email | 不覆写；旧 email 仍记录在新 identity 的 raw_claims 里供审计 |
| tenant_admin 强制解绑某用户的 IdP | 走 admin API + 审计日志；同样不动 users 表字段 |
| 同一 IdP 在不同 tenant 的不同 user 上 | 由 external_key 全局唯一约束保证不冲突（§6.2）|
| **首次登录 IdP 未返回 email / phone（类 D 字段为空）** | 用户后续走**显式验证流程**补全（不能靠再绑 IdP 触发自动填充）：<br>`POST /api/me/email/request-change` { email } → 邮件 OTP → `POST /api/me/email/confirm` { code } → 写入 users.email；phone 同理走短信码。tenant 可在 `tenant_configs.features.require_email_at_onboarding` 强制 onboarding 页一并补全 |

**类 D（email/phone）自助补全端点**（区别于 §11.3.4 的 PATCH /api/me/profile，类 D 字段必须走验证流程）：

```
POST /api/me/email/request-change      # 触发邮件 OTP
  Body: { "email": "alice@new.com" }
  Resp: 202 { "pending_token": "..." }   # 5min TTL

POST /api/me/email/confirm             # 验证 OTP 并写入
  Body: { "pending_token": "...", "code": "123456" }
  Resp: 200 { email, email_verified: true }

POST /api/me/phone/request-change      # 触发短信码
POST /api/me/phone/confirm             # 验证短信码并写入
```

> 类 D 字段（email/phone）即便用户手改也必须经 OTP 验证，与类 B（display_name 等）直接 PATCH 不同。理由：email 是全局唯一防钓鱼键（§6.1）+ 密码找回渠道，必须证明归属权。

#### 11.4.6 字段填充矩阵（一图速查）

```
                          首次登录                   二次绑 IdP                 用户手改        切换 primary_provider
                                                    employment  非 employment                  employment  非 employment
─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
users.id                  系统生成                  -          -                -              -           -
users.username            onboarding                不覆写     不覆写           v1 ✗           不覆写      不覆写
users.tenant_id           首次写入                  不覆写     不覆写           v1 ✗           不覆写      不覆写
users.email               IdP 求值                  不覆写     不覆写           ✓ 验证流       不覆写      不覆写
users.display_name        IdP 求值                  不覆写     不覆写           ✓ PATCH        不覆写      不覆写
users.avatar_url          IdP 求值                  不覆写     不覆写           ✓ PATCH        不覆写      不覆写
users.locale/timezone     IdP 求值                  不覆写     不覆写           ✓ PATCH        不覆写      不覆写
users.created_by_idp      首次写入                  -          -                -              -           -
users.last_login_at       首次写入                  更新       更新             -              更新        更新
user_profile.*            IdP 求值                  不覆写     不覆写           ✓ PATCH        不覆写      不覆写
enterprise_identities.*   employment provider 写入  按 strategy 不动           ✗              按 refresh  不动
user_auth_identities      INSERT 首行               INSERT 增量行               -              不新增行    不新增行
```

> **核心原则一句话**：**身份合并按 append-only + first-write-wins；雇佣上下文（enterprise）绑定到 user，仅由 tenant 信任的 employment provider 写入/刷新，与 primary_provider 解耦**。这两条共同保证用户不会因多 IdP 绑定或登录方式切换导致身份信息被无意覆盖，也让 tenant 能严格指定哪些 IdP 是权威雇佣源。

---

## 12. JWT 机制：双层契约（Casdoor JWT / 业务 JWT）+ claims + 选型

### 12.1 claims 结构（标准化业务身份）

业务 JWT claims 采用**嵌套结构**，按职责分组：标准注册 claims、Tenant 上下文、用户基本身份（`user`）、企业身份（`enterprise`，Map 类型）、成员关系、会话元数据。

```jsonc
{
  // === 标准注册 claims（RFC 7519）===
  "iss": "https://cs-user.example.com",
  "sub": "u_abc123def456",                     // user_id (UUID)，跨登录源不变
  "universal_id": "u_abc123def456",            // 同 sub（cs-cloud 主 UserID 来源，不可降级，§9.2 硬约束 #2 / GLOSSARY §2.1 三字段同值约定）
  "aud": ["cs-cloud", "app-ai-native", "csc"], // 受众白名单
  "iat": 1730000000,
  "nbf": 1730000000,
  "exp": 1730001800,
  "jti": "<uuid>",                              // 撤销 / 审计用

  // === Tenant 上下文 ===
  "tenant_id": "t_acme_uuid",
  "tenant_slug": "acme",
  "tenant_edition": "enterprise",
  "tenant_roles": ["tenant_admin"],
  "platform_admin": false,                       // platform_admin 用户此处 true，tenant_id 为 null

  // === 用户基本身份信息（base + profile 层，§6 4 层 UserInfo）===
  "user": {
    "id": "u_abc123def456",                      // 同 sub，业务表 FK
    "username": "alice",                         // tenant 内唯一
    "display_name": "Alice Wang",
    "email": "alice@acme.com",                   // 用户主邮箱
    "email_verified": true,
    "phone": "+86-138xxxx1234",
    "phone_verified": true,
    "avatar_url": "https://avatars.../alice.png",
    "locale": "zh-CN",
    "timezone": "Asia/Shanghai",
    "status": "active"                           // active / suspended / deleted
  },

  // === 企业身份信息（enterprise 层，Map 类型，由 IdP 字段映射填充）===
  // 标准字段名固定；具体值由 tenant 级 provider_mapping yaml 从 IdP 字段映射
  "enterprise": {
    "uid": "EMP001",                             // 企业身份唯一标识（tenant 内唯一），默认 = employee_id，详见 §6.5.1
    "employee_id": "EMP001",                     // 工号
    "title": "Senior Engineer",                  // 职位
    "department": "Platform Team",               // 部门
    "enterprise_email": "alice@enterprise.com",  // 企业邮箱（可能与 user.email 不同）
    "cost_center": "CC-PLAT-001",                // 成本中心
    "manager": "bob@enterprise.com",             // 直属上级
    "level": "P7",                               // 职级
    "location": "Beijing",                       // 办公地
    "joined_at": "2023-05-15",                   // 入职日期 (ISO 8601 date)
    // tenant 自定义字段（详见 §18，由 enterprise_schema_ext 定义）
    "custom_fields": {
      "project_code": "PRJ-X1",
      "security_clearance": "L2"
    }
  },

  // === 成员关系（轻量级，避免下游频繁查库）===
  "groups": ["acme:member", "dept:engineering"],

  // === 会话与认证元数据 ===
  "sid": "sess_xxx",                             // session id
  "auth_time": 1730000000,                       // 最近一次 IdP 认证时间
  "amr": ["pwd", "mfa"],                         // 认证方法引用 (RFC 8176)
  "primary_provider": "idtrust",                 // 当前 session 主 IdP
  "scope": "capability:read capability:write",

  // === onboarding 状态（§11.3）===
  "onboarding_required": []                      // 空数组 = 已完成；非空 = 待完成步骤，如 ["username"]
}
```

#### 用户基本身份 (`user`) 标准字段

| 字段 | 类型 | 必填 | 来源 | 说明 |
|---|---|---|---|---|
| `id` | UUID | ✓ | cs-user 自生成 | 同 `sub` / `universal_id`（三字段同值，GLOSSARY §2.1），业务表 FK |
| `username` | string | onboarding 完成后 ✓ | cs-user 自管 | tenant 内唯一（§6.1），**v1 一经确认不可变**（§11.3）；onboarding 窗口期为 NULL |
| `display_name` | string | ✗（推荐）| IdP / 用户编辑 | 展示名，用户可随时修改（`PATCH /api/me/profile`，§11.3.4）|
| `email` | string | ✓ | IdP / 用户绑定 | 用户主邮箱，全局唯一（§6.1）|
| `email_verified` | bool | ✓ | cs-user 校验 | OIDC 标准 |
| `phone` | string | ✗ | IdP / 用户绑定 | E.164 格式 |
| `phone_verified` | bool | ✗ | cs-user 校验 | — |
| `avatar_url` | string | ✗ | IdP / 用户上传 | — |
| `locale` | string | ✗ | IdP / 用户设置 | BCP 47（如 zh-CN）|
| `timezone` | string | ✗ | 用户设置 | IANA tz（如 Asia/Shanghai）|
| `status` | enum | ✓ | cs-user 自管 | active / suspended / deleted |

#### 企业身份 (`enterprise`) 标准字段（Map）

| 字段 | 类型 | 必填 | 默认 IdP 来源（可重映射）| 说明 |
|---|---|---|---|---|
| `uid` | string | ✗（推荐）| 同 `employee_id`，由 `enterprise_field_mapping.uid.source` 配置（§12.1.1）| **企业身份唯一标识**（tenant 内唯一），用于部门映射定位 / 跨系统关联，详见 §6.5.1 |
| `employee_id` | string | ✗ | `${idp.employeeNumber}` / `${aad.employeeId}` | 工号 |
| `title` | string | ✗ | `${idp.title}` / `${aad.jobTitle}` | 职位 |
| `department` | string | ✗ | `${idp.department}` / `${aad.department}` | 部门（可经 transformer 转换）|
| `enterprise_email` | string | ✗ | `${idp.mail}` / `${aad.mail}` | 企业邮箱 |
| `cost_center` | string | ✗ | `${idp.costCenter}` | 成本中心 |
| `manager` | string | ✗ | `${idp.manager}` | 直属上级（可经 dn_to_email 转换）|
| `level` | string | ✗ | `${idp.employeeType}` | 职级 |
| `location` | string | ✗ | `${idp.physicalDeliveryOfficeName}` | 办公地 |
| `joined_at` | date (ISO 8601) | ✗ | `${idp.whenCreated}` | 入职日期（经 ldap_date_to_iso 转换）|
| `custom_fields` | Map<string, any> | ✗ | tenant 自定义（§18）| tenant 级扩展字段，schema 由 `enterprise_schema_ext` 定义 |

> 「Map 类型」语义：`enterprise` 对象本身是字段→值的映射；`custom_fields` 是嵌套 Map，承载 tenant 自定义字段。下游服务可按 key 直接读取，无需预定义 schema。

**platform_admin 不属于 tenant**：claims 中 `tenant_id=null` + `tenant_roles=[]` + `platform_admin=true`，且 `enterprise={}`（platform_admin 无企业身份）。

### 12.1.1 企业身份字段映射配置（tenant 级，可配置）

企业身份字段从 IdP 返回的原始 claims 经 **tenant 级 provider_mapping yaml** 映射填充。映射规则、transformer、优先级均可在 tenant 级配置，覆盖平台默认值。

#### 配置示例

```yaml
# tenant_configs/acme/provider_mapping.yaml
enterprise_field_mapping:
  # 企业身份唯一标识（首位稳定 key，详见 §6.5.1）
  uid:
    source: "${provider.employeeNumber}"   # 默认 employee_id 来源；可指向其他稳定字段（如 badge_id）
    required: true                          # acme 强制要求工号，缺失拒绝登录
    unique_in_tenant: true                  # 校验 (tenant_id, uid) 唯一（与 DB 索引双重保障）
    validator: regex
    validator_args: { pattern: '^EMP\d+$' }

  # 标准字段 ← IdP 字段路径（点号路径 + ${} 插值）
  employee_id:
    source: "${provider.employeeNumber}"    # 通常与 uid 同源；若企业有不同 badge_id 体系可分离
    required: true
    validator: regex
    validator_args: { pattern: '^EMP\d+$' }

  title:
    source: "${provider.title}"
    fallback: "${provider.jobTitle}"  # 主字段缺失时尝试 fallback
    transformer: trim                 # 去除前后空白

  department:
    source: "${provider.department}"
    transformer: dept_code_to_name    # 应用 dept_code → dept_name 转换器（§17.3）；可按 uid 查用户当前部门

  enterprise_email:
    source: "${provider.mail}"
    validator: email
    required: true

  cost_center:
    source: "${provider.costCenter}"
    default: "CC-DEFAULT"             # source 与 fallback 都缺失时填默认值

  manager:
    source: "${provider.manager}"
    transformer: dn_to_email          # LDAP DN → email（如 CN=Bob,OU=... → bob@acme.com）

  joined_at:
    source: "${provider.whenCreated}"
    transformer: ldap_date_to_iso     # 20230801000000Z → 2023-08-01

  # 自定义字段（详见 §18 enterprise_schema_ext）
  custom_fields:
    project_code:
      source: "${provider.extensionAttribute1}"
    security_clearance:
      source: "${provider.extensionAttribute2}"
      validator: enum
      validator_args: { values: [L1, L2, L3] }

# 多 IdP 来源合并优先级（用户绑多个 IdP 时按顺序合并，靠前的优先）
# uid / 标准字段都按此优先级求值
# 注意：priority_providers 中的 IdP 必须 ∈ tenant_configs.employment_providers.enabled（§11.4.4）；
#       非 employment provider（如 github / feishu）即使列在此处也会被静默忽略，避免社交 IdP 污染雇佣上下文
priority_providers:
  - idtrust           # 最高（主企业 IdP）
  - azure_ad
  # github / feishu 不列出（非 employment provider，不参与雇佣字段求值）

# 字段合并策略
merge_strategy:
  uid: first_wins                     # uid 按 priority_providers 第一个非空值
  standard_fields: first_wins         # 按优先级，第一个非空值胜出
  custom_fields: last_wins            # 自定义字段允许低优先级覆盖（业务侧最新数据更可信）
```

#### `uid` 字段（企业身份唯一标识）特殊处理

`uid` 是 §6.5.1 定义的 tenant 内企业身份唯一键，求值时有以下额外约束：

| 维度 | 约束 |
|---|---|
| 唯一性 | `(tenant_id, uid)` 在 `enterprise_identities` 表内唯一（DB partial unique index + 应用层 validator 双重保障）|
| 冲突处理 | 求值后写入前先查 `(tenant_id, uid)`；若已被其他用户占用 → 拒绝登录并报警（疑似 IdP 配置错误或员工账号复用）|
| 缺失处理 | 若 `required: true` 且 priority_providers 全部为空 → 拒绝登录；若 `required: false` → `enterprise.uid=null`，业务侧按个人用户处理 |
| 不可变性 | 一旦写入不可改；如员工换工号，走「注销旧 identity（带 1 年保护）+ 创建新 identity」流程（类似 §6.4 username_history） |
| 跨企业场景 | 模式 B（集团子公司）下，不同子公司的 uid 体系可能不同（EMP001 vs A-12345），但 `(tenant_id, uid)` 仍唯一——只要在 tenant `acme-group` 内不冲突即可 |

#### transformer 注册表（cs-user 内置 + tenant 可扩展）

| transformer | 输入 → 输出 | 说明 |
|---|---|---|
| `trim` | string → string | 去前后空白 |
| `lowercase` / `uppercase` | string → string | 大小写转换 |
| `email` | string → string | 邮箱规范化（trim + lowercase）|
| `dn_to_email` | LDAP DN → email | `CN=Bob,OU=...` → `bob@acme.com` |
| `dept_code_to_name` | dept code → dept name | tenant 配置的 code→name 映射查表 |
| `ldap_date_to_iso` | `20230801000000Z` → `2023-08-01` | LDAP 时间格式转换 |
| `timestamp_to_iso` | unix timestamp → ISO 8601 | 通用时间戳转换 |
| `regex_extract` | string → string | 按 regex 提取子串（如工号正则）|
| `enum_map` | enum → enum | tenant 配置的 enum 映射（如 `1→P7, 2→P8`）|

tenant 可在 `tenant_configs[<t_id>].custom_transformers` 注册自定义 transformer（Lua / Go plugin，详见 §17.3）。

#### 求值流程（OAuth callback 时执行）

```go
// cs-user/internal/identity/enterprise_mapper.go
func MapEnterpriseIdentity(
    rawClaimsByProvider map[string]map[string]any,  // {idtrust: {...}, aad: {...}}
    mapping EnterpriseFieldMapping,
    priority []string,
) (EnterpriseIdentity, error) {
    result := EnterpriseIdentity{CustomFields: map[string]any{}}

    for _, stdField := range mapping.StandardFields {
        value, sourceProvider, err := resolveByPriority(
            stdField, rawClaimsByProvider, priority)
        if err != nil {
            if stdField.Required {
                return EnterpriseIdentity{}, fmt.Errorf(
                    "required enterprise field %s missing from all providers", stdField.Name)
            }
            value = stdField.Default
        }
        if value != nil {
            value = applyTransformers(value, stdField.Transformers)
            if err := applyValidator(value, stdField.Validator); err != nil {
                return EnterpriseIdentity{}, fmt.Errorf(
                    "enterprise field %s validation failed: %w", stdField.Name, err)
            }
            result.Set(stdField.Name, value)
        }
    }

    // custom_fields 同理
    for name, cfg := range mapping.CustomFields {
        // ... 同 standard_fields 处理流程
    }

    return result, nil
}
```

求值后写入 `enterprise_identities.attributes` JSONB（详见 §18.2），并在签发业务 JWT 时把 `enterprise` 子对象填入 claims。

#### 平台默认映射 + tenant override

| 层级 | 来源 | 优先级 |
|---|---|---|
| 平台默认 | `cs_user.global_config.default_enterprise_mapping.yaml` | 低 |
| Tenant 级 | `tenant_configs[<t_id>].enterprise_field_mapping` | 高（深合并，§9.3）|

tenant 级配置覆盖平台默认：相同字段名以 tenant 配置为准；不同字段名合并保留。详见 §9.3 配置合并语义。

### 12.2 下游服务消费 tenant

```go
// 业务服务从 JWT 拿 tenant 上下文
func RequireTenant() gin.HandlerFunc {
    return func(c *gin.Context) {
        tenantID := c.GetString("tenant_id")  // 由 auth middleware 从 JWT 注入
        if tenantID == "" {
            c.AbortWithStatusJSON(403, gin.H{"error": "tenant context required"})
            return
        }
        c.Set("tenant_id", tenantID)
        c.Next()
    }
}

// 业务查询自动带 tenant_id
func ListDevices(c *gin.Context) {
    tenantID := c.GetString("tenant_id")
    var devices []Device
    db.Where("tenant_id = ?", tenantID).Find(&devices)
    // ...
}
```

### 12.3 JWT 双层契约（Casdoor JWT vs 业务 JWT）

Casdoor 在本架构中定位为**多登录源协议适配器**（OAuth 2.0 / OIDC / SAML 转发），cs-user 不读 Casdoor 的 organization / user / RBAC 模型。JWT 因此分两层，职责严格隔离：

| 层 | 签发者 | 消费者 | 信任边界 | 用途 |
|---|---|---|---|---|
| **Casdoor JWT**（id_token）| Casdoor | **仅 cs-user** | cs-user 入口一次性消费 | 证明「用户成功通过了某 source 的 OAuth」，提供 sub / email / source_claims |
| **业务 JWT**（cs-user 签发）| cs-user | cs-cloud / app-ai-native / csc / costrict-web / 业务 API | 全内网 | 携带 tenant_id / tenant_roles / 用户快照，**下游唯一信任的凭证** |

**铁律**：

1. Casdoor JWT **不下发到前端 / 不下发到下游服务**。前端拿到的永远是 cs-user 签发的业务 JWT。
2. 下游服务**不信任任何 Casdoor JWT**——它们不知道 Casdoor 存在，只通过 cs-user 的 JWKS 端点验签。
3. Casdoor JWT 在 cs-user 完成「验证 + ResolveOrCreateUserByIdentity + ApplyEnterpriseMapping」后被丢弃，不缓存。
4. 业务侧调 cs-user 之外的任何内部服务，必须带 `Authorization: Bearer <业务 JWT>`。

```
[前端] --Casdoor OAuth--> [Casdoor]
[前端] <--Casdoor JWT--   [Casdoor]
[前端] --Casdoor JWT-->   [cs-user]
[前端] <--业务 JWT--      [cs-user]   ← Casdoor JWT 至此终止
[前端] --业务 JWT-->      [cs-cloud / app-ai-native / csc]
                          （只信任 cs-user JWKS）
```

### 12.4 Go 实现选型

| 用途 | 库 | 版本 | 许可证 | 角色 |
|---|---|---|---|---|
| 业务 JWT 签发 + 验证 | `github.com/golang-jwt/jwt/v5` | v5 | MIT | Go JWT 事实标准，v5 强化 claims 强类型校验，`alg=none` 默认拒绝 |
| 验证 Casdoor JWT 时拉 JWKS | `github.com/MicahParks/keyfunc/v3` | v3 | MIT | golang-jwt 官方 README 推荐的 JWKS 扩展，自动后台刷新 + 未知 kid 触发即时刷新 |

**不选**：

- `zitadel/oidc/v3`：完整 OIDC client+server，cs-user 不是 OP，过度。
- `coreos/go-oidc`：偏 OIDC 客户端，JWT 验证仍要配 golang-jwt，重复依赖。
- `lestrrat-go/jwx/v3`：仅在未来需要 JWE 加密或多 issuer 复杂路由时再升级，v1 不引入。

**所有下游服务统一通过 cs-user 输出的 Go SDK `cs-user/pkg/jwtclaims` 消费业务 JWT**，避免 claims 解析代码漂移。SDK 内部封装 golang-jwt + keyfunc，下游 import 即用。

### 12.5 cs-user 业务 JWT 签发

**算法**：RS256（非对称）。业务 JWT 要被多个下游服务验证，公钥可下发而不暴露签名能力。

**JWKS 端点**：cs-user 暴露 `GET /.well-known/jwks.json`，下游启动时拉取并缓存，定期刷新。密钥轮换不需要重启下游。

**TTL 策略**：

- 业务 JWT `exp`：15min–1h（推荐 30min）
- Refresh token：7d，cs-user 自己签发的 opaque token（不走 JWT，存 DB 可撤销）
- Sliding refresh：业务 JWT 过期 → 前端用 refresh token 调 `POST /api/auth/refresh` → cs-user 重签业务 JWT

**签发约束**：

```go
// 伪代码示意（实际由 cs-user/pkg/jwtclaims 封装）
// 1. 收集所有 IdP 的 raw claims（用户绑多个 IdP 时按 priority_providers 合并）
rawClaimsByProvider := map[string]map[string]any{
    primaryProvider: casdoorClaims["source_claims"].(map[string]any),
    // 已绑其他 IdP 的，从 user_auth_identities.raw_claims 加载
}

// 2. 按 tenant 级 provider_mapping yaml 求值企业身份（§12.1.1）
enterprise, err := MapEnterpriseIdentity(
    rawClaimsByProvider,
    tenant.EnterpriseFieldMapping,    // tenant_configs[t_id].enterprise_field_mapping
    tenant.EnterprisePriorityProviders,
)
if err != nil {
    // 401：必填企业字段缺失（如 acme 要求 employee_id 必填）
    return
}

// 3. 构造业务 JWT claims（嵌套结构，§12.1）
claims := jwt.MapClaims{
    // 标准注册
    "iss": "https://cs-user.example.com",
    "aud": []string{"cs-cloud", "app-ai-native", "csc"},
    "sub": user.ID,
    "universal_id": user.ID,                   // 同 sub（cs-cloud 硬依赖，§9.2 #2 / GLOSSARY §2.1）
    "iat": now.Unix(),
    "nbf": now.Unix(),
    "exp": now.Add(30 * time.Minute).Unix(),
    "jti": uuid.NewString(),

    // Tenant 上下文
    "tenant_id":      tenantID,
    "tenant_slug":    tenantSlug,
    "tenant_edition": tenant.Edition,
    "tenant_roles":   user.TenantRoles,
    "platform_admin": user.IsPlatformAdmin,

    // 用户基本身份（base + profile 层，§12.1）
    "user": map[string]any{
        "id":             user.ID,
        "username":       user.Username,
        "display_name":   user.DisplayName,
        "email":          user.Email,
        "email_verified": user.EmailVerified,
        "phone":          user.Phone,
        "phone_verified": user.PhoneVerified,
        "avatar_url":     user.AvatarURL,
        "locale":         user.Locale,
        "timezone":       user.Timezone,
        "status":         user.Status,
    },

    // 企业身份（Map，由 §12.1.1 字段映射求值得出）
    "enterprise": enterprise.AsMap(),

    // 成员关系 + 会话元数据
    "groups":           user.Groups,
    "sid":              sessionID,
    "auth_time":        authTime.Unix(),
    "amr":              authMethods,
    "primary_provider": primaryProvider,
    "scope":            grantedScope,
}
token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
token.Header["kid"] = currentKeyID
signed, _ := token.SignedString(csUserRSAPrivateKey)
```

**安全检查清单**：

| 项 | 约束 | 检测点 |
|---|---|---|
| alg 锁定 | `jwt.WithValidMethods([]string{"RS256"})` | 防 `alg=none` 与 alg 混淆攻击（golang-jwt README 安全提示） |
| iss 校验 | `jwt.WithIssuer("https://cs-user.example.com")` | 防跨 issuer 重放 |
| aud 校验 | `jwt.WithAudience(currentServiceName)` | 防业务 JWT 被发给其他服务的攻击者重放 |
| exp 强制 | `jwt.WithExpirationRequired()` | v5 显式开关 |
| nbf / iat | 默认校验 | 防未来 token / 时钟倒拨 |
| kid 必填 | 签发时填 `Header["kid"]`，验证时按 kid 查 JWKS | 未知 kid 触发 JWKS 即时刷新 |
| 密钥轮换 | 双 kid 并存窗口 ≥ 2 倍 TTL | 无缝切换 |

### 12.6 Casdoor JWT 验证（仅 cs-user 边界）

```go
// 伪代码示意（实际由 cs-user 内部封装）
jwks, _ := keyfunc.NewDefaultCtx(ctx, []string{
    "https://casdoor.example.com/.well-known/jwks.json",
})
// 启用未知 kid 即时刷新（Casdoor 轮换密钥时无缝）
// keyfunc v3 默认 WithRefreshUnknownKID(true)

parserOpts := []jwt.ParserOption{
    jwt.WithValidMethods([]string{"RS256"}),    // 锁算法
    jwt.WithIssuer("https://casdoor.example.com"),
    jwt.WithAudience("cs-user"),                 // Casdoor 给 cs-user 的 client_id
    jwt.WithExpirationRequired(),
}

token, err := jwt.Parse(casdoorIDToken, jwks.Keyfunc, parserOpts...)
if err != nil { /* 401 */ }

claims := token.Claims.(jwt.MapClaims)
sub := claims["sub"].(string)
email := claims["email"].(string)
sourceClaims := claims["source_claims"]   // 透传给 ApplyEnterpriseMapping
```

验证完成后，Casdoor JWT **不入缓存**，调用栈即结束。下一步进入 ResolveOrCreateUserByIdentity（§11.1 第 [5] 步）。

### 12.7 下游服务消费规范

下游服务（cs-cloud / app-ai-native / csc / costrict-web）**必须**：

1. `go get github.com/costrict/cs-user/pkg/jwtclaims`，调用 `jwtclaims.Middleware()` 装在路由根。
2. 不直接调 `golang-jwt/jwt/v5`，不自己写验证逻辑。
3. 不持有 Casdoor 的任何配置 / 密钥。
4. 启动时拉 cs-user 的 `/.well-known/jwks.json`（由 SDK 完成，自动后台刷新）。
5. middleware 顺序：**签名 → kid 解析 → exp → iss → aud → tenant_id 非空（platform_admin 路径除外）**。

middleware 注入到 gin context 的字段：

| context key | 来源 claim（按优先级 fallback） | 类型 | 用途 |
|---|---|---|---|
| `jwt_user_id` | `universal_id`（首选）/ `sub`（fallback）/ `user.id`（最终 fallback） | string | 业务表 `created_by` / `updated_by` FK |
| `jwt_tenant_id` | `tenant_id` | string | RLS 与查询过滤 |
| `jwt_tenant_roles` | `tenant_roles` | []string | tenant_admin 端点鉴权 |
| `jwt_platform_admin` | `platform_admin` | bool | platform 端点鉴权 |
| `jwt_scope` | `scope` | string | 细粒度操作授权 |
| `jwt_jti` | `jti` | string | 撤销列表查询 |
| `jwt_sid` | `sid` | string | 会话级追踪 / 单点登出 |
| `jwt_user` | `user` | map (UserClaims) | **用户基本身份快照**（id / username / display_name / email / phone / avatar_url / locale / timezone / status），业务侧读取免查库 |
| `jwt_enterprise` | `enterprise` | map (EnterpriseClaims) | **企业身份快照**（uid / employee_id / title / department / enterprise_email / cost_center / manager / level / location / joined_at / custom_fields，详见 §6.5.1），业务侧按 key 直接读取 |
| `jwt_groups` | `groups` | []string | 成员关系鉴权 |
| `jwt_primary_provider` | `primary_provider` | string | 审计 / 风控（用户当前用什么 IdP 登录）|

> **`universal_id` / `sub` / `user.id` 三字段同值约定**：cs-user 签发的 JWT 在 `universal_id`（costrict 历史遗留字段，cs-cloud 主用）/ `sub`（OIDC 标准）/ `user.id`（嵌套快照）三处都填同一个 cs-user 内部 ID（格式 `u_[a-f0-9]{12}`）。SDK 按 `universal_id` → `sub` → `user.id` 顺序 fallback 取值，业务侧统一读 `jwt_user_id`。cs-cloud 仍直接读 `universal_id` 字段以维持零侵入（详见 [IDENTITY_ARCHITECTURE_ROADMAP](./IDENTITY_ARCHITECTURE_ROADMAP.md) §A5 / §下游兼容矩阵）。

> **嵌套字段读取约定**：下游业务代码通过 SDK 提供的 typed accessor 读取嵌套字段，例如 `jwtclaims.UserEnterprise(c).UID` / `jwtclaims.UserEnterprise(c).EmployeeID` / `jwtclaims.UserEnterprise(c).Department`。SDK 内部封装 MapClaims 类型断言与默认值处理，避免业务代码直接操作 `map[string]any`。

下游失效场景（必须 401 / 403）：

- 签名失败 / kid 未知且 JWKS 刷新后仍未知 → 401
- exp 过期 → 401，前端走 refresh
- iss / aud 不匹配 → 401
- `jwt_tenant_id` 与 URL `:tenant_slug` 解析出的 tenant_id 不一致 → 403
- `jti` 命中撤销列表 → 401

---

## 13. 跨租户 SSO 与身份迁移

### 13.1 v1 决策：严格 1 用户 1 tenant

- 用户 `tenant_id` 创建后不可变
- 同一 GitHub 账号（即同一 `external_key`，如 `https://github.com|alice`）不能在多个 tenant 各创建账号（§6.2 全局 unique 约束）

### 13.2 例外：tenant_email_allowlist（**v1 不实现**，仅保留 schema）

如集团子公司场景（员工同时属多个 tenant）：

```sql
INSERT INTO cs_user.tenant_email_allowlist (email, tenant_ids, reason, created_by)
VALUES ('alice@group.com', '{t_acme, t_subsidiary}', '集团共享员工', '<platform_admin>');
```

> **v1 决策**：allowlist 机制涉及 `uq_users_email_global` + `uq_user_auth_identities_external_key` 双全局 unique 约束的 partial index 例外（需 `WHERE email NOT IN (SELECT email FROM tenant_email_allowlist)`），与"身份不可重复"原则冲突。**v1 仅保留 `tenant_email_allowlist` 表 schema，不实现例外逻辑**——所有用户行仍受全局 unique 约束。表保留以便未来需要时直接启用。
>
> 集团子公司场景的临时变通：员工必须用**不同的 email / external_key**在两个 tenant 分别注册，或走 v2 多 tenant membership 演进（§13.3）。

### 13.3 v2 演进方向（不在本提案）

- `user_tenant_memberships` 多对多表：1 用户 N tenant 成员关系
- 用户登录后选 tenant（或最近活跃 tenant 默认）
- 跨 tenant 资源访问（如 platform 内置 cross-tenant search）

---

# Part V：权限模型

## 14. platform_admin / tenant_admin / tenant_member 三级

### 14.1 权限矩阵

| 操作 | platform_admin | tenant_admin | tenant_member | 普通 tenant 用户 |
|---|---|---|---|---|
| 创建 / 删除 tenant | ✓ | ✗ | ✗ | ✗ |
| 查看 / 操作任意 tenant | ✓ | ✗ | ✗ | ✗ |
| 查看本 tenant 用户列表 | ✓ | ✓ | ✗ | ✗ |
| 邀请 / 禁用本 tenant 用户 | ✓ | ✓ | ✗ | ✗ |
| 配置本 tenant provider mapping | ✓ | ✓ | ✗ | ✗ |
| 查看本 tenant 审计日志 | ✓ | ✓ | ✓（只读） | ✗ |
| 改自己 profile | ✓ | ✓ | ✓ | ✓ |
| 创建 capability / device | ✓ | ✓ | ✓ | ✓ |
| 删除 capability | ✓ | role-based | role-based | role-based |

> 注：业务级权限（capability / device 级别）由 `authz` 服务评估，与 user system role 解耦。本提案只覆盖 tenant 维度。

### 14.2 tenant_admin 三级（§7.2）

`owner` > `admin` > `billing`，权限差异见 §7.2。

### 14.3 platform_admin scope

| scope | 权限 |
|---|---|
| `full` | 全部 platform 操作 |
| `support` | 仅查看 tenant 数据 + 重置 tenant_admin 密码（不能改 tenant 配置） |
| `read_only` | 只读，所有 platform 操作被拒 |

---

## 15. API 鉴权与 tenant 隔离规则

### 15.1 鉴权链路

**核心原则**：cs-user API 只接受**业务 JWT**（cs-user 自己签发，见 §12.5）。Casdoor JWT 仅在 OAuth callback 端点（`GET /api/auth/callback/:source`）一次性消费（见 §12.6），不进入常规 API 鉴权链路。

```
请求 → cs-user API（除 /api/auth/callback 外）
   │
   ▼
[1] Auth middleware：验业务 JWT（按 §12.7 下游消费规范）
   ├─ 从 Authorization: Bearer <token> 提取
   ├─ 按 kid 查 cs-user 本地缓存的 JWKS（自身 /.well-known/jwks.json）
   ├─ WithValidMethods([]string{"RS256"}) 防 alg 混淆
   ├─ 校验 iss == "https://cs-user.example.com" + aud 含本服务名
   ├─ 校验 exp / nbf / iat；查 jti 不在撤销列表
   └─ 注入 ctx：jwt_user_id / jwt_tenant_id / jwt_tenant_roles
                  / jwt_platform_admin / jwt_scope / jwt_jti
   │
   ▼
[2] Tenant context middleware（详见 §23.2，含 30 天 JWT 兼容期）：
   ├─ JWT 有 tenant_id claim → 用 JWT 中的 tenant_id
   ├─ JWT 缺失 tenant_id claim（N4 上线后 30 天兼容期内）→ fallback 按 user_id 查 DB
   ├─ JWT tenant_id=null（platform_admin 显式 null）+ X-Tenant-Id header
   │   → 校验 platform_admin → 用 header 中的 tenant_id
   └─ JWT tenant_id=null + 无 header → 403（platform_admin 操作必须显式指定 tenant）

   注：在 Go 中 `c.GetString("jwt_tenant_id")` 对 absent key 与 JSON null 都返回 ""，
       兼容期内统一走 fallback 路径；硬切后路径 [1] 的 fallback 失效。
   │
   ▼
[3] Authorization middleware（按端点）：
   ├─ 用户自助端点：校验 user_id == :id（或 platform_admin）
   ├─ tenant_admin 端点：校验 tenant_roles 含 tenant_admin / owner
   ├─ platform_admin 端点：校验 platform_admin scope
   └─ 跨 tenant 端点：默认 403，仅 platform_admin 可
   │
   ▼
[3.5] Onboarding 拦截 middleware（详见 §11.3.6）：
   ├─ 读 ctx 的 jwt_onboarding_required（claims.onboarding_required）
   ├─ 非空 + 当前端点不在白名单（POST /api/me/username / GET /api/me/username/availability /
   │   GET /api/me / POST /api/auth/logout）→ 428 Precondition Required +
   │   Location: /onboarding/username
   └─ 空或字段缺失 → 通过
   │
   ▼
[4] Handler 执行
   └─ 应用层查询自动带 tenant_id（ctx 注入）
   └─ DB 层 RLS 兜底
```

**例外端点**：`GET /api/auth/callback/:source`（OAuth 回调）与 `POST /api/auth/refresh`（refresh token 换业务 JWT）不走 [1] 业务 JWT 验证：

| 端点 | 入口凭证 | 验证流程 | 出口 |
|---|---|---|---|
| `GET /api/auth/callback/:source` | Casdoor JWT（ID Token）| 按 §12.6 验证 Casdoor JWT（keyfunc 拉 Casdoor JWKS）→ ResolveOrCreateUserByIdentity（§11.1 [5]）→ ApplyEnterpriseMapping | 签发业务 JWT + Set-Cookie |
| `POST /api/auth/refresh` | opaque refresh token（cs-user 自签，DB 存）| 校验 refresh token 有效 + 用户未禁用 + tenant 未暂停 | 重签业务 JWT |

下游服务（cs-cloud / app-ai-native / csc / costrict-web）的鉴权链路与本节 [1]–[4] 一致，区别仅在于：JWKS 来源是 cs-user 的 `/.well-known/jwks.json`，业务逻辑不经 [2] tenant resolution（业务 JWT 已携带 tenant_id），直接进 [3] Authorization middleware。

### 15.2 越权防护规则

| 风险 | 防护 |
|---|---|
| 用户 A 试图查 tenant B 用户数据 | JWT tenant_id 与 URL `:tenant_slug` 不一致 → 403 |
| tenant_admin 跨 tenant 操作 | 同上 + tenant_roles 仅在 JWT tenant 内有效 |
| platform_admin 误操作 | 所有 platform 操作写审计日志 + 二次确认（重要操作） |
| 应用层漏带 tenant_id | RLS 兜底（DB 层过滤） |
| 跨 tenant SQL 注入 | 参数化查询 + RLS 双重保障 |

---

## 16. 越权防护与审计

### 16.1 审计日志表扩展

```sql
ALTER TABLE cs_user.user_center_audit_log
  ADD COLUMN tenant_id UUID,
  ADD COLUMN tenant_role VARCHAR(32),
  ADD COLUMN platform_scope VARCHAR(32);

CREATE INDEX idx_audit_log_tenant
  ON cs_user.user_center_audit_log (tenant_id, created_at DESC);
```

### 16.2 关键审计事件

- 跨 tenant 访问尝试（含被拒）
- tenant_admin 误删用户
- platform_admin 改 tenant 配置
- RLS 策略命中（每 100 条抽样 1 条审计）

---

# Part VI：企业身份与租户配置

## 17. tenant 级 provider mapping yaml

### 17.1 配置存储

```sql
-- §9.2 tenant_configs.provider_mapping 字段（YAML）
INSERT INTO cs_user.tenant_configs (tenant_id, provider_mapping, updated_by)
VALUES ('t_acme', '
providers:
  idtrust:
    enabled: true
    rank: 300
    field_map:
      employee_number: "emp_id"     # acme 的 idtrust 用 emp_id
      cost_center: "cost_ctr"
', '<admin_user_id>');
```

### 17.2 加载与合并

```go
// cs-user 启动 + 每次 tenant_configs 更新
func LoadProviderMapping(tenantID uuid.UUID) (*ProviderMappingConfig, error) {
    global := loadGlobalYAML()                       // 全局默认
    tenant := loadTenantConfig(tenantID)             // tenant 覆盖
    return mergeProviderMapping(global, tenant), nil // 深合并
}

// 缓存：Redis key "provider_mapping:tenant:<id>"，TTL 5min
// 配置更新时主动失效：POST /api/admin/provider-mapping/reload
```

### 17.3 transformer 复用

tenant 级 mapping 复用全局内置 transformer（parse_dn_to_path / lookup_by_employee_number 等）；不同 tenant 只改字段名映射，不改 transformer。

如 tenant 需要全新 transformer（如自定义 LDAP 解析逻辑），走代码 PR（共享 transformer 库），不允许 per-tenant 自定义 transformer（维护成本太高）。

---

## 18. tenant 级 enterprise schema 扩展字段

### 18.1 tenant 自定义字段

```yaml
# tenant_configs[acme].enterprise_schema_ext
extra_fields:
  project_code:
    type: string
    required: false
    description: "Acme 内部项目代码"
  security_clearance:
    type: enum
    values: [L1, L2, L3]
    required: true
  cost_center_code:
    type: string
    indexed: true   # 业务侧常用查询字段
```

### 18.2 字段求值

tenant 自定义字段经 provider mapping 求值后，写入 `enterprise_identities.attributes` JSONB：

```json
{
  "employee_number": "EMP001",
  "cost_center": "CC-AI-001",
  "org_path": "...",
  "...": "...",
  "attributes": {
    "project_code": "PRJ-CS-USER",
    "security_clearance": "L2",
    "cost_center_code": "CC-AI-001-2026"
  }
}
```

### 18.3 索引策略

- 默认：`attributes` JSONB 不加索引（GIN 索引膨胀）
- tenant 标记 `indexed: true` 的字段：自动加表达式索引

```sql
CREATE INDEX idx_enterprise_identities_acme_cost_center
  ON cs_user.enterprise_identities ((attributes->>'cost_center_code'))
  WHERE tenant_id = 't_acme';
```

---

## 19. tenant 级 IdP 接入

### 19.1 IdP 分类：全局 IdP vs 租户专用 IdP

cs-user 把 Casdoor source（= IdP 接入条目）按可见范围分两类：

| 类别 | 配置位置 | 可见性 | 典型例子 |
|---|---|---|---|
| **全局 IdP**（global）| platform_admin 配置，`tenant_scope IS NULL` | 所有 tenant 登录页默认可见（除非某 tenant 显式禁用）| GitHub / Google / Apple / 微信 / 平台默认 password |
| **租户专用 IdP**（tenant-scoped）| tenant_admin 配置，`tenant_scope = <t_id>` | 仅该 tenant 登录页可见 | acme 的 idtrust / Azure AD；contoso 的飞书 / 钉钉 |

**设计意图**：

- 全局 IdP 解决「平台希望所有用户都能用公共社交登录」的诉求（典型 SaaS 公共入口）
- 租户专用 IdP 解决「企业客户要求只能用我家的 Azure AD 登录」的诉求（典型 B2B 企业 SSO）
- 两者通过统一表 `idp_sources` 表达，区别仅在 `tenant_scope` 字段

#### 数据模型

```sql
CREATE TABLE cs_user.idp_sources (
  source_id        VARCHAR(64) PRIMARY KEY,
  display_name     TEXT NOT NULL,
  category         VARCHAR(32) NOT NULL,           -- oidc / saml / cas / ldap / password / oauth
  tenant_scope     UUID,                            -- NULL = global；非 NULL = 租户专用
  casdoor_org      VARCHAR(64) NOT NULL DEFAULT 'costrict-default',
  config           JSONB NOT NULL,                  -- issuer / client_id / scopes / etc.
  -- client_secret 走 vault，不入 DB
  enabled          BOOLEAN NOT NULL DEFAULT true,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  FOREIGN KEY (tenant_scope) REFERENCES cs_user.tenants(id) ON DELETE CASCADE
);

CREATE INDEX idx_idp_sources_global ON cs_user.idp_sources (enabled) WHERE tenant_scope IS NULL;
CREATE INDEX idx_idp_sources_tenant ON cs_user.idp_sources (tenant_scope) WHERE tenant_scope IS NOT NULL;

-- tenant 显式禁用某些全局 IdP（如 acme 禁用 GitHub，只允许员工走企业 SSO）
CREATE TABLE cs_user.tenant_global_idp_overrides (
  tenant_id    UUID NOT NULL REFERENCES cs_user.tenants(id) ON DELETE CASCADE,
  source_id    VARCHAR(64) NOT NULL REFERENCES cs_user.idp_sources(source_id) ON DELETE CASCADE,
  enabled      BOOLEAN NOT NULL,
  PRIMARY KEY (tenant_id, source_id)
);
```

#### 权限矩阵

| 操作 | platform_admin | tenant_admin |
|---|---|---|
| 新增 / 修改 / 删除**全局 IdP** | ✓ | ✗ |
| 新增 / 修改 / 删除本 tenant 的**租户专用 IdP** | ✓ | ✓ |
| 在本 tenant 上禁用 / 启用看到的**全局 IdP** | ✗ | ✓（仅 toggle，不能改 source 配置）|
| 配置 Casdoor 端实际 source（如 client_secret）| ✓ | 通过 cs-user API 间接配置（secret 写 vault）|

#### 登录页 source 列表合并逻辑

```go
// cs-user 内部：按 tenant 合并 source 列表
func ListSourcesForTenant(tenantID string) ([]IDPSource, error) {
    // 1. 全局 IdP：默认启用，减去 tenant 显式禁用的
    var globals []IDPSource
    err := db.Raw(`
        SELECT s.* FROM cs_user.idp_sources s
        WHERE s.tenant_scope IS NULL
          AND s.enabled = true
          AND NOT EXISTS (
            SELECT 1 FROM cs_user.tenant_global_idp_overrides o
            WHERE o.tenant_id = ?
              AND o.source_id = s.source_id
              AND o.enabled = false
          )
        ORDER BY s.display_name
    `, tenantID).Scan(&globals).Error
    if err != nil { return nil, err }

    // 2. 本 tenant 专用 IdP
    var scoped []IDPSource
    err = db.Where("tenant_scope = ? AND enabled = true", tenantID).
        Order("display_name").Scan(&scoped).Error
    if err != nil { return nil, err }

    return append(globals, scoped...), nil
}
```

#### Casdoor 端配置约定

Casdoor 单 organization `costrict-default` 下，所有 source（全局 + 各 tenant 专用）共存。Casdoor **不感知** `tenant_scope`：

- cs-user 跳转 Casdoor `authorize` 端点时**不带 `org=` 参数**（与 §11.2 一致），仅通过 `state.tenant_id` 标识租户
- 如使用 Casdoor 内置登录 UI：Casdoor 会展示所有 source，体验不佳；**推荐** cs-user 自实现 tenant 级登录选择器（按上面合并逻辑渲染按钮），跳 Casdoor 时带选定的 `source_id` 参数直达该 source 的 OAuth 流程

### 19.2 三种 IdP 部署模式

| 模式 | 描述 | 适用 |
|---|---|---|
| **共享 Casdoor + source 元数据区分** | 单 Casdoor 实例 + 单 organization，source 的 `tenant_scope` 字段区分全局/租户专用 | **默认推荐** |
| **共享 Casdoor + multi-organization** | 单 Casdoor 实例，每 tenant 一个 organization | **已否决**（违反「Casdoor 仅作协议层」约束，见 §11.2）|
| **独立 Casdoor（per-tenant silo）** | 每 tenant 独立 Casdoor 实例 | 强隔离 / 合规客户 |

### 19.3 默认模式：共享 Casdoor + source 元数据区分

```
cs-user 解析 tenant → 渲染登录页（按 §19.1 合并逻辑列 source）
   │
   ▼
用户点 GitHub（全局）或 idtrust（acme 专用）
   │
   ▼
cs-user 跳 Casdoor authorize（不带 org=）：
  https://casdoor.example.com/login/oauth/authorize?
    client_id=<cs-user-client>&
    state=<tenant_id+nonce+HMAC>&
    scope=openid+profile+email
   │
   ▼
Casdoor 走完 OAuth → callback 带 Casdoor JWT
   │
   ▼
cs-user 验 Casdoor JWT（§12.6）→ 反查 state.tenant_id → 签业务 JWT
```

所有 tenant 共享单 Casdoor organization（`costrict-default`），tenant 上下文完全由 cs-user 通过 `state.tenant_id` 自管。source 的「全局 / 租户专用」属性仅存在于 cs-user 的 `idp_sources` 表，Casdoor 不感知。

### 19.4 独立 Casdoor（特殊客户）

- 大客户要求物理隔离时部署独立 Casdoor 实例（per-tenant silo）
- cs-user 配置 `tenants[t_id].settings.casdoor_endpoint = 'https://casdoor.acme.com'`
- 登录链路按 tenant 路由到不同 Casdoor endpoint
- 该模式下 §19.1 的「全局 IdP」概念在该 tenant 内不适用（独立 Casdoor 实例的 source 列表完全由该 tenant 自管），但 cs-user 仍按统一 `idp_sources` 表抽象，`tenant_scope` 字段在该 tenant 下恒为 `<t_id>`（无 global source）

---

# Part VII：跨服务边界

## 20. 通用 Git Server 适配层 + tenant 独立实例

> **v3 跨服务分工说明（2026-07-15）**：本节描述的 `GitServerAdapter` 是 **multi-tenant Git server 注册与隔离层**（每个 tenant 绑定一个 Git server 实例），与 [`CS_USER_SERVICE_DESIGN.md`](./CS_USER_SERVICE_DESIGN.md) Part VII 的 `GitServerAdapter`（**team_user 同步专用**，已迁到 @server）**是两个不同的 adapter**，仅命名重合。本节 adapter 的方法按职责拆分归属：
>
> | 方法 | 归属服务 | 理由 |
> |---|---|---|
> | `ProvisionUser` / `GetUserPAT` / `EnsureUserNamespace`（用户级） | **cs-user** | user-level 开户归 cs-user |
> | `EnsureTenantOrg` / `CreateRepo` / `ForkRepo` / `ValidatePushScope` / `ValidateCrossTenantAccess` | **@server** | 业务侧 repo 操作 + 跨 tenant 隔离归 @server（与 workflow / KB / capability init 同侧）|
>
> 下文代码示例中的 `cs-user/pkg/gitserver/adapter.go` 应理解为**接口定义共享 SDK**（建议放在共享 module，如 `pkg/gitserver/`），具体实现由 cs-user 和 @server 各自完成自己负责的方法。如果共享 SDK 不可行，可拆为 `cs-user-side-adapter` + `server-side-adapter` 两个独立 interface。详见 [`TEAM_ORG_UNIFICATION.md`](./TEAM_ORG_UNIFICATION.md) ADR-3 v3。

### 20.1 设计原则：不绑死 Gitea + 严格 1 tenant : 1 git server

cs-user 与 @server 与代码托管服务的关系**抽象为通用 `GitServerAdapter` 接口**，不绑死 Gitea。默认实现是 Gitea adapter，但架构上支持替换为 GitLab / Forgejo / Gerrit / 裸 Git HTTP server 等。

**抽象边界**：

- cs-user / @server 业务代码只调 `GitServerAdapter` 接口（共享 SDK），不直接调 Gitea API
- 不同后端差异（API 路径、认证机制、webhook payload 格式、namespace 模型）封装在各自 adapter 实现
- **严格 1:1 绑定**：每个 tenant 必须绑定**恰好一个** Git server 实例；一个 Git server 实例（`git_servers.server_id`）只能被一个 tenant 引用。**不再支持「多 tenant 共享一个 Git server 实例 + namespace 隔离」模式**（详见 §20.3）

### 20.2 GitServerAdapter 接口

```go
// cs-user/pkg/gitserver/adapter.go
type GitServerAdapter interface {
    // 用户管理
    ProvisionUser(ctx context.Context, user User, tenantID string) (ExternalUser, error)
    GetUserPAT(ctx context.Context, user User) (string, error)

    // namespace 管理
    EnsureTenantOrg(ctx context.Context, tenantSlug string) (OrgRef, error)
    EnsureUserNamespace(ctx context.Context, tenantSlug, username string) (NamespaceRef, error)

    // repo 管理
    CreateRepo(ctx context.Context, ns NamespaceRef, name string, opts RepoOpts) (RepoRef, error)
    ForkRepo(ctx context.Context, src RepoRef, dst NamespaceRef) (RepoRef, error)

    // 跨 tenant 隔离
    ValidatePushScope(ctx context.Context, user User, repoPath string) error
    ValidateCrossTenantAccess(ctx context.Context, userTenant, repoTenant string) error

    // 元数据
    Endpoint() string
    Kind() string   // "gitea" / "gitlab" / "forgejo" / "gerrit" / "git-http"
}
```

**已知 adapter 实现**：

| Adapter | 实现状态 | 协议 | 说明 |
|---|---|---|---|
| `gitea_adapter` | v1 默认 | Gitea API + fork JWT 中间件 + pre-receive hook | 见 §20.6 |
| `gitlab_adapter` | v2 计划 | GitLab API + Group as tenant namespace | GitLab Group 模型天然支持层级 |
| `forgejo_adapter` | v2 计划 | Forgejo API（Gitea fork，协议兼容） | 几乎零成本适配 gitea_adapter |
| `gerrit_adapter` | v2 评估 | Gerrit REST + project as namespace | 适合大代码评审场景 |
| `git_http_adapter` | v2 评估 | 裸 git http-backend + 自建 user/repo 元数据表 | 极简部署，无平台 UI |

### 20.3 部署模式：严格 1 tenant : 1 git server 实例

**唯一支持的模式**：每个 tenant 独立绑定一个 Git server 实例。**已否决**「多 tenant 共享一个 Git server 实例 + namespace 隔离」模式（隔离弱、跨 tenant 故障风险高）。

数据模型上严格 1:1：`tenants.git_server_id` NOT NULL + UNIQUE（一个 `git_server_id` 只能被一个 tenant 引用）。物理部署上有两种形态，但数据模型一致：

| 形态 | 描述 | 适用 |
|---|---|---|
| **物理独立** | 每 tenant 部署独立 Gitea 实例（不同 endpoint），数据完全隔离 | 强隔离 / 合规 / 大客户 |
| **逻辑独立**（共享物理）| 多 tenant 共用同一物理 Gitea 实例（同 endpoint），但 `git_servers` 表里每 tenant 独立一行配置（不同 server_id、不同 API token、不同 fork JWT secret）| 小客户共享资源池 |

不管哪种形态，cs-user 业务代码都把每个 tenant 的 Git server 视为独立实例——adapter 实例按 `git_server_id` 隔离配置与认证。物理共享形态下，namespace 命名约定（§20.5）作为额外隔离手段。

### 20.4 Git server 注册表 + 1:1 严格绑定

#### 数据模型

```sql
-- 平台级 Git server 注册表（每行 = 一个 tenant 的 Git server 配置）
CREATE TABLE cs_user.git_servers (
  server_id     VARCHAR(64) PRIMARY KEY,
  kind          VARCHAR(32) NOT NULL,            -- gitea / gitlab / forgejo / gerrit / git-http
  endpoint      TEXT NOT NULL,                    -- https://gitea-acme.costrict.local
  display_name  TEXT NOT NULL,
  config        JSONB NOT NULL,                   -- adapter 特定配置（API token, fork branch, etc.）
                                                   -- secret 字段走 vault，不入 DB
  is_template   BOOLEAN NOT NULL DEFAULT false,   -- 模板行：新 tenant 创建时 clone 此行
  enabled       BOOLEAN NOT NULL DEFAULT true,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 模板行唯一（全局仅一行 is_template=true，作为新 tenant 默认配置源）
CREATE UNIQUE INDEX idx_git_servers_template
  ON cs_user.git_servers (is_template) WHERE is_template = true;

-- tenant 1:1 严格绑定：NOT NULL + UNIQUE
ALTER TABLE cs_user.tenants
  ADD COLUMN git_server_id VARCHAR(64) NOT NULL REFERENCES cs_user.git_servers(server_id);

-- 1:1 严格约束：一个 git_server_id 只能被一个 tenant 引用
CREATE UNIQUE INDEX idx_tenants_git_server
  ON cs_user.tenants (git_server_id);
```

#### 新 tenant 创建流程

```go
// cs-user/internal/tenant/create.go
func CreateTenant(ctx context.Context, name, slug string, opts CreateTenantOpts) (*Tenant, error) {
    // 1. 创建 tenant 行（git_server_id 暂留空，由事务后续填）
    tenant := &Tenant{ID: uuid.New(), Name: name, Slug: slug, ...}

    // 2. clone 模板 git_server 行，生成该 tenant 专属的新行
    var template GitServer
    db.First(&template, "is_template = true AND enabled = true")
    if template.ServerID == "" {
        return nil, errors.New("no git server template configured")
    }
    tenantServer := &GitServer{
        ServerID:    "gs-" + slug + "-" + shortid(),
        Kind:        template.Kind,
        Endpoint:    template.Endpoint,         // 物理共享形态：同 endpoint
                                                  // 物理独立形态：tenant_admin 创建后改 endpoint
        DisplayName: name + " Git Server",
        Config:      template.Config,            // 深拷贝；secret 字段从 vault 重新拉
        IsTemplate:  false,
    }
    db.Create(tenantServer)

    // 3. 绑定 tenant.git_server_id
    tenant.GitServerID = tenantServer.ServerID
    db.Create(tenant)

    return tenant, nil
}
```

#### adapter 解析

```go
// cs-user/pkg/gitserver/resolver.go
func ResolveAdapterForTenant(tenantID string) (GitServerAdapter, error) {
    var tenant Tenant
    err := db.First(&tenant, "id = ?", tenantID).Error
    if err != nil { return nil, err }

    if tenant.GitServerID == "" {
        // 严格 1:1 下不应发生（NOT NULL 约束保证）
        return nil, errors.New("tenant has no git_server_id bound (data integrity violation)")
    }
    return adapterRegistry.Get(tenant.GitServerID)
}
```

#### platform_admin / tenant_admin 权限

| 操作 | platform_admin | tenant_admin |
|---|---|---|
| 新增 / 修改 / 删除 `git_servers` 模板行 | ✓ | ✗ |
| 为本 tenant 创建新的 `git_servers` 实例行（如改 endpoint / 升级 kind）| ✓ | ✓（仅本 tenant 的） |
| 修改本 tenant 的 `git_servers.config`（如 API token）| ✓ | ✓（仅本 tenant 的）|
| 切换本 tenant 的 `git_server_id`（指向新实例）| ✓ | ✓（需先创建新实例行，再切换 FK）|
| 删除其他 tenant 的 `git_servers` 行 | ✓ | ✗（UNIQUE 约束 + tenant_id 隔离保证）|

### 20.5 namespace 命名约定（物理共享形态适用）

**物理独立形态**下，每 tenant 独占 Git server 实例，namespace 命名自由，无前缀要求。

**逻辑独立（物理共享）形态**下，多 tenant 共用同一物理 Gitea 实例（同 endpoint），通过 namespace 前缀做逻辑隔离（adapter 层仍按 server_id 隔离配置，namespace 是额外防护）：

```
gitea.costrict.local/                          （物理共享形态：多 tenant 同 endpoint）
├── g-acme/                                    ← tenant "acme" 的主 org/group
│   ├── costrict/                              ← acme 的官方能力项 repo
│   ├── costrict-plugins/
│   ├── costrict-mirror/
│   ├── costrict-config/platform-config/
│   └── u-alice/                               ← acme 内 alice 的个人 namespace
├── g-contoso/                                 ← tenant "contoso" 的主 org/group
│   └── ...
└── costrict-system/                           ← 平台级（跨 tenant）
    └── <接管注销用户的 repo>
```

**命名规则**：

- tenant 主 namespace：`g-<tenant_slug>`（前缀 `g-` 防与平台其他 org 冲突）
- tenant 内官方能力项：`g-<tenant_slug>/costrict*`
- tenant 内用户 namespace：`g-<tenant_slug>/u-<username>`
- 平台级 namespace：`costrict-system`

**adapter 实现**：各 adapter 把上述 namespace 约定映射到自身概念——

| adapter | tenant 主 namespace 映射为 | 用户 namespace 映射为 |
|---|---|---|
| gitea_adapter | Organization `g-acme` | Organization `g-acme` 下属无子 org，用 `g-acme/u-alice` repo owner workaround |
| gitlab_adapter | Group `g-acme`（含 subgroup 能力）| Subgroup `g-acme/u-alice` |
| forgejo_adapter | 同 gitea | 同 gitea |
| gerrit_adapter | Project prefix `g-acme/` | Project `g-acme/u-alice` |
| git_http_adapter | 自建目录树 `g-acme/` | 自建子目录 `g-acme/u-alice/` |

### 20.6 Gitea adapter 实现（v1 默认）

Gitea adapter 通过 fork JWT 中间件 + pre-receive hook 实现 tenant 隔离：

fork JWT 中间件从 JWT claims 读 `tenant_id` + `preferred_username`：

- auto-provisioning Gitea user 时把 `tenant_id` 写入 Gitea 自定义字段（fork 加 column）
- repo 创建按 `g-<tenant_slug>/` 前缀强制（fork pre-receive hook 校验）
- 跨 tenant push 拒绝（用户 A 的 PAT 不能 push 到 tenant B 的 org）

> 其他 adapter 实现细节由各 adapter 文档单独维护，本提案不展开。adapter 注册时必须满足 `GitServerAdapter` 接口契约，并通过 cs-user 的 adapter conformance test。

---

## 21. webhook：tenant-scoped 订阅与事件

### 21.1 事件 payload 增加 tenant

```json
{
  "event_id": "evt_xxx",
  "event_type": "user.updated",
  "tenant_id": "t_acme",
  "tenant_slug": "acme",
  "subject": { "user_id": "u_abc123" },
  "data": { "...": "..." }
}
```

### 21.2 订阅方支持 tenant filter

```sql
-- webhook_subscriptions 加 tenant filter
ALTER TABLE cs_user.webhook_subscriptions
  ADD COLUMN tenant_filter UUID[] DEFAULT '{}';
    -- 空 = 全部 tenant；非空 = 仅指定 tenant 的事件
```

订阅方可选：

- **跨 tenant 订阅**（如 platform 监控）：`tenant_filter = '{}'`
- **单 tenant 订阅**（如 acme 内部审计）：`tenant_filter = '{t_acme}'`

---

## 22. costrict-web 业务侧 tenant 上下文传递

### 22.1 业务表加 tenant_id

```sql
ALTER TABLE public.devices ADD COLUMN tenant_id UUID NOT NULL REFERENCES cs_user.tenants(tenant_id);
ALTER TABLE public.capability_items ADD COLUMN tenant_id UUID NOT NULL REFERENCES cs_user.tenants(tenant_id);
-- 所有业务表都加
```

### 22.2 业务查询带 tenant_id

```go
// costrict-web 业务代码
func ListDevices(c *gin.Context) {
    tenantID := c.GetString("tenant_id")  // 从 JWT
    var devices []Device
    db.Where("tenant_id = ?", tenantID).Find(&devices)
}
```

### 22.3 业务 RLS（可选）

costrict-web 业务库也可启用 RLS 兜底（同 §10.2 配置）。

---

# Part VIII：API 变更

## 23. API tenant 上下文传递

### 23.1 三种传递方式

| 方式 | 适用场景 | 示例 |
|---|---|---|
| **JWT claim**（默认） | 用户态请求 | `Authorization: Bearer <jwt>`，JWT 含 `tenant_id` |
| **X-Tenant-Id header** | platform_admin 跨 tenant 操作 | `X-Tenant-Id: t_acme` + platform_admin JWT |
| **URL path**（不推荐） | 极少数场景 | `/api/tenants/:slug/users/...` |

### 23.2 默认所有端点都要求 tenant 上下文（含 30 天 JWT 兼容期）

> **N4 切换的兼容性**：N4 起 JWT 新增 `tenant_id` 等 claim。但 N4 上线瞬间，所有在途 access token（最长 15min）和 refresh token（最长 30d）都不带新 claim。直接强制 `403` 会导致服务级故障。**解决**：30 天兼容期——中间件对缺失 claim 容错（fallback 到 default tenant + 日志告警），兼容期结束后硬切。

```go
// TenantContext middleware（除 platform_admin 端点外，所有端点必经）
// N4 阶段：兼容期 graceFallback=true；N4+30 天后切 graceFallback=false
func TenantContext(graceFallback bool) gin.HandlerFunc {
    return func(c *gin.Context) {
        tenantID := c.GetString("jwt_tenant_id") // Go 区分 absent key 与 null：
                                                 // absent → ""; null → ""
                                                 // 兼容期内二者都走 fallback

        if tenantID == "" {
            // 路径 A：X-Tenant-Id header（仅 platform_admin 允许）
            if h := c.GetHeader("X-Tenant-Id"); h != "" {
                if !isPlatformAdmin(c) {
                    c.AbortWithStatusJSON(403, gin.H{"error": "X-Tenant-Id requires platform_admin"})
                    return
                }
                tenantID = h
            }
        }

        if tenantID == "" {
            // 路径 B：兼容期 fallback（N4 ~ N4+30d）
            if graceFallback {
                userID := c.GetString("jwt_user_id")
                if userID != "" {
                    tID, err := lookupUserTenant(userID) // DB 查询用户当前 tenant_id
                    if err == nil && tID != "" {
                        metrics.IncCounter("jwt_fallback_to_default",
                            map[string]string{"user_id": userID})
                        // 以 WARN 级别记录；N4+30d 硬切前最后 1 周升级到 ERROR
                        log.Warn("jwt missing tenant_id claim; falling back to user row",
                            "user_id", userID, "tenant_id", tID)
                        tenantID = tID
                    }
                }
            }
        }

        if tenantID == "" {
            // 路径 C：兼容期结束 / fallback 也失败
            c.AbortWithStatusJSON(403, gin.H{"error": "tenant context required"})
            return
        }
        c.Set("tenant_id", tenantID)
        c.Next()
    }
}
```

**兼容期切换操作**（N4 上线日 → N4+30 天）：

| 时间点 | `graceFallback` | 行为 |
|---|---|---|
| N4 上线日 | `true` | 缺失 claim 时按 user_id 查 DB 回填；WARN 日志 |
| N4+29 天（最后 1 天） | `true` | 日志升级到 ERROR；监控告警阈值降低 |
| N4+30 天 | `false` | 缺失 claim 直接 `403`；旧 token 全部失效 |

> 此模式镜像 [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md) §16 M0 / M7（JWT 自签 30 天 dual-trust）与 [`CS_USER_SERVICE_DESIGN.md`](./CS_USER_SERVICE_DESIGN.md) §16 M3 / M7（JWT iss 切换 30 天兼容期）。

---

## 24. platform admin API

```http
# tenant 管理
POST   /api/platform/tenants                       创建 tenant
GET    /api/platform/tenants                       列出全部 tenant
GET    /api/platform/tenants/:id                   查任意 tenant 详情
PATCH  /api/platform/tenants/:id                   改 tenant 配置（edition / limits / features）
POST   /api/platform/tenants/:id/suspend           暂停 tenant
POST   /api/platform/tenants/:id/restore           恢复 tenant
POST   /api/platform/tenants/:id/delete            强制注销 tenant（30 天 grace）

# 跨 tenant 用户操作
GET    /api/platform/users                         全局用户列表（含跨 tenant）
GET    /api/platform/users/:id                     查任意 tenant 用户
POST   /api/platform/users/:id/disable             禁用任意 tenant 用户

# allowlist
GET    /api/platform/email-allowlist               查 email 跨 tenant 例外
POST   /api/platform/email-allowlist               加例外
DELETE /api/platform/email-allowlist/:email        删例外

# 审计
GET    /api/platform/audit-logs                    跨 tenant 审计
GET    /api/platform/webhook-deliveries            全局 webhook 状态
```

**鉴权**：所有端点要求 `platform_admin` JWT（scope=full 或对应子权限）。

---

## 25. tenant admin API

```http
# tenant 内用户管理
GET    /api/tenants/me/users                       本 tenant 用户列表（带筛选）
GET    /api/tenants/me/users/:id                   本 tenant 用户详情
PATCH  /api/tenants/me/users/:id                   改用户 profile / role
POST   /api/tenants/me/users/:id/disable           禁用本 tenant 用户
POST   /api/tenants/me/users/:id/enable            恢复
POST   /api/tenants/me/users/:id/delete            强制注销

# tenant 配置
GET    /api/tenants/me/config                      查 provider mapping / features
PATCH  /api/tenants/me/config                      改配置（含热加载）
POST   /api/tenants/me/config/provider-mapping/reload   热加载
GET    /api/tenants/me/config/enterprise-schema    查 enterprise schema ext
PATCH  /api/tenants/me/config/enterprise-schema    改扩展字段

# tenant 内 admin 管理
GET    /api/tenants/me/admins                      列出 admin
POST   /api/tenants/me/admins                      指派 admin（owner 权限）
DELETE /api/tenants/me/admins/:user_id             撤销 admin（owner 权限）

# tenant 审计
GET    /api/tenants/me/audit-logs                  本 tenant 审计
GET    /api/tenants/me/quota                       本 tenant 配额使用
```

**鉴权**：JWT 必须包含 `tenant_id == :tenant_slug` 对应 ID + `tenant_roles` 含 `tenant_admin` / `owner`。

---

# Part IX：实施与迁移

## 26. 默认 tenant 引导（brownfield 迁移）

### 26.1 创建默认 tenant

```sql
-- 迁移期：把现有所有用户归入"默认 tenant"
INSERT INTO cs_user.tenants (tenant_id, slug, display_name, status, edition, email_domains, features, limits)
VALUES (
    't_default_uuid',
    'default',
    'Default Tenant (Legacy)',
    'active',
    'enterprise',
    '{}',
    '{}',
    '{}'
);

-- 现有 users 表填 tenant_id
UPDATE cs_user.users
SET tenant_id = 't_default_uuid'
WHERE tenant_id IS NULL;

-- user_auth_identities / user_profile / enterprise_identities 同理
```

### 26.2 现有用户无感知

- 现有用户登录后 JWT 自动带 `tenant_id=t_default_uuid` / `tenant_slug=default`
- 子域名 `default.cs-user.example.com` 或 fallback 通用域名 `cs-user.example.com`
- 现有 capability / device / repo 全部归 default tenant

### 26.3 新建真实 tenant

- 平台管理员后台创建 Acme tenant
- `tenants[email_domains] := ['acme.com']`
- Acme 员工首次登录 → 邮箱域识别命中 → 归入 acme tenant（不再进 default）
- default tenant 内 Acme 员工可由 platform_admin 批量迁移（如需）

---

## 27. 分阶段切换路径

| 阶段 | 周期 | 关键产出 |
|---|---|---|
| **N0：tenant 表 + 默认 tenant 引导** | 1 周 | tenants / tenant_admins / platform_admins 表；**同时**对 users / user_auth_identities / user_profile / enterprise_identities / user_gitea_binding 五张表 `ADD COLUMN tenant_id`（multi-step: 先 nullable → backfill `'t_default_uuid'` → `SET NOT NULL`）+ brownfield 全表回填 default tenant |
| **N1：唯一约束调整 + 索引** | 1 周 | `uq_users_tenant_username`、`uq_users_email_global`、`uq_user_auth_identities_external_key`、各表 `idx_*_tenant` 索引 |
| **N2：RLS 启用** | 1 周 | PostgreSQL Row Security Policy 配置（`ENABLE` + `FORCE`）；三角色（owner / app / platform_admin）+ SECURITY DEFINER 函数；应用层连接池 hook 注入 session 变量 |
| **N3：tenant resolution 登录链路** | 2 周 | 子域 / 邮箱域 / 显式选择三层 fallback；Casdoor multi-organization 接入 |
| **N4：JWT claims 扩展（带 30 天兼容期）** | 1 周 | JWT 加 tenant_id / tenant_slug / tenant_roles；中间件对缺失 claim 容错（fallback 到 default tenant）+ 30 天后硬切；下游 SDK 适配 |
| **N5：tenant 级 provider mapping** | 1 周 | tenant_configs 表；合并语义；热加载 |
| **N6：API tenant 上下文 + 三级权限** | 2 周 | TenantContext middleware；platform / tenant_admin API 端点 |
| **N7：Gitea tenant namespace** | 1 周 | `g-<tenant_slug>/` 前缀；fork pre-receive hook 校验 |
| **N8：webhook tenant-scoped 订阅** | 1 周 | webhook_subscriptions.tenant_filter；事件 payload 含 tenant |
| **N9：costrict-web 业务侧 tenant_id** | 2 周 | 业务表加 tenant_id；查询带过滤；webhook 订阅切换 |
| **N10：稳定 + 监控** | N9+2 周 | 越权检测监控；性能 baseline；RLS 命中率 |

**前置条件**：
- [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md) Stage 0（4 层数据模型 + JWT 自签 + JWKS）已完成。
- [`CS_USER_SERVICE_DESIGN.md`](./CS_USER_SERVICE_DESIGN.md) Stage 1（M0-M8，cs-user 服务独立运行）已完成。
- 本提案**取代** [`CS_USER_SERVICE_DESIGN.md`](./CS_USER_SERVICE_DESIGN.md) §15 开放问题 Q8（"多租户不预留"）—— 业务方向已演进为 B2B SaaS（§1.2）。

**阶段门（gate criteria）**：
- N0 → N1：default tenant 内全部既有用户已写入 `tenant_id`，无 NULL 残留（CI 校验脚本：`SELECT count(*) FROM users WHERE tenant_id IS NULL` 必须为 0）。
- N1 → N2：唯一约束已切换；新写入按新约束；应用代码兼容（无业务查询依赖旧约束）。
- N2 → N3：RLS shadow 模式运行 1 周（记录 RLS 拒绝事件，但不真正拦截），无 false positive。
- N4 → N5：JWT 兼容期生效；30 天后硬切前最后 1 周无 `fallback_to_default` 命中事件。
- N6 → N7：所有 platform / tenant_admin API 覆盖率 ≥ 80%；权限矩阵 e2e 通过。

---

## 28. 风险与对策

| 风险 | 严重度 | 对策 |
|---|---|---|
| **应用层 bug 漏 tenant_id 过滤 → 数据泄漏** | 高 | PostgreSQL RLS 兜底（强制 tenant_id 过滤）；CI lint 规则禁止裸查询；定期跑泄漏检测任务 |
| **子域名 spoofing** | 高 | tenant slug 严格校验（白名单字符）；HSTS + SameSite cookie；CSP 限制 |
| **JWT tenant_id 篡改** | 高 | JWT 签名保护；下游 SDK 校验 `tenant_id` 类型 + 格式 |
| **跨 tenant 钓鱼（alice@acme.com 被诱骗到 contoso tenant）** | 高 | email 全局唯一（默认禁止跨 tenant）；external_key 全局唯一；登录页展示 tenant 品牌 + 域名 |
| **tenant suspension 误伤** | 中 | suspension 走 30 天 grace + 双重确认；webhook 提前通知 |
| **provider mapping 合并错误** | 中 | 配置 schema 验证；dry-run 模式（用 raw_claims 跑映射 + 输出 diff） |
| **RLS 性能影响** | 中 | 索引覆盖（tenant_id 加索引）；监控慢查询；必要时调优 |
| **大 tenant 噪音影响其他 tenant** | 中 | 按 tenant 限流；超限触发 hybrid 升级（独立实例） |
| **跨 tenant 审计困难** | 低 | 审计日志带 tenant_id；admin 后台跨 tenant 查询 |
| **default tenant 数据迁移滞后** | 中 | N0 阶段强制完成；后续新建 tenant 后由 platform_admin 决定是否迁移 default 内的用户 |
| **Casdoor multi-organization 兼容性** | 中 | 提前压测；准备 fallback（单 org + state.tenant_id） |
| **业务表加 tenant_id 大表迁移** | 中 | online schema change（gh-ost / pg_repack）；分批 backfill |
| **platform_admin 误操作跨 tenant 删数据** | 高 | 重要操作二次确认 + 24h 延迟执行；审计日志全量保留；操作可回滚（软删） |

---

# Part X：已决策项与开放问题

## 29. 已决策项

| # | 决策点 | 决议 | 来源 |
|---|---|---|---|
| 1 | 多租户模型 | **Shared（共享基础设施 + 行级隔离）** | 本提案 §3 |
| 2 | tenant 识别 | **三层 fallback：子域 / 邮箱域 / 显式选择** | 本提案 §5 |
| 3 | 用户唯一性 | `(tenant_id, username)` tenant-scoped；`email` 全局唯一（防钓鱼） | 本提案 §6 |
| 4 | external_key 唯一性 | **全局唯一**（禁止跨 tenant 复用） | 本提案 §6.2 |
| 5 | 数据隔离 | 应用层 + PostgreSQL RLS 双层 | 本提案 §10 |
| 6 | tenant 作用域 | 所有用户相关表加 `tenant_id` | 本提案 §8 |
| 7 | 跨服务引用 | 仍用全局 `user_id`（UUID）；JWT 同时带 `tenant_id` | 本提案 §6.3 + §12 |
| 8 | 权限模型 | platform_admin / tenant_admin / tenant_member 三级 | 本提案 §14 |
| 9 | provider mapping | 全局默认 + tenant 覆盖（深合并） | 本提案 §9 + §17 |
| 10 | Gitea 隔离 | 单 Gitea + org namespace `g-<tenant_slug>/` | 本提案 §20 |
| 11 | Casdoor 多租户 | multi-organization 模式（每 tenant 一个 Casdoor org） | 本提案 §11.2 + §19.2 |
| 12 | 跨 tenant 用户 | v1 严格 1 用户 1 tenant；allowlist 是高复杂度特性默认禁用 | 本提案 §13 |
| 13 | 默认 tenant 引导 | 现有数据迁入 default tenant，零感知切换 | 本提案 §26 |
| 14 | tenant 生命周期 | 创建 / 配置 / 暂停（grace）/ 注销（30 天 grace） | 本提案 §4.2 |

## 30. 开放问题

| # | 问题 | 推荐 | 备注 |
|---|---|---|---|
| 1 | 用户是否允许跨 tenant 成员关系（v2 演进）？ | 默认不做；future `user_tenant_memberships` 多对多 | v1 严格 1 用户 1 tenant |
| 2 | silo（独立实例）模式何时启用？ | 大客户 ≥ 10 万用户 / 合规硬性要求时 | 走单独 silo 迁移项目 |
| 3 | tenant 级 UI 品牌定制（LOGO / 主题）？ | 第一阶段不在；后续视客户需求 | 低优先级 |
| 4 | tenant 数据导出 / 携带权？ | 必须支持（合规要求） | `pg_dump --tenant-id` |
| 5 | tenant 级密码策略 / 2FA 差异化？ | 不做差异化；统一 cs-user 安全策略 | 简化 |
| 6 | tenant 级独立 Casdoor 是否支持？ | 支持（`tenants[t].settings.casdoor_endpoint`） | 强隔离客户 |
| 7 | 跨 tenant 协作（如 capability 跨 tenant 分享）？ | v1 不支持；tenant 内部完全隔离 | 后续按需评估 |
| 8 | tenant 配额超限的优雅降级？ | 读 OK / 写拒绝 + 引导升级 | UX 待细化 |
| 9 | webhook 跨 tenant 订阅如何授权？ | platform_admin 显式 approve；记录审计 | 防滥用 |
| 10 | RLS 是否覆盖所有表？ | 所有 tenant-scoped 表必须；全局表（tenants / platform_admins）不覆盖 | 工程约束 |
| 11 | default tenant 长期保留还是清理？ | 保留为 "internal" tenant；用于 CoStrict 内部测试 / dogfood | 不删 |
| 12 | tenant slug 改名？ | 不允许（破坏子域 / Gitea namespace）；display_name 可改 | 强约束 |

---

# 附录

## 附录 A：tenants 表完整 schema

```sql
CREATE TABLE cs_user.tenants (
    tenant_id        UUID PRIMARY KEY,
    slug             VARCHAR(32) NOT NULL,
    display_name     VARCHAR(191) NOT NULL,
    status           VARCHAR(32) NOT NULL DEFAULT 'active',
    edition          VARCHAR(32) NOT NULL DEFAULT 'team',
    email_domains    TEXT[] NOT NULL DEFAULT '{}',
    features         JSONB NOT NULL DEFAULT '{}',
    limits           JSONB NOT NULL DEFAULT '{}',
    settings         JSONB NOT NULL DEFAULT '{}',
    enterprise_config JSONB NOT NULL DEFAULT '{}',  -- tenant 级企业元信息，详见 §6.5
    git_server_id    VARCHAR(64) NOT NULL REFERENCES cs_user.git_servers(server_id),  -- §20.4 严格 1:1
    deletion_requested_at TIMESTAMPTZ,
    deleted_at       TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_slug_format CHECK (slug ~ '^[a-z0-9-]{3,32}$'),
    CONSTRAINT chk_status CHECK (status IN ('active', 'suspended', 'deleted')),
    CONSTRAINT chk_edition CHECK (edition IN ('free', 'team', 'enterprise', 'on_premise'))
);

CREATE UNIQUE INDEX uq_tenants_slug
  ON cs_user.tenants (slug) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX uq_tenants_email_domain
  ON cs_user.tenants (unnest(email_domains)) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX idx_tenants_git_server
  ON cs_user.tenants (git_server_id);   -- §20.4 严格 1:1（一个 git_server_id 只能被一个 tenant 引用）
CREATE INDEX idx_tenants_status
  ON cs_user.tenants (status) WHERE deleted_at IS NULL;
```

**`enterprise_config` JSONB schema**（§6.5）：

```jsonc
{
  "legal_name": "Acme Inc.",                  // 法律主体名（合同 / 发票）
  "display_name": "Acme",                      // 展示名（登录页 / 邮件签名）
  "logo_url": "https://cdn.../acme-logo.png",
  "brand_color": "#FF5733",                    // 登录页主题色
  "contact_email": "admin@acme.com",
  "industry": "technology",                    // 行业（technology / finance / ...）
  "size_band": "1000-5000"                     // 规模区间
}
```

> enterprise_config 仅承载 tenant 级企业元信息（名片性质），不是企业实体。用户的雇佣元数据由 `enterprise_identities.enterprise_uid` 等字段承载（§6.5.1）。

## 附录 B：tenant-scoped provider mapping yaml 示例

```yaml
# tenant_configs[t_acme].provider_mapping
version: "1.0"

# 仅声明 override / 新增；其他字段继承全局
providers:
  idtrust:
    rank: 300                          # 覆盖全局（如全局=200）
    enterprise_sync:
      interval: "6h"                   # acme 要求 6h 同步一次
    field_map:
      employee_number: "emp_id"        # acme idtrust 用 emp_id
      cost_center: "cost_ctr"
      attributes:
        project_code: "proj_code"      # acme 内部字段

  ldap:
    enabled: false                     # acme 不用 LDAP

  aad:
    enabled: true                      # acme 启用 AAD
    field_map:
      employee_number: "employeeId"

# tenant_configs[acme].username_strategy（详见 §11.3.3）
# acme 是企业内部租户，强制用工号做 username（auto 模式，免填写）
mode: auto
source: employee_id                # 直接用 enterprise_identities.employee_id
fallback_to_user_input: false      # 工号缺失视为登录失败（不允许用户自填）
reserve_patterns: ["admin*", "root", "^(cs|costrict)-.*"]
normalize:
  case: lower
  trim: true
  charset: "[a-z0-9._-]"
  min_length: 3
  max_length: 32

# tenant_configs[acme].display_name_strategy（详见 §11.3.5）
source_priority:
  - "idp_claim:name"
  - "compose:given_name+family_name"
  - "username"
fallback_format: "User-{user_id_short}"
allow_user_override: true
max_length: 64

# tenant_configs[acme].employment_providers（详见 §11.4.4）
# acme 把 idtrust / aad / ldap 列为雇佣上下文提供方；github / feishu 仅用于登录
enabled: [idtrust, aad, ldap]
primary_source: idtrust             # 多个 employment provider 时优先用 idtrust 的 claims
resolution_strategy: first_wins     # 多源 claims 冲突时保留首次写入
refresh: on_login                   # 用户每次用 employment provider 登录刷新雇佣字段（员工调岗同步）
uid_immutability: enforce           # enterprise_uid 不一致 → 403（§6.5.1）
```

## 附录 C：RLS（Row Security Policy）配置参考

> **必读**：本附录为完整参考实现，主体设计见 §10.2。`FORCE` 子句、三角色分离、SECURITY DEFINER 函数 都是必需项——缺一即视为 RLS 配置失败。

```sql
-- 1. 启用 RLS + FORCE（所有 tenant-scoped 表）
--    FORCE 强制对 table owner 也生效，杜绝 owner 绕过
ALTER TABLE cs_user.users ENABLE ROW LEVEL SECURITY;
ALTER TABLE cs_user.users FORCE ROW LEVEL SECURITY;
ALTER TABLE cs_user.user_auth_identities ENABLE ROW LEVEL SECURITY;
ALTER TABLE cs_user.user_auth_identities FORCE ROW LEVEL SECURITY;
ALTER TABLE cs_user.user_profile ENABLE ROW LEVEL SECURITY;
ALTER TABLE cs_user.user_profile FORCE ROW LEVEL SECURITY;
ALTER TABLE cs_user.user_system_roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE cs_user.user_system_roles FORCE ROW LEVEL SECURITY;
ALTER TABLE cs_user.enterprise_identities ENABLE ROW LEVEL SECURITY;
ALTER TABLE cs_user.enterprise_identities FORCE ROW LEVEL SECURITY;
ALTER TABLE cs_user.user_gitea_binding ENABLE ROW LEVEL SECURITY;
ALTER TABLE cs_user.user_gitea_binding FORCE ROW LEVEL SECURITY;
ALTER TABLE cs_user.username_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE cs_user.username_history FORCE ROW LEVEL SECURITY;

-- 2. 角色分离（详见 §10.2）
CREATE ROLE cs_user_owner NOLOGIN NOBYPASSRLS;            -- DDL/migration 角色
CREATE ROLE cs_user_app    NOLOGIN NOBYPASSRLS;            -- 运行时角色
CREATE ROLE cs_user_platform_admin NOLOGIN NOBYPASSRLS;    -- 跨 tenant 查询角色
GRANT cs_user_app    TO cs_user_app_login;
GRANT cs_user_platform_admin TO cs_user_pa_login;

-- 3. 创建策略（每张表一条；仅授予 cs_user_app 角色）
--    platform_admin 路径不走 RLS，走 SECURITY DEFINER 函数（步骤 4）
CREATE POLICY tenant_isolation_users ON cs_user.users
  FOR ALL TO cs_user_app
  USING (tenant_id = NULLIF(current_setting('cs_user.tenant_id', true), '')::UUID);

CREATE POLICY tenant_isolation_auth_identities ON cs_user.user_auth_identities
  FOR ALL TO cs_user_app
  USING (tenant_id = NULLIF(current_setting('cs_user.tenant_id', true), '')::UUID);
-- ... 其他表同理

-- 4. platform_admin 跨 tenant 读取走专用 SECURITY DEFINER 函数
--    （独立登录角色 cs_user_pa_login 才有 EXECUTE 权限）
CREATE FUNCTION cs_user.list_all_users(p_offset INT, p_limit INT)
RETURNS SETOF cs_user.users
LANGUAGE sql SECURITY DEFINER SET search_path = cs_user, pg_temp AS $$
  SELECT * FROM cs_user.users
  WHERE deleted_at IS NULL
  ORDER BY created_at DESC
  OFFSET p_offset LIMIT p_limit;
$$;
REVOKE EXECUTE ON FUNCTION cs_user.list_all_users FROM PUBLIC;
GRANT  EXECUTE ON FUNCTION cs_user.list_all_users TO cs_user_platform_admin;

-- 5. 应用层连接池 hook（Go pgx 示例，BeforeAcquire 重置 GUC）
func setupConn(ctx context.Context, conn *pgx.Conn) error {
    tenantID := ctx.Value(TenantIDKey).(string)
    if _, err := conn.Exec(ctx, "SET cs_user.tenant_id = $1", tenantID); err != nil {
        return err
    }
    return nil
}
// 注：不再设置 is_platform_admin GUC；platform_admin 走独立连接池（cs_user_pa_login）

-- 6. platform_admin NULL tenant_id 行的处理
--    users 表中 platform_admin 的 tenant_id 可为 NULL；这些行只能通过
--    cs_user_pa_login 角色 + SECURITY DEFINER 函数访问，
--    普通用户查询（cs_user_app 角色）因 RLS WHERE 子句匹配 NULL 永远不可见。

-- 7. 测试
SET cs_user.tenant_id = 't_acme';
SELECT * FROM cs_user.users;             -- 仅返回 t_acme 的用户
RESET cs_user.tenant_id;
SELECT cs_user.list_all_users(0, 100);   -- platform_admin 跨 tenant 查询

-- 8. 性能影响监控
EXPLAIN ANALYZE SELECT * FROM cs_user.users WHERE username = 'alice';
-- 期望：使用 idx_users_tenant_username 索引
```

## 附录 D：tenant.* webhook 事件 payload schema

> 6 个新增 tenant.* 事件继承 [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md) §10.2 的投递契约：6 次指数退避（1s / 5s / 30s / 2min / 10min / 1h）→ 死信队列 → admin 手工处理 + 每日全量对账。

### D.1 `tenant.created`

```json
{
  "event_id": "evt_abc123",
  "event_type": "tenant.created",
  "event_time": "2026-07-10T10:00:00Z",
  "subject": {"tenant_id": "t_acme_uuid"},
  "data": {
    "tenant_id": "t_acme_uuid",
    "slug": "acme",
    "display_name": "Acme Corp",
    "edition": "enterprise",
    "email_domains": ["acme.com"],
    "created_by": "u_platform_admin"
  },
  "delivery": {"attempt": 1, "max_attempts": 6}
}
```

### D.2 `tenant.config_changed`

```json
{
  "event_id": "evt_def456",
  "event_type": "tenant.config_changed",
  "event_time": "2026-07-10T10:05:00Z",
  "subject": {"tenant_id": "t_acme_uuid"},
  "data": {
    "tenant_id": "t_acme_uuid",
    "slug": "acme",
    "changes": {
      "limits.max_users": {"old": 50, "new": 200},
      "features.advanced_sso": {"old": false, "new": true}
    },
    "changed_by": "u_platform_admin"
  }
}
```

### D.3 `tenant.suspended`

```json
{
  "event_id": "evt_ghi789",
  "event_type": "tenant.suspended",
  "event_time": "2026-07-10T10:10:00Z",
  "subject": {"tenant_id": "t_acme_uuid"},
  "data": {
    "tenant_id": "t_acme_uuid",
    "slug": "acme",
    "reason": "billing_overdue",
    "suspended_by": "u_platform_admin",
    "effective_at": "2026-07-10T10:10:00Z"
  }
}
```

### D.4 `tenant.restored`

```json
{
  "event_id": "evt_jkl012",
  "event_type": "tenant.restored",
  "event_time": "2026-07-10T10:15:00Z",
  "subject": {"tenant_id": "t_acme_uuid"},
  "data": {
    "tenant_id": "t_acme_uuid",
    "slug": "acme",
    "restored_by": "u_platform_admin",
    "effective_at": "2026-07-10T10:15:00Z"
  }
}
```

### D.5 `tenant.deletion_requested`

```json
{
  "event_id": "evt_mno345",
  "event_type": "tenant.deletion_requested",
  "event_time": "2026-07-10T10:20:00Z",
  "subject": {"tenant_id": "t_acme_uuid"},
  "data": {
    "tenant_id": "t_acme_uuid",
    "slug": "acme",
    "grace_until": "2026-08-09T10:20:00Z",
    "requested_by": "u_tenant_owner"
  }
}
```

### D.6 `tenant.deleted`（hard）

```json
{
  "event_id": "evt_pqr678",
  "event_type": "tenant.deleted",
  "event_time": "2026-08-09T10:20:00Z",
  "subject": {"tenant_id": "t_acme_uuid"},
  "data": {
    "tenant_id": "t_acme_uuid",
    "slug": "acme",
    "hard_deleted_at": "2026-08-09T10:20:00Z",
    "user_count": 150,
    "cascaded_tables": ["users", "user_auth_identities", "user_profile",
                        "enterprise_identities", "user_gitea_binding"]
  }
}
```

## 附录 E：现有调用点改造清单

| 调用点 | 迁移后 |
|---|---|
| 所有 cs-user service 方法 | 签名加 `ctx context.Context`；从 ctx 拿 tenant_id |
| `db.Where("id = ?", userID)` | 改 `db.Where("id = ? AND tenant_id = ?", userID, tenantID)` |
| JWT signing | claims 加 `tenant_id` / `tenant_slug` / `tenant_roles` |
| costrict-web 业务查询 | 所有 list / get 加 `.Where("tenant_id = ?", tenantID)` |
| GitServerAdapter（gitea_adapter 默认）| 读 JWT `tenant_id` claims；auto-provisioning 写入 Git server 自定义字段；repo 创建按 `g-<tenant_slug>/` 前缀强制；跨 tenant push 拒绝 |
| webhook event payload | 加 `tenant_id` / `tenant_slug` |
| SDK `userclient.Get(ctx, userID)` | ctx 内自动带 tenant_id（从 JWT 注入） |

---

> 本提案与 [`CS_USER_SERVICE_DESIGN.md`](./CS_USER_SERVICE_DESIGN.md) 严格互补：前者定义"cs-user 服务化 + 标准契约 + 配置化映射"，本提案定义"多租户扩展"。评审通过后，可作为 cs-user Stage 2（多租户能力）的实施基线，与 Stage 1（服务化）顺序衔接。Stage 1 启动后即可并行启动 Stage 2 的设计细化。
