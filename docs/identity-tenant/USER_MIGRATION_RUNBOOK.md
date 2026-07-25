# User / IdP 迁移上线手册 (cs-user 切换 Runbook)

> **目标读者**:平台运维 / SRE / on-call 工程师
> **适用分支**:`feat/identity-tenant`
> **相关设计**:`docs/identity-tenant/USER_CENTER_DESIGN.md`、`docs/identity-tenant/IDENTITY_FEDERATION_DECISION.md`

---

## 1. 背景

`feat/identity-tenant` 分支引入独立的 `cs-user` 服务,接管 user + IdP(casdoor subject / enterprise provider)数据。原 server(`api`)保留兼容路径,通过 `USER_SERVICE_BACKEND` env 在 `local`(旧)和 `rpc`(走 cs-user)之间切换。

迁移采用 **strangler fig 渐进模式**:ETL 一次性灌历史数据 → DualWriter 双写过渡 → JWT 三态切换 → 最终下线 server 侧 user 表。

## 2. 风险与回滚原则

| 风险 | 缓解 |
|---|---|
| ETL 中断留下脏数据 | 工具幂等 + 每批事务,可重跑 |
| DualWriter 期间 cs-user 不可用 | Secondary 失败容忍,只记日志不阻塞主流程 |
| JWT 验证逻辑切换造成登录失败 | `JWT_SIGN_MODE` 三态:off → dual → single,可在 dual 长期停留 |
| server user 表与 cs-user 漂移 | T2 阶段对账脚本;DualWriter 主备一致 |

**任何阶段失败都可回滚到上一阶段**(各阶段回滚步骤见末尾)。

---

## 3. 组件速查

| 组件 | 位置 | 作用 |
|---|---|---|
| `cs-user` 服务 | `cs-user/` (Go) | 用户/IdP/租户新 owner,:8081 ClusterIP |
| `cs-user-migrate` | `cs-user/cmd/migrate/` | 建表/迁移工具 |
| `cs-user-etl` | `cs-user/cmd/etl/` | 一次性历史数据搬迁 |
| `cs-user-bootstrap` | `cs-user/cmd/bootstrap/` | 拉起首个 tenant / git_server / employment mapping |
| `DualWriter` | `server/internal/user/` | 双写过渡 |
| `RPCWriter` | `server/internal/user/` | server → cs-user RPC 写 |
| Helm charts | `deploy/charts/{api,cs-user}/` | 部署模板 |

---

## 4. 前置条件

执行迁移前确认:

- [ ] k8s 集群可用,`costrict` namespace 存在
- [ ] 现有 server(api)部署健康,`/health` 返回 200
- [ ] PostgreSQL 实例可用,有备份(server DB 全量备份,推荐 PITR)
- [ ] Casdoor 实例可用, OAuth 配置正确
- [ ] 镜像 `ghcr.io/xdfield/costrict-web-{api,cs-user}:0.0.93-integration.7+` 在仓库
- [ ] Helm chart `api` + `cs-user` 版本 ≥ 0.0.93-integration.7
- [ ] 已生成 RSA 私钥 (`openssl genrsa -out jwt.pem 4096`)
- [ ] 已生成 internal token (`openssl rand -hex 32`)
- [ ] maintenance window 通告(每阶段约 30-60 分钟)

---

## 5. 阶段 T0:部署 cs-user(无业务影响)

**目标**:cs-user 起来,有自己的 DB,但不接任何业务流量。server 仍走 `local` 模式。

### 5.1 部署独立 PG(cs-user DB)

```bash
helm install pg-cs-user deploy/charts/postgres -n costrict \
  --set persistence.size=10Gi \
  --set existingClaim=""
```

进入 pod 建库建用户:

```bash
kubectl exec -it deploy/pg-cs-user -n costrict -- psql -U postgres <<'SQL'
CREATE DATABASE cs_user;
CREATE USER cs_user WITH ENCRYPTED PASSWORD '<PG_PASSWORD>';
GRANT ALL PRIVILEGES ON DATABASE cs_user TO cs_user;
SQL
```

### 5.2 写 cs-user dev overlay

`cs-user.values.yaml`(明文 dev 路径):

```yaml
image:
  tag: "0.0.93-integration.7"

replicaCount: 1
networkPolicy:
  enabled: false

database:
  host: pg-cs-user.costrict.svc.cluster.local
  password: "<PG_PASSWORD>"

internalToken:
  value: "<INTERNAL_TOKEN>"            # 与 server INTERNAL_SECRET 保持一致

jwt:
  audience: "cs-cloud,app-ai-native,csc"
  signingKey:
    value: |
      -----BEGIN RSA PRIVATE KEY-----
      <jwt.pem 内容>
      -----END RSA PRIVATE KEY-----

eventBus:
  targetUrl: ""                         # T2 阶段再开
```

### 5.3 部署 + 跑 migrate

```bash
helm install cs-user deploy/charts/cs-user -n costrict -f cs-user.values.yaml

# 等 pod ready
kubectl rollout status deploy/cs-user -n costrict

# 跑 schema migrations（pod 内已含 cs-user-migrate 二进制）
kubectl exec deploy/cs-user -n costrict -- /app/cs-user-migrate up
```

### 5.4 拉起首个 tenant(用 bootstrap)

```bash
kubectl exec deploy/cs-user -n costrict -- /app/cs-user-bootstrap tenant-stack \
  --tenant default \
  --tenant-display "Default Tenant" \
  --tenant-edition enterprise \
  --cs-user-url http://localhost:8081 \
  --server-url   http://api.costrict.svc.cluster.local:8080 \
  --gitea-endpoint http://gitea.costrict.svc.cluster.local:3000 \
  --admin-token <GITEA_ADMIN_TOKEN> \
  --dry-run   # 先 dry-run 看请求
```

确认无误后去掉 `--dry-run` 正式跑。

### 5.5 验证

```bash
kubectl exec deploy/cs-user -n costrict -- curl -s localhost:8081/healthz
kubectl exec deploy/cs-user -n costrict -- curl -s \
  -H "X-Internal-Token:<INTERNAL_TOKEN>" \
  localhost:8081/api/internal/platform/tenants/default
```

**T0 完成 cs-user 单独可用,server 还没碰它。**

---

## 6. 阶段 T1:历史数据 ETL(无业务影响)

**目标**:把 server DB 的 `users` + `user_auth_identities` 一次性灌进 cs-user DB。

### 6.1 dry-run

```bash
kubectl exec deploy/cs-user -n costrict -- /app/cs-user-etl \
  --source-dsn "postgres://costrict:<SERVER_PG_PW>@postgres.costrict.svc.cluster.local:5432/costrict?sslmode=disable" \
  --target-dsn "postgres://cs_user:<CS_USER_PG_PW>@pg-cs-user.costrict.svc.cluster.local:5432/cs_user?sslmode=disable" \
  --batch-size 500 \
  --dry-run \
  --report /tmp/etl-report.json
```

读取报告,确认:
- 源表行数与目标表行数差值合理
- 字段级 diff 数量符合预期
- 无 schema 不匹配错误

### 6.2 正式执行

去掉 `--dry-run`:

```bash
kubectl exec deploy/cs-user -n costrict -- /app/cs-user-etl \
  --source-dsn "..." \
  --target-dsn "..." \
  --batch-size 500 \
  --report /tmp/etl-report.json
```

工具特性:**幂等**,重跑只写差异。

### 6.3 对账

```bash
# 行数对账
kubectl exec deploy/pg-cs-user -n costrict -- psql -U cs_user -d cs_user -c \
  "SELECT count(*) FROM users; SELECT count(*) FROM user_auth_identities;"

kubectl exec deploy/postgres -n costrict -- psql -U costrict -d costrict -c \
  "SELECT count(*) FROM users; SELECT count(*) FROM user_auth_identities;"
```

两边数量应该一致(或差值 = T1 开始后新增的 users,因为 server 仍走 local,新数据没同步到 cs-user —— 这是 T2 要解决的问题)。

---

## 7. 阶段 T2:DualWriter 双写过渡(canary)

**目标**:server 同时写自己的 DB(Primary)和 cs-user(Secondary)。cs-user outbox 也开起来,反向同步。

### 7.1 更新 api overlay

```yaml
userService:
  backend: rpc
  url: "http://cs-user.costrict.svc.cluster.local:8081"
  internalToken:
    value: "<INTERNAL_TOKEN>"            # 等于 cs-user internalToken.value

jwtSignMode: "off"                       # T2 仍 server 签 JWT
```

`env` 数组里追加(如果之前没设):

```yaml
env:
  - name: USER_SERVICE_WRITE_MODE
    value: "local"                       # DualWriter:server DB Primary + cs-user Secondary
  - name: INTERNAL_SECRET
    value: "<INTERNAL_TOKEN>"            # 接收 cs-user outbox 投递
```

### 7.2 开 cs-user outbox

`cs-user.values.yaml` 改:

```yaml
eventBus:
  targetUrl: "http://api.costrict.svc.cluster.local:8080/api/internal/users/created"
  targetToken:
    value: "<INTERNAL_TOKEN>"            # 等于 server INTERNAL_SECRET
```

### 7.3 helm upgrade 两边

```bash
helm upgrade cs-user deploy/charts/cs-user -n costrict -f cs-user.values.yaml
helm upgrade api     deploy/charts/api     -n costrict -f api.values.yaml
```

### 7.4 canary 观察(至少 24 小时)

监控指标:

| 指标 | 期望 | 命令 |
|---|---|---|
| cs-user `/healthz` | 持续 200 | `kubectl get pods -l app.kubernetes.io/instance=cs-user` |
| server 双写日志 | 无 "secondary write failed" | `kubectl logs deploy/api \| grep -i dualwriter` |
| outbox 投递成功率 | > 99% | `kubectl logs deploy/cs-user \| grep event_outbox` |
| 两边 user 行数差 | ≤ 1 | 见 6.3 对账命令 |

### 7.5 完整对账(可选,推荐)

写一个对账 job:

```bash
kubectl exec deploy/cs-user -n costrict -- /app/cs-user-etl \
  --source-dsn "..." --target-dsn "..." \
  --dry-run --report /tmp/reconcile.json
```

报告里 `diff_records: 0` 即一致。

---

## 8. 阶段 T3:JWT 签发切换

**目标**:cs-user 开始签 JWT,server 同时接受两边签的 token。

### 8.1 切到 dual

`api.values.yaml`:

```yaml
jwtSignMode: "dual"
```

```bash
helm upgrade api deploy/charts/api -n costrict -f api.values.yaml
```

### 8.2 等所有旧 token 过期

JWT TTL 默认 1 小时。**等待至少 1 小时**(或等于 TTL 的时长),让所有 server-issued token 自然过期。

监控旧 token 命中率:

```bash
kubectl logs deploy/api -n costrict | grep "jwt issuer=server"
```

直到没有 server-issued token 出现(持续一个 TTL 周期)。

---

## 9. 阶段 T4:完全切到 cs-user

**目标**:server 只接受 cs-user 签的 JWT,但仍可写自己 DB(降级备份)。

### 9.1 切 JWT

```yaml
jwtSignMode: "single"
```

### 9.2 (可选)切 WriteMode

如果对 cs-user 稳定运行 1 周以上有信心,可继续:

```yaml
env:
  - name: USER_SERVICE_WRITE_MODE
    value: "readonly"        # server DB 只读,所有写都走 cs-user
```

此时 server user 表变成只读快照,cs-user 是唯一真相源。

---

## 10. 阶段 T5(可选,长期):下线 server user 表

**前置**:T4 稳定运行 ≥ 30 天,无回滚需求。

- 备份 server DB `users` / `user_auth_identities` 表
- 删除 server 代码中对这两个表的 GORM model 引用
- 删除表

> **此阶段不可逆**,务必先备份。一般 SaaS 平台会无限期保留 T4 状态作为降级备份,不执行 T5。

---

## 11. 回滚预案

### 从 T4 回 T3

```yaml
jwtSignMode: "dual"
```

`helm upgrade` 即可。

### 从 T3 回 T2

```yaml
jwtSignMode: "off"
```

### 从 T2 回 T1(关闭双写)

```yaml
userService:
  backend: local    # server 完全回自己 DB
```

cs-user outbox 可保留(`eventBus.targetUrl` 不变),不会造成问题,因为 server 不再消费。

### 从 T1 回 T0

无需操作。ETL 数据保留在 cs-user DB,下次再切时可跳过 T1。

### 从 T0 卸载 cs-user

```bash
helm uninstall cs-user -n costrict
helm uninstall pg-cs-user -n costrict    # 数据丢失!先备份 PVC
```

---

## 12. 故障排查

### cs-user 启动失败

| 错误 | 原因 | 修复 |
|---|---|---|
| `CS_USER_POSTGRES_USER required` | `database.user` 空 | values.yaml 填 user |
| `CS_USER_INTERNAL_TOKEN required` | `internalToken.value` 和 `existingSecret` 都空 | 至少填一个 |
| `mount volume jwt-signing-key failed` | `jwt.signingKey.value` 和 `existingSecret` 都空 | 至少填一个 |
| `pq: database "cs_user" does not exist` | T0 5.1 没建库 | 在 pg pod 里 `CREATE DATABASE cs_user` |

### DualWriter 双写不一致

```bash
kubectl logs deploy/api -n costrict | grep "dualwriter.*secondary.*failed"
```

如果有失败记录,等 cs-user 恢复后跑一次 ETL dry-run,会自动补齐差异。

### JWT 验证 401

确认:
1. cs-user 的 `jwt.signingKey.value` 与 server 期望的公钥匹配(server 通过 JWKS 或直接配置公钥)
2. `jwtSignMode` 不低于当前 token 类型(server 签的 token + `single` 模式 = 401)
3. audience claim 包含 server 期望的 audience

### outbox 不投递

```bash
kubectl exec deploy/cs-user -n costrict -- \
  psql -U cs_user -d cs_user -c "SELECT count(*) FROM event_outbox WHERE delivered_at IS NULL;"
```

如果有积压:
- 看 cs-user pod 日志里 poller 是否启动
- 确认 `eventBus.targetUrl` 不为空
- 确认 `eventBus.targetToken.value` 等于 server `INTERNAL_SECRET`

---

## 13. 附录:env 速查

### cs-user

| env | 来源 | 必填 |
|---|---|---|
| `CS_USER_POSTGRES_*` | `database.*` | 是 |
| `CS_USER_INTERNAL_TOKEN` | `internalToken.value` 或 Secret | 是 |
| `CS_USER_JWT_SIGNING_KEY_PATH` | `jwt.signingKeyPath` | 是 |
| `CS_USER_JWT_ISSUER` | `jwt.issuer` | 是 |
| `CS_USER_JWT_AUDIENCE` | `jwt.audience` | 否(dual 期可空) |
| `CS_USER_APEX_DOMAINS` | `tenant.apexDomains` | 否 |
| `CS_USER_EVENT_TARGET_URL` | `eventBus.targetUrl` | 否(空 = outbox 关) |
| `CS_USER_EVENT_TARGET_TOKEN` | `eventBus.targetToken.value` 或 Secret | 否 |

### server (api)

| env | 来源 | 必填 |
|---|---|---|
| `DATABASE_URL` | `env` 数组 | 是 |
| `INTERNAL_SECRET` | `env` 数组 | T2 起必填 |
| `USER_SERVICE_BACKEND` | `userService.backend` | 默认 local |
| `USER_SERVICE_URL` | `userService.url` | backend=rpc 时必填 |
| `USER_SERVICE_INTERNAL_TOKEN` | `userService.internalToken.value` 或 Secret | backend=rpc 时必填 |
| `USER_SERVICE_WRITE_MODE` | `env` 数组 | T2=local, T4=readonly |
| `JWT_SIGN_MODE` | `jwtSignMode` | off/dual/single |
| `PROFILE_GATE_ENABLED` | `profileGateEnabled` | R0-R6 设计启用时为 true |

---

## 14. 时间预估

| 阶段 | 工作量 | 等待时间 |
|---|---|---|
| T0 | 30 min | — |
| T1 | 1-2 hour(取决于数据量) | — |
| T2 | 1 hour | 24-72 hour canary |
| T3 | 30 min | 1 hour(token TTL) |
| T4 | 30 min | 长期 |
| T5 | 1 hour | — |

**总周期**:典型 3-7 天(主要在 T2 canary 等待)。
