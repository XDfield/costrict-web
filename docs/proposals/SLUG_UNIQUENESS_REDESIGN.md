# Slug 唯一性重构方案

> 将 `capability_items.slug` 从全局唯一改为 `(repo_id, item_type, slug)` 复合唯一。

## 变更摘要

| 维度 | 现状 | 目标 |
|---|---|---|
| DB 唯一约束 | `slug` 全局唯一 (`idx_item_slug_global`) | `(repo_id, item_type, slug)` 复合唯一 |
| `capability_items` 表 | 无 `repo_id` 字段 | 新增 `repo_id` 冗余字段（来源于 `capability_registries.repo_id`） |
| 下载路由 | `/registry/:repo/:slug/*file` | `/registry/:repo/:itemType/:slug/*file` |
| index.json | `indexItem` 含 `slug`, `type`, `files` | 不变（已含 type 信息，客户端可拼出新 URL） |

## 逐文件改动清单

### 后端 (costrict-web/server) — 7 个文件

| # | 文件 | 改动点 | 行数估算 |
|---|---|---|---|
| 1 | `internal/models/models.go` | `CapabilityItem` 新增 `RepoID` 字段 + 复合唯一索引 gorm tag | ~5 行 |
| 2 | `cmd/api/main.go` | (a) 删旧索引迁移 (b) 新增 `repo_id` 回填迁移 (c) 更新 `deduplicateSlugs` 适配新复合键 (d) 路由改为 `/registry/:repo/:itemType/:slug/*file` | ~40 行 |
| 3 | `internal/handlers/capability_item.go` | (a) `persistNewItem` 入参加 `RepoID`，UNIQUE 错误检测不变 (b) `CreateItem` / `createItemFromJSON` / `createItemFromZip` 三个入口需查 registry→repo_id 填入 (c) `MoveItemToRegistry` 和 `TransferItemToRepo` 两处手动冲突检查改为 `WHERE repo_id=? AND item_type=? AND slug=?`，且 move/transfer 后需更新 `repo_id` | ~50 行 |
| 4 | `internal/handlers/registry.go` | (a) `DownloadRegistryFile` 从 URL 取 `itemType` 参数，查询加 `item_type=?` (b) Swagger 注释更新 | ~15 行 |
| 5 | `internal/services/sync_service.go` | 新建 item 时填入 `RepoID`（从 registry 关系获取） | ~5 行 |
| 6 | `internal/handlers/capability_item_test.go` | slug 冲突测试改为同 repo+type 下冲突，新增跨 repo 同 slug 不冲突的用例 | ~40 行 |
| 7 | `internal/handlers/registry_test.go` | (a) 建表 DDL `UNIQUE(slug)` 改为复合唯一 (b) 测试路由路径更新 (c) 下载测试适配新 URL | ~30 行 |

### 后端自动生成 — 1 步

| # | 文件 | 操作 |
|---|---|---|
| 8 | `docs/` (swagger) | `swag init` 重新生成 swagger.json / swagger.yaml / docs.go |

### 前端 (opencode/packages/app) — 1 个文件

| # | 文件 | 改动点 | 行数估算 |
|---|---|---|---|
| 9 | `src/pages/store/lib/api.ts` | `CapabilityItem` 接口新增 `repoId?: string` 字段 | ~2 行 |

`item-card.tsx` 和 `item-detail.tsx` 的 `installCmd` 已含 `itemType`，无需改动。

### 设备端 (opencode/packages/opencode) — 0 个文件

设备端不直接调用 `/registry/:repo/:slug/*file`：

- `Discovery.pull` 使用 GitHub raw 的 index.json 格式，与 server registry API 无关。
- `pusher.ts` 调用 `/api/items`（POST），不涉及 slug 路由。

### 确认不需要改动的文件

| 文件 | 原因 |
|---|---|
| `internal/llm/tools.go` | 仅 `GeneratedSkill` 结构体定义，slug 字段语义不变 |
| `internal/llm/prompts.go` | LLM prompt 模板，不涉及唯一性约束 |
| `internal/services/parser_service.go` | `InferSlug` / `slugifyKey` — slug 生成逻辑不变 |
| `internal/services/search_service.go` | 原始 SQL SELECT 列表，可选加 `repo_id` 但非必需 |

## 总量

- **后端改动文件**: 7 + swagger 重生成
- **前端改动文件**: 1（仅接口类型加字段）
- **设备端改动文件**: 0
- **预计代码变更**: ~190 行（含测试）
- **风险等级**: 中等 — DB schema 变更需要迁移回填

## 数据迁移策略

### 回填 `repo_id`

```sql
UPDATE capability_items ci
SET repo_id = cr.repo_id
FROM capability_registries cr
WHERE ci.registry_id = cr.id;
```

对于 `capability_registries.repo_id` 为空的记录（如 public registry），使用 `"public"` 作为 fallback。

### 去重策略

在新复合索引 `(repo_id, item_type, slug)` 下，原先全局唯一的 slug 不会有冲突。但如果存在同一 repo 下不同 registry 有同名 slug+itemType 的情况（一个 repo 可以有 internal + external sync 两个 registry），需要先排查。

去重规则：同组中最早创建的记录保留原 slug，后续记录追加 `-2`, `-3` 后缀。

### 索引变更顺序

1. 回填 `repo_id`（含 fallback）
2. 去重同 `(repo_id, item_type, slug)` 组合
3. 删除旧索引 `idx_item_slug_global`
4. AutoMigrate 创建新复合索引

## 关键风险点

1. **数据迁移**: 已有 item 的 `repo_id` 必须从 `capability_registries.repo_id` 回填。如果有 registry 的 `repo_id` 为空（如 public registry），需要明确 fallback 值（建议用 `"public"`）。
2. **事务一致性**: `TransferItemToRepo` 需要同时更新 `registry_id` 和 `repo_id`，必须在同一事务中。
3. **向后兼容**: URL 从 `/registry/:repo/:slug/*file` 变为 `/registry/:repo/:itemType/:slug/*file` 是 breaking change。如果有外部系统调用旧 URL，需要考虑过渡期兼容（301 重定向或同时保留旧路由）。
