# V4 Git Registry 故障排查手册

> 按「症状 → 可能原因 → 如何确认 → 怎么修」组织。每一条都是真实发生过的形态，不是推演。
> 上线操作序列见 `V4_PRODUCTION_ROLLOUT.md`；日常运维动作见 `V4_OPERATIONS.md`；
> 验收流程见 `CAPABILITY_GIT_E2E_RUNBOOK.md`。

## 约定

本文所有示例用这几个变量：

```bash
API=http://127.0.0.1:8080                  # costrict-web api
PSQL='psql -h 127.0.0.1 -U costrict -d costrict_db'   # 生产改成 kubectl exec ... psql
# 用户名是 costrict：本地实测 -U postgres 会 FATAL: role "postgres" does not exist
GITEA=http://127.0.0.1:3001                # Gitea 浏览器可达地址
```

---

## 0. 分诊：任何问题先做这三步

**不要跳过。** 下面 80% 的条目最终都归结到这三个之一，先做完能省掉大半排查。

### 0.1 这一行到底是 DB 还是 Git

```sql
SELECT id, slug, item_type, status,
       content_backend, git_sync_status, git_lifecycle_reason,
       left(git_sha, 7) AS sha, git_last_synced_at,
       source_git_server_id, source_git_repo_id, source_repo_path, source_git_entry_key,
       length(content) AS content_len, left(git_sync_error, 200) AS sync_error
FROM capability_items
WHERE id = '<item-id>';
```

`content_backend='db'` 的行**与 V4 无关**，别在这份文档里继续找。

> `git_lifecycle_reason` 列在这里只是为了将来 —— 它**当前恒为 NULL**（写入方未接线，见附录 A）。
> 判断「是谁下架的」用 `git_sync_status`。

### 0.2 worker 是不是活着

discovery / 同步 / reconcile **全部跑在 worker 里，不在 api 里**。只重启 api 看不到任何同步行为的变化。

```bash
# 本地
lsof -nP -iTCP:8080 -sTCP:LISTEN     # api 在不在
ps -o pid,command -p "$(pgrep -f 'exe/worker')"
# k8s
kubectl -n costrict get pods -l app=costrict-web-worker
kubectl -n costrict logs -l app=costrict-web-worker --tail=100 | grep -i "git capability"
```

worker 启动时会打印一行 `Git capability worker pool started with N workers`。**没有这行 = 池子没起来。**

### 0.3 队列有没有积压

```sql
SELECT status, count(*) FROM git_capability_sync_jobs GROUP BY status ORDER BY 2 DESC;
```

隔 60 秒再跑一次。**`pending` 单调下降 = 正常在排空；持平或上涨 = 真问题**（见 F6）。

---

## 1. 症状索引

| 症状 | 去看 |
|---|---|
| fork 返回 201，但 `contentBackend` 是 `"db"` | [F1](#f1-fork-成功但-contentbackend-仍是-db) |
| fork 返回 4xx / 5xx | [F2](#f2-fork-被拒按-error_code-分诊) |
| 详情页 / 下载 502 或 503 | [F3](#f3-详情下载assets-返回-5xx) |
| 少数老项**恒定**502，其余正常 | [F4](#f4-孤儿行db-记着仓库gitea-上已经没有) |
| 新 fork 的 `git_sha` 为空、`git_sync_status='pending'` | [F5](#f5-新-fork-的-git_sha-为空status-是-pending) |
| Gitea 改了，页面半天不跟随 | [F6](#f6-gitea-改了页面不跟随) |
| 页面跟随了，但 csc 装的还是旧的 | [F7](#f7-页面对了csc-不更新) |
| csc 装到的是线上的东西，不是本地 | [F8](#f8-csc-装的是生产的-plugin) |
| 所有需要登录的接口都 503 | [F9](#f9-需要登录的接口全-503) |
| item 突然从列表里消失 / 变成 archived | [F10](#f10-item-自己消失了) |
| 推了一个仓库，冒出几十条重复能力项 | [F11](#f11-discovery-造出大量重复行) |
| 站内编辑 / 重传 / publish 被 409 拒绝 | [F12](#f12-写入被-409-拒绝) |
| push 了但队列里连 job 都没有 | [F13](#f13-webhook-根本没进队列) |
| 页面链接点开 404 / 打不开 | [F14](#f14-仓库链接是坏链) |
| 点「去 Gitea 编辑」后登不进 Gitea / 被要求输密码 | [F15](#f15-用户点去-gitea-编辑后登不进去--被要求输密码) |

---

## F1. fork 成功但 `contentBackend` 仍是 `"db"`

**这是本轮最耗时的一个坑。** 有一条 stdout 日志可抓（见下方「如何确认」），
但它只在 rpc 后端下打印、且不进 `app.log`，很容易被当成「什么都没打」。

### 可能原因

`CS_BOT_TOKEN_KEY` 没有以**真实进程环境变量**的形式注入。

链路是：`loadBotTokenKey()` 用**裸 `os.Getenv`** 读它（`server/cmd/api/main.go`）→ 失败则
`aesForProvision == nil` → `InitUserSpaceService` 整段不执行 →
`gitBackingWired()` 为 false（`handlers/capability_item_git_provision.go`，判据是
`gitsyncDB != nil && gitsyncResolver != nil && gitsyncCrypt != nil`）→ fork 走
`unavailable()` 分支（`handlers/capability_item_fork_git.go`）。

而 `unavailable()` 对 **DB-backed 源**是**静默回落 DB fork**（返回 nil, nil），只有源本身已经是
git-backed 时才会报 503 `GIT_BACKING_UNAVAILABLE`。所以最常见的表现就是：**功能上了、一切 200、就是没生效。**

> ⚠️ **本地 `go run` 时**写进 `server/.env` 是无效的：`config.Load()` 把 `.env` 喂给 viper，
> 而 **viper 从不写 `os.Environ`**，这些调用点读的是裸 `os.Getenv`。
> 同类的还有：`PLUGIN_GIT_MIRROR_OWNER`、`GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS`、
> `GIT_SERVER_TEMPLATE_*`、`GIT_CAPABILITY_WORKER_CONCURRENCY`、`GIT_CAPABILITY_RECONCILE_*`、
> `GIT_SYSTEM_HOOK_RECONCILE_INTERVAL_SECONDS`。
> （`GIT_SYSTEM_WEBHOOK_BASE_URL` 走 config/viper，是这批里的例外。）
>
> **但容器里挂 `.env` 是有效的**：`server/docker-entrypoint.sh` 对
> `${COSTRICT_ENV_FILE:-/app/.env}` 做 `set -a; . "$env_file"; set +a` 后才 exec 应用，
> `Dockerfile` / `Dockerfile.worker` 都走这个 entrypoint，api chart 还提供
> `envFile.existingConfigMap` 专门把 ConfigMap 挂到 `/app/.env`。
> 经这条路注入的变量，裸 `os.Getenv` 读得到。**别因为「用的是 ConfigMap」就直接判定没生效。**

### 如何确认

```bash
# 1. 唯一可靠的判据：真 fork 一次
curl -sS -X POST "$API/api/items/<某个db-backed的id>/fork" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{}' \
  | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d.get("contentBackend"), d.get("sourceRepoUrl"))'
```

⚠️ **必须换一个没 fork 过的 (用户, 源 item) 组合。** fork 有「一人一源只能 fork 一次」的短路，
命中时直接返回已有那行（200 而非 201）。用之前失败那次留下的 DB-backed 行重试，
结果永远是 `"db"`，会让人以为「配全了还是不生效」。

```bash
# 2. 辅助：启动日志（log.Printf → stdout，不进 server/logs/app.log）
kubectl -n costrict logs deploy/costrict-web-api | grep CS_BOT_TOKEN_KEY
#   teamns: CS_BOT_TOKEN_KEY not configured (...); team-namespace API disabled

# 3. 辅助：进程环境（仅在用 k8s env:/envFrom: 注入时可靠）
kubectl -n costrict exec deploy/costrict-web-api -- sh -c 'env | grep -c "^CS_BOT_TOKEN_KEY="'

# 本地
cd server && set -a && source .env && set +a && go run ./cmd/api
```

关于这两条辅助判据的限制：

- **日志**只在 `USER_SERVICE_BACKEND=rpc`（`rpcClient.Configured()` 分支）下打印。
  生产必然满足 —— 空 `USER_SERVICE_URL` 会让 api 启动即 Fatal。grep 不到既可能是配好了，
  也可能是你在 `app.log` 里 grep 的（那是 zap，这行不在里面）。
- **`kubectl exec ... env`** 在 envFile / ConfigMap 挂载方式下会**假阴性**：exec 起的新进程
  环境来自容器 spec，不含 entrypoint 为应用进程 source 进去的那批。数到 0 不代表没生效。

### 怎么修

以真实环境变量注入 base64 编码的 32 字节 AES key，**api 与 worker 都要**，然后重启：

```bash
head -c 32 /dev/urandom | base64      # 生成
export CS_BOT_TOKEN_KEY='<base64>'
```

⚠️ **key 一旦换掉，已加密存下的 PAT（`user_credentials`）就解不开了**，表现为 fork 报
`GIT_CREDENTIALS_UNAVAILABLE`。轮换 key 是单独的运维动作，不是随手换。

### 相邻原因（同样表现为回落 DB）

- **没有任何 git server 记录**：`GIT_SERVER_TEMPLATE_ENDPOINT` + `_ADMIN_TOKEN` 没配，
  且 `git_servers` 表为空 → `Resolve` 返回 `ErrTenantMissingGitServer` → 同样走 `unavailable()`。
- **租户没绑 git server**：`tenant_git_server_binding` 里没有该租户的行。

```sql
SELECT server_id, kind, endpoint, enabled, is_template FROM git_servers;
SELECT * FROM tenant_git_server_binding;
```

---

## F2. fork 被拒：按 `error_code` 分诊

fork 是错误码最密集的一条路径。**先看 `error_code`，别看 HTTP 状态码** —— 同一个 409 有五种含义。

| `error_code` | 状态 | 真实含义 | 处置 |
|---|---|---|---|
| `GIT_BACKING_UNAVAILABLE` | 503 | git backing 没接线 / 租户无 git server；**只有源已经是 git-backed 时才报**，否则静默回落 | 见 F1 |
| `GIT_ACCOUNT_NOT_READY` | 409 | `user_git_binding` 缺行，或 `sync_status != 'synced'`。用户的 Gitea 账号还没开出来 | 见下方「用户账号没开通」 |
| `GIT_CREDENTIALS_UNAVAILABLE` | 500 | PAT 签发失败或解密失败（常见于换过 `CS_BOT_TOKEN_KEY`） | 删掉该用户 `user_credentials` 行让它重签，或回滚 key |
| `GIT_CREDENTIALS_MISMATCH` | 500 | PAT 属主与 `user_git_binding.git_username` 不一致 | 数据修复；不修的话 fork 会落到别人 namespace |
| `GIT_FORK_NAME_TAKEN` | 409 | **用户 namespace 里已有同名仓库，且它不是本源的 fork** | 让用户改名/删仓，或换 fork slug |
| `GIT_REPO_NAME_TAKEN` | 409 | provision 路径：同名仓库存在但**没有本能力项的 ownership marker**（或是 private、或树里有别的 manifest） | 同上 |
| `fork_slug_race` | 409 | **DB 侧** slug 唯一键冲突（`repo_id + item_type + slug`），并发请求抢到了同一个 slug | 直接重试即可 |
| `fork_slug_conflict` | 409 | 候选 slug 全被占。候选是 `<src>-fork-<hash8>` 本身（**首个候选无后缀**），其后是 `-2`…`-10`，共 10 个 | 让用户自己指定 slug |
| `GIT_SOURCE_HAS_ASSETS` | 409 | 源是 DB-backed 且带 `capability_assets`。**写侧本轮只支持单文件 provision**（读侧支持多文件） | 走 `migrate capability-to-git`，它接管整棵树 |
| `GIT_SOURCE_CONTENT_EMPTY` | 409 | 源 DB-backed 但 `content` 是空的，没东西可发布 | 数据问题，先查这行为什么空 |
| `GIT_SOURCE_MANIFEST_INVALID` | 409 | Gitea 上找到了源仓库，但里面没有这条能力项的合法 manifest | 查 mirror 是不是传错了，见 `BUNDLE_TO_GITEA_IMPORT.md` §4 |
| `GIT_SOURCE_LOOKUP_FAILED` | 502 | 探测源仓库时 Gitea 不可达 / 权限不足。**故意 fail-closed，不退化成 DB copy** | 修 Gitea 连通性或 admin token |
| `GIT_FORK_FAILED` | 502 | Gitea 的 fork API 报错（非重名） | 看返回体里带的原始错误 |
| `GIT_FORK_NAMESPACE_MISMATCH` | 500 | fork 落到了别人的 namespace —— PAT 身份与 binding 不符 | 立刻停手，这是数据一致性问题 |
| `GIT_FORK_REPO_ID_INVALID` / `GIT_FORK_RESPONSE_INVALID` | 502 | Gitea 返回的 repo 没有可用 id / `full_name` 解不开 | Gitea 侧异常 |
| `GIT_SERVER_ID_MISSING` | 503 | `git_servers.server_id` 为空 | 修 `git_servers` 行 |

> **纠正一个常见误解**：「删了 DB 行但没删 Gitea 仓库」导致的重名，报的是
> **`GIT_FORK_NAME_TAKEN` / `GIT_REPO_NAME_TAKEN`**，不是 `fork_slug_race`。
> `fork_slug_race` 是纯 DB 侧的并发插入冲突，此时 Gitea 上可能什么都没有。

### 用户账号没开通（`GIT_ACCOUNT_NOT_READY`）

```sql
SELECT user_subject_id, tenant_id, git_username, sync_status, last_synced_at, last_error
FROM user_git_binding WHERE user_subject_id = '<subject_id>';
```

`sync_status` 只有三个值：`pending` / `synced` / `error`。**只有 `synced` 能 fork。**
开户是由 cs-user 的 `user.created` 事件驱动的（`POST /api/internal/users/created`），
fork 这条路径**不会**顺手建账号 —— 它拿不到权威的 `short_id`，不允许自己编一个。

---

## F3. 详情 / 下载 / assets 返回 5xx

**这是设计行为，不是 bug。** git 行的 `content` 列不再是真相，读不到就必须报错，
绝不能拿列里的残值冒充成功（`services/git_capability_content.go` 头部注释）。

四条读路径会 read-through：详情（`GetItem`）、`/items/{id}/download`、
`/registry/{repo}/{itemType}/{slug}/*file`、`/items/{id}/assets`。
**列表接口故意不 read-through**（它把 content 置空，零出站；`?favorited=true` 也一样）——
所以「列表 200 但详情 502」是正确形态，不是矛盾。

### 五个错误码分别意味什么

| `error_code` | 状态 | 触发条件 | 是不是暂时的 |
|---|---|---|---|
| `GIT_CONTENT_UNREACHABLE` | 502 | 传输失败 / 超时 / 未识别错误。**Gitea 进程挂了走这条** | 是，恢复后自愈 |
| `GIT_CONTENT_MISSING` | 502 | Gitea 返回 **404**：仓库或文件在该 ref 上不存在 | **否**，索引行活过了它的源 |
| `GIT_CONTENT_FORBIDDEN` | 502 | Gitea 返回 401/403：admin token 失效或掉了 scope | 否，改配置 |
| `GIT_CONTENT_COORDINATE_INVALID` | 502 | 行本身残缺：缺 `source_git_server_id` / `source_git_repo_id<=0` / `source_repo_path` 为空 / 路径不安全 | 否，修数据 |
| `GIT_CONTENT_SERVER_UNAVAILABLE` | **503** | `git_servers` 里找不到这个 server、或它 `enabled=false`、或 `config.admin_token` 为空 | 否，改配置 |

> 用户口头描述里常把这五个统称为「502 `GIT_CONTENT_UNREACHABLE`」。**排查时必须区分**：
> 前者是「Gitea 挂了」，`GIT_CONTENT_MISSING` 是「Gitea 好好的，但这行指的东西没了」（见 F4），
> `GIT_CONTENT_SERVER_UNAVAILABLE` 则**根本没出网**。

响应体里会带 `repoUrl` / `repoRef` / `repoPath`，直接拿去手动验证：

```bash
curl -sS -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: token $GITEA_ADMIN_TOKEN" \
  "$GITEA/api/v1/repos/<owner>/<repo>/contents/<path>?ref=<ref>"
```

### 读的是哪个 ref

`gitCapabilityContentRef()`：**分支优先，`git_sha` 只是兜底**。
所以 push 完当场刷新详情就能看到新正文，**不必等同步 job** —— 同步 job 只负责元数据
（version / name / description / `git_sha`）。「正文变了但版本号没变」在这种时序下是正常的。

---

## F4. 孤儿行：DB 记着仓库，Gitea 上已经没有

**判据是「恒定失败」而不是「偶尔失败」**：某几条项每次都 502，其余项一切正常。

典型成因：Gitea 数据目录被重建过 / 手工删过仓库 / 换过 Gitea 实例。
注意 **repo id 会变** —— 用同样的 owner/name 重建出来的仓库是**另一个身份**，
稳定身份四元组 `git_server_id + git_repo_id + manifest_path + entry_key` 认的是数字 id。

### 如何确认

错误码是 **`GIT_CONTENT_MISSING`**（不是 `UNREACHABLE`）。逐个核对：

```sql
SELECT id, slug, source_git_server_id, source_git_repo_id, source_repo_path, git_sync_status,
       git_lifecycle_reason, left(git_sync_error, 200)
FROM capability_items
WHERE content_backend = 'git' AND git_sync_status IN ('error','orphaned')
ORDER BY git_last_synced_at NULLS FIRST;
```

```bash
# 数字 id 直查（owner/name 改名不影响）
curl -sS -H "Authorization: token $GITEA_ADMIN_TOKEN" \
  -w '\nHTTP %{http_code}\n' "$GITEA/api/v1/repositories/<git_repo_id>"
```

404 ⇒ 确认是孤儿行。

### 怎么修

worker 已经能自己识别并下架这类行：一次同步跑到 `GetRepoByID` 返回 404，
`archiveGitCapabilitiesForMissingRepository` 就会把绑定的行置 `status='archived'`、
`git_sync_status='orphaned'`（并更新 `git_last_synced_at` / 清空 `git_sync_error`）。
所以第一步是**让它跑一次**（见 `V4_OPERATIONS.md` §4 单仓 resync）。

> ⚠️ **`git_lifecycle_reason` 目前恒为 NULL —— 别拿它做判断。**
> 列和索引由 migration 建好了，`models.IsRecoverableGitLifecycleReason` 也写好了，
> 但**写入方属于尚未接线的 Phase C**：`git_capability_sync_service.go` 的三条归档路径
> 全文没有一次写 lifecycle 字段，`IsRecoverableGitLifecycleReason` **零生产调用方**
> （只有定义）。本地库实测该列 100% 为 NULL。

**决定能不能自动复活的实际字段是 `git_sync_status`**（`gitCapabilityActivateStatus` /
`gitCapabilityArchiveSyncStatus`）：manifest 回来时，**只有带 `orphaned` 标记的行会被重新激活**，
人工 archive 的行（`git_sync_status='synced'`）不会被 push 复活，`banned` 无条件不复活。

下面这张表描述的**行为是对的**，但它的成因是「Git 有没有重新看到同一个仓库身份」，不是 lifecycle 字段：

| 归档原因（概念，不是当前可查的列） | 含义 | 仓库/文件恢复后会自动复活吗 |
|---|---|---|
| manifest 从默认分支消失 | 文件没了，仓库还在 | **会**（行带 `orphaned`，下次 push 重新激活） |
| 默认分支没了 | 分支被删 | **会**（同上） |
| 仓库整个被删 | `GetRepoByID` 404 | **不会** —— 重建的仓库是**新的数字 id**，稳定身份四元组认不出它是同一个 |

第三种是终态，只能人工处置：要么接受这些行永久归档，要么把它们重新指向新仓库
（需要同时改 `source_git_repo_id` / `source_repo_url` / `git_sha`，并且**必须绕开 GORM 守卫**，
见 `V4_OPERATIONS.md` §6）。

区分方法：查 `git_capability_repositories` 里这条绑定还在不在、拿 `git_repo_id` 直接
`GET /api/v1/repositories/<id>` 打一次（见上方「如何确认」）。

---

## F5. 新 fork 的 `git_sha` 为空、`status` 是 `pending`

**多数情况下这不是故障，只是还没轮到。** 这一条历史上造成过一次假失败告警。

`git_sha` 是**异步**填充的：fork 返回 201 时行已经写好了坐标，但 `git_sync_status='pending'`、
`git_sha=''`，要等 worker 处理完对应的 job 才落值。S5 验收时抽样 30 个 fork 后**立即**查，
27/30 是空 sha + pending，形态与真故障**一模一样**；实际队列在约 7 分钟内单调排空。

### 判据

**看趋势，不看快照。**

```sql
-- 隔 60 秒跑两次，对比
SELECT count(*) FILTER (WHERE git_sync_status='pending') AS pending,
       count(*) FILTER (WHERE git_sync_status='error')   AS err,
       count(*) FILTER (WHERE git_sync_status='synced')  AS synced
FROM capability_items WHERE content_backend='git';
```

- **`pending` 在降 ⇒ 正常，等着就行。**
- `pending` 持平且 worker 存活 ⇒ 去看 F13（job 根本没入队）或 F6（队列堵住了）。
- **`error` 才是失败**，它带 `git_sync_error`：

```sql
SELECT id, slug, left(git_sync_error, 300) FROM capability_items
WHERE content_backend='git' AND git_sync_status='error';
```

> 顺带记住：`git_sync_status` 有**四个**值 —— `pending` / `synced` / `error` / `orphaned`，
> 而 `git_capability_sync_jobs.status` 是另一套 —— `pending` / `running` / `success` / **`failed`**。
> 两者别混。

---

## F6. Gitea 改了，页面不跟随

先分清改的是什么：

- **只改正文** ⇒ 详情页**立刻**就该变（read-through 读分支，不等 job）。不变的话去看 F3。
- **改 frontmatter 的 version / name / description** ⇒ 这些是投影字段，**必须等同步 job**。

### 可能原因：队列被周期 reconcile 挤爆

真实案例：周期巡检攒下 **355 个 job**，把 webhook 触发的实时同步挤到队尾，等了一个多小时。

**算一下就知道是必然的**（`internal/worker/git_capability_worker.go`）：

- 每个 worker goroutine 每个 tick **只处理一个 job**（`runWorker` → `processOne()`）。
- tick 周期 = `PollInterval`，来自 `WORKER_POLL_INTERVAL_SECONDS`，**默认 30 秒**，
  且与主 sync worker 池**共用同一个值**。
- 并发 = `GIT_CAPABILITY_WORKER_CONCURRENCY`，**默认 2**。

⇒ 默认吞吐 = **2 job / 30 秒 = 4 job/分钟 = 40 job / 10 分钟**。

而 reconcile 每 `GIT_CAPABILITY_RECONCILE_INTERVAL`（默认 **10 分钟**）一轮，
每轮最多入队 `GIT_CAPABILITY_RECONCILE_BATCH_SIZE`（默认 **50**）个 job。

**50 入 > 40 出 ⇒ 队列单调增长。** 355 个积压 ÷ 4/分钟 ≈ 89 分钟，和实测的「一个多小时」对得上。

### 如何确认

```sql
-- 积压总量 + 最老的 pending 等了多久
SELECT status, count(*), min(created_at) AS oldest
FROM git_capability_sync_jobs GROUP BY status;

-- 按来源拆开：delivery_id 前缀就是来源
SELECT CASE
         WHEN delivery_id LIKE 'reconcile:%' THEN 'reconcile'
         WHEN delivery_id LIKE 'manual:%'    THEN 'manual'
         ELSE 'webhook' END AS source,
       status, count(*)
FROM git_capability_sync_jobs
GROUP BY 1, 2 ORDER BY 1, 2;
```

`reconcile` 占绝大多数且 `pending` 不降 ⇒ 就是这个问题。

### 怎么修

两边一起调（只调一边效果有限）：

```bash
GIT_CAPABILITY_WORKER_CONCURRENCY=8        # 提高吞吐（默认 2）
GIT_CAPABILITY_RECONCILE_INTERVAL=30m      # 降低入队速率（默认 10m，Go duration 字符串）
GIT_CAPABILITY_RECONCILE_BATCH_SIZE=20     # 降低每轮批量（默认 50）
```

`WORKER_POLL_INTERVAL_SECONDS` 也能降（默认 30），但它同时影响主 sync worker 池，
改之前先确认对方扛得住。

> 应急：`git_capability_sync_jobs` 的 `pending` 行可以直接删 —— 它们是幂等的重放请求，
> 删掉最坏结果是下一轮 reconcile 重新入队。**不要删 `running` 的行**（会让 lease 记录对不上）。

### 另一个可能：这个仓库的 job 被别的 job 堵着

`claimOne()` 有 per-repo 互斥：同一个 `(git_server_id, repo_id)` 同时只允许一个 `running`。
一个卡住的 job 会让**同仓库**后续的 job 全部排队。

```sql
SELECT id, repo_full_name, status, started_at, retry_count, left(last_error,200)
FROM git_capability_sync_jobs
WHERE status='running' ORDER BY started_at;
```

`started_at` 超过 15 分钟（`LeaseTimeout`）的会被自动回收：重试次数没用完的回到 `pending`，
用完的置 `failed`。所以卡住的 job 最多堵 15 分钟，**超过 15 分钟还 running 说明 worker 挂了**。

失败重试退避是 `10^(retry_count+1) × 3 秒` —— 第一次重试等 30 秒，第二次等 **5 分钟**，
`max_attempts=3` 用完就 `failed`。所以一个反复失败的仓库最多拖 ~6 分钟就退出队列，不会永久占位。

---

## F7. 页面对了，csc 不更新

### 原因

csc 判断「装的是不是最新」**只比 version 字符串**（`src/costrict/favorite/favorite.ts`），
既不看 `updatedAt`，也不看 `contentMd5` / `gitSha`。
而 V4 的语义是**「改正文不改版本号」** —— 两者天然冲突。

服务端的解法是把 git 行的对外 version **投影**成 `<version>+<git_sha[:7]>`
（`handlers/capability_item_git_version.go`）：

- DB-backed 行：原样返回列值，**零变化**。
- git-backed 且 `len(git_sha) >= 7`：`1.2.0` → `1.2.0+e7dea05`。
- manifest 本身已带 build metadata（含 `+`）：追加为 `1.2.0+build.1.e7dea05`，保持 semver 可解析。
- `version` 为空：直接就是短 SHA。
- **`git_sha` 不足 7 位（还没同步过）：返回原始 version**，不编造锚点。

⇒ **通过的标志是：设备本地装的 version 带短 SHA 后缀。** 如果本地是干净的语义版本号，
说明它拿到的不是 git 行的投影。

### 如何确认

```bash
ID=<item-id>

# 详情侧
curl -sS "$API/api/items/$ID" \
  | python3 -c 'import sys,json;print("detail:", json.load(sys.stdin)["version"])'

# 列表侧（设备就是从这里读 version 来判断要不要更新的）
curl -sS "$API/api/items?limit=200" \
  | ID=$ID python3 -c 'import sys,json,os
d=json.load(sys.stdin)
rows=d.get("items") or d.get("data") or d
for it in rows:
    if it.get("id")==os.environ["ID"]:
        print("list:  ", it.get("version"))'
```

**两者必须逐字节相同**。不一致 ⇒ 有读路径绕过了 `itemWireVersion`，那是 bug，得改代码 ——
设备会因此每轮都认为「装的不是最新」，无限重装。

### 注意

`capability_items.version` 列里存的**仍是 manifest 原值**，投影只发生在出口。
客户端把带后缀的字符串回写过来会被守卫拒绝（`version` 在 `gitOwnedCapabilityColumns` 里）。
所以 **SQL 查出来的 version 和 API 返回的 version 不一样，是正确的**，不要去「修」它。

---

## F8. csc 装的是生产的 plugin

### 原因

`AGGREGATED_MARKETPLACE_SOURCE` 的默认值**指向生产**：

```
https://gitea.costrict.ai/costrict-plugins-repo/marketplace.git
```

（`csc/src/costrict/favorite/reconcileCloudPlugins.ts:65`）

用一个装过同名 plugin 的旧 HOME 去验，装到的很可能是**公共镜像**那份，和本地 fork 毫无关系——
**看起来通过，实则验的是线上**。

### 怎么修

用干净 HOME，并显式指向本地：

```bash
export COSTRICT_PLUGIN_MARKETPLACE_URL=http://127.0.0.1:8099/cloud-api/api/marketplace/costrict-plugins/marketplace.json
```

⚠️ 这个变量只影响 marketplace **被创建**时的 source，**不会**改写一个已经建好的同名 marketplace
（见该文件 `ensureAggregatedMarketplace` 的注释）。所以必须用干净 HOME，改环境变量对旧 HOME 无效。

验证落点：plugin 的安装状态在 `~/.costrict/plugins/installed_plugins.json` + 版本化缓存目录，
**不在 `.claude.json`**（那里 `plugins` 为空是正常的）。落点要用 `git remote -v` 证实，别看日志文字。

---

## F9. 需要登录的接口全 503

### 原因

`a863ca2` 之后，JWT 的签名与过期校验**整体委托给 cs-user**
（`POST <cs-user>/api/internal/auth/verify`）。中间件本身不再做本地解码 —— 那正是原来的 SSRF/JWT 漏洞路径。

cs-user 不可用（未配置 / 网络不通 / 5xx）时：

- `RequireAuth` → **503 fail closed**（不是 401）
- `OptionalAuth` → 静默降级为匿名（所以公开列表还能看）

### 如何确认

```bash
# api 侧配置
kubectl -n costrict exec deploy/costrict-web-api -- sh -c 'echo $USER_SERVICE_URL'
# cs-user 活着没
curl -sS -o /dev/null -w '%{http_code}\n' "$CS_USER/health"
```

> api **启动时就 fast-fail**：`USER_SERVICE_URL` 或 `USER_SERVICE_INTERNAL_TOKEN` 为空
> 直接 `logger.Fatal` 退出（`cmd/api/main.go`，`cfg.UserService.BaseURL == ""` 分支）。所以「api 起来了但全 503」
> 只可能是 cs-user 本身不通，不可能是没配 —— 没配的话 api 根本起不来。
> ⇒ 本地想跳过 cs-user 是不行的，**必须真的把它跑起来，不要加 auth 开关**。

本地：cs-user 源码就在 `costrict-web/cs-user`（独立 go.mod），**端口必须显式设 8082**（默认 8081 会撞 multica），
且两侧的 internal token 要一致。

> **有 5 分钟迷惑窗口**：introspect 结果按 token 的 SHA-256 缓存 5 分钟（`tokenCacheTTL`）。
> cs-user 刚挂时，缓存里的 token 仍然放行，看起来「一半人能用一半人 503」。别据此判断是灰度问题。

### ⚠️ 重启时别误杀

`costrict-web/server/cmd/api` 和 `cs-user/cmd/api` **两个二进制都叫 `api`**：

```bash
pkill -f "exe/api$"                        # ❌ 会把 cs-user 一起杀掉
kill <pid>                                 # ✅ 按 PID
lsof -nP -iTCP:8082 -sTCP:LISTEN           # ✅ 判断存活（lsof -ti 会把出站连接也算成占用）
```

---

## F10. item 自己消失了

git-backed 行会被 worker **自主下架**，这是设计的一部分。

```sql
SELECT id, slug, status, git_sync_status, git_lifecycle_reason, git_lifecycle_changed_at
FROM capability_items WHERE id = '<item-id>';
```

`status='archived'` + `git_sync_status='orphaned'` ⇒ **是 Git 把它下架的**，不是人。
（`git_lifecycle_reason` 当前恒为 NULL，写入方未接线 —— 见 F4 的说明，别据它判断。）

规则（`services/git_capability_sync_service.go:60-103`）：

- manifest 从 HEAD 消失 → 归档，并打 `orphaned` 标记。
- manifest 回来 → **只有带 `orphaned` 标记的行才会被自动重新激活**。
  人工 archive 的行（`git_sync_status='synced'`）不会被 push 复活 —— 否则「管理员下架 + 下一个 commit 复活」
  就成了绕过审核的洞。
- **`banned` 无条件不复活**，这是绝对的审核状态。
- 残留情形（已知并接受）：管理员去 archive 一个**已经是 orphaned** 的行，标记不会被清除
  （`git_sync_status` 是 Git 独占的列），manifest 回来时它仍会复活。

设计上，用户侧应收到一条 `capability_sync_tombstones`（`reason='git_archived'`、
`source='git_lifecycle'`），csc 靠它把本地副本卸载掉 —— **缺失不等于删除**，只有 tombstone 才授权卸载。

> ⚠️ **git 归档的 tombstone 当前未接线，别指望能查到。** 三处叠加：
> 1. 写入方 `services.RecordGitArchiveTombstonesTx` **零生产调用方**（只有测试调），
>    其自身注释写明「reserved for the Phase C lifecycle writer」。目前**唯一有真实调用方的
>    是 unfavorite 路径**（`unfavoriteItemTx` → `RecordEntitlementRemovalTx`）。
> 2. 即使写了，投递端点 `GET /api/sync/v2/snapshot` 受 `CSC_SNAPSHOT_V2_ENABLED` 门禁，
>    **默认 false**（关闭时该路由 404，是「回落 v1」信号）。
> 3. 生命周期传播另受 `CSC_SNAPSHOT_LIFECYCLE_PROPAGATION_ENABLED` 门禁，**默认 false**。
>
> 本地库实测 `capability_sync_tombstones` **0 行**。
> ⇒ 现阶段「item 被 Git 下架后设备本地副本没被卸载」是**预期行为**，不是故障。

```sql
SELECT user_id, item_id, reason, source, lifecycle_reason, removed_at
FROM capability_sync_tombstones WHERE item_id = '<item-id>';
```

---

## F11. discovery 造出大量重复行

### 原因

Gitea 的 system webhook 是**服务器级**的：push 到**任何**仓库都会投递。
worker 拿到一个**没有已绑定能力行**的仓库时，会走 `discoverGitCapabilities` ——
**扫全树，把每一个能被识别的 manifest 都建成一条新的 `capability_items` 行**。

本地**实测**：3 个 mirror 仓库各产生 **1 条**重复 plugin 行。
另外三个大仓库的 8 / 15 / 28 条是对 bundle 目录树里可分类 manifest 的**静态统计（推导，未逐个推上去验证）**。
按 309 个 MATCH 仓库估算，全量导入会额外产生**数千条**行 —— 结论方向成立，
但请把 8/15/28 当作推导上限，不是实测结果（口径与 `BUNDLE_TO_GITEA_IMPORT.md` §验证状态一致）。

### 防御在哪里

`gitcapability.DiscoveryOwnerExcluded(owner)`，**两层各执行一次**：

1. **webhook ingress**（`handlers/git_capability_webhook.go:163`）——
   owner 被排除**且**该仓库没有任何已绑定的 git 行时，直接 202 `reason=discovery_owner_excluded`，**不入队**。
2. **worker 同步**（`services/git_capability_sync_service.go` 里的同名调用）——
   `len(boundItems) == 0` 且 owner 被排除时直接返回，不做 discovery。

排除集合 = **`PLUGIN_GIT_MIRROR_OWNER`（默认 `costrict-plugins-repo`，恒定包含）**
∪ `GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS`（逗号分隔，默认空）。

> ⚠️ 常见误配：**如果你把 mirror 导进了 `PLUGIN_GIT_MIRROR_OWNER` 以外的 namespace，
> 就必须把那个 namespace 加进 `GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS`**，
> 默认值救不了你。反过来，导进默认 namespace 时，`EXCLUDED_OWNERS` 是冗余的（配了也无害）。

两个变量都走裸 `os.Getenv`，**api 与 worker 都要配**，改完必须重启（见 F1 的 viper 说明）。

### 事后清理

```sql
-- 先数，再删
SELECT count(*) FROM capability_items
WHERE content_backend='git' AND source_repo_url LIKE '<gitea>/<owner>/%';
```

删之前确认这批不是想要的产物；删的时候要连 `item_tags` / `capability_versions` / `capability_assets`
子行一起清（用 `internal/itemdelete` 那条统一硬删路径，别手写 DELETE）。

---

## F12. 写入被 409 拒绝

### `error_code: GIT_BACKED_ITEM`

站内编辑 / zip 重传 / `csc skill publish` 更新一个 git-backed 项时的**正常拒绝**。
响应带 `repoUrl` / `repoRef`，提示语是「push 到绑定的仓库」。

### `ErrGitOwnedField`（models 层兜底）

隔离机制是 **GORM hook 默认拒绝 + 显式放行**，不是逐个加 if
（`models/capability_item_git_guard.go`）。放行标记是：

```go
tx.Set(models.GitSyncBypassSetting, true)     // = "capability:gitsync"
```

受保护的列清单**以代码为准**（`models/capability_item_git_guard.go:50`），
可以程序化拿到：`models.GitOwnedCapabilityColumns()` / `models.IsGitOwnedCapabilityColumn(col)`。
**故意不在集合里**的（因为两个写者都合法）：运行时计数器（`preview_count` / `install_count` /
`favorite_count` / `experience_score`）、平台状态（`status` / `security_status` / `last_scan_id` /
`is_builtin`）、位置（`registry_id` / `repo_id`）、`descriptions` / `metadata`。

### ⚠️ hook 的盲区（**当前是三个，不是四个**）

只能在 SQL 层解决，别指望 hook：

1. `tx.Exec("UPDATE capability_items ...")` 裸 SQL —— 完全绕过 model hook。
2. `tx.Table("capability_items")` —— 同上。
3. `UpdateColumn` / `UpdateColumns` / `Session{SkipHooks: true}` —— 按定义跳过；
   守卫自己也 honor `stmt.SkipHooks`。当前调用方只写运行时计数器（`behavior_service.go`），
   那些列本来就不在保护集合里；但把同样的写法套到内容列上会**静默通过**。

> **已被堵上的第四个**：`db.Save(&[]CapabilityItem{...})` 传 slice。
> GORM 会把 slice destination 变成 `Create` + `ON CONFLICT UpdateAll`，`BeforeUpdate` 确实从不触发——
> 但现在 `BeforeCreate` → `guardGitOwnedCapabilityUpsert` 接住了它
> （`models/capability_item_git_guard.go` 的 `guardGitOwnedCapabilityUpsert`）。**旧文档里「四个盲区」的说法已过时。**

上述前 3 个盲区的写法，落到 git 行上都是**静默写坏数据**。所以裸 SQL 的调用点必须自带
`content_backend = 'db'` 谓词。

---

## F13. webhook 根本没进队列

push 之后 `git_capability_sync_jobs` 里连一条对应的行都没有。

### 逐个排除

| 检查 | 命令 / 判据 |
|---|---|
| Gitea 上有没有挂 system webhook | `curl -H "Authorization: token $GITEA_ADMIN_TOKEN" "$GITEA/api/v1/admin/hooks"` |
| `GIT_SYSTEM_WEBHOOK_BASE_URL` 配了没 | 空 ⇒ worker 的 reconciler 直接禁用，日志有 `Git system webhook reconciliation disabled` |
| 目标 URL 对不对 | 必须是 `<base>/api/internal/git-sync/<git_server_id>` |
| **推的是不是默认分支** | ingress 只认 `ref == "refs/heads/" + repository.default_branch`，否则 202 `reason=non_default_branch`。⚠️ 比的是**同一个 payload 内部的两个字段**，两个都由 Gitea 按该仓库实际情况给出 —— 仓库默认分支是 `master` 时两者同为 `master`，照样入队。**Gitea 全局 `DEFAULT_BRANCH` 不参与比较，不是失效根因**；真正的原因是「push 的是 feature 分支」或「该仓库的默认分支被改过」 |
| 签名对不对 | 签名错 ⇒ **401**，body 是 `invalid webhook signature`。注意 server 不存在 / 被禁用 / 没配 secret **也返回同一个 401**（故意不泄露配置存在性） |
| `webhook_secret` 配了没 | `SELECT config->>'webhook_secret' IS NOT NULL FROM git_servers WHERE server_id='<id>'` |
| event 类型 | 非 push ⇒ 202 `ignored`（故意的：让 Gitea 别重投） |
| owner 被排除了 | 202 `reason=discovery_owner_excluded`，见 F11 |
| 重复投递 | 202 `status=duplicate`。`(git_server_id, delivery_id)` 是唯一键，同一个 delivery 只入队一次 |
| payload 合法性 | `before`/`after` 必须是 **40 位十六进制**，`repository.id > 0`，`full_name` 是 `a/b` 且只含 `[A-Za-z0-9._-]`（首字符必须是字母数字），否则 400 |
| body 太大 | > 1 MiB ⇒ 413 |

Gitea 侧看投递记录（webhook 的 Deliveries 页）比猜快得多：它会显示我们返回的状态码和 body。

### 手工重放

见 `V4_OPERATIONS.md` §3（含 HMAC 签名脚本）与 §4（更省事的 resync 端点）。

---

## F14. 仓库链接是坏链

页面上的 Gitea 链接点开 404 / DNS 解析不了 / 指向 `https://gitea_upstream/...`。

### 原因

**`git_servers` 一行里有两个地址，容器化 / 集群化之后它们必须取不同的值：**

| 字段 | 值 | 谁用 |
|---|---|---|
| `endpoint` | 集群内可达的 API 地址 | api / worker 出站 |
| `config.web_url` | **浏览器可达**的地址 | 页面链接、csc clone |

`gitWebBase(cfg)`（`handlers/capability_item_fork_git.go:586`）用的是 `WebURL`，**为空时才退回 `Endpoint`**。
代码**故意不用 Gitea 自己返回的 `html_url`** —— ROOT_URL 配错时它会给出谁都解析不了的地址。

### 如何确认

```sql
SELECT server_id, endpoint,
       config->>'web_url'  AS web_url,
       (config->>'admin_token') IS NOT NULL AS has_token,
       (config->>'webhook_secret') IS NOT NULL AS has_secret,
       enabled
FROM git_servers;
```

`web_url` 为空且 `endpoint` 是集群内地址 ⇒ 坏链的成因。

同时确认 Gitea 自己的 `ROOT_URL` 是浏览器可达地址 —— 它影响 Gitea 页面内部生成的所有链接。

### 已经写坏的 `source_repo_url` 怎么办

`source_repo_url` 是在 fork/provision **当时**算好写死的，改 `git_servers.web_url` 不会追溯修正存量行。
批量修正需要绕开 GORM 守卫（`source_repo_url` 在保护列表里），见 `V4_OPERATIONS.md` §6。

---

## F15. 用户点「去 Gitea 编辑」后登不进去 / 被要求输密码

用户从 Cloud 详情页跳到 Gitea，落到 Gitea 登录页；或者提示需要用户名密码，而**用户从来没有 Gitea 密码**
（账号是 sync worker 用 admin token 自动开的）。

> ### ⚠️ 这一类故障**监控和冒烟都不会报**
>
> 后端 → Gitea 那一面（fork / webhook / sync / read-through）走的是 **admin token**，
> 和用户登录**完全不共用**认证路径。所以你会看到：`contentBackend: "git"` 正常、webhook 正常、
> 队列干净、健康检查全绿 —— 唯独**真人点进去用不了**。
> 只有 `V4_PRODUCTION_ROLLOUT.md` §9 那条「用真实用户身份登录 Gitea 网页」能抓到它。

### 原因（按出现概率排序）

#### 1. 跑的是**官方版 Gitea**，缺 `CoStrictJWT`

最常见。用户在 Gitea 上没有密码，登进网页**只有魔改版 `github.com/zgsm-ai/gitea` 的
`CoStrictJWT` 认证方法这一条路**（`services/auth/jwt.go`，注册在 `routers/web/web.go`
与 `routers/api/v1/api.go` 两条 auth 链上）。官方版没有它 ⇒ 必然登不进。

```bash
strings <gitea二进制> | grep -c CoStrictJWT     # 0 = 官方版（就是这个原因）
# 容器里：docker exec <gitea容器> sh -c 'strings /usr/local/bin/gitea | grep -c CoStrictJWT'
```

**修法：把镜像换成魔改版。** 没有别的绕法 —— 给用户发密码、共用 `gitadmin` 都不是方案。

> 历史成因：本套文档一度写着「官方镜像即可，不需要魔改版」，依据是「`internal/gitsync/` 只调标准端点」。
> 那个依据只覆盖后端出站，**不覆盖用户浏览器入站**。已于 2026-08-06 修正，见
> `V4_PRODUCTION_ROLLOUT.md` §2。

#### 2. 镜像是魔改版，但 `[costrict] ENABLED` 没打开

`CoStrictConfig.Enabled` **默认 false**（`modules/setting/costrict.go:69`），且它 gate 住整个 fork 面。
关闭时 `CoStrictJWT.Verify` 直接 `return nil, nil` 退给下一个认证方法 ⇒ **表现与官方版一模一样**。

```ini
[costrict]
ENABLED = true
```

#### 3. `JWT_JWKS_URL` 指错了签发方

fork 拿 JWKS 验签。当前 JWT 的**实际签发方是 cs-user**，端点是 **`/.well-known/jwks`（无 `.json` 后缀）**；
fork 源码注释里的示例 `https://costrict-web/.well-known/jwks.json` 是早期「costrict-web 自签」方案的遗留，
那个方向已被反向决策（main `a863ca2` 改为委托 cs-user）。配错的表现是 401 或 500，不是静默放行
——`Verify` 在 verifier 初始化失败 / JWKS 拉不到时是 **fail closed**。

```bash
# 魔改版自带探活端点（需 internal token），直接回显当前配置
curl -H "Authorization: Bearer $GITEA_INTERNAL_TOKEN" \
  <gitea>/api/internal/costrict/healthz
# → {"quota_enabled":..., "jwks_url":"..."}
```

改完 JWKS 后可用 `POST /api/internal/costrict/jwks-invalidate` 立即失效缓存，不必重启。

#### 4. `short_id` claim 为空，或与 Gitea 用户名对不上

fork 把 **Gitea 用户名当作 ShortID 逐字匹配**（不加 `u-` 前缀、不转小写）：

- `short_id` 为空 → 报 `costrict_jwt: empty short_id claim (user pending backfill?)`。
  老用户没回填 `short_id` 就是这个；修法是跑 cs-user 的 backfill CLI。
- `short_id` 有值但 Gitea 里没有同名用户 → 返回 **503 + `Retry-After: 5`**，
  日志 `[costrict_jwt] verified JWT for %q but Gitea user %q missing — sync worker lag?`。
  fork **故意不自动建号**（sync worker 是唯一写者）；持续 503 说明 provision 没跑成，查 `user_git_binding`。

#### 5. `BINDING_CHECK_URL` 配了但绑定还没 synced

fork 会回调 costrict-web 查 `user_git_binding.sync_status`，非 `synced` 时返回
**503 `user-gitea binding not yet synced` + `Retry-After: 5`**。通常 1 秒内自愈；
持续不好就是 provision 卡住，按 F5 / `user_git_binding` 查。

### 分诊顺序

```
strings | grep -c CoStrictJWT
 ├─ 0        → 原因 1：换魔改版镜像（到此为止，其余都不用看）
 └─ 非 0     → curl /api/internal/costrict/healthz
      ├─ 连不上 / 404   → 原因 2：ENABLED 没开
      ├─ jwks_url 不对  → 原因 3
      └─ 都对           → 看 Gitea 日志里的 [costrict_jwt] 行 → 原因 4 / 5
```

---

## 附录 A：状态字段速查

### `capability_items.content_backend`

| 值 | 含义 |
|---|---|
| `db`（默认） | 内容真相在 `content` 列 + `capability_assets` |
| `git` | 内容真相在 Gitea；`content` 列不再被写入 |

### `capability_items.git_sync_status`

| 值 | 含义 |
|---|---|
| `''` | db-backed 行 |
| `pending` | 已绑定，等 worker 首次投影。**不是失败** |
| `synced` | 正常 |
| `error` | 同步失败，看 `git_sync_error` |
| `orphaned` | **本行是被 Git 自己下架的**（区别于人工 archive），manifest 回来可自动复活 |

### `capability_items.git_lifecycle_reason` — ⚠️ **当前恒为 NULL，未接线**

列与索引已由 migration 建好，常量与 `models.IsRecoverableGitLifecycleReason` 也已定义，
但**生产代码里没有任何写入方**（归档路径只写 `status` / `git_sync_status` / `git_last_synced_at` /
`git_sync_error`），判定函数**零生产调用方**。写入方属于尚未落地的 Phase C。本地库实测该列 100% NULL。

⇒ **不要**把 NULL 读成「Git 没有主张归档」——已经被 Git 归档的行，这一列同样是 NULL。
**判断归档来源看 `git_sync_status`**：`orphaned` = Git 自己下架的，`synced` + `status='archived'` = 人工下架。

下面是这些常量**将来**的语义，现在还查不到：

| 值 | 可自动恢复（设计语义） |
|---|---|
| `manifest_removed` | ✅ |
| `default_branch_missing` | ✅ |
| `repository_deleted` | ❌ 终态 |

### `git_capability_sync_jobs.status`

`pending` → `running` → `success` / `failed`。
失败且还有重试次数时会**退回 `pending`** 并推迟 `scheduled_at`（退避 `10^(n) × 3s`）。

### `git_capability_repositories.identification_status`

`clean` / `warning` / `polluted` / `unknown`。

> ⚠️ 同表的 `next_due_at` / `reconcile_paused` / `reconcile_failures` 三列**已建但尚未接线**：
> 当前 reconciler 走的是 `last_synced_at IS NULL OR last_synced_at < now()-interval`
> （`internal/worker/git_capability_worker.go:132`）。所以**别把 `reconcile_paused=true` 当成暂停开关用**，
> 它现在不起作用。

### `user_git_binding.sync_status`

`pending` / `synced` / `error`。**只有 `synced` 能 fork。**

---

## 附录 B：诊断 SQL 速查（可直接粘贴）

```sql
-- 全局体检
SELECT content_backend, git_sync_status, status, count(*)
FROM capability_items GROUP BY 1,2,3 ORDER BY 4 DESC;

-- 队列
SELECT status, count(*), min(created_at) AS oldest, max(created_at) AS newest
FROM git_capability_sync_jobs GROUP BY status;

-- 队列按来源
SELECT CASE WHEN delivery_id LIKE 'reconcile:%' THEN 'reconcile'
            WHEN delivery_id LIKE 'manual:%'    THEN 'manual'
            ELSE 'webhook' END AS src, status, count(*)
FROM git_capability_sync_jobs GROUP BY 1,2 ORDER BY 1,2;

-- 最近失败的 job（含原因）
SELECT id, repo_full_name, ref, retry_count, max_attempts, left(last_error,300), finished_at
FROM git_capability_sync_jobs WHERE status='failed'
ORDER BY finished_at DESC NULLS LAST LIMIT 20;

-- 同步失败的能力项
SELECT id, slug, item_type, git_sync_status, left(git_sync_error,300), git_last_synced_at
FROM capability_items
WHERE content_backend='git' AND git_sync_status IN ('error','orphaned')
ORDER BY git_last_synced_at NULLS FIRST;

-- 坐标残缺的 git 行（一定读不出内容 → GIT_CONTENT_COORDINATE_INVALID）
SELECT id, slug, source_git_server_id, source_git_repo_id, source_repo_path
FROM capability_items
WHERE content_backend='git'
  AND (source_git_server_id='' OR source_git_repo_id<=0 OR source_repo_path='');

-- git 行里还残留 content 的（AC5b「不回落」要用这种做样本）
SELECT id, slug, length(content) AS content_len
FROM capability_items
WHERE content_backend='git' AND coalesce(content,'') <> ''
ORDER BY content_len DESC LIMIT 20;

-- 一个仓库上挂了多少能力项（共享仓库很常见，最大的挂过 55 个）
SELECT source_git_server_id, source_git_repo_id, count(*) AS items,
       min(source_repo_path), max(source_repo_path)
FROM capability_items WHERE content_backend='git'
GROUP BY 1,2 ORDER BY items DESC LIMIT 20;

-- 仓库绑定表（resync 端点要用它的两段 id）
SELECT git_server_id, git_repo_id, full_name, default_branch,
       left(last_synced_commit,7) AS sha, last_synced_at, left(last_error,200)
FROM git_capability_repositories ORDER BY last_synced_at NULLS FIRST LIMIT 50;

-- 某个 item 的 Git 修订史
SELECT revision_no, left(git_sha,7) AS sha, version_label, source, observed_at
FROM capability_item_git_revisions WHERE item_id='<item-id>'
ORDER BY revision_no DESC LIMIT 20;
```

---

## 附录 C：容易踩空的八件事

1. **`git_sha` 为空 + `pending` 不是故障**，是排队。判据是队列单调下降，不是瞬时快照。
2. **`error` 才是失败**；`orphaned` 是「Git 自己下架的」，不是错误。
3. **列表 200 但详情 502 是正确的** —— 列表故意不 read-through。
4. **SQL 里的 `version` 和 API 返回的 `version` 不一样是正确的** —— 后者是带短 SHA 的投影。
5. **改 discovery 相关的环境变量必须重启 worker**，只重启 api 什么都不会变。
6. **本地 `go run` 时 `.env` 对绝大多数 `GIT_*` / `CS_BOT_TOKEN_KEY` 无效**（viper 不写 `os.Environ`）；
   但**容器里挂 `/app/.env` 是有效的**（entrypoint 会 source + export）。
   连带：`kubectl exec ... env | grep` 在 envFile 方式下不可靠，判据回到真 fork 一次。见 F1。
7. **`git_lifecycle_reason` 恒为 NULL**（写入方未接线）；判归档来源看 `git_sync_status`。见 F4。
8. **同一个 (用户, 源 item) 只能 fork 一次**，第二次直接返回旧行（200）。
   验证 git backing 时务必换组合，否则会拿到第一次失败留下的 DB-backed 结果。
