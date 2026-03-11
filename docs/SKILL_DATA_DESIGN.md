# 技能中心数据托管方案

## 背景与诉求

企业级技能中心，核心诉求：
- **研发共建**：团队成员在平台上创建、编辑、共享 MCP/Skill 等内容
- **公网同步**：接入外部公开技能仓库（如 GitHub 上的社区仓库）
- **私有化工具**：组织内部的敏感工具，访问受控、不出网
- **用户体验**：所有操作在平台内完成，用户感知不到底层 Git

---

## 现状问题

当前项目存在三套数据层完全割裂：

```
① 文件系统（buildwithclaude 原始设计）
   skills-server.ts     → 读 plugins/all-skills/skills/*/SKILL.md
   subagents-server.ts  → 读 plugins/all-agents/agents/*.md
   commands-server.ts   → 读 plugins/commands-*/commands/*.md
   ↑ 只读，无法在平台上创建/编辑

② PostgreSQL-Drizzle（前端直连）
   mcp_servers, marketplaces, plugins, skills
   ↑ 只有外部索引数据，无内部创作内容

③ Go 后端模型（SkillRepository / Skill / Agent ...）
   ↑ 有权限结构设计，但完全没有被前端使用
```

---

## 推荐方案：DB 主存储 + 透明 Git 版本化

**核心原则**：用户只看到平台，Git 是不可见的底层机制。
类比 Confluence 编辑页面背后有版本历史，用户不需要知道内容怎么存储。

### 整体数据流

```
用户在平台操作（创建 / 编辑 / 删除）
        ↓
   Go 后端 API
        ↓
┌───────────────────────────────────────┐
│  PostgreSQL（主存储 + 索引）            │  ← 所有读操作走这里，毫秒级响应
│  skill_registries  技能仓库            │
│  skill_items       统一条目            │
│  skill_versions    版本历史            │
└───────────────────────────────────────┘
        ↓ 异步（非阻塞，用户不感知）
┌───────────────────────────────────────┐
│  内置 bare Git 仓库                    │  ← 版本备份 + 导出 + 外部同步锚点
│  （服务器本地，用户不可见）             │
└───────────────────────────────────────┘

外部公网仓库（GitHub / Gitea 等）
        ↓ 定时拉取（已有 indexer 雏形）
   解析 SKILL.md / plugin.json / mcp.yaml
        ↓
   写入 skill_items（标记 source_type=external）
```

---

## 核心数据模型

### `skill_registries`（技能仓库）

```sql
CREATE TABLE skill_registries (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name         VARCHAR(255) NOT NULL,
  description  TEXT,

  -- 来源类型
  source_type  VARCHAR(32) NOT NULL,
  -- 'internal' : 用户在平台创建
  -- 'external' : 从公网仓库同步（GitHub 等）
  -- 'private'  : 私有化工具，手动导入

  -- 外部仓库配置（source_type='external' 时使用）
  external_url    VARCHAR(512),        -- https://github.com/org/repo
  external_branch VARCHAR(128) DEFAULT 'main',
  sync_enabled    BOOLEAN DEFAULT false,
  sync_interval   INTEGER DEFAULT 3600,  -- 秒
  last_synced_at  TIMESTAMP,
  last_sync_sha   VARCHAR(64),         -- 上次同步的 commit sha，用于增量对比

  -- 权限
  visibility   VARCHAR(32) DEFAULT 'org',
  -- 'public'  : 平台所有人可见
  -- 'org'     : 仅归属组织成员可见
  -- 'private' : 仅创建者可见

  org_id       VARCHAR(191),           -- Casdoor organization name
  owner_id     VARCHAR(191) NOT NULL,  -- Casdoor user id

  created_at   TIMESTAMP DEFAULT NOW(),
  updated_at   TIMESTAMP DEFAULT NOW()
);
```

### `skill_items`（统一技能条目）

```sql
CREATE TABLE skill_items (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  registry_id  UUID REFERENCES skill_registries(id) ON DELETE CASCADE,

  -- 内容标识
  slug         VARCHAR(255) NOT NULL,
  item_type    VARCHAR(32) NOT NULL,
  -- 'skill' | 'subagent' | 'command' | 'hook' | 'mcp' | 'plugin'

  name         VARCHAR(255) NOT NULL,
  description  TEXT,
  category     VARCHAR(128),
  version      VARCHAR(64) DEFAULT '1.0.0',

  -- 主体内容（Markdown，与现有 SKILL.md 格式完全兼容）
  content      TEXT NOT NULL,
  -- frontmatter 解析后的结构化数据，方便查询过滤
  metadata     JSONB DEFAULT '{}',

  -- 来源追踪（外部同步时使用）
  source_path  VARCHAR(512),   -- 在源仓库中的路径，如 skills/my-skill/SKILL.md
  source_sha   VARCHAR(64),    -- 文件的 git blob sha，用于判断是否有变更

  -- 权限（继承 registry，可单独覆盖）
  visibility   VARCHAR(32),    -- NULL 表示继承 registry

  -- 统计
  install_count INTEGER DEFAULT 0,

  -- 状态
  status       VARCHAR(32) DEFAULT 'active',
  -- 'active' | 'draft' | 'deprecated'

  -- 审计
  created_by   VARCHAR(191) NOT NULL,
  updated_by   VARCHAR(191),
  created_at   TIMESTAMP DEFAULT NOW(),
  updated_at   TIMESTAMP DEFAULT NOW(),

  UNIQUE(registry_id, slug, item_type)
);
```

### `skill_versions`（版本历史，对标 git log）

```sql
CREATE TABLE skill_versions (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  item_id      UUID REFERENCES skill_items(id) ON DELETE CASCADE,

  version      INTEGER NOT NULL,       -- 自增版本号
  content      TEXT NOT NULL,          -- 该版本的完整内容快照
  metadata     JSONB DEFAULT '{}',

  -- 类比 git commit message
  commit_msg   VARCHAR(500),

  created_by   VARCHAR(191) NOT NULL,
  created_at   TIMESTAMP DEFAULT NOW(),

  UNIQUE(item_id, version)
);
```

---

## 三种场景的实现路径

### 场景一：研发共建（内部创作）

用户在平台填写表单 → `POST /api/registries/:id/items`

```
写 skill_items（status='draft' 或 'active'）
写 skill_versions（version=1，commit_msg="Initial version"）
异步：git commit 到内置 bare repo（用户不感知）
前端立即可见
```

编辑时：

```
用户修改内容 → PUT /api/items/:id
更新 skill_items
写新的 skill_versions 记录（version 自增，用户可填写变更说明）
异步 git commit
```

版本历史对用户的呈现（读 `skill_versions` 表）：

```
v3 · 张三 · 2小时前 · "修复了示例代码"
v2 · 李四 · 昨天   · "添加了使用说明"
v1 · 张三 · 3天前  · "初始版本"
```

### 场景二：同步公网仓库

管理员在平台配置：
- 仓库 URL：`https://github.com/davepoon/buildwithclaude`
- 同步分支：`main`
- 同步间隔：每天
- 可见性：`public`

```
创建 skill_registries 记录（source_type='external'）
后台定时任务启动
拉取仓库，遍历 skills/*/SKILL.md
对比 source_sha，只处理变更文件（增量同步）
upsert skill_items（记录 source_path, source_sha）
前端展示，标记"来源: buildwithclaude"
```

用户体验：
- 可以浏览、安装外部技能
- 不能直接编辑（只读）
- 可以 **Fork** 到自己的内部仓库后再编辑

### 场景三：私有化工具

```
管理员上传 ZIP 包，或配置内网 Git 地址
解析内容，写入 skill_items
visibility='org'，org_id='内部组织名'
只有该组织成员可见
不出网，不同步到任何外部
```

---

## 与现有代码的整合策略

| 现有模块 | 改造方式 | 工作量 |
|---------|---------|-------|
| `lib/skills-server.ts`（读文件） | 改为从 `skill_items WHERE item_type='skill'` 读取 | 小 |
| `lib/subagents-server.ts`（读文件） | 同上，`item_type='subagent'` | 小 |
| `lib/commands-server.ts`（读文件） | 同上，`item_type='command'` | 小 |
| `lib/indexer/mcp-server-indexer.ts` | 保留逻辑，写入目标改为 `skill_items`（`item_type='mcp'`） | 中 |
| `lib/indexer/marketplace-indexer.ts` | 保留逻辑，`skill_registries` 对应 marketplace | 中 |
| Go 后端 `SkillRepository` model | 重命名/对应 `skill_registries` | 小 |
| Go 后端 `Skill/Agent/Command` model | 合并为 `skill_items`，加 `item_type` 字段 | 中 |

---

## 关于 Git 机制的使用边界

**不推荐**把 Git 作为主存储（用户每次保存都触发 git commit）：
- 延迟高（git 操作比 DB 写慢 10-100x）
- 并发冲突处理复杂
- 搜索/过滤需要额外索引层

**推荐**的 Git 使用场景：

| 用途 | 说明 |
|-----|------|
| 导出/备份 | 定期把 `skill_items` 内容 dump 成 SKILL.md 并 commit 到内置 bare repo |
| 增量同步锚点 | 用 `source_sha`（git blob sha）判断外部文件是否有变更，避免重复处理 |
| 内容格式标准 | `content` 字段存储与现有 SKILL.md 完全兼容的 Markdown+frontmatter |
| 可移植性 | 用户可以将仓库内容导出为标准格式，不被平台锁定 |

---

## MCP Server 制品托管

MCP Server 与普通 Skill 的核心差异：普通 Skill 的"制品"就是 Markdown 文本本身，而 MCP Server 是**可执行的代码包**（通常是 npm 包，通过 `npx` 启动）。

### MCP 托管的三种形态

第一版同时支持以下三种形态，创建者按实际情况选择，平台不做可用性校验，**由创建者保证命令的可用性**。

| 形态 | 适用场景 | 用户操作 |
|-----|---------|---------|
| **命令直连** | 公网包、内网已部署的服务 | 填写运行命令，平台生成配置，用户复制即用 |
| **压缩包下载** | 完全离线、隔离网络 | 上传 .tgz，用户下载后本地安装 |
| **远程 URL** | HTTP/SSE 类型的 MCP 服务 | 填写服务地址，平台生成配置 |

---

### 形态一：命令直连

创建者直接提供运行命令，平台存储并展示，用户复制到本地使用。命令可用性由创建者保证，平台不做验证。

**创建时填写：**

```
MCP 名称:    my-internal-tool
运行命令:    npx -y @internal/my-mcp-server@1.0.0
             或: node /opt/tools/my-server/index.js
             或: python3 /opt/tools/my-server/main.py
环境变量:    API_KEY（必填）、LOG_LEVEL（选填，默认 info）
描述:        ...
```

**平台自动生成的配置（用户复制）：**

```json
{
  "mcpServers": {
    "my-internal-tool": {
      "command": "npx",
      "args": ["-y", "@internal/my-mcp-server@1.0.0"],
      "env": {
        "API_KEY": "<your-api-key>"
      }
    }
  }
}
```

同时生成 Claude Code CLI 安装命令：

```bash
claude mcp add my-internal-tool -e API_KEY=<your-api-key> -- npx -y @internal/my-mcp-server@1.0.0
```

**`skill_items.metadata` 结构（形态一）：**

```jsonc
{
  "hosting_type": "command",          // 形态标识
  "server_type": "stdio",             // stdio | http | sse
  "command": "npx",
  "args": ["-y", "@internal/my-mcp-server@1.0.0"],
  "environment_variables": [
    { "name": "API_KEY", "required": true,  "description": "服务 API 密钥" },
    { "name": "LOG_LEVEL", "required": false, "default": "info" }
  ]
}
```

---

### 形态二：压缩包下载

平台作为**制品仓库**，提供上传和下载能力。第一版不引入私有 npm Registry。

```
开发者打包本地 MCP 工具
  npm pack → my-mcp-server-1.0.0.tgz
        ↓
  在平台上传压缩包 + 填写元数据
        ↓
  平台存储文件（本地磁盘 / S3 / MinIO）
  写入 skill_items（元数据）
  写入 skill_artifacts（制品记录）
        ↓
  其他用户在技能中心找到该工具
  点击下载 .tgz 文件
  本地手动安装：
    npm install -g ./my-mcp-server-1.0.0.tgz
    claude mcp add my-server -- my-mcp-server
```

**`skill_items.metadata` 结构（形态二）：**

```jsonc
{
  "hosting_type": "artifact",         // 形态标识
  "server_type": "stdio",
  "package_name": "my-mcp-server",    // 安装后的可执行命令名
  "environment_variables": [
    { "name": "API_KEY", "required": true, "description": "服务 API 密钥" }
  ],
  "install_hint": "npm install -g ./my-mcp-server-1.0.0.tgz"
}
```

**适用场景**：完全离线/隔离网络环境，无需额外基础设施。

---

### 形态三：远程 URL

针对 HTTP / SSE 传输类型的 MCP 服务，服务已部署在某个地址，用户只需配置 URL。

**创建时填写：**

```
MCP 名称:    my-remote-service
服务地址:    https://mcp.internal.corp/api/v1
传输类型:    streamable-http
请求头:      Authorization: Bearer <token>（可选）
```

**平台自动生成的 Claude Code CLI 命令：**

```bash
claude mcp add my-remote-service --transport http https://mcp.internal.corp/api/v1
```

**`skill_items.metadata` 结构（形态三）：**

```jsonc
{
  "hosting_type": "remote",           // 形态标识
  "server_type": "http",              // http | sse | streaming-http
  "url": "https://mcp.internal.corp/api/v1",
  "headers": [
    { "name": "Authorization", "required": true, "description": "Bearer token" }
  ]
}
```

---

### 第一版：压缩包上传 + 离线下载（形态二详细方案）

### 文件管理详细方案

#### 文件范围说明

项目中有两类文件，性质完全不同，处理方式不同：

| 类型 | 内容 | 体积 | 存储方式 |
|-----|------|------|---------|
| 内容文件 | Skill/Subagent/Command 的 Markdown | 几 KB | 直接存 `skill_items.content` 字段，不需要文件系统 |
| 制品文件 | MCP Server 压缩包（.tgz/.zip） | 几 KB ~ 几百 MB | 文件系统 + `skill_artifacts` 表记录元数据 |

本节专注**制品文件**的管理。

#### 存储目录结构

```
/data/artifacts/                        ← ARTIFACT_STORAGE_PATH 环境变量
  └── {org_id}/                         ← 组织隔离，不同组织文件物理分离
        └── {item_id}/                  ← 对应 skill_items.id
              ├── v1.0.0/
              │     └── my-mcp-1.0.0.tgz
              └── v1.1.0/
                    └── my-mcp-1.1.0.tgz
```

三级目录的理由：
- `org_id`：组织间物理隔离，配合权限校验防止越权访问
- `item_id`：一个 MCP 工具对应一个目录，版本在内部管理
- `v{version}`：多版本并存，删除某个版本不影响其他版本

#### 数据库表

```sql
CREATE TABLE skill_artifacts (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  item_id          UUID NOT NULL REFERENCES skill_items(id) ON DELETE CASCADE,

  -- 文件基本信息
  filename         VARCHAR(255) NOT NULL,   -- 原始文件名，如 my-mcp-1.0.0.tgz
  file_size        BIGINT NOT NULL,         -- 字节数，用于展示和配额检查
  checksum_sha256  VARCHAR(64) NOT NULL,    -- 上传时计算，下载时可验证完整性
  mime_type        VARCHAR(128),            -- application/gzip 等

  -- 存储位置（解耦存储后端）
  storage_backend  VARCHAR(32) DEFAULT 'local',  -- 'local' | 's3' | 'minio'
  storage_key      VARCHAR(512) NOT NULL,
  -- local:        artifacts/{org_id}/{item_id}/v1.0.0/my-mcp-1.0.0.tgz
  -- s3 / minio:   {bucket}/{org_id}/{item_id}/v1.0.0/my-mcp-1.0.0.tgz

  -- 版本信息
  artifact_version VARCHAR(64) NOT NULL,    -- 语义化版本，如 1.0.0
  is_latest        BOOLEAN DEFAULT false,   -- 是否为最新版本

  -- 下载统计
  download_count   INTEGER DEFAULT 0,

  -- 审计
  uploaded_by      VARCHAR(191) NOT NULL,   -- Casdoor user id
  created_at       TIMESTAMP DEFAULT NOW(),

  UNIQUE(item_id, artifact_version)
);
```

#### API 设计

**上传**

```
POST /api/artifacts/upload
Content-Type: multipart/form-data

字段：
  file          文件本体（.tgz / .zip / .tar.gz）
  item_id       关联的 skill_items.id
  version       版本号，如 1.0.0
  description   本次版本的说明（可选）
```

后端处理流程：

```
① 校验请求
   检查 item_id 存在且属于当前用户的组织
   检查 version 格式（semver 正则）
   检查文件类型（校验文件头 magic bytes，不只信 Content-Type）
   检查文件大小（默认上限 500MB，可配置）
   检查组织存储配额

② 流式计算 SHA-256 checksum（不全量读入内存）

③ 写文件到存储层
   path = {ARTIFACT_STORAGE_PATH}/{org_id}/{item_id}/v{version}/{filename}
   创建目录（如不存在）
   流式写入文件（io.Copy，避免内存溢出）

④ 写数据库
   将旧的 is_latest=true 记录更新为 false
   INSERT 新记录，is_latest=true

⑤ 返回
   { "artifact_id": "...", "download_url": "/api/artifacts/{id}/download" }
```

**下载**

```
GET /api/artifacts/{artifact_id}/download
```

后端处理流程：

```
① 鉴权
   验证 token（Cookie 或 Authorization header）
   查 skill_artifacts → skill_items → skill_registries 获取 visibility 和 org_id
   public  → 任何登录用户可下载
   org     → 必须是同一 org_id 的成员
   private → 必须是 owner 本人

② 查文件记录
   SELECT storage_backend, storage_key, filename, checksum_sha256
   FROM skill_artifacts WHERE id = ?

③ 异步增加下载计数（不阻塞响应）

④ 返回文件流
   local:      c.FileAttachment(path, filename)
   s3/minio:   生成预签名 URL，302 重定向（文件不经过后端，节省带宽）
```

响应头：
```
Content-Disposition: attachment; filename="my-mcp-1.0.0.tgz"
Content-Type: application/gzip
X-Checksum-SHA256: abc123...
```

**列出版本**

```
GET /api/items/{item_id}/artifacts

响应：
{
  "artifacts": [
    {
      "id": "...",
      "version": "1.1.0",
      "filename": "my-mcp-1.1.0.tgz",
      "file_size": 1234567,
      "is_latest": true,
      "download_count": 42,
      "uploaded_by": "张三",
      "created_at": "2026-03-10T10:00:00Z",
      "download_url": "/api/artifacts/xxx/download"
    }
  ]
}
```

**删除**

```
DELETE /api/artifacts/{artifact_id}

① 鉴权（只有 owner 或 org admin 可删）
② 物理删除文件
③ 删除数据库记录
④ 如果删的是 latest，将次新版本标记为 latest
```

#### 存储后端抽象

Go 后端定义 interface，本地存储和 S3/MinIO 实现同一接口，切换时只改配置：

```go
type StorageBackend interface {
    Put(ctx context.Context, key string, reader io.Reader, size int64) error
    Get(ctx context.Context, key string) (io.ReadCloser, error)
    Delete(ctx context.Context, key string) error
    // S3/MinIO 用预签名 URL；本地存储返回空字符串（走后端代理下载）
    PresignURL(ctx context.Context, key string, expiry time.Duration) (string, error)
}
```

环境变量配置：

```bash
# 第一版：本地存储
STORAGE_BACKEND=local
ARTIFACT_STORAGE_PATH=/data/artifacts
ARTIFACT_MAX_SIZE_MB=500

# 后续可切换为 MinIO（私有化对象存储，零代码改动）
# STORAGE_BACKEND=minio
# MINIO_ENDPOINT=minio.internal:9000
# MINIO_ACCESS_KEY=...
# MINIO_SECRET_KEY=...
# MINIO_BUCKET=costrict-artifacts
```

#### 安全要点

| 风险 | 措施 |
|-----|------|
| 路径穿越攻击 | `filepath.Clean` 后校验路径必须在 `ARTIFACT_STORAGE_PATH` 内 |
| 文件类型伪造 | 校验文件头 magic bytes（`.tgz` 为 `1f 8b`，`.zip` 为 `50 4b`） |
| 未授权下载 | 下载接口强制鉴权，不暴露真实文件路径，URL 不含存储路径信息 |
| 存储爆满 | 上传前按 org 统计已用空间，超配额拒绝上传 |
| 大文件内存溢出 | 上传/下载全程流式处理（`io.Copy`），不 `ReadAll` |

#### `skill_items.metadata` 中的 MCP 安装说明

```jsonc
{
  "package_name": "@internal/my-mcp-server",
  "server_type": "stdio",
  "environment_variables": [
    { "name": "API_KEY", "required": true, "description": "服务 API 密钥" }
  ],
  "install_hint": "下载 .tgz 后执行：npm install -g ./my-mcp-server-1.0.0.tgz",
  "mcp_config_example": {
    "mcpServers": {
      "my-server": {
        "command": "my-mcp-server",
        "env": { "API_KEY": "<your-key>" }
      }
    }
  }
}
```

### 形态四：公网数据源同步（兼容 buildwithclaude 方案）

平台内置定时同步任务，从公网 MCP 数据源拉取数据写入本地 `skill_items` 表，用户在平台上直接浏览和使用，无需手动录入。这是对 [buildwithclaude.com/mcp-servers](https://buildwithclaude.com/mcp-servers) 同步方案的完整继承。

#### 数据来源

| 来源 | 地址 | 内容 | 同步频率 |
|-----|------|------|---------|
| Official MCP Registry | `registry.modelcontextprotocol.io/v0.1/servers` | 官方认证的 MCP Server，含完整安装信息 | 每天 |
| Docker Hub MCP namespace | `hub.docker.com/v2/namespaces/mcp/repositories` | Docker 官方 MCP 镜像集合 | 每天 |
| GitHub stats 同步 | GitHub API `/repos/{owner}/{repo}` | 补充 star 数等统计数据 | 每6小时 |

#### 同步流程

```
定时任务触发（或管理员手动触发）
        ↓
┌─────────────────────────────────────────────┐
│  拉取 Official MCP Registry（分页，最多20页）  │
│  拉取 Docker Hub mcp namespace（全量）        │
└─────────────────────────────────────────────┘
        ↓ 对每条记录
解析字段：name / description / category / tags
         packages（npm 包信息）/ remotes（HTTP 地址）
         environment_variables / installation_methods
        ↓
upsert skill_items
  source_type = 'external-sync'
  item_type   = 'mcp'
  hosting_type 根据包信息自动推断：
    有 npm package  → 'command'（生成 npx 安装命令）
    有 docker image → 'command'（生成 docker run 命令）
    有 remote URL   → 'remote'
  source_sha = 版本号或内容 hash（用于增量判断）
        ↓
异步任务：同步 GitHub stars / Docker pulls 统计数据
```

#### 与现有 indexer 的关系

现有代码（`lib/indexer/mcp-server-indexer.ts`）已实现完整的拉取和解析逻辑，写入目标是 `mcp_servers` 表（Drizzle）。迁移策略：

- **第一版**：保留现有 indexer 不动，`mcp_servers` 表继续作为公网同步数据的存储，`skill_items` 表存内部创建的数据，前端分别查询后合并展示
- **后续**：将 indexer 写入目标统一迁移到 `skill_items`，`mcp_servers` 表废弃

#### 同步来的数据在平台上的展示

```
技能中心 MCP 列表
  ├── 内部创建（hosting_type: command/artifact/remote）
  │     标记：[内部]  可编辑  可下载（如有制品）
  └── 公网同步（source_type: external-sync）
        标记：[官方] / [Docker]  只读
        展示：来源 Registry、GitHub stars、Docker pulls
        操作：复制安装命令  查看详情
              [Fork 到内部] → 创建一条 source_type=internal 的副本，可编辑
```

#### 安装命令自动生成规则

同步时根据 `packages` 和 `remotes` 字段自动生成，写入 `metadata.install_commands`：

```
有 npm package（registryType=npm）：
  claude mcp add {name} {env_flags} -- npx -y {identifier}

有 OCI package（registryType=oci）：
  docker run -i {identifier}（不生成 claudeCode，Docker 不被 Claude Code CLI 直接支持）

有 streamable-http remote：
  claude mcp add {name} --transport http {url}

有 sse remote：
  claude mcp add {name} --transport sse {url}
```

### 后续迭代：私有 npm Registry（Verdaccio）

> 第二版规划，第一版不实现。

引入 Verdaccio 后，安装体验从"手动下载安装"升级为"一行命令"：

```bash
# 平台自动生成，用户直接复制
NPM_CONFIG_REGISTRY=http://npm.internal:4873 \
  claude mcp add my-server -- npx -y @internal/my-mcp-server@1.0.0
```

同时支持三种场景：
- **内部开发包**：平台上传 → 自动发布到私有 Registry
- **公网包镜像**：从 npmjs.org 拉取（走代理）→ 缓存到内部 Registry
- **完全离线**：外网打包 → 传入内网 → 推送到内部 Registry

---

## 实施路线图

```
Step 1 — 建表 + 迁移现有文件数据
  ✦ 创建 skill_registries / skill_items / skill_versions / skill_artifacts 表
  ✦ 写一次性迁移脚本：读文件系统 → 写 DB
  ✦ 改 skills-server.ts 等从 DB 读取
  ✦ 前端表现不变，但数据来源切换完成

Step 2 — 平台内创作 CRUD
  ✦ 创建/编辑/删除技能的界面和 API（Skill / Subagent / Command / Hook）
  ✦ Markdown 编辑器（带 frontmatter 表单）
  ✦ 版本历史查看与回滚界面

Step 3 — MCP 压缩包上传与下载（第一版制品托管）
  ✦ 上传 .tgz / .zip 文件界面
  ✦ 文件存储（本地磁盘，预留 S3/MinIO 接口）
  ✦ 下载接口（鉴权后返回文件流）
  ✦ 自动生成离线安装说明

Step 4 — 公网 MCP 数据源同步
  ✦ 保留现有 mcp-server-indexer 逻辑（已实现 Official MCP Registry + Docker Hub 拉取）
  ✦ 前端 MCP 列表合并展示内部创建 + 公网同步数据
  ✦ 标记来源（[内部] / [官方] / [Docker]），公网数据只读
  ✦ Fork 功能：将公网条目复制为内部可编辑副本
  ✦ 后续再将 indexer 写入目标统一迁移到 skill_items 表

Step 5 — 权限控制
  ✦ 对接 Casdoor org/group 做组织隔离
  ✦ visibility 过滤（public/org/private）
  ✦ 细粒度权限（只读/可下载/可管理）

Step 6（后续）— 私有 npm Registry
  ✦ 引入 Verdaccio，加入 docker-compose
  ✦ 上传时自动发布到私有 Registry
  ✦ 平台生成 npx 一键安装命令
  ✦ 支持公网包镜像缓存
```
