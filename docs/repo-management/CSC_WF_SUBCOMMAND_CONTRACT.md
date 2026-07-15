# csc wf 子命令契约（轻量）

| 版本 | v1.0 |
|---|---|
| 创建日期 | 2026-07-14 |
| 依据 | [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) v2.16 §17 |
| 关联算法 spec | [`WORKFLOW_REPO_PATH_ALGORITHM.md`](./WORKFLOW_REPO_PATH_ALGORITHM.md) |

> 本文定义 `csc wf` 子命令集的输入输出契约。csc 是 **thin client**——不内置路径算法、不维护本地状态、不编排节点 PR 流程（PR 自动 merge 由 Gitea CI / auto-approve bot 完成）。

---

## 1. 总则

1. **统一前置 init**：除 `csc wf list` 外，所有命令必须先调 `POST /api/workflow/init` 拿到 `wf_repo_path` + `role`
2. **role=none 前置失败**：除 `csc wf list` 外，所有命令在 `role=none` 时**立即打印 owner + hint 并 exit ≠ 0**，不允许后续操作
3. **path 来源唯一**：`wf_repo_path` 只来自 init 响应，**csc 不内置算法副本**
4. **PAT 使用**：所有 Gitea API 调用使用调用者本人 fine-grained PAT（scope 须含对应 wf repo 的 read/write）
5. **remote URL 构造**：clone / push / pull 的 remote URL = `<tenant_gitea_base_url>/<wf_repo_path>`（base_url 从 csc config 读取）
6. **instance_id 来源**：由平台 workflow 编排器（独立服务）在实例启动时分配，csc 不生成；命令行 / 环境变量 / 配置文件三选一传入

---

## 2. 输入参数来源

### 2.1 命令行显式

```bash
csc wf init bug-fix-flow f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab \
  --definition-snapshot=./def.yaml \
  --audit-config='{"default_audit_level":"auto"}'
```

### 2.2 环境变量

```bash
export CSC_WF_DEF_SLUG=bug-fix-flow
export CSC_WF_INSTANCE_ID=f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab
csc wf node push 001
```

### 2.3 配置文件（实例运行期推荐）

```bash
# 当前工作目录下 .costrict/wf.yaml
def_slug: bug-fix-flow
instance_id: f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab
```

由平台编排器在实例启动时写入，节点执行器（agent / 用户）进入工作目录即可使用。

---

## 3. 命令清单

### 3.1 `csc wf init`

首次初始化 workflow 实例 repo（由平台编排器在实例启动时调用）。

**用法**：
```bash
csc wf init <def_slug> <instance_id>
  --definition-snapshot=<path-to-yaml>      # 必填，workflow 定义快照
  [--audit-config=<json>]                   # 可选，节点级 PR 策略
  [--force]                                 # 可选，跳过交互式确认
```

**流程**：
```
1. 校验 def_slug 非空、instance_id 合法 UUID
2. 读取 definition-snapshot 文件内容
3. POST /api/workflow/init
   Header: Authorization: Bearer <caller JWT>
   Body: {
     workflow_def_slug,
     instance_id,
     definition_snapshot: <yaml 内容>,
     audit_config: <json 或默认 {default_audit_level:"auto"}>
   }
4. switch response:
   - created=true, role=owner:
     print "✓ Workflow repo initialized: <wf_repo_path>"
     exit 0
   - created=false, role=owner/write:
     print "✓ Workflow repo already exists: <wf_repo_path> (role=<role>)"
     exit 0   # 幂等重试
   - role=none:
     print "✗ Access denied"
     print owner + hint
     exit 1
```

**输出**（首次创建）：
```
✓ Workflow repo initialized: costrict-workflow/bug-fix-flow__f3a8b2c1
  Owner:      alice
  Definition: snapshot written to .workflow/definition.snapshot.yaml (sha=abc123)
  Audit:      default=auto, overrides={003-deliver: strict}
```

**错误**（定义漂移）：
```
✗ Definition drift detected
  Existing:  sha=def456 (in .workflow/definition.snapshot.yaml)
  Incoming:  sha=abc123
  Hint: 实例运行中不允许修改 definition；如需重启实例请新建 instance_id
exit 7
```

### 3.2 `csc wf node push <seq>`

推送指定节点的交付物到 feat 分支 + 开 PR。

**用法**：
```bash
csc wf node push <seq>                       # 例：001 / 002 / 003-deliver
  [--message="node(<seq>): <subject>"]
  [--input-json=<path>]                      # 节点输入快照（含上游 commit SHA）
```

**流程**：
```
1. POST /api/workflow/init（幂等校验 role；def_slug+instance_id 从配置文件 / 环境变量）
2. switch role:
   - "owner" | "write":
     a. 解析 seq → 拼接节点分支名 node/<seq>
     b. git checkout -b node/<seq> main
     c. 写 nodes/<seq>/output.md + artifacts/（用户 / agent 已在工作区准备好）
     d. 写 nodes/<seq>/input.json（如有 --input-json）
     e. git add nodes/<seq>/ && git commit -m "node(<seq>): <subject>"
     f. git push origin node/<seq>
     g. POST <gitea_base_url>/api/v1/repos/<wf_repo_path>/pulls
        body: {
          title: "[node-<seq>] <node_name>",
          body: 节点 input/output 摘要 + 上游 commit SHA,
          base: "main",
          head: "node/<seq>"
        }
     h. server 异步追加 .workflow/node-prs.json: {"<seq>": <pr_number>}
     i. 按 audit_level 决定后续:
        - "strict":   打印 "PR opened: #<n>. Waiting for reviewer approval."
        - "auto":     等待 CI 通过后 auto-approve bot 自动 merge（csc 不参与）
        - "experimental": 仅 push 到分支，不开 PR
     exit 0
   - "none":
     print owner + hint
     exit 1
```

**输出**（成功）：
```
✓ Node 001 pushed to branch node/001-collect
  Commit:   abc1234 (2026-07-14 11:00)
  PR:       #12 opened (audit_level=auto, will auto-merge after CI)
  Files:    nodes/001-collect/output.md (+ 2 artifacts)
```

### 3.3 `csc wf node list`

列出当前实例所有节点 + PR 状态。

**用法**：
```bash
csc wf node list [--state=all|pending|merged|closed]
```

**流程**：
```
1. POST /api/workflow/init（拿到 wf_repo_path）
2. 读取 .workflow/node-prs.json
3. 对每个 PR 调 Gitea API 查状态
4. 输出表格
```

**输出**：
```
NODE                  PR    STATE      AUDIT     COMMIT     PUSHED AT
001-collect           #12   merged     auto      abc1234    2026-07-14 11:00
002-process           #13   pending    auto      def5678    2026-07-14 11:30
003-deliver           -     -          strict    -          (未推送)
```

### 3.4 `csc wf node approve <pr-number>`

reviewer 审计节点 PR（仅 audit_level=strict 时需要）。

**用法**：
```bash
csc wf node approve <pr-number>
  [--comment="..."]
```

**流程**：
```
1. POST /api/workflow/init（校验 role=owner 或 reviewer write）
2. POST <gitea_base_url>/api/v1/repos/<wf_repo_path>/pulls/<pr-number>/reviews
   body: { event: "APPROVED", body: "<comment>" }
3. print "✓ Approved PR #<n>"
```

**输出**（成功）：
```
✓ Approved PR #15 by @alice
  Comment: LGTM, output meets acceptance criteria.
```

### 3.5 `csc wf node merge <pr-number>`

合并节点 PR（强制 merge commit 策略）。

**用法**：
```bash
csc wf node merge <pr-number> [--strategy=merge]
```

**流程**：
```
1. POST /api/workflow/init（校验 role=owner 或 reviewer write）
2. 校验 audit_level：
   - "strict": 必须已有 approve review，否则拒绝
   - "auto":   通常由 auto-approve bot 自动调；csc 手动调用作为兜底
   - "experimental": 拒绝（experimental 不开 PR）
3. POST <gitea_base_url>/api/v1/repos/<wf_repo_path>/pulls/<pr-number>/merge
   body: { Do: "merge", MergeTitleField: "node(<seq>): merge ...", MergeMessageField: "..." }
4. print "✓ Merged PR #<n> into main (commit <merge_sha>)"
```

**约束**：
- **强制 merge commit**（`Do: "merge"`），禁 squash / rebase——保留节点 commit SHA 在主历史
- audit_level=experimental 不允许 merge（实验性节点不入主线）

### 3.6 `csc wf list`

列出调用者参与的所有 workflow repo。

**用法**：
```bash
csc wf list [--limit=20] [--offset=0] [--mine] [--def=<slug>]
```

**流程**：
```
1. 调 Gitea API:
   GET /repos/costrict-workflow?limit=...&offset=...
   Authorization: Bearer <用户 PAT>
2. Gitea 自动按 collaborator 关系过滤
3. 如有 --mine：再读每个 repo 的 .workflow/instance.json.owner，过滤 == 当前用户
4. 如有 --def=<slug>：按 def_slug 前缀过滤
5. 输出表格
```

**输出**：
```
WF REPO                                              ROLE     OWNER     STATUS     LAST ACTIVITY
costrict-workflow/bug-fix-flow__f3a8b2c1             owner    alice     running    2026-07-14 11:30
costrict-workflow/release-pipeline__a9e7d4f2         write    bob       completed  2026-07-13 16:00
costrict-workflow/compliance-check__00000000         write    system    archived   2026-07-10 09:00
```

> `csc wf list` **不调用 init**——避免无端为列出的每个 repo 触发副作用；直接调 Gitea repo list API（按 JWT 过滤）。

### 3.7 `csc wf authorize <username>`

把指定用户加为当前 wf repo 的 collaborator（write 权限，可作为节点执行器或 reviewer）。

**用法**：
```bash
csc wf authorize <username> [--permission=write]
```

**流程**：
```
1. POST /api/workflow/init（必须 role=owner）
2. PUT <gitea_base_url>/api/v1/repos/<wf_repo_path>/collaborators/<username>
   body: { "permission": "write" }
3. print "✓ Authorized @<username> as write on <wf_repo_path>"
```

### 3.8 `csc wf revoke <username>`

移除指定用户的 collaborator 关系。

**用法**：
```bash
csc wf revoke <username>
```

**流程**：
```
1. POST /api/workflow/init（必须 role=owner）
2. DELETE <gitea_base_url>/api/v1/repos/<wf_repo_path>/collaborators/<username>
3. print "✓ Revoked @<username>"
```

### 3.9 `csc wf transfer-owner <username>`

转让 wf repo 的 ownership（repo 仍留在 `costrict-workflow/` org，仅 owner 字段改变）。

**用法**：
```bash
csc wf transfer-owner <username>
```

**流程**：
```
1. POST /api/workflow/init（必须 role=owner）
2. POST <gitea_base_url>/api/v1/repos/<wf_repo_path>/transfer
   body: { "new_owner": "<username>" }
   // 注意：Gitea transfer 把 repo 移到新 owner 个人 namespace
   //        workflow 场景需要"留 org 仅改 owner 字段"，由 server 在 init 接口侧拦截
   //        调用 PATCH /repos/<wf_repo_path> 改 owner_display 字段 + 写 .workflow/instance.json.owner
3. server 同步更新 .workflow/instance.json: owner=<new_username>
4. print "✓ Transferred ownership of <wf_repo_path> to @<username>"
   print "  You have been auto-downgraded to write collaborator."
```

> **注意**：workflow 场景下 transfer **不改 repo path**（repo 仍在 `costrict-workflow/`），仅更新 `.workflow/instance.json.owner` 字段。Gitea 原生 transfer API 会移走 repo，server 在 init 接口或专用 transfer API 中拦截处理，csc 端透明调用即可。

### 3.10 `csc wf archive`

归档当前实例 repo（实例结束后触发）。

**用法**：
```bash
csc wf archive [--reason="..."]
```

**流程**：
```
1. POST /api/workflow/init（必须 role=owner 或 admin）
2. PATCH <gitea_base_url>/api/v1/repos/<wf_repo_path>
   body: { "archived": true }
3. 更新 .workflow/instance.json: status=archived, archived_at=<now>
4. 强化 branch protection（禁止任何新 commit）
5. print "✓ Archived <wf_repo_path>"
```

**输出**（成功）：
```
✓ Archived costrict-workflow/bug-fix-flow__f3a8b2c1
  Status:        archived
  Archived at:   2026-07-14 12:00
  Branch protection strengthened (read-only)
```

### 3.11 `csc wf pr <action>`

Gitea PR API 透传（高级用法，正常情况下用 `csc wf node push / approve / merge`）。

**用法**：
```bash
csc wf pr open   --title="..." --body="..." [--base=main] [--head=<branch>]
csc wf pr list   [--state=open|closed|all]
csc wf pr merge  <pr-number> [--strategy=merge]    # 强制 merge commit
csc wf pr close  <pr-number>
```

**说明**：
- 子命令直接调 Gitea PR API，**不强制 init**（但建议先 `csc wf node list` 确认 path）
- merge 默认 `--strategy=merge`（强制 merge commit，禁 squash / rebase）
- 详见 Gitea PR API 文档

---

## 4. 通用错误处理

### 4.1 init 接口错误

| HTTP | csc 行为 | exit code |
|---|---|---|
| 400 | `✗ Invalid request: <detail>` | 2 |
| 401 | `✗ Not authenticated. Run: csc login` | 3 |
| 403 | `✗ Tenant mismatch. Check your JWT tenant_id.` | 3 |
| 409 | `✗ Definition drift detected (existing=<hash_existing> incoming=<hash_incoming>). 如需重启实例请新建 instance_id.` | 7 |
| 500 | `✗ Server error: <detail>. Retry later or contact admin.` | 4 |
| 网络超时 | 自动重试 2 次（指数退避 1s / 3s），仍失败 → exit 4 | 4 |

### 4.2 git 操作错误

| 错误 | csc 行为 |
|---|---|
| push 被 branch protection 拒绝（force push） | `✗ Force push is disabled on main / node/* branches. Use 'git revert' instead.` → exit 5 |
| 节点分支已存在 | 提示用户切到该分支后追加 commit，或加 `--new-branch=<suffix>` 新建 |
| merge 出现冲突 | 不自动 resolve，提示用户 `git status` + 手动 fix → push 同一分支（PR 自动更新） |
| archived 后尝试 push | `✗ Repo is archived. Push not allowed.` → exit 5 |

### 4.3 PAT 权限不足

| 错误 | csc 行为 |
|---|---|
| git push 401 | `✗ PAT missing or expired. Run: csc auth refresh` |
| git push 403 | `✗ PAT scope insufficient. Need: write on <wf_repo_path>` |
| Gitea API 403（approve / merge / authorize） | `✗ PAT scope insufficient or role mismatch (require owner/reviewer).` |
| audit_level=strict 时 merge 无 approve | `✗ Cannot merge: audit_level=strict requires approve review first.` → exit 5 |

### 4.4 audit_level 校验错误

| 错误 | csc 行为 |
|---|---|
| experimental 节点尝试 push PR | 自动跳过 PR 创建，仅 push 到分支 |
| experimental 节点尝试 merge | `✗ Cannot merge experimental node. Use --force-merge-experimental to override (admin only).` → exit 5 |

---

## 5. 配置文件依赖

`~/.costrict/config.yaml`（csc 全局配置）新增字段：

```yaml
wf:
  gitea_base_url: https://gitea.costrict.local    # 当前 tenant 的 Gitea base_url
  api_endpoint: https://api.costrict.local        # costrict-web server base_url（用于调 /api/workflow/init）
  default_node_branch_prefix: "node/"             # 节点分支前缀
  auto_merge_poll_interval: 5                     # audit_level=auto 时 csc 轮询 PR merge 状态间隔（秒），仅用于 UX 反馈
```

实例级配置（`.costrict/wf.yaml`）：

```yaml
# 平台编排器在实例启动时写入实例工作目录
def_slug: bug-fix-flow
instance_id: f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab
```

> `gitea_base_url` 与 `api_endpoint` 优先从 `csc login` 响应中获取，本地配置仅作 fallback。

---

## 6. 退出码约定

| 退出码 | 含义 |
|---|---|
| 0 | 成功 |
| 1 | role=none（无权访问 wf repo，需联系 owner 授权） |
| 2 | 输入错误（def_slug / instance_id 不合法、参数缺失） |
| 3 | 认证错误（未登录 / JWT 过期 / tenant 不匹配） |
| 4 | 服务器错误（5xx / 网络超时） |
| 5 | git 操作错误（branch protection / 冲突 / archived / audit_level 阻塞） |
| 6 | Gitea API 权限不足（PAT scope 不够 / role mismatch） |
| 7 | 定义漂移（409 definition_drift，需新建 instance_id） |

---

## 7. 与其他子命令的关系

| 命令族 | 与 wf 的关系 |
|---|---|
| `csc capability *` | **完全独立**——capability 是 §11 wizard 流程，wf 是 §17 业务数据流 |
| `csc kb *` | **完全独立**——kb 是 §16 业务数据流（KB 文档），wf 是 §17 业务数据流（workflow 交付物）；二者共用 init/ensure 设计哲学但 namespace / API / 子命令集分离 |
| `csc auth *` | 共享认证（登录、PAT 管理）；wf 命令复用同一 PAT |
| `csc config *` | 共享全局配置；`wf.*` 字段独立 |
| `csc plugin *` | 完全独立（plugin 是 capability item type） |

---

## 修订记录

| 版本 | 日期 | 修订内容 |
|---|---|---|
| v1.0 | 2026-07-14 | 首次发布：定义 11 个子命令（init / node push / node list / node approve / node merge / list / authorize / revoke / transfer-owner / archive / pr）+ 输入参数来源（命令行 / 环境变量 / 配置文件三选一）+ 错误处理矩阵（含 409 definition_drift）+ 退出码约定（含 code 7 定义漂移）+ 配置文件依赖（含实例级 .costrict/wf.yaml） |
