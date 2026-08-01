# Git 方案下的使用者旅程

> 系列文档之一，总览见 [`GIT_UX_JOURNEYS_OVERVIEW.md`](./GIT_UX_JOURNEYS_OVERVIEW.md)。
> 角色：订阅者 / 下发接收方。
> 判定图例：✅ 天然满足 · 🔧 补足后满足 · 📌 需决策

## 来源标注约定

每个功能点标注来源，避免把「本文认为该有的」误读成「V4 承诺的」：

| 标注 | 含义 |
|---|---|
| `[现状]` | 代码已实现（附 file:line），Git 方案下需保住 |
| `[V4 §x]` | V4 原文写了 |
| `[推导]` | V4 未写，但属该架构的必然要求 |
| **`[新增建议]`** | ⚠️ 非现状非 V4，本文提议的产品增强，可独立取舍 |

`[新增建议]` 类统一汇总在文末「可选增强」节，不进主线补足清单。

## 内容获取路线

**V4 §17 第 18 项已决定**：`HTTP 代理 + Gitea permission API 反查（A2 方案）`，并写明「csc 端零改造」。本文以此为默认前提走查；涉及多文件、本地修改合并等 A2 方案未覆盖的场景，在对应故事内说明缺口，但**不重开路线选择**——若要改用 clone 方案，须先修改 V4 §17 的决策条目。

> 注：V4 内部对 csc 是否走 git 存在表述不一致（§7.3.1 表格内两行写「用户 PAT（csc / AI agent 代用户）clone private repo / push main」，同表最后一行却写「csc 设备端（不走 git，走 HTTP 代理）」）。见总览 §5 V4-2。

## U1 订阅后自动出现在 CLI

> 作为订阅者，我希望收藏一个好技能后它自动出现在 CLI 中，以便立即使用而不必手工安装。

**Git 方案下的流程**:

1. **Gitea** 保存能力项内容和历史；push webhook 驱动索引刷新，使 marketplace 展示最新 metadata。
2. **server** 从 DB 发现索引返回能力项，并在用户订阅时写入 `ItemFavorite`，不复制内容。
3. **csc** 启动时及周期同步时查询云端收藏集合，识别尚未安装或尚未激活的能力项。
4. `[V4 §17 第 18 项]` csc 请求 REST 接口，server 从 Gitea 取 raw 内容（A2 方案）——按 commit SHA 锚定的不可变快照。
5. **csc** 将内容落入本地缓存并注册到技能运行目录，随后把生命周期记为 `active`。

**V4 已支持**: `[V4 §2.1]` Git 内容真相源 + DB 运行时索引 + 稳定可解析地址；`[V4 §3.3]` 用户 favorite 关系留在 DB；`[V4 §10.3]` webhook 索引同步；`[V4 附录 C.2.3]` 保留 csc 本地副本机制。

**现状**: `[现状]` 客户端取内容走 `GET /api/items/:id` 读响应的 `content` 字段（`favorite.ts:252-298`），落盘只写主内容与 `item.json`（`:382-426`）。

**需补足**:

| 补足项 | 来源 | 组件 |
|---|---|---|
| **详情接口 `content` 的取数契约**——V4 §14.1 是否废弃 `capability_items.Content` 口径不一（见总览 V4-1），而 C.2.1 只定义了列表 API 与 `/download` 的替代方案，未覆盖详情接口。这是整条链路的第一块多米诺 | `[推导]` | web |

**判定**: 🔧 —— 订阅关系本身 V4 不动；唯一缺口是内容读取契约。

## U2 作者发布后自动获得更新

> 作为订阅者，我希望作者发布新版本后自动获得更新，以便持续使用最新能力。

**Git 方案下的流程**:

1. **Gitea** 为每次发布生成唯一 commit SHA，无需依赖作者是否修改 frontmatter `version`。
2. **server** 收到 push webhook 后更新能力项索引中的 `git_sha`。
3. **csc** 比较远端 resolved SHA 与本地 applied SHA；二者不同即表示存在一个精确的新快照。
4. **csc / server** 按该 SHA 取内容（A2 方案下由 server 代取 Gitea raw）。
5. **csc** 完成落盘后再记录 applied SHA；能力项正在使用时，只在新副本准备完成后切换。

**V4 已支持**: `[V4 §14.1]` 新增 `git_sha`（顶层 metadata 文件所在 commit 的 SHA）；`[V4 §10.3/§10.4]` webhook 以 commit SHA 驱动同步与幂等；`[V4 §12]` 扫描短路键迁 `git_sha`。

> 注：V4 §14.1 只新增了服务端字段，附录 C.2.3 同时写着 `favorite.ts` **零改造**。因此「客户端如何感知新版本」在 V4 中并未定义——下方第一项是架构必然要求（`CurrentRevision` 被废弃后必须有替代信号），但具体的客户端状态机设计不在 V4 范围。

**现状**:

- `[现状缺陷]` 更新判定比 frontmatter `version` 字符串（`favorite.ts:1117`），作者不 bump 就永远认为是最新
- `[现状缺陷]` 即使判定出有更新也会失败——`updateFavoriteItem` 收到的是 slug（`sync.ts:97`），而服务端详情接口只按 UUID 查（`capability_item.go:1165`），必 404

**需补足**:

| 补足项 | 来源 | 组件 |
|---|---|---|
| `git_sha` 透出到列表/详情 API，客户端更新判定改比 sha | `[推导]` | web / csc |
| 修复更新链路参数 bug（始终传 item UUID） | `[现状缺陷]` | csc |

**判定**: 🔧 —— `git_sha` 让「内容变了没有」第一次可精确判定，这是现状（靠作者手写版本号）结构上做不到的。

> 客户端保存 applied SHA、准备完成后原子切换等状态机设计属实现方案，见「可选增强」E-9。

## U3 本地修改在更新时得到保留

> 作为会定制技能的订阅者，我希望更新时保留自己的本地修改，以便持续使用个性化版本。

**Git 方案下的流程**:

1. **csc** 以已安装 commit SHA 作为共同基线，识别远端变化和本地变化。
2. **Gitea / csc** 在 clone 路线下天然提供工作区变更识别、共同祖先和三方合并。
3. **csc** 若能自动合并，则在新工作副本中完成合并并保留本地提交或工作区修改。
4. **csc** 遇到需要用户选择的并行修改时，继续运行旧副本，并把新版本置为“待处理”。
5. **server** 只管理订阅与源版本，不把本地定制误报为云端正式版本。

**V4 已支持**: 无。

> ⚠️ 上一版以 §9.3「diff 即审查证据、git revert 即回滚、fork 即实验」作为依据，属**引用错误**——§9.3 标题是「AI 友好性的来源」，描述的是 AI agent 在**创作端**的 Git 体验，不涉及 csc 设备端本地合并。§7.3.1 反而明写「csc 设备端（不走 git，走 HTTP 代理）」。

**现状**：

- `[现状]` 更新是「备份后整体覆盖」——先 `copyDir` 到 `.backup/<ISO时间戳>/`，再覆盖（`favorite.ts:1125-1143`）。用户本地修改会被静默丢弃，有备份但无提示、无合并、无冲突检测
- `[现状缺陷]` 备份目录写在源目录自身的 `.backup/` 下，而 `copyDir` 不排除它（`favorite.ts:575-586`），存在自我递归复制

**需补足**:

| 补足项 | 来源 | 组件 |
|---|---|---|
| 修复备份目录自我递归 | `[现状缺陷]` | csc |
| 覆盖前提示用户「你的本地修改将被备份到 X」，而非静默进行 | `[推导]` | csc |

**判定**: 🔧 —— 在 A2 方案（HTTP 代理）下，三方合并不是 V4 的能力范围。真正的合并/冲突保留见「可选增强」E-3。

## U4 停用保持稳定，重新下发后恢复

> 作为订阅者，我希望停用的技能保持停用，但管理员重新下发时能够恢复，以便个人选择和管理要求都被尊重。

**Git 方案下的流程**:

1. **csc** 停用能力项时移除运行副本，并把本地生命周期记为 `unloaded`。
2. **Gitea** 后续内容提交只改变内容版本，不改变用户的本地生命周期，因此不会自行激活。
3. **server** 新建一次管理员下发时产生更晚的 `distribution.createdAt`。
4. **csc** 将该时间与 `lastAppliedDistributionAt` 比较；只有未应用过的新下发才可穿透 `unloaded` 并重新激活。
5. **csc** 激活成功后推进水位线，普通内容更新仍然保持停用状态。

**V4 已支持**: `[V4 §3.3]` favorite 留在 DB、内容版本留在 Git，使内容变化与用户状态天然分离；`[V4 附录 C.2.1]` 保留下发业务语义。

**现状**: `[现状]` 该行为**已完整实现**——`planFavoriteEnable` 的 watermark 机制（`favorite.ts:1040-1105`）：未激活项自动启用；被 unload 的项仅在收到比 `lastAppliedDistributionAt` 更新的下发时才穿透复活；纯内容更新不复活。

**需补足**:

| 补足项 | 来源 | 组件 |
|---|---|---|
| `redeliver_at`：现状 pause→active 复用同一条 distribution，`createdAt` 不变，客户端 watermark 不感知，管理员「恢复」对客户端不生效 | `[现状缺陷]` | db / web / csc |

**判定**: ✅ 主路径天然满足（Git 迁移不影响）；恢复场景有一处现状缺陷。

## U5 取消订阅后自动清理

> 作为订阅者，我希望取消订阅后本地内容自动清理干净，以便不留下不再使用的文件和凭据。

**Git 方案下的流程**:

1. **server** 移除用户的自主订阅来源，并计算该能力项是否仍有有效下发来源。
2. **csc** 从权威收藏集合中发现已无任何使用权来源的 item。
3. **csc** 停用运行副本，并删除 `_localPath` 缓存、备份、状态记录和临时文件。
4. **csc / Gitea** 在 clone 路线下同时删除本地仓库并回收该仓库专用凭据；共享凭据只在无其他引用时回收。
5. **server** 保留 Git 仓库和能力项历史，取消订阅只影响该用户的本地使用状态。

**V4 已支持**: `[V4 §3.3]` 用户 favorite 关系留在 DB；`[V4 附录 C.2.1]` 保留 favorite 接口语义——即这条链路 V4 不改动。

**现状**：

- `[现状]` 云端取消收藏 → 客户端 orphan 检测（fail-closed）→ 批量 unload（`favorite.ts:1157`、`sync.ts:109`）
- `[现状缺陷]` `removeItemFromConfig` 只清运行副本，第三个参数 `_localPath` 未使用（`favorite.ts:675-720`），`~/.costrict/favorites/` 下的缓存副本残留
- `[现状缺陷]` 同一 item 若既被自己收藏又被下发，共用一行 `ItemFavorite`（`(item_id,user_id)` 唯一索引），撤销下发会连带删掉用户自己的订阅（`behavior_service.go:237-269,307-345`）

**需补足**:

| 补足项 | 来源 | 组件 |
|---|---|---|
| 卸载时一并清理缓存副本 | `[现状缺陷]` | csc |
| 收藏来源可区分，避免撤销下发误删自主订阅 | `[现状缺陷]` | db / web |

**判定**: 🔧 —— 两项均为与 Git 迁移无关的现状缺陷修复。

> 完整的 entitlement 多来源账本（把每条下发记为可分别撤销的来源）是更彻底的重构方案，见「可选增强」E-1；安全擦除级 purge（含凭据引用计数）见 E-4。

## U6 下发自动激活，收回后自动卸载

> 作为下发接收方，我希望收到下发后自动激活，并在下发被收回时自动卸载，以便设备状态跟随管理策略收敛。

**Git 方案下的流程**:

1. **server** 创建 distribution 和 receipt，并为接收方增加对应 entitlement。
2. **server** 在内容层校验接收方是否有权读取该版本——这正是总览 P-1 尚未解决的部分。
3. **csc** 收到通知或在下一轮同步中看到新增能力项，获取指定 SHA 并自动激活。
4. **server** 收回下发时撤销该 entitlement；若没有自主订阅或其他有效下发，则不再把 item 返回给接收方。
5. **csc** 识别 orphan 后同时清除运行副本与 `_localPath` 缓存，并向 server 回报最终状态。

**V4 已支持**: `[V4 附录 C.2.1]` 保留 `POST /items/:id/distribute` 语义及不可变用户 ID 关联。

> ⚠️ 上一版以 §4.7.1、§7.2 作为「下发授权」依据，属**引用错误**——§4.7 是「纯 Gitea visibility 透传」；§7.2.2 的 admin PAT 只有两个允许场景（capability 索引同步、用户生命周期级联），**均不含下发授权**。

**现状**：

- `[现状]` 下发 → 建 receipt → 自动收藏 → 客户端下轮 sync 激活（`distribution_service.go:404-513`）
- `[现状]` 收回 → 删 favorite + receipt 置 dismissed → 客户端 orphan 卸载（`:684-794`）
- `[现状缺陷]` **私有仓库的能力项下发后接收方拿不到内容**——内容层查 `RepoMember`（`registry.go:308`），下发链路不写 `RepoMember`，接收方 403；但管理端显示下发成功

**Git 方案下的关键缺口**：

`[推导]` A2 方案下内容访问改查 Gitea collaborator（§4.7.1），而下发接收方同样不是 collaborator——**现状这个缺口在 V4 下原样存在**，且 V4 未定义解法。这是总览 P-1，需 V4 作者定夺。

**需补足**:

| 补足项 | 来源 | 组件 |
|---|---|---|
| 📌 私有能力项下发的内容授权（见总览 P-1，任何解法都需修改 V4 §4.7 或 §7.2.2 的既有决策） | `[推导]` | web / gitea |
| 卸载时清理缓存副本（同 U5） | `[现状缺陷]` | csc |

**判定**: 📌 —— 私有能力项下发能否成立，取决于 P-1 的决策。

> 生效回执（客户端上报实际 SHA）见「可选增强」E-2。

## U7 弱网、离线和多设备使用

> 作为经常移动办公的订阅者，我希望在弱网、离线和多设备环境中继续使用技能，以便网络状况不阻断工作。

**Git 方案下的流程**:

1. **csc** 始终从已校验的本地副本启动；云端暂不可达时继续使用 last-known-good SHA。
2. **server / Gitea** 通过 webhook 重投、SHA 幂等和定时巡检保持云端索引最终收敛。
3. **csc** 恢复联网后重新对账，重试获取指定 SHA 的不可变快照（A2 方案下每次为全量取数，无增量能力）。
4. **csc** 在任一类型列表未完整取得时保持本地能力，不据此执行清理。
5. **csc** 在每台设备独立记录 applied SHA；各设备按自身上线时间收敛到同一目标版本。

**V4 已支持**: `[V4 §10.4]` SHA 去重与 webhook 重试；`[V4 §10.7]` 超时、退避、限流——这些保障的是**服务端索引**的最终收敛，不直接作用于客户端。

**现状**：

- `[现状]` 客户端始终从本地副本启动，云端不可达不影响使用
- `[现状]` 孤儿检测 fail-closed：任一类型列表未完整取得就跳过整轮清理（`favorite.ts:790`、`sync.ts:109-125`），弱网下不会误删
- `[现状]` 轮询天然幂等，多设备各自收敛
- `[现状缺陷]` `state.json` 是无锁非原子 read-modify-write（`favorite.ts:136-153`），解析失败当作空状态；`startCloudFavoritesSync` 的定时器无 in-flight guard（`sync.ts:146-165`）；`/hub` 手动操作用 `Promise.allSettled` 并行——多进程 / 多 worktree / 轮询与手动重叠时会互相覆盖

**需补足**:

| 补足项 | 来源 | 组件 |
|---|---|---|
| `state.json` 跨进程文件锁 + 原子写 + 同步防重入 | `[现状缺陷]` | csc |

**判定**: 🔧 —— 弱网/离线/多设备现状表现已可接受；真正的风险是本地状态并发，与 Git 迁移无关。

> 按设备记录目标/实际 SHA 的对账账本见「可选增强」E-2。

## U8 使用 plugin、多文件技能和二进制资产

> 作为订阅者，我希望 plugin 或包含多文件、二进制资产的技能也能完整安装，以便获得与单文件技能一致的体验。

**Git 方案下的流程**:

1. **Gitea** 将文本文件保存在 Git 中，小型二进制直接入库，大型二进制通过 LFS 或 Release attachment 保存。
2. **server** 以顶层 metadata 建立发现索引，并用 commit SHA 标识可重现的仓库快照。
3. **csc / server / Gitea** 获取能力项根目录下的完整文件树，而不只读取顶层 metadata。
4. **csc** 校验路径、大小和哈希后原子展开文件树；plugin 的磁盘同步可在运行中进行，运行态切换等待 REPL 空闲。
5. **csc** 更新或卸载时以整棵能力项文件树为单位处理，不遗留旧资产。

**V4 已支持**: `[V4 §5.4]` 规定 Git / LFS / Release 的资源分工；`[V4 §4.1/§5.1]` 把 standalone repo 或 pack 子目录定义为能力项边界。

> ⚠️ 上一版称 §10.2 的 recursive trees 支持完整子树读取，属**引用过度**——§10.2 原文把该 API 限定为「一次性拉整棵文件树（**mirror 初始同步**）」；§10.3 更明确写「子目录变更（plugin 内 skill/command 文件、assets 等）**直接忽略**」。**V4 的同步粒度是顶层 metadata，分发侧的子树读取路径尚未定义。**

**现状**（上一版把这些误列为缺口，实际早已实现）：

- `[现状]` plugin 的自动安装 / 卸载 / 失败重试已由 favorite 驱动实现：`csc/src/costrict/favorite/reconcileCloudPlugins.ts:320-487`（含 ledger 去重、removal pass、尊重用户手动安装）
- `[现状]` plugin 自动更新已实现：`csc/src/utils/plugins/pluginAutoupdate.ts:161` `updatePluginsForMarketplaces` / `:227` 后台自动更新
- `[现状缺陷]` 但**技能侧**的多文件资产客户端未接通——`getRemoteItem` 只读 `content` 字段，不消费 `/items/:id/assets`（`favorite.ts:252-298,382-426`），PR #185 的完整文件树分发在客户端侧尚无入口

> V4 附录 C.2.3 称 csc plugin 仅需「marketplace 安装跳转」，该判断基于 2026-07-06 基线，已过时。

**需补足**:

| 补足项 | 来源 | 组件 |
|---|---|---|
| **分发侧子树读取路径**：§10.3 忽略子目录、§10.2 的 trees 限于 mirror 初始化，A2 代理方案下需定义如何取整棵能力项文件树 | `[推导]` | web |
| 客户端接通多文件资产（消费 `/assets` 或新的树接口） | `[现状缺陷]` | csc |
| 三套二进制载体收敛：DB / Git LFS+Release（§5.4）/ S3（PR #185） | `[推导]` | web |

**判定**: 🔧 —— plugin 侧现状已完善；技能侧多文件是真缺口，且 V4 的同步粒度设计与分发需求不匹配。

## U9 两个能力项同名时可辨认、可选择

> 作为订阅者，我希望两个同名技能都能被清楚辨认，并由我决定启用哪一个，以便不会误用其他来源的内容。

**Git 方案下的流程**:

1. **Gitea** 通过 `owner/repo@ref:path` 天然区分不同来源，即使 frontmatter slug 相同。
2. **server** 以 item UUID 和 source repo/path 返回两个独立市场条目，并展示作者、owner 和官方标识。
3. **csc** 以 item UUID或规范化 source coordinate 管理缓存，不再仅以 slug 作为状态键。
4. **csc** 在运行时名称相同且该运行时不支持别名时，请用户选择优先项；支持别名时按用户配置注册。
5. **csc** 卸载时依据所有权标记只删除目标 item 的文件。

**V4 已支持**: `[V4 §4.5]` 唯一可解析地址；`[V4 §14.1]` `source_repo_url + source_repo_path` 唯一对应一个能力项；`[V4 §4.2/§4.3]` 官方 / pack / mirror / 用户 namespace 分区——**来源辨识度在 V4 下天然变好**。

**现状**：

- `[现状缺陷]` 服务端唯一键是 `(repo_id, item_type, slug)`（`models.go:445-450`），同 slug 跨仓库合法；但客户端跨所有类型只用一个 slug `Set` 去重，并以 slug 作 state key（`favorite.ts:811-820,428-445`）——两个同名能力项会静默互斥
- `[现状缺陷]` slug 无格式校验，同时是本地目录名与（V4 下的）Gitea 路径名

**需补足**:

| 补足项 | 来源 | 组件 |
|---|---|---|
| 客户端身份键从 slug 迁到 UUID / source coordinate（含存量状态迁移） | `[推导]` | csc |
| slug 格式白名单 | `[推导]` | web / gitea |

**判定**: 🔧 —— 身份键对齐是 V4 唯一坐标模型的必然要求。

> 同名时的别名机制、显式优先级、运行时选择 UI 属新增产品规则，见「可选增强」E-5。

## U10 切换账号和项目技能集

> 作为多账号、多项目用户，我希望切换账号或项目时使用对应的技能集，以便不同身份和工作区保持清晰边界。

**Git 方案下的流程**:

1. **server** 依据不可变 user ID 返回当前账号的 favorite、distribution 和访问权限。
2. **csc** 将账号身份解析为 issuer、tenant、user ID 组成的本地 profile，并只激活当前 profile 的能力项。
3. **Gitea** 在 clone 路线下使用当前账号自己的 PAT或JWT；切换账号时不复用上一账号的私有仓库凭据。
4. **csc** 进入项目时读取项目能力集及 pinned SHA，在用户级集合之上计算当前项目的有效集合。
5. **csc** 切换账号或项目时先完成差异对账，再原子切换运行层；缓存可按策略保留，但不会跨边界自动激活。

**V4 已支持**: `[V4 §14.4]` 新增 `user_gitea_binding`（用户与 Gitea 账号绑定）。

> ⚠️ 上一版把 §6.3 / §6.4 / §14.4 整体列为本旅程的 V4 支持，属**引用过度**——这些条款定义的是**跨服务身份绑定**，不涉及客户端本地多 profile、项目能力集或切换事务。

**现状**：

- `[现状缺陷]` 本地状态是单一全局文件、按 slug 存储（`favorite.ts:53-55,136-153`），不记账号或租户；同一配置目录切换账号后，前账号的项会被权威对账判为 orphan
- `[现状]` 只有用户级全局作用域——收藏缓存、状态、active skills 全部基于 `getClaudeConfigHomeDir()`（`favorite.ts:103-133`），无项目/workspace 隔离

**需补足**:

| 补足项 | 来源 | 组件 |
|---|---|---|
| 本地状态按账号分区，避免切换账号时误判 orphan | `[现状缺陷]` | csc |

**判定**: 🔧 —— 账号切换是现状缺陷；项目级能力集属新增产品能力，见「可选增强」E-6。

---

## 知识中心（Store 页面）体验

> 以上 U1–U10 走的是 CLI 侧旅程；使用者的另一半旅程在 Web 知识中心（Store）。以下按现状信息架构走查 Git 方案下的页面体验。

### U11 我在知识中心浏览、筛选、发现技能

> 作为使用者，我希望在 Store 首页按分类、标签、类型、安全状态筛选并搜索，以便快速找到合适的能力项。

**现状信息架构**（`store/pages/home.tsx`）：筛选条（StoreFilterBar：分类/标签/类型/安全状态/排序）+ 卡片/列表双视图 + 侧边导航 + 最佳实践轮播（best-practice-carousel）+ 频道区块（channels-section，对接 csc plugin marketplace 渠道）。数据来自 `/api/items`（体验分排序、收藏数 tiebreak）、`/api/categories`、`/api/tags`、`/api/items/filter-options`。

**Git 方案下的流程**：

1. **server** 发现层保持纯 DB 索引（§4.7.1 明确发现层零 Gitea 调用）——列表、筛选、搜索、排序体验完全不变。
2. **Gitea / server** webhook 把 push 实时同步进索引（§10.3），列表新鲜度从"定时 ingest"提升为"push 即可见"。
3. **portal** private 项带锁形标记出现在列表（§4.7.1 全量返回含 `visibility` + `owner`），点开走权限校验。

**V4 已支持**：§4.7.1 发现层/权限层分离；§10.3 webhook 同步。

**需补足**：

- （portal）private 标记与"申请权限"入口的 UI（§4.7.1 已定义交互约定，需实现）。

**判定**：✅ —— 浏览体验不变，新鲜度变好。

### U12 我打开详情页，看内容、版本历史与健康状态

> 作为使用者，我希望订阅前看到技能的完整内容、历史版本、扫描结论和健康状态，以便判断值不值得用。

**现状信息架构**（`store/pages/detail.tsx` + item-detail-content）：内容渲染（SKILL.md）、plugin 子项树（sub-item-tree）、健康雷达（health-radar）、安全标签（security-tag）、版本历史（`/items/:id/versions`，读 `CapabilityVersion` 快照表）、相似推荐（`/items/:id/similar`）、统计（`/items/:id/stats`）、扫描状态与结果、订阅按钮、下发入口（distribute-dialog）。

**Git 方案下的流程**：

1. **server / Gitea** 内容预览按 `source_repo_url+path@git_sha` 从 Gitea raw 取并缓存渲染（与 U1 的读路径契约同源）。
2. **server** 版本历史从快照表切到 Gitea commits API（§10.2 `commits?path=`）——历史第一次带上真实作者、提交信息，"版本对比"用 compare API 免费获得 diff。
3. **portal** 健康雷达直接消费 §11 的四态与 issues 明细，`introduced_commit` 可点击跳转。
4. **server** 扫描结论按 `git_sha` 对齐到具体版本（§12）。
5. plugin 子项树受 📌 粒度决策影响：若跟随 V4 整包模式，子项树改由前端解析包内结构而非 DB 行。

**V4 已支持**：`[V4 §10.2]` 逐字原文「列出文件历史 commit（**替代 CapabilityVersion**）| `GET .../commits?path={filepath}`」——版本历史切 commits API 是 V4 明确的落地项；`[V4 §11]` 健康度四态；`[V4 §12]` 扫描短路键迁 `git_sha`。

**现状**：`[现状]` 详情页版本历史读 `CapabilityVersion` 快照表（`capability_item.go:1916-1939`）。

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| 版本历史接口切 commits API（含分页） | `[V4 §10.2]` | web |
| 详情内容预览接入新读路径（依赖 G-01 的契约） | `[推导]` | web / portal |
| 健康雷达字段与 §11 的 health 结构对齐 | `[推导]` | portal |

**判定**：🔧 —— commits API 切换后历史带上真实作者与提交信息，是快照表给不了的。

> 版本对比 UI（调 compare API 做 diff 页）属产品增强——§10.2 中 compare 的用途是「拿两次 commit 之间的文件清单」，服务于 sync worker，无产品对比页设计。见「可选增强」E-7。

### U13 我认得出技能来自哪个大客户或官方

> 作为使用者，我希望一眼看出技能的来源背书（官方认证 / 大客户贡献），以便建立信任。

**现状机制**：平台管理员维护"大客户品牌配置"（`enterprise_customers` 表：名称 + logo + 账号 universal_id 列表，`models.go:101-118`）。公开端点 `GET /api/enterprise-customers` 把 universal_id 解析成 subject_id 下发；前端 `matchEnterprise(item.createdBy)` 命中后在卡片/详情渲染大客户品牌 logo（`store/lib/enterprise.ts`）。

**Git 方案下的流程**：

1. **server** 大客户配置是 DB 业务数据，V4 不改动，配置后台照常。
2. `[V4 §4.7]` 官方背书由 `costrict/` org 表达（原文：「`costrict/` org 仅是官方印章」），比纯前端标识更结构化——但这仅覆盖**官方认证**，不覆盖大客户品牌。
3. `[推导]` **衔接点**：命中依赖 `item.createdBy`；Git 路径下内容经 push 产生，`createdBy` 需从 commit author email（§14.1 `git_author_email`）映射回平台账号——映射断裂则大客户标识失效。

**V4 已支持**：`[V4 §14.1]` 新增 `git_author_email` 字段。

> ⚠️ 上一版提出"大客户命中升级为按 Gitea org 命中"并称其为 Git 方案的增益，属**引用过度**——§8 规定的是 4 个固定 org（`costrict` / `costrict-plugins` / `costrict-mirror` / `costrict-config`），**V4 没有企业客户 org/namespace 模型**。

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| webhook 同步时 author email → 平台账号的稳定映射（含 email 变更、多 email），保证 `createdBy` 语义在 Git 路径下延续 | `[推导]` | web |

**判定**：🔧 —— 大客户配置本身 V4 不改动，唯一衔接点是 `createdBy` 的来源变化。

> 按企业 org 命中需先引入企业 org 模型，属独立提案，见「可选增强」E-8。

### U14 我不想学 git，也能在网页上创建和维护能力项

> 作为使用者/轻创作者，我希望继续用 Store 页面的表单与上传（创建能力项、上传 plugin zip、配置外部仓库同步），以便不接触 git 也能贡献。

**现状信息架构**：create-capability-dialog（表单直建）、upload-plugin-dialog（zip 上传）、create-repo-dialog / repo-sync-tab（外部 repo 接入，同步状态读 sync-jobs / sync-logs）、invite-dialog（repo 成员管理）。

**Git 方案下的流程**：

1. `[V4 §10.6]` 外部仓库接入从"server 定时拉取"换成 Gitea mirror pull + webhook（方式 A），同步状态 UI 从 sync-jobs 改读 webhook/registry 状态。
2. `[推导]` repo 成员邀请与 Gitea collaborator 的关系需明确（见总览 G-15）。
3. 📌 **表单直建与 zip 上传的落点 V4 未定义**——见下。

**V4 已支持**：`[V4 §10.6]` mirror 接入。

> ⚠️ 上一版称「§9 的 git 直推覆盖 AI/高级用户，表单用户由代提交覆盖」，属**引用错误**——§9 只描述用户/AI 直接 clone、commit、push；**代提交不存在于 V4**。更关键的是 §3.2 明确规定「**禁止方向：DB → Git**」，而表单直建的天然实现就是 server 把表单内容写进 Git，与该约束正面冲突。

**需补足**：

| 补足项 | 来源 | 组件 |
|---|---|---|
| 📌 **网页表单/上传用户在 V4 下如何创作**（见总览 P-2）——现有入口是真实功能（`create-capability-dialog` / `upload-plugin-dialog`），而 §3.2「禁止方向：DB → Git」与 §9 只有直推路径，两者未覆盖这类用户。任何解法都需修改 V4 决策 | `[推导]` | web |
| 同步管理 UI 从 sync-jobs / sync-logs 切换为 webhook 驱动的状态视图 | `[推导]` | web / portal |

**判定**：📌 —— 表单创作路径是现存功能，V4 未定义其落点，需 V4 作者定夺。

## 主线补足项清单

仅含 `[现状缺陷]` 与 `[推导]`。需求扩张项见下节「可选增强」。

| 补足项 | 来源 | 组件 | 故事 |
|---|---|---|---|
| 详情接口 `content` 取数契约（依赖总览 V4-1 澄清） | `[推导]` | web | U1 |
| `git_sha` 透出 + 客户端更新判定改比 sha | `[推导]` | web / csc | U2 |
| 修复更新链路参数 bug（传 slug 但服务端只认 UUID，必 404） | `[现状缺陷]` | csc | U2 |
| 修复备份目录自我递归；覆盖前提示本地修改将被备份 | `[现状缺陷]` | csc | U3 |
| 卸载时清理缓存副本（`_localPath` 未使用） | `[现状缺陷]` | csc | U5 U6 |
| 收藏来源可区分，避免撤销下发误删自主订阅 | `[现状缺陷]` | db / web | U5 |
| 📌 私有能力项下发的内容授权（总览 P-1） | `[推导]` | web / gitea | U6 |
| `state.json` 跨进程锁 + 原子写 + 同步防重入 | `[现状缺陷]` | csc | U7 |
| 分发侧子树读取路径（§10.3 忽略子目录、§10.2 trees 限于 mirror 初始化） | `[推导]` | web | U8 |
| 客户端接通多文件资产 | `[现状缺陷]` | csc | U8 |
| 三套二进制载体收敛（DB / LFS+Release / S3） | `[推导]` | web | U8 |
| 客户端身份键 slug → UUID / source coordinate | `[推导]` | csc | U9 |
| slug 格式白名单 | `[推导]` | web / gitea | U9 |
| 本地状态按账号分区 | `[现状缺陷]` | csc | U10 |
| Store private 标记与申请权限入口 | `[推导]` | portal | U11 |
| 版本历史切 commits API | `[V4 §10.2]` | web | U12 |
| 详情预览接新读路径；健康雷达对齐 §11 | `[推导]` | web / portal | U12 |
| author email → 平台账号稳定映射 | `[推导]` | web | U13 |
| 📌 网页表单创作在 V4 下的落点（总览 P-2） | `[推导]` | web | U14 |
| 同步管理 UI 切 webhook 视图 | `[推导]` | web / portal | U14 |

---

## 可选增强（`[新增建议]`，非 V4 范围）

以下**既非现状能力也非 V4 内容**，是本文基于审计发现提出的产品增强。与 Git 迁移**无依赖关系**，可独立立项或直接否掉。评审 Git 方案时可跳过。

| # | 增强 | 动机 | 与 V4 的关系 | 组件 |
|---|---|---|---|---|
| **E-1** | entitlement 多来源账本：自主订阅与每条下发记为可分别撤销的来源，最后一个消失才移出收藏集合 | 撤销下发会误删用户自己的订阅 | V4 §3.3 只写「用户 favorite 关系 \| DB」，无 entitlement 模型。属现状一致性重构 | db / web / csc |
| **E-2** | 端到端生效对账：客户端上报 `installed_sha`，按设备记录目标/实际版本 | 管理端「下发成功」与用户实际能用之间无反馈 | V4 无回执概念（全文 grep 零命中）；sha 锚定使其可精确实现 | db / web / csc |
| **E-3** | 本地修改三方合并 / 冲突待处理状态 | 更新覆盖用户定制 | §17 第 18 项已定 A2 HTTP 代理、csc 零改造；三方合并需 clone 路线，属推翻既有决策 | csc |
| **E-4** | 安全擦除级 purge（含 `.backup`、clone 目录、凭据引用计数） | 卸载后仍有内容残留 | V4 附录 C.2.3 只说「本地副本机制保留」 | csc |
| **E-5** | 同名能力项的别名机制 / 显式优先级 / 运行时选择 UI | 同名静默互斥 | §4.5/§14.1 只定义唯一坐标，无运行时别名策略 | web / csc |
| **E-6** | 项目级能力集（`.costrict/capabilities.lock` 记 UUID+SHA） | 不同项目想用不同技能集 | V4 §14.4 只定义跨服务身份绑定，无项目 lock 概念 | web / csc / db |
| **E-7** | 版本对比 UI（compare API 做 diff 页） | 详情页想看两版差异 | §10.2 中 compare 用途是「拿两次 commit 之间的文件清单」，服务于 sync worker | web / portal |
| **E-8** | 大客户按 Gitea org/namespace 命中 | 比维护账号列表稳 | §8 只规定 4 个固定 org，无企业 org 模型 | web / portal |
| **E-9** | 客户端 SHA 状态机：保存 applied SHA、暂存目录、准备完成后原子切换 | 更新过程非原子，失败会留下半新半旧 | V4 附录 C.2.3 明确 `favorite.ts` 零改造，客户端状态机不在其范围 | csc |

> 建议取舍：E-1 / E-2 解决真实痛点且不与 V4 冲突，优先级最高；E-3 需先推翻 §17 第 18 项，代价大；其余锦上添花。
