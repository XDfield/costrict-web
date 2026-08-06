# `flatten-plugins` 迁移执行手册

> 本文件是**唯一权威版本**（纳入版本控制）。
> `.trellis/tasks/08-06-flatten-plugin-capabilities/research/migration-runbook.md` 是任务归档副本，
> `.trellis/` 被 `.gitignore` 忽略，别拿它当交付物。

> **状态**：测试环境将是这个迁移第一次碰真实数据。
> **本文档的用法**：照着走，每一步都有判据。判据不满足就停，不要临场判断。
> 唯一允许临场决定的地方，是 §6「ambiguous 行怎么办」，那本来就是留给人的。

---

## 0. 这个迁移在做什么（一句话）

把 `capability_items.parent_plugin_id` 派生出来的**包内子项行**归档并解链，
让「一个 Cloud item = 一个显式能力坐标」成立。

**它不做的事**（这几条是安全边界，不是免责声明）：

- 不硬删任何行 —— 一条 `capability_items`、`item_favorites`、`item_distributions` 都不删
- 不删任何 Git 仓库、不碰 Gitea
- 不把收藏转移给父 plugin（归档行仍可寻址，关系仍可审计）
- 不物理删列 —— `parent_plugin_id` 保留一个发布兼容窗口

---

## 1. ⚠️ 顺序依赖：写者必须先停（**最容易漏，漏了就白干**）

**这条排在最前面，因为它是唯一一个"做对了每一步、结果仍然错"的失败模式。**

清理的对象正由 api 产生。旧 api 有四条路径会写 `parent_plugin_id`：

| 写者 | 位置 |
|---|---|
| catalog ingest 的 `bundled_in` 二次连接 | `services.CatalogIngestService.reconcileParentPluginLinks` |
| archive 上传的子项提升 | `handlers.reconcilePluginSubSkills` |
| fork plugin 时的子项复制 | `handlers.forkPluginChildren` |
| Git 未绑定仓库的递归发现 | `services.discoverGitCapabilities` |

新 api 里这四个**符号已经不存在了**（不是加开关关掉，是删掉），
`createItemRequest` 连 `ParentPluginID` 字段都删了 —— 建行内核不再有这个入口。

⇒ **先清理再部署 api，就是边清边生**：apply 跑完那一刻数据是对的，
下一次 catalog ingest 又把 6716 条子项建回来，而且新建的行**不在任何一次 run 的计划里**，
审计上无迹可寻，rollback 也管不到它们。

### 写死的顺序

```
1. goose migration（含 20260806000400，只建两张工具表）
2. 部署 api + worker（写者已随代码消失）
3. 观察一轮 catalog ingest —— 判据见 §2.3
4. 才 plan / apply
```

> 与本文档其它步骤方向相反：V4 其它所有 migration 都是"migrate 先行"，
> 只有这个**数据清理**必须在 api 之后。别把两件事混成一条规则。

---

## 2. 前置检查（四条，全过才能进 §3）

### 2.1 工具表在位

```bash
psql -c "\dt plugin_flatten_migration_*"
```

判据：`plugin_flatten_migration_runs` 和 `plugin_flatten_migration_rows` 都在。

> 不在也能跑 —— 命令启动时会用等价的幂等 DDL 自建（`ensurePluginFlattenTables`）。
> 但如果这里是空的，说明 goose 没跑完，**先去查 goose，别绕过去**。

### 2.2 api 确实是新的

问**正在跑的那个 api**，不要问镜像标签（标签会骗人，尤其是复用 tag 重新部署时）：

```bash
curl -s https://<api-host>/swagger/doc.json \
  | grep -c 'DEPRECATED (flat capability model)'
```

判据：**2**（`parentPluginId` 和 `excludeSubSkills` 两个 query 参数各一处）。
拿到 **0** 说明这个 api 的 Swagger 还是旧的 ⇒ 部的是旧镜像，回到 §1。

> 为什么用 Swagger 而不是 `strings <binary> | grep reconcileParentPluginLinks`：
> 那是个不可靠的判据 —— 未导出符号在 strip 过的构建里可能本来就查不到，
> 于是"旧镜像"和"新镜像"都会给你 0，一个永远通过的检查比没有检查更糟。
> Swagger 是这个进程真正在提供的服务内容，骗不了人。

**这一条只是快速筛查，真正的判据是下一条的行为验证。**

### 2.3 观察一轮 catalog ingest，确认不再新建子项（**最硬的判据**）

```sql
-- 跑 ingest 之前记下
SELECT count(*) AS before FROM capability_items WHERE parent_plugin_id IS NOT NULL;
```

跑一次 `migrate ingest-upstream --source=<bundle>`（或等一次定时的 admin import job），然后：

```sql
SELECT count(*) AS after FROM capability_items WHERE parent_plugin_id IS NOT NULL;
```

判据：**`after == before`，一条不多**。

同时看 ingest 的汇总行，应该出现新计数器（旧 api 完全没有这个字段）：

```
catalog-ingest: done entries=N added=… updated=… metadataUpdated=… skipped=…
                bundledChildrenIgnored=<N> deleted=… failed=… incomplete=… duration=…
```

`bundledChildrenIgnored` **非 0** 就是闸门在工作的正面证据 ——
它说明 bundle 里确实还带着 `bundled_in` 条目，而 api 认出来并忽略了。
这些条目同时也计入 `skipped`（`bundledChildrenIgnored` 是它的一个子集，单独列出来
是为了让"被忽略的量"不淹没在"没变化"里）。

> `bundledChildrenIgnored=0` 且 `after == before` 是**弱证据**：可能只是这批 bundle 恰好没有子项条目。
> 这种情况下换一个已知含 plugin 的 bundle 再跑一次。

### 2.4 确认这是测试库，不是共享/生产库

```sql
SELECT current_database(), inet_server_addr(), inet_server_port();
```

⚠️ **`apply` 是本迁移唯一会改业务数据的动作，跑错库没有撤销键（只有 rollback，见 §7）。**

---

## 3. plan（只读，随时可跑）

```bash
cd server
DATABASE_URL=<...> go run ./cmd/migrate flatten-plugins plan \
  --artifact=/path/to/flatten-plan-$(date +%Y%m%d).json \
  --report-limit=-1 \
  --created-by="<你的名字>"
```

- `--artifact` **建议永远带上**：控制台只打前 20 行，artifact 是完整的逐行证据，带校验和。
- `--report-limit=-1` 打全部；`0` 打 0 行；不给则默认 20。
- plan 可以反复跑，每次生成一个新 run id，互不干扰。**plan 不写 `capability_items`。**

### plan 输出长这样

```
plugin flatten plan <run-id> (schema v1)
  digest      bf3a0454530013a71989ddbe9e746340f5ff79d3b72c613e89ca6aae84b0d163
  candidates  6739
    derived_catalog  6716
    derived_archive     0
    derived_fork       22
    independent         0
    ambiguous           1
  action archive_and_unlink  6738
  action unlink_only            0
  action skip                   1
  favorites on candidates       1
  distributions on candidates   0
```

---

## 4. 审阅：apply 前必须核对的数字

**这一节是人工闸门，不是走过场。下面每一条不满足就停。**

### 4.1 总数对得上数据库

```sql
SELECT count(*) FROM capability_items WHERE parent_plugin_id IS NOT NULL;
```

判据：**等于 `candidates`**。不等说明 plan 之后有人写了 `parent_plugin_id`（新 api 不该发生），
或者你连错库了。**重新 plan，别用旧 run。**

### 4.2 五个分类计数相加等于总数

```
derived_catalog + derived_archive + derived_fork + independent + ambiguous == candidates
```

本地基线：`6716 + 0 + 22 + 0 + 1 = 6739`。

### 4.3 `independent` 是重点看的那一个

**`independent` 是带独立 Git 坐标的行，它们只解链、不归档。**

- `independent = 0`（本地基线）：**这是最省心的情况** —— 没有任何真能力被卷进来。
- `independent > 0`：**停下来逐条看**。抽查方式：

```sql
SELECT item_id, item_slug, git_server_id, git_repo_id, git_repo_path
FROM plugin_flatten_migration_rows
WHERE run_id = '<run-id>' AND classification = 'independent';
```

确认每一条的 `(git_server_id, git_repo_id, git_repo_path)` 三件套都非空 ——
它们是 `uq_capability_items_git_manifest` 那个唯一索引的组成部分，
三件套齐全才叫"可以从 Git 重新同步回来"。齐全就放行（只解链，状态不变）。

### 4.4 `ambiguous` 全部要读一遍 reason

```sql
SELECT item_id, item_type, source_type, before_status, reason
FROM plugin_flatten_migration_rows
WHERE run_id = '<run-id>' AND classification = 'ambiguous';
```

**ambiguous 一律 `action=skip`，apply 不会碰它们。** 但你要知道它们是什么，见 §6。

本地基线只有 1 条：一个 `source_type=fork` 的 mcp，父行**根本不存在**（父 plugin 被删、子行没跟着走）。

### 4.5 收藏 / 下发影响面

```
favorites on candidates       1
distributions on candidates   0
```

- 收藏**保留在归档行上**，不转移给父 plugin（SD-3）。用户的收藏列表里那一条会变成已归档条目。
- 下发数只算 live（`status='active' AND revoked_at IS NULL`）—— 已撤销的没有消费者可影响。
- 这两个数**大到你意外**的时候要停：说明真有人在用这些子项，需要产品先知会。

### 4.6 artifact 自校验

```bash
python3 -c "
import json;d=json.load(open('flatten-plan-YYYYMMDD.json'))
print('rows', len(d['rows']), 'candidates', d['totals']['candidates'], 'digest', d['planDigest'])
assert len(d['rows']) == d['totals']['candidates'], '行数与汇总不符'
print('OK')"
```

判据：行数 == `candidates` == 数据库计数，三者相等。

> digest 只覆盖 identity + CAS 谓词 + 目标态，**故意不含** `conflict` / `rowState` ——
> 那两个字段在 apply 过程中会变，含进去的话续跑时就校验不过了，而续跑正是最需要校验的时刻。

---

## 5. apply

### 5.1 先空跑一次（不带 `--confirm`）

```bash
go run ./cmd/migrate flatten-plugins apply --run=<run-id> --report-limit=0
```

判据：打印

```
DRY RUN: run <run-id> has 6738 pending row(s) ready to apply. Re-run with --confirm.
```

`pending` 数应该等于 `archive_and_unlink + unlink_only`（= `candidates − ambiguous`）。
**空跑不写任何东西**，可以随便跑。

### 5.2 真跑

```bash
go run ./cmd/migrate flatten-plugins apply \
  --run=<run-id> --confirm \
  --batch-size=500 \
  --artifact=/path/to/flatten-plan-YYYYMMDD.json \
  --report-limit=0
```

- `--artifact` 在 apply 时是**校验**用的：文件与库里的 run 不符会直接拒绝，一行不写。
- `--batch-size` 每批一个有界事务。500 在本地 6739 行上是秒级。

输出：

```
run <run-id>: applied=6735 skipped=3 status=applied
```

### 5.3 中途崩了怎么办

**原地重跑同一条命令即可。** 已提交的批次保持不变，只处理还是 `pending` 的行。
run 状态会是 `applying` 或 `partial`，两者都能续跑。

```bash
go run ./cmd/migrate flatten-plugins status --run=<run-id>
```

看 `state_pending`：非 0 就是还有活没干完，重跑；0 就是干完了。

---

## 6. apply 后校验：「6738 行归档且列表数没对不上」

### 6.1 归档数对得上

```sql
SELECT row_state, count(*) FROM plugin_flatten_migration_rows
WHERE run_id = '<run-id>' GROUP BY 1;
```

期望：`applied` + `skipped` == `candidates`，**`pending` 和 `failed` 必须是 0**。

```sql
SELECT status, (parent_plugin_id IS NULL) AS unlinked, count(*)
FROM capability_items GROUP BY 1,2 ORDER BY 3 DESC;
```

判据：
- `archived + unlinked=true` 的行数 == `applied`
- 仍带 `parent_plugin_id` 的行数 == `ambiguous` + apply 期冲突数

### 6.2 一行都没被删（**这条必查**）

```sql
SELECT count(*) FROM capability_items;      -- 与 apply 前一致
SELECT count(*) FROM item_favorites;        -- 与 apply 前一致
SELECT count(*) FROM item_distributions;    -- 与 apply 前一致
```

**三个数一个都不能变。** 变了就是出了本迁移设计之外的事，立即停并上报。

### 6.3 列表数"对不上"是预期的，要能解释清楚

前台可见条目会**少 6735 条左右**。这是清理，不是数据丢失。给运营/客服的口径：

> 这些条目是插件包内部的文件（技能、命令、子代理等），过去被错误地当成独立能力上架。
> 插件本身和它的全部内容一个字节都没变，安装插件后这些文件照常工作。
> 变的只是它们不再单独出现在列表里。

**发布说明里必须把 cleanup 和 data loss 区分开**，否则第一个看到数字掉的人会当成事故。

#### 验证内容真的没丢 —— **两类 plugin 查法不同，别用错**

先看这个 plugin 是哪一类：

```sql
SELECT id, slug, source_type,
       (SELECT count(*) FROM capability_assets a WHERE a.item_id = p.id) AS assets,
       (metadata ? 'install') AS has_install,
       metadata->'install'->>'method' AS install_method
FROM capability_items p WHERE p.id = '<父 plugin id>';
```

**A. catalog 派生（`source_type='direct'`，本轮 6716 条子项的父全是这一类，共 693 个）**

判据：`assets = 0`、`has_install = t`、`install_method = 'plugin_marketplace'`。

**`assets = 0` 是正常的，不是内容丢失。** 这类 plugin 的行本身只存 `.plugin.json`（约 800 字节），
真正的插件内容在上游 marketplace 仓库里，csc 按 `metadata.install` 的坐标去拉：

```sql
SELECT jsonb_pretty(metadata->'install') FROM capability_items WHERE id = '<父 plugin id>';
-- {"method":"plugin_marketplace","marketplace":"@owner/repo","plugin_name":"…", …}
```

⇒ **安装路径根本不经过被归档的那些子项行**，所以归档它们对"装完之后有什么"零影响。
判据：`metadata.install` 在 apply 前后**逐字节相同**（本迁移只写 `status` 和 `parent_plugin_id` 两列）。

```sql
-- 693 个父 plugin 的 install 坐标应当 100% 完好
SELECT count(*) FILTER (WHERE metadata ? 'install') AS with_install, count(*) AS total
FROM capability_items
WHERE id IN (SELECT DISTINCT parent_plugin_id FROM plugin_flatten_migration_rows WHERE run_id = '<run-id>');
```

判据：两个数相等（基线 693 = 693）。

> 计划里有 **694** 个不同的 `parent_plugin_id`，但只有 **693** 行真的存在 ——
> 差的那一个就是 §4.4 里唯一那条 ambiguous 的悬空父。`IN` 子查询自然跳过它，
> 所以这里两个数都是 693，**不是漏了一条**。

**B. archive 上传（`source_type='archive'`，本轮真实数据 0 条，但代码路径仍在）**

这类 plugin 的内容**确实**在 `capability_assets` 里，子项行只是同一批字节的第二个身份：

```sql
SELECT rel_path FROM capability_assets WHERE item_id = '<父 plugin id>' ORDER BY rel_path;
```

判据：`skills/*/SKILL.md`、`commands/*.md`、`.mcp.json` 这些**仍在**。

> 两类的结论一样（装完之后什么都没变），但**理由不同**，查法也不同。
> 拿 A 类去查 `capability_assets` 会得到空结果 —— 那是这类 plugin 本来就没有资产，
> **不是**迁移删了东西。别在这里报警。

### 6.4 冲突行要逐条看

```sql
SELECT item_id, conflict FROM plugin_flatten_migration_rows
WHERE run_id = '<run-id>' AND conflict <> '';
```

冲突 = plan 到 apply 之间有人改了这行，CAS 拒绝写入，**行保持第三方留下的样子**。
这是设计行为，不是失败。处理方式：确认那个改动是正常业务后，**重新 plan 一个新 run** 收尾。

> 有冲突时 run 状态仍是 `applied` 而不是 `partial`：冲突跳过是**已决定**的结果。
> `partial` 专指还有 `pending`。冲突数在 `totals.apply_conflicts` 里单独计
> （不能只看 `state_skipped`，那里还混着计划期就跳过的 ambiguous 行）。

### 6.5 ambiguous 行的处置（**唯一留给人的决定**）

apply 不碰它们。按 reason 选处置：

| reason 形态 | 建议 |
|---|---|
| `parent row … does not exist` | 父 plugin 已被删、子行没跟着走。人工确认后**只解链、别归档** —— 它可能是用户自己 fork 的东西 |
| `status=banned is a moderation decision` | 别动。等封禁解除后再纳入下一轮 plan |
| `fork … is not itself a package child` | 可能是用户 fork 了一个独立能力。**找到用户问**，别猜 |
| `partial Git identity` | 半迁移状态。查 `git_capability_repositories` 补齐坐标后重新 plan |

⚠️ **手工 `UPDATE` 是这份手册里唯一不经过 run 记录的写操作**，所以它必须自己留痕：

```sql
-- 例：解链一条悬空父的行。改之前先把当前值抄下来
SELECT id, status, parent_plugin_id FROM capability_items WHERE id = '<item-id>';
UPDATE capability_items SET parent_plugin_id = NULL
 WHERE id = '<item-id>' AND parent_plugin_id = '<抄下来的那个值>';   -- 带上 CAS，别裸改
```

- **必须带 `AND parent_plugin_id = <旧值>`**，理由和工具里的 compare-and-set 一样。
- 把「谁、什么时候、为什么、改了哪些 id」写进工单，并回填到 run 的 `notes` 列：

```sql
UPDATE plugin_flatten_migration_runs
   SET notes = notes || E'\n<日期> 手工解链 <item-id>：父 <parent-id> 已不存在，工单 #NNN'
 WHERE id = '<run-id>';
```

  不留痕的话，下一个人做 rollback 时会发现一条状态对不上的行，而没有任何东西能解释它。

处置完重新 `plan` 一个新 run 即可 —— plan 是幂等的、只读的。

---

## 7. rollback

### 7.1 什么时候用

- apply 之后发现分类规则错了（比如某类 `derived_catalog` 其实不该归档）
- 产品临时叫停，要恢复原状

**不该用的时候**：只是想改几条 ambiguous 的处置 —— 那些行 apply 根本没碰过，rollback 也不会碰。

### 7.2 怎么做

```bash
# 1) 规划（只读）
go run ./cmd/migrate flatten-plugins rollback-plan \
  --run=<原 run-id> \
  --artifact=/path/to/flatten-rollback-YYYYMMDD.json \
  --report-limit=0

# 2) 空跑
go run ./cmd/migrate flatten-plugins rollback-apply --run=<rollback-run-id> --report-limit=0

# 3) 真跑
go run ./cmd/migrate flatten-plugins rollback-apply \
  --run=<rollback-run-id> --confirm \
  --artifact=/path/to/flatten-rollback-YYYYMMDD.json --report-limit=0
```

### 7.3 限制（**这三条要先讲清楚，别到跟前才发现**）

1. **只恢复原 run 里 `row_state=applied` 的行。** 跳过的行迁移根本没改过，
   "恢复"它们等于写一个迁移从未造成的状态。
2. **CAS 打在迁移后的状态上。** 迁移之后有人正当改过的行会**被跳过并报冲突**，不会被覆盖回去。
   本地实测：迁移后手工把 2 条归档行改回 active，rollback 就跳过这 2 条（`applied=6733 skipped=2`）。
3. **有 30 天兼容窗口。** 超期 `rollback-plan` 直接报错，要 `--force`。
   `--force` **只能**越过时间窗口，**不能**越过 digest 不符或 CAS 失败 —— 那两个是正确性，不是策略。

### 7.4 rollback 之后

run 状态变成 `rolled_back`。**两张工具表都不自动清理**，
整个兼容窗口 + 验收签字期间都要留着 —— 它们是这次操作的唯一完整记录。

---

## 8. 一页速查

```bash
# 看所有 run
go run ./cmd/migrate flatten-plugins status

# 看某个 run 的完整计数
go run ./cmd/migrate flatten-plugins status --run=<run-id> --report-limit=0

# 完整流程
flatten-plugins plan --artifact=X.json --report-limit=-1 --created-by=me
  → §4 审阅（总数 / 五分类 / independent / ambiguous / 收藏下发 / artifact 自校验）
flatten-plugins apply --run=R --report-limit=0                       # 空跑
flatten-plugins apply --run=R --confirm --artifact=X.json            # 真跑
  → §6 校验（归档数 / 三张表行数不变 / 冲突逐条看）
flatten-plugins rollback-plan  --run=R  --artifact=Y.json            # 需要时
flatten-plugins rollback-apply --run=RB --confirm --artifact=Y.json
```

**任何一步不满足判据 → 停 → 上报。这个迁移没有"先跑跑看"的模式，`plan` 就是那个模式。**

---

## 9. 本地已验证到什么程度（交接用）

| 项 | 环境 | 结果 |
|---|---|---|
| plan 真实数据 | 真实 `costrict_db`，`search_path=<scratch>,public` | 6739 候选，分类分布如 §3；共享 schema **零写入**，跑完 drop scratch |
| plan 可复现 | 同上，多次 + 跨 schema | digest 恒为 `bf3a0454…`，分布一致 |
| apply | 隔离 schema，**真实 6739 行的完整副本** | `applied=6735 skipped=3`，注入的 3 处并发改动全部被 CAS 拒绝并原样保留 |
| rollback | 同上 | `applied=6733 skipped=2`，迁移后改过的 2 行未被覆盖；最终计数逐项对上 |
| 无硬删 | 同上 | `capability_items` / `item_favorites` / `item_distributions` 行数全程不变 |
| 崩溃续跑 | 隔离 schema 单测 | 手工提交一批后置 `applying`，续跑不重复触碰已完成行，收敛为 `applied` |
| CAS 非空测 | 变异测试 | 把 `AND status = ?` 改成 `AND (status = ? OR TRUE)`，测试立即红并报"并发改动被覆盖" |
| 篡改防护 | 单测 | 改库里的计划行 / 改 artifact 文件，apply 都在写第一行之前拒绝 |

**没验过的**：生产规模下的耗时（本地 6739 行秒级；真实库若量级相同，不预期有差异）。
