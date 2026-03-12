# 数据库设计文档

## 概述

服务端使用 PostgreSQL 作为数据库，通过 GORM 进行 ORM 映射。数据库分为两个层次：
- **Casdoor 层**：用户、组织等身份认证相关数据，由 Casdoor 服务管理
- **业务层**：能力注册表、条目、同步任务等核心业务数据，由 costrict-web server 管理

---

## 数据模型

### 1. Organization（组织）

组织是多租户隔离的顶层单元，对应 Casdoor 中的组织概念在本地的扩展存储。

| 字段 | 类型 | 约束 | 说明 |
|------|------|------|------|
| id | uuid | PK | 组织唯一标识 |
| name | varchar(255) | NOT NULL, UNIQUE | 组织名称（唯一） |
| display_name | varchar(255) | | 显示名称 |
| description | text | | 组织描述 |
| visibility | varchar(32) | DEFAULT 'private' | 可见性：`public` \| `private` |
| org_type | varchar(32) | DEFAULT 'normal' | 类型：`normal` \| `sync` |
| owner_id | varchar(191) | NOT NULL | 所有者用户 ID（引用 Casdoor user） |
| created_at | timestamp | autoCreateTime | 创建时间 |
| updated_at | timestamp | autoUpdateTime | 更新时间 |

**关联**：
- `members` → `OrgMember`（一对多，外键 `org_id`）

---

### 2. OrgMember（组织成员）

记录用户与组织的成员关系及角色。

| 字段 | 类型 | 约束 | 说明 |
|------|------|------|------|
| id | uuid | PK | 成员记录唯一标识 |
| org_id | uuid | NOT NULL, INDEX | 所属组织 ID |
| user_id | varchar(191) | NOT NULL | 用户 ID（引用 Casdoor user） |
| username | varchar(255) | | 用户名快照 |
| role | varchar(32) | DEFAULT 'member' | 角色：`owner` \| `admin` \| `member` |
| created_at | timestamp | autoCreateTime | 加入时间 |

---

### 3. CapabilityRegistry（能力注册表）

能力注册表是能力条目的容器，支持从外部 Git 仓库同步内容。

| 字段 | 类型 | 约束 | 说明 |
|------|------|------|------|
| id | uuid | PK, DEFAULT gen_random_uuid() | 注册表唯一标识 |
| name | varchar | NOT NULL | 注册表名称 |
| description | text | | 描述 |
| source_type | varchar | NOT NULL, DEFAULT 'internal' | 来源类型：`internal` \| `external` |
| external_url | varchar | | 外部 Git 仓库 URL |
| external_branch | varchar | DEFAULT 'main' | 同步分支 |
| sync_enabled | bool | DEFAULT false | 是否启用自动同步 |
| sync_interval | int | DEFAULT 3600 | 同步间隔（秒） |
| last_synced_at | timestamp | nullable | 上次同步时间 |
| last_sync_sha | varchar | | 上次同步的 Git commit SHA |
| sync_status | varchar | DEFAULT 'idle' | 同步状态：`idle` \| `syncing` \| `error` \| `paused` |
| sync_config | jsonb | DEFAULT '{}' | 同步配置（includePatterns、excludePatterns、conflictStrategy） |
| last_sync_log_id | uuid | nullable | 最近一次同步日志 ID |
| visibility | varchar | DEFAULT 'org' | 可见性 |
| org_id | varchar | | 所属组织 ID |
| owner_id | varchar | NOT NULL | 所有者用户 ID |
| created_at | timestamp | | 创建时间 |
| updated_at | timestamp | | 更新时间 |

**关联**：
- `items` → `CapabilityItem`（一对多，外键 `registry_id`）

**sync_config 结构**：
```json
{
  "includePatterns": ["**/*.md", "**/plugin.json"],
  "excludePatterns": ["**/node_modules/**"],
  "conflictStrategy": "keep_remote"
}
```
`conflictStrategy` 可选值：`keep_remote`（默认，远端覆盖本地）、`keep_local`（本地有修改时跳过）。

---

### 4. CapabilityItem（能力条目）

注册表中的单个能力条目，可以是 skill、agent、command 等类型。

| 字段 | 类型 | 约束 | 说明 |
|------|------|------|------|
| id | uuid | PK, DEFAULT gen_random_uuid() | 条目唯一标识 |
| registry_id | uuid | NOT NULL | 所属注册表 ID |
| slug | varchar | NOT NULL | URL 友好的唯一标识 |
| item_type | varchar | NOT NULL | 条目类型（skill / agent / command 等） |
| name | varchar | NOT NULL | 条目名称 |
| description | text | | 描述 |
| category | varchar | | 分类 |
| version | varchar | DEFAULT '1.0.0' | 版本号 |
| content | text | | 条目内容（Markdown 或 JSON） |
| metadata | jsonb | DEFAULT '{}' | 扩展元数据 |
| source_path | varchar | | 来源文件相对路径 |
| source_sha | varchar | | 内容哈希（用于增量同步判断） |
| visibility | varchar | | 可见性 |
| install_count | int | DEFAULT 0 | 安装次数 |
| status | varchar | DEFAULT 'active' | 状态：`active` \| `archived` |
| created_by | varchar | NOT NULL | 创建者 |
| updated_by | varchar | | 最后更新者 |
| created_at | timestamp | | 创建时间 |
| updated_at | timestamp | | 更新时间 |

**关联**：
- `registry` → `CapabilityRegistry`（多对一）
- `versions` → `CapabilityVersion`（一对多，外键 `item_id`）
- `assets` → `CapabilityAsset`（一对多，外键 `item_id`）
- `artifacts` → `CapabilityArtifact`（一对多，外键 `item_id`）

---

### 5. CapabilityVersion（能力版本）

记录能力条目内容的历史版本，每次同步更新时追加一条版本记录。

| 字段 | 类型 | 约束 | 说明 |
|------|------|------|------|
| id | uuid | PK, DEFAULT gen_random_uuid() | 版本记录唯一标识 |
| item_id | uuid | NOT NULL | 所属条目 ID |
| version | int | NOT NULL | 版本序号（自增整数） |
| content | text | NOT NULL | 该版本的内容快照 |
| metadata | jsonb | DEFAULT '{}' | 扩展元数据 |
| commit_msg | varchar | | 版本说明（如 sync commit SHA） |
| created_by | varchar | NOT NULL | 创建者 |
| created_at | timestamp | | 创建时间 |

---

### 6. CapabilityAsset（能力资产文件）

能力条目的附属文件（如图片、配置文件等），支持本地存储和对象存储后端。

| 字段 | 类型 | 约束 | 说明 |
|------|------|------|------|
| id | uuid | PK, DEFAULT gen_random_uuid() | 资产唯一标识 |
| item_id | uuid | NOT NULL, INDEX | 所属条目 ID |
| rel_path | varchar | NOT NULL | 文件相对路径 |
| text_content | text | nullable | 文本内容（文本文件直接存储） |
| storage_backend | varchar | DEFAULT 'local' | 存储后端：`local` \| `s3` 等 |
| storage_key | varchar | | 存储后端的对象键 |
| mime_type | varchar | | MIME 类型 |
| file_size | int64 | DEFAULT 0 | 文件大小（字节） |
| content_sha | varchar | | 内容 SHA 哈希 |
| created_at | timestamp | | 创建时间 |
| updated_at | timestamp | | 更新时间 |

---

### 7. CapabilityArtifact（能力制品）

能力条目的可分发制品（如打包文件、二进制等），支持版本管理和下载统计。

| 字段 | 类型 | 约束 | 说明 |
|------|------|------|------|
| id | uuid | PK, DEFAULT gen_random_uuid() | 制品唯一标识 |
| item_id | uuid | NOT NULL | 所属条目 ID |
| filename | varchar | NOT NULL | 文件名 |
| file_size | int64 | NOT NULL | 文件大小（字节） |
| checksum_sha256 | varchar | NOT NULL | SHA256 校验和 |
| mime_type | varchar | | MIME 类型 |
| storage_backend | varchar | DEFAULT 'local' | 存储后端 |
| storage_key | varchar | NOT NULL | 存储后端的对象键 |
| artifact_version | varchar | NOT NULL | 制品版本号 |
| is_latest | bool | DEFAULT false | 是否为最新版本 |
| source_type | varchar | DEFAULT 'upload' | 来源：`upload` \| `sync` |
| download_count | int | DEFAULT 0 | 下载次数 |
| uploaded_by | varchar | NOT NULL | 上传者 |
| created_at | timestamp | | 创建时间 |

---

### 8. SyncJob（同步任务）

同步任务队列，支持手动触发、定时触发和 Webhook 触发，带重试机制。

| 字段 | 类型 | 约束 | 说明 |
|------|------|------|------|
| id | uuid | PK, DEFAULT gen_random_uuid() | 任务唯一标识 |
| registry_id | uuid | NOT NULL, INDEX | 目标注册表 ID |
| trigger_type | varchar | NOT NULL | 触发方式：`scheduled` \| `manual` \| `webhook` |
| trigger_user | varchar | | 触发用户 ID |
| priority | int | NOT NULL, DEFAULT 5 | 优先级（数值越小优先级越高） |
| status | varchar | NOT NULL, DEFAULT 'pending', INDEX | 状态：`pending` \| `running` \| `success` \| `failed` \| `cancelled` |
| payload | jsonb | DEFAULT '{}' | 任务参数（如 dryRun） |
| retry_count | int | DEFAULT 0 | 已重试次数 |
| max_attempts | int | DEFAULT 3 | 最大重试次数 |
| last_error | text | | 最近一次错误信息 |
| scheduled_at | timestamp | NOT NULL, INDEX | 计划执行时间 |
| started_at | timestamp | nullable | 实际开始时间 |
| finished_at | timestamp | nullable | 完成时间 |
| sync_log_id | uuid | nullable | 关联的同步日志 ID |
| created_at | timestamp | | 创建时间 |

**关联**：
- `registry` → `CapabilityRegistry`（多对一）

**幂等性保障**：入队时检查同一注册表是否存在 `pending` 或 `running` 状态的任务，若存在则拒绝入队（`scheduled` 触发时静默跳过）。

---

### 9. SyncLog（同步日志）

记录每次同步执行的详细结果，包括变更统计和错误信息。

| 字段 | 类型 | 约束 | 说明 |
|------|------|------|------|
| id | uuid | PK, DEFAULT gen_random_uuid() | 日志唯一标识 |
| registry_id | uuid | NOT NULL, INDEX | 目标注册表 ID |
| trigger_type | varchar | NOT NULL | 触发方式 |
| trigger_user | varchar | | 触发用户 ID |
| status | varchar | NOT NULL, DEFAULT 'running' | 状态：`running` \| `success` \| `failed` \| `cancelled` |
| commit_sha | varchar | | 本次同步的 Git commit SHA |
| previous_sha | varchar | | 上次同步的 Git commit SHA |
| total_items | int | DEFAULT 0 | 处理的条目总数 |
| added_items | int | DEFAULT 0 | 新增条目数 |
| updated_items | int | DEFAULT 0 | 更新条目数 |
| deleted_items | int | DEFAULT 0 | 归档（删除）条目数 |
| skipped_items | int | DEFAULT 0 | 跳过条目数 |
| failed_items | int | DEFAULT 0 | 失败条目数 |
| error_message | text | | 错误信息摘要 |
| duration_ms | int64 | | 执行耗时（毫秒） |
| started_at | timestamp | NOT NULL | 开始时间 |
| finished_at | timestamp | nullable | 完成时间 |
| created_at | timestamp | | 创建时间 |

**关联**：
- `registry` → `CapabilityRegistry`（多对一）

---

## ER 图

```
Organization ──< OrgMember
     │
     └── org_id

CapabilityRegistry ──< CapabilityItem ──< CapabilityVersion
        │                    │
        └──< SyncJob         ├──< CapabilityAsset
        └──< SyncLog         └──< CapabilityArtifact
```

---

## 同步流程与状态机

### CapabilityRegistry.sync_status

```
idle ──→ syncing ──→ idle
                └──→ error
```

### SyncJob.status

```
pending ──→ running ──→ success
    │              └──→ failed (retry → pending)
    └──→ cancelled
```

### CapabilityItem.status

```
active ──→ archived
```
条目在同步时若源文件已被删除，状态变更为 `archived`（软删除）。

---

## 索引说明

| 表 | 索引字段 | 用途 |
|---|---|---|
| org_members | org_id | 按组织查询成员 |
| capability_items | registry_id | 按注册表查询条目 |
| capability_assets | item_id | 按条目查询资产 |
| sync_jobs | registry_id | 按注册表查询任务 |
| sync_jobs | status | 查询待执行任务 |
| sync_jobs | scheduled_at | 按计划时间排序 |
| sync_logs | registry_id | 按注册表查询日志 |

---

## 技术选型

| 组件 | 选型 |
|---|---|
| 数据库 | PostgreSQL |
| ORM | GORM v2 |
| JSON 字段 | `gorm.io/datatypes` JSONB |
| UUID 生成 | `github.com/google/uuid` / PostgreSQL `gen_random_uuid()` |
| 连接初始化 | `gorm.io/driver/postgres` |
