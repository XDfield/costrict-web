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

---

## Portal 镜像的环境变量机制

portal 服务涉及两类环境变量，作用时机不同，需分别配置。

### 构建时变量（`docker build --build-arg`）

这些变量由 Vite 在构建时打包进前端 JS，**镜像构建后无法修改**。

| 变量                     | 默认值      | 说明                                   |
| ------------------------ | ----------- | -------------------------------------- |
| `VITE_API_PREFIX`        | _(空)_      | 前端请求 `/api` 和 `/cloud` 的路径前缀 |
| `VITE_CLOUD_SERVER_HOST` | `localhost` | 仅影响 Vite dev server proxy，生产无效 |
| `VITE_CLOUD_SERVER_PORT` | `18080`     | 同上                                   |
| `VITE_APP_PORT`          | `3000`      | 前端应用监听端口                       |
| `VITE_CASDOOR_ENDPOINT`  | _(空)_      | Casdoor 服务地址（浏览器直接访问）     |
| `VITE_CASDOOR_CLIENT_ID` | _(空)_      | Casdoor OAuth2 Client ID               |
| `VITE_APP_URL`           | _(空)_      | 前端访问地址，用于 OAuth2 redirect_uri |

当前 CI 中已固定传入：

```
VITE_API_PREFIX=/costrict-web-api
```

如需修改，编辑 `build-images.yaml` 中 `Set build args` 步骤的 `args` 值，然后重新构建镜像。

### 运行时变量（`docker run -e`）

这些变量由容器启动时的 entrypoint 注入到 nginx 配置，**无需重新构建镜像**。

| 变量                | 默认值      | 说明                                                 |
| ------------------- | ----------- | ---------------------------------------------------- |
| `CLOUD_SERVER_HOST` | `localhost` | 后端服务地址，nginx 反向代理目标                     |
| `CLOUD_SERVER_PORT` | `18080`     | 后端服务端口                                         |
| `NGINX_PREFIX`      | _(空)_      | nginx location 匹配前缀，须与 `VITE_API_PREFIX` 一致 |

### 路径前缀工作原理

前端请求路径和 nginx 代理路径通过同一个前缀变量控制，但分别在构建时和运行时注入：

```
浏览器发起请求
  └─ /costrict-web-api/api/items        ← VITE_API_PREFIX 控制（构建时）
       │
       ▼
     nginx location ~ ^/costrict-web-api/api  ← NGINX_PREFIX 控制（运行时）
       │  rewrite 去掉前缀
       ▼
     后端收到 /api/items
```

**两个变量必须保持一致**，否则 nginx 无法匹配请求。

### 示例

```bash
# 构建（CI 已自动处理）
docker build \
  --build-arg VITE_API_PREFIX=/costrict-web-api \
  -t zgsm/costrict-web-portal:latest \
  -f portal/packages/app/Dockerfile ./portal

# 运行
docker run \
  -e CLOUD_SERVER_HOST=192.168.1.100 \
  -e CLOUD_SERVER_PORT=18080 \
  -e NGINX_PREFIX=/costrict-web-api \
  -p 3000:3000 \
  zgsm/costrict-web-portal:latest
```
