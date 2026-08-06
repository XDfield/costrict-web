# 灰度迁移执行方案：DB-backed → Git-backed

> 适用范围：**skill / subagent / command / mcp** 四类。plugin 走另一条路（marketplace 投影 + mirror），见 §7。
> 本文是**执行方案**，策略决策的结论已直接写进 §1 / §4.1 / §6（**存量不自动迁移、按 owner 或 type
> 手工分批、退出条件三条全满足才下线 legacy 通道**）；终态定义见
> `CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md` §2.1。
> 上线序列见 `V4_PRODUCTION_ROLLOUT.md`；迁移中出问题见 `V4_TROUBLESHOOTING.md`；
> 日常操作（resync / 队列 / git_servers）见 `V4_OPERATIONS.md`。目录导航见 `README.md`。

## 0. 一句话

**两套共存，新内容一律走 Gitea，存量原地不动、按批手工迁移；任何时刻两套都必须各自自洽，不存在"迁移中"的半成品状态。**

---

## 1. 为什么是灰度而不是一次切换

存量规模决定的：本地实测 `capability_items` **14948** 行，其中 `content_backend='git'` **538** 行、
`db` **14410** 行（2026-08-06 复测。538 条 git 行里只有 **75** 条带 `forked_from_item_id`，
其余是 discovery / 迁移落下的 —— 换句话说 git 化率约 3.6%，远谈不上完成）。
一次性全切要求 Gitea 侧凭空出现一万多个仓库，且每个都要建仓、写文件、验证、翻转 —— 
中途任何一步失败都会留下"DB 说是 git、Gitea 上没有"的行，那种行**读不出内容**（read-through 会 502），
比不迁移更糟。

所以节奏由人控制，且**失败必须留在迁移前的状态**，而不是留在中间态。

---

## 2. 两套如何共存（判据只有一个字段）

`capability_items.content_backend`：

| 值 | 内容真相 | 读路径 | 写路径 |
|---|---|---|---|
| `db`（默认） | `capability_items.content` 列 | 直接读列 | legacy API / catalog ingest / publish |
| `git` | Gitea 仓库里的文件 | **read-through 实时拉**，DB 不持有 | **只有 git push**，legacy 路径一律拒绝 |

**这一个字段是全部分叉点**。四条 read-through 读路径 ——
详情（`GetItem`）/ `/items/{id}/download` / `/registry/{repo}/{itemType}/{slug}/*file` /
**`/items/{id}/assets`** —— 与代码里编号至少到 **W29** 的那批写入点（守卫测试里还引用了 W31 / W32）
都按它分流。

⚠️ **列表接口不在这四条里**：它对 git 行**故意不 read-through**（把 content 置空、零出站），
`?favorited=true` 也一样。所以「列表 200 而详情 502」是正确形态 —— 见 §8 与
`V4_TROUBLESHOOTING.md` §F3。

### 2.1 隔离是"默认拒绝 + 显式放行"，不是逐个加 if

GORM hook（`models/capability_item_git_guard.go`）对 git 行**默认拒绝**所有内容写入，
git sync worker 自己带放行标记。这样新写的代码不需要记得加守卫 —— 忘了就会被拒绝，而不是悄悄写坏数据。

⚠️ **hook 有三个已知盲区**，它们只能在 SQL 层解决，不要指望 hook：
`tx.Exec` 裸 SQL · `tx.Table()` · `UpdateColumn(s)`/`Session{SkipHooks}`。
这些调用点必须自带 `content_backend = 'db'` 谓词。

> `db.Save(&[]T{})` 传 slice 曾经是第四个（GORM 转成 `Create`+`ON CONFLICT UpdateAll`，
> `BeforeUpdate` 确实从不触发），**现已由 `BeforeCreate` → `guardGitOwnedCapabilityUpsert` 堵上**
> （`models/capability_item_git_guard.go` 的 `guardGitOwnedCapabilityUpsert`）。旧文档里「四个盲区」的说法已过时。

---

## 3. 新内容默认走 Git（已生效）

用户在 multica 新建 / fork 一个四类能力项时，`planGitBackedFork` 会尝试 Git 通道：
建仓 → 写 manifest → 写 ownership marker → 翻转 `content_backend='git'`。

**部署前提**（漏了会静默退化成 DB 通道）：

```bash
# 本地 go run：必须真实导出，只写进 server/.env 不生效
# config.Load() 把 .env 喂给 viper，而 viper 从不写 os.Environ，
# 但 loadBotTokenKey() 等处用的是裸 os.Getenv
export CS_BOT_TOKEN_KEY=<base64 32 字节>
```

缺它 ⇒ `aesForProvision == nil` ⇒ `InitUserSpaceService` 不执行 ⇒ `gitBackingWired()` 为 false
⇒ fork 走 `unavailable()` 静默回落 DB。

> **容器部署另说**：`server/docker-entrypoint.sh` 会 source + export `/app/.env`，
> 所以走 chart 的 `envFile.existingConfigMap` 注入同样有效。k8s `env:` 仍是推荐方式（可观测性更好）。

关于日志：缺 key 时 api **会**打一行
`teamns: CS_BOT_TOKEN_KEY not configured (...); team-namespace API disabled`
（`log.Printf` → stdout，`kubectl logs | grep CS_BOT_TOKEN_KEY` 抓得到，
但**不进** `server/logs/app.log`，且只在 `USER_SERVICE_BACKEND=rpc` 分支下打印）。
它是辅助判据，不是充分判据。

> 部署检查清单里应包含：**bootstrap 做完之后**，用一个**没 fork 过的 (用户, 源 item) 组合** fork 一次，
> 确认返回 `contentBackend: "git"`。
> 两个都不能省：租户没绑 git server 时结果同样是 `"db"`；而 fork 有「一人一源只能 fork 一次」的短路，
> 复用上一次失败留下的组合会永远拿到旧的 DB-backed 行。完整序列见 `V4_PRODUCTION_ROLLOUT.md` §3。

---

## 4. 存量迁移的执行流程

工具：`migrate capability-to-git`（S6 交付，`dfaaa19`）。**dry-run 是默认行为**，不是可选开关。

### 4.1 分批原则

按 **owner** 或 **type** 切批，一批别超过几十条。理由不是性能，是**可回溯**：
一批出问题时要能一眼看出影响面。

```bash
cd server
export CS_BOT_TOKEN_KEY='<base64 32 字节>'     # 必须真实导出，见 §3

# 1. 先看这批会动什么（默认 dry-run，零写入）
go run ./cmd/migrate capability-to-git --type=skill --owner=<short_id|subject_id>

# 2. 确认无误后执行
go run ./cmd/migrate capability-to-git --type=skill --owner=<short_id|subject_id> --confirm
```

⚠️ **两个容易写错的地方**：

- **执行开关是 `--confirm`，不是 `--apply`**（`--apply` 会被当成未知 flag 直接报错。
  用 `--apply` 的是另一个工具 `scripts/import-bundle-to-gitea.sh`，别混）。
- **flag 必须用 `=` 形式**（`--type=skill`），空格分隔同样报 `unknown flag`。

其余 flag（`cmd/migrate/capability_to_git.go:86 parseCapabilityToGitArgs`）：

| flag | 默认 | 说明 |
|---|---|---|
| `--ids=a,b,c` | — | 精确指定 |
| `--limit=N` | **50** | |
| `--tenant=<id>` | `default` | |
| `--include-catalog` | 关 | 默认**跳过** catalog 镜像行（真相在上游 GitHub，republish 会造出第二真相源） |
| `--clear-stale-content` | 关 | **另一件事**：清空「已经是 git-backed 但还残留旧 content 快照」的行 |

**必须至少给一个 `--type=` / `--owner=` / `--ids=`**，否则命令直接拒绝——
无范围的 plan 会打出上万条 catalog 镜像，且离误执行只差一个 flag。

### 4.2 每批必须验证的三件事

1. **仓库确实存在且内容一致**：`sha256(Gitea raw)` == 迁移前的 `content` 列
2. **DB 行已翻转且坐标完整**：`content_backend='git'`、`source_repo_url` 非空、
   `git_sha` 为 **40 位十六进制**、`git_sync_status='synced'`
3. **设备侧能拿到**：至少抽一条订阅它，跑 csc，比对落盘文件的 sha256

第 3 条不能省。前两条只证明服务端自洽，**证明不了 csc 能装**——历史上就是靠这一步发现
「csc 只比 version 字符串，改正文不 bump version 设备永远不更新」的。

### 4.3 失败处理

迁移**不得留下半 git 半 db 的中间态**（R7.3）。当前实现的顺序是：
建仓 → 写文件 → 验证 → **才**翻转 `content_backend`。
所以失败点在翻转之前 ⇒ DB 行仍是 `db`，内容还在列里，重跑即可（脚本幂等）。

⚠️ 失败可能留下**空仓库**。fork 路径已有回滚（`01c9248`），迁移路径需确认同样覆盖；
残留空仓库会让重试撞名字冲突。

---

## 5. 回滚

**没有自动回滚，也不该有。** 一旦 `content_backend='git'`，DB 的 `content` 列就不再被写入，
Git 成为唯一真相。回滚意味着把 Git 内容抄回 DB 列，那等于重新制造第二真相源。

真需要回滚时，正确做法是**从 Gitea 把内容取回来、手工写回列、再翻转标志**，
并明确接受"此后 Gitea 上的改动不再生效"。这是运维动作，不是产品功能。

---

## 6. 退出条件（灰度何时结束）

灰度期结束 = 可以下线 legacy 写入通道，判据：

| 条件 | 当前状态 |
|---|---|
| 存量四类全部迁完（`content_backend='db'` 且 type ∈ 四类 的行数 = 0） | ❌ 远未达成 |
| mirror 自动化落地 | ⏸ 手工脚本已有，自动化未做 |
| V3 通道稳定运行 ≥ 2 周 | ❌ 未开始计时 |

三条全满足后，才按 V4 §15.1 Stage 5 删除 `CatalogIngestService`。
**在此之前 catalog ingest 必须保留**，它仍是存量上游数据的唯一入口。

---

## 7. plugin 不走这条路

plugin 的内容从来不在 DB 列里（`.plugin.json` 是 manifest，真内容由 csc 直接 clone 仓库获得），
所以它没有"交出内容所有权"的问题。plugin 的 Git 化 = **把上游仓库镜像进自建 Gitea**，
让 fork 探测得到，见 `BUNDLE_TO_GITEA_IMPORT.md`。

⚠️ 大批量导入前**必须**确认 namespace 排除生效，否则 discovery 会把每个 mirror 仓库当成新能力项索引，
造出数千条重复行。
证据强度要说清楚：**实测**是 3 个 mirror 仓库各产生 **1 条**重复行；
8 / 15 / 28 是对 bundle 目录树里可分类 manifest 的**静态统计（推导，未逐个推上去验证）**。
结论方向（全量导入会造出数千条）成立，但别把 8/15/28 当实测值 ——
口径以 `BUNDLE_TO_GITEA_IMPORT.md` 的「本手册的验证状态」表为准。

排除集合 = **`PLUGIN_GIT_MIRROR_OWNER`（默认 `costrict-plugins-repo`，恒定包含）**
∪ `GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS`（逗号分隔，默认空）——
见 `internal/gitcapability/discovery_policy.go:24 DiscoveryOwnerExcluded`。

```
PLUGIN_GIT_MIRROR_OWNER=costrict-plugins-repo
GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS=costrict-plugins-repo   # 导进默认 namespace 时是冗余的
```

⇒ **导进默认 namespace 时第二行可以省**；**导进任何其它 namespace 时，那个 namespace 必须显式加进
`GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS`**，默认值救不了你。

排除在 **webhook ingress 与 worker 同步两层各执行一次**
（`handlers/git_capability_webhook.go:163` / `services/git_capability_sync_service.go` 里的同名调用），
所以漏配一层不会立刻出事，但两个变量都走裸 `os.Getenv`，**api 与 worker 都要配，且改完必须重启**。

---

## 8. 灰度期的已知语义差异（对用户可见，需要产品口径）

| 差异 | DB-backed | Git-backed |
|---|---|---|
| 编辑入口 | 站内编辑 | **跳转 Gitea**（U3 决策：Cloud 只展示不编辑）；站内写入返回 409 `GIT_BACKED_ITEM` |
| 版本号 | 站内版本行 | 取自 frontmatter；**改正文不改版本号**。对外投影成 `<version>+<git_sha[:7]>` |
| 历史版本 | `/versions` 可查 | `/versions` 返回空数组 + `versionBackend:"git"`，取单个版本 404 `GIT_VERSION_NOT_SERVED`；改用 `/git-history` |
| Gitea 不可用时 | 无影响 | 详情/下载/assets **fail-closed**，不回落旧值；列表仍 200（本就置空 content、零出站） |

Gitea 侧异常按原因分成五个错误码（`GIT_CONTENT_UNREACHABLE` / `_MISSING` / `_FORBIDDEN` /
`_COORDINATE_INVALID` 都是 502，`GIT_CONTENT_SERVER_UNAVAILABLE` 是 **503**）——
分诊表见 `V4_TROUBLESHOOTING.md` §F3。

**这些差异对同一个列表里的两种项同时存在**，且列表上不加标记（用户裁决）。
产品侧需要接受"用户点进去才知道是哪一种"。
