# Gateway DaemonSet + APISIX chash 部署方案

本方案把 `costrict-web-gateway` 以 **DaemonSet** 方式部署（每个 Node 一个 Gateway Pod），并通过 **APISIX 的 Kubernetes 服务发现 + chash（一致性哈希）** 实现按 `deviceID` 的会话粘滞。

相比 Deployment + NodePort 方案，优势：

- 集群扩容/缩容时自动在每个 Node 上增删 Gateway Pod，**无需修改 APISIX 配置**。
- APISIX 通过服务发现自动获取所有 Gateway Pod 的 endpoint。
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
│  ├─ 从 /device/{deviceID}/... 提取 deviceID → X-Device-ID header
│  └─ chash 一致性哈希到 Gateway Pod
└──────┬───────────────────────┘
       │ Kubernetes 服务发现
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
- APISIX 的 ServiceAccount 有权限 watch Gateway 所在 namespace 的 Endpoints/EndpointSlices。
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

## 2. APISIX 配置

### 2.1 启用 Kubernetes 服务发现

在 APISIX 的 `config.yaml` 中增加：

```yaml
discovery:
  kubernetes:
    service:
      schema: https
      host: ${KUBERNETES_SERVICE_HOST}
      port: ${KUBERNETES_SERVICE_PORT}
    client:
      token_file: /var/run/secrets/kubernetes.io/serviceaccount/token
    namespace_selector:
      equal: costrict          # Gateway 所在 namespace
    watch_endpoint_slices: true
```

> 如果你通过 APISIX Helm Chart 部署，通常需要把这段配置放到 chart values 中对应的 `config` / `configuration` 字段下，具体路径取决于 chart 版本。

### 2.2 给 APISIX 授权

APISIX 需要能 list/watch Gateway 所在 namespace 的 Endpoints/EndpointSlices。

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: apisix-discovery
  namespace: costrict          # Gateway 所在 namespace
rules:
  - apiGroups: [""]
    resources: ["endpoints", "services", "pods"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: apisix-discovery
  namespace: costrict
subjects:
  - kind: ServiceAccount
    name: apisix-service-account   # 改成 APISIX 实际使用的 ServiceAccount 名称
    namespace: apisix              # APISIX 所在 namespace
roleRef:
  kind: Role
  name: apisix-discovery
  apiGroup: rbac.authorization.k8s.io
```

如果 APISIX 需要跨 namespace 发现服务，使用 `ClusterRole` + `ClusterRoleBinding`。

### 2.3 创建 WebSocket 路由

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
    "plugins": {
      "serverless-pre-function": {
        "phase": "rewrite",
        "functions": [
          "return function(conf, ctx) local m, err = ngx.re.match(ngx.var.uri, \"^/device/([^/]+)/\", \"jo\"); if m then ngx.req.set_header(\"X-Device-ID\", m[1]) end end"
        ]
      }
    },
    "upstream": {
      "type": "chash",
      "hash_on": "header",
      "key": "X-Device-ID",
      "discovery_type": "kubernetes",
      "service_name": "costrict/costrict-web-gateway:8081",
      "scheme": "http",
      "pass_host": "pass"
    }
  }'
```

字段说明：

| 字段 | 说明 |
|------|------|
| `uri: /device/*` | 匹配 cs-cloud 的 WebSocket 路径 `/device/{deviceID}/tunnel` |
| `enable_websocket: true` | 允许 WebSocket upgrade |
| `serverless-pre-function` | Lua 插件，从 path 提取 `deviceID` 写入 `X-Device-ID` header |
| `type: chash` | 一致性哈希负载均衡 |
| `hash_on: header` / `key: X-Device-ID` | 按 `deviceID` 哈希 |
| `discovery_type: kubernetes` | 从 K8s 自动发现后端 |
| `service_name` | `namespace/service-name:port`，对应 Gateway Service |
| `pass_host: pass` | 透传原始 Host header |

#### 方式 B：APISIX Ingress Controller CRD

```yaml
apiVersion: apisix.apache.org/v2
kind: ApisixUpstream
metadata:
  name: costrict-web-gateway
  namespace: costrict
spec:
  loadbalancer:
    type: chash
    hashOn: header
    key: "X-Device-ID"
---
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
        - serviceName: costrict-web-gateway
          servicePort: 8081
      plugins:
        - name: serverless-pre-function
          enable: true
          config:
            phase: rewrite
            functions:
              - "return function(conf, ctx) local m, err = ngx.re.match(ngx.var.uri, \"^/device/([^/]+)/\", \"jo\"); if m then ngx.req.set_header(\"X-Device-ID\", m[1]) end end"
```

> 不同版本 APISIX Ingress Controller 的 CRD 字段可能略有差异。如果 `websocket` 不生效，可尝试在 `match` 同级加 `enable_websocket: true`。

---

## 3. 验证

### 3.1 查看 APISIX 自动发现的 upstream nodes

```bash
curl "$APISIX_ADMIN/apisix/admin/routes/gateway-tunnel" \
  -H "X-API-KEY: $ADMIN_KEY" | jq '.value.upstream.nodes'
```

应输出类似：

```json
{
  "10.0.1.10:8081": 50,
  "10.0.2.15:8081": 50,
  "10.0.3.20:8081": 50
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

新增 Node 后，DaemonSet 会自动在新 Node 上拉起 Gateway Pod。APISIX 的服务发现会在数秒内更新 upstream nodes，无需手动修改 APISIX 配置。

---

## 4. 常见问题

### 4.1 APISIX 发现不到 Gateway 节点

- 检查 APISIX 的 ServiceAccount 是否正确绑定了 Role/ClusterRole。
- 检查 `service_name` 是否为 `namespace/service-name:port` 格式。
- 检查 APISIX `config.yaml` 中的 `namespace_selector` 是否包含 Gateway 所在 namespace。

### 4.2 同一 deviceID 路由到不同 Gateway

- 检查 `serverless-pre-function` 是否正确提取了 `X-Device-ID`。
- 检查 upstream 是否为 `type: chash` 且 `hash_on: header`、`key: X-Device-ID`。
- 检查 Gateway Pod 是否使用了唯一的 `GATEWAY_ID`（DaemonSet 下应为 Node 名称）。

### 4.3 Gateway 重启后设备连接失败

- Gateway 重启后 Pod IP 会变，APISIX 服务发现会自动更新。
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
  --set replicaCount=3
```

同时更新 APISIX 路由的 `service_name` 或直接使用静态 nodes。

---

## 6. 相关文件

- Helm Chart：`deploy/charts/gateway/`
  - `templates/daemonset.yaml` — DaemonSet 定义
  - `templates/deployment.yaml` — Deployment 定义（默认）
  - `values.yaml` — `daemonSet.enabled` 开关
- 本文档：`docs/deployment/gateway-daemonset-apisix-chash.md`
