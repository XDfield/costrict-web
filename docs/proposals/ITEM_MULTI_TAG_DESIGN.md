# Capability Item 多标签支持设计（修订版）

## 1. 概述

本文档描述 capability item 多标签支持的修订设计。

相较于早期方案，本次设计将标签系统收敛为：

- 轻量级标签字典
- 三类标签：`system`、`builtin`、`custom`
- 面向 slug 的查询与过滤
- 标签管理接口仅系统管理员可调用
- 普通用户不得通过接口向 item 绑定 `system` 类型标签

设计目标是在保留 item 多标签过滤能力的同时，降低模型复杂度，减少国际化与描述字段维护成本，并增强平台治理能力。

---

## 2. 目标

- 支持 capability item 拥有多个标签。
- 标签类型调整为：`system`、`builtin`、`custom`。
- 提供标签列表查询接口，支持关键字匹配与分页/数量限制。
- 在 item 列表 API 中支持基于标签的过滤。
- 在注册表同步时，从 `SKILL.md` 和 `plugin.json` 的 `tags` 中提取标签。
- 在直接创建 item（JSON / 文件上传）时支持设置标签。
- 通过 migration 初始化系统标签。
- 保持与现有 `item_type` 和 `category` 字段的向后兼容。

---

## 3. 数据模型

### 3.1 标签字典表（`item_tag_dicts`）

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | UUID | 主键，自动生成 |
| `slug` | TEXT | 唯一标识，如 `skill`、`http-client` |
| `tag_class` | TEXT | `system` \| `builtin` \| `custom` |
| `created_by` | TEXT | `system` 或用户 ID |
| `created_at` | TIMESTAMPTZ | 创建时间 |

说明：

- 移除 `names`
- 移除 `descriptions`
- 移除 `updated_at`

### 3.2 Item-标签关联表（`item_tags`）

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | UUID | 主键 |
| `item_id` | UUID | 外键 → `capability_items.id` |
| `tag_id` | UUID | 外键 → `item_tag_dicts.id` |
| `created_at` | TIMESTAMPTZ | 创建时间 |

约束：

- (`item_id`, `tag_id`) 唯一索引
- 外键使用 `ON DELETE CASCADE`
- `tag_id` 索引支持反向查询

### 3.3 CapabilityItem 模型扩展

```go
type CapabilityItem struct {
    // ... existing fields ...
    Tags []ItemTagDict `gorm:"-" json:"tags,omitempty"`
}
```

该字段不由 GORM 持久化，仅在查询后批量填充并序列化到响应中。

---

## 4. 标签类型

| 类型 | 说明 | 示例 |
|---|---|---|
| `system` | 系统保留标签，仅管理员可配置到 item | `official`、`best-practice` |
| `builtin` | 系统内置标准标签，用户可选择配置到 item | `planning`、`design`、`development` |
| `custom` | 用户自定义标签，以及旧设计中的 `functional` 标签 | `auth`、`http-client`、`team-alpha` |

说明：

- 旧设计中的 `functional` 类型被移除，统一归并到 `custom`
- `system` 标签不允许普通用户通过接口直接分配给 item
- `builtin` 标签为平台预置标准标签，普通用户可以选择配置
- `item_type` 不再自动映射为任何 tag

---

## 5. slug 规范

标签 slug 必须满足以下约束：

- 仅允许小写字母、数字、中划线、下划线
- 不允许空格
- 不允许其他特殊字符

正则规则：

```regex
^[a-z0-9_-]+$
```

建议统一处理流程：

1. `TrimSpace`
2. 转小写
3. 按正则校验

非法时返回：

```json
{
  "error": "Tag slug may only contain lowercase letters, numbers, hyphens, and underscores",
  "code": "invalid_tag_slug"
}
```

---

## 6. 数据库迁移与初始化

迁移文件：`server/migrations/20260422100000_create_item_tags.sql`

### 6.1 Up 阶段

1. 创建 `item_tag_dicts` 与 `item_tags`
2. 初始化 `system` 标签：
   - `official`
   - `best-practice`
3. 初始化 `builtin` 标签：
   - `planning`
   - `design`
   - `development`
   - `testing`
   - `staging`
   - `release`
   - `maintenance`

### 6.2 Down 阶段

按顺序删除：

1. `item_tags`
2. `item_tag_dicts`

### 6.3 初始化要求

系统标签初始化必须在 migration 中完成，保证：

- 新环境迁移后可立即使用
- 不依赖额外手工步骤

---

## 7. 服务层（TagService）

位置：`server/internal/services/tag_service.go`

```go
type TagService struct {
    DB *gorm.DB
}
```

### 7.1 核心方法

#### ValidateTagSlug

```go
func ValidateTagSlug(slug string) error
```

- 清洗并校验 slug
- 仅允许 `[a-z0-9_-]+`

#### EnsureTags

```go
func (s *TagService) EnsureTags(slugs []string, tagClass, createdBy string) ([]models.ItemTagDict, error)
```

- 去重并过滤空值
- 校验 slug
- 查询已有标签
- 创建缺失标签
- 并发冲突时重试查询
- 按输入顺序返回

说明：

- 普通业务流程创建的标签默认应为 `custom`
- `builtin` 标签由 migration 或管理员预置，不通过普通流程自动创建
- `system` 标签由 migration 或管理员预置，仅管理员可分配

#### SetItemTags

```go
func (s *TagService) SetItemTags(itemID string, tagIDs []string) error
```

- 在事务中执行
- 删除旧关联
- 写入新关联

#### GetItemTags

```go
func (s *TagService) GetItemTags(itemIDs []string) (map[string][]models.ItemTagDict, error)
```

- 批量查询多个 item 的标签
- 返回 `itemID -> []ItemTagDict`

#### List

```go
type ListTagsOptions struct {
    Query    string
    TagClass string
    Page     int
    PageSize int
}

func (s *TagService) List(opts ListTagsOptions) ([]models.ItemTagDict, int64, error)
```

- 支持 slug 关键字匹配
- 支持按 `tagClass` 过滤
- 支持 `page` / `pageSize`

### 7.2 CRUD 权限要求

以下能力仅允许系统管理员调用：

- Create
- Update
- Delete

---

## 8. 文件解析

位置：`server/internal/services/parser_service.go`

`ParsedItem` 保持 `Tags []string` 字段。

### 8.1 SKILL.md

从 frontmatter 中读取：

```yaml
---
name: My Skill
tags:
  - rest-api
  - authentication
---
```

### 8.2 plugin.json

从 JSON 中读取：

```json
{
  "name": "My Plugin",
  "tags": ["etl", "pipeline"]
}
```

这些标签在落库时统一视为 `custom`。

---

## 9. 同步与创建规则

### 9.1 注册表同步

在 `SyncRegistry` 中：

1. 若解析到 `parsed.Tags`，调用 `EnsureTags(parsed.Tags, custom, triggerUser)`
2. 调用 `SetItemTags(itemID, tagIDs)` 建立关联
3. 不再根据 `item_type` 自动附加任何 tag

### 9.2 JSON 创建 item

接口：`POST /api/items`

请求体支持：

```json
{
  "itemType": "skill",
  "name": "My Skill",
  "tags": ["rest-api", "authentication"]
}
```

规则：

- `tags` 为可选 slug 数组
- 普通流程通过 `EnsureTags(..., custom, createdBy)` 创建或获取标签
- 之后通过 `SetItemTags` 建立 item-tag 关联

### 9.3 文件上传创建 item

接口：`POST /api/items`（multipart/form-data）

规则：

- 从 archive 中解析 `SKILL.md` / `plugin.json` 的 `tags`
- 解析出的标签统一作为 `custom`
- 不额外通过表单字段传入 tags

---

## 10. 权限规则

### 10.1 标签管理接口

以下接口仅系统管理员可调用：

- `POST /api/tags`
- `PUT /api/tags/:id`
- `DELETE /api/tags/:id`

### 10.2 item 标签分配限制

接口：

- `POST /api/items/:id/tags`
- `POST /api/items`（JSON）
- `POST /api/items`（multipart）

规则：

- 普通用户可为 item 分配 `builtin` 与 `custom` 标签
- 只有系统管理员可以通过接口为 item 分配 `system` 标签
- 非系统管理员请求中的 `system` 标签会被静默过滤

普通用户尝试通过接口提交 `system` 标签时，不报错，系统会在数据层静默过滤这些标签。

---

## 11. API 设计

### 11.1 标签列表接口

```http
GET /api/tags?q=auth&tagClass=custom&page=1&pageSize=20
```

Query 参数：

- `q`：按 `slug` 关键字匹配
- `tagClass`：可选，`system` / `custom`
- `page`：可选，默认 `1`
- `pageSize`：可选，默认 `20`，最大建议 `100`

返回示例：

```json
{
  "tags": [
    {
      "id": "uuid",
      "slug": "auth-client",
      "tagClass": "custom",
      "createdBy": "u1",
      "createdAt": "2026-04-22T10:00:00Z"
    }
  ],
  "total": 132,
  "page": 1,
  "pageSize": 20,
  "hasMore": true
}
```

### 11.2 标签管理接口

| 方法 | 接口 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/tags` | 公开可读 | 列表查询，支持关键字匹配与分页 |
| GET | `/api/tags/:id` | 公开可读 | 按 ID 查询单个标签 |
| POST | `/api/tags` | 系统管理员 | 创建新标签 |
| PUT | `/api/tags/:id` | 系统管理员 | 更新标签 |
| DELETE | `/api/tags/:id` | 系统管理员 | 删除标签 |

### 11.3 Item 标签设置接口

```http
POST /api/items/:id/tags
```

请求体：

```json
{
  "tagIds": ["uuid1", "uuid2"]
}
```

或：

```json
{
  "tags": ["slug1", "slug2"]
}
```

规则：

- 若提供 `tags`，不存在的 slug 通过 `EnsureTags(..., custom, createdBy)` 创建
- 已存在的 `builtin` 标签可被普通用户直接绑定
- 非系统管理员提交的 `system` 标签会被静默过滤
- 返回 item 更新后的标签列表

---

## 12. Item 列表过滤

公开和个人 item 列表接口均支持标签过滤：

- `GET /api/items?tags=rest-api,authentication`
- `GET /api/items/my?tags=mcp,hook`

过滤条件通过 `item_tag_dicts.slug IN (...)` 进行匹配。

### 12.1 多标签过滤语义

当前采用 **OR 语义**：

- 传入多个 tag slug 时，只要 item 拥有任意一个标签即可命中

示例 SQL：

```sql
id IN (
    SELECT item_id
    FROM item_tags
    JOIN item_tag_dicts ON item_tags.tag_id = item_tag_dicts.id
    WHERE item_tag_dicts.slug IN ('rest-api', 'auth')
)
```

---

## 13. 查询与响应增强

### 13.1 虚拟 Tags 字段

`CapabilityItem.Tags` 为虚拟字段：

```go
Tags []ItemTagDict `gorm:"-" json:"tags,omitempty"`
```

查询时采用：

1. 先查 item 主记录
2. 批量查询 `item_tags`
3. 批量查询 `item_tag_dicts`
4. 回填到响应结构体

### 13.2 当前覆盖范围

建议至少在以下接口中支持 tags 返回：

- `GET /api/items`
- `GET /api/items/:id`
- `GET /api/items/my`

创建类接口如需返回 tags，可在创建成功后补充一次批量查询或单项查询。

---

## 14. 关键设计决策

### 14.1 去掉国际化与描述字段

标签系统仅作为轻量分类与过滤字典使用，不再承载展示型多语言元数据。

### 14.2 标签类型收敛

将旧设计中的 `functional` 并入 `custom`，避免类型边界模糊。

### 14.3 严格治理 system 标签

`system` 标签属于平台保留语义，普通用户不得通过接口直接分配。

### 14.4 面向 slug 的查询

外部接口统一使用 slug，而非内部 UUID，以提升可读性与稳定性。

### 14.5 保持兼容

保留现有 `item_type` 与 `category` 字段，不破坏既有逻辑；标签作为增强维度逐步接入。

---

## 15. 文件变更清单（目标）

| 文件 | 变更 |
|---|---|
| `server/internal/models/models.go` | 精简 `ItemTagDict` 字段；保留 `CapabilityItem.Tags` 虚拟字段 |
| `server/internal/services/tag_service.go` | 增加 slug 校验；列表查询支持关键字与分页；支持 `system/builtin/custom` 三类标签 |
| `server/internal/services/parser_service.go` | 保持 tags 解析逻辑 |
| `server/internal/services/sync_service.go` | 将同步提取标签统一落为 custom；不再自动附加 item_type 对应 tag |
| `server/internal/handlers/tag.go` | 列表接口支持 `q/page/pageSize`；system 仅管理员可配，builtin/custom 可按规则绑定 |
| `server/internal/handlers/capability_item.go` | item 创建时 tags 仅自动创建 custom；builtin 可直接绑定；非管理员提交的 system tag 静默过滤 |
| `server/internal/handlers/capability_registry.go` | 继续支持 `?tags` 过滤 |
| `server/cmd/api/main.go` | 注册 tags 写接口，并接入系统管理员权限中间件 |
| `server/migrations/20260422100000_create_item_tags.sql` | 简化表结构；migration 中初始化 system 与 builtin tags |
