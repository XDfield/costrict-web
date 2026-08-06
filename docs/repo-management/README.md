# docs/repo-management 导航

这个目录里堆了两代设计（V3 / V4）加一批周边契约，光看文件名分不出该读哪份。
**先看这一页，再进具体文档。**

---

## 我想做 X，该读哪份

| 我想…… | 读这份 |
|---|---|
| 搞清楚 V4 到底是什么、为什么这么设计 | [`CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md`](./CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md) |
| **把 V4 发到生产上** | [`V4_PRODUCTION_ROLLOUT.md`](./V4_PRODUCTION_ROLLOUT.md) |
| **线上出问题了，查原因** | [`V4_TROUBLESHOOTING.md`](./V4_TROUBLESHOOTING.md) |
| **日常查状态 / 手工重放 webhook / 触发 resync / 改 git_servers** | [`V4_OPERATIONS.md`](./V4_OPERATIONS.md) |
| 决定存量什么时候迁、怎么分批迁 | [`GRAYSCALE_MIGRATION_PLAN.md`](./GRAYSCALE_MIGRATION_PLAN.md) |
| 人工跑一遍端到端验收 | [`CAPABILITY_GIT_E2E_RUNBOOK.md`](./CAPABILITY_GIT_E2E_RUNBOOK.md) |
| 把 plugin 上游镜像批量导进自建 Gitea | [`BUNDLE_TO_GITEA_IMPORT.md`](./BUNDLE_TO_GITEA_IMPORT.md) |
| 知道现在实现到哪一步了 | [`CAPABILITY_GIT_REGISTRY_IMPLEMENTATION_HANDOFF.md`](./CAPABILITY_GIT_REGISTRY_IMPLEMENTATION_HANDOFF.md) |
| 查仓库命名 / 目录布局 / manifest 规范 | [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) |
| 接 team namespace 的内部接口 | [`TEAM_NAMESPACE_API_REFERENCE.md`](./TEAM_NAMESPACE_API_REFERENCE.md) |
| 弄清 `git_servers` 归谁管、cs-user 和 server 怎么分工 | [`GIT_OWNERSHIP_REFACTOR_PROPOSAL.md`](./GIT_OWNERSHIP_REFACTOR_PROPOSAL.md) |

---

## V4 主线（当前有效，按使用顺序）

```
        设计                  上线                 日常
  PROPOSAL_V4 ──┬──► V4_PRODUCTION_ROLLOUT ──┬──► V4_OPERATIONS
                │            │                │
                │            ▼                ▼
                │   CAPABILITY_GIT_E2E   V4_TROUBLESHOOTING
                │       _RUNBOOK
                │
                └──► GRAYSCALE_MIGRATION_PLAN ──► BUNDLE_TO_GITEA_IMPORT
                        （存量四类）                 （plugin mirror）
```

| 文档 | 回答什么问题 | 谁维护 |
|---|---|---|
| **`CAPABILITY_GIT_REGISTRY_PROPOSAL_V4.md`** | 终态是什么样、为什么这么定 | ⚠️ **用户自己维护，其他人不要改** |
| **`V4_PRODUCTION_ROLLOUT.md`** | 从零到「V4 可用」的操作序列、migration 清单、冒烟判据 | 运维/发布负责人 |
| **`V4_TROUBLESHOOTING.md`** | 症状 → 原因 → 确认方法 → 修法。含错误码全表与诊断 SQL | 值班 |
| **`V4_OPERATIONS.md`** | webhook 重放（含 HMAC）、单仓 resync、队列管理、`git_servers` 改法、环境变量全表 | 运维 |
| **`GRAYSCALE_MIGRATION_PLAN.md`** | 两套怎么共存、存量按什么节奏迁、灰度什么时候算结束 | 产品 + 运维 |
| **`CAPABILITY_GIT_E2E_RUNBOOK.md`** | 人工怎么跑 E2E、哪些地方会假绿。**当前只写了 AC1 / 2 / 2b / 3 / 4 / 5b / 6 / 10 / 19**，不是 AC1–AC19 全覆盖 | 验收执行人 |
| **`BUNDLE_TO_GITEA_IMPORT.md`** | plugin mirror 批量导入脚本用法、导入前必配的 discovery 排除 | 执行导入的人 |
| `CAPABILITY_GIT_REGISTRY_IMPLEMENTATION_HANDOFF.md` | 当前实现检查点（已完成 / 已推迟） | 实现者 |

### 四份运行时文档的分工（别写重）

- **ROLLOUT** = 「第一次上线」的一次性序列。写**顺序**和**门禁**。
- **OPERATIONS** = 上线之后反复做的动作。写**怎么操作**。
- **TROUBLESHOOTING** = 出了问题才翻。写**症状 → 修法**。
- **E2E_RUNBOOK** = 证明链路是通的。写**判据**，尤其是「怎样才不算假绿」。

---

## 记住这几条，能省掉大半排查

1. **稳定身份四元组** = `git_server_id` + `git_repo_id` + `manifest_path`(`source_repo_path`) + `entry_key`。
   owner/name 改名不影响它，数字 repo id 才是身份。
2. **git 行的 `content` 列不再被写入、也不再被读**，内容每次请求实时 read-through 自 Gitea，**不缓存**。
   Gitea 不可达就报错（fail-closed），**不回落 DB 旧值**。
   ⚠️ 但列里**可能还有残值**（只有 migrate 与 fork 会主动清空；discovery 期的行不会）——
   本地实测 538 条 git 行里 14 条 `content` 非空。它不是内容来源，是清理对象。
3. **版本锚点是 `git_sha`**；对外的 version 投影成 `<version>+<git_sha[:7]>`。
   因为 csc 判断更新只比 version 字符串，而 V4 语义是「改正文不改版本号」。
   ⇒ SQL 里的 version 和 API 返回的 version 不一样，是对的。
4. **写者隔离 = GORM hook 默认拒绝 + 显式放行标记**（`models.GitSyncBypassSetting`）。
   **三个盲区**：`tx.Exec` 裸 SQL、`tx.Table()`、`UpdateColumn(s)`/`SkipHooks`。
   （`db.Save(&[]T{})` 传 slice 曾经是第四个，现已由 `BeforeCreate` 堵上。）
5. **discovery 跑在 worker 里**。只重启 api 看不到任何同步行为的变化。
6. **`CS_BOT_TOKEN_KEY` 等一批变量不走 viper**（调用点是裸 `os.Getenv`），所以本地 `go run` 时
   只写 `.env` 无效；**但容器里挂 `/app/.env` 是有效的** —— entrypoint 会 source + export。
   缺它 ⇒ fork **静默**回落 DB；启动时有一行 stdout 日志可 grep（`CS_BOT_TOKEN_KEY not configured`），
   但它不进 `app.log`，**真 fork 一次才是可靠判据**。详见 `V4_TROUBLESHOOTING.md` F1。
7. **验证 git backing 必须换一个没 fork 过的 (用户, 源 item) 组合** —— fork 有一次性短路，
   第二次直接返回旧行（200），会让人误判「配全了还是不生效」。
   且**冒烟必须排在 bootstrap 之后**（`tenant_git_server_binding` 没建时结果恒为 `db`）。
8. **重启服务只能按 PID 杀**：`pkill -f "exe/api$"` 会连 cs-user 一起杀（两个二进制都叫 `api`）。
   存活判断用 `lsof -nP -iTCP:<port> -sTCP:LISTEN`（`lsof -ti` 会把出站连接也算成占用）。

---

## 周边契约（V4 之外，但同一套 Gitea 底座）

| 文档 | 内容 |
|---|---|
| [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) | 仓库管理总规范（最大的一份，v2.18） |
| [`TEAM_NAMESPACE_API.md`](./TEAM_NAMESPACE_API.md) / [`TEAM_NAMESPACE_API_REFERENCE.md`](./TEAM_NAMESPACE_API_REFERENCE.md) | team ns 内部接口：设计 / 实现级参考 |
| [`KB_USER_ENSURE_API.md`](./KB_USER_ENSURE_API.md) | `POST /api/kb/ensure` 用户侧契约 |
| [`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) · [`WORKFLOW_REPO_PATH_ALGORITHM.md`](./WORKFLOW_REPO_PATH_ALGORITHM.md) | KB / workflow 的仓库路径推断（v2.0） |
| [`CSC_KB_SUBCOMMAND_CONTRACT.md`](./CSC_KB_SUBCOMMAND_CONTRACT.md) · [`CSC_WF_SUBCOMMAND_CONTRACT.md`](./CSC_WF_SUBCOMMAND_CONTRACT.md) | csc 侧子命令契约（v2.0） |
| [`GIT_OWNERSHIP_REFACTOR_PROPOSAL.md`](./GIT_OWNERSHIP_REFACTOR_PROPOSAL.md) | `git_servers` 从 cs-user 迁回 server（Phase 1–5） |

---

## 历史文档（**别当现状读**）

| 文档 | 状态 |
|---|---|
| [`CAPABILITY_GIT_REGISTRY_PROPOSAL.md`](./CAPABILITY_GIT_REGISTRY_PROPOSAL.md) | **V3**，已被 V4 取代。看历史演进用 |
| [`CAPABILITY_GIT_REGISTRY_ROADMAP.md`](./CAPABILITY_GIT_REGISTRY_ROADMAP.md) | V3 时代的 8-Phase 路线图，**基线是 V3 提案**。V4 的实施节奏以 ROLLOUT + GRAYSCALE 为准 |
| [`CAPABILITY_PORTAL_DECISION.md`](./CAPABILITY_PORTAL_DECISION.md) | V3 时代的 portal 部署形态选型（2026-07-07 锁定） |
| [`E2E_TESTING.md`](./E2E_TESTING.md) | ⚠️ 文档自己声明「部分过期」（Git Ownership Refactor Phase 4 之前的架构）。**且它是 Team Namespace API 的 Go 测试套件，不是 V4 的端到端** —— 后者看 `CAPABILITY_GIT_E2E_RUNBOOK.md` |
