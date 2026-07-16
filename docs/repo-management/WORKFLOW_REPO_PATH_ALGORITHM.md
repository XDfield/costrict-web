# Workflow Repo / Branch 路径推断算法规范

| 版本 | v2.0 |
|---|---|
| 创建日期 | 2026-07-14 |
| 最近更新 | 2026-07-15（v2.0：架构反转，类型 repo + 实例 branch 模型） |
| 依据 | [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) v2.17 §4.7 / §17 / §18 |
| 算法实现唯一归属 | **costrict-web server**（csc 不内置副本） |

> 本规范定义两个**确定性纯函数**：§A `(workflow_def_slug, team_id) → wf_repo_path` 决定类型 repo；§B `(instance_id) → instance_branch` 决定实例 branch。相同输入在任何时间、任何 tenant、任何调用方必须得到完全相同的输出。

---

## 0. 版本演进说明

| 版本 | 日期 | 输入 | 输出 | 模型 | 触发变更 |
|---|---|---|---|---|---|
| v1.0 | 2026-07-14 | `(def_slug, instance_id)` | `costrict-workflow/<def>__<inst_short>` 单层路径 | 每实例一 repo | v2.16 引入 workflow repo |
| **v2.0** | 2026-07-15 | §A `(def_slug, team_id)` + §B `(instance_id)` | §A 类型 repo `t-<team_short>/wf-<def_slug>` + §B instance branch `inst-<inst_short>` | 类型 repo + 实例 branch | v2.17 反转 v2.16 C 方案否决（详见 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) §17.1.1） |

**v1.0 → v2.0 兼容性**：v2.16 之前未实际部署 workflow repo（v1.0 仅文档定稿），**不存在迁移问题**。v2.0 直接覆盖 v1.0。

**v1.0 → v2.0 模型差异**：
- v1.0：1 def + 1 instance = **1 repo**（N instance = N repo）
- v2.0：1 (team, def) = **1 类型 repo**，N instance = **N branch**（in 同一类型 repo）

---

## 1. 设计目标

1. **类型聚合**：同一 (team, def_slug) 下所有实例聚合到**同一类型 repo**，不同实例以 branch 区分
2. **team 隔离**：相同 def_slug 在不同 team 下产生**不同** wf_repo_path（team A 与 team B 对同一 workflow 各自维护独立类型 repo）
3. **Gitea 路径合规**：输出必须满足 Gitea `<owner>/<repo>` 2 层路径硬约束（owner = `t-<team_short>`，repo = `wf-<def_slug>`）
4. **可读性**：repo 标识符保留 def_slug 原意，admin / owner 在 Gitea UI 列表能识别对应 workflow def
5. **冲突安全**：不同 def_slug 不会碰撞到同一 repo；branch 命名空间严格隔离（main / inst-* / node/*）
6. **零状态**：纯函数，无 DB 依赖、无 cache、无锁

---

## 2. 输入 / 输出

### 2.1 §A wfRepoPath

```
input:  workflow_def_slug : string  (团队自定义的 def 标识符，转义规则见 §3.A.5)
        team_id           : string  (UUIDv4)

        // 例：
        //   workflow_def_slug = "bug-fix-flow"
        //   team_id           = "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a"

output: wf_repo_path : string
        // 形如 "t-<team_short>/wf-<def_slug_escaped>"
        // 例：
        //   "t-7f3c9a1e/wf-bug-fix-flow"
```

### 2.2 §B wfBranchName

```
input:  instance_id : string  (UUIDv4，由 workflow 编排器分配)

        // 例：
        //   instance_id = "f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab"

output: instance_branch : string
        // 形如 "inst-<inst_short_id>"（8 hex）
        // 例：
        //   "inst-f3a8b2c1"
```

**owner 推导**：`t-<team_short>`，其中 `team_short = team_id`（去连字符）前 8 hex（与 [`KB_REPO_PATH_ALGORITHM.md`](./KB_REPO_PATH_ALGORITHM.md) §3.0 一致）。

---

## 3. 算法步骤

### §A wfRepoPath

```
function wfRepoPath(workflow_def_slug: string, team_id: string) -> string:

  step A.0  team_short 推导
            team_short = teamShortId(team_id)        # 见 §3.0
            owner = "t-" + team_short

  step A.1  def_slug 转义
            escaped_slug = escapeDefSlug(workflow_def_slug)

  step A.2  拼接 + wf- 前缀
            return owner + "/" + "wf-" + escaped_slug
```

#### §3.A.5 escapeDefSlug 规则

```
function escapeDefSlug(s: string) -> string:
    out = ""
    for ch in s:
        if ch ∈ {[a-z],[0-9],'.','_','-'}:
            out += ch
        elif ch ∈ {[A-Z]}:
            out += lowercase(ch)         # 大写字母转小写
        else:
            out += "_"
    if out startsWith ".": out = "_" + out  # 不允许 "." 开头（wf- 前缀已挡，双保险）
    if out == "": out = "unnamed"            # 空 slug 兜底
    return out
```

> def_slug 由 team 自定义（不像 code_repo_url 来自外部 URL），输入约束相对宽松：调用方应保证 def_slug 符合 `[a-z0-9._-]+`；server 仅做兜底转义。

### §B wfBranchName

```
function wfBranchName(instance_id: string) -> string:

  step B.0  校验 instance_id 为合法 UUID
            if instance_id 不是合法 UUID:
                throw InvalidInstanceId

  step B.1  截取前 8 hex
            hex = instance_id.replace("-", "")
            inst_short = hex[:8].toLowerCase()

  step B.2  拼接 inst- 前缀
            return "inst-" + inst_short
```

### 3.0 teamShortId（与 KB 算法 §3.0 完全一致）

```
function teamShortId(team_id: string) -> string:
    if team_id 不是合法 UUID:
        throw InvalidTeamId
    hex = team_id.replace("-", "")        # 32 hex chars
    return hex[:8].toLowerCase()          # 取前 8 hex，小写
```

### 3.7 关键归一化点

| 维度 | 归一化规则 | 示例 |
|---|---|---|
| team_id | 取前 8 hex；大小写归一（UUID 实际不会出现大小写差异） | `7f3c9a1e-...` ≡ `7F3C9A1E-...` |
| def_slug 大小写 | 大写字母转小写 | `Bug-Fix-Flow` ≡ `bug-fix-flow` |
| def_slug 特殊字符 | 非 `[a-z0-9._-]` → `_` | `bug fix flow` → `bug_fix_flow` |
| instance_id | 取前 8 hex；大小写归一 | `F3A8B2C1-...` ≡ `f3a8b2c1-...` |

---

## 4. 测试用例

> 以下 §A 用例 `team_id = "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a"`（→ `t-7f3c9a1e`）

### 4.1 §A wfRepoPath 基础用例

| # | 输入 (def_slug, team_id) | 期望输出 | 说明 |
|---|---|---|---|
| W01 | `("bug-fix-flow", 7f3c9a1e-...)` | `t-7f3c9a1e/wf-bug-fix-flow` | 标准 kebab-case slug |
| W02 | `("release_v2", 7f3c9a1e-...)` | `t-7f3c9a1e/wf-release_v2` | 允许 `_` |
| W03 | `("ci.deploy", 7f3c9a1e-...)` | `t-7f3c9a1e/wf-ci.deploy` | 允许 `.` |
| W04 | `("Bug-Fix-Flow", 7f3c9a1e-...)` | `t-7f3c9a1e/wf-bug-fix-flow` | 大写转小写 |
| W05 | `("bug fix flow", 7f3c9a1e-...)` | `t-7f3c9a1e/wf-bug_fix_flow` | 空格 → `_` |
| W06 | `("release/v2", 7f3c9a1e-...)` | `t-7f3c9a1e/wf-release_v2` | `/` → `_`（避免路径歧义） |
| W07 | `("Bug.Fix_Flow-v2", 9b8c7d6e-...)` | `t-9b8c7d6e/wf-bug.fix_flow-v2` | 不同 team 不同 owner |

### 4.2 §B wfBranchName 基础用例

| # | 输入 (instance_id) | 期望输出 | 说明 |
|---|---|---|---|
| WB01 | `f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab` | `inst-f3a8b2c1` | 标准 UUID |
| WB02 | `F3A8B2C1-9D7E-4A2B-8E1F-1234567890AB` | `inst-f3a8b2c1` | 大写 → 小写 |
| WB03 | `00000000-0000-0000-0000-000000000001` | `inst-00000000` | 全 0 前缀（合法） |

### 4.3 等价类（必须输出相同）

| 组 | 输入组 | 期望输出 |
|---|---|---|
| WE01 | `("bug-fix-flow", 7f3c9a1e-...)` / `("Bug-Fix-Flow", 7f3c9a1e-...)` | `t-7f3c9a1e/wf-bug-fix-flow` |
| WE02 | `("bug fix flow", 7f3c9a1e-...)` | `t-7f3c9a1e/wf-bug_fix_flow`（与 WE01 输出不同——空格转 `_`，连字符保留） |

> **调用方应保证 def_slug 唯一性**——server 不主动 dedup "bug-fix-flow" 和 "bug fix flow" 为同一 slug。

### 4.4 team_id 维度等价类

| 组 | team_id 组 | def_slug | 期望 wf_repo_path |
|---|---|---|---|
| WT01 | `7f3c9a1e-...` | `bug-fix-flow` | `t-7f3c9a1e/wf-bug-fix-flow` |
| WT02 | `9b8c7d6e-1234-...` | `bug-fix-flow` | `t-9b8c7d6e/wf-bug-fix-flow`（**与 WT01 不同**——team 隔离） |

> WT01 vs WT02 验证：**相同 def_slug 在不同 team 下产生不同 wf_repo_path**——team A 与 team B 各自维护独立类型 repo（v2.17 设计原则）。

### 4.5 非法输入（必须 throw）

| # | 输入 | 拒绝原因 |
|---|---|---|
| X01 | `team_id = "not-a-uuid"` | team_id 非 UUID |
| X02 | `team_id = ""` | team_id 空 |
| X03 | `team_id = null` | team_id null |
| X04 | `workflow_def_slug = ""` | def_slug 空（兜底转 "unnamed" 但调用方应禁止） |
| X05 | `workflow_def_slug = null` | def_slug null |
| X06 | `instance_id = "not-a-uuid"` | instance_id 非 UUID |
| X07 | `instance_id = ""` | instance_id 空 |
| X08 | `instance_id = null` | instance_id null |

### 4.6 边界用例

| # | 输入 | 期望输出 | 说明 |
|---|---|---|---|
| B01 | `(".hidden-def", 7f3c9a1e-...)` | `t-7f3c9a1e/wf-_.hidden-def` | `.` 开头被 wf- 前缀挡掉（escape 仍双保险补 `_`） |
| B02 | `("", 7f3c9a1e-...)` | 兜底 `t-7f3c9a1e/wf-unnamed` | 空 slug 兜底；调用方应禁止 |
| B03 | `("中文-def", 7f3c9a1e-...)` | `t-7f3c9a1e/wf-__-def` | 中文 2 字符 → 各 `_`（共 2 个） |
| B04 | `("a" * 100, 7f3c9a1e-...)` | 触发长度截断 | 见 §5 |

### 4.7 branch 命名空间冲突（不能复用）

| # | 输入 | 期望 | 说明 |
|---|---|---|---|
| C01 | instance_id = `11111111-...` | `inst-11111111` | 与 `main` / `node/*` 无冲突（前缀隔离） |
| C02 | instance_id = `00000000-...` | `inst-00000000` | 全 0 也合法（前缀 inst-） |

---

## 5. 长度截断策略

### 5.A wfRepoPath 截断

Gitea repo 名上限 64 字符。考虑 `wf-` 前缀（3 字符）：

```
repo_part = "wf-" + escaped_slug
if len(repo_part) > 64:
    hash_suffix = sha1(escaped_slug)[:8]
    # 预算：3 (wf-) + slice + 2 ("~~") + 8 (hash) ≤ 64
    # ⇒ slice ≤ 51
    slice_len = 51
    repo_part = "wf-" + escaped_slug[:slice_len] + "~~" + hash_suffix
```

| 字段 | 长度 |
|---|---|
| `wf-` 前缀 | 3 |
| 截断后的 escaped_slug 切片 | 51 |
| `~~` 分隔符 | 2 |
| 8 字符 hash | 8 |
| **合计** | **= 64** |

### 5.B wfBranchName 截断

branch 名上限 255 字符（Gitea），`inst-<8hex>` 仅 13 字符，**永不触发截断**。

---

## 6. Gitea 路径合规约束

| 约束 | 满足方式 |
|---|---|
| 必须 `<owner>/<repo>` 2 层 | owner = `t-<team_short>`（来自 team_id），repo = `wf-<escaped_slug>` |
| repo 名字符集 `[a-z0-9._-]` | escapeDefSlug 兜底；`wf-` 前缀天然合规 |
| repo 名不以 `.` 开头 | `wf-` 前缀保证（双保险：escapeDefSlug 也补 `_`） |
| repo 名长度 ≤ 64 | 见 §5.A |
| repo 名 team 内唯一 | 由纯函数保证（相同 `(def_slug, team_id)` → 相同输出） |
| repo 名跨 team 独立 | owner 不同（`t-<team_short>` 不同） |
| branch 名字符集 `[a-z0-9._/-]` | `inst-<8hex>` 全合规 |
| branch 命名空间隔离 | `main` / `inst-*` / `node/<seq>-<slug>` 三类前缀严格隔离 |

---

## 7. 类型 repo 内部 branch 命名空间约定

类型 repo `t-<team_short>/wf-<def_slug>` 内部 branch 命名空间：

| branch 命名 | 用途 | 创建者 | 生命周期 |
|---|---|---|---|
| `main` | workflow def canonical 存储（仅 `definition.yaml`） | server 在类型 repo 首次创建时初始化 | 永久（随类型 repo） |
| `inst-<inst_short>` | 单个实例的完整时间线（含 `.workflow/` 元数据 + `nodes/` 节点交付物） | server 在 `POST /api/internal/workflow/init` 时创建（base = main HEAD） | 实例 running / completed → archived |
| `node/<seq>-<slug>` | 节点 feat 分支，base = `inst-<short>` | 节点执行器切分支 | merge 后删除 |

> **branch 命名约定不在本算法范围内**——本算法仅规定 `inst-<short>` 的推导；`main` / `node/*` 是约定，由 csc / 节点执行器按 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) §17 遵守。

---

## 8. 算法版本化

### 8.1 当前版本

- **算法版本**：v2
- **生效日期**：2026-07-15
- **依赖**：UUID 解析、SHA-1（仅截断分支）

### 8.2 版本演进策略

- v2 算法**不做兼容变更**（输出格式不可变）
- 若必须演进：
  1. 引入 v3 算法
  2. server 在 `POST /api/internal/workflow/init` 响应中保持 `algorithm_version` 字段
  3. 对 v2 创建的类型 repo / branch **保留原路径**
  4. csc 永远从 server 响应读取 path / branch，不参与版本协商

### 8.3 v1.0 → v2.0 迁移

**无需迁移**：v1.0 仅文档定稿，未实际部署；v2.0 直接覆盖。

### 8.4 测试集回归

每次算法或实现变更必须跑通 §4 全部测试用例。

---

## 9. 与 csc / server 的契约

### 9.1 csc 行为

| 行为 | 合规要求 |
|---|---|
| `csc wf init` | 通过编排器代调 `POST /api/internal/workflow/init`，从响应读 `wf_repo_path` + `instance_branch` |
| `csc wf node push` | base branch = `instance_branch`（来自 init 响应）；**不向 main 直接 push 节点交付物** |
| `csc wf def update` | base = `main`，PR 改 `definition.yaml`（团队级 def 演进） |
| team_id 来源 | 从 csc config / 环境变量 / `.costrict/wf.yaml` 读取；csc **不反查** team 归属 |
| 不内置算法 | path / branch 来源仅来自 server 响应 |

### 9.2 server 行为

| 行为 | 合规要求 |
|---|---|
| 接受 `workflow_def_slug` + `instance_id` + `team_id` | 类型校验，否则 400 |
| 前置 team ns 校验 | 调本算法前先查 team ns Gitea org 是否存在；不存在返回 412 `TEAM_NS_NOT_INITIALIZED` |
| 内部计算 path / branch | 使用本规范算法（v2） |
| 不持久化 path 表 | 路径每次实时计算 |
| 与 Gitea 交互 | 用 admin PAT 操作 `t-<team_short>/wf-<def_slug>` |

---

## 10. 示例：完整调用链

```
编排器启动 workflow 实例:
  workflow_def_slug = "bug-fix-flow"
  instance_id       = "f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab"
  team_id           = "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a"
  definition_snapshot = "<yaml>"

1. 编排器调 POST /api/internal/workflow/init
   Header: X-Internal-Service-Token

2. server 内部执行算法 (v2):
   team_short = "7f3c9a1e"
   owner = "t-7f3c9a1e"
   escaped_slug = "bug-fix-flow"
   wf_repo_path = "t-7f3c9a1e/wf-bug-fix-flow"
   inst_short = "f3a8b2c1"
   instance_branch = "inst-f3a8b2c1"

3. server 用 admin PAT 调 Gitea:
   GET /repos/t-7f3c9a1e/wf-bug-fix-flow
   - 404 → 类型 repo 不存在分支:
     POST /admin/users/t-7f3c9a1e/repos ... 创建 repo（private）
     PUT /repos/.../contents/definition.yaml ... 写入 main（首次 canonical）
     POST /repos/.../branch_protections ... 配置 main + inst-* 通配保护
     POST /repos/.../branches ... base=main, ref=inst-f3a8b2c1
     返回 { wf_repo_path, instance_branch: "inst-f3a8b2c1", created: {type_repo: true, instance_branch: true}, algorithm_version: "v2" }
   - 200 → 类型 repo 存在:
     GET /repos/.../contents/definition.yaml ... 读 main HEAD 校验与 definition_snapshot 一致
     - 不一致 → 409 DEFINITION_DRIFT
     - 一致 →
       GET /repos/.../branches/inst-f3a8b2c1
       - 404 → POST /repos/.../branches base=main ref=inst-f3a8b2c1 → created.instance_branch=true
       - 200 → 幂等 no-op → created.instance_branch=false
   - team ns 不存在（org 不存在）→ 412 TEAM_NS_NOT_INITIALIZED

4. csc 按响应行为:
   - team_ns_exists=true → 节点执行器后续切分支 node/<seq>-<slug>, base=inst-f3a8b2c1
   - team_ns_exists=false → 打印 hint, exit ≠ 0
```

---

## 11. 开放问题（暂不解决）

| 问题 | 现状 | 后续 |
|---|---|---|
| def_slug 重命名（团队想改名） | 类型 repo 路径不变；rename 会破坏既有 inst-* branch 的可读性 | 暂不支持；admin 可在 Gitea UI rename（路径将不一致，server 重新 init 会创建新类型 repo） |
| 同 def_slug 跨 team 复用 | 不同 team_id → 不同 wf_repo_path | 设计上视为不同 workflow def，分别维护 |
| instance_id 碰撞（前 8 hex 在同类型 repo 已被占用） | server init 返回 409 INSTANCE_BRANCH_CONFLICT | 调用方重新分配 instance_id |
| branch 命名 `inst-*` 与未来其它前缀冲突 | 当前保留 `main` / `inst-*` / `node/*` | 后续扩展需用其它前缀（如 `archived/*`） |

---

## 修订记录

| 版本 | 日期 | 修订内容 |
|---|---|---|
| v1.0 | 2026-07-14 | 首次发布：定义 `(def_slug, instance_id) → costrict-workflow/<def>__<inst_short>` 单层路径推断算法（每实例一 repo） |
| v2.0 | 2026-07-15 | **架构反转：类型 repo + 实例 branch 模型**：①§A 拆分独立函数 `wfRepoPath(def_slug, team_id) → t-<team_short>/wf-<def_slug>`；②§B 拆分独立函数 `wfBranchName(instance_id) → inst-<inst_short>`；③新增 §0 v1.0 → v2.0 演进说明（明确无需迁移）；④新增 §3.A.5 `escapeDefSlug` 规则（含大写转小写）；⑤新增 §4 测试用例（W01-W07 / WB01-WB03 / WT01-WT02）；⑥新增 §5.A wfRepoPath 长度截断策略（考虑 `wf-` 前缀）；⑦新增 §7 类型 repo 内部 branch 命名空间约定（main / inst-* / node/*）；⑧§8 算法版本 v1 → v2；⑨§9 csc / server 契约更新（节点 PR base = `inst-<short>`、新增 def update 命令）；⑩§10 完整调用链更新（含 type repo 首次创建 / def drift 校验 / branch 幂等）；⑪依据 [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) v2.17 §4.7 / §17 / §18，并对照 §17.1.1 v2.16 → v2.17 C 方案决策反转说明 |
