# KB Repo 路径推断算法规范

| 版本 | v1.0 |
|---|---|
| 创建日期 | 2026-07-14 |
| 依据 | [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) v2.15 §16 |
| 算法实现唯一归属 | **costrict-web server**（csc 不内置副本） |

> 本规范定义 `code_repo_url → kb_repo_path` 的**确定性纯函数**。相同输入在任何时间、任何 tenant、任何调用方（server 内部 / csc 经由 `POST /api/kb/ensure` 返回值）必须得到完全相同的输出。

---

## 1. 设计目标

1. **唯一映射**：任意 `code_repo_url` 在 tenant 内确定唯一 `kb_repo_path`，保证同代码 repo 的不同用户算出同一路径
2. **Gitea 路径合规**：输出必须满足 Gitea `<owner>/<repo>` 2 层路径硬约束（owner = 固定 `costrict-kb`，repo = 算法输出标识符）
3. **可读性**：标识符保留 host + path 主体信息，admin / owner 在 Gitea UI 列表能识别对应代码 repo
4. **冲突安全**：不同 host / path 不会碰撞到同一标识符；URL 大小写差异、协议差异、`.git` 后缀差异、trailing slash 差异归一为同一标识符
5. **零状态**：纯函数，无 DB 依赖、无 cache、无锁

---

## 2. 输入 / 输出

```
input:  code_repo_url : string
        // 必须 http(s):// 起头；其他 scheme（ssh / git / file）必须先经 csc 归一化为 https 形式
        // 例：
        //   "https://github.com/ownerA/proj.git"
        //   "https://gitlab.com/Group.Foo/bar-baz"
        //   "https://gitea.costrict.local/team-x/internal-svc"

output: kb_repo_path : string
        // 形如 "costrict-kb/<host>__<escaped_segments_joined>"
        // 例：
        //   "costrict-kb/github.com__ownera__proj"
        //   "costrict-kb/gitlab.com__group.foo__bar-baz"
        //   "costrict-kb/gitea.costrict.local__team-x__internal-svc"
```

---

## 3. 算法步骤

```
function kbRepoPath(code_repo_url: string) -> string:

  step 1  解析 URL
          ┌─ 用标准 URL parser 解析 code_repo_url
          │  （必须含 scheme + host；缺 scheme 视为非法输入 → throw）
          ├─ host  = parsed.host           // 含端口则保留（如 "git.example.com:8443"）
          └─ path  = parsed.pathname       // 不含 query / fragment

  step 2  host 归一化
          host = lowercase(host)

  step 3  path 归一化
          path = lowercase(path)
          path = stripSuffix(path, ".git")  // 仅去尾部 ".git"，不去 ".GIT" 以外的变体
          path = stripSuffix(path, "/")     // 去 trailing slash（循环直到不再以 / 结尾）
          if path == "" or path == "/":
              throw InvalidPath             // 裸 host 不合法，必须有至少一段 owner/repo

  step 4  切分 segments
          segments = path.split("/")
          // 注意：连续 "//" 会产生空段，过滤掉
          segments = [s for s in segments if s != ""]

  step 5  每个 segment 转义
          function escapeSegment(s: string) -> string:
              out = ""
              for ch in s:
                  if ch ∈ {[a-z],[0-9],'.','_','-'}:   // 经 step3 lowercase 后只剩小写
                      out += ch
                  else:
                      out += "_"                        // 其他字符统一替换为下划线
              # 不允许段以 "." 开头（Gitea repo 名禁用）
              if out startsWith ".": out = "_" + out
              # 不允许段为空（连续特殊字符全替换后可能为空）
              if out == "": out = "_"
              return out
          escaped = [escapeSegment(s) for s in segments]

  step 6  拼接
          joined = "__".join(escaped)                   // 双下划线分隔
          return "costrict-kb/" + host + "__" + joined
```

### 3.1 关键归一化点

| 差异源 | 归一化规则 | 示例 |
|---|---|---|
| 大小写 | host / path 全部 lowercase | `GitHub.com/OwNeRa/Proj` ≡ `github.com/ownera/proj` |
| `.git` 后缀 | 尾部 `.git` 去除 | `https://github.com/o/p.git` ≡ `https://github.com/o/p` |
| Trailing slash | 全部去除 | `https://github.com/o/p/` ≡ `https://github.com/o/p` |
| 协议 | 不参与计算（仅 scheme 校验） | `http://` 与 `https://` 输出一致 |
| Query / fragment | 不参与计算 | `?branch=main` / `#readme` 不影响输出 |
| Port | 保留在 host 中 | `git.example.com:8443` 保留 |
| 端口默认值 | **不归一化**（443/80 显式时保留） | `gitea.x:443` ≠ `gitea.x`（csc 端应在调用前自行去除默认端口） |

### 3.2 SSH / git scheme 输入预处理

csc 接到 ssh 形式（`git@github.com:ownerA/proj.git`）或 git scheme（`git://github.com/o/p`）时，必须在调用 `POST /api/kb/ensure` 前归一化为 https 形式：

```
ssh  : git@HOST:PATH       → https://HOST/PATH
git  : git://HOST/PATH     → https://HOST/PATH
file : file:///...         → 不支持（throw）
```

server 端**不做归一化**，只校验 `https?://` 起头，避免双端规则漂移。

---

## 4. 测试用例

### 4.1 基础用例

| # | 输入 | 期望输出 | 说明 |
|---|---|---|---|
| T01 | `https://github.com/ownerA/proj.git` | `costrict-kb/github.com__ownera__proj` | 最常见 GitHub 形式 |
| T02 | `https://github.com/ownerA/proj` | `costrict-kb/github.com__ownera__proj` | 无 `.git` 后缀，与 T01 等价 |
| T03 | `https://github.com/ownerA/proj/` | `costrict-kb/github.com__ownera__proj` | trailing slash，与 T01 等价 |
| T04 | `https://GITLAB.COM/Group.Foo/bar-baz.git` | `costrict-kb/gitlab.com__group.foo__bar-baz` | host + path 大小写归一 |
| T05 | `https://gitea.costrict.local/team-x/internal-svc` | `costrict-kb/gitea.costrict.local__team-x__internal-svc` | 同 tenant Gitea 自身代码 repo |
| T06 | `https://gitlab.example.com:8443/group/sub/proj.git` | `costrict-kb/gitlab.example.com:8443__group__sub__proj` | 含端口 + 3 段 path |
| T07 | `http://gitea.intranet/myteam/Svc` | `costrict-kb/gitea.intranet__myteam__svc` | http scheme 同样支持 |

### 4.2 字符转义用例

| # | 输入 | 期望输出 | 说明 |
|---|---|---|---|
| T10 | `https://github.com/owner.A/proj_v2` | `costrict-kb/github.com__owner.a__proj_v2` | `.` / `_` 保留 |
| T11 | `https://github.com/owner+test/proj` | `costrict-kb/github.com__owner_test__proj` | `+` → `_` |
| T12 | `https://github.com/中文仓/repo` | `costrict-kb/github.com_______repo` | 中文 3 字符各转 `_`（共 3 个）+ segment 分隔符 `__`（2 个）+ host 与首段分隔符 `__`（2 个）= 7 个 `_` |
| T13 | `https://github.com/.hidden/proj` | `costrict-kb/github.com___.hidden__proj` | segment 以 `.` 开头时前面补 `_`（Gitea 合规） |
| T14 | `https://github.com/team/proj@v1` | `costrict-kb/github.com__team__proj_v1` | `@` → `_` |

### 4.3 等价类（必须输出相同）

| 组 | 输入组 | 期望输出 |
|---|---|---|
| E01 | `https://github.com/o/p.git` / `https://github.com/o/p` / `https://github.com/o/p/` / `HTTPS://GitHub.Com/o/p` | `costrict-kb/github.com__o__p` |
| E02 | `https://github.com/o/p?branch=main` / `https://github.com/o/p#readme` / `https://github.com/o/p.git?x=1` | `costrict-kb/github.com__o__p` |

### 4.4 非法输入（必须 throw）

| # | 输入 | 拒绝原因 |
|---|---|---|
| X01 | `not-a-url` | 缺 scheme |
| X02 | `ftp://github.com/o/p` | 非 http/https scheme |
| X03 | `file:///path/to/repo` | file scheme 不支持 |
| X04 | `https://github.com/` | 裸 host，无 path |
| X05 | `https://github.com` | 同上 |
| X06 | `""` (空串) | 空 |
| X07 | `null` | null |

### 4.5 边界用例

| # | 输入 | 期望输出 | 说明 |
|---|---|---|---|
| B01 | `https://github.com/o/p/.git` | `costrict-kb/github.com__o__p` | step 3 stripSuffix 把 `.git` 整体去除 → path `/o/p/` → 去 trailing `/` → `/o/p`，仅剩 2 段（与 T01 等价） |
| B02 | `https://github.com/o//p` | `costrict-kb/github.com__o__p` | 空段过滤（step 4） |
| B03 | `https://github.com/o/p/.` | `costrict-kb/github.com__o__p___.` | 单点 segment `.` 经 step 5 转义：`.` 是白名单字符保留 → startsWith `.` → 前缀补 `_` → `_.`；最终保留尾部 `.` |
| B04 | `https://gitea.costrict.local/costrict-kb/foo` | `costrict-kb/gitea.costrict.local__costrict-kb__foo` | 不做语义拦截，path 算法纯函数 |

> 边界用例 B01-B03 在 csc 实际使用中极少出现（来自人类输入的 URL 通常规范），server 端**接受且不报错**——按算法确定性输出，避免增加例外分支。csc 端建议在 UI 层做规范化提示。

---

## 5. Gitea 路径合规约束

| 约束 | 满足方式 |
|---|---|
| 必须 `<owner>/<repo>` 2 层 | owner 固定 `costrict-kb`，repo 为 `host__segments` 单层 |
| repo 名字符集 `[a-z0-9._-]` | step 5 escape 兜底，所有非白名单字符 → `_` |
| repo 名不以 `.` 开头 | step 5 补前缀 `_` |
| repo 名长度 ≤ 64 | 见 §6（超长截断策略） |
| repo 名全局唯一 | 由纯函数保证（相同 URL → 相同输出） |

---

## 6. 长度截断策略

Gitea repo 名上限 64 字符。若算法输出超长，server 端在 step 6 末尾追加截断：

```
raw = "costrict-kb/" + host + "__" + joined       # 例：long URL 可能产生 100+ 字符
repo_part = host + "__" + joined                  # 仅看 "owner/" 后的 repo 标识部分
if len(repo_part) > 64:
    # 截断保留尾部 8 字符 hash 防碰撞
    hash_suffix = sha1(joined)[:8]                 # 取 joined 内容的 sha1 前 8 字符
    # 总长预算：len(host) + 2 (host 分隔符 "__") + slice + 2 ("~~") + 8 (hash) ≤ 64
    # ⇒ slice ≤ 64 - len(host) - 2 - 10
    slice_len = 64 - len(host) - 2 - 10
    truncated = joined[:slice_len] + "~~" + hash_suffix
    repo_part = host + "__" + truncated            # 总长恰好 ≤ 64
```

截断规则：

| 字段 | 长度 |
|---|---|
| host | 原长（host 部分一般 ≤ 30） |
| `__` 分隔符（host 与 joined 之间） | 2 |
| 截断后的 joined 切片 | 64 - len(host) - 2 - 10 |
| `~~` 分隔符 | 2 |
| 8 字符 hash | 8 |
| **合计** | **= 64** |

**注意**：截断仅在极长 URL（如 Gitea 子组嵌套很深）触发，正常代码 repo URL 不会进入截断分支。一旦触发，hash 后缀保证不同输入仍得到不同输出（碰撞概率 1/2^32 以内，对内部系统可接受）。

---

## 7. 算法版本化

### 7.1 当前版本

- **算法版本**：v1
- **生效日期**：2026-07-14
- **依赖**：URL parser（标准库）、SHA-1（仅截断分支）

### 7.2 版本演进策略

- v1 算法**不做兼容变更**（输出格式不可变）
- 若必须演进（如改 escape 规则 / 改分隔符）：

  1. 引入 v2 算法
  2. server 在 `POST /api/kb/ensure` 响应中新增 `algorithm_version` 字段
  3. 对 v1 创建的 repo **保留原路径**（server 维护 v1→v2 alias 表或迁移脚本）
  4. csc 永远从 server 响应读取 path，不参与版本协商

- 不引入路径前缀版本号（如 `costrict-kb/v1/...`）—— Gitea 路径只支持 2 层

### 7.3 测试集回归

每次算法或实现变更必须跑通 §4 全部测试用例；新增用例追加到 §4 即可，无需版本号变更（只要输出与既有用例一致）。

---

## 8. 与 csc 的契约

| csc 行为 | 合规要求 |
|---|---|
| URL 输入预处理 | ssh / git scheme → https（§3.2） |
| 不内置算法 | 路径来源仅来自 `POST /api/kb/ensure` 响应的 `kb_repo_path` 字段 |
| 错误处理 | server 返回 400（非法 URL）时透传错误信息给用户，不本地兜底 |

server 行为：

| server 行为 | 合规要求 |
|---|---|
| 接受 `code_repo_url` | 必须 http/https，否则 400 |
| 内部计算 path | 使用本规范算法 |
| 不持久化 path 表 | 路径每次实时计算（无 `kb_repos` 表） |
| 与 Gitea 交互 | 用 admin PAT 操作 `costrict-kb/<repo>` |

---

## 9. 示例：完整调用链

```
用户 alice 在本地仓库 https://github.com/ownerA/proj.git 执行 csc kb push

1. csc 调用 POST /api/kb/ensure
   请求: { "code_repo_url": "https://github.com/ownerA/proj.git" }
   Header: Authorization: Bearer <alice JWT>

2. server 内部执行算法:
   host = "github.com"
   path = "ownera/proj"
   segments = ["ownera", "proj"]
   escaped = ["ownera", "proj"]
   joined = "ownera__proj"
   kb_repo_path = "costrict-kb/github.com__ownera__proj"

3. server 用 admin PAT 调 Gitea:
   GET /repos/costrict-kb/github.com__ownera__proj
   - 404 → 不存在分支:
     POST /admin/users/costrict-kb/repos ... 创建 repo
     POST /repos/costrict-kb/github.com__ownera__proj/collaborators/alice ... 加 alice 为 owner
     POST /repos/costrict-kb/github.com__ownera__proj/branch_protections ... 配置 main 保护
     返回 { role: "owner", created: true }
   - 200 → 存在分支:
     GET /repos/costrict-kb/.../collaborators/alice/permission
     - permission == "owner" or "write": 返回 { role: ..., created: false }
     - permission == "none":
       GET /repos/costrict-kb/...  → owner username
       返回 { role: "none", created: false, owner: {username, display_name}, hint: "..." }

4. csc 按响应行为:
   - role=owner/write → git push origin main
   - role=none → 打印 owner + hint, exit ≠ 0
```

---

## 10. 开放问题（暂不解决）

| 问题 | 现状 | 后续 |
|---|---|---|
| code_repo_url 变更（如项目迁移到新 host） | 旧 kb_repo_path 不会被自动重新指向 | 暂不支持迁移；如需，owner 可手动 `csc kb transfer` 或在 Gitea UI rename repo（路径将不一致，server 重新 ensure 会创建新 repo） |
| 同代码 repo 多端点（如 GitHub + Gitea 镜像） | 不同 URL → 不同 kb repo | 设计时即视为不同代码 repo，分别维护 kb |
| 代码 repo 内部分支粒度 | kb 只跟踪 main（详见 REPOSITORY_MANAGEMENT_SPEC §16） | 不在算法范畴 |

---

## 修订记录

| 版本 | 日期 | 修订内容 |
|---|---|---|
| v1.0 | 2026-07-14 | 首次发布：定义确定性纯函数算法、字符转义规则、长度截断策略、20+ 测试用例（含等价类与非法输入）、与 csc / server 的契约 |
