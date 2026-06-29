# Gateway DaemonSet + nginx-router chash 部署方案

本方案把 `costrict-web-gateway` 以 **DaemonSet** 方式部署（每个 Node 一个 Gateway Pod），并在 APISIX 与 Gateway 之间增加一个 **OpenResty nginx-router 中间层**，由 nginx-router 负责：

- 从 `/device/{deviceID}/...` 提取 `deviceID`。
- 使用一致性哈希（chash）按 `deviceID` 选择 Gateway Pod。
- 通过解析 Gateway 的 **headless Service DNS** 自动发现所有 Gateway Pod IP（无需 K8s API / RBAC）。

相比原 APISIX chash 方案，优势：

- 集群扩容/缩容时自动在每个 Node 上增删 Gateway Pod，**无需修改 APISIX 配置**。
- APISIX 退化为只做 TLS 终止 + WebSocket 透传的薄入口，不再需要 K8s 服务发现及对应 RBAC。
- 同一台设备（`deviceID`）始终被路由到同一个 Gateway Pod。

---

## 架构

```text
┌─────────────┐
│   cs-cloud   │
└──────┬──────┘
       │ wss://api.example.com/device/{deviceID}/tunnel
       ▼
┌──────────────────────────────┐
│           APISIX             │
│  只做 TLS 终止 + WebSocket 透传 │
│  1 条路由: /device/*  → nginx-router │
└──────┬───────────────────────┘
       │
       ▼
┌─────────────────────────────────────┐
│         nginx-router (OpenResty)     │
│  ├─ 从 /device/{deviceID}/... 提取 deviceID
│  ├─ chash 一致性哈希到 Gateway Pod
│  └─ headless Service DNS 发现 Pod
└──────┬──────────────────────────────┘
       ▼
┌──────────────────────────────┐
│   Gateway DaemonSet Pod      │  （每个 Node 一个）
│   yamux session with cs-cloud │
└──────────────────────────────┘
```

Server 到设备的反向流量仍然走 `GATEWAY_INTERNAL_URL`（Pod IP），**不经过 APISIX**。

---

## 前置条件

- Kubernetes 集群已部署 APISIX（推荐也部署在 K8s 内）。
- APISIX 不再需要 watch Gateway 所在 namespace 的 Endpoints/EndpointSlices；Pod 发现由 nginx-router 通过 DNS 完成。
- `costrict-web-server`（API）和 Gateway 使用 **Redis** 作为 `GatewayStore`。

---

## 1. Gateway 改造为 DaemonSet

仓库 Helm Chart 已支持通过 `daemonSet.enabled=true` 切换到 DaemonSet 模式。

### 1.1 关键改动说明

- `GATEWAY_ID` 使用 **Node 名称**，同一节点重启后 ID 不变。
- `GATEWAY_INTERNAL_URL` 使用 **Pod IP**，Server 直接回连该 Pod。
- `GATEWAY_ENDPOINT` 统一填 APISIX 公网地址（所有 Gateway 返回给设备的地址相同）。

### 1.2 部署命令

```bash
helm upgrade --install costrict-web-gateway ./deploy/charts/gateway \
  --namespace costrict \
  --set daemonSet.enabled=true \
  --set service.type=ClusterIP \
  --set config.serverUrl="http://costrict-web-api:8080" \
  --set config.endpoint="wss://api.example.com/device" \
  --set config.region="default" \
  --set config.capacity=1000
```

> 默认 `daemonSet.enabled=false`，即保持原来的 Deployment 模式，兼容现有部署。

### 1.3 验证 Gateway Pod

```bash
kubectl get daemonset -n costrict costrict-web-gateway
kubectl get pods -n costrict -l app.kubernetes.io/name=gateway -o wide
```

每个 Node 上应该有一个 Gateway Pod，且 `READY` 为 `1/1`。

---

## 2. APISIX 配置（薄透传）

本方案中 APISIX 只负责把公网 WebSocket 流量原样透传给后端的 nginx-router，
不再需要 K8s 服务发现、RBAC、`serverless-pre-function` 或 `chash` upstream。

### 2.1 创建 WebSocket 透传路由

#### 方式 A：APISIX Admin API

```bash
ADMIN_KEY="your-admin-key"
APISIX_ADMIN="http://apisix-admin:9180"

curl -i "$APISIX_ADMIN/apisix/admin/routes/gateway-tunnel" \
  -H "X-API-KEY: $ADMIN_KEY" \
  -X PUT \
  -d '{
    "uri": "/device/*",
    "enable_websocket": true,
    "upstream": {
      "type": "roundrobin",
      "nodes": {
        "costrict-web-gateway-nginx-router:8080": 1
      },
      "scheme": "http",
      "pass_host": "pass"
    }
  }'
```

> 把 `costrict-web-gateway-nginx-router` 替换成实际 namespace 下的 Service 名（如 `costrict-web-gateway-nginx-router.costrict`）。

字段说明：

| 字段 | 说明 |
|------|------|
| `uri: /device/*` | 匹配 cs-cloud 的 WebSocket 路径 `/device/{deviceID}/tunnel` |
| `enable_websocket: true` | 允许 WebSocket upgrade |
| `type: roundrobin` | APISIX 到 nginx-router 用简单轮询即可；粘滞由 nginx-router 保证 |
| `nodes` | nginx-router 的 ClusterIP Service |
| `pass_host: pass` | 透传原始 Host header |

#### 方式 B：APISIX Ingress Controller CRD

```yaml
apiVersion: apisix.apache.org/v2
kind: ApisixRoute
metadata:
  name: gateway-tunnel
  namespace: costrict
spec:
  http:
    - name: tunnel
      match:
        paths:
          - /device/*
      websocket: true
      backends:
        - serviceName: costrict-web-gateway-nginx-router
          servicePort: 8080
```

### 2.2 部署 nginx-router 中间层

nginx-router 由 Gateway Helm Chart 内置，通过 `--set nginxRouter.enabled=true` 启用。

它做三件事：

1. 从请求路径 `/device/{deviceID}/...` 提取 `deviceID`。
2. 维护一个基于 `deviceID` 的一致性哈希环（纯 Lua ketama 实现，随 ConfigMap 下发，无额外编译依赖）。
3. 每隔 `nginxRouter.discovery.refreshIntervalMs`（默认 5s）解析 Gateway 的 headless Service DNS，拿到当前所有 Gateway Pod IP，重建哈希环。

因为环是从**排序后的 Pod IP 集合**构建的，多个 nginx-router 副本各自重建出的环一致，所以同一 `deviceID` 无论打到哪个副本都会落到同一个 Gateway Pod。

#### 2.2.1 启用 nginx-router

在已有的 DaemonSet 部署命令上加上 `nginxRouter.enabled=true`：

```bash
helm upgrade --install costrict-web-gateway ./deploy/charts/gateway \
  --namespace costrict \
  --set daemonSet.enabled=true \
  --set service.type=ClusterIP \
  --set config.serverUrl="http://costrict-web-api:8080" \
  --set config.endpoint="wss://api.example.com/device" \
  --set config.region="default" \
  --set config.capacity=1000 \
  --set nginxRouter.enabled=true
```

启用后会额外创建：

- `costrict-web-gateway-headless`：Gateway 的 headless Service，用于 DNS 发现。
- `costrict-web-gateway-nginx-router`：nginx-router Deployment 与 ClusterIP Service。
- `costrict-web-gateway-nginx-router-config`：包含 `nginx.conf` 与 `router.lua` 的 ConfigMap。

#### 2.2.2 关键配置说明

`deploy/charts/gateway/values.yaml` 中新增：

```yaml
nginxRouter:
  enabled: false
  replicaCount: 2
  image:
    repository: openresty/openresty
    tag: "1.25.3.1-0-alpine"
    pullPolicy: IfNotPresent
  service:
    type: ClusterIP
    port: 8080
  discovery:
    headlessServiceName: ""      # 留空默认使用 <fullname>-headless
    gatewayPort: 8081
    refreshIntervalMs: 5000       # DNS 轮询间隔
  resolver: "kube-dns.kube-system.svc.cluster.local"
```

> 如果你的集群 DNS 不是 `kube-dns.kube-system.svc.cluster.local`（如 CoreDNS 在其它地址），把 `nginxRouter.resolver` 改成集群实际的 nameserver。

#### 2.2.3 nginx.conf 核心逻辑

ConfigMap 中的 `nginx.conf` 主要包含：

- `resolver` 指向 K8s DNS。
- `init_worker_by_lua_block` 启动定时器，周期性解析 `costrict-web-gateway-headless.costrict.svc.cluster.local`。
- `/device/{deviceID}/` 这个 location 用正则把 `deviceID` 捕获到变量，WebSocket 透传到 `gateway_backend`。
- `upstream gateway_backend` 的 `balancer_by_lua_block` 从 shared dict 读取已排序的 Pod IP 列表，用 `deviceID` 做 ketama 一致性哈希选 peer。
- `/router_status` 调试接口输出当前发现的 Pod 列表。

`router.lua` 是随 ConfigMap 下发的纯 Lua 辅助模块，提供 DNS 解析与 ketama 哈希函数，不依赖任何第三方 `.so`。

---

## 3. 验证

### 3.1 查看 nginx-router 发现到的 Gateway Pod

```bash
kubectl port-forward -n costrict svc/costrict-web-gateway-nginx-router 8080:8080 &
curl -s http://localhost:8080/router_status | jq
```

应输出类似：

```json
{
  "source": "nginx-router",
  "discovered_ips": [
    "10.0.1.10:8081",
    "10.0.2.15:8081",
    "10.0.3.20:8081"
  ]
}
```

节点数量应等于集群中 Gateway DaemonSet Pod 数量。

### 3.2 测试 WebSocket 粘滞

```bash
websocat "wss://api.example.com/device/test-device-001/tunnel?token=xxx"
```

查看 Gateway 日志：

```bash
kubectl logs -n costrict -l app.kubernetes.io/name=gateway --tail=100 | grep test-device-001
```

多次重连同一个 `deviceID`，应始终落在同一个 Gateway Pod 上。

### 3.3 验证集群扩容

新增 Node 后，DaemonSet 会自动在新 Node 上拉起 Gateway Pod；headless Service DNS 会在数秒内更新。nginx-router 在 `refreshIntervalMs`（默认 5s）内刷新哈希环，无需手动修改 APISIX 或 nginx 配置。

---

## 4. 常见问题

### 4.1 nginx-router 发现不到 Gateway 节点

- 检查 Gateway headless Service 是否已创建：`kubectl get svc -n costrict costrict-web-gateway-headless`。
- 检查 `nginxRouter.resolver` 是否配置为集群实际 DNS 地址。
- 进入 nginx-router Pod 执行 `nslookup costrict-web-gateway-headless.costrict.svc.cluster.local` 看能否解析到所有 Pod IP。
- 检查 `nginxRouter.discovery.gatewayPort` 是否与 Gateway 实际监听端口一致。

### 4.2 同一 deviceID 路由到不同 Gateway

- 检查 nginx-router 日志中的 `device_id` 是否提取正确。
- 检查 `/router_status` 输出在多个 nginx-router Pod 上是否一致（IP 集合排序后应相同）。
- 检查 Gateway Pod 是否使用了唯一的 `GATEWAY_ID`（DaemonSet 下应为 Node 名称）。

### 4.3 Gateway 重启后设备连接失败

- Gateway 重启后 Pod IP 会变；headless DNS 更新后 nginx-router 会在刷新间隔内重建环。
- 设备重连后会重新绑定到新的 Gateway Pod，属于正常行为。
- 如果长时间失败，检查 Redis Store 中的 Gateway 心跳是否已超时并被清理。

### 4.4 Server 回包失败

- Server 通过 `GATEWAY_INTERNAL_URL`（Pod IP）直接访问 Gateway。
- 确保 Server 和 Gateway 在同一集群网络内可互通。
- 确保 `GATEWAY_INTERNAL_URL` 是 Pod IP，而不是 Service 的 ClusterIP。

---

## 5. 回滚到 Deployment 模式

如果需要切回 Deployment：

```bash
helm upgrade --install costrict-web-gateway ./deploy/charts/gateway \
  --namespace costrict \
  --set daemonSet.enabled=false \
  --set nginxRouter.enabled=false \
  --set replicaCount=3
```

如果仍想保留 nginx-router 但使用 Deployment 模式，保持 `nginxRouter.enabled=true` 即可；
headless Service 会选中 Deployment 的 Pod IP，chash 行为不变。

---

## 6. 相关文件

- Helm Chart：`deploy/charts/gateway/`
  - `templates/daemonset.yaml` — DaemonSet 定义
  - `templates/deployment.yaml` — Deployment 定义（默认）
  - `values.yaml` — `daemonSet.enabled` 开关
- 本文档：`docs/deployment/gateway-daemonset-apisix-chash.md`
