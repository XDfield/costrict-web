# 移除 server-registry 发现模式 + static 模式自动生成 Pod FQDN 设计

日期:2026-07-21
状态:已获用户批准(2026-07-21)

## 背景

Gateway 高可用方案演进至今,main 上 nginx-router 支持三种 Gateway Pod 发现模式:

- `dns`:解析 headless Service A 记录
- `static`:解析显式配置的 StatefulSet Pod FQDN 列表
- `server-registry`:每 5s 调用 Server `GET /internal/gateway/list` 拉取在线 Gateway 的 `internalURL`

用户部署形态已固定为 **StatefulSet**,Pod 名恒定(`<release>-0`、`<release>-1`…)。用户判断:既然 Pod 名固定,再查 API 获取 Gateway 列表属于多此一举,要求:

1. StatefulSet 部署形态保留。
2. nginx-router 不再通过 Server API 发现 Gateway。
3. server-registry 相关代码**彻底删除**(chart 分支、Server 接口、关联配置、文档)。
4. 替代发现方式为**按副本数自动生成 Pod FQDN 列表**——运维不需要手动维护任何逐 Pod 列表,两套集群零差异配置。

## 架构变化

变更前(server-registry 模式):

```text
nginx-router --HTTP(X-Internal-Secret)--> Server /internal/gateway/list
    <-- [{gatewayID, internalURL=http://PodIP:8081}] --
```

变更后(static 自动生成模式):

```text
Helm 模板 / generate.sh 烧入(与集群无关):
  headless_base = "<release>-headless.<namespace>.svc"
  replicas      = N

nginx-router 运行时(detect_dns 已能自动探测 cluster domain 与 nameservers):
  for i in 0..N-1:
    fqdn_i = "<release>-<i>." .. headless_base .. "." .. cluster_domain
    resolve A(fqdn_i) --> Pod IP
  排序后写入共享字典作为 chash 后端列表
```

不受影响的链路(明确保留):

- Gateway StatefulSet(`GATEWAY_INTERNAL_URL=http://$(POD_IP):8081`,Downward API)。
- Gateway → Server 注册/心跳/注销:`POST /internal/gateway/register`、`POST /internal/gateway/:id/heartbeat`、`DELETE /internal/gateway/:id`、`POST /internal/gateway/device/*`。
- `INTERNAL_SECRET` / `InternalAuth` 中间件:上述内部路由及 `/internal/auth/verify` 仍在使用。
- Server → Gateway 反向代理(Server 经注册表中的 `internalURL` 直连 Pod IP)。

## static 模式自动生成规则

- `nginxRouter.discovery.staticFQDNs` **留空**时:按 `.Values.replicaCount` 自动生成 `<release>-<i>.<headless-base>` 列表(i = 0..N-1)。
- `staticFQDNs` **显式填写**时:沿用显式列表(向后兼容,适用于 Pod 命名不规则的特殊环境)。
- FQDN 的 cluster domain 部分在 Lua 运行时拼接(`detect_dns` 自动探测,可用 `nginxRouter.discovery.clusterDomain` 覆盖),chart 烧入的只有短 base,配置与集群无关。
- 解析失败重试复用 #178 引入的 search 域展开机制(`<ns>.svc.<domain>` → `svc.<domain>` → `<domain>`)。
- 扩缩容:Helm 路径改 `replicaCount`;半手动路径改 `generate.sh` 副本数参数;之后 `kubectl rollout restart` nginx-router。

## 删除清单

### chart(`deploy/charts/gateway/`)

| 文件 | 删除/修改内容 |
|---|---|
| `templates/nginx-configmap.yaml` | server-registry init 分支整体删除;router.lua 中 `resolve_from_registry`、`http_get`、`read_chunked_body`(仅 registry 客户端使用)删除;`mode == "server-registry"` 时 `{{ fail }}` 明确报错 |

> 修正(2026-07-21,写实施计划时):原稿称 `resolve_host`/`query_a`/`read_pod_namespace`/search 域展开保留("static 自动生成依赖")。实际上 static 自动生成镜像 dns 模式的 FQDN 构造(运行时拼接探测到的 cluster domain,`headless_fqdn = headless_base .. "." .. cluster_domain`),不依赖 search 展开;server-registry 删除后这些函数成为死代码,按 YAGNI 一并删除。
| `values.yaml` | 删除 `nginxRouter.discovery.serverUrl`、`nginxRouter.internalSecret`;`discovery.mode` 注释改为 `dns \| static` |
| `templates/nginx-deployment.yaml` | 删除 `INTERNAL_SECRET` env 条件块 |
| `templates/statefulset.yaml` / `deployment.yaml` / `daemonset.yaml` | 清理残留的 server-registry 相关注释 |

### Server(`server/`)

| 文件 | 删除内容 |
|---|---|
| `internal/gateway/handlers.go` | `GatewayListHandler` 及 `RegisterInternalRoutes` 中的 `GET /list` 路由;对应 swagger 注释 |
| `internal/gateway/registry.go` | `ListLiveGateways()`(仅被上述 handler 调用,已确认无其他调用方) |

注:list 接口与 `ListLiveGateways` 无既有测试覆盖(已确认),无需删除测试;只需保证其余测试仍通过。

注意:`InternalAuth`、`InternalSecret` 配置、`/internal/auth/verify`、其余 `/internal/gateway/*` 路由一律保留。

### 文档与示例

| 文件 | 修改内容 |
|---|---|
| `docs/deployment/gateway-statefulset-static.md` | 删除 server-registry 章节(含 ClusterIP 指引);发现方式描述更新为自动生成 FQDN |
| `examples/gateway-statefulset-static/generate.sh` | 入参从 FQDN CSV 改为副本数 |
| `examples/gateway-statefulset-static/nginx-router-configmap.yaml` | 用新 generate.sh 重新生成 |
| `examples/gateway-statefulset-static/nginx-router-deployment.yaml` | 删除 `INTERNAL_SECRET` env |
| `examples/gateway-statefulset-static/README.md` | 用法说明同步更新 |

## 兼容性

- `mode: server-registry` 的存量 values 在 `helm template/upgrade` 时得到明确报错信息(提示改用 static 或 dns),不会静默降级到意外行为。
- `mode: dns`(DaemonSet/Deployment 用户)行为完全不变。
- `mode: static` + 显式 `staticFQDNs` 行为完全不变。
- `mode: static` + 空 `staticFQDNs` 是新增行为(此前为空列表导致无后端),不算破坏。

## 验证

1. `helm lint ./deploy/charts/gateway`。
2. `helm template` 四组渲染断言:
   - `mode=dns`:与 main 渲染结果一致(除 server-registry 分支消失)。
   - `mode=static` + `statefulSet.enabled=true` + `replicaCount=2`:configmap 中出现自动生成的 `gateway-0/1` FQDN 生成逻辑。
   - `mode=static` + 显式 `staticFQDNs`:渲染显式列表。
   - `mode=server-registry`:渲染失败且报错信息清晰。
3. 渲染出的 router.lua / dns_utils.lua 通过 `luac -p`(或 `loadfile`)语法检查。
4. mock DNS 功能测试(沿用 #178 的测试思路):自动生成 2 个 Pod FQDN → 解析 → 得到排序后的 `ip:port` 列表;部分 FQDN NXDOMAIN 时跳过并保留 last-known-good。
5. Server:`go build ./... && go test ./internal/gateway/...`。
6. 示例目录 `generate.sh` 重新生成并做同样的 Lua 语法检查。

## PR 流程

新分支 `feat/gateway-remove-server-registry` → 提交 → push → 创建 PR。**不合并**,等用户 review 批准。
