# Catalog Ingest — 上游 catalog 同步到 costrict-web 的端到端链路

本文档描述 `costrict-web` 从上游 `costrict-skills-repo`（[`everything-ai-coding`](https://github.com/zgsm-ai/everything-ai-coding)）拉取 catalog 数据落地到本地 `capability_items` 表的完整流程。

替代旧的 `migrate import-everything-ai-coding` 路径（fake-git/fake-registry 双层中转），改成基于 bundle tarball 的直接 ingest。

## 链路总览

```
┌──────────────────────────── 上游 costrict-skills-repo ────────────────────────────┐
│                                                                                    │
│   各 source listing (mcp registry / mastra / antigravity / …)                      │
│        │                                                                           │
│        ▼                                                                           │
│   merge_index.py  ──→  catalog/index.json (顶层 9000+ entries 含元数据 + security) │
│        │                                                                           │
│        ▼                                                                           │
│   download_catalog.py                                                              │
│        ├─ _download_mcp:    缺 command/url → 不写文件，返回失败                    │
│        ├─ _download_skill / _download_rule / _download_prompt / _download_plugin   │
│        ├─ → 文件落到 catalog-download/<type-dir>/<id>/...                          │
│        └─ _filter_top_index_to_downloaded:  用磁盘真实文件作 ground truth          │
│             回写 catalog/index.json，剔除下载失败的孤儿 entry                       │
│                                                                                    │
│   build_catalog_bundle.py                                                          │
│        ├─ 再过滤一道（orphan / mcp_empty_stub / md_yaml_broken）作为 safety net    │
│        └─ → dist/catalog-bundle.tar.gz                                             │
│             ├─ manifest.json   (schema_version, entry_count, index_sha256, …)     │
│             ├─ index.json       (per-entry metadata + security 块)                 │
│             └─ catalog-download/  (per-entry primary file + assets)                │
└────────────────────────────────────────┬───────────────────────────────────────────┘
                                         │   HTTPS / local file / dir
                                         ▼
┌──────────────────────────────── costrict-web ─────────────────────────────────────┐
│                                                                                    │
│   migrate ingest-upstream --source=<url|tarball|dir>                               │
│        │                                                                           │
│        ▼                                                                           │
│   services.CatalogIngestService.Ingest(ctx, src, opts)                             │
│        ├─ materialize:    http GET → 解压 → 临时目录                               │
│        ├─ readManifest:   校验 schema_version                                      │
│        ├─ readIndex:      解析 9000+ entries                                       │
│        ├─ diff:           per-entry fileSHA vs db.source_sha                       │
│        │     ├─ unchanged:        skip / metadata-only update                      │
│        │     ├─ changed:          reparse + bump version + enqueue scan            │
│        │     ├─ new:              insert                                           │
│        │     └─ upstream removed: soft archive (status='archived')                 │
│        └─ result: {added, updated, metadataUpdated, skipped, deleted,              │
│                    failed, incomplete}                                             │
│                                                                                    │
│   capability_items (PublicRegistryID = 00000000-…-0001)                            │
└────────────────────────────────────────────────────────────────────────────────────┘
```

## 上游侧

仓库 `costrict-skills-repo`，分支 `feat/catalog-bundle`（一次性流程示例）。

```bash
cd /path/to/costrict-skills-repo

# 全量下载 + 自动 reconciliation
python3 scripts/download_catalog.py

# 打 bundle
python3 scripts/build_catalog_bundle.py
# → dist/catalog-bundle.tar.gz
```

`download_catalog.py` 的关键不变量：
- **`_download_mcp` 缺 install 信息直接拒绝**：mcp entry 的 `install.config` 既没有 `command` 也没有 `url` 时不写文件，避免下游 NormalizeMCPMetadata 失败。
- **`_filter_top_index_to_downloaded` 末尾回填**：用磁盘真实文件作 ground truth 重写顶层 `catalog/index.json`，保证 index 和 catalog-download/ 永远一致。

`build_catalog_bundle.py` 三道安全网：
- `orphan_no_file` — index.json 列了但 catalog-download/ 没有文件的 entry
- `mcp_empty_stub` — JSON 解析后 `mcpServers.<name>` 是空对象
- `md_yaml_broken` — PROMPT.md / SKILL.md / RULE.md 的 frontmatter PyYAML 解析失败

## 下游侧

仓库 `costrict-web`，分支 `feat/catalog-ingest-refactor`。

### 准备：本地 Casdoor + Postgres + Redis

详见 [`LOCAL_DEV_CASDOOR.md`](LOCAL_DEV_CASDOOR.md)，确保 Postgres 可达 + `migrate` 跑过基础 schema。

### 拉数据

```bash
cd server
cp .env.example .env
cp .env.local.example .env.local

set -a; . ./.env; . ./.env.local; set +a

# 本地 tarball（路径换成你 costrict-skills-repo checkout 的 dist/ 目录）
go run ./cmd/migrate ingest-upstream \
  --source=../../costrict-skills-repo/dist/catalog-bundle.tar.gz

# 本地目录（已解压的 bundle，dev 调试用）
go run ./cmd/migrate ingest-upstream --source=/path/to/extracted-bundle/

# 远端 URL（release 后）
go run ./cmd/migrate ingest-upstream \
  --source=https://github.com/zgsm-ai/everything-ai-coding/releases/download/catalog-2026-W21/catalog-bundle.tar.gz

# 干跑：不写 DB 只算 diff
go run ./cmd/migrate ingest-upstream --source=... --dry-run

# 错误清单写文件
INGEST_ERROR_LOG=/tmp/ingest-errors.log go run ./cmd/migrate ingest-upstream --source=...
```

### 输出说明

```
ingest-upstream summary (dry-run=false): entries=3325 added=0 updated=18 \
  metadataUpdated=0 skipped=3307 deleted=0 failed=0 incomplete=0 duration=2.4s
```

| 字段 | 含义 |
|---|---|
| `entries` | 本次 bundle manifest 声明的 entry 总数 |
| `added` | DB 没有、本次新建的 capability_items 行 |
| `updated` | DB 有、文件 sha 变化、内容 + 元数据全部刷新、capability_versions revision +1 |
| `metadataUpdated` | DB 有、文件 sha 未变、仅 metadata 字段刷新（source / description / category / tags / experience_score） |
| `skipped` | DB 有、文件和元数据都没变化、不动 |
| `deleted` | DB 有、但本次 bundle 不包含 → 软归档 `status='archived'`（保留 favorites / scan 历史） |
| `failed` | **真正的 ingest 代码错误**（DB 写失败、tar 解压错误等）。> 0 一定要查 |
| `incomplete` | **上游数据问题**导致 entry 落不了地（已识别的 signature：mcp 缺 install / PROMPT 帧 YAML 错）。**不是 ingest bug**，定期反馈给上游 |

`failed` 和 `incomplete` 的区分见 `services.isUpstreamDataIncomplete()`，列表必要时可扩展。

## 数据契约

### bundle 内部布局

```
catalog-bundle.tar.gz
├── manifest.json
│   {
│     "schema_version": 1,
│     "generated_at": "2026-05-21T11:23:45Z",
│     "entry_count": 3325,
│     "index_sha256": "70a92ded94a3ee5d...",
│     "type_counts": {"skill": 1652, "mcp": 601, "plugin": 633, "prompt": 385, "rule": 54},
│     "filtered_from": 9381,
│     "orphan_dropped": 0,
│     "unknown_type_dropped": 0
│   }
├── index.json   (过滤后的 entries 数组)
└── catalog-download/
    ├── mcp/<id>/.mcp.json
    ├── skills/<id>/SKILL.md  (+ references/, scripts/ 等附件)
    ├── plugins/<id>/.plugin.json
    ├── prompts/<id>/PROMPT.md
    └── rules/<id>/RULE.md
```

升级 `schema_version` 时，`services.SupportedBundleSchemaVersion` 也要同步升，否则 ingest 会拒绝执行（避免向后不兼容字段写脏数据）。

### type → 目录 / 文件名映射

| index.json `type` | catalog-download 目录 | primary 文件 |
|---|---|---|
| `mcp` | `mcp/<id>/` | `.mcp.json` |
| `skill` | `skills/<id>/` | `SKILL.md` |
| `plugin` | `plugins/<id>/` | `.plugin.json` |
| `prompt` | `prompts/<id>/` | `PROMPT.md` |
| `rule` | `rules/<id>/` | `RULE.md` |

下游 `ParserService.InferItemType` 当前把 `PROMPT.md` / `RULE.md` 也归到 `item_type=skill`（前端 store 只有 5 个 tab，没有独立的 Prompt / Rule tab）。一旦前端加上对应 tab，可在 InferItemType 里恢复独立分类。

### 上游不提供的类型

| 类型 | 状态 | 说明 |
|---|---|---|
| `subagent` | 永远 0 | 上游不收集 agents/*.md，依赖用户上传 |
| `command` | 永远 0 | 同上 |
| `hook` | 永远 0 | 同上 |

`/store` 页面 Subagents / Commands tab 显示 "No items were found" 是预期行为。

## 验证 ingest 结果

数据库分布：

```sql
SELECT item_type, status, count(*)
  FROM capability_items
  WHERE registry_id = '00000000-0000-0000-0000-000000000001'
  GROUP BY 1, 2
  ORDER BY 1, 2;
```

预期参考值（基于 2026-05-21 bundle）：

| item_type | status | count |
|---|---|---|
| mcp | active | ~593 |
| plugin | active | ~633 |
| skill | active | ~2086 (= 真 skill 1663 + prompt 369 + rule 54 合并) |

前端：浏览器打开 http://localhost:3000/store，5 个 tab 应当显示：

| Tab | 数据 |
|---|---|
| Skills | ~2086 items |
| Subagents | No items found（预期）|
| Commands | No items found（预期）|
| MCP Servers | ~593 items |
| Plugins | ~633 items |

## 失败排查

### `failed > 0`

真 bug，查 `INGEST_ERROR_LOG=...` 给出的具体行：

- `read <path>: no such file` → bundle 内 manifest 和文件不一致，重 build bundle
- `parse <path>: …` → 不在已识别 incomplete signature 内，让 parser / `isUpstreamDataIncomplete` 加一条
- `insert / update <id>: …` → 通常是数据库约束冲突（unique slug 等），看 DB 现状

### `incomplete > 0`

上游数据问题。`INGEST_ERROR_LOG` 出来的 `[incomplete]` 行能定位到具体 entry，反馈给上游 `costrict-skills-repo` 改 `download_catalog.py` 或 `merge_index.py`。下游不应该容忍它"消失"，因此专门 counter 暴露出来。

### `deleted > 0` 突然变大

bundle 比上次少了大量 entry。检查上游是不是改了某个 source 的 listing 逻辑、或某个 source 短暂下线。soft-archive 不掉数据，再来一次正常 bundle 即可恢复。

## 重构前后对比

| 维度 | 旧 (import-everything-ai-coding) | 新 (ingest-upstream) |
|---|---|---|
| 网络协议 | git clone 全历史 | 单 HTTPS GET tarball |
| 体积 | 170 MB（含 .git） | 33 MB（gzip 后） |
| 中间步骤 | 复制 → 临时目录 → fake git init → 临时 registry → SyncRegistry → 重新 path 到 public | 解压 → diff → upsert |
| 失败分类 | 全部混在 `failed` | `failed` vs `incomplete` 分桶 |
| metadata backfill | 独立 step：`backfill-everything-ai-coding-metadata` | ingest 路径内联完成 |
| 代码量 | 540 行 fake-git/fake-registry 逻辑 | 一个 CatalogIngestService（~830 行带详尽注释） |
| 增量同步 | 全文件 walk + glob | 按 manifest entry 逐条对账，content_hash 命中即 skip |

## 相关代码与文档

- 下游 service：`server/internal/services/catalog_ingest_service.go`
- 下游 CLI：`server/cmd/migrate/main.go` → `ingestUpstreamCatalog`
- 上游 bundle 构造：`costrict-skills-repo/scripts/build_catalog_bundle.py` + `build_catalog_bundle.md`
- 上游下载流程：`costrict-skills-repo/scripts/download_catalog.py` + `download_catalog.md`
- 本地开发环境：[`LOCAL_DEV_CASDOOR.md`](LOCAL_DEV_CASDOOR.md)
