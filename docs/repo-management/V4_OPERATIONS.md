# V4 Git Registry 运维日常手册

> 面向已经上线之后的日常操作：**怎么查状态、怎么手工干预、怎么改配置而不弄坏东西。**
> 出问题的排查见 `V4_TROUBLESHOOTING.md`；首次上线见 `V4_PRODUCTION_ROLLOUT.md`。

## 约定

```bash
API=http://127.0.0.1:8080
PSQL='psql -h 127.0.0.1 -U costrict -d costrict_db'   # 用户名是 costrict；-U postgres 会 FATAL: role does not exist
GITEA=http://127.0.0.1:3001
ADMIN_JWT=...          # 带 platform_admin 系统角色的用户 token
INTERNAL_SECRET=...     # = server 的 INTERNAL_SECRET
GITEA_ADMIN_TOKEN=...   # = git_servers.config.admin_token
```

两套鉴权别搞混：

| 路由前缀 | 鉴权 | 头 |
|---|---|---|
| `/api/admin/*` | 登录 + **平台管理员系统角色**（查 DB 的 `user_system_roles`，不是 JWT claim） | `Authorization: Bearer <jwt>` |
| `/api/internal/*` | 共享密钥 | `X-Internal-Secret: <INTERNAL_SECRET>` |
| `/api/internal/git-sync/:git_server_id` | **只认 HMAC 签名**（故意绕开 `InternalAuth`） | `X-Gitea-Signature` |

---

## 1. 例行体检

一条 SQL 看全局：

```sql
SELECT content_backend, coalesce(git_sync_status,'-') AS sync, status, count(*)
FROM capability_items
GROUP BY 1,2,3 ORDER BY 4 DESC;
```

期望形态：

- `db / - / active` —— 存量，占绝大多数。
- `git / synced / active` —— 正常的 git 行。
- `git / pending / *` —— **只应该是刚创建的少量行**；数量稳定不降就是队列问题。
- `git / error / *` —— 每一条都要看 `git_sync_error`。
- `git / orphaned / archived` —— Git 自己下架的，正常存在，但数量突增要查。

配套三条：

```sql
-- 队列
SELECT status, count(*), min(created_at) AS oldest
FROM git_capability_sync_jobs GROUP BY status;

-- 最久没同步的仓库
SELECT git_server_id, git_repo_id, full_name, last_synced_at, left(last_error,150)
FROM git_capability_repositories
ORDER BY last_synced_at NULLS FIRST LIMIT 20;

-- 用户开户情况
SELECT sync_status, count(*) FROM user_git_binding GROUP BY 1;
```

worker 日志里两行是心跳性质的，缺了就说明池子没起：

```
Git capability worker pool started with N workers
Git system webhook reconciler started, interval=...
```

---

## 2. 判断某个 item 是 DB 还是 Git

### 2.1 从 API（最快，也是用户看到的口径）

```bash
curl -sS "$API/api/items/<item-id>" \
  | python3 -c 'import sys,json;d=json.load(sys.stdin);print({k:d.get(k) for k in
    ("contentBackend","gitSyncStatus","gitLastSyncedAt","sourceRepoUrl","sourceRepoRef","sourceRepoPath","version")})'
```

`contentBackend` 缺失或为 `"db"` ⇒ DB-backed（字段是 `omitempty`）。

> ⚠️ **详情接口不返回 `gitSha`。** `ItemResponse` 里没有这个字段：git 分支只补
> `sourceRepoUrl` / `sourceRepoRef` / `sourceRepoPath` + `gitSyncStatus` + `gitLastSyncedAt`。
> 往 §2.1 的 snippet 里塞 `gitSha` 只会恒定拿到 `None`。
> 要 sha 走 §2.2 的 SQL；**列表**接口倒是有（`ItemWithRepo` 内嵌 model，带 `gitSha`）。

> `version` 这里是**投影值**（git 行带 `+<短SHA>` 后缀），和 DB 列里的值不一样，这是对的。

### 2.2 从 DB（权威）

```sql
SELECT content_backend, git_sync_status, left(git_sha,7) AS sha,
       source_git_server_id, source_git_repo_id, source_repo_path, source_git_entry_key,
       length(content) AS content_len
FROM capability_items WHERE id = '<item-id>';
```

**稳定身份四元组** = `source_git_server_id` + `source_git_repo_id` + `source_repo_path`(manifest_path) + `source_git_entry_key`。
`entry_key` 只有多 entry 的 manifest（如 `.mcp.json` 的多个 server）才非空。

> git 行的 `content` 列**不再被写入**，read-through 每次实时拉、**不缓存**。
> 但**存量 git 行可能还残留改造前的旧快照** —— 那是 `migrate capability-to-git --clear-stale-content`
> 要清理的对象，不是内容来源。

### 2.3 副作用式判据

- `GET /api/items/{id}/versions` 对 git 行返回 **200 + 空数组 + `versionBackend: "git"`**
  （平台不为它保存版本快照；discovery 之前留下的 revision 1 是快照残留，故意不服务）。
- `GET /api/items/{id}/versions/{version}` 对 git 行返回 **404 `GIT_VERSION_NOT_SERVED`**。
- `GET /api/items/{id}/git-history` 对 db 行返回 200 + `historyBackend: "db"` + 空数组，
  对 git 行返回真实修订列表（`historyBackend: "git"`）。
- `GET /api/items/{id}/assets` 对 git 行返回 `assetsBackend: "git"`。

---

## 3. 手工重放 webhook

**优先用 §4 的 resync 端点**（不用签名、不用构造 payload）。只有在需要精确模拟一次 push
（比如验证签名链路本身、或模拟默认分支删除）时才走这里。

### 3.1 签名算法

```
X-Gitea-Signature = hex( HMAC-SHA256( key = git_servers.config.webhook_secret,
                                      msg = 原始请求体字节 ) )
```

小写十六进制，对**原始 body 字节**计算 —— 序列化后再改一个空格签名就失效。
实现在 `handlers/git_capability_webhook.go:291 verifyGiteaSignature`。

### 3.2 必需的 header

| header | 值 | 缺了会怎样 |
|---|---|---|
| `X-Gitea-Signature` | 上面的 hex | 401（与「server 不存在/被禁用/没配 secret」返回同一个 401，故意不泄露配置存在性） |
| `X-Gitea-Event` | `push` | 空 → 400；非 push → 202 `ignored` |
| `X-Gitea-Delivery` | 唯一字符串，**≤128 字符** | 空或超长 → 400；重复 → 202 `duplicate`（`(git_server_id, delivery_id)` 是唯一键） |
| `Content-Type` | `application/json` | — |

### 3.3 payload 的硬性校验

```json
{
  "ref": "refs/heads/main",
  "before": "0000000000000000000000000000000000000000",
  "after":  "<40 位十六进制 commit sha>",
  "repository": {
    "id": 123,
    "full_name": "owner/repo",
    "default_branch": "main"
  }
}
```

- `ref` **必须**等于 `"refs/heads/" + repository.default_branch`，否则 202 `reason=non_default_branch`。
- `before` / `after` **都必须是 40 位十六进制**，否则 400。
- ⚠️ **`after` 全零表示「默认分支被删除」**（`isDefaultGitBranchDeletion`），会让 worker 走归档分支。
  重放时**不要**用全零的 `after`。`before` 用全零是安全的。
- `repository.full_name` 必须是 `a/b` 形态，两段都只含 `[A-Za-z0-9._-]` 且首字符是字母数字。
- body 上限 1 MiB，超了 413。

### 3.4 完整脚本

```bash
SERVER_ID=$($PSQL -tAc "SELECT server_id FROM git_servers WHERE enabled ORDER BY server_id LIMIT 1")
SECRET=$($PSQL -tAc "SELECT config->>'webhook_secret' FROM git_servers WHERE server_id='$SERVER_ID'")
REPO_ID=123
FULL_NAME=alice/my-skill
BRANCH=main
AFTER=$(curl -sS -H "Authorization: token $GITEA_ADMIN_TOKEN" \
  "$GITEA/api/v1/repos/$FULL_NAME/branches/$BRANCH" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["commit"]["id"])')

BODY=$(printf '{"ref":"refs/heads/%s","before":"%040d","after":"%s","repository":{"id":%d,"full_name":"%s","default_branch":"%s"}}' \
  "$BRANCH" 0 "$AFTER" "$REPO_ID" "$FULL_NAME" "$BRANCH")

SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $NF}')

curl -sS -X POST "$API/api/internal/git-sync/$SERVER_ID" \
  -H 'Content-Type: application/json' \
  -H 'X-Gitea-Event: push' \
  -H "X-Gitea-Delivery: manual-replay-$(date +%s)" \
  -H "X-Gitea-Signature: $SIG" \
  --data-binary "$BODY"
```

期望：`202 {"status":"queued","job_id":"...","duplicate":false}`。

> ⚠️ 必须 `--data-binary` 且 `printf` 不带换行 —— `-d` 会做换行处理，body 一变签名就对不上。

### 3.5 常见 202 但没效果的原因

| 返回 | 含义 |
|---|---|
| `{"status":"ignored","reason":"non_default_branch"}` | `ref` 与 `default_branch` 不匹配 |
| `{"status":"ignored","reason":"discovery_owner_excluded"}` | owner 在排除名单里**且**该仓库没有已绑定的 git 行（见排查手册 F11） |
| `{"status":"duplicate"}` | `delivery_id` 重了，没有新建 job |
| `{"status":"ignored"}`（无 reason） | `X-Gitea-Event` 不是 push |

---

## 4. 触发单个仓库 resync（推荐路径）

```
POST /api/admin/git-capability-repositories/{git_server_id}/{git_repo_id}/resync
```

**路径含两段，两段都是身份的一部分**：Gitea 的数字 repo id **只在单个 server 内唯一**。
表上的唯一键就是 `(git_server_id, git_repo_id)`，也是 V4 稳定身份四元组的前半截。
少写一段会被路由 404 挡掉 —— 这是故意的设计，避免「resync 了别人的仓库还报成功」。

```bash
curl -sS -X POST \
  "$API/api/admin/git-capability-repositories/$SERVER_ID/$REPO_ID/resync" \
  -H "Authorization: Bearer $ADMIN_JWT"
# 202 {"status":"queued","job_id":"...","duplicate":false}
```

要点：

- **鉴权是平台管理员系统角色**（查 DB 的 `user_system_roles`，不是 JWT claim）。非管理员 403。
- **幂等窗口是「分钟桶」**：`delivery_id = "manual:<repo_id>:<unix分钟>"`。
  同一分钟内重复调用返回 `duplicate: true`，不会重复排队；跨分钟可以再次触发。
- 参数来自 `git_capability_repositories`，**404 表示这个仓库还没有绑定行**。
  刚 fork 出来、第一次同步 job 还没跑过的仓库就会 404 —— 这种情况等它自然排到，或走 §3 重放。
- 排队后仍然要等 worker 轮到（见 §5 的吞吐算式），**不是同步执行**。

找参数：

```sql
SELECT git_server_id, git_repo_id, full_name, default_branch, last_synced_at
FROM git_capability_repositories WHERE full_name = 'alice/my-skill';
```

或者从能力项反查：

```sql
SELECT DISTINCT source_git_server_id, source_git_repo_id
FROM capability_items WHERE id = '<item-id>';
```

---

## 5. 同步队列的读与管

### 5.1 队列长什么样

```sql
SELECT status, count(*), min(created_at) AS oldest FROM git_capability_sync_jobs GROUP BY status;

SELECT CASE WHEN delivery_id LIKE 'reconcile:%' THEN 'reconcile'
            WHEN delivery_id LIKE 'manual:%'    THEN 'manual'
            ELSE 'webhook' END AS src,
       status, count(*)
FROM git_capability_sync_jobs GROUP BY 1,2 ORDER BY 1,2;
```

`delivery_id` 的前缀就是来源（`models.GitCapabilitySyncDeliveryPrefix*`）：

| 前缀 | 来源 |
|---|---|
| 无前缀（UUID） | Gitea webhook 的 `X-Gitea-Delivery` |
| `reconcile:<repo_id>:<bucket>` | worker 周期巡检 |
| `manual:<repo_id>:<分钟>` | §4 的 resync 端点 |

### 5.2 吞吐是可以算的

`internal/worker/git_capability_worker.go`：每个 worker goroutine **每 tick 只处理一个 job**。

```
吞吐 = GIT_CAPABILITY_WORKER_CONCURRENCY  /  WORKER_POLL_INTERVAL_SECONDS
默认 =            2                       /            30 秒          = 4 job/分钟
```

而 reconcile 每 `GIT_CAPABILITY_RECONCILE_INTERVAL`（默认 10m）入队至多
`GIT_CAPABILITY_RECONCILE_BATCH_SIZE`（默认 50）个。**默认参数下 50 入 > 40 出，队列会单调增长。**
仓库数上来之后必须调参，见排查手册 F6。

### 5.3 其它时间常数

| 常数 | 值 | 含义 |
|---|---|---|
| `LeaseTimeout` | 15 分钟 | `running` 超过它会被回收：还有重试次数 → 退回 `pending`；用完 → `failed` |
| `JobTimeout` | 10 分钟 | 单个 job 的 context 超时 |
| 重试退避 | `10^(n) × 3s` | 第 1 次重试 30s，第 2 次 5min |
| `MaxAttempts` | 3 | 用完置 `failed` |
| per-repo 互斥 | — | 同一 `(git_server_id, repo_id)` 同时只允许一个 `running` |

### 5.4 清积压

```sql
-- 只删 pending。它们是幂等的重放请求，最坏结果是下轮 reconcile 重新入队。
DELETE FROM git_capability_sync_jobs
WHERE status = 'pending' AND delivery_id LIKE 'reconcile:%';
```

⚠️ **不要删 `running` 行** —— lease 记录对不上会让 worker 在 finalize 时报
`ErrGitCapabilityLeaseLost`。等它自己被 lease 超时回收（≤15 分钟）。

`failed` 的行留着做审计，不影响运行。

---

## 6. 改 `git_servers`：三个必须知道的坑

### 6.1 一行两个地址，取值必须不同

```sql
SELECT server_id, endpoint,
       config->>'web_url' AS web_url,
       (config->>'admin_token')    IS NOT NULL AS has_token,
       (config->>'webhook_secret') IS NOT NULL AS has_secret,
       (config->>'admin_user')     IS NOT NULL AS has_admin_user,
       enabled, is_template
FROM git_servers;
```

| 字段 | 值 | 谁在用 |
|---|---|---|
| `endpoint` | 集群内 API 地址 | api / worker 出站 |
| `config.web_url` | **浏览器可达**地址 | 页面链接、csc clone。**为空时才退回 `endpoint`** |
| `config.admin_token` | Gitea admin token | 一切 API 调用。**为空 ⇒ 整个 server 被判 `ErrConfigMalformed`** |
| `config.webhook_secret` | HMAC 密钥 | webhook 验签 + system hook 注册 |
| `config.admin_user` / `admin_password` | Gitea 管理员账密 | 少数需要 basic auth 的路径 |

### 6.2 ⚠️ `gitserver-config -mode update` 会**丢掉** `web_url` 和 `webhook_secret`

`server/cmd/gitserver-config/main.go` 里的 `gitServerConfigJSON` **只声明了三个字段**
（`admin_token` / `admin_user` / `admin_password`）。它的 update 流程是
「反序列化 → 改字段 → 重新序列化 → 整体覆盖 `config`」，
所以 **`web_url` 和 `webhook_secret` 会被静默抹掉**。

后果：所有仓库链接退回集群内地址（浏览器打不开），且 webhook 全部 401。

⇒ **用它 `-mode show` 是安全的；`-mode update` 之前必须先备份 config，之后必须补回丢掉的键。**

```bash
# 用之前先存一份
$PSQL -tAc "SELECT config FROM git_servers WHERE server_id='$SERVER_ID'" > /tmp/gs-config.json
```

### 6.3 HTTP API 的 `config` 是**整体替换**

```
PUT /api/internal/git-servers/{server_id}      X-Internal-Secret: <secret>
```

body 里的 `config` 是 `json.RawMessage`：**给了就整体替换**（`{}` = 清空），不给则不动。
所以要改一个键，必须把完整对象发过去：

```bash
CUR=$(curl -sS -H "X-Internal-Secret: $INTERNAL_SECRET" "$API/api/internal/git-servers/$SERVER_ID" \
      | python3 -c 'import sys,json;print(json.load(sys.stdin)["config"])')
NEW=$(printf '%s' "$CUR" | python3 -c 'import sys,json;c=json.load(sys.stdin);c["web_url"]="https://gitea.example.com";print(json.dumps(c))')
curl -sS -X PUT "$API/api/internal/git-servers/$SERVER_ID" \
  -H "X-Internal-Secret: $INTERNAL_SECRET" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys;print(json.dumps({"config":json.loads(sys.argv[1])}))' "$NEW")"
```

### 6.4 `GIT_SERVER_TEMPLATE_*` 只种一半

启动时的自动种子（`gitserver.BootstrapTemplate`）**只写** `admin_token`（+ 可选的
`admin_user`/`admin_password`），**不写 `web_url`，也不写 `webhook_secret`**。

⇒ 光配环境变量是**不够**的，还得手工补这两个键，否则：

- 缺 `webhook_secret` ⇒ worker 的 system hook reconciler 跳过该 server，
  日志是 `Git system webhook skipped serverID=... reason=missing-config fields=webhook_secret`，
  且所有入站 webhook 401。
- 缺 `web_url` 且 `endpoint` 是集群内地址 ⇒ 页面链接全是坏链。

另外它建的是 `is_template=true` 的行，**租户绑定仍需手工做**：

```bash
curl -sS -X PUT "$API/api/internal/tenants/default/git-server" \
  -H "X-Internal-Secret: $INTERNAL_SECRET" -H 'Content-Type: application/json' \
  -d '{"git_server_id":"'$SERVER_ID'"}'
```

### 6.5 system webhook 是否已挂

```bash
curl -sS -H "Authorization: token $GITEA_ADMIN_TOKEN" "$GITEA/api/v1/admin/hooks" \
  | python3 -m json.tool | head -40
```

目标 URL 必须是 `<GIT_SYSTEM_WEBHOOK_BASE_URL>/api/internal/git-sync/<server_id>`，
`events: ["push"]`，`is_system_webhook: true`。worker 每
`GIT_SYSTEM_HOOK_RECONCILE_INTERVAL_SECONDS`（默认 300 秒）收敛一次，
**它只维护、不生成 secret**。

---

## 7. 存量迁移的正确调用形式

```bash
cd server
export CS_BOT_TOKEN_KEY='<base64 32 字节>'        # 必须真实导出
go run ./cmd/migrate capability-to-git --type=skill --owner=<short_id|subject_id>
```

**flag 必须用 `=` 形式**（`--type=skill`），空格分隔会报 `unknown flag`。

| flag | 默认 | 说明 |
|---|---|---|
| `--type=a,b` | — | 逗号分隔，只接受可迁移类型 |
| `--owner=<id>` | — | `short_id` 或 `subject_id` |
| `--ids=a,b,c` | — | 精确指定 |
| `--limit=N` | **50** | |
| `--tenant=<id>` | `default` | |
| `--include-catalog` | 关 | 默认**跳过** catalog 镜像行（它们的真相在上游 GitHub，republish 会造出第二真相源） |
| `--clear-stale-content` | 关 | **另一件事**：清空「已经是 git-backed 但还残留旧 content 快照」的行 |
| `--dry-run` | — | 接受但无意义，dry-run 本来就是默认 |
| **`--confirm`** | — | **只有它才真写** |

⚠️ 别和 `scripts/import-bundle-to-gitea.sh` 混：那个脚本的执行开关是 **`--apply`**。
`migrate capability-to-git` 只认 `--confirm`，写 `--apply` 会被当成未知 flag 报错。

⚠️ **必须至少给一个 `--type=` / `--owner=` / `--ids=`**，否则命令直接拒绝
（无范围的 plan 会打出上万条 catalog 镜像，且离误执行只差一个 flag）。

分批建议见 `GRAYSCALE_MIGRATION_PLAN.md` §4。

---

## 8. 运维要改 git 行的数据怎么办

写者隔离是 **GORM hook 默认拒绝 + 显式放行**：`models.GitSyncBypassSetting`
（值 `"capability:gitsync"`）。受保护的列清单**以代码为准**，可以程序化拿到：
`models.GitOwnedCapabilityColumns()` / `models.IsGitOwnedCapabilityColumn(col)`
（定义在 `models/capability_item_git_guard.go:50`）。

运维改数据有两条路：

### 8.1 直接 SQL（会绕过 hook —— 这是它的三个盲区之一）

```sql
-- 例：把一批行从旧仓库指到新仓库
UPDATE capability_items
SET source_git_repo_id = <新id>, source_repo_url = '<新url>', git_sha = '', git_sync_status = 'pending'
WHERE content_backend = 'git' AND source_git_repo_id = <旧id>;
```

**能改成功，但责任全在你身上**：hook 拦不住 `tx.Exec` / `tx.Table` /
`UpdateColumn(s)` / `Session{SkipHooks}`。改完立刻用 §4 触发一次 resync 验证。

> `db.Save(&[]CapabilityItem{})` 传 slice 这条**已经被 `BeforeCreate` 堵上了**
> （GORM 转成 `Create` + `ON CONFLICT UpdateAll`，`guardGitOwnedCapabilityUpsert` 会拒绝）。
> 旧文档写「四个盲区」的地方，现在是**三个**。

### 8.2 让 worker 自己收敛（首选）

绝大多数「数据不对」的情况，正确解法是**修 Gitea 上的内容，然后 resync**，
而不是手改 DB —— 手改的字段下一次同步就会被 worker 覆盖回去。

**不在保护集合里**（两个写者都合法，可以放心改）的完整清单以
`models/capability_item_git_guard.go` 的 `gitOwnedCapabilityColumns` 注释为准，当前是四类：

- 运行时计数器：`preview_count` / `install_count` / `favorite_count` / `experience_score`
- 平台状态：`status` / `security_status` / **`last_scan_id`** / **`is_builtin`**
- 位置：`registry_id` / `repo_id`
- **`descriptions` / `metadata`** —— fork 建行后会回填 descriptions，metadata 是合并而非独占

（`V4_TROUBLESHOOTING.md` §F12 列的是同一份清单，两处必须一致。）

---

## 9. 环境变量全表

「走 viper 吗」= `config.Load()` 是否负责读它。**❌ 表示调用点是裸 `os.Getenv`**，
必须是进程环境变量 —— k8s `env:` / `envFrom:` 直接满足，容器里挂 `/app/.env` 经 entrypoint export
也满足（见表下说明）；本地 `go run` 只写 `.env` 不满足。

| 变量 | 默认 | 谁读 | 走 viper 吗 |
|---|---|---|---|
| `CS_BOT_TOKEN_KEY` | — | api（裸 `os.Getenv`） | ❌ |
| `GIT_SERVER_TEMPLATE_ENDPOINT` | — | api 启动种子 | ❌ |
| `GIT_SERVER_TEMPLATE_ADMIN_TOKEN` | — | 同上 | ❌ |
| `GIT_SERVER_TEMPLATE_DISPLAY_NAME` / `_ADMIN_USER` / `_ADMIN_PASSWORD` | — | 同上（可选） | ❌ |
| `PLUGIN_GIT_MIRROR_OWNER` | `costrict-plugins-repo` | api + worker | ❌ |
| `GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS` | 空 | api + worker | ❌ |
| `GIT_CAPABILITY_WORKER_CONCURRENCY` | **2** | worker | ❌ |
| `GIT_CAPABILITY_RECONCILE_INTERVAL` | **10m**（Go duration） | worker | ❌ |
| `GIT_CAPABILITY_RECONCILE_BATCH_SIZE` | **50** | worker | ❌ |
| `WORKER_POLL_INTERVAL_SECONDS` | **30** | worker（**两个池子共用**） | ❌ |
| `GIT_SYSTEM_HOOK_RECONCILE_INTERVAL_SECONDS` | **300** | worker | ❌ |
| `GIT_SYSTEM_WEBHOOK_BASE_URL` | 空（= 禁用 reconciler） | worker | **✅ 走 config/viper** |
| `USER_SERVICE_URL` + `USER_SERVICE_INTERNAL_TOKEN` | — | api（JWT 校验委托给 cs-user）。**为空则 api 启动即 Fatal 退出** | ✅ |
| `INTERNAL_SECRET` | — | api（`/api/internal/*`） | ✅ |
| `CSC_SNAPSHOT_V2_ENABLED` | **false** | api —— 挂载 `GET /api/sync/v2/snapshot`。关闭时该路由 **404**，这是给混合车队的「回落 v1」信号 | ✅ |
| `CSC_SNAPSHOT_LIFECYCLE_PROPAGATION_ENABLED` | **false** | api —— 允许把 `git_archived` tombstone 放进 snapshot，指示客户端**删除设备上的文件**。这是生产 kill switch | ✅ |

> ⚠️ 后两个开关随 csc snapshot v2 一同交付。**配之前先确认你部署的镜像里有这个功能**
> （打开开关后 `GET /api/sync/v2/snapshot` 不再 404），否则配了也不会有任何效果。
>
> 后两个开关**都默认关闭，且当前也没有写入方**：git 归档的 tombstone 写入函数
> `RecordGitArchiveTombstonesTx` 零生产调用方（Phase C 未接线），所以即便打开
> `CSC_SNAPSHOT_LIFECYCLE_PROPAGATION_ENABLED` 也不会有 git 生命周期 tombstone 可传播。
> 关掉永远是安全的（被抑制的 tombstone 只表现为「该项缺席」，缺席是 no-op）。
> ⚠️ 打开 `LIFECYCLE_PROPAGATION` 前必须确认全部在网客户端支持 contract v2，
> 否则老客户端会永久持有云端已删除的能力项。详见排查手册 F10。

**「走 viper 吗 = ❌」的那些**：`config.Load()` 把 `.env` 喂给 viper，而 **viper 从不写
`os.Environ`**，这些调用点用的是裸 `os.Getenv`。⇒ 本地 `go run` 时只写 `.env` 无效，
必须 `set -a && source .env && set +a`。

> ⚠️ **容器里另当别论。** `server/docker-entrypoint.sh` 会对
> `${COSTRICT_ENV_FILE:-/app/.env}` 做 `set -a; . "$env_file"; set +a` 后再 exec 应用，
> api / worker 两个 Dockerfile 都用它，api chart 还提供 `envFile.existingConfigMap`
> 把 ConfigMap 挂到 `/app/.env`。**经这条路注入的变量，裸 `os.Getenv` 读得到**，
> 上表 ❌ 那一列描述的是 viper 不参与，不是「容器里挂 .env 没用」。
>
> k8s 的 `env:` / `envFrom:` 仍是**推荐**方式，因为它可观测：`kubectl exec ... env | grep`
> 只看得到容器 spec 里的变量，**看不到** entrypoint 为应用进程 source 进去的那批 ——
> 用 envFile 时这条命令会给出假阴性。

改任何一个 `GIT_CAPABILITY_*` / `PLUGIN_GIT_MIRROR_OWNER` /
`GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS` 都**必须重启 worker** —— discovery 跑在 worker 里，
只重启 api 什么都不会变。

---

## 10. 本地环境的两个必踩项

```bash
# ❌ 会连 cs-user 一起杀 —— costrict-web/server/cmd/api 与 cs-user/cmd/api 二进制同名
pkill -f "exe/api$"

# ✅
kill <pid>
lsof -nP -iTCP:8080 -sTCP:LISTEN     # api；lsof -ti 会把出站连接也算成占用
lsof -nP -iTCP:8082 -sTCP:LISTEN     # cs-user（端口必须显式设 8082，默认 8081 撞 multica）
```

> ⚠️ **[修正 2026-08-06] 这里原先写着「官方镜像即可，不需要魔改版」，那是错的。**
> 生产**必须**用魔改版 `github.com/zgsm-ai/gitea`，理由见 `V4_PRODUCTION_ROLLOUT.md` §2。

本地**只验后端链路**时可以用官方镜像（1.24.x 实测可跑）—— `internal/gitsync/` 调的全是标准端点
（`admin/hooks`、`admin/users`、`orgs`、`repos`、`repositories/{id}`、`teams`、user tokens），
后端→Gitea 这一面确实不依赖 fork。

**但官方镜像验不到「用户浏览器 → Gitea」那一段**：V4 的核心 UX 是「Cloud 只展示，编辑跳 Gitea」（U3），
而用户在 Gitea 上**没有密码**，靠魔改版的 `CoStrictJWT` 才登得进去。官方镜像下用户点「去 Gitea 编辑」
会撞到登录页，且**后端一切正常，冒烟和监控都不会报**。所以：

- 只验后端（fork → webhook → sync → read-through）：官方镜像够用，但要清楚**没验用户登录**
- 验完整 UX / 任何要复现生产行为的场景：必须用魔改版

判别当前跑的是哪个：

```bash
strings <gitea二进制> | grep -c CoStrictJWT     # 0 = 官方版；非 0 = 魔改版
# 容器里：docker exec <gitea容器> sh -c 'strings /usr/local/bin/gitea | grep -c CoStrictJWT'
```

魔改版还要求 `app.ini` 里 `[costrict] ENABLED = true`（**默认 false**，`modules/setting/costrict.go:69`），
否则整个 fork 面（auth method + hook + 内部端点）全是 no-op —— 镜像对了但开关没开，表现与官方版完全一样。

`DEFAULT_BRANCH` **建议**保持 `main`（平台自建的能力仓库显式用 `main`，保持一致少一层认知负担），
但它**不是**同步链路失效的根因：webhook ingress 比的是同一个 payload 内部的
`ref` 与 `repository.default_branch`，两个都由 Gitea 按该仓库实际情况给出，读侧也全程用动态值。
详见 `V4_TROUBLESHOOTING.md` F13。
