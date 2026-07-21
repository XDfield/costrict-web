# Gateway StatefulSet + nginx-router 静态 FQDN 半手动部署示例

本目录提供一套**去除 Helm 模板语法**的静态 Kubernetes manifest 示例，
适用于容器云平台无法直接渲染 Helm chart 变量的场景。

相比原 DaemonSet + headless Service DNS 自动发现方案，本方案：

- 使用 **StatefulSet** 提供稳定的 Pod 名（`gateway-0`, `gateway-1`…）。
- nginx-router 按副本数**自动生成 StatefulSet Pod FQDN** 并逐个解析，不依赖 headless Service DNS 自动发现。
- 运维只需维护**副本数**(`gateway_replicas`)，**无需跟踪漂移的 Pod IP，也无需手填 FQDN 列表**。

> 如果你可以使用 Helm，更推荐直接用 chart 部署：
> ```bash
> helm upgrade --install costrict-web-gateway ./deploy/charts/gateway \
>   --set statefulSet.enabled=true \
>   --set replicaCount=2 \
>   --set nginxRouter.enabled=true \
>   --set nginxRouter.discovery.mode=static
> ```
>
> static 模式下 `staticFQDNs` 留空即按 `replicaCount` 自动生成 Pod FQDN。

## 文件说明

| 文件 | 用途 |
|---|---|
| `headless-service.yaml` | Gateway 的 headless Service，为 StatefulSet 提供稳定网络标识 |
| `statefulset.yaml` | Gateway StatefulSet，Pod 名稳定 |
| `nginx-router-configmap.yaml` | nginx-router 配置与 Lua 路由逻辑（static 模式） |
| `nginx-router-deployment.yaml` | nginx-router 部署 |
| `nginx-router-service.yaml` | nginx-router 的 ClusterIP Service（APISIX 上游） |
| `generate.sh` | 用 Helm 重新生成本目录 ConfigMap 的辅助脚本 |

## 快速部署步骤

1. **替换所有占位符**

   在 `headless-service.yaml`、`statefulset.yaml`、`nginx-router-configmap.yaml`、
   `nginx-router-deployment.yaml`、`nginx-router-service.yaml` 中，把以下占位符替换为实际值：

   - `<NAMESPACE>` — 命名空间
   - `<RELEASE_NAME>` — release 名，如 `costrict-web-gateway`
   - `<CLUSTER_DNS>` — （可选）集群 DNS 服务地址，如 `kube-dns.kube-system.svc.cluster.local`；留空时 nginx-router 自动从 `/etc/resolv.conf` 探测
   - `<GATEWAY_IMAGE>` — Gateway 镜像，如 `ghcr.io/xdfield/costrict-web-gateway:latest`
   - `<GATEWAY_PORT>` — Gateway 端口，默认 `8081`
   - `<SERVER_URL>` — Server 内网地址，如 `http://costrict-web-api:8080`
   - `<GATEWAY_ENDPOINT>` — 设备连接的公网入口，如 `wss://api.example.com/device`
   - `<GATEWAY_REGION>` / `<GATEWAY_CAPACITY>` — region / 容量
   - `<GATEWAY_ID_PREFIX>` — Gateway ID 前缀，可留空
   - `<INTERNAL_SECRET>` — 与 Server 内部接口共享的密钥

2. **确认副本数一致**

   在 `nginx-router-configmap.yaml` 的 `nginx.conf` 中，找到
   `local gateway_replicas = {{REPLICAS}}`，替换为 StatefulSet 实际副本数。
   nginx-router 会据此自动生成 `<RELEASE_NAME>-0/1/...` 的 Pod FQDN，
   cluster domain 运行时自动探测，无需手填。

3. **应用 manifest**

   ```bash
   kubectl apply -f examples/gateway-statefulset-static/
   ```

4. **验证**

   ```bash
   kubectl get statefulset -n <NAMESPACE>
   kubectl get pods -n <NAMESPACE> -l app.kubernetes.io/name=gateway
   kubectl port-forward -n <NAMESPACE> svc/<RELEASE_NAME>-nginx-router 8080:8080 &
   curl -s http://localhost:8080/router_status | jq
   ```

   应返回解析后的 Gateway Pod IP 列表。

## 扩缩容与 Pod 重建

- **扩缩容**：修改 StatefulSet `replicas` 和 `nginx-router-configmap.yaml` 中的
  `local gateway_replicas`，然后重新应用 ConfigMap，最后滚动重启 nginx-router Deployment：

  ```bash
  kubectl rollout restart deployment/<RELEASE_NAME>-nginx-router -n <NAMESPACE>
  ```

- **Pod 重建**：StatefulSet Pod 名不变，但 IP 可能变化。nginx-router 每 5 秒刷新一次 FQDN，
  自动解析到新的 IP，无需手动修改 IP。

## 从 DaemonSet / Deployment 迁移

1. 用新 release 名部署本 StatefulSet 方案，避免标签冲突。
2. 更新 APISIX 路由指向新的 nginx-router Service。
3. 等待设备重连并重新分配。
4. 下线旧的 DaemonSet / Deployment。
5. 旧 Gateway ID 会在心跳超时（默认 60 秒）后从 Server 注册表自动清理。

## 重新生成 ConfigMap

当升级 Gateway chart 后，可用 `generate.sh` 重新生成 `nginx-router-configmap.yaml`：

```bash
cd examples/gateway-statefulset-static
./generate.sh <RELEASE_NAME> <NAMESPACE> <REPLICAS> [CLUSTER_DNS]
```

`CLUSTER_DNS` 为可选参数；省略时 nginx-router 会自动从 Pod 的 `/etc/resolv.conf` 探测 DNS 配置。仅在 hostNetwork、自定义 `dnsConfig` 等非常规场景需要显式指定。

然后再次替换生成的占位符即可。
