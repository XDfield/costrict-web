# Plugin 上传与查询接口文档

## 基础信息

- **Base URL**: `http://localhost:3000/api`
- **认证方式**: Cookie-based Session（需先登录获取 session）
- **Content-Type**: `application/json`（查询） / `multipart/form-data`（上传）

---

## 1. 上传 Plugin

### 接口

```http
POST /api/plugins/upload
```

### 请求参数

**Content-Type**: `multipart/form-data`

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `repo_id` | string | 是 | 目标仓库 ID |
| `file` | file | 是 | Plugin 压缩包（.zip），最大 50MB |

### 请求示例

```bash
curl -X POST http://localhost:3000/api/plugins/upload \
  -H "Cookie: session=xxx" \
  -F "repo_id=your-repo-id" \
  -F "file=@cospowers-solution-design-plugin.zip"
```

### 响应示例

**201 Created**（新建成功）

```json
{
  "id": "uuid",
  "registryId": "uuid",
  "repoId": "your-repo-id",
  "slug": "cospowers-solution-design",
  "itemType": "plugin",
  "name": "cospowers-solution-design",
  "description": "This plugin helps with design...",
  "content": "# cospowers Solution Design...",
  "metadata": {
    "templates": { "system-design": "templates/system-design-template.md" },
    "rules": { "design-review": "rules/design-review/" }
  },
  "sourceType": "archive",
  "assets": [
    { "relPath": "skills/solution-design/SKILL.md", ... },
    { "relPath": "cospowers.config.json", ... }
  ],
  "createdAt": "2026-06-04T10:00:00Z",
  "updatedAt": "2026-06-04T10:00:00Z"
}
```

**200 OK**（覆盖更新成功）

结构同 201，但返回 200 表示已覆盖旧版本。

### 错误码

| 状态码 | 场景 | 示例响应 |
|---|---|---|
| 400 | 非 zip 文件 / 解压失败 / 缺少 `CLAUDE.md` / 缺少 `cospowers.config.json` | `{"error": "missing main file CLAUDE.md"}` |
| 403 | 用户不是目标仓库成员 | `{"error": "Forbidden"}` |
| 404 | 目标仓库不存在 | `{"error": "Repository not found"}` |
| 409 | 同名 plugin 存在但用户无更新权限 | `{"error": "Conflict"}` |
| 413 | 文件超过 50MB | `{"error": "file too large"}` |

### Plugin 包结构

上传的 zip 文件应包含以下内容：

```
cospowers-solution-design-plugin.zip
├── CLAUDE.md                  # Plugin 主文档（必需）
├── cospowers.config.json      # Plugin 配置清单（必需）
├── skills/                    # Skill 定义
│   └── solution-design/
│       └── SKILL.md
├── rules/                     # 规则文件
│   └── design-review/
│       └── checklist.md
├── templates/                 # 模板文件
│   └── system-design-template.md
└── evaluators/                # Evaluator 定义
    └── design-evaluator.md
```

---

## 2. 查询 Plugin 列表

### 接口

```http
GET /api/items/my?type=plugin&page=1&pageSize=10
```

### 请求参数

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `type` | string | 否 | 过滤类型，传 `plugin` 只返回 plugin |
| `page` | number | 否 | 页码，默认 1 |
| `pageSize` | number | 否 | 每页数量，默认 10 |
| `search` | string | 否 | 关键词搜索（匹配 name / description） |
| `sortBy` | string | 否 | 排序字段：`favoriteCount` / `createdAt` / `updatedAt` |
| `sortOrder` | string | 否 | 排序方向：`asc` / `desc` |

### 响应示例

```json
{
  "items": [
    {
      "id": "uuid",
      "name": "cospowers-solution-design",
      "slug": "cospowers-solution-design",
      "itemType": "plugin",
      "description": "...",
      "content": "# ...",
      "metadata": { ... },
      "registryId": "uuid",
      "repoName": "my-repo",
      "createdAt": "2026-06-04T10:00:00Z",
      "updatedAt": "2026-06-04T10:00:00Z",
      "favoriteCount": 0,
      "installCount": 0
    }
  ],
  "total": 1,
  "page": 1,
  "pageSize": 10
}
```

---

## 3. 查询单个 Plugin

### 接口

```http
GET /api/items/:id
```

### 响应示例

```json
{
  "id": "uuid",
  "name": "cospowers-solution-design",
  "slug": "cospowers-solution-design",
  "itemType": "plugin",
  "description": "...",
  "content": "# ...",
  "metadata": { ... },
  "sourceType": "archive",
  "sourcePath": "CLAUDE.md",
  "assets": [
    { "relPath": "skills/solution-design/SKILL.md", "textContent": "..." },
    { "relPath": "cospowers.config.json", "textContent": "..." }
  ],
  "createdAt": "2026-06-04T10:00:00Z",
  "updatedAt": "2026-06-04T10:00:00Z"
}
```

---

## 4. 注册表索引查询（商店分发）

### 接口

```http
GET /api/registries/:id/index
```

返回注册表的 `index.json`，包含所有 plugin 条目及其文件列表。

### 响应示例

```json
{
  "entries": [
    {
      "slug": "cospowers-solution-design",
      "itemType": "plugin",
      "name": "cospowers-solution-design",
      "description": "...",
      "files": ["CLAUDE.md", "skills/solution-design/SKILL.md", "cospowers.config.json"]
    }
  ]
}
```

---

## 5. 下载 Plugin 文件

### 接口

```http
GET /api/registries/:id/items/:slug/download/:filename
```

| 参数 | 说明 |
|---|---|
| `:id` | 注册表 ID |
| `:slug` | Plugin slug |
| `:filename` | 文件名，如 `CLAUDE.md` 或 `skills/solution-design/SKILL.md` |

---

## 6. 查询我的仓库列表

### 接口

```http
GET /api/repositories/my
```

### 响应示例

```json
{
  "repositories": [
    {
      "id": "uuid",
      "name": "my-repo",
      "displayName": "My Repo",
      "description": "...",
      "visibility": "public",
      "repoType": "normal"
    }
  ]
}
```

> 若 `repositories` 为 `null` 或空数组，表示当前用户没有可用仓库，需先调用 **创建仓库** 接口。

---

## 7. 创建仓库

### 接口

```http
POST /api/repositories
```

### 请求体

```json
{
  "name": "my-repo",
  "displayName": "My Repo",
  "description": "My plugin repository",
  "visibility": "public",
  "repoType": "normal"
}
```

### 响应示例

```json
{
  "id": "uuid",
  "name": "my-repo",
  "displayName": "My Repo",
  "description": "My plugin repository",
  "visibility": "public",
  "repoType": "normal",
  "createdAt": "2026-06-04T10:00:00Z"
}
```

### 错误码

| 状态码 | 场景 |
|---|---|
| 409 | 仓库名称已存在 |

