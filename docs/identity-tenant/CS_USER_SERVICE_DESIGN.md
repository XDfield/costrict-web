# cs-user 服务拆分与标准化用户信息设计提案

| 字段 | 内容 |
|---|---|
| 状态 | Draft · 评审中（v2，2026-07-15 v3 决策同步：Part VII team 同步迁出） |
| 作者 | CoStrict 团队 |
| 创建日期 | 2026-07-10 |
| 最近修订 | 2026-07-15（v3 决策同步：Part VII 设计保留作参考，team_user 同步实施迁移到 @server；cs-user 收缩为仅 user-level Gitea 协作。详见 [`TEAM_ORG_UNIFICATION.md`](./TEAM_ORG_UNIFICATION.md) ADR-3 v3） |
| 评审范围 | server（承担 team-level GitServerAdapter + 业务侧 repo 操作）/ cs-user（新，承担 user-level Gitea 开户）/ costrict-web（瘦身后）/ casdoor / gitea fork / app-ai-native / csc / cs-cloud |
| 关联文档 | [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md)（用户中心主权架构基线）、[`CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md`](../repo-management/CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md)（§6 用户中心 + JWT 中间件）、[`IDENTITY_FEDERATION_DECISION.md`](./IDENTITY_FEDERATION_DECISION.md)（v3 身份联邦决策）、[`USER_TABLE_DESIGN.md`](../proposals/USER_TABLE_DESIGN.md)（现有 users 表）、[`MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md`](../proposals/MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md)（多 provider 绑定方案 B）、[`CLOUD_TEAM_ARCHITECTURE.md`](../proposals/CLOUD_TEAM_ARCHITECTURE.md)（团队/成员关系）、[`TEAM_ORG_UNIFICATION.md`](./TEAM_ORG_UNIFICATION.md)（ADR-3 v3：team 同步归属） |

> 本提案在 [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md) 已决策的"用户中心主权归 costrict-web"基础上推进下一步：**把用户中心从 costrict-web 单体中拆分出来，独立为 `cs-user` 微服务**；同时设计**标准化用户信息结构**（含可选企业身份），并定义**企业身份字段的可配置映射机制**（按登录源 provider 动态映射）。所有身份联邦层面的决策（方案 II + 方式 3）严格继承自既有文档，本提案只补充服务边界、数据契约、映射机制三类实施细节。

---

## TL;DR

**三件事**：

1. **拆 `cs-user` 服务**：把 costrict-web 中所有用户/身份/profile/角色相关代码（`models.User*` / `casdoor/` / `authidentity/` / `user/` / `systemrole/` / `adminuser/` / `middleware/auth*` / `handlers/auth.go` 等）抽到独立 Go 服务 `cs-user`，对外暴露 REST + gRPC API；costrict-web 通过 client 调用，不再持有用户表读写权。

2. **标准化用户信息结构**：定义 `UserInfo` 标准契约（4 层）—— base（不可变 `user_id` + 可变 `username`/`email`/`display_name`/`avatar`）+ identities（已绑定的多 provider 列表）+ profile（业务属性：业务线/部门/角色/配额）+ **enterprise**（可选企业身份：员工号/工号/组织路径/直线经理等）。所有下游服务（costrict-web / cs-cloud / app-ai-native / csc）统一消费该契约。

3. **企业身份字段可配置映射**：不同登录源（LDAP / idtrust / Azure AD / GitHub / 短信）的企业字段命名差异巨大（如 LDAP `departmentNumber` vs idtrust `org_path` vs AAD `department`）。引入 **per-provider field mapping yaml 配置**，登录时按 provider 查映射表，把外部字段归一化进统一 `enterprise` schema；不写死任何 provider 字段名。

**一句话价值**：用户主权独立部署（故障域隔离）+ 跨服务统一契约（消除 N 个客户端 N 种 user struct）+ 企业身份按需扩展（新增 IdP 零代码改动，仅加 yaml 配置）。

---

## 目录

```
Part I：动机与目标
  1. 背景与痛点
  2. 目标与非目标

Part II：cs-user 服务架构
  3. 服务边界（In / Out / Shared）
  4. 部署拓扑与依赖关系
  5. 数据所有权与跨服务引用

Part III：标准化用户信息结构
  6. UserInfo 标准契约（4 层模型）
  7. base / identities / profile / enterprise 分层语义
  8. 序列化格式（REST + gRPC + JWT claims）

Part IV：企业身份可配置映射
  9. 企业身份 schema 与字段清单
  10. per-provider mapping 配置规范
  11. 映射求值流程与示例
  12. 多源企业身份合并策略

Part V：cs-user API 契约
  13. 公开 / 受保护 / 内部 / 管理面 API
  14. 跨服务调用模式（sync / async / cache）
  15. webhook 事件契约

Part VI：实施与风险
  16. 数据迁移与服务切换路径
  17. 风险与对策
  18. 已决策项与开放问题

Part VII：git-server team adapter（团队授权适配层，2026-07-15 新增）
  19. 设计目标与边界
  20. GitServerAdapter 接口设计
  21. 同步流程与失败处理
  22. 与 TEAM_ORG_UNIFICATION 的关系
  23. 开放问题

附录 A：UserInfo 完整 JSON Schema
附录 B：per-provider mapping yaml 完整示例
附录 C：现有 costrict-web 调用点改造清单
```

---

# Part I：动机与目标

## 1. 背景与痛点

### 1.1 现状：用户中心嵌在 costrict-web 单体内

依据 [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md) 已决策方向，用户中心主权已落在 costrict-web，但代码物理上仍嵌在单体 server 内：

```
costrict-web/server/internal/
├── models/models.go                # User / UserAuthIdentity / UserSystemRole 混在业务模型中
├── casdoor/client.go               # Casdoor OAuth client
├── authidentity/normalize.go       # JWT 归一化逻辑
├── user/                           # UserService / AdminService / CachedService
├── systemrole/                     # 角色权限
├── adminuser/                      # 后台用户管理
├── middleware/auth.go              # 全生态共用的 auth 中间件
└── handlers/                       # /api/auth/* /api/users/* 路由混在业务 handler 中
```

**问题**：

| 痛点 | 表现 |
|---|---|
| **职责耦合** | 用户中心代码与 capability / device / project 业务代码共进程，部署耦合、故障耦合 |
| **认证链路单点** | costrict-web 重启 → 所有下游服务 auth 中间件失灵（JWT 验签依赖 costrict-web JWKS） |
| **跨服务契约缺失** | cs-cloud / csc / app-ai-native 各自 `parseJWT` + 本地 cache，UserInfo 字段定义各写一份 |
| **企业身份硬编码** | LDAP `departmentNumber` / idtrust `org_path` 等字段写死在 `authidentity/normalize.go`，新增 IdP 必须改代码 |
| **多 IdP 字段冲突** | 不同 IdP 同语义字段命名不一（工号：`employeeID` vs `emp_no` vs `staffId`），下游无法统一消费 |
| **profile 扩展成本高** | 业务方提新字段（如 cost_center / hierarchy_path）需改 `users` 表 schema + 改 4 个客户端 |
| **测试 / 演进阻塞** | 改 user 模型要发全量 costrict-web，回归范围大 |

### 1.2 已有资产（不推翻重做）

- [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md)：用户中心 4 层数据模型（Identity / Account / Profile / Gitea binding）+ JWT 自签 + webhook 广播系统已设计完成
- [`MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md`](../proposals/MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md)：`user_auth_identities` 表 + provider rank 规则 + 显式绑定方案 B 已实施
- [`USER_TABLE_DESIGN.md`](../proposals/USER_TABLE_DESIGN.md)：`users` 表（含 Casdoor 兼容字段）已落地
- [`IDENTITY_FEDERATION_DECISION.md`](./IDENTITY_FEDERATION_DECISION.md) v2：方案 II + 方式 3（costrict-web 当 OP + fork Gitea JWT 中间件）已决策
- 现有代码：`server/internal/user/` / `authidentity/` / `casdoor/` 等模块边界已较清晰，迁移工作量可控

### 1.3 触发拆分的核心驱动

1. **企业身份需求**：业务方需要从 idtrust / LDAP / 企业 SSO 拉取员工工号、组织路径、直线经理、成本中心等字段，且不同客户接入的 IdP 字段命名各异——**必须配置化**
2. **多客户端契约统一**：cs-cloud / csc / app-ai-native 各自实现 user 解析导致字段漂移——**需要标准化 UserInfo 契约**
3. **故障域隔离**：用户中心是全生态认证根，不应与业务 server 共进程——**独立部署 + 独立扩缩容**
4. **未来演进**：用户中心可能引入 2FA / 风控 / 行为审计等重逻辑——**独立服务便于垂直深化**

---

## 2. 目标与非目标

### 2.1 目标

1. **`cs-user` 服务独立部署**：从 costrict-web 抽出用户中心相关代码（约 8 个 internal package + 6 张表 + 12 个端点）成为独立 Go 服务，独立 DB schema（`cs_user` database 或独立 schema）
2. **标准化 UserInfo 契约**：定义 4 层用户信息结构（base + identities + profile + enterprise），所有下游服务通过统一 client / proto 消费，禁止各自 parse JWT + 写 struct
3. **企业身份字段可配置映射**：per-provider yaml 配置驱动，把外部 IdP 字段（如 LDAP `departmentNumber`）归一化进统一 `enterprise` schema；新增 IdP 仅改 yaml，零代码改动
4. **明确服务边界**：cs-user 仅负责"用户是谁 + 用户属性"，不掺业务（capability ownership / device ownership 等仍在 costrict-web）；costrict-web 通过 client 调 cs-user
5. **JWT 自签 + JWKS 分发迁到 cs-user**：用户中心主权物理转移到 cs-user 服务，costrict-web 仅作为业务消费方
6. **webhook 广播归属 cs-user**：用户变更事件（user.updated / disabled / deleted 等）由 cs-user 发布，下游订阅
7. **零数据丢失迁移**：现有 `users` / `user_auth_identities` / `user_system_roles` 数据全量迁移到 cs-user，对外引用键 `user_id` 保持不变

### 2.2 非目标

- **不重新决策身份联邦方案**：II + 方式 3 已定稿，cs-user 只是物理承载方，不改变信任模型
- **不替换 Casdoor**：Casdoor 继续作多登录源 UI 提供者，cs-user 通过 Casdoor 完成 OAuth 流程后自签 JWT
- **不拆 webhook 基础设施**：通用 webhook 广播框架（`webhook_subscriptions` / `webhook_deliveries`）可作为共享 library 或独立服务存在，本提案只决定"谁发布事件"
- **不引入 OAuth2 OP 标准端点**：`/oauth2/authorize` 等不在范围（cs-user 仍走自签 JWT，不做标准 OIDC OP）
- **不下钻业务权限**：`authz` 服务（capability / device / project 级权限策略）仍留在 costrict-web；cs-user 只管"用户身份 + 系统角色"，不评估业务权限
- **不实现组织架构树**：dept-tree / 业务线层级由 `dept-sync` 服务继续承担，cs-user 仅引用 `dept_id` / `business_line_id` 外键
- **不做用户社交图谱**：好友 / 关注 / 圈子等社交概念 out of scope

---

# Part II：cs-user 服务架构

## 3. 服务边界

### 3.1 In Scope（迁入 cs-user）

| 类别 | 现位置 | 内容 |
|---|---|---|
| **数据表** | costrict-web DB | `users` / `user_auth_identities` / `user_system_roles` / `user_profile` / `user_gitea_binding` / `enterprise_identities` / `username_history`（迁移到 cs-user 独立 schema） |
| **模型 / GORM** | `server/internal/models/models.go` | `User` / `UserAuthIdentity` / `UserSystemRole` / `UserProfile` / `UserGiteaBinding`（迁到 `cs-user/internal/models/`；`EnterpriseIdentity` / `UsernameHistory` 为本提案新增） |
| **Migrations** | `server/migrations/` | 所有 `*user*` / `*auth_identit*` / `*system_role*` / `*gitea_binding*` 迁移文件 |
| **Casdoor client** | `server/internal/casdoor/` | OAuth exchange + userinfo 拉取，整个 package 迁出 |
| **Identity 归一化** | `server/internal/authidentity/normalize.go` | JWT claims 归一化、provider 识别、`external_key` 生成 |
| **User service** | `server/internal/user/` | `UserService` / `AdminUserService` / `CachedUserService` 全部迁出 |
| **System role** | `server/internal/systemrole/` | 角色定义、角色授权、角色检查中间件 |
| **Admin user handlers** | `server/internal/adminuser/` | 后台用户管理端点 |
| **Auth middleware** | `server/internal/middleware/auth.go` + `jwks.go` | JWT 验签、RequireAuth / OptionalAuth 中间件 → 抽成共享 library `cs-user/sdk-go` |
| **Auth handlers** | `server/internal/handlers/auth.go` | `/api/auth/*` 路由 → cs-user 对外端点 |
| **JWT 签发** | （USER_CENTER_DESIGN 待实施） | RS256 私钥 + JWKS endpoint → cs-user 持有 |
| **Webhook 发布** | costrict-web `WebhookService` | 用户事件发布权迁 cs-user；webhook 投递基础设施共用 |
| **Gitea 用户绑定与自动开户** | （USER_CENTER_DESIGN §11 待实施） | `user_gitea_binding` 表 + `GiteaUserSyncWorker`（监听 `user.*` webhook 调 Gitea admin API 做 **user-level** 自动开户）迁 cs-user；与 Gitea fork 中间件协作（fork 调 `/api/internal/users/:id/gitea-binding` 回写绑定）。**仅 user-level**——`team_user` 同步不在 cs-user 范围（v3 决策反转，详见下文） |

> **Gitea 绑定归属说明**：`user_gitea_binding` 是"cs-user 用户 ↔ Gitea user"的桥接表，逻辑上属于用户身份范畴，跟随 OP 主权一起迁 cs-user。`GiteaUserSyncWorker` 也迁 cs-user（仍是 webhook 订阅方 + Gitea admin token 持有方）。`costrict-system` Gitea admin PAT 的"用户生命周期级联"使用场景（V4 §7.2.2 场景 #2）由 cs-user 持有；"capability 索引同步"使用场景（V4 §7.2.2 场景 #1）仍由 costrict-web sync worker 持有——同一 token 双方共享（参考 V4 §7.2.3 共享方条款）。
>
> **team-level 同步 v3 决策（2026-07-15）**：原计划放在 cs-user 的 `team_user` 同步工作（曾规划于本提案 Part VII）**反转到 @server 承担**。cs-user 收缩为仅负责 user-level Gitea 工作（自动开户 + `user_gitea_binding` 维护）；team-level `team_user` 同步统一归集到 @server（实施位置 `server/internal/gitsync/`）。理由：职责内聚（@server 已承担业务侧 Gitea 协作）+ GitServerAdapter 复用 + cs-user 保持纯粹 + 故障隔离。详见 [`TEAM_ORG_UNIFICATION.md`](./TEAM_ORG_UNIFICATION.md) ADR-3 v3。

### 3.2 Out of Scope（保留 costrict-web）

| 类别 | 内容 | 保留原因 |
|---|---|---|
| **业务表** | `capability_items` / `devices` / `repositories` / `projects` / `item_favorites` / `mcp_user_configs` | 业务实体的 ownership，仅以 `user_id` 外键引用 cs-user |
| **业务权限** | `permission_grants` 表 / `authz` 服务 | 业务级 RBAC/ABAC 策略评估与 user 身份解耦 |
| **dept-sync 集成** | 部门树同步、业务线层级 | 真相源是 `dept-sync` 服务，cs-user 仅做外键引用 + cache |
| **业务 webhook 订阅** | capability.push 等业务事件 | cs-user 只发布 user.* 事件；业务事件由 costrict-web 发布 |
| **Gitea 内容同步** | `capability_items` 索引、安全扫描 | 是 Gitea 内容侧关注点，与 user 身份无关 |
| **Gitea `team_user` 表同步** | team-level 成员授权（fork repo 权限）| v3 决策反转（2026-07-15）：由 **@server** 承担。理由：team 同步是业务侧 Gitea 协作的延伸（@server 已承担 workflow / KB / capability init 等），归集到一处更内聚；cs-user 仅保留 user-level 开户 + binding。详见 [`TEAM_ORG_UNIFICATION.md`](./TEAM_ORG_UNIFICATION.md) ADR-3 v3 + 本提案 Part VII（设计参考）|

### 3.3 Shared（跨服务共用）

| 资产 | 形式 | 说明 |
|---|---|---|
| **Auth middleware SDK** | `cs-user/sdk-go` 共享 library | `RequireAuth(jwksURL)` / `OptionalAuth()` / `UserInfoFromContext(ctx)` |
| **UserInfo client SDK** | `cs-user/sdk-go` | `userclient.Get(ctx, userID)` / `BatchGet(ctx, ids)` 带 Redis cache |
| **Webhook 投递基础设施** | 共享 library 或独立 `cs-webhook` 服务 | 6 次指数退避 + 死信队列；cs-user 发布，costrict-web 也可发布业务事件 |
| **JWKS 公钥** | `https://cs-user/.well-known/jwks.json` | 全生态 5min cache |
| **gRPC proto** | `cs-user/api/proto/user.proto` | 跨服务强类型契约 |

### 3.4 边界判定原则

> **核心判据**：数据是"关于用户本身的"还是"关于用户做某事的"。
>
> - "用户本身"（用户名、邮箱、绑定的 provider、profile 偏好、企业身份、系统角色）→ **cs-user**
> - "用户做某事"（用户拥有的设备、用户创建的 capability、用户的权限策略）→ **costrict-web（业务）**

判据细化：

| 问题 | 是 → cs-user | 否 → 留业务 |
|---|---|---|
| 字段是否描述用户身份/属性？ | ✓ | ✗ |
| 字段是否在多个业务场景共用？ | ✓ | ✗（仅单一业务用 → 留业务） |
| 字段是否需要跨服务一致？ | ✓ | ✗（业务本地状态 → 留业务） |
| 字段是否随 user 生命周期变化（而非业务活动）？ | ✓ | ✗ |

---

## 4. 部署拓扑与依赖关系

### 4.1 部署拓扑

```
┌────────────────────── 用户 / AI agent ──────────────────────┐
│  浏览器 / csc / SDK           AI agent（PAT）                │
└───┬────────────────────────────┬─────────────────────────────┘
    │                            │
    ▼                            ▼
┌──────────────────────────────────────────────────────────────┐
│              Casdoor（多登录源 UI 提供者）                    │
│   GitHub OAuth / 短信 / LDAP / idtrust / AAD / 密码           │
└──────────────┬───────────────────────────────────────────────┘
               │ OAuth callback (code)
               ▼
┌──────────────────────────────────────────────────────────────┐
│                  cs-user（用户中心服务）                      │
│                                                              │
│   POST /api/auth/login            颁发 JWT                   │
│   POST /api/auth/refresh           刷新 token                │
│   GET  /.well-known/jwks.json     JWKS 分发                  │
│   GET  /api/users/:id             UserInfo 标准契约          │
│   POST /api/users/batch           批量查询                   │
│   PATCH /api/users/me             改资料                     │
│   POST /api/users/me/identities   绑定 provider              │
│   ...                                                        │
│                                                              │
│   独立 PostgreSQL schema：cs_user                             │
│     users / user_auth_identities / user_profile              │
│     user_system_roles / username_history / enterprise_identities│
│                                                              │
│   独立 secret：JWT RS256 私钥、Casdoor client secret          │
│   配置：provider-mapping.yaml（企业身份字段映射）            │
└──┬─────────────────────────┬────────────────────────┬────────┘
   │ JWT (Bearer / cookie)   │ webhook user.*         │ gRPC / REST
   │                         │                        │ query
   ▼                         ▼                        ▼
┌──────────────────┐    ┌────────────────┐    ┌──────────────────┐
│  全生态服务       │    │ 订阅方：        │    │ costrict-web     │
│  验 JWKS          │    │ - costrict-web │    │ 通过 cs-user-sdk │
│  (Gitea fork /   │    │ - cs-cloud     │    │ 调 user 信息     │
│   cs-cloud /     │    │ - csc-notify   │    │                  │
│   app-ai-native) │    │ - app-ai-native│    │                  │
└──────────────────┘    └────────────────┘    └──────────────────┘
```

### 4.2 依赖方向（关键约束）

```
Casdoor ──► cs-user                       cs-user 拉 Casdoor userinfo
cs-user ──► Casdoor                        cs-user 调 Casdoor token endpoint（仅登录链路）
cs-user ──► costrict-web（无）             cs-user 不依赖任何业务服务
costrict-web ──► cs-user                   业务侧调 cs-user 查 UserInfo（gRPC）
cs-cloud ──► cs-user                       设备端调 cs-user 验 JWT + 查用户
cs-user ──► dept-sync                      cs-user 拉 dept 树做 profile 字段校验（读 only）

禁止：
  cs-user ──► capability_items / devices / projects（任何业务表）
  cs-user 持 admin token 调 Gitea API（除 §7.2 V4 列出的 2 场景）
```

### 4.3 服务发现与网络

| 项 | 值 |
|---|---|
| 服务名 | `cs-user` |
| 对内端点 | `cs-user:8080`（HTTP/REST）+ `cs-user:9090`（gRPC） |
| 对外端点 | 经 gateway 暴露 `/api/auth/*` 与 `/api/users/*` |
| 服务发现 | DNS（docker-compose）/ Consul（k8s） |
| mTLS | 内部网络启用 mTLS（cs-user ↔ costrict-web ↔ cs-cloud） |
| 限流 | cs-user 入口 RPS=1000；批量查询 RPS=100 |

---

## 5. 数据所有权与跨服务引用

### 5.1 数据所有权矩阵

| 数据 | 真相源 | 写权 | 读权 |
|---|---|---|---|
| `users`（user_id / username / email / status） | cs-user | cs-user only | 全生态只读（通过 client） |
| `user_auth_identities` | cs-user | cs-user only | 全生态只读 |
| `user_profile`（business_line / dept / role / preferences / quota） | cs-user | cs-user only | 全生态只读 |
| `user_system_roles` | cs-user | cs-user only | 全生态只读 |
| `username_history` | cs-user | cs-user only | cs-user 内部 |
| `enterprise_identities`（企业身份归一化后） | cs-user | cs-user only | 全生态只读 |
| `user_gitea_binding`（cs-user user ↔ Gitea user 映射） | cs-user | cs-user only | cs-user 内部（fork Gitea 中间件通过 `/api/internal/users/:id/gitea-binding` 回调写入） |
| **业务表 user_id 外键**（devices.user_id 等） | 业务库 | 业务服务 | 业务服务 |
| **业务表 user 快照**（devices.owner_username 冗余字段） | 业务库 | 业务服务 | 业务服务（cache invalidation by webhook） |

### 5.2 跨服务引用键

> 严格沿用 [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md) §4.2 的 `user_id`（不可变 UUID）作为唯一跨服务引用键。

| 字段 | 类型 | 用途 |
|---|---|---|
| `user_id` | UUID | 跨服务引用；下游业务表外键；JWT `sub` claim |
| `username` | VARCHAR(64) | 仅展示与 URL；可改，禁止作外键 |
| `email` | VARCHAR(191) | 仅联系用；可改，禁止作外键 |
| `external_key` | VARCHAR(255) | provider 内唯一身份键；仅 cs-user 内部用 |

### 5.3 业务表 user_id 外键处理

**问题**：迁移后 `devices.user_id` / `capability_items.created_by` 等外键无法跨数据库做 FK 约束。

**方案**：

1. **业务表去掉 SQL FK 约束**，改为应用层校验（写时调 cs-user 验证 user_id 存在 + status=active）
2. **冗余快照字段**（如 `devices.owner_username_snapshot`）：业务表保留 username 冗余字段，user 改名时由 webhook 触发业务侧异步更新快照（最终一致）
3. **批量展示场景**：业务表 join 走 cs-user `BatchGet(user_ids)` API（Redis cache 命中率 > 95%）

```go
// costrict-web 示例：批量查设备 + 用户名
devices := deviceRepo.List()
userIDs := extractUserIDs(devices)
users, _ := userClient.BatchGet(ctx, userIDs)  // cs-user SDK，Redis cache 5min
for _, d := range devices {
    d.OwnerUsername = users[d.UserID].Username  // 注入展示字段
}
```

### 5.4 数据库 schema 隔离

```sql
-- cs-user 独立 PostgreSQL schema（推荐）
CREATE SCHEMA cs_user;

-- 所有 cs-user 表建在 cs_user schema 下
CREATE TABLE cs_user.users (...);
CREATE TABLE cs_user.user_auth_identities (...);
CREATE TABLE cs_user.user_profile (...);
CREATE TABLE cs_user.user_system_roles (...);
CREATE TABLE cs_user.username_history (...);
CREATE TABLE cs_user.enterprise_identities (...);

-- 业务表保留在 public schema
-- devices / capability_items / ... 的 user_id 仅做应用层校验，不做 SQL FK
```

**两种部署形态**：

| 形态 | 描述 | 适用场景 |
|---|---|---|
| **共享 DB，独立 schema** | cs-user 与 costrict-web 共用一个 PostgreSQL 实例，schema 隔离 | 私有化部署 / 小规模 |
| **独立 DB** | cs-user 独占 PostgreSQL 实例 | 公有云 / 大规模 / 强隔离 |

迁移期默认走"共享 DB + 独立 schema"，未来按需切独立 DB。

---

# Part III：标准化用户信息结构

## 6. UserInfo 标准契约（4 层模型）

### 6.1 整体结构

```protobuf
// cs-user/api/proto/user.proto

message UserInfo {
  // ───── Layer 1: base ─────
  string user_id = 1;                  // 不可变 UUID，跨服务引用键
  string username = 2;                 // 可变，仅展示
  string display_name = 3;
  string email = 4;
  bool   email_verified = 5;
  string avatar_url = 6;
  string locale = 7;
  string timezone = 8;
  string status = 9;                   // active | disabled | suspended | deleted

  // ───── Layer 2: identities ─────
  repeated Identity identities = 20;
  Identity primary_identity = 21;      // 便于消费方直接取

  // ───── Layer 3: profile ─────
  Profile profile = 40;

  // ───── Layer 4: enterprise ─────
  EnterpriseIdentity enterprise = 60;  // 可选；无企业身份时为空

  // ───── 元数据 ─────
  google.protobuf.Timestamp created_at = 100;
  google.protobuf.Timestamp updated_at = 101;
  google.protobuf.Timestamp last_login_at = 102;
}

message Identity {
  string provider = 1;                 // github | ldap | phone | idtrust | aad | casdoor
  string issuer = 2;
  string external_user_id = 3;         // provider 内稳定 id
  string external_subject = 4;         // OAuth sub 原值
  string display_name = 5;
  string email = 6;
  string phone = 7;
  string avatar_url = 8;
  bool   is_primary = 9;
  google.protobuf.Timestamp last_login_at = 10;
  google.protobuf.Timestamp bound_at = 11;
}

message Profile {
  string business_line_id = 1;         // 引用 dept_tree
  string dept_id = 2;                  // 引用 dept_tree
  string system_role = 3;              // admin | member | approver | auditor
  google.protobuf.Struct preferences = 4;  // 主题 / 通知 / 语言
  google.protobuf.Struct quota = 5;    // max_repos / max_pat_count / ...
  repeated string tags = 6;
  string employee_id = 7;              // 工号（profile 层冗余，便于业务快速访问）
  google.protobuf.Timestamp hired_at = 8;
}

message EnterpriseIdentity {
  // 见 §9 详细 schema
  string employee_number = 1;          // 员工号
  string cost_center = 2;              // 成本中心
  string org_path = 3;                 // 组织路径（如 "总部/研发/AI/平台组"）
  string direct_manager_id = 4;        // 直线经理 user_id（如有）
  string direct_manager_display = 5;   // 直线经理展示名（cs-user 异步解析）
  string job_title = 6;
  string job_level = 7;
  string employment_type = 8;          // full_time | part_time | contractor | intern
  google.protobuf.Timestamp hire_date = 9;
  google.protobuf.Timestamp regular_date = 10;  // 转正日期
  string work_location = 11;
  google.protobuf.Struct attributes = 100;  // 扩展字段（按客户定制）
}
```

### 6.2 分层语义

| 层 | 描述 | 变更频率 | 来源 |
|---|---|---|---|
| **base** | 用户基础身份（"这个人是谁"） | 低（用户偶尔改资料） | 用户自助 / admin |
| **identities** | 多 provider 绑定关系 | 中（绑定/解绑） | 用户主动绑定 |
| **profile** | 业务属性 | 中（admin 改角色 / 部门调整） | admin / dept-sync webhook |
| **enterprise** | 企业身份 | 低（HR 数据相对稳定） | 登录时从企业 IdP 拉取 + webhook 同步 |

### 6.3 投影规则（不同场景返回不同层）

不同 API 端点 / 不同调用方需要的字段不同。cs-user 按场景返回不同投影（**禁止让消费方自己挑字段**，避免实现不一致）：

| 投影名 | 字段范围 | 适用场景 |
|---|---|---|
| `basic` | base.username + display_name + avatar | 列表展示（如 device.owner 展示） |
| `summary` | base + primary_identity.provider | 业务详情页 |
| `full` | 全部 4 层 | 用户自己查 `/api/users/me`、admin 后台 |
| `internal` | 全部 + 内部字段（last_login / tags / risk_score） | admin / 审计 |
| `public` | base.username + display_name + avatar（无 email） | 公开搜索结果 |

```http
GET /api/users/u_abc123?projection=basic
GET /api/users/u_abc123?projection=summary
GET /api/users/me?projection=full
```

批量查询同理：

```http
POST /api/users/batch
Content-Type: application/json
{ "user_ids": ["u_abc", "u_def"], "projection": "basic" }
```

---

## 7. base / identities / profile / enterprise 分层语义

### 7.1 base 层

不可缺失。所有 UserInfo 必含 base 层。

**字段约束**（沿用 [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md) §4.2）：

| 字段 | 可变性 | 唯一性 | 备注 |
|---|---|---|---|
| `user_id` | 不可变 | 全局唯一 | UUID v4；注销后不复用 |
| `username` | 可改（90 天冷却） | 大小写不敏感唯一 | `[a-z0-9_-]{3,64}` |
| `email` | 可改 | unique among active | 校验格式；可空 |
| `display_name` | 可改 | 无 | UTF-8 任意字符 |
| `status` | admin 改 | 无 | 4 态枚举 |

### 7.2 identities 层

至少 1 条。`MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md` 方案 B 已定义。

**关键字段**：

- `provider`：`github` / `ldap` / `phone` / `idtrust` / `aad` / `casdoor` / 自定义
- `external_key`：内部用（`provider + external_user_id` 归一化），不暴露给消费方
- `is_primary`：当前主身份（按 rank 自动选）
- `last_login_at`：上次用此 provider 登录时间

**禁止暴露**：`external_key`（用于内部反查）、`oauth_access_token` / `oauth_refresh_token`（绝不持久化）。

### 7.3 profile 层

业务属性。`business_line_id` / `dept_id` 仅作外键引用，真正 dept 信息由 dept-sync 服务提供。

**关键字段**：

| 字段 | 写权 | 备注 |
|---|---|---|
| `business_line_id` / `dept_id` | admin / dept-sync webhook | 改动触发 webhook 让 cs-user 刷新 |
| `system_role` | admin | `admin` / `member` / `approver` / `auditor`（不含业务权限） |
| `preferences` | 用户自助 | JSON；UI 主题 / 通知开关 / 语言 |
| `quota` | admin | JSON；`max_repos` / `max_pat_count` 等 |
| `employee_id` | admin / enterprise 同步 | 工号；profile 冗余便于业务直接访问 |

### 7.4 enterprise 层（可选）

详见 Part IV。

---

## 8. 序列化格式（REST + gRPC + JWT claims）

### 8.1 REST JSON

```json
{
  "user_id": "u_abc123def456",
  "username": "alice_wonderland",
  "display_name": "Alice Wonderland",
  "email": "alice@example.com",
  "email_verified": true,
  "avatar_url": "https://avatars.example.com/u/abc123",
  "locale": "zh-CN",
  "timezone": "Asia/Shanghai",
  "status": "active",

  "identities": [
    {
      "provider": "github",
      "issuer": "https://github.com",
      "external_user_id": "12345",
      "display_name": "alice-gh",
      "email": "alice@example.com",
      "is_primary": true,
      "last_login_at": "2026-07-09T10:00:00Z",
      "bound_at": "2026-05-01T08:00:00Z"
    },
    {
      "provider": "idtrust",
      "issuer": "https://idtrust.corp.example.com",
      "external_user_id": "emp_67890",
      "display_name": "Alice (Corp)",
      "is_primary": false,
      "bound_at": "2026-06-01T08:00:00Z"
    }
  ],
  "primary_identity": { "...": "github identity above" },

  "profile": {
    "business_line_id": "bl_ai_platform",
    "dept_id": "dept_engineering",
    "system_role": "member",
    "preferences": { "theme": "dark", "locale": "zh-CN" },
    "quota": { "max_repos": 50, "max_pat_count": 10 },
    "tags": ["pilot_user"],
    "employee_id": "EMP001"
  },

  "enterprise": {
    "employee_number": "EMP001",
    "cost_center": "CC-AI-001",
    "org_path": "总部/研发/AI/平台组",
    "direct_manager_id": "u_boss01",
    "direct_manager_display": "Bob Boss",
    "job_title": "Senior Engineer",
    "job_level": "P6",
    "employment_type": "full_time",
    "hire_date": "2022-03-01T00:00:00Z",
    "regular_date": "2022-09-01T00:00:00Z",
    "work_location": "Beijing-HQ",
    "attributes": {
      "project_code": "PRJ-CS-USER",
      "security_clearance": "L2"
    }
  },

  "created_at": "2026-05-01T08:00:00Z",
  "updated_at": "2026-07-09T10:00:00Z",
  "last_login_at": "2026-07-09T10:00:00Z"
}
```

### 8.2 JWT claims（历史简化版，**已弃用**）

> ⚠️ **本节为 multi-tenancy 引入前的旧设计，已弃用**。当前 canonical claims 结构以 [`MULTI_TENANCY_DESIGN.md` §12.1](./MULTI_TENANCY_DESIGN.md#121-claims-结构标准化业务身份) 的嵌套结构为准（`user` / `enterprise` 两个 Map + tenant 上下文 + 标准注册 claims）。
>
> 本节保留下来仅为说明历史字段名映射，**新代码不要按此结构签发或解析 JWT**。下方示例与 canonical 结构的差异：
>
> | 项 | 本节（旧） | §12.1（canonical） |
> |---|---|---|
> | 用户身份字段 | 顶层扁平 `preferred_username` / `name` / `email` / `picture` | 嵌套 `user.{username, display_name, email, avatar_url, ...}` |
> | 企业身份 | `has_enterprise: true` 布尔 | 完整 `enterprise` Map（uid / employee_id / ...） |
> | 角色字段 | `system_role: "member"` | `tenant_roles: ["tenant_admin"]` + `platform_admin: false` |
> | session id | `session_id` | `sid`（OIDC 标准） |
> | 头像字段名 | `picture` | `user.avatar_url` |
> | 缺失字段 | 无 `tenant_id` / `tenant_slug` / `tenant_edition` / `aud` / `nbf` / `jti` / `universal_id` / `provider` / `properties` | — |
>
> 下游兼容硬约束（保留 `universal_id` / `sub` / `preferred_username` / `name` / `displayName` / `email` / `phone` / `provider` / `properties` / `exp` / `iat` 等现有 claim 名称）见 [`IDENTITY_ARCHITECTURE_ROADMAP.md` §9.2](./IDENTITY_ARCHITECTURE_ROADMAP.md#92-兼容性硬约束不可违反)。

历史示例（**仅供字段名对照参考，不要照抄**）：

```json
{
  "iss": "https://cs-user.example.com",
  "sub": "u_abc123def456",
  "preferred_username": "alice_wonderland",
  "email": "alice@example.com",
  "name": "Alice Wonderland",
  "picture": "https://avatars...",
  "locale": "zh-CN",
  "groups": ["costrict:member", "dept:engineering", "bl:ai_platform"],
  "primary_provider": "github",
  "has_enterprise": true,
  "system_role": "member",
  "session_id": "sess_xxx",
  "auth_time": 1735686000,
  "exp": 1735689600,
  "iat": 1735686000
}
```

`has_enterprise` 布尔标识：消费方快速判断是否有企业身份，详情再查 `/api/users/:id`。canonical 结构中改为完整 `enterprise` Map，消费方可直接读字段无需二次查询。

### 8.3 gRPC

跨服务调用优先 gRPC（强类型 + 性能）。proto 定义见 §6.1。SDK 生成：`cs-user/sdk-go` / `cs-user/sdk-ts`。

---

# Part IV：企业身份可配置映射

## 9. 企业身份 schema 与字段清单

### 9.1 统一 enterprise schema

cs-user 对外暴露统一的 `EnterpriseIdentity` 结构（见 §6.1 proto），但不同企业 IdP 字段命名差异巨大：

| 统一字段 | LDAP | idtrust | Azure AD | 飞书 | 钉钉 |
|---|---|---|---|---|---|
| `employee_number` | `employeeNumber` | `emp_id` | `employeeId` | `employee_no` | `job_number` |
| `cost_center` | `departmentNumber` | `cost_center` | `department` | `department_id` | `dept_id` |
| `org_path` | `dn` (parsed) | `org_path` | `onPremisesDistinguishedName` | `department_path` | `dept_path` |
| `direct_manager_id` | `manager` (DN) | `manager_emp_id` | `manager` (UPN) | `leader_user_id` | `leader_id` |
| `job_title` | `title` | `position` | `jobTitle` | `job_title` | `title` |
| `job_level` | - | `job_level` | - | `grade` | `job_level` |
| `employment_type` | `employeeType` | `emp_type` | `employeeType` | `employee_type` | `employ_type` |
| `hire_date` | `hireDate` | `join_date` | `employeeHireDate` | `join_time` | `hire_date` |

**关键决策**：**不在代码中硬编码任何 IdP 字段名**。所有字段映射走 yaml 配置。

### 9.2 enterprise_identities 表（归一化后存储）

```sql
CREATE TABLE cs_user.enterprise_identities (
    user_id              UUID PRIMARY KEY REFERENCES cs_user.users(id) ON DELETE CASCADE,
    provider             VARCHAR(64) NOT NULL,        -- 最近一次同步来源 provider
    employee_number      VARCHAR(64),
    cost_center          VARCHAR(64),
    org_path             TEXT,
    direct_manager_id    UUID,                        -- 解析后指向 cs-user.users.id（如经理也是用户）
    direct_manager_external_ref VARCHAR(255),          -- 解析失败时存原始值（DN / UPN）
    job_title            VARCHAR(191),
    job_level            VARCHAR(32),
    employment_type      VARCHAR(32),
    hire_date            DATE,
    regular_date         DATE,
    work_location        VARCHAR(191),
    attributes           JSONB NOT NULL DEFAULT '{}', -- 客户自定义扩展字段

    sync_status          VARCHAR(32) NOT NULL DEFAULT 'fresh',  -- fresh | stale | error
    last_synced_at       TIMESTAMPTZ NOT NULL,
    next_sync_due_at     TIMESTAMPTZ NOT NULL,         -- 按 provider 配置的 TTL
    raw_payload_hash     VARCHAR(64),                  -- IdP 原始 payload hash（变更检测）

    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_enterprise_identities_provider
  ON cs_user.enterprise_identities (provider);
CREATE INDEX idx_enterprise_identities_cost_center
  ON cs_user.enterprise_identities (cost_center);
CREATE INDEX idx_enterprise_identities_manager
  ON cs_user.enterprise_identities (direct_manager_id);
```

### 9.3 字段分类

| 类别 | 字段 | 说明 |
|---|---|---|
| **核心字段**（schema 内置） | employee_number / cost_center / org_path / direct_manager_id / job_title / job_level / employment_type / hire_date / regular_date / work_location | 90% 客户场景覆盖；字段集稳定，避免无限扩张 |
| **扩展字段**（attributes JSONB） | 客户私有（如 security_clearance / project_code） | 由 mapping yaml 指定字段名 + 类型；不做 SQL 索引（除非业务必须） |

**核心字段保持 10 个上限**——超过的字段进 `attributes` JSONB，避免 schema 膨胀。

---

## 10. per-provider mapping 配置规范

### 10.1 配置文件位置

```
cs-user/
├── config/
│   ├── provider-mapping.yaml          # 主配置（部署时挂载）
│   └── provider-mapping-overlay.yaml  # 客户私有覆盖（可选）
```

热加载：cs-user 监听配置文件变化（fsnotify），变更后无需重启；或通过 admin API `POST /api/admin/provider-mapping/reload` 触发。

### 10.2 配置 schema

```yaml
# cs-user/config/provider-mapping.yaml
version: "1.0"

# 全局默认值（被 provider 覆盖）
defaults:
  enterprise_sync_interval: "24h"        # 企业身份定期同步周期
  enterprise_sync_retry_max: 3
  enterprise_sync_retry_backoff: "5m"

# per-provider 映射
providers:
  # ─── LDAP ───
  ldap:
    enabled: true
    rank: 150                            # provider 优先级（沿用 MULTI_PROVIDER §5）
    enterprise_sync:
      enabled: true
      interval: "24h"
      on_login: refresh_if_stale         # always | refresh_if_stale | never
      stale_threshold: "12h"
    field_map:
      # 标准字段映射
      employee_number: "employeeNumber"
      cost_center: "departmentNumber"
      org_path:
        source: "dn"                     # 从 LDAP dn 字段提取
        transform: "parse_dn_to_path"    # 内置 transformer：把 DN 转 org path
      direct_manager_id:
        source: "manager"                # LDAP manager 存 DN
        transform: "lookup_by_dn"        # 查 LDAP 反查 user_id
        fallback: "store_raw_ref"        # 解析失败存 direct_manager_external_ref
      job_title: "title"
      employment_type: "employeeType"
      hire_date:
        source: "hireDate"
        transform: "parse_date"
        format: "2006-01-02"
      work_location: "physicalDeliveryOfficeName"

      # 扩展字段（attributes JSONB）
      attributes:
        security_clearance: "extensionAttribute1"
        project_codes: "extensionAttribute2"

  # ─── idtrust ───
  idtrust:
    enabled: true
    rank: 300                            # 比 ldap 优先级高
    enterprise_sync:
      enabled: true
      interval: "12h"
      on_login: always                   # 每次登录都拉新
    field_map:
      employee_number: "emp_id"
      cost_center: "cost_center"
      org_path: "org_path"               # idtrust 直接给 path
      direct_manager_id:
        source: "manager_emp_id"
        transform: "lookup_by_employee_number"
      job_title: "position"
      job_level: "job_level"
      employment_type: "emp_type"
      hire_date:
        source: "join_date"
        transform: "parse_date"
        format: "2006-01-02T15:04:05Z"
      regular_date: "regular_date"
      work_location: "work_location"
      attributes:
        project_code: "project_code"

  # ─── Azure AD ───
  aad:
    enabled: true
    rank: 200
    enterprise_sync:
      enabled: true
      interval: "24h"
      on_login: refresh_if_stale
    field_map:
      employee_number: "employeeId"
      cost_center: "department"
      org_path:
        source: "onPremisesDistinguishedName"
        transform: "parse_dn_to_path"
      direct_manager_id:
        source: "manager"
        transform: "lookup_by_upn"
      job_title: "jobTitle"
      employment_type: "employeeType"
      hire_date:
        source: "employeeHireDate"
        transform: "parse_date"
        format: "2006-01-02T15:04:05Z"
      work_location: "officeLocation"
      attributes:
        security_clearance: "extension_sec_clearance"

  # ─── GitHub（无企业身份）───
  github:
    enabled: true
    rank: 200
    enterprise_sync:
      enabled: false                     # GitHub 不提供企业身份
    field_map: {}                        # 空 map

  # ─── 短信 / 手机号（无企业身份）───
  phone:
    enabled: true
    rank: 100
    enterprise_sync:
      enabled: false
    field_map: {}
```

### 10.3 配置字段语义

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `enabled` | bool | 是 | provider 是否启用 |
| `rank` | int | 是 | provider 优先级（决定 primary identity） |
| `enterprise_sync.enabled` | bool | 是 | 是否从该 provider 拉企业身份 |
| `enterprise_sync.interval` | duration | 是 | 定期同步周期（如 24h） |
| `enterprise_sync.on_login` | enum | 否 | `always` / `refresh_if_stale` / `never`（默认 `refresh_if_stale`） |
| `enterprise_sync.stale_threshold` | duration | 否 | 触发刷新的过期阈值（默认 = interval / 2） |
| `field_map.<standard_field>` | string \| object | 是 | 字段映射；string 表示直接取同名键；object 支持 source + transform + format |
| `field_map.attributes.<custom_field>` | string | 否 | 扩展字段映射（写入 attributes JSONB） |

### 10.4 transformer 内置清单

| transformer | 输入 | 输出 | 用途 |
|---|---|---|---|
| `parse_dn_to_path` | `"OU=平台组,OU=AI,OU=研发,DC=corp"` | `"总部/研发/AI/平台组"` | LDAP DN 转 path |
| `parse_date` | string + format | RFC3339 timestamp | 日期格式归一化 |
| `lookup_by_dn` | LDAP DN | user_id | DN → user_id 反查 |
| `lookup_by_upn` | `alice@corp.com` | user_id | UPN → user_id 反查 |
| `lookup_by_employee_number` | `"EMP001"` | user_id | 工号 → user_id 反查 |
| `parse_int` | string | int | 字符串数字 |
| `parse_json` | JSON string | object | JSON 字符串解析 |
| `lowercase` / `uppercase` / `trim` | string | string | 字符串规范化 |
| `regex_extract` | string + pattern | string | 正则提取 |
| `map_values` | string + mapping dict | string | 枚举值映射（如 `["FT","PT"]` → `["full_time","part_time"]`） |

transformer 实现位置：`cs-user/internal/enterprise/transformers/`；每个 transformer 一个文件 + 单元测试。新增 transformer 需代码改动（但比改业务逻辑成本低得多）。

---

## 11. 映射求值流程与示例

### 11.1 登录时拉取企业身份

```
用户用 idtrust 登录
   └─► Casdoor OAuth callback → cs-user
       └─► 拉 Casdoor userinfo（含 idtrust provider 原始 claims）
           └─► NormalizeJWTClaims：
               - 识别 provider = idtrust
               - 提取 external_key / display_name / email
               - INSERT/UPDATE user_auth_identities
           └─► ApplyEnterpriseMapping(provider="idtrust", raw_claims)：
               1. 加载 provider-mapping.yaml[idtrust].field_map
               2. 对每个 standard_field：
                  - 简单映射（string）：raw_claims[field_name]
                  - 复合映射（object）：raw_claims[source] → transform 求值
               3. 求值结果填入 EnterpriseIdentity struct
               4. INSERT ON CONFLICT UPDATE enterprise_identities
               5. raw_payload_hash 校验：若 IdP 数据未变，跳过更新（避免无效写）
               6. 更新 next_sync_due_at = now() + interval
           └─► 触发 user.enterprise_updated webhook（如关键字段变更）
```

### 11.2 求值示例（idtrust）

**输入**（Casdoor 返回的 idtrust raw claims）：

```json
{
  "sub": "idtrust:emp_67890",
  "emp_id": "EMP001",
  "cost_center": "CC-AI-001",
  "org_path": "总部/研发/AI/平台组",
  "manager_emp_id": "EMP000",
  "position": "Senior Engineer",
  "job_level": "P6",
  "emp_type": "FT",
  "join_date": "2022-03-01T00:00:00Z",
  "work_location": "Beijing-HQ",
  "project_code": "PRJ-CS-USER"
}
```

**配置**（idtrust.field_map，见 §10.2）：

```yaml
field_map:
  employee_number: "emp_id"
  cost_center: "cost_center"
  org_path: "org_path"
  direct_manager_id:
    source: "manager_emp_id"
    transform: "lookup_by_employee_number"
  job_title: "position"
  job_level: "job_level"
  employment_type:
    source: "emp_type"
    transform: "map_values"
    mapping: { "FT": "full_time", "PT": "part_time", "CT": "contractor" }
  hire_date:
    source: "join_date"
    transform: "parse_date"
  work_location: "work_location"
  attributes:
    project_code: "project_code"
```

**求值过程**：

| standard_field | source | raw value | transform | output |
|---|---|---|---|---|
| employee_number | `emp_id` | `"EMP001"` | — | `"EMP001"` |
| cost_center | `cost_center` | `"CC-AI-001"` | — | `"CC-AI-001"` |
| org_path | `org_path` | `"总部/研发/AI/平台组"` | — | `"总部/研发/AI/平台组"` |
| direct_manager_id | `manager_emp_id` | `"EMP000"` | `lookup_by_employee_number` | `"u_boss01"`（DB 反查） |
| job_title | `position` | `"Senior Engineer"` | — | `"Senior Engineer"` |
| job_level | `job_level` | `"P6"` | — | `"P6"` |
| employment_type | `emp_type` | `"FT"` | `map_values` | `"full_time"` |
| hire_date | `join_date` | `"2022-03-01T00:00:00Z"` | `parse_date` | `"2022-03-01T00:00:00Z"` |
| work_location | `work_location` | `"Beijing-HQ"` | — | `"Beijing-HQ"` |
| attributes.project_code | `project_code` | `"PRJ-CS-USER"` | — | `{"project_code": "PRJ-CS-USER"}` |

**输出**（EnterpriseIdentity）：

```json
{
  "employee_number": "EMP001",
  "cost_center": "CC-AI-001",
  "org_path": "总部/研发/AI/平台组",
  "direct_manager_id": "u_boss01",
  "direct_manager_display": "Bob Boss",
  "job_title": "Senior Engineer",
  "job_level": "P6",
  "employment_type": "full_time",
  "hire_date": "2022-03-01T00:00:00Z",
  "work_location": "Beijing-HQ",
  "attributes": { "project_code": "PRJ-CS-USER" },
  "provider": "idtrust",
  "last_synced_at": "2026-07-10T10:00:00Z",
  "next_sync_due_at": "2026-07-10T22:00:00Z"
}
```

### 11.3 定期同步（offline refresh）

cs-user 启动后台 worker `EnterpriseSyncWorker`：

```
每 5min 扫一次：
  SELECT user_id FROM cs_user.enterprise_identities
  WHERE next_sync_due_at < now() AND sync_status != 'error'
  LIMIT 100;

对每个 user_id：
  - 加载该用户 primary enterprise provider（enterprise_identities.provider）
  - 调 provider 对应 IdP API（如 LDAP search by DN / idtrust /users/:emp_id）
  - 重新求值 field_map
  - ON CONFLICT UPDATE
  - 推进 next_sync_due_at
```

provider API 适配器在 `cs-user/internal/enterprise/providers/`，每个 provider 一个文件。

---

## 12. 多源企业身份合并策略

### 12.1 问题

一个用户可能绑多个含企业身份的 provider（如同时绑 idtrust + AAD），不同 provider 给的企业字段可能冲突。

### 12.2 策略：**单一 source of truth，不合并**

```
一个用户的 enterprise_identities 行只有 1 条
source_provider = highest_rank_provider_with_enterprise_data
```

**优先级规则**（沿用 [`MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md`](../proposals/MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md) §5 rank + [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md) §7.2 扩展）：

```
企业 IdP（按 rank 排）：idtrust(300) > aad(200) > ldap(150) > casdoor(50)
非企业 IdP（不参与 enterprise 合并）：github(200) / phone(100)
```

> 注：rank 同时驱动两件事——(a) 主身份 primary identity 选择（所有 provider 参与，含 github/phone）；(b) 企业身份 source 选择（仅企业 IdP 参与）。两者规则独立，不互相影响。github / phone 即便 rank=200/100 高于 casdoor(50)，对企业身份 source 选择无影响（无企业数据可贡献）。

**当用户绑了多个企业 provider**：

- 选 rank 最高的作为 source of truth，写入 enterprise_identities
- 其他 provider 的企业数据**丢弃**（不合并、不存历史）
- 用户解绑当前 source provider → 触发重选（剩余中 rank 最高的）

### 12.3 切换示例

```
Alice 绑了 idtrust（rank 300）+ AAD（rank 200）+ GitHub（rank 200 无企业）

初始：source = idtrust；enterprise_identities 一行用 idtrust 数据
   └─► 用户解绑 idtrust
       └─► 重选：剩余有企业数据的 = AAD
           └─► 同步触发：source = AAD；enterprise_identities 整行被 AAD 数据覆盖
           └─► webhook user.enterprise_source_changed
```

### 12.4 attributes 字段冲突

**不冲突**——attributes 来自当前 source provider 的 field_map.attributes；切换 source 时整体覆盖，不部分保留。

---

# Part V：cs-user API 契约

## 13. 公开 / 受保护 / 内部 / 管理面 API

### 13.1 公开端点（无需 auth）

```http
POST   /api/auth/login              OAuth code → JWT
POST   /api/auth/refresh            刷新 access token
POST   /api/auth/logout             撤销 session
GET    /.well-known/jwks.json       JWKS 公钥
GET    /api/health                  健康检查
```

### 13.2 用户自助端点（require auth）

```http
GET    /api/users/me                        当前用户 full projection
PATCH  /api/users/me                        改 base / profile.preferences
POST   /api/users/me/username               改 username（90 天冷却）
POST   /api/users/me/delete                 申请注销

GET    /api/users/me/identities             列出绑定 identity
POST   /api/users/me/identities             绑定新 provider
DELETE /api/users/me/identities/:id         解绑

GET    /api/users/me/enterprise             当前企业身份
POST   /api/users/me/enterprise/refresh     手动触发刷新（rate-limited）

GET    /api/users/me/sessions               活跃 session
DELETE /api/users/me/sessions/:id           撤销 session

GET    /api/users/me/pats                   PAT 列表
POST   /api/users/me/pats                   生成 PAT
DELETE /api/users/me/pats/:id               撤销 PAT
```

### 13.3 跨服务查询端点（require service token）

```http
GET    /api/users/:id?projection=basic|summary|full   单用户查询
POST   /api/users/batch                                批量查询
       body: { user_ids: [...], projection: "basic" }
GET    /api/users/by-username/:username                username → user_id 反查
GET    /api/users/by-email/:email                       email → user_id 反查
POST   /api/users/resolve-by-identity                   external_key → user_id
       body: { provider, external_user_id }

GET    /api/enterprise/:user_id                        企业身份查询
POST   /api/enterprise/batch                           批量
```

**鉴权**：service-to-service 赃 mTLS + 服务 token（admin scope）；不走用户 JWT。

### 13.4 admin 端点（require admin role）

```http
GET    /api/admin/users                       列表 + 筛选
GET    /api/admin/users/:id                   full projection
PATCH  /api/admin/users/:id                   改任意字段
POST   /api/admin/users/:id/disable           禁用
POST   /api/admin/users/:id/suspend           静默
POST   /api/admin/users/:id/enable            恢复
POST   /api/admin/users/:id/delete            强制注销

PATCH  /api/admin/users/:id/profile           改 profile
PATCH  /api/admin/users/:id/system-role       改系统角色
GET    /api/admin/users/:id/identities        列出 identity
DELETE /api/admin/users/:id/identities/:identityId   强制解绑

POST   /api/admin/provider-mapping/reload     热加载配置
GET    /api/admin/provider-mapping            查看当前配置

GET    /api/admin/audit-logs                  审计日志
GET    /api/admin/webhook-deliveries          投递状态
POST   /api/admin/webhook-deliveries/:id/replay   重投死信
```

### 13.5 内部端点（仅 fork Gitea / 内部 worker）

```http
POST   /api/internal/users/:id/gitea-binding   fork Gitea auto-provision 回调
POST   /api/internal/webhooks/replay           死信手工重投
```

---

## 14. 跨服务调用模式（sync / async / cache）

### 14.1 同步调用（gRPC）

适用：业务请求路径中需要 user 信息（如 device 详情展示 owner）。

```go
// costrict-web 业务代码示例
user, err := userClient.Get(ctx, &userpb.GetRequest{
    UserId: device.UserID,
    Projection: userpb.Projection_BASIC,
})
device.OwnerUsername = user.Username
```

**性能要求**：P99 < 20ms（含 Redis cache）；CS-user 水平扩缩容。

### 14.2 异步事件（webhook）

适用：用户属性变更通知（username / status / enterprise 改变）。

cs-user 发布 webhook（沿用 [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md) §10 通用广播系统）：

```json
{
  "event_id": "evt_xxx",
  "event_type": "user.enterprise_updated",
  "subject": { "user_id": "u_abc123" },
  "data": {
    "changed_fields": ["job_title", "job_level"],
    "source_provider": "idtrust",
    "previous_values": { "job_title": "Engineer", "job_level": "P5" },
    "current_values": { "job_title": "Senior Engineer", "job_level": "P6" }
  }
}
```

### 14.3 缓存策略

**SDK 内置两级 cache**：

| 层 | TTL | 失效方式 |
|---|---|---|
| 进程内 LRU（SDK 自带） | 60s | 自然过期 + webhook 主动失效 |
| Redis（共享） | 5min | 自然过期 + webhook 主动失效 |

**webhook 主动失效**：

cs-user 发布 `user.updated` → SDK 订阅 → 失效 cache 中 `user_id` 对应条目。订阅方 SDK 实现示例：

```go
// costrict-web 启动时注册 webhook 订阅
userClient.SubscribeWebhook("user.updated", func(event *WebhookEvent) {
    userClient.InvalidateCache(event.Subject.UserID)
})
```

### 14.4 批量查询优化

业务侧 list 场景（如展示 100 个 device 的 owner）：

```go
devices := deviceRepo.List()  // 100 rows
userIDs := extractUniqueUserIDs(devices)
users, _ := userClient.BatchGet(ctx, &userpb.BatchGetRequest{
    UserIds: userIDs,
    Projection: userpb.Projection_BASIC,
})
// 1 次 gRPC + 1 次 Redis mget，O(1) 网络往返
```

**禁止**：循环单查（100 device = 100 gRPC 调用）。

---

## 15. webhook 事件契约

### 15.1 事件类型清单（cs-user 发布）

继承 [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md) §10.1 + 新增企业身份事件：

| event_type | 触发 | 关键字段 |
|---|---|---|
| `user.created` | 首次登录创建 | user_id / username / primary_provider |
| `user.updated` | username / email / display_name 变更 | changed_fields |
| `user.profile_changed` | profile（business_line / dept / role）变更 | changed_fields |
| `user.disabled` | admin 禁用 / 静默 | muted_until |
| `user.enabled` | 恢复 | — |
| `user.deleted` (soft / hard) | 注销 | deletion_type |
| `user.identity_bound` | 绑新 provider | provider |
| `user.identity_unbound` | 解绑 | provider |
| **`user.enterprise_updated`** | 企业身份字段变更 | changed_fields / source_provider |
| **`user.enterprise_source_changed`** | source provider 切换 | old_source / new_source |

### 15.2 投递保证

沿用 [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md) §10.2：6 次指数退避 + 死信 + 日级全量校对。

---

# Part VI：实施与风险

## 16. 数据迁移与服务切换路径

### 16.1 迁移阶段

> **前置条件**：[`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md) Stage 0（M0-M7，用户中心 + JWT 自签 + fork Gitea 中间件 + Casdoor 退化）必须先完成。cs-user 拆分（本节 M0-M8）属于 Stage 1，在 Stage 0 稳定运行 ≥ 2 周后启动；Stage 0 未完成时启动 Stage 1 会导致用户中心主权与 JWT 签发权在迁移期短暂无主。

| 阶段 | 周期 | 关键产出 |
|---|---|---|
| **M0：cs-user 骨架** | 2 周 | cs-user 仓库初始化 / proto 定义 / SDK 生成 / 独立 schema 建立 |
| **M1：模型 + 数据迁移** | 1 周 | 抽出 User / UserAuthIdentity / UserProfile / UserSystemRole / UserGiteaBinding 模型；迁数据到 cs_user schema |
| **M2：API 切换** | 2 周 | cs-user 暴露标准 API；costrict-web 通过 SDK 调用；旧 in-process 调用逐步替换 |
| **M3：JWT 自签迁出** | 1 周 | RS256 私钥 + JWKS endpoint 迁 cs-user；下游服务 dual-trust 兼容期 |
| **M4：企业身份映射上线** | 2 周 | provider-mapping.yaml + EnterpriseSyncWorker + transformer 实现 |
| **M5：webhook 接通** | 1 周 | cs-user 接管 user.* 事件发布；下游订阅切换 |
| **M6：costrict-web 瘦身** | 1 周 | 删除 costrict-web 中 user / authidentity / casdoor / systemrole 代码 + 测试 |
| **M7：兼容期下线** | M3+30 天 | 关闭旧 JWKS endpoint；下线 costrict-web `/api/auth/*` 端点 |
| **M8：稳定运行** | M7+2 周 | 无 P0 故障；cs-user 独立运行稳定 |

### 16.2 数据迁移策略

```sql
-- M1 阶段：迁数据到新 schema（共享 DB 模式）

-- 1. 创建 cs_user schema
CREATE SCHEMA cs_user;

-- 2. 在 cs_user 下重建表（与原表结构一致）
CREATE TABLE cs_user.users (LIKE public.users INCLUDING ALL);
CREATE TABLE cs_user.user_auth_identities (LIKE public.user_auth_identities INCLUDING ALL);
CREATE TABLE cs_user.user_system_roles (LIKE public.user_system_roles INCLUDING ALL);
-- ...

-- 3. 数据迁移（一次性 dump）
INSERT INTO cs_user.users SELECT * FROM public.users;
INSERT INTO cs_user.user_auth_identities SELECT * FROM public.user_auth_identities;
-- ...

-- 4. 业务表 FK 暂时去掉 SQL 约束（迁移期保留外键值，由应用层校验）
ALTER TABLE public.devices DROP CONSTRAINT devices_user_id_fkey;
-- ...

-- 5. 迁移完成后删除 public schema 下旧表
DROP TABLE public.users;
-- ...
```

### 16.3 SDK 替换策略

```go
// M2 阶段：costrict-web 旧代码（直接查 DB）
user, err := db.User.Get(userID)

// M2 阶段：替换为 SDK 调用（带 cache）
user, err := userClient.Get(ctx, &userpb.GetRequest{
    UserId: userID,
    Projection: userpb.Projection_BASIC,
})
```

替换按调用点逐步进行（按业务模块），每个 PR 替换一个模块，避免一次性大改。

### 16.4 灰度策略

| 维度 | 方式 |
|---|---|
| 服务范围 | cs-user 先 dogfood → costrict-web 接入 → cs-cloud → csc → app-ai-native |
| 兼容期 | 30 天 dual-trust（旧 costrict-web JWKS 与新 cs-user JWKS 并存） |
| 回滚开关 | env `USER_CENTER_BACKEND=costrict-web|cs-user`（SDK 识别后切调用目标） |

---

## 17. 风险与对策

| 风险 | 严重度 | 对策 |
|---|---|---|
| **cs-user 单点故障 → 全生态登录瘫痪** | 高 | cs-user 多副本部署；JWKS CDN 缓存兜底；Redis cache 兜底已查 user 信息 |
| **跨服务调用延迟增加**（原 in-process → 现 gRPC） | 中 | SDK 二级 cache + 批量查询接口；P99 监控 < 20ms |
| **provider-mapping 配置错误 → 企业身份丢失 / 错乱** | 高 | 配置 schema 验证（启动时 yaml lint）；dry-run 模式（用真实 raw claims 跑映射 + 输出 diff）；admin UI 配置编辑 |
| **transformer 实现不全 → 部分字段无法解析** | 中 | 内置 10 个 transformer 覆盖 90% 场景；新增 transformer 走代码 PR + 单元测试 |
| **数据迁移期 FK 约束去除 → 数据不一致** | 高 | 迁移期加应用层校验（写时调 cs-user 验证 user_id 存在）；定时对账（每小时扫一致性） |
| **SDK 版本漂移**（costrict-web 用旧 SDK，cs-user 升级） | 中 | proto 强类型 + gRPC 兼容性规则（只加字段不删字段）；SDK 自动化版本管理 |
| **企业身份 source 切换数据丢失**（如 idtrust → AAD 切换） | 中 | 切换前 webhook 预通知；旧 source 数据归档到 `enterprise_identities_history` 表（可选） |
| **多 IdP 同步 race condition**（同一 user 多 provider 并发同步） | 低 | enterprise_identities 行级锁（SELECT FOR UPDATE）；按 user_id 串行化 |
| **webhook 风暴**（用户大量改资料 → 下游被压垮） | 中 | webhook 投递限流 + 批量事件（如 `user.profile_changed.batch`）；下游幂等 |
| **transformer 中 lookup_by_employee_number 反查慢** | 中 | employee_number 加索引；反查结果 cache（5min TTL） |
| **Casdoor raw claims schema 变更**（IdP 升级） | 中 | raw_payload_hash 检测变更告警；mapping yaml 热加载；管理员告警通道 |
| **cs-user 与 dept-sync 一致性漂移**（business_line_id 失效） | 中 | cs-user 启动时拉 dept 全量；webhook 订阅 dept-sync 变更；定时对账 |

---

## 18. 已决策项与开放问题

### 18.1 已决策项（继承既有 + 本提案新增）

| # | 决策点 | 决议 | 来源 |
|---|---|---|---|
| 1 | 用户中心主权方 | costrict-web（迁 cs-user 后为 cs-user） | `IDENTITY_FEDERATION_DECISION.md` v2 + 本提案 |
| 2 | JWT 自签 + fork Gitea 中间件 | RS256 + JWKS + fork 中间件 | V4 §6 + `USER_CENTER_DESIGN.md` §5 |
| 3 | 多 provider 绑定 | 显式绑定 + provider rank 自动升级 | `MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md` |
| 4 | 跨服务引用键 | 不可变 user_id（UUID） | `USER_CENTER_DESIGN.md` §4.2 |
| 5 | webhook 通用广播系统 | 6 次指数退避 + 死信 + 日级全量校对 | V4 §6.5 + `USER_CENTER_DESIGN.md` §10 |
| 6 | cs-user 独立服务 | 独立 Go 服务 + 独立 schema | **本提案 Part II** |
| 7 | UserInfo 标准契约 | 4 层模型（base / identities / profile / enterprise） | **本提案 Part III** |
| 8 | 企业身份字段映射 | per-provider yaml 配置驱动 + 内置 transformer | **本提案 Part IV** |
| 9 | 多源企业身份合并策略 | 单一 source of truth（rank 最高），不合并 | **本提案 §12** |
| 10 | 部署形态 | 迁移期共享 DB + 独立 schema；未来按需切独立 DB | **本提案 §5.4** |
| 11 | SDK 模式 | gRPC proto + Go / TS SDK + 二级 cache | **本提案 §14** |
| 12 | 企业身份 schema 字段集 | 10 个核心字段 + attributes JSONB 扩展 | **本提案 §9** |

### 18.2 开放问题（待评审确认）

| # | 问题 | 推荐 | 备注 |
|---|---|---|---|
| 1 | cs-user 是否独立 git 仓库？ | 是（`github.com/costrict/cs-user`） | 与 costrict-web 解耦，独立 CI / 发布周期 |
| 2 | proto / SDK 是否独立仓库？ | 否（放在 cs-user 仓库的 `api/` 与 `sdk-go/` 子目录） | 单仓库降低版本同步成本 |
| 3 | webhook 广播基础设施归属？ | 独立 `cs-webhook` 服务（长期）/ cs-user 内嵌（短期） | 短期先内嵌，长期可拆出 |
| 4 | 共享 DB vs 独立 DB？ | 共享 DB + 独立 schema（迁移期）；独立 DB（规模化） | 看私有化 vs 公有云形态 |
| 5 | 跨服务调用协议：gRPC vs REST？ | 内部 gRPC；对外 REST | gRPC 性能 + REST 易用 |
| 6 | 企业身份历史归档？ | 默认不归档（覆盖式更新）；可加 `enterprise_identities_history` 表 | 视合规需求 |
| 7 | transformer 是否允许自定义（用户写 Go plugin）？ | 第一阶段不允许；客户走 attributes JSONB 逃生 | 后续评估 plugin 机制 |
| 8 | 多租户是否预留？ | 不预留（单 tenant） | 多租户需求未明确 |
| 9 | 用户密码是否迁 cs-user？ | 否（继续透传 Casdoor） | 密码管理非 cs-user 主目标 |
| 10 | provider-mapping.yaml 是否暴露给客户编辑？ | 是（部署时挂载 + admin reload） | 私有化场景必备 |

---

# 附录

## 附录 A：UserInfo 完整 JSON Schema

```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "title": "UserInfo",
  "type": "object",
  "required": ["user_id", "username", "status"],
  "properties": {
    "user_id": { "type": "string", "pattern": "^u_[a-f0-9]{12}$" },
    "username": { "type": "string", "pattern": "^[a-z0-9_-]{3,64}$" },
    "display_name": { "type": "string", "maxLength": 191 },
    "email": { "type": ["string", "null"], "format": "email" },
    "email_verified": { "type": "boolean" },
    "avatar_url": { "type": ["string", "null"] },
    "locale": { "type": "string", "pattern": "^[a-z]{2}(-[A-Z]{2})?$" },
    "timezone": { "type": "string" },
    "status": { "enum": ["active", "disabled", "suspended", "deleted"] },

    "identities": {
      "type": "array",
      "minItems": 1,
      "items": { "$ref": "#/definitions/Identity" }
    },
    "primary_identity": { "$ref": "#/definitions/Identity" },

    "profile": { "$ref": "#/definitions/Profile" },
    "enterprise": { "oneOf": [{ "$ref": "#/definitions/EnterpriseIdentity" }, { "type": "null" }] },

    "created_at": { "type": "string", "format": "date-time" },
    "updated_at": { "type": "string", "format": "date-time" },
    "last_login_at": { "type": ["string", "null"], "format": "date-time" }
  },

  "definitions": {
    "Identity": {
      "type": "object",
      "required": ["provider", "external_user_id", "is_primary"]
    },
    "Profile": {
      "type": "object",
      "properties": {
        "business_line_id": { "type": "string" },
        "dept_id": { "type": "string" },
        "system_role": { "enum": ["admin", "member", "approver", "auditor"] },
        "preferences": { "type": "object" },
        "quota": { "type": "object" },
        "tags": { "type": "array", "items": { "type": "string" } },
        "employee_id": { "type": "string" }
      }
    },
    "EnterpriseIdentity": {
      "type": "object",
      "properties": {
        "employee_number": { "type": "string" },
        "cost_center": { "type": "string" },
        "org_path": { "type": "string" },
        "direct_manager_id": { "type": "string" },
        "direct_manager_display": { "type": "string" },
        "job_title": { "type": "string" },
        "job_level": { "type": "string" },
        "employment_type": { "enum": ["full_time", "part_time", "contractor", "intern"] },
        "hire_date": { "type": "string", "format": "date-time" },
        "regular_date": { "type": "string", "format": "date-time" },
        "work_location": { "type": "string" },
        "attributes": { "type": "object" }
      }
    }
  }
}
```

## 附录 B：per-provider mapping yaml 完整示例

见 §10.2 配置规范。完整示例随 cs-user 仓库 `config/provider-mapping.example.yaml` 发布。

## 附录 C：现有 costrict-web 调用点改造清单

| 调用点 | 现位置 | 迁移动作 |
|---|---|---|
| `models.User` / `UserAuthIdentity` | `server/internal/models/models.go` | 迁 `cs-user/internal/models/` |
| `casdoor.CasdoorClient` | `server/internal/casdoor/client.go` | 迁 `cs-user/internal/providers/casdoor/` |
| `authidentity.NormalizeJWTClaims` | `server/internal/authidentity/normalize.go` | 迁 `cs-user/internal/identity/normalize.go` |
| `UserService.GetOrCreateUser` | `server/internal/user/service.go` | 迁 `cs-user/internal/user/service.go` |
| `systemrole.RequireSystemRole` | `server/internal/systemrole/middleware.go` | 抽 `cs-user/sdk-go/authz` |
| `middleware.RequireAuth` | `server/internal/middleware/auth.go` | 抽 `cs-user/sdk-go/auth` |
| `handlers.AuthCallback` | `server/internal/handlers/auth.go` | 迁 `cs-user/internal/handlers/auth.go` |
| `handlers.GetUserNames` | `server/internal/handlers/users.go` | 改为调 cs-user SDK |
| `device.owner_username_snapshot` 等冗余字段 | 业务表 | 加 webhook 订阅失效 |
| `CASDOOR_*` env vars | `server/internal/config/config.go` | 迁 cs-user config |
| `users.user_id` 外键（capability_items / devices / etc.） | public schema | 去掉 SQL FK 约束；应用层校验 |

---

> 本提案与 [`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md) 严格互补：前者定义"主权与架构"，本提案定义"服务化拆分 + 标准契约 + 配置化映射"。评审通过后，可作为 Stage 1（cs-user 服务化）的实施基线，与 Stage 0（用户中心 + fork Gitea JWT 中间件）顺序衔接。

---

# Part VII：git-server team adapter（设计参考——已迁移到 @server 实施）

> **2026-07-15 v3 反转**：原计划在 cs-user 实施 team-level Gitea 同步（`team_user` 表写入）。经评审，team 同步职责**反转到 @server**：cs-user 收缩为仅 user-level（自动开户 + `user_gitea_binding` 维护），team-level `team_user` 同步归集到 @server（实施位置 `server/internal/gitsync/`）。
>
> **本部分保留下文作为设计参考**——@server 实施 team 同步时可直接采用本设计的 GitServerAdapter 接口 / Gitea 实现 / HR team → Gitea team 映射策略 / 失败重试机制。但代码归属、模块路径、运维责任均在 @server。决策依据详见 [`TEAM_ORG_UNIFICATION.md`](./TEAM_ORG_UNIFICATION.md) ADR-3 v3。

## 19. 设计目标与边界

### 19.1 设计目标

| 目标 | 说明 |
|---|---|
| **fork 权限自动化** | 用户在 org-team-service 加入 / 离开团队时，自动同步到 Gitea team_user 表，无需 admin 手动干预 |
| **真相源单一** | @server 是 Gitea team 成员的唯一写入方（Gitea 内部手动变更会被下次同步覆盖） |
| **实时性** | org-team-service webhook 触发后，秒级反映到 Gitea |
| **可扩展** | 抽象 GitServerAdapter interface，首期实现 Gitea，未来可扩展 GitLab / GitHub Enterprise |

### 19.2 边界（非目标）

| 非目标 | 理由 |
|---|---|
| **读取 git-server 的 team 变更** | 单向同步——admin 在 Gitea UI 的手动操作会被下次同步覆盖（明确告知 admin 不要直接操作 Gitea team） |
| **批量定时同步** | 实时事件驱动已足够；批量仅作为失败补偿（详见 §21.4） |
| **多 git-server 同时支持** | 首期仅 Gitea；interface 已预留扩展点 |
| **fork 权限中间件改造** | fork 中间件继续内部查 `team_user`（不变）；@server 负责保证 team_user 数据正确 |

### 19.3 数据流向

```
org-team-service                      @server                         Gitea
─────────────────                    ──────────                       ──────
member.added webhook ──────────→  GitServerAdapter
                                  ├─ 通过 cs-user SDK 查 user_gitea_binding 找 Gitea username
                                  ├─ 计算 target team_id（HR team → Gitea team 映射）
                                  └─ GiteaAdapter.AddTeamMember()
                                                              ─────→  PUT /api/v1/teams/{id}/members/{username}

member.removed webhook ────────→  GitServerAdapter
                                  └─ GiteaAdapter.RemoveTeamMember()
                                                              ─────→  DELETE /api/v1/teams/{id}/members/{username}

team.created webhook ──────────→  GitServerAdapter
                                  └─ GiteaAdapter.CreateTeam()
                                                              ─────→  POST /api/v1/orgs/{org}/teams
```

---

## 20. GitServerAdapter 接口设计

### 20.1 Interface 定义（Go 伪代码）

```go
// server/internal/gitsync/adapter.go

type GitServerAdapter interface {
    // Team 操作
    CreateTeam(ctx context.Context, t TeamSpec) (teamID string, err error)
    UpdateTeam(ctx context.Context, teamID string, changes TeamChanges) error
    DeleteTeam(ctx context.Context, teamID string) error

    // Member 操作
    AddTeamMember(ctx context.Context, teamID, userID string, role MemberRole) error
    RemoveTeamMember(ctx context.Context, teamID, userID string) error
    ListTeamMembers(ctx context.Context, teamID string) ([]Member, error)

    // 健康检查
    HealthCheck(ctx context.Context) error
}

type TeamSpec struct {
    Name        string
    DisplayName string  // Gitea 4.x+ 支持
    Description string
    OrgName     string  // Gitea organization name
    Permission  string  // "read" / "write" / "admin"
}

type MemberRole string  // "member" / "admin"（Gitea team 内的角色）
```

### 20.2 GiteaAdapter 实现

```go
// server/internal/gitsync/adapters/gitea.go

type GiteaAdapter struct {
    baseURL    string
    adminToken string  // @server 持有的 Gitea admin token（V4 §7.2.2 场景 #1 capability sync token 可复用，或独立 team-sync token；详见 G-3）
    httpClient *http.Client
    bindingResolver UserGiteaBindingResolver  // 通过 cs-user SDK 跨服务读 user_gitea_binding
}

func (a *GiteaAdapter) AddTeamMember(ctx context.Context, teamID, userID string, role MemberRole) error {
    // 1. 通过 cs-user SDK 查 user_gitea_binding 找 Gitea username（跨服务）
    binding, err := a.bindingResolver.GetByUserID(ctx, userID)
    if err != nil {
        return fmt.Errorf("user %s has no gitea binding: %w", userID, err)
    }

    // 2. 调 Gitea admin API
    url := fmt.Sprintf("%s/api/v1/teams/%s/members/%s", a.baseURL, teamID, binding.GiteaUsername)
    req, _ := http.NewRequestWithContext(ctx, "PUT", url, nil)
    req.Header.Set("Authorization", "token "+a.adminToken)

    resp, err := a.httpClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
        return fmt.Errorf("gitea returned %d", resp.StatusCode)
    }
    return nil
}
```

### 20.3 HR team → Gitea team 映射

HR org-team-service 的 team 与 Gitea team 不一一对应——需要**映射策略**：

| 映射策略 | 说明 | 适用场景 |
|---|---|---|
| **基于 path 前缀**（推荐） | Gitea organization 对应 HR 顶层部门；Gitea team 对应 HR 子部门 | 多数企业组织 |
| **基于 metadata.git_sync 配置** | HR team 元数据显式声明 `git_org` + `git_team` | 复杂映射 |
| **统一映射表** | @server 维护 `team_git_mapping` 表 | 兜底方案 |

**首期实现**：基于 path 前缀（最简单）：
- HR team path `/engineering/backend` → Gitea org `engineering` + Gitea team `backend`
- 由 @server 内部 TeamMapper 计算映射，不需要 Gitea 端配置

### 20.4 模块组织

```
server/internal/gitsync/
├── adapter.go              # GitServerAdapter interface
├── team_mapper.go          # HR team → Gitea team 映射逻辑
├── service.go              # 业务编排（webhook → adapter）
├── retry.go                # 失败重试 + 死信队列
└── adapters/
    ├── gitea.go            # GiteaAdapter 实现
    └── gitlab.go           # 未来扩展（占位）
```

---

## 21. 同步流程与失败处理

### 21.1 实时同步流程（事件驱动）

@server 监听 org-team-service 的 webhook（与 TEAM_ORG_UNIFICATION 共用 endpoint，或独立 endpoint）：

```go
// server/internal/gitsync/service.go

func (s *Service) HandleOrgTeamEvent(ctx context.Context, event OrgTeamEvent) error {
    switch event.Type {
    case "team.created":
        return s.handleTeamCreated(ctx, event.Payload)
    case "team.deleted":
        return s.handleTeamDeleted(ctx, event.Payload)
    case "member.added":
        return s.handleMemberAdded(ctx, event.Payload)
    case "member.removed":
        return s.handleMemberRemoved(ctx, event.Payload)
    case "member.role_changed":
        return s.handleMemberRoleChanged(ctx, event.Payload)
    }
    return nil  // 忽略未识别事件
}

func (s *Service) handleMemberAdded(ctx context.Context, p MemberPayload) error {
    // 1. HR team → Gitea team 映射
    giteaTeamID, err := s.TeamMapper.HRTeamToGitea(p.TeamID, p.TeamPath)
    if err != nil {
        return err  // 无映射关系的 team 静默跳过
    }

    // 2. 调 adapter（带重试）
    return s.Adapter.AddTeamMember(ctx, giteaTeamID, p.UserID, p.Role)
}
```

### 21.2 同步触发条件（哪些 HR 变更需要同步）

| HR 事件 | 是否触发同步 | 理由 |
|---|---|---|
| team.created | **是**（如 HR team 有 git 映射） | 创建对应 Gitea team |
| team.updated（改名） | **是** | 更新 Gitea team 元数据 |
| team.deleted | **是** | 删除 Gitea team（注意：会移除所有成员的 fork 权限） |
| member.added | **是** | 加成员到 Gitea team |
| member.removed | **是** | 从 Gitea team 移除成员 |
| member.role_changed | **是**（角色对应 Gitea team admin） | 更新 Gitea team member 角色 |
| team 仅 metadata 变更 | **否** | 不影响 fork 权限 |

### 21.3 与 OrgService / GiteaUserSyncWorker 的协作

> **v3 决策调整**：原计划三个同步链路都在 cs-user 内；v3 反转后 GitServerAdapter 迁到 @server。cs-user 仅保留 GiteaUserSyncWorker（user-level 开户），OrgService + GitServerAdapter 在 @server 内（@server 同时承担业务侧 Gitea 协作与 team 同步）。

跨服务三个同步链路的边界：

| 链路 | 触发 | 数据流向 | 所在服务 | 状态 |
|---|---|---|---|---|
| **GiteaUserSyncWorker**（§11 已有） | Casdoor `user.*` webhook | user → Gitea 个人账号 | **cs-user** | 本提案前已有 |
| **OrgService**（消费 org-team-service） | org-team-service webhook | HR team → @server 缓存 | **@server** | TEAM_ORG_UNIFICATION 已设计 |
| **GitServerAdapter**（v3 迁入） | org-team-service webhook（同上） | HR team 变更 → Gitea team | **@server** | v3 反转，详见 ADR-3 v3 |

@server 内 OrgService 与 GitServerAdapter 共用同一个 webhook 接收点，分发到两个内部 handler（独立处理 + 独立失败重试）。

### 21.4 失败处理与重试

| 失败类型 | 处理 |
|---|---|
| 网络抖动（5xx / timeout） | 指数退避重试（5s / 30s / 2min / 10min / 1h），最多 5 次 |
| 4xx 错误（除 404 / 409） | 不重试，记录错误日志 |
| 404（team / user 不存在） | 静默跳过（HR 数据过期 / Gitea 数据已被删除） |
| 409（已存在 / 冲突） | 视为成功（幂等） |
| 重试 5 次仍失败 | 进入死信队列；admin 后台可见；可手动重放 |

**死信处理**：在 @server DB 增加 `gitsync_failed_events` 表（event_id / payload / last_error / retry_count / next_retry_at），admin 后台提供重放按钮。

### 21.5 全量重同步（冷启动 / 数据修复）

提供 admin 命令 `costrict-server gitsync resync --dry-run`（或等价 API）：

1. 从 org-team-service 拉全量 team + member 数据（详见 TEAM_ORG_UNIFICATION §VIII-A R-F1）
2. 对每条记录计算映射 → 调用 GiteaAdapter 对应方法（幂等）
3. 比对 Gitea 当前 team_user 与 HR 期望状态，输出 diff 报告
4. `--apply` 标志实际执行差异同步

适用场景：@server 冷启动、误操作后的数据修复、git-server 迁移后回填。

---

## 22. 与 TEAM_ORG_UNIFICATION 的关系（v3 调整）

### 22.1 职责矩阵（v3 反转后）

| 关注点 | TEAM_ORG_UNIFICATION | @server（v3 承担 GitServerAdapter） | cs-user（v3 收缩） |
|---|---|---|---|
| 消费 org-team-service webhook | ✓（OrgService 缓存失效） | ✓（GitServerAdapter 写入 Gitea team_user） | — |
| HR 团队 / 成员结构（admin / distribution 用） | ✓ | — | — |
| Gitea team_user 表写入 | — | ✓（v3 迁入） | — |
| Gitea user 自动开户 + `user_gitea_binding` | — | — | ✓（保留） |
| fork repo 权限校验 | — | （Gitea 内部，不感知） | — |
| HR team → Gitea team 映射 | — | ✓（v3 迁入） | — |

### 22.2 接口边界

- TEAM_ORG_UNIFICATION 的 OrgService（@server）与 GitServerAdapter（@server）**同服务内**，但仍保持**独立 module**，互不调用
- 两者**共享** org-team-service webhook endpoint，但内部 handler 独立
- `user_gitea_binding` 表（cs-user §11）由 cs-user 的 `GiteaUserSyncWorker` 维护；@server 的 GitServerAdapter 通过 cs-user SDK 只读查询 user_id → Gitea username 映射（**跨服务读取**）

### 22.3 部署耦合

- @server 部署后即可启动 GitServerAdapter（不依赖 TEAM_ORG_UNIFICATION 的 OrgService 上线）
- 即使 TEAM_ORG_UNIFICATION 整体未实施，@server 也可以独立消费 org-team-service webhook + 写 Gitea team_user
- @server 的 GitServerAdapter 依赖 cs-user 的 `user_gitea_binding`（通过 cs-user SDK），cs-user 故障会导致 AddTeamMember 失败（user_id 无法解析为 Gitea username）—— 需要重试 + 死信兜底
- 反之亦然：TEAM_ORG_UNIFICATION 可以独立部署，不依赖 @server 启动 GitServerAdapter（fork 权限需要其他方式维护）

---

## 23. 开放问题

| # | 问题 | 阻塞实施 |
|---|---|---|
| **G-1** | HR team path → Gitea org/team 的**映射策略**最终选择？基于 path 前缀（推荐）还是显式 metadata？ | @server M0 设计 |
| **G-2** | org-team-service 是否提供 **team.path 字段**（GitServerAdapter 计算映射时需要）？ | 与外部团队对齐 |
| **G-3** | Gitea admin token 在 @server 是复用 capability sync worker 的 token（V4 §7.2.2 场景 #1），还是独立 token（权限最小化）？ | @server M0 设计 |
| **G-4** | `gitsync_failed_events` 死信表的 admin 后台重放 UI 由谁实现？ | @server admin 子任务 |
| **G-5** | 是否需要 dry-run 模式（写入但不实际改 Gitea，用于试运行）？ | @server M0 设计 |
| **G-6** | @server GitServerAdapter 通过 cs-user SDK 读 `user_gitea_binding` 的 cache miss 处理策略？cs-user SDK 是否提供 `BatchGetGiteaBinding` 批量接口？ | cs-user SDK API 设计 |

---
