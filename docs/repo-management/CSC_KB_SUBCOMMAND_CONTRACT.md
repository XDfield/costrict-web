# csc kb 子命令契约（轻量）

| 版本 | v2.0 |
|---|---|
| 创建日期 | 2026-07-14 |
| 最近更新 | 2026-07-15（v2.0：team ns 路径 + 编排器代调内部接口 + 去除 authorize/revoke/transfer-owner） |
| 依据 | [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) v2.17 §16 / §18 |
| 关联算法 spec | [`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) v2.0 |
| 关联接口 spec | [`TEAM_NAMESPACE_API.md`](./TEAM_NAMESPACE_API.md) v2.0 §4 |

> 本文定义 `csc kb` 子命令集的输入输出契约。csc 是 **thin client**——不内置路径算法、不维护本地状态、不编排协作流程、**不持 service token**（由编排器代调内部接口）。

---

## 0. 版本演进说明

| 版本 | 日期 | 关键差异 |
|---|---|---|
| v1.0 | 2026-07-14 | 8 个子命令（push / pull / status / list / authorize / revoke / transfer-owner / pr）；KB 落 `costrict-kb/`；csc 直接调 `POST /api/kb/ensure`（用户 JWT 鉴权） |
| **v2.0** | 2026-07-15 | **架构反转**：①KB 路径改 `t-<team_short>/kb-<host>__<segs>`（落 team ns）；②接口改 `POST /api/internal/kb/ensure`（**内部接口**，csc 经编排器代调，无用户 JWT 直接鉴权）；③**去除 authorize / revoke / transfer-owner**——成员管理统一走 team 级 `members:sync`；④`team_id` 成为必传参数；⑤错误处理新增 412 `TEAM_NS_NOT_INITIALIZED` 分支 |

---

## 1. 总则

1. **统一前置 ensure**：除 `csc kb list` 外，所有命令必须先调 `POST /api/internal/kb/ensure`（**经编排器代调**）拿到 `kb_repo_path`
2. **team_id 必传**：所有 ensure 调用必须传 `team_id`（来自 csc config / 环境变量 / `.costrict/kb.yaml`）；csc 不反查 team 归属
3. **path 来源唯一**：`kb_repo_path` 只来自 ensure 响应，**csc 不内置算法副本**
4. **PAT 使用**：所有 git 操作（clone / push / pull）使用调用者本人 fine-grained PAT（用户必须是 team ns org 成员才有 write 权限）；csc 不持 service token
5. **remote URL 构造**：clone / push / pull 的 remote URL = `<tenant_gitea_base_url>/<kb_repo_path>`（base_url 从 csc config 读取）
6. **不再有 per-repo 权限管理**：成员管理统一走 team ns（详见 [`TEAM_NAMESPACE_API.md`](./TEAM_NAMESPACE_API.md) §2 `members:sync`）；csc 不暴露 authorize / revoke / transfer-owner

---

## 2. URL 输入预处理

`code_repo_url` 的获取与归一化由 csc 负责（server 不做归一化，详见算法 spec §3.6）。

### 2.1 从当前 git repo 自动获取

```bash
# 默认从 origin remote 取
origin_url = $(git remote get-url origin)

# 多 remote 场景显式指定
csc kb push --remote=upstream
```

### 2.2 显式覆盖

```bash
csc kb push --code-repo-url=https://github.com/ownerA/proj.git
```

### 2.3 scheme 归一化（csc 端规则）

| 输入形式 | 归一化结果 |
|---|---|
| `git@github.com:ownerA/proj.git` | `https://github.com/ownerA/proj.git` |
| `git://github.com/ownerA/proj.git` | `https://github.com/ownerA/proj.git` |
| `ssh://git@github.com:22/ownerA/proj.git` | `https://github.com/ownerA/proj.git` |
| `file:///path/to/repo` | 报错（不支持本地路径） |
| 默认端口（443/80）显式 | 去除（`gitea.x:443/...` → `gitea.x/...`） |

---

## 3. team_id 来源

csc 按以下顺序解析 `team_id`（任一命中即用）：

| 优先级 | 来源 | 例 |
|---|---|---|
| 1 | 命令行参数 `--team-id` | `csc kb push --team-id=7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a` |
| 2 | 环境变量 `CSC_TEAM_ID` | `export CSC_TEAM_ID=7f3c9a1e-...` |
| 3 | 本地代码 repo 内 `.costrict/kb.yaml` | `team_id: 7f3c9a1e-...` |
| 4 | csc 全局配置 `~/.costrict/config.yaml` `kb.default_team_id` | `kb:\n  default_team_id: 7f3c9a1e-...` |

> **缺省拒绝执行**：以上 4 个来源都未命中时，csc 打印 `team_id is required (pass --team-id, set CSC_TEAM_ID, or configure .costrict/kb.yaml)` 并 exit 2。

---

## 4. 命令清单

### 4.1 `csc kb push`

推送本地代码 repo 生成的 KB 文档到 KB repo 的 main 分支。

**用法**：
```bash
csc kb push [--code-repo-url=URL] [--remote=NAME] [--team-id=UUID] [--force-local-repo=PATH]
```

**流程**：
```
1. 取 code_repo_url（命令行参数 / --remote / origin）
2. 取 team_id（§3 优先级链）
3. 经编排器代调 POST /api/internal/kb/ensure
   Header: X-Internal-Service-Token（编排器持有）
   Body: { team_id, code_repo_url }
   → { kb_repo_path, team_ns_exists, created, algorithm_version }
4. switch team_ns_exists:
   - true:
     remote_url = config.gitea_base_url + "/" + kb_repo_path
     git push <remote_url> main:main  # 用调用者本人 PAT
     exit 0
   - false:
     print "Team namespace not initialized. Contact team admin to run members:sync."
     exit 1
```

**输出**（成功）：
```
✓ KB pushed to t-7f3c9a1e/kb-github.com__ownera__proj @ <commit_sha>
```

**输出**（team ns 未初始化）：
```
✗ Team namespace not initialized

Team:     7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a
Hint:     Contact team admin to run members:sync via portal UI,
          or wait for org-team-service webhook auto-forward.

After team ns is initialized, re-run: csc kb push
```

### 4.2 `csc kb pull`

拉取 KB repo 最新 main 到本地工作区。

**用法**：
```bash
csc kb pull [--code-repo-url=URL] [--remote=NAME] [--team-id=UUID]
```

**流程**：
```
1-4. 同 push
5. switch team_ns_exists:
   - true:
     git fetch <remote_url> main
     git merge --ff-only FETCH_HEAD
     exit 0
   - false:
     同 push 的 team ns 未初始化输出
     exit 1
```

### 4.3 `csc kb status`

对比本地工作区与 KB repo main 分支的差异。

**用法**：
```bash
csc kb status [--code-repo-url=URL] [--remote=NAME] [--team-id=UUID]
```

**输出**（示例）：
```
KB repo:     t-7f3c9a1e/kb-github.com__ownera__proj
Team:        7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a
Your role:   team member (write)
Local HEAD:  a1b2c3d (2026-07-14 10:00)
Remote HEAD: e4f5g6h (2026-07-14 11:00)
Behind:      2 commits
Modified:    docs/api.md
              docs/architecture.md
Untracked:   docs/new-feature.md
```

> v2.0 起不再有 per-repo role——`Your role` 字段统一显示 team 成员身份（team ns org 成员 = write）。

### 4.4 `csc kb list`

列出调用者参与的所有 KB repo（基于 team ns org 成员关系）。

**用法**：
```bash
csc kb list [--limit=20] [--offset=0] [--team-id=UUID]
```

**流程**：
```
1. 取 team_id（§3 优先级链；--team-id 显式指定）
2. 调 Gitea API（用调用者 PAT）:
   GET /orgs/t-<team_short>/repos?limit=...&offset=...
   Authorization: Bearer <用户 PAT>
3. 过滤 name 以 "kb-" 开头的 repo
4. 输出表格
```

**输出**：
```
KB REPO                                            PUSH ACCESS  LAST PUSH
t-7f3c9a1e/kb-github.com__ownera__proj             ✓            2026-07-14 11:00
t-7f3c9a1e/kb-gitlab.com__group.foo__bar-baz       ✓            2026-07-13 15:30
t-7f3c9a1e/kb-gitea.costrict.local__team-x__svc    ✓            2026-07-12 09:00
```

> `csc kb list` **不调用 ensure**——避免无端为列出的每个 repo 触发副作用；直接调 Gitea org repo list API（按 JWT + org membership 过滤）。

### 4.5 `csc kb pr <action>`

Gitea PR API 透传（KB 文档 review 用，可选）。

**用法**：
```bash
csc kb pr open   --title="..." --body="..." [--base=main]
csc kb pr list   [--state=open|closed|all]
csc kb pr merge  <pr-number> [--strategy=merge|rebase|squash]
csc kb pr close  <pr-number>
```

**说明**：
- KB 默认走直推 main（[`SPEC`](./REPOSITORY_MANAGEMENT_SPEC.md) §16.6），PR 是可选 review 通道
- 子命令直接调 Gitea PR API（用调用者本人 PAT），**不强制 ensure**（但建议先 `csc kb status` 确认 path）
- 详见 Gitea PR API 文档

---

## 5. 已移除的命令（v1.0 → v2.0）

| 命令 | v1.0 行为 | v2.0 替代 |
|---|---|---|
| `csc kb authorize <username>` | 把指定用户加为当前 KB repo 的 collaborator write | **移除**——成员管理统一走 team ns `members:sync`（[TEAM_NAMESPACE_API](./TEAM_NAMESPACE_API.md) §2）；team admin 在 portal UI 操作 |
| `csc kb revoke <username>` | 移除指定用户的 collaborator | **移除**——同上，走 team 级 delta sync |
| `csc kb transfer-owner <username>` | 转让 KB repo ownership | **移除**——v2.0 起 KB repo owner = `t-<team_short>` org（不可转让）；ownership 转让无意义 |

**设计理由**：v2.0 的权限模型从「per-repo collaborator」改为「team ns org 成员关系」，所有成员管理统一收口到 `members:sync`，避免命令族膨胀与权限碎片化。

---

## 6. 通用错误处理

### 6.1 ensure 接口错误（编排器代调透传）

| HTTP | error_code | csc 行为 |
|---|---|---|
| 400 | `INVALID_REQUEST` | `✗ Invalid input: <detail>` → exit 2 |
| 401 | `UNAUTHORIZED_SERVICE` | `✗ Orchestrator service token rejected. Contact admin.` → exit 3 |
| 412 | `TEAM_NS_NOT_INITIALIZED` | `✗ Team namespace not initialized. Contact team admin to run members:sync.` → exit 1 |
| 500 | `GITEA_API_FAILURE` | `✗ Server error: <detail>. Retry later or contact admin.` → exit 4 |
| 网络超时 | — | 自动重试 2 次（指数退避 1s / 3s），仍失败 → exit 4 |

> **不再有 403 tenant mismatch / 401 not authenticated**——v2.0 接口走 service token（编排器代调），用户侧 JWT / tenant 校验由编排器在调用前完成。

### 6.2 git 操作错误

| 错误 | csc 行为 |
|---|---|
| push 被 branch protection 拒绝（force push） | `✗ Force push is disabled on main. Use 'git revert' instead.` → exit 5 |
| pull 出现冲突 | 不自动 resolve，提示用户 `git status` 手动处理 → exit 5 |
| 本地工作区脏（push 前） | 默认拒绝，加 `--allow-dirty` 跳过检查 |

### 6.3 PAT 权限不足

| 错误 | csc 行为 |
|---|---|
| git push 401 | `✗ PAT missing or expired. Run: csc auth refresh` |
| git push 403 | `✗ You are not a member of team <team_short>. Ask team admin to add you via members:sync.` |

---

## 7. 配置文件依赖

`~/.costrict/config.yaml`（csc 全局配置）字段：

```yaml
kb:
  gitea_base_url: https://gitea.costrict.local    # 当前 tenant 的 Gitea base_url
  orchestrator_endpoint: https://orchestrator.costrict.local  # 编排器 base_url（用于代调内部接口）
  default_remote: origin                          # 默认 git remote 名
  default_team_id: 7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a  # 可选；缺省 fallback
```

> `gitea_base_url` 与 `orchestrator_endpoint` 优先从 `csc login` 响应中获取，本地配置仅作 fallback。

---

## 8. 退出码约定

| 退出码 | 含义 |
|---|---|
| 0 | 成功 |
| 1 | team ns 未初始化（需联系 team admin 调 members:sync） |
| 2 | 输入错误（URL 不合法 / team_id 缺失 / 参数缺失） |
| 3 | 编排器鉴权失败（service token 不一致） |
| 4 | 服务器错误（5xx / 网络超时） |
| 5 | git 操作错误（branch protection / 冲突 / 本地脏） |
| 6 | PAT 权限不足（用户不是 team ns org 成员） |

---

## 9. 与其他子命令的关系

| 命令族 | 与 kb 的关系 |
|---|---|
| `csc capability *` | **完全独立**——capability 是 §11 wizard 流程，kb 是 §16 业务数据流，子命令不混用 |
| `csc wf *`（[WF 契约](./CSC_WF_SUBCOMMAND_CONTRACT.md)） | 共享 team_id 概念（同一 team 下既有 KB 又有 workflow）；编排器代调接口共用 |
| `csc auth *` | 共享认证（登录、PAT 管理）；kb 命令复用同一 PAT |
| `csc config *` | 共享全局配置；`kb.*` 字段独立 |
| `csc plugin *` | 完全独立（plugin 是 capability item type） |

---

## 10. 完整示例

### 10.1 首次推送 KB

```bash
# 用户 alice 在本地 https://github.com/ownerA/proj.git 工作
$ cd /work/proj
$ export CSC_TEAM_ID=7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a
$ csc kb push

→ Resolving code_repo_url from origin... https://github.com/ownerA/proj.git
→ Resolving team_id from CSC_TEAM_ID... 7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a
→ Calling kb/ensure via orchestrator...
  ✓ team ns exists: t-7f3c9a1e
  ✓ kb_repo_path: t-7f3c9a1e/kb-github.com__ownera__proj
  ✓ created: false  # 已存在
→ git push https://gitea.costrict.local/t-7f3c9a1e/kb-github.com__ownera__proj main:main

✓ KB pushed to t-7f3c9a1e/kb-github.com__ownera__proj @ abc1234
```

### 10.2 team ns 未初始化

```bash
$ csc kb push

→ Resolving team_id... 99999999-...  (新 team 尚未 members:sync)
→ Calling kb/ensure via orchestrator...
  ✗ HTTP 412: team_ns_not_initialized

✗ Team namespace not initialized

Team:     99999999-...
Hint:     Contact team admin to run members:sync via portal UI,
          or wait for org-team-service webhook auto-forward.

After team ns is initialized, re-run: csc kb push
```

---

## 修订记录

| 版本 | 日期 | 修订内容 |
|---|---|---|
| v1.0 | 2026-07-14 | 首次发布：定义 8 个子命令（push / pull / status / list / authorize / revoke / transfer-owner / pr）+ URL 预处理规则 + 错误处理矩阵 + 退出码约定 + 配置文件依赖 |
| v2.0 | 2026-07-15 | **架构反转：KB 落 team ns + 编排器代调内部接口 + 移除 per-repo 权限命令**：①路径示例从 `costrict-kb/...` 改为 `t-<team_short>/kb-<host>__<segs>`；②接口从 `POST /api/kb/ensure` 改为 `POST /api/internal/kb/ensure`（内部接口，csc 经编排器代调）；③**移除 `csc kb authorize / revoke / transfer-owner`**——成员管理统一走 team 级 `members:sync`；④新增 §3 team_id 来源优先级链（命令行 / 环境变量 / `.costrict/kb.yaml` / 全局 config）；⑤§4 各命令文档更新（含 team ns 未初始化分支）；⑥§5 新增"已移除的命令"对照表，解释移除理由；⑦§6 错误处理新增 412 `TEAM_NS_NOT_INITIALIZED` 分支，移除 403 tenant mismatch（编排器代调场景无用户 JWT）；⑧§7 配置字段新增 `orchestrator_endpoint` 与 `default_team_id`；⑨§8 退出码语义调整（exit 1 从 role=none 改为 team ns 未初始化；exit 6 从 PAT scope 改为非 team ns 成员）；⑩新增 §10 完整示例（首次推送 / team ns 未初始化）；⑪依据 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) v2.17 §16 / §18 与 [`TEAM_NAMESPACE_API.md`](./TEAM_NAMESPACE_API.md) v2.0 §4 |
