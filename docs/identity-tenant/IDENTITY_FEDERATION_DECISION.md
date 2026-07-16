# 统一身份联邦决策

| 字段 | 内容 |
|---|---|
| 状态 | **Accepted (v3)** · 已决策 |
| 作者 | CoStrict 团队 |
| 创建日期 | 2026-07-06 |
| 决策日期 | 2026-07-07 |
| 评审范围 | server / cs-user / gateway / gitea fork / casdoor / app-ai-native |
| 关联文档 | `CAPABILITY_GIT_REGISTRY_PROPOSAL.md`（V3 架构基线，§9 已同步重写）、`CAPABILITY_PORTAL_DECISION.md`（portal 部署形态）、[`TEAM_ORG_UNIFICATION.md`](./TEAM_ORG_UNIFICATION.md) ADR-3 v3、[`CS_USER_SERVICE_DESIGN.md`](./CS_USER_SERVICE_DESIGN.md) Part VII |
| v2 变更 | 从 "II + 方式 1（RP Auth）" 改为 "II + 方式 3（fork Gitea JWT 中间件）"；新增 username 全生命周期 + 通用 webhook 广播系统 |
| v3 变更（2026-07-15） | 拓扑图修订：cs-user 作为独立服务与 @server 平级；Gitea 协作分工明确——cs-user 持 admin token 做 user-level（开户 + binding），@server 持 admin token 做 team-level + 业务侧（repo CRUD / workflow init / team_user 同步） |

---

## TL;DR

V3 架构下生态内服务众多（device gateway / proxy / wecom-bot-proxy / app-ai-native / capability-portal / cs-user / @server / Gitea），需要选定**统一鉴权端**。Casdoor 当前作为唯一 IdP 存在根本局限：多登录源（GitHub / LDAP / 微信）颁发的 JWT **sub 不统一**，下游无法识别为同一用户。

本文档列出 3 类候选方案（**I: Gitea / II: costrict-web / III: 独立 IdP**），并针对推荐方案 II 细化出 4 种技术实现路径，给出推荐与理由。

> 本文档**只**决策"谁是统一鉴权端 + 多身份合并真相源"，不决策具体 OAuth/OIDC 协议细节。

---

## 1. 背景

### 1.1 现状

```
Casdoor (IdP, 多登录源: LDAP/GitHub/微信/钉钉)
   ├─► cs-user (验 JWKS, 应用层 UserAuthIdentity；JWT 自签)
   │     ├─► device gateway / proxy / wecom-bot-proxy / app-ai-native / capability-portal
   │     ├─► cs-user (持 Gitea admin token 调 Gitea API 做 user-level: 自动开户 + user_gitea_binding 维护)
   │     └─► @server (持 Gitea admin token 调 Gitea API 做 team-level + 业务侧: workflow/KB/capability init + team_user 同步)
   └─► Gitea (配置 OAuth2 Source = Casdoor OIDC) [V3 下用户不直连]
```

> **v3 拓扑修订说明**：原 v2 拓扑把 cs-user 概念内嵌在 costrict-web 单体内，只画了 server 持 PAT。v3 明确 cs-user 是独立服务（与 @server 平级），**两个服务各持 Gitea admin PAT 用于不同职责**——cs-user 仅做 user-level（开户 + binding），@server 做 team-level（team_user 同步）+ 业务侧 repo 操作。详见 [`TEAM_ORG_UNIFICATION.md`](./TEAM_ORG_UNIFICATION.md) ADR-3 v3 + [`CS_USER_SERVICE_DESIGN.md`](./CS_USER_SERVICE_DESIGN.md) Part VII。

### 1.2 核心痛点

Casdoor 多身份源不统一：

| 用户操作 | Casdoor 颁发 JWT sub |
|---|---|
| Alice 用 GitHub 登录 | `github|12345` |
| Alice 用 LDAP 登录 | `ldap|alice` |
| Alice 用微信登录 | `wechat|openid_xxx` |

**Casdoor 内部默认把每个 source 当独立 user**，下游服务（包括 costrict-web / Gitea）拿到 3 个不同 sub，无法识别为同一人。

costrict-web 应用层 `UserAuthIdentity` 表（`server/migrations/20260524000000_create_user_auth_identities_table.sql`）已经做了应用层合并——支持一 user 绑多 identity，但合并**只在 costrict-web 内有效**，Gitea 和其他下游服务感知不到。

### 1.3 目标

选定一个**统一鉴权端**：

1. 多身份合并的真相源在此
2. 所有服务（含 Gitea）信任它
3. 颁发的 token 下游能直接识别用户身份

---

## 2. 候选方案

### 方案 I：Gitea 当统一 OP

**描述**：让 Gitea 作为 OAuth2 Provider，所有服务（含 costrict-web）信任 Gitea。多身份绑定在 Gitea fork 内扩展（UserAuthIdentity 等价能力）。

**优点**：
- 复用 Gitea 成熟 user/org/team 模型
- Gitea 已是 fork，扩展自然

**缺点（致命）**：
- Gitea user 模型为 Git 权限设计，**不是为外部服务设计**——device/runtime/kanban 业务权限塞进 Gitea team 别扭
- Gitea OAuth2 Sources 解决"用 GitHub 登录 Gitea"，**不是"Gitea 给其他服务当 OP"**——JWKS/introspection/revocation 端点弱
- Gitea 多身份 UI 只能"登录时合并"，无"已登录后管理"页面
- **循环依赖**：Gitea 是业务子系统（capability registry），又给所有业务当 OP——Gitea 挂了整个生态挂
- 退出成本极高：一旦 OP 化，迁移成本数量级上升

**结论**：✗ 否决

---

### 方案 II：costrict-web 当统一 OP

**描述**：costrict-web 作为身份联邦 OP，所有服务（含 Gitea）信任它。多身份合并用现有 `UserAuthIdentity` 表。Casdoor 退化为 costrict-web 内部的一个 source（仅承担多登录源）。

**优点**：
- UserAuthIdentity 已实现，多身份合并机制成熟
- 业务层自掌控，无 fork 依赖
- 与 V3 架构契合（costrict-web 本就是核心业务服务）

**缺点**：
- 需要让外部服务（特别是 Gitea）能识别 costrict-web 颁发的身份
- 实施方式有多种选择（见 §3）

**结论**：✓ 进入技术方式细化

---

### 方案 III：独立 IdP（保留 Casdoor 或换 Keycloak/Authentik）

**描述**：保留 IdP 作为纯基础设施，所有服务信任它。多身份绑定在 IdP 内做强化。

**优点**：
- IdP 与业务解耦最干净
- 现有 Casdoor 在此位置

**缺点**：
- Casdoor 自身多身份合并能力弱（这就是当前痛点）
- 替换为 Keycloak 等迁移成本高
- 多身份合并语义不灵活（IdP 层难做复杂业务规则）

**结论**：△ 备选——若方案 II 实施遇阻可回退

---

## 3. 方案 II 技术实现路径（4 种）

选定 costrict-web 当 OP 后，**让 Gitea 识别 costrict-web 身份**有 4 种实现方式。

### 方式 1：Reverse Proxy Authentication（推荐）

**原理**：gateway 验 JWT 后注入 HTTP header，Gitea 信任 header 自动登录。

```
浏览器
  └─► gateway (nginx)
      ├─ 转发 cookie 到 costrict-web /api/auth/resolve
      ├─ server: UserAuthIdentity 合并 → 返回 { user_id, email }
      ├─ 注入 X-Forwarded-User: <user_id>
      ├─ 注入 X-Forwarded-Email: <email>
      └─► Gitea (反代)
          └─ ENABLE_REVERSE_PROXY_AUTHENTICATION=true
              └─ 自动登录/创建对应 user
```

| 维度 | 评估 |
|---|---|
| Gitea 是否 fork | ❌ 原生支持 |
| costrict-web 工作量 | 低（gateway 注入 header + server 加 `/api/auth/resolve` 端点） |
| 安全前提 | Gitea 原始端口必须严格隔离，仅 gateway 可达 |
| V3 契合度 | ★★★★★（V3 下 Gitea 已是"无头"，所有访问天然经 gateway） |

**Gitea 配置**（`app.ini`）：

```ini
[service]
ENABLE_REVERSE_PROXY_AUTHENTICATION = true
ENABLE_REVERSE_PROXY_AUTO_REGISTRATION = true
ENABLE_REVERSE_PROXY_EMAIL = true

[security]
REVERSE_PROXY_AUTHENTICATION_USER = X-Forwarded-User
REVERSE_PROXY_AUTHENTICATION_EMAIL = X-Forwarded-Email
```

### 方式 2：costrict-web 实现标准 OIDC OP

**原理**：costrict-web 暴露标准 OIDC 端点（`/oauth2/authorize` + `/oauth2/token` + `/oauth2/userinfo` + `/.well-known/openid-configuration` + JWKS），Gitea 配置 OAuth2 Source 指向它。

| 维度 | 评估 |
|---|---|
| Gitea 是否 fork | ❌ 原生支持 |
| costrict-web 工作量 | 中（实现 4 个端点 + JWKS + token lifecycle） |
| 安全前提 | OIDC 标准安全模型 |
| V3 契合度 | ★★★（协议重，但跨服务扩展性最好） |

### 方式 3：Gitea fork 加 JWT middleware

**原理**：改 Gitea 源码，加 auth middleware 验 costrict-web JWKS。

| 维度 | 评估 |
|---|---|
| Gitea 是否 fork | ✅ 必须 fork |
| costrict-web 工作量 | 0（暴露 JWKS 即可） |
| 安全前提 | JWT 签名验证 |
| V3 契合度 | ★★（fork 维护成本高，每次 upstream 同步冲突） |

### 方式 4：自定义 OAuth2 Source 插件

**原理**：Gitea 没插件系统，等于方式 3。

**结论**：与方式 3 等价，不单列。

---

## 4. 推荐与对比矩阵

### 4.1 推荐方案

**方案 II + 方式 1（Reverse Proxy Authentication）**

理由：

1. **完美契合 V3**——V3 已把 Gitea 定位"无头"，所有访问天然经 gateway，方式 1 零新机制
2. **多身份合并彻底解决**——gateway 调 costrict-web `/api/auth/resolve`，server 用 UserAuthIdentity 做合并，Gitea 看到的就是合并后的统一 user
3. **零 Gitea fork**——原生配置即支持
4. **零新协议**——不需要实现 OIDC OP 端点
5. **gateway 已有 JWT 验证基础**（验 Casdoor JWKS），扩展成本低

### 4.2 完整对比矩阵

| 维度 | I: Gitea OP | II-1: RP Auth | II-2: OIDC OP | II-3: Fork JWT | III: 独立 IdP |
|---|---|---|---|---|---|
| 多身份合并真相源 | Gitea fork | **costrict-web** | costrict-web | costrict-web | IdP |
| Gitea 是否 fork | ✓（必须） | ❌ | ❌ | ✓（必须） | ❌ |
| Gitea 是否直连 | 是 | **否（必须经 gateway）** | 否 | 否 | 否 |
| 实施工作量 | 高 | **低** | 中 | 中 | 高（替换 IdP） |
| 长期维护成本 | 高 | 低 | 低 | 高 | 中 |
| 跨服务扩展性 | 中 | 高 | **最高** | 中 | 高 |
| 与 V3 契合度 | ★ | ★★★★★ | ★★★ | ★★ | ★★★ |
| 循环依赖风险 | 高 | 无 | 无 | 无 | 无 |

### 4.3 何时应重新评估

- 浏览器**频繁**直连 Gitea（不只运维场景）→ 评估方式 2（OIDC OP）
- 第三方服务（非 costrict-web 系）需要信任同一 OP → 评估方式 2 或方案 III
- Casdoor 被替换为 Keycloak/Authentik → 重新评估方案 III

---

## 5. 推荐方案的实施细节

### 5.1 角色分工（修订后）

| 组件 | 角色 |
|---|---|
| Casdoor | 退化为 costrict-web 内部 source（多登录源：LDAP/微信/钉钉/GitHub） |
| **costrict-web server** | **统一身份联邦 OP**：UserAuthIdentity 合并 + 颁发统一身份（cookie/header 形式） |
| **costrict-web gateway** | JWT 验证 + 调 server `/api/auth/resolve` + 注入 `X-Forwarded-*` header |
| Gitea | 反代模式信任 header，原生配置 `ENABLE_REVERSE_PROXY_AUTHENTICATION=true` |
| 其他服务（device/runtime/...） | 信任 costrict-web JWT（与现状一致） |

### 5.2 链路图

```
┌────────────── 用户 / AI agent ──────────────┐
│                                              │
│  浏览器（多登录源）         AI agent           │
│   │                            │              │
└───┼────────────────────────────┼──────────────┘
    │                            │
    ▼                            ▼
┌──────────────────────────────────────────────┐
│           Casdoor (source only)              │
│   LDAP / GitHub / 微信 / 钉钉 / ...           │
└──────────────┬───────────────────────────────┘
               │ OIDC (原 JWT sub 不统一)
               ▼
┌──────────────────────────────────────────────┐
│       costrict-web server (OP)               │
│   /api/auth/login       (Casdoor OIDC 回调)  │
│   /api/auth/resolve     (UserAuthIdentity 合并)│
│   /api/auth/bind/*      (绑定/解绑 identity)  │
│   颁发统一 cookie/JWT (sub = costrict user_id)│
└──────────────┬───────────────────────────────┘
               │
        ┌──────┴───────┐
        ▼              ▼
┌──────────────┐  ┌─────────────────────────┐
│ 其他服务      │  │ gateway (nginx)         │
│ (device/...) │  │  验 cookie/JWT          │
│ 信任 JWT     │  │  调 /api/auth/resolve   │
└──────────────┘  │  注入 X-Forwarded-*     │
                  └────────┬────────────────┘
                           │
                           ▼
                  ┌─────────────────────────┐
                  │ Gitea (反代模式)         │
                  │  ENABLE_RP_AUTH=true    │
                  │  仅 gateway 可达         │
                  └─────────────────────────┘

(PAT 路径独立)
server ─── PAT (scoped) ───► Gitea API
```

### 5.3 新增端点（costrict-web server）

```http
GET /api/auth/resolve
Cookie: costrict_session=...
→ 200 { "user_id": "...", "email": "...", "primary_identity": "..." }
→ 401 (未登录)
```

### 5.4 gateway 配置（nginx 伪代码）

```nginx
location /gitea/ {
    auth_request /api/auth/resolve;  # 子请求验 cookie
    auth_request_set $cs_user $upstream_http_x_cs_user;
    auth_request_set $cs_email $upstream_http_x_cs_email;

    proxy_set_header X-Forwarded-User $cs_user;
    proxy_set_header X-Forwarded-Email $cs_email;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_pass http://gitea:3000/;
}
```

### 5.5 Gitea 配置（`app.ini`）

```ini
[service]
ENABLE_REVERSE_PROXY_AUTHENTICATION = true
ENABLE_REVERSE_PROXY_AUTO_REGISTRATION = true
ENABLE_REVERSE_PROXY_EMAIL = true
ENABLE_CAPTCHA = false

[security]
REVERSE_PROXY_AUTHENTICATION_USER = X-Forwarded-User
REVERSE_PROXY_AUTHENTICATION_EMAIL = X-Forwarded-Email
INTERNAL_TOKEN = <generated>
SECRET_KEY = <generated>

[oauth2]
# 关闭原生 OAuth2 Source（不再信任 Casdoor）
ENABLE = false
```

### 5.6 网络隔离（关键安全前提）

```yaml
# docker-compose.yml 片段
services:
  gitea:
    networks:
      - internal
    expose:
      - "3000"  # 仅同网络可达
    # 不发布端口，不暴露 :3000 到宿主机

  gateway:
    networks:
      - internal
      - external
    ports:
      - "443:443"

networks:
  internal:
    internal: true  # 不可路由到外网
  external:
```

---

## 6. 风险与缓解

| 风险 | 严重度 | 缓解措施 |
|---|---|---|
| Gitea 原始端口泄露 → header 伪造 = 任意登录 | **高** | docker network `internal: true` + 防火墙规则 + 监控端口扫描 |
| costrict-web server 挂 → gateway 拿不到 resolve → Gitea 登录全挂 | 中 | gateway 缓存 resolve 结果（5 分钟）+ server 高可用部署 |
| UserAuthIdentity 合并冲突（两个 user 绑了同一个 identity） | 中 | 加唯一约束 + 后台合并工具 + 审计日志 |
| header 名冲突（其他中间件注入同名 header） | 低 | 用非标准 header 名（`X-Costrict-User`）+ gateway 严格清洗上游 header |
| PAT 路径与 RP Auth 路径混淆 | 低 | PAT 走 `/api/v1/*` 不经 gateway；RP Auth 走 `/gitea/*` 经 gateway |

---

## 7. 待确认点

| # | 问题 | 推荐 |
|---|---|---|
| 1 | 是否选定方案 II + 方式 1（RP Auth）？ | ✓ 推荐 |
| 2 | gateway 是否已有 JWT 验证逻辑可直接扩展？ | （待用户确认现状） |
| 3 | 是否还有需要浏览器直连 Gitea 的场景（除运维外）？ | 默认仅运维 |
| 4 | Casdoor 多登录源是否保留（作为 costrict-web 内部 source）？ | ✓ 保留 |
| 5 | costrict-web 是否新增 `/api/auth/resolve` 端点？ | ✓ 必需 |
| 6 | Gitea 内 user 自动创建策略（首次访问即创建）是否接受？ | ✓ 接受 |

---

## 8. 决策记录

### 8.1 v1 决策（2026-07-06 草案）

| 决策项 | 内容 |
|---|---|
| 决策状态 | 已被 v2 取代 |
| 选择方案 | II + 方式 1（RP Auth） |
| 决策理由 | 零 Gitea fork、与 V3 架构契合、多身份合并彻底解决 |
| 否决原因 | RP Auth 仅识别 HTTP header（明文），属性同步仅 username；为深度集成与 JWT 一致性，转向方式 3 |

### 8.2 v2 决策（2026-07-07 定稿）

| 决策项 | 内容 |
|---|---|
| 决策状态 | ✅ Accepted |
| 决策人 | 评审组 |
| 决策日期 | 2026-07-07 |
| 选择方案 | **II + 方式 3**：costrict-web 用户中心 + fork Gitea JWT 中间件 |
| 决策理由 | (1) username 主权归 costrict-web，业务字段（业务线/部门/角色/偏好/配额）自由扩展；(2) fork 中间件深度集成 JWT，属性映射可控；(3) 故障域分离（Gitea 故障不影响用户中心）；(4) 与 §13 决策表第 14-18 行对齐 |
| 否决方向 | (a) Gitea 当用户中心 + Gitea HA（HA 不成熟、UI 割裂、故障域合并，详见主提案 §14.6）；(b) RP Auth（属性同步浅，无法支持 username 可改 + 多字段透传） |
| 重新评估触发条件 | Gitea upstream 大版本变化导致 fork rebase 成本 > 1 周；或 costrict-web HA 已成熟且需要 Gitea HA 时 |

### 8.3 v2 完整设计要点

| 维度 | 决策 |
|---|---|
| Casdoor 角色 | 退化为多登录源 UI 提供者（GitHub OAuth / 短信 / LDAP），costrict-web 通过 Casdoor 完成多源登录后**自签 JWT** |
| fork Gitea 范围 | 最小化：`routers/common/auth_jwt.go` 中间件（~200 行）+ auth 链注册（~50 行）= 总计 ~250 行；不动 UI / cron / mirror / webhook 投递 |
| JWT 验证 | RS256，costrict-web 暴露 `/.well-known/jwks.json`；Gitea fork JWKS cache 5min TTL |
| auto-provisioning | 走 Gitea internal `models.CreateUser`（不走 admin API 权限校验，中间件不持 admin token） |
| username 主权 | costrict-web 自管（用户注册填、可改）；变更触发 `user.updated` webhook → sync worker 调 Gitea admin API `PATCH /admin/users/{old}` 改名 |
| 跨服务引用 | 统一使用不可变 `user_id`；username 仅用于显示与 URL |
| commit author 历史 | 接受不改（git immutable），文档说明 |
| webhook 多目标广播 | 通用系统：`webhook_subscriptions` + `webhook_deliveries` 表，6 次指数退避 + 死信队列 + 日全量校对；HMAC 签名 + event_id 幂等 |
| Gitea site-admin token | costrict-web server 端持有（90 天轮换 + 审计日志），用于 user 改名 / team 管理 / 禁用注销 |
| AI bot 账号 | 不经 costrict-web / Casdoor，直接 Gitea 创建 site-level user + PAT（`read:repository` + `write:repository`） |

---

## 9. 与主提案的关系

v2 决策已同步修订到 `CAPABILITY_GIT_REGISTRY_PROPOSAL.md`：

- §9 整体重写：标题改为 "认证集成（costrict-web 用户中心 + fork Gitea JWT 中间件）"，9.1-9.7 全部更新为 v2 方案
- §10.4 新增 5 张表：`user_gitea_binding` / `user_profile` / `webhook_subscriptions` / `webhook_deliveries` / `gitea_admin_audit_log`
- §11 改为 6 阶段，新增 Stage 0（fork Gitea + JWT 中间件 + costrict-web 用户中心，2-3 周）
- §13 决策表新增 5 行（第 14-18 行）：用户中心主权 / fork 范围 / username 主权 / webhook 广播 / content 下发流程
- §14.6 新增已否决方案记录："Gitea 做用户中心 + Gitea HA"
- 附录 C 新增：现有调用链影响与适配（覆盖 costrict-web / cs-cloud / csc 三方调研结论与改造影响）

后续可在主提案 §6 阶段（实施路径 Stage 0）启动时直接引用本文档作为基线。

---

> 本文档与 `CAPABILITY_PORTAL_DECISION.md` 并列，构成 V3 架构的两大决策维度：身份联邦（本文档）+ portal 部署形态（portal 决策文档）。
