# SKILL 下载接口

本文档以已部署环境为准：

- Store：`https://zgsm.sangfor.com/cloud/store`
- API Base URL：`https://zgsm.sangfor.com/cloud-api`
- Swagger：`https://zgsm.sangfor.com/cloud-api/swagger/index.html`

## 快速结论

下载一个 SKILL 的主文件，只需要提供 SKILL 的 `itemId`：

```http
GET /cloud-api/api/items/{itemId}/download
```

- 公开 SKILL：不需要 Auth。
- 私有仓库中的 SKILL：需要 Auth，并且当前用户必须是仓库成员。
- 成功时直接返回文件内容，不返回 JSON。
- SKILL 的下载文件名固定为 `SKILL.md`。

## 1. 按 itemId 下载 SKILL.md

这是最简单、推荐使用的下载接口。

### 请求

```http
GET https://zgsm.sangfor.com/cloud-api/api/items/{itemId}/download
```

### 输入

| 位置 | 参数 | 类型 | 必填 | 说明 |
|---|---|---:|:---:|---|
| Path | `itemId` | string (UUID) | 是 | SKILL 在 Store 中的条目 ID |
| Header | `Authorization` | string | 否/条件必填 | 私有 SKILL 必填，格式为 `Bearer <access_token>` |
| Cookie | `zgsmAdminToken` | string | 否/条件必填 | 浏览器登录态；可代替 `Authorization` Header |

`Authorization` 和 `zgsmAdminToken` 二选一即可。如果二者同时提供，服务端优先使用 `Authorization`。

### Auth 规则

| SKILL 所属仓库 | 是否需要 Auth | 结果 |
|---|:---:|---|
| 公开仓库 | 否 | 匿名请求可以下载 |
| 私有仓库，当前用户是仓库成员 | 是 | 可以下载 |
| 私有仓库，未登录或不是仓库成员 | 是 | 返回 `403 Forbidden` |

### 成功输出

```http
HTTP/1.1 200 OK
Content-Type: text/plain; charset=utf-8
Content-Disposition: attachment; filename="SKILL.md"

---
name: example-skill
description: Example skill description
---

# Example Skill

Skill instructions...
```

响应 Body 是原始 `SKILL.md` 文本，不需要 JSON 解析。

### curl 示例：公开 SKILL

```bash
curl -L \
  'https://zgsm.sangfor.com/cloud-api/api/items/ada00474-027b-46d2-b16f-78a5c2148a2d/download' \
  -o SKILL.md
```

### curl 示例：私有 SKILL，Bearer Token

```bash
curl -L \
  -H 'Authorization: Bearer <access_token>' \
  'https://zgsm.sangfor.com/cloud-api/api/items/<itemId>/download' \
  -o SKILL.md
```

### curl 示例：私有 SKILL，浏览器 Cookie

```bash
curl -L \
  -b 'zgsmAdminToken=<access_token>' \
  'https://zgsm.sangfor.com/cloud-api/api/items/<itemId>/download' \
  -o SKILL.md
```

### JavaScript 示例

浏览器同源登录态下，使用 `credentials: "include"` 携带 Cookie：

```javascript
const itemId = "ada00474-027b-46d2-b16f-78a5c2148a2d"
const response = await fetch(
  `https://zgsm.sangfor.com/cloud-api/api/items/${itemId}/download`,
  { credentials: "include" },
)

if (!response.ok) {
  throw new Error(`下载失败: ${response.status}`)
}

const skillMarkdown = await response.text()
```

## 2. 按 repo 和 slug 下载文件

已知仓库、SKILL slug 和文件相对路径时，可直接下载主文件或附属文件。

```http
GET /cloud-api/api/registry/{repo}/skill/{slug}/{filePath}
```

### 输入

| 位置 | 参数 | 类型 | 必填 | 说明 |
|---|---|---:|:---:|---|
| Path | `repo` | string | 是 | 仓库 ID 或仓库名称，例如 `public` |
| Path | `skill` | string | 是 | 固定值，表示条目类型是 SKILL |
| Path | `slug` | string | 是 | SKILL 的 slug |
| Path | `filePath` | string | 是 | `SKILL.md` 或附属文件的相对路径 |
| Header/Cookie | Auth | string | 否/条件必填 | 规则与按 `itemId` 下载相同 |

### 下载主文件

```bash
curl -L \
  'https://zgsm.sangfor.com/cloud-api/api/registry/public/skill/slug-keywords-skill/SKILL.md' \
  -o SKILL.md
```

### 下载附属文件

例如 SKILL 中包含 `references/guide.md`：

```bash
curl -L \
  'https://zgsm.sangfor.com/cloud-api/api/registry/public/skill/<slug>/references/guide.md' \
  -o guide.md
```

成功响应的 `Content-Type` 使用文件原始 MIME 类型，`Content-Disposition` 中包含原始文件名。

## 3. 查找 SKILL 的 itemId

如果调用方只有名称或 slug，可以先查询 SKILL 列表：

```http
GET /cloud-api/api/items?type=skill&search={keyword}&page=1&pageSize=20&paginated=true
```

### 示例

```bash
curl -G 'https://zgsm.sangfor.com/cloud-api/api/items' \
  --data-urlencode 'type=skill' \
  --data-urlencode 'search=slug-keywords' \
  --data-urlencode 'page=1' \
  --data-urlencode 'pageSize=20' \
  --data-urlencode 'paginated=true'
```

### 输出示例

```json
{
  "items": [
    {
      "id": "ada00474-027b-46d2-b16f-78a5c2148a2d",
      "repoId": "public",
      "slug": "slug-keywords-skill",
      "itemType": "skill",
      "name": "slug-keywords",
      "status": "active"
    }
  ],
  "total": 1,
  "hasMore": false
}
```

调用方应使用返回的 `items[].id` 作为下载接口的 `itemId`。建议同时确认：

- `itemType` 必须是 `skill`；
- `status` 应为 `active`。

## 4. 附属文件列表

需要下载完整的多文件 SKILL 时，可以先查询附属文件列表：

```http
GET /cloud-api/api/items/{itemId}/assets
```

输出示例：

```json
{
  "assets": [
    {
      "relPath": "references/guide.md",
      "textContent": "# Guide\n...",
      "mimeType": "text/markdown",
      "fileSize": 1024,
      "contentSha": "<sha256>"
    },
    {
      "relPath": "assets/example.png",
      "mimeType": "image/png",
      "fileSize": 4096,
      "contentSha": "<sha256>"
    }
  ]
}
```

下载完整 SKILL 的建议流程：

1. 调用 `/items/{itemId}/download` 保存主文件 `SKILL.md`。
2. 调用 `/items/{itemId}/assets` 获取每个附属文件的 `relPath`。
3. 调用 `/registry/{repo}/skill/{slug}/{relPath}` 逐个下载附属文件。
4. 按 `relPath` 保持原有目录结构。

当前没有“把 SKILL 主文件和全部附属文件打成一个 ZIP”的专用接口。

## 5. 错误输出

错误响应均为 JSON。

### 403 Forbidden

条目存在，但当前用户无权访问：

```json
{
  "error": "You don't have access to this item"
}
```

处理方式：登录后携带 Bearer Token 或 `zgsmAdminToken` Cookie，并确认用户是私有仓库成员。

### 404 Not Found

`itemId` 不存在：

```json
{
  "error": "Item not found"
}
```

按文件路径下载时，文件不存在：

```json
{
  "error": "File not found"
}
```

### 500 Internal Server Error

附属文件存在记录，但服务端读取文件失败：

```json
{
  "error": "Failed to retrieve file"
}
```

## 6. 接入注意事项

- 主下载接口返回文件流，不是 `{ "content": "..." }` 形式的 JSON。
- 客户端应读取 `Content-Disposition` 决定文件名；SKILL 主文件当前固定为 `SKILL.md`。
- 路径参数应做 URL 编码，尤其是 `slug` 和附属文件路径。
- 不要在日志或 URL Query 中传递 access token。
- 下载主文件会记录一次 install 行为，因此不要用下载接口做高频健康检查。
- 公开接口当前有请求频率限制，调用方遇到 `429` 时应根据响应头退避重试。
