# Git 方案下的创作者旅程

> 系列文档之三，总览见 [`GIT_UX_JOURNEYS_OVERVIEW.md`](./GIT_UX_JOURNEYS_OVERVIEW.md)。
> 角色：技能作者（人类或 AI agent）——创建、发布、迭代、协作的一方。
> 这是 Git 方案**收益最直接**的旅程：V4 §9「AI 操作工作流」的设计初衷就是它。
> 判定图例：✅ 天然满足 · 🔧 补足后满足 · 📌 需决策

## 来源标注约定

`[现状]` 代码已实现 · `[V4 §x]` V4 原文写了 · `[推导]` V4 未写但属架构必然要求 · **`[新增建议]`** ⚠️ 本文提议的产品增强（汇总在文末「可选增强」，可独立取舍）

---

### C1 我写好一个技能，push 即发布

> 作为创作者，我希望写完就能发布，不用填表单、不用等后台，以便保持创作节奏。

**Git 方案下的流程**：

1. 在 `u-alice/my-skill` 建 repo（Gitea，个人 namespace 自动归属，§4.3）
2. 写 `skill.md`（统一 frontmatter schema，§5.2），`git push`（用户自己的 PAT，§7.1）
3. webhook 触发 → server 解析 frontmatter → upsert 索引 → marketplace 可见（server，§10.3）
4. AI agent 同样能做：读写文件 + git 命令，不用学 REST schema（§9.3）

**V4 已支持**：§4.8.1 完整定义（默认 public、直推 main、无需 admin 介入）；版本历史、diff、blame 全部免费。

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| slug 格式白名单校验——它同时是客户端本地目录名与 Gitea 路径名，现状无任何校验 | `[推导]` | web / gitea |

**判定**：✅ —— 这是 V4 的主场，相比现状的表单上传 + tarball 摄取是质的改善。

---

### C2 我迭代技能，订阅者自然拿到新版本

> 作为创作者，我希望每次 push 后订阅者能感知到更新，以便迭代真正触达用户。

**Git 方案下的流程**：

1. `git push` 新 commit（Gitea）
2. webhook → server 更新 `git_sha`（§10.3）
3. 订阅者客户端比对 sha 发现更新 → 按各自策略更新（csc）
4. 我**不需要**记得手动 bump frontmatter 里的 version 字段——sha 就是版本

**V4 已支持**：`git_sha` 字段（§14.1）；webhook 增量同步（§10.3）。

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| `git_sha` 透出到列表/详情 API，客户端更新判定切到 sha | `[推导]` | web / csc |
| 客户端拉更新的参数修正（现状传 slug 但服务端只认 UUID，必 404） | `[现状缺陷]` | csc |

**判定**：🔧 —— 现状"作者不改版本号，用户永远拿不到更新"的坑被结构性消除；这是创作者最有感的改善。

---

### C3 我想先打草稿，改好了再发布

> 作为创作者，我希望有草稿区，以便未完成的内容不被人看到。

**Git 方案下的流程**：

- **方式一**：repo 设为 private（§4.7 用户可在自己 namespace 内改 private 作草稿）→ 完成后切 public
- **方式二**：开 feature branch 迭代 → 完成后 merge 到 main（发现层只索引 main）

**V4 已支持**：§4.7 明确"草稿/未发布通过 private 表达"；branch 是 Git 原生能力。

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| 确认 webhook 只处理默认分支的 push（§10.3 未明说是否过滤非 main 分支） | `[推导]` | web |

**判定**：✅ —— 两种草稿方式都是 Git 原生能力，现状反而没有草稿概念。

---

### C4 别人 fork 我的技能改进，我可以合回来

> 作为创作者，我希望别人能基于我的技能改进并回馈，以便技能越用越好。

**Git 方案下的流程**：

1. 使用者点 fork → Gitea 原生 fork 到 `u-bob/my-skill`（§9.2）
2. Bob 修改、push、对我的 repo 发 PR（Gitea 原生）
3. 我在 Gitea UI 里 review、merge —— 订阅者随 C2 链路拿到合入后的版本
4. marketplace 展示 fork 关系：`ForkedFromItemID` 由 Gitea fork 关系反查（§14 字段映射表）

**V4 已支持**：fork / PR / upstream 追踪全部原生；§9.2 可选 PR 通道已设计。

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| fork 关系反查接入 marketplace 展示（`[V4 §14]` 已列字段来源「`ForkedFromItemID` \| 由 Gitea fork 关系反查」，需实现） | `[V4 §14]` | web |

**判定**：✅ —— 现状 fork 完全断链（fork 件独立、无 upstream、无回流）；Git 方案下这条链路是原生的。

> 把下发的 `permission_mode=forkable` 接到 Gitea fork API 属新增功能——该权限模式现状从未实现（实际只有 `readonly \| dismissible`，`distribution_service.go:71`），V4 也未涉及。见「可选增强」CE-1。

---

### C5 我申请官方认证

> 作为创作者，我希望把打磨好的技能升级为官方认证，以便获得更高曝光与信任。

**Git 方案下的流程**：

1. 提 PR 到 `costrict/<slug>`（§4.8.2）
2. admin 审核质量 / 安全 / 方向 → merge → repo transfer-ownership 到 `costrict/` org
3. 原 `u-alice/` 地址留 redirect，存量链接不断；DB 的 `source_repo_url` 由 sync worker 自动跟随（§4.8.2）
4. 已订阅/已下发该技能的用户**无感**——item ID 不变，只有 repo 坐标更新

**V4 已支持**：§4.8.2 完整定义，含 redirect 与索引跟随。

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| transfer 后 `source_repo_url` 更新与既有引用可解析性的验证用例 | `[推导]` | web |

**判定**：✅ —— 设计完整，只差落地验证。

---

### C6 我要下架/删除我的技能

> 作为创作者，我希望下架后订阅者得到干净的退场，以便不留死链接。

**Git 方案下的流程**：

1. 删 repo 或归档 repo（Gitea）
2. webhook → server 标记 `status='archived'`（§10.3 删除语义）
3. 订阅者列表里消失 → 客户端 orphan 检测 → 自动卸载（现状链路保留）

**V4 已支持**：§10.3 的 archived 标记语义；下游卸载链路现状已有。

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| archived 与现状物理删除的语义对齐——现状 `itemdelete.go:76,107-119` 级联物理删 favorites/distributions/receipts；V4 的 archived 保留审计更好，但需联动下发状态（现状归档后下发记录仍显示 active） | `[推导]` | web |

**判定**：🔧 —— V4 的软删除语义反而比现状更好（下发审计历史得以保留），补一个状态联动即可。

---

### C7 我不小心 force-push 重写了历史 / 有人恶意篡改

> 作为创作者（以及平台），我希望已发布的历史不可被悄悄改写，以便订阅者拿到的内容可信。

**Git 方案下的流程**：

1. main 设受保护分支：禁 force-push、禁 delete（§13.4 branch-protection.yaml 已设计，GitOps 下发）
2. `[V4 §7.3.2]` 分支保护「仅防历史覆写」——已发布的 commit 不会被悄悄改写
3. 万一历史仍被改写（admin 操作）：check hook 检测到 metadata 文件消失/漂移 → 标 `polluted`（§11.2）

**V4 已支持**：分支保护 GitOps（§13.4）、健康度兜底（§11）双层防护都已设计。

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| 新 repo 自动应用保护规则（§13.4 配置已设计，需落地执行器） | `[V4 §13.4]` | web / gitea |

**判定**：✅ —— 防护设计完整，这正是 DB 方案给不了的完整性保证。

---

### C8 我的技能带二进制资产 / 是多文件结构

> 作为创作者，我希望图片、脚本、子目录都能随技能一起发布，以便技能是完整可用的。

**Git 方案下的流程**：

1. repo 里自然组织多文件（Git 原生）
2. 小于 1MB 的二进制直接进 Git；大文件走 LFS 或 Release attachment（§5.4）
3. 订阅者安装时拿到完整文件树

**V4 已支持**：§5.4 资源策略。

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| 分发侧子树读取路径——§10.2 的 trees API 原文限定为「一次性拉整棵文件树（**mirror 初始同步**）」，§10.3 又明确「子目录变更……**直接忽略**」，分发侧如何取整棵能力项文件树未定义 | `[推导]` | web / csc |
| 三套二进制载体收敛：Git/LFS/Release（§5.4）vs S3（PR #185 已落地） | `[推导]` | web |

**判定**：✅ 创作侧天然满足（多文件组织是 Git 原生）+ 🔧 分发侧读路径需补（与使用者旅程 U8 同一问题）。

---

## 补足项清单（本篇汇总）

| # | 补足项 | 来源 | 组件 | 出处 |
|---|---|---|---|---|
| C-a | slug 格式白名单 | `[推导]` | web / gitea | C1 |
| C-b | `git_sha` 透出 + 客户端判定切 sha | `[推导]` | web / csc | C2 |
| C-c | 客户端更新参数 bug 修复 | `[现状缺陷]` | csc | C2 |
| C-d | webhook 默认分支过滤确认 | `[推导]` | web | C3 |
| C-e | fork 关系反查接入 marketplace | `[V4 §14]` | web | C4 |
| C-f | transfer 后可解析性验证用例 | `[推导]` | web | C5 |
| C-g | archived 联动下发状态 | `[推导]` | web | C6 |
| C-h | 新 repo 自动应用分支保护 | `[V4 §13.4]` | web / gitea | C7 |
| C-i | 分发侧子树读路径 + 三载体收敛 | `[推导]` | web / csc | C8 |

---

## 可选增强（`[新增建议]`，非 V4 范围）

| # | 增强 | 动机 | 与 V4 的关系 | 组件 |
|---|---|---|---|---|
| **CE-1** | 下发的 `permission_mode=forkable` 接 Gitea fork API | 接收方想基于下发内容改造 | 该权限模式现状从未实现；V4 未涉及下发与 fork 的联动 | web / gitea |
| **CE-2** | 被下发能力项要求来自受保护分支或官方 org | §4.8.1 允许无审核直推 main，下发内容可被作者随时改 | ⚠️ 比 V4 默认策略更严格——§4.8.1 明确「无需 admin 介入，无审核流程」。需与 V4 作者对齐（见总览 P-3） | web / gitea |
