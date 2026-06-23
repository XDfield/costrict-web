# Gateway Docker Compose + APISIX chash 部署方案

本方案用于**非 Kubernetes 环境**，通过 Docker Compose 启动多个 `costrict-web-gateway` 容器，并使用 **APISIX** 做 WebSocket 入口和按 `deviceID` 的一致性哈希（chash）粘滞。

与 K8s DaemonSet 方案的核心差异：

| K8s 方案 | Docker Compose 方案 |
|----------|---------------------|
| Gateway DaemonSet | 多个独立 Gateway 容器 |
| Kubernetes 服务发现 | APISIX 静态 upstream nodes 或 DNS/Consul 服务发现 |
| Pod IP 作为 `GATEWAY_INTERNAL_URL` | 容器名/容器 IP / `host.docker.internal` |

---

## 架构

```text
┌─────────────┐
│   cs-cloud   │
└──────┬──────┘
       │ wss://api.example.com/device/{deviceID}/tunnel
       ▼
┌──────────────────────────────┐
│        APISIX (Docker)       │
│  ├─ 从 /device/{deviceID}/... 提取 deviceID → X-Device-ID header
│  └─ chash 一致性哈希到 Gateway 容器
└──────┬───────────────────────┘
       │ Docker network
       ▼
┌──────────────────────────────┐
│   costrict-web-gateway-1     │
│   costrict-web-gateway-2     │
│   ...                        │
└──────────────────────────────┘
```

Server 到设备的反向流量仍然走 `GATEWAY_INTERNAL_URL`，**不经过 APISIX**。

---

## 前置条件

- Docker Engine 20.10+ 和 Docker Compose v2+
- 可用的 APISIX 镜像（`apache/apisix`）
- `costrict-web-server`（API）和 Gateway 使用 **Redis** 作为 `GatewayStore`
- Server 必须能访问到每个 Gateway 容器的 `GATEWAY_INTERNAL_URL`

---

## 1. 创建 Docker Compose 文件

`docker-compose.gateway.yml`：

```yaml
version: "3.8"

services:
  apisix:
    image: apache/apisix:3.9.0-debian
    container_name: costrict-apisix
    restart: always
    ports:
      - "443:9443"      # HTTPS / WSS
      - "9180:9180"     # Admin API
    volumes:
      - ./apisix/config.yaml:/usr/local/apisix/conf/config.yaml:ro
    environment:
      - APISIX_STAND_ALONE=true
    networks:
      - costrict-net
    depends_on:
      - gateway-1
      - gateway-2

  gateway-1:
    image: ghcr.io/xdfield/costrict-web-gateway:latest
    container_name: costrict-web-gateway-1
    restart: always
    environment:
      - GATEWAY_ID=gateway-1
      - GATEWAY_ENDPOINT=wss://api.example.com/device
      - GATEWAY_INTERNAL_URL=http://costrict-web-gateway-1:8081
      - SERVER_URL=http://costrict-web-api:8080
      - GATEWAY_REGION=default
      - GATEWAY_CAPACITY=1000
      - INTERNAL_SECRET=${INTERNAL_SECRET:?INTERNAL_SECRET is required}
    networks:
      - costrict-net
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:8081/health"]
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 5s

  gateway-2:
    image: ghcr.io/xdfield/costrict-web-gateway:latest
    container_name: costrict-web-gateway-2
    restart: always
    environment:
      - GATEWAY_ID=gateway-2
      - GATEWAY_ENDPOINT=wss://api.example.com/device
      - GATEWAY_INTERNAL_URL=http://costrict-web-gateway-2:8081
      - SERVER_URL=http://costrict-web-api:8080
      - GATEWAY_REGION=default
      - GATEWAY_CAPACITY=1000
      - INTERNAL_SECRET=${INTERNAL_SECRET:?INTERNAL_SECRET is required}
    networks:
      - costrict-net
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:8081/health"]
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 5s

networks:
  costrict-net:
    driver: bridge
```

说明：

- 所有 Gateway 的 `GATEWAY_ENDPOINT` 必须相同（APISIX 公网入口）。
- `GATEWAY_INTERNAL_URL` 使用容器名，Server 需要在同一 Docker network 内，或可通过该 hostname 解析。
- `INTERNAL_SECRET` 通过 `.env` 文件注入，与 Server 保持一致。
- 需要几个 Gateway 实例，就复制几份 `gateway-N` service。

---

## 2. 环境变量文件

`.env`：

```bash
INTERNAL_SECRET=change-me-to-a-long-random-string
```

---

## 3. APISIX 配置

### 3.1 Standalone 模式配置

创建 `apisix/config.yaml`：

```yaml
apisix:
  node_listen: 9443
  enable_admin: true
  admin_key:
    - name: admin
      key: your-admin-key-change-me
      role: admin

discovery:
  # Docker 环境下默认不使用 Kubernetes 服务发现
  # 使用静态 upstream nodes（见 routes 配置）

routes:
  - uri: /device/*
    enable_websocket: true
    plugins:
      serverless-pre-function:
        phase: rewrite
        functions:
          - "return function(conf, ctx) local m, err = ngx.re.match(ngx.var.uri, \"^/device/([^/]+)/\", \"jo\"); if m then ngx.req.set_header(\"X-Device-ID\", m[1]) end end"
    upstream:
      type: chash
      hash_on: header
      key: X-Device-ID
      scheme: http
      pass_host: pass
      nodes:
        "costrict-web-gateway-1:8081": 1
        "costrict-web-gateway-2:8081": 1

# SSL 证书（可选，生产必须配置）
ssl:
  enable: true
  listen:
    - port: 9443
      enable_http2: true
  cert: /usr/local/apisix/conf/cert.pem
  cert_key: /usr/local/apisix/conf/key.pem
```

> 生产环境务必替换 `your-admin-key-change-me` 为强密码，并挂载真实 TLS 证书。

### 3.2 使用 Admin API 动态配置

如果不使用 Standalone 模式，也可以在 APISIX 启动后通过 Admin API 创建路由：

```bash
ADMIN_KEY="your-admin-key"
APISIX_ADMIN="http://localhost:9180"

curl -i "$APISIX_ADMIN/apisix/admin/routes/gateway-tunnel" \
  -H "X-API-KEY: $ADMIN_KEY" \
  -X PUT \
  -d '{
    "uri": "/device/*",
    "enable_websocket": true,
    "plugins": {
      "serverless-pre-function": {
        "phase": "rewrite",
        "functions": [
          "return function(conf, ctx) local m, err = ngx.re.match(ngx.var.uri, "^/device/([^/]+)/", "jo"); if m then ngx.req.set_header("X-Device-ID", m[1]) end end"
        ]
      }
    },
    "upstream": {
      "type": "chash",
      "hash_on": "header",
      "key": "X-Device-ID",
      "scheme": "http",
      "pass_host": "pass",
      "nodes": {
        "costrict-web-gateway-1:8081": 1,
        "costrict-web-gateway-2:8081": 1
      }
    }
  }'
```

---

## 4. 启动服务

```bash
docker compose -f docker-compose.gateway.yml up -d
```

检查状态：

```bash
docker compose -f docker-compose.gateway.yml ps
```

---

## 5. 验证

### 5.1 查看 APISIX upstream nodes

```bash
ADMIN_KEY="your-admin-key"
curl "http://localhost:9180/apisix/admin/routes/gateway-tunnel" \
  -H "X-API-KEY: $ADMIN_KEY" | jq '.value.upstream.nodes'
```

应输出：

```json
{
  "costrict-web-gateway-1:8081": 1,
  "costrict-web-gateway-2:8081": 1
}
```

### 5.2 测试 WebSocket 粘滞

```bash
websocat "wss://api.example.com/device/test-device-001/tunnel?token=xxx"
```

查看 Gateway 日志：

```bash
docker logs -f costrict-web-gateway-1 | grep test-device-001
docker logs -f costrict-web-gateway-2 | grep test-device-001
```

多次重连同一个 `deviceID`，应始终落在同一个 Gateway 容器上。

---

## 6. 扩缩容

### 6.1 增加 Gateway 实例

1. 在 `docker-compose.gateway.yml` 中新增 `gateway-3` service。
2. 在 `apisix/config.yaml` 的 `nodes` 中新增 `"costrict-web-gateway-3:8081": 1`。
3. 重启 APISIX 以加载新配置（Standalone 模式）：

```bash
docker compose -f docker-compose.gateway.yml up -d --scale gateway-3=1
docker restart costrict-apisix
```

> 如果使用 Admin API，直接 PUT 更新 route 的 `nodes` 即可，无需重启 APISIX。

### 6.2 动态服务发现（推荐生产使用）

如果实例频繁变化，建议用 **Consul** 做服务注册与发现：

1. 每个 Gateway 启动时向 Consul 注册自己的地址。
2. APISIX 配置 DNS 服务发现：

```yaml
routes:
  - uri: /device/*
    enable_websocket: true
    plugins:
      serverless-pre-function:
        phase: rewrite
        functions:
          - "return function(conf, ctx) local m, err = ngx.re.match(ngx.var.uri, \"^/device/([^/]+)/\", \"jo\"); if m then ngx.req.set_header(\"X-Device-ID\", m[1]) end end"
    upstream:
      type: chash
      hash_on: header
      key: X-Device-ID
      discovery_type: dns
      service_name: costrict-gateway.service.consul:8081
```

> 注意：Consul 中必须为每个 Gateway 实例配置独立 SRV/A 记录，否则 DNS 轮询会破坏粘滞。

---

## 7. 常见问题

### 7.1 Server 无法访问 Gateway 内部地址

- 确保 Server 和 Gateway 在同一个 Docker network，或 Server 能通过 `GATEWAY_INTERNAL_URL` 访问 Gateway。
- 如果 Server 在容器外运行，把 `GATEWAY_INTERNAL_URL` 改成 `http://host.docker.internal:<host-port>` 或宿主机的 IP 和映射端口。

### 7.2 同一 deviceID 路由到不同 Gateway

- 检查 APISIX 是否配置了 `type: chash`、`hash_on: header`、`key: X-Device-ID`。
- 检查 `serverless-pre-function` 是否正确提取了 `X-Device-ID`。
- 检查 Gateway 容器的 `GATEWAY_ID` 是否唯一。

### 7.3 新增 Gateway 后 APISIX 未生效

- Standalone 模式下需要重启 APISIX 加载新配置。
- Admin API 模式下 PUT 更新 route 即可实时生效。

### 7.4 TLS/HTTPS 未生效

- 生产环境必须在 APISIX 配置真实证书。
- 测试环境可以用自签名证书，但 cs-cloud 客户端需要信任该证书。

---

## 8. 与 K8s 方案对比

| 能力 | K8s DaemonSet + APISIX | Docker Compose + APISIX |
|------|------------------------|-------------------------|
| 自动按 Node 扩展 | ✅ DaemonSet | ❌ 手动添加 service |
| APISIX 自动发现后端 | ✅ Kubernetes discovery | ❌ 静态 nodes / Consul |
| Gateway 扩缩容无需改配置 | ✅ | ❌ 需要更新 APISIX nodes |
| 会话粘滞 | ✅ chash by deviceID | ✅ chash by deviceID |
| 适合生产 | ✅ 推荐 | ⚠️ 中小规模/测试 |

---

## 9. 相关文件

- K8s 方案文档：`docs/deployment/gateway-daemonset-apisix-chash.md`
- Docker Compose 方案文档：`docs/deployment/gateway-docker-compose.md`
- Gateway Helm Chart：`deploy/charts/gateway/`
