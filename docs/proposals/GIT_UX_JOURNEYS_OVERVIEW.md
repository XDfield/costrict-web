# Git 方案下的用户旅程走查 · 总览

| 字段 | 内容 |
|---|---|
| 目的 | 站在用户角度走查 Git 方案（V4）下的使用体验，标出 V4 已覆盖的、必须补的、以及本文额外提议的 |
| 状态 | 评估稿 v2（经独立溯源审查修订） |
| 基线 | `refs/heads/main` @ `1be76e0`；portal gitlink `187073c` |
| 对照 | `docs/repo-management/CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md` |

---

## 1. 文档索引

| 旅程 | 角色 | 文档 |
|---|---|---|
| 使用者 | 订阅者 / 下发接收方 | [`GIT_UX_JOURNEY_CONSUMER.md`](./GIT_UX_JOURNEY_CONSUMER.md) |
| 下发者 | 部门主管 / 平台管理员 | [`GIT_UX_JOURNEY_DISTRIBUTOR.md`](./GIT_UX_JOURNEY_DISTRIBUTOR.md) |
| 创作者 | 技能作者（人类 / AI agent） | [`GIT_UX_JOURNEY_CREATOR.md`](./GIT_UX_JOURNEY_CREATOR.md) |
| 角色与权限 | 跨旅程 | [`GIT_UX_ROLES_AND_PERMISSIONS.md`](./GIT_UX_ROLES_AND_PERMISSIONS.md) |
| 能力项治理与控制台 | 使用者 / 作者 / 管理员 | [`GIT_UX_JOURNEY_ITEM_LIFECYCLE.md`](./GIT_UX_JOURNEY_ITEM_LIFECYCLE.md) |

> 第六篇基于**实读 portal 全部 9 个页面目录**补齐前五篇遗漏的功能面：fork、个人控制台、MCP 密钥配置、下发向导、管理后台治理、**3128 行的能力项编辑器**、仓库管理（唯一入口）、会话消费链路等。
>
> 扫描结论：`kanban/`、`projects/`、`multica/` 与能力项**完全无关**（全量 grep 零命中）；`workspace/`、`session/` 只含消费端链路；`admin/`、`console/` 含大量遗漏功能。

> 各篇文末均有「可选增强」节，集中放置 `[新增建议]` 类条目（合计 66 项）——评审 Git 迁移时可整节跳过。

---

## 2. V4 已明确带来的增益

只列 `[V4 §x]` 有逐字依据的。上一版列的 8 条净增益中，「下发可锚定、可受控升级」等条已被溯源为扩张，此处移除。

| 增益 | V4 依据（逐字） | 受益 |
|---|---|---|
| **内容与版本真相源迁 Git** | §2.1「把 `capability_items` 的内容与版本真相源从 PostgreSQL 迁移到基于 Gitea 的 Git 仓库」 | 全部 |
| **版本历史由 commits API 替代快照表** | §10.2「列出文件历史 commit（**替代 CapabilityVersion**）\| `GET .../commits?path={filepath}`」 | U12 详情页 |
| **`git_sha` 作为精确的内容标识** | §14.1 新增 `git_sha`（顶层 metadata 文件所在 commit 的 SHA） | U2 C2 更新感知 |
| **push 即发布，无需 admin 介入** | §4.8.1「用户 PAT 直推 main → 进发现层（marketplace）…无需 admin 介入，无审核流程」 | C1 创作 |
| **fork / PR 原生协作** | §9.2 可选 PR 通道；§14 字段映射「`ForkedFromItemID` \| 由 Gitea fork 关系反查」 | C4 |
| **官方认证走 transfer + redirect** | §4.8.2「merge → repo 落到 `costrict/` org（Gitea 原生 transfer-ownership…）」「原 repo 留 redirect」 | C5 |
| **软删除保留审计** | §10.3「删除 → 标记 `status='archived'`」 | C6 |
| **历史防篡改** | §13.4 branch-protection GitOps；§7.3.2「仅防历史覆写」 | C7 |
| **健康度治理体系** | §11 四态 + §11.3 多端透传 + §11.5 消费端策略 + §11.8 告警 | D7 治理 |
| **扫描短路键迁 `git_sha`** | §12「保留 LLM + 切触发源 + 短路键改 `git_sha`（双写兼容期）」 | D4 |
| **webhook 增量同步替代 tarball 摄取** | §10.6 catalog ingest 冻结或废弃，改 Gitea mirror pull + webhook | L3 演进 |

> **正交能力（Git 方案不影响，迁移后原样保留）**：主管下发视野由组织树拓扑推导（非叶子部门成员 ⇒ 管辖该子树），支持子树/子部门/个人混合目标，depth-1 懒加载，后端前缀校验兜底。详见下发者篇 D4.5。

---

## 3. 主线补足项

仅保留 `[现状缺陷]` 与 `[推导]`。上一版的 G-01~G-19 中属需求扩张的已移至各篇「可选增强」。

| # | 补足项 | 来源 | 组件 | 量级 |
|---|---|---|---|---|
| **G-01** | **详情接口 `content` 字段的取数契约**——客户端唯一内容来源是 `GET /api/items/:id` 的 `content`（`favorite.ts:252-298`），而 V4 对该字段的口径自相矛盾（见 §5 V4-1），必须先澄清再定契约 | `[推导]` | web | 大 |
| **G-02** | **`git_sha` 透出到 API + 客户端更新判定改比 sha**；现状比 frontmatter `version`（`favorite.ts:1117`），作者不 bump 就永不更新 | `[推导]` | web / csc | 中 |
| G-03 | **修复更新链路参数 bug**：`updateFavoriteItem` 传 slug（`sync.ts:97`）但服务端只认 UUID（`capability_item.go:1165`），自动更新必 404 | `[现状缺陷]` | csc | 小 |
| G-04 | 版本历史接口从 `CapabilityVersion` 切到 commits API | `[V4 §10.2]` | web | 中 |
| G-05 | 多文件/二进制读路径——§10.3 只同步顶层 metadata、忽略子目录；§10.2 的 recursive trees 限定「mirror 初始同步」用途。需定义分发侧的子树读取方式 | `[推导]` | web / csc | 大 |
| G-06 | 三套二进制载体收敛：DB / Git LFS+Release（§5.4）/ S3（PR #185 已落地） | `[推导]` | web | 中 |
| G-07 | 下发结果返回未覆盖名单与原因；现状未登录成员被静默丢弃（`distribution_service.go:551-564`） | `[现状缺陷]` | web | 小 |
| G-08 | 缓存副本清理：`removeItemFromConfig` 的 `_localPath` 参数未使用（`favorite.ts:675-720`），卸载后 `~/.costrict/favorites/` 残留 | `[现状缺陷]` | csc | 小 |
| G-09 | `redeliver_at`：resume 不变更 `createdAt`，客户端 watermark 不感知，恢复对客户端不生效 | `[现状缺陷]` | db / web / csc | 小 |
| G-10 | 备份目录自我递归：备份写入源目录自身 `.backup/`，而 `copyDir` 不排除它（`favorite.ts:575-586`） | `[现状缺陷]` | csc | 小 |
| G-11 | slug 格式白名单——slug 同时是客户端本地目录名与 Gitea 路径名，现状无任何校验 | `[推导]` | web / gitea | 小 |
| G-12 | 客户端身份键从 slug 迁到 UUID / source coordinate；服务端唯一键是 `(repo_id,item_type,slug)`，客户端只用 slug（`favorite.ts:428-445`） | `[推导]` | csc | 中 |
| G-13 | `state.json` 跨进程锁与原子写（现状无锁 read-modify-write，`favorite.ts:136-153`） | `[现状缺陷]` | csc | 中 |
| G-14 | health 透传落点——§11.3 要求内嵌「下发 manifest」，但下发链路无 manifest 概念，需确定挂载点 | `[推导]` | web / csc | 小 |
| G-15 | `repo_members` 与 Gitea collaborator 的关系（V4 §4.7「纯 Gitea visibility 透传」下两套成员模型的归属） | `[推导]` | web / gitea / db | 中 |
| G-16 | 迁移期：item ID 保持、历史版本是否重建为 commit、存量 plugin 子项归属 | `[推导]` | web / db | 中 |
| G-17 | webhook 默认分支过滤；transfer 后坐标更新一致性 | `[推导]` | web | 小 |
| G-18 | Store 门户适配：详情预览接新读路径、健康雷达对齐 §11、private 标记与申请入口 | `[推导]` | web / portal | 中 |
| **G-20** | **fork 从 DB 复制切到 Gitea 原生 fork**——现状是 DB 行复制（`ForkItem`），V4 §14 要求 `ForkedFromItemID` 由 Gitea fork 关系反查；现状的业务规则（不可 fork 自己、archive 类不支持、fork 不自动订阅、三态按钮）需在新实现下保持 | `[V4 §14]` | web / portal | 中 |
| **G-21** | **在 V4 显式声明「用户级配置值永不进 Git」**——`MCPUserConfig`（per-user 密钥，`models.go:53-67`）V4 全文未提及，需写入非目标清单避免误迁 | `[推导]` | 文档 | 小 |
| G-22 | 标签归属决策（DB 业务字段 vs frontmatter）——影响「改标签是否产生 commit」 | `[推导]` | web | 小 |
| G-23 | `itemApi.transfer` 与 §4.8.2 transfer-ownership 的语义对齐 | `[推导]` | web | 小 |
| **G-24** | **仓库管理页的 Gitea 版实现**——`console/dashboard-repositories.tsx` 是全站唯一 repo 管理入口，成员/可见性/手动同步三个语义全在迁移影响面上 | `[推导]` | web / portal | 中 |
| **G-25** | **G-12 身份键迁移的影响面扩展**——portal 的 `/hub` 弹窗经 `/api/v1/agents/favorites/{slug}` 控制设备端加载，slug 是跨系统主键 | `[推导]` | portal / csc | 中 |
| G-26 | 编辑器的双分支写入语义收敛 + 远端二进制回读校验路径的 Git 版实现 | `[推导]` | web / portal | 中 |
| G-19 | `createdBy` 语义延续：Git 路径下需从 commit author email（§14.1 `git_author_email`）映射回平台账号，否则大客户品牌标识失效 | `[推导]` | web | 小 |

---

## 4. 真正未决的架构问题

上一版列了 6 个决策项，其中 **P-2 / P-3 / P-5 已被 V4 明确决定**，本文不应平行重开——若要推翻，须先修改 V4 决策章节，而非在 UX 文档里另起炉灶：

| 曾列为决策 | V4 实际已决 |
|---|---|
| ~~P-2 plugin 下发粒度~~ | §2.1「能力项粒度 = repo 或 repo + path，**不下钻**」；§17 第 5 项已定 |
| ~~P-3 内容分发路线三选一~~ | §17 第 18 项已定「HTTP 代理 + Gitea permission API 反查（A2 方案）」「csc 端零改造」 |
| ~~P-5 部门真相源~~ | §8.1「dept-sync \| 部门树 + 成员关系**真相源**」 |

剩余真正未决的：

| # | 问题 | 说明 |
|---|---|---|
| **P-1** | **下发的内容授权如何实现** | V4 §4.7「纯 Gitea visibility 透传」+ §7.2.2 admin PAT 仅限 2 个场景（capability 索引同步、用户生命周期级联），**均不含下发授权**。而下发接收方不是 Gitea collaborator ⇒ 私有能力项下发后拿不到内容。任何解法都需修改 V4 的既有决策，需 V4 作者定夺 |
| **P-2** | **网页创作路径的落点** | 主体是 `console/capability-editor-page.tsx`——**3128 行的完整多文件编辑器**（多文件树、资产管理、二进制 SHA-256 校验重打包、版本回滚），加上 store 的表单直建/zip 上传/外部仓库同步。而 V4 §3.2 明确「**禁止方向：DB → Git**」、§9 只有用户/AI 直接 push。**这条路径的影响面比表单弹窗大一个数量级**，V4 未定义其落点 |
| **P-3** | **下发内容的可变性是否可接受** | §4.8.1 允许作者无审核直推 main，被下发的能力项内容可在下发者不知情时变化。这是产品口径问题：接受，还是对被下发内容加约束（如限受保护分支/官方 org）？ |

---

## 5. V4 自身需先澄清的矛盾

溯源过程中发现 V4 文档内部有两处不一致，建议先修 V4 再谈落地：

| # | 矛盾 | 原文对照 |
|---|---|---|
| **V4-1** | **`Content` 字段是否废弃** | §5.3 仍写「`Content` \| 顶层 metadata 文件正文」；§10.3 流程仍写「update content」；§14.1 的废弃字段清单**未列** Content。但附录 C.3 Stage 5 写「`capability_items.Content` … 字段删除」。**这直接决定 G-01 能否定义** |
| **V4-2** | **csc 走不走 git** | §7.3.1 表格内两行写「**用户 PAT（csc / AI agent 代用户）** \| clone private repo / push main」，同表最后一行却写「csc 设备端 \|（**不走 git**，走 HTTP 代理）」；附录 C.2.2 又写「csc 直接拉 Gitea」而 C.2.3 写「content 下发仍走 costrict-web HTTP 代理」 |

另附**两条新增反馈**（第六篇 I1/I3）：① `MCPUserConfig` 的存在 V4 完全未提及，这是 per-user 密钥数据，建议显式写入非目标清单；② fork 现状是 DB 复制且带一批业务规则（不可 fork 自己、archive 类不支持、不自动订阅），这些在 Gitea 原生 fork 下如何保持 V4 未涉及。

另附一处**基线过时**：附录 C.2.3 称 csc 的 plugin 仅需「marketplace 安装跳转」，但客户端实际已实现 favorite 驱动的自动安装/卸载/重试（`csc/src/costrict/favorite/reconcileCloudPlugins.ts:320-487`）与自动更新（`csc/src/utils/plugins/pluginAutoupdate.ts:108-209`）。V4 附录 C 基于 2026-07-06 代码，需按新基线重做。

---

## 6. 使用建议

1. **先解决 §5 的两处 V4 内部矛盾**——V4-1 不澄清，G-01（整条链路的第一块多米诺）无法定义。
2. **再定 §4 的 P-1**——它决定私有能力项下发是否成立。
3. §3 的补足项中，G-03 / G-08 / G-09 / G-10 是**与 Git 迁移无关的现状 bug**，今天就能修，不必等 V4。
4. 各篇「可选增强」节（共 66 项）建议按主题拆成独立 RFC（entitlement 账本、生效对账、企业 org 品牌、项目能力集、网页代提交等），不计入 Git registry 迁移的收益或必需项。
