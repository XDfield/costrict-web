> **实现状态：服务端已完成**
>
> - 状态：✅ 服务端已完成
> - 实现位置：`server/internal/models/models.go`（`ItemCategory` 模型）、`server/internal/services/category_service.go`、`server/internal/handlers/category.go`
> - 说明：Category 分类字典表、CRUD API、国际化支持、所有 item 写入接口的关联已完成。

---

# Item Category 分类字典表技术提案

## 目录

- [概述](#概述)
- [背景与动机](#背景与动机)
- [架构设计](#架构设计)
- [数据模型](#数据模型)
- [API 设计](#api-设计)
- [写入接口关联](#写入接口关联)
- [数据迁移](#数据迁移)
- [文件变更清单](#文件变更清单)

---

## 概述

### 背景与动机

`CapabilityItem.Category` 原为自由文本字符串字段，存在以下问题：

1. **无分类字典**：category 值无统一管理，同一分类可能出现不同拼写（如 `dev`、`development`、`Development`）
2. **不支持国际化**：前端无法获取分类的多语言名称和描述
3. **无审计记录**：不知道分类是谁、何时、通过什么途径创建的

### 设计目标

- 新增独立的 `item_categories` 分类字典表，支持 i18n
- 保持 `CapabilityItem.Category` 字段为 string（存储 slug），零迁移成本
- 所有写入 item category 的接口自动确保对应分类记录存在（EnsureCategory 模式）
- 提供分类的 CRUD API

---

## 架构设计

### 松耦合设计

```
┌──────────────────┐          ┌──────────────────┐
│  CapabilityItem  │          │   ItemCategory   │
│                  │  slug    │                  │
│  category: str ──┼─ ─ ─ ─ ─▷  slug: str (UK)  │
│                  │          │  names: jsonb    │
│                  │          │  descriptions:   │
│                  │          │    jsonb          │
└──────────────────┘          └──────────────────┘
```

- **不使用外键约束**：`CapabilityItem.Category` 存储 slug 字符串，`ItemCategory.Slug` 为唯一索引
- **自动创建**：写入 item 时通过 `EnsureCategory` 自动创建缺失的分类记录
- **兼容现有数据**：现有 item 的 category 字段无需变更

### i18n 方案

采用 JSONB 存储多语言内容，与项目现有 `Metadata`、`SystemConfig` 等 JSONB 字段模式一致：

```json
{
  "names": {"en": "Development", "zh": "开发工具", "ja": "開発ツール"},
  "descriptions": {"en": "Tools for development", "zh": "开发相关工具"}
}
```

---

## 数据模型

### ItemCategory 表结构

```go
type ItemCategory struct {
    ID           string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
    Slug         string         `gorm:"not null;uniqueIndex"                           json:"slug"`
    Icon         string         `                                                      json:"icon"`
    SortOrder    int            `gorm:"default:0"                                      json:"sortOrder"`
    Names        datatypes.JSON `gorm:"type:jsonb;not null;default:'{}'"               json:"names"`
    Descriptions datatypes.JSON `gorm:"type:jsonb;default:'{}'"                        json:"descriptions"`
    CreatedBy    string         `gorm:"not null"                                       json:"createdBy"`
    CreatedAt    time.Time      `                                                      json:"createdAt"`
    UpdatedAt    time.Time      `                                                      json:"updatedAt"`
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | UUID | 主键，自动生成 |
| `slug` | string | 唯一标识（如 `development`、`testing`），与 item.category 对应 |
| `icon` | string | 图标标识（可选，供前端展示） |
| `sort_order` | int | 排序权重，默认 0 |
| `names` | JSONB | 多语言名称 `{"en": "...", "zh": "..."}` |
| `descriptions` | JSONB | 多语言描述 |
| `created_by` | string | 创建者用户 ID |
| `created_at` | timestamp | 创建时间 |
| `updated_at` | timestamp | 更新时间 |

---

## API 设计

### 公开接口（无需认证）

#### 获取分类列表

```
GET /api/categories
```

**Response 200:**
```json
{
  "categories": [
    {
      "id": "uuid",
      "slug": "development",
      "icon": "code",
      "sortOrder": 1,
      "names": {"en": "Development", "zh": "开发工具"},
      "descriptions": {"en": "Tools for development"},
      "createdBy": "user-id",
      "createdAt": "2026-04-07T00:00:00Z",
      "updatedAt": "2026-04-07T00:00:00Z"
    }
  ]
}
```

#### 获取单个分类

```
GET /api/categories/:id
```

**Response 200:**
```json
{
  "category": { ... }
}
```

### 认证接口（需登录）

#### 创建分类

```
POST /api/categories
Content-Type: application/json

{
  "slug": "development",
  "icon": "code",
  "sortOrder": 1,
  "names": {"en": "Development", "zh": "开发工具"},
  "descriptions": {"en": "Tools for development", "zh": "开发相关工具"}
}
```

**Response 201:** `{"category": {...}}`
**Response 409:** `{"error": "Category slug already exists", "slug": "development"}`

#### 更新分类

```
PUT /api/categories/:id
Content-Type: application/json

{
  "icon": "wrench",
  "sortOrder": 2,
  "names": {"en": "Dev Tools", "zh": "开发工具"},
  "descriptions": {"en": "Updated description"}
}
```

**Response 200:** `{"category": {...}}`
**Response 404:** `{"error": "Category not found"}`

#### 删除分类

```
DELETE /api/categories/:id
```

**Response 204:** 无内容
**Response 404:** `{"error": "Category not found"}`

---

## 写入接口关联

所有写入 item category 的路径均通过 `EnsureCategory(slug, createdBy)` 自动确保分类记录存在：

| 写入路径 | 文件 | 触发位置 |
|----------|------|---------|
| `CreateItem`（registry 创建） | `capability_item.go` | `persistNewItem` 后调用 `CategorySvc.EnsureCategory` |
| `createItemFromJSON`（直接创建） | `capability_item.go` | `persistNewItem` 后调用 `h.categorySvc.EnsureCategory` |
| `createItemFromArchive`（上传创建） | `capability_item.go` | category 解析完成后调用 `h.categorySvc.EnsureCategory` |
| `updateItemFromJSON`（JSON 更新） | `capability_item.go` | category 变更时调用 `h.categorySvc.EnsureCategory` |
| `updateItemFromArchive`（上传更新） | `capability_item.go` | category 赋值后调用 `h.categorySvc.EnsureCategory` |
| sync 更新已有 item | `sync_service.go` | `existing.Category = parsed.Category` 后调用 |
| sync 创建新 item | `sync_service.go` | `DB.Create(newItem)` 后调用 |

### EnsureCategory 逻辑

```go
func (s *CategoryService) EnsureCategory(slug, createdBy string) (*ItemCategory, error) {
    // 1. slug 为空 → 跳过
    // 2. 按 slug 查找 → 存在则返回
    // 3. 不存在 → 创建（slug 作为默认英文名）
    // 4. 并发竞争时 duplicate key → 重新查找返回
}
```

自动创建的分类记录默认 `names` 为 `{"en": "<slug>"}`，管理员可后续通过 API 补充多语言名称和描述。

---

## 数据迁移

### Goose 迁移脚本

文件：`server/migrations/20260407000000_seed_item_categories.sql`

从现有 `capability_items.category` 去重提取，自动生成 seed 数据：

```sql
INSERT INTO item_categories (id, slug, names, descriptions, created_by, created_at, updated_at)
SELECT
    gen_random_uuid(),
    category,
    jsonb_build_object('en', category),
    '{}'::jsonb,
    'system',
    NOW(),
    NOW()
FROM (
    SELECT DISTINCT category
    FROM capability_items
    WHERE category IS NOT NULL AND category <> ''
) AS cats
ON CONFLICT (slug) DO NOTHING;
```

---

## 文件变更清单

### 新增文件

| 文件 | 说明 |
|------|------|
| `server/internal/services/category_service.go` | CategoryService：CRUD + EnsureCategory |
| `server/internal/handlers/category.go` | Category CRUD HTTP handlers |
| `server/migrations/20260407000000_seed_item_categories.sql` | Seed 迁移脚本 |

### 修改文件

| 文件 | 变更内容 |
|------|---------|
| `server/internal/models/models.go` | 新增 `ItemCategory` struct |
| `server/cmd/migrate/main.go` | AutoMigrate 加入 `&models.ItemCategory{}` |
| `server/cmd/api/main.go` | 初始化 CategoryService，注册路由，传入 ItemHandler |
| `server/internal/handlers/capability_item.go` | ItemHandler 新增 `categorySvc` 字段；5 个写入路径调用 EnsureCategory |
| `server/internal/handlers/sync.go` | 新增包级 `CategorySvc` 变量 |
| `server/internal/services/sync_service.go` | SyncService 新增 `CategorySvc` 字段；sync 创建/更新时调用 |
| `server/cmd/worker/main.go` | SyncService 初始化时注入 CategorySvc |
| `server/internal/handlers/capability_item_test.go` | NewItemHandler 调用签名更新 |
