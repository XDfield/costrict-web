# V4 Git Registry 上线操作手册

> 面向运维与发布负责人。**从"生产完全是 V4 之前的状态"到"V4 可用"的完整操作序列。**
> 迁移策略见 `GRAYSCALE_MIGRATION_PLAN.md`；mirror 导入见 `BUNDLE_TO_GITEA_IMPORT.md`；
> 验收方法见 `CAPABILITY_GIT_E2E_RUNBOOK.md`；上线后的日常操作见 `V4_OPERATIONS.md`；
> 出问题查 `V4_TROUBLESHOOTING.md`。目录导航见 `README.md`。本文只讲**怎么上线**。

## 0. 一句话

**部署本身不改变任何存量行为**——V4 的代码上线后，如果不配那几个环境变量，所有能力项继续走 DB 老路径，
和上线前一模一样。**Git 化是"配置开关 + 手工迁移"驱动的，不是部署驱动的。**
这也是回滚容易的原因。

---

## ⚠️ 1. 三个会让上线静默失败的落差（**上线前必须解决**）

这三个都是 **charts 里没有、而代码需要**的配置。前两个不解决，功能上线即失效且**不报错**。

| # | 变量 | chart | 不配的后果 |
|---|---|---|---|
| 1 | **`CS_BOT_TOKEN_KEY`** | ❌ **deploy/ 下完全没有** | **最致命**：`gitBackingWired()` 为 false ⇒ fork/迁移**静默回落 DB 通道**。有一条 stdout 日志可抓（见 §5.1），但很容易被淹没；表现是"功能上了但完全没生效"，我们本地为此排查了数小时 |
| 2 | **`GIT_SERVER_TEMPLATE_ENDPOINT`** + **`GIT_SERVER_TEMPLATE_ADMIN_TOKEN`** | ❌ 缺失 | 没有任何 git server 记录 ⇒ 所有 Git 操作无处可去。（另有可选的 `_DISPLAY_NAME` / `_ADMIN_USER` / `_ADMIN_PASSWORD`）|
| 3 | `GIT_CAPABILITY_WORKER_CONCURRENCY` | ❌ 缺失 | 默认 **2**，配合默认 30 秒轮询 ⇒ 吞吐只有 4 job/分钟，而周期巡检每 10 分钟入队至多 50 个：**入 > 出，队列单调增长**。本地实测攒到 355 个 job 后，用户 push 触发的实时同步排到队尾等了一个多小时。建议 **8** |

> ⚠️ **`GIT_SERVER_TEMPLATE_*` 只种一半。** 启动种子（`gitserver.BootstrapTemplate`）只写
> `config.admin_token`（+ 可选 `admin_user`/`admin_password`），**不写 `config.web_url`，也不写
> `config.webhook_secret`**，而且只建 `is_template=true` 的行、**不建租户绑定**。
> 这三件事必须在 §6 手工补齐，否则：webhook 全部 401、页面链接全是坏链、租户解析不到 git server。

已在 chart 里的（无需额外动作）：`PLUGIN_GIT_MIRROR_OWNER`、`GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS`、
`GIT_CAPABILITY_RECONCILE_INTERVAL`、`GIT_CAPABILITY_RECONCILE_BATCH_SIZE`、`GIT_SYSTEM_WEBHOOK_BASE_URL`、
`GIT_SYSTEM_HOOK_RECONCILE_INTERVAL_SECONDS`。

### 为什么 #1 特别危险

`config.Load()` 把 `.env` 喂给 **viper**，而 **viper 从不写 `os.Environ`**；
`loadBotTokenKey()` 用的是**裸 `os.Getenv`**。所以：

- **推荐**：k8s 的 `env:` / `envFrom:`，即真实进程环境变量 —— 最稳，且 `kubectl exec ... env` 查得到
- 本地 `go run` 只写 `server/.env` **无效**，必须 `set -a && source .env && set +a`

> ⚠️ **但「挂 `.env` / ConfigMap 一定无效」这句话对容器不成立。**
> 容器 entrypoint（`server/docker-entrypoint.sh`）对 `${COSTRICT_ENV_FILE:-/app/.env}` 做了
> `set -a; . "$env_file"; set +a`，`server/Dockerfile` 与 `Dockerfile.worker` 都用这个 entrypoint；
> api chart 还专门提供了 `envFile.existingConfigMap` 把 ConfigMap 挂到 `/app/.env`
> （values 注释原文：「The container entrypoint exports it before starting the API」）。
> ⇒ 走这条路注入的 `CS_BOT_TOKEN_KEY`，**裸 `os.Getenv` 是读得到的**。
> 代价是可观测性：`kubectl exec ... env` 起的是新进程，环境来自容器 spec，**看不到** entrypoint
> 为应用进程 source 进去的那批 —— 详见 §5.1。

**同样绕过 viper、直接走裸 `os.Getenv` 的还有**：`GIT_SERVER_TEMPLATE_*`、`PLUGIN_GIT_MIRROR_OWNER`、
`GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS`、`GIT_CAPABILITY_WORKER_CONCURRENCY`、
`GIT_CAPABILITY_RECONCILE_INTERVAL`、`GIT_CAPABILITY_RECONCILE_BATCH_SIZE`、
`WORKER_POLL_INTERVAL_SECONDS`、`GIT_SYSTEM_HOOK_RECONCILE_INTERVAL_SECONDS`。
**这批 Git 相关变量里，唯一走 config/viper 的是 `GIT_SYSTEM_WEBHOOK_BASE_URL`**
（`INTERNAL_SECRET` / `USER_SERVICE_*` / `CSC_SNAPSHOT_*` 等非 Git 变量也走 viper，不在本节范围内）。
全表见 `V4_OPERATIONS.md` §9。

**上线并做完 §6 的 bootstrap 之后，第一件事就是验证它生效**（见 §5 的冒烟；
bootstrap 之前跑冒烟必然假失败，理由见 §3）。

---

## 2. 前置条件

| 项 | 说明 |
|---|---|
| **线上 Gitea** | 必须先有一个可用实例，且有 admin token。V4 的所有内容都存在这里 |
| Gitea 版本 | 官方镜像即可（本地验证用 1.24.x）。**不需要**魔改版——代码只调标准 API |
| `ROOT_URL` | Gitea 的 `ROOT_URL` 必须是**用户浏览器可达**的地址，否则它生成的 `html_url` 全是坏链 |
| 默认分支 | **建议**把 Gitea 的 `DEFAULT_BRANCH` 保持 `main`（与平台自建仓库一致，少一层认知负担），但它**不是**链路失效的根因 —— 见下方说明 |
| 网络 | api 与 worker 的 pod 必须能出站访问 Gitea；Gitea 必须能回调 api（webhook） |

> **关于 `DEFAULT_BRANCH`：全局值不参与 webhook 判定。**
> ingress 比的是**同一个 payload 内部的两个字段**（`payload.Ref != "refs/heads/"+payload.Repo.DefaultBranch`，
> `handlers/git_capability_webhook.go`），两个都由 Gitea 按**该仓库的实际默认分支**给出 ——
> 仓库默认分支是 `master` 时两者同为 `master`，照样入队。读侧全程用动态值。
> 而平台自己建的能力仓库**显式指定 `main`**（`capability_item_git_provision.go` 的
> `gitCapabilityRepoBranch`），不受全局配置影响。
> ⇒ 保持 `main` 是一致性建议，不是必过项；排查「webhook 没进队列」时不要停在这一条上（见排查手册 F13）。

---

## 3. 部署顺序

**后端先，前端后。** 前端依赖后端的新字段（`gitSyncStatus` / `assetsBackend` / version 投影），
反过来会看到一堆 undefined。

```
1. 数据库 migration（见 §4；当前 14 个，以实际文件为准）
2. api + worker（同批，它们共用 migration 结果）
3. bootstrap（§6）：补 git_servers 的 web_url / webhook_secret + **建租户绑定** + 挂 system webhook
4. 验证（§5 冒烟）—— 不通过就停在这里，不要继续
5. 前端
6. 再次验证（§5 完整）
```

### ⚠️ bootstrap 必须排在冒烟之前（顺序反了会 100% 假失败）

§5.1 的冒烟判据是「真实 fork 一次，`contentBackend` 必须是 `git`」，而这**要求 §6.2 的
`tenant_git_server_binding` 已经存在**：`gitserver.DBResolver.Resolve` 查不到绑定行时返回
`ErrTenantMissingGitServer`（`internal/gitserver/resolver.go`，**没有 template 行的回退**），
fork 侧接住它走 `unavailable("no git server is bound to this tenant")`
（`handlers/capability_item_fork_git.go`），而 `unavailable()` 对 DB-backed 源是**静默回落**。
⇒ 绑定没建时冒烟结果**恒为 `"db"`**，与「`CS_BOT_TOKEN_KEY` 没配」的症状完全一样（§6.2 也写了这条因果）。

**更麻烦的是它会把错误结论固化下来**：fork 有「一人一源只能 fork 一次」的短路 ——
已存在 fork 行时直接返回那一行（**200，不是 201**）。所以配置补齐后用**同一个用户 + 同一个源**
再冒烟一次，返回的还是 bootstrap 之前落下的那条 DB-backed 旧行，`contentBackend` 依旧是 `"db"`，
于是运维会得出「配全了还是不生效」，转而去排查一个根本不存在的 `CS_BOT_TOKEN_KEY` 问题。

⇒ **重跑冒烟时必须换一个没 fork 过的 (用户, 源 item) 组合**，或先删掉上一次留下的 fork 行。

⚠️ **worker 必须与 api 同批上线**，有两个独立的理由：

1. **worker 是 Git 同步与 discovery 的唯一执行者**。只升 api，页面上一切"看起来"上线了，
   但没有任何东西会去 Gitea 拉内容 —— 新 fork 的行会永远停在 `git_sync_status='pending'`。
2. `content_md5` 的列宽 32→64 **没有 SQL migration**，靠 GORM AutoMigrate。
   跑 AutoMigrate 的是 `cmd/migrate`（`prepareSchema` → `autoMigrateAll`，含 `models.CapabilityItem{}`）
   **和 worker**；**api 完全不跑 AutoMigrate**。
   ⇒ 只要步骤 1 的 migration 真的执行过，列宽就已经到位；但如果你的部署跳过了 `cmd/migrate`
   而指望 worker 补，那 worker 落后就会导致 64 位 SHA-256 写入失败。

---

## 4. 数据库 migration

本轮共 **14 个**，goose 按序执行：

```
20260802000000  add_capability_items_git_backing
20260803000000  add_capability_items_source_repo_path
20260803010000  add_capability_items_git_identity
20260803020000  create_git_capability_sync_jobs
20260804000000  create_git_capability_repositories
20260804010000  add_item_tags_source
20260805000000  add_capability_items_git_lifecycle
20260805000100  create_capability_item_git_revisions
20260805000200  create_capability_sync_tombstones
20260805000300  create_capability_sync_snapshot_generations
20260805000400  add_git_capability_repositories_reconcile_schedule
20260805000500  add_capability_item_git_revision_content_digest
20260805000600  constrain_capability_item_git_revision_sha
20260805000700  constrain_capability_sync_tombstone_triples
```

> 这个清单会随开发继续增长。**发布前以 `server/migrations/` 下 `2026080[2-9]*` 的实际文件为准**，
> 别照抄本表：
> ```bash
> ls server/migrations/2026080[2-9]*.sql | wc -l
> ```

**前 11 个里有 10 个是纯新增列/新建表，不删不改存量数据**；最后三个是给 V4 自己新建的表加列与约束
（`capability_item_git_revisions` / `capability_sync_tombstones`），同样不触碰存量。

**唯一一个动了既有对象的是第 6 个** `20260804010000_add_item_tags_source`：它 `DROP CONSTRAINT
IF EXISTS uq_item_tag`，再建宽键 `uq_item_tag_source (item_id, tag_id, source)`，
并给 `item_tags` 加 `source` 列（存量行回填 `'legacy'`）。**不动任何数据行**，只换约束；
目的是让 git 域与用户域的同名 tag 不再互相顶掉。

---

## 5. 冒烟验证（**每一步都必须通过才能继续**）

### 5.1 确认 `CS_BOT_TOKEN_KEY` 真的生效（最重要的一条）

**前提**：§6 的 bootstrap（尤其是 §6.2 的租户绑定）必须已经做完，否则这一步恒定失败，见 §3 的警告。

**判据只有一条：真实 fork 一次，返回体里 `contentBackend` 必须是 `"git"`。**

```bash
curl -X POST "$API/api/items/<某个db-backed的id>/fork" -H "Authorization: Bearer $TOKEN" -d '{}'
```

⚠️ **换一个没 fork 过的 (用户, 源 item) 组合**。fork 有「一人一源只能 fork 一次」的短路，
命中时直接返回旧行（**200 而非 201**）—— 拿之前失败那次留下的 DB-backed 行重试，结果永远是 `"db"`。
非要复用同一组合就先删掉上一次的 fork 行。

**返回 `"db"` 就说明 git backing 没接线** —— 回到 §1（环境变量）与 §6.2（租户绑定）检查。

#### 两个辅助判据（都不能替代上面那次 fork）

```bash
# 1. 启动日志：缺 key 时 api 会打这一行（log.Printf → stdout）
kubectl -n costrict logs deploy/costrict-web-api | grep CS_BOT_TOKEN_KEY
#   teamns: CS_BOT_TOKEN_KEY not configured (...); team-namespace API disabled
```

限定条件：这行只在 `USER_SERVICE_BACKEND=rpc`（`rpcClient.Configured()`）分支里打印 ——
生产必然满足，因为空 `USER_SERVICE_URL` 会让 api 启动即 Fatal。它走 `log.Printf` 进 **stdout**，
**不进** `server/logs/app.log`（那是 zap 的输出）。所以「grep 不到」既可能是配好了，
也可能是你 grep 错了地方。

```bash
# 2. 进程环境（只在用 k8s env:/envFrom: 注入时可靠）
kubectl -n costrict exec deploy/costrict-web-api -- sh -c 'env | grep -c "^CS_BOT_TOKEN_KEY="'
```

⚠️ **这条在 envFile / ConfigMap 挂载方式下会给出假阴性**：`kubectl exec` 起的是新进程，
环境来自容器 spec，**不含** entrypoint 为应用进程 source 进去的那批（见 §1）。数到 0 不代表没生效。

⇒ **两条辅助判据都可能骗你，真 fork 一次不会。**

### 5.2 存量零回归

随便找一个 DB-backed 的 skill：详情内容、`/download` 字节、订阅、csc 落盘——**与上线前完全一致**。
V4 的所有改动都以 `content_backend='git'` 为闸，存量行不该有任何变化。

### 5.3 完整链路（可选，建议至少做一次）

按 `CAPABILITY_GIT_E2E_RUNBOOK.md` 走一遍：改 Gitea 内容 → 页面跟随 → csc 落盘 sha256 与 Gitea raw 逐字节相等。

---

## 6. bootstrap：git_servers 与 webhook

### 6.1 git_servers 的两个地址必须取不同的值

这是最容易配错的一处：

| 字段 | 值 | 谁在用 | 谁来写 |
|---|---|---|---|
| `endpoint` | Gitea 的**集群内**地址 | api / worker 出站 | `GIT_SERVER_TEMPLATE_ENDPOINT` |
| `config.admin_token` | Gitea admin token | 一切 API 调用；**为空则整个 server 被判 `ErrConfigMalformed`** | `GIT_SERVER_TEMPLATE_ADMIN_TOKEN` |
| `config.web_url` | Gitea 的**浏览器可达**地址 | 页面链接、csc clone；**为空时才退回 `endpoint`** | ❗**手工** |
| `config.webhook_secret` | HMAC 密钥 | webhook 验签 + system hook 注册 | ❗**手工** |

配成同一个值时：要么服务连不上，要么页面上全是打不开的链接。

`GIT_SERVER_TEMPLATE_*` 的自动种子（`gitserver.BootstrapTemplate`）是幂等的（已有 template 行时 no-op），
但**它只写上表前两行**。`web_url` 与 `webhook_secret` 必须手工补：

```sql
UPDATE git_servers
SET config = config
      || jsonb_build_object('web_url', 'https://gitea.example.com')
      || jsonb_build_object('webhook_secret', '<随机 32+ 字节>')
WHERE server_id = '<server_id>';
```

⚠️ **不要用 `server/cmd/gitserver-config -mode update` 改这行** —— 它的 config 结构体只声明了
`admin_token` / `admin_user` / `admin_password` 三个字段，update 时是整体重写，
**会把 `web_url` 和 `webhook_secret` 静默抹掉**。`-mode show` 是安全的。

### 6.2 租户绑定（种子不会替你做）

```bash
curl -sS -X PUT "$API/api/internal/tenants/default/git-server" \
  -H "X-Internal-Secret: $INTERNAL_SECRET" -H 'Content-Type: application/json' \
  -d '{"git_server_id":"<server_id>"}'
```

没有这一行，`gitserver.DBResolver.Resolve` 返回 `ErrTenantMissingGitServer`
（**它不会回退到 `is_template=true` 的那行**），fork 会走 `unavailable()` ——
对 DB-backed 源就是**静默回落**，症状与 §1 的 #1 一模一样。

⚠️ **所以这一步必须早于 §5 的冒烟**，否则冒烟恒定返回 `"db"`，还会因为 fork 的一次性短路
把错误结论固化住（见 §3）。

### 6.3 实例级 system webhook

Gitea 侧需要一条指向 `<GIT_SYSTEM_WEBHOOK_BASE_URL>/api/internal/git-sync/<server_id>` 的
**push** webhook。配了 `GIT_SYSTEM_WEBHOOK_BASE_URL` 后 worker 的 reconciler 会自动维护它
（默认每 300 秒收敛一次）。

⚠️ reconciler **只维护、不生成 secret**：`config.webhook_secret` 为空时它会跳过该 server，
日志是 `Git system webhook skipped serverID=... reason=missing-config fields=webhook_secret`。

⚠️ **webhook 是唯一的实时同步通道**（legacy sync scheduler 已整体禁用）。
周期 reconcile 是兜底，默认 10 分钟一轮。

---

## 7. 灰度节奏（上线 ≠ 迁移）

**上线后立刻成立的**：新 fork / 新建的能力项走 Git。
**上线后不会自动发生的**：存量 14000+ 条**原地不动**，仍是 DB-backed。

存量要 Git 化只有两条路，都是**手工触发**的：

1. 用户自己 fork（fork 产物是 git-backed）
2. 运维跑 `migrate capability-to-git`（**dry-run 是默认行为**，要 `--confirm` 才真写；
   flag 必须写成 `--type=skill` 这种 `=` 形式，空格分隔会报 `unknown flag`）

建议节奏：**先只让新内容走 Git，观察一到两周**，再考虑分批迁移存量。
分批的理由不是性能，是可回溯——一批出问题时要能一眼看出影响面。

plugin 是特例：它需要先把上游镜像导进自建 Gitea（`BUNDLE_TO_GITEA_IMPORT.md`），
否则 fork 探测不到会回落 DB。**大批量导入前必须确认 namespace 排除生效**，
否则 discovery 会把每个 mirror 仓库当成新能力项索引，造出数千条重复行。
排除集合恒定包含 `PLUGIN_GIT_MIRROR_OWNER`（默认 `costrict-plugins-repo`）；
**导进其它 namespace 时必须把它加进 `GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS`**。

---

## 8. 回滚

**代码回滚是安全的**，因为 V4 的改动全部以 `content_backend` 为闸：

- 回滚 api/worker 镜像 → 已 git 化的行会**读不到内容**（旧代码不认识 read-through），
  但存量 DB-backed 行完全正常
- migration **不需要回滚**（全是新增列/表，旧代码忽略它们）

⚠️ **已经 Git 化的行没有自动回退路径**。一旦 `content_backend='git'`，`content` 列就**不再被写入**，
读路径也不再看它。要退回需要人工把 Git 内容抄回列再翻转标志——那是运维动作，不是产品功能。

> **别指望 `content` 列一定是空的。** 主动清空只发生在两条路径：`migrate capability-to-git`
> 与 fork。**discovery 期直接建成 git-backed 的行仍可能残留旧值** ——
> 本地实测 538 条 git 行里有 **14 条** `content` 非空。
> 这些残值**不是内容来源**（读路径一律 read-through），只是清理对象
> （`migrate capability-to-git --clear-stale-content`）；
> 同时它们也正是 E2E AC5b「不回落 DB 旧值」需要的样本。

⇒ **所以灰度期务必控制迁移规模**：迁得越少，回滚代价越小。

---

## 9. 上线检查清单

**顺序即门禁**：bootstrap（后四条配置项）必须在冒烟之前完成，理由见 §3。

- [ ] 线上 Gitea 就绪，admin token 可用，`ROOT_URL` 浏览器可达
- [ ] （建议，非必过）Gitea `DEFAULT_BRANCH=main` —— 一致性考虑，不是链路失效根因（§2）
- [ ] **`CS_BOT_TOKEN_KEY` 已注入 api 与 worker**（推荐 k8s `env:`；envFile/ConfigMap 挂 `/app/.env` 同样有效，见 §1）
- [ ] `GIT_SERVER_TEMPLATE_ENDPOINT` + `_ADMIN_TOKEN` 已配
- [ ] `GIT_CAPABILITY_WORKER_CONCURRENCY=8`
- [ ] `server/migrations/2026080[2-9]*` 下的 migration **全部**执行成功（当前 14 个，以实际文件为准）
- [ ] api 与 worker **同批**上线
- [ ] git_servers 的 `endpoint` 与 `config.web_url` 取值不同
- [ ] **`git_servers.config.webhook_secret` 已手工写入**（种子不会写）
- [ ] **`tenant_git_server_binding` 已建**（种子不会建）—— **漏了这条，下一步冒烟必然假失败**
- [ ] system webhook 已挂且能收到 push
- [ ] **冒烟：用一个没 fork 过的 (用户, 源 item) 组合 fork，返回 `contentBackend: "git"`**（不通过就停）
- [ ] 存量 DB-backed 项零回归
- [ ] 前端已部署
- [ ] 明确本期**不迁移存量**（或已定分批计划）

上线后把 `V4_OPERATIONS.md` §1 的例行体检加进值班动作。
