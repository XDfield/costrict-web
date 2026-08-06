# Bundle → Gitea 导入手册（plugin mirror 补齐）

> **脚本**：`scripts/import-bundle-to-gitea.sh`
> **做什么**：把 costrict marketplace bundle 里的 plugin 仓库导入自建 Gitea，让 Cloud 上的 plugin fork 走 Git 通道而不是回落到旧的 DB 编辑页。
> **读者**：执行导入的运维/研发。生产执行需单独批准（见 §10）。
> **上下文**：V4 整体上线序列见 `V4_PRODUCTION_ROLLOUT.md`；导入后出问题（尤其是「冒出一堆重复行」）
> 见 `V4_TROUBLESHOOTING.md` §F11；目录导航见 `README.md`。

### 本手册的验证状态（2026-08-04）

| 内容 | 状态 |
|---|---|
| §4 的文件名 / frontmatter 口径 | **代码核对**：`classifyGitCapabilityManifest`、`pluginManifestPaths`、`probeRepoManifest`、`buildMarkdownSkeleton` 逐条比对 |
| §4.4 的 309/95/297 计数 | **实测**：对本地 v0.1.0 bundle 全部 701 个 bare repo 跑 plan |
| §5 脚本行为（plan/apply/断点续跑/幂等/单仓失败不阻断/空壳清理/lineage 拒绝覆盖/索引过滤） | **实测**：本地 Gitea `:3001`，4 个仓库的小样本（含 1 个故意损坏的 bare repo） |
| §6 障碍 1–6 | **实测**：http scheme、admin 建仓路径、bash 3.2、`repos/search?owner=` 失效、空壳 409、`is_empty` 异步（~1.8s） |
| §6 障碍 7–8（nginx 413 / 大仓 504） | **未复现**：沿用既有运维记录，脚本侧的速度守卫来自 `import.sh` 已验证的配置 |
| §7 discovery 副作用 | **实测**：3 个 mirror 各产生 1 条重复 plugin 行；28/15/8 的可分类 manifest 数为 bundle 树静态统计（**推导**，未逐个推上去验证） |
| §8.3 fork 走 Git 通道的端到端 | **未执行**：归 S7 验收（AC15） |
| §10 生产 | **未执行**（D6：只出手册） |

---

## 1. 这件事在解决什么

Cloud 的 plugin fork 会按顺序探测三个候选仓库坐标，主力是候选 2：

```
<PLUGIN_GIT_MIRROR_OWNER>/<item.slug>        默认 costrict-plugins-repo/<slug>
```

判定实现在 `server/internal/handlers/capability_item_fork_git.go`
（`pluginGitMirrorOwner()` `:53`、候选构造 `:194-199`、`locateGiteaSourceRepo`、`probeRepoManifest` `:828`）。

命名不需要任何转换：**bundle 里 bare repo 的 basename == catalog `index.json` 的 `id` == DB 的 `slug`**，三者恒等（实测 1395 条 catalog id 无一含大写/下划线/空格，`slugifyKey` 是恒等变换）。

所以 fork 落不到 Git 通道，唯一原因是**目标 Gitea 里没有那个仓库**。本手册就是补这批仓库。

**必须先理解的一条产品语义**：探测到「仓库存在但装的不是这个 plugin」时，**不会**静默回落 DB，而是 **HTTP 409 `GIT_SOURCE_MANIFEST_INVALID`**。也就是说——

| 目标仓库状态 | 用户看到的结果 |
|---|---|
| 不存在 | 静默回落旧编辑页（**能用**） |
| 存在且 manifest `name` 匹配 | 走 Git 通道（**目标状态**） |
| 存在但 manifest `name` 是别的 plugin | **409，fork 直接失败** |
| 存在但**读不到 manifest**（空仓库 / 只有 README） | **409，fork 直接失败**（比上一条更早触发，不会继续尝试下一个候选） |

**无脑把 bundle 全推进去 = 制造第 3、4 类**。实测 v0.1.0 bundle 与 DB 有交集的 404 个仓库里，95 个（23.5%）装的是错的 plugin（monorepo 塌回仓库根）。脚本的过滤闸门就是为这条存在的，**不要用 `--allow-*` 之类的开关绕过它**。

---

## 2. 前置条件

| 项 | 要求 |
|---|---|
| Gitea | 可达；**匿名可读**即可完成校验，写操作需 token |
| owner namespace | 必须**恰好等于** api 进程的 `PLUGIN_GIT_MIRROR_OWNER`（未设时为常量 `costrict-plugins-repo`）。导到别的 namespace 等于没导 |
| token | Gitea **site admin** PAT。建仓走 `POST /admin/users/{owner}/repos`（见 §6 障碍 2），普通 PAT 会 403 |
| DB | 只读访问 `capability_items`（构造「每个仓库应该装哪个 plugin」的期望集） |
| 工具 | `bash`（3.2 即可，无需 brew 的 bash 4）、`git`、`curl`、`python3`、`tar` |
| 磁盘 | v0.17.0 bundle 压缩包 621 MB，解包后 repos/plugins 约 1.5–2 GB |

本地 E2E 环境取 token：

```bash
# git_servers.config 是明文 JSONB；别把它打进日志
export GITEA_TOKEN=$(docker exec costrict-postgres psql -U costrict -d costrict_db -tAc \
  "select config->>'admin_token' from git_servers where server_id='e2e-gitea';" | tr -d '\n')
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: token $GITEA_TOKEN" \
  http://localhost:3001/api/v1/user      # 期望 200
```

owner 账号不存在时用 `--ensure-owner` 让脚本建（`POST /admin/users`，随机密码、`must_change_password:false`）。

---

## 3. bundle 从哪来

### 3.1 两种 bundle，别拿错

| | **catalog bundle** | **marketplace bundle** ← 本手册用这个 |
|---|---|---|
| 产出 | `release-catalog-bundle.yaml` | `costrict-plugin-marketplace/scripts/build.py` |
| 内容 | `index.json` + `catalog-download/<type>/<id>/` | `repos/plugins/<id>.git`（**真 bare repo**）+ `marketplace.json.tmpl` |
| 含 plugin 实际内容 | **否**（每个 plugin 目录只有一个 `.plugin.json`） | **是** |
| 用途 | `migrate ingest-upstream` 灌 DB | 导入 Git |

catalog bundle **不能**当仓库内容源用——脚本会因为找不到 `repos/plugins/` 直接报错并说明原因。

### 3.2 下载

```bash
gh release download v0.17.0 -R costrict-plugins-repo/costrict-plugin-marketplace \
  -p 'costrict-marketplace-bundle-*.tar.gz'
# costrict-marketplace-bundle-v0.17.0.tar.gz — 621 MB，2026-08-03
```

> 注意版本：`v0.9.0` 是本地仓库的 tag，不是 Release 现状；Release 已到 **v0.17.0**。

### 3.3 解包

脚本可以直接吃 `.tar.gz`（解到同目录的同名文件夹，`--strip-components=1`；已解开则复用，不重复解）。也可以手动：

```bash
mkdir -p /data/bundle && tar -xzf costrict-marketplace-bundle-v0.17.0.tar.gz \
  -C /data/bundle --strip-components=1
ls /data/bundle/repos/plugins | wc -l
```

### 3.4 取不到 Release 的退路

用当前 catalog 本地重建：

```bash
cd /Volumes/Work/Projects/costrict-plugin-marketplace
python3 scripts/build.py --version 0.17.1-local --catalog-bundle <catalog-bundle.tar.gz> \
  --output build --workers 8
```

代价是 clone ~303 个上游 GitHub 仓库（`--depth 1`，8 并发），是整条路上最贵的一步。理论正确率约 90.8%，残余塌根 128 条里 117 条集中在 `rohitg00/awesome-claude-code-toolkit` 一个仓库——**单点处理它就能吃掉大半残余**。塌根的那些会被脚本判成 `NAME_MISMATCH` 跳过，不会变成 409。

---

## 4. V4 范式：什么样的仓库才算「传对了」

**核心判据只有一条：仓库要能被平台自己认出来。** 认不出来的仓库，推得再成功也等于没推——它不会报错，只会安静地什么都不产生（或者更糟，变成 409）。

### 4.1 顶层 manifest 的文件名（**最容易搞错的一条**）

判定实现在 `server/internal/services/git_capability_discovery.go` 的
`classifyGitCapabilityManifest`（约 `:435-485`）。**以代码为准，V4 文档 §5.1 只列了 `skill.md` 一种，不全**：

| 类型 | discovery 认的路径 | fork probe 认的路径 |
|---|---|---|
| skill | 根 `skill.md`（按小写匹配，`SKILL.md` 同样成立）；`skills/<x>/skill.md` | `skill.md` / `SKILL.md` |
| subagent | 根 `agent.md` / `agents.md` / `subagent.md`；`.agent/agent.yaml|yml`；`agents/**.md`、`subagents/**.md`、`.claude/agents/**.md` | `agent.md` / `agents.md` / `subagent.md` |
| command | 根 `command.md`；`commands/**.md`；`.claude/commands/**.md` | `command.md` |
| mcp | 根 `mcp.json` / `.mcp.json` / `mcp.md`；`mcp/<x>/mcp.(json|md)`；（可选匹配）根 `manifest.json` / `package.json` / `pyproject.toml` | `mcp.json` / `.mcp.json` / `mcp.md` |
| **plugin** | 根 `.plugin.json` / `plugin.json` / `plugin-manifest.json`；**`.claude-plugin/plugin.json`**；`plugins/<x>/.plugin.json` | `.claude-plugin/plugin.json` → `.plugin.json` → `plugin.json`（按此顺序） |

两条硬红线：

- **绝不能用 `<slug>.md`。** 那是 `contentFilename`（`registry.go:292-305`），只负责 HTTP 下载的 attachment 文件名。仓库根放 `<slug>.md`，discovery 的根目录分类表里没有这一项 → push 成功、能力项 0 个。`capability_item_git_provision.go:46-69` 的注释把这两个函数的边界写死了：**它们回答的是不同问题，谁也不能改成谁**。
- plugin 的 **`plugin-manifest.json` 只有 discovery 认、fork probe 不认**。用它命名，fork 时会判成「manifest 读不到」→ 409。本脚本因此只接受上表 plugin 那一行的**前三个交集路径**。

### 4.2 frontmatter（markdown 四类）

按 V4 §5.2，生成形态以 `capability_item_git_provision.go` 的 `buildMarkdownSkeleton`（`:549`）为准：

```yaml
---
slug: skill-vetter
type: skill                # skill | subagent | command | mcp | plugin
name: Skill Vetter
description: Security-first skill vetting for AI agents.
category: security
version: 1.0.0             # 必须有
tags:                      # ← 顶层，不是 metadata.tags
  - security
  - vetting
metadata:
  author: costrict
  license: MIT
---

# Skill Vetter

正文……
```

- **`tags` 必须在顶层。** 解析器把整个 frontmatter map 投影成 `CapabilityItem.Metadata`，标签读的是 `Metadata["tags"]`（`parser_service.go` 与 `applyExplicitGitIndexFields`）。写成 `metadata.tags:` 会被**写进去然后永远读不到**——V4 §5.2 的示例把 tags 画在 `metadata:` 下面，那里以代码为准。`author` / `license` 对解析器没有意义，放哪都行，所以跟随 schema 放在 `metadata:` 下。
- **`version` 必须有。** 缺了设备端的更新判据就没有比较对象。
- **正文含 frontmatter 一起就是内容真相**：read-through 读回来的就是这个文件的完整字节，不要在别处重建/重排/剥离 frontmatter（键序漂移会让同步反复产生假 diff）。

### 4.3 plugin 的额外一条：`name` 必须对得上 DB

plugin 的 manifest 是 JSON，没有 frontmatter。fork 探测比对的是：

```
manifest 顶层 "name"   ==(EqualFold)==   DB 行 metadata.install.plugin_name
```

对不上 → 409（`probeRepoManifest:852-857`）。**这就是脚本 plan 阶段那张判决表的全部依据。**

注意 `仓库名 ≠ plugin_name` 是正常的：仓库名是 slug（`alirezarezvani-claude-code-skills-code-to-prd`），`plugin_name` 是 manifest 名（`code-to-prd`）。探测用 slug 找仓库、用 plugin_name 验内容，两个字段各司其职。

### 4.4 本 bundle 的实测合规率

对 v0.1.0 bundle 全部 701 个 bare repo 跑 plan（`--limit` 不设）：

```
MATCH            309   （其中 30 个 manifest 没有 version 字段）
NAME_MISMATCH     95   monorepo 塌根，装的是别的 plugin
NO_MANIFEST        0
INVALID_MANIFEST   0
NO_DB_ROW        297   bundle 有、DB 没有的条目
```

309 + 95 = 404，与 DB 的交集完全吻合。**这 95 个是导入必须挡掉的**。

---

## 5. 脚本用法

`scripts/import-bundle-to-gitea.sh`。**默认只出计划，不写任何东西**；要真导必须显式 `--apply`。

### 5.1 参数

| 参数 | 说明 |
|---|---|
| `--bundle PATH` | 解包后的 bundle 目录（含 `repos/plugins/`）或 `.tar.gz`（自动解包，已解开则复用） |
| `--gitea-url URL` | Gitea base URL，含 scheme（env `GITEA_URL`，默认 `http://localhost:3001`） |
| `--owner NAME` | mirror namespace（env `GITEA_OWNER`，默认 `costrict-plugins-repo`）。**必须与 api 的 `PLUGIN_GIT_MIRROR_OWNER` 一致** |
| `--token TOKEN` | Gitea admin PAT（env `GITEA_TOKEN`）；`--apply` 时必需 |
| `--ensure-owner` | owner 账号不存在时创建 |
| `--expect-file FILE` | 期望集 TSV（`<slug>\t<plugin_name>`），完全不碰 DB |
| `--psql-cmd CMD` | 用它跑期望集查询，例：`'docker exec -i costrict-postgres psql -U costrict -d costrict_db'`（env `PSQL_CMD`；都不给则回落 `psql "$DATABASE_URL"`） |
| `--limit N` | 只处理排序后的前 N 个（冒烟） |
| `--only a,b,c` / `--only @file` | 只处理指定 id |
| `--jobs N` | 并发推送数，默认 4；反向代理不稳时用 `1` |
| `--apply` | 真正建仓 + 推送（默认 plan-only） |
| `--dry-run` | 显式声明 plan-only（默认行为） |
| `--allow-unmatched` | 连 DB 里没有对应行的仓库也导（默认跳过） |
| `--require-version` | manifest 没有 `version` 就跳过（默认只告警照导） |
| `--keep-empty-on-failure` | 保留本次建了但没推成功的空仓库（默认删掉，见 §6 障碍 5） |
| `--no-marketplace-index` | 不渲染/推送 `.claude-plugin/marketplace.json` |
| `--force-marketplace-index` | 部分导入（`--limit`/`--only`）也推索引 |
| `--state-dir DIR` | 计划与断点状态目录，默认 `<bundle>/.costrict-import/<owner>` |

环境变量：`VERIFY_SETTLE_SECONDS`（默认 30，见 §6 障碍 6）、`HTTP_TIMEOUT`（默认 60）。

退出码：`0` 全部成功 · `3` 用法/前置条件错误 · `5` 有仓库失败 · `6` marketplace 索引推送失败。

### 5.2 典型场景

```bash
# 0) 先看计划（不写任何东西）——任何一次导入都从这一步开始
./scripts/import-bundle-to-gitea.sh --bundle /data/bundle \
  --psql-cmd 'docker exec -i costrict-postgres psql -U costrict -d costrict_db'

# 1) 冒烟：前 30 个
./scripts/import-bundle-to-gitea.sh --bundle /data/bundle --apply --limit 30 \
  --psql-cmd '...' --ensure-owner

# 2) 全量
./scripts/import-bundle-to-gitea.sh --bundle /data/bundle --apply --psql-cmd '...'

# 3) 断点续跑：同一个 --state-dir 直接重跑，已完成的自动跳过
./scripts/import-bundle-to-gitea.sh --bundle /data/bundle --apply --psql-cmd '...'

# 4) 只补几个
./scripts/import-bundle-to-gitea.sh --bundle /data/bundle --apply \
  --only anthropic-asana,anthropic-context7 --expect-file <state>/expect.tsv

# 5) 期望集离线化（生产 DB 不给直连时，在能连 DB 的机器上先导出）
psql "$DATABASE_URL" -tA -F$'\t' -c "
  SELECT slug, btrim(metadata->'install'->>'plugin_name') FROM capability_items
   WHERE item_type='plugin' AND btrim(coalesce(metadata->'install'->>'plugin_name','')) <> ''
" > expect.tsv
```

### 5.3 状态目录

| 文件 | 内容 |
|---|---|
| `plan.tsv` | 每个仓库一行：`id · 判决 · manifest 路径 · 期望 plugin · version · 备注` |
| `plan-summary.txt` | 各判决计数 |
| `expect.tsv` | 本次使用的期望集（可直接喂给下一次 `--expect-file`，保证复跑口径一致） |
| `pushed.txt` | **已推送并验证通过**的 id（断点续跑的依据；删掉它会强制重新推+重新验证） |
| `created.txt` | 本次真正新建的仓库（回滚用，见 §9） |
| `failed.tsv` | 本次失败：`id · 阶段 · 原因` |
| `logs/<id>.log` | 失败仓库的 git push 输出（成功即删） |

### 5.4 判决表

| 判决 | 处置 | 为什么 |
|---|---|---|
| `MATCH` | 导入 | manifest 名与 DB 期望一致 |
| `NAME_MISMATCH` | 跳过 | 导进去就是 409，比现状更差 |
| `NO_MANIFEST` | 跳过 | 探测读不到 manifest 是**立即 409**，不会继续下一个候选 |
| `INVALID_MANIFEST` | 跳过 | 同上（JSON 坏了 / 没有 `name`） |
| `NO_DB_ROW` | 跳过 | 没有任何行会去探测它；`--allow-unmatched` 可强导 |
| `NO_VERSION` | 默认照导并计数告警 | 设备端更新判据失效，但不影响 fork；`--require-version` 变成跳过 |

---

## 6. 已知障碍与规避

前四条是 `costrict-plugin-marketplace` 里既有工具（`scripts/mirror-to-gitea.sh` + `bundle-assets/import.sh`）的坑；本脚本已全部绕开，列在这里是为了说明**为什么不直接用那两个脚本**，以及万一你要用它们时该改什么。

| # | 障碍 | 本脚本的处置 |
|---|---|---|
| 1 | `mirror-to-gitea.sh` **硬编码 `https://`** 三处（`API=`、`BASE_URL=`、`http.extraHeader` 的 key）。自建 Gitea 是 http 时 `GITEA_HOST=localhost:3001` 会拼出 `https://localhost:3001/...` 直接失败 | 参数改成完整 base URL `--gitea-url`，API / push URL / extraHeader 的 key 全部从它派生，scheme 不再是拼接出来的 |
| 2 | `POST /user/repos` 建在 **token 所有者**名下。`costrict-plugins-repo` 是 **user 不是 org**（没有 `/orgs/...`），用 `gitadmin` 的 token 会把仓库建到 `gitadmin/` 下 → 候选 2 永远找不到 | 走 **`POST /admin/users/{owner}/repos`**，owner 显式指定（与 `gitsync/repo_ops.go:93` 同一条路径）。需要 site admin token |
| 3 | `import.sh` 要 **bash ≥ 4**（`mapfile`、关联数组），macOS 自带 3.2 | 全脚本 bash 3.2 兼容：无 `mapfile`、无关联数组、无 `wait -n`；并发用 `xargs -P` 拉起自身的 worker 模式 |
| 4 | `GET /repos/search?owner=<name>` **不按 owner 过滤**（Gitea 忽略该参数），别人 namespace 下的同名仓库会被误判成「已存在」 | 用 `GET /users/{owner}/repos` 判存在；每个仓库单独 `GET /repos/{owner}/{name}` 确认 |
| 5 | `mirror-to-gitea.sh` **先批量预建全部仓库、再推送**。任何一次推送失败都会留下一个空仓库，而空仓库 = 该 plugin 的 fork **永久 409** | 逐仓「建仓 → 推送 → 校验」；失败时把**本次创建的**仓库删掉，恢复到「不存在」这个安全状态（`--keep-empty-on-failure` 可关，但那意味着主动留下 409） |
| 6 | **Gitea 的 `is_empty` 是异步清的**：push 返回后 contents API 仍然短路返回 `[]`，本地实测滞后 **~1.8s**。而 contents API 正是 `probeRepoManifest` 的读法 | 校验带退避重试（默认最多 30s，`VERIFY_SETTLE_SECONDS`）。**平台侧同样受影响：刚导完的 plugin 在最初一两秒内 fork 会 409**，验收脚本别在 push 后立刻 fork |
| 7 | nginx `client_max_body_size` → **413** | 大仓推送前把它设成 `0`（不限）。本脚本无法从客户端规避 |
| 8 | 大仓 **504** / 传输卡死 | 每次 push 带 `http.lowSpeedLimit=1000` + `lowSpeedTime=60` + `postBuffer=524288000`；并发批次结束后对**传输类失败**再串行重试一轮（数据类失败不重试，重试也一样） |
| 9 | `import.sh` 的 `set -e` 曾**漏推 marketplace 索引**且不报错 | 索引步骤的结果**永远打印**，包括「跳过 + 跳过原因」；索引只包含真正存在的仓库，且**部分导入默认不推**（避免 30 条的索引覆盖掉完整索引），空索引拒绝发布 |

token 安全：API 走 `Authorization` header；git 走**按 host 限定**的 `http.extraHeader`，写在临时 `GIT_CONFIG_GLOBAL` 里、退出即删。token 不进 URL、不进 argv、不进日志、不进 push 失败的 stderr。

---

## 7. ⚠️ 导入前必配：discovery 的双层防御

导入 mirror 前必须在 **api 与 worker** 同时配置 `PLUGIN_GIT_MIRROR_OWNER`（默认
`costrict-plugins-repo`）以及 `GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS`（默认空）。
ingress/API 层用 owner 排除规则阻止 mirror 被重复发现，worker 层再次执行同一排除，
避免配置漂移或绕过入口后产生重复能力项。不要用“无排除开关”或停 hook 的方式规避；
导入前先发布配置并滚动重启 api、worker，再执行本脚本。

**这是导入前必须做决策的一条，不是注意事项。**

Gitea 的 system webhook 是**服务器级**的，push 到**任何**仓库都会投递。默认分支的 push 会入队一个 sync job；worker 跑到时，如果该仓库没有已绑定的能力行，就走 `discoverGitCapabilities` —— **扫全树、把每一个能被 `classifyGitCapabilityManifest` 认出的文件都建成一条新的 `capability_items` 行**。

**唯一挡住这件事的就是 owner 排除**（`gitcapability.DiscoveryOwnerExcluded`），它在两层各执行一次：webhook ingress（`handlers/git_capability_webhook.go:163` —— owner 被排除**且**该仓库没有任何已绑定的 git 行时，直接 202 `reason=discovery_owner_excluded`，**不入队**）和 worker 同步（`services/git_capability_sync_service.go:235`）。排除名单没配对，两层都拦不住。

本地实测（3 个 mirror + 1 个索引仓库）：

- 每个 mirror 仓库各产生 **1 条新的 git-backed plugin 行**，slug 取的是 manifest 的 `name`（`asana`），与 catalog 原有行（`anthropic-asana`）**不是同一条** → 目录里出现重复条目。
- 更大的 plugin 仓库不止一条：`addyosmani-…-agent-skills` 树里有 **28** 个可分类 manifest（`skills/<x>/skill.md`、`commands/*.md` 等），`anthropic-superpowers` **15** 个，`abhigyanpatwari-gitnexus` **8** 个。

按 309 个 MATCH 仓库估算，全量导入会额外产生**数千条** capability 行。

**处置：配置 owner 排除（这是现在的正确做法）**

mirror 仓库的定位是**给 fork 探测用的内容源**，不是「一个待索引的能力仓库」。平台现在有对应的开关，
导入前配好即可，不需要靠停 webhook 绕过：

```
PLUGIN_GIT_MIRROR_OWNER=costrict-plugins-repo
GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS=costrict-plugins-repo
```

排除在 **ingress/API 层与 worker 层各执行一次**——两层用的是同一份策略，所以既挡得住 webhook 入口，
也挡得住绕过入口直接入队的路径，不会因为某一层漏配而产生重复能力项。

> **排除集合的精确定义**（`internal/gitcapability/discovery_policy.go:24`）：
> `PLUGIN_GIT_MIRROR_OWNER`（默认 `costrict-plugins-repo`）**恒定包含在排除集合里**，
> `GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS` 是在此之上追加。
> ⇒ 导进默认 namespace 时，上面第二行是**冗余的**（配了也无害）；
> **导进任何其它 namespace 时，那个 namespace 必须显式加进第二行**，默认值救不了你。

**备选（只在无法改配置时用）**：

1. 导入期间不让 webhook 生效：确认目标 Gitea 上没有指向本平台的 system webhook（或临时移除），导完再恢复。
   `GIT_SYSTEM_WEBHOOK_BASE_URL` 为空时 worker 的 webhook reconciler 直接禁用，不会注册。
2. 接受并事后清理：导完后按 `source_repo_url LIKE '<gitea>/<owner>/%'` 找出这批行，
   确认它们不是想要的产物再删（含 `item_tags` / `capability_versions` / `capability_assets` 子行）。

⚠️ 备选方案 1 的代价是**导入窗口内所有仓库的正常 push 都不会被索引**，不只是 mirror 那批；
方案 2 则要求你事后能准确区分「导入产生的」与「本来就该有的」。所以优先配排除开关。

导入前后都记一次数，别靠感觉：

```sql
SELECT count(*) FROM capability_items WHERE content_backend='git';
SELECT count(*) FROM capability_items WHERE item_type='plugin';
```

---

## 8. 导入后如何验证

### 8.1 Gitea 侧：仓库在不在

```bash
curl -s -H "Authorization: token $GITEA_TOKEN" \
  "http://localhost:3001/api/v1/users/costrict-plugins-repo/repos?limit=50&page=1" \
  | python3 -c 'import sys,json;d=json.load(sys.stdin);print(len(d));[print(" ",r["name"],r["default_branch"],"empty="+str(r["empty"])) for r in d]'
```

### 8.2 平台读法回放：仓库是不是「传对了」

脚本每导一个就已经做过这件事（读不回来的会被判失败并删仓）。事后复核用同一条读法——**注意用 contents API，不是 raw**，因为平台读的就是 contents：

```bash
id=anthropic-asana
gitea_name=$(curl -s -G -H "Authorization: token $GITEA_TOKEN" --data-urlencode ref=main \
  "http://localhost:3001/api/v1/repos/costrict-plugins-repo/$id/contents/.claude-plugin/plugin.json" \
  | python3 -c 'import sys,json,base64;print(json.loads(base64.b64decode(json.load(sys.stdin)["content"]))["name"])')
db_name=$(docker exec costrict-postgres psql -U costrict -d costrict_db -tAc \
  "select metadata->'install'->>'plugin_name' from capability_items where slug='$id' and item_type='plugin';")
[ "$gitea_name" = "$db_name" ] && echo MATCH || echo MISMATCH
```

### 8.3 端到端：fork 真的走了 Git 通道

在 Cloud 上对一个已导入的 plugin 点 fork，判据：

- 进的是 **Git 详情页**而不是旧编辑页；
- 新行 `content_backend='git'`、`source_repo_url` 非空、`git_sha` 是 40 位小写十六进制；
- **不是 409**。

```sql
SELECT slug, content_backend, source_repo_url, git_sha, git_sync_status
  FROM capability_items WHERE id = '<fork 出来的 id>';
```

> 刚 push 完的仓库要等 §6 障碍 6 的 ~2s 稳定期再 fork，否则会看到一次假的 409。

### 8.4 discovery 落库（只在按 §7 选了「接受」时才需要）

```sql
SELECT item_type, count(*) FROM capability_items
 WHERE content_backend='git' AND source_repo_url LIKE 'http://localhost:3001/costrict-plugins-repo/%'
 GROUP BY 1;
SELECT repo_full_name, status, created_at FROM git_capability_sync_jobs ORDER BY created_at DESC LIMIT 10;
```

### 8.5 覆盖率怎么算

分母 = DB 里 `item_type='plugin'` 的总行数。三类互斥：

| 类别 | 判据 |
|---|---|
| **可走 Git 通道** | `<owner>/<slug>` 存在**且** manifest `name` == `install.plugin_name` |
| **回落 DB** | `<owner>/<slug>` 不存在（`errNoGiteaMirror`，静默回落） |
| **409** | 仓库存在但 manifest 读不到 / 名字不符（`errGiteaMirrorManifestInvalid`） |

导入后 409 应当恒为 **0**：脚本从不推 `NAME_MISMATCH`，也从不留下填不满的空仓库。出现 409 说明有别的写入者动过那个 namespace，**先查数据，不要放宽守卫**。

离线算法（不打 fork 接口）：拿 §5.3 的 `expect.tsv` 与 `GET /users/<owner>/repos` 的仓库清单做交集，再对交集里的每个仓库回放 §8.2 的比对。

---

## 9. 失败、重跑与回滚

### 9.1 失败分类（`failed.tsv` 第二列）

| 阶段 | 含义 | 处置 |
|---|---|---|
| `push` | git 传输失败（504 / 连接断 / 坏 bare repo） | 已自动串行重试一轮；仍失败就查 `logs/<id>.log`。若是 bundle 里的 bare repo 本身坏了，跳过它 |
| `create` | 建仓 API 失败 | 看 HTTP 码：403 → token 不是 site admin；422 → 仓库名非法 |
| `lookup` | 查仓库失败 | Gitea 不可达 / token 失效 |
| `verify` | 推完了但平台读不到 manifest | 先确认不是 §6 障碍 6 的稳定期问题（调大 `VERIFY_SETTLE_SECONDS`）；再看仓库默认分支是不是推上去的那个（脚本会尝试自动纠正默认分支） |
| `conflict` | 目标仓库已存在且装着**别的** plugin | **不会覆盖**。人工判断该仓库归谁：要么改名/删除，要么这个 slug 本来就不该导 |
| `plan` / `bundle` | 计划里没有该 id / bare repo 不在 | 通常是 `--only` 拼错，或 bundle 不完整 |

单仓失败不会中断整批；全批跑完后统一汇总，退出码 `5`。

### 9.2 重跑与断点续跑

同一个 `--state-dir` 直接重跑即可：`pushed.txt` 里的仓库会被跳过，其余补齐。中途 `Ctrl-C`/被 kill 也一样——状态是逐条落盘的。

幂等性：重复跑一次，**新建仓库数 = 0**，Gitea 仓库总数不变（`git push --force --mirror` 内容一致时是 no-op）。

要强制全部重新推 + 重新校验：删掉 `pushed.txt`。

### 9.3 回滚

`created.txt` 记录了**本次真正新建**的仓库。回滚就是删掉它们（不会碰任何本来就存在的仓库）：

```bash
while read -r id; do
  curl -s -o /dev/null -w "$id:%{http_code}\n" -X DELETE \
    -H "Authorization: token $GITEA_TOKEN" \
    "$GITEA_URL/api/v1/repos/$GITEA_OWNER/$id"
done < <state-dir>/created.txt
```

删仓库会让对应 plugin 的 fork 回到「静默回落 DB 编辑页」——即导入前的状态，用户仍然可用。

若按 §7 选了「接受 discovery」，回滚还要清掉它产生的行（先 `SELECT` 确认再删）。

---

## 10. 生产环境补齐（**本轮不执行，需单独批准**）

生产是 `gitea.costrict.ai/costrict-plugins-repo`。记忆记载 2026-06-24 已导入约 1563 个 plugin，**先核实、别假设**。

### 10.1 核实现状（只读）

```bash
# 1. 生产 Gitea 现有多少仓库
curl -s -H "Authorization: token $PROD_TOKEN" \
  "https://gitea.costrict.ai/api/v1/users/costrict-plugins-repo/repos?limit=1&page=1" -D- -o /dev/null | grep -i x-total-count
# 2. 生产 api 的 PLUGIN_GIT_MIRROR_OWNER 到底是什么（导错 namespace = 白导）
kubectl -n costrict-web get deploy <api-deploy> -o jsonpath='{.spec.template.spec.containers[0].env}' | tr ',' '\n' | grep -i plugin_git_mirror
# 3. 用生产 DB 的期望集跑一次 plan（不加 --apply），看差多少
./scripts/import-bundle-to-gitea.sh --bundle /data/bundle \
  --gitea-url https://gitea.costrict.ai --owner costrict-plugins-repo \
  --expect-file prod-expect.tsv
```

### 10.2 执行顺序

1. 按 §7 决定 webhook 处置，并**先确认生产的 system webhook 状态**；
2. nginx `client_max_body_size` 设为 `0`；
3. `--apply --limit 30 --jobs 1` 冒烟，人工核对 §8.2；
4. 全量 `--apply --jobs 4`（网络抖就降到 1）；
5. marketplace 索引：全量跑完那次会自动推，确认输出里 `marketplace index: pushed to …`；
6. 复核覆盖率（§8.5），记录三类计数。

### 10.3 风险与配额

- 全量约 1300 次建仓 API + 1300 次 push，是**外部可见的写操作**，选低峰期；
- 生产 Gitea 与本地不同，可能有仓库数配额 / 速率限制，先小批量确认；
- 一旦 `conflict` 出现，说明生产 namespace 里已有内容——**停下来查，不要覆盖**；
- 回滚见 §9.3；导入是可逆的（删仓库回到静默回落），discovery 产生的行是需要额外清理的部分。

---

## 附：相关代码位置

| 关注点 | 位置 |
|---|---|
| fork 候选与探测 | `server/internal/handlers/capability_item_fork_git.go`（`:53` owner、`:194-199` 候选 2、`:640-680` 判决、`:812` `pluginManifestPaths`、`:828` `probeRepoManifest`） |
| discovery 分类表 | `server/internal/services/git_capability_discovery.go:435-485` |
| 骨架文件名 / frontmatter | `server/internal/handlers/capability_item_git_provision.go:46-69`、`:549`（`buildMarkdownSkeleton`） |
| webhook 入口 | `server/internal/handlers/git_capability_webhook.go` |
| 同步与发现 | `server/internal/services/git_capability_sync_service.go:146`（`SyncRepository`） |
| bundle 构建 | `costrict-plugin-marketplace/scripts/build.py` |
| 既有导入工具（本脚本替代的对象） | `costrict-plugin-marketplace/scripts/mirror-to-gitea.sh`、`bundle-assets/import.sh` |
