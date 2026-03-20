# Workflows

## build-images.yaml

构建并推送所有服务的 Docker 镜像到 Docker Hub。

### 触发方式

- **push tag**：格式 `v*.*.*`，自动构建全部服务并推送
- **workflow_dispatch**：手动触发，可指定版本号、服务列表、是否推送

### 服务列表

| service | 镜像名               | Dockerfile                     |
| ------- | -------------------- | ------------------------------ |
| gateway | costrict-web-gateway | gateway/Dockerfile             |
| api     | costrict-web-api     | server/Dockerfile              |
| worker  | costrict-web-worker  | server/Dockerfile.worker       |
| portal  | costrict-web-portal  | portal/packages/app/Dockerfile |

## Portal 服务环境变量配置

Portal 前端采用**运行时注入**机制，环境变量在容器启动时通过 `docker-entrypoint.sh` 注入到 `index.html`，**无需重新构建镜像**即可修改配置。

### 架构说明

```
┌─────────────────────────────────────────────────────────────────┐
│                        Docker 容器启动                           │
├─────────────────────────────────────────────────────────────────┤
│  docker-entrypoint.sh                                           │
│    ↓ envsubst                                                   │
│  index.html 中的 ${VITE_*} 占位符 → 替换为实际环境变量值          │
│    ↓                                                            │
│  window.__ENV__ = { VITE_API_PREFIX: "/prefix", ... }          │
│    ↓                                                            │
│  src/lib/env.ts 读取 window.__ENV__ → 回退到 import.meta.env    │
│    ↓                                                            │
│  前端代码通过 env.API_PREFIX 等属性访问                          │
└─────────────────────────────────────────────────────────────────┘
```

### 环境变量优先级

1. **运行时值** (`window.__ENV__`) - Docker 容器启动时注入
2. **构建时值** (`import.meta.env`) - Vite 构建时嵌入
3. **默认值** - `src/lib/env.ts` 中定义的默认值

### 支持的运行时变量

| 变量                            | 默认值               | 说明                           |
| ------------------------------- | -------------------- | ------------------------------ |
| `VITE_CLOUD_SERVER_HOST`        | `localhost`          | 后端服务地址                   |
| `VITE_CLOUD_SERVER_PORT`        | `18080`              | 后端服务端口                   |
| `VITE_APP_PORT`                 | `3000`               | 前端应用端口                   |
| `VITE_API_PREFIX`               | (空)                 | API 路径前缀                   |
| `VITE_APP_URL`                  | `http://localhost:3000` | 前端访问地址 (OAuth redirect) |
| `VITE_CASDOOR_ENDPOINT`         | `http://localhost:18000` | Casdoor 服务地址            |
| `VITE_CASDOOR_CLIENT_ID`        | (空)                 | Casdoor OAuth2 Client ID       |
| `VITE_CASDOOR_APP_NAME`         | `app-built-in`       | Casdoor 应用名称               |
| `VITE_CASDOOR_ORG_NAME`         | `built-in`           | Casdoor 组织名称               |
| `VITE_STORE_URL`                | (空)                 | Store URL                     |
| `VITE_OPENCODE_CLOUD_DEVICE_ID` | (空)                 | OpenCode 云设备 ID             |
| `VITE_OPENCODE_SERVER_HOST`     | `localhost`          | OpenCode 服务地址              |
| `VITE_OPENCODE_SERVER_PORT`     | `8080`               | OpenCode 服务端口              |

### 开发模式兼容

在 `bun run dev` 开发模式下，`index.html` 中的占位符（如 `${VITE_API_PREFIX}`）不会被替换。

`src/lib/env.ts` 会自动检测这些未替换的占位符并使用默认值：

```typescript
// 检测未替换的占位符格式
const PLACEHOLDER_PATTERN = /^\$\{.+\}$/

// 如果值是 "${VITE_API_PREFIX}" 格式，则视为无效值，使用默认值
if (PLACEHOLDER_PATTERN.test(value)) return false
```

这确保了：
- **Docker 生产环境**：占位符被 envsubst 替换为实际值
- **本地开发环境**：检测到占位符后使用 `import.meta.env` 或默认值
- **无配置运行**：使用代码中定义的默认值

### 使用示例

```bash
# 构建镜像（CI 已自动处理）
docker build -t zgsm/costrict-web-portal:latest -f portal/packages/app/Dockerfile ./portal

# 运行 - 基础配置
docker run -p 3000:3000 zgsm/costrict-web-portal:latest

# 运行 - 自定义后端地址
docker run \
  -e VITE_CLOUD_SERVER_HOST=192.168.1.100 \
  -e VITE_CLOUD_SERVER_PORT=18080 \
  -p 3000:3000 \
  zgsm/costrict-web-portal:latest

# 运行 - 带前缀和认证配置
docker run \
  -e VITE_API_PREFIX=/costrict-web-api \
  -e VITE_CLOUD_SERVER_HOST=backend.example.com \
  -e VITE_CLOUD_SERVER_PORT=443 \
  -e VITE_CASDOOR_ENDPOINT=https://casdoor.example.com \
  -e VITE_CASDOOR_CLIENT_ID=your-client-id \
  -e VITE_APP_URL=https://app.example.com \
  -p 3000:3000 \
  zgsm/costrict-web-portal:latest
```

### API 请求路径

当设置 `VITE_API_PREFIX=/costrict-web-api` 时：

```
浏览器请求 /costrict-web-api/api/items
     ↓
Bun server 匹配前缀并去掉
     ↓
后端收到 /api/items
```

### 代码中使用环境变量

```typescript
import { env } from "@/lib/env"

// 获取环境变量（带自动回退）
const prefix = env.API_PREFIX           // 返回 string（有默认值）
const clientId = env.CASDOOR_CLIENT_ID  // 返回 string | undefined（可能为空）

// 或使用 getEnv 函数
import { getEnv } from "@/lib/env"
const customValue = getEnv("VITE_CUSTOM_VAR", "default")
```

### 相关文件

| 文件                              | 作用                               |
| --------------------------------- | ---------------------------------- |
| `packages/app/Dockerfile`         | 安装 gettext，设置 ENTRYPOINT      |
| `packages/app/docker-entrypoint.sh` | 使用 envsubst 替换 index.html 占位符 |
| `packages/app/index.html`         | 包含 `window.__ENV__` 占位符配置   |
| `packages/app/src/lib/env.ts`     | 统一的环境变量访问工具              |
