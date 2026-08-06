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
| 1 | **`CS_BOT_TOKEN_KEY`** | ❌ **deploy/ 下完全没有** | **最致命**：`gitBackingWired()` 为 false ⇒ fork/迁移**静默回落 DB 通道**，且**启动日志一个字都不打**。表现是"功能上了但完全没生效"，我们本地为此排查了数小时 |
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

- 写进 `.env` / ConfigMap 的 `.env` 文件 → **无效**
- 必须是**真实的进程环境变量**（k8s 的 `env:` / `envFrom:` 是对的）

**同样只认真实环境变量的还有**：`GIT_SERVER_TEMPLATE_*`、`PLUGIN_GIT_MIRROR_OWNER`、
`GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS`、`GIT_CAPABILITY_WORKER_CONCURRENCY`、
`GIT_CAPABILITY_RECONCILE_INTERVAL`、`GIT_CAPABILITY_RECONCILE_BATCH_SIZE`、
`WORKER_POLL_INTERVAL_SECONDS`、`GIT_SYSTEM_HOOK_RECONCILE_INTERVAL_SECONDS`。
**唯一走 config/viper（即 `.env` 有效）的是 `GIT_SYSTEM_WEBHOOK_BASE_URL`。**
全表见 `V4_OPERATIONS.md` §9。

**上线后第一件事就是验证它生效**（见 §5 的冒烟）。

---

## 2. 前置条件

| 项 | 说明 |
|---|---|
| **线上 Gitea** | 必须先有一个可用实例，且有 admin token。V4 的所有内容都存在这里 |
| Gitea 版本 | 官方镜像即可（本地验证用 1.24.x）。**不需要**魔改版——代码只调标准 API |
| `ROOT_URL` | Gitea 的 `ROOT_URL` 必须是**用户浏览器可达**的地址，否则它生成的 `html_url` 全是坏链 |
| 默认分支 | Gitea 的 `DEFAULT_BRANCH` 必须是 **`main`**。webhook 判定依赖 `ref == refs/heads/<default_branch>`，配成 master 会让整条同步链路**静默失效** |
| 网络 | api 与 worker 的 pod 必须能出站访问 Gitea；Gitea 必须能回调 api（webhook） |

---

## 3. 部署顺序

**后端先，前端后。** 前端依赖后端的新字段（`gitSyncStatus` / `assetsBackend` / version 投影），
反过来会看到一堆 undefined。

```
1. 数据库 migration（见 §4；当前 14 个，以实际文件为准）
2. api + worker（同批，它们共用 migration 结果）
3. 验证（§5 冒烟）—— 不通过就停在这里，不要继续
4. 前端
5. bootstrap：写 git_servers + 挂 system webhook（§6）
6. 再次验证（§5 完整）
```

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

**前 11 个全部是新增列/新建表，不删不改存量数据**，所以对存量行为零影响。
最后三个是给 V4 自己新建的表加列与约束（`capability_item_git_revisions` /
`capability_sync_tombstones`），同样不触碰存量。
`item_tags` 那个把唯一键扩成 `(item_id, tag_id, source)`——目的是让 git 域与用户域的同名 tag 不再互相顶掉。

---

## 5. 冒烟验证（**每一步都必须通过才能继续**）

### 5.1 确认 `CS_BOT_TOKEN_KEY` 真的生效（最重要的一条）

```bash
# 在 api pod 里
env | grep -c '^CS_BOT_TOKEN_KEY='     # 必须是 1
```

然后**真实 fork 一次**，返回体里 `contentBackend` 必须是 `"git"`：

```bash
curl -X POST "$API/api/items/<某个db-backed的id>/fork" -H "Authorization: Bearer $TOKEN" -d '{}'
```

**返回 `"db"` 就说明 git backing 没接线** —— 回到 §1 检查。
这一步没有替代方案：日志不会告诉你它没生效。

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

没有这一行，`gitserver.DBResolver.Resolve` 返回 `ErrTenantMissingGitServer`，
fork 会走 `unavailable()` —— 对 DB-backed 源就是**静默回落**，症状与 §1 的 #1 一模一样。

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

⚠️ **已经 Git 化的行没有自动回退路径**。一旦 `content_backend='git'`，
`content` 列就被清空了（留着旧快照等于制造第二个真相源）。要退回需要人工把 Git 内容抄回列再翻转标志——
那是运维动作，不是产品功能。

⇒ **所以灰度期务必控制迁移规模**：迁得越少，回滚代价越小。

---

## 9. 上线检查清单

- [ ] 线上 Gitea 就绪，admin token 可用，`ROOT_URL` 浏览器可达，`DEFAULT_BRANCH=main`
- [ ] **`CS_BOT_TOKEN_KEY` 以真实环境变量注入 api 与 worker**（不是 `.env` 文件）
- [ ] `GIT_SERVER_TEMPLATE_ENDPOINT` + `_ADMIN_TOKEN` 已配
- [ ] `GIT_CAPABILITY_WORKER_CONCURRENCY=8`
- [ ] `server/migrations/2026080[2-9]*` 下的 migration **全部**执行成功（当前 14 个，以实际文件为准）
- [ ] api 与 worker **同批**上线
- [ ] **冒烟：fork 返回 `contentBackend: "git"`**（不通过就停）
- [ ] 存量 DB-backed 项零回归
- [ ] git_servers 的 `endpoint` 与 `config.web_url` 取值不同
- [ ] **`git_servers.config.webhook_secret` 已手工写入**（种子不会写）
- [ ] **`tenant_git_server_binding` 已建**（种子不会建）
- [ ] system webhook 已挂且能收到 push
- [ ] 前端已部署
- [ ] 明确本期**不迁移存量**（或已定分批计划）

上线后把 `V4_OPERATIONS.md` §1 的例行体检加进值班动作。
