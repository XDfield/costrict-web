# Memory Module（记忆上报模块）

## 概述

AI Agent 在会话过程中会产生自动记忆（Auto Memory），本模块负责接收、存储和管理这些记忆文件，支持版本控制和多维度查询。

记忆格式参考 [CSC 项目记忆系统](https://github.com/XDfield/csc)，每条记忆为一个 `.md` 文件，包含 YAML frontmatter 和 markdown 正文。

---

## 数据模型

### MemoryFile（记忆元信息表）

| 字段 | 类型 | 说明 |
|------|------|------|
| ID | uuid PK | 记忆唯一标识 |
| UserID | string, index | 用户标识（subject_id） |
| ProjectPath | string, index | 项目路径，如 `/Users/linkai/code/csc` |
| WorkDir | string | 工作目录 |
| Name | string | 记忆名称（对应 frontmatter name） |
| Slug | string | 记忆文件标识，如 `user_language` |
| Type | string | `user` / `feedback` / `project` / `reference` |
| Description | string | 记忆描述 |
| CurrentVersion | int | 当前版本号，默认 1 |

唯一约束：`(user_id, project_path, slug)` — 同一用户在同一个项目下 slug 唯一。

### MemoryVersion（记忆版本表）

| 字段 | 类型 | 说明 |
|------|------|------|
| ID | uuid PK | |
| MemoryFileID | uuid, FK | 关联 MemoryFile |
| Version | int | 版本号，从 1 开始递增 |
| ContentMD5 | string | 内容 MD5，用于去重检测 |
| StorageKey | string | 文件在 storage backend 中的 key |

外键约束：`memory_versions.memory_file_id` -> `memory_files.id`，级联删除。

---

## 文件存储

记忆文件实体通过 `storage.Backend` 接口存储，默认使用 `LocalBackend`（PVC 挂载目录）。

**存储 Key 格式：**

```
memory/{userID}/{memoryID}/v{version}.md
```

**示例：**

```
memory/usr_abc123/def456/v1.md
memory/usr_abc123/def456/v2.md
```

文件内容为完整的 markdown，包含 YAML frontmatter，与 CSC 项目格式一致：

```markdown
---
name: User language preference
description: User communicates in Chinese and prefers Chinese responses
type: user
---

The user writes in Chinese (Mandarin) and expects responses in Chinese.
Git username is 林凯90331.

**How to apply:** Default to Chinese for all conversational responses.
```

---

## API 接口

所有接口需要用户认证（`Authorization: Bearer <JWT>`）。

### POST /api/memories — 上报记忆

创建新记忆。如果 `(userID, projectPath, slug)` 已存在，则自动执行更新（默认创建新版本）。

**Request Body:**

```json
{
  "name": "User language preference",
  "slug": "user_language",
  "projectPath": "/Users/linkai/code/csc",
  "workDir": "/Users/linkai/code/csc",
  "type": "user",
  "description": "User communicates in Chinese",
  "content": "---\nname: User language preference\ndescription: ...\ntype: user\n---\n\n..."
}
```

### PUT /api/memories/:id — 更新记忆

**Request Body:**

```json
{
  "name": "Updated name",
  "description": "Updated description",
  "content": "...",
  "bumpVersion": true
}
```

- `bumpVersion=true`：创建新版本（version + 1），保留历史
- `bumpVersion=false`：覆盖当前版本内容

### GET /api/memories — 查询列表

**Query Parameters:**

| 参数 | 类型 | 说明 |
|------|------|------|
| projectPath | string | 按项目路径过滤 |
| workDir | string | 按工作目录过滤 |
| type | string | 按类型过滤 |
| keyword | string | 按 name/description 模糊搜索 |

**Response:**

```json
{
  "items": [
    {
      "id": "...",
      "userId": "...",
      "projectPath": "/Users/linkai/code/csc",
      "slug": "user_language",
      "name": "User language preference",
      "type": "user",
      "currentVersion": 2,
      ...
    }
  ]
}
```

### GET /api/memories/:id — 获取详情

返回记忆元信息（不含内容）。

### GET /api/memories/:id/content — 获取当前版本内容

返回纯文本 markdown（`Content-Type: text/markdown`）。

### GET /api/memories/:id/versions — 获取版本列表

```json
{
  "items": [
    { "version": 2, "contentMD5": "...", "createdAt": "..." },
    { "version": 1, "contentMD5": "...", "createdAt": "..." }
  ]
}
```

### GET /api/memories/:id/versions/:version/content — 获取指定版本内容

返回纯文本 markdown。

### DELETE /api/memories/:id — 删除记忆

软删除记忆文件记录，级联删除版本记录，文件实体保留在 storage 中。

---

## 版本管理策略

1. **创建记忆**：初始版本为 1，同时创建 `MemoryFile` 和 `MemoryVersion(v1)`
2. **更新（bumpVersion=true）**：递增版本号，创建新版本记录和文件，保留历史
3. **更新（bumpVersion=false）**：覆盖当前版本的内容和 MD5，不增加版本号
4. **内容未变更**：目前由调用方控制，服务端不强制去重跳过

---

## 模块结构

```
server/internal/memory/
├── memory.go      # Module 定义和路由注册
├── service.go     # 业务逻辑：CRUD、版本管理、文件读写
├── handlers.go    # HTTP handlers + Swagger 文档
└── README.md      # 本文档
```

---

## 关键代码复用

| 组件 | 路径 | 用途 |
|------|------|------|
| storage.Backend | `server/internal/storage/storage.go` | 文件存储抽象接口 |
| LocalBackend | `server/internal/storage/storage.go` | 本地文件系统实现，已有路径遍历保护 |
| CapabilityVersion | `server/internal/models/models.go` | 参考版本管理设计模式 |
| 模块注册模式 | `server/internal/project/project.go` | Module + RegisterRoutes |
