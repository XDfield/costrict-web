# csc wf 子命令契约（轻量）

| 版本 | v2.0 |
|---|---|
| 创建日期 | 2026-07-14 |
| 最近更新 | 2026-07-15（v2.0：类型 repo + 实例 branch 模型 + 编排器代调内部接口 + 节点 PR base = `inst-<short>` + 新增 def update） |
| 依据 | [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) v2.17 §17 / §18 |
| 关联算法 spec | [`WORKFLOW_REPO_PATH_ALGORITHM.md`](./WORKFLOW_REPO_PATH_ALGORITHM.md) v2.0 |
| 关联接口 spec | [`TEAM_NAMESPACE_API.md`](./TEAM_NAMESPACE_API.md) v2.0 §5 |

> 本文定义 `csc wf` 子命令集的输入输出契约。csc 是 **thin client**——不内置路径算法、不维护本地状态、不编排节点 PR 流程（PR 自动 merge 由 Gitea CI / auto-approve bot 完成）、**不持 service token**（由编排器代调内部接口）。

---

## 0. 版本演进说明

| 版本 | 日期 | 关键差异 |
|---|---|---|
| v1.0 | 2026-07-14 | 11 个子命令；workflow = 每实例一 repo（`costrict-workflow/<def>__<inst>`）；csc 直接调 `POST /api/workflow/init`（用户 JWT）；节点 PR base = `main` |
| **v2.0** | 2026-07-15 | **架构反转**：①workflow 模型改 **类型 repo + 实例 branch**（`t-<team_short>/wf-<def>` + `inst-<short>`）；②接口改 `POST /api/internal/workflow/init`（**内部接口**，csc 经编排器代调）；③**节点 PR base 从 main 改为 `inst-<short>`**（main 仅放 def canonical）；④**移除 authorize / revoke / transfer-owner**——成员管理统一走 team 级 `members:sync`；⑤**新增 `csc wf def update`**——main 上的 workflow def PR（团队级 def 演进）；⑥`team_id` 成为必传参数；⑦init 响应含 `instance_branch` 字段 |

---

## 1. 总则

1. **统一前置 init**：除 `csc wf list` / `csc wf pr` 外，所有命令必须先调 `POST /api/internal/workflow/init`（**经编排器代调**）拿到 `wf_repo_path` + `instance_branch`
2. **team_id 必传**：所有 init 调用必须传 `team_id`（来自 csc config / 环境变量 / `.costrict/wf.yaml`）；csc 不反查 team 归属
3. **path / branch 来源唯一**：`wf_repo_path` + `instance_branch` 只来自 init 响应，**csc 不内置算法副本**
4. **PAT 使用**：所有 git 操作（clone / push / pull）使用调用者本人 fine-grained PAT（用户必须是 team ns org 成员才有 write 权限）；csc 不持 service token
5. **remote URL 来源**：clone / push / pull 的 remote URL **直接使用 init 响应的 `wf_clone_url` 字段**（server 已拼接完整 `<tenant_gitea_base_url>/<wf_repo_path>.git`）；csc **禁止**自行拼接 `gitea_base_url + wf_repo_path`。Gitea REST API 调用（PR / review / merge / archive）仍用 `<gitea_base_url>/api/v1/...`，其中 `gitea_base_url` 优先取 `csc login` 响应下发值，本地配置仅 fallback（详见 §6 配置）
6. **instance_id 来源**：由平台 workflow 编排器（独立服务）在实例启动时分配，csc 不生成；命令行 / 环境变量 / 配置文件三选一传入
7. **分支命名约定**：
   - `main`：workflow def canonical 存储（仅 `definition.yaml`）；**不直接 push 节点交付物**
   - `inst-<inst_short>`：实例完整时间线（来自 init 响应）；节点 PR base = 此分支
   - `node/<seq>-<slug>`：节点 feat 分支，base = `inst-<short>`

---

## 2. 输入参数来源

### 2.1 命令行显式

```bash
csc wf init bug-fix-flow f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab \
  --team-id=7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a \
  --definition-snapshot=./def.yaml
```

### 2.2 环境变量

```bash
export CSC_WF_DEF_SLUG=bug-fix-flow
export CSC_WF_INSTANCE_ID=f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab
export CSC_TEAM_ID=7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a
csc wf node push 001
```

### 2.3 配置文件（实例运行期推荐）

```bash
# 当前工作目录下 .costrict/wf.yaml
def_slug: bug-fix-flow
instance_id: f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab
team_id: 7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a
```

由平台编排器在实例启动时写入，节点执行器（agent / 用户）进入工作目录即可使用。

---

## 3. team_id 来源

csc 按以下顺序解析 `team_id`（与 [`CSC_KB_SUBCOMMAND_CONTRACT.md`](./CSC_KB_SUBCOMMAND_CONTRACT.md) §3 一致）：

| 优先级 | 来源 | 例 |
|---|---|---|
| 1 | 命令行参数 `--team-id` | `csc wf init ... --team-id=7f3c9a1e-...` |
| 2 | 环境变量 `CSC_TEAM_ID` | `export CSC_TEAM_ID=7f3c9a1e-...` |
| 3 | 本地代码 repo 内 `.costrict/wf.yaml` `team_id` | `team_id: 7f3c9a1e-...` |
| 4 | csc 全局配置 `~/.costrict/config.yaml` `wf.default_team_id` | `wf:\n  default_team_id: 7f3c9a1e-...` |

> **缺省拒绝执行**：以上 4 个来源都未命中时，csc 打印 `team_id is required (pass --team-id, set CSC_TEAM_ID, or configure .costrict/wf.yaml)` 并 exit 2。

---

## 4. 命令清单

### 4.1 `csc wf init`

首次初始化 workflow 类型 repo + 实例 branch（由平台编排器在实例启动时代调）。

**用法**：
```bash
csc wf init <def_slug> <instance_id>
  --team-id=<UUID>                            # 必填
  --definition-snapshot=<path-to-yaml>        # 类型 repo 首次创建时必填；已存在时用于 drift 校验
  [--force]                                   # 可选，跳过交互式确认
```

**流程**：
```
1. 校验 def_slug 非空、instance_id / team_id 合法 UUID
2. 读取 definition-snapshot 文件内容
3. 经编排器代调 POST /api/internal/workflow/init
   Header: X-Internal-Service-Token（编排器持有）
   Body: {
     workflow_def_slug,
     instance_id,
     team_id,
     definition_snapshot: <yaml 内容>
   }
4. switch response:
   - team_ns_exists=true, created.type_repo=true:
     print "✓ Type repo created: <wf_repo_path>"
     print "  wf_clone_url: <wf_clone_url>"
     print "  Definition written to main (sha=abc123)"
     print "  Instance branch: <instance_branch>"
     exit 0
   - team_ns_exists=true, created.type_repo=false, created.instance_branch=true:
     print "✓ Instance branch created: <instance_branch> (base=main HEAD)"
     exit 0
   - team_ns_exists=true, created=false/false（幂等重入）:
     print "✓ Instance branch already exists: <instance_branch>"
     exit 0
   - team_ns_exists=false:
     print "✗ Team namespace not initialized"
     print "  Hint: Contact team admin to run members:sync."
     exit 1
```

**输出**（首次创建类型 repo + 实例 branch）：
```
✓ Type repo initialized: t-7f3c9a1e/wf-bug-fix-flow
  Team:            7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a
  Definition:      snapshot written to main (sha=abc123)
  Instance branch: inst-f3a8b2c1 (base=main HEAD)
  Algorithm:       v2
```

**错误**（定义漂移）：
```
✗ Definition drift detected
  Existing:  sha=def456 (on main of t-7f3c9a1e/wf-bug-fix-flow)
  Incoming:  sha=abc123
  Hint:      Open PR on main to update def first: csc wf def update
exit 7
```

**错误**（实例 branch 碰撞）：
```
✗ Instance branch conflict
  Branch inst-f3a8b2c1 already exists in t-7f3c9a1e/wf-bug-fix-flow
  Hint:      Regenerate instance_id and retry.
exit 8
```

### 4.2 `csc wf node push <seq>`

推送指定节点的交付物到 feat 分支 + 开 PR。

**用法**：
```bash
csc wf node push <seq>                       # 例：001 / 002 / 003-deliver
  [--message="node(<seq>): <subject>"]
  [--input-json=<path>]                      # 节点输入快照（含上游 commit SHA）
```

**流程**：
```
1. 经编排器代调 POST /api/internal/workflow/init（幂等；def_slug + instance_id + team_id 从配置文件 / 环境变量）
   → { wf_repo_path, wf_clone_url, wf_web_url, instance_branch, ... }
2. switch team_ns_exists:
   - true:
     a. 解析 seq → 拼接节点分支名 node/<seq>
     b. git fetch origin instance_branch
     c. git checkout -b node/<seq> origin/<instance_branch>   # ⚠ base = instance_branch（不是 main）
     d. 写 nodes/<seq>/output.md + artifacts/（用户 / agent 已在工作区准备好）
     e. 写 nodes/<seq>/input.json（如有 --input-json）
     f. git add nodes/<seq>/ && git commit -m "node(<seq>): <subject>"
     g. git push origin node/<seq>
     h. POST <gitea_base_url>/api/v1/repos/<wf_repo_path>/pulls
        body: {
          title: "[node-<seq>] <node_name>",
          body: 节点 input/output 摘要 + 上游 commit SHA,
          base: "<instance_branch>",                          # ⚠ base = inst-<short>
          head: "node/<seq>"
        }
     i. server 异步追加 .workflow/node-prs.json: {"<seq>": <pr_number>}
     j. 按 audit_level 决定后续:
        - "strict":       打印 "PR opened: #<n>. Waiting for reviewer approval."
        - "auto":         等待 CI 通过后 auto-approve bot 自动 merge（csc 不参与）
        - "experimental": 仅 push 到分支，不开 PR
     exit 0
   - false:
     print "✗ Team namespace not initialized. Contact team admin."
     exit 1
```

**输出**（成功）：
```
✓ Node 001 pushed to branch node/001-collect
  Commit:       abc1234 (2026-07-15 11:00)
  Base:         inst-f3a8b2c1  ← NOT main
  PR:           #12 opened (audit_level=auto, will auto-merge after CI)
  Files:        nodes/001-collect/output.md (+ 2 artifacts)
```

> **关键差异（v1.0 → v2.0）**：节点 PR base 从 `main` 改为 `instance_branch`——main 永远只放 def canonical，实例时间线独立。

### 4.3 `csc wf node list`

列出当前实例所有节点 + PR 状态。

**用法**：
```bash
csc wf node list [--state=all|pending|merged|closed]
```

**流程**：
```
1. 经编排器代调 POST /api/internal/workflow/init（拿到 wf_repo_path + wf_clone_url + instance_branch）
2. git fetch origin instance_branch
3. 读 .workflow/node-prs.json（从 instance_branch HEAD）
4. 对每个 PR 调 Gitea API 查状态
5. 输出表格
```

**输出**：
```
NODE                  PR    STATE      AUDIT     COMMIT     PUSHED AT
001-collect           #12   merged     auto      abc1234    2026-07-15 11:00
002-process           #13   pending    auto      def5678    2026-07-15 11:30
003-deliver           -     -          strict    -          (未推送)

Base branch: inst-f3a8b2c1
```

### 4.4 `csc wf node approve <pr-number>`

reviewer 审计节点 PR（仅 audit_level=strict 时需要）。

**用法**：
```bash
csc wf node approve <pr-number>
  [--comment="..."]
```

**流程**：
```
1. 经编排器代调 POST /api/internal/workflow/init（确认 team ns 存在）
2. POST <gitea_base_url>/api/v1/repos/<wf_repo_path>/pulls/<pr-number>/reviews
   body: { event: "APPROVED", body: "<comment>" }
3. print "✓ Approved PR #<n>"
```

### 4.5 `csc wf node merge <pr-number>`

合并节点 PR（强制 merge commit 策略，merge 入 `instance_branch`）。

**用法**：
```bash
csc wf node merge <pr-number> [--strategy=merge]
```

**流程**：
```
1. 经编排器代调 POST /api/internal/workflow/init
2. 校验 audit_level：
   - "strict":       必须已有 approve review，否则拒绝
   - "auto":         通常由 auto-approve bot 自动调；csc 手动调用作为兜底
   - "experimental": 拒绝（experimental 不开 PR）
3. POST <gitea_base_url>/api/v1/repos/<wf_repo_path>/pulls/<pr-number>/merge
   body: { Do: "merge", MergeTitleField: "node(<seq>): merge ...", MergeMessageField: "..." }
4. print "✓ Merged PR #<n> into <instance_branch> (commit <merge_sha>)"
```

**约束**：
- **强制 merge commit**（`Do: "merge"`），禁 squash / rebase——保留节点 commit SHA 在实例时间线
- audit_level=experimental 不允许 merge

### 4.6 `csc wf def update`

**v2.0 新增**。在 workflow 类型 repo 的 main 分支开 PR 更新 workflow def（团队级 def 演进）。

**用法**：
```bash
csc wf def update --def-file=<path-to-yaml>
  [--title="def: <change summary>"]
  [--body="..."]
```

**流程**：
```
1. 取 def_slug + team_id（§2 / §3 优先级链；不需要 instance_id）
2. git clone <wf_clone_url> -b main /tmp/wf-def-<random>   # wf_clone_url 来自 init 响应（如本地无缓存则先代调 init）
3. cp <def-file> /tmp/wf-def-<random>/definition.yaml
4. git add definition.yaml && git commit -m "def: <change summary>"
5. git push origin main:refs/heads/def-update-<short>   # 注意 push 到 feat branch 而非直接 main
6. POST <gitea_base_url>/api/v1/repos/<wf_repo_path>/pulls
   body: {
     title: "[def-update] <change summary>",
     body: <body or default>,
     base: "main",
     head: "def-update-<short>"
   }
7. print "✓ Def update PR opened: #<n>"
8. 提示用户：def merge 入 main 后，新实例 init 会使用新 def；既有实例不受影响（实例 base = main HEAD 在 init 时已固定）
```

**输出**（成功）：
```
✓ Def update PR opened: #15
  Repo:  t-7f3c9a1e/wf-bug-fix-flow
  Base:  main
  Head:  def-update-a1b2c3d4
  File:  definition.yaml (+12 -3 lines)

After PR merged into main, new instances will use the updated def.
Existing instances (inst-*) are not affected.
```

> **关键场景**：v2.0 起 def canonical 存储在 main，团队对 def 的修改走 main 上的 PR 协作，由 reviewer 审计后 merge；不再像 v1.0 那样把 def 嵌入每个实例 repo 的 `.workflow/definition.snapshot.yaml`。

### 4.7 `csc wf list`

列出调用者参与的所有 workflow 类型 repo（基于 team ns org 成员关系）。

**用法**：
```bash
csc wf list [--limit=20] [--offset=0] [--team-id=UUID] [--def=<slug>]
```

**流程**：
```
1. 取 team_id（§3 优先级链）
2. 调 Gitea API（用调用者 PAT）:
   GET /orgs/t-<team_short>/repos?limit=...&offset=...
   Authorization: Bearer <用户 PAT>
3. 过滤 name 以 "wf-" 开头的 repo
4. 如有 --def=<slug>：按 def_slug 过滤
5. 对每个类型 repo，可选用 --show-instances 列出 inst-* branch 数量
6. 输出表格
```

**输出**：
```
WF REPO                                  INSTANCES  LAST ACTIVITY
t-7f3c9a1e/wf-bug-fix-flow               3          2026-07-15 11:30
t-7f3c9a1e/wf-release-pipeline           1          2026-07-13 16:00
t-9b8c7d6e/wf-compliance-check           5          2026-07-10 09:00
```

> `csc wf list` **不调用 init**——避免无端为列出的每个 repo 触发副作用；直接调 Gitea org repo list API（按 JWT + org membership 过滤）。

### 4.8 `csc wf archive`

归档实例 branch（实例结束后触发）。

**用法**：
```bash
csc wf archive [--instance-id=UUID] [--reason="..."]
```

**流程**：
```
1. 经编排器代调 POST /api/internal/workflow/init（确认 team ns 存在 + 实例 branch 存在）
2. 调 Gitea API 删除实例 branch（保留 commit 历史：通过 tag 标记 archived）
   POST <gitea_base_url>/api/v1/repos/<wf_repo_path>/tags
     body: { tag_name: "archived-<instance_branch>", message: "Instance archived: <reason>" }
   DELETE <gitea_base_url>/api/v1/repos/<wf_repo_path>/branches/<instance_branch>
3. 更新 .workflow/node-prs.json（在 main 上写 metadata：实例 archived 标记）
4. print "✓ Archived instance <instance_branch>"
```

> **重要**：v2.0 不再"归档整个 repo"——类型 repo 永久存在；实例生命周期 = branch 生命周期。实例归档通过 **tag 保留 commit + 删除 branch** 实现（commit 通过 tag 仍可访问）。

**输出**（成功）：
```
✓ Archived instance inst-f3a8b2c1
  Repo:      t-7f3c9a1e/wf-bug-fix-flow
  Tag:       archived-inst-f3a8b2c1 (commit history preserved)
  Branch:    deleted
  Reason:    workflow completed
```

### 4.9 `csc wf pr <action>`

Gitea PR API 透传（高级用法，正常情况下用 `csc wf node push / approve / merge` 或 `csc wf def update`）。

**用法**：
```bash
csc wf pr open   --title="..." --body="..." [--base=<branch>] [--head=<branch>]
csc wf pr list   [--state=open|closed|all]
csc wf pr merge  <pr-number> [--strategy=merge]    # 强制 merge commit
csc wf pr close  <pr-number>
```

**说明**：
- 子命令直接调 Gitea PR API，**不强制 init**（但建议先 `csc wf node list` 确认 path）
- merge 默认 `--strategy=merge`（强制 merge commit，禁 squash / rebase）
- `--base` 可指定为 `main`（def PR）或 `inst-<short>`（node PR）；默认从上下文推断
- 详见 Gitea PR API 文档

---

## 5. 已移除的命令（v1.0 → v2.0）

| 命令 | v1.0 行为 | v2.0 替代 |
|---|---|---|
| `csc wf authorize <username>` | 把指定用户加为当前 wf repo 的 collaborator write | **移除**——成员管理统一走 team ns `members:sync`（[TEAM_NAMESPACE_API](./TEAM_NAMESPACE_API.md) §2） |
| `csc wf revoke <username>` | 移除指定用户的 collaborator | **移除**——同上 |
| `csc wf transfer-owner <username>` | 转让 wf repo ownership | **移除**——v2.0 起 wf repo owner = `t-<team_short>` org（不可转让）；ownership 转让无意义 |

**设计理由**：v2.0 的权限模型从「per-repo collaborator」改为「team ns org 成员关系」，所有成员管理统一收口到 `members:sync`，避免命令族膨胀与权限碎片化。

---

## 6. 通用错误处理

### 6.1 init 接口错误（编排器代调透传）

| HTTP | error_code | csc 行为 | exit code |
|---|---|---|---|
| 400 | `INVALID_REQUEST` | `✗ Invalid request: <detail>` | 2 |
| 401 | `UNAUTHORIZED_SERVICE` | `✗ Orchestrator service token rejected. Contact admin.` | 3 |
| 412 | `TEAM_NS_NOT_INITIALIZED` | `✗ Team namespace not initialized. Contact team admin to run members:sync.` | 1 |
| 409 | `DEFINITION_DRIFT` | `✗ Definition drift detected (existing=<hash_existing> incoming=<hash_incoming>). Open PR on main via 'csc wf def update' first.` | 7 |
| 409 | `INSTANCE_BRANCH_CONFLICT` | `✗ Instance branch conflict. Regenerate instance_id and retry.` | 8 |
| 500 | `GITEA_API_FAILURE` | `✗ Server error: <detail>. Retry later or contact admin.` | 4 |
| 网络超时 | — | 自动重试 2 次（指数退避 1s / 3s），仍失败 → exit 4 | 4 |

> **不再有 403 tenant mismatch / 401 not authenticated**——v2.0 接口走 service token（编排器代调），用户侧 JWT / tenant 校验由编排器在调用前完成。

### 6.2 git 操作错误

| 错误 | csc 行为 |
|---|---|
| push 被 branch protection 拒绝（force push） | `✗ Force push is disabled on main / inst-* / node/* branches. Use 'git revert' instead.` → exit 5 |
| 节点分支已存在 | 提示用户切到该分支后追加 commit，或加 `--new-branch=<suffix>` 新建 |
| merge 出现冲突 | 不自动 resolve，提示用户 `git status` + 手动 fix → push 同一分支（PR 自动更新） |
| 实例已 archived（branch 已删）后尝试 push | `✗ Instance branch archived. Push not allowed.` → exit 5 |

### 6.3 PAT 权限不足

| 错误 | csc 行为 |
|---|---|
| git push 401 | `✗ PAT missing or expired. Run: csc auth refresh` |
| git push 403 | `✗ You are not a member of team <team_short>. Ask team admin to add you via members:sync.` |
| Gitea API 403（approve / merge） | `✗ PAT scope insufficient or audit_level=strict requires approve review first.` |
| audit_level=strict 时 merge 无 approve | `✗ Cannot merge: audit_level=strict requires approve review first.` → exit 5 |

### 6.4 audit_level 校验错误

| 错误 | csc 行为 |
|---|---|
| experimental 节点尝试 push PR | 自动跳过 PR 创建，仅 push 到分支 |
| experimental 节点尝试 merge | `✗ Cannot merge experimental node. Use --force-merge-experimental to override (admin only).` → exit 5 |

---

## 7. 配置文件依赖

`~/.costrict/config.yaml`（csc 全局配置）字段：

```yaml
wf:
  # gitea_base_url: 已弃用（deprecated）作为 git remote URL 源——init 响应直接返回 wf_clone_url。
  #                 仅保留作为 Gitea REST API（PR/review/merge/archive）调用的 base，
  #                 且优先从 csc login 响应获取，本地配置仅 fallback。
  gitea_base_url: https://gitea.costrict.local
  orchestrator_endpoint: https://orchestrator.costrict.local  # 编排器 base_url（用于代调内部接口）
  default_node_branch_prefix: "node/"             # 节点分支前缀
  default_def_branch_prefix: "def-update-"        # def update 分支前缀
  auto_merge_poll_interval: 5                     # audit_level=auto 时 csc 轮询 PR merge 状态间隔（秒），仅用于 UX 反馈
  default_team_id: 7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a  # 可选；缺省 fallback
```

实例级配置（`.costrict/wf.yaml`）：

```yaml
# 平台编排器在实例启动时写入实例工作目录
def_slug: bug-fix-flow
instance_id: f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab
team_id: 7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a
# 可选：init 响应缓存（避免重复代调）
wf_repo_path: t-7f3c9a1e/wf-bug-fix-flow
wf_clone_url: https://gitea.costrict.local/t-7f3c9a1e/wf-bug-fix-flow.git
instance_branch: inst-f3a8b2c1
```

> `gitea_base_url` 与 `orchestrator_endpoint` 优先从 `csc login` 响应中获取，本地配置仅作 fallback。

---

## 8. 退出码约定

| 退出码 | 含义 |
|---|---|
| 0 | 成功 |
| 1 | team ns 未初始化（需联系 team admin 调 members:sync） |
| 2 | 输入错误（def_slug / instance_id / team_id 不合法、参数缺失） |
| 3 | 编排器鉴权失败（service token 不一致） |
| 4 | 服务器错误（5xx / 网络超时） |
| 5 | git 操作错误（branch protection / 冲突 / archived / audit_level 阻塞） |
| 6 | PAT 权限不足（用户不是 team ns org 成员） |
| 7 | 定义漂移（409 `DEFINITION_DRIFT`，需先 `csc wf def update`） |
| 8 | 实例 branch 碰撞（409 `INSTANCE_BRANCH_CONFLICT`，需重新分配 instance_id） |

---

## 9. 与其他子命令的关系

| 命令族 | 与 wf 的关系 |
|---|---|
| `csc capability *` | **完全独立**——capability 是 §11 wizard 流程，wf 是 §17 业务数据流 |
| `csc kb *`（[KB 契约](./CSC_KB_SUBCOMMAND_CONTRACT.md)） | **共享 team_id 概念**（同一 team 下既有 KB 又有 workflow）；编排器代调接口共用；namespace / 子命令集分离 |
| `csc auth *` | 共享认证（登录、PAT 管理）；wf 命令复用同一 PAT |
| `csc config *` | 共享全局配置；`wf.*` 字段独立 |
| `csc plugin *` | 完全独立（plugin 是 capability item type） |

---

## 10. 完整示例

### 10.1 首次启动 workflow 实例

```bash
# 平台编排器在实例启动时执行（或 csc 用户手动）
$ export CSC_TEAM_ID=7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a
$ csc wf init bug-fix-flow f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab \
    --definition-snapshot=./def.yaml

→ Resolving team_id from CSC_TEAM_ID... 7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a
→ Calling workflow/init via orchestrator...
  ✓ team ns exists: t-7f3c9a1e
  ✓ type repo: t-7f3c9a1e/wf-bug-fix-flow (created: true)
  ✓ definition written to main (sha=abc123)
  ✓ instance branch: inst-f3a8b2c1 (created: true)

✓ Workflow type repo initialized
  Type repo:       t-7f3c9a1e/wf-bug-fix-flow
  Instance branch: inst-f3a8b2c1
  Algorithm:       v2
```

### 10.2 推送节点交付物

```bash
# 节点执行器在实例工作目录
$ cat .costrict/wf.yaml
def_slug: bug-fix-flow
instance_id: f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab
team_id: 7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a
wf_repo_path: t-7f3c9a1e/wf-bug-fix-flow
instance_branch: inst-f3a8b2c1

$ csc wf node push 001-collect --input-json=./inputs/001.json

→ Calling workflow/init via orchestrator (idempotent)...
  ✓ team ns exists
  ✓ instance branch exists: inst-f3a8b2c1
→ git fetch origin inst-f3a8b2c1
→ git checkout -b node/001-collect origin/inst-f3a8b2c1
→ writing nodes/001-collect/output.md + input.json
→ git commit -m "node(001-collect): collect inputs"
→ git push origin node/001-collect
→ POST /repos/.../pulls { base: inst-f3a8b2c1, head: node/001-collect }

✓ Node 001-collect pushed
  PR:    #12 opened (audit_level=auto, will auto-merge after CI)
  Base:  inst-f3a8b2c1  ← NOT main
```

### 10.3 团队级 def 演进

```bash
# 团队 owner 想给 bug-fix-flow 加新节点
$ csc wf def update --def-file=./def-v2.yaml --title="def: add 004-notify node"

→ Resolving team_id from CSC_TEAM_ID... 7f3c9a1e-...
→ git clone https://gitea.costrict.local/t-7f3c9a1e/wf-bug-fix-flow.git -b main /tmp/wf-def-a1b2c3
→ cp ./def-v2.yaml /tmp/wf-def-a1b2c3/definition.yaml
→ git commit -m "def: add 004-notify node"
→ git push origin main:refs/heads/def-update-a1b2c3
→ POST /repos/.../pulls { base: main, head: def-update-a1b2c3 }

✓ Def update PR opened: #15
  After merge into main, new instances will use the updated def.
  Existing instances (inst-*) are not affected.
```

### 10.4 team ns 未初始化

```bash
$ csc wf init bug-fix-flow f3a8b2c1-... --team-id=99999999-...

→ Calling workflow/init via orchestrator...
  ✗ HTTP 412: team_ns_not_initialized

✗ Team namespace not initialized

Team:     99999999-...
Hint:     Contact team admin to run members:sync via portal UI,
          or wait for org-team-service webhook auto-forward.

After team ns is initialized, re-run: csc wf init ...
```

---

## 修订记录

| 版本 | 日期 | 修订内容 |
|---|---|---|
| v1.0 | 2026-07-14 | 首次发布：定义 11 个子命令（init / node push / node list / node approve / node merge / list / authorize / revoke / transfer-owner / archive / pr）+ 输入参数来源 + 错误处理矩阵 + 退出码约定（含 code 7 定义漂移）+ 配置文件依赖 |
| v2.0 | 2026-07-15 | **架构反转：类型 repo + 实例 branch 模型 + 编排器代调 + 节点 PR base = inst-<short>**：①workflow 模型从「每实例一 repo」改为「类型 repo + 实例 branch」（路径 `t-<team_short>/wf-<def>` + branch `inst-<short>`）；②接口从 `POST /api/workflow/init` 改为 `POST /api/internal/workflow/init`（内部接口，csc 经编排器代调）；③**节点 PR base 从 main 改为 `instance_branch`**——main 永远只放 def canonical，实例时间线独立；④**新增 §4.6 `csc wf def update`**——团队级 def PR（main 上协作）；⑤**移除 `csc wf authorize / revoke / transfer-owner`**——成员管理统一走 team 级 `members:sync`；⑥§4.8 archive 行为调整：v2.0 不再归档整个 repo，而是 tag + 删除实例 branch；⑦新增 §3 team_id 来源优先级链；⑧§6 错误处理新增 412 `TEAM_NS_NOT_INITIALIZED` 与 409 `INSTANCE_BRANCH_CONFLICT` 分支，移除 403 tenant mismatch；⑨§7 配置字段新增 `orchestrator_endpoint` 与 `default_team_id`，实例级 `.costrict/wf.yaml` 增加 `team_id` + init 响应缓存；⑩§8 退出码新增 code 8（实例 branch 碰撞）；⑪新增 §10 完整示例（4 个场景：首次启动 / 推送节点 / def 演进 / team ns 未初始化）；⑫依据 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) v2.17 §17 / §18 与 [`TEAM_NAMESPACE_API.md`](./TEAM_NAMESPACE_API.md) v2.0 §5 |
