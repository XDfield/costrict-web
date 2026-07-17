# Gateway StatefulSet + nginx-router 静态 FQDN 部署方案

本方案适用于容器云环境复杂、集群 DNS 不一致或无法使用 Helm 变量渲染的场景。

与 [DaemonSet + headless Service DNS 自动发现方案](./gateway-daemonset-apisix-chash.md) 相比：

- **Gateway 改为 StatefulSet**：Pod 名稳定（`costrict-web-gateway-0`、`costrict-web-gateway-1`…），重启后 `GATEWAY_ID` 不变。
- **nginx-router 改为静态 FQDN 发现**：不再解析 headless Service DNS A 记录，而是逐个解析运维配置的固定 Pod FQDN。
- **无需维护漂移的 Pod IP**：IP 漂移由 nginx-router 定时刷新自动跟踪，运维只维护稳定的 FQDN 列表。

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
│  └─ 解析固定的 StatefulSet Pod FQDN 列表
└──────┬──────────────────────────────┘
       │ (轮询解析每个 Pod FQDN 到当前 IP)
       ▼
┌──────────────────────────────┐
│   Gateway StatefulSet Pod    │
│   yamux session with cs-cloud │
│   costrict-web-gateway-0/1/...│
└──────────────────────────────┘
```

Server 到设备的反向流量仍然走 `GATEWAY_INTERNAL_URL`（Pod IP），**不经过 APISIX/nginx-router**。

## 前置条件

- Kubernetes 集群已部署 APISIX（推荐也部署在 K8s 内）。
- `costrict-web-server`（API）和 Gateway 使用 **Redis** 作为 `GatewayStore`。
- 容器云平台可创建 StatefulSet、Service、Deployment、ConfigMap。
- 集群 DNS 能够解析单个 StatefulSet Pod FQDN（不需要一致的 headless Service DNS 发现）。
- nginx-router 默认会从 Pod 的 `/etc/resolv.conf` 自动探测 nameserver 和 cluster domain；仅在非常规场景需要显式指定。

## 1. 部署 Gateway StatefulSet

### 1.1 Helm 方式（推荐，若平台支持 Helm）

```bash
helm upgrade --install costrict-web-gateway ./deploy/charts/gateway \
  --namespace costrict \
  --set statefulSet.enabled=true \
  --set service.type=ClusterIP \
  --set config.serverUrl="http://costrict-web-api:8080" \
  --set config.endpoint="wss://api.example.com/device" \
  --set config.region="default" \
  --set config.capacity=1000 \
  --set nginxRouter.enabled=true \
  --set nginxRouter.discovery.mode=static \
  --set 'nginxRouter.discovery.staticFQDNs={costrict-web-gateway-0.costrict-web-gateway-headless.costrict.svc.cluster.local,costrict-web-gateway-1.costrict-web-gateway-headless.costrict.svc.cluster.local}'
```

关键参数：

| 参数 | 说明 |
|---|---|
| `statefulSet.enabled=true` | 启用 StatefulSet 模式，与 Deployment/DaemonSet 互斥 |
| `nginxRouter.discovery.mode=static` | nginx-router 使用固定 FQDN 列表 |
| `nginxRouter.discovery.staticFQDNs` | 逗号分隔的 StatefulSet Pod FQDN 列表 |
| `nginxRouter.resolver` | （可选）集群 DNS 地址；留空时自动从 `/etc/resolv.conf` 探测 |
| `nginxRouter.discovery.clusterDomain` | （可选）集群域名后缀；留空时自动探测 |

### 1.2 半手动方式（容器云无法渲染 Helm）

若容器云平台无法使用 Helm，请使用 `examples/gateway-statefulset-static/` 目录下的静态 manifest：

1. 复制示例目录到本地：
   ```bash
   cp -r examples/gateway-statefulset-static /path/to/your/project
   ```

2. 运行生成脚本获得最新的 `nginx-router-configmap.yaml`：
   ```bash
   cd /path/to/your/project/gateway-statefulset-static
   ./generate.sh costrict-web-gateway costrict \
     "costrict-web-gateway-0.costrict-web-gateway-headless.costrict.svc.cluster.local,costrict-web-gateway-1.costrict-web-gateway-headless.costrict.svc.cluster.local"
   ```

   `CLUSTER_DNS` 为可选参数；省略时 nginx-router 会自动从 Pod 的 `/etc/resolv.conf` 探测 DNS 配置。仅在 hostNetwork、自定义 `dnsConfig` 等非常规场景需要显式指定，例如：

   ```bash
   ./generate.sh costrict-web-gateway costrict \
     "costrict-web-gateway-0.costrict-web-gateway-headless.costrict.svc.cluster.local,costrict-web-gateway-1.costrict-web-gateway-headless.costrict.svc.cluster.local" \
     kube-dns.kube-system.svc.cluster.local
   ```

3. 替换所有 `{{...}}` 占位符为实际值。

4. 在容器云平台依次应用：
   ```bash
   kubectl apply -f headless-service.yaml
   kubectl apply -f statefulset.yaml
   kubectl apply -f nginx-router-configmap.yaml
   kubectl apply -f nginx-router-deployment.yaml
   kubectl apply -f nginx-router-service.yaml
   ```

详见 `examples/gateway-statefulset-static/README.md`。

## 2. APISIX 配置

与 DaemonSet 方案相同，APISIX 只做薄透传：

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

## 3. 验证

### 3.1 查看 StatefulSet Pod

```bash
kubectl get statefulset -n costrict costrict-web-gateway
kubectl get pods -n costrict -l app.kubernetes.io/name=gateway -o wide
```

应看到 `costrict-web-gateway-0`、`costrict-web-gateway-1` 等稳定 Pod 名。

### 3.2 查看 nginx-router 发现到的 Gateway IP

```bash
kubectl port-forward -n costrict svc/costrict-web-gateway-nginx-router 8080:8080 &
curl -s http://localhost:8080/router_status | jq
```

应输出：

```json
{
  "source": "nginx-router",
  "mode": "static",
  "discovered_ips": [
    "10.0.1.10:8081",
    "10.0.1.11:8081"
  ],
  "dns_source": "auto",
  "nameservers": ["10.96.0.10"],
  "cluster_domain": "cluster.local"
}
```

### 3.3 测试 WebSocket 粘滞

```bash
websocat "wss://api.example.com/device/test-device-001/tunnel?token=xxx"
```

多次重连同一 `deviceID`，应始终落在同一个 Gateway Pod 上。

## 4. 扩缩容与 Pod 重建

- **扩缩容**：
  1. 修改 StatefulSet `replicas`。
  2. 同步更新 `nginxRouter.discovery.staticFQDNs`（Helm）或 `nginx-router-configmap.yaml` 中的 FQDN 列表（半手动）。
  3. 滚动重启 nginx-router 以加载新 ConfigMap：
     ```bash
     kubectl rollout restart deployment/costrict-web-gateway-nginx-router -n costrict
     ```

- **Pod 重建**：
  StatefulSet Pod 名不变，IP 可能变化。nginx-router 每 5 秒刷新 FQDN，自动解析到新 IP，**无需手动修改 IP**。

## 5. 从 DaemonSet / Deployment 迁移

1. 以新 release 名（如 `costrict-web-gateway-ss`）部署 StatefulSet，避免标签冲突。
2. 更新 APISIX 路由指向新的 nginx-router Service。
3. 等待设备重连并重新分配。
4. 下线旧的 DaemonSet / Deployment。
5. 旧 Gateway ID 会在 60 秒心跳超时后从 `GatewayRegistry` 自动清理。

## 6. 常见问题

### 6.1 nginx-router 发现不到 Gateway

- 检查 FQDN 是否正确：`costrict-web-gateway-0.costrict-web-gateway-headless.costrict.svc.cluster.local`
- 在 nginx-router Pod 内执行解析测试：
  ```bash
  kubectl exec -n costrict deploy/costrict-web-gateway-nginx-router -- nslookup costrict-web-gateway-0.costrict-web-gateway-headless.costrict.svc.cluster.local
  ```
- 如果 `/etc/resolv.conf` 自动探测失败（如 hostNetwork、自定义 `dnsConfig`），可显式设置 `nginxRouter.resolver` 或 `CLUSTER_DNS` 占位符。

### 6.2 同一 deviceID 路由到不同 Pod

- 检查所有 nginx-router 副本的 `/router_status` 返回的 IP 列表是否一致。
- 确保 FQDN 列表在所有副本中相同，且解析结果一致。
- StatefulSet Pod 重建后 IP 变化是正常的，但多个副本必须在同一时间点看到相同的有序 IP 集合。

### 6.3 从 StatefulSet 回滚到 DaemonSet

- 切换 `statefulSet.enabled=false`、`daemonSet.enabled=true`。
- 注意 `GATEWAY_ID` 会从 Pod 名变为 Node 名，已绑定设备会重新分配。
