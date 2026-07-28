# Git 方案下的能力项治理与个人控制台

> 系列文档之六，总览见 [`GIT_UX_JOURNEYS_OVERVIEW.md`](./GIT_UX_JOURNEYS_OVERVIEW.md)。
> 本篇补齐前五篇遗漏的前端功能面——**基于实读 portal 全部 9 个页面目录**（store 16328 行 + admin 6557 + console 7583 + workspace/session/kanban/projects/multica）。
> 来源标注：`[现状]` 代码已实现 · `[V4 §x]` V4 原文写了 · `[推导]` 架构必然要求 · **`[新增建议]`** 本文提议

---

## 0. 遗漏说明

前四篇基于组件文件名与 API 路由推断功能面，遗漏了前端实际实现的一批能力。本篇按实读代码补齐，重点是 **fork**、**个人控制台**、**MCP 用户级配置**三块——其中后两块在 Git 方案下有**结构性影响**。

---

### I1 我 fork 一个能力项，改造成自己的版本

> 作为使用者，我希望基于别人的技能改造出自己的版本，以便复用他人成果。

**现状实现**（前端 `item-detail-content.tsx:618-640`，后端 `capability_item.go` `ForkItem`）：

- `[现状]` **fork 按钮三态**：已有我的 fork（`myForkItemId`）→ 跳转编辑该 fork；无 fork → 执行 fork；未登录 → 禁用并提示
- `[现状]` **可 fork 条件**：仅 public、`status=active`、非本人创建的 item；`SourceType=archive`（打包类）明确不支持（返回 `fork_archive_unsupported`）
- `[现状]` **fork 后不自动订阅**——代码注释明确「Fork only creates a DB copy — it deliberately does NOT auto-subscribe」
- `[现状]` **溯源展示**：详情页显示「Fork 自 xxx」，`forkedFromOwnerId === "system"` 时显示「官方」友好文案；`forkedFromItemId` 可点击跳原件（`:1333`）

**Git 方案下的变化**：

`[V4 §9.2]` fork 变成 **Gitea 原生 fork**——`u-bob/my-skill` 是真实的 Git fork，带 upstream 关系；`[V4 §14]` 字段映射表写明「`ForkedFromItemID` \| **由 Gitea fork 关系反查**」。

这带来三个现状做不到的能力：

| 能力 | 现状 | Git 方案 |
|---|---|---|
| upstream 追踪 | ❌ fork 件是独立 DB 行，与原件无连接 | ✅ Gitea 原生 fork 关系 |
| ahead/behind 可见 | ❌ | ✅ 可查落后原件几个 commit |
| 改进回流 | ❌ 无回流路径 | ✅ 对原 repo 发 PR |
| archive 类可 fork | ❌ 明确不支持 | ✅ Git fork 与内容形态无关 |

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| fork 从「DB 复制」切到「Gitea fork API + 关系反查」，`ForkedFromItemID` 改为反查而非存储 | `[V4 §14]` | web |
| 三态按钮的判定改为查 Gitea fork 关系（`myForkItemId` 语义变化） | `[推导]` | web / portal |
| archive 类 fork 限制可解除——需确认 V4 下打包类能力项的表示形态 | `[推导]` | web |
| fork 后是否自动订阅：现状明确不自动，Git 方案下建议保持该产品口径 | `[现状]` | — |

**判定**：✅ Git 方案下 fork 能力显著增强 + 🔧 需切换实现。

---

### I2 我在个人控制台管理我的能力项与下发

> 作为使用者，我希望有一个集中位置管理我创建的、收藏的、收到的、发出的能力项。

**现状实现**（`pages/manager.tsx`，1566 行，四个 tab）：

| Tab | 内容 | 可执行动作 |
|---|---|---|
| `created` 我创建的 | 我发布的能力项 | 编辑、删除、批量删除、打标签、转移仓库 |
| `favorited` 我收藏的 | 我订阅的能力项 | 取消收藏 |
| `received` 我收到的 | 收到的下发 | 标记已读、dismiss（忽略） |
| `sent` 我发出的 | 我发出的下发 | 撤销下发 |

`[现状]` 每个 tab 带独立分页、筛选（类型/分类/安全状态/来源/标签）、计数徽章（`refreshTabCounts`）、「选中全部匹配项」的批量语义。

**Git 方案下的变化**：

1. `[推导]` `created` tab 的编辑动作——现状走表单 `PUT /items/:id`，Git 方案下落点未定义（总览 P-2 网页表单创作问题的一部分）
2. `[V4 §4.8.2]` 转移仓库（`itemApi.transfer`）与 V4 的 `transfer-ownership` 语义重合，需对齐——V4 讲的是 `u-alice/` → `costrict/` 的官方认证升级，现状是 repo 间移动
3. `[推导]` 打标签（`setTags`）——标签存 DB 还是进 frontmatter？V4 §5.2 定义了统一 frontmatter schema，但未说明标签归属
4. `[现状]` `received` / `sent` 两个 tab 的下发管理不受 Git 方案影响

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| 📌 `created` tab 的编辑落点（同总览 P-2） | `[推导]` | web |
| `transfer` 与 V4 §4.8.2 transfer-ownership 的语义对齐 | `[推导]` | web |
| 标签归属决策：DB 业务字段 vs frontmatter（影响「改标签是否产生 commit」） | `[推导]` | web |
| 批量删除在 V4 软删除语义（§10.3 `archived`）下的行为 | `[推导]` | web |

**判定**：🔧 —— 控制台本身是 DB 业务功能，但「我创建的」这一 tab 的写操作全部依赖 P-2 的决策。

---

### I3 我给订阅的 MCP 能力填自己的密钥

> 作为使用者，我订阅一个 MCP 能力后需要填入自己的 API Key 等参数才能使用。

**现状实现**（`PUT /api/items/:id/mcp-config`，`handlers/mcp_config.go:203`；模型 `MCPUserConfig`，`models.go:53-67`）：

- `[现状]` 能力项作者在 MCP 配置里留占位符（`env:<NAME>` / `args:<INDEX>`）
- `[现状]` 每个用户填自己的值，存 `MCPUserConfig` 表，**唯一索引是 `(user_id, item_id)`——per-user 私密数据**
- `[现状]` 支持 `secret: true` 标记，返回时做 masked 处理
- `[现状]` 合并语义：空值清除该键；不匹配格式的键被忽略

**Git 方案下的关键约束**：

> **这是 Git 方案的一条硬边界：用户填入的密钥绝对不能进 Git。**

`[V4 §2.2]` 明确「不重写业务字段存储」，`MCPUserConfig` 属业务数据留在 DB——这一点 V4 是安全的。但需要明确的是：

| 层 | 归属 | 说明 |
|---|---|---|
| MCP 配置**模板**（含占位符声明） | Git（能力项内容的一部分） | 随 `mcp.md` / 配置文件进 repo |
| 用户**填入的值**（含密钥） | **DB，永不进 Git** | `MCPUserConfig` 表 |

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| 在 V4 中明确「用户级配置值不进 Git」这条边界（当前 V4 未提及 MCP 用户配置的存在） | `[推导]` | 文档 |
| 模板变更（作者改了占位符名）时用户已填值的迁移策略——Git 方案下模板变更是一个 commit，需定义兼容处理 | `[推导]` | web |

**判定**：✅ 现状设计与 Git 方案兼容（业务数据留 DB）+ 🔧 需在 V4 中显式声明该边界，避免后续误迁。

---

### I4 我把能力项设为内置

> 作为平台管理员，我希望把某些能力项标记为「内置」，让所有用户默认获得。

**现状实现**：`[现状]` 详情页有「设为内置 / 取消内置」动作（`item-detail-content.tsx:896-929`，含 plugin 与非 plugin 两种文案）；`GET /api/plugins/builtin` 列出内置 plugin（`main.go:434`）；`BuiltinContentDialog` 组件负责配置。

**Git 方案下的变化**：`[推导]` 内置标记是平台侧的分发策略（DB 业务字段），与内容存储无关，V4 迁移不影响。但它与「下发」是两套并行的分发机制——V4 讨论下发时未涉及内置。

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| 明确内置与下发的关系（两套分发机制是否统一到一套 entitlement 语义） | `[推导]` | web |

**判定**：✅ 不受 Git 方案影响。

---

### I5 我手动触发一次安全扫描

> 作为能力项作者，我希望改完内容后立即触发扫描，而不必等自动流程。

**现状实现**：`[现状]` `scanApi.trigger` → `POST /api/items/:id/scan`；详情页展示扫描状态、风险等级、结论、扫描模型（`scanApi.getStatus` / `list`）。

**Git 方案下的变化**：`[V4 §12]` 扫描触发源从 ingest/sync 迁到 webhook，短路键从 `item_revision` 改 `git_sha`。**手动触发入口 V4 未提及**——但它在新触发源下仍有意义（重扫某个 commit）。

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| 手动触发扫描在 `git_sha` 短路键下的行为（同一 sha 重复触发是否跳过、如何强制重扫） | `[推导]` | web |

**判定**：🔧 —— 入口保留，短路逻辑需适配。

---

### I6 其他已实现但未被前五篇覆盖的功能

以下均为 `[现状]` 已实现，Git 方案下**不受影响或影响很小**，此处登记以确保清单完整：

| 功能 | 实现位置 | Git 方案影响 |
|---|---|---|
| **分享**（复制链接 / 二维码） | `item-detail-content.tsx` share.* | 无 |
| **AI 内容评价**（contentQuality / evaluator / 多维打分 / overall breakdown） | `item-detail-content.tsx:1044-1161` | 无（DB 业务数据）；但评价绑定的是 item 还是 commit 需明确 |
| **健康度雷达**（含 popularityExcluded / signalsExcluded 说明） | `health-radar.tsx` | 🔧 需与 V4 §11 的 health 四态对齐（总览 G-18） |
| **plugin 子项树** | `sub-item-tree.tsx` | 🔧 受 §2.1「不下钻 plugin」影响，子项来源需改 |
| **相似推荐** | `searchApi` / `/items/:id/similar` | 无 |
| **最佳实践轮播** | `best-practice-carousel.tsx` | 无 |
| **通知渠道管理**（企业微信 / 微信扫码登录） | `channelApi`、`wechat-login-dialog.tsx` | 无 |
| **仓库成员邀请** | `invite-dialog.tsx`、`repoApi.listMembers` | 🔧 与 Gitea collaborator 的关系（总览 G-15） |
| **外部仓库同步管理** | `repo-sync-tab.tsx`、`syncApi`、`repoRegistryApi` | 🔧 §10.6 改 mirror pull + webhook |
| **版本详情查看** | `itemApi.getVersion(id, revision)` | 🔧 切 commits API（总览 G-04） |
| **资产清单** | `itemApi.getAssets` | 🔧 多文件读路径（总览 G-05） |
| **管理员批量导入** | `adminImportApi`（含 `confirmLargeDelete` 保护） | 🔧 §10.6 catalog ingest 冻结后该入口的去留 |
| **管理员能力项管理 / 审计 / 公告 / 权限授予** | `adminItemApi` / `adminAuditApi` / `adminAnnouncementApi` / `adminGrantApi` | 无 |
| **设备管理与远程命令** | `deviceApi` / `updateApi.sendCommand` | 无 |

---

### I7 我用下发向导批量投放能力项（管理后台）

> 作为平台管理员/部门主管，我希望一次把多个能力项投放给一批目标，以便批量铺开。

**现状实现**（`admin/components/distribution-wizard-dialog.tsx`，616 行，三步向导）：

| 步骤 | 内容 |
|---|---|
| **step 1 选能力项** | `[现状]` **支持多选**（`selectedItems` 数组，`:47`）——搜索后勾选多个 |
| **step 2 选目标** | `[现状]` `scopeType: user \| department`；部门树懒加载（首次切到 department scope 才拉） |
| **step 3 选项** | `[现状]` `permissionMode`（readonly/dismissible）、`expiresAt`、附言 |

**权限视野的实现**（修正下发者篇 D4.5 的两处描述）：

- `[现状]` `displayedDepts = unlimited ? deptTree : authorityDepts`（`:96`）——平台管理员拉全量树（`adminDeptApi.tree()` → `/api/admin/departments/tree`），**部门主管只渲染 `/distributions/my/authority` 返回的管辖子树**。代码注释：「guarantees a manager can never even see an out-of-reach dept」（`:95`）
- `[现状]` dept-sync 不可用时有专门的 inline notice（`DEPT_UNAVAILABLE_CODE = "dept_sync_unavailable"`，`:36`）
- ⚠️ **修正**：下发者篇 D4.5 称「depth-1 懒加载」不准确——向导用的是**全量树接口** `adminDeptApi.tree()`；depth-1 的 `adminDeptApi.children(parentId)` 是另一个接口（`api.ts:1553-1556`），用在别处

**批量下发的实现方式**：

`[现状]` 提交时前端 **循环调用单 item 接口**（`:249-250` `for (const item of store.selectedItems) { await distributionApi.distribute(item.id, ...) }`）。

⚠️ 这意味着：**UX 层支持批量，API 层仍是单 item × 多 target，且整个批量操作无事务性**——第 3 个 item 失败时前 2 个已经下发。这与下发者篇 L1-7（单请求多 target 无事务边界）是同类问题，但在更外层。

**Git 方案下的变化**：`[推导]` 向导本身是 DB 业务流程，不受影响。但若采用需要 Gitea 授权的方案（总览 P-1），批量 × 部门展开会放大为 M×N 次 Gitea API 调用。

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| 批量下发的失败处理与幂等（现状循环调用无回滚、无断点续传） | `[现状缺陷]` | web / portal |
| 若走 Gitea 授权路线：批量场景的异步化与限流（§10.7 per-repo 并发 ≤ 20） | `[推导]` | web |

**判定**：✅ 向导交互完整 + 🔧 批量事务性欠缺（与 Git 迁移无关的现状问题）。

---

### I8 我在管理后台治理全平台能力项与下发

> 作为平台管理员，我需要全局视角管理能力项与下发记录。

**现状实现**：

| 页面 | 行数 | 能力 |
|---|---|---|
| `admin/pages/content.tsx` | 902 | `[现状]` 能力项列表+筛选、**上下架**（`setStatus`）、单个/批量删除（带 `MAX_BATCH_DELETE` 上限与 plugin 数量提示）、**「选中全部匹配项」语义**、**导出 CSV**（`exportCsvUrl`） |
| `admin/pages/distributions.tsx` | 602 | `[现状`] 全平台下发列表、按 status/scope 筛选、active/paused 计数统计、**直接 pause / resume / revoke**（`:187-211`）、查看 receipts 明细 |
| `admin/pages/import.tsx` | 725 | `[现状]` catalog bundle 导入的完整工作流：stats → preview → confirm 两阶段；状态机 `pending/running/previewed/success/failed/expired/cancelled`；**大量删除保护**（`confirmLargeDelete` + `largeDeleteBlocked` 双重）；页面离开后可恢复预览状态 |
| `admin/pages/enterprise.tsx` | 132 | `[现状]` 大客户品牌配置 CRUD |
| `admin/pages/ops.tsx` | 743 | `[现状]` 通知渠道、系统设置、审计日志（与能力项弱相关） |

> 前五篇只写了管理员「可查看」全平台下发，实际**可直接操作** pause/resume/revoke。

**Git 方案下的变化**：

| 功能 | 影响 |
|---|---|
| 上下架（`setStatus`） | `[推导]` V4 §10.3 的删除语义是标记 `archived`；上下架与之的关系需对齐 |
| 批量删除 | `[推导]` 软删除语义下的行为（同 I2） |
| **catalog bundle 导入** | 🔴 `[V4 §10.6]` 明确「catalog ingest **冻结**（方式 B）或**完全废弃**（方式 C）」——**这是一个有 725 行完整 UI、带两阶段确认与大量删除保护的功能，V4 未说明该入口的去留** |
| 导出 CSV | `[推导]` 不受影响 |

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| 📌 **catalog bundle 导入入口在 ingest 冻结后的去留**——若废弃，现有的两阶段预览确认、大量删除保护等治理能力需要在新的 Git 入口重建 | `[推导]` | web / portal |
| 上下架与 §10.3 `archived` 语义对齐 | `[推导]` | web |

**判定**：🔧 —— 治理能力大部分不受影响；**bundle 导入是唯一被 V4 直接判处的功能，需明确迁移路径**。

---

### I9 我在网页上编辑能力项的完整内容（能力项编辑器）

> 作为创作者，我希望在网页上编辑技能的多个文件、管理资产、回滚版本，而不必接触 git。

**现状实现**：`console/capability-editor-page.tsx`，**3128 行，全站最大文件**。路由 `/capabilities/new` 与 `/capabilities/:itemId/edit`（`routes.tsx:173-174`），从 store 详情页与控制台跳入。

- `[现状]` **多文件树编辑器**——编辑 SKILL.md + 资产 + 二进制，打包成 zip 或 JSON assets 写入
- `[现状]` **双分支写入语义**（`:2440-2470`，注释在 `:2395-2405`）：JSON 分支不带 `assets` 字段时**服务端保留原资产行**；multipart(zip) 分支是 **delete-and-rebuild**
- `[现状]` **远端二进制回读**（`:2299-2325` `fetchRemoteBinaryBytes`）：从 `/api/registry/:repo/:itemType/:slug/*path` 读回二进制，**SHA-256 校验后重新打包**
- `[现状]` **版本回滚已是 commit 语义**（`:2226-2247`）：读旧版本内容 → 以 `commitMsg: "restore revision N"` 重新 PUT
- `[现状]` 版本历史、标签、分类、namespace 下拉（public / repo:）、plugin 子项列表（`itemApi.list({parentPluginId})`）

**这修正了总览 P-2 的性质判断**：

> ⚠️ 前几篇把「网页表单创作」描述为 `create-capability-dialog` 这类轻量弹窗。**实际主体是这个 3128 行的完整编辑器**——它有多文件树、资产管理、二进制校验重打包、版本回滚。P-2（V4 §3.2「禁止方向：DB → Git」与网页创作路径的冲突）的影响面比原先估计的大一个数量级。

**Git 方案下的变化**：

| 现状能力 | Git 方案下 | 判定 |
|---|---|---|
| 版本回滚（`commitMsg: restore revision N`） | `[推导]` 前端已是 commit 语义，**天然对齐**——可直接映射为 git revert 或新 commit | ✅ 概念契合 |
| 多文件树编辑 | `[推导]` 一次编辑 = 一个 commit，比现状的 revision 更自然 | ✅ |
| 双分支写入（JSON 保留资产 / zip 重建） | `[推导]` Git 下只有「提交一棵树」一种语义，双分支需收敛 | 🔧 |
| 远端二进制回读 + SHA-256 重打包 | `[推导]` **必须保住这条路径**——Git/LFS/Release 下的读回方式需重新设计 | 🔧 |
| 写入落点 | 📌 **依赖 P-2 决策**——§3.2 明确「禁止方向：DB → Git」 | 📌 |

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| 📌 编辑器写入落点（总览 P-2 的主体） | `[推导]` | web / portal |
| 双分支写入语义在 Git 下的收敛 | `[推导]` | web |
| 远端二进制回读 + 校验重打包路径的 Git 版实现 | `[推导]` | web / portal |

**判定**：📌 —— 编辑器是网页创作路径的核心资产，其存续完全取决于 P-2。

---

### I10 我管理自己的仓库（全站唯一入口）

> 作为创作者，我需要创建仓库、邀请成员、设置可见性、触发同步。

**现状实现**：`console/dashboard-repositories.tsx`（321 行），路由 `/console` 根路径。

- `[现状]` repo CRUD（`repoApi.listMy/create/update/delete`）
- `[现状]` 成员邀请（`POST /api/repositories/:id/invitations`）
- `[现状]` 可见性展示（public / private / repo，`:140-145`）
- `[现状]` 对 `repoType === "sync"` 的 repo **手动触发同步 + 15 秒轮询状态**（`:57`、`:75`、`:125`）

> ⚠️ **这是全站唯一的仓库管理入口**——store manager 里的 repo 管理已被整块注释掉（`manager.tsx:26` import 与 `:1051-1080` 的 `CreateRepoDialog` 均在注释中）。

**Git 方案下的变化**：`[推导]` 三个语义全部落在迁移影响面上——

| 语义 | Git 方案下 |
|---|---|
| repo 成员 | 与 Gitea collaborator 的关系（总览 G-15） |
| 可见性 | §4.7「纯 Gitea visibility 透传」，判据换成 Gitea `is_private` |
| 手动同步 | §10.6 改 mirror pull + webhook，手动触发入口与轮询状态需重做 |

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| 仓库管理页三个语义（成员/可见性/同步）的 Gitea 版实现 | `[推导]` | web / portal |

**判定**：🔧 —— 唯一入口，迁移必须覆盖。

---

### I11 我在会话里使用能力项（消费端链路）

> 作为使用者，我希望在会话中启用技能、切换 agent、执行自定义命令。

前五篇只写到「客户端落盘」，没写落盘之后如何被消费。实际有一条独立链路：

- `[现状]` **portal 网页的 `/hub` 弹窗**（`session/slash-actions.tsx:62-66` → `components/dialog-favorites.tsx`）——**调用的是设备端 daemon API，不是 portal 的 `/api/items`**：
  - `GET /api/v1/agents/favorites`
  - `POST /api/v1/agents/favorites/{slug}/load` / `/unload`
  - 状态机：`Cloud / Downloaded / Active / Unloaded`（`dialog-favorites.tsx:11-19`）
- `[现状]` **agent 选择器**（`/agents` 斜杠命令）——切换当前会话使用的 agent
- `[现状]` **自定义 slash command 执行透传**（`slash-actions.tsx:107-115`）——command 类能力项的执行入口
- `[现状缺陷]` 会话视图向渲染层注入 `agent` / `command` 数据集，但 **`mcp: {}` 是空壳未接线**（`workspace/components/device-session-view.tsx:380-400`）

**Git 方案下的关键影响**：

> `[推导]` 这条链路以 **`slug` 作为跨系统主键**（portal → daemon → 本地状态）。总览 G-12 提出「客户端身份键从 slug 迁到 UUID / source coordinate」——**该改动的影响面因此扩大到 portal 与设备 daemon 的接口契约**，不只是 csc 内部。

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| G-12 身份键迁移需同步覆盖 portal ↔ daemon 的 `/api/v1/agents/favorites/{slug}` 接口 | `[推导]` | portal / csc |
| MCP 在会话渲染层的接线（现状空壳） | `[现状缺陷]` | portal |

**判定**：🔧 —— 消费链路本身不受内容真相源影响，但身份键迁移的影响面比原估计大。

---

### I12 重复实现与孤儿路由（登记）

`[现状]` `console/dashboard-capabilities.tsx`（587 行）与 store 的 `manager.tsx` **高度重复**——同样的 listMy / favorited / favorite 切换，但 console 版**没有任何 distribution 调用**，是功能更薄的重复实现。

`[现状]` `/console`（仓库管理）与 `/console/capabilities` 的菜单项均被注释掉（`console/lib/menu-registry.ts:17,21`），当前是**孤儿路由**——可路由但导航不可达。且 `dashboard-repositories.tsx:191` 的回跳路径 `/store/dashboard/repositories` 与实际路由 `/console` 对不上。

> 这与 Git 迁移无关，但评估改动面时需知道：**仓库管理这个"唯一入口"目前导航不可达**，迁移时容易被漏掉。

---

## 补足项清单（本篇汇总）

| # | 补足项 | 来源 | 组件 | 出处 |
|---|---|---|---|---|
| I-a | fork 从 DB 复制切到 Gitea fork + 关系反查 | `[V4 §14]` | web / portal | I1 |
| I-b | fork 三态按钮判定改查 Gitea fork 关系 | `[推导]` | web / portal | I1 |
| I-c | archive 类 fork 限制的解除条件 | `[推导]` | web | I1 |
| I-d | 📌 控制台「我创建的」编辑落点（同总览 P-2） | `[推导]` | web | I2 |
| I-e | `transfer` 与 §4.8.2 transfer-ownership 语义对齐 | `[推导]` | web | I2 |
| I-f | 标签归属决策（DB vs frontmatter） | `[推导]` | web | I2 |
| I-g | 批量删除在软删除语义下的行为 | `[推导]` | web | I2 |
| **I-h** | **在 V4 中显式声明「用户级配置值（含密钥）永不进 Git」** | `[推导]` | 文档 | I3 |
| I-i | MCP 配置模板变更时已填值的迁移策略 | `[推导]` | web | I3 |
| I-j | 内置与下发两套分发机制的关系 | `[推导]` | web | I4 |
| I-k | 手动扫描在 `git_sha` 短路键下的强制重扫 | `[推导]` | web | I5 |
| I-l | AI 评价绑定 item 还是 commit | `[推导]` | web | I6 |
| I-n | 批量下发的失败处理与幂等（前端循环无事务性） | `[现状缺陷]` | web / portal | I7 |
| I-o | 📌 catalog bundle 导入入口在 §10.6 ingest 冻结后的去留（含两阶段确认与大量删除保护的重建） | `[推导]` | web / portal | I8 |
| I-p | 上下架（`setStatus`）与 §10.3 `archived` 语义对齐 | `[推导]` | web | I8 |
| **I-q** | 📌 **能力项编辑器（3128 行）的写入落点**——P-2 的主体，含多文件树、双分支写入、二进制校验重打包 | `[推导]` | web / portal | I9 |
| I-r | 双分支写入语义（JSON 保留资产 vs zip 重建）在 Git 下的收敛 | `[推导]` | web | I9 |
| I-s | 远端二进制回读 + SHA-256 校验重打包的 Git 版实现 | `[推导]` | web / portal | I9 |
| I-t | 仓库管理页三语义（成员/可见性/手动同步）的 Gitea 版实现 | `[推导]` | web / portal | I10 |
| **I-u** | G-12 身份键迁移需覆盖 portal ↔ daemon 的 `/api/v1/agents/favorites/{slug}` 契约 | `[推导]` | portal / csc | I11 |
| I-v | MCP 在会话渲染层接线（现状 `mcp: {}` 空壳） | `[现状缺陷]` | portal | I11 |

---

## 给 V4 的两条新增反馈

1. **`MCPUserConfig` 的存在 V4 完全未提及**（I3）。这是 per-user 的密钥数据，必须明确留在 DB。V4 §2.2 的「不重写业务字段存储」隐含覆盖了它，但因为涉及密钥，建议**显式写入非目标清单**，避免后续实施时误判为「能力项配置」而迁入 Git。

2. **fork 的实现形态差异**（I1）。V4 §14 说 `ForkedFromItemID` 由 Gitea fork 关系反查，但现状 fork 是 DB 复制且有一批业务规则（不可 fork 自己的、archive 类不支持、fork 不自动订阅、三态按钮）。这些规则在 Gitea 原生 fork 下如何保持，V4 未涉及。
