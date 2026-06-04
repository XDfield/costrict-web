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
| `is_builtin` | string | 否 | 是否标记为内置 Plugin，传 `"true"` 表示内置 |

### 请求示例

```bash
curl -X POST http://localhost:3000/api/plugins/upload \
  -H "Cookie: session=xxx" \
  -F "repo_id=your-repo-id" \
  -F "file=@cospowers-solution-design-plugin.zip" \
  -F "is_builtin=true"
```

### 响应示例

**201 Created**（新建成功）

```json
{
  "id": "9dc7adde-9e0f-43fc-83c4-0a6dfbedea05",
  "registryId": "00000000-0000-0000-0000-000000000001",
  "repoId": "public",
  "slug": "cospowers-solution-design",
  "itemType": "plugin",
  "name": "cospowers-solution-design",
  "description": "一个 Claude Code 插件，教授 TDD、调试和子代理驱动开发等结构化编码工作流。",
  "descriptions": {
    "en": "A Claude Code plugin that teaches structured coding workflows like TDD, debugging, and subagent-driven development.",
    "zh": "一个 Claude Code 插件，教授 TDD、调试和子代理驱动开发等结构化编码工作流。"
  },
  "category": "testing",
  "version": "1.0.0",
  "content": "# cospowers Solution Design...",
  "contentMd5": "",
  "currentRevision": 1,
  "metadata": {
    "name": "cospowers-solution-design",
    "tags": [],
    "bundle": {
      "hook_events": ["SessionStart"],
      "hooks_count": 1,
      "agents_count": 0,
      "skills_count": 14,
      "commands_count": 0,
      "mcp_server_names": [],
      "mcp_servers_count": 0,
      "skills_namespaces": ["superpowers:brainstorming"],
      "is_marketplace_repo": false
    },
    "install": {
      "method": "plugin_marketplace",
      "marketplace": "anthropics/claude-plugins-official",
      "plugin_name": "cospowers-solution-design",
      "marketplace_name": "claude-plugins-official",
      "marketplace_repo": "anthropics/claude-plugins-official",
      "marketplace_verified": true
    },
    "category": "testing",
    "description": "This plugin helps with design..."
  },
  "health": {
    "score": 100,
    "signals": {
      "freshness": 100,
      "popularity": 100,
      "source_trust": 100,
      "install_popularity": 0
    },
    "last_commit": "2026-05-08T02:47:44Z",
    "freshness_label": "active"
  },
  "evaluation": {
    "decision": "accept",
    "model_id": "mimo-v2.5-pro",
    "final_score": 94,
    "specificity": 5,
    "evaluated_at": "2026-05-09T07:31:09.076593Z",
    "desc_accuracy": 5,
    "rubric_version": "2.85974ec3",
    "writing_quality": 5,
    "coding_relevance": 5,
    "doc_completeness": 4
  },
  "sourcePath": "CLAUDE.md",
  "sourceSha": "b76ecaa7fc5daed4d57acbb45e838a8ced1acbafc632c5eaca34c9f37b95d46f",
  "sourceType": "archive",
  "source": "upload",
  "isBuiltIn": true,
  "previewCount": 24,
  "installCount": 0,
  "favoriteCount": 4,
  "status": "active",
  "securityStatus": "clean",
  "lastScanId": "72f85dac-eef6-4420-be43-bde40e84c972",
  "createdBy": "user-id",
  "updatedBy": "user-id",
  "registry": {
    "id": "00000000-0000-0000-0000-000000000001",
    "name": "public",
    "description": "Default public registry — anyone can browse and contribute",
    "sourceType": "internal",
    "repoId": "public",
    "ownerId": "system",
    "createdAt": "2026-03-26T02:50:16.42816Z",
    "updatedAt": "2026-05-26T09:36:56.070603Z"
  },
  "assets": [
    { "relPath": "skills/solution-design/SKILL.md", "textContent": "...", "mimeType": "text/markdown", "fileSize": 1234 },
    { "relPath": "cospowers.config.json", "textContent": "...", "mimeType": "application/json", "fileSize": 567 }
  ],
  "createdAt": "2026-06-04T10:00:00Z",
  "updatedAt": "2026-06-04T10:00:00Z",
  "experienceScore": 94,
  "embeddingUpdatedAt": null,
  "tags": [
    { "id": "32898659-3189-48fc-8e0c-931579d3a24c", "slug": "hooks", "tagClass": "custom", "createdBy": "system" },
    { "id": "b2bfe84f-4cf3-4d3e-be8d-d95c6c150968", "slug": "testing", "tagClass": "builtin", "createdBy": "system" }
  ],
  "favorited": true,
  "forkCount": 0
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

## 2. 查询内置 Plugin 列表

### 接口

```http
GET /api/plugins/builtin?page=1&pageSize=20
```

### 请求参数

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `page` | number | 否 | 页码，默认 1 |
| `pageSize` | number | 否 | 每页数量，默认 20，最大 100 |

### 响应示例

```json
{
  "items": [
    {
      "id": "9dc7adde-9e0f-43fc-83c4-0a6dfbedea05",
      "registryId": "00000000-0000-0000-0000-000000000001",
      "repoId": "public",
      "slug": "anthropic-superpowers",
      "itemType": "plugin",
      "name": "superpowers",
      "description": "一个 Claude Code 插件，教授 TDD、调试和子代理驱动开发等结构化编码工作流。",
      "descriptions": {
        "en": "A Claude Code plugin that teaches structured coding workflows like TDD, debugging, and subagent-driven development.",
        "zh": "一个 Claude Code 插件，教授 TDD、调试和子代理驱动开发等结构化编码工作流。"
      },
      "category": "testing",
      "version": "1.0.0",
      "content": "---\nname: \"superpowers\"\nplugin_name: \"superpowers\"\n...",
      "contentMd5": "",
      "currentRevision": 1,
      "metadata": {
        "name": "superpowers",
        "tags": [],
        "bundle": {
          "hook_events": ["SessionStart"],
          "hooks_count": 1,
          "agents_count": 0,
          "skills_count": 14,
          "commands_count": 0,
          "mcp_server_names": [],
          "mcp_servers_count": 0,
          "skills_namespaces": [
            "superpowers:brainstorming",
            "superpowers:dispatching-parallel-agents",
            "superpowers:executing-plans",
            "superpowers:finishing-a-development-branch",
            "superpowers:receiving-code-review",
            "superpowers:requesting-code-review",
            "superpowers:subagent-driven-development",
            "superpowers:systematic-debugging",
            "superpowers:test-driven-development",
            "superpowers:using-git-worktrees",
            "superpowers:using-superpowers",
            "superpowers:verification-before-completion",
            "superpowers:writing-plans",
            "superpowers:writing-skills"
          ],
          "is_marketplace_repo": false
        },
        "install": {
          "method": "plugin_marketplace",
          "marketplace": "anthropics/claude-plugins-official",
          "plugin_name": "superpowers",
          "marketplace_name": "claude-plugins-official",
          "marketplace_repo": "anthropics/claude-plugins-official",
          "marketplace_verified": true
        },
        "category": "testing",
        "description": "Superpowers teaches Claude brainstorming, subagent driven development with built in code review, systematic debugging, and red/green TDD. Additionally, it teaches Claude how to author and test new skills."
      },
      "health": {
        "score": 100,
        "signals": {
          "freshness": 100,
          "popularity": 100,
          "source_trust": 100,
          "install_popularity": 0
        },
        "last_commit": "2026-05-08T02:47:44Z",
        "freshness_label": "active"
      },
      "evaluation": {
        "decision": "accept",
        "model_id": "mimo-v2.5-pro",
        "final_score": 94,
        "specificity": 5,
        "evaluated_at": "2026-05-09T07:31:09.076593Z",
        "desc_accuracy": 5,
        "rubric_version": "2.85974ec3",
        "writing_quality": 5,
        "coding_relevance": 5,
        "doc_completeness": 4
      },
      "sourcePath": "plugins/anthropic-superpowers/.plugin.json",
      "sourceSha": "b76ecaa7fc5daed4d57acbb45e838a8ced1acbafc632c5eaca34c9f37b95d46f",
      "sourceType": "direct",
      "source": "claude-plugins-official",
      "isBuiltIn": true,
      "previewCount": 24,
      "installCount": 0,
      "favoriteCount": 4,
      "status": "active",
      "securityStatus": "clean",
      "lastScanId": "72f85dac-eef6-4420-be43-bde40e84c972",
      "createdBy": "system",
      "updatedBy": "system",
      "registry": {
        "id": "00000000-0000-0000-0000-000000000001",
        "name": "public",
        "description": "Default public registry — anyone can browse and contribute",
        "sourceType": "internal",
        "externalUrl": "",
        "externalBranch": "main",
        "syncEnabled": false,
        "syncInterval": 3600,
        "lastSyncedAt": "2026-05-26T09:36:56.070422Z",
        "lastSyncSha": "",
        "syncStatus": "idle",
        "syncConfig": {},
        "lastSyncLogId": null,
        "repoId": "public",
        "ownerId": "system",
        "createdAt": "2026-03-26T02:50:16.42816Z",
        "updatedAt": "2026-05-26T09:36:56.070603Z"
      },
      "assets": [
        { "relPath": "CLAUDE.md", "textContent": "...", "mimeType": "text/markdown", "fileSize": 1234 },
        { "relPath": "cospowers.config.json", "textContent": "...", "mimeType": "application/json", "fileSize": 567 }
      ],
      "createdAt": "2026-05-25T03:39:39.611093Z",
      "updatedAt": "2026-05-26T09:36:37.203529Z",
      "experienceScore": 94,
      "embeddingUpdatedAt": null,
      "tags": [
        { "id": "32898659-3189-48fc-8e0c-931579d3a24c", "slug": "hooks", "tagClass": "custom", "createdBy": "system", "createdAt": "2026-05-25T03:39:39.619296Z" },
        { "id": "b2bfe84f-4cf3-4d3e-be8d-d95c6c150968", "slug": "testing", "tagClass": "builtin", "createdBy": "system", "createdAt": "2026-04-23T23:51:08.322503Z" },
        { "id": "cd6d842e-1359-4c9e-824e-d09cbf12fd4f", "slug": "skills", "tagClass": "custom", "createdBy": "system", "createdAt": "2026-05-25T03:39:39.618134Z" },
        { "id": "7d46ae07-cf1c-4456-a849-a36c0b369fb2", "slug": "development", "tagClass": "builtin", "createdBy": "system", "createdAt": "2026-04-23T23:51:08.322503Z" }
      ],
      "favorited": true,
      "forkCount": 0
    }
  ],
  "total": 1,
  "page": 1,
  "pageSize": 20,
  "hasMore": false
}
```

---

## 3. 查询 Plugin 列表

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

