# Capability Git Registry 提案（v4 章节重排版）

| 字段 | 内容 |
|---|---|
| 状态 | Accepted (v3) · 章节顺序 v4 草稿 |
| 作者 | CoStrict 团队 |
| 创建日期 | 2026-07-06 |
| 决策日期 | 2026-07-07 |
| 重排版日期 | 2026-07-07 |
| 评审范围 | server / adminitem / services / casdoor / gitea fork |
| 关联文档 | [`CAPABILITY_GIT_REGISTRY_PROPOSAL.md`](./CAPABILITY_GIT_REGISTRY_PROPOSAL.md)（v3 章节原版，保留对照）、[`CAPABILITY_GIT_REGISTRY_ROADMAP.md`](./CAPABILITY_GIT_REGISTRY_ROADMAP.md)（实施路线图）、[`CAPABILITY_PORTAL_DECISION.md`](./CAPABILITY_PORTAL_DECISION.md)（portal 部署决策）、`IDENTITY_FEDERATION_DECISION.md`、`CATALOG_INGEST.md`、`SCAN_SKILL.md`、`DATABASE_DESIGN.md`、`HTTP_TUNNEL_DESIGN.md` |

> 本文件是 v3 PROPOSAL 的**章节重排版**，无内容删改，仅按"动机 → 静态架构 → 身份权限 → 运行时流程 → 数据与实施 → 附录"的逻辑顺序重新分段。所有决策与字段定义与 v3 一致；若发现冲突以 v3 为准（或反馈修正两边）。

---

## TL;DR

把 `capability_items`（Skill / Subagent / Command / MCP / Plugin 五类能力项）的**内容与版本真相源**从 PostgreSQL 迁移到基于 Gitea 的 Git 仓库；PostgreSQL 退化为运行时索引（缓存 + 业务字段）。每个能力项通过 `source_repo_url + source_repo_path + ref` 获得稳定的唯一可解析地址。**用户中心主权归 costrict-web**（自签 JWT，§6）；Casdoor 退化为多登录源 UI 提供者；Gitea fork 加 JWT 中间件（验证 JWT + 校验 `user_gitea_binding` 状态，账号由 sync worker 在 `user.created` 时 eager 创建，§6）；用户侧行为（csc push / AI agent 代用户 / API 调用）走用户自己 PAT 或 JWT，系统服务侧仅 `costrict-system` 单一 admin PAT 用于 2 个明确场景（capability 索引同步 + 用户生命周期级联，§7.2）。

仓库策略采用**按来源类型混合**：

- 独立 skill / subagent / command / mcp：一能力项一 repo
- Plugin 集中式 pack：一 pack 一 repo，子 plugin 用 path 寻址
- 上游镜像：保留原 repo 结构
- CoStrict 官方精选：可选 mono-repo（`costrict/curated-seed`）；统一动态配置中心独立 org `costrict-config/platform-config`

**发现层**：server 通过 webhook 实时同步 metadata 入 DB（`capability_items` 表），客户端 / AI 调 server REST API 搜索发现，**不**经过 Gitea API。Gitea repo description 由用户自行维护，server 不读不写。

---

## 目录

```
Part I：动机与目标
  1. 背景与痛点
  2. 目标与非目标

Part II：架构（Static View）
  3. 整体架构（拓扑 + 数据流 + Git/DB 职责边界）
  4. 仓库策略（来源类型 + 物理布局 + 公私能力可见性）
  5. 文件 frontmatter Schema

Part III：身份与权限（Who & How to Access）
  6. 认证集成（用户中心 + fork JWT 中间件）
  7. 权限模型（用户 PAT + admin PAT + Git 操作权限 + 硬配额）
  8. 业务线层级（E4 简化方案）

Part IV：运行时流程（Dynamic View）
  9. AI 操作工作流（直推 main + 可选 PR）
  10. 同步链路：Gitea API 驱动
  11. 能力项健康度与污染治理
  12. 安全扫描迁移
  13. 动态配置中心

Part V：数据与实施
  14. 数据模型变更
  15. 实施路径（瘦身版，详情见 ROADMAP）
  16. 风险与对策

Part VI：附录
  17. 已决策项
  18. 替代方案
  19. 后续工作
  20. 参考资料
  附录 A：Gitea 部署参考配置
  附录 B：webhook payload 关键字段
  附录 C：现有调用链影响与适配
```

---

# Part I：动机与目标

## 1. 背景与痛点

### 1.1 当前的"半个身子在 Git 里"现状

`CapabilityItem` 模型已经隐含 Git 语义：

- `SourcePath` / `CatalogEntryDir`：repo-relative 路径
- `SourceSHA`：文件指纹
- `CurrentRevision`：自增版本号
- `SourceType: direct | archive | fork` + `ForkedFromItemID`：fork 概念
- `Versions`：历史快照表

但实际链路是（见 `CATALOG_INGEST.md`）：

```
costrict-skills-repo（外部 git 仓库）
    └─ build_catalog_bundle.py
        └─ catalog-bundle.tar.gz（中转）
            └─ migrate ingest-upstream
                └─ capability_items 表
```

中间的 tarball 是把 Git 仓库"翻译"成 DB 行的胶水层。两套真相源、双重 schema、双重版本号。

### 1.2 痛点

| 痛点 | 表现 | 根因 |
|---|---|---|
| AI 操作不友好 | AI agent 想新增 skill 要走 REST API + 鉴权 + 业务校验 + 异步扫描 + 部署 | 内容存储是 DB 行而非文件，AI 必须学 OAS schema 而非直接读写文件 |
| 版本化管理缺失 | 没有原生 diff / branch / revert；`CapabilityVersion` 只是快照表 | DB 模拟版本系统代价高，缺少 Git 成熟生态 |
| 双重真相源 | catalog repo 改了，DB 还要等下次 ingest | tarball 中转引入不一致窗口 |
| 审核流程割裂 | 用户/AI 改内容走 API；上游批量改走 bundle | 没有统一的 PR/审核入口 |
| 跨环境同步难 | 多个私有化部署间共享能力项需要导出/导入 | DB 表难以跨实例同步，Git 本来就是为此而生 |
| 仓库地址不唯一 | 同一能力项在上游/镜像/本地下游 repo 地址混乱 | 缺少稳定的可解析 URN |

### 1.3 已有资产（不推翻重做）

- `services.CatalogIngestService`：差分、扫描、版本号逻辑可复用
- `CapabilityRegistry` 模型：已支持多 registry 概念，天然适配"公共 + 企业私有 + 镜像"
- 安全扫描体系（`docs/SCAN_SKILL.md`）：扫描逻辑与存储无关，可直接保留
- `UserAuthIdentity`：跨 IdP 身份关联已有，可扩展到 Gitea

---

## 2. 目标与非目标

### 2.1 目标

1. **Git 为内容真相源**：Skill/Subagent/Command/MCP/Plugin 的 content、version、author、history 全部由 Git 承载
2. **DB 为运行时索引**：保留 `capability_items` 表用于搜索、计数、状态、扫描结果，但内容字段是 Git 的缓存
3. **每能力项有稳定可解析的唯一地址**：`source_repo_url + path + ref`，覆盖 standalone / pack / mirror / seed 四类来源
4. **能力项粒度 = repo 或 repo + path，不下钻**：一个能力项对应一个 standalone/mirror/seed repo，或一个 pack repo 内的某个 plugin 顶层目录。**plugin 内部的子 skill / command / mcp 不再单独作为能力项管理**，server 不解析、不索引、不入 DB
5. **AI 操作原生 Git**：AI 通过 `git clone → 编辑 → commit → push → PR` 流程完成能力项管理，零 REST API 调用
6. **统一审核工作流（双轨制）**：默认通道为 **git push 直推 main**（§7.3 简化方案，与 V2 编辑 UX 一致）；**可选 PR 通道**用于公有能力贡献 / 跨业务线变更 / 重要能力项评审（AI 起草 → 人类审核 merge）；两条通道都触发 sync + health/security check；误推送靠 §11 健康度治理 + git revert 兜底
7. **消除 catalog bundle 中转**：上游 repo 通过 Gitea mirror 或 git remote 直接接入
8. **用户中心主权归 costrict-web**（自签 JWT + 业务字段），**Casdoor 退化为多登录源 UI 提供者**（GitHub OAuth / 短信 / LDAP 等社交登录入口），Gitea fork 加 JWT 中间件验证 costrict-web 签发的 JWT + 校验 `user_gitea_binding` 状态（账号由 sync worker 在 `user.created` 时 eager 创建，§6）；用户/AI 在 costrict-web 与 Gitea 之间通过同域 cookie + JWT 实现 SSO
9. **发现层走 server REST API**：客户端 / AI 通过 `GET /api/capabilities` 搜索发现，server DB（`capability_items`）是唯一发现索引；不维护派生 Git 索引，Gitea repo description 由用户自行维护

### 2.2 非目标

- **不动 Casdoor 多源登录 UI 能力**（继续承担 GitHub OAuth / 短信 / LDAP 等社交登录入口），但**用户中心主权移到 costrict-web**——Casdoor 不再是 IdP，仅是登录方式选择器；用户身份 / 属性 / 角色由 costrict-web 主权管理并通过 webhook 广播（§6）
- **不引入 Gitea Cluster（Praefect 类）** —— 单实例 + 共享存储足够当前规模
- **不强制一 item 一 repo** —— Plugin pack 与 mirror 保留原生结构
- **不下钻 plugin 内部** —— Plugin pack 整体是**一个**能力项；plugin 内部子 skill / command / mcp 文件由 plugin 自身运行时管理，server 不解析、不索引、不入 DB
- **不重写业务字段存储** —— `PreviewCount` / `InstallCount` / `FavoriteCount` / `SecurityStatus` 等运行时字段仍在 DB
- **不替换 PostgreSQL** —— DB 仍是业务/搜索/计数的依赖

---

# Part II：架构（Static View）

## 3. 整体架构

### 3.1 部署拓扑

```
┌────────────────────────────── 用户 / AI agent ───────────────────────────────┐
│                                                                              │
│   浏览器（Casdoor 多源登录 UI）       AI agent / csc（用户 fine-grained PAT）│
│         │                                    │                                │
└─────────┼────────────────────────────────────┼────────────────────────────────┘
          │                                    │
          ▼                                    │
┌─────────────────────────────────────────┐    │
│  Casdoor（多登录源 UI 提供者）           │    │
│  GitHub OAuth / 短信 / LDAP / 密码      │    │
└─────────┬───────────────────────────────┘    │
          │ 登录回调                             │
          ▼                                    │
┌─────────────────────────────────────────┐    │
│  costrict-web（用户中心主权方）          │    │
│  自签 JWT (RS256 + JWKS) + 业务字段     │    │
│  user.updated/disabled/deleted webhook  │    │
└─────────┬───────────────────────────────┘    │
          │ JWT (Authorization: Bearer)         │
          ▼                                    ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                  Gitea (capability registry, fork + JWT 中间件)              │
│                                                                             │
│   costrict-config/platform-config   平台配置中心（.gitea/*.yaml GitOps 真相源）│
│   costrict/<slug>                   官方独立能力项 repo（admin 维护 / public）│
│   costrict/curated-seed (可选)      CoStrict 精选能力项 mono-repo            │
│   costrict-plugins/<pack-slug>      Plugin 集中式 pack（admin 维护）         │
│   costrict-mirror/<escaped-url>     上游镜像 repo（read-only）               │
│   u-<username>/<slug>               用户个人 namespace（默认 public，可改 private）│
│                                                                             │
│   fork JWT 中间件（验证 JWT + 校验 binding 状态，§6.4）                      │
│   webhook ──push──► costrict-web server                                     │
└──────────────────────────────┬──────────────────────────────────────────────┘
                               │ Gitea REST API: compare / raw / trees
                               │ (`costrict-system` admin PAT，§7.2)
                               ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                       costrict-web server                                    │
│                                                                             │
│   sync worker（新）        ──►  capability_items 表（运行时索引）            │
│   security scan worker     ──►  security_status / last_scan_id              │
│   REST API（发现 + 业务字段）──►  /api/capabilities、favorite_count、...     │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 3.2 数据流方向（单向，关键约束）

```
唯一允许方向：Git → DB
禁止方向：DB → Git
```

所有内容变更必须经 Git push。业务字段（计数、状态、扫描结果）的变更走 DB，不影响 Git。**发现层（搜索、列表）走 server REST API 查 DB**，不维护任何派生 Git 索引。

### 3.3 Git 与 DB 职责边界

| 数据类型 | 真相源 | 说明 |
|---|---|---|
| 文件内容（md/yaml/json） | **Git** | AI 直接改 |
| 版本与历史 | **Git** | tag/commit/branch |
| 作者与时间戳 | **Git** | `git log` 即审计日志 |
| Fork 关系 | **Git fork** | 替代 `ForkedFromItemID` |
| 资源（assets/binaries） | **Git LFS** 或 **Gitea Release** | 二进制走 LFS |
| `PreviewCount` / `InstallCount` / `FavoriteCount` | **DB** | 运行时计数，不能写进 Git |
| `SecurityStatus` / `LastScanID` | **DB** | 扫描结果是 server 判断，不应入 Git |
| `Metadata`（索引字段，如 tags） | **DB（派生）** | 由 frontmatter 解析得到 |
| 用户 favorite 关系 | **DB** | 用户私有状态 |
| `Health` / `Evaluation` | **DB** | 运行时数据 |
| `Status`（active / archived / banned） | **混合** | archived = Git 删除/移动；banned = DB 行政状态 |

**核心原则**：Git 管"是什么"，DB 管"运行得怎么样"。

---

## 4. 仓库策略

### 4.1 来源类型矩阵

每个 repo 在 server 端注册一个 `CapabilityRegistry`，含 `kind` 字段决定同步策略：

| kind | 来源类型 | 同步策略 | AI 写入 | item 地址形式 | 能力项粒度 |
|---|---|---|---|---|---|
| `standalone` | 独立 skill/subagent/command/mcp | 单 repo 单 item，直接 push | 允许 PR | `<repo-url>/skill.md` | repo 级：整个 repo = 一个能力项 |
| `pack` | Plugin 集中式 pack | 单 repo 多 item，按 path | 允许 PR（pack owner） | `<repo-url>/-/tree/main/plugins/<id>/.plugin.json` | path 级：`plugins/<id>/` 目录整体 = 一个能力项（plugin 内部子文件不下钻） |
| `mirror` | 上游镜像 | 单向 pull（read-only） | 禁止 | `<mirror-repo-url>/-/tree/main/<path>` | 视上游形态：standalone 上游 = repo 级；pack 上游 = path 级 |
| `seed` | CoStrict 官方精选 | 集中维护，单 repo 多 item | 允许 PR | `<seed-repo>/-/tree/main/<type>/<slug>/...` | path 级：`<type>/<slug>/` = 一个能力项 |

> **关键约束**：sync handler 永远只读**能力项顶层 metadata 文件**（standalone 是 `skill.md` / `subagent.md` 等，pack 是 `plugins/<id>/.plugin.json`），不深入子目录解析内部 skill/command/mcp 文件。详见 §10.3。
>
> **pack 平级原则**：`costrict-plugins/` org 下所有 pack repo **完全平级**——无论是用户自建 / 官方维护 / plugin marketplace 收敛产出的 pack，V3 视角下都是同一种 `pack` kind，按相同规则 sync。pack 内 plugin 数量 / 来源 / 分组方式由 pack owner 自由决定，V3 不规定。marketplace 的 build pipeline（独立项目 `costrict-plugin-marketplace`）只是 pack 内容的**生产侧**，产出的 pack repo 推送到 `costrict-plugins/` 后即成为普通 pack，无任何特殊待遇。

server webhook 接到不同 kind 的 repo push 时，走不同的 sync handler。

### 4.2 物理仓库布局

```
gitea.costrict.local/
├── costrict-config/                          # 平台配置中心 org（admin 强制 public，仅 admin push）
│   └── platform-config/                      # GitOps 真相源
│       ├── .gitea/                           # 全局 Gitea 动态配置（§13）
│       │   ├── branch-protection.yaml
│       │   ├── quota.yaml
│       │   ├── teams.yaml
│       │   ├── webhooks.yaml
│       │   └── labels.yaml
│       ├── ISSUE_TEMPLATE/
│       ├── PULL_REQUEST_TEMPLATE.md
│       └── README.md                         # yaml schema 与编辑流程说明
│
├── costrict/                                 # 官方能力项 org（admin 维护，public）
│   ├── skill-vetter                          # standalone kind（一 item 一 repo）
│   ├── code-reviewer-subagent
│   ├── refactoring-command
│   ├── filesystem-mcp
│   └── curated-seed/                         # seed kind（可选 mono-repo）
│       ├── skills/<slug>/skill.md
│       ├── commands/<slug>/command.md
│       └── README.md
│
├── costrict-plugins/                         # Plugin 集中式 pack（admin 维护）
│   ├── anthropic-skill-pack                  # 一 pack 一 repo，多子 plugin
│   │   └── plugins/<plugin-id>/.plugin.json
│   └── openai-function-pack
│
├── costrict-mirror/                          # 上游镜像（read-only）
│   ├── github-com-zgsm-ai-everything-ai-coding   # 完整 mirror，保留 path 结构
│   └── github-com-anthropics-skills
│
└── u-<username>/                             # 用户个人 namespace（每个认证用户 1 个 owner）
    ├── u-alice/
    │   ├── skill-experiment                  # 用户自建 standalone（默认 public）
    │   └── draft-subagent
    └── u-bob/
        └── my-mcp-test
```

> **org / namespace 总览**：
> - **4 个固定 org**：`costrict-config`（配置中心）/ `costrict`（官方能力项）/ `costrict-plugins`（pack）/ `costrict-mirror`（镜像）
> - **用户 namespace**：`u-<username>/`，sync worker 在 `user.created` 时 eager 创建 Gitea user 后，Gitea 原生支持 user namespace
>
> **`costrict-config/platform-config` 的特殊地位**：
> - GitOps 控制面板，所有 Gitea 全局配置的真相源
> - branch protection 稍严（仅 admin push；应急 force push 走 admin override，§13.4）
> - `GiteaConfigSyncWorker` 监听其 push webhook，自动 diff `.gitea/*.yaml` 变更并应用
>
> **用户 namespace 规则**：
> - 命名：`u-` 前缀 + 用户 username（避免与官方 org 命名冲突）
> - 创建：sync worker 在 `user.created` 时调 `POST /admin/users` 创建 Gitea user（eager 模式），Gitea 原生支持 user namespace `u-<username>/`
> - 默认 visibility：**public**（用户能力项天然公开，被发现 / 被 fork / 被下发是主路径）；用户可在自己 namespace 内把 repo 改 private 作草稿/未发布
> - 发现层不主动过滤：marketplace 列表全量返回所有 item（含 `u-*/*` 下 public 与 private 项），通过 `visibility` + `owner` 字段标识；前端 UI 上 private 项加锁形图标，详情访问走 Gitea 权限校验（§4.7.1）
> - 升级为官方：用户提 PR 到 `costrict/<slug>` → admin merge 后转官方 org（§4.8）；`costrict/` org 仅作**官方印章**（admin 审核背书），与是否 public 无关——用户在自己 `u-<username>/` 下已经可以 public

### 4.3 仓库归属规则

> **核心原则**：用户行为产生的 repo（自建 / fork）**一律自动归属到 `u-<username>/` 个人 namespace**；`costrict-*` 四个固定 org 只能由 admin 维护，是**官方资产 / 官方印章**——`costrict/` 与 public/private 无关，仅表示"admin 已审核背书"。用户在自己 namespace 下默认就 public，可被发现 / 被 fork / 被下发；升级为"官方认证"才走 §4.8 PR 流程。

**归属矩阵**：

| 来源 | 自动归属 namespace | 谁能创建 | 默认 visibility |
|---|---|---|---|
| 用户独立创建（直推 main） | `u-<username>/<slug>` | 用户本人（PAT 限定 owner=`u-<self-username>`） | **public**（用户可改 private 作草稿） |
| 用户 fork 官方 repo（§9.2 PR 流程） | `u-<username>/<repo-name>`（Gitea 原生 fork-to-personal） | 用户本人 | **public**（继承自用户 namespace 默认） |
| 用户 fork 上游 mirror（改进 mirror） | `u-<username>/<repo-name>`（mirror 本身 read-only，必须 fork） | 用户本人 | **public** |
| 官方 standalone | `costrict/<slug>` | admin（PR merge 后入官方 org） | public（admin 强制） |
| 官方 pack | `costrict-plugins/<pack-slug>` | admin / pack owner | public（pack owner 可改 private） |
| 上游 mirror | `costrict-mirror/<escaped-url>` | admin（Gitea mirror 配置） | 跟随上游 |
| 精选 seed | `costrict/curated-seed/<type>/<slug>/` | admin（PR merge） | public（admin 强制） |
| 平台配置 | `costrict-config/platform-config` | admin（强制） | public（GitOps 真相源） |

**两条强约束**：

1. **`costrict-*` org 仅 admin 维护**——用户 PAT scope 限定 `read:repository` + `write:repository` 且 owner 限定 `costrict` / `costrict-plugins` / `u-<self-username>`（§7.3.3）；用户对 `costrict*` org 无 admin 权限，不能直接 push main（branch protection 已放开 §7.3.2，但 collaborator write 权限需要 admin 主动授予，普通用户默认无）。官方认证入口仅 PR（§4.8 / §9.2）
2. **用户自建 / fork 默认 public + 全量返回带标记**——`u-*/*` 下 repo 默认 `is_private=false`；发现层 API 全量返回所有 item（含 `u-*/*` 下 public 与 private），通过 `visibility` 字段 + owner 标识；用户可自主在自己 namespace 内把 repo 改 private 作草稿/未发布；访问详情/内容时按 Gitea 权限校验，无权直接 403（§4.7.1）；升级为官方（`costrict/` org 印章）才走 §4.8 PR 流程

**与 fork JWT 中间件的协作**（§6.6）：

- sync worker 在 `user.created` 时已创建 Gitea user（eager 模式）→ Gitea 原生支持 user namespace `u-<username>/` 自动可写；用户首次 push 时 binding 已是 `synced` 状态，fork JWT 中间件直接放行
- 用户 PAT 创建 repo 时若 owner 不在白名单（`costrict` / `costrict-plugins` / `u-<self-username>`），Gitea 原生权限校验直接拒绝；无需额外 server 端逻辑

**fork 上游 mirror 的特殊性**：

- mirror repo（`costrict-mirror/<escaped-url>`）只读，用户 PAT 无法 push
- 用户 fork mirror 到 `u-<username>/<repo-name>` 后变成普通 standalone repo（kind 仍为 `mirror`，但 visibility / write 权限独立）
- 改进通过 PR 提交回上游 GitHub（不在 V3 平台闭环内），或直接维护在用户 namespace 作为新 standalone

### 4.4 镜像仓库命名规则

镜像仓库名采用 **escaped upstream URL** 形式：

| 上游 URL | Gitea mirror repo |
|---|---|
| `https://github.com/zgsm-ai/everything-ai-coding` | `costrict-mirror/github-com-zgsm-ai-everything-ai-coding` |
| `https://gitlab.com/anthropics/skills` | `costrict-mirror/gitlab-com-anthropics-skills` |

转换规则：
- scheme 与 host 用 `-` 连接
- path 的 `/` 替换为 `-`
- 全部小写
- 末尾保留 owner-repo 段，便于人眼识别

优势：
- 一眼看出上游来源
- 不会与本地 `costrict/<owner>` 命名冲突
- 上游重命名时本地 id 不变，DB 中 `source_repo_url` 自动跟随

### 4.5 唯一仓库地址

每个 `CapabilityItem` 在 DB 中存储：

```
source_repo_url     = "https://gitea.costrict.local/costrict-plugins/anthropic-skill-pack"
source_repo_path    = "plugins/claude-foundation/.plugin.json"
source_repo_ref     = "main"
source_repo_kind    = "pack"
```

对外暴露的可解析 URL：

```
https://gitea.costrict.local/<owner>/<repo>/-/tree/<ref>/<path>
```

这是 Gitea/GitLab 通用 URL 形式，用户/Al 一键跳转到对应文件。

### 4.6 能力类型识别机制

能力类型（`skill` / `subagent` / `command` / `mcp` / `plugin`）与仓库 kind（`standalone` / `pack` / `mirror` / `seed`）是**两个独立维度**。识别走"文件结构启发式 + 首次锁定"模式，不引入新的声明文件（与社区习惯兼容）。

**启发式识别规则**（任一命中即识别）：

| 能力类型 | 识别规则 | 社区先例 |
|---|---|---|
| `skill` | 根或 `skills/<slug>/` 下有 `SKILL.md` / `skill.md`（带 YAML frontmatter） | Anthropic Skills、Claude Code |
| `subagent` | `agent.md` / `subagent.md` / `AGENT.md` / `.agent/agent.yaml` | Claude Code subagents |
| `command` | `commands/*.md` 或 `.claude/commands/*.md`（每个 .md 一个 command） | Claude Code slash commands |
| `mcp` | `mcp.json` / `.mcp.json` / `manifest.json`（含 `mcp` schema）；或 `pyproject.toml` / `package.json` 名称含 `-mcp` | MCP server 标准 |
| `plugin` | `.plugin.json` / `plugin.json` / `plugin-manifest.json`；pack 形式 `plugins/<id>/.plugin.json` | OpenAI plugins、VSCode extensions |

**仓库 kind 推断**（独立维度）：

| kind | 识别规则 |
|---|---|
| `standalone` | 根目录直接命中上述文件 |
| `pack` | 命中 `plugins/<id>/.plugin.json`（多个子项） |
| `mirror` | `capability_registries` 表的 `mirror_of` 字段非空（注册时人工标注） |
| `seed` | 根目录同时有多个 `<type>s/` 一级目录（如 `skills/` + `commands/` + `plugins/`） |

**首次识别 + 锁定流程**：

```
1. 新 repo push → sync worker 扫描根结构
2. 启发式规则推断 capability_type + repo_kind
3. 写入 capability_items（含 capability_type 字段，identification_status = clean）
4. 后续 sync 不再识别，按已锁定类型处理
5. 识别置信度低 → identification_status = unknown，进 pending_review 队列
```

**混合 repo 处理**：一个 repo 同时命中多类型文件（如 `SKILL.md` + `commands/*.md`）合法——识别为**复合 repo**，`capability_items` 多行（每行一个 capability_type，共享 `source_repo_url`），`repo_kind = standalone`，每个 item 独立 `source_repo_path`。

**类型锁定后的变更**：必须先在 costrict-web 后台"申请类型变更"——审核通过后 unlock → 改文件 → re-sync。直接改文件结构会触发健康度告警（见 §11）。

> **POC 简化决策（2026-07）**：`capability_type` 仅用于 costrict-web 应用层的锁定与展示，**不参与 §7.4 硬配额的差异化计算**——配额仅按 owner/repo 维度生效（统一默认值 + per-repo 覆盖）。fork Gitea 完全不感知类型，类型识别与锁定均由 sync worker 在应用层完成。

**Mirror repo 特殊处理**：mirror pull 后 server 检测 upstream 文件结构变化（如 `SKILL.md` 消失）→ 标记 `polluted`（见 §11），由 owner 决定是否继续 mirror。

### 4.7 公私能力与可见性

采用**纯 Gitea visibility 透传**模型——公私边界完全由 Gitea repo 的 `public/private` 决定，不引入 costrict-web 层的细粒度 ACL。

**可见性取值**（与 Gitea 原生对齐）：

| visibility | 含义 | Gitea 配置 |
|---|---|---|
| `public` | 所有登录用户可见、可 clone、可下发 | repo `is_private=false` |
| `private` | 仅 owner + collaborator + team 成员可见 | repo `is_private=true` |

**不引入** `internal` / `hidden` 等中间态——业务线层级仅作为 metadata 标签（§8），不参与权限控制；草稿/未发布通过用户在自己 namespace 内把 repo 改 private 表达。

**来源类型 × 默认 visibility**：

| kind / org | 默认 visibility | 可调整 |
|---|---|---|
| `costrict/` 官方 standalone | public | 否（admin 强制；官方印章） |
| `costrict-plugins/` pack | public（pack owner 可改 private） | 是 |
| `costrict-mirror/` | 跟随上游（公开上游→public） | 否 |
| `costrict-config/platform-config` | public | 否（admin 强制；GitOps 真相源） |
| `costrict/curated-seed` | public | 否 |
| **`u-<username>/` 用户自建 standalone / fork** | **public**（默认可被发现、可 fork、可下发；与 `costrict/` org 平级可见） | 是（用户可在自己 namespace 内改 private 作草稿；与"官方认证"无关，仅控制可见性） |

> **`costrict/` org 仅是官方印章**：在 `costrict/` 下的 repo 表示 admin 已审核背书（如官方维护、质量认证、安全扫描通过等），其 public 性质与 `u-<username>/` 下的 public repo 完全等价——`costrict/` 不是"公共"的反义，也不是 public 的前置条件。用户 repo 是否 public 完全由用户自主决定。

#### 4.7.1 核心设计：发现层与权限层分离

为最大化发现层 API 性能（避免每次列表请求都调 Gitea 反查），可见性控制分两层：

| 层 | 行为 | 性能权衡 |
|---|---|---|
| **发现层**（`GET /api/capabilities` 列表 / 搜索） | **不做权限过滤**——全量返回所有 capability_items，每行带 `visibility: public\|private` 字段 + `owner`（如 `costrict` / `u-alice`） | 纯 DB 查询，零 Gitea API 调用；列表可缓存 |
| **内容访问层**（`GET /api/capabilities/:id` 详情 / `/download` 内容下发 / `git clone`） | 实时按 Gitea 权限校验：调 `GET /repos/{owner}/{repo}/collaborators/{username}/permission`（5min Redis cache）；无权 → **403 + 告警**"无访问权限，请联系 admin 或申请 collaborator" | 单次 Gitea API 调用，可缓存 |

**列表面板表现**（前端约定）：

- marketplace 默认展示所有 item（含 private 标记），private 项加锁形图标 + tooltip "Private — 详情需权限"
- 用户点击 private item 详情 → server 调 Gitea 校验 → 403 时前端展示"无访问权限"页 + 申请 collaborator 入口
- AI agent 调用发现 API 拿到 private item 列表，但尝试拉 content 时被 403 拒绝（agent 自行决策是否申请权限）

**理由**：
- 性能：发现层是高频访问，每次列表请求都调 Gitea `GET /user/repos` 反查会击穿 Gitea API 限流（默认 1500 req/h/user）
- 一致性：metadata（slug / name / description）默认不算敏感，"能看到名字"不等于"能用内容"；真正的 secret 在 content 里
- 简化：双层过滤的伪代码删除，发现层 API 实现极简

#### 4.7.2 Server sync worker 访问权限

- service account PAT scope = `read:repository`（全局，含 private repo）
- sync worker 扫描所有 repo（含 private）写入 `capability_items`，同时刷新 `visibility` 字段
- `capability_items.visibility` 字段每次 sync 时从 Gitea repo 配置（`is_private`）写入

#### 4.7.3 数据库字段

```sql
-- capability_items 新增字段（§14.1）
ALTER TABLE capability_items
  ADD COLUMN visibility VARCHAR(16) NOT NULL DEFAULT 'public';
  -- public | private，sync worker 同步时从 Gitea repo is_private 字段写入

-- capability_registries 同样新增（registry 级聚合用）
ALTER TABLE capability_registries
  ADD COLUMN gitea_visibility VARCHAR(16);
```

#### 4.7.4 其他约束

**Pack 内 plugin 私有化不支持**：pack repo 整体 public/private，pack 内某 plugin 不能单独私有。需要"部分 plugin 私有"时拆成两个 pack repo。

**业务线 internal 边界**（§8 E4 简化方案）：

- 业务线层级仅作为 metadata 标签（`capability_items.business_line`），**不参与发现层过滤、不参与权限控制**
- private repo 仅靠 per-user collaborator（admin 手动邀请，离职自动清理）
- 业务线外用户访问 → 内容访问层 Gitea 权限校验直接 403
- 假设条件：private capability 占比 < 10%；未来占比上升可平滑切到方案 3（dept → Gitea team sync），不影响现有 repo 命名

### 4.8 公有能力贡献流程

公有能力有**两条独立路径**——可见性与官方认证完全解耦：

#### 4.8.1 用户公开（默认路径，自主）

- 用户在 `u-<username>/<slug>` 下创建 repo，**默认即 public**（§4.7）
- 用户 PAT 直推 main → 进发现层（marketplace）→ 所有登录用户可见、可 fork、可下发
- 无需 admin 介入，无审核流程

#### 4.8.2 官方认证（可选路径，PR 升级）

`costrict/` org 是**官方印章**（admin 审核背书，表示"官方维护 / 质量认证 / 安全达标"），与是否 public 无关。用户可主动申请把已 public 的能力项升级为官方认证：

- 用户/业务线提 PR 到 `costrict/<slug>` 或 `costrict/curated-seed`（精选 mono-repo）
- admin 审核内容质量 / 安全扫描 / 与官方方向一致性
- merge → repo 落到 `costrict/` org（Gitea 原生 transfer-ownership 或 admin 重建 + redirect）
- 平台配置走 `costrict-config/platform-config` PR（admin 审核 yaml 变更）

**升级为官方后**：原 `u-<username>/` 下 repo 留 redirect（避免存量链接断），DB 中 `source_repo_url` 由 sync worker 自动跟随。

**mirror 上游限制**：仅支持**公开上游**（公开 GitHub repo 等）；私有上游 mirror 暂不支持（out of scope）。

**下发与调用层校验**：

- runtime 拉 manifest：server 调 Gitea API 校验 device owner 对该 repo 的 read 权限
- AI agent 选择能力：同上
- 校验失败返回 403，不返回 manifest
- public repo（无论 `costrict/` 还是 `u-<username>/`）所有认证用户均有 read；private repo 仅 owner + collaborator

**计费/限额**：暂不实施（out of scope）。

---

## 5. 文件 frontmatter Schema

### 5.1 目录内文件结构

```
costrict/skill-vetter/
├── skill.md           # 能力项顶层 metadata：frontmatter + 正文（server 解析此文件）
└── assets/            # 图片等附件

costrict-plugins/anthropic-skill-pack/
└── plugins/
    ├── claude-foundation/
    │   ├── .plugin.json    # 能力项顶层 metadata（server 解析此文件）
    │   ├── skills/         # plugin 内部子文件：server 不解析、不索引
    │   └── commands/       # plugin 内部子文件：server 不解析、不索引
    └── skill-pack-utils/
        ├── .plugin.json    # 另一个能力项
        └── ...

costrict-mirror/github-com-zgsm-ai-everything-ai-coding/
└── skills/<slug>/skill.md    # 保留上游原生结构

costrict/curated-seed/
└── skills/<slug>/skill.md
```

> **server 视角**：每个能力项 = 一个**顶层 metadata 文件**。standalone / mirror / seed 的 metadata 是 `skill.md` / `subagent.md` / `command.md` / `mcp.md`；pack 的 metadata 是 `plugins/<id>/.plugin.json`。子目录内容（plugin 内的 skill/command 文件、assets 等）由 plugin 自身运行时管理，server 视为黑盒。

### 5.2 统一 frontmatter（所有 kind 通用）

```yaml
---
slug: skill-vetter
type: skill                          # skill | subagent | command | mcp | plugin
name: Skill Vetter
description: Security-first skill vetting for AI agents.
descriptions:
  en: Security-first skill vetting for AI agents.
  zh: 针对 AI agent 的安全优先 skill 审查工具。
category: security
version: 1.0.0                       # 语义版本
metadata:
  tags: [security, vetting]
  author: costrict
  license: MIT
---

# Skill Vetter

正文内容...
```

> 对 pack 类，`plugins/<id>/.plugin.json` 已是 plugin 体系标准格式，server 直接消费其顶层字段（`name` / `version` / `description` / `install.marketplace_name` 等），不做语义改写。

### 5.3 CapabilityItem 字段映射

| CapabilityItem 字段 | 来源 |
|---|---|
| `Slug` | frontmatter `slug` |
| `ItemType` | frontmatter `type` 或目录 |
| `Name` | frontmatter `name` |
| `Description` / `Descriptions` | frontmatter `description` / `descriptions` |
| `Category` | frontmatter `category` |
| `Version` | frontmatter `version` |
| `Content` | 顶层 metadata 文件正文（frontmatter 之外的部分；plugin 类为空） |
| `ContentMD5` | server 同步时计算 |
| `Metadata` | frontmatter `metadata` |
| `SourceRepoUrl` | repo URL（kind 决定形态） |
| `SourceRepoPath` | **能力项顶层 metadata 文件**的 repo-relative path |
| `SourceRepoRef` | git ref（默认 main） |
| `SourceRepoKind` | standalone / pack / mirror / seed |
| `SourceSHA` | 顶层 metadata 文件所在 commit 的 SHA |
| `CreatedBy` | commit author email → 反查 user |
| `UpdatedBy` | committer email → 反查 user |
| `IsBuiltIn` | 由 path 前缀判定（如 `costrict/` 命名空间下视为 built-in） |
| `ForkedFromItemID` | 由 Gitea fork 关系反查 |
| `MirrorOf` | 仅 mirror kind：上游原始 URL |

### 5.4 资源与二进制

- 文本资源（markdown、yaml、json）直接进 Git
- 二进制资源（图片、plugin tarball、demo 视频）：
  - **小于 1 MB**：直接进 Git
  - **大于 1 MB**：使用 **Git LFS** 或 **Gitea Release attachment**
  - plugin tarball 一律走 Gitea Release，repo 内只放元数据

---

# Part III：身份与权限（Who & How to Access）

## 6. 认证集成（costrict-web 用户中心 + fork Gitea JWT 中间件）

> v3 方案：用户中心主权归 **costrict-web**（含 username / email / 密码 / 业务字段全部自主管理），Casdoor 退化为多登录源 UI 提供者，Gitea fork 加 JWT 认证中间件实现用户自动创建与同步。详见 `IDENTITY_FEDERATION_DECISION.md`。

### 6.1 角色分工

- **costrict-web（用户中心）**：username / email / 密码 / 业务字段（业务线 / 部门 / 角色 / 偏好 / 配额）的主权方；自签 JWT（RS256 + JWKS endpoint）；维护 `user_gitea_binding` 与 `user_profile` 表
- **Casdoor**：退化为**多登录源 UI 提供者**（GitHub OAuth / 短信 / LDAP 等社交登录），costrict-web 通过 Casdoor 完成多源登录后**自己签发 JWT**（不用 Casdoor JWT）；与现有 `UserAuthIdentity` 表配合做多身份绑定
- **Gitea（fork）**：在 `routers/common/auth.go` 链最前插入 JWT 中间件（~250 行 fork），验证 costrict-web 签发的 JWT + 校验 `user_gitea_binding.sync_status='synced'`（非 synced 返回 503）；账号创建由 sync worker 在 `user.created` 时 eager 完成，中间件不持 admin token、不调 internal `models.CreateUser`；不暴露用户管理 UI（headless）
- **系统服务账号 `costrict-system`**：单一 site-level user，签发**单一 admin PAT**；仅用于真正"无用户上下文 + 必须跨用户身份"的 2 个场景（见 §7.2）；AI agent / csc 等用户侧行为**不走 bot**，统一按用户自己 PAT（见 §7.3）

### 6.2 用户登录链路

```
用户访问 costrict-web
   └─► 选登录方式（密码 / GitHub OAuth / 短信 / ...）
       └─► Casdoor 处理多源登录 UI（仅登录 UI 提供者）
           └─► 回调 costrict-web ─► costrict-web 自签 JWT（含 user_id / preferred_username / email / groups）
               └─► 首次登录创建用户时触发 user.created webhook
                   └─► sync worker 异步：INSERT user_gitea_binding (pending) → POST /admin/users → UPDATE (synced, gitea_uid)

用户访问 Gitea
   └─► 携带 costrict-web JWT（Authorization: Bearer ...）
       └─► Gitea fork JWT 中间件：
           ├─ 验证签名（拉 costrict-web JWKS）
           ├─ 解析 claims（user_id / preferred_username / email / groups）
           ├─ 查 user_gitea_binding.sync_status：
           │   ├─ synced → 注入 session（按 gitea_username 匹配 Gitea user）
           │   └─ pending / error / 行不存在 → 503 + Retry-After: 5（worker 通常 100ms~1s 内完成）
           └─ 后续 handler 走原生 Gitea 流程
```

### 6.3 身份关联

- **跨服务引用统一使用 user_id（不可变）**，username 仅用于显示与 URL（可改）
- costrict-web `users.id` → Gitea `user.email` 字段（写入 `external_id` 形式，便于反查）；Gitea `user.name` = `u-<preferred_username>`（带前缀，避免与官方 org 命名冲突）
- costrict-web 维护 `user_gitea_binding(user_id, gitea_username, gitea_uid, sync_status)` 表，记录映射关系

### 6.5 username / namespace 命名约束（Gitea 兼容）

> 强约束：costrict `username` 必须能无损映射到 Gitea user name `u-<username>` 并通过 Gitea 服务端校验。

**Gitea 服务端实际约束**（源码 `modules/validation/validation.go` + `routers/api/v1/utils.go`）：

| Gitea 字段 | 默认上限 | 字符集 | 说明 |
|---|---|---|---|
| User name (`user.name`) | **40** (`MaxUserNameLength`) | `[a-z0-9_-]`（lower-cased），必须字母/数字开头 | 由 `AlphaDashPattern` 校验 |
| Org name (`user.name` type=org) | 40 | `[a-z0-9_-]` | 同上 |
| Repo name (`repository.name`) | 100 | `[a-z0-9_.-]` | 允许点 |
| Reserved names | - | - | 默认约 30 个：`admin` / `api` / `user` / `owner` / `repo` / `org` / `explore` / `login` / `help` / `settings` / `notifications` / `migrations` / `mirror` / `swagger` / `assets` / `vendor` 等，可在 `app.ini` `[user] RESERVED_USER_NAMES` 扩展 |

**`u-<username>` 推导**：

| 推导项 | 计算 | 结果 |
|---|---|---|
| costrict `username` 最大长度 | Gitea max 40 − `u-` 前缀 2 字符 | **38 字符** |
| costrict `username` 字符集 | 与 Gitea `AlphaDashPattern` 同源，但**主动收窄去掉 `-`** | `[a-z0-9_]`（**禁止短横线**） |
| costrict `username` 首字符 | Gitea user 名 `u-` 已是字母开头（合规），但 `u-` 后第二字符若为 `_` 会变成 `u-_xxx`——正则允许但语义混乱（prefix 误识别） | **强制字母/数字开头** |
| costrict `username` 尾字符 | 若以 `_` 结尾 → Gitea user 名以 `_` 结尾，Gitea 允许但不规范 | **强制字母/数字结尾** |
| 连续下划线 | `__` 会和 `u-` 前缀产生视觉混淆（`u-__foo`） | **禁止 `__` 连续** |
| Reserved words | Gitea reserved 默认包含 `admin` / `user` 等 30 个，但 Gitea user 名是 `u-<username>`（带前缀），`u-admin` 不在 reserved 列表 | **冲突自动隔离**，costrict 无需扩黑名单兜底 |

> **为什么禁止 `-`？** Gitea 原生允许 username 含 `-`，但 costrict 把 `u-` 作为 namespace 前缀保留——如果允许用户 username 中含 `-`，会产生 `u-alice-cool` 这种**无法解析 prefix 边界**的名字（是 `u-alice` + `-cool`？还是 `u-` + `alice-cool`？）。直接禁 `-` 从源头消除歧义，校验逻辑也更简洁（一条正则而非组合规则）。

**最终 costrict `username` 校验规则**（注册 / 改名时 server 端强校验）：

```
正则：^[a-z0-9](?!.*__)[a-z0-9_]{1,36}[a-z0-9]$
等价简化（推荐服务端实现）：
  - re.match(r'^[a-z0-9][a-z0-9_]{1,36}[a-z0-9]$', username)          # 长度 3-38，首尾字母数字
  - and not re.search(r'__', username)                                  # 禁止连续下划线
存储：lower(username)，与 username_lower 唯一索引配合
```

**校验时机**：
- `POST /api/users`（注册）
- `PATCH /api/users/me`（改名）— admin 改名同样走此校验
- Gitea sync worker **信任** costrict 已校验过的 username，不再重复校验（避免双源真相）；若 Gitea `POST /admin/users` 仍失败（例如 Gitea 升级后规则变严），sync worker 标记 `sync_status='error'` + 告警，由 costrict-web 排查

**`gitea_username` 列长度**：`VARCHAR(40)`（不是 64），对齐 Gitea 上限。`u-` 前缀 + 38 字符 username = 40 上限恰好填满。
- `UserAuthIdentity` 表扩展支持 Casdoor 各登录源 provider（github/sms/ldap）

### 6.4 username 全生命周期

| 阶段 | costrict-web 动作 | Gitea 动作 | 其他订阅方 |
|---|---|---|---|
| 注册 | 校验 username 唯一 + 写 `users` + 签 JWT → 触发 `user.created` webhook | sync worker 调 `POST /admin/users` 创建 + 写 `user_gitea_binding`（pending → synced，eager 模式） | 无 |
| 首次访问 Gitea | 无 | JWT 中间件校验 `user_gitea_binding.sync_status='synced'` 放行；否则返回 503 + `Retry-After: 5` | 无 |
| **username 变更** | 校验新 username 唯一 → 更新 `users` → 触发 `user.updated` webhook | sync worker 调 `PATCH /admin/users/{old}` 改名（Gitea 自动级联 repo ownership + redirect） | cs-cloud / csc 清缓存 |
| 用户禁用 | 标记 `users.status=disabled` → webhook `user.disabled` | 调 Gitea admin API 设 `login_prohibited` | 各服务拒绝该 user 请求 |
| 用户注销 | 删 `users` → webhook `user.deleted` | 调 Gitea admin API 删除（repo ownership 转给 `costrict-system`，保留 repo 历史，不归档不删除） | 各服务清理本地状态 |

**username 变更不可改的部分**：
- git commit author 历史（immutable，旧 commit 显示旧 username）
- 用户已签发的 PAT（绑定 user_id，仍有效）

### 6.5 webhook 多目标广播

`user.updated` / `user.disabled` / `user.deleted` 事件通过 costrict-web 通用 webhook 系统广播给所有订阅方。

**事件 schema**：

```json
{
  "event_id": "evt_<uuid>",
  "event_type": "user.updated",
  "occurred_at": "2026-07-07T10:00:00Z",
  "subject": {
    "user_id": "u_abc123",
    "old_username": "alice",
    "new_username": "alice_wonderland"
  },
  "changed_fields": ["username"],
  "signature": "<HMAC-SHA256>"
}
```

**重试策略**：6 次指数退避（1s / 5s / 30s / 2min / 10min / 1h）→ 死信队列 → admin 手工处理；订阅方按 `event_id` 幂等；每日 cron 全量校对兜底。

### 6.6 fork JWT 中间件设计要点

**改动范围（最小化）**：
- 新增 `routers/common/auth_jwt.go`（JWT 中间件 ~200 行）
- 修改 `routers/web/routes.go`（注册中间件 ~10 行）
- 修改 `routers/common/auth.go`（中间件链插入 ~30 行）
- 不动 UI / cron / mirror / webhook 投递

**关键技术点**：
- JWKS cache：5min TTL，从 `https://costrict-web/.well-known/jwks.json` 拉
- JWT 验证：RS256，clock skew ±60s
- **JWT 来源**：双通道 fallback（适配 portal iframe 直连 Gitea API 与 csc/SDK PAT 两种客户端形态）
  - 优先读 `Authorization: Bearer <jwt>` 头（csc / SDK 走 PAT 风格调用）
  - fallback 读 HttpOnly cookie `costrict_jwt`（domain=`.costrict.local`，portal iframe 同域自动带）
  - 两条通道共用同一套 JWKS 验证逻辑，仅在 request context 里取 token 的位置不同
  - cookie 解析仅 +20 行（`r.Cookie("costrict_jwt")` + 头部 fallback），不引入 session 状态
- auto-provisioning：**已移除**（eager 模式下由 sync worker 在 `user.created` 时调 `POST /admin/users` 创建）
- **binding 状态校验**：中间件查 `user_gitea_binding.sync_status`，非 `synced`（pending / error / 行不存在）一律返回 `503 Service Unavailable` + `Retry-After: 5`；客户端退避重试，worker 通常 100ms~1s 内完成创建
- groups claim → team membership：不在中间件做（避免阻塞），由 costrict-web sync worker 监听 webhook 后异步调 Gitea team API
- 性能：JWKS cache hit 时 < 5ms；JWKS refresh 失败时降级到 5min 前的旧 key

---

## 7. 权限模型

### 7.1 主体与凭据矩阵

| 主体 | 凭据 | 范围 | 用途 |
|---|---|---|---|
| 人类用户 | Casdoor 多源登录 → costrict-web 自签 JWT → Gitea fork JWT 中间件（§6.2） | 按用户角色 | 浏览 Gitea / PR / 审核 / web UI 操作 |
| 用户（git 客户端 / csc / AI agent 代用户） | 用户自己 fine-grained PAT（D 方案 C 通道，§7.3） | `read:repository` + `write:repository` 限定 owner | git push / clone / API 操作（commit author = 用户本人） |
| 服务端 sync worker | `costrict-system` 单一 admin PAT（§7.2） | `read:repository` + 跨 owner 含 private | 调 compare / raw / trees API 拉 capability 内容 + 用户生命周期级联 |
| Mirror 上游同步 | Gitea 自身（mirror pull） | Gitea → 上游，与 server 无关 | 由 Gitea 周期性执行，server 仅消费 mirror 后的 webhook |

> 注：传统 "deploy token" 仅适用于 git HTTPS clone/pull，本提案 server 端零 git CLI、纯 REST API（compare / raw / trees），因此所有凭据统一为 **PAT**（Personal Access Token，Gitea 的 bitmap scope 模型可精确到读/写、repo/issue/category 粒度）。

### 7.2 Gitea site-admin token（单一 `costrict-system` 账号）

#### 7.2.1 设计原则（D 混合方案）

| 通道 | 主体 | 鉴权 | 适用场景 |
|---|---|---|---|
| **用户侧**（D 方案 C 通道） | 用户本人 / AI agent 代用户操作 | 用户自己 PAT 或 JWT → fork 中间件 | 所有有用户上下文的行为（push / clone / API 操作） |
| **系统侧**（D 方案 B 通道） | 单一 `costrict-system` 账号 + 单一 admin PAT | costrict-web server 进程持有 | 仅 2 个真正"无用户上下文 + 必须跨用户身份"场景 |

**禁止把 admin PAT 用于**：用户代理 push、AI agent 自动化操作（这类必须用用户 PAT，审计到个人）。

#### 7.2.2 admin PAT 的 2 个明确使用场景

| # | 场景 | 触发 | API | 为什么必须 admin scope |
|---|---|---|---|---|
| **1** | **capability 索引同步** | Gitea push webhook + 5min 兜底 poll | `compare / raw / trees` 跨所有 owner **包括 private repo** | sync worker 是后端服务无用户上下文；private repo 任意单一用户无全量访问权但要建索引 |
| **2** | **用户生命周期级联** | `user.created` / `user.updated` / `user.deleted` / `user.disabled` webhook | `POST /admin/users`（创建）+ `PATCH /admin/users/{name}`（改名 / 禁启用）+ repo ownership transfer + collaborator 删除 | 这些是 Gitea admin-only API，JWT 中间件走不通 |

**marketplace mirror 同步不需要 admin token**：走 Gitea 内置 mirror pull 功能（§10.6 方式 A），Gitea 自己周期 `git fetch upstream`，不调 REST API。

#### 7.2.3 admin PAT 属性

| 字段 | 值 |
|---|---|
| 账号 | 单一 `costrict-system`（site-level user，admin scope） |
| Token 数 | 1 个 PAT |
| scope | `read:repository` + `write:repository` + `admin:org` + `admin:repo` + `read:user` + `write:user` |
| 持有方 | costrict-web server 进程（env / secret store 注入，不进 git） |
| 共享方 | cs-cloud sync worker（同一 token）/ capability 索引 cron job（同一 token） |
| 轮换 | costrict-web `BotTokenRotationWorker` 提前 14 天签发新 token → 写 secret store → webhook 通知下游 reload → 老 PAT 自然过期 |
| 撤销 | admin UI 一键 revoke（应急） |
| 审计 | Gitea audit log 全量记录 + costrict-web `gitea_admin_audit_log` 表关联触发源（`sync_runs` / `user_lifecycle_events`） |

#### 7.2.4 用户注销时 repo ownership 接管

注销时 admin token 把用户所有 repo ownership **转移给 `costrict-system` 账号**（不归档、不删除）：
- 保留 commit 历史、issue、PR 完整可追溯
- repo 自动转为 `costrict-system/<原-repo-name>`，原 URL 自动 redirect（Gitea 原生级联）
- `costrict-system` 名下 repo 列表在 admin UI 单独展示，admin 可按需手动清理

#### 7.2.5 fork 中间件不持 admin token

用户创建逻辑下沉到 sync worker（调 `POST /admin/users`，由 server 进程持 admin token），fork JWT 中间件仅做 JWT 验证 + `user_gitea_binding` 状态校验（非 synced 返回 503），不持有任何特权凭据，也不再调 Gitea internal `models.CreateUser`。

### 7.3 Git 操作权限管理

> 保留 Gitea 原生 git 协议接口（HTTP + SSH），用户与 AI agent 直接走 `git clone / push`；权限闭环由 Gitea 8 层原生机制 + fork 全局 pre-receive hook 实现。

#### 7.3.1 权限矩阵

| 主体 | 操作 | 需要的权限 | 实现方式 |
|---|---|---|---|
| 匿名用户 | `git clone https://gitea/costrict/<public-repo>` | 无 | Gitea 默认（public repo） |
| 认证用户（密码） | clone public | 无 | 同上 |
| 认证用户 | clone private | repo collaborator | Gitea collaborator |
| 用户（SSH key） | clone + push main / feature branch | SSH key 绑 user + repo write | Gitea SSH key（与 PAT 体验一致） |
| **用户 PAT（csc / AI agent 代用户）** | clone private repo | fine-grained PAT `read:repository` | Gitea PAT（§7.3.3） |
| **用户 PAT（csc / AI agent 代用户）** | push main / feature branch | fine-grained PAT `write:repository` | Gitea PAT（§7.3.3）；允许直接 push main（§7.3.2 已放开） |
| **`costrict-system` admin PAT** | sync worker 跨 owner 拉 private repo | admin scope | 仅 §7.2.2 列出的 2 个场景，禁止他用 |
| 用户/agent | push 到 main / master | **允许**（直接 push） | Gitea PAT + 健康度治理兜底（§11）+ 硬配额（§7.4） |
| CI（Gitea Actions） | push 后跑校验，标 commit status | 内置 `${{ secrets.GITHUB_TOKEN }}` | 非阻塞，仅信息展示 |
| csc 设备端 | （不走 git，走 HTTP 代理） | N/A | 见附录 C.2.3 |

#### 7.3.2 Branch Protection（简化：仅防历史覆写）

> **决策**：V3 是认证用户内部协作平台，依赖健康度治理（§11）+ webhook 触发 sync + security scan + 硬配额（§7.4）+ git log 完整审计兜底，**不强制 PR + review + CI 通过门**。保留 git 原生直接 push 体验，与 V2 编辑 UX 一致，对 AI agent 操作最友好。

```
protected branch: main / master
规则：
  ✗ 不允许 force push（防历史覆写）
  ✗ 不允许 delete（防误删）
  ✓ 不要求 PR（直接 push main 即可）
  ✓ 不要求 reviewer approve
  ✓ 不要求 CI status check（capability-check CI 仍跑，仅作 commit status badge，非阻塞）
  ? 管理员应急 override：临时打开 force push（如清理恶意推送），全程记录审计
```

**直接 push 模式的兜底链路**：

| 风险 | 兜底 |
|---|---|
| AI agent 误推送 | §11 健康度自动标 polluted + admin 标 archived |
| 用户 typo / 误操作 | `git revert` 30s 恢复；commit 历史保留完整 |
| 恶意推送 | commit author 归因 + Gitea audit log + admin 标 archived + 用户 PAT 撤销 |
| 大量垃圾推送 | §7.4 fork pre-receive hook 硬配额（10MB / 50MB）拦截 |
| 上游 mirror 关键文件消失 | §11 polluted 自动检测 + 卡片灰显 |

**适用前提**：
- 用户全部为 costrict-web 认证用户（无公网匿名推送）
- 团队规模 < 10 人（内部协作）
- 接受误推送后由健康度治理 + revert 兜底

**未来如需切回强制 PR 模式**：直接修改 Gitea branch protection 规则即可，无架构改动。

#### 7.3.3 用户 PAT 设计（fine-grained，D 方案 C 通道）

> 适用范围：**所有有用户上下文的 git 操作**——csc 设备端 push/clone、AI agent 代用户操作、用户自己 SSH 兜底走 HTTPS+PAT 等。统一用用户本人 PAT，不走任何 bot。

| 字段 | 推荐值 |
|---|---|
| scope | `read:repository` + `write:repository` |
| 限定 owner | `costrict` / `costrict-plugins` / `u-<self-username>`（不允许 `costrict-mirror` 写，mirror 只读） |
| 限定 repo | 不限定（owner 内全允许，权限差异由 collaborator 兜底） |
| 有效期 | 90 天 |
| 轮换 | 用户主动续期，过期自动失效 |
| 撤销 | 用户在 costrict-web `/settings/git-tokens` UI 一键 revoke（调 Gitea API） |
| 审计 | 每次 PAT 调用记录到 Gitea audit log，归因到用户本人 |
| 共享 | **明确禁止**（每用户独立 PAT，审计到个人） |
| commit author | 用户本人（PAT 与 user 绑定，无需 `Co-authored-by` trailer） |

**csc 端 PAT 获取流程**：用户在 costrict-web `/settings/git-tokens` 点"生成 PAT"→ 复制粘贴到 `~/.config/csc/git.json`（文件权限 0600）；csc 启动时校验 token 有效性，过期引导用户重新生成。

#### 7.3.4 用户鉴权方式

- **SSH key 优先**（与 GitHub 体验一致）：用户在 costrict-web `/settings/ssh-keys` 添加（前端调 Gitea user API `POST /user/keys`），强身份绑定
- **PAT 兼容**：HTTPS + PAT，方便无 SSH 环境的客户端
- **Deploy key**：仅 CI / 外部系统使用（read-only 优先），AI agent 不使用（不绑用户，审计难）

### 7.4 硬配额拦截（fork 全局 pre-receive hook）

Gitea 原生不支持全局 server-side git hook（per-repo 机制），fork 扩展加全局 pre-receive hook（~150 行），实现：

1. **单文件大小限制**：默认 10 MB（mirror owner 例外）
2. **repo 总大小配额**：按 owner 分级，**支持 per-repo 覆盖**
3. **commit message 检查**：不要求（用户决策）

**配额矩阵（owner 默认 + repo 覆盖）**：

| owner | repo（NULL = owner 默认） | max_file_size_mb | repo_quota_mb |
|---|---|---|---|
| `costrict` | NULL | 10 | 50 |
| `costrict-plugins` | NULL | 10 | 500 |
| `costrict-mirror` | NULL | 0（不限） | 2048 |
| `costrict` | `curated-seed` | 10 | 1024 |
| `costrict-config` | `platform-config` | 10 | 64 |
| `costrict` | `<特例 repo>` | 自定义 | 自定义 |

**fork hook 读配额的 cache 策略**：

- 直连共享 PostgreSQL `gitea_ext.quota_rules` 表（与 Gitea 自身 schema 隔离）
- **5min TTL cache**（key = `{owner}/{repo}` + `{owner}/-` + `default`），命中率高
- 配额变更时 sync worker 调 fork Gitea internal endpoint `POST /api/internal/quota-cache-invalidate` 主动失效
- 失败降级到自然过期（5min 后刷新）

**配额查询优先级**：repo 级覆盖 > owner 级默认 > 全局 default。

**用户被拒时的体验**：

```
remote: ERROR: File plugin-binary.exe (15.5 MB) exceeds max file size (10 MB).
remote: ERROR: Please use Git LFS for large files (git lfs install && git lfs track "*.exe").
remote:
remote: OR
remote:
remote: ERROR: Repository quota exceeded: 49.8 MB / 50 MB (after push: 65.3 MB).
remote: ERROR: Owner 'costrict' quota is 50 MB. Please remove unused files or contact admin.
! [remote rejected] main -> main (pre-receive hook declined)
```

错误消息**双语（en + zh）**，错误码标准化（`FILE_TOO_LARGE` / `QUOTA_EXCEEDED`），csc 端识别后给出 UI 提示。

### 7.5 硬配额与健康度的分工

| 场景 | §7.4 硬拦截 | §11 软治理 |
|---|---|---|
| 大文件 push | **拒绝 push**（pre-receive） | 不触发 |
| repo 超额 push | **拒绝 push**（pre-receive） | 不触发 |
| 文件结构异常（如 SKILL.md 消失） | 允许 push | 标记 polluted |
| 类型不符（首次 skill，改后变 plugin） | 允许 push | 标记 polluted |
| 安全扫描异常 | 允许 push | 标记 polluted |

两套机制不冲突，互补：硬限制防数据爆炸（量），软治理防内容问题（质）。

---

## 8. 业务线层级与 Gitea 关系（E4 简化方案）

> **决策**：Gitea 不掺业务组织概念，业务线层级仅作为 costrict-web metadata 标签（不参与发现层过滤、不参与权限控制）；Gitea 仅承担 git 协议 + repo visibility + per-user collaborator。dept-sync 服务保留真相源地位，costrict-web 仅做 webhook 接收 + Redis cache。

### 8.1 角色边界

| 系统 | 职责 |
|---|---|
| **dept-sync** | 部门树 + 成员关系真相源（HR 维护） |
| **costrict-web** | 发现层 API 全量返回（不做 dept 过滤）；内容访问层按 Gitea permission API 校验；private repo admin 邀请 UI；离职自动化清理 |
| **Gitea** | 4 个固定 org（`costrict-config` 配置中心 / `costrict` 官方能力项 / `costrict-plugins` pack / `costrict-mirror` 镜像）+ 用户个人 namespace（`u-<username>/`，每认证用户 1 个）；仅 public/private visibility；per-user collaborator；**不维护 team 业务概念** |

### 8.2 数据流

```
[dept-sync]（真相源）
   ├─ webhook 推送 dept.updated / dept.member_changed 事件
   ↓
[costrict-web]
   ├─ DeptSyncCacheWorker：接收 webhook → 写 Redis（5min TTL）
   ├─ 发现层 API：全量返回 capability_items（带 visibility + business_line 标签）
   ├─ 内容访问层：调 Gitea permission API 校验当前 user 对该 repo 的 read 权限
   ├─ private repo admin 邀请 UI（/admin/private-repos）
   └─ 离职清理 worker：监听 user.disabled/deleted → 调 Gitea admin API 移除 collaborator + 禁用账号
   ↓（不调 Gitea team API）
[Gitea]
   ├─ 4 个 owner，无业务 team
   ├─ private repo collaborator（admin 手动 + 离职自动清理）
   └─ AI agent PAT scope 仅按 owner 限定
```

### 8.3 关键设计点

| 维度 | 设计 |
|---|---|
| dept-sync 改造 | 加 webhook 推送接口（dept.updated / dept.member_changed）+ costrict-web cache 写入 |
| sync worker | **零 Gitea API 调用**（不调 team / collaborator API） |
| 业务线 → capability 关系 | costrict-web `capability_items.business_line` 字段（已有），仅作为 metadata 标签（前端筛选 / 统计），**不参与权限控制** |
| private capability 鉴权 | A2 链路不变：csc → costrict-web → Gitea permission API（per-user collaborator）反查 |
| private repo 成员管理 | admin 手动邀请（costrict-web `/admin/private-repos` 页面调 Gitea collaborator API） |
| 离职清理 | user.disabled/deleted webhook → sync worker → `DELETE /admin/users/{username}/repos` + 禁用 Gitea 账号 |
| 假设条件 | private capability 占比 < 10%（典型场景），admin 手动邀请成本可接受 |

### 8.4 优势

| 维度 | 收益 |
|---|---|
| **真正简化** | sync worker 零 Gitea API 调用；Gitea admin token 仅用于 user 改名 + 离职清理 |
| **Gitea 纯粹** | 4 个固定 org（配置中心 / 官方能力项 / 插件 / 镜像）+ 用户 namespace（草稿区），不掺业务概念，admin UI 干净 |
| **跨 Git 服务解耦** | 未来换 GitLab/Bitbucket 业务逻辑不变 |
| **审计单一视图** | 业务线层级只在 costrict-web，不跨系统 |
| **dept-sync 改造最小** | 仅 webhook 推送 + costrict-web cache |

### 8.5 适用边界

- ✅ 适用：80%+ capability 是 public；private 是少数（secret skill / 内部工具）
- ❌ 不适用：private 占比 > 30%（admin 邀请成本爆炸）；需自动按业务线授权（如所有 backend 自动可见某 repo）—— 此时回退到方案 3（dept → Gitea team sync）

未来如 private 占比上升，可平滑切换到方案 3（保留 dept-sync webhook 接口，扩展 sync worker 加 Gitea team API 调用即可，无架构推翻）。

---

# Part IV：运行时流程（Dynamic View）

## 9. AI 操作工作流

### 9.1 默认通道：直推 main（V3 简化方案）

```
AI agent 任务："加一个 skill-vetter"
   │
   ├─ GET /api/capabilities?q=vetter  (查 server REST API，确认 slug 未占用)
   │
   ├─ git clone https://gitea.costrict.local/u-<username>/skill-vetter.git  (用户 namespace 下新建 repo)
   │
   ├─ 写 skill.md（含 frontmatter）
   ├─ git add && git commit -m "feat(skill): add skill-vetter"
   ├─ git push origin main     # 用 scoped PAT（§7.3.3）
   │
   ├─ 主分支 push 触发 webhook
   │  ├─ server sync → capability_items 更新
   │  ├─ capability-check worker → 启发式识别 + schema 校验 → 写 health_issues
   │  └─ SecurityScan 异步触发
   │
   └─ 后续发现走 server REST API，不需要 Git clone 索引
```

**适用场景**：用户自建 standalone（`u-<username>/<slug>`）/ 用户业务线 pack（`costrict-plugins/<pack>` owner 已授权）/ 官方 org 内迭代（`costrict/<slug>` collaborator）

### 9.2 可选通道：PR 流程

```
AI agent 任务："把我的 skill-vetter 贡献到官方 org"
   │
   ├─ fork https://gitea.costrict.local/costrict/skill-vetter.git → u-<username>/skill-vetter
   ├─ git checkout -b feat/improve-vetting
   ├─ 改 skill.md → commit → git push origin feat/improve-vetting
   ├─ POST /api/v1/repos/costrict/skill-vetter/pulls  (创建 PR)
   │
   ├─ PR webhook 触发 capability-check worker：
   │  ├─ 拉文件树 → 启发式识别 + schema 校验
   │  ├─ 写 health_issues（PR head SHA 维度）
   │  └─ 调 Gitea API 在 PR 评论 health + security summary
   │
   ├─ admin 审核 → merge → push webhook（post-merge）
   │  └─ server sync → capability_items 更新 + SecurityScan 覆盖主表
```

**适用场景**：
- 用户 namespace `u-<username>/` 下的 repo 贡献到 `costrict/<slug>` 官方 org
- `costrict/curated-seed` 精选集维护
- 跨业务线重要变更评审
- mirror repo 的 fork 改进（mirror 本身 read-only，必须 PR）

**两条通道的 check worker 行为**：

| 触发事件 | capability-check worker 动作 |
|---|---|
| `push`（直推 main） | 拉 commit 文件树 → 写 `health_issues` 主表（按 commit SHA 维度）+ 标 commit status badge |
| `pull_request` opened/synchronize | 拉 PR head 文件树 → 写 `health_issues`（PR head SHA 维度，临时）+ **PR 评论** summary（不阻断 merge） |
| `pull_request` closed/merged | sync worker 接管 → 用 merge commit SHA 覆盖 `capability_items.health_issues` 主表 |

### 9.3 AI 友好性的来源

1. **文件 > API**：AI 天然擅长读写文件，不需要学 OAS schema
2. **直推 main 即默认体验**：AI 直接 push 主分支即可上线能力项，与 V2 编辑 UX 一致
3. **可选 PR 即审核**：AI 起草 + 人类审核是"公有能力贡献 / 重要变更评审"的可选通道
4. **diff 即审查证据**：每个变更都是可追溯的 commit
5. **失败可回滚**：`git revert` 即下线能力项
6. **fork 即实验**：AI 在自己 fork 里随便改，PR 上来才入主流程
7. **统一发现 API**：AI 调 `GET /api/capabilities` 即可发现能力项，不需要遍历所有 repo

---

## 10. 同步链路：Gitea API 驱动（无 git CLI）

### 10.1 设计原则

**server 进程永不 exec `git` 命令**。所有"Git 操作"都通过 Gitea REST API 完成。理由：

| 问题 | exec git 的代价 | API 方案的收益 |
|---|---|---|
| 运行时二进制依赖 | server 镜像要装 git、SSH client | scratch 镜像即可 |
| 工作目录管理 | 每 repo 要维护本地工作副本 + 文件锁 | **完全无状态**，无工作目录 |
| 凭据管理 | deploy token / SSH key 分发到每个副本 | PAT 环境变量，scope 受限 |
| 多副本一致性 | 共享 PV 或独立副本都要协调 | HTTP 调用天然支持并发 |
| 错误模式 | 网络抖动、磁盘满、ref 不存在，多种 | HTTP 状态码 + 标准重试 |
| 安全 | shell 注入、token 泄漏面 | 纯 HTTP，无 shell |

参考：ArgoCD 等成熟 GitOps 系统也只在 controller 层用 git CLI，业务面全是 API。

### 10.2 Gitea API 调用清单

| 用途 | Gitea API |
|---|---|
| 拿两次 commit 之间的文件清单（added/modified/removed） | `GET /api/v1/repos/{owner}/{repo}/compare/{before}...{after}` |
| 拉单个文件内容（指定 ref/SHA） | `GET /api/v1/repos/{owner}/{repo}/raw/{filepath}?ref={sha}` |
| 一次性拉整棵文件树（mirror 初始同步） | `GET /api/v1/repos/{owner}/{repo}/git/trees/{sha}?recursive=true&per_page=100` |
| 拿某个 commit 的元数据（author/committer/message） | `GET /api/v1/repos/{owner}/{repo}/git/commits/{sha}` |
| 列出文件历史 commit（替代 CapabilityVersion） | `GET /api/v1/repos/{owner}/{repo}/commits?path={filepath}` |
| 列出 / 创建 PR（AI 流程可选） | `GET/POST /api/v1/repos/{owner}/{repo}/pulls` |
| 验证 token 与权限 | `GET /api/v1/user` |

### 10.3 内容 repo 的 webhook 流程

```
AI / 用户 / 上游 mirror  ──push──►  内容 repo
                                       │
                                       │ POST webhook (push event)
                                       ▼
                             costrict-web server（/api/internal/git-sync）
                                       │
                                       ├─ 校验 webhook 签名（HMAC + shared secret）
                                       ├─ 幂等检查：commit SHA 是否已处理（Redis SETNX）
                                       ├─ GET /repos/{owner}/{repo}/compare/{before}...{after}
                                       │    → 拿到 added/modified/removed 文件路径列表
                                       ├─ 路径过滤：只保留**能力项顶层 metadata 文件**
                                       │    - standalone/mirror/seed：skill.md / subagent.md / command.md / mcp.md
                                       │    - pack：plugins/<id>/.plugin.json
                                       │    子目录变更（plugin 内 skill/command 文件、assets 等）直接忽略
                                       ├─ 对每个变更 metadata 文件（通常 1-3 个）：
                                       │    GET /repos/{owner}/{repo}/raw/{path}?ref={after_sha}
                                       │    → 返回 metadata 原文
                                       ├─ 解析 frontmatter（或 .plugin.json）
                                       ├─ upsert capability_items（一行 metadata 对应一行 DB）
                                       │    ├─ 新增：insert，记录 source_repo_url/path/sha
                                       │    ├─ 修改：update content + bump version
                                       │    └─ 删除：标记 status='archived'
                                       ├─ 同步 visibility：调 Gitea GET /repos/{owner}/{repo}
                                       │    读 is_private → 写 capability_items.visibility（§4.7.2）
                                       ├─ 异步触发 SecurityScan（已有）
                                       └─ 更新 registry.last_synced_commit
```

**全程 0 次 exec git 命令**。server 容器只依赖标准库 HTTP client + Gitea PAT。

> **关键简化**：由于能力项粒度 = 顶层 metadata 文件，单次 push 即使改了大量子文件（plugin 内重写、批量改 skill），server 也只拉顶层 metadata。子文件改动对 DB 索引透明，但 commit SHA 仍会更新（用于版本追踪）。

### 10.4 幂等与一致性

- 每个 registry 维护 `last_synced_commit` 字段（DB）
- webhook 收到后用 commit SHA 去重（Redis `SETNX` + 24h TTL）；同 SHA 重复推送直接返回 200
- webhook 失败时 Gitea 自动重试（默认 5 次），server 必须幂等
- 增量 diff 通过 Gitea `compare` API 拿到，不在 server 内做 git log
- webhook 风暴防护：同 repo 短时多次 push → Redis 队列 debounce 1-2 秒，取最新 commit

### 10.5 Mirror repo 大批量初始同步

新接入一个 mirror 仓库时可能含上千文件，但 server **只关心顶层 metadata 文件**：

```
1. 收到首次 push webhook（before = 0000...）
2. GET /repos/.../git/trees/{after_sha}?recursive=true&per_page=100
   分页拿到完整文件列表，**只筛选**：
   - standalone 上游：根目录 / 顶层 skill.md|subagent.md|command.md|mcp.md
   - pack 上游：所有 plugins/*/.plugin.json
3. 对筛选后的 metadata 文件（通常 <50 个）并发调用 raw API
4. 解析 → upsert DB
```

避免 clone 整个 repo（节省带宽和磁盘），server 仍无本地副本。子目录文件一次性跳过，不入索引。

### 10.6 上游 catalog repo 接入

- **方式 A（推荐）：Gitea mirror pull** —— Gitea 自带 mirror 同步功能，定期 pull 上游到 `costrict-mirror/...`，由 Gitea 自己执行 git，server 完全不参与
- **方式 B：应急 fallback**（迁移期保留）—— 旧 `migrate ingest-upstream` 命令 + `services.CatalogIngestService` 代码不删，但**冻结**（admin 不主动跑）；仅当 V3 sync worker 大规模故障（如 webhook 链路挂掉数小时）时由 admin 手动触发补数据
- **方式 C：完全废弃**（V3 稳定后）

所有内容都先落到 Gitea repo，再由 webhook 同步到 DB。server 永远不直接拉 GitHub。

### 10.7 工程实现要点

| 维度 | 实现 |
|---|---|
| HTTP client | 标准库 `net/http`，连接池复用 |
| 超时 | 单文件拉取 30s；compare API 60s |
| 重试 | 5xx 与网络错误指数退避（max 3 次） |
| 限流 | per-repo 并发拉取 ≤ 20；全局并发 ≤ 200 |
| 鉴权 | PAT 走 `Authorization: token <PAT>` 头 |
| 可观测 | Prometheus 指标：`gitea_api_latency_seconds`、`gitea_api_errors_total`、`webhook_processed_total` |
| 日志 | 每次调用 trace ID 关联 webhook 事件 ID |
| 镜像 | server 镜像基于 scratch + Go binary，无 git、无 ssh、无 shell |

---

## 11. 能力项健康度与污染治理

采用**软治理 + 状态透传**模式：不阻断 PR merge，靠健康度状态多端透传让消费端自主决策。

### 11.1 4 级健康度状态

| 状态 | 含义 | 触发条件 | 用户可见性 |
|---|---|---|---|
| `clean` | 健康 | 识别通过 + 结构稳定 + schema 通过 | 正常显示 |
| `warning` | 轻度告警 | 引入疑似其他类型文件、frontmatter 缺字段、命名不规范 | 徽章 + tooltip，仍可用 |
| `polluted` | 污染 | 关键文件被删/改名、schema 错误、识别置信度暴跌 | 徽章 + 默认折叠/灰显，需二次确认 |
| `unknown` | 待识别 | 首次识别失败 | 不进发现层（API 过滤） |

### 11.2 触发规则矩阵

| 检测项 | 严重度 | 触发时机 |
|---|---|---|
| 首次识别置信度高 + 结构合法 | → `clean` | sync worker 首次同步 |
| 首次识别置信度低 | → `unknown` | sync worker 首次同步 |
| 引入疑似其他类型文件（如 skill repo 出现 `plugin.json`） | → `warning` | check hook |
| frontmatter 缺字段（license/description 等） | → `warning` | check hook |
| 关键 metadata 文件被删除/重命名 | → `polluted` | check hook + sync worker |
| frontmatter schema 错误（必填缺失、类型错） | → `polluted` | check hook |
| mirror upstream 关键文件消失 | → `polluted` | mirror pull 后 sync worker 检测 |
| 命名/slug 不规范 | → `warning` | check hook |

### 11.3 多端透传

| 触点 | 行为 |
|---|---|
| 发现层 API `GET /api/capabilities` | 默认仅返回 `clean` + `warning`；`polluted` 需显式 `?include=polluted`；`unknown` 永不返回 |
| marketplace 卡片 | clean: 正常；warning: 黄色徽章；polluted: 灰显 + "查看问题"按钮 |
| 详情页 | 顶部 banner 显示状态 + issues 列表 + 引入 commit 链接 |
| clone 操作 | 显示状态；polluted 需勾选"我已了解风险" |
| 能力下发 manifest | 内嵌 `health` 字段，消费端决策 |
| AI agent 自动选择 | 默认仅选 `clean` + `warning`；`polluted` 所有 agent 可 override（决策权下放） |

### 11.4 Manifest 字段示例

```json
{
  "capability_id": "vetter",
  "type": "skill",
  "content_url": "https://gitea/.../raw/skill.md",
  "health": {
    "status": "polluted",
    "allow_override": true,
    "last_checked_at": "2026-07-07T10:00:00Z",
    "issues": [
      {
        "code": "MISSING_FILE",
        "severity": "polluted",
        "message": "skill.md was deleted in commit abc123",
        "introduced_commit": "abc123"
      }
    ]
  }
}
```

### 11.5 消费端默认策略（可配置）

| 消费端 | `clean` | `warning` | `polluted` | `unknown` |
|---|---|---|---|---|
| runtime（device） | 自动接受 | 接受 + 日志 | 接受 + 显著告警 | 拒绝 |
| AI agent | 可选 | 可选 | 可选（override 全开放） | 拒绝 |
| costrict-web portal | 全部显示 | 全部显示 | 全部显示 | 后台可见 |

### 11.6 Check Hook 实现（不阻断，全 server 端）

- 平台：**server 端 capability-check worker**，由 Gitea system webhook 触发（`models/webhook/webhook_system.go` 原生支持）
- 触发事件：`pull_request`（opened/synchronize/reopened）+ `push`（post-merge）+ mirror pull 引发的 push
- worker 流程：调 Gitea API 拉 PR head / commit 文件树 → 启发式识别（§4.6 规则）+ schema 校验 → 写入 `capability_items.health_issues` + `identification_status` → 调 Gitea API 在 PR 评论 health summary
- **不阻断 merge**：不依赖 Gitea Actions required status check，PR 即使有问题也能 merge
- **零 Gitea Actions 依赖**：不需要 `act_runner`、不需要 workflow 文件、不需要 `options/actions/` 全局模板——所有逻辑收敛在 server

### 11.7 与 sync worker 的关系

- sync worker（post-merge webhook 触发）：拉能力项内容 → 写 `capability_items` 主表数据
- capability-check worker（PR webhook 触发）：拉文件结构 → 写 `health_issues` + `identification_status`
- 两者共享 Gitea API client 与启发式识别规则模块，但是独立 worker，独立触发

### 11.8 告警与通知

- `clean → polluted`：通知 owner + 业务线 editors + watchers（站内 + email）
- `clean → warning`：仅通知 owner + watchers
- polluted 持续 30 天未处理：暂不实施 `deprecated` 自动升级（out of scope）
- owner 申诉机制（`acknowledged` 标记）：暂不实施（out of scope）

---

## 12. 安全扫描迁移

现有 LLM 扫描机制（`server/internal/services/scan_service.go:235-241` + `scan_job_service.go:36-94` + `worker/scan_worker.go:16-132`）整体保留，迁移以"切触发源 + 加 PR 入口"为主。

### 12.1 保留不变

- LLM 扫描逻辑（callLLM + 固定 prompt + 6000 runes 截断）
- scan_jobs 表 + 30s worker pool 轮询
- Plugin 跳过逻辑（`scan_service.go:252-254`）
- 短路机制（`SECURITY_SCAN_SHORT_CIRCUIT_DISABLED` 环境变量保留语义）

### 12.2 触发源迁移

| 触发 | 当前入口 | 迁移后入口 |
|---|---|---|
| sync 触发 | `sync_service.go:421,491` Enqueue | 新 sync worker（监听 Gitea webhook）Enqueue |
| catalog_ingest 触发 | `catalog_ingest_service.go:1024,1130` | 废弃（catalog_ingest 整体下线） |
| API 手动触发 | `POST /items/{id}/scan` | 保留 |
| **PR 触发（新增）** | — | capability-check worker 在 PR webhook 时调 Enqueue |

### 12.3 短路键迁移

- 当前：`item_id + CurrentRevision`（自增版本号）
- 迁移：`item_id + git_sha`（40 字符 commit SHA）
- 兼容期：`security_scans` 表双写 `item_revision` + `git_sha`，短路优先用 `git_sha`
- 迁移完成：删 `item_revision`（见 §15 实施路径）

### 12.4 PR 扫描结果存储

- 写入 `security_scans` 表，标记 `trigger_type=pr-check` + `pr_number`
- **不覆盖**主表 `capability_items.security_status`（PR 未 merge 时主表保持上一次正式 sync 的状态）
- PR 评论展示完整结果：`risk_level` + `verdict` + `red_flags` + `permissions`
- 评论折叠规则：`clean` / `low` 默认展开；`medium` / `high` / `extreme` 默认折叠（点击展开）

### 12.5 post-merge 流程

- PR merge → Gitea push webhook → sync worker → 写 `capability_items` 主表（含 `git_sha`）→ Enqueue(`trigger_type=git-push`) → LLM 扫描 → 覆盖 `capability_items.security_status` + `last_scan_id`

### 12.6 与 `identification_status` 的关系

- `identification_status`（§11）：文件结构健康度，capability-check worker 产出
- `security_status`：内容安全风险，ScanWorkerPool 产出
- 两者**并存独立**，manifest 双字段透传：

```json
{
  "health": {
    "identification": "clean",
    "security": "high",
    "issues": [...],
    "red_flags": [...]
  }
}
```

### 12.7 scan_service 不拆分

暂不抽出 `LLMScanService` 与未来静态分析/依赖扫描解耦——保留现状，未来如有第二种扫描引擎再重构。

---

## 13. 动态配置中心

> 所有 Gitea 动态配置（branch protection / 配额 / teams / webhooks / labels 等）通过 `costrict-config/platform-config` repo 的 `.gitea/*.yaml` 声明，PR merge 后由 costrict-web `GiteaConfigSyncWorker` 自动应用到所有 repo。与 §3.2 "Git → DB 单向数据流"原则一致。

### 13.1 配置类型矩阵

| 配置类型 | 文件路径 | sync 方式 |
|---|---|---|
| **Branch Protection** | `.gitea/branch-protection.yaml` | sync worker → 调 Gitea API `POST /repos/{owner}/{repo}/branch_protections` |
| **配额矩阵** | `.gitea/quota.yaml` | sync worker → 写 `gitea_ext.quota_rules` 表 → fork pre-receive hook 读 |
| **默认 Teams** | `.gitea/teams.yaml` | sync worker → 调 Gitea team API |
| **系统级 Webhook** | `.gitea/webhooks.yaml` | sync worker → 调 Gitea admin webhook API |
| **PR/Issue Labels** | `.gitea/labels.yaml` | sync worker → 调 Gitea label API（按 owner 批量应用） |
| **PR/Issue 模板** | `.gitea/ISSUE_TEMPLATE/*.md`, `.gitea/PULL_REQUEST_TEMPLATE.md` | Gitea 原生约定路径（自动生效，无需 sync） |
| **CODEOWNERS** | `CODEOWNERS`（repo 根） | Gitea 原生（per-repo，不全局） |

### 13.2 配置中心架构

```
┌─────────────────────────────────────────────────────┐
│  costrict-config/platform-config repo (git 真相源)  │
│  ├─ .gitea/branch-protection.yaml                   │
│  ├─ .gitea/quota.yaml                               │
│  ├─ .gitea/teams.yaml                               │
│  ├─ .gitea/webhooks.yaml                            │
│  ├─ .gitea/labels.yaml                              │
│  ├─ .gitea/ISSUE_TEMPLATE/*.md                      │
│  └─ README.md（说明每个 yaml 的 schema）            │
└────────────────┬────────────────────────────────────┘
                 │ push webhook
                 ▼
┌─────────────────────────────────────────────────────┐
│  costrict-web GiteaConfigSyncWorker                 │
│  ├─ diff 出哪些 yaml 变更                           │
│  ├─ 解析 yaml → 调 Gitea API 应用（branch / teams / │
│ │  webhooks / labels）                              │
│  └─ 写入 costrict-web DB gitea_ext.quota_rules     │
│     （fork hook 读）                                │
│     + 调 fork Gitea 内部失效 cache endpoint        │
└────────────────┬────────────────────────────────────┘
                 │
        ┌────────┴────────┐
        ▼                 ▼
┌──────────────┐  ┌───────────────────────────────┐
│   Gitea      │  │ fork Gitea pre-receive hook   │
│ (per-repo    │  │ 读 gitea_ext.quota_rules      │
│  state)      │  │ (5min cache + 主动失效)       │
└──────────────┘  └───────────────────────────────┘
```

### 13.3 quota.yaml 示例

```yaml
# .gitea/quota.yaml
rules:
  # owner 级默认
  - owner: costrict                          # standalone skill/subagent/command/mcp
    max_file_size_mb: 10
    repo_quota_mb: 50
    allow_override: false

  - owner: costrict-plugins                  # plugin pack
    max_file_size_mb: 10
    repo_quota_mb: 500
    allow_override: false

  - owner: costrict-mirror                   # mirror repo
    max_file_size_mb: 0                      # 0 = 不限制（mirror 不可控）
    repo_quota_mb: 2048
    allow_override: false

  # per-repo 覆盖（特例）
  - owner: costrict
    repo: curated-seed                      # seed mono-repo
    max_file_size_mb: 10
    repo_quota_mb: 1024

  - owner: costrict-config
    repo: platform-config                   # GitOps 真相源（仅 yaml + templates，体量小）
    max_file_size_mb: 10
    repo_quota_mb: 64

  - owner: costrict
    repo: filesystem-mcp                     # 含较多 assets 的特例
    max_file_size_mb: 50
    repo_quota_mb: 200

# 全局默认（owner 不在 rules 中时使用）
default:
  max_file_size_mb: 10
  repo_quota_mb: 50
  allow_override: false
```

### 13.4 branch-protection.yaml 示例（v3 简化方案）

```yaml
# .gitea/branch-protection.yaml
# v3 简化：仅保留禁 force push + 禁 delete main；
# 不要求 PR / reviewer approve / CI 必过（详见 §7.3.2 决策）
default_rules:
  - pattern: main
    required_approvals: 0                # 不要求 reviewer approve
    block_force_push: true               # 防历史覆写（保留）
    block_delete: true                   # 防误删（保留）
    required_status_checks: []           # 不要求 CI 必过；capability-check CI 仍跑，仅作 commit status badge，非阻塞
    enable_push: true                    # 允许直接 push main（默认）

overrides:
  - repo: costrict-config/platform-config
    rules:
      - pattern: main
        # platform-config 是平台 GitOps 真相源，保留稍严的约束
        required_approvals: 0
        block_force_push: true
        block_delete: true
        enable_push: true                # 允许 admin 直接 push（默认通道）
        allow_force_push: false          # 应急时由 admin 临时打开（绕过此规则，记录审计）
```

### 13.5 fork Gitea 内部 endpoint

fork Gitea 新增（仅在内部网络访问，监听 127.0.0.1）：

| Endpoint | 用途 |
|---|---|
| `POST /api/internal/quota-cache-invalidate` | costrict-web sync worker 写完 `gitea_ext.quota_rules` 后调用，使 fork hook 的 5min cache 立即失效 |
| `GET /api/internal/healthz` | costrict-web 监控 fork Gitea 健康 |

### 13.6 优势汇总

| 维度 | 收益 |
|---|---|
| **统一治理** | 所有 Gitea 动态配置走 git 工作流（PR + review） |
| **版本化** | 配置变更可追溯（git log / blame） |
| **批量更新** | 改一个 yaml，sync worker 自动应用到所有 repo |
| **降低 admin UI 成本** | 不用给每类配置做 UI |
| **per-repo 灵活覆盖** | owner 默认 + repo 特例，平衡统一与灵活 |
| **fork hook cache** | 5min TTL + 主动失效，避免每次 push 触发 DB 查询 |
| **可扩展** | 未来加新配置类型只需加 yaml + sync worker handler |

---

# Part V：数据与实施

## 14. 数据模型变更

### 14.1 `capability_items` 表

新增字段：

```sql
ALTER TABLE capability_items
  ADD COLUMN source_repo_url        VARCHAR(512),   -- 仓库 URL
  ADD COLUMN source_repo_path       VARCHAR(512),   -- 能力项顶层 metadata 文件的 repo-relative path（standalone: skill.md; pack: plugins/<id>/.plugin.json）
  ADD COLUMN source_repo_ref        VARCHAR(64) DEFAULT 'main',
  ADD COLUMN source_repo_kind       VARCHAR(32),    -- standalone | pack | mirror | seed
  ADD COLUMN capability_type        VARCHAR(32),    -- skill | subagent | command | mcp | plugin
  ADD COLUMN git_sha                VARCHAR(40),    -- 顶层 metadata 文件所在 commit 的 SHA
  ADD COLUMN git_last_synced_at     TIMESTAMPTZ,
  ADD COLUMN git_author_email       VARCHAR(255),   -- commit author email
  ADD COLUMN mirror_of              VARCHAR(512),   -- 仅 mirror kind：上游 URL
  ADD COLUMN identification_status  VARCHAR(32) DEFAULT 'unknown',  -- clean | warning | polluted | unknown
  ADD COLUMN health_issues          JSONB,          -- 健康度问题数组（code/severity/message/introduced_commit/detected_at）
  ADD COLUMN last_checked_at        TIMESTAMPTZ,    -- 最后一次健康度检测时间
  ADD COLUMN last_clean_at          TIMESTAMPTZ,    -- 最后一次 clean 状态时间
  ADD COLUMN visibility             VARCHAR(16) NOT NULL DEFAULT 'public';  -- public | private，sync worker 从 Gitea repo is_private 同步（§4.7）
```

> **粒度约束**：`source_repo_url + source_repo_path` 唯一对应一个能力项。pack 模式下，同一 repo 不同 `plugins/<id>/.plugin.json` 是不同行；plugin 内部子文件不产生行。
>
> **类型锁定**：`capability_type` 在首次 sync 时根据文件结构识别后写入，后续 sync 不再重新推断。混合 repo（同时含多类型文件）合法——同一 `source_repo_url` 下多行，每行一个 `capability_type`。

废弃字段（保留兼容期后删除）：

- `SourceSHA` → 由 `git_sha` 替代
- `CatalogEntryDir` → 由 `source_repo_path` 替代
- `CurrentRevision` → 由 git tag/commit 替代
- `SourceType` → 由 `source_repo_kind` 替代
- `Source`（free text） → 由 `source_repo_url` 替代

### 14.2 `capability_registries` 表

```sql
ALTER TABLE capability_registries
  ADD COLUMN kind                 VARCHAR(32),    -- standalone | pack | mirror | seed
  ADD COLUMN git_remote_url       VARCHAR(512),
  ADD COLUMN git_default_branch   VARCHAR(64) DEFAULT 'main',
  ADD COLUMN last_synced_commit   VARCHAR(40),
  ADD COLUMN last_synced_at       TIMESTAMPTZ,
  ADD COLUMN mirror_of            VARCHAR(512),   -- mirror kind 专用
  ADD COLUMN gitea_visibility     VARCHAR(16);    -- public | private，registry 级聚合用（§4.7）
```

### 14.3 索引调整

新增：

```sql
CREATE INDEX idx_capability_items_repo
  ON capability_items (source_repo_url, source_repo_path);

CREATE INDEX idx_capability_items_kind
  ON capability_items (source_repo_kind);
```

### 14.4 用户中心与 Gitea 同步相关新表

#### `user_gitea_binding`（用户与 Gitea 账号绑定）

```sql
CREATE TABLE user_gitea_binding (
    user_id          UUID PRIMARY KEY REFERENCES users(id),
    gitea_uid        INT       UNIQUE,    -- Gitea user.id；pending 期间为 NULL，sync worker 调 POST /admin/users 成功后回填
    gitea_username   VARCHAR(40) NOT NULL,    -- 对齐 Gitea MaxUserNameLength=40；格式 `u-<costrict-username>`         -- Gitea user.name（可变，username 改名后更新）
    sync_status      VARCHAR(32) NOT NULL DEFAULT 'pending',  -- pending | synced | error
    last_synced_at   TIMESTAMPTZ,
    last_error       TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_user_gitea_binding_gitea_username
  ON user_gitea_binding (gitea_username);
```

#### `user_profile`（业务字段，与 Gitea user 表解耦）

承载 Gitea user 模型无法表达的业务字段（业务线 / 部门 / 自定义角色 / 偏好 / 配额）。**Gitea 只存 identity（username/email/密码），所有业务字段都在 costrict-web**。

```sql
CREATE TABLE user_profile (
    user_id          UUID PRIMARY KEY REFERENCES users(id),
    business_line_id UUID REFERENCES dept_tree(id),
    dept_id          UUID REFERENCES dept_tree(id),
    role             VARCHAR(64),                  -- 业务角色（如 audit / approver）
    preferences      JSONB,                        -- UI 主题 / 通知设置 / 语言等
    quota            JSONB,                        -- capability_items 配额 / repo 创建配额
    hired_at         DATE,
    left_at          DATE,
    status           VARCHAR(32) NOT NULL DEFAULT 'active',  -- active | suspended | left
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

#### `webhook_subscriptions`（通用 webhook 订阅表，承载 §6.5 多目标广播）

```sql
CREATE TABLE webhook_subscriptions (
    id              UUID PRIMARY KEY,
    subscriber_name VARCHAR(64) NOT NULL,          -- "gitea-sync" / "cs-cloud" / "csc-notify" / ...
    target_url      TEXT NOT NULL,
    event_types     TEXT[] NOT NULL,               -- {"user.updated","user.disabled","user.deleted", ...}
    secret          TEXT NOT NULL,                 -- HMAC 密钥（加密存储）
    active          BOOLEAN NOT NULL DEFAULT true,
    retry_policy    JSONB,                         -- 重试覆盖（默认 6 次指数退避）
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_webhook_subscriptions_event
  ON webhook_subscriptions USING GIN (event_types) WHERE active = true;
```

#### `webhook_deliveries`（事件投递记录与死信队列）

```sql
CREATE TABLE webhook_deliveries (
    id              UUID PRIMARY KEY,
    event_id        UUID NOT NULL,                 -- 同一事件多次重试共享 event_id
    event_type      VARCHAR(64) NOT NULL,
    subscription_id UUID NOT NULL REFERENCES webhook_subscriptions(id),
    payload         JSONB NOT NULL,
    status          VARCHAR(32) NOT NULL,          -- pending | delivered | failed | dead_letter
    attempt_count   INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ,
    last_error      TEXT,
    delivered_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_webhook_deliveries_pending
  ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX idx_webhook_deliveries_dead_letter
  ON webhook_deliveries (created_at) WHERE status = 'dead_letter';
```

#### `gitea_admin_audit_log`（Gitea admin API 调用审计）

```sql
CREATE TABLE gitea_admin_audit_log (
    id              BIGSERIAL PRIMARY KEY,
    actor_user_id   UUID NOT NULL,                 -- 触发 admin API 调用的 costrict-web user
    endpoint        TEXT NOT NULL,                 -- 如 "PATCH /admin/users/alice"
    payload_hash    VARCHAR(64) NOT NULL,          -- SHA256 of payload（不存原文，避免泄漏）
    request_id      UUID,
    response_status INT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_gitea_admin_audit_log_actor
  ON gitea_admin_audit_log (actor_user_id, created_at DESC);
```

#### `gitea_ext.quota_rules`（独立 schema：fork Gitea pre-receive hook 读取）

> **schema 隔离**：`gitea_ext` schema 由 costrict-web 维护，fork Gitea 只读，避免污染 Gitea 自身 schema。

```sql
CREATE SCHEMA IF NOT EXISTS gitea_ext;

CREATE TABLE gitea_ext.quota_rules (
    owner              VARCHAR(64) NOT NULL,        -- costrict / costrict-plugins / costrict-mirror
    repo               VARCHAR(64),                 -- NULL = owner 级默认；非 NULL = per-repo 覆盖
    max_file_size_mb   INT NOT NULL,                -- 0 = 不限制
    repo_quota_mb      INT NOT NULL,
    allow_override     BOOLEAN NOT NULL DEFAULT false,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner, repo)                       -- (owner, NULL) 是 owner 默认；(owner, repo) 是 repo 覆盖
);
```

**fork Gitea pre-receive hook 读取规则**：
- 启动时建 PostgreSQL connection pool（指向共享 DB）
- hook 触发时按优先级查询：`{owner}/{repo}` → `{owner}/-` → 全局 default
- **5min TTL cache**（key = `{owner}/{repo}`），命中率高
- 配额变更时 costrict-web `GiteaConfigSyncWorker` 调 fork Gitea 内部 endpoint `POST /api/internal/quota-cache-invalidate` 主动失效
- 失败降级到自然过期（5min 后刷新）

#### `gitea_ext.config_versions`（配置版本号，可选优化）

> 用于未来扩展：fork Gitea 周期性（30s）查 `MAX(version)` 决定是否刷 cache，进一步降低耦合。

```sql
CREATE TABLE gitea_ext.config_versions (
    config_type      VARCHAR(64) PRIMARY KEY,       -- "quota_rules" / "branch_protection" / ...
    version          BIGINT NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

---

## 15. 实施路径（瘦身版）

> 详细分阶段任务、依赖关系、里程碑见独立文档 [`CAPABILITY_GIT_REGISTRY_ROADMAP.md`](./CAPABILITY_GIT_REGISTRY_ROADMAP.md)。本节仅给出阶段摘要与关键里程碑。

### 15.1 阶段摘要

| 阶段 | 名称 | 周期 | 关键产出 |
|---|---|---|---|
| Stage 0 | fork Gitea + JWT 中间件 + 全局 hook + 用户中心 | 3-4 周 | fork Gitea 镜像灰度可用 + costrict-web 自签 JWT + Casdoor 退化 |
| Stage 1 | 基础设施 | 1 周 | 4 org 创建 + system webhook 注册 + dept-sync 集成 |
| Stage 2 | 仓库分类与数据导出 | 2 周 | 现有 capability_items 全量迁到 V3 repo + V2 通道冻结 |
| Stage 3 | webhook 接通与双写灰度 | 2-3 周 | sync worker + capability-check worker 上线 + 安全扫描迁移 |
| Stage 4 | mirror 接入自动化 | 1 周 | 24h mirror pull + 上游删除检测 |
| Stage 5 | 下线旧通道与清理 | 1 周 | CatalogIngestService 删除 + 冗余字段删除 |

### 15.2 关键启动条件

- Stage 5 启动条件：V3 sync worker 稳定运行 ≥ 2 周无 P0 故障；期间未触发过 V2 应急 fallback（即 V3 自身足够可靠）

### 15.3 关键里程碑

详见 ROADMAP §M0–M7。关键路径：决策闭环 → fork baseline → 用户中心切换 → 配置中心 → 数据迁移 → V3 稳定 → V2 下线。

---

## 16. 风险与对策

| 风险 | 严重度 | 对策 |
|---|---|---|
| 仓库数量大（standalone 一 item 一 repo 可能数千） | 中 | Gitea 实测万级 repo 性能可接受；按 org 分组管理；为 AI 提供 index 仓库降低遍历成本 |
| 大量小文件性能（>5 万） | 中 | 起步阶段 <2 万文件无需处理；超过则评估 sparse checkout 或拆 org |
| 二进制污染 repo | 高 | 强制 Git LFS 或 Gitea Release，PR CI 拒收大文件 |
| Gitea 单点故障 | 高 | 部署 HA（共享存储 + 多副本），见附录 A |
| Gitea REST API 限流（默认 1500 req/h/user） | 中 | 单独 PAT 走服务账号；客户端 token bucket 限速；429 退避重试；大量初始同步改用 trees API 一次性拉取 |
| Gitea API 偶发 5xx / 网络抖动 | 中 | 指数退避重试（最多 3 次）；失败入死信队列告警；webhook 失败由 Gitea 自动重投 |
| webhook 投递延迟或丢失 | 中 | webhook 投递失败保留 24h 重试；server 端定时巡检（每小时拉取每个 repo 最新 commit SHA 比对）兜底 |
| webhook 重复/乱序 | 中 | server 端 commit SHA 幂等去重 + 顺序处理 |
| PAT 泄漏 | 高 | bot PAT 仅 `read:repository` / `write:repository` 最小 scope；限定 org；定期 90 天轮换；Gitea 审计日志监控异常调用 |
| fork 关系迁移遗漏 | 中 | 迁移脚本生成 dry-run 报告，人工核对后再切换 |
| frontmatter schema 演进 | 中 | `.catalog/schema.json` 加 schema_version，server 兼容多版本 |
| 镜像上游删除/重命名 | 中 | Gitea mirror pull 失败检测 + 标记 archived；本地 repo id 稳定不随上游变更 |
| 审核员体验下降（vs GitHub） | 低 | 给审核员做 Gitea PR UI 培训，或在 server 端做统一收件箱页面 |
| 私有部署客户离线场景 | 中 | 提供 Gitea 镜像导出/导入工具，支持完全离线同步 |
| AI agent 误操作 | 中 | scoped PAT 限定目录 + CI 阻断 + 人工 review gate |
| plugin 内部子文件版本漂移（运行时与 metadata 不一致） | 低 | server 不索引子文件，运行时由 csc 客户端按 .plugin.json 内 manifest 自校验；CI 阶段校验 plugin 内文件 checksum 一致性 |

---

# Part VI：附录

## 17. 已决策项

经评审已确定的设计决策：

| # | 决策点 | 决议 |
|---|---|---|
| 1 | "唯一仓库地址"的解读 | **弱解读**：`source_repo_url + path + ref` 作为稳定 URN，不强制一 item 一 repo |
| 2 | 是否维护独立索引层 | **不维护**：v2.1 决定去掉 `capability-index` 仓库与 index sync worker；发现层走 server DB（`capability_items`）+ REST API（`GET /api/capabilities`），Gitea repo description 由用户自行维护 |
| 3 | 镜像仓库命名 | **escaped upstream URL**：`costrict-mirror/<scheme-host-path>` |
| 4 | Plugin pack 是否拆 | **不拆**：保留上游集中式形态，子 plugin 用 path 寻址 |
| 5 | 能力项粒度 | **不下钻**：一个能力项 = 一个顶层 metadata 文件（standalone/mirror/seed 是 `skill.md` 等；pack 是 `plugins/<id>/.plugin.json`）。plugin 内部子 skill/command/mcp 文件由 plugin 自身运行时管理，server 不解析、不索引、不入 DB |
| 6 | server 是否执行 git 命令 | **不执行**：所有同步通过 Gitea REST API（compare / raw / trees），PAT 鉴权，scratch 镜像可运行 |
| 7 | 能力类型识别机制 | **文件结构启发式 + 首次锁定**：基于社区约定文件名/路径（`SKILL.md` / `agent.md` / `commands/*.md` / `mcp.json` / `.plugin.json` 等），首次 sync 时识别后锁定 `capability_type`；不引入新的声明文件（与社区习惯兼容） |
| 8 | 健康度治理策略 | **软治理 + 状态透传**：不阻断 PR merge，靠 4 级状态（`clean` / `warning` / `polluted` / `unknown`）多端透传（API / clone / 下发 manifest / agent），消费端自主决策；polluted override 全开放给所有 AI agent |
| 9 | Mirror pull 频率 | **统一 24h**：所有 mirror repo 配置 24 小时同步一次，不分级 |
| 10 | 健康度检测实现位置 | **全 server 端**：capability-check worker 监听 Gitea system webhook（PR 事件 + push 事件 + mirror pull），server 内调 Gitea API 拉文件树 + 启发式识别 + schema 校验，结果写入 `health_issues`；PR 评论由 server 调 Gitea API 写入；**零 Gitea Actions 依赖**（不需要 `act_runner`、不需要 workflow 文件） |
| 11 | 安全扫描迁移策略 | **保留 LLM + 切触发源**：scan_service / scan_job_service / scan_worker / Plugin 跳过 / 短路机制全部保留；触发源从 `catalog_ingest_service` 迁移到新 sync worker；新增 PR 触发分支（trigger_type=pr-check，不覆盖主表）；短路键 `CurrentRevision` → `git_sha`（双写兼容期）；scan_service 不拆分 |
| 12 | identification 与 security 关系 | **两个独立维度并存**：`identification_status`（文件结构健康度，capability-check worker 产出）+ `security_status`（内容安全，ScanWorkerPool 产出）；manifest 双字段透传；互不替代 |
| 13 | 公私能力 visibility 策略 | **纯 Gitea visibility 透传 + 发现层与权限层分离 + 用户默认 public**：取值仅 `public`/`private`（对齐 Gitea 原生，不引入 `internal`/`hidden`）；业务线层级仅作为 metadata 标签（不参与权限控制）；pack 内 plugin 不支持单独私有（拆 pack repo 解决）；**`costrict/` org 仅是官方印章**（admin 审核背书，与 public/private 无关）；用户自建 / fork 走 `u-<username>/` namespace **默认 public**（用户可改 private 作草稿）；公有能力两条路径——用户自主 public（§4.8.1）+ 可选官方认证 PR 升级到 `costrict/`（§4.8.2）；**发现层 API（`GET /api/capabilities`）不做权限过滤**——全量返回所有 item，每行带 `visibility` 字段；**内容访问层（详情 / download / clone）按 Gitea permission API 校验**，无权直接 403（§4.7.1）；server 用 `costrict-system` admin PAT sync 所有 repo（含 private）写 DB，sync worker 同步时把 `is_private` → `visibility` 字段 |
| 14 | 用户中心主权归属 | **costrict-web 承担用户中心**（username / email / 密码 / 业务字段主权）；Casdoor 退化为多登录源 UI 提供者；Gitea fork 加 JWT 中间件实现 user 自动同步。否决"Gitea 做用户中心 + HA"方向（见 §18.6） |
| 15 | Gitea fork 改动范围 | **最小化 fork**：JWT 中间件 + auth 链注册（~250 行）+ 全局 pre-receive hook（~150 行）= **总计 ~400 行**；不动 UI / cron / mirror / webhook 投递 / Actions；fork 维护成本可控，每季度 rebase upstream；详细见 §15 / ROADMAP Stage 0 |
| 16 | username 主权与可改性 | **costrict-web 自管 username，用户可改**：注册时填写 + 后续可改；变更通过通用 webhook 广播（`user.updated` 事件），Gitea sync worker 调 admin API 改名（Gitea 自动级联 repo ownership + redirect）；跨服务引用统一使用不可变 user_id；commit author 历史不改（git immutable，文档说明） |
| 17 | 用户变更事件广播 | **通用 webhook 系统**（`webhook_subscriptions` + `webhook_deliveries` 表）：任何业务服务（Gitea-sync / cs-cloud / csc / 其他）可订阅 `user.updated` / `user.disabled` / `user.deleted` 等事件；6 次指数退避重试（1s / 5s / 30s / 2min / 10min / 1h）+ 死信队列 + 日全量校对；HMAC 签名 + event_id 幂等 |
| 18 | content 下发流程 | **HTTP 代理 + Gitea permission API 反查（A2 方案）**：csc → costrict-web `/api/items/:id/download`（JWT）→ server 调 Gitea `GET /repos/{owner}/{repo}/collaborators/{username}/permission` 反查私有 repo 权限（5min Redis cache + webhook 主动失效）→ fine-grained service PAT（限定 owner）调 Gitea raw API 拉文件 → 返回 csc。csc 端零改造；权限闭环由 Gitea permission API 兜底 |
| 19 | Git 操作权限管理 | **保留 Gitea 原生 git 协议接口**（HTTP + SSH），用户/AI agent 直接 `git clone / push`；**branch protection 仅保留禁 force push + 禁 delete main**（不强制 PR / review / CI 通过门，详见 §7.3.2 简化方案）；AI agent / csc 用 fine-grained 用户 PAT（`read:repository` + `write:repository`，限 owner `costrict` / `costrict-plugins`，90 天轮换，明确禁止共享，D 方案 C 通道）；用户用 SSH key 优先（与 GitHub 体验一致）；deploy key 仅 CI/外部系统使用；误推送兜底靠 §11 健康度治理 + git revert + admin 标 archived + 硬配额（§7.4）|
| 20 | 硬配额拦截 | **fork Gitea 加全局 pre-receive hook**（~150 行）：单文件大小限制 + repo 总大小配额（owner 默认 + per-repo 覆盖）；commit message 不检查；hook 直连共享 PostgreSQL `gitea_ext.quota_rules` 表，5min TTL cache + sync worker 主动失效；配额查询优先级 repo 级 > owner 级 > 全局 default；mirror owner 单文件不限 |
| 21 | 统一动态配置中心 | **`costrict-config/platform-config` 仓库 `.gitea/*.yaml` 声明式**：所有 Gitea 动态配置（branch-protection / quota / teams / webhooks / labels / ISSUE_TEMPLATE）通过 yaml 文件管理；PR merge 后 costrict-web `GiteaConfigSyncWorker` 自动 diff + 调 Gitea API 应用；与 §3.2 "Git → DB 单向数据流"对齐；admin UI 仅 fallback（应急通道）；精选能力项 mono-repo 拆到 `costrict/curated-seed`（v3 决策：原 `costrict/registry-seed` 一身二任拆为配置 org + 能力项 repo） |
| 22 | 业务线层级与 Gitea 关系 | **E4 简化方案**：Gitea 不掺业务组织概念（4 个固定 org + 用户 namespace，无业务 team）；dept-sync 仍是部门树真相源；costrict-web 仅 webhook 接收 + Redis cache；sync worker **零 Gitea API 调用**；private repo collaborator 由 admin 手动邀请（`/admin/private-repos` 页面）；离职清理自动化（user.disabled/deleted → 调 Gitea admin API 移除 collaborator + 禁用账号）；适用前提：private capability 占比 < 10%；未来 private 占比上升可平滑切换到方案 3（dept → Gitea team sync） |
| 23 | Bot 身份模型 / admin token | **D 混合方案**：用户侧行为（csc push、AI agent 代用户操作、用户自己 git/API）统一按用户自己 PAT 或 JWT（不走 bot）；系统服务侧仅一个 `costrict-system` 账号 + 单一 admin PAT，明确 2 个使用场景（capability 索引同步 / 用户生命周期级联）；marketplace mirror 同步走 Gitea 内置 mirror pull 不需要 token；用户注销时 repo ownership 转给 `costrict-system` 保留历史不归档；admin PAT 90 天自动轮换（`BotTokenRotationWorker`）+ `gitea_admin_audit_log` 全量审计 |
| 24 | Branch Protection 简化（直接 push 模式） | **放开强制 PR / review / CI 阻塞门**：V3 是认证用户内部协作平台，main 分支仅保留禁 force push + 禁 delete（防历史覆写）；用户/AI agent 可直接 push main，体验与 V2 一致；不要求 PR、不要求 reviewer approve、不要求 CI 必过（capability-check CI 仍跑，仅作 commit status badge，非阻塞）；误推送兜底链路：§11 健康度自动 polluted + git revert 30s 恢复 + admin 标 archived + §7.4 硬配额拦截大文件 + commit author 归因 + Gitea audit log；适用前提：全部认证用户 + 团队 < 10 人；未来如需切回强制 PR 仅改 Gitea 配置无架构改动；实现工时 0 天 |

---

## 18. 替代方案

### 18.1 已否决：全 mono-repo

- 丢失上游原生形态，与 mirror / pack 来源冲突
- 跨 org 权限隔离难
- "唯一仓库地址"语义弱化

### 18.2 已否决：全一 item 一 repo（强解读）

- Plugin pack 被强行拆解，失去上游一致性
- Mirror 失去原貌
- 仓库数爆炸，运维负担重

### 18.3 已否决：完全去掉 DB，纯 Git

- 业务字段（计数、扫描结果、用户关系）无处安放
- 跨 repo 搜索能力差

### 18.4 已否决：用 Gitea 替代 Casdoor 当 IdP

- Gitea 多登录源管理 UI 弱于 Casdoor
- 见 `MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md`、`USER_TABLE_DESIGN.md` 等历史决策

### 18.5 已否决：用 PostgreSQL 的 git-like 扩展（如 Dolt）

- 缺乏 AI 工具链支持
- 引入新组件运维成本高

### 18.6 已否决：Gitea 做用户中心 + Gitea HA（v3 评审否决）

用户提议"既然 fork Gitea 了，让 Gitea 做用户中心 + HA"，评审否决理由：

- **Gitea HA 不成熟**：官方未正式支持 Cluster（Praefect 类），多实例有 cron 重复 / webhook 重复投递 / repo avatar 缓存不一致等已知问题；社区案例少；运维成本远高于 costrict-web HA（K8s Deployment 多副本成熟方案）
- **Gitea user 模型无法承载业务字段**：业务线 / 部门 / 自定义角色 / 偏好 / 配额 等无处安放，最终还是要 costrict-web 维护 `user_profile` 表，相当于"半个用户中心"在 cw
- **UI 品牌割裂**：用户登录 / 注册 / 改密码都要跳 Gitea UI（`gitea.xxx.com`），与 costrict-web 品牌不一致；fork UI 等于自建一半用户中心
- **故障域合并**：Gitea 故障 = 用户中心故障 = 全系统瘫；costrict-web 用户中心 + Gitea 业务系统 = 故障域分离
- **职责混淆**：Gitea 是 Git 服务器，让 Gitea 做用户中心违背工具本质；扩展字段需要更深的 fork（user 表 schema + UI + 业务逻辑），fork 维护成本爆炸

保留方案（已采纳）：costrict-web 用户中心 + fork Gitea JWT 中间件 + 全局 pre-receive hook（~400 行最小化 fork），见 §6 / §7。

---

## 19. 后续工作（Out of Scope）

- 跨企业能力项联邦（多 Gitea / server 实例间的 federation，走 server 间 REST API 互调或导出/导入工具）
- 能力项依赖图谱（基于 Git submodule 或 manifest）
- AI agent 自动 PR（agent 主动起草能力项）
- Gitea Actions 替代部分 server worker
- Plugin marketplace 私有化镜像同步（`costrict-plugin-marketplace` 项目独立）
- 跨 repo 批量操作工具（如"一次性给所有 skill 加 frontmatter 字段"）

---

## 20. 参考资料

- 当前 catalog ingest：`docs/CATALOG_INGEST.md`
- 安全扫描机制：`docs/SCAN_SKILL.md`
- 数据库设计：`docs/DATABASE_DESIGN.md`
- Casdoor 集成：`server/internal/casdoor/client.go`
- Capability 模型：`server/internal/models/models.go`
- Gitea JWT 中间件 fork 改动：见 §6.6（替代 v1 的 OAuth2 Source 方案，v3 不再使用 OAuth2 Source）
- Gitea webhook 文档：https://docs.gitea.com/development/webhooks
- Gitea mirror pull：https://docs.gitea.com/administration/command-line#admin-mirror-sync

---

## 附录 A：Gitea 部署参考配置

最小可售卖 HA 套件：

```
2 个 Gitea 副本（Deployment replicas=2）
+ PostgreSQL（与 costrict-web 共用，独立 schema）
+ Redis（与 costrict-web 共用，独立 db）
+ MinIO（附件/LFS）
+ NAS/CephFS（Git 仓库存储）
```

Gitea 关键配置（`app.ini` 节选）：

```ini
[database]
DB_TYPE  = postgres
HOST     = costrict-postgres:5432
NAME     = gitea
USER     = gitea

[cache]
ADAPTER  = redis
HOST     = redis://costrict-redis:6379/1

[session]
PROVIDER = redis
PROVIDER_CONFIG = redis://costrict-redis:6379/2

[queue]
TYPE     = redis
CONN_STR = redis://costrict-redis:6379/3

[storage]
STORAGE_TYPE = minio
MINIO_ENDPOINT = minio:9000

[oauth2_client]
ENABLE_AUTO_REGISTRATION = true
USERNAME = email
UPDATE_AVATAR = true
ACCOUNT_LINKING = login

[server]
LOCAL_ROOT_URL = https://gitea.costrict.local/
```

---

## 附录 B：webhook payload 关键字段

Gitea push event 中 server 关心的字段：

```json
{
  "action": "push",
  "secret": "<webhook-secret>",
  "repository": {
    "name": "skill-vetter",
    "owner": {"login": "costrict"},
    "default_branch": "main"
  },
  "ref": "refs/heads/main",
  "before": "<old-commit-sha>",
  "after": "<new-commit-sha>",
  "commits": [
    {
      "id": "<commit-sha>",
      "added":    ["skill.md"],
      "modified": [],
      "removed":  [],
      "author":    {"email": "alice@costrict.com"},
      "committer": {"email": "alice@costrict.com"},
      "timestamp": "2026-07-06T10:00:00Z",
      "message": "feat(skill): add skill-vetter"
    }
  ]
}
```

server 端用 `before..after` 做增量 diff，对 `added/modified/removed` 文件分别处理；按 `repository.owner.login` 前缀（`costrict` / `costrict-plugins` / `costrict-mirror`）判断 kind。

---

## 附录 C：现有调用链影响与适配

> 本附录基于对 `costrict-web` / `cs-cloud` / `csc` 三方调用链的调研（详见调研报告），分析 V3 整改对各组件的具体影响。

### C.1 三方调用链现状（V2 baseline）

```
csc（CLI 工具，opencode 改造）
  ├─ 命令实现：commands.ts:520 (getCommands) / :607 (getSkillToolCommands)
  ├─ 加载路径：
  │   ├─ 内置（src/skills/bundled/、src/plugins/bundled/）
  │   ├─ 用户级（~/.costrict/skills/、~/.costrict/plugins/installed_plugins.json）
  │   └─ 项目级（.costrict/skills/）
  ├─ 远程同步：
  │   ├─ costrict-web /api/items（metadata + content HTTP 代理）
  │   └─ favorite → cloudPluginSync.ts 自动同步
  └─ 鉴权：JWT（OAuth 流转，存 auth.json）

cs-cloud（设备端，opencode 改造）
  ├─ favorites_handler.go:96-163（fetchCloudFavorites）
  ├─ HTTP 代理角色：透传 csc ↔ costrict-web（无本地缓存、无 git 操作）
  └─ 不参与能力项 content 下发的真实数据流（仅代理 HTTP）

costrict-web（平台，server）
  ├─ /api/items/:id/download（content 下发，DB 直读）
  ├─ /api/items/:id/favorite（用户收藏）
  ├─ /api/items/:id/distribute（管理员分发）
  ├─ catalog_ingest_service.go（catalog bundle 摄取）
  ├─ sync_service.go（外部 git repo 同步）
  ├─ scan_service.go + scan_job_service.go + scan_worker.go（LLM 安全扫描）
  └─ capability_items 表（content 字段直存）
```

### C.2 V3 整改对各组件的影响

#### C.2.1 costrict-web server 端

| 模块 | V2 行为 | V3 改造 |
|---|---|---|
| `GET /api/items` 列表 API | DB content 字段直读 | DB 改为 metadata + business fields 索引；content 不再直读（参考字段）；**全量返回所有 item 含 private**（每行带 `visibility` + `owner` 字段），**不做权限过滤**；权限校验下沉到内容访问层（§4.7.1） |
| `GET /api/items/:id/download` | DB content 字段返回 | **A2 方案**：调 Gitea `GET /repos/{owner}/{repo}/raw/{ref}/{path}`（fine-grained service PAT），实时拉不缓存；私有 repo 先调 `GET /repos/{owner}/{repo}/collaborators/{username}/permission` 反查（5min Redis cache + webhook 主动失效） |
| `POST /api/items/:id/favorite` | plugin 类型返回 409 | **方案 B 标记式 favorite**：移除 409 gate（一行删除），plugin 卡片渲染 favorite 按钮；csc 端 favorite plugin 列表加 "Install via marketplace" 跳转 |
| `POST /api/items/:id/distribute` | 同上分发逻辑 | 保留语义；target_user_id 引用 user_id（不可变，不受 username 改名影响） |
| `catalog_ingest_service.go` | 触发 scan Enqueue（L1024, L1130） | **迁移到 sync worker**（监听 Gitea webhook）；Stage 5 下线 |
| `sync_service.go` | 触发 scan Enqueue（L421, L491） | **迁移到新 sync worker**；Stage 5 下线 |
| `scan_service.go` `scan_job_service.go` `scan_worker.go` | 短路键 CurrentRevision | **保留 LLM + 切触发源 + 短路键改 git_sha**（双写兼容期）；scan_service.go:252-254 plugin 跳过逻辑保留 |
| `capability_items` 表 | 含 SourceSHA / CatalogEntryDir / CurrentRevision / Content | **新增**：source_repo_url / source_repo_path / source_repo_ref / source_repo_kind / capability_type / git_sha / git_last_synced_at / mirror_of / identification_status / health_issues / last_checked_at / last_clean_at / **visibility**（§4.7：sync worker 从 Gitea repo `is_private` 同步，发现层 API 全量返回带标记）；**废弃**：SourceSHA / CatalogEntryDir / CurrentRevision / SourceType（双写兼容期后删） |
| 用户体系 | Casdoor 是 IdP，JWT 直验 Casdoor JWKS | **costrict-web 自签 JWT**（RS256 + 暴露 `/.well-known/jwks.json`）；Casdoor 退化为登录 UI 提供者；新增 `user_gitea_binding` / `user_profile` / `webhook_subscriptions` / `webhook_deliveries` / `gitea_admin_audit_log` 表 |

#### C.2.2 cs-cloud（设备端）

| 模块 | V2 行为 | V3 改造 |
|---|---|---|
| `internal/cloud/client.go` | HTTP 客户端调 costrict-web `/api/items` | **零改造** |
| `internal/localserver/favorites_handler.go` | HTTP 代理 csc ↔ costrict-web | **零改造**（不参与 content 下发真实数据流，仅代理） |
| 数据存储 | 无本地缓存（实时代理） | **零改造**（保持现状，不引入 manifest 缓存） |
| 与 Gitea 交互 | 无 | **零改造**（csc 直接拉 Gitea，cs-cloud 不参与 git 流程） |

cs-cloud 整体零改造。

#### C.2.3 csc（CLI 工具）

| 模块 | V2 行为 | V3 改造 |
|---|---|---|
| `~/.costrict/skills/` / `~/.costrict/plugins/installed_plugins.json` | 本地缓存优先 | **零改造**（本地副本机制保留） |
| `costrict/provider/auth.ts` | costrict-web OAuth 流转 + JWT 存 `auth.json` | **零改造**（仍走 costrict-web OAuth，JWT 不变） |
| `costrict/favorite/cloudPluginSync.ts` | 收藏后从 costrict-web 拉取同步 | **零改造**（同步链路不变，content 下发仍走 costrict-web HTTP 代理） |
| `costrict/favorite/favorite.ts` | saveFavoriteItem / getFavoriteItems | **零改造** |
| `commands.ts:520,607` | 命令加载本地副本 | **零改造** |
| `plugins/bundled/`、`skills/bundled/` | 内置能力 | **零改造** |
| **新增逻辑（plugin favorite 激活）** | favorite plugin 仅展示 | **方案 B**：plugin 卡片渲染 favorite 按钮 + 列表 "Install via marketplace" 跳转按钮（拼接 `marketplace_repo` + `plugin_name` 调 csc 现有 `csc plugin marketplace add / install` 命令）；不自动 install |

csc 端整体轻改造（仅 plugin favorite UI 跳转），其他零改造。

#### C.2.4 fork Gitea（新增组件）

| 模块 | 改造内容 |
|---|---|
| `routers/common/auth_jwt.go`（新增） | JWT 中间件 ~200 行：JWKS cache（5min TTL）+ RS256 验证 + `user_gitea_binding` 状态校验（非 synced 返回 503）+ cookie fallback |
| `routers/common/auth.go` | 中间件链注册（~30 行修改） |
| `routers/web/routes.go` | 路由注册（~10 行修改） |
| `modules/gitrepo/hooks.go` | `CreateDelegateHooks` 加系统级 pre-receive hook 路径 fallback（~20 行修改） |
| `modules/git/preceive_global.go`（新增） | 全局 pre-receive hook 处理逻辑 ~80 行：单文件大小 + repo 配额检查 + cache 查询 |
| `modules/setting/quota.go`（新增） | app.ini quota 配置加载 ~20 行 |
| `routers/internal/quota.go`（新增） | `POST /api/internal/quota-cache-invalidate` endpoint ~30 行 |
| `gitea_ext` schema（独立于 Gitea 自身 schema） | fork Gitea 直读 `gitea_ext.quota_rules` 表，不修改 Gitea 自身 schema |
| 不动 | UI / cron / mirror / webhook 投递 / Actions / Gitea 自身 DB schema |

### C.3 调用链整改迁移路径

```
Stage 0：fork Gitea + JWT 中间件 + 全局 pre-receive hook + costrict-web 用户中心（3-4 周）
    ↓
Stage 1：基础设施（1 周）
    ↓
Stage 2：仓库分类与数据导出（2 周）
    ↓
Stage 3：webhook 接通与双写灰度（2-3 周）
    ├─ /api/items/:id/download 内部实现切换为 A2（Gitea raw + permission 反查）
    ├─ /api/items/:id/favorite 移除 plugin 409 gate（方案 B）
    └─ scan_service 短路键双写（CurrentRevision + git_sha）
    ↓
Stage 4：mirror 接入自动化（1 周）
    ↓
Stage 5：下线旧通道与清理（1 周）
    ├─ catalog_ingest_service.go 下线
    ├─ sync_service.go scan Enqueue 调用删除
    ├─ capability_items.Content / SourceSHA / CatalogEntryDir 字段删除
    └─ security_scans.item_revision 字段删除
```

### C.4 关键决策对照表

| 维度 | V2 现状 | V3 整改后 | 决策编号 |
|---|---|---|---|
| content 下发流程 | DB 直读 | HTTP 代理 + Gitea permission API 反查（A2） | §17 第 18 行 |
| 用户中心 | Casdoor + costrict-web 双层 | costrict-web 主权 + Casdoor 退化 + fork 中间件 | §17 第 14-17 行 |
| plugin favorite | 409 gate（display-only） | 方案 B（标记式 favorite + 跳转 install） | 本附录 C.2.1 / C.2.3 |
| cs-cloud 改造 | - | 零改造 | 本附录 C.2.2 |
| csc 改造 | - | 仅 plugin favorite UI 跳转 | 本附录 C.2.3 |
| fork Gitea 改动 | - | ~400 行（auth_jwt.go + 全局 pre-receive hook + 注册） | §17 第 15 行 / 本附录 C.2.4 |

---

## v4 vs v3 章节对照

| v4 章节 | 来源（v3） | 说明 |
|---|---|---|
| §1 背景与痛点 | §1 | 不变 |
| §2 目标与非目标 | §2 | 不变 |
| §3 整体架构 | §3 + §6 | §3.3 合并原 §6 Git/DB 职责边界 |
| §4 仓库策略 | §4 + §8.6 | §4.3 新增"仓库归属规则"明确自建/fork → `u-<username>/`；§4.7 合并原 §8.6 公私能力可见性；§4.8 公有能力贡献流程合并原 §8.6 末段 |
| §5 文件 frontmatter Schema | §5 | 不变 |
| §6 认证集成 | §9.1–9.6 | 原 §9 上半部分 |
| §7 权限模型 | §8.3 + §9.7 + §9.8 | 合并三处权限相关内容 |
| §8 业务线层级 | §9.10 | 原 §9.10 独立成章 |
| §9 AI 操作工作流 | §8.1 + §8.1.1 + §8.2 | 原 §8 工作流相关 |
| §10 同步链路 | §7 | 原 §7 整体 |
| §11 能力项健康度 | §8.4 | 原 §8.4 独立成章 |
| §12 安全扫描迁移 | §8.5 | 原 §8.5 独立成章 |
| §13 动态配置中心 | §9.9 | 原 §9.9 独立成章 |
| §14 数据模型变更 | §10 | 不变 |
| §15 实施路径 | §11 | 瘦身，详情指向 ROADMAP |
| §16 风险与对策 | §12 | 不变 |
| §17 已决策项 | §13 | 决议内容中原引用按新章节号同步 |
| §18 替代方案 | §14 | 不变 |
| §19 后续工作 | §15 | 不变 |
| §20 参考资料 | §16 | 不变 |
| 附录 A/B/C | 附录 A/B/C | 不变 |
