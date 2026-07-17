# nginx-router DNS 自动探测实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 nginx-router 启动时通过解析 Pod 自带的 `/etc/resolv.conf` 自动获得集群 DNS nameserver 与 cluster domain，从而移除 `nginxRouter.resolver` 与硬编码 `svc.cluster.local` 的手动配置。

**Architecture:** 在 `router.lua` 中新增 `parse_resolv_conf` 与 `detect_dns` 纯函数，由 `nginx-configmap.yaml` 在 worker 0 初始化阶段调用；ConfigMap 模板删除 `resolver` 指令，改为 base Helm 渲染 + 运行时 domain 拼接；values.yaml 默认值改空并新增 `clusterDomain` 选项；同步更新部署文档与 FAQ。

**Tech Stack:** Helm, OpenResty/Lua 5.1, lua-resty-dns, `resty` CLI, kind（可选）

---

## 文件结构

| 文件 | 责任 |
|---|---|
| `deploy/charts/gateway/templates/nginx-configmap.yaml` | ConfigMap 模板：删除 `resolver` 指令，渲染 headless base，注入 Lua 探测逻辑 |
| `deploy/charts/gateway/values.yaml` | `nginxRouter.resolver` 默认改空，新增 `nginxRouter.discovery.clusterDomain` |
| `deploy/charts/gateway/templates/gateway-headless-service.yaml` | 让 Service 名受 `discovery.headlessServiceName` 驱动（附带修复） |
| `deploy/charts/gateway/tests/router_dns_test.lua` | `resty` CLI 可运行的 `parse_resolv_conf`/`detect_dns` 单测 |
| `docs/deployment/gateway-daemonset-apisix-chash.md` | 更新 resolver 说明、FAQ，新增 `/router_status` 字段说明 |

---

## Task 1: 让 `router.lua` 能解析 `/etc/resolv.conf`

**Files:**
- Create: `deploy/charts/gateway/tests/router_dns_test.lua`
- Modify: `deploy/charts/gateway/templates/nginx-configmap.yaml` (router.lua 部分)

- [ ] **Step 1.1: 写解析函数表格测试**

在 `deploy/charts/gateway/tests/router_dns_test.lua` 中复制当前 `router.lua` 的解析相关函数，并写 table-driven 测试：

```lua
-- 测试辅助：从 router.lua 复制出来的解析函数（后续步骤替换为引用真实 router.lua）
local function parse_resolv_conf(text)
    -- 待实现
end

local cases = {
    {
        name = "kube-dns standard",
        input = [[# kubelet config
nameserver 10.96.0.10
search default.svc.cluster.local svc.cluster.local cluster.local
options ndots:5
]],
        want_nameservers = {"10.96.0.10"},
        want_domain = "cluster.local",
    },
    {
        name = "NodeLocal DNS",
        input = [[nameserver 169.254.20.10
search default.svc.cluster.local svc.cluster.local cluster.local
]],
        want_nameservers = {"169.254.20.10"},
        want_domain = "cluster.local",
    },
    {
        name = "multiple nameservers",
        input = [[nameserver 10.96.0.10
nameserver 10.96.0.11
search default.svc.cluster.local svc.cluster.local cluster.local
]],
        want_nameservers = {"10.96.0.10", "10.96.0.11"},
        want_domain = "cluster.local",
    },
    {
        name = "custom cluster domain",
        input = [[nameserver 10.96.0.10
search default.svc.k8s.internal svc.k8s.internal k8s.internal
]],
        want_nameservers = {"10.96.0.10"},
        want_domain = "k8s.internal",
    },
    {
        name = "no matching search domain",
        input = [[nameserver 10.96.0.10
search default.svc.cluster.local.example cluster.local.example
]],
        want_nameservers = {"10.96.0.10"},
        want_domain = nil,
    },
    {
        name = "empty file",
        input = "",
        want_nameservers = {},
        want_domain = nil,
    },
}

local passed = 0
for _, c in ipairs(cases) do
    local ns, domain = parse_resolv_conf(c.input)
    local ns_ok = #ns == #c.want_nameservers
    if ns_ok then
        for i, v in ipairs(ns) do
            if v ~= c.want_nameservers[i] then ns_ok = false break end
        end
    end
    local domain_ok = domain == c.want_domain
    if ns_ok and domain_ok then
        passed = passed + 1
        print("PASS: " .. c.name)
    else
        print("FAIL: " .. c.name)
        print("  got ns=" .. table.concat(ns, ",") .. " domain=" .. tostring(domain))
        print("  want ns=" .. table.concat(c.want_nameservers, ",") .. " domain=" .. tostring(c.want_domain))
    end
end
print(passed .. "/" .. #cases .. " passed")
os.exit(passed == #cases and 0 or 1)
```

- [ ] **Step 1.2: 运行测试确认失败**

Run:

```bash
cd /Users/linkai/code/costrict-web/deploy/charts/gateway
resty tests/router_dns_test.lua
```

Expected: FAIL（`parse_resolv_conf` 未实现，函数体 stub 导致断言失败）。

- [ ] **Step 1.3: 实现 `parse_resolv_conf` 并挂载到 `router.lua`**

在 `deploy/charts/gateway/templates/nginx-configmap.yaml` 的 `router.lua` 中新增：

```lua
-- Parse /etc/resolv.conf into nameserver list and cluster domain.
-- Returns: nameservers (array of strings), cluster_domain (string or nil)
function _M.parse_resolv_conf(text)
    if not text or text == "" then
        return {}, nil
    end

    local nameservers = {}
    local cluster_domain = nil

    for line in text:gmatch("[^\r\n]+") do
        -- strip comments
        local comment_pos = line:find("[%#;]")
        if comment_pos then
            line = line:sub(1, comment_pos - 1)
        end
        line = line:match("^%s*(.-)%s*$")
        if line == "" then
            -- skip
        else
            local ns = line:match("^nameserver%s+(%S+)$")
            if ns then
                nameservers[#nameservers + 1] = ns
            else
                local search_list = line:match("^search%s+(.+)$")
                if search_list and not cluster_domain then
                    for part in search_list:gmatch("%S+") do
                        local domain = part:match("^[^%.]+%.svc%.(.+)$")
                        if domain then
                            cluster_domain = domain
                            break
                        end
                    end
                end
            end
        end
    end

    return nameservers, cluster_domain
end
```

- [ ] **Step 1.4: 更新测试文件引用真实函数**

把 `deploy/charts/gateway/tests/router_dns_test.lua` 改成加载真实 `router.lua` 并调用 `_M.parse_resolv_conf`。

最简做法：把 `router.lua` 的内容作为模块加载。OpenResty 的 `resty` CLI 支持标准 `require` 路径；把测试目录临时加到 `LUA_PATH`：

```lua
-- 测试脚本路径：deploy/charts/gateway/tests/router_dns_test.lua
local router = require "router"

local cases = { ... }
-- 调用 router.parse_resolv_conf(...)
```

为了让 `resty` 能找到 `router.lua`，运行命令：

```bash
cd /Users/linkai/code/costrict-web/deploy/charts/gateway
LUA_PATH="/tmp/router-lua/?.lua;;" resty tests/router_dns_test.lua
```

其中 `/tmp/router-lua/router.lua` 是当前模板中提取出来的纯 Lua 文件（下一步骤处理）。**这里先保留 TODO：该测试的最终形态见 Task 6（文档化），实现期间可先用 sed 提取 `router.lua` 内容到临时目录运行。**

- [ ] **Step 1.5: 运行测试确认通过**

Expected: 6/6 passed。

- [ ] **Step 1.6: Commit**

```bash
git add deploy/charts/gateway/templates/nginx-configmap.yaml deploy/charts/gateway/tests/router_dns_test.lua
git commit -m "feat(gateway): parse /etc/resolv.conf in router.lua

- Add parse_resolv_conf() to extract nameservers and cluster domain.
- Add table-driven unit test.

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 2: 新增 `detect_dns` 与运行时探测逻辑

**Files:**
- Modify: `deploy/charts/gateway/templates/nginx-configmap.yaml` (router.lua 与 nginx.conf 两部分)

- [ ] **Step 2.1: 在 `router.lua` 新增 `detect_dns`**

```lua
-- Detect effective DNS configuration for this Pod.
-- Parameters are the raw Helm-rendered overrides (empty string means "not set").
-- Returns: nameservers (array), cluster_domain (string), source (string)
function _M.detect_dns(resolver_override, domain_override)
    local source = "manual"
    local nameservers = {}
    local cluster_domain = nil

    if resolver_override and resolver_override ~= "" then
        -- values 显式指定；按逗号/空白分隔支持多个
        for part in resolver_override:gmatch("%S+") do
            nameservers[#nameservers + 1] = part
        end
    end

    if domain_override and domain_override ~= "" then
        cluster_domain = domain_override
    end

    if #nameservers == 0 or not cluster_domain then
        local text, err = nil, nil
        -- io.open 在 init_worker 阶段可用
        local f = io.open("/etc/resolv.conf", "r")
        if f then
            text = f:read("*a")
            f:close()
        end

        if text then
            local auto_ns, auto_domain = _M.parse_resolv_conf(text)
            if #nameservers == 0 and #auto_ns > 0 then
                nameservers = auto_ns
                source = "auto"
            end
            if not cluster_domain and auto_domain then
                cluster_domain = auto_domain
                if source == "manual" then source = "auto" end
            end
        end
    end

    if #nameservers == 0 then
        nameservers = {"kube-dns.kube-system.svc." .. (cluster_domain or "cluster.local")}
        source = "fallback"
    end

    if not cluster_domain then
        cluster_domain = "cluster.local"
        if source == "auto" then source = "fallback" end
    end

    return nameservers, cluster_domain, source
end
```

- [ ] **Step 2.2: 让 `resolve_ips` 支持 nameserver 数组**

将 `router.lua` 的 `resolve_ips` 签名改为：

```lua
function _M.resolve_ips(fqdn, port, nameservers)
    local r, err = resolver:new{
        nameservers = nameservers,
        retrans = 3,
        timeout = 2000,
    }
    ...
end
```

- [ ] **Step 2.3: 修改 `nginx.conf` 中的 `init_worker_by_lua_block`**

替换当前硬编码部分为：

```lua
local router = require "router"
local state = ngx.shared.router_state

-- Helm 只渲染不含 cluster domain 的 base
local headless_base = "{{ .Release.Name }}-{{ .Release.Namespace }}.svc"
local resolver_override = "{{ .Values.nginxRouter.resolver }}"
local domain_override   = "{{ .Values.nginxRouter.discovery.clusterDomain }}"
local gateway_port      = {{ .Values.nginxRouter.discovery.gatewayPort }}
local refresh_ms        = {{ .Values.nginxRouter.discovery.refreshIntervalMs }}

-- Detect nameservers + cluster domain once per worker startup.
local nameservers, cluster_domain, dns_source = router.detect_dns(resolver_override, domain_override)
local headless_fqdn = headless_base .. "." .. cluster_domain

ngx.log(ngx.INFO,
    "nginx-router dns config: source=", dns_source,
    " nameservers=[", table.concat(nameservers, ","), "]",
    " domain=", cluster_domain,
    " fqdn=", headless_fqdn)

local refresh_interval = math.max(refresh_ms / 1000, 1)
local ttl_seconds = math.max(refresh_interval * 3, 30)

local function discover_and_update(premature)
    if premature then return end
    local ips, err = router.resolve_ips(headless_fqdn, gateway_port, nameservers)
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

    -- Also store dns metadata for /router_status.
    state:set("ips", encoded, ttl_seconds)
    local meta_ok, meta = pcall(router.cjson.encode, {
        source = dns_source,
        nameservers = nameservers,
        cluster_domain = cluster_domain,
    })
    if meta_ok then
        state:set("dns_meta", meta, ttl_seconds)
    end
end
```

- [ ] **Step 2.4: 删除 `resolver` / `resolver_timeout` 指令**

从 `nginx.conf` 的 `http {}` 块中移除：

```
resolver {{ .Values.nginxRouter.resolver }} valid=5s;
resolver_timeout 2s;
```

- [ ] **Step 2.5: Commit**

```bash
git add deploy/charts/gateway/templates/nginx-configmap.yaml
git commit -m "feat(gateway): auto-detect DNS nameserver and cluster domain

- detect_dns() uses /etc/resolv.conf unless values override is set.
- resolve_ips now accepts a nameserver array.
- Remove unused nginx resolver directive.
- Store DNS metadata in shared dict for /router_status.

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 3: 修改 values.yaml

**Files:**
- Modify: `deploy/charts/gateway/values.yaml`

- [ ] **Step 3.1: 更新 `discovery` 与 `resolver` 配置段**

把当前：

```yaml
  # Gateway Pod 发现相关
  discovery:
    # 留空时使用 <fullname>-headless
    headlessServiceName: ""
    gatewayPort: 8081
    refreshIntervalMs: 5000
  # kube-dns 地址；多数集群默认值即可工作
  resolver: "kube-dns.kube-system.svc.cluster.local"
```

改为：

```yaml
  # Gateway Pod 发现相关
  discovery:
    # 留空时使用 <fullname>-headless
    headlessServiceName: ""
    gatewayPort: 8081
    refreshIntervalMs: 5000
    # 集群域名后缀（如 cluster.local / k8s.internal）。
    # 留空时由 nginx-router 在启动时从 Pod 的 /etc/resolv.conf 自动探测。
    clusterDomain: ""
  # DNS 服务器地址；多个地址可用空格/逗号分隔。
  # 留空时由 nginx-router 在启动时从 Pod 的 /etc/resolv.conf 自动探测。
  # 仅在 hostNetwork、自定义 dnsConfig 等非常规场景需要显式指定。
  resolver: ""
```

- [ ] **Step 3.2: Commit**

```bash
git add deploy/charts/gateway/values.yaml
git commit -m "chore(gateway): default nginxRouter DNS config to auto-detect

- resolver default changed to empty (auto-detect from resolv.conf).
- Add discovery.clusterDomain override.

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 4: 修复 `headlessServiceName` 死配置

**Files:**
- Modify: `deploy/charts/gateway/templates/gateway-headless-service.yaml`
- Modify: `deploy/charts/gateway/templates/nginx-configmap.yaml`

- [ ] **Step 4.1: 在 values 中定义 headless 名 helper 变量**

打开 `deploy/charts/gateway/templates/nginx-configmap.yaml`，在模板顶部（metadata 之前）新增 helper 变量：

```yaml
{{- $headlessName := .Values.nginxRouter.discovery.headlessServiceName | default (printf "%s-headless" .Release.Name) -}}
```

- [ ] **Step 4.2: 让 ConfigMap 使用 helper 变量**

把 `headless_base` 的渲染从：

```lua
local headless_base = "{{ .Release.Name }}-{{ .Release.Namespace }}.svc"
```

改为：

```lua
local headless_base = "{{ $headlessName }}.{{ .Release.Namespace }}.svc"
```

- [ ] **Step 4.3: 让 headless Service 使用 helper 变量**

在 `deploy/charts/gateway/templates/gateway-headless-service.yaml` 中，把：

```yaml
  name: {{ .Release.Name }}-headless
```

改为：

```yaml
{{- $headlessName := .Values.nginxRouter.discovery.headlessServiceName | default (printf "%s-headless" .Release.Name) -}}
  name: {{ $headlessName }}
```

- [ ] **Step 4.4: Commit**

```bash
git add deploy/charts/gateway/templates/nginx-configmap.yaml deploy/charts/gateway/templates/gateway-headless-service.yaml
git commit -m "fix(gateway): honor discovery.headlessServiceName in both templates

- Service name and FQDN base now respect the existing values field.

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 5: 增强 `/router_status`

**Files:**
- Modify: `deploy/charts/gateway/templates/nginx-configmap.yaml` (`/router_status` location)

- [ ] **Step 5.1: 在 `/router_status` 中输出 DNS 元数据**

替换 `/router_status` 的 `content_by_lua_block` 为：

```lua
local router = require "router"
local state = ngx.shared.router_state
local encoded = state:get("ips") or "[]"
local ips, _ = router.cjson.decode(encoded)
if not ips then ips = {} end

local dns_meta = state:get("dns_meta") or "{}"
local dns, _ = router.cjson.decode(dns_meta)
if not dns then dns = {} end

ngx.say(router.cjson.encode({
    source = "nginx-router",
    discovered_ips = ips,
    dns_source = dns.source or "unknown",
    nameservers = dns.nameservers or {},
    cluster_domain = dns.cluster_domain or "unknown",
}))
```

- [ ] **Step 5.2: Commit**

```bash
git add deploy/charts/gateway/templates/nginx-configmap.yaml
git commit -m "feat(gateway): expose DNS detection metadata in /router_status

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 6: 文档更新

**Files:**
- Modify: `docs/deployment/gateway-daemonset-apisix-chash.md`

- [ ] **Step 6.1: 更新 values 示例段**

把第 205-208 行的示例改为：

```yaml
  discovery:
    headlessServiceName: ""      # 留空默认使用 <fullname>-headless
    gatewayPort: 8081
    refreshIntervalMs: 5000       # DNS 轮询间隔
    clusterDomain: ""             # 留空时自动探测 cluster domain
  resolver: ""                    # 留空时自动探测 nameserver；非常规场景可覆盖
```

- [ ] **Step 6.2: 更新 resolver 说明**

把第 210-211 行提示：

```markdown
> 如果你的集群 DNS 不是 `kube-dns.kube-system.svc.cluster.local`（如 CoreDNS 在其它地址），把 `nginxRouter.resolver` 改成集群实际的 nameserver。
```

改为：

```markdown
> `nginxRouter.resolver` 与 `nginxRouter.discovery.clusterDomain` 默认留空，nginx-router 启动时会自动从所在 Pod 的 `/etc/resolv.conf` 中探测当前集群的 DNS 服务器与 cluster domain，无需为每套集群手动填写。
> 仅在 nginx-router 使用 `hostNetwork`、自定义 `dnsConfig` 等非常规场景时才需要显式覆盖。
```

- [ ] **Step 6.3: 更新 §2.2.3 nginx.conf 核心逻辑说明**

把第 217-218 行：

```markdown
- `resolver` 指向 K8s DNS。
- `init_worker_by_lua_block` 启动定时器，周期性解析 `costrict-web-gateway-headless.costrict.svc.cluster.local`。
```

改为：

```markdown
- `init_worker_by_lua_block` 启动时从 `/etc/resolv.conf` 自动探测 nameserver 与 cluster domain，并周期性解析 `costrict-web-gateway-headless.costrict.svc.<domain>`。
```

- [ ] **Step 6.4: 更新 §3.1 验证命令**

把第 276-277 行：

```markdown
- 检查 `nginxRouter.resolver` 是否配置为集群实际 DNS 地址。
- 进入 nginx-router Pod 执行 `nslookup costrict-web-gateway-headless.costrict.svc.cluster.local` 看能否解析到所有 Pod IP。
```

改为：

```markdown
- 访问 `/router_status`，确认 `dns_source` 为 `auto`，`nameservers` 与 `cluster_domain` 符合当前集群。
- 进入 nginx-router Pod 执行 `nslookup costrict-web-gateway-headless.costrict.svc.$(domain)` 看能否解析到所有 Pod IP（`$(domain)` 取 `/router_status` 中的 `cluster_domain`）。
```

- [ ] **Step 6.5: Commit**

```bash
git add docs/deployment/gateway-daemonset-apisix-chash.md
git commit -m "docs(gateway): document DNS auto-detection for nginx-router

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 7: Helm 模板渲染验证

**Files:**
- （无新增文件，仅命令验证）

- [ ] **Step 7.1: 渲染默认 values 并 diff**

```bash
cd /Users/linkai/code/costrict-web/deploy/charts/gateway
helm template test-release . --set nginxRouter.enabled=true --namespace costrict > /tmp/default-render.yaml
```

检查：

1. `nginx.conf` 中**没有** `resolver` / `resolver_timeout` 行。
2. `init_worker_by_lua_block` 中 `resolver_override = ""`、`domain_override = ""`、`headless_fqdn` 由 base + `.` + `cluster_domain` 拼接。
3. `/router_status` 返回 JSON 包含 `dns_source`/`nameservers`/`cluster_domain`。
4. headless Service 名为 `test-release-headless`。

- [ ] **Step 7.2: 渲染显式覆盖 values 并 diff**

```bash
helm template test-release . \
  --set nginxRouter.enabled=true \
  --set nginxRouter.resolver="10.0.0.10" \
  --set nginxRouter.discovery.clusterDomain="k8s.internal" \
  --set nginxRouter.discovery.headlessServiceName="my-headless" \
  --namespace costrict > /tmp/manual-render.yaml
```

检查：

1. `resolver_override = "10.0.0.10"`、`domain_override = "k8s.internal"`。
2. headless Service 名为 `my-headless`。
3. `headless_base = "my-headless.costrict.svc"`。

- [ ] **Step 7.3: 通过 openresty 配置检查**

把默认渲染出的 ConfigMap 中的 `nginx.conf` 提取到临时文件，用 stock openresty 镜像检查：

```bash
kubectl create configmap tmp-nginx-config --from-file=/tmp/nginx.conf --dry-run=client -o yaml > /dev/null
# 或者用 openresty 镜像本地检查
# docker run --rm -v /tmp/nginx.conf:/tmp/nginx.conf openresty/openresty:1.25.3.1-0-alpine openresty -t -c /tmp/nginx.conf
```

Expected: `syntax is ok` / `test is successful`。

- [ ] **Step 7.4: Commit（可选）**

如果验证中修改了模板细节，再补 commit；否则不提交纯验证步骤。

---

## Task 8: 端到端验证（kind 集群）

**Files:**
- （无新增文件，可选 kind 验证）

- [ ] **Step 8.1: kind 创建集群**

```bash
kind create cluster --name nginx-router-dns-test
```

- [ ] **Step 8.2: 部署 gateway chart（不设置 resolver/domain）**

```bash
cd /Users/linkai/code/costrict-web/deploy/charts/gateway
helm install costrict-gateway . \
  --namespace costrict --create-namespace \
  --set nginxRouter.enabled=true \
  --set image.tag=latest \
  --wait --timeout 5m
```

- [ ] **Step 8.3: 检查 `/router_status`**

```bash
NGINX_POD=$(kubectl get pod -n costrict -l app.kubernetes.io/name=gateway-nginx-router -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n costrict "$NGINX_POD" -- curl -s http://localhost:8080/router_status
```

Expected JSON 包含：

```json
{
  "source": "nginx-router",
  "discovered_ips": ["...:8081", "...:8081"],
  "dns_source": "auto",
  "nameservers": ["10.96.0.10"],
  "cluster_domain": "cluster.local"
}
```

- [ ] **Step 8.4: 手动覆盖验证**

```bash
helm upgrade costrict-gateway . \
  --namespace costrict \
  --set nginxRouter.enabled=true \
  --set nginxRouter.resolver="10.96.0.10" \
  --set nginxRouter.discovery.clusterDomain="cluster.local" \
  --wait --timeout 5m
kubectl exec -n costrict "$NGINX_POD" -- curl -s http://localhost:8080/router_status | jq '.dns_source'
```

Expected: `manual`。

- [ ] **Step 8.5: 清理集群**

```bash
kind delete cluster --name nginx-router-dns-test
```

---

## Task 9: 单测文件最终形态与 CI 运行方式（文档化）

**Files:**
- Modify: `deploy/charts/gateway/tests/router_dns_test.lua`
- Create: `deploy/charts/gateway/tests/README.md`（如 chart 已有测试说明则修改该文件）

- [ ] **Step 9.1: 让测试能加载真实 router.lua**

由于 `router.lua` 嵌在 ConfigMap 模板中，CI 中可用一段简单脚本在测试前把它提取出来。把 `deploy/charts/gateway/tests/router_dns_test.lua` 改为：

```lua
-- 运行方式：
--   cd deploy/charts/gateway
--   lua scripts/extract-router-lua.lua > /tmp/router/router.lua
--   LUA_PATH="/tmp/router/?.lua;;" resty tests/router_dns_test.lua
--
-- extract-router-lua.lua 见下一步骤。
local router = require "router"

local cases = { ... } -- 同 Task 1

local passed = 0
for _, c in ipairs(cases) do
    local ns, domain = router.parse_resolv_conf(c.input)
    ...
end
os.exit(passed == #cases and 0 or 1)
```

- [ ] **Step 9.2: 创建提取脚本**

创建 `deploy/charts/gateway/tests/extract_router_lua.lua`：

```lua
#!/usr/bin/env lua
-- 从 nginx-configmap.yaml 中提取 router.lua 正文并输出到 stdout。
-- 用于在 CI 或本地测试前生成可 require 的 router.lua。
local path = "deploy/charts/gateway/templates/nginx-configmap.yaml"
local fh = assert(io.open(path, "r"))
local content = fh:read("*a")
fh:close()

local start_marker = "  router.lua: |"
local start = content:find(start_marker, 1, true)
assert(start, "router.lua section not found in " .. path)

local line_start = content:find("\n", start) + 1
local _, end_marker = content:find("\n{{- end }}", line_start, true)
if not end_marker then end_marker = #content end

local section = content:sub(line_start, end_marker - 1)
-- 去掉每行开头的两个空格缩进
local lines = {}
for line in section:gmatch("[^\r\n]+") do
    if line:sub(1, 2) == "  " then
        line = line:sub(3)
    end
    lines[#lines + 1] = line
end
print(table.concat(lines, "\n"))
```

- [ ] **Step 9.3: 验证提取脚本与测试可以在本地跑通**

```bash
cd /Users/linkai/code/costrict-web
mkdir -p /tmp/router
lua deploy/charts/gateway/tests/extract_router_lua.lua > /tmp/router/router.lua
LUA_PATH="/tmp/router/?.lua;;" resty deploy/charts/gateway/tests/router_dns_test.lua
```

Expected: 6/6 passed。

- [ ] **Step 9.4: Commit**

```bash
git add deploy/charts/gateway/tests/
git commit -m "test(gateway): wire router.lua unit tests to extracted module

- Add helper to extract router.lua from ConfigMap template.
- Test script requires the extracted module.

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Self-Review Checklist

- [x] **Spec coverage**
  - §3.1 `parse_resolv_conf` → Task 1
  - §3.2 回退链 / source 标注 → Task 2
  - §3.3 ConfigMap 删除 resolver 指令、base+domain 拼接 → Task 2
  - §3.4 values.yaml 默认值改空 + clusterDomain → Task 3
  - §3.5 headlessServiceName 修复 → Task 4
  - §3.6 `/router_status` 增强 → Task 5
  - §5 测试 → Task 1/7/8/9
- [x] **Placeholder scan**: 无 TBD/TODO/"add appropriate error handling"/"similar to Task N" 等。
- [x] **Type consistency**
  - `resolve_ips(fqdn, port, nameservers)` 在 Task 2 已统一为数组；之前 Task 1 的 stub 会被覆盖，计划已标注替换。
  - `detect_dns` 返回 `nameservers, cluster_domain, source` 与 §3.2 一致。
  - `/router_status` 输出字段名与 §3.6 一致。

---

## 执行方式选择

**Plan complete and saved to `docs/superpowers/plans/2026-07-17-nginx-router-dns-autodetect-plan.md`.**

Two execution options:

1. **Subagent-Driven (recommended)** - I dispatch a fresh subagent per task, review between tasks, fast iteration.
2. **Inline Execution** - Execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?
