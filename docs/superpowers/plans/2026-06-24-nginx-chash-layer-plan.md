# nginx chash 中间层 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 APISIX 上的 deviceID 提取、一致性哈希、Gateway Pod 发现逻辑迁移到一个 OpenResty nginx-router 中间层；通过扩展现有 `deploy/charts/gateway` chart 并改写 `docs/deployment/gateway-daemonset-apisix-chash.md` 实现。

**Architecture:** cs-cloud → APISIX（只做 TLS + WebSocket 透传）→ nginx-router（OpenResty，从 `/device/{deviceID}/...` 提取 deviceID，使用纯 Lua 的一致性哈希，按固定间隔解析 headless Service DNS 发现 Gateway Pod）→ Gateway DaemonSet Pod。Server→设备反向流量仍走 Pod IP，不经过 nginx/APISIX。

**Tech Stack:** Helm 3, Kubernetes, OpenResty 1.25.x, LuaJIT（内置 `ngx.crc32_long`、`lua-resty-dns`、`ngx.balancer`），纯 Lua ketama 实现。

---

## 0. 先验信息

- 当前分支：`feat/nginx-chash-router`（已包含设计 spec 提交）。
- 设计 spec：`docs/superpowers/specs/2026-06-24-nginx-chash-layer-design.md`。
- 目标 chart：`deploy/charts/gateway/`。
- 测试渲染命令：`helm template costrict-web-gateway ./deploy/charts/gateway --namespace costrict [-f /tmp/values.yaml]` 与 `helm lint ./deploy/charts/gateway`。
- 提交消息要求：每个任务完成后一次提交；末尾带 `Co-Authored-By: Claude <noreply@anthropic.com>`。
- **重要修正**：不要使用 `resty.chash`；upstream 的 `lua-resty-balancer` 中的 `chash` 依赖编译好的 `.so`，无法 vendoring 成纯 Lua 文件。本计划改用 100% 纯 Lua 的 ketama 一致性哈希模块，直接放在 ConfigMap 中。

---

## 文件结构总览

| 文件 | 动作 | 职责 |
|------|------|------|
| `deploy/charts/gateway/values.yaml` | 修改 | 新增 `nginxRouter.*` 配置块 |
| `deploy/charts/gateway/templates/_helpers.tpl` | 修改 | 新增 nginx-router 专用 name / fullname / labels helper |
| `deploy/charts/gateway/templates/gateway-headless-service.yaml` | 新建 | 暴露 Gateway Pod IP 的 headless Service |
| `deploy/charts/gateway/templates/nginx-configmap.yaml` | 新建 | nginx.conf + `router.lua`（纯 Lua DNS 发现 + ketama 哈希） |
| `deploy/charts/gateway/templates/nginx-deployment.yaml` | 新建 | nginx-router Deployment |
| `deploy/charts/gateway/templates/nginx-service.yaml` | 新建 | nginx-router ClusterIP Service |
| `docs/deployment/gateway-daemonset-apisix-chash.md` | 修改 | 改写 §2.3，简化 §2.1/§2.2，更新验证/FAQ/架构图 |

---

## Task 1: values.yaml 新增 nginxRouter 配置块

**Files:**
- Modify: `deploy/charts/gateway/values.yaml`

- [ ] **Step 1: 在 `config:` 区块下方追加 `nginxRouter:` 默认配置**

在 `deploy/charts/gateway/values.yaml` 的末尾追加：

```yaml
# nginx-router 中间层配置。
# 启用后，由 OpenResty 负责从 /device/{deviceID}/... 提取 deviceID、
# 一致性哈希(chash)路由以及通过 headless Service DNS 发现 Gateway Pod。
# APISIX 退化为薄 WebSocket 透传层，不再需要 K8s 服务发现及对应 RBAC。
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
  # Gateway Pod 发现相关
  discovery:
    # 留空时使用 <fullname>-headless
    headlessServiceName: ""
    gatewayPort: 8081
    refreshIntervalMs: 5000
  # kube-dns 地址；多数集群默认值即可工作
  resolver: "kube-dns.kube-system.svc.cluster.local"
  resources:
    limits:
      cpu: 500m
      memory: 256Mi
    requests:
      cpu: 50m
      memory: 64Mi
  podAnnotations: {}
```

- [ ] **Step 2: 验证 values.yaml 语法**

Run:
```bash
helm lint ./deploy/charts/gateway
```

Expected: `1 chart(s) linted, 0 chart(s) failed`。

- [ ] **Step 3: 验证默认关闭时不渲染 nginx-router 资源**

Run:
```bash
helm template costrict-web-gateway ./deploy/charts/gateway --namespace costrict | grep -E "nginx-router|headless" || true
```

Expected: 无 `costrict-web-gateway-nginx-router` 或 `costrict-web-gateway-headless` 输出。

- [ ] **Step 4: 提交**

```bash
git add deploy/charts/gateway/values.yaml
git commit -m "feat(gateway): add nginxRouter values block

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 2: 新增 nginx-router helper

**Files:**
- Modify: `deploy/charts/gateway/templates/_helpers.tpl`

- [ ] **Step 1: 在 `_helpers.tpl` 末尾追加 nginx-router 专用 helper**

```yaml
{{/*
nginx-router 名称
*/}}
{{- define "gateway.nginxRouter.name" -}}
{{- printf "%s-nginx-router" (include "gateway.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
nginx-router fullname
*/}}
{{- define "gateway.nginxRouter.fullname" -}}
{{- printf "%s-nginx-router" (include "gateway.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
nginx-router selector labels
*/}}
{{- define "gateway.nginxRouter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gateway.nginxRouter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
nginx-router common labels
*/}}
{{- define "gateway.nginxRouter.labels" -}}
helm.sh/chart: {{ include "gateway.chart" . }}
{{ include "gateway.nginxRouter.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
```

- [ ] **Step 2: 验证模板渲染**

Run:
```bash
helm template costrict-web-gateway ./deploy/charts/gateway --namespace costrict --set nginxRouter.enabled=true 2>&1 | grep -A1 "name: costrict-web-gateway-nginx-router" | head -5
```

Expected: 出现 `name: costrict-web-gateway-nginx-router`。

- [ ] **Step 3: 提交**

```bash
git add deploy/charts/gateway/templates/_helpers.tpl
git commit -m "feat(gateway): add nginx-router helm helpers

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 3: Gateway headless Service

**Files:**
- Create: `deploy/charts/gateway/templates/gateway-headless-service.yaml`

- [ ] **Step 1: 新建 headless Service 模板**

```yaml
{{- if .Values.nginxRouter.enabled }}
apiVersion: v1
kind: Service
metadata:
  name: {{ include "gateway.fullname" . }}-headless
  namespace: {{ include "gateway.namespace" . }}
  labels:
    {{- include "gateway.labels" . | nindent 4 }}
spec:
  type: ClusterIP
  clusterIP: None
  publishNotReadyAddresses: false
  ports:
    - port: {{ .Values.nginxRouter.discovery.gatewayPort }}
      targetPort: http
      protocol: TCP
      name: http
  selector:
    {{- include "gateway.selectorLabels" . | nindent 4 }}
{{- end }}
```

- [ ] **Step 2: 验证渲染**

Run:
```bash
helm template costrict-web-gateway ./deploy/charts/gateway --namespace costrict --set nginxRouter.enabled=true -s templates/gateway-headless-service.yaml
```

Expected: 输出包含 `name: costrict-web-gateway-headless`、`clusterIP: None`、selector 与 gateway 一致。

- [ ] **Step 3: 关闭时不渲染**

Run:
```bash
helm template costrict-web-gateway ./deploy/charts/gateway --namespace costrict -s templates/gateway-headless-service.yaml 2>&1 | grep "Error" || true
```

Expected: 报错 "could not find template ..."（因为 `enabled=false` 不渲染）。

- [ ] **Step 4: 提交**

```bash
git add deploy/charts/gateway/templates/gateway-headless-service.yaml
git commit -m "feat(gateway): add headless Service for nginx-router discovery

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 4: nginx-router ConfigMap（nginx.conf + router.lua）

**Files:**
- Create: `deploy/charts/gateway/templates/nginx-configmap.yaml`

- [ ] **Step 1: 新建 ConfigMap 模板**

```yaml
{{- if .Values.nginxRouter.enabled }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "gateway.nginxRouter.fullname" . }}-config
  namespace: {{ include "gateway.namespace" . }}
  labels:
    {{- include "gateway.nginxRouter.labels" . | nindent 4 }}
data:
  nginx.conf: |
    worker_processes auto;
    error_log /var/log/nginx/error.log notice;
    pid /run/nginx.pid;

    events {
        worker_connections 1024;
    }

    http {
        include /usr/local/openresty/nginx/conf/mime.types;
        default_type application/octet-stream;

        # DNS resolver for Kubernetes headless Service lookups
        resolver {{ .Values.nginxRouter.resolver }} valid=5s;
        resolver_timeout 2s;

        # Shared across workers: JSON-encoded discovered pod list
        lua_shared_dict router_state 1m;

        lua_package_path "/usr/local/openresty/site/lualib/?.lua;;";

        init_worker_by_lua_block {
            local router = require "router"
            local state = ngx.shared.router_state

            local headless_fqdn = "{{ include \"gateway.fullname\" . }}-headless.{{ include \"gateway.namespace\" . }}.svc.cluster.local"
            local gateway_port = {{ .Values.nginxRouter.discovery.gatewayPort }}
            local refresh_ms = {{ .Values.nginxRouter.discovery.refreshIntervalMs }}
            local resolver_addr = "{{ .Values.nginxRouter.resolver }}"

            local function discover_and_update(premature)
                if premature then return end
                local ips, err = router.resolve_ips(headless_fqdn, gateway_port, resolver_addr)
                if not ips then
                    ngx.log(ngx.ERR, "discovery failed: ", err)
                    return
                end
                if #ips == 0 then
                    ngx.log(ngx.WARN, "no gateway pods discovered from ", headless_fqdn)
                    return
                end

                local ok, encoded = pcall(router.cjson.encode, ips)
                if not ok then
                    ngx.log(ngx.ERR, "failed to encode ips: ", encoded)
                    return
                end

                state:set("ips", encoded)
                -- each worker rebuilds its own ring on demand from the sorted list
            end

            discover_and_update()
            local ok, err = ngx.timer.every(refresh_ms / 1000, discover_and_update)
            if not ok then
                ngx.log(ngx.ERR, "failed to create discovery timer: ", err)
            end
        }

        upstream gateway_backend {
            server 0.0.0.1;  # placeholder, overwritten by balancer
            balancer_by_lua_block {
                local router = require "router"
                local state = ngx.shared.router_state
                local balancer = require "ngx.balancer"

                local device_id = ngx.var.device_id
                if not device_id or device_id == "" then
                    ngx.log(ngx.ERR, "device_id missing in balancer")
                    return ngx.exit(502)
                end

                local encoded = state:get("ips")
                if not encoded then
                    ngx.log(ngx.ERR, "gateway pod list not ready")
                    return ngx.exit(502)
                end

                local ips, err = router.cjson.decode(encoded)
                if not ips then
                    ngx.log(ngx.ERR, "failed to decode ips: ", err)
                    return ngx.exit(502)
                end

                local peer, err = router.chash_pick(ips, device_id)
                if not peer then
                    ngx.log(ngx.ERR, "chash pick failed: ", err)
                    return ngx.exit(502)
                end

                local host, port = peer:match("^(.+):(%d+)$")
                local ok, err = balancer.set_current_peer(host, tonumber(port))
                if not ok then
                    ngx.log(ngx.ERR, "set_current_peer failed: ", err)
                    return ngx.exit(502)
                end
            }
        }

        server {
            listen 8080;

            location ~ ^/device/(?P<device_id>[^/]+)/ {
                set $device_id $device_id;

                proxy_http_version 1.1;
                proxy_set_header Upgrade $http_upgrade;
                proxy_set_header Connection "upgrade";
                proxy_set_header Host $host;
                proxy_read_timeout 86400s;
                proxy_send_timeout 86400s;
                proxy_pass http://gateway_backend;
            }

            location /router_status {
                default_type application/json;
                content_by_lua_block {
                    local router = require "router"
                    local state = ngx.shared.router_state
                    local encoded = state:get("ips") or "[]"
                    local ips, _ = router.cjson.decode(encoded)
                    if not ips then ips = {} end
                    ngx.say(router.cjson.encode({
                        source = "nginx-router",
                        discovered_ips = ips,
                    }))
                }
            }

            location /health {
                access_log off;
                add_header Content-Type text/plain;
                return 200 "ok\n";
            }
        }
    }
  router.lua: |
    -- Pure-Lua DNS discovery + ketama consistent-hash helper for nginx-router.
    -- Runs inside OpenResty; uses only built-in modules.

    local resolver = require "resty.dns.resolver"
    local cjson = require "cjson.safe"
    local ngx_crc32_long = ngx.crc32_long
    local table_sort = table.sort
    local ipairs = ipairs
    local pairs = pairs
    local floor = math.floor

    local _M = {
        cjson = cjson,
    }

    -- Resolve all A records for a headless Service and return sorted "ip:port" list.
    function _M.resolve_ips(fqdn, port, nameserver)
        local r, err = resolver:new{
            nameservers = { nameserver },
            retrans = 3,
            timeout = 2000,
        }
        if not r then
            return nil, "resolver init failed: " .. (err or "unknown")
        end

        local answers, err = r:query(fqdn, { qtype = r.TYPE_A })
        if not answers then
            return nil, "dns query failed: " .. (err or "unknown")
        end
        if answers.errcode then
            return nil, "dns query error code: " .. tostring(answers.errcode)
        end

        local ips = {}
        for _, ans in ipairs(answers) do
            if ans.address then
                ips[#ips + 1] = ans.address .. ":" .. tostring(port)
            end
        end
        table_sort(ips)
        return ips
    end

    -- Build ketama ring points deterministically from sorted node list.
    -- Returns a flat array of {hash=uint32, id=string}, already sorted by hash.
    local function build_ring(ips, replicas)
        replicas = replicas or 160
        local points = {}
        for _, ip in ipairs(ips) do
            for i = 1, replicas do
                local key = ip .. "-" .. i
                points[#points + 1] = {
                    hash = ngx_crc32_long(key),
                    id = ip,
                }
            end
        end
        table_sort(points, function(a, b) return a.hash < b.hash end)
        return points
    end

    -- Pick a backend for the given key using ketama consistent hashing.
    function _M.chash_pick(ips, key)
        if not ips or #ips == 0 then
            return nil, "empty node list"
        end

        local ring = build_ring(ips)
        local hash = ngx_crc32_long(key)

        -- binary search for first point >= hash
        local lo, hi = 1, #ring
        while lo < hi do
            local mid = floor((lo + hi) / 2)
            if ring[mid].hash < hash then
                lo = mid + 1
            else
                hi = mid
            end
        end

        if lo == #ring and ring[lo].hash < hash then
            lo = 1
        end

        return ring[lo].id
    end

    return _M
{{- end }}
```

- [ ] **Step 2: 验证模板能渲染**

Run:
```bash
helm template costrict-web-gateway ./deploy/charts/gateway --namespace costrict --set nginxRouter.enabled=true -s templates/nginx-configmap.yaml > /tmp/nginx-cm.yaml
echo "rendered bytes: $(wc -c < /tmp/nginx-cm.yaml)"
```

Expected: 渲染成功，输出文件大小 > 5KB，包含 `nginx.conf:` 与 `router.lua:` 两个键。

- [ ] **Step 3: 提交**

```bash
git add deploy/charts/gateway/templates/nginx-configmap.yaml
git commit -m "feat(gateway): add nginx-router ConfigMap with pure-Lua chash + discovery

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 5: nginx-router Deployment

**Files:**
- Create: `deploy/charts/gateway/templates/nginx-deployment.yaml`

- [ ] **Step 1: 新建 Deployment 模板**

```yaml
{{- if .Values.nginxRouter.enabled }}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "gateway.nginxRouter.fullname" . }}
  namespace: {{ include "gateway.namespace" . }}
  labels:
    {{- include "gateway.nginxRouter.labels" . | nindent 4 }}
spec:
  replicas: {{ .Values.nginxRouter.replicaCount }}
  selector:
    matchLabels:
      {{- include "gateway.nginxRouter.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      annotations:
        checksum/config: {{ include (print $.Template.BasePath "/nginx-configmap.yaml") . | sha256sum }}
        {{- with .Values.nginxRouter.podAnnotations }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      labels:
        {{- include "gateway.nginxRouter.selectorLabels" . | nindent 8 }}
    spec:
      {{- with .Values.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        fsGroup: 1000
      containers:
        - name: nginx-router
          image: "{{ .Values.nginxRouter.image.repository }}:{{ .Values.nginxRouter.image.tag }}"
          imagePullPolicy: {{ .Values.nginxRouter.image.pullPolicy }}
          ports:
            - name: http
              containerPort: 8080
              protocol: TCP
          volumeMounts:
            - name: config
              mountPath: /usr/local/openresty/nginx/conf/nginx.conf
              subPath: nginx.conf
              readOnly: true
            - name: config
              mountPath: /usr/local/openresty/site/lualib/router.lua
              subPath: router.lua
              readOnly: true
          livenessProbe:
            httpGet:
              path: /health
              port: http
            initialDelaySeconds: 5
            periodSeconds: 10
            timeoutSeconds: 3
            failureThreshold: 3
          readinessProbe:
            httpGet:
              path: /health
              port: http
            initialDelaySeconds: 2
            periodSeconds: 5
            timeoutSeconds: 3
            failureThreshold: 3
          resources:
            {{- toYaml .Values.nginxRouter.resources | nindent 12 }}
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
      volumes:
        - name: config
          configMap:
            name: {{ include "gateway.nginxRouter.fullname" . }}-config
{{- end }}
```

- [ ] **Step 2: 验证渲染**

Run:
```bash
helm template costrict-web-gateway ./deploy/charts/gateway --namespace costrict --set nginxRouter.enabled=true -s templates/nginx-deployment.yaml | grep -E "name:|replicas:|image:|checksum/config"
```

Expected: 包含 `name: costrict-web-gateway-nginx-router`、`replicas: 2`、镜像、`checksum/config`。

- [ ] **Step 3: 提交**

```bash
git add deploy/charts/gateway/templates/nginx-deployment.yaml
git commit -m "feat(gateway): add nginx-router Deployment

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 6: nginx-router Service

**Files:**
- Create: `deploy/charts/gateway/templates/nginx-service.yaml`

- [ ] **Step 1: 新建 Service 模板**

```yaml
{{- if .Values.nginxRouter.enabled }}
apiVersion: v1
kind: Service
metadata:
  name: {{ include "gateway.nginxRouter.fullname" . }}
  namespace: {{ include "gateway.namespace" . }}
  labels:
    {{- include "gateway.nginxRouter.labels" . | nindent 4 }}
spec:
  type: {{ .Values.nginxRouter.service.type }}
  ports:
    - port: {{ .Values.nginxRouter.service.port }}
      targetPort: http
      protocol: TCP
      name: http
  selector:
    {{- include "gateway.nginxRouter.selectorLabels" . | nindent 4 }}
{{- end }}
```

- [ ] **Step 2: 验证渲染**

Run:
```bash
helm template costrict-web-gateway ./deploy/charts/gateway --namespace costrict --set nginxRouter.enabled=true -s templates/nginx-service.yaml
```

Expected: 输出 `name: costrict-web-gateway-nginx-router`，type `ClusterIP`，selector 指向 nginx-router Pod。

- [ ] **Step 3: 提交**

```bash
git add deploy/charts/gateway/templates/nginx-service.yaml
git commit -m "feat(gateway): add nginx-router Service

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 7: 文档改造 `gateway-daemonset-apisix-chash.md`

**Files:**
- Modify: `docs/deployment/gateway-daemonset-apisix-chash.md`

- [ ] **Step 1: 更新标题、引言、架构图**

将文档第 1-3 段替换为：

```markdown
# Gateway DaemonSet + nginx-router chash 部署方案

本方案把 `costrict-web-gateway` 以 **DaemonSet** 方式部署（每个 Node 一个 Gateway Pod），并在 APISIX 与 Gateway 之间增加一个 **OpenResty nginx-router 中间层**，由 nginx-router 负责：

- 从 `/device/{deviceID}/...` 提取 `deviceID`。
- 使用一致性哈希（chash）按 `deviceID` 选择 Gateway Pod。
- 通过解析 Gateway 的 **headless Service DNS** 自动发现所有 Gateway Pod IP（无需 K8s API / RBAC）。

相比原 APISIX chash 方案，优势：

- 集群扩容/缩容时自动在每个 Node 上增删 Gateway Pod，**无需修改 APISIX 配置**。
- APISIX 退化为只做 TLS 终止 + WebSocket 透传的薄入口，不再需要 K8s 服务发现及对应 RBAC。
- 同一台设备（`deviceID`）始终被路由到同一个 Gateway Pod。
```

将第 13-34 行的架构图替换为：

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

- [ ] **Step 2: 简化前置条件**

将「前置条件」小节的第 2 点改为：

```markdown
- APISIX 不再需要 watch Gateway 所在 namespace 的 Endpoints/EndpointSlices；Pod 发现由 nginx-router 通过 DNS 完成。
```

- [ ] **Step 3: 改造 §2 APISIX 配置章节**

将「## 2. APISIX 配置」及其子节整体替换为：

```markdown
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
```

- [ ] **Step 4: 新增 §2.2 nginx-router 中间层章节**

在 §2.1 后插入新的小节：

```markdown
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
```

- [ ] **Step 5: 更新 §3 验证章节**

将 §3 整体替换为：

```markdown
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
```

- [ ] **Step 6: 更新 §4 常见问题**

将 §4 替换为：

```markdown
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
```

- [ ] **Step 7: 更新 §5 回滚到 Deployment 模式**

将 §5 改为：

```markdown
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
```

- [ ] **Step 8: 验证文档无占位符**

Run:
```bash
grep -n "TODO\|TBD\|FIXME" docs/deployment/gateway-daemonset-apisix-chash.md || echo "no placeholders"
```

Expected: `no placeholders`。

- [ ] **Step 9: 提交**

```bash
git add docs/deployment/gateway-daemonset-apisix-chash.md
git commit -m "docs(deployment): rewrite gateway-daemonset-apisix-chash for nginx-router

Move deviceID extraction, chash routing, and Gateway pod discovery
off APISIX into the OpenResty nginx-router layer.

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 8: 最终 lint / 渲染 / 兼容性验证

**Files:**
- N/A（整体验证）

- [ ] **Step 1: helm lint 通过**

Run:
```bash
helm lint ./deploy/charts/gateway
```

Expected: `1 chart(s) linted, 0 chart(s) failed`。

- [ ] **Step 2: 默认关闭时无 nginx-router 资源**

Run:
```bash
helm template costrict-web-gateway ./deploy/charts/gateway --namespace costrict > /tmp/default.yaml
grep -cE "name: .*nginx-router|clusterIP: None" /tmp/default.yaml || echo "count=0"
```

Expected: `count=0`。

- [ ] **Step 3: 启用后渲染出所有预期资源**

Run:
```bash
helm template costrict-web-gateway ./deploy/charts/gateway \
  --namespace costrict \
  --set daemonSet.enabled=true \
  --set nginxRouter.enabled=true > /tmp/enabled.yaml
for kind in ConfigMap Deployment Service; do
  echo "--- $kind ---"
  grep -E "^kind: $kind" /tmp/enabled.yaml || true
done
echo "--- headless service ---"
grep -A3 "name: costrict-web-gateway-headless" /tmp/enabled.yaml | head -4
```

Expected: 出现 `kind: ConfigMap`（nginx-router-config）、`kind: Deployment`（nginx-router）、两个 `kind: Service`（nginx-router + headless）。

- [ ] **Step 4: 提交汇总（若前面已分别提交，此步可省略）**

本任务为整体验证，不强制产生新提交。

---

## Self-Review Checklist

- [ ] **Spec coverage**: 每个 spec 章节（架构、headless Service、ConfigMap 内容、Deployment/Service、文档 §2.3 改写、验证 FAQ）都有对应 Task。
- [ ] **Placeholder scan**: 计划全文无 `TODO`、`TBD`、`implement later`、无模糊描述。
- [ ] **Type 一致性**: ConfigMap 中 Lua 用到的 `device_id` 变量、shared dict 名 `router_state`、resolver、headless FQDN 在 nginx.conf / balancer / status location 中保持一致。
- [ ] **Helm 兼容**: `nginxRouter.enabled=false` 时不渲染任何新资源；`enabled=true` 时渲染四个资源。
- [ ] **文档与 chart 同步**: `values.yaml` 中的默认值（resolver、image tag、refreshIntervalMs）在文档中有说明。
- [ ] **chash 实现修正**: 已用纯 Lua ketama 替代 `resty.chash`，无 `.so` 依赖。

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-06-24-nginx-chash-layer-plan.md`. Two execution options:

1. **Subagent-Driven (recommended)** - I dispatch a fresh subagent per task, review between tasks, fast iteration
2. **Inline Execution** - Execute tasks in this session using executing-plans, batch execution with checkpoints

Which approach?
