# Capability Git Registry —— 前端到 csc 的 E2E 验收 runbook

> 与 `E2E_TESTING.md` 不同：那份是 Team Namespace API 的 Go 测试套件，
> **这份是人工执行的真实端到端**，链路为 multica 前端 → costrict-web → Gitea → webhook → worker → csc 落盘。
> 单测证明不了这条链路——历史上多次"测试全绿但链路是断的"。

## 0. 为什么必须人工跑

已经发生过三次「测试全绿 / 验收假绿」：

1. **S0**：用 discovery 路径的样本验 fork 的哈希 —— fork 行从诞生就带 git 坐标，
   走的是 reconcile 分支，**根本碰不到被改的那段代码**。
2. **AC10**：只比对 4 个预期文件的 SHA，**没断言"清单里只有这 4 个"** ⇒
   漏掉了泄漏进来的 auto-init README。
3. **csc typecheck**：`bun test` 131 个测试全绿，而 `tsc` 有 7 个错 —— bun 不做类型检查。

⇒ **判据必须同时覆盖「该出现的」和「不该出现的」**，且必须按真实入口设计，不能按被改的代码设计。

---

## 1. 环境前置（漏一条就会得到误导性结果）

| 服务 | 端口 | 起法 |
|---|---|---|
| Gitea | 3001 | 裸跑进程，非容器：`~/.local/share/costrict-gitea/bin/gitea web --config <...>/app.ini` |
| costrict-web api | 8080 | 见下方导出要求 |
| worker | — | discovery 跑在这里 |
| cs-user | 8082 | `cd cs-user && go run ./cmd/api`；**没有它登录接口全 503** |
| Postgres / Casdoor | 5432 / 8000 | docker |
| csc shim | 8099 | 剥 `/cloud-api` 前缀转发 8080 |

### 1.1 必须真实导出环境变量

```bash
cd server && set -a && source .env && set +a && go run ./cmd/api
```

**只写进 `.env` 是无效的**：`config.Load()` 把 `.env` 喂给 viper，而 **viper 从不写 `os.Environ`**，
多处代码用裸 `os.Getenv`。受影响的至少有：

- `CS_BOT_TOKEN_KEY` —— 缺它 ⇒ `gitBackingWired()` 为 false ⇒ **fork 静默回落 DB**，且启动日志无任何提示
- `PLUGIN_GIT_MIRROR_OWNER` / `GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS`

**开跑前的冒烟**：fork 一次，确认返回 `contentBackend: "git"`。不是 git 就别往下跑，后面全是假结果。

### 1.2 重启服务的正确姿势

```bash
# ❌ 会连 cs-user 一起杀掉 —— 两者入口都是 ./cmd/api，二进制同名都叫 api
pkill -f "exe/api$"

# ✅ 按 PID
kill <pid>
# ✅ 判断存活（lsof -ti 会把出站连接也算成占用）
lsof -nP -iTCP:8080 -sTCP:LISTEN
```

**改了 discovery 行为必须重启 worker**，只重启 api 看到的是旧行为。

---

## 2. 固定样本集

| 样本 | 类型 | 形态 | 覆盖 |
|---|---|---|---|
| FIX-01 | skill | 单文件 | AC1–AC5 |
| FIX-02 | subagent | 单文件 | AC1–AC5 |
| FIX-03 | command | 单文件 | AC1–AC5 |
| FIX-04 | mcp | 多 entry | AC1–AC5，验 `entry_key` |
| FIX-05 | skill | **含 `assets/`** | AC10 |
| FIX-06 | skill | **DB-backed，不动** | AC6 回归对照 |

⚠️ FIX-06 的基线 SHA 要在改造**前**存档，否则 AC6 无从比对。

---

## 3. 逐条执行

### AC1 创建 / fork
前端 fork → 校验三处：Gitea `GET /repos/<owner>/<slug>` 返回 200；
DB 行 `content_backend='git'`、`source_repo_url` 非空、**`git_sha` 是 40 位十六进制**、
`git_sync_status='synced'`。

> `git_sha` 为空 + `status='error'` 是已知故障形态，历史上由 binding 认领共享 registry 引起（已修）。

⚠️ **`git_sha` 是异步填充的 —— 这是最容易造成假失败的一处。**
fork 返回 201 时 `git_sha` 通常还是空、`git_sync_status='pending'`，要等 worker 处理完 `fork:` 队列才落值。
S5 验收时抽样 30 个 fork 后**立即**查，27/30 显示空 sha + pending，形态与「binding 缺陷未修」的故障**完全一样**；
实际是 job 才 47 秒、队列在约 7 分钟内单调排空（3 → 17 → 23 → 27 → 30）。

**判断方法**：别看瞬时快照，看队列是否**单调下降**。
连续查两次仍无变化、且 worker 存活，才是真故障。
区分要点 —— `pending` 是排队中，`error` 才是失败；只有后者需要停下排查。

### AC2 正文回流
Gitea 改**正文**、**不动 frontmatter** → push → 详情页正文变、`gitLastSyncedAt` 前进、
**版本号不变**（新语义下的正确行为）。

### AC2b 版本变更
改 frontmatter 的 `version` → push → 详情页版本号跟着变。

### AC3 下发
订阅 → 跑 csc → **`sha256(落盘文件)` == `sha256(Gitea raw)`**，逐字节相等（含 frontmatter）。

```bash
HOME=<隔离HOME> COSTRICT_BASE_URL=http://127.0.0.1:8099 \
  node /Volumes/Work/Projects/zgsm/csc/dist/cli.js --print "hi"
# 收藏同步在进程启动时异步触发，等 60-80 秒再 kill
```

⚠️ 用**隔离 HOME**，别污染日常凭据。csc 真身是 `dist/cli.js`（31M），不是 `cli-node.js`。

### AC4 二次回流
再改一次 → csc 再同步 → SHA 再次相等。
**同时断言 DB 的 `content` 列对该行为空/未更新** —— 这才证明内容来自 Gitea 而非 DB。

> 这条曾经失败：csc 的更新判定只比 version 字符串，而 V4 语义是"改正文不改版本号"。
> 服务端已把 git 行的 version 投影成 `<version>+<git_sha[:7]>`。

### AC5b Gitea 不可达
**样本要选 DB `content` 列里有残值的那种** —— 用空 content 的行验"不回落"等于没验，
没东西可回落，不回落是自然发生的而非守卫生效。

停 Gitea → 详情 / download / **assets** 三条路径都应 502 `GIT_CONTENT_UNREACHABLE`，
响应里**不含**那段残值；列表仍 200（本就置空 content、零出站，是正确行为）。
重启后应**无需干预即自愈**。

### AC6 灰度回归
FIX-06 的详情 content、`/download` 字节、订阅、csc 落盘 SHA 与改造前基线**逐字节一致**。

### AC10 多文件
落盘后逐个比 SHA，**并断言清单长度恰好等于预期** —— 缺陷可能是"多了一个"而不是"少了一个"。

### AC19 plugin 更新闭环
已订阅且已安装的 git-backed plugin → Gitea 改 manifest（name + version）→ push →
Cloud 显示新版本 → **csc 下一轮 reconcile 后本地副本的 name/version 跟着变**。

**最有价值的是第二轮：只改内容、不 bump version。** 那才是 V4「改正文不改版本号」
与 csc 判据冲突的场景；只验第一轮（同时改了 version）会漏掉整个问题。
通过的标志是本地装的 version 是**短 SHA** 而不是 manifest 里的语义版本号。

> plugin 的安装落点与 skill 不同：状态在 `~/.costrict/plugins/installed_plugins.json`
> 加版本化缓存目录，**不在 `.claude.json`**（那里 `plugins` 为空是正常的）。
> 落点要用 `git remote -v` 证实，别靠日志文字。

⚠️ **两个会导致假通过的陷阱**

**1. `AGGREGATED_MARKETPLACE_SOURCE` 默认指向生产** `https://gitea.costrict.ai/…`。
用一个装过同名 plugin 的旧 HOME 去验，装的很可能是**公共镜像**那份，与本地 fork 毫无关系，
结果看起来通过、实则验的是线上。必须用干净 HOME 并显式指向本地：
```bash
export COSTRICT_PLUGIN_MARKETPLACE_URL=http://127.0.0.1:8099/cloud-api/api/marketplace/costrict-plugins/marketplace.json
```

**2. worker 按 ~10 分钟 tick 排空 `git_capability_sync_jobs`，不是按需触发。**
实测有 job 从 pending 到开始等了 2 分 27 秒；另一轮恰好撞上 tick 只等了 5 秒、看起来像实时。
**验收脚本里不要写秒级等待**，否则会把"还没轮到"误判成"链路断了"。

---

## 4. 收尾

- 截图证据统一落 `.playwright-mcp/`
- 结论写进 `.trellis/tasks/08-04-s7-e2e-acceptance/research/`，**未执行的条目要显式写"未执行"**，
  不要留空让人误以为通过
- 清理造出来的测试样本（Gitea 仓库 + DB 行），否则下轮的计数基线对不上
