# 设计：用 nginx 中间层承接 deviceID 提取 + chash 路由

- **日期**：2026-06-24
- **目标文档**：`docs/deployment/gateway-daemonset-apisix-chash.md`
- **目标 Chart**：`deploy/charts/gateway/`

## 1. 背景与目标

现有部署方案（`gateway-daemonset-apisix-chash.md`）把以下三件事都压在 APISIX 上：

1. Kubernetes 服务发现（`discovery.kubernetes`，§2.1）
2. 服务发现所需的 RBAC（Role/RoleBinding，§2.2）
3. WebSocket 路由：`serverless-pre-function` Lua 提取 `deviceID` + `chash` 一致性哈希 upstream + `discovery_type: kubernetes`（§2.3）

这让 APISIX 的部署配置变得复杂，且与具体业务路由逻辑强耦合。

**目标**：把「从 path 提取 deviceID + 按 deviceID 一致性哈希到 Gateway Pod + 发现 Gateway Pod」整体下沉到一个独立的 **nginx（OpenResty）中间层容器**，配置集中在 nginx 自己的 ConfigMap 中。APISIX 退化为一个**只做 TLS 终止 + WebSocket 透传**的薄入口。

**非目标**：

- 不改动 Gateway 自身的会话逻辑、`GATEWAY_INTERNAL_URL` 反向回连机制。
- 不改动 Server→设备 的反向流量路径（仍走 Pod IP，不经过 APISIX / nginx）。
- 不引入额外的 K8s controller / operator。

## 2. 架构

```text
cs-cloud
  │ wss://api.example.com/device/{deviceID}/tunnel
  ▼
┌─────────────────────────────┐
│  APISIX  (薄透传)             │   1 条路由: uri=/device/* , enable_websocket=true
│  无 discovery / 无 RBAC       │   upstream = nginx-router Service (roundrobin)
│  无 serverless-pre-function   │
└──────┬──────────────────────┘
       ▼
┌─────────────────────────────┐
│  nginx-router (OpenResty)     │   Deployment, 默认 2 副本, ClusterIP Service :8080
│  ├─ 从 path 提取 deviceID      │
│  ├─ chash(deviceID) 选 Pod    │   resty.chash 一致性哈希环
│  └─ DNS 轮询 headless svc 发现 │   无需 K8s API / 无需 RBAC
└──────┬──────────────────────┘
       ▼
┌─────────────────────────────┐
│  Gateway DaemonSet Pods       │   新增 headless Service 暴露所有 Pod IP
│  yamux session with cs-cloud  │
└─────────────────────────────┘
```

Server→设备的反向流量仍走 `GATEWAY_INTERNAL_URL`（Pod IP），**不变**。

### 关键决策

| 决策点 | 选择 | 理由 |
|--------|------|------|
| Pod 发现机制 | OpenResty + Lua，DNS 轮询 headless Service | 无需 K8s RBAC/Token，配置全在 nginx |
| APISIX 角色 | 薄透传（保留 TLS + 公网入口） | 不重做 TLS/证书/公网暴露 |
| 交付形态 | 扩展现有 gateway chart（`nginxRouter.enabled`）+ 更新文档 | 与 Gateway 同 chart，复用 namespace/labels |
| 一致性哈希实现 | 把纯 Lua 的 `resty.chash`（ketama）vendoring 进 ConfigMap | 用 stock `openresty/openresty` 镜像，零构建步骤 |

## 3. 组件设计：nginx-router（扩展 gateway chart）

在 `deploy/charts/gateway/values.yaml` 新增可选组件开关，默认关闭以兼容现有部署：

```yaml
nginxRouter:
  enabled: false
  replicaCount: 2
  image:
    repository: openresty/openresty
    tag: "1.25.3.1-alpine"      # stock 镜像，具体 tag 实现时确认
    pullPolicy: IfNotPresent
  service:
    type: ClusterIP
    port: 8080
  # Gateway headless Service 的 DNS 名（用于发现 Pod）
  discovery:
    headlessServiceName: ""      # 留空则默认 <fullname>-headless
    gatewayPort: 8081
    refreshIntervalMs: 5000      # 重新解析 DNS 的间隔
  resolver: "kube-dns.kube-system.svc.cluster.local"   # 或 169.254.20.10 等
  resources:
    limits: { cpu: 500m, memory: 256Mi }
    requests: { cpu: 50m, memory: 64Mi }
```

新增模板（复用 `gateway.labels` / `gateway.namespace` helper，新增 `nginxRouter.*` selector labels）：

### 3.1 `templates/nginx-configmap.yaml`

包含两部分内容：

1. **`nginx.conf`**：
   - `resolver <kube-dns>;`
   - 一个 `lua_shared_dict` 用于跨 worker 缓存当前 Pod 列表 / 环。
   - `init_worker_by_lua_block`：启动一个 `ngx.timer.every(refreshInterval)` 定时器，用 `lua-resty-dns` 解析 headless Service 的 A 记录，拿到全部 Gateway Pod IP，**按 IP 排序**后用 `resty.chash:reinit()` 重建一致性哈希环，存入 shared dict / 模块级变量。
   - `server { listen 8080; location ~ ^/device/ { ... } }`：
     - `set_by_lua` / `ngx.re.match(ngx.var.uri, "^/device/([^/]+)/")` 提取 `deviceID`。
     - `balancer_by_lua_block`：用 `chash:find(deviceID)` 选定 peer，`balancer.set_current_peer(ip, port)`。
     - WebSocket 透传：`proxy_http_version 1.1; proxy_set_header Upgrade $http_upgrade; proxy_set_header Connection "upgrade"; proxy_pass http://gateway_backend;`，`proxy_read_timeout` 拉长。
   - 一个调试 `location /router_status`：输出当前发现到的 Pod 列表与环大小，便于验证。
2. **`chash.lua`**（vendored）：纯 Lua 的 `resty.chash`（ketama 实现），随 ConfigMap 挂载到 Lua 包路径，`require "resty.chash"` 可加载。

> **副本间一致性**：每个 nginx 副本都从**排序后的相同 Pod IP 集合**、用相同算法构建环，因此同一 deviceID 在任意副本上都会命中同一个 Gateway Pod。

### 3.2 `templates/nginx-deployment.yaml`

- `image: openresty/openresty`（stock）。
- `replicas: {{ .Values.nginxRouter.replicaCount }}`。
- 挂载 ConfigMap 到 `/usr/local/openresty/nginx/conf/`（nginx.conf）和 Lua 包路径（chash.lua）。
- liveness/readiness 探针打 `/router_status` 或一个轻量 health location。
- `serviceAccountName: default`（不需要任何 RBAC）。

### 3.3 `templates/nginx-service.yaml`

- `ClusterIP`，端口 8080，selector 指向 nginx-router Pod。这是 APISIX 路由 upstream 指向的目标。

### 3.4 `templates/gateway-headless-service.yaml`

- `clusterIP: None`，selector = `gateway.selectorLabels`（与 DaemonSet/Deployment Pod 相同），端口 8081。
- 可选 `publishNotReadyAddresses: false`（仅就绪 Pod 进环，避免把未就绪 Pod 纳入哈希）。
- DNS 名 `<headless>.<namespace>.svc.cluster.local` 的 A 记录会返回所有 Gateway Pod IP，供 nginx-router 轮询发现。

## 4. APISIX 配置简化

### 4.1 删除/不再需要

- §2.1 `discovery.kubernetes` 配置。
- §2.2 服务发现 RBAC（Role/RoleBinding）——**仅当 APISIX 没有其它路由依赖 K8s 发现时**才删除；否则保留，但本路由不再使用。

### 4.2 新的薄透传路由

Admin API 形态（CRD 形态同理简化）：

```bash
curl -i "$APISIX_ADMIN/apisix/admin/routes/gateway-tunnel" \
  -H "X-API-KEY: $ADMIN_KEY" -X PUT \
  -d '{
    "uri": "/device/*",
    "enable_websocket": true,
    "upstream": {
      "type": "roundrobin",
      "nodes": { "costrict-web-gateway-nginx:8080": 1 },
      "scheme": "http",
      "pass_host": "pass"
    }
  }'
```

无 `serverless-pre-function`、无 `chash`、无 `discovery_type`。upstream 指向 nginx-router 的 ClusterIP Service（也可用 K8s 普通 Service 名，无需发现 endpoint）。

## 5. 文档改造（`gateway-daemonset-apisix-chash.md`）

- **标题/简介/架构图**：加入 nginx-router 中间层，说明一致性哈希现由 nginx 承担。
- **§2.1 / §2.2**：缩减为「若 APISIX 仅服务本路由，则不再需要 K8s 发现与 RBAC」的说明。
- **§2.3**：
  - 改为「APISIX 薄透传路由」（上面的简化 curl / CRD）。
  - 新增「nginx-router 中间层」小节：helm 开启方式、ConfigMap（nginx.conf + chash.lua）讲解、headless Service 说明、副本间一致性说明。
- **§3 验证**：
  - 新增「查看 nginx-router 发现到的 Pod」：访问 `/router_status` 或看 nginx 日志。
  - WebSocket 粘滞测试不变（多次重连同一 deviceID 落在同一 Gateway Pod）。
  - 扩容验证：新增 Node → DaemonSet 起新 Pod → headless DNS 更新 → nginx-router 在 `refreshIntervalMs` 内刷新环。
- **§4 常见问题**：新增 nginx-router 相关排查（DNS 解析失败、环为空、resolver 配置错误、副本间路由不一致）。

## 6. 验证策略

1. `helm template`/`helm lint` 通过，`nginxRouter.enabled=false` 时不渲染任何新资源（兼容现有部署）。
2. `nginxRouter.enabled=true` 渲染出 ConfigMap / Deployment / Service / headless Service 四个资源。
3. 集群内：APISIX → nginx-router → Gateway 全链路 WebSocket 建连成功。
4. 同一 deviceID 多次重连命中同一 Gateway Pod（粘滞）。
5. 删除/新增一个 Gateway Pod 后，nginx-router 在刷新间隔内更新环；未受影响的 deviceID 仍粘滞在原 Pod。

## 7. 风险与权衡

- **vendored `resty.chash`**：引入约 200 行第三方 Lua 到 ConfigMap；好处是零镜像构建。需注明来源与版本。
- **DNS 发现的实时性**：取决于 `refreshIntervalMs` 与 kube-dns TTL；扩缩容时有秒级窗口，期间个别新设备可能短暂路由不稳定，重连即恢复。
- **nginx-router 自身可用性**：成为链路上一跳，需 ≥2 副本 + 探针；它是无状态的，重启不影响粘滞（环可重建）。
- **headless Service 就绪语义**：用就绪地址进环可避免打到未就绪 Pod，但 Pod 刚就绪瞬间各副本环可能短暂不一致。

## 8. 相关文件

- `deploy/charts/gateway/values.yaml` — 新增 `nginxRouter.*`
- `deploy/charts/gateway/templates/nginx-configmap.yaml` — 新增
- `deploy/charts/gateway/templates/nginx-deployment.yaml` — 新增
- `deploy/charts/gateway/templates/nginx-service.yaml` — 新增
- `deploy/charts/gateway/templates/gateway-headless-service.yaml` — 新增
- `deploy/charts/gateway/templates/_helpers.tpl` — 新增 nginx-router 的 name/labels helper
- `docs/deployment/gateway-daemonset-apisix-chash.md` — 改造 §2.3 等
