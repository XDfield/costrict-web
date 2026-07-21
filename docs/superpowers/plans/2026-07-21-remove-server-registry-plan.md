# 移除 server-registry + static 自动生成 Pod FQDN 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 彻底删除 server-registry 发现模式(chart + Server 接口 + 配置 + 文档),static 模式在 `staticFQDNs` 留空时按 `replicaCount` 自动生成 StatefulSet Pod FQDN。

**Architecture:** 发现模式收敛为 `dns` / `static` 两种。static 自动生成的 FQDN 在 Lua 运行时拼接(`<release>-<i>.<headless>.<ns>.svc.` + 自动探测的 cluster domain),渲染产物与集群无关,两套集群零差异配置。Server 侧仅删除 `GET /internal/gateway/list` 与 `ListLiveGateways()`,其余内部路由与 `INTERNAL_SECRET` 认证保留。

**Tech Stack:** Helm chart(gotemplate)、OpenResty Lua、Go(gin)、bash + python3(generate.sh)。

**与 spec 的一处偏差(需用户知悉):** spec 删除清单中说保留 `resolve_host`/`query_a`/`read_pod_namespace`/search 域展开("static 自动生成依赖")。实际设计中 static 自动生成**镜像 dns 模式的 FQDN 构造方式**(运行时拼接探测到的 cluster domain,见 `nginx-configmap.yaml:79`),不依赖 search 展开;server-registry 删除后这些函数成为死代码,按 YAGNI 一并删除。spec 文件已同步修正。

**分支:** `feat/gateway-remove-server-registry`(已基于 origin/main 创建,当前在此分支)。

---

### Task 1: chart — 删除 server-registry 发现分支与 registry 专用 Lua

**Files:**
- Modify: `deploy/charts/gateway/templates/nginx-configmap.yaml`

背景:该文件内嵌三个 discovery 分支(dns/static/server-registry,行 70-218)和完整 router.lua(行 324-699)。registry 专用代码全部位于 router.lua 中 `resolve_static_fqdns` 结束(行 406)到 `build_ring` 注释(行 640)之间,是一段**连续块**,可整段删除。

- [ ] **Step 1: 记录变更前基线(用于后续对比)**

```bash
cd /Users/linkai/code/costrict-web/.claude/worktrees/fuzzy-dazzling-melody
helm template rel ./deploy/charts/gateway --namespace ns \
  --set nginxRouter.enabled=true --set nginxRouter.discovery.mode=dns > /tmp/baseline-dns.yaml
helm template rel ./deploy/charts/gateway --namespace ns \
  --set nginxRouter.enabled=true --set nginxRouter.discovery.mode=static \
  --set 'nginxRouter.discovery.staticFQDNs={a.example.com,b.example.com}' > /tmp/baseline-static-explicit.yaml
# 变更前 server-registry 能正常渲染(变更后必须失败)
helm template rel ./deploy/charts/gateway --namespace ns \
  --set nginxRouter.enabled=true --set nginxRouter.discovery.mode=server-registry \
  --set nginxRouter.discovery.serverUrl=http://x:8080 > /dev/null && echo "BASELINE: server-registry renders (expected before change)"
```

Expected: 三个渲染都成功。

- [ ] **Step 2: 删除 nginx.conf 顶部的 `env INTERNAL_SECRET;`**

用 Edit 删除以下 4 行(含尾部空行),位于 `pid /tmp/nginx.pid;` 与 `events {` 之间:

```
    # Make the internal secret visible to Lua for server-registry mode.
    # NOTE: the env directive is only valid in the main context (top level).
    env INTERNAL_SECRET;

```

- [ ] **Step 3: 删除 server-registry init 分支**

用 Edit 删除从 `            {{- else if eq .Values.nginxRouter.discovery.mode "server-registry" }}` 开始、到 `            {{- else }}` **之前**为止的整段(即原行 165-215,含 `local registry_url` 到 server-registry 版 `discover_and_update` 函数结束的 `            end`)。**保留** `{{- else }}` 行。删除后该位置直接从 static 分支的 `            end` 连接到 `            {{- else }}`。

- [ ] **Step 4: 更新 fail 报错信息**

Edit:

old_string:
```
            {{- fail (printf "Unsupported nginxRouter.discovery.mode: %s" .Values.nginxRouter.discovery.mode) }}
```
new_string:
```
            {{- fail (printf "Unsupported nginxRouter.discovery.mode: %q (supported: dns, static)" .Values.nginxRouter.discovery.mode) }}
```

- [ ] **Step 5: 删除 router.lua 中的 registry 专用连续块**

用 Edit 删除从:

```
    -- Minimal HTTP/1.1 GET client using ngx.socket.tcp. Does not depend on
    -- lua-resty-http, so it works in the stock openresty image.
    local function read_chunked_body(sock)
```

开始,到 `resolve_from_registry` 函数的结束 `    end`(即 `    -- Build ketama ring points deterministically from sorted node list.` 注释之前的那个 `    end` + 其后空行)为止的**整段**。该段包含:`read_chunked_body`、`read_pod_namespace`、`query_a`、`resolve_host`、`http_get`、`resolve_from_registry`。删除后 `resolve_static_fqdns` 的结束 `    end` 直接连接空行 + `    -- Build ketama ring points deterministically from sorted node list.`。

- [ ] **Step 6: 删除失效的 upvalue 声明**

上述函数删除后,以下三个局部声明不再被使用,用 Edit 逐个删除对应行:

```
    local table_insert = table.insert
```
```
    local pairs = pairs
```
```
    local type = type
```

(保留 `ipairs`、`tonumber`、`tostring` 等仍在使用的声明。)

- [ ] **Step 7: 验证 — 渲染矩阵 + Lua 语法 + 死代码扫描**

```bash
cd /Users/linkai/code/costrict-web/.claude/worktrees/fuzzy-dazzling-melody
helm lint ./deploy/charts/gateway
# dns / static 仍正常渲染
helm template rel ./deploy/charts/gateway --namespace ns \
  --set nginxRouter.enabled=true --set nginxRouter.discovery.mode=dns > /tmp/new-dns.yaml
helm template rel ./deploy/charts/gateway --namespace ns \
  --set nginxRouter.enabled=true --set nginxRouter.discovery.mode=static \
  --set 'nginxRouter.discovery.staticFQDNs={a.example.com,b.example.com}' > /tmp/new-static-explicit.yaml
# server-registry 必须渲染失败且报错清晰
helm template rel ./deploy/charts/gateway --namespace ns \
  --set nginxRouter.enabled=true --set nginxRouter.discovery.mode=server-registry 2>&1 | grep 'Unsupported nginxRouter.discovery.mode' && echo "server-registry correctly rejected"
# dns 渲染结果与基线的差异仅限:env INTERNAL_SECRET 行消失
diff <(grep -v 'env INTERNAL_SECRET\|Make the internal secret\|NOTE: the env directive' /tmp/baseline-dns.yaml) /tmp/new-dns.yaml && echo "dns mode: no unexpected diff"
# 提取渲染出的 router.lua 做语法检查与死代码扫描
python3 - <<'EOF'
import re
content = open('/tmp/new-dns.yaml').read()
m = re.search(r'^  router\.lua: \|\n((?:    .*\n?)+)', content, re.M)
open('/tmp/new-router.lua', 'w').write('\n'.join(l[4:] for l in m.group(1).splitlines()))
EOF
lua -e "assert(loadfile('/tmp/new-router.lua'))" && echo "router.lua syntax OK"
grep -c "resolve_from_registry\|http_get\|read_chunked_body\|read_pod_namespace\|query_a\|resolve_host\|INTERNAL_SECRET" /tmp/new-router.lua /tmp/new-dns.yaml || echo "no registry leftovers"
```

Expected: lint 通过;server-registry 报 `Unsupported nginxRouter.discovery.mode: "server-registry" (supported: dns, static)`;dns diff 为空;语法 OK;死代码扫描两个 grep 计数都为 0(命中 `||` 分支)。

- [ ] **Step 8: 提交**

```bash
git add deploy/charts/gateway/templates/nginx-configmap.yaml
git commit -m "feat(gateway)!: remove server-registry discovery mode from nginx-router

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 2: chart — static 模式自动生成 Pod FQDN

**Files:**
- Modify: `deploy/charts/gateway/templates/nginx-configmap.yaml`(static init 分支)

- [ ] **Step 1: 先跑失败检查(变更前 static + 空 staticFQDNs 渲染出空表且无生成逻辑)**

```bash
cd /Users/linkai/code/costrict-web/.claude/worktrees/fuzzy-dazzling-melody
helm template rel ./deploy/charts/gateway --namespace ns \
  --set statefulSet.enabled=true --set replicaCount=2 \
  --set nginxRouter.enabled=true --set nginxRouter.discovery.mode=static \
  | grep -A3 "local static_fqdns" 
```

Expected(变更前):只有空表 `local static_fqdns = {`,没有 for 循环、没有 `pod_name_prefix`。

- [ ] **Step 2: 修改 static 分支,加入自动生成逻辑**

Edit,old_string(当前 static 分支开头到 static_fqdns 表):

```
            {{- else if eq .Values.nginxRouter.discovery.mode "static" }}
            local resolver_override = "{{ .Values.nginxRouter.resolver }}"
            local domain_override   = "{{ .Values.nginxRouter.discovery.clusterDomain }}"
            -- For static FQDN mode we still need a nameserver to resolve each Pod FQDN.
            -- Auto-detect from /etc/resolv.conf unless explicitly overridden.
            local nameservers, cluster_domain, dns_source = dns_utils.detect_dns(resolver_override, domain_override)

            ngx.log(ngx.INFO,
                "nginx-router static fqdn config: source=", dns_source,
                " nameservers=[", table.concat(nameservers, ","), "]")

            local static_fqdns = {
                {{- range .Values.nginxRouter.discovery.staticFQDNs }}
                "{{ . }}",
                {{- end }}
            }
```

new_string:

```
            {{- else if eq .Values.nginxRouter.discovery.mode "static" }}
            local resolver_override = "{{ .Values.nginxRouter.resolver }}"
            local domain_override   = "{{ .Values.nginxRouter.discovery.clusterDomain }}"
            -- For static FQDN mode we still need a nameserver to resolve each Pod FQDN.
            -- Auto-detect from /etc/resolv.conf unless explicitly overridden.
            local nameservers, cluster_domain, dns_source = dns_utils.detect_dns(resolver_override, domain_override)

            ngx.log(ngx.INFO,
                "nginx-router static fqdn config: source=", dns_source,
                " nameservers=[", table.concat(nameservers, ","), "]",
                " domain=", cluster_domain or "unknown")

            local static_fqdns = {
                {{- range .Values.nginxRouter.discovery.staticFQDNs }}
                "{{ . }}",
                {{- end }}
            }
            {{- if not .Values.nginxRouter.discovery.staticFQDNs }}
            {{- if not .Values.statefulSet.enabled }}
            {{- fail "nginxRouter.discovery.staticFQDNs is empty: automatic Pod FQDN generation requires statefulSet.enabled=true; otherwise set staticFQDNs explicitly" }}
            {{- end }}
            -- staticFQDNs left empty: generate the StatefulSet Pod FQDNs from the
            -- replica count. Pod names are stable (<statefulset>-<ordinal>) and the
            -- cluster domain comes from the runtime detection above, so the
            -- rendered config stays identical across clusters.
            local pod_name_prefix = "{{ .Release.Name }}"
            local headless_base = "{{ $headlessName }}.{{ .Release.Namespace }}.svc"
            local gateway_replicas = {{ .Values.replicaCount }}
            for i = 0, gateway_replicas - 1 do
                static_fqdns[#static_fqdns + 1] = pod_name_prefix .. "-" .. i .. "." .. headless_base .. "." .. cluster_domain
            end
            ngx.log(ngx.INFO, "auto-generated ", #static_fqdns, " gateway pod FQDN(s) for ", pod_name_prefix)
            {{- end }}
```

- [ ] **Step 3: 验证 — 三种 static 渲染形态**

```bash
cd /Users/linkai/code/costrict-web/.claude/worktrees/fuzzy-dazzling-melody
# 3a. 自动生成:statefulSet + 空 staticFQDNs → 出现生成逻辑,且无显式 FQDN
helm template rel ./deploy/charts/gateway --namespace ns \
  --set statefulSet.enabled=true --set replicaCount=2 \
  --set nginxRouter.enabled=true --set nginxRouter.discovery.mode=static > /tmp/new-static-auto.yaml
grep -q 'local pod_name_prefix = "rel"' /tmp/new-static-auto.yaml && \
grep -q 'local gateway_replicas = 2' /tmp/new-static-auto.yaml && \
grep -q 'static_fqdns\[#static_fqdns + 1\] = pod_name_prefix' /tmp/new-static-auto.yaml && \
echo "3a auto-gen renders OK"
# 3b. 显式 staticFQDNs:不出现自动生成逻辑,行为与 Task 1 基线一致
helm template rel ./deploy/charts/gateway --namespace ns \
  --set nginxRouter.enabled=true --set nginxRouter.discovery.mode=static \
  --set 'nginxRouter.discovery.staticFQDNs={a.example.com,b.example.com}' > /tmp/new-static-explicit2.yaml
grep -q '"a.example.com",' /tmp/new-static-explicit2.yaml && \
! grep -q 'pod_name_prefix' /tmp/new-static-explicit2.yaml && \
echo "3b explicit list unchanged OK"
# 3c. 空 staticFQDNs 但未开 statefulSet → 渲染失败且报错清晰
helm template rel ./deploy/charts/gateway --namespace ns \
  --set nginxRouter.enabled=true --set nginxRouter.discovery.mode=static 2>&1 \
  | grep 'automatic Pod FQDN generation requires statefulSet.enabled=true' && \
echo "3c missing-statefulSet correctly rejected"
# 3d. 渲染出的 nginx.conf init 块语法检查(提取 static 分支所在 nginx.conf 整体)
python3 - <<'EOF'
import re
content = open('/tmp/new-static-auto.yaml').read()
m = re.search(r'^  nginx\.conf: \|\n((?:    .*\n?)+)', content, re.M)
open('/tmp/new-nginx.conf', 'w').write('\n'.join(l[4:] for l in m.group(1).splitlines()))
EOF
# init_worker_by_lua_block 是 nginx 指令包 Lua,提取 Lua 块单独过 loadfile
python3 - <<'EOF'
import re
conf = open('/tmp/new-nginx.conf').read()
m = re.search(r'init_worker_by_lua_block \{(.*?)\n        \}', conf, re.S)
lua = m.group(1)
# 去掉每行前导 12 空格缩进
lua = '\n'.join(l[12:] if l.startswith(' ' * 12) else l for l in lua.splitlines())
open('/tmp/new-init.lua', 'w').write(lua)
EOF
lua -e "assert(loadfile('/tmp/new-init.lua'))" && echo "3d init_worker lua syntax OK"
```

Expected: 3a/3b/3c/3d 全部打印 OK。

- [ ] **Step 4: 功能测试 — mock DNS 验证自动生成的 FQDN 能被解析成 ip:port 列表**

```bash
cat > /tmp/test_static_autogen.lua <<'EOF'
-- 模拟 static 自动生成 + resolve_static_fqdns 的完整链路
ngx = { crc32_long = function(s) return 1 end, log = function() end, WARN = 1, ERR = 1, INFO = 1 }
local answers_db = {}
package.preload["resty.dns.resolver"] = function()
    return {
        TYPE_A = 1,
        new = function(self, opts)
            return {
                TYPE_A = 1,
                query = function(_, name, _)
                    local ans = answers_db[name]
                    if ans then return ans end
                    return { errcode = 3 }  -- NXDOMAIN
                end,
            }
        end,
    }
end
package.preload["cjson.safe"] = function() return { encode = function(x) return "" end, decode = function() return nil end } end
package.preload["dns_utils"] = function() return {} end

local router = dofile("/tmp/new-router.lua")

-- 与 nginx.conf static 分支相同的生成逻辑
local cluster_domain = "cluster.local"
local pod_name_prefix = "rel"
local headless_base = "rel-headless.ns.svc"
local gateway_replicas = 2
local static_fqdns = {}
for i = 0, gateway_replicas - 1 do
    static_fqdns[#static_fqdns + 1] = pod_name_prefix .. "-" .. i .. "." .. headless_base .. "." .. cluster_domain
end
assert(static_fqdns[1] == "rel-0.rel-headless.ns.svc.cluster.local", "fqdn[1]: " .. static_fqdns[1])
assert(static_fqdns[2] == "rel-1.rel-headless.ns.svc.cluster.local", "fqdn[2]: " .. static_fqdns[2])
print("gen OK: " .. table.concat(static_fqdns, ", "))

-- 两个 Pod 都解析成功
answers_db = {
    ["rel-0.rel-headless.ns.svc.cluster.local"] = { { address = "10.0.0.11", type = 1 } },
    ["rel-1.rel-headless.ns.svc.cluster.local"] = { { address = "10.0.0.12", type = 1 } },
}
local ips, err = router.resolve_static_fqdns(static_fqdns, 8081, {"10.0.0.2"})
assert(ips and #ips == 2, "expect 2 ips, got err: " .. tostring(err))
assert(ips[1] == "10.0.0.11:8081" and ips[2] == "10.0.0.12:8081", "sorted ip:port list: " .. table.concat(ips, ","))
print("resolve-all OK: " .. table.concat(ips, ", "))

-- 一个 Pod NXDOMAIN(未就绪):跳过它,返回另一个
answers_db["rel-1.rel-headless.ns.svc.cluster.local"] = nil
ips, err = router.resolve_static_fqdns(static_fqdns, 8081, {"10.0.0.2"})
assert(ips and #ips == 1 and ips[1] == "10.0.0.11:8081", "partial NXDOMAIN tolerated")
print("partial-nxdomain OK: " .. table.concat(ips, ", "))
EOF
lua /tmp/test_static_autogen.lua
```

Expected: 三行 OK 输出(gen / resolve-all / partial-nxdomain)。

- [ ] **Step 5: 提交**

```bash
git add deploy/charts/gateway/templates/nginx-configmap.yaml
git commit -m "feat(gateway): auto-generate StatefulSet Pod FQDNs in static discovery mode

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 3: chart — values.yaml 与 nginx-deployment.yaml 清理

**Files:**
- Modify: `deploy/charts/gateway/values.yaml`
- Modify: `deploy/charts/gateway/templates/nginx-deployment.yaml`

- [ ] **Step 1: values.yaml — 更新模式注释**

Edit:

old_string:
```
# 支持三种发现模式：
#   dns            - 解析 Gateway headless Service DNS A 记录（默认，兼容旧行为）
#   static         - 解析固定的 StatefulSet Pod FQDN 列表
#   server-registry- 从 Server /internal/gateway/list 拉取在线 Gateway 的 internalURL
```
new_string:
```
# 支持两种发现模式：
#   dns            - 解析 Gateway headless Service DNS A 记录（默认，兼容旧行为）
#   static         - 解析 StatefulSet Pod FQDN；staticFQDNs 留空时按 replicaCount 自动生成
```

- [ ] **Step 2: values.yaml — 更新 mode 注释与 staticFQDNs 注释**

Edit:

old_string:
```
    # 发现模式：dns | static | server-registry
    mode: "dns"
```
new_string:
```
    # 发现模式：dns | static
    mode: "dns"
```

Edit:

old_string:
```
    # static 模式下需要解析的 StatefulSet Pod FQDN 列表
    # 示例：
    #   - "costrict-web-gateway-0.costrict-web-gateway-headless.costrict.svc.cluster.local"
    #   - "costrict-web-gateway-1.costrict-web-gateway-headless.costrict.svc.cluster.local"
    staticFQDNs: []
    # server-registry 模式下 Server 的地址，用于拉取在线 Gateway 列表。
    # 可填集群内域名（如 http://costrict-web-api:8080）；当集群 DNS 不可用时，
    # 直接填 costrict-web-api Service 的固定 ClusterIP（如 http://10.96.0.10:8080），
    # ClusterIP 在 Service 生命周期内不漂移，可完全不依赖 DNS。
    serverUrl: ""
  # server-registry 模式下访问 Server 内部接口的共享密钥，与 Gateway 的 INTERNAL_SECRET 一致
  internalSecret: ""
```
new_string:
```
    # static 模式下需要解析的 StatefulSet Pod FQDN 列表。
    # 留空时按 statefulSet 的 replicaCount 自动生成
    # <release>-<i>.<release>-headless.<namespace>.svc.<cluster-domain>（i = 0..N-1），
    # cluster-domain 由 nginx-router 运行时自动探测；仅在 Pod 命名不规则时才需显式填写。
    # 示例：
    #   - "costrict-web-gateway-0.costrict-web-gateway-headless.costrict.svc.cluster.local"
    #   - "costrict-web-gateway-1.costrict-web-gateway-headless.costrict.svc.cluster.local"
    staticFQDNs: []
```

- [ ] **Step 3: nginx-deployment.yaml — 删除 INTERNAL_SECRET env 块**

Edit 删除:

```
          {{- if eq .Values.nginxRouter.discovery.mode "server-registry" }}
          env:
            - name: INTERNAL_SECRET
              value: {{ .Values.nginxRouter.internalSecret | quote }}
          {{- end }}
```

- [ ] **Step 4: 验证**

```bash
cd /Users/linkai/code/costrict-web/.claude/worktrees/fuzzy-dazzling-melody
helm lint ./deploy/charts/gateway
helm template rel ./deploy/charts/gateway --namespace ns \
  --set statefulSet.enabled=true --set nginxRouter.enabled=true \
  --set nginxRouter.discovery.mode=static > /tmp/t3.yaml
grep -c "INTERNAL_SECRET\|internalSecret\|serverUrl" /tmp/t3.yaml || echo "no leftovers in rendered output"
grep -n "server-registry\|serverUrl\|internalSecret" deploy/charts/gateway/values.yaml deploy/charts/gateway/templates/nginx-deployment.yaml || echo "no leftovers in chart"
```

Expected: lint 通过;两个 grep 均命中 `||` 分支(计数 0 / 无匹配)。

- [ ] **Step 5: 提交**

```bash
git add deploy/charts/gateway/values.yaml deploy/charts/gateway/templates/nginx-deployment.yaml
git commit -m "feat(gateway)!: drop serverUrl/internalSecret values for removed server-registry mode

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 4: Server — 删除 GET /internal/gateway/list 与 ListLiveGateways

**Files:**
- Modify: `server/internal/gateway/handlers.go`
- Modify: `server/internal/gateway/registry.go`

- [ ] **Step 1: 删除路由注册行**

Edit `server/internal/gateway/handlers.go`:

old_string:
```
	gatewayGroup.GET("/list", GatewayListHandler(registry))
```
new_string:(整行删除,即替换为空;注意 Edit 要求 new_string 与 old_string 不同,直接将该行连同换行删掉)

实际操作:old_string 为
```
	gatewayGroup := group.Group("/gateway")
	gatewayGroup.GET("/list", GatewayListHandler(registry))
	gatewayGroup.POST("/register", GatewayRegisterHandler(registry))
```
new_string 为
```
	gatewayGroup := group.Group("/gateway")
	gatewayGroup.POST("/register", GatewayRegisterHandler(registry))
```

- [ ] **Step 2: 删除 GatewayListHandler**

Edit `server/internal/gateway/handlers.go`,删除从:

```
// GatewayListHandler godoc
// @Summary      List live gateways
```

到 `	c.JSON(http.StatusOK, gin.H{"gateways": items})
	}
}
`(含尾部空行)的整个函数(含全部 swagger 注释,约行 20-60)。

- [ ] **Step 3: 删除 ListLiveGateways**

Edit `server/internal/gateway/registry.go`,删除:

```
// ListLiveGateways returns all gateways whose heartbeat has not timed out.
func (r *GatewayRegistry) ListLiveGateways() ([]*GatewayInfo, error) {
	gateways, err := r.store.ListGateways()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	var live []*GatewayInfo
	for _, gw := range gateways {
		if now-gw.LastHeartbeat <= GatewayHeartbeatTimeoutMs {
			live = append(live, gw)
		}
	}
	return live, nil
}

```

- [ ] **Step 4: 验证编译与测试**

```bash
cd /Users/linkai/code/costrict-web/.claude/worktrees/fuzzy-dazzling-melody/server
gofmt -l internal/gateway/   # 期望无输出(若列出的是本任务未触碰的既有文件则保持不动)
go build ./...
go vet ./internal/gateway/
go test ./internal/gateway/...
```

Expected: build/vet/test 全部通过。注意仓库中存在**既有** gofmt 不合格文件(handlers_test.go、session_proxy.go、session_service.go),不要顺手格式化它们;只保证本任务改过的两个文件不在 `gofmt -l` 输出中。

- [ ] **Step 5: 提交**

```bash
git add server/internal/gateway/handlers.go server/internal/gateway/registry.go
git commit -m "feat(server)!: remove internal gateway list endpoint

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 5: 文档更新

**Files:**
- Modify: `docs/deployment/gateway-statefulset-static.md`

- [ ] **Step 1: 删除 server-registry 章节**

先读取确认章节边界:

```bash
grep -n "^## " /Users/linkai/code/costrict-web/.claude/worktrees/fuzzy-dazzling-melody/docs/deployment/gateway-statefulset-static.md
```

删除从 `## 5. 备选：server-registry 发现模式（完全不依赖 DNS）`(约行 196)到**下一个 `## ` 标题之前**(或文件末尾)的整节。

- [ ] **Step 2: 更新发现方式描述**

在同一文档中找到介绍 static 发现的位置(文档开头方案描述部分),将"解析固定的 StatefulSet Pod FQDN 列表"之类的表述更新为:

> static 模式下 `staticFQDNs` 留空,nginx-router 会按 Gateway 副本数(`replicaCount`)自动生成 StatefulSet Pod FQDN(`<release>-<i>.<release>-headless.<namespace>.svc.<cluster-domain>`),cluster-domain 运行时自动探测,跨集群无需修改配置。扩缩容只需修改 `replicaCount` 并滚动重启 nginx-router。

同时检查文档中扩缩容相关段落:把"同步修改 FQDN 列表"改为"同步修改 replicaCount/副本数"。

- [ ] **Step 3: 全库扫描残留**

```bash
cd /Users/linkai/code/costrict-web/.claude/worktrees/fuzzy-dazzling-melody
grep -rn "server-registry" docs/ --include="*.md" | grep -v superpowers || echo "docs clean"
```

Expected: 命中 `||` 分支(superpowers 规格/计划文档中的历史记录保留)。

- [ ] **Step 4: 提交**

```bash
git add docs/deployment/gateway-statefulset-static.md
git commit -m "docs(gateway): drop server-registry section, document auto-generated FQDNs

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 6: 示例目录更新(generate.sh / configmap / deployment / README)

**Files:**
- Modify: `examples/gateway-statefulset-static/generate.sh`(整体重写)
- Modify: `examples/gateway-statefulset-static/nginx-router-deployment.yaml`
- Modify: `examples/gateway-statefulset-static/nginx-router-configmap.yaml`(由 generate.sh 重新生成)
- Modify: `examples/gateway-statefulset-static/README.md`

- [ ] **Step 1: 重写 generate.sh**

用 Write 整体替换 `examples/gateway-statefulset-static/generate.sh` 为:

```bash
#!/usr/bin/env bash
# 用 Helm 重新生成 nginx-router-configmap.yaml（static 发现模式，自动生成 Pod FQDN）。
# 用法：
#   ./generate.sh <RELEASE_NAME> <NAMESPACE> <REPLICAS> [CLUSTER_DNS]
#
# 示例：
#   ./generate.sh costrict-web-gateway costrict 2
#
# REPLICAS 是 Gateway StatefulSet 的副本数；生成的 ConfigMap 会按副本数自动
# 生成 Pod FQDN（<release>-0/1/...）。CLUSTER_DNS 留空时，nginx-router 会从
# Pod 的 /etc/resolv.conf 自动探测 DNS；仅在 hostNetwork、自定义 dnsConfig
# 等非常规场景需要显式指定。

set -euo pipefail

RELEASE_NAME="${1:-costrict-web-gateway}"
NAMESPACE="${2:-default}"
REPLICAS="${3:-}"
CLUSTER_DNS="${4:-}"

if [ -z "$REPLICAS" ]; then
    echo "ERROR: REPLICAS is required" >&2
    echo "Usage: $0 <RELEASE_NAME> <NAMESPACE> <REPLICAS> [CLUSTER_DNS]" >&2
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="${SCRIPT_DIR}/../../deploy/charts/gateway"
OUT_FILE="${SCRIPT_DIR}/nginx-router-configmap.yaml"
TMP_PY=$(mktemp)
trap 'rm -f "$TMP_PY"' EXIT

cat > "$TMP_PY" <<'PY'
import sys, yaml

release_name, namespace, replicas, cluster_dns = sys.argv[1:5]

docs = list(yaml.safe_load_all(sys.stdin))
for doc in docs:
    if doc and doc.get('kind') == 'ConfigMap' and doc.get('metadata', {}).get('name') == f'{release_name}-nginx-router-config':
        header = f'''# AUTO-GENERATED from deploy/charts/gateway (static discovery mode).
# You can regenerate this file with:
#   ./generate.sh {release_name} {namespace} {replicas} \\
'''
        if cluster_dns:
            header += f'#     "{cluster_dns}"\n'
        else:
            header += '#     (omit CLUSTER_DNS to auto-detect from /etc/resolv.conf)\n'
        header += '''#
# The ConfigMap auto-generates StatefulSet Pod FQDNs from the replica count
# below ("local gateway_replicas"); keep it in sync when you scale the
# StatefulSet, then restart nginx-router.
'''
        lua_header = header.replace('# ', '-- ')
        conf = doc['data']['nginx.conf']
        lua = doc['data']['router.lua']
        dns_utils = doc['data']['dns_utils.lua']
        if cluster_dns:
            conf = conf.replace(cluster_dns, '{{CLUSTER_DNS}}')
        else:
            conf = conf.replace('local resolver_override = ""', 'local resolver_override = ""  -- set {{CLUSTER_DNS}} to override auto-detection')
        conf = conf.replace(f'local pod_name_prefix = "{release_name}"', 'local pod_name_prefix = "{{RELEASE_NAME}}"')
        conf = conf.replace(f'local headless_base = "{release_name}-headless.{namespace}.svc"', 'local headless_base = "{{RELEASE_NAME}}-headless.{{NAMESPACE}}.svc"')
        conf = conf.replace(f'local gateway_replicas = {replicas}', 'local gateway_replicas = {{REPLICAS}}')
        doc['data']['nginx.conf'] = header + conf
        doc['data']['router.lua'] = lua_header + lua
        doc['data']['dns_utils.lua'] = lua_header + dns_utils
        doc['metadata']['name'] = '{{RELEASE_NAME}}-nginx-router-config'
        doc['metadata']['namespace'] = '{{NAMESPACE}}'
        print(yaml.dump(doc, sort_keys=False, allow_unicode=True))
        sys.exit(0)
print('ERROR: nginx-router ConfigMap not found in helm output', file=sys.stderr)
sys.exit(1)
PY

# Build helm args
HELM_ARGS=(
  --namespace "${NAMESPACE}"
  --set statefulSet.enabled=true
  --set nginxRouter.enabled=true
  --set nginxRouter.discovery.mode=static
  --set "replicaCount=${REPLICAS}"
)
if [ -n "${CLUSTER_DNS}" ]; then
  HELM_ARGS+=(--set "nginxRouter.resolver=${CLUSTER_DNS}")
fi

# Generate and post-process in one shot. We keep only the nginx-router ConfigMap,
# then replace rendered values with obvious placeholders.
helm template "${RELEASE_NAME}" "${CHART_DIR}" "${HELM_ARGS[@]}" \
  | python3 "$TMP_PY" "${RELEASE_NAME}" "${NAMESPACE}" "${REPLICAS}" "${CLUSTER_DNS}" > "${OUT_FILE}"

echo "Generated: ${OUT_FILE}"
echo "Please review and replace remaining placeholders before applying."
```

- [ ] **Step 2: 删除示例 deployment 的 INTERNAL_SECRET env 块**

Edit `examples/gateway-statefulset-static/nginx-router-deployment.yaml` 删除:

```
          env:
            # server-registry 模式下需要，与 Gateway 的 INTERNAL_SECRET 一致；
            # static/dns 模式下可留空。
            - name: INTERNAL_SECRET
              value: "<INTERNAL_SECRET>"
```

- [ ] **Step 3: 重新生成示例 configmap**

```bash
cd /Users/linkai/code/costrict-web/.claude/worktrees/fuzzy-dazzling-melody/examples/gateway-statefulset-static
./generate.sh costrict-web-gateway costrict 2
```

Expected: 输出 `Generated: ...`;生成文件中包含 `local pod_name_prefix = "{{RELEASE_NAME}}"`、`local gateway_replicas = {{REPLICAS}}` 占位符。

- [ ] **Step 4: 验证生成产物**

```bash
cd /Users/linkai/code/costrict-web/.claude/worktrees/fuzzy-dazzling-melody/examples/gateway-statefulset-static
# YAML 可被解析
python3 -c "import yaml; yaml.safe_load(open('nginx-router-configmap.yaml'))" && echo "yaml OK"
# 提取 router.lua / dns_utils.lua 做语法检查(YAML 双引号串需反转义,复用既有提取方式)
python3 - <<'EOF'
import re
content = open('nginx-router-configmap.yaml').read()
for name in ['router.lua', 'dns_utils.lua']:
    m = re.search(r'^  ' + re.escape(name) + r': "(.*?)"$', content, re.M | re.S)
    raw = m.group(1).replace('\\\n', '').replace('\\ ', ' ')
    text = raw.encode().decode('unicode_escape')
    open('/tmp/ex-' + name, 'w').write(text)
EOF
lua -e "assert(loadfile('/tmp/ex-router.lua'))" && echo "router.lua syntax OK"
lua -e "assert(loadfile('/tmp/ex-dns_utils.lua'))" && echo "dns_utils.lua syntax OK"
# 死代码扫描
grep -c "server-registry\|INTERNAL_SECRET\|resolve_from_registry\|http_get" nginx-router-configmap.yaml nginx-router-deployment.yaml || echo "examples clean"
```

Expected: yaml/lua 检查全过;死代码扫描命中 `||` 分支。

- [ ] **Step 5: 更新 README.md**

对 `examples/gateway-statefulset-static/README.md` 做以下 Edit:

1. old_string:
```
- nginx-router 通过**固定 FQDN 列表**解析 Gateway Pod，不依赖 headless Service DNS 自动发现。
- 运维只需维护 FQDN 列表，**无需跟踪漂移的 Pod IP**。
```
new_string:
```
- nginx-router 按副本数**自动生成 StatefulSet Pod FQDN** 并逐个解析，不依赖 headless Service DNS 自动发现。
- 运维只需维护**副本数**(`gateway_replicas`)，**无需跟踪漂移的 Pod IP，也无需手填 FQDN 列表**。
```

2. old_string:
```
> helm upgrade --install costrict-web-gateway ./deploy/charts/gateway \
>   --set statefulSet.enabled=true \
>   --set nginxRouter.enabled=true \
>   --set nginxRouter.discovery.mode=static \
>   --set 'nginxRouter.discovery.staticFQDNs={...}'
> ```
```
new_string:
```
> helm upgrade --install costrict-web-gateway ./deploy/charts/gateway \
>   --set statefulSet.enabled=true \
>   --set replicaCount=2 \
>   --set nginxRouter.enabled=true \
>   --set nginxRouter.discovery.mode=static
> ```
>
> static 模式下 `staticFQDNs` 留空即按 `replicaCount` 自动生成 Pod FQDN。
```

3. old_string(快速部署步骤 2 整节):
```
2. **重点修改 FQDN 列表**

   在 `nginx-router-configmap.yaml` 的 `nginx.conf` 中，找到 `static_fqdns` 表，
   替换为实际 StatefulSet Pod FQDN：

   ```lua
   local static_fqdns = {
       "costrict-web-gateway-0.costrict-web-gateway-headless.costrict.svc.cluster.local",
       "costrict-web-gateway-1.costrict-web-gateway-headless.costrict.svc.cluster.local",
   }
   ```

   FQDN 格式：`<pod-name>.<headless-service-name>.<namespace>.svc.<cluster-domain>`
```
new_string:
```
2. **确认副本数一致**

   在 `nginx-router-configmap.yaml` 的 `nginx.conf` 中，找到
   `local gateway_replicas = {{REPLICAS}}`，替换为 StatefulSet 实际副本数。
   nginx-router 会据此自动生成 `<RELEASE_NAME>-0/1/...` 的 Pod FQDN，
   cluster domain 运行时自动探测，无需手填。
```

4. old_string(扩缩容段落):
```
- **扩缩容**：修改 StatefulSet `replicas` 和 `nginx-router-configmap.yaml` 中的 `static_fqdns`，
  然后重新应用 ConfigMap，最后滚动重启 nginx-router Deployment：
```
new_string:
```
- **扩缩容**：修改 StatefulSet `replicas` 和 `nginx-router-configmap.yaml` 中的
  `local gateway_replicas`，然后重新应用 ConfigMap，最后滚动重启 nginx-router Deployment：
```

5. old_string(重新生成章节用法):
```
```bash
cd examples/gateway-statefulset-static
./generate.sh <RELEASE_NAME> <NAMESPACE> \
  "costrict-web-gateway-0.costrict-web-gateway-headless.costrict.svc.cluster.local,costrict-web-gateway-1.costrict-web-gateway-headless.costrict.svc.cluster.local" \
  [CLUSTER_DNS]
```
```
new_string:
```
```bash
cd examples/gateway-statefulset-static
./generate.sh <RELEASE_NAME> <NAMESPACE> <REPLICAS> [CLUSTER_DNS]
```
```

- [ ] **Step 6: 提交**

```bash
git add examples/gateway-statefulset-static/
git commit -m "docs(examples): regenerate static example with auto-generated Pod FQDNs

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 7: 全量验证 + push + 创建 PR

**Files:** 无新增改动,仅验证与 PR。

- [ ] **Step 1: 全量验证矩阵**

```bash
cd /Users/linkai/code/costrict-web/.claude/worktrees/fuzzy-dazzling-melody
helm lint ./deploy/charts/gateway
# 渲染矩阵:dns / static 显式 / static 自动 / server-registry(必须失败) / 默认 values
helm template rel ./deploy/charts/gateway --namespace ns --set nginxRouter.enabled=true --set nginxRouter.discovery.mode=dns > /dev/null && echo "dns OK"
helm template rel ./deploy/charts/gateway --namespace ns --set nginxRouter.enabled=true --set nginxRouter.discovery.mode=static --set 'nginxRouter.discovery.staticFQDNs={a.example.com}' > /dev/null && echo "static-explicit OK"
helm template rel ./deploy/charts/gateway --namespace ns --set statefulSet.enabled=true --set nginxRouter.enabled=true --set nginxRouter.discovery.mode=static > /dev/null && echo "static-auto OK"
helm template rel ./deploy/charts/gateway --namespace ns --set nginxRouter.enabled=true --set nginxRouter.discovery.mode=server-registry 2>&1 | grep -q "Unsupported nginxRouter.discovery.mode" && echo "server-registry rejected OK"
helm template rel ./deploy/charts/gateway --namespace ns > /dev/null && echo "default values OK"
# Go 侧
cd server && go build ./... && go test ./internal/gateway/... && cd ..
# Lua 功能测试(Task 2 Step 4 脚本,依赖 /tmp/new-router.lua,如已清理则重新提取)
lua /tmp/test_static_autogen.lua
```

Expected: 全部 OK。

- [ ] **Step 2: push 并创建 PR(网络超时需重试,本环境 api.github.com 间歇性超时)**

```bash
cd /Users/linkai/code/costrict-web/.claude/worktrees/fuzzy-dazzling-melody
git push ssh://git@github.com/XDfield/costrict-web.git feat/gateway-remove-server-registry:feat/gateway-remove-server-registry
gh pr create --base main --head feat/gateway-remove-server-registry \
  --title "feat(gateway)!: remove server-registry discovery, auto-generate static Pod FQDNs" \
  --body "<按 PR 模板填写:Summary(三点:删除 server-registry;static 按 replicaCount 自动生成 FQDN;Server 删除 /internal/gateway/list)+ 升级注意事项(mode=server-registry 的 values 会渲染报错,改用 mode=static)+ Test Plan(本计划各验证项)>"
```

若 `gh pr create` 因网络超时失败,间隔 8 秒重试最多 3 次(此前会话中重试均最终成功)。

- [ ] **Step 3: 向用户汇报,等待 review——不合并 PR**

报告:PR 链接、改动摘要、对用户集群的影响(他们的 values 里 `discovery.mode: server-registry` 需要在 chart 升级时改为 `static` + 删除 `serverUrl`/`internalSecret`,改完后 `kubectl rollout restart deployment/gateway-nginx-router -n costrict-web`)。**未经用户明确批准不合并。**

---

## Self-Review 记录

- Spec 覆盖:删除清单 7 项(configmap/values/nginx-deployment/handlers/registry/docs/examples)→ Task 1/3/4/5/6 全覆盖;static 自动生成 → Task 2;验证矩阵 → Task 1/2/3/7;PR 流程 → Task 7。
- 偏差:search 域展开相关函数随死代码一并删除(见头部说明),spec 已同步修正。
- 命名一致性:`pod_name_prefix`/`headless_base`/`gateway_replicas`/`static_fqdns`/`resolve_static_fqdns` 在 Task 2/4/6 间一致;占位符 `{{RELEASE_NAME}}/{{NAMESPACE}}/{{REPLICAS}}/{{CLUSTER_DNS}}` 与 generate.sh 替换逻辑一致。
