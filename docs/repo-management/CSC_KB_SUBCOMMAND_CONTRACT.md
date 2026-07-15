# csc kb 子命令契约（轻量）

| 版本 | v1.0 |
|---|---|
| 创建日期 | 2026-07-14 |
| 依据 | [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) v2.15 §16 |
| 关联算法 spec | [`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) |

> 本文定义 `csc kb` 子命令集的输入输出契约。csc 是 **thin client**——不内置路径算法、不维护本地状态、不编排协作流程。

---

## 1. 总则

1. **统一前置 ensure**：除 `csc kb list` 外，所有命令必须先调 `POST /api/kb/ensure` 拿到 `kb_repo_path` + `role`
2. **role=none 前置失败**：除 `csc kb list` 外，所有命令在 `role=none` 时**立即打印 owner + hint 并 exit ≠ 0**，不允许后续操作
3. **path 来源唯一**：`kb_repo_path` 只来自 ensure 响应，**csc 不内置算法副本**
4. **PAT 使用**：所有 Gitea API 调用使用调用者本人 fine-grained PAT（scope 须含对应 KB repo 的 read/write）
5. **remote URL 构造**：clone / push / pull 的 remote URL = `<tenant_gitea_base_url>/<kb_repo_path>`（base_url 从 csc config 读取）

---

## 2. URL 输入预处理

`code_repo_url` 的获取与归一化由 csc 负责（server 不做归一化，详见算法 spec §3.2）。

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

## 3. 命令清单

### 3.1 `csc kb push`

推送本地代码 repo 生成的 KB 文档到 KB repo 的 main 分支。

**用法**：
```bash
csc kb push [--code-repo-url=URL] [--remote=NAME] [--force-local-repo=PATH]
```

**流程**：
```
1. 取 code_repo_url（命令行参数 / --remote / origin）
2. POST /api/kb/ensure → { kb_repo_path, role, created, owner, hint }（详见 REPOSITORY_MANAGEMENT_SPEC §16.3.2）
3. switch role:
   - "owner" | "write":
     remote_url = config.gitea_base_url + "/" + kb_repo_path
     git push <remote_url> main:main
     exit 0
   - "none":
     print "KB repo owned by <owner.display_name>(@<owner.username>)"
     print hint
     exit 1
```

**输出**（成功）：
```
✓ KB pushed to costrict-kb/github.com__ownera__proj @ <commit_sha>
```

**输出**（无权）：
```
✗ Access denied

KB repo:  costrict-kb/github.com__ownera__proj
Owner:    Alice (@alice)
Hint:     KB repo 已由 Alice(@alice) 创建，请通过站内消息 / IM / email
          联系其授予 collaborator write 权限后重试

完成授权后重新执行: csc kb push
```

### 3.2 `csc kb pull`

拉取 KB repo 最新 main 到本地工作区。

**用法**：
```bash
csc kb pull [--code-repo-url=URL] [--remote=NAME]
```

**流程**：
```
1-2. 同 push
3. switch role:
   - "owner" | "write":
     git fetch <remote_url> main
     git merge --ff-only FETCH_HEAD
     exit 0
   - "none":
     同 push 的无权输出
     exit 1
```

### 3.3 `csc kb status`

对比本地工作区与 KB repo main 分支的差异。

**用法**：
```bash
csc kb status [--code-repo-url=URL] [--remote=NAME]
```

**输出**（示例）：
```
KB repo:     costrict-kb/github.com__ownera__proj
Your role:   owner
Local HEAD:  a1b2c3d (2026-07-14 10:00)
Remote HEAD: e4f5g6h (2026-07-14 11:00)
Behind:      2 commits
Modified:    docs/api.md
              docs/architecture.md
Untracked:   docs/new-feature.md
```

### 3.4 `csc kb list`

列出调用者参与的所有 KB repo（owner + collaborator）。

**用法**：
```bash
csc kb list [--limit=20] [--offset=0]
```

**流程**：
```
1. 调 Gitea API:
   GET /repos/costrict-kb?limit=...&offset=...
   Authorization: Bearer <用户 PAT>
2. 过滤出调用者有 collaborator 关系的 repo（Gitea 自动按权限过滤）
3. 输出表格
```

**输出**：
```
KB REPO                                          ROLE     LAST PUSH
costrict-kb/github.com__ownera__proj             owner    2026-07-14 11:00
costrict-kb/github.com__myteam__internal-svc     write    2026-07-13 15:30
costrict-kb/gitlab.com__group.foo__bar-baz       write    2026-07-12 09:00
```

> `csc kb list` **不调用 ensure**——避免无端为列出的每个 repo 触发副作用；直接调 Gitea repo list API（按 JWT 过滤）。

### 3.5 `csc kb authorize <username>`

把指定用户加为当前 KB repo 的 collaborator（write 权限）。

**用法**：
```bash
csc kb authorize <username> [--code-repo-url=URL] [--permission=write]
```

**流程**：
```
1-2. 同 push（先 ensure）
3. switch role:
   - "owner":
     PUT <gitea_base_url>/api/v1/repos/<kb_repo_path>/collaborators/<username>
       body: { "permission": "write" }   // 默认 write，可选 read / admin
     exit 0
   - "write" | "none":
     print "✗ Only owner can authorize collaborators"
     exit 1
```

**输出**（成功）：
```
✓ Authorized @bob as write on costrict-kb/github.com__ownera__proj
```

### 3.6 `csc kb revoke <username>`

移除指定用户在当前 KB repo 的 collaborator 关系。

**用法**：
```bash
csc kb revoke <username> [--code-repo-url=URL]
```

**流程**：
```
1-2. 同 push
3. switch role:
   - "owner":
     DELETE <gitea_base_url>/api/v1/repos/<kb_repo_path>/collaborators/<username>
     exit 0
   - else: 拒绝
```

### 3.7 `csc kb transfer-owner <username>`

把当前 KB repo 的 ownership 转让给指定用户（repo 仍留在 `costrict-kb/` org）。

**用法**：
```bash
csc kb transfer-owner <username> [--code-repo-url=URL]
```

**流程**：
```
1-2. 同 push
3. switch role:
   - "owner":
     POST <gitea_base_url>/api/v1/repos/<kb_repo_path>/transfer
       body: { "new_owner": "<username>" }
     // 注意：transfer 后原 owner 自动降级为 collaborator write
     exit 0
   - else: 拒绝
```

**输出**（成功）：
```
✓ Transferred ownership of costrict-kb/github.com__ownera__proj to @bob
  You have been auto-downgraded to write collaborator.
```

> **注意**：transfer 后 `kb_repo_path` 改变（仍是 `costrict-kb/...` 但归属用户变）。原 owner 后续 ensure 仍能命中 repo（Gitea 原 owner 自动保留 collaborator write）。

### 3.8 `csc kb pr <action>`

Gitea PR API 透传（KB 文档 review 用，可选）。

**用法**：
```bash
csc kb pr open   --title="..." --body="..."
csc kb pr list   [--state=open|closed|all]
csc kb pr merge  <pr-number> [--strategy=merge|rebase|squash]
csc kb pr close  <pr-number>
```

**说明**：
- KB 默认走直推 main（§16.6），PR 是可选 review 通道
- 子命令直接调 Gitea PR API，**不强制 ensure**（但建议先 `csc kb status` 确认 path）
- 详见 Gitea PR API 文档

---

## 4. 通用错误处理

### 4.1 ensure 接口错误

| HTTP | csc 行为 |
|---|---|
| 400 | `✗ Invalid code repo URL: <detail>` → exit 2 |
| 401 | `✗ Not authenticated. Run: csc login` → exit 3 |
| 403 | `✗ Tenant mismatch. Check your JWT tenant_id.` → exit 3 |
| 500 | `✗ Server error: <detail>. Retry later or contact admin.` → exit 4 |
| 网络超时 | 自动重试 2 次（指数退避 1s / 3s），仍失败 → exit 4 |

### 4.2 git 操作错误

| 错误 | csc 行为 |
|---|---|
| push 被 branch protection 拒绝（force push） | `✗ Force push is disabled on main. Use 'git revert' instead.` → exit 5 |
| pull 出现冲突 | 不自动 resolve，提示用户 `git status` 手动处理 → exit 5 |
| 本地工作区脏（push 前） | 默认拒绝，加 `--allow-dirty` 跳过检查 |

### 4.3 PAT 权限不足

| 错误 | csc 行为 |
|---|---|
| git push 401 | `✗ PAT missing or expired. Run: csc auth refresh` |
| git push 403 | `✗ PAT scope insufficient. Need: write on <kb_repo_path>` |
| Gitea API 403 | `✗ PAT scope insufficient for this operation.` |

---

## 5. 配置文件依赖

`~/.costrict/config.yaml`（csc 全局配置）新增字段：

```yaml
kb:
  gitea_base_url: https://gitea.costrict.local    # 当前 tenant 的 Gitea base_url
  api_endpoint: https://api.costrict.local        # costrict-web server base_url（用于调 /api/kb/ensure）
  default_remote: origin                          # 默认 git remote 名
```

> `gitea_base_url` 与 `api_endpoint` 优先从 `csc login` 响应中获取，本地配置仅作 fallback。

---

## 6. 退出码约定

| 退出码 | 含义 |
|---|---|
| 0 | 成功 |
| 1 | role=none（无权访问 KB repo，需联系 owner 授权） |
| 2 | 输入错误（URL 不合法、参数缺失） |
| 3 | 认证错误（未登录 / JWT 过期 / tenant 不匹配） |
| 4 | 服务器错误（5xx / 网络超时） |
| 5 | git 操作错误（branch protection / 冲突 / 本地脏） |
| 6 | Gitea API 权限不足（PAT scope 不够） |

---

## 7. 与其他子命令的关系

| 命令族 | 与 kb 的关系 |
|---|---|
| `csc capability *` | **完全独立**——capability 是 §11 wizard 流程，kb 是 §16 业务数据流，子命令不混用 |
| `csc auth *` | 共享认证（登录、PAT 管理）；kb 命令复用同一 PAT |
| `csc config *` | 共享全局配置；`kb.*` 字段独立 |
| `csc plugin *` | 完全独立（plugin 是 capability item type） |

---

## 修订记录

| 版本 | 日期 | 修订内容 |
|---|---|---|
| v1.0 | 2026-07-14 | 首次发布：定义 8 个子命令（push / pull / status / list / authorize / revoke / transfer-owner / pr）+ URL 预处理规则 + 错误处理矩阵 + 退出码约定 + 配置文件依赖 |
