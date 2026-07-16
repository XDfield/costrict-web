# KB Repo 路径推断算法规范

| 版本 | v2.0 |
|---|---|
| 创建日期 | 2026-07-14 |
| 最近更新 | 2026-07-15（v2.0：加入 `team_id` 入参，路径从 `costrict-kb/...` 改为 `t-<team_short>/kb-...`） |
| 依据 | [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) v2.17 §4.6 / §16 / §18 |
| 算法实现唯一归属 | **costrict-web server**（csc 不内置副本） |

> 本规范定义 `(code_repo_url, team_id) → kb_repo_path` 的**确定性纯函数**。相同输入在任何时间、任何 tenant、任何调用方（server 内部 / csc 经由 `POST /api/internal/kb/ensure` 返回值）必须得到完全相同的输出。

---

## 0. 版本演进说明

| 版本 | 日期 | 输入 | 输出 | 触发变更 |
|---|---|---|---|---|
| v1.0 | 2026-07-14 | `(code_repo_url)` | `costrict-kb/<host>__<segs>` | v2.15 引入 KB repo 概念 |
| **v2.0** | 2026-07-15 | `(code_repo_url, team_id)` | `t-<team_short>/kb-<host>__<segs>` | v2.17 架构反转：KB 落 team namespace（详见 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) v2.17 修订记录） |

**v1.0 → v2.0 兼容性**：v2.17 之前未实际部署 KB（v1.0 仅文档定稿，无线上数据），**不存在迁移问题**。v2.0 直接覆盖 v1.0。

---

## 1. 设计目标

1. **唯一映射**：任意 `(code_repo_url, team_id)` 在 tenant 内确定唯一 `kb_repo_path`，保证同代码 repo 在同 team 内的不同用户算出同一路径
2. **team 隔离**：相同 `code_repo_url` 在不同 team 下产生**不同** kb_repo_path（team A 与 team B 对同一 code repo 各自维护独立 KB）
3. **Gitea 路径合规**：输出必须满足 Gitea `<owner>/<repo>` 2 层路径硬约束（owner = `t-<team_short>`，repo = `kb-<host>__<segs>`）
4. **可读性**：repo 标识符保留 host + path 主体信息，admin / owner 在 Gitea UI 列表能识别对应代码 repo
5. **冲突安全**：不同 host / path 不会碰撞到同一标识符；URL 大小写差异、协议差异、`.git` 后缀差异、trailing slash 差异归一为同一标识符
6. **零状态**：纯函数，无 DB 依赖、无 cache、无锁

---

## 2. 输入 / 输出

```
input:  code_repo_url : string
        team_id       : string  (UUIDv4)

        // code_repo_url 必须 http(s):// 起头；其他 scheme（ssh / git / file）必须先经 csc 归一化为 https 形式
        // team_id 必须是合法 UUIDv4；server 在调本算法前先校验 team ns 是否存在（§18）

        // 例：
        //   code_repo_url = "https://github.com/ownerA/proj.git"
        //   team_id       = "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a"

output: kb_repo_path : string
        // 形如 "t-<team_short>/kb-<host>__<escaped_segments_joined>"
        // 例：
        //   "t-7f3c9a1e/kb-github.com__ownera__proj"
        //   "t-7f3c9a1e/kb-gitlab.com__group.foo__bar-baz"
        //   "t-9b8c7d6e/kb-gitea.costrict.local__team-x__internal-svc"
```

> **算法 output vs HTTP 响应**：本算法只产出 `kb_repo_path`（路径段，与 Gitea 实例无关）。HTTP 端点层 `POST /api/internal/kb/ensure` 在算法外多拼一字段：
> - `kb_clone_url` = `<tenant_gitea_base_url>/<kb_repo_path>.git` — 客户端 `git clone` / `push` / `pull` 直接用，**禁止**客户端自行拼 base_url（详见 [`CSC_KB_SUBCOMMAND_CONTRACT.md`](./CSC_KB_SUBCOMMAND_CONTRACT.md) §1.5 与 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) §10.3.1 的 SoT 约定）
> - `kb_web_url` = `<tenant_gitea_base_url>/<kb_repo_path>` — 浏览器跳转用
>
> `<tenant_gitea_base_url>` 由 server 从 `tenant_configs.<JWT.tenant_id>.git.base_url` 解析，对客户端透明。

**owner 推导**：`t-<team_short>`，其中 `team_short = team_id`（去连字符）前 8 hex（详见 §3.0 与 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) §18.3）。

---

## 3. 算法步骤

### 3.0 step 0 team_short 推导

```
function teamShortId(team_id: string) -> string:
    if team_id 不是合法 UUID:
        throw InvalidTeamId
    hex = team_id.replace("-", "")        # 32 hex chars
    return hex[:8].toLowerCase()          # 取前 8 hex，小写
```

例：`team_id = "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a"` → `team_short = "7f3c9a1e"` → `owner = "t-7f3c9a1e"`。

### 3.1 step 1 解析 URL

```
function kbRepoPath(code_repo_url: string, team_id: string) -> string:

  step 0  team_short 推导
          team_short = teamShortId(team_id)
          owner = "t-" + team_short

  step 1  解析 URL
          ┌─ 用标准 URL parser 解析 code_repo_url
          │  （必须含 scheme + host；缺 scheme 视为非法输入 → throw）
          ├─ host  = parsed.host           // 含端口则保留（如 "git.example.com:8443"）
          └─ path  = parsed.pathname       // 不含 query / fragment
```

### 3.2 step 2-3 host / path 归一化

```
  step 2  host 归一化
          host = lowercase(host)

  step 3  path 归一化
          path = lowercase(path)
          path = stripSuffix(path, ".git")  // 仅去尾部 ".git"
          path = stripSuffix(path, "/")     // 去 trailing slash（循环直到不再以 / 结尾）
          if path == "" or path == "/":
              throw InvalidPath             // 裸 host 不合法，必须有至少一段 owner/repo
```

### 3.3 step 4-5 切分 + 转义

```
  step 4  切分 segments
          segments = path.split("/")
          segments = [s for s in segments if s != ""]   // 过滤空段（连续 "//"）

  step 5  每个 segment 转义
          function escapeSegment(s: string) -> string:
              out = ""
              for ch in s:
                  if ch ∈ {[a-z],[0-9],'.','_','-'}:
                      out += ch
                  else:
                      out += "_"
              if out startsWith ".": out = "_" + out   # Gitea repo 名禁 "." 开头
              if out == "": out = "_"                   # 全替换为空时填 "_"
              return out
          escaped = [escapeSegment(s) for s in segments]
```

### 3.4 step 6 拼接 + `kb-` 前缀

```
  step 6  拼接
          joined = "__".join(escaped)                   // 双下划线分隔
          repo_name = "kb-" + host + "__" + joined      # 加 kb- 前缀（v2.0）
          return owner + "/" + repo_name                # t-<team_short>/kb-<host>__<joined>
```

### 3.5 关键归一化点

| 差异源 | 归一化规则 | 示例 |
|---|---|---|
| team_id | 取前 8 hex；同一 team_id 不同大小写（理论上 UUID 不存在该差异）→ 等价 | `7f3c9a1e-...` ≡ `7F3C9A1E-...`（实际不会出现） |
| code_repo_url 大小写 | host / path 全部 lowercase | `GitHub.com/OwNeRa/Proj` ≡ `github.com/ownera/proj` |
| `.git` 后缀 | 尾部 `.git` 去除 | `https://github.com/o/p.git` ≡ `https://github.com/o/p` |
| Trailing slash | 全部去除 | `https://github.com/o/p/` ≡ `https://github.com/o/p` |
| 协议 | 不参与计算（仅 scheme 校验） | `http://` 与 `https://` 输出一致 |
| Query / fragment | 不参与计算 | `?branch=main` / `#readme` 不影响输出 |
| Port | 保留在 host 中 | `git.example.com:8443` 保留 |
| 端口默认值 | **不归一化**（443/80 显式时保留） | `gitea.x:443` ≠ `gitea.x`（csc 端应在调用前自行去除默认端口） |

### 3.6 SSH / git scheme 输入预处理

csc 接到 ssh 形式（`git@github.com:ownerA/proj.git`）或 git scheme（`git://github.com/o/p`）时，必须在调用 `POST /api/internal/kb/ensure` 前归一化为 https 形式：

```
ssh  : git@HOST:PATH       → https://HOST/PATH
git  : git://HOST/PATH     → https://HOST/PATH
file : file:///...         → 不支持（throw）
```

server 端**不做归一化**，只校验 `https?://` 起头，避免双端规则漂移。

---

## 4. 测试用例

### 4.1 基础用例

> 以下用例 `team_id = "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a"`（→ `t-7f3c9a1e`）

| # | 输入 (code_repo_url) | 期望输出 | 说明 |
|---|---|---|---|
| T01 | `https://github.com/ownerA/proj.git` | `t-7f3c9a1e/kb-github.com__ownera__proj` | 最常见 GitHub 形式 |
| T02 | `https://github.com/ownerA/proj` | `t-7f3c9a1e/kb-github.com__ownera__proj` | 无 `.git` 后缀，与 T01 等价 |
| T03 | `https://github.com/ownerA/proj/` | `t-7f3c9a1e/kb-github.com__ownera__proj` | trailing slash，与 T01 等价 |
| T04 | `https://GITLAB.COM/Group.Foo/bar-baz.git` | `t-7f3c9a1e/kb-gitlab.com__group.foo__bar-baz` | host + path 大小写归一 |
| T05 | `https://gitea.costrict.local/team-x/internal-svc` | `t-7f3c9a1e/kb-gitea.costrict.local__team-x__internal-svc` | 同 tenant Gitea 自身代码 repo |
| T06 | `https://gitlab.example.com:8443/group/sub/proj.git` | `t-7f3c9a1e/kb-gitlab.example.com:8443__group__sub__proj` | 含端口 + 3 段 path |
| T07 | `http://gitea.intranet/myteam/Svc` | `t-7f3c9a1e/kb-gitea.intranet__myteam__svc` | http scheme 同样支持 |

### 4.2 字符转义用例

| # | 输入 | 期望输出 | 说明 |
|---|---|---|---|
| T10 | `https://github.com/owner.A/proj_v2` | `t-7f3c9a1e/kb-github.com__owner.a__proj_v2` | `.` / `_` 保留 |
| T11 | `https://github.com/owner+test/proj` | `t-7f3c9a1e/kb-github.com__owner_test__proj` | `+` → `_` |
| T12 | `https://github.com/中文仓/repo` | `t-7f3c9a1e/kb-github.com_______repo` | 中文 3 字符各转 `_` + segment 分隔符 `__`（2 个）+ host 与首段分隔符 `__`（2 个）= 7 个 `_`（与 v1.0 T12 同结构，仅前缀加 `kb-`） |
| T13 | `https://github.com/.hidden/proj` | `t-7f3c9a1e/kb-github.com___.hidden__proj` | segment 以 `.` 开头时前面补 `_`（Gitea 合规） |
| T14 | `https://github.com/team/proj@v1` | `t-7f3c9a1e/kb-github.com__team__proj_v1` | `@` → `_` |

### 4.3 等价类（必须输出相同）

| 组 | 输入组 | 期望输出 |
|---|---|---|
| E01 | `https://github.com/o/p.git` / `https://github.com/o/p` / `https://github.com/o/p/` / `HTTPS://GitHub.Com/o/p` | `t-7f3c9a1e/kb-github.com__o__p` |
| E02 | `https://github.com/o/p?branch=main` / `https://github.com/o/p#readme` / `https://github.com/o/p.git?x=1` | `t-7f3c9a1e/kb-github.com__o__p` |

### 4.4 team_id 维度等价类

| 组 | team_id 组 | code_repo_url | 期望输出 |
|---|---|---|---|
| ET01 | `7f3c9a1e-...` / `7F3C9A1E-...`（理论不会出现） | `https://github.com/o/p.git` | `t-7f3c9a1e/kb-github.com__o__p`（两者等价） |
| ET02 | `7f3c9a1e-...` | `https://github.com/o/p.git` | `t-7f3c9a1e/kb-github.com__o__p` |
| ET03 | `9b8c7d6e-1234-...` | `https://github.com/o/p.git` | `t-9b8c7d6e/kb-github.com__o__p`（**与 ET02 不同**——team 隔离） |

> ET02 vs ET03 验证：**相同 code_repo_url 在不同 team 下产生不同 kb_repo_path**——team A 与 team B 各自维护独立 KB（v2.17 设计原则）。

### 4.5 非法输入（必须 throw）

| # | 输入 | 拒绝原因 |
|---|---|---|
| X01 | `code_repo_url = "not-a-url"` | 缺 scheme |
| X02 | `code_repo_url = "ftp://github.com/o/p"` | 非 http/https scheme |
| X03 | `code_repo_url = "file:///path/to/repo"` | file scheme 不支持 |
| X04 | `code_repo_url = "https://github.com/"` | 裸 host，无 path |
| X05 | `code_repo_url = "https://github.com"` | 同上 |
| X06 | `code_repo_url = ""` | 空串 |
| X07 | `code_repo_url = null` | null |
| X08 | `team_id = "not-a-uuid"` | team_id 非 UUID |
| X09 | `team_id = ""` | team_id 空 |
| X10 | `team_id = null` | team_id null |

### 4.6 边界用例

| # | 输入 | 期望输出 | 说明 |
|---|---|---|---|
| B01 | `https://github.com/o/p/.git` | `t-7f3c9a1e/kb-github.com__o__p` | step 3 stripSuffix 把 `.git` 整体去除 → path `/o/p/` → 去 trailing `/` → `/o/p`，仅剩 2 段 |
| B02 | `https://github.com/o//p` | `t-7f3c9a1e/kb-github.com__o__p` | 空段过滤（step 4） |
| B03 | `https://github.com/o/p/.` | `t-7f3c9a1e/kb-github.com__o__p___.` | 单点 segment `.` 经 step 5 转义保留 → startsWith `.` → 前缀补 `_` → `_.`；最终保留尾部 `.` |
| B04 | `https://gitea.costrict.local/costrict-kb/foo` | `t-7f3c9a1e/kb-gitea.costrict.local__costrict-kb__foo` | 不做语义拦截（v2.0 下 `costrict-kb` 仅作 path 一段，不影响算法） |

> 边界用例 B01-B03 在 csc 实际使用中极少出现（来自人类输入的 URL 通常规范），server 端**接受且不报错**——按算法确定性输出。csc 端建议在 UI 层做规范化提示。

---

## 5. Gitea 路径合规约束

| 约束 | 满足方式 |
|---|---|
| 必须 `<owner>/<repo>` 2 层 | owner = `t-<team_short>`（来自 team_id），repo 为 `kb-<host>__<segs>` 单层 |
| repo 名字符集 `[a-z0-9._-]` | step 5 escape 兜底；`kb-` 前缀天然合规 |
| repo 名不以 `.` 开头 | `kb-` 前缀保证（即使 host 段以 `.` 开头，前缀已挡） |
| repo 名长度 ≤ 64 | 见 §6（超长截断策略，预算时考虑 `kb-` 前缀 3 字符） |
| repo 名 team 内唯一 | 由纯函数保证（相同 `(code_repo_url, team_id)` → 相同输出） |
| repo 名跨 team 独立 | owner 不同（`t-<team_short>` 不同），同名 repo 在不同 team ns 内互不冲突 |

---

## 6. 长度截断策略

Gitea repo 名上限 64 字符。v2.0 因加入 `kb-` 前缀（3 字符）和 host 中可能含 `t-<team_short>/`（owner 部分），repo 部分预算有所变化：

```
raw = owner + "/" + "kb-" + host + "__" + joined
              ↑           ↑              ↑
              不计入       3 字符         joined body

repo_part = "kb-" + host + "__" + joined
if len(repo_part) > 64:
    # 截断保留尾部 8 字符 hash 防碰撞
    hash_suffix = sha1(joined)[:8]
    # 总长预算：3 (kb-) + len(host) + 2 (host 分隔符 "__") + slice + 2 ("~~") + 8 (hash) ≤ 64
    # ⇒ slice ≤ 64 - 3 - len(host) - 2 - 10 = 49 - len(host)
    slice_len = 49 - len(host)
    if slice_len < 0:
        # host 本身超长（极罕见），降级策略：把 host 也截断到 hash 后缀
        # 此分支在生产环境几乎不触发，server 端记 warn 日志即可
        throw HostTooLong  # 提示 csc：原始 URL host 异常
    truncated = joined[:slice_len] + "~~" + hash_suffix
    repo_part = "kb-" + host + "__" + truncated
```

截断规则：

| 字段 | 长度 |
|---|---|
| `kb-` 前缀 | 3 |
| host | 原长 |
| `__` 分隔符（host 与 joined 之间） | 2 |
| 截断后的 joined 切片 | `49 - len(host)` |
| `~~` 分隔符 | 2 |
| 8 字符 hash | 8 |
| **合计** | **= 64** |

**注意**：截断仅在极长 URL（如 Gitea 子组嵌套很深）触发，正常代码 repo URL 不会进入截断分支。一旦触发，hash 后缀保证不同输入仍得到不同输出（碰撞概率 1/2^32 以内）。

---

## 7. 算法版本化

### 7.1 当前版本

- **算法版本**：v2
- **生效日期**：2026-07-15
- **依赖**：URL parser（标准库）、SHA-1（仅截断分支）、UUID 解析

### 7.2 版本演进策略

- v2 算法**不做兼容变更**（输出格式不可变）
- 若必须演进（如改 escape 规则 / 改分隔符）：
  1. 引入 v3 算法
  2. server 在 `POST /api/internal/kb/ensure` 响应中保持 `algorithm_version` 字段
  3. 对 v2 创建的 repo **保留原路径**（server 维护 alias 表或迁移脚本）
  4. csc 永远从 server 响应读取 path，不参与版本协商

- 不引入路径前缀版本号（如 `t-<short>/v2/kb-...`）—— Gitea 路径只支持 2 层

### 7.3 v1.0 → v2.0 迁移

**无需迁移**：v1.0 仅文档定稿，未实际部署；v2.0 直接覆盖。

### 7.4 测试集回归

每次算法或实现变更必须跑通 §4 全部测试用例；新增用例追加到 §4 即可，无需版本号变更（只要输出与既有用例一致）。

---

## 8. 与 csc 的契约

| csc 行为 | 合规要求 |
|---|---|
| URL 输入预处理 | ssh / git scheme → https（§3.6） |
| team_id 来源 | 从 csc config / 环境变量 / `.costrict/kb.yaml` 读取；csc **不反查** team 归属 |
| 不内置算法 | 路径来源仅来自 `POST /api/internal/kb/ensure` 响应的 `kb_repo_path` 字段 |
| 错误处理 | server 返回 400（非法 URL / team_id）时透传错误信息给用户，不本地兜底 |

server 行为：

| server 行为 | 合规要求 |
|---|---|
| 接受 `code_repo_url` + `team_id` | `code_repo_url` 必须 http/https，`team_id` 必须 UUID，否则 400 |
| 前置 team ns 校验 | 调本算法前先查 team ns Gitea org 是否存在；不存在返回 412 `TEAM_NS_NOT_INITIALIZED` |
| 内部计算 path | 使用本规范算法（v2） |
| 不持久化 path 表 | 路径每次实时计算（无 `kb_repos` 表） |
| 与 Gitea 交互 | 用 admin PAT 操作 `t-<team_short>/<kb-repo>` |

---

## 9. 示例：完整调用链

```
用户 alice (team 7f3c9a1e) 在本地仓库 https://github.com/ownerA/proj.git 执行 csc kb push

1. csc 解析 team_id（从 .costrict/kb.yaml）→ "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a"
2. csc 通过编排器代调 POST /api/internal/kb/ensure
   请求: {
     "team_id": "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
     "code_repo_url": "https://github.com/ownerA/proj.git"
   }
   Header: X-Internal-Service-Token（编排器持有）

3. server 内部执行算法 (v2):
   team_short = "7f3c9a1e"
   owner = "t-7f3c9a1e"
   host = "github.com"
   path = "ownera/proj"
   segments = ["ownera", "proj"]
   escaped = ["ownera", "proj"]
   joined = "ownera__proj"
   kb_repo_path = "t-7f3c9a1e/kb-github.com__ownera__proj"

4. server 用 admin PAT 调 Gitea:
   GET /repos/t-7f3c9a1e/kb-github.com__ownera__proj
   - 404 → 不存在分支:
     POST /admin/users/t-7f3c9a1e/repos ... 创建 repo（private）
     POST /repos/t-7f3c9a1e/kb-github.com__ownera__proj/branch_protections ... 配置 main 保护
     返回 {
       kb_repo_path: "t-7f3c9a1e/kb-github.com__ownera__proj",
       kb_clone_url: "https://gitea.costrict.local/t-7f3c9a1e/kb-github.com__ownera__proj.git",
       kb_web_url:   "https://gitea.costrict.local/t-7f3c9a1e/kb-github.com__ownera__proj",
       team_ns_exists: true, created: true, algorithm_version: "v2"
     }
   - 200 → 存在分支:
     返回 { kb_repo_path, kb_clone_url, kb_web_url, team_ns_exists: true, created: false, algorithm_version: "v2" }
   - team ns 不存在（org 不存在）→ 412 TEAM_NS_NOT_INITIALIZED + hint
     返回 { error: "team_ns_not_initialized", hint: "call POST /api/internal/teams/:team_id/members:sync first" }

5. csc 按响应行为:
   - team_ns_exists=true → git push origin main（remote = response.kb_clone_url，csc 直接使用不再拼接）
   - team_ns_exists=false → 打印 hint, exit ≠ 0
```

---

## 10. 开放问题（暂不解决）

| 问题 | 现状 | 后续 |
|---|---|---|
| code_repo_url 变更（如项目迁移到新 host） | 旧 kb_repo_path 不会被自动重新指向 | 暂不支持迁移；如需，admin 可手动在 Gitea UI rename repo（路径将不一致，server 重新 ensure 会创建新 repo） |
| 同代码 repo 多端点（如 GitHub + Gitea 镜像） | 不同 URL → 不同 kb repo | 设计时即视为不同代码 repo，分别维护 kb |
| 同 team 内 code_repo_url 别名（如子组路径变化） | 不同 path → 不同 kb repo | 同上，视为不同代码 repo |
| team 迁移（合并 / 拆分） | team_id 不变 → kb_repo_path 不变；team 解散 → archive | 详见 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) §18.6 |

---

## 修订记录

| 版本 | 日期 | 修订内容 |
|---|---|---|
| v1.0 | 2026-07-14 | 首次发布：定义确定性纯函数算法、字符转义规则、长度截断策略、20+ 测试用例（含等价类与非法输入）、与 csc / server 的契约 |
| v2.0 | 2026-07-15 | **架构反转：KB 落 team namespace**：①输入增加 `team_id` 参数（UUIDv4）；②输出从 `costrict-kb/<host>__<segs>` 改为 `t-<team_short>/kb-<host>__<segs>`（加 `kb-` 统一前缀）；③新增 §3.0 `teamShortId` 算法（UUID 前 8 hex）；④新增 §4.4 team_id 维度等价类（验证 team 隔离）；⑤§4.5 非法输入增加 team_id 校验；⑥§5 / §6 Gitea 路径合规与长度截断预算更新（考虑 `kb-` 前缀）；⑦§7.1 算法版本 v1 → v2；⑦§7.3 明确 v1.0 → v2.0 无需迁移；⑧§8 契约更新（csc team_id 来源、server 前置 team ns 校验）；⑨§9 完整调用链更新（含 team ns 不存在的 412 分支）；⑩依据 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) v2.17 §4.6 / §16 / §18 |
