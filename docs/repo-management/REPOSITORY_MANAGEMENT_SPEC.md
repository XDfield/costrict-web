# Capability Git 仓库管理规范

| 版本 | v2.17（**架构反转**：KB / workflow repo 不再落全局 `costrict-kb/` / `costrict-workflow/` org，改为落 **per-team Gitea org `t-<team_short_id>/`**（team ns）；workflow 模型从「每实例一 repo」改为「每 (team, def) 一类型 repo + 实例 = branch `inst-<inst_short_id>`」；workflow 定义 canonical 存储在类型 repo 的 main 分支；新增 §18 Team Namespace Management；KB/WF 路径算法 v2.0 加 `team_id` 入参） |
|---|---|
| 创建日期 | 2026-07-09 |
| 依据 | [`CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md`](./CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md)、[`TEAM_ORG_UNIFICATION.md`](../identity-tenant/TEAM_ORG_UNIFICATION.md) ADR-3 v3、[`TEAM_NAMESPACE_API.md`](./TEAM_NAMESPACE_API.md) |

> 本规范定义 capability 仓库的**创建、命名、归属、协作**规则。健康度、配额、审计等细节见 v4 提案。
>
> **v2.17 重大变更说明**：v2.15/v2.16 引入的全局 `costrict-kb/` / `costrict-workflow/` org 模型在 v2.17 中**整体替换**为 **per-team namespace** 模型。原模型下 KB / workflow repo 的权限管理走 per-repo collaborator，团队场景下需要为每个新 repo 重复加人；v2.17 改为 team 维度统一授权（成员加入 Gitea org 自动覆盖所有 kb / wf repo），与 [TEAM_ORG_UNIFICATION.md](../identity-tenant/TEAM_ORG_UNIFICATION.md) 的平台级 team 概念对齐。v2.16 §17.1 明文否决过的「类型 repo + 实例 branch」C 方案在引入 team ns 后被**重新评估并采纳**——team ns 缓解了原否决理由中的「权限粒度只能到类型级」核心痛点，详见 §17.1 与 §18。

---

## 1. 核心原则

1. **Git 为内容真相源**：内容变更一律经 `git push`；PostgreSQL 仅作索引
2. **数据流单向**：Git → DB，禁止反向写回
3. **能力项粒度 = 顶层 metadata 文件**：1 repo = 1 能力项（如 `skill.md` / `.plugin.json`）；plugin 实体子文件不解析
4. **`costrict-*` org 仅 admin 维护**：用户行为产生的 repo 一律归属 `u-<username>/`
5. **直推 main 为默认通道**：与 V2 编辑 UX 一致；PR 为可选审核通道
6. **Git server 跟随 tenant**：每个 tenant 拥有独立 Gitea 实例（数据隔离 + JWT 隔离），下文所有 `gitea.costrict.local` 占位符均指**当前 JWT 所归属 tenant 的 Gitea 实例 base_url**；详见 §1.5

---

## 1.5 Git Server 实例寻址（多租户）

> **设计前提**：依据 [`IDENTITY_ARCHITECTURE_ROADMAP.md`](../identity-tenant/IDENTITY_ARCHITECTURE_ROADMAP.md) 阶段 A/B，平台引入 tenant 维度后，**每个 tenant 拥有独立 Gitea 实例**——独立的 user 表 / repo 表 / collaborator 表 / JWKS endpoint。本规范下文所有 `gitea.costrict.local` / `gitea.<owner>/<repo>` 形式 URL **均为文档可读性占位符**，实施时必须按本节规则动态解析为当前 tenant 的实际 Gitea base_url。

### 1.5.1 tenant → Gitea base_url 映射存储

Gitea 实例元数据**存放在 `tenant_configs` 表（阶段 A2 引入）的 yaml 字段**，每 tenant 一行，schema 片段：

```yaml
# tenant_configs.<tenant_id>.yaml
tenant_id: acme
git:
  base_url: https://gitea.acme.costrict.local/        # 必须 https，结尾含 /，DNS 由 platform_admin 配置
  jwks_endpoint: https://api.costrict.local/.well-known/jwks.json   # 平台共享 JWKS（RS256）
  admin_pat_secret_ref: vault://kv/gitea/acme/admin-pat              # costrict-system admin PAT 引用
  webhook_secret_ref: vault://kv/gitea/acme/webhook-hmac             # push webhook HMAC 密钥引用
  fork_version: ">=1.22-costrict.3"                                  # fork JWT 中间件最低版本
```

`tenant_id` 在 costrict-web `users.tenant_id` 列上绑定（阶段 B2），用户登录时通过 subdomain / email domain / 显式选择（阶段 B3）解析。

### 1.5.2 JWT 携带 tenant_id 的硬约束

- 阶段 A5 强制 JWT claims 新增 `tenant_id` 字段（与 `universal_id` / `provider` / `email` / `exp` 并列）
- fork JWT 中间件（§6.6）**先校验 `tenant_id` claim 与本实例归属 tenant 一致**，不一致直接 401（防 URL 被恶意拼接跨 tenant 串号）
- 一致后再走原 RS256 → JWKS 验签流程

### 1.5.3 URL 解析流程

```
客户端 / 用户                costrict-web API                tenant_configs                Gitea 实例
   │                            │                                │                            │
   │ GET /api/capabilities      │                                │                            │
   │ Authorization: Bearer <JWT>│                                │                            │
   ├───────────────────────────►│                                │                            │
   │                            │ 解析 JWT.tenant_id              │                            │
   │                            │ SELECT yaml FROM tenant_configs WHERE tenant_id=$1
   │                            ├───────────────────────────────►│                            │
   │                            │   yaml（含 git.base_url）       │                            │
   │                            │◄───────────────────────────────┤                            │
   │                            │ 拼接 gitea_*_url（绝对 URL，含 tenant base_url）            │
   │   200 OK                   │                                │                            │
   │   items[].gitea_*_url      │                                │                            │
   │   = https://gitea.acme...  │                                │                            │
   │◄───────────────────────────┤                                │                            │
   │                                                             │                            │
   │ GET <gitea_tree_url>（绝对 URL）                            │                            │
   │ Authorization: Bearer <JWT>（同上）                         │                            │
   ├────────────────────────────────────────────────────────────────────────────────────────►│
   │                                                             │                            │ fork 中间件：
   │                                                             │                            │ 1) JWT.tenant_id == 本实例 tenant？
   │                                                             │                            │ 2) RS256 → JWKS 验签
   │                                                             │                            │ 3) Gitea 原生 collaborator 校验
   │                                                             │                            │ 200 OK / 401 / 403
   │◄────────────────────────────────────────────────────────────────────────────────────────┤
```

### 1.5.4 客户端硬约束

| 客户端 | 行为 |
|---|---|
| portal iframe / 浏览器 | **必须从 API 响应读取 `gitea_*_url` 后跳转**；不得自行把 `https://gitea.costrict.local` 写死作前缀 |
| csc CLI | clone 时从 `gitea_clone_url` 字段取完整 URL；不依赖本地配置的 `costrict.gitea.host`（仅作 fallback） |
| AI agent / SDK | 同 csc，所有 URL 一律来自 API 响应；本地 config 只保存 api endpoint，不保存 gitea base_url |
| 嵌入式 git 客户端（IDE 等） | clone URL 由用户从 portal 复制（已含 tenant base_url）；PAT 仍按 §12 用户 fine-grained PAT 流程 |

### 1.5.5 跨 tenant 场景

| 场景 | 处理 |
|---|---|
| 用户 A（tenant=acme）访问 tenant=contoso 的 URL | JWT 中 `tenant_id=acme` 与目标 Gitea 实例（contoso）不匹配 → fork 中间件 401 |
| `costrict-official/`（官方 org）下发到所有 tenant 实例 | 每个 tenant Gitea 实例独立维护 `costrict-official/` org 副本，由 `costrict-system` admin PAT 周期同步（GitOps pipeline，不在本规范范围） |
| `costrict-mirror/` 上游镜像 | 每个 tenant 实例独立 mirror pull（同一批上游；各 tenant 互不感知） |
| 用户跨 tenant fork repo | **不可达**（不同 Gitea 实例之间无原生 fork 关系）；跨 tenant 共享必须走 `costrict-official/`（官方印章）或显式打包迁移流程 |

### 1.5.6 文档约定

- 本规范所有示意 URL（如 `https://gitea.costrict.local/costrict-official/skill-vetter`）应理解为 **`<tenant_gitea_base_url>/costrict-official/skill-vetter`** 的占位写法
- 阅读时请把 `gitea.costrict.local` 替换为当前读者所属 tenant 的实际 base_url（如 `gitea.acme.costrict.local`）
- 文档不重复书写 "tenant 的 Gitea 实例" 短语——所有 Gitea 行为均隐含 tenant 上下文

---

## 2. Namespace 与 Org 结构

### 2.1 固定 org（admin 维护）+ per-team org（平台动态创建）

#### 2.1.1 平台固定 org（4 个）

| org | 用途 | 默认 visibility |
|---|---|---|
| `costrict-config` | 平台配置中心（GitOps 真相源；tenant 级 yaml 配置，含 admin PAT secret_ref / webhook HMAC secret_ref 等敏感字段） | **private**（强制；仅 `costrict-system` + platform_admin 可见） |
| `costrict-official` | 官方能力项（standalone skill / subagent / command / mcp / plugin；含 admin 打 `curated` tag 的精选条目） | public（强制；官方印章） |
| `costrict-template` | 模板存放（wizard 脚手架 init 的 git 真相源；§11.7） | public（强制） |
| `costrict-mirror` | 上游镜像（read-only） | 跟随上游 |

#### 2.1.2 Per-team org（动态创建，命名 `t-<team_short_id>`）

| 属性 | 值 |
|---|---|
| 创建时机 | @server 在收到 team 首次 `members:sync` 调用时自动创建（详见 §18 与 [`TEAM_NAMESPACE_API.md`](./TEAM_NAMESPACE_API.md)） |
| 真相源 | team 元数据 + 成员归属由外部 `org-team-service` 模块维护（详见 [`TEAM_ORG_UNIFICATION.md`](../identity-tenant/TEAM_ORG_UNIFICATION.md) ADR-10）；@server 仅承接 Gitea 侧 org + team 同步 |
| 命名规则 | `t-<team_short_id>`，其中 `team_short_id` = `team_id`（UUID）去连字符取前 8 hex；详见 §18.3 |
| 内含 repo 类型 | (a) `kb-<host>__<segments>` KB repo（§16）；(b) `wf-<def_slug>` workflow 类型 repo（§17，实例以 branch 表达） |
| 成员模型 | Gitea org members = team 当前成员（由 @server 通过 `members:sync` 接口同步）；org 层级 visibility 控制 |
| 默认 visibility | **private**（强制；`members_can_create_repos=false`，所有 repo 由 server admin PAT 在 `POST /api/internal/workflow/init` / `POST /api/internal/kb/ensure` 中创建） |
| 生命周期 | team 解散时 archive（不立即删，保留审计窗口，详见 §18.6） |

> **v2.17 删除项**：原 `costrict-kb/` 与 `costrict-workflow/` 两个全局 org **不再使用**——v2.16 之前的设计假设"业务数据集中放全局 org"，但实际团队协作场景下需要重复为每个新 repo 加 collaborator，运维成本高。v2.17 改为 per-team org 后，**新 KB / WF repo 一律落 `t-<team_short_id>/`**。由于 v2.16 模型尚未实施（无生产数据），不存在迁移问题。

> **v2.11 改动说明**：
> 1. **`costrict/` 改名为 `costrict-official/`**——名字更明确表达"官方印章"语义，避免与品牌 prefix `costrict-*` 混淆
> 2. **`curated-seed` 合并入 `costrict-official/`**——`costrict-official/<slug>` 直接存放，不再用 `costrict-official/curated-seed/<type>/<slug>/` 子路径；"精选"是 admin 打的 tag（`capability_items.tags` 含 `curated`），与 namespace 无关
> 3. **新增 `costrict-template/`**——§11.7 wizard 模板下载 API 的 git 真相源；模板版本化、可 fork 定制、多租户可覆盖
> 4. v2.10 的"slug 跨 kind 唯一"约束在 v2.11 期间继续生效；v2.12 取消 `pack` / `seed` kind 后简化为「`costrict-official/` org 内 slug 唯一」（只剩 standalone 一种用户可建 kind，不再需要跨 kind 限定）

### 2.2 用户 namespace

- 每个认证用户对应**唯一** namespace：**`u-<username>/`**（如 `u-alice/`）
- 由 fork JWT 中间件在用户首次访问 Gitea 时 auto-provision
- 用户自建 / fork 的 repo 一律归属此 namespace
- **username 不可变（immutable）**：注册后即冻结，不支持改名；用户显示名（display_name）/ 头像 / 邮箱等业务字段可改，但 `username` 与 `u-<username>/` namespace 一旦建立即终身绑定
  - **理由**：避免 Gitea `PATCH /admin/users/{name}` + repo ownership 级联 + redirect 的复杂同步链路；保持 `gitea_username` 单调，审计 / 日志 / URL 永不失效；commit author 历史与当前 username 1:1 对齐，无歧义
  - **替代方案**：用户若坚持更换 username，走「注销旧账号 + 注册新账号 + 申请 ownership transfer」三步流程（admin 介入）

---

## 3. 仓库类型（kind）

| kind | 含义 | 能力项粒度 | 写权限 |
|---|---|---|---|
| `standalone` | 独立 skill / subagent / command / mcp / plugin | repo 级（整个 repo = 一个能力项） | 用户直推 + 可选 PR |
| `mirror` | 上游镜像 | 视上游形态 | **read-only**，必须 fork 后改 |

> v2.12 取消 `pack` / `seed` kind——前者经评估无独立场景（多 plugin 集合可通过 tags / marketplace catalog / bundle 表达，不必引入 path 级粒度），后者在 v2.11 curated-seed 取消后已失效。"官方精选"语义改为 admin 在 `capability_items.tags` 打 `curated` 标签（与 namespace 解耦），不需要独立 kind。

---

## 4. 仓库命名规范

### 4.1 总则

- 全部小写，仅允许 `[a-z0-9-]`
- 以字母开头，字母或数字结尾，长度 1–64
- 使用 kebab-case（`skill-vetter`，非 `skill_vetter` / `SkillVetter`）

### 4.2 standalone 命名约定

| 类型 | 命名格式 | 示例 |
|---|---|---|
| skill | `skill-<slug>` 或 `<slug>` | `skill-vetter` |
| subagent | `<slug>-subagent` 或 `<slug>` | `code-reviewer-subagent` |
| command | `<slug>-command` 或 `<slug>` | `refactoring-command` |
| mcp | `<slug>-mcp` 或 `<slug>` | `filesystem-mcp` |
| plugin | `<slug>-plugin` 或 `<vendor>-<slug>-plugin` | `vetter-plugin` / `acme-fs-plugin` |

### 4.3 mirror 命名（escaped upstream URL）

转换规则：scheme 与 host 用 `-` 连接、path 的 `/` 替换为 `-`、全部小写、保留 owner-repo 段。

| 上游 URL | 本地 mirror repo |
|---|---|
| `https://github.com/zgsm-ai/everything-ai-coding` | `costrict-mirror/github-com-zgsm-ai-everything-ai-coding` |
| `https://gitlab.com/anthropics/skills` | `costrict-mirror/gitlab-com-anthropics-skills` |

### 4.4 固定名 repo

| 仓库 | 用途 |
|---|---|
| `costrict-config/platform-config` | 平台 GitOps 真相源（tenant 级 yaml；**private**，仅 admin / `costrict-system` 可见） |
| `costrict-template/templates` | wizard 模板真相源（§11.7）；mono-repo，**按 type 分子目录**，每子目录是该 type 的完整脚手架：`skill/`（`skill.md.tmpl` + `README.md.tmpl`） / `subagent/`（`agent.md.tmpl`） / `command/`（`commands/<slug>.md.tmpl`） / `mcp/`（`mcp.json.tmpl`） / `plugin/`（`.plugin.json.tmpl` + `index.js.tmpl`）；用户 / csc 通过 `git clone --filter=blob:none --sparse` + `git sparse-checkout set <type>` 拉对应子目录做 init（详见 §11.7） |

> v2.11 取消 `costrict/curated-seed` 固定名——"精选"语义改为 admin 在 `capability_items.tags` 上加 `curated` 标签，与 namespace 解耦；任何 `costrict-official/<slug>` 都可被标记为 curated。

### 4.5 命名冲突规则

- `costrict-*` org：由 admin 在 PR merge 时把关，不允许与现有 repo 重名
- `u-<username>/`：用户 namespace 内 slug 不可重复；不同用户 namespace 间可重名（`u-alice/skill-x` 与 `u-bob/skill-x` 合法）

### 4.6 KB repo 命名（v2.17，落 team namespace）

KB repo 命名不依赖用户输入 slug，由 server 端纯函数从 `(code_repo_url, team_id)` **确定性地推导**，输出固定格式：

```
t-<team_short_id>/kb-<host>__<escaped_segments_joined>
```

详细算法、字符转义规则、长度截断策略、测试用例见独立 spec：[`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) v2.0。

示例：

| 代码 repo URL | team_id（前 8 hex） | KB repo path |
|---|---|---|
| `https://github.com/ownerA/proj.git` | `7f3c9a1e` | `t-7f3c9a1e/kb-github.com__ownera__proj` |
| `https://gitlab.com/Group.Foo/bar-baz.git` | `7f3c9a1e` | `t-7f3c9a1e/kb-gitlab.com__group.foo__bar-baz` |
| `https://gitea.costrict.local/team-x/internal-svc` | `99aa88bb` | `t-99aa88bb/kb-gitea.costrict.local__team-x__internal-svc` |

**强约束**：
- 算法**唯一来源是 server**（csc 不内置副本，路径仅来自 `POST /api/internal/kb/ensure` 响应）
- 输出路径**永远落在 `t-<team_short_id>/` org 下**，不允许用户指定其他 namespace
- **同 code repo 在不同 team 下各有独立 KB repo**（per-team 视角；与原 v2.15 全局唯一模型不同）
- 算法版本当前为 v2，**不加路径前缀版本号**（Gitea 路径只支持 2 层 `<owner>/<repo>`）
- 用户 PAT **无权访问 `t-<team_short_id>/` org 的 repo 创建**——所有创建走 server admin PAT
- **team_id 是平台级概念**（来自外部 `org-team-service`），不是 workflow 业务专属；KB / workflow / 未来 capability 共享同一 team ns

### 4.7 Workflow repo 命名（v2.17，类型 repo + 实例 branch）

Workflow repo 模型从 v2.16「每实例一 repo」改为「每 (team, def) 一类型 repo + 实例 = branch」。命名由两个独立的 server 端纯函数推导：

**Repo path（每 (team, def) 一个类型 repo）**：

```
t-<team_short_id>/wf-<def_slug_escaped>
```

**Branch name（每个实例一个 branch）**：

```
inst-<instance_short_id_8chars>
```

详细算法、字符转义规则、长度截断策略、测试用例见独立 spec：[`WORKFLOW_REPO_PATH_ALGORITHM.md`](./WORKFLOW_REPO_PATH_ALGORITHM.md) v2.0。

示例：

| workflow_def_slug | team_id（前 8 hex） | instance_id（前 8 hex） | wf_repo_path | instance_branch |
|---|---|---|---|---|
| `bug-fix-flow` | `7f3c9a1e` | `f3a8b2c1` | `t-7f3c9a1e/wf-bug-fix-flow` | `inst-f3a8b2c1` |
| `compliance-check` | `7f3c9a1e` | `a9e7d4f2` | `t-7f3c9a1e/wf-compliance-check` | `inst-a9e7d4f2` |
| `release-pipeline` | `99aa88bb` | `00000000` | `t-99aa88bb/wf-release-pipeline` | `inst-00000000` |

**强约束**：
- 算法**唯一来源是 server**（csc 不内置副本，path 与 branch 都来自 `POST /api/internal/workflow/init` 响应）
- 输出路径**永远落在 `t-<team_short_id>/` org 下**
- **类型 repo 的 main 分支 = workflow 定义的 canonical 存储**（团队对 def 的修改走 main 上的 PR；实例启动时从 main HEAD 创建 `inst-<short>` branch）
- **同 def 在不同 team 下各有独立类型 repo**（per-team 视角；团队自定义 def 不污染其他 team）
- **同 team 同 def 的多个实例共享同一类型 repo**，每个实例 = 一个 branch；实例间 PR / commit 共存于同一 repo（审计需按 branch 过滤）
- 节点 PR 分支命名 `node/<seq>-<slug>`，**base 为 `inst-<short>`**（不是 main）；详见 §17.5
- 算法版本当前为 v2，**不加路径前缀版本号**
- 用户 PAT **无权访问 `t-<team_short_id>/` org 的 repo 创建**——所有创建走 server admin PAT
- `instance_id` 由平台 workflow 编排器（独立服务）在实例启动时分配 team 内唯一 UUID，server / csc 不生成
- `team_id` 来自外部 `org-team-service`，server / csc 不生成

---

## 5. 仓库归属矩阵

| 来源 | 归属 namespace | 谁能创建 | 默认 visibility |
|---|---|---|---|
| 用户自建 standalone（skill/subagent/command/mcp/plugin） | `u-<username>/<slug>` | 用户本人（PAT 限定 owner=`u-<self-username>`） | **public**（可改 private 作草稿） |
| 用户 fork 官方 repo | `u-<username>/<repo-name>`（Gitea 原生 fork-to-personal） | 用户本人 | **public** |
| 用户 fork 上游 mirror | `u-<username>/<repo-name>`（mirror 本身 read-only，必须 fork） | 用户本人 | **public** |
| 官方 standalone | `costrict-official/<slug>` | admin（PR merge 后入官方 org） | public（强制） |
| 官方精选（curated） | `costrict-official/<slug>` + admin 在 `capability_items.tags` 打 `curated` 标签 | admin（PR merge + tag） | public（强制） |
| 平台模板 | `costrict-template/templates`（mono-repo） | admin（PR merge；wizard 脚手架 init 的 git 真相源，§11.7） | public（强制） |
| 上游 mirror | `costrict-mirror/<escaped-url>` | admin（Gitea mirror 配置） | 跟随上游 |
| 平台配置 | `costrict-config/platform-config` | admin（强制） | **private**（仅 `costrict-system` + platform_admin 可见） |
| 知识库（KB）repo | `t-<team_short_id>/kb-<host>__<escaped_segments>`（v2.17；路径由 server 纯函数从 `(code_repo_url, team_id)` 推导，详见 [`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) v2.0；落 team ns） | **server admin PAT**（`POST /api/internal/kb/ensure` 内部接口；team ns 必须先经 `members:sync` 创建，详见 §16 与 §18） | **private**（强制；`members_can_create_repos=false`；权限通过 team ns org 成员关系继承） |
| Workflow 类型 repo | `t-<team_short_id>/wf-<def_slug>`（v2.17；path 由 server 纯函数从 `(workflow_def_slug, team_id)` 推导，branch 由 `instance_id` 推导，详见 [`WORKFLOW_REPO_PATH_ALGORITHM.md`](./WORKFLOW_REPO_PATH_ALGORITHM.md) v2.0；落 team ns；main = workflow def canonical 存储，每实例一个 `inst-<short>` branch） | **server admin PAT**（`POST /api/internal/workflow/init` 内部接口；team ns 必须先经 `members:sync` 创建，详见 §17 与 §18） | **private**（强制；`members_can_create_repos=false`；权限通过 team ns org 成员关系继承） |

**强约束**：
- 用户 PAT scope 限定 owner = `costrict-official` / `costrict-template`（read-only）/ `u-<self-username>`；**不允许写 `costrict-mirror`**（mirror 只读）；**不允许任何权限（read / write）访问 `costrict-config`**（含 admin PAT secret_ref / webhook HMAC secret_ref 等敏感字段，仅 `costrict-system` 服务账号 + platform_admin 可见）
- **`t-<team_short_id>/` org 的 repo 创建权独占给 server admin PAT**（§16 / §17 / §18）；用户 PAT 仅可对**自己作为 org member 的 team ns repo**做 push / clone / PR；用户 PAT **不允许任何 scope 包含 `t-*` owner 的 repo:create 权限**——避免用户绕过 `POST /api/internal/workflow/init` / `kb/ensure` 直接建 repo
- **team ns org 成员关系是权限的唯一来源**：用户加入 team（即加入 Gitea org）→ 自动获得该 team ns 下所有 repo 的 read/write 权限（按 org 内 role）；不再为每个 repo 显式加 collaborator（per-repo collaborator 仍保留作 owner-level fine-grained 调整）
- `costrict-official/` org 仅是**官方印章**（admin 审核背书），与 public/private 无关——用户在自己 namespace 下已经可以 public
- `costrict-official/` org 内 slug 唯一：一个 slug 对应一个 repo（不可二义）
- `costrict-template/` 仅 admin 维护，用户 read-only；多租户场景下 tenant 可 fork `costrict-template/templates` 到自己 Gitea 实例并自定义（覆盖上游）
- `costrict-config/` org **恒定 private**——`GiteaConfigSyncWorker` 用 `costrict-system` admin PAT 拉 yaml（§10.10）；platform_admin 通过 costrict-web `/admin/git-config` UI 查看 / 编辑（背后仍走 admin PAT）；普通用户在 portal / csc / Gitea UI 均**不可见、不可 clone**

---

## 6. 可见性

| visibility | 含义 | Gitea 配置 |
|---|---|---|
| `public` | 所有登录用户可见、可 clone、可下发 | `is_private=false` |
| `private` | 仅 owner + collaborator 可见 | `is_private=true` |

**不引入** `internal` / `hidden` 等中间态。`u-<username>/` 下 repo 默认 `public`，用户可在自己 namespace 内改 private 作草稿。

**发现层与权限层分离**：
- **发现层**（`GET /api/capabilities` 列表）：不做权限过滤，全量返回（带 `visibility` + `owner` 字段）
- **内容访问层**（详情 / download / clone）：实时调 Gitea permission API 校验，无权返回 403

---

## 7. 协作工作流

### 7.1 默认通道：直推 main（V3 简化方案）

```
AI agent / 用户:
   ├─ GET /api/capabilities?q=<slug>  (确认 slug 未占用)
   ├─ git clone <repo-url>            (在 u-<username>/<slug> 下)
   ├─ 写顶层 metadata 文件（如 skill.md，含 frontmatter）
   ├─ git commit -m "feat(skill): add <slug>"
   ├─ git push origin main            (用 fine-grained PAT)
   └─ webhook 自动触发 sync + capability-check + SecurityScan
```

**适用**：
- 用户自建 standalone（`u-<username>/<slug>`）
- 官方 org 内迭代（`costrict-official/<slug>` collaborator）

### 7.2 可选通道：PR 流程

```
AI agent / 用户:
   ├─ fork 官方 repo → u-<username>/<repo-name>
   ├─ git checkout -b feat/<branch>
   ├─ 改文件 → commit → push branch
   ├─ POST /api/v1/repos/<owner>/<repo>/pulls  (创建 PR)
   ├─ admin 审核 → merge
   └─ push webhook → sync worker 覆盖主表
```

**适用**：
- 用户 repo 升级为官方认证（`costrict-official/<slug>`；v2.11 取消 curated-seed 独立路径）
- 跨业务线重要变更评审
- mirror repo 的 fork 改进（mirror 本身 read-only，必须 PR）

### 7.3 Branch Protection

仅保留两条硬约束（不强制 PR / review / CI 通过门）：

```
main / master:
  ✗ 不允许 force push（防历史覆写）
  ✗ 不允许 delete（防误删）
  ✓ 允许直推 main
  ✓ CI 非阻塞（capability-check 仅作 commit status badge）
```

兜底链路：误推送靠 `git revert` 30s 恢复 + 健康度自动 polluted + commit author 归因 + Gitea audit log。

---

## 8. 能力项 CRUD 与同步场景

### 8.1 Create（创建）

| 场景 | 入口 | 触发同步 |
|---|---|---|
| 用户自建 standalone | `u-<username>/<slug>` 下 `git init` + 写 `skill.md` + push main | push webhook → sync worker |
| AI agent 代用户创建 | 同上，使用用户本人 PAT（commit author = 用户） | 同上 |
| admin 新增 mirror | Gitea UI 配置 mirror pull，周期同步上游 | Gitea mirror pull → push webhook |
| 官方 curated 新条目 | admin PR 到 `costrict-official/<slug>` → merge → 在 `capability_items.tags` 加 `curated` | post-merge push webhook + tag worker |

**关键约束**：
- 顶层 metadata 文件**首次 push 即锁定** `capability_type`；后续切换必须先在后台申请 unlock（否则触发 polluted 告警）
- 启发式识别置信度低 → `identification_status=unknown`，进 pending_review，**不进发现层**
- 同时含多类型文件的混合 repo（如 `SKILL.md` + `commands/*.md`）合法，`capability_items` 多行（每行一个 `capability_type`）

### 8.2 Read（查看）

| 用途 | API / 入口 | 权限校验 |
|---|---|---|
| 列表 / 搜索（发现层） | `GET /api/capabilities` | **不过滤**——全量返回，每行带 `visibility` + `owner` |
| 详情 | `GET /api/capabilities/:id` | 调 Gitea `GET /repos/{owner}/{repo}/collaborators/{user}/permission`（5min Redis cache） |
| 内容下发 | `GET /api/items/:id/download` | server 代理 → Gitea raw API（fine-grained service PAT），无权返回 403 |
| 历史版本 | Gitea `GET /repos/{owner}/{repo}/commits?path={filepath}` | 按 repo visibility + collaborator 校验 |
| 原文跳转 | `https://gitea.costrict.local/<owner>/<repo>/-/tree/<ref>/<path>` | 由 Gitea 直接校验 |

### 8.3 Update（更新）

| 触发 | 路径 | server 行为 |
|---|---|---|
| 顶层 metadata 文件改动 | push main | sync worker upsert `capability_items`（content + version + git_sha） |
| plugin 子文件改动 | push main | server **忽略**（不下钻），但 commit SHA 更新用于版本追踪 |
| PR 改动 | PR webhook | check worker 写 `health_issues`（PR head SHA 维度，临时）；merge 后覆盖主表 |
| 平台配置改动 | `costrict-config/platform-config` PR merge | `GiteaConfigSyncWorker` 自动 diff + 应用到 Gitea |
| 类型锁定后改结构 | 必须先 unlock → 改文件 → re-sync | 未 unlock 触发 polluted |
| 可见性切换 | 用户在 Gitea UI 改 `is_private` | sync worker 同步写 `visibility` 字段 |

**版本号**：frontmatter `version` 字段（语义版本），由作者手动 bump；server 不自动递增。

**短路键**：`item_id + git_sha`（旧 `CurrentRevision` 双写兼容期后删除）。

### 8.4 Delete（删除）

> **不直接删除 repo**——保留历史完整可追溯。

| 场景 | 操作 | 效果 |
|---|---|---|
| 作者主动下线 | Git 删除 / 移动顶层 metadata 文件 → push | sync worker 标 `status='archived'`；repo 历史保留 |
| 行政下架（违规 / 安全问题） | admin 在 costrict-web UI 标 `status='banned'` | **不动 Git**；发现层不返回；下发被拒 |
| 用户注销 | admin token 调 Gitea API 转移 ownership | repo 落到 `costrict-system/<原-repo-name>`，原 URL 自动 redirect |
| 误删恢复 | `git revert` 30s 内 | sync worker 标 `status='active'` |
| 物理删除（极端情况） | admin 在 Gitea UI 手动（**非默认**） | 不可逆，全程审计；DB 同步清理 |

### 8.5 同步链路与触发场景

```
push / mirror pull / PR merge
   │
   ├─► Gitea system webhook (push event)
   │     ├─ commit SHA 幂等去重（Redis SETNX, 24h TTL）
   │     ├─ 风暴防护：同 repo 短时多次 push → Redis 队列 debounce 1-2s，取最新 commit
   │     │
   │     ▼
   │   costrict-web sync worker
   │     ├─ GET /repos/{owner}/{repo}/compare/{before}...{after}
   │     ├─ 路径过滤：只保留顶层 metadata 文件
   │     │   └─ skill.md / subagent.md / command.md / mcp.md / .plugin.json
   │     ├─ GET /repos/{owner}/{repo}/raw/{path}?ref={sha} 拉每个变更文件
   │     ├─ 解析 frontmatter / .plugin.json → upsert capability_items
   │     ├─ 同步 visibility（GET /repos/{owner}/{repo} → is_private → DB）
   │     ├─ 异步触发 SecurityScan（含 PR 触发分支 trigger_type=pr-check，不覆盖主表）
   │     └─ 更新 registry.last_synced_commit
```

**同步触发场景矩阵**：

| 触发源 | 频率 | 场景 |
|---|---|---|
| 内容 repo push | 实时 | 用户 / AI 直推、PR merge 后 |
| mirror pull | 24h 周期 | Gitea 周期 `git fetch upstream`，server 仅消费 webhook |
| 兜底巡检 | 每小时 | server 拉每个 repo 最新 commit SHA 与 `last_synced_commit` 比对，覆盖 webhook 丢失 |
| 手动触发 | 按需 | admin 在 costrict-web UI 点"立即同步"（应急 / 排障） |
| 全量重扫 | 按需 | admin 触发（如 schema 升级、迁移后），按 registry 并发 ≤ 20 |

**幂等保证**：每个 webhook payload 携带 `after` commit SHA，server 用 Redis SETNX 去重；Gitea 自身 5 次重试，server 必须幂等。

**Mirror 同步特殊性**：
- mirror pull 后 server 检测 upstream 文件结构变化（如 `SKILL.md` 消失）→ 标 `polluted`
- mirror owner 决定是否继续 mirror，可随时在 Gitea UI 改 mirror 配置
- mirror pull 频率统一 24h，不分级

---

## 9. 协作编辑能力项

### 9.1 协作场景矩阵

| 场景 | 典型案例 | 推荐通道 | 权限要求 |
|---|---|---|---|
| **单人维护** | 用户自建 standalone 草稿（`u-<username>/<slug>`） | 直推 main | owner 本人 PAT |
| **多人协作同一 repo** | 用户 namespace 内多人维护；或官方 repo 协作 | 直推 main（共享 collaborator） | admin 邀请 collaborator write |
| **fork 改进贡献回上游** | 用户改进官方 skill | PR（feat 分支 → merge） | 用户 fork + PR |
| **业务线集体协作** | 用户 namespace 内多人维护（`u-<team-lead>/<slug>`）；或升级到 `costrict-official/<slug>` 后业务线共同维护 | 直推 main（业务线成员 collaborator） | repo owner 邀请 collaborator write |
| **AI agent 代用户操作** | AI 自动写 skill | 直推 main（用户 PAT） | 用户 PAT，commit author = 用户 |
| **跨业务线评审** | 重大改动需多方审核 | PR（reviewer approve） | PR 通道 + reviewers |

### 9.2 collaborator 邀请与管理

**邀请路径**：repo owner 或 admin 通过 Gitea UI（或 costrict-web `/admin/private-repos` 页面调 Gitea collaborator API）邀请用户加入 repo。

| 操作 | 谁能做 | 入口 |
|---|---|---|
| 邀请 collaborator（write 权限） | repo owner / admin | Gitea repo Settings → Collaborators |
| 移除 collaborator | repo owner / admin | 同上；离职通过 webhook 自动清理 |
| 改 collaborator 权限 | repo owner / admin | read / write / admin 三级 |
| 批量业务线邀请 | admin | costrict-web `/admin/private-repos` |

**离职自动清理**：dept-sync webhook 推送 `user.disabled` / `user.deleted` → costrict-web sync worker → 调 Gitea admin API 移除该用户在所有 private repo 的 collaborator 关系 + 禁用 Gitea 账号。

### 9.3 冲突解决

直推 main 模式下多人协作时，push 冲突由 git 原生机制处理：

| 情况 | 处理 |
|---|---|
| 不同文件 / 同文件不同区域 | `git pull --rebase` 后 push，自动合并 |
| 同文件同区域 | 编辑者本地手动 resolve conflict → commit → push |
| 历史分叉较深 | 推荐 `git merge`（保留双历史）或开 feat 分支走 PR |
| 长期高冲突 | 拆分 repo / 文件粒度，或转为 PR 流程 |

> Branch Protection 禁止 force push main，所以**冲突必须正面解决**，不允许覆盖他人提交。

### 9.4 fork 改进 → PR 贡献流程

适用于：改进官方 repo（`costrict-official/<slug>`）或改进 mirror（`costrict-mirror/<escaped-url>`）。

```
1. 在 Gitea UI fork 目标 repo → u-<username>/<repo-name>
2. git clone u-<username>/<repo-name>
3. git remote add upstream <原 repo>
4. git checkout -b feat/<improvement>
5. 编辑 → commit → git push origin feat/<improvement>
6. POST /api/v1/repos/<原 owner>/<原 repo>/pulls  (创建 PR)
7. capability-check worker 自动在 PR 评论 health + security summary
8. admin / owner 审核 → merge
9. post-merge push webhook → sync worker 覆盖主表
10. （可选）原 repo 留 fork 关系，便于后续持续贡献
```

**审核重点**：
- 内容质量（frontmatter 完整性 / 正文质量）
- 安全扫描结果（`security_scans` 表）
- 健康度状态（`identification_status`）
- 与官方方向一致性（官方 repo 才需此条）

### 9.5 AI agent 代用户协作

| 维度 | 规则 |
|---|---|
| 凭据 | **必须用用户本人 PAT**（D 方案 C 通道）；禁止走 admin PAT 或 bot |
| commit author | 用户本人（PAT 与 user 绑定，无需 `Co-authored-by` trailer） |
| 操作粒度 | 与人类用户完全一致——直推 main / 开 PR / fork |
| 审计 | Gitea audit log 全量归因到用户 |
| 默认通道 | 与人类一致——直推 main |
| AI 起草 + 人类审核 | 公有能力贡献 / 重要变更评审走 PR（§7.2） |

### 9.6 协作建议（最佳实践）

| 改动类型 | 推荐方式 |
|---|---|
| typo / 单字段修正 | 直推 main |
| 单文件结构性改动 | 直推 main |
| 跨多文件改动 | 开 feat 分支 + PR（即使可直推） |
| 公有能力贡献 | 必须走 PR（`costrict-official/` org） |
| 跨业务线变更 | 必须走 PR + 多 reviewer |
| mirror 改进 | 必须 fork 后 PR（mirror read-only） |
| 实验性改动 | 在 `u-<username>/` 下改 `is_private=true` 草稿 |
| 撤销已合并改动 | `git revert`（保留历史） / `git push` 后 admin 标 archived |

### 9.7 通知与订阅

| 事件 | 通知对象 |
|---|---|
| push 到 main | repo watchers（站内 + email） |
| `clean → polluted` | owner + 业务线 editors + watchers |
| `clean → warning` | owner + watchers |
| PR opened / merged | repo owner + reviewers + watchers |
| Health summary 评论 | PR 作者 + reviewers |

---

## 10. 能力项操作流程图（Gitea API 调用链）

> 本章用流程图 + API 清单形式，列出主要操作涉及的 Gitea API 调用点。所有 server 端调用均使用 `costrict-system` admin PAT；用户侧调用（push / clone / PR）走用户本人 PAT。
>
> **核心原则**：server 进程零 `git` 命令，纯 REST API。所有同步通过 Gitea `compare / raw / trees / commits` API 完成。

### 10.1 Gitea API 清单速查

| API endpoint | 用途 | 调用方 |
|---|---|---|
| `POST /git-receive-pack` | git push（HTTPS） | 用户 / AI agent |
| `GET /repos/{owner}/{repo}/info/refs` | git clone / fetch | 用户 / AI agent |
| `POST /api/internal/git-sync` | webhook 投递（非 Gitea API） | Gitea webhook |
| `GET /repos/{owner}/{repo}/compare/{before}...{after}` | 增量 diff（added/modified/removed） | sync worker |
| `GET /repos/{owner}/{repo}/raw/{filepath}?ref={sha}` | 拉单个文件内容 | sync worker / download API |
| `GET /repos/{owner}/{repo}/git/trees/{sha}?recursive=true&per_page=100` | 一次性拉整棵文件树（mirror 初始同步） | sync worker |
| `GET /repos/{owner}/{repo}/git/commits/{sha}` | commit 元数据（author/committer/message） | sync worker |
| `GET /repos/{owner}/{repo}/commits?path={filepath}` | 文件历史 commit 列表（替代 CapabilityVersion） | sync worker / 详情页 |
| `GET /repos/{owner}/{repo}` | repo 配置（`is_private` / `default_branch`） | sync worker（同步 visibility） |
| `GET /repos/{owner}/{repo}/collaborators/{user}/permission` | per-user 权限反查（5min Redis cache） | download API / 详情页 |
| `GET/POST /repos/{owner}/{repo}/pulls` | PR 列表 / 创建 | 用户 / AI agent |
| `POST /repos/{owner}/{repo}/issues/{pr_number}/comments` | PR 评论（health / security summary） | capability-check worker |
| `POST /admin/users` | 创建 Gitea user（auto-provisioning） | fork JWT 中间件（internal `models.CreateUser`） |
| `PATCH /admin/users/{name}` | 禁用登录（`prohibit_login=true`）；**不用于改名**（username immutable） | sync worker（admin PAT） |
| `DELETE /admin/users/{name}` | 删除用户（注销） | sync worker（admin PAT） |
| `POST /repos/{owner}/{repo}/transfer` | repo ownership 转移（注销接管） | sync worker（admin PAT） |
| `DELETE /repos/{owner}/{repo}/collaborators/{username}` | 移除 collaborator（离职清理） | sync worker（admin PAT） |
| `POST /repos/{owner}/{repo}/branch_protections` | Branch Protection 应用 | GiteaConfigSyncWorker |
| `POST /api/internal/quota-cache-invalidate` | 配额 cache 失效（fork 内部） | GiteaConfigSyncWorker |

### 10.2 Create：用户自建 standalone 能力项

```
用户/AI                csc/IDE           Gitea              costrict-web server
   │                      │                 │                      │
   │ 创建 skill           │                 │                      │
   ├─────────────────────►│                 │                      │
   │                      │ git clone/init  │                      │
   │                      ├────────────────►│                      │
   │                      │                 │                      │
   │                      │ git push origin main（用户 PAT）       │
   │                      ├────────────────►│                      │
   │                      │                 │ fork pre-receive hook│
   │                      │                 │ 查 gitea_ext.quota_rules
   │                      │                 │ 校验配额              │
   │                      │  push 成功       │                      │
   │                      │◄────────────────┤                      │
   │                      │                 │                      │
   │                      │                 │ POST webhook (push) ─►  /api/internal/git-sync
   │                      │                 │                      │ HMAC 校验 + Redis SETNX 去重
   │                      │                 │                      │ debounce 1-2s
   │                      │                 │                      │
   │                      │                 │◄─── GET /repos/{o}/{r}/compare/{before}...{after}
   │                      │                 │     → added: ["skill.md"]
   │                      │                 │                      │
   │                      │                 │◄─── GET /repos/{o}/{r}/raw/skill.md?ref={after_sha}
   │                      │                 │     → skill.md 内容
   │                      │                 │                      │
   │                      │                 │◄─── GET /repos/{o}/{r}
   │                      │                 │     → is_private=false
   │                      │                 │                      │
   │                      │                 │                      │ 解析 frontmatter
   │                      │                 │                      │ upsert capability_items
   │                      │                 │                      │   (identification_status=clean/warning)
   │                      │                 │                      │ visibility=public
   │                      │                 │                      │ 异步触发 SecurityScan
   │                      │                 │                      │ 更新 last_synced_commit
   │                      │                 │                      │
   │                      │ GET /api/capabilities?q=<slug>          │
   │                      │ ───────────────────────────────────────►│
   │                      │ 确认已收录                                │
   │                      │◄───────────────────────────────────────┤
```

### 10.3 Read：发现层 vs 内容访问层

> **设计原则**：发现层走 server DB（轻量、可缓存、全量返回带标记）；**内容访问层直连 Gitea**——server 仅返回可解析的 Gitea URL，客户端（portal iframe / csc / AI agent）携带用户 JWT 直接访问 Gitea，由 **fork JWT 中间件 + Gitea 原生权限校验**完成鉴权。
>
> 这样做的好处：
> - **权限校验下沉到 git server**：Gitea 自己最清楚 repo 的 collaborator / visibility，避免 server 维护权限反查 cache（去掉 5min Redis cache + service PAT 调用）
> - **server 零内容代理**：不再调 Gitea raw API 拉文件，降低 server 负载（尤其大文件）
> - **审计链路最短**：用户 → Gitea（无 server 中间环节）
> - **统一 JWT 透传**：与 §6.6 fork JWT 中间件双通道 fallback 设计对齐（Authorization header + 同域 cookie）

#### 10.3.1 字段约定（API 响应）

`GET /api/capabilities` 与 `GET /api/capabilities/:id` 在 DB 字段之外，额外返回以下 Gitea 直连 URL：

| 字段 | 形式 | 用途 |
|---|---|---|
| `gitea_tree_url` | `<tenant_gitea_base_url>/<owner>/<repo>/-/tree/<ref>/<path>` | 浏览器跳转 / iframe 嵌入（Gitea 文件树页面） |
| `gitea_raw_url` | `<tenant_gitea_base_url>/<owner>/<repo>/raw/<ref>/<path>` | 直接拉取文件原文 |
| `gitea_clone_url` | `<tenant_gitea_base_url>/<owner>/<repo>.git` | `git clone`（私有 repo 需 PAT） |
| `gitea_commits_url` | `<tenant_gitea_base_url>/<owner>/<repo>/commits/<ref>/<path>` | 文件历史 |
| `gitea_compare_url` | `<tenant_gitea_base_url>/<owner>/<repo>/-/compare/<before>...<after>` | diff 视图 |

> **多租户 base_url 解析**：`<tenant_gitea_base_url>` 由 server 端从 `tenant_configs.<JWT.tenant_id>.git.base_url` 取得并**拼接为绝对 URL**返回（详见 §1.5）。客户端**禁止**自行拼接 `gitea.costrict.local` 前缀；所有 `gitea_*_url` 字段均为 ready-to-use 绝对地址。下文示例中以 `https://gitea.costrict.local/...` 书写，按 §1.5.6 文档约定应理解为当前 tenant 的 base_url 占位符。

#### 10.3.2 JWT 透传双通道（与 §6.6 fork JWT 中间件对齐）

| 客户端 | 通道 | 说明 |
|---|---|---|
| portal iframe | HttpOnly cookie `costrict_jwt`（domain=`.costrict.local`） | 同域自动携带；fork 中间件 cookie fallback 读取 |
| csc / SDK | HTTP header `Authorization: Bearer <jwt>` | 主通道；fork 中间件优先读取 |
| AI agent | HTTP header `Authorization: Bearer <jwt>` | 同 csc |
| 浏览器直接访问（罕见） | cookie + 跳转 | 用户从 portal 点击跳到 Gitea，cookie 自动携带 |

fork JWT 中间件按 §6.6 实现：优先读 header，fallback 读 cookie；同一套 JWKS 验证逻辑。

#### 10.3.3 流程图

```
csc / AI / browser               costrict-web server              Gitea (fork JWT 中间件)
   │                                  │                                  │
   │  GET /api/capabilities（发现层）   │                                  │
   ├──────────────────────────────────►│                                  │
   │                                  │ 纯 DB 查询（不过滤权限）          │
   │  全量列表 + visibility + owner   │                                  │
   │  + gitea_tree_url / raw_url      │                                  │
   │◄──────────────────────────────────┤                                  │
   │                                  │                                  │
   │  GET /api/capabilities/:id（详情入口）                              │
   ├──────────────────────────────────►│                                  │
   │                                  │ 纯 DB 查询                       │
   │                                  │ （不调 Gitea permission API）    │
   │  返回 metadata + gitea_*_url     │                                  │
   │  （不返回文件内容）                │                                  │
   │◄──────────────────────────────────┤                                  │
   │                                  │                                  │
   │  ──────── 客户端直连 Gitea ────────                                  │
   │                                  │                                  │
   │  GET <gitea_raw_url>             │                                  │
   │  Authorization: Bearer <jwt>     │                                  │
   │  （portal iframe 走同域 cookie）  │                                  │
   ├──────────────────────────────────────────────────────────────────►│
   │                                  │  fork JWT 中间件:                │
   │                                  │  ├─ 验证 JWT 签名（JWKS, 5min cache）
   │                                  │  ├─ auto-provision（首次见到 user）
   │                                  │  └─ Gitea 原生权限校验           │
   │                                  │     public repo → 所有认证用户   │
   │                                  │     private repo → collaborator  │
   │                                  │                                  │
   │  200 OK + 文件原文               │                                  │
   │  / 401（JWT 无效 / 过期）         │                                  │
   │  / 403（无访问权限）              │                                  │
   │◄──────────────────────────────────────────────────────────────────┤
   │                                  │                                  │
   │  GET /api/items/:id/download（兼容入口，可选）                      │
   ├──────────────────────────────────►│                                  │
   │                                  │ 不查权限、不拉内容               │
   │                                  │ 直接构造 Gitea raw URL           │
   │  302 Location: <gitea_raw_url>   │                                  │
   │◄──────────────────────────────────┤                                  │
   │                                  │                                  │
   │  浏览器自动跟随 redirect          │                                  │
   │  （同域 cookie 自动携带）         │                                  │
   ├──────────────────────────────────────────────────────────────────►│
   │  ...（同上 JWT 校验 + 原生权限）  │                                  │
   │                                  │                                  │
   │  GET /api/capabilities/:id/history（版本历史，仅返回 URL）          │
   ├──────────────────────────────────►│                                  │
   │  返回 gitea_commits_url           │                                  │
   │◄──────────────────────────────────┤                                  │
   │                                  │                                  │
   │  GET <gitea_commits_url>          │                                  │
   │  Authorization: Bearer <jwt>     │                                  │
   ├──────────────────────────────────────────────────────────────────►│
   │  commit list                     │                                  │
   │◄──────────────────────────────────────────────────────────────────┤
```

#### 10.3.4 与旧方案（server 代理）对比

| 维度 | 旧方案（server 代理） | **新方案（直连 Gitea）** |
|---|---|---|
| 权限校验位置 | server 调 Gitea permission API + 5min Redis cache | **Gitea fork 中间件 + 原生 collaborator**（无 cache 一致性问题） |
| 文件内容拉取 | server 用 service PAT 调 Gitea raw API | **客户端直接 GET Gitea raw URL**（透传用户 JWT） |
| service PAT 用于 download | 需要（fine-grained，限定 owner） | **不需要**（仅保留 sync worker / admin 操作的 PAT） |
| server 负载 | 高（每个 download 都经过 server） | **低**（仅返回 URL，不代理内容） |
| 大文件下载 | 占用 server 内存 / 带宽 | **直连 Gitea**（Gitea 直接 stream） |
| 审计链路 | 用户 → server → Gitea | **用户 → Gitea** |
| 客户端复杂度 | 简单（统一调 server API） | 略增（需处理 302 / 直接访问 Gitea） |
| 跨域问题 | 无 | 同域部署（`.costrict.local`）+ CORS 配置；csc / AI 直接走 HTTPS |
| 失败响应 | server 包装后返回 | **Gitea 原生返回**（401 / 403 / 404） |

#### 10.3.5 失败响应处理

| HTTP 状态 | 含义 | 客户端处理建议 |
|---|---|---|
| 200 | 文件原文 / metadata | 正常处理 |
| 401 | JWT 无效 / 过期 | csc / AI：刷新 JWT 或引导用户重新登录；portal：跳登录页 |
| 403 | repo 私有 + 用户非 collaborator | 显示"无访问权限，请联系 admin 或申请 collaborator" |
| 404 | repo / 文件 / ref 不存在 | 显示"能力项不存在或已删除"；可能是 archived |
| 429 | Gitea API 限流 | 指数退避重试 |
| 5xx | Gitea 故障 | 显示"Gitea 暂时不可用"；server 端告警 |

#### 10.3.6 私有 repo 的 git clone

`git clone` 走 Gitea 原生 git 协议（HTTP / SSH），与 JWT 透传分离：

| 通道 | 凭据 | 适用 |
|---|---|---|
| HTTPS + 用户 PAT | fine-grained PAT（§12.1） | csc / AI agent / IDE |
| HTTPS + basic auth（username + password） | costrict-web 密码（fork 中间件兼容） | 临时使用 |
| SSH + SSH key | 用户 SSH 公钥（§12.2） | 长期开发 |

> Gitea fork JWT 中间件当前覆盖 **REST API + web 路由**，不覆盖 git 协议（`/git-receive-pack` / `/info/refs`）；git 协议仍走 PAT / SSH key 原生鉴权。如需让 git 协议也支持 JWT，可在 fork 中扩展（HTTP basic auth 桥接 JWT），暂 out of scope。

#### 10.3.7 兼容入口（可选保留）

为支持不支持 JWT 透传的旧客户端 / 临时分享链接，server 可保留兼容入口：

```
GET /api/items/:id/download
  → 不查权限、不拉内容
  → 302 Location: <gitea_raw_url>
```

浏览器自动跟随 redirect，同域 cookie 自动携带；csc / AI 拿到 302 后，需主动设置 `Authorization: Bearer <jwt>` 重新请求 Gitea。

> **建议**：长期目标是将所有客户端迁移到直连 Gitea 模式，移除 `/api/items/:id/download` 入口；过渡期保留作为兼容。

### 10.4 Update：直推 main vs PR

**直推 main（默认通道）**：

```
用户/AI                Gitea                                      costrict-web server
   │                      │                                              │
   │ git push origin main │                                              │
   ├─────────────────────►│                                              │
   │                      │ pre-receive hook（配额校验）                  │
   │                      │ POST webhook (push) ──────────────────────►   │
   │                      │                                              │ compare {before}...{after}
   │                      │◄─────────────────────────────────────────── │
   │                      │ raw / trees / commits API（如 10.2）         │
   │                      │                                              │ upsert capability_items
   │                      │                                              │ SecurityScan 异步触发
   │                      │                                              │ health_issues 更新（如有）
   │                      │                                              │
   │                      │ capability-check worker（健康度）             │
   │                      │ 拉文件树 → 启发式识别 → schema 校验            │
   │                      │ 写 health_issues 主表                        │
```

**PR 通道（可选 / 公有贡献）**：

```
用户/AI                Gitea                                      costrict-web server
   │                      │                                              │
   │ git push feat/branch │                                              │
   ├─────────────────────►│                                              │
   │                      │                                              │
   │ POST /repos/{o}/{r}/pulls（创建 PR）                                 │
   ├─────────────────────►│                                              │
   │  PR created          │                                              │
   │◄─────────────────────┤                                              │
   │                      │ POST webhook (pull_request opened) ────────►  │
   │                      │                                              │ capability-check worker
   │                      │                                              │   compare base...head
   │                      │                                              │   raw PR head 文件
   │                      │                                              │   写 health_issues (PR head SHA 维度)
   │                      │                                              │ SecurityScan trigger_type=pr-check
   │                      │                                              │   （不覆盖主表）
   │                      │                                              │
   │                      │ POST /repos/{o}/{r}/issues/{pr}/comments ── │
   │                      │   health + security summary                  │
   │                      │                                              │
   │ admin 审核 → merge   │                                              │
   │                      │ POST webhook (push post-merge) ───────────► │
   │                      │                                              │ sync worker 覆盖主表
   │                      │                                              │ SecurityScan trigger_type=git-push
   │                      │                                              │   覆盖 capability_items.security_status
```

### 10.5 Delete：归档 / 行政下架 / 注销接管

```
场景                          Gitea API 调用                              server 行为
─────────────────────────────────────────────────────────────────────────────────────
作者 Git 删除顶层 metadata     git push (removed: skill.md)                sync worker 标 status='archived'
                              POST webhook (push) → compare {before}...{after}
                                                                          → added/modified/removed
                                                                          → removed: ["skill.md"]
                                                                          → UPDATE capability_items SET status='archived'

行政下架（违规 / 安全）         无 Gitea 调用                                admin UI 标 status='banned'
                                                                          （DB only，不动 Git）

误删恢复                      git revert → push main                      sync worker 标 status='active'
                              POST webhook → compare 标 added             upsert capability_items

用户注销（ownership 接管）      ① GET /admin/users/{name}/repos            sync worker 列出用户所有 repo
                              ② POST /repos/{o}/{r}/transfer             ownership 转移给 costrict-system
                                  body: {"new_owner":"costrict-system"}
                              ③ DELETE /admin/users/{name}                删除 Gitea 用户
                              ④ DELETE /repos/{o}/{r}/collaborators/{user}（其他 repo）
                                                                          repo URL 自动 redirect
                                                                          commit 历史保留

物理删除（极端）               DELETE /repos/{owner}/{repo}                admin 手动（非默认）
                                                                          全程审计；DB 同步清理
```

### 10.6 Fork 改进 → PR 贡献（含权限校验）

```
用户                  Gitea                                       costrict-web server
   │                    │                                                │
   │ POST /repos/{costrict}/{slug}/forks（fork 到 u-<username>/）        │
   ├───────────────────►│                                                │
   │  fork created      │                                                │
   │◄───────────────────┤                                                │
   │                    │                                                │
   │ git clone u-<username>/<slug>                                       │
   ├───────────────────►│                                                │
   │ git checkout feat/<branch>                                          │
   │ 改 skill.md → commit → push                                         │
   ├───────────────────►│                                                │
   │                    │ POST webhook (push) ────────────────────────► │
   │                    │                                                │ sync worker 同步 fork 内容
   │                    │                                                │
   │ POST /repos/{costrict}/{slug}/pulls（创建 PR）                      │
   ├───────────────────►│                                                │
   │  PR created        │                                                │
   │◄───────────────────┤                                                │
   │                    │ POST webhook (pull_request opened) ─────────► │
   │                    │                                                │ capability-check worker
   │                    │                                                │   compare base...head
   │                    │                                                │   raw PR head 文件
   │                    │                                                │   启发式识别 + schema 校验
   │                    │                                                │ SecurityScan trigger_type=pr-check
   │                    │                                                │
   │                    │ POST /repos/{costrict}/{slug}/issues/{pr}/comments
   │                    │◄──────────────────────────────────────────── │
   │                    │   health + security summary                    │
   │                    │                                                │
   │ admin 审核 → merge │                                                │
   │                    │ POST webhook (push post-merge) ─────────────►│
   │                    │                                                │ sync worker 覆盖主表
   │                    │                                                │ Gitea 原生 redirect 原 u-<username>/<slug>
   │                    │                                                │ DB source_repo_url 自动跟随
```

### 10.7 Mirror 同步（Gitea 周期 pull）

```
Gitea cron                  Gitea mirror worker         上游 GitHub        costrict-web server
   │                            │                          │                    │
   │ 24h trigger                │                          │                    │
   ├───────────────────────────►│                          │                    │
   │                            │ git fetch upstream       │                    │
   │                            ├─────────────────────────►│                    │
   │                            │   upstream commits       │                    │
   │                            │◄─────────────────────────┤                    │
   │                            │                          │                    │
   │                            │ 内部生成 push 事件（post-mirror）              │
   │                            │ POST webhook (push) ────────────────────────►│
   │                            │                                                │ sync worker
   │                            │                                                │   compare {before}...{after}
   │                            │                                                │   raw / trees API
   │                            │                                                │ upsert capability_items
   │                            │                                                │
   │                            │                                                │ health 检测（如 SKILL.md 消失）
   │                            │                                                │   → polluted
   │                            │                                                │ 通知 owner
```

### 10.8 Mirror 初始批量同步（首次接入）

```
admin 配置 mirror             Gitea                    costrict-web server
   │                            │                          │
   │ Gitea UI 配置 mirror        │                          │
   ├───────────────────────────►│                          │
   │                            │ git clone --mirror        │
   │                            │ 首次 mirror pull 完成     │
   │                            │ POST webhook (before=0000...) ─────►│
   │                            │                          │ sync worker
   │                            │                          │   detect first-sync
   │                            │                          │
   │                            │◄────────────────────── GET /repos/{o}/{r}/git/trees/{after_sha}?recursive=true&per_page=100
   │                            │   分页拉完整文件列表     │
   │                            │──────────────────────►│
   │                            │                          │ 筛选顶层 metadata 文件
   │                            │                          │   - 根目录 skill.md / subagent.md / command.md / mcp.md / .plugin.json
   │                            │                          │
   │                            │◄──────────────────────  并发调用 raw API（≤ 20 并发）
   │                            │   每个 metadata 文件内容 │
   │                            │──────────────────────►│
   │                            │                          │ 解析 → 批量 upsert DB
   │                            │                          │ 触发批量 SecurityScan
```

### 10.9 兜底巡检（每小时）

```
costrict-web cron            costrict-web server           Gitea
   │                            │                            │
   │ 每小时触发                  │                            │
   ├───────────────────────────►│                            │
   │                            │ 列出所有 capability_registries
   │                            │ 循环每个 registry：         │
   │                            │                            │
   │                            │ GET /repos/{o}/{r}/commits?sha={default_branch}&per_page=1
   │                            ├───────────────────────────►│
   │                            │   latest commit SHA        │
   │                            │◄───────────────────────────┤
   │                            │                            │
   │                            │ 比对 last_synced_commit    │
   │                            │ if (sha != last_synced) {  │
   │                            │   触发 sync worker          │
   │                            │ }                          │
   │                            │                            │
   │                            │ 覆盖 webhook 丢失场景      │
```

### 10.10 平台配置变更（GitOps）

```
admin                costrict-config/platform-config      Gitea              costrict-web server
   │                      │                                  │                      │
   │ git clone            │                                  │                      │
   │ 编辑 .gitea/quota.yaml                                  │                      │
   │ git commit + push    │                                  │                      │
   ├─────────────────────►│                                  │                      │
   │                      │                                  │ POST webhook (push) ─►
   │                      │                                  │                      │ GiteaConfigSyncWorker
   │                      │                                  │                      │   diff .gitea/*.yaml
   │                      │                                  │                      │   解析 yaml
   │                      │                                  │                      │
   │                      │                                  │                      │ 写 gitea_ext.quota_rules
   │                      │                                  │                      │
   │                      │                                  │ POST /api/internal/quota-cache-invalidate
   │                      │                                  │◄─────────────────────│
   │                      │                                  │ fork hook cache 失效 │
   │                      │                                  │                      │
   │                      │                                  │                      │ 其他 yaml 变更：
   │                      │                                  │                      │   POST /repos/{o}/{r}/branch_protections
   │                      │                                  │                      │   POST /admin/teams（teams.yaml）
   │                      │                                  │                      │   POST /admin/hooks（webhooks.yaml）
   │                      │                                  │                      │   POST /repos/{o}/{r}/labels（labels.yaml）
```

### 10.11 AI agent 代用户操作

```
AI agent              csc                  Gitea              costrict-web server
   │                    │                     │                     │
   │ 用户授权 PAT       │                     │                     │
   ├───────────────────►│                     │                     │
   │                    │                     │                     │
   │ 任务：新增 skill                                                │
   │ GET /api/capabilities?q=<slug>                                  │
   │ ───────────────────────────────────────────────────────────────►│
   │ 确认 slug 未占用                                                │
   │◄───────────────────────────────────────────────────────────────┤
   │                    │                     │                     │
   │ git clone / init                                                │
   │ 写 skill.md                                                    │
   │ git push origin main（用户 PAT，commit author = 用户本人）      │
   │ ─────────────────►│                     │                     │
   │                    ├────────────────────►│                     │
   │                    │                     │ POST webhook (push) │
   │                    │                     ├────────────────────►│
   │                    │                     │                     │ sync worker（同 §10.2）
   │                    │                     │                     │
   │                    │                     │                     │ Gitea audit log
   │                    │                     │                     │ 归因到用户本人 PAT
   │                    │                     │                     │
   │ 全程与人类用户操作完全一致                                        │
   │ 无 admin PAT / 无 bot / 无特殊通道                              │
```

### 10.12 用户生命周期级联（webhook 广播）

> **username 不可变**：注册即冻结，无 `user.updated.username_change` 事件，无 Gitea `PATCH /admin/users/{name}` 改名调用。display_name / email / avatar 等可变字段不影响 Gitea user 模型，仅在 costrict-web `user_profile` 表更新。注销后若用户希望换 username，走「`user.deleted` 流程 + 注册新账号 + 申请 ownership transfer」三步（admin 介入）。

```
costrict-web user event       costrict-web server          Gitea
   │                              │                            │
   │ user.disabled                 │                            │
   ├─────────────────────────────►│                            │
   │                              │ PATCH /admin/users/{name}  │
   │                              │   body: {"login_name":"...","prohibit_login":true}
   │                              ├───────────────────────────►│
   │                              │                            │
   │                              │ 遍历该用户参与的 private repo │
   │                              │ DELETE /repos/{o}/{r}/collaborators/{user}
   │                              ├───────────────────────────►│
   │                              │                            │
   │ user.deleted                  │                            │
   ├─────────────────────────────►│                            │
   │                              │ ① GET /admin/users/{name}/repos
   │                              ├───────────────────────────►│
   │                              │   repo 列表                 │
   │                              │◄───────────────────────────┤
   │                              │                            │
   │                              │ ② 对每个 repo：              │
   │                              │   POST /repos/{o}/{r}/transfer
   │                              │   body: {"new_owner":"costrict-system"}
   │                              ├───────────────────────────►│
   │                              │                            │
   │                              │ ③ DELETE /admin/users/{name}│
   │                              ├───────────────────────────►│
```

---

## 11. 平台引导式创建能力项（Git-first）

> **设计哲学**：与 v4 提案 §2.1 目标 #5「AI 操作原生 Git」对齐——平台 portal 仅作**引导 wizard**（slug 校验 + 命名建议 + 脚手架 init 命令模板 + 只读预览），repo 创建与文件初始化由用户 / AI 在 Git 上直接完成（v2.14 起 init 直接 `git clone costrict-template/templates`，无 server 中间环节）。**平台不代替用户调 Gitea contents API 写入文件**。
>
> 这样所有 commit author 直接归因到用户本人（无 server 中间环节），与 §9 / §10 的协作 / 流程图模型完全一致。

### 11.1 与原方案对比

| 维度 | 旧方案（平台自动创建） | **新方案（Git-first 引导）** |
|---|---|---|
| 创建动作发起方 | costrict-web server（代用户调 Gitea API） | **用户 / AI agent**（直接 git push） |
| Gitea 写入 API | server 调 `POST /user/repos` + `POST /contents/*` | **无**（用户走 Gitea 原生 git 协议） |
| commit author | 用户 PAT（server 转发） | 用户 PAT（直接 push） |
| 审计链路 | server → Gitea | 用户 → Gitea（**无中间环节**） |
| 平台复杂度 | 高（错误处理 / 回滚 / 多文件初始化） | **低**（仅校验 + 命令模板 / 只读预览） |
| 用户体验 | 一键创建（限制多） | 复制命令到本地（灵活） |
| 与 v4 提案对齐 | 部分（server 介入内容创建） | **完全**（Git 为真相源，平台仅发现 + 索引） |
| AI 友好性 | 中（需学 REST API） | **高**（原生 git 流程） |

### 11.2 三种创建入口

| 入口 | 用户场景 | 创建动作 | 平台介入程度 |
|---|---|---|---|
| **平台引导 wizard**（默认推荐） | 普通用户首次创建 | 平台校验 + 提供命令模板（v2.14 起 init 从 `costrict-template/templates` git clone） + 只读预览；用户在本地 / IDE 执行 git 命令 | 中（仅引导） |
| **直接 git 命令** | 熟悉 git 的开发者 | 用户直接 `git init` + `git remote add` + push；完全跳过 portal | 无 |
| **AI agent 自动化** | AI 代用户操作 | AI 直接走 git 命令链路（与人类用户完全一致）；可选调用 slug-check API | 低（仅可选校验） |

### 11.3 适用场景

| 场景 | 是否走本章流程 |
|---|---|
| 用户在 portal 点"新建能力项"（wizard 引导） | ✅ |
| AI agent 代用户创建 | ✅（直接 git，可选 slug-check） |
| 熟悉 git 的用户直接 clone + push | ✅（跳过 wizard，最终殊途同归） |
| admin 创建官方能力项 | ✅（同样归到 `u-<admin>/`；升级 `costrict-official/` 走 PR §9.4） |
| 上游 mirror 接入 | ❌（admin 在 Gitea UI 配置 mirror pull） |
| **Standalone plugin 创建**（`u-<username>/<plugin-slug>`，根目录 `.plugin.json`） | ✅（与其他 standalone 类型同链路；plugin 是 source-agnostic——可来自 marketplace catalog 或 git 自托管，详见 §11.11） |
| 用户 fork 上游 / 官方 | ❌（走 Gitea 原生 fork-to-personal） |

### 11.4 Namespace 决议（统一规则）

> **v2.10 原则**：repo 归属与 type **完全解耦**——所有用户创建的 repo（任何 type）一律落 `u-<username>/`。官方 org（`costrict-official/`）仅作 PR 升级目标，不由用户直接创建。

| 维度 | 规则 |
|---|---|
| 默认 namespace | `u-<username>/`（用户首次访问 Gitea 时由 fork JWT 中间件 auto-provision）——**所有 type 共用** |
| 用户可选范围 | **不允许选择其他 namespace**（即使是 admin 也走 `u-<admin-username>/`） |
| 升级为官方 | → `costrict-official/<slug>`（同 §9.4 PR 流程；slug 在 `costrict-official/` 内唯一） |
| AI agent 代用户 | 归属到 AI 代表的用户（即 PAT 持有者），**严禁**归到 `costrict-system` 或 bot |
| 未绑定 Gitea 的用户 | wizard 警告，引导访问 Gitea 完成首次登录 |
| **Gitea 原生 auto-create** | **启用 `[repository] ENABLE_PUSH_CREATE_USER = true`**：用户 push 到不存在的 `u-<username>/<new-repo>.git` 时，Gitea 自动创建空 repo；type 由 sync worker 按顶层 metadata 文件名自动判定（`skill.md` → skill / `.plugin.json` → plugin / 等） |

### 11.5 Repo 名称生成算法

#### 11.5.1 命名规则（强约束，与 §4.1 一致）

```
正则: ^[a-z][a-z0-9-]{1,62}[a-z0-9]$
```

| 规则 | 说明 |
|---|---|
| 字符集 | 仅 `[a-z0-9-]`，禁止大写 / 下划线 / 中文 / 特殊符号 |
| 首字符 | 必须字母（避免与 Gitea 内部 ID 冲突） |
| 末字符 | 字母或数字（不允许 `-` 结尾） |
| 长度 | 3–64 字符 |
| 连续连字符 | 不允许 `--` |
| 全数字 | 不允许 |

#### 11.5.2 Slug 自动改写规则（wizard 实时应用）

| 用户输入 | 改写后 | 规则 |
|---|---|---|
| `Skill Vetter` | `skill-vetter` | 小写 + 空格转 `-` |
| `skill_vetter` | `skill-vetter` | 下划线转 `-` |
| `SkillVetter` | `skill-vetter` | 驼峰拆分 + 小写 |
| `-skill-vetter` / `skill-vetter-` | `skill-vetter` | 去掉首尾 `-` |
| `skill--vetter` | `skill-vetter` | 合并多个 `-` |
| `123-skill` | `s123-skill` | 数字开头加 `s` 前缀 |
| `AI 助手` | `ai-zhu-shou` | 中文转拼音（无法识别时拒绝） |

#### 11.5.3 保留字黑名单

slug 不可命中：

| 类别 | 保留字 |
|---|---|
| Gitea 路由 | `admin`、`api`、`explore`、`issues`、`pulls`、`notifications`、`settings`、`search`、`login`、`signup`、`org`、`user`、`repo`、`stars`、`following`、`migrate`、`auth`、`dev`、`swagger`、`assets`、`avatars`、`attachments` |
| 通用协议 | `www`、`mail`、`ftp`、`smtp`、`pop`、`imap`、`ns`、`dns`、`localhost` |
| Windows 文件系统 | `con`、`prn`、`aux`、`nul`、`com1`–`com9`、`lpt1`–`lpt9` |
| Gitea 内部 | 以 `-` 开头（如 `-git`、`-templates`） |
| CoStrict 平台 | `costrict`、`costrict-config`、`costrict-official`、`costrict-template`、`costrict-mirror`、`costrict-system`、`platform-config`、`templates` |
| Git 特殊 | `.git`、`.gitea`、`.github`、`.gitignore`、`.gitattributes` |

#### 11.5.4 推荐命名约定（非强制，wizard 显示占位符）

| 能力类型 | 推荐 slug | namespace | 示例 |
|---|---|---|---|
| skill | `skill-<subject>` 或 `<subject>` | `u-<username>/` | `skill-vetter` |
| subagent | `<subject>-subagent` | `u-<username>/` | `code-reviewer-subagent` |
| command | `<subject>-command` | `u-<username>/` | `refactoring-command` |
| mcp | `<subject>-mcp` | `u-<username>/` | `filesystem-mcp` |
| **plugin** | `<subject>-plugin` 或 `<vendor>-<subject>-plugin` | `u-<username>/` | `vetter-plugin` / `acme-fs-plugin` |

### 11.6 Slug 校验 API（只读，wizard 实时调用）

```
GET /api/capabilities/slug-check?slug=<slug>&type=skill
```

**响应示例（可用 / standalone）**：

```json
{
  "slug": "skill-vetter",
  "is_valid": true,
  "is_available": true,
  "normalized": "skill-vetter",
  "reserved_reason": null,
  "suggestions": [],
  "preview": {
    "namespace": "u-alice",
    "repo_name": "skill-vetter",
    "metadata_path": "skill.md",
    "repo_url": "https://gitea.costrict.local/u-alice/skill-vetter",
    "clone_url": "https://gitea.costrict.local/u-alice/skill-vetter.git"
  }
}
```

**响应示例（可用 / standalone plugin）**：

```json
{
  "slug": "vetter-plugin",
  "is_valid": true,
  "is_available": true,
  "normalized": "vetter-plugin",
  "reserved_reason": null,
  "suggestions": [],
  "preview": {
    "namespace": "u-alice",
    "repo_name": "vetter-plugin",
    "metadata_path": ".plugin.json",
    "repo_url": "https://gitea.costrict.local/u-alice/vetter-plugin",
    "clone_url": "https://gitea.costrict.local/u-alice/vetter-plugin.git"
  }
}
```

**响应示例（已占用）**：

```json
{
  "slug": "skill-vetter",
  "is_valid": true,
  "is_available": false,
  "suggestions": ["skill-vetter-2", "alice-skill-vetter", "skill-vetter-x7k2"]
}
```

**响应示例（不合规）**：

```json
{
  "slug": "Admin",
  "is_valid": false,
  "errors": [
    {"code": "RESERVED", "message": "slug 命中 Gitea 路由保留字 'admin'"},
    {"code": "UPPERCASE", "message": "slug 必须全小写"}
  ],
  "normalized": "admin-tools",
  "suggestions": ["admin-tools", "tools-admin"]
}
```

**校验逻辑**：
1. 字符集 / 长度 / 首末字符 / 连续 `-` 检查
2. 保留字黑名单匹配
3. namespace 永远是 `u-<username>/`（所有 type 共用）
4. 查 `capability_items` DB（namespace 内是否已存在）
5. 兜底 GET `/repos/u-<username>/<slug>`（确认 Gitea 上 repo 是否存在）

**关键约束**：此 API **只读**，不创建任何资源。

### 11.7 模板仓库与 init 流程（git-first，从 `costrict-template/templates` 拉脚手架）

> **v2.14 设计原则**：既然 `costrict-template/templates` 已经是模板真相源，wizard 的脚手架 init 应**直接 `git clone` 该 mono-repo 的对应 `<type>/` 子目录**，不再走 server HTTP 模板下载 API。`GET /api/capabilities/templates/{type}` 仅保留为**只读预览 API**（portal 实时显示模板内容 + 占位符替换预览），不参与实际 init 动作。这样：
> - 与 §11 "Git-first 引导模式" 完全对齐——init 一律走 git，无 server 中间环节
> - 模板可以是**多文件脚手架**（如 plugin type 含 `.plugin.json` + `index.js` + `README.md`），而非单文件
> - 模板版本化天然支持（pin 到 `<tag>` / `<branch>` / `<commit>`）
> - tenant fork `costrict-template/templates` 自定义模板时，init 流程不变（csc `--from` 参数指向 tenant fork URL）

#### 11.7.1 模板仓库结构（mono-repo，按 type 分子目录）

```
costrict-template/templates/                # public mono-repo
├── README.md                               # 仓库级 README：模板维护指南
├── skill/
│   ├── skill.md.tmpl                       # 必填：顶层 metadata
│   ├── README.md.tmpl                      # 可选：repo README
│   └── .gitignore                          # 可选：通用忽略规则
├── subagent/
│   └── agent.md.tmpl
├── command/
│   └── commands/
│       └── <slug>.md.tmpl                  # <slug> 在 apply 阶段替换为实际 slug
├── mcp/
│   └── mcp.json.tmpl
└── plugin/
    ├── .plugin.json.tmpl                   # 必填：根目录 metadata
    ├── index.js.tmpl                       # 可选：entrypoint 骨架
    └── README.md.tmpl
```

#### 11.7.2 init 流程（csc 一键命令，推荐）

```bash
csc capability init \
  --type=skill \
  --slug=skill-vetter \
  --name="Skill Vetter" \
  --description="..." \
  --from=https://gitea.costrict.local/costrict-template/templates.git \
  --branch=main
```

csc 内部执行（v2.14 实现）：
1. `git clone --depth 1 --filter=blob:none --sparse <from> --branch <branch> <slug>` 拉 mono-repo（无 blob、仅 commit 元数据）
2. `cd <slug> && git sparse-checkout set <type>/` 只检出对应子目录
3. 把 `<type>/*` 上移到根目录、清空 `<type>/` 前缀
4. 占位符替换（`{{SLUG}}` / `{{NAME}}` / `{{DESCRIPTION}}` / `{{USERNAME}}` / `{{DATE}}`）
5. 去掉 `.tmpl` 后缀；对 `command` type 还需把 `commands/<slug>.md.tmpl` 改名为 `commands/<实际-slug>.md`
6. `rm -rf .git && git init -b main` 重置 history（drop 模板仓库 commit 链）
7. `git add . && git commit -m "feat(<type>): init <slug> from costrict-template/templates"`
8. `git remote add origin https://gitea.costrict.local/u-<username>/<slug>.git && git push -u origin main`

#### 11.7.3 init 流程（纯 git，无 csc）

```bash
# 1. sparse clone mono-repo
git clone --depth 1 --filter=blob:none --sparse \
  https://gitea.costrict.local/costrict-template/templates.git \
  skill-vetter
cd skill-vetter
git sparse-checkout set skill/

# 2. 上移到根目录、清空子目录前缀
mv skill/* skill/.* . 2>/dev/null
rmdir skill

# 3. 替换占位符（sed 或手工）
find . -type f -name "*.tmpl" -exec sed -i \
  -e 's/{{SLUG}}/skill-vetter/g' \
  -e 's/{{NAME}}/Skill Vetter/g' \
  -e 's/{{DESCRIPTION}}/<描述>/g' \
  -e 's/{{USERNAME}}/alice/g' \
  -e 's/{{DATE}}/2026-07-13/g' \
  {} \;

# 4. 去掉 .tmpl 后缀；command type 还需把 <slug>.md 改为实际 slug
find . -type f -name "*.tmpl" | while read f; do git mv "$f" "${f%.tmpl}"; done
# command type 专用：[ -f "commands/<slug>.md" ] && git mv "commands/<slug>.md" "commands/skill-vetter.md"

# 5. 编辑正文（补充 skill 内容 / 调整 frontmatter）
$EDITOR skill.md

# 6. drop 模板 commit 历史、re-init
rm -rf .git
git init -b main
git add .
git commit -m "feat(skill): init skill-vetter from costrict-template/templates"

# 7. 推送到用户 namespace（auto-create）
git remote add origin https://gitea.costrict.local/u-alice/skill-vetter.git
git push -u origin main
```

#### 11.7.4 只读预览 API（portal 实时显示用）

```
GET /api/capabilities/templates/{type}?slug=<slug>&name=<name>&description=<desc>
```

**v2.14 起此 API 仅作 portal 预览**：服务端从 `costrict-template/templates` mono-repo 取 `<type>/` 子目录（5min cache + webhook 失效），替换占位符后返回**预览文本**（用户在 wizard Step 4 实时看到将要生成的文件内容）。**实际 init 不走此 API**——一律走 §11.7.2 / §11.7.3 的 `git clone` 流程。

> **多租户覆盖**：tenant 可在自己 Gitea 实例 fork `costrict-template/templates`，自定义模板（如调整 frontmatter 默认值、补充 tenant 专属 license）；csc `--from` 参数 + server 预览 API 都按 `tenant_configs.<tenant_id>.template.repo` 决议拉哪份（默认回退到 platform-shared `costrict-template/templates`）。

**占位符**（apply 阶段由 csc 或用户 sed 替换）：
- `{{SLUG}}` → 用户填写的 slug
- `{{NAME}}` → 用户填写的 name
- `{{DESCRIPTION}}` → 用户填写的 description
- `{{USERNAME}}` → 当前用户 username
- `{{DATE}}` → 当前 ISO 日期

### 11.8 平台引导 wizard 流程

```
用户                       costrict-web portal                  Gitea
   │                              │                                  │
   │ 点击"新建能力项"               │                                  │
   ├─────────────────────────────►│                                  │
   │                              │                                  │
   │ Step 1: 选择 type             │                                  │
   │   ◉ skill / subagent / command / mcp / plugin                │
   │   （所有 type 共用 `u-<username>/` namespace）   │
   │                              │                                  │
   │ Step 2: 填写 slug            │                                  │
   │   [skill-vetter________]     │                                  │
   │   实时 GET /slug-check       │                                  │
   │   ├─────────────────────────►│                                  │
   │   ✓ 可用 / ✗ 已占用           │                                  │
   │   ◄─────────────────────────┤                                  │
   │                              │                                  │
   │ Step 3: 填写 name/description/license/tags                       │
   │                              │                                  │
   │ Step 4: 实时预览（按 type 切换 namespace）                              │
   │   ┌─────────────────────────────────────────────┐ │                                  │
   │   │ type=skill / subagent / command / mcp：      │ │                                  │
   │   │   namespace: u-alice                         │ │                                  │
   │   │   repo: skill-vetter                         │ │                                  │
   │   │   file: skill.md                             │ │                                  │
   │   │   URL: gitea.costrict.local/u-alice/skill-vetter                            │
   │   ├─────────────────────────────────────────────┤ │                                  │
   │   │ type=plugin（standalone）：                  │ │                                  │
   │   │   namespace: u-alice                         │ │                                  │
   │   │   repo: vetter-plugin                        │ │                                  │
   │   │   file: .plugin.json                         │ │                                  │
   │   │   URL: gitea.costrict.local/u-alice/vetter-plugin                            │
   │   └─────────────────────────────────────────────┘ │                                  │
   │                              │                                  │
   │ Step 5: 获取创建命令（一键复制）                                    │
   │ ┌────────────────────────────────────────────────────────────┐ │
   │ │ # 方案 A：csc 一键 init（推荐，自动 sparse-clone + 占位符替换）  │ │
   │ │ csc capability init \                                       │ │
   │ │   --type=skill \                                            │ │
   │ │   --slug=skill-vetter \                                     │ │
   │ │   --name="Skill Vetter" \                                   │ │
   │ │   --description="..." \                                     │ │
   │ │   --from=https://gitea.costrict.local/costrict-template/templates.git │
   │ │ # csc 自动完成：sparse-clone templates mono-repo →           │ │
   │ │   取 skill/ 子目录 → 占位符替换 → re-init → push 到           │ │
   │ │   u-alice/skill-vetter（auto-create repo）                  │ │
   │ │                                                            │ │
   │ │ # 方案 B：纯 git + sed（无 csc，掌握细节的开发者）             │ │
   │ │ git clone --depth 1 --filter=blob:none --sparse \           │ │
   │ │   https://gitea.costrict.local/costrict-template/templates.git \ │ │
   │ │   skill-vetter                                             │ │
   │ │ cd skill-vetter && git sparse-checkout set skill/           │ │
   │ │ mv skill/* skill/.* . 2>/dev/null && rmdir skill            │ │
   │ │ find . -type f -name "*.tmpl" -exec sed -i \                │ │
   │ │   -e 's/{{SLUG}}/skill-vetter/g' \                          │ │
   │ │   -e 's/{{NAME}}/Skill Vetter/g' \                          │ │
   │ │   -e 's/{{DESCRIPTION}}/.../g' \                            │ │
   │ │   -e 's/{{USERNAME}}/alice/g' \                             │ │
   │ │   -e 's/{{DATE}}/2026-07-13/g' {} \;                        │ │
   │ │ find . -type f -name "*.tmpl" | while read f; do            │ │
   │ │   mv "$f" "${f%.tmpl}";                                     │ │
   │ │ done                                                        │ │
   │ │ $EDITOR skill.md     # 补充正文                              │ │
   │ │ rm -rf .git && git init -b main                             │ │
   │ │ git add . && git commit -m "feat(skill): init skill-vetter" │ │
   │ │ git remote add origin \                                     │ │
   │ │   https://gitea.costrict.local/u-alice/skill-vetter.git    │ │
   │ │ git push -u origin main                                     │ │
   │ │                                                            │ │
   │ │ # 方案 C：Gitea UI 创建（适合不熟悉 git 的用户）              │ │
   │ │ 1. 打开 https://gitea.costrict.local/repo/create           │ │
   │ │ 2. Owner: u-alice, Name: skill-vetter                      │ │
   │ │ 3. 在 README.md 旁点 "New File" → 命名 skill.md             │ │
   │ │ 4. 复制 Step 4 预览的模板内容粘贴                            │ │
   │ │ 5. Commit changes                                          │ │
   │ └────────────────────────────────────────────────────────────┘ │
   │                              │                                  │
   │ 用户在本地执行命令            │                                  │
   │ git push -u origin main      │                                  │
   ├──────────────────────────────────────────────────────────────►│
   │                              │  Gitea auto-create repo           │
   │                              │  POST webhook (push) ─────────────►
   │                              │  ────────────────────────────────►│
   │                              │                                  │
   │                              │ sync worker:                     │
   │                              │   GET compare {before}...{after} │
   │                              │   GET raw skill.md               │
   │                              │   upsert capability_items        │
   │                              │   SecurityScan 异步触发            │
   │                              │                                  │
   │ 平台轮询能力项状态            │                                  │
   │ GET /api/capabilities?slug=skill-vetter                         │
   │ ├────────────────────────────►│                                  │
   │ │ status: synced（30s 内）   │                                  │
   │ │ ◄──────────────────────────┤                                  │
   │                              │                                  │
   │ Toast 通知：                  │                                  │
   │ "✓ 能力项 skill-vetter 已创建"                                    │
   │ [查看详情]                    │                                  │
```

### 11.9 模板内容（位于 `costrict-template/templates/<type>/` 子目录，由 §11.7 init 流程拉取）

#### `skill/skill.md.tmpl`

```yaml
---
slug: {{SLUG}}
type: skill
name: {{NAME}}
description: {{DESCRIPTION}}
descriptions:
  en: {{DESCRIPTION}}
category: general
version: 1.0.0
metadata:
  tags: []
  author: {{USERNAME}}
  license: MIT
  created_at: '{{DATE}}'
  updated_at: '{{DATE}}'
---

# {{NAME}}

{{DESCRIPTION}}

<!-- TODO: 在此填写 skill 的详细说明 -->

## 使用方法

<!-- TODO: 描述如何调用此 skill -->
```

> 其他 type 模板位于 `costrict-template/templates/<type>/`，结构对称（subagent `agent.md.tmpl` / command `commands/<slug>.md.tmpl` / mcp `mcp.json.tmpl`），apply 阶段同样执行占位符替换 + `.tmpl` 后缀剥离。command type 还需把 `commands/<slug>.md.tmpl` 改名为 `commands/<实际-slug>.md`。

#### `plugin/.plugin.json.tmpl`（standalone plugin 根目录 metadata）

```json
{
  "slug": "{{SLUG}}",
  "name": "{{NAME}}",
  "description": "{{DESCRIPTION}}",
  "version": "1.0.0",
  "type": "plugin",
  "author": "{{USERNAME}}",
  "license": "MIT",
  "created_at": "{{DATE}}",
  "updated_at": "{{DATE}}",
  "entrypoint": "./index.js",
  "install": {
    "method": "git-clone",
    "source": "https://gitea.costrict.local/{{USERNAME}}/{{SLUG}}.git",
    "subpath": "."
  },
  "capabilities": [],
  "config_schema": {}
}
```

**字段说明**：

| 字段 | 用途 |
|---|---|
| `slug` / `name` / `description` / `version` | DB 索引层（`capability_items` 表）核心字段 |
| `entrypoint` | plugin 实体入口（csc install 后 `require(entrypoint)`） |
| `install.method=git-clone` | **统一 git 安装通道**：csc `plugin install` 时按此字段决定拉取方式（git-clone / marketplace-bundle / local-path） |
| `install.source` | git repo URL（与 §10.3 `gitea_clone_url` 对齐；server 端 sync worker 不解析，仅作为元数据回写） |
| `install.subpath` | standalone plugin 恒为 `.`（整个 repo 即 plugin）；marketplace bundle 模式下由 marketplace 项目自定义 |
| `capabilities` / `config_schema` | 运行时元数据，server 不解析，由 csc 加载 |

> **plugin 实体代码**：standalone plugin 的 repo 内除 `.plugin.json` 外的子文件（如 `index.js` / `lib/`）由用户自行组织；`.plugin.json.entrypoint` 告知 csc 入口位置。sync worker **只**索引 `.plugin.json`，不解析实体代码。

### 11.10 错误处理（极简）

由于平台不创建资源，错误处理大幅简化：

| 错误场景 | 处理 |
|---|---|
| 用户未登录 | wizard 第一步拦截，引导登录 |
| 用户未绑定 Gitea | wizard 警告 + 提供 Gitea 首次登录链接 |
| slug 不合规 | wizard 实时显示错误，禁用"获取命令"按钮 |
| slug 已占用 | wizard 实时显示提示 + 3 个候选 |
| 模板预览 API 失败（4xx / 5xx） | wizard Step 4 预览框显示"预览不可用"；不影响 Step 5 命令复制（init 走 git clone，不依赖此 API） |
| `git clone` 模板仓库失败（网络 / 认证 / 404） | csc / git client 原生错误返回；wizard Step 5 命令注释提示"如 clone 失败，检查网络或访问 https://gitea.costrict.local/costrict-template/templates 确认 repo 可达" |
| sparse-checkout 指定 `<type>/` 不存在 | csc 报错并退出；纯 git 用户由 git sparse-checkout 原生报错；wizard Step 5 列出当前可用 type 子目录（`skill / subagent / command / mcp / plugin`） |
| 占位符替换后文件名冲突（如 `<slug>.md.tmpl` 改名后已存在） | csc 报错并保留 `.tmpl.orig` 备份；纯 git 用户由 `mv` 原生报错 |
| 用户 push 时 Gitea 拒绝 | 由 Gitea 直接返回错误，用户在 git client 看到（如 PAT 无效 / 配额超限） |
| webhook 30s 未回流 | 平台 toast 提示"未检测到 push，请检查 git 命令是否执行成功"；详情页轮询最长 5min |
| 5min 后仍未 synced | 平台标 `pending`，建议用户检查 Gitea 上的 repo 状态；admin 巡检兜底（§10.9） |

### 11.11 Plugin 多源架构与 fav/install

> **设计原则**：plugin 在新 git 机制中是 **first-class item_type**——与 skill / subagent / command / mcp 完全平权。`costrict-plugin-marketplace` 只是 plugin 的**多种来源之一**，不是 plugin 的专属通道；plugin favorite / install 链路与其他 item_type 共用同一套机制。

#### 11.11.1 Plugin 两种来源（写入同一索引层）

| 来源 | repo 结构 | sync 路径 | `capability_items.source` 字段值 |
|---|---|---|---|
| **A. Marketplace catalog** | （不在 Gitea，由 marketplace build pipeline 产出） | `catalog-sync` worker 扫 `catalog-download/plugins/<id>/.plugin.json` → upsert | `marketplace:<bundle-name>` |
| **B. Git 自托管（standalone）** | `u-<username>/<plugin-slug>` 根目录 `.plugin.json` | git webhook → sync worker 读 root `.plugin.json` → upsert（与 skill/subagent 同链路） | `git:u-<username>/<plugin-slug>` |

**关键点**：
- 两种来源**共用** `capability_items` 表（item_type=plugin），上层查询时**无需区分** source
- `source` 字段记录来源，仅用于溯源 / 审计 / 安装路径决策
- catalog-sync worker 与 git sync worker 是**两条独立的 ingestion 路径**，互不感知，分别写 DB

#### 11.11.2 Plugin favorite 流程（与其他 item_type 同链路）

```
用户                    portal                costrict-web server          DB
   │                       │                          │                    │
   │ 点 favorite（plugin）  │                          │                    │
   ├──────────────────────►│                          │                    │
   │                       │ POST /api/items/:id/favorite                  │
   │                       │   body: {item_type: "plugin"}                 │
   │                       ├─────────────────────────►│                    │
   │                       │                          │ INSERT user_favorites (user_id, item_id)
   │                       │                          ├───────────────────►│
   │                       │                          │   201 Created      │
   │                       │                          │◄───────────────────┤
   │                       │   200 OK                 │                    │
   │                       │◄─────────────────────────┤                    │
   │ ♥ 已收藏              │                          │                    │
   │◄──────────────────────┤                          │                    │
```

**与 skill / subagent / command / mcp 完全一致**——`POST /api/items/:id/favorite` 对 item_type=plugin **不返 409**，没有 display-only gate。favorite 是**纯索引层操作**（DB 表写一行），与 plugin 实体代码 / 安装路径解耦。

#### 11.11.3 Plugin install 流程（csc 主导，server 仅给 URL）

> Plugin install 是**客户端行为**——csc 拉取 plugin 实体到本地，写入 csc 配置。server 端不参与 install 动作，仅通过 `GET /api/capabilities/:id` 返回 `gitea_clone_url` / `install.method` / `install.source` 字段。

```
csc 客户端                    costrict-web server            Gitea
   │                                │                            │
   │ GET /api/capabilities/<id>     │                            │
   │ Authorization: Bearer <JWT>    │                            │
   ├───────────────────────────────►│                            │
   │                                │ 返回 item + gitea_clone_url + install.method=git-clone
   │◄───────────────────────────────┤                            │
   │                                                             │
   │ 按 install.method 分支：                                       │
   │                                                             │
   │ ├─ git-clone：                                               │
   │ │   git clone <gitea_clone_url> ~/.costrict/plugins/<slug>   │
   │ │   cd ~/.costrict/plugins/<slug>                            │
   │ │   checkout <item.ref>  # 锁版本                            │
   │ │   require(<entrypoint>)  # 加载                            │
   │ │                                                            │
   │ ├─ marketplace-bundle：                                       │
   │ │   csc plugin marketplace install <bundle>/<plugin-id>     │
   │ │   （走 marketplace 项目，与本 spec 无关）                    │
   │ │                                                            │
   │ └─ local-path：直接 require(<install.source>)                │
   │                                                             │
   │ git clone（git-clone 分支）                                  │
   ├────────────────────────────────────────────────────────────►│
   │   200 OK + repo 内容                                         │
   │◄────────────────────────────────────────────────────────────┤
   │                                                             │
   │ 写入 ~/.costrict/config.toml:                                │
   │   [plugins.<slug>]                                           │
   │   enabled = true                                             │
   │   source = "git:u-alice/vetter-plugin@<ref>"                │
```

**关键点**：
- `install.method` 是 `.plugin.json` 的字段，由 plugin 作者声明，sync worker 原样写入 `capability_items.install` (jsonb)
- **git-clone 是默认 / 推荐方式**——与其他 capability type 一致；marketplace-bundle 仅用于离线分发的企业客户
- server 不下载 / 不代理 plugin 实体代码（与 §10.3 v2.5 一致：直连 Gitea + JWT 透传）
- **plugin install 不依赖 marketplace**——可纯 git 化运行（marketplace 是 add-on，不是 prerequisite）

#### 11.11.4 marketplace 与本 spec 的关系

| 维度 | 定位 |
|---|---|
| `costrict-plugin-marketplace` | **独立项目**：build pipeline + 客户 `import.sh` + bundle 发布渠道；离线分发企业客户的 770+ plugin 打包 |
| 与本 spec 关系 | **正交，零耦合**：marketplace 不读本 spec，本 spec 不引用 marketplace；两者在 DB 索引层（`capability_items` 表）汇合但不互相调用 |
| marketplace 产出的 plugin 在 portal 中展示 | 通过 catalog-sync worker 写入 `capability_items`（source=marketplace），与 git 自托管 plugin（source=git:...）同列表展示，**用户无感知差异** |
| 用户自托管 plugin 是否依赖 marketplace | **不依赖**——用户 `git push` 即可，无需 marketplace 介入；marketplace 仅作为离线分发可选项 |

#### 11.11.5 与 AGENTS.md 的差异

> **本 spec 明确覆盖** `AGENTS.md` 中关于 plugin 的 display-only gate 约定：

| AGENTS.md 当前表述 | 本 spec v2.9 立场 |
|---|---|
| favorite 按钮在 plugin 类型上不渲染 | **渲染**（与其他 item_type 一致） |
| 服务端 `POST /api/items/:id/favorite` 对 plugin 返回 HTTP 409 | **返回 201**（与其他 item_type 一致） |
| 用户**不能**通过 create-capability-dialog 自传 plugin | **可以**（§11 wizard 支持 standalone plugin） |
| 未来阶段（follow-up `add-plugin-favorite-csc`）才会激活 | **立即激活**——本 spec 直接定义完整链路 |

理由：display-only 是早期 stage gate，新 git 机制不应被早期 gate 约束；plugin 与其他 item_type 在索引层 / favorite / install 上完全平权，差异仅在 install.method 字段值。

**关键优势**：平台**无需回滚**（不持有任何中间状态）；所有失败都是用户侧 git 错误，由 git client / Gitea 原生提示。

### 11.12 AI agent 创建路径

AI agent 代用户创建时**完全等同于人类用户**，有两种内容生成路径：

```
AI agent (用户 PAT)
   │
   ├─ GET /api/capabilities/slug-check?slug=<generated>&type=skill   # 可选，校验
   │
   ├─ 内容生成（二选一）：
   │
   │  ├─ 路径 A：从模板 init（适合标准化能力项，AI 仅填正文）
   │  │   git clone --depth 1 --filter=blob:none --sparse \
   │  │     https://gitea.costrict.local/costrict-template/templates.git <slug>
   │  │   cd <slug> && git sparse-checkout set skill/
   │  │   mv skill/* . && rmdir skill
   │  │   # AI 在本地替换占位符 + 写正文
   │  │   sed -i '...占位符替换...' skill.md.tmpl && mv skill.md.tmpl skill.md
   │  │   rm -rf .git && git init -b main
   │  │   git add . && git commit -m "feat(skill): init <slug> from template"
   │  │
   │  └─ 路径 B：纯 AI 生成（适合 AI 已有完整内容，跳过模板）
   │     git init -b main <slug> && cd <slug>
   │     # AI 自己生成 frontmatter + 正文，直接写 skill.md
   │     git add skill.md && git commit -m "feat(skill): add <slug>"
   │
   ├─ git remote add origin https://gitea.costrict.local/u-<username>/<slug>.git
   ├─ git push -u origin main                                         # auto-create repo
   │
   └─ GET /api/capabilities?slug=<slug>                                # 轮询确认 synced
```

**关键约束**：
- AI 不需要任何 REST 写入 API（slug-check / templates 预览都是 GET）
- 路径 A（模板 init）与人类用户 §11.7.2 csc 流程完全一致——只是 sed / 文件编辑由 AI 完成
- 路径 B（纯 AI 生成）适合 AI 已生成完整内容的场景，跳过模板可省一次 clone
- 所有 commit author = 用户本人（PAT 绑定）
- 与人类用户操作完全一致，无特殊通道
- 审计归因到用户

### 11.13 平台 UI 表单字段（wizard）

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `type` | select | * | skill / subagent / command / mcp |
| `slug` | text | * | 实时校验（保留字 / 字符 / 唯一性）；显示自动改写预览 |
| `name` | text | * | 显示名 |
| `description` | textarea | * | 写入 frontmatter `description` |
| `category` | select | - | security / coding / productivity / general（默认 general） |
| `version` | text | - | 默认 `1.0.0` |
| `license` | select | - | MIT / Apache-2.0 / GPL-3.0 / 不填 |
| `tags` | tag-input | - | 数组，写入 frontmatter `metadata.tags` |
| `description_i18n` | collapsible | - | 多语言描述（en / zh / ja） |

**注意**：表单字段**仅用于填充模板占位符**，不触发任何 Gitea 写入。用户必须复制命令到本地执行才完成创建。

### 11.14 用户后续操作

| 操作 | 入口 |
|---|---|
| 编辑内容 | 本地 `git clone` → 编辑 → push；或 Gitea web editor |
| 改 visibility（作草稿） | Gitea repo Settings → "Toggle visibility" |
| 改 description / README | git 编辑 + push |
| 添加 collaborator | Gitea repo Settings → "Collaborators" |
| 升级为官方认证 | 走 §9.4 PR 流程 |
| 删除能力项 | git 删除 `skill.md` → push（自动标 archived） |

### 11.15 Standalone plugin 创建流程

Standalone plugin 已纳入 §11 wizard（v2.9）：

| 类型 | wizard 入口 | 流程入口 |
|---|---|---|
| Standalone plugin（`u-<username>/<plugin-slug>`） | §11.3 ✅ | §11.4 namespace（统一规则）+ §11.7 plugin 模板（根目录 `.plugin.json`） |

**多源架构 / favorite / install**：详见 §11.11——plugin 是 source-agnostic 的 first-class item_type，`costrict-plugin-marketplace` 只是其中一种来源，不是 plugin 的专属通道。

## 12. 凭据与权限

### 12.1 用户 PAT（fine-grained）

| 字段 | 推荐值 |
|---|---|
| scope | `read:repository` + `write:repository` |
| 限定 owner | `costrict-official` / `costrict-template`（read-only）/ `u-<self-username>`；**禁止 `costrict-config`**（任何权限）；**禁止写 `costrict-mirror`**（mirror 只读） |
| 有效期 | **90 天** |
| 共享 | **明确禁止**（每用户独立 PAT，审计到个人） |
| commit author | 用户本人 |

csc 端 PAT 获取：用户在 costrict-web `/settings/git-tokens` 生成 → 复制到 `~/.config/csc/git.json`（**文件权限 0600**）。

### 12.2 SSH key

优先级：**SSH key 优先**（与 GitHub 体验一致）；PAT 兼容 HTTPS。Deploy key 仅 CI / 外部系统使用，AI agent 不使用。

### 12.3 系统服务账号

单一 `costrict-system` 账号 + 单一 admin PAT，仅用于：
1. capability 索引同步（跨 owner 含 private）
2. 用户生命周期级联（`user.deleted` / `user.disabled` webhook；**username immutable，无 `user.updated` 改名事件**）

**禁止**把 admin PAT 用于用户代理 push、AI agent 自动化操作。

---

## 13. 创建 Checklist

### 13.1 用户自建 standalone（默认路径）

- [ ] `GET /api/capabilities?q=<slug>` 确认 slug 未占用
- [ ] 在 `u-<self-username>/<slug>` 下 `git init` + 写顶层 metadata（如 `skill.md`）
- [ ] `git commit -m "feat(<type>): add <slug>"`
- [ ] `git push origin main`（PAT 限定 owner=`u-<self-username>`）
- [ ] 等待 webhook 触发 sync + SecurityScan
- [ ] 检查 `GET /api/capabilities` 是否已收录
- [ ] （可选）改 `is_private=true` 作草稿

### 13.2 官方认证升级（PR 路径）

- [ ] 确认 `u-<self-username>/<slug>` 已 public 且 `clean`
- [ ] fork `costrict-official/<slug>` 到 `u-<self-username>/<slug>`，或新建 PR 直接升级到 `costrict-official/<slug>`
- [ ] 在 `feat/<branch>` 上 commit → push branch
- [ ] `POST /api/v1/repos/costrict-official/<slug>/pulls` 创建 PR
- [ ] 等 admin 审核（内容质量 / 安全扫描 / 与官方方向一致性）
- [ ] merge 后 DB 中 `source_repo_url` 自动跟随；原 `u-<username>/` repo 留 redirect

### 13.3 fork 上游 mirror 改进

- [ ] 在 Gitea UI 把 `costrict-mirror/<escaped-url>` fork 到 `u-<self-username>/<repo-name>`
- [ ] 本地 clone 后编辑文件
- [ ] 选项 1：直接维护在 `u-<self-username>/` 作为新 standalone → push main
- [ ] 选项 2：通过 PR 提交回上游 GitHub（不在平台闭环内）

---

## 14. 命名示例速查

### 14.1 用户 namespace

| 场景 | URL |
|---|---|
| 用户 alice 自建 skill | `gitea.costrict.local/u-alice/skill-vetter` |
| 用户 alice 自建 plugin | `gitea.costrict.local/u-alice/vetter-plugin` |
| 用户 alice fork 官方 skill | `gitea.costrict.local/u-alice/skill-vetter`（Gitea fork-to-personal） |
| 用户 alice 草稿 | `gitea.costrict.local/u-alice/draft-skill-x`（改 `is_private=true`） |

### 14.2 官方 org

| 场景 | URL |
|---|---|
| 官方 standalone skill | `gitea.costrict.local/costrict-official/skill-vetter` |
| 官方 standalone plugin | `gitea.costrict.local/costrict-official/vetter-plugin` |
| 官方 curated | `gitea.costrict.local/costrict-official/skill-vetter` + admin 打 `curated` tag（与官方 standalone 同 namespace，仅 tags 字段不同） |
| 平台模板 | `gitea.costrict.local/costrict-template/templates` |
| 平台配置 | `gitea.costrict.local/costrict-config/platform-config` |

### 14.3 可解析 URL 示例

```
# standalone skill
https://gitea.costrict.local/costrict-official/skill-vetter/-/tree/main/skill.md

# 用户 namespace skill
https://gitea.costrict.local/u-alice/skill-vetter/-/tree/main/skill.md

# 用户 namespace standalone plugin
https://gitea.costrict.local/u-alice/vetter-plugin/-/tree/main/.plugin.json
```

---

## 15. 禁止事项

| 类别 | 禁止行为 | 原因 |
|---|---|---|
| 内容 | 提交密钥 / `.env` / 超大二进制 | 安全 / 数据爆炸 |
| 内容 | 在 DB 里直接改 content 字段 | 违反"Git 为真相源" |
| 权限 | 用户 PAT 写 `costrict-mirror` | mirror read-only |
| 权限 | 共享用户 PAT | 审计不到个人 |
| 权限 | admin PAT 用于用户代理 push | 审计不到个人 |
| 流程 | 未 unlock 类型锁定直接改文件结构 | 触发 polluted 健康度告警 |
| 流程 | Force push / delete main（除 admin 应急） | 防历史覆写 / 误删 |
| 流程 | 直接删除 repo（绕过 ownership 接管） | 历史丢失 |
| Team ns | 在 `members:sync` 之前手动建 `t-<team_short>/` org | 绕过 lazy 创建策略，owner / visibility / branch protection 默认值与 §18.4 不一致 |
| KB | 用户 PAT 在 `t-<team_short>/` org 内直接建 `kb-*` repo（绕过 `POST /api/internal/kb/ensure`） | 导致首个 push 用户未进入 team ns membership，后续协作授权链断裂；team ns 未初始化时也无法 412 拦截 |
| KB | csc 内置算法副本计算 KB path（与 server 漂移） | 双端不一致，同一 (URL, team_id) 算出不同 path |
| KB | server 维护 `kb_repos` / `codebase_kbs` 等绑定表 | 纯函数已能唯一映射，多维护一份状态反成漂移源 |
| KB | 在 server 内编排 KB 协作流程（issue / 授权 / 转让） | 应回归 git 原生协作 + Gitea API 透传，权限收口到 team ns membership |
| Workflow | 用户 PAT 在 `t-<team_short>/` org 内直接建 `wf-*` 类型 repo 或 `inst-*` 分支（绕过 `POST /api/internal/workflow/init`） | 导致 definition.snapshot.yaml 缺失，节点级 PR 审计链断；instance 元数据未登记 |
| Workflow | csc 内置算法副本计算 wf repo path / instance branch 名（与 server 漂移） | 双端不一致，同一 (def_slug, team_id, instance_id) 算出不同 path / branch |
| Workflow | server 维护 `wf_instances` / `wf_repos` 等绑定表 | 纯函数已能唯一映射，多维护一份状态反成漂移源 |
| Workflow | 在 server 内编排 workflow 节点 PR 流程（建分支 / 开 PR / merge） | 应回归 git 原生 + Gitea API 透传 |
| Workflow | 跨**类型 repo**共享同一实例 branch（一个实例 branch 跨多个 `wf-*` repo） | branch 必须严格 1:1 对应 (type repo, instance)；跨 repo 共享导致 git history 不可追溯 |

---

## 16. KB 仓库管理与协作（v2.17）

> **定位**：KB（knowledge base）是**业务数据**，不是 capability item type，不进 `capability_items` 表、不进 sync worker、不进 SecurityScan、不进 portal 列表。本规范仅约束 KB repo 的**命名空间、创建、归属、协作**。KB 文档内容结构、生成 pipeline 由独立的 KB 生成服务负责，不在本规范范围。
>
> **v2.17 关键变更**：KB repo 不再落全局 `costrict-kb/`，改为落 **`t-<team_short_id>/`** per-team namespace；权限模型从 per-repo collaborator 改为 per-team org membership。同 code repo 在不同 team 下各有独立 KB repo（per-team 视角）。

### 16.1 设计原则

1. **代码 repo → KB repo 在 team 内一一对应**：同一 team 内，每个 code repo 对应一个独立 KB repo；不同 team 对同一 code repo 各自维护独立 KB repo（per-team 视角，文档风格 / 协作圈不同）
2. **路径推导纯函数化**：KB repo path 由 server 端纯函数从 `(code_repo_url, team_id)` 推导（详见 [`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) v2.0），**不维护绑定关系表**
3. **server 仅预创建**：`POST /api/internal/kb/ensure` 幂等接口承担"判 team ns 存在 + 判 KB repo 存在 + 首次创建"三件事，**其他全部回归 git 原生协作**（push / pull / branch / PR 均走 Gitea 原生 API）
4. **无 server 状态**：server 不持久化 KB 元数据，不维护 `kb_repos` 表——每次 ensure 实时计算 path + 实时查 Gitea
5. **权限通过 team ns 继承**：用户加入 team ns Gitea org 自动获得该 team 下所有 KB repo 的 read/write 权限；不再为每个 KB repo 单独加 collaborator（owner-level fine-grained 调整仍可选）

### 16.2 team namespace 配置

KB repo 落在 §18 定义的 `t-<team_short_id>/` org 内。org 配置详见 §18.4。本节仅列 KB 特定约束：

| 字段 | 值 | 说明 |
|---|---|---|
| KB repo owner | `t-<team_short_id>` | KB repo 的 Gitea owner = team ns org |
| KB repo 命名 | `kb-<host>__<escaped_segments>` | 详见 §4.6 与 [`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) v2.0 |
| visibility | **private**（强制） | KB 内容仅 team 成员可见 |
| 默认分支 | `main` | 单分支，仅跟踪主线 |
| 分支保护 | `main` 禁 force push / delete | 与 §7.3 对齐 |
| 成员授权 | 通过 Gitea org 成员关系继承 | 不再 per-repo 加 collaborator；如需 fine-grained 限制，由 owner 显式调整某 repo 的 collaborator（覆盖 org 默认） |
| webhook | 复用 tenant `costrict-config/platform-config` 中 `webhook_secret_ref` | KB repo push webhook 投递到独立的 KB 生成服务（不在本规范范围） |

### 16.3 server API：`POST /api/internal/kb/ensure`

唯一接口。**内部接口**（网关不放行 `/api/internal/*`，仅可信服务经 service token 调用；详见 [`TEAM_NAMESPACE_API.md`](./TEAM_NAMESPACE_API.md)）。幂等。

#### 16.3.1 请求

```http
POST /api/internal/kb/ensure HTTP/1.1
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Tenant-Id: <tenant_id>（可选，缺省走默认租户）
X-Request-Id: <uuid>（推荐）
Content-Type: application/json

{
  "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
  "code_repo_url": "https://github.com/ownerA/proj.git"
}
```

#### 16.3.2 响应（统一 schema）

```json
{
  "kb_repo_path": "t-7f3c9a1e/kb-github.com__ownera__proj",
  "kb_clone_url": "https://gitea.costrict.local/t-7f3c9a1e/kb-github.com__ownera__proj.git",
  "kb_web_url": "https://gitea.costrict.local/t-7f3c9a1e/kb-github.com__ownera__proj",
  "team_ns_exists": true,
  "created": false,
  "algorithm_version": "v2"
}
```

> **URL 字段使用约束（§10.3.1 SoT）**：`kb_clone_url` / `kb_web_url` 由 server 拼接 `<tenant_gitea_base_url>/<kb_repo_path>` 后返回；调用方（csc / 编排器）**必须直接使用响应字段**，禁止自行拼接 `gitea_base_url + kb_repo_path`。
>
> KB repo 的权限不再 per-repo 显式表达——team ns org 成员自动有 read/write；接口不返回 `role` 字段。如调用方需判断"用户 X 是否能 push"，应改问"用户 X 是否为 team ns org 成员"。

#### 16.3.3 行为分支

| 场景 | server 行为 | created | team_ns_exists |
|---|---|---|---|
| **team ns 不存在**（尚未 `members:sync`） | 直接 412 `TEAM_NS_NOT_INITIALIZED` + hint "先调 members:sync 创建 team namespace" | — | false |
| **KB repo 不存在**（首次 ensure） | ① 校验 team ns 存在 ② 用 admin PAT 调 `POST /admin/users/t-<team_short>/repos` 建 repo（private） ③ 调 `POST /repos/.../branch_protections` 配置 main 保护 | true | true |
| **KB repo 已存在** | 视为幂等成功（no-op） | false | true |

#### 16.3.4 错误码

| HTTP | error_code | 触发条件 | 响应体 |
|---|---|---|---|
| 400 | `INVALID_REQUEST` | `code_repo_url` 缺失 / 非 http(s) scheme / `team_id` 非 UUID / 解析失败（详见算法 spec §4.4） | `{ "error": "invalid_request", "detail": "..." }` |
| 401 | `UNAUTHORIZED_SERVICE` | `X-Internal-Service-Token` 缺失或与约定值不一致 | `{ "error": "unauthorized_service" }` |
| 412 | `TEAM_NS_NOT_INITIALIZED` | team ns Gitea org 不存在（未先调 `members:sync`） | `{ "error": "team_ns_not_initialized", "hint": "call POST /api/internal/teams/:team_id/members:sync first" }` |
| 500 | `GITEA_API_FAILURE` | admin PAT 调 Gitea 失败（建 repo / 配保护） | `{ "error": "gitea_api_failed", "detail": "..." }` |

#### 16.3.5 幂等保证

- 同一调用者重复 ensure 同一 `(code_repo_url, team_id)`：第二次进入"已存在"分支，返回 `created=false`
- 不同 team 对同一 code_repo_url：互不感知，各自 ensure 创建独立 KB repo

### 16.4 协作流程

#### 16.4.1 主流程：csc kb push

```
1. csc 解析当前 git remote URL → code_repo_url（https 形式）
   ├─ ssh / git scheme 须先归一化为 https（详见算法 spec §3.2）
   └─ 多 remote 场景：默认取 origin

2. csc 解析当前 team_id（从 csc config / 环境变量 / .costrict/kb.yaml）
   ├─ csc 不反查 team 归属——由调用方/编排器负责传入
   └─ team_id 缺失 → 拒绝执行并提示用户指定

3. csc 调用 POST /api/internal/kb/ensure
   ├─ Header: X-Internal-Service-Token（csc 通过编排器代调，详见 §16.5）
   └─ Body: { team_id, code_repo_url }

4. 按 ensure 响应分支:
   ├─ team_ns_exists=true, created=true/false:
   │   ├─ git push origin main（remote URL = response.kb_clone_url，server 已拼接完整 URL，详见 §10.3.1）
   │   ├─ 使用调用者 PAT（csc login 颁发；用户已是 team ns org 成员即有 write 权限）
   │   └─ 推送成功 → done
   └─ team_ns_exists=false:
       ├─ 打印: "Team namespace not initialized. Contact team admin to run members:sync."
       └─ exit code ≠ 0
```

#### 16.4.2 team 成员管理

KB repo 不再 per-repo 加 collaborator——成员管理统一走 [`TEAM_NAMESPACE_API.md`](./TEAM_NAMESPACE_API.md) 的 `members:sync` 接口（team 级别）：

| 时机 | 操作 |
|---|---|
| 用户加入 team | workspace 服务（消费 `org-team-service` webhook）→ 调 `POST /api/internal/teams/:team_id/members:sync` (delta, add) |
| 用户离开 team | 同上 (delta, remove) → 自动失去该 team ns 所有 KB repo 权限 |
| 团队解散 | 全员 remove + archive team ns |

> KB owner 若需 fine-grained 限制某成员对特定 KB repo 的访问（如机密项目），可在 Gitea UI 对该 repo 显式覆盖 org 默认权限；此为高级用法，不在 API 标准流程内。

#### 16.4.3 KB 内容更新流程

由独立的 KB 生成服务负责（不在本规范范围）。该服务持独立 PAT，由 admin 直配 scope = `t-*/kb-*` write；按 KB 内容生成 pipeline 周期 push 更新。

### 16.5 csc 子命令契约（轻量）

详见 [`CSC_KB_SUBCOMMAND_CONTRACT.md`](./CSC_KB_SUBCOMMAND_CONTRACT.md)。

csc 是 **thin client**：

- 所有命令**统一先经编排器代调 `POST /api/internal/kb/ensure`** 拿到 `kb_repo_path`（csc 客户端无 service token，由编排器代为鉴权）
- 不内置路径算法（path 来自 ensure 响应）
- 不维护本地缓存（每次命令重新 ensure，幂等成本低）
- **team_id 由调用方 / 编排器传入**，csc 不反查归属

命令清单（与 v1.0 相比，移除 `authorize` / `revoke` / `transfer-owner`——权限管理统一到 team ns `members:sync`）：

| 命令 | 行为 |
|---|---|
| `csc kb push` | ensure → `git push origin main` |
| `csc kb pull` | ensure → `git fetch && git pull origin main` |
| `csc kb status` | ensure → 对比本地工作区与远端 main 的 commit diff |
| `csc kb list` | 调 `GET /user/repos?type=member` 列出调用者所属 team ns 下的 KB repo（按 repo name `kb-*` 前缀过滤） |
| `csc kb pr open/list/merge/close` | Gitea PR API 透传 |

### 16.6 分支与可见性策略

| 维度 | 规则 |
|---|---|
| 分支 | **只跟踪 main**（KB 文档主线由生成服务推送；不支持 feature 分支开发） |
| Force push | **禁止** |
| 删除 main | **禁止** |
| visibility | **恒定 private**（不允许 owner 改 public；防 KB 内容外泄） |
| fork | 不允许 fork 到 `u-<username>/`（KB 是协作产物，非 capability） |

### 16.7 多租户约束

| 场景 | 处理 |
|---|---|
| 同一外部代码 repo URL（如 `https://github.com/o/p`）在 tenant A 与 tenant B 各自 ensure | 两 tenant 各自创建独立 KB repo（数据隔离；与 §1.5.5 一致） |
| 同 tenant 内不同 team 对同 code_repo_url | 各自独立 KB repo（per-team 视角） |
| 跨 tenant 协作 | **不可达**——不同 Gitea 实例之间无原生关系 |
| JWT tenant_id 校验 | 内部接口走 service token；tenant_id 由 X-Tenant-Id header 决定（缺省走默认租户，§1.5 多租户约束不直接适用内部接口） |

### 16.8 审计

| 事件 | 审计来源 |
|---|---|
| KB repo 创建（ensure → created=true） | Gitea audit log（admin PAT 创建事件） |
| team 成员变更（影响 KB 权限） | Gitea audit log（org member API 调用，归因到 server admin PAT） |
| KB push | Gitea audit log（push 事件，归因到调用者 PAT） |
| ensure 调用本身 | costrict-web server access log |

### 16.9 与 §11（wizard）的关系

KB repo **不参与 §11 wizard 流程**——

- KB 不是 capability item type，不进 §11.3 适用场景表
- KB repo 路径算法化（用户不能选 namespace / slug）
- 不需要 §11.6 slug-check、§11.7 模板下载、§11.8 wizard step
- csc `kb` 子命令集与 `capability` 子命令集**完全分离**

### 16.10 用户 PAT 规则补充

| PAT 类型 | 对 `t-<team_short_id>/kb-*` 的权限 |
|---|---|
| 用户 fine-grained PAT | 仅可对**调用者作为 org member 的 team ns** 的 KB repo 做 push / pull / clone；权限继承自 org 成员关系；**不允许 `t-*` org 层的 repo:create scope** |
| admin PAT（`costrict-system`） | 全权：repo 创建 / org 成员管理 / branch protection 配置 |
| KB 生成服务 PAT | 由 admin 单独签发，scope 限定为 `t-*/kb-*` write（admin 直配，不在本规范范围） |

---

## 17. Workflow 仓库管理与协作（v2.17）

> **定位**：Workflow 是**业务数据**，不是 capability item type，不进 `capability_items` 表、不进 sync worker、不进 SecurityScan、不进 portal 列表。本规范仅约束 workflow 类型 repo / 实例 branch 的**命名空间、创建、归属、协作**。Workflow 定义本身（节点编排 / DAG）由独立的平台 workflow 编排器负责，不在本规范范围。
>
> **v2.17 关键变更**：模型从「每实例一 repo」改为「每 (team, def) 一类型 repo + 实例 = branch」，类型 repo 的 `main` 分支承载 workflow 定义的 canonical 存储；实例以 `inst-<inst_short_id>` branch 表达。v2.16 §17.1 明文否决过的 C 方案在引入 team ns 后**重新评估并采纳**——详见 §17.1 决策反转说明。

### 17.1 设计原则（v2.17 反转 C 方案）

1. **类型 repo + 实例 branch**：每个 (team, workflow_def) 共享一个**类型 repo** `t-<team_short>/wf-<def_slug>`；每个实例对应一个 branch `inst-<inst_short_id>`。**多实例共享同一类型 repo**，权限粒度落到 (team, def) 级别
2. **路径推导纯函数化（双函数）**：
   - 类型 repo path：`wfRepoPath(def_slug, team_id)` → `t-<team_short>/wf-<def_slug>`（详见 [`WORKFLOW_REPO_PATH_ALGORITHM.md`](./WORKFLOW_REPO_PATH_ALGORITHM.md) v2.0 §A）
   - 实例 branch name：`wfBranchName(instance_id)` → `inst-<inst_short_id>`（详见算法 spec v2.0 §B）
   - **不维护绑定关系表**
3. **main = workflow def canonical 存储**：类型 repo 的 `main` 分支存放 workflow 定义的 canonical 版本（节点定义 / DAG / audit_level 配置）；团队对 def 的修改走 main 上的 PR；实例启动时从 main HEAD 创建 `inst-<short>` branch（实例分支后续不再随 main 更新——避免运行中定义漂移）
4. **server 仅 init**：`POST /api/internal/workflow/init` 内部接口承担"判 team ns 存在 + 判类型 repo 存在 + 判实例 branch 存在 + 首次创建"四件事，**其他全部回归 git 原生协作**
5. **无 server 状态**：server 不持久化 workflow 实例元数据，不维护 `wf_instances` 表——每次 init 实时计算 path + branch + 实时查 Gitea
6. **权限通过 team ns 继承**：用户加入 team（即 Gitea org）→ 自动获得该 team ns 所有类型 repo 的 read/write 权限（按 org 内 role）；不再为每个 wf repo 显式加 collaborator

#### 17.1.1 v2.16 → v2.17 C 方案决策反转说明

v2.16 §17.1 明文否决「类型 repo + 实例 branch」C 方案，理由四条：

| 原否决理由 | v2.17 重新评估 |
|---|---|
| 权限粒度只能到类型级 | ✅ **被 team ns 缓解**——team ns org 成员关系即权限，粒度从"全局 owner"细化为"per-team"，对 workflow 业务场景已足够（同 team 内成员本来就该能看所有实例） |
| branch 不能删 | ⚠️ **仍成立但可接受**——实例归档改为"标记 + 加保护 + 不删 branch"，不依赖 branch 删除；archive branch 等价 read-only |
| PR 流程嵌套 | ⚠️ **仍成立但 csc 已确认可改造**——节点 PR base 显式设为 `inst-<short>` 而非 main（决策 4）；csc `wf node push` 自动处理 |
| 路径反推破坏纯函数 | ❌ **不成立**——path 函数仍可纯函数化，只是改为接受 `team_id` 作为入参；输出双函数（repo path + branch name） |

**结论**：4 条理由中 1 条根本不成立、2 条仍成立但已有缓解方案、1 条被 team ns 缓解。引入 team ns 后 C 方案的收益（权限模型对齐业务直觉、KB per-team 独立、repo 数量级降低）大于剩余成本。

### 17.2 team namespace 配置

Workflow 类型 repo 落在 §18 定义的 `t-<team_short_id>/` org 内。org 配置详见 §18.4。本节仅列 workflow 特定约束：

| 字段 | 值 | 说明 |
|---|---|---|
| 类型 repo owner | `t-<team_short_id>` | wf repo 的 Gitea owner = team ns org |
| 类型 repo 命名 | `wf-<def_slug>` | 详见 §4.7 与 [`WORKFLOW_REPO_PATH_ALGORITHM.md`](./WORKFLOW_REPO_PATH_ALGORITHM.md) v2.0 |
| visibility | **private**（强制） | wf 内容仅 team 成员可见 |
| 默认分支 | `main` | 承载 workflow def canonical 存储 |
| 实例 branch 命名 | `inst-<inst_short_id>`（8 hex） | 详见算法 spec v2.0 §B |
| 节点 feat branch 命名 | `node/<seq>-<slug>` | base = `inst-<short>`（不是 main） |
| 分支保护 | (a) `main` 禁 force push / delete（保护 def canonical）  (b) `inst-*` 通配禁 force push / delete（保护实例时间线） | 需 Gitea 1.21+ 的 glob branch protection |
| 成员授权 | 通过 Gitea org 成员关系继承 | 不再 per-repo 加 collaborator |
| webhook | 复用 tenant `costrict-config/platform-config` 中 `webhook_secret_ref` | wf repo push / PR webhook 投递到独立的 workflow 编排器 |

### 17.3 server API：`POST /api/internal/workflow/init`

唯一接口。**内部接口**（详见 [`TEAM_NAMESPACE_API.md`](./TEAM_NAMESPACE_API.md)）。幂等。

#### 17.3.1 请求

```http
POST /api/internal/workflow/init HTTP/1.1
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Tenant-Id: <tenant_id>（可选，缺省走默认租户）
X-Request-Id: <uuid>（推荐）
Content-Type: application/json

{
  "workflow_def_slug": "bug-fix-flow",
  "instance_id": "f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab",
  "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
  "definition_snapshot": "<yaml 内容：节点定义 / DAG / audit_level 配置>"
}
```

> **定义来源**：v2.17 下，定义的 canonical 存储是 `wf-<def>` repo 的 `main` 分支。init 接口的 `definition_snapshot` 字段仅在**类型 repo 首次创建**时使用——把 def 写入 main；后续 init 应从 main HEAD 读取定义（而非每次重传）。`definition_snapshot` 与 main 现存版本不一致时返回 409 `DEFINITION_DRIFT`，提示调用方先在 main 上 PR 更新 def。

#### 17.3.2 响应（统一 schema）

```json
{
  "wf_repo_path": "t-7f3c9a1e/wf-bug-fix-flow",
  "wf_clone_url": "https://gitea.costrict.local/t-7f3c9a1e/wf-bug-fix-flow.git",
  "wf_web_url": "https://gitea.costrict.local/t-7f3c9a1e/wf-bug-fix-flow",
  "instance_branch": "inst-f3a8b2c1",
  "created": {
    "type_repo": false,
    "instance_branch": true
  },
  "team_ns_exists": true,
  "algorithm_version": "v2"
}
```

> **URL 字段使用约束（§10.3.1 SoT）**：`wf_clone_url` / `wf_web_url` 由 server 拼接 `<tenant_gitea_base_url>/<wf_repo_path>` 后返回；调用方（workflow 编排器 / 节点执行器）**必须直接使用响应字段**做 `git fetch` / `git push`，禁止自行拼接 `gitea_base_url + wf_repo_path`。
>
> 同 §16.3.2，权限不再 per-repo 显式表达——team ns org 成员自动有 read/write；接口不返回 `role` 字段。

#### 17.3.3 行为分支

| 场景 | server 行为 | created.type_repo | created.instance_branch | team_ns_exists |
|---|---|---|---|---|
| **team ns 不存在** | 412 `TEAM_NS_NOT_INITIALIZED` + hint | — | — | false |
| **类型 repo 不存在**（首次 init 该 def） | ① 用 admin PAT 调 `POST /admin/users/t-<team_short>/repos` 建 type repo ② 把 `definition_snapshot` 写入 main（首次 canonical 存储） ③ 配置 main + `inst-*` 通配 branch protection | true | true | true |
| **类型 repo 已存在 + 实例 branch 不存在** | ① 从 main HEAD 读 def 校验与 `definition_snapshot` 一致（不一致 → 409 `DEFINITION_DRIFT`） ② 从 main 创建 `inst-<short>` branch | false | true | true |
| **实例 branch 已存在**（幂等重入） | 视为成功（no-op） | false | false | true |
| **实例 branch 碰撞**（同 team 内 inst-<8hex> 已被占用） | 409 `INSTANCE_BRANCH_CONFLICT` + hint "重新分配 instance_id" | false | false | true |

#### 17.3.4 错误码

| HTTP | error_code | 触发条件 | 响应体 |
|---|---|---|---|
| 400 | `INVALID_REQUEST` | `workflow_def_slug` 为空 / `instance_id` 非 UUID / `team_id` 非 UUID / `definition_snapshot` 在类型 repo 首次创建时缺失 | `{ "error": "invalid_request", "detail": "..." }` |
| 401 | `UNAUTHORIZED_SERVICE` | `X-Internal-Service-Token` 缺失或不一致 | `{ "error": "unauthorized_service" }` |
| 412 | `TEAM_NS_NOT_INITIALIZED` | team ns Gitea org 不存在 | `{ "error": "team_ns_not_initialized", "hint": "call members:sync first" }` |
| 409 | `DEFINITION_DRIFT` | `definition_snapshot` 与 main 现存 def hash 不一致 | `{ "error": "definition_drift", "detail": "existing=<hash> incoming=<hash>, open PR on main to update def" }` |
| 409 | `INSTANCE_BRANCH_CONFLICT` | `inst-<short>` 在该类型 repo 已被占用（碰撞或 UUID 重复） | `{ "error": "instance_branch_conflict", "hint": "regenerate instance_id" }` |
| 500 | `GITEA_API_FAILURE` | admin PAT 调 Gitea 失败 | `{ "error": "gitea_api_failed", "detail": "..." }` |

#### 17.3.5 幂等保证

- 相同 `(workflow_def_slug, instance_id, team_id)` 二次调用：进入"实例 branch 已存在"分支，no-op 返回成功
- 类型 repo 已存在但实例 branch 新建：返回 `created.type_repo=false / instance_branch=true`

### 17.4 类型 repo 结构

每个 `t-<team_short>/wf-<def_slug>` 类型 repo 内部结构：

```
t-<team_short>/wf-<def_slug>/
├── [main 分支]                            # canonical workflow def 存储
│   └── definition.yaml                    # workflow def（节点 / DAG / audit_level）
│
├── [inst-<inst_short_id> 分支]            # 每个实例一个 branch，base = main
│   ├── .workflow/
│   │   ├── instance.json                  # 实例元数据（owner / 起止时间 / 状态）
│   │   ├── audit-config.json              # 实例 audit_level（init 时从 def 拷贝）
│   │   └── node-prs.json                  # 节点 PR 索引（仅本实例）
│   ├── nodes/
│   │   ├── 001-<node-slug>/
│   │   │   ├── input.json                 # 节点输入快照（含上游 commit SHA）
│   │   │   ├── output.md                  # 节点主交付物
│   │   │   └── artifacts/
│   │   ├── 002-<node-slug>/...
│   │   └── ...
│   └── README.md                          # 实例总览（自动生成）
│
└── [node/<seq>-<slug> 分支]                # 节点 feat 分支，base = inst-<short>
```

**关键约束**：
- `main` 上仅放 `definition.yaml`（canonical def），**不放实例数据**——避免不同实例的 commit 互相污染
- `inst-<short>` 分支独立持有该实例的全部交付物；实例间不互相影响
- `node/<seq>-<slug>` 分支以 `inst-<short>` 为 base（而非 main），merge 后写入 `inst-<short>`
- `definition.yaml` 一旦首次写入 main，后续修改必须走 PR；实例 init 时若发现传入的 `definition_snapshot` 与 main HEAD 不一致，返回 409

### 17.5 节点级 PR 审计流程

每个节点的交付物变更走 PR，**base = `inst-<short>`**（不是 main）：

```
节点执行器（agent / 用户）
  ├─ git checkout inst-<inst_short_id>           # 切到实例 branch
  ├─ git checkout -b node/<seq>-<slug>           # base = inst-<short>
  ├─ 写 nodes/<seq>-<slug>/output.md + artifacts/
  ├─ git commit -m "node(<seq>): <node_name>"
  ├─ git push origin node/<seq>-<slug>
  ├─ POST /repos/.../pulls                       # 开 PR
  │    title:  "[<inst_short>][node-<seq>] <node_name>"
  │    base:   "inst-<inst_short_id>"            # ← 关键：base 不是 main
  │    head:   "node/<seq>-<slug>"
  ├─ reviewer 审计 → approve / comment / request_changes
  ├─ merge → inst-<short> commit history 保留节点时间线
  └─ server 同步 .workflow/node-prs.json（仅本实例的索引文件）
```

**Merge 策略**：强制 **merge commit**（不用 squash / rebase），保证节点 commit SHA 在 `inst-<short>` 历史可追溯。

**节点级 PR 策略分级**（audit_config，定义中每个 node 显式声明）：

| audit_level | 行为 | 默认 reviewer |
|---|---|---|
| `strict` | 强制 PR + 人工 approve 后方可 merge | instance owner |
| `auto`（默认） | 自动开 PR + CI 通过后 auto-approve bot 自动 merge | auto-approve |
| `experimental` | 不开 PR，留 branch 不并 inst-<short> | 无 |

策略来源：main 上 `definition.yaml` 中每个 node 显式声明 `audit_level`；实例 init 时 server 从 def 拷贝到 `inst-<short>:.workflow/audit-config.json`；实例运行中策略固化，不再随 main 更新。

### 17.6 协作流程

#### 17.6.1 主流程：csc wf node push

```
1. 平台编排器启动实例时调 POST /api/internal/workflow/init
   → 拿到 { wf_repo_path, wf_clone_url, wf_web_url, instance_branch }
2. 节点执行器（agent / 用户）：
   ├─ csc wf node push <seq>
   │   ├─ 内部先 init（幂等）
   │   ├─ git checkout inst-<short>
   │   ├─ git checkout -b node/<seq>-<slug>     # base = inst-<short>
   │   ├─ git push origin node/<seq>-<slug>
   │   └─ POST /repos/.../pulls (base=inst-<short>)
   └─ reviewer（audit_level=strict 时）：
       └─ csc wf node approve <pr-number>
```

#### 17.6.2 workflow def 更新流程（团队级）

团队成员修改 def 必须走 main 上的 PR：

```
0. csc wf status（或 init）→ 拿到 response.wf_clone_url（server 已拼接完整 URL，详见 §10.3.1）
1. git clone <wf_clone_url>
2. git checkout main
3. git checkout -b def/<change-desc>
4. 编辑 definition.yaml
5. git commit -m "def: <change-desc>"
6. git push origin def/<change-desc>
7. POST /repos/.../pulls (base=main, head=def/<change-desc>)
8. team 成员 review + merge 到 main
```

> merge 到 main 不影响已运行的实例（实例固化为 init 时的 def）；新实例 init 时会从 main HEAD 拉取最新 def。

#### 17.6.3 team 成员管理

同 §16.4.2，统一走 [`TEAM_NAMESPACE_API.md`](./TEAM_NAMESPACE_API.md) 的 `members:sync` 接口（team 级别）。

### 17.7 实例生命周期

| 阶段 | 类型 repo 状态 | 实例 branch 状态 | instance.json.status |
|---|---|---|---|
| 实例启动 | （已存在或本次创建） | 从 main HEAD 创建 `inst-<short>` + 写入 instance.json + audit-config.json + 空 node-prs.json | `running` |
| 节点执行中 | 不变 | 持续 PR + merge 到 `inst-<short>` | `running` |
| 实例完成 | 不变 | server 在 `inst-<short>:.workflow/instance.json` 标 `status=completed` + `completed_at` | `completed` |
| 长期归档 | 不变 | server 强化 `inst-<short>` branch protection（read-only，禁止任何新 commit） | `archived` |
| 物理删除 | 不变（type repo 永久保留） | 删除 `inst-<short>` branch（仅 admin 显式操作；推荐 archive 而非 delete） | （删除） |

**保留期**：实例 branch 默认永久保留（审计意义大于存储成本）；归档 = 强化 branch protection 而非删 branch。

### 17.8 可溯源机制

三层溯源（与 v2.16 一致，仅 commit history 路径调整）：

| 层级 | 载体 | 用途 |
|---|---|---|
| **commit 历史** | `git log inst-<short>` | 节点时间线（merge commit 含节点元信息） |
| **PR 链接** | `inst-<short>:.workflow/node-prs.json` + Gitea PR 页面 | 节点 input/output 摘要 + reviewer + 评审讨论 |
| **跨节点数据流** | 每个节点的 `input.json` 记录上游节点 commit SHA | 上溯任何一个交付物的来源链 |
| **def 溯源** | `git log main -- definition.yaml` | workflow 定义本身的演进历史（团队 def 变更） |

### 17.9 csc 子命令契约（轻量）

详见 [`CSC_WF_SUBCOMMAND_CONTRACT.md`](./CSC_WF_SUBCOMMAND_CONTRACT.md)。

csc 是 **thin client**：

- 所有命令**统一先经编排器代调 `POST /api/internal/workflow/init`** 拿到 `wf_repo_path` + `wf_clone_url` + `instance_branch`（csc 客户端无 service token）
- 不内置路径算法（path + branch 都来自 init 响应）
- 不维护本地缓存
- **team_id + instance_id 由调用方 / 编排器传入**，csc 不反查归属

命令清单（与 v1.0 相比，移除 `authorize` / `revoke` / `transfer-owner`——权限管理统一到 team ns `members:sync`；新增 `def update` 走 main 上的 PR）：

| 命令 | 行为 |
|---|---|
| `csc wf init <def_slug> <instance_id>` | 调 `POST /api/internal/workflow/init`（首次必须含 `--definition-snapshot=<path>`，类型 repo 已存在则忽略） |
| `csc wf node push <seq>` | init → 切 `inst-<short>` → 切 `node/<seq>-<slug>`（base=inst-<short>） → push → 开 PR（base=inst-<short>，按 audit_level 自动 merge） |
| `csc wf node list` | init → 列本实例所有节点 + PR 状态（读 `inst-<short>:.workflow/node-prs.json`） |
| `csc wf node approve <pr-number>` | init → 调 `POST /repos/.../pulls/<n>/reviews` approve |
| `csc wf node merge <pr-number>` | init → 调 `POST /repos/.../pulls/<n>/merge`（merge commit, base=inst-<short>） |
| `csc wf def update` | 切 main → 切 `def/<change>` → 编辑 definition.yaml → push → 开 PR (base=main) |
| `csc wf list [--mine] [--def=<slug>]` | 调 Gitea API 列调用者所属 team ns 下的 wf repo（`wf-*` 前缀过滤）；`--mine` 进一步按 `inst-<short>:.workflow/instance.json.owner` 过滤 |
| `csc wf archive` | init（必须 team owner 或 admin）→ 强化 `inst-<short>` branch protection + instance.json.status=archived |
| `csc wf pr open/list/merge/close` | Gitea PR API 透传 |

### 17.10 分支与可见性策略

| 维度 | 规则 |
|---|---|
| 类型 repo 默认分支 | `main`（canonical def 存储） |
| `main` 分支 | 禁 force push / delete；merge 走 PR（团队 def 治理） |
| `inst-*` 分支 | 通配禁 force push / delete（保护实例时间线） |
| `node/*` 分支 | merge 后默认保留（溯源需要）；admin 可显式清理（仅在实例 archive 后） |
| `def/*` 分支 | main 上的 def 修改 PR 分支，merge 后可清理 |
| Merge 策略 | **强制 merge commit**（禁 squash / rebase，保留 commit SHA） |
| visibility | **恒定 private**（不允许改 public；防 workflow 业务数据外泄） |
| fork | 不允许 fork 到 `u-<username>/`（workflow 是业务产物） |

### 17.11 多租户约束

| 场景 | 处理 |
|---|---|
| 同一 `(def_slug, instance_id, team_id)` 在 tenant A 与 tenant B 各自 init | 两 tenant 各自独立 team ns → 独立类型 repo + 独立实例 branch（数据隔离；与 §1.5.5 一致） |
| 跨 tenant 协作 | **不可达**——不同 Gitea 实例之间无原生关系；如需跨 tenant 共享，走"显式打包迁移" |
| JWT tenant_id 校验 | 内部接口走 service token；tenant_id 由 X-Tenant-Id header 决定 |
| JWT tenant_id 校验 | init 接口前置校验（§1.5.2），不匹配 → 403 |

### 17.12 审计

| 事件 | 审计来源 |
|---|---|
| wf repo 创建（init → created=true） | Gitea audit log（admin PAT 创建事件） |
| definition.snapshot.yaml 写入 | Gitea commit history（admin PAT commit） |
| owner / collaborator 变更 | Gitea audit log（collaborator API 调用） |
| 节点 push / PR / merge | Gitea audit log（push / PR 事件，归因到调用者 PAT） |
| init 调用本身 | costrict-web server access log（与所有 API 一致） |
| 实例归档 | Gitea audit log（archive 事件）+ instance.json.status 变更 commit |

> server 不维护独立 workflow 审计表——所有审计能力由 Gitea 原生 audit log + git commit history 提供。

### 17.13 与 §11（wizard）的关系

workflow repo **不参与 §11 wizard 流程**——

- workflow 不是 capability item type，不进 §11.3 适用场景表
- workflow repo 路径算法化（用户不能选 namespace / slug）
- 不需要 §11.6 slug-check、§11.7 模板下载、§11.8 wizard step
- csc `wf` 子命令集与 `capability` / `kb` 子命令集**完全分离**

### 17.14 用户 PAT 规则补充

| PAT 类型 | 对 `t-<team_short>/wf-*` 的权限 |
|---|---|
| 用户 fine-grained PAT | 仅可对**所在 team ns 下已授权的 `wf-*` 类型 repo**做 push / pull / clone / 开 PR / merge；**不允许 team ns org 层的 repo:create scope**（须由 `POST /api/internal/workflow/init` 触发首次创建） |
| admin PAT（`costrict-system`） | 全权：跨所有 `t-*` org 的 repo 创建 / 文件写入 / collaborator 管理 / branch protection 配置 / archive |
| 平台 workflow 编排器 PAT | 由 admin 单独签发，scope 限定为 `t-*/wf-*` write（用于节点 PR 自动 merge + 实例状态更新；不在本规范范围） |

### 17.15 与 KB（§16）的设计一致性

| 维度 | KB（§16） | Workflow（§17） |
|---|---|---|
| namespace | `t-<team_short>/kb-*` | `t-<team_short>/wf-*`（类型 repo）|
| 实例模型 | 单 repo 单 main 分支 | 类型 repo + 每实例 `inst-<inst_short>` 分支 |
| 路径推断输入 | `(code_repo_url, team_id)` | `(workflow_def_slug, team_id)` + `instance_id` 推 branch 名 |
| server API | `POST /api/internal/kb/ensure`（幂等） | `POST /api/internal/workflow/init`（首次 init + 重试幂等 + 定义漂移 409） |
| 前置依赖 | team ns 已初始化（`members:sync` 触发懒创建），否则 412 | 同左 |
| repo 结构 | KB 生成服务自定义 | 强制 `.workflow/` 元目录 + `nodes/<seq>-<slug>/` 节点目录；main 存 def canonical |
| 默认协作模式 | 直推 main | 节点级 PR base = `inst-<inst_short>`（按 audit_level 分级）；def 演进 PR base = `main` |
| 生命周期 | 跟随代码 repo（per-team） | 实例完成 → archive（tag + 删 `inst-*` 分支）；类型 repo 永久存在 |
| 数据流 | KB 生成服务 push | 节点执行器 push + reviewer 审计 |
| 归属语义 | 共享（同 team 成员） | owner 字段记录触发者（init 调用者）；类型 repo 永久归 team |

两者共用 v2.15 / v2.16 / v2.17 设计哲学：**server 仅预创建 + 纯函数路径推断 + 回归 git 原生协作 + 零 server 业务状态 + per-team namespace 隔离**。

---

## 18. Team Namespace 管理（v2.17）

team 是**平台级概念**——不是 workflow / kb 专属业务概念。team namespace（team ns）= 一个 per-team 的 Gitea org `t-<team_short_id>`，承载 KB repo（§16）、workflow 类型 repo（§17）以及后续扩展的任何 team-scoped 业务数据 repo。

team 的 truth source 是外部 [`org-team-service`](../identity-tenant/TEAM_ORG_UNIFICATION.md)（与 [`IDENTITY_ARCHITECTURE_ROADMAP`](../identity-tenant/IDENTITY_ARCHITECTURE_ROADMAP.md) ADR-3 v3 一致：team 同步职责由 cs-user 反转给 @server）。本 spec 仅定义 team ns 在 git server 侧的镜像生命周期，不定义 team 业务语义。

### 18.1 设计原则

1. **平台共享**：一个 team 只对应一个 team ns，KB / workflow / 未来扩展共用，不重复创建
2. **lazy 创建**：team ns 不在 team 注册时立即创建——首次 `members:sync` 时才真正建 org；避免孤儿 team 占用 Gitea 资源
3. **零 server 业务状态**：server **不维护 team 表**——team 列表 / 成员关系一律从 `org-team-service` 实时拉取；server 仅镜像 Gitea 侧的 org 状态（可重建、可对账）
4. **命名确定性**：`team_short_id` 由 `team_id`（UUID）经纯函数推导，详见 §18.3
5. **生命周期跟随 team**：team 解散 → team ns archive；team 成员变更 → org 成员同步（delta 模式）
6. **多租户隔离**：team ns 严格受 tenant 边界约束（与 §1.5 协同）；跨 tenant 一律不可见

### 18.2 team ns Gitea org 生命周期

```
                  org-team-service          @server                  Gitea
                        │                       │                      │
team 注册                │── team lifecycle ────▶│                      │
（仅元数据）             │   webhook / pull       │                      │
                        │                       │   （尚未创建 org）   │
                        │                       │                      │
首次加成员               │── members:changed ───▶│                      │
                        │   webhook              │                      │
                        │                       │── GET team info ─────▶│
                        │◀──────── team ─────────│                      │
                        │                       │── POST /admin/users ─▶│ 创建 t-<team_short>
                        │                       │   + invite members     │ org 创建
                        │                       │                      │
后续成员变更             │── members:changed ───▶│── PATCH org members ─▶│ delta 同步
                        │   webhook              │                      │
                        │                       │                      │
team 解散               │── team:dissolved ────▶│── archive org ───────▶│ org archive
                        │   webhook              │   + remove members    │
```

| 状态 | 触发 | server 行为 |
|---|---|---|
| **未创建** | team 已注册但无 `members:sync` | 不做任何 Gitea 操作；KB ensure / workflow init 收到 `TEAM_NS_NOT_INITIALIZED` 412 |
| **已创建** | 首次 `members:sync` | 用 admin PAT 调 Gitea 创建 org `t-<team_short>` + 配置默认权限 + 邀请首批成员 |
| **活跃** | 持续 `members:sync`（delta） | 仅变更成员关系，不动 org 元数据 |
| **archived** | team 解散 webhook | archive org（保留审计窗口），不立即删除 |

### 18.3 命名算法（team_short_id）

`team_short_id` 是 `team_id`（UUIDv4）的**确定性纯函数推导**：

```
function teamShortId(team_id: string) -> string:
    # team_id 形如 "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a"
    if team_id 不是合法 UUID:
        throw InvalidTeamId

    hex = team_id.replace("-", "")              # 32 hex chars
    return hex[:8].toLowerCase()                # 取前 8 hex，小写
                                              # 例："7f3c9a1e"
```

**Gitea owner 命名**：

```
team_ns_org_name = "t-" + team_short_id
# 例："t-7f3c9a1e"
```

| 约束 | 满足方式 |
|---|---|
| Gitea owner 字符集 `[a-z0-9_-]` | `t-` 前缀 + 8 hex（全小写）天然合规 |
| owner 全局唯一 | UUID 前 8 hex 在 tenant 内的碰撞概率 1/2^32（同 tenant 1000 team 期望碰撞 < 10^-6，可接受；碰撞时 `members:sync` 返回 409 `TEAM_NS_CONFLICT`，由 admin 处理） |
| 跨 tenant 隔离 | Gitea 实例本身按 tenant 物理隔离（§1.5），不同 tenant 的 `t-7f3c9a1e` 互不可见 |
| 可读性 | admin 在 Gitea UI 列表能从 owner 名识别"team ns 类 org"，进一步看 description 中的 `team_id` / `team display_name` 可定位具体 team |

**org description**（admin / 审计用）：

```
{
  "description": "team_id=<full uuid>; display_name=<display>; source=org-team-service",
  "website": "<org-team-service 团队页 URL>"
}
```

### 18.4 team ns org 配置

| 字段 | 值 | 说明 |
|---|---|---|
| owner name | `t-<team_short_id>` | 由 §18.3 算法推导 |
| visibility | **private**（强制） | team 内数据天然私密 |
| `members_can_create_repos` | **false** | 仅 server admin PAT 能在该 org 下创建 repo（KB ensure / workflow init 触发） |
| 默认权限 | `member` = **write** | team ns 成员对 org 所有 repo 自动 write（KB / workflow 共用此模型；fine-grained 限制由 owner 显式覆盖） |
| team 映射（Gitea team） | 单一隐式 team "members" | org 创建时自动创建，对应 org 全部成员；不区分 owner / admin / dev 角色（角色语义来自 `org-team-service`，server 不镜像） |
| webhook | 复用 tenant `costrict-config/platform-config` 中 `webhook_secret_ref` | org-level push / member webhook |
| admin PAT | `costrict-system` 的 admin PAT | 唯一执行创建 org / 邀请成员 / 创建 repo 的凭据 |

### 18.5 server API：`POST /api/internal/teams/:team_id/members:sync`

**内部接口**（网关不放行 `/api/internal/*`，仅 `org-team-service` / 编排器等可信服务经 service token 调用；详见 [`TEAM_NAMESPACE_API.md`](./TEAM_NAMESPACE_API.md)）。幂等。

调用方典型场景：
- `org-team-service` 收到 team 成员变更 webhook 后转发到 @server
- 编排器首次为某 team 部署 KB / workflow 时主动触发（lazy init）

#### 18.5.1 请求

```http
POST /api/internal/teams/7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a/members:sync HTTP/1.1
Host: server.costrict.internal
X-Internal-Service-Token: <service_token>
X-Tenant-Id: <tenant_id>（可选，缺省走默认租户）
X-Request-Id: <uuid>（推荐）
Content-Type: application/json

{
  "mode": "delta",                          // "delta" | "full_sync"
  "team_display_name": "Platform Team",     // 可选；首次创建时用作 org description
  "add_members": [
    { "user_id": "u-alice", "gitea_username": "alice" },
    { "user_id": "u-bob",   "gitea_username": "bob"   }
  ],
  "remove_members": [
    { "user_id": "u-charlie", "gitea_username": "charlie" }
  ]
}
```

> `mode=full_sync` 时：add_members / remove_members 之外现存成员**全部 remove**——用于 `org-team-service` 全量对账（每小时 / 每日）。`mode=delta` 仅按 add/remove 变更。

#### 18.5.2 响应（统一 schema）

```json
{
  "team_ns_org": "t-7f3c9a1e",
  "team_ns_exists": true,
  "created": false,
  "members_changed": {
    "added": ["alice", "bob"],
    "removed": ["charlie"]
  },
  "current_members_count": 12
}
```

#### 18.5.3 行为分支

| 场景 | server 行为 | created | team_ns_exists |
|---|---|---|---|
| **team ns 不存在**（首次 sync） | ① 用 admin PAT 调 `POST /admin/users` 创建 org `t-<team_short>` ② 配置 `members_can_create_repos=false` + private ③ 邀请 add_members 加入 org ④ 写入 description | true | true |
| **team ns 已存在 + delta 模式** | ① add_members 调 `PUT /orgs/<org>/members/<user>` 邀请 ② remove_members 调 `DELETE /orgs/<org>/members/<user>` 移除 | false | true |
| **team ns 已存在 + full_sync 模式** | ① `GET /orgs/<org>/members` 取当前成员 ② 计算 diff（add / remove） ③ 按差异调用同上 API | false | true |
| **用户尚未在 Gitea 创建** | 该用户加入被跳过，记入 `skipped` 字段并附 reason（与 §10.12 用户生命周期级联协同） | — | true |

#### 18.5.4 错误码

| HTTP | error_code | 触发条件 | 响应体 |
|---|---|---|---|
| 400 | `INVALID_REQUEST` | `team_id` 非 UUID / mode 不在 enum / add_members 中 `gitea_username` 缺失 | `{ "error": "invalid_request", "detail": "..." }` |
| 401 | `UNAUTHORIZED_SERVICE` | `X-Internal-Service-Token` 缺失或不一致 | `{ "error": "unauthorized_service" }` |
| 404 | `TEAM_NOT_FOUND` | `team_id` 在 `org-team-service` 不存在（server 先调外部校验） | `{ "error": "team_not_found", "detail": "team_id not in org-team-service" }` |
| 409 | `TEAM_NS_CONFLICT` | `team_short_id` 已被另一 team 占用（UUID 前 8 hex 碰撞） | `{ "error": "team_ns_conflict", "hint": "admin must rename or migrate; see ops runbook" }` |
| 500 | `GITEA_API_FAILURE` | admin PAT 调 Gitea 失败 | `{ "error": "gitea_api_failed", "detail": "..." }` |
| 502 | `UPSTREAM_ERROR` | `org-team-service` 调用失败 | `{ "error": "upstream_error", "detail": "org-team-service returned 5xx" }` |

#### 18.5.5 幂等保证

- 相同 `team_id` + 相同成员集重复 `members:sync`：进入 delta 空操作分支，返回 `members_changed.added=[]/removed=[]`
- `mode=full_sync` 多次调用同一目标集：第二次 diff 为空，no-op

### 18.6 解散与归档

team 解散由 `org-team-service` 推送 `team:dissolved` webhook，@server 调专用接口：

```http
POST /api/internal/teams/:team_id/dissolve HTTP/1.1
X-Internal-Service-Token: <service_token>

{ "reason": "team merged into another" }
```

行为：

| 步骤 | 操作 |
|---|---|
| 1. archive org | 调 Gitea API 把 `t-<team_short>` 标记 archived（Gitea 1.20+ 支持的 `PATCH /orgs/<org>` `visibility=private` + custom property `archived=true`，或对所有 repo archive） |
| 2. 移除成员 | 全员从 org 移除（保留 repo 内容） |
| 3. 审计窗口 | 默认 90 天保留；过期后由 ops runbook 决定是否物理删除 |

> **不解散立即删除**：避免误触发 webhook 导致数据丢失；保留审计窗口便于事后取证。

### 18.7 team_id provenance（来源）

| 调用场景 | `team_id` 来源 |
|---|---|
| `members:sync` 调用 | `org-team-service` webhook 直接携带 |
| KB ensure / workflow init | csc 编排器代调时从 team 业务上下文传入；csc **不反查** team 归属——由调用方/编排器负责 |
| team ns org description | 完整 `team_id`（UUID 全量）记录在 Gitea org description，便于 admin 反查 |

**关键约束**：@server **不维护 team 表**。任何"team 是否存在 / team 有哪些成员"的查询，server 都**实时调 `org-team-service`**——server 仅镜像 Gitea 侧 org 状态。

### 18.8 与 §16 / §17 的契约

| 调用方 | 依赖 |
|---|---|
| `POST /api/internal/kb/ensure`（§16.3） | 前置：team ns 已存在；否则返回 412 `TEAM_NS_NOT_INITIALIZED` |
| `POST /api/internal/workflow/init`（§17.3） | 同上 |
| 用户访问 KB / workflow repo | 用户必须是 team ns org 成员（write 默认）；非成员无任何访问权（repo private） |

### 18.9 多租户约束

- team ns 严格落在 tenant 对应的 Gitea 实例（§1.5）；跨 tenant 的 `t-<short>` 互不可见
- `X-Tenant-Id` header 决定 team ns 操作的 Gitea base_url；JWT 中 `tenant_id` claim 必须一致
- 跨 tenant 的 team 共享 / 合并：**不支持**（与 §1.5.5 跨 tenant 场景对齐）

### 18.10 审计

- team ns 生命周期事件（create / members change / dissolve）落入 @server audit log（与 §16.8 / §17.12 同链路）
- Gitea 原生 audit log 保留 org member 变更历史
- `org-team-service` 侧也是独立审计来源（双向对账）

### 18.11 csc 命令契约（轻量）

csc **不直接调 team ns 接口**——team ns 管理是平台侧职责。csc 仅：

| csc 命令 | 行为 |
|---|---|
| `csc kb push`（§16.5） | 调 ensure 后 push；ensure 返回 412 时打印"contact team admin to run members:sync" |
| `csc wf init` / `node push`（§17.9） | 同上 |

team admin 触发 `members:sync` 的入口在 **portal UI**（admin 操作）或 `org-team-service` webhook 自动转发——不在 csc 范围内。

### 18.12 命名约定的未来扩展

`t-` 前缀预留为 team ns 专属；未来引入的其它平台 ns 不得复用 `t-` 前缀。当前规划：

| 前缀 | 当前含义 | 备注 |
|---|---|---|
| `t-` | team namespace（per-team org） | 本节定义 |
| `costrict-*` | 平台固定 org（§2.1.1） | 与 team ns 命名空间严格隔离 |
| `u-<username>` | 用户 namespace（§2.2） | 与 team ns 严格隔离 |

---

## 修订记录

| 版本 | 日期 | 修订内容 |
|---|---|---|
| v1.0 | 2026-07-09 | 基于 v4 提案首次发布（18 章 + 3 附录，详细版） |
| v2.0 | 2026-07-13 | 精简版：聚焦 namespace / repo name / 创建归属 / 协作流程；其余细节指向 v4 提案 |
| v2.1 | 2026-07-13 | 补充 §8 能力项 CRUD 与同步场景、§9 协作编辑能力项（含 collaborator 管理、冲突解决、fork-PR 流程、AI 代用户协作） |
| v2.2 | 2026-07-13 | 补充 §10 能力项操作流程图（含 Gitea API 调用链）：Create / Read / Update / Delete / Fork-PR / Mirror 同步 / 兜底巡检 / GitOps / AI 代用户 / 用户生命周期级联 共 12 张图 |
| v2.3 | 2026-07-13 | 补充 §11 平台自动化创建能力项（repo 自动生成）：namespace 决议强制 `u-<username>/`、repo name 生成算法（slug 规则 + 保留字黑名单 + 冲突处理）、创建调用链、初始化文件模板、错误处理与回滚、AI 代用户路径 |
| v2.4 | 2026-07-13 | 重构 §11 为 **Git-first 引导模式**：平台不再 auto-create repo，改为 wizard 提供 namespace / repo name / 文件模板引导，用户在 Gitea 上手动完成 push 创建；移除 §11.5 自动创建调用链与回滚段；§13 Checklist 同步改为引导式步骤 |
| v2.5 | 2026-07-13 | 重构 §10.3 Read 为**直连 Gitea + JWT 透传**：server 仅返回 `gitea_*_url`（tree / raw / clone / commits / compare），客户端（portal iframe / csc / AI）携带 JWT 直接访问 Gitea，由 fork JWT 中间件 + Gitea 原生 collaborator 权限校验完成鉴权；移除 server 端 permission API 反查（5min Redis cache）与 service PAT download 代理；与 §6.6 双通道 JWT 透传设计对齐；§10.3.7 保留可选 302 兼容入口 |
| v2.6 | 2026-07-13 | **username 改为不可变（immutable）**：§2.2 用户 namespace 新增「username 注册即冻结」原则，禁止改名；§10.1 Gitea API 表删除 `PATCH /admin/users/{name}` 改名行；§10.12 用户生命周期级联删除 `user.updated（username 改名）` webhook 分支与对应 sync worker 调用；保留 disabled / deleted 两路级联不变；display_name / email / avatar 等业务字段仍可改（仅落在 costrict-web `user_profile` 表，不影响 Gitea user 模型）；与 v4 提案 §6.4 / 决策 #16「username 可改性」背离——后续将同步更新提案文档 |
| v2.7 | 2026-07-13 | **新增 §1.5 Git Server 实例寻址（多租户）**：依据 `IDENTITY_ARCHITECTURE_ROADMAP.md` 阶段 A/B，每个 tenant 拥有独立 Gitea 实例（数据隔离 + JWT 隔离）；§1 新增原则 #6「Git server 跟随 tenant」；§1.5 定义 `tenant_configs.<tenant_id>.git.{base_url,jwks_endpoint,admin_pat_secret_ref,...}` yaml schema；强制 JWT claims 新增 `tenant_id`（与 §6.6 fork JWT 中间件协同：先校验 tenant 一致再 RS256 验签）；§1.5.3 给出 URL 解析流程图（客户端 → server 查 tenant_configs → 返回绝对 URL → 客户端直连）；§1.5.4 客户端硬约束（禁止自行拼 `gitea.costrict.local` 前缀）；§1.5.5 跨 tenant 场景（用户串号 → 401、`costrict/` 官方 org 各实例独立副本、跨 tenant fork 不可达）；§10.3.1 字段约定改为 `<tenant_gitea_base_url>/<owner>/<repo>/...` 形式 + 添加多租户解析 note；§1.5.6 文档约定：所有示意 URL 中的 `gitea.costrict.local` 应理解为当前 tenant base_url 占位符 |
| v2.8 | 2026-07-13 | **§11 wizard 流程纳入 Plugin pack 类型**：§11.3 适用场景把 "Plugin pack 创建" 从 ❌ 改为 ✅（前置：用户须为 `costrict-plugins` org collaborator write 权限）；§11.4 重构为 §11.4-A pack（namespace=`costrict-plugins/`，依赖 `ENABLE_PUSH_CREATE_ORG=true`）与 §11.4-B standalone（namespace=`u-<username>/`）双分支；§11.5.4 推荐命名新增 pack 行（`<subject>-pack`，namespace=`costrict-plugins/`）；§11.6 slug-check 新增 pack 响应示例（含 `permission_check` 字段，5min cache + org membership webhook 失效）+ 权限不足示例；§11.7 模板下载 pack 改为返回 `README.md` + `plugins/.gitkeep` 双文件；§11.8 wizard 流程图 Step 1 加 pack 选项（无权限禁用）+ Step 4 按 type 切换 namespace 预览；§11.9 新增 pack 模板内容；§11.10 错误处理加 pack 权限不足场景 |
| v2.9 | 2026-07-13 | **Plugin 升为 first-class item_type**（**明确覆盖 AGENTS.md display-only gate**）：catalog-download pipeline **只是 plugin 的多种来源之一**，非专属通道；plugin / skill / subagent / command / mcp 在新 git 机制中完全平权。§11.3 standalone plugin ✅，去掉 ❌ "display-only 阶段" 行；§11.4-B standalone 类型加 plugin；§11.5.4 加 plugin 命名行（`<subject>-plugin`，namespace=`u-<username>/`）；§11.6 加 standalone plugin slug-check 响应示例（metadata_path=`.plugin.json`）；§11.7 模板下载加 plugin 行（返回 `.plugin.json`）；§11.8 wizard Step 1 加 plugin / Step 4 加 plugin 预览；§11.9 新增 standalone plugin `.plugin.json` 模板（含 `install.method=git-clone` 字段）；§11.11 新增「Plugin 多源架构与 fav/install」章节——三种来源（marketplace catalog / git standalone / git pack 内）共用 `capability_items` 表、favorite 与其他 item_type 同链路无 409 gate、install 走 csc `git-clone` 默认通道；§11.11.5 显式列出与 AGENTS.md 的差异（favorite 渲染 / 409 移除 / 用户可自传 / 立即激活）；§11.14 改为指向 §11.x wizard 入口的索引（删除原"独立流程"占位） |
| v2.10 | 2026-07-13 | **合并 `costrict-plugins/` 入 `costrict/`；pack 与 standalone 完全平权**：v2.9 让 plugin first-class 后暴露 v4 遗留的不对称——pack 强制塞 `costrict-plugins/` org 而 standalone 走 `u-<username>/`，差异纯属历史包袱。本轮统一：①§2.1 四个固定 org 减为**三个**（删 `costrict-plugins`，pack 与 standalone 共享 `costrict/`，slug 跨 kind 唯一）；②§5 归属矩阵新增「用户自建 pack」行（`u-<username>/<pack-slug>`）；PAT scope 删 `costrict-plugins`；③§11.3 删 "pack collaborator 前置" 行，新增"pack 同链路无前置"行；④§11.4 重构为**统一规则**（删 §11.4-A / §11.4-B 双分支），所有 type / kind 一律 `u-<username>/`、ENABLE_PUSH_CREATE_USER 创建、PR 升级到 `costrict/`；⑤§11.5.3 保留字去 `costrict-plugins`；§11.5.4 pack namespace 改为 `u-<username>/`（升级 `costrict/`）；⑥§11.6 slug-check 删 pack permission_check / 权限不足示例，pack namespace 改为 `u-<username>/`；⑦§11.8 wizard 删 "pack 无权限禁用" 提示；Step 4 pack 预览改 `u-alice/` namespace；⑧§11.10 删 "type=pack 权限不足" 错误行；⑨§11.11.1 来源 C 改为 `u-<username>/<pack-slug>/` 或 `costrict/<pack-slug>/`；⑩§12 PAT owner 删 `costrict-plugins`；⑪§14 命名示例全面更新（用户 namespace 加 pack；官方 org 改 `costrict/<pack-slug>`）；⑫`costrict-plugins` 整体退出 spec，所有引用清零 |
| v2.11 | 2026-07-13 | **官方 ns 改名 + curated 合并 + 新增 `costrict-template/`**：①§2.1 固定 org 三→四：`costrict/` 改名 **`costrict-official/`**（语义更明确）；新增 **`costrict-template/`**（wizard 模板真相源）；②`curated-seed` mono-repo 概念**取消**——精选改为 admin 在 `capability_items.tags` 打 `curated` 标签，与 namespace 解耦；§4.4 删 `costrict/curated-seed` 固定名 repo 行；§5 归属矩阵删 "精选 seed" 行、加 "官方 curated" 行；③§4.4 新增 `costrict-template/templates` 固定名 repo（mono-repo，含 `skill.md.tmpl` / `agent.md.tmpl` / `commands/<slug>.md.tmpl` / `mcp.json.tmpl` / `.plugin.json.tmpl` / `pack/README.md.tmpl` + `pack/plugins/.gitkeep`）；④§11.7 模板下载 API 改写：真相源迁移到 `costrict-template/templates` git repo（5min cache + webhook 失效），admin 可 PR 维护无需 server 发布；支持 tenant fork 覆盖（`tenant_configs.<tenant_id>.template.repo` 决议）；⑤§11.5.3 保留字黑名单加 `costrict-official` / `costrict-template` / `templates`，删 `curated-seed`；⑥全文 `costrict/<slug>` → `costrict-official/<slug>`（§1.5.5 / §5 / §7-§9 / §11 / §12 / §14 共 20+ 处引用）；⑦§14 命名示例加 "官方 curated" + "平台模板" 行；可解析 URL 示例改 `costrict-official/` 前缀 |
| v2.12 | 2026-07-13 | **取消 `pack` / `seed` kind**——前者经评估无独立场景（多 plugin 集合可通过 `capability_items.tags` / marketplace catalog / bundle 表达，不必引入 path 级粒度），后者在 v2.11 curated-seed 概念取消后已是僵尸行：①§3 kind 表删除 `pack` / `seed` 两行，仅保留 `standalone` / `mirror`，新增 v2.12 解释段；②§1 原则 #3 简化为「1 repo = 1 能力项」；③§4.2 标题改 standalone 命名约定，删 pack 命名行，加 plugin 命名行；④§4.4 `costrict-template/templates` mono-repo 文件清单删 `pack/README.md.tmpl` + `pack/plugins/.gitkeep`；⑤§5 归属矩阵删 "用户自建 pack" / "官方 pack" 两行；强约束删 "slug 跨 kind 唯一" 改为 "slug 唯一"；⑥§7.1 默认通道适用列表删 pack 行；⑦§8.1 Create 表删 "pack owner 新增 plugin" 行；⑧§8.5 sync worker 路径过滤说明改为单行（删 standalone/pack 分支）；⑨§9.1 协作场景矩阵去除所有 pack 提及；⑩§10.8 mirror 初始同步文件筛选说明删 "pack 上游 plugins/*/.plugin.json" 分支；⑪§11.3 适用场景删 "Pack 创建" 行；⑫§11.4 namespace 决议原则去除 kind 措辞；ENABLE_PUSH_CREATE_USER 说明改为按顶层 metadata 文件名判定 type；⑬§11.5.4 推荐命名删 pack 行；⑭§11.6 slug-check 删 pack 响应示例（含重复 header）+ 关键约束删 v2.10 历史包袱；⑮§11.7 模板下载表删 pack 行 + 删 pack 模板两份文件 note；⑯§11.8 wizard Step 1 type 选项删 pack、Step 4 预览删 type=pack 区块；⑰§11.9 整段删除 pack README 模板（含 plugins/.gitkeep 占位说明）；install.subpath 字段说明改为「standalone plugin 恒为 `.`」；⑱§11.11.1 Plugin 来源三种→两种（删 C. Git 自托管 pack 内）；⑲§11.15 标题改 "Standalone plugin 创建流程"，删 Plugin pack 行；⑳§14.1 / §14.2 / §14.3 命名示例删 pack 相关行；㉑v2.10 "slug 跨 kind 唯一" 约束随 pack kind 一并退出 |
| v2.13 | 2026-07-13 | **`costrict-config` org 改为 private（强制）**——封堵 tenant 配置泄露面：①§2.1 固定 org 表 `costrict-config` visibility 由 `public（强制）` 改为 **`private（强制；仅 costrict-system + platform_admin 可见）`**，并在用途列明确列出 admin PAT secret_ref / webhook HMAC secret_ref 等敏感字段；②§4.4 固定名 repo 行 `costrict-config/platform-config` 同步标注 private + 仅 admin / `costrict-system` 可见；③§5 归属矩阵 "平台配置" 行 visibility 改为 private；④§5 强约束新增两条：用户 PAT **不允许任何权限（read / write）访问 `costrict-config`**、`costrict-config/` org **恒定 private**——`GiteaConfigSyncWorker` 用 `costrict-system` admin PAT 拉 yaml（§10.10），platform_admin 通过 costrict-web `/admin/git-config` UI 编辑（背后仍走 admin PAT），普通用户在 portal / csc / Gitea UI 均**不可见、不可 clone**；⑤§12 PAT 规则隐含一致（用户 fine-grained PAT scope 不含 `costrict-config` owner） |
| v2.14 | 2026-07-13 | **模板 init 改为从 `costrict-template/templates` git clone，HTTP 模板 API 降级为只读预览**：v2.11 引入 `costrict-template/templates` 作为模板真相源后，wizard 仍按"下载单文件 + 用户手动 mkdir"流程组装 repo，未充分利用 git-truth 源。本轮将脚手架 init 完全 git 化：①§4.4 `costrict-template/templates` 描述改为 **按 type 分子目录**（`skill/` / `subagent/` / `command/` / `mcp/` / `plugin/`），每子目录是该 type 的完整脚手架（含可选 README / index.js 骨架）；②§11.7 重构为「**模板仓库与 init 流程**」：新增 §11.7.1 仓库结构图、§11.7.2 csc 一键 init 命令（`csc capability init --type --slug --from=<template-repo-url>`，内部 sparse-clone + 占位符替换 + re-init + push）、§11.7.3 纯 git + sed 流程（详尽命令模板）、§11.7.4 HTTP 模板 API 降级为 **portal 只读预览**（Step 4 实时显示将生成的文件内容）；③§11.8 wizard Step 5 命令模板三选一：**方案 A** csc 一键 init（推荐）/ **方案 B** 纯 git + sed（无 csc）/ **方案 C** Gitea UI 手建（与之前一致）；④§11.9 模板内容章节小标题改为 `<type>/<file>.tmpl` 路径形式（`skill/skill.md.tmpl` / `plugin/.plugin.json.tmpl`），其他 type 用对称说明替代原"同 v2.3 §11.5.2-11.5.4"悬空引用；⑤§11.10 错误处理表新增三类场景：**模板预览 API 失败**（不影响 init 命令复制）、**`git clone` 模板仓库失败**（csc / git 原生报错 + wizard 提示）、**sparse-checkout `<type>/` 不存在**（csc 报错 + wizard 列出可用 type）、**占位符替换后文件名冲突**（保留 `.tmpl.orig` 备份）；⑥§11.12 AI agent 路径明确两种内容生成方案：**路径 A** 从模板 init（AI 替换占位符 + 写正文）/ **路径 B** 纯 AI 生成（跳过模板）；⑦设计收益：模板可以是多文件脚手架（plugin type 含 `.plugin.json` + `index.js` + `README.md`）；模板版本化天然支持（pin 到 tag / branch / commit）；tenant fork 自定义模板时 init 流程不变（csc `--from` 指向 tenant fork URL）；与 §11 "Git-first 引导模式" 完全对齐——init 一律走 git，无 server 中间环节 |
| v2.15 | 2026-07-14 | **新增 KB（知识库）业务数据管理与协作**：KB 不是 capability item type，是独立的业务数据——本地代码 repo 经 csc 生成 KB 文档，推送到 git server 上"代码 repo ↔ KB repo"一一对应，同代码 repo 的不同用户共享同一 KB repo。设计要点：**server 仅预创建，其余全部回归 git 原生协作**。具体改动：①§2.1 固定 org 表四→**五**：新增 `costrict-kb/`（**private** + `members_can_create_repos=false` + 无 team + SSO 不自动加入，所有 repo 由 server admin PAT 在 ensure 接口中创建）；②§4 新增 §4.6「KB repo 命名（v2.15）」——命名不依赖用户输入 slug，由 server 端**纯函数从 `code_repo_url` 确定性推导**（格式 `costrict-kb/<host>__<escaped_segments>`，详细算法、转义、长度截断、20+ 测试用例见独立 spec [`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md)）；③§5 归属矩阵新增「知识库（KB）repo」行（创建权独占给 server admin PAT，visibility private）；§5 强约束新增两条——`costrict-kb/` org 的 repo:create 权限**独占给 server admin PAT**（用户 PAT 不允许任何 `costrict-kb` owner 的 repo:create scope，绕过 ensure 会断 owner 授权链）；④新增 §16「KB 仓库管理与协作」全章——§16.1 设计原则（一一对应 / 纯函数 / server 仅预创建 / 无 server 状态 / 无权场景透明化）；§16.2 `costrict-kb/` org 配置（private + members_can_create_repos=false + 无 team + 复用 tenant webhook）；§16.3 唯一 API `POST /api/kb/ensure`（幂等，三分支：首次创建+加 owner+配 branch protection / 存在且可访问 / 存在但无权返回 owner 信息）；§16.4 协作流程（push / authorize / revoke / transfer-owner 全部回归 Gitea API 原生）；§16.5 csc 子命令契约（thin client，统一先 ensure 再走 git/Gitea API）；§16.6 分支与可见性（仅 main、禁止 fork、恒 private）；§16.7 多租户隔离；§16.8 审计（不维护独立审计表，依赖 Gitea 原生 audit log）；§16.9 与 §11 wizard 关系（KB 不参与 wizard 流程）；§16.10 PAT 规则补充；⑤§15 禁止事项新增 4 条（用户绕过 ensure 直建 / csc 内置算法副本 / 维护绑定表 / server 编排协作流程）；⑥设计收益：server 零状态、零编排；路径算法单点真理（仅 server，csc 不内置）；权限模型扁平到 repo 级 collaborator；无权场景对调用者透明可读；与 §1 "Git 为内容真相源" 原则对齐——所有 KB 内容操作走 git 原生 |
| v2.16 | 2026-07-14 | **新增 Workflow（业务数据）实例级管理与节点级 PR 审计**：Workflow 不是 capability item type，是独立的业务数据——平台 workflow 编排器启动实例时，每个任务节点的交付物（artifact）按节点序号推送、PR 审计、merge 入 main，最终 repo 沉淀完整可溯源的实例时间线。设计要点：**实例 → repo 一一对应；节点级 PR 审计；server 仅 init，其余全部回归 git 原生协作**。具体改动：①§2.1 固定 org 表五→**六**：新增 `costrict-workflow/`（**private** + `members_can_create_repos=false` + 无 team + SSO 不自动加入，所有 repo 由 server admin PAT 在 init 接口中创建）；②§4 新增 §4.7「Workflow repo 命名（v2.16）」——命名不依赖用户输入 slug，由 server 端**纯函数从 `(workflow_def_slug, instance_id)` 确定性推导**（格式 `costrict-workflow/<def_slug>__<instance_short_id_8hex>`，详细算法、UUID 截短、转义、长度截断、20+ 测试用例见独立 spec [`WORKFLOW_REPO_PATH_ALGORITHM.md`](./WORKFLOW_REPO_PATH_ALGORITHM.md)）；③§5 归属矩阵新增「Workflow 实例交付物 repo」行（创建权独占给 server admin PAT，visibility private）；§5 强约束新增一条——`costrict-workflow/` org 的 repo:create 权限**独占给 server admin PAT**（用户 PAT 不允许任何 `costrict-workflow` owner 的 repo:create scope，绕过 init 会断 owner 授权链 + definition snapshot 缺失）；④新增 §17「Workflow 仓库管理与协作」全章——§17.1 设计原则（实例一一对应 / 纯函数 / server 仅 init / 无 server 状态 / 无权场景透明化 / 平台 namespace + owner 字段）；§17.2 `costrict-workflow/` org 配置；§17.3 唯一 API `POST /api/workflow/init`（**非幂等**：重复 init 同 instance_id + 相同 definition 重试允许，不同 definition 返回 409 definition_drift 防漂移；三分支行为同 KB ensure）；§17.4 实例 repo 结构（`.workflow/` 元目录含 instance.json / definition.snapshot.yaml / audit-config.json / node-prs.json + `nodes/<seq>-<slug>/` 节点目录）；§17.5 节点级 PR 审计流程（节点执行器切分支 + 开 PR + reviewer 审计 + merge commit 强制保留节点 commit SHA + audit_level 三级分级 strict/auto/experimental）；§17.6 协作流程（node push / authorize / revoke / transfer-owner 全部回归 Gitea API 原生）；§17.7 实例生命周期（running → completed → archived；归档触发 Gitea archived flag + 强化 branch protection；保留期默认永久）；§17.8 三层可溯源机制（commit 历史 + PR 链接 + 跨节点 input.json 数据流）；§17.9 csc 子命令契约（thin client，统一先 init 再走 git/Gitea API；含 `wf node push/list/approve/merge` + `wf archive` + `wf list --mine`）；§17.10 分支与可见性（主跟踪 main + node feat 分支；强制 merge commit；恒 private；禁止 fork）；§17.11 多租户隔离；§17.12 审计（Gitea audit log + git commit history 双重）；§17.13 与 §11 wizard 关系（workflow 不参与 wizard）；§17.14 PAT 规则补充；§17.15 与 KB 设计一致性对照（共用 server 仅预创建 + 纯函数 + 零状态哲学）；⑤§15 禁止事项新增 5 条（用户绕过 init 直建 / csc 内置算法副本 / 维护绑定表 / server 编排节点 PR 流程 / 跨实例共享同 repo（C 方案））；⑥设计收益：实例级权限隔离 + 节点级 PR 审计 + 完整可溯源 + 算法单点真理 + server 零状态；与 §1 "Git 为内容真相源" 原则对齐——所有 workflow 内容操作走 git 原生 + PR；设计讨论否决了 C 方案（类型 repo + 实例 branch）与 D 方案（实例归属用户 namespace），理由详见 v2.16 设计讨论纪要 |
| v2.17 | 2026-07-15 | **架构反转：KB / workflow repo 落 team namespace；workflow 改类型 repo + 实例 branch**。v2.16 之前的"全局 `costrict-kb/` + `costrict-workflow/` org"模型在 team 权限边界上不自然——同 team 内的多个 KB / workflow 共享一组 team 成员，per-repo collaborator 既冗余又无法统一管理。本轮把团队作为**平台级概念**（不是 workflow 业务专属）注入 git server 命名空间：①§2.1 重组为 §2.1.1「平台固定 org（4 个）」+ §2.1.2「Per-team org（动态创建）」——`costrict-kb/` / `costrict-workflow/` **整体退出** spec，所有引用清零；新增 `t-<team_short_id>/` per-team org 模型；②§4.6 KB repo 命名改为 `t-<team_short>/kb-<host>__<escaped_segments>`（加 `kb-` 统一前缀）；③§4.7 Workflow repo 命名改为 `t-<team_short>/wf-<def_slug>` 类型 repo（加 `wf-` 统一前缀），每个实例是 `inst-<inst_short_id>` branch（base = main）；④§5 归属矩阵 KB / workflow 两行路径与权限模型全面更新——**team ns org 成员关系 = 唯一权限来源**，不再 per-repo collaborator；§5 强约束新增 `t-*` owner repo:create 独占 server admin PAT；⑤**§16 KB 章重写**：唯一接口改 `POST /api/internal/kb/ensure`（内部接口 + service token），去除 per-repo collaborator 模型，去除 authorize / revoke / transfer-owner csc 子命令（统一走 `members:sync` team 级管理）；⑥**§17 Workflow 章重写**：**§17.1.1 v2.16 → v2.17 C 方案决策反转说明**——重新评估 v2.16 否决理由：(a) "路径纯度"理由作废（路径仍由纯函数推导，team ns 引入不破坏算法性）；(b) "分支删除即销毁实例"理由缓解（archive 而非 delete + branch protection 保护）；(c) "PR 嵌套 base = inst"理由解决（csc 适配多级 base）；(d) "权限粒度粗"理由被 team ns 解决（team ns org 成员 = 自然权限边界）。新增 `POST /api/internal/workflow/init` 内部接口（响应含 `wf_repo_path` + `instance_branch`），新增 `csc wf def update` 子命令（main 上的 def PR），节点 PR base = `inst-<short>`（非 main），新增 `inst-*` 通配 branch protection；⑦**新增 §18「Team Namespace 管理」全章**：定义 team ns Gitea org 生命周期（lazy 创建于首次 `members:sync`）、`team_short_id` 命名算法（UUID 前 8 hex）、`POST /api/internal/teams/:team_id/members:sync` 内部接口（delta / full_sync 双模式）、team 解散归档（90 天审计窗口）、team_id provenance（外部 `org-team-service` 为 truth source，@server 不维护 team 表）、与 §16 / §17 / §1.5 多租户的契约；⑧§15 禁止事项更新——`t-*` repo:create 独占、用户 PAT 禁止 `t-*` repo:create scope；⑨KB_REPO_PATH_ALGORITHM.md / WORKFLOW_REPO_PATH_ALGORITHM.md 同步 bump v1.0 → v2.0（加 `team_id` 入参 / 拆分双函数）；⑩WORKFLOW_WORKSPACE_API.md 重构为 TEAM_NAMESPACE_API.md（平台级 team ns + 成员同步 + workflow init + kb ensure 内部接口集）；⑪CSC_KB_SUBCOMMAND_CONTRACT.md / CSC_WF_SUBCOMMAND_CONTRACT.md 同步更新（team ns 路径 / 去除 authorize-revoke-transfer-owner / PR base = `inst-<short>` / 新增 def update）；⑫与 [TEAM_ORG_UNIFICATION ADR-3 v3](../identity-tenant/TEAM_ORG_UNIFICATION.md) 对齐——team 同步职责由 cs-user 反转到 @server，本 spec 提供具体 git server 侧镜像机制；⑬设计收益：(a) team 权限边界自然（一个 team 一个 ns，成员关系即权限）/ (b) workflow 类型 repo + 实例 branch 减少 repo 膨胀（同 def N 个实例从 N repo 降为 1 repo + N branch）/ (c) workflow def canonical 存储统一（main 上的 PR 协作，团队级 def 演进）/ (d) 节点 PR base = `inst-<short>` 让实例时间线独立（main 永远干净）/ (e) csc 简化（无 service token，由编排器代调内部接口） |
