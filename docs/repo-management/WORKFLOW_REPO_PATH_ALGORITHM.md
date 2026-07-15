# Workflow Repo 路径推断算法规范

| 版本 | v1.0 |
|---|---|
| 创建日期 | 2026-07-14 |
| 依据 | [`REPOSITORY_MANAGEMENT_SPEC.md`](./REPOSITORY_MANAGEMENT_SPEC.md) v2.16 §17 |
| 算法实现唯一归属 | **costrict-web server**（csc 不内置副本） |

> 本规范定义 `(workflow_def_slug, instance_id) → wf_repo_path` 的**确定性纯函数**。相同输入在任何时间、任何 tenant、任何调用方（server 内部 / csc 经由 `POST /api/workflow/init` 返回值）必须得到完全相同的输出。

---

## 1. 设计目标

1. **唯一映射**：任意 `(workflow_def_slug, instance_id)` 在 tenant 内确定唯一 `wf_repo_path`，平台调度场景下"凭 instance_id 找 repo"无需任何 owner 上下文
2. **Gitea 路径合规**：输出必须满足 Gitea `<owner>/<repo>` 2 层路径硬约束（owner = 固定 `costrict-workflow`，repo = 算法输出标识符）
3. **可读性**：标识符保留 def_slug（人类可读 workflow 名）+ short_id（实例短标识），admin / owner 在 Gitea UI 列表能识别对应实例
4. **冲突安全**：相同 def_slug 的不同 instance 不会碰撞；不同 def_slug 不会碰撞
5. **零状态**：纯函数，无 DB 依赖、无 cache、无锁

---

## 2. 输入 / 输出

```
input:  workflow_def_slug : string
        // workflow 定义的 kebab-case slug，标识"哪一类 workflow"
        // 例：
        //   "bug-fix-flow"
        //   "compliance-check"
        //   "release-pipeline"

        instance_id : string
        // 平台为 workflow 实例分配的 UUID（v4 / v7 均可）
        // 例：
        //   "f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab"
        //   "a9e7d4f2-1234-5678-9abc-def012345678"

output: wf_repo_path : string
        // 形如 "costrict-workflow/<def_slug_escaped>__<short_id>"
        // 例：
        //   "costrict-workflow/bug-fix-flow__f3a8b2c1"
        //   "costrict-workflow/compliance-check__a9e7d4f2"
```

---

## 3. 算法步骤

```
function wfRepoPath(workflow_def_slug: string, instance_id: string) -> string:

  step 1  输入校验
          if workflow_def_slug == "" or workflow_def_slug == null:
              throw InvalidDefSlug
          if instance_id == "" or instance_id == null:
              throw InvalidInstanceId
          if not matches(instance_id, "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"):
              throw InvalidInstanceId    # 必须是合法 UUID

  step 2  def_slug 归一化
          slug = lowercase(workflow_def_slug)
          slug = stripPrefix(slug, "-")    # 去前导连字符
          slug = stripSuffix(slug, "-")    # 去尾部连字符

  step 3  def_slug 转义
          function escapeSlug(s: string) -> string:
              out = ""
              for ch in s:
                  if ch ∈ {[a-z],[0-9],'-'}:    # 经 step2 lowercase 后只剩小写
                      out += ch
                  else:
                      out += "-"                  # 其他字符统一替换为连字符（保持 kebab-case 风格）
              # 合并连续连字符
              out = replaceAll(out, "--+", "-")
              if out == "": out = "wf"             # 极端兜底
              return out
          escaped_slug = escapeSlug(slug)

  step 4  instance_id 截短
          # 取 UUID 第一段（前 8 个十六进制字符）作为 short_id
          short_id = lowercase(instance_id.substring(0, 8))
          # 例：UUID "f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab" → "f3a8b2c1"

  step 5  拼接
          joined = escaped_slug + "__" + short_id
          return "costrict-workflow/" + joined
```

### 3.1 关键归一化点

| 差异源 | 归一化规则 | 示例 |
|---|---|---|
| 大小写 | def_slug / instance_id 全部 lowercase | `Bug-Fix-Flow` ≡ `bug-fix-flow` |
| 前导 / 尾部 `-` | 去除 | `-bug-fix-flow-` ≡ `bug-fix-flow` |
| 非法字符 | 替换为 `-`（连续合并） | `bug_fix.flow` → `bug-fix-flow` |
| UUID 形式 | 仅取首段 8 hex | 完整 UUID ≡ short_id |
| UUID 大小写 | lowercase | `F3A8B2C1-...` ≡ `f3a8b2c1-...` |

### 3.2 instance_id 来源约束

instance_id 由平台 workflow 编排器（独立服务，不在本规范范围）在实例启动时一次性分配，**全 tenant 内唯一**。server / csc 端**不生成** instance_id，仅校验格式 + 取首段。

---

## 4. 测试用例

### 4.1 基础用例

| # | 输入 (def_slug, instance_id) | 期望输出 | 说明 |
|---|---|---|---|
| T01 | `("bug-fix-flow", "f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab")` | `costrict-workflow/bug-fix-flow__f3a8b2c1` | 最常见场景 |
| T02 | `("compliance-check", "a9e7d4f2-1234-5678-9abc-def012345678")` | `costrict-workflow/compliance-check__a9e7d4f2` | 长 slug |
| T03 | `("release", "00000000-0000-0000-0000-000000000001")` | `costrict-workflow/release__00000000` | 全零 UUID 边界（仍合法） |
| T04 | `("ci", "abcdef12-3456-7890-abcd-ef1234567890")` | `costrict-workflow/ci__abcdef12` | 短 slug |

### 4.2 字符转义用例

| # | 输入 | 期望输出 | 说明 |
|---|---|---|---|
| T10 | `("Bug_Fix.Flow", "f3a8b2c1-...")` | `costrict-workflow/bug-fix-flow__f3a8b2c1` | `_` / `.` → `-` |
| T11 | `("Bug--Fix__Flow", "f3a8b2c1-...")` | `costrict-workflow/bug-fix-flow__f3a8b2c1` | 连续分隔符合并 |
| T12 | `("-bug-fix-flow-", "f3a8b2c1-...")` | `costrict-workflow/bug-fix-flow__f3a8b2c1` | 前导 / 尾部 `-` 去除 |
| T13 | `("bug@fix#flow", "f3a8b2c1-...")` | `costrict-workflow/bug-fix-flow__f3a8b2c1` | `@` / `#` → `-` |
| T14 | `("Bug修复流程", "f3a8b2c1-...")` | `costrict-workflow/bug-__f3a8b2c1` | 中文 4 字符各转 `-`（共 4 个）经 step 3 连字符合并规则 `replaceAll("--+","-")` 后保留为单 `-`，最终 `bug-` + `__` + `f3a8b2c1` = `bug-__f3a8b2c1`（详见 B03 边界讨论） |

### 4.3 等价类（必须输出相同）

| 组 | 输入组 | 期望输出 |
|---|---|---|
| E01 | `("bug-fix-flow", "f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab")` / `("Bug-Fix-Flow", "F3A8B2C1-9D7E-4A2B-8E1F-1234567890AB")` | `costrict-workflow/bug-fix-flow__f3a8b2c1` |
| E02 | `("release_pipeline", "...")` / `("release-pipeline", "...")` / `("release.pipeline", "...")` | `costrict-workflow/release-pipeline__<short_id>` |

### 4.4 非法输入（必须 throw）

| # | 输入 | 拒绝原因 |
|---|---|---|
| X01 | `("", "f3a8b2c1-...")` | def_slug 为空 |
| X02 | `(null, "f3a8b2c1-...")` | def_slug 为 null |
| X03 | `("bug-fix-flow", "")` | instance_id 为空 |
| X04 | `("bug-fix-flow", "not-a-uuid")` | instance_id 非 UUID 格式 |
| X05 | `("bug-fix-flow", "f3a8b2c1")` | instance_id 缺少 UUID 完整分段 |
| X06 | `("bug-fix-flow", "g3a8b2c1-9d7e-4a2b-8e1f-1234567890ab")` | instance_id 含非十六进制字符 `g` |
| X07 | `("bug-fix-flow", null)` | instance_id 为 null |

### 4.5 边界用例

| # | 输入 | 期望输出 | 说明 |
|---|---|---|---|
| B01 | `("---", "f3a8b2c1-...")` | `costrict-workflow/wf__f3a8b2c1` | 全部字符被 strip / 替换后为空 → 兜底为 `wf` |
| B02 | `("a", "00000000-0000-0000-0000-000000000000")` | `costrict-workflow/a__00000000` | 单字符 slug + 全零 UUID（合法） |
| B03 | `("Bug修复流程", "f3a8b2c1-...")` | `costrict-workflow/bug-__f3a8b2c1` | "Bug修复流程" → step2 lowercase → "bug修复流程" → step3 escape：`b/u/g` 保留，4 个中文字符各转 `-` → `bug----` → step3 连字符合并 → `bug-`；最终 `bug-` + `__` + `f3a8b2c1` = `bug-__f3a8b2c1`（注意 `bug-` 尾部 `-` **不剥离**——stripSuffix 仅在 step2 一开始执行，step3 输出后不再处理） |

> 边界用例 B03 在实际使用中极少出现（workflow def_slug 通常由平台预定义，规范为 `[a-z0-9-]`）。server 端**接受且不报错**——按算法确定性输出。csc 端建议在 workflow 定义注册阶段做规范化校验。

---

## 5. Gitea 路径合规约束

| 约束 | 满足方式 |
|---|---|
| 必须 `<owner>/<repo>` 2 层 | owner 固定 `costrict-workflow`，repo 为 `slug__short_id` 单层 |
| repo 名字符集 `[a-z0-9._-]` | step 3 escape 兜底，所有非白名单字符 → `-`（白名单包含 `_`，但算法输出仅在分隔符 `__` 处出现） |
| repo 名不以 `.` 开头 | def_slug 经 step3 输出绝不会以 `.` 开头（仅 `[a-z0-9-]`） |
| repo 名长度 ≤ 64 | 见 §6（超长截断策略） |
| repo 名全局唯一 | 由纯函数保证（相同输入 → 相同输出） |

---

## 6. 长度截断策略

Gitea repo 名上限 64 字符。算法输出 `repo_part = escaped_slug + "__" + short_id`，正常情况长度 = `len(escaped_slug) + 10`。当 `len(escaped_slug) > 54` 时（即 `repo_part` 超 64 字符），server 端在 step 5 末尾**仅对 escaped_slug 切片**，short_id 完全丢弃（其信息已纳入 hash 计算）：

```
repo_part = escaped_slug + "__" + short_id            # 正常形式
if len(repo_part) > 64:
    # 触发条件：len(escaped_slug) > 54
    hash_suffix = sha1(escaped_slug + "__" + short_id)[:8]   # 8 字符
    # 截断后形式：truncated_slug + "~~" + hash
    # 总长预算：slice_len + 2 ("~~") + 8 (hash) = 64
    # ⇒ slice_len = 54
    slice_len = 64 - 2 - 8                # = 54，常量（不依赖 len(escaped_slug)）
    truncated_slug = escaped_slug[:slice_len]
    repo_part = truncated_slug + "~~" + hash_suffix
```

截断后形式与字段长度：

| 字段 | 长度 |
|---|---|
| truncated_slug（escaped_slug 前 54 字符切片） | 54 |
| `~~` 分隔符（切片与 hash 之间） | 2 |
| 8 字符 hash（基于 escaped_slug + short_id 计算） | 8 |
| **合计** | **= 64** |

> 注意：截断后形式 `truncated_slug + "~~" + hash` **不再含 `__` 分隔符**——short_id 已被 hash 替代。这是与正常形式的结构差异，但 Gitea repo 名仅校验总长 + 字符集，不要求结构一致。

**触发阈值**：`len(escaped_slug) > 54`（即 `len(repo_part) = len(escaped_slug) + 10 > 64`）。实际场景中 workflow def_slug 极少超过 30 字符，截断分支极少触发。

**唯一性保证**：hash 基于 `escaped_slug + "__" + short_id` 计算——即使 short_id 在切片中丢失，仍被纳入 hash 防碰撞（碰撞概率 1/2^32 以内，对内部系统可接受）。相同 (escaped_slug, short_id) → 相同 hash → 相同输出（确定性保持）。

---

## 7. 算法版本化

### 7.1 当前版本

- **算法版本**：v1
- **生效日期**：2026-07-14
- **依赖**：UUID 格式正则、SHA-1（仅截断分支）

### 7.2 版本演进策略

- v1 算法**不做兼容变更**（输出格式不可变）
- 若必须演进（如改 escape 规则 / 改分隔符 / 改 short_id 长度）：

  1. 引入 v2 算法
  2. server 在 `POST /api/workflow/init` 响应中新增 `algorithm_version` 字段
  3. 对 v1 创建的 repo **保留原路径**（server 维护 v1→v2 alias 表或迁移脚本）
  4. csc 永远从 server 响应读取 path，不参与版本协商

- 不引入路径前缀版本号（如 `costrict-workflow/v1/...`）—— Gitea 路径只支持 2 层

### 7.3 测试集回归

每次算法或实现变更必须跑通 §4 全部测试用例；新增用例追加到 §4 即可，无需版本号变更（只要输出与既有用例一致）。

---

## 8. 与 csc / 平台编排器的契约

| 调用方行为 | 合规要求 |
|---|---|
| csc 不内置算法 | 路径来源仅来自 `POST /api/workflow/init` 响应的 `wf_repo_path` 字段 |
| csc 不生成 instance_id | instance_id 由平台 workflow 编排器在实例启动时分配 |
| 平台编排器校验 def_slug | 注册 workflow 定义时校验 def_slug 符合 `[a-z0-9-]+`（实际允许更宽，但建议规范化） |
| 平台编排器校验 instance_id | 必须是合法 UUID v4 / v7 |
| 错误处理 | server 返回 400（非法输入）时透传错误信息给调用方，不本地兜底 |

server 行为：

| server 行为 | 合规要求 |
|---|---|
| 接受 `(workflow_def_slug, instance_id)` | 必须 def_slug 非空 + instance_id 合法 UUID，否则 400 |
| 内部计算 path | 使用本规范算法 |
| 不持久化 path 表 | 路径每次实时计算（无 `wf_repos` 表） |
| 与 Gitea 交互 | 用 admin PAT 操作 `costrict-workflow/<repo>` |

---

## 9. 示例：完整调用链

```
平台编排器启动 workflow 实例：
  def_slug    = "bug-fix-flow"
  instance_id = "f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab"
  owner       = alice (触发者 JWT)

1. 平台编排器 / alice 调用 POST /api/workflow/init
   请求: {
     "workflow_def_slug": "bug-fix-flow",
     "instance_id": "f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab",
     "definition_snapshot": "<yaml 内容>",
     "audit_config": { ... }
   }
   Header: Authorization: Bearer <alice JWT>

2. server 内部执行算法:
   slug       = "bug-fix-flow"
   escaped    = "bug-fix-flow"
   short_id   = "f3a8b2c1"
   wf_repo_path = "costrict-workflow/bug-fix-flow__f3a8b2c1"

3. server 用 admin PAT 调 Gitea:
   GET /repos/costrict-workflow/bug-fix-flow__f3a8b2c1
   - 404 → 不存在分支:
     POST /admin/users/costrict-workflow/repos ... 创建 repo（private）
     PUT /repos/.../contents/.workflow/instance.json ... 写入实例元数据
     PUT /repos/.../contents/.workflow/definition.snapshot.yaml ... 写入定义快照
     PUT /repos/.../contents/.workflow/node-prs.json ... 写入空索引 "{}"
     POST /repos/.../collaborators/alice ... 加 alice 为 owner permission
     POST /repos/.../branch_protections ... 配置 main 保护（禁 force push / delete）
     返回 { role: "owner", created: true }
   - 200 → 存在分支:
     GET /repos/.../collaborators/alice/permission
     - permission == "owner" or "write": 返回 { role: ..., created: false }
     - permission == "none":
       GET /repos/... → owner username
       返回 { role: "none", created: false, owner: {username, display_name}, hint: "..." }

4. 调用方按响应行为:
   - role=owner/write → 后续节点 push / PR 流程
   - role=none → 打印 owner + hint, exit ≠ 0
```

---

## 10. 开放问题（暂不解决）

| 问题 | 现状 | 后续 |
|---|---|---|
| workflow_def_slug 改名（如重新组织业务分类） | 旧 wf_repo_path 不会自动重新指向 | 暂不支持迁移；如需，owner 可手动 transfer 或在 Gitea UI rename（路径将不一致，server 重新 init 会创建新 repo） |
| instance_id 碰撞（极小概率） | short_id 取前 8 hex，碰撞概率 1/16M | 实际场景下 tenant 内 workflow 实例数远低于碰撞阈值；如发生，server 建库时会因 Gitea repo 已存在而走"存在分支"路径，由调用方 owner 信息暴露冲突 |
| def_slug 含保留字（如 `admin` / `system`） | 算法不拦截 | 建议在 workflow 定义注册阶段由平台编排器校验保留字 |

---

## 修订记录

| 版本 | 日期 | 修订内容 |
|---|---|---|
| v1.0 | 2026-07-14 | 首次发布：定义 `(def_slug, instance_id) → wf_repo_path` 确定性纯函数算法、字符转义规则、长度截断策略、基础/转义/等价类/非法/边界共 20+ 测试用例、与 csc / server / 平台编排器的契约 |
