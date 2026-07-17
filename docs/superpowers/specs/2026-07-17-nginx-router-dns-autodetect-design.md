# nginx-router DNS 自动探测设计：零配置发现 Gateway Pod

- **日期**：2026-07-17
- **状态**：已批准（等待实施）
- **目标 Chart**：`deploy/charts/gateway/`
- **前置文档**：`docs/superpowers/specs/2026-06-24-nginx-chash-layer-design.md`、`docs/deployment/gateway-daemonset-apisix-chash.md`

## 1. 背景与目标

nginx-router 当前通过 Lua 定时解析 headless Service 的 DNS A 记录发现 Gateway Pod。其中两处依赖**每套集群可能不同**的配置：

1. `nginxRouter.resolver`（默认 `kube-dns.kube-system.svc.cluster.local`）——DNS 服务器地址。不同集群形态下各不相同：CoreDNS 自定义 Service 名/命名空间、NodeLocal DNS（`169.254.20.10`）、其他发行版私有 DNS。
2. headless FQDN 中硬编码的 `svc.cluster.local` 后缀——自定义 cluster domain 的集群会直接解析失败。

用户侧运维环境复杂、存在多套 K8s 集群，每套都要手动核对并修改 values，部署成本高。

**目标**：让 nginx-router **零配置**开箱即用——不设置任何 values 也能在任意标准集群中正确发现 Gateway Pod。

**非目标**：

- 不解决 DNS 解析结果在副本间瞬时不一致的问题（已确认不是当前痛点）。
- 不应对"集群 DNS 服务本身不可信"的场景（已确认不是当前痛点；该场景应选 K8s API/注册中心方案）。
- 不改动 chash 路由、TTL/last-known-good 兜底、APISIX 透传等既有逻辑。
- 不引入 RBAC、K8s API watch、sidecar 或自建镜像（沿用 stock `openresty/openresty`）。

## 2. 方案概述

kubelet 会为每个以默认 `dnsPolicy: ClusterFirst` 运行的 Pod 自动生成**正确的** `/etc/resolv.conf`：

- `nameserver` 行就是该集群真实可用的 DNS IP（无论底层是 kube-dns、CoreDNS 自定义名还是 NodeLocal DNS，kubelet 已解析好）；
- `search` 行携带真实的 cluster domain（如 `search <ns>.svc.cluster.local svc.cluster.local cluster.local ...`）。

因此让 `router.lua` 在启动时解析 `/etc/resolv.conf`，自动得到 nameserver 列表和 cluster domain，替代手动配置。values 中的对应项保留为**可选覆盖**（逃生门）：留空 = 自动探测，显式设置 = 强制指定。

## 3. 详细设计

### 3.1 `parse_resolv_conf` 纯函数（router.lua 新增）

```lua
function _M.parse_resolv_conf(text)
    -- 返回 nameservers（数组，保持文件顺序）与 cluster_domain（或 nil）
end
```

解析规则：

- 跳过空行与 `#`/`;` 开头的注释行。
- `nameserver <addr>` 行：收集地址，支持多行，保持顺序。IPv4 原样收集；IPv6 地址按 lua-resty-dns 约定以 `["<addr>"] = true` 形式加入（集群 DNS 几乎总是 IPv4 ClusterIP，此为防御性处理）。
- `search <d1> <d2> ...` 行：按顺序取第一个匹配 `^[^.]+%.svc%.(.+)$` 的域，捕获组即为 cluster domain。
  - 例：`search default.svc.cluster.local svc.cluster.local cluster.local lan` → `cluster.local`。
  - 例（自定义 domain）：`search default.svc.k8s.internal svc.k8s.internal k8s.internal` → `k8s.internal`。
- `domain` / `options` 等其他指令行忽略。
- 文件不可读、无 nameserver、无匹配 search 域时不报错，返回已解析到的部分（调用方走回退链）。

### 3.2 优先级与回退链

| 配置项 | 优先级（高 → 低） |
|---|---|
| DNS nameserver | ① `nginxRouter.resolver` 显式设置 → ② resolv.conf 的 nameserver 行 → ③ `kube-dns.kube-system.svc.<最终确定的 domain>`（WARN 日志） |
| cluster domain | ① `nginxRouter.discovery.clusterDomain` 显式设置 → ② resolv.conf search 域提取 → ③ `cluster.local`（WARN 日志） |

每次回退都在日志中标注来源（`source=manual|auto|fallback`）。

### 3.3 ConfigMap 模板变更（`templates/nginx-configmap.yaml`）

- **删除** `resolver` / `resolver_timeout` 两条 nginx 指令。核实过：当前配置中 `proxy_pass` 指向 upstream 且 `balancer_by_lua` 用 IP 调 `set_current_peer`，nginx 层不做任何域名解析，这两条指令是死配置。
- FQDN 构造从"Helm 渲染完整 FQDN"改为"Helm 渲染 base + 运行时拼接"：

  ```lua
  -- Helm 渲染（不再包含 cluster domain 后缀）
  local headless_base = "{{ .Release.Name }}-headless.{{ .Release.Namespace }}.svc"
  local resolver_override = "{{ .Values.nginxRouter.resolver }}"          -- 空串表示未设置
  local domain_override   = "{{ .Values.nginxRouter.discovery.clusterDomain }}"

  -- 运行时（worker 0 init 阶段，只做一次）
  local nameservers, domain, source = router.detect_dns(resolver_override, domain_override)
  local headless_fqdn = headless_base .. "." .. domain
  ngx.log(ngx.INFO, "dns config: source=", source,
          " nameservers=[", table.concat(nameservers, ","), "]",
          " domain=", domain, " fqdn=", headless_fqdn)
  ```

- `router.detect_dns()`：读取 `/etc/resolv.conf`，按 §3.2 回退链返回最终结果与来源标注；结果在 worker 内缓存（resolv.conf 在 Pod 生命周期内不变）。
- `resolve_ips()` 签名从单个 nameserver 字符串改为 nameserver 数组，传入 `resty.dns.resolver` 的 `nameservers` 选项（多个服务器自动轮询/容错）。

### 3.4 values.yaml 变更

```yaml
nginxRouter:
  discovery:
    headlessServiceName: ""     # 留空时使用 <release>-headless（本次顺带修复，见 §3.5）
    gatewayPort: 8081
    refreshIntervalMs: 5000
    # 集群域名；留空时从 /etc/resolv.conf 的 search 域自动探测
    clusterDomain: ""
  # DNS 服务器；留空时从 /etc/resolv.conf 的 nameserver 自动探测。
  # 仅在 hostNetwork、自定义 dnsConfig 等非常规场景需要显式指定。
  resolver: ""
```

**默认值变更注意**：`resolver` 默认值由 `kube-dns.kube-system.svc.cluster.local` 改为 `""`。已显式设置过的部署行为不变；依赖旧默认值的部署转入自动探测，在标准集群中探测结果与旧默认等价或更准确。

### 3.5 附带修复：`discovery.headlessServiceName` 死配置

现状：values.yaml 声明了 `discovery.headlessServiceName`（"留空时使用 <fullname>-headless"），但 `gateway-headless-service.yaml` 与 `nginx-configmap.yaml` 均**未引用**该值，Service 名被硬编码为 `{{ .Release.Name }}-headless`。

本次顺带让两处统一由该值驱动（默认行为完全不变）：

- Service 名：`<headlessServiceName 或 .Release.Name + "-headless">`
- FQDN base：`<同名>.<namespace>.svc`

### 3.6 `/router_status` 增强

响应中新增字段，便于运维一条命令确认零配置是否生效：

```json
{
  "source": "nginx-router",
  "discovered_ips": ["10.244.1.5:8081", "10.244.2.7:8081"],
  "dns_source": "auto",
  "nameservers": ["10.96.0.10"],
  "cluster_domain": "cluster.local"
}
```

## 4. 错误处理

| 场景 | 行为 |
|---|---|
| `/etc/resolv.conf` 不可读/为空 | 走回退链 ③，WARN 日志，进程不 crash |
| 无 nameserver 行 | 同上 |
| 无匹配的 search 域 | domain 走回退链 ③，WARN 日志 |
| 探测到的 DNS 全部查询失败 | 沿用既有逻辑：保留 last-known-good 列表，TTL（≥30s）到期后过期，`/router_status` 可见空列表 |
| values 显式设置了非法值 | 原样传给 resty.dns.resolver，查询失败时按上一行处理（与现状一致） |

## 5. 测试与验证

1. **纯函数单测**：`parse_resolv_conf` / `detect_dns` 不依赖 ngx 上下文的拆分为独立函数，用 stock openresty 镜像自带的 `resty` CLI 跑 table-driven 用例：
   - 标准 kube-dns（`nameserver 10.96.0.10` + 标准 search 行）
   - NodeLocal DNS（`nameserver 169.254.20.10`）
   - 多 nameserver 行（顺序保持）
   - 自定义 cluster domain（`*.svc.k8s.internal`）
   - 无 search 域 / 空文件 / 只有注释
   - override 优先于探测结果
2. **helm template 验证**：values 空（自动）与非空（手动）两种渲染结果 diff 符合预期；渲染产物通过 `openresty -t` 配置检查。
3. **kind 集群端到端**：
   - 不设置任何 `nginxRouter.resolver`/`clusterDomain` 部署，`/router_status` 显示 `dns_source=auto`、`discovered_ips` 非空；
   - 设备 WS 连接建立且同 deviceID 稳定落同一 Gateway Pod；
   - 显式设置 `resolver` 后 `/router_status` 显示 `dns_source=manual`。

## 6. 兼容性

- 已显式设置 `nginxRouter.resolver` 的存量部署：行为完全不变。
- 依赖旧默认值的存量部署：升级后转入自动探测；在标准集群中等价（resolv.conf 的 nameserver 即 kube-dns Service IP）。唯一行为差异场景是 Pod 使用非默认 dnsPolicy，此时应显式设置 values 覆盖（这正是保留逃生门的原因）。
- 删除 nginx `resolver` 指令无运行时影响（§3.3 已论证其为死配置）。

## 7. 假设与限制

- nginx-router Pod 以默认 `dnsPolicy: ClusterFirst` 运行（chart 不设置 hostNetwork/dnsConfig，现状即如此）。hostNetwork 等非常规场景 resolv.conf 是宿主机的，需用 values 覆盖。
- 仍依赖集群 DNS 服务可用（已确认 DNS 可信，痛点是配置差异）。若未来出现"DNS 不可信"的集群，应评估 K8s API EndpointSlice 或 Server 注册中心方案（本设计不影响后续切换）。
