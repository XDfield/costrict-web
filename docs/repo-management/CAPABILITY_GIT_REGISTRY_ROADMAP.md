# Capability Git Registry 实施路线图

| 字段 | 内容 |
|---|---|
| 状态 | Active |
| 关联文档 | [`CAPABILITY_GIT_REGISTRY_PROPOSAL.md`](./CAPABILITY_GIT_REGISTRY_PROPOSAL.md)（架构基线）、[`CAPABILITY_PORTAL_DECISION.md`](./CAPABILITY_PORTAL_DECISION.md)（portal 部署决策） |
| 创建日期 | 2026-07-07 |
| 维护原则 | 渐进式演进——每个 Phase 可独立启动、独立验收、独立回滚；Phase 之间允许暂停数周做观察期 |

---

## TL;DR：8 个 Phase 总览

```
Phase 0 ─── 决策闭环（2-3 天）
   ├─ 17 项待决策细节（安全参数 / 部署参数 / 配额默认值 / 产品 UX）
   └─ 跨文档同步（AGENTS.md / openspec / 历史 proposals）
       │
       ▼
Phase 1 ─── Fork Gitea baseline（3-4 周）
   ├─ JWT 中间件（cookie fallback）
   ├─ 全局 pre-receive hook
   ├─ fork 内部 endpoint（quota-cache-invalidate / healthz）
   └─ 每季度 rebase upstream 计划
       │
       ▼
Phase 2 ─── costrict-web 用户中心（2-3 周，与 Phase 1 部分并行）
   ├─ JWT 自签（RS256 + JWKS endpoint）
   ├─ 新表（user_gitea_binding / user_profile / webhook_* / gitea_admin_audit_log / gitea_ext.*）
   ├─ Casdoor 退化为多登录源 UI 提供者
   ├─ 通用 webhook 广播 worker（user.updated / disabled / deleted）
   └─ GiteaConfigSyncWorker
       │
       ▼
Phase 3 ─── 平台配置中心 + 4 org 初始化（1 周）
   ├─ costrict-config/platform-config repo（.gitea/*.yaml + templates）
   ├─ costrict/{costrict-plugins,costrict-mirror} org 创建
   ├─ system webhook 注册
   └─ dept-sync 集成（E4 简化方案）
       │
       ▼
Phase 4 ─── capability-portal 实施（7-10.5 天，可与 Phase 5 并行）
   ├─ packages/capability-portal/ 脚手架
   ├─ marketplace / 详情 / 编辑器页
   ├─ iframe 容器（app-ai-native shell 侧）
   └─ 5 层安全防护落地
       │
       ▼
Phase 5 ─── 数据迁移 + 双通道运行（3-5 周）
   ├─ 现有 capability_items 分类（standalone / pack / mirror / seed）
   ├─ 迁移脚本（mirror pull / pack push / standalone push / seed push）
   ├─ sync worker + capability-check worker 上线
   └─ V2 ingest-upstream **冻结**（不主动跑，仅应急 fallback）
       │
       ▼
Phase 6 ─── V3 稳定运行观察期（≥ 2 周）
   ├─ 监控 P99 延迟 / webhook 失败率 / mirror pull 成功率
   ├─ V3 可靠性验收（无 P0 故障持续 2 周）
   └─ 用户反馈收集（portal UX / sync 延迟感受）
       │
       ▼
Phase 7 ─── 下线 V2 通道（1 周）
   ├─ 删除 CatalogIngestService / migrate ingest-upstream 命令
   ├─ 删除冗余字段（SourceSHA / CatalogEntryDir / SourceType / CurrentRevision）
   ├─ 删除 security_scans.item_revision
   └─ 文档更新（CATALOG_INGEST.md → CAPABILITY_GIT_SYNC.md）
       │
       ▼
Phase 8 ─── 后续优化（持续，无终点）
   ├─ 联邦（多实例间能力项同步）
   ├─ 能力项依赖图谱
   ├─ AI agent 自动 PR
   └─ Plugin marketplace 完全收敛进 V3
```

**总工时估算**：8-12 周（小团队 2-3 人），关键路径在 Phase 1 + 2。

---

## Phase 0：决策闭环

> **目标**：在写第一行代码前，把所有悬而未决的实施细节决策做完。**无代码改动**，纯文档 / 决策记录。

### Phase 0.1：安全参数（P0）

| # | 决策项 | 选项 | 默认推荐 | 影响 |
|---|---|---|---|---|
| 0.1.1 | JWT 私钥存储 | K8s Secret / Vault / 文件 + chmod 600 | K8s Secret（小团队运维成本最低） | fork Gitea 镜像配置 |
| 0.1.2 | JWKS endpoint 域名 | 主域 `costrict-web/.well-known/jwks.json` / 独立子域 `auth.costrict.local` | 主域（cookie domain 最简） | cookie SameSite 配置 |
| 0.1.3 | JWT TTL & cookie 属性 | access token TTL（1h / 8h / 24h）+ refresh 机制 + SameSite（Lax / None+Secure） | 1h TTL + refresh + SameSite=Lax（同域够用） | 用户登录体验 |
| 0.1.4 | admin PAT 轮换 grace period | 90 天轮换期间新旧 token 并存多久（5min / 1h / 24h） | 24h（覆盖跨时区 in-flight） | BotTokenRotationWorker 实现 |

### Phase 0.2：部署 / 运维参数（P0）

| # | 决策项 | 选项 | 默认推荐 | 影响 |
|---|---|---|---|---|
| 0.2.1 | Gitea fork 锁定基线 | 1.21.x / 1.22.x / 1.23.x | 最新稳定 LTS（撰写时 1.23.x）+ 季度 rebase | fork 维护成本 |
| 0.2.2 | PostgreSQL 隔离方式 | 同 instance 不同 schema / 不同 instance | 同 instance 不同 schema（gitea / gitea_ext / costrict_web 三 schema） | DBA 运维 |
| 0.2.3 | costrict-web / Gitea fork 副本数 | cw N 副本 / Gitea 单实例 | cw 2 副本（HA）+ Gitea 单实例（v3 不做 HA） | K8s Deployment 配置 |
| 0.2.4 | 审计日志保留期 | 30 天 / 90 天 / 1 年 / 永久 | `gitea_admin_audit_log` 1 年 / `webhook_deliveries` 90 天 / 死信队列永久 | 存储成本 / 合规 |
| 0.2.5 | 监控 SLI/SLO 阈值 | sync P99 / webhook 失败率 / mirror 成功率 | P99 < 30s / 失败率 < 0.5% / mirror > 95% | 告警通道（Slack? 邮件?） |

### Phase 0.3：配额与默认值（P0）

| # | 决策项 | 选项 | 默认推荐 | 影响 |
|---|---|---|---|---|
| 0.3.1 | 单文件大小默认上限 | 5MB / 10MB / 50MB | **统一 5MB**（POC 简化：不按 capability_type 差异化，仅作量级防护；类型锁定与限制隔离由 costrict-web 应用层维护，见 §4.6） | fork pre-receive hook 逻辑 |
| 0.3.2 | owner 默认配额 | 50MB / 100MB / 500MB | `costrict` 50MB / `costrict-plugins` 500MB / `costrict-mirror` 2GB / `costrict-config` 64MB / `u-*` 100MB | quota.yaml 内容 |
| 0.3.3 | mirror pull 频率 | 全部 24h / hot mirror 1h | 默认 24h，白名单 hot mirror 1h（如 awesome-claude-skills） | Gitea mirror 配置 |

### Phase 0.4：产品 / UX 细节（P1，可在 Phase 4-5 期间解决）

| # | 决策项 | 选项 | 默认推荐 | 影响 |
|---|---|---|---|---|
| 0.4.1 | csc 兼容策略 | csc 同时支持 v2 + v3 / 强制升级 | v3 上线后 csc 强制升级（v2 通道下线前给 4 周迁移窗口） | csc 发版计划 |
| 0.4.2 | app-ai-native `/store/*` 与 portal 关系 | portal 完全接管 / 部分接管 | portal 接管 marketplace + detail + editor + PR 审核；favorite list / install history 留 shell | iframe 路由设计 |
| 0.4.3 | capability-check 启发式优先级 | 多类型命中时按什么顺序判定 | skill > subagent > command > mcp > plugin（按 metadata 文件明确性排序） | capability-check worker 逻辑 |
| 0.4.4 | favorite / install 状态 SSR 体验 | 骨架屏 / 默认 0 / 占位符 | 骨架屏（与现有 store 卡片体验一致） | portal 卡片组件 |
| 0.4.5 | plugin marketplace 对接 | 独立项目保留 / 收敛进 V3 | V3 后 `costrict-plugin-marketplace` 项目继续作为 build pipeline，产出 pack 推到 `costrict-plugins/` | AGENTS.md 更新 |

### Phase 0.5：跨文档同步（P0）

| # | 任务 | 文件 | 状态 |
|---|---|---|---|
| 0.5.1 | 更新 AGENTS.md：marketplace 描述对齐 V3（pack 平级原则） | `costrict-web/AGENTS.md` | ⏳ |
| 0.5.2 | 更新 AGENTS.md：加 V3 关键文档索引（PROPOSAL / PORTAL_DECISION / ROADMAP） | `costrict-web/AGENTS.md` | ⏳ |
| 0.5.3 | 检查 openspec changes（`add-plugin-marketplace` / `add-plugin-display-only` / `add-plugin-capability-type`）是否需要 V3 对齐 | `costrict-web/openspec/changes/` | ⏳ |
| 0.5.4 | 历史 proposals（`USER_TABLE_DESIGN.md` / `MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md` / `IDENTITY_FEDERATION_DECISION.md`）交叉引用补全 | `costrict-web/docs/proposals/` | ⏳ |

### Phase 0 验收标准

- [ ] 17 项决策（0.1-0.4）全部写入 `CAPABILITY_GIT_REGISTRY_PROPOSAL.md` 对应章节
- [ ] 跨文档同步（0.5）全部完成
- [ ] 用户（决策人）签字确认所有决策

---

## Phase 1：Fork Gitea baseline（3-4 周）

> **目标**：建立可维护的 Gitea fork，加 JWT 中间件 + 全局 pre-receive hook，与 upstream 保持低冲突 rebase 节奏。
>
> **启动条件**：Phase 0 完成。
>
> **可并行**：与 Phase 2 部分并行（Phase 2 的 costrict-web JWT 自签可独立开发）。

### 任务清单

#### 1.1 Fork 与基线选择（1 天）

- [ ] 基于 Phase 0.2.1 决策的版本 fork Gitea（仓库 `costrict-plugins-repo/gitea` 维护）
- [ ] 建立 fork 维护文档：rebase 流程 / 改动清单 / 冲突解决记录
- [ ] CI：fork 镜像自动构建（tag = `costrict-<gitea-version>-<fork-version>`）

#### 1.2 JWT 中间件（~250 行，1.5-2 周）

- [ ] 新增 `routers/common/auth_jwt.go`（~200 行）：
  - JWKS cache（5min TTL，从 `https://costrict-web/.well-known/jwks.json` 拉）
  - RS256 验证 + clock skew ±60s
  - **JWT 来源双通道 fallback**（§9.6）：
    - 优先 `Authorization: Bearer <jwt>` 头
    - fallback HttpOnly cookie `costrict_jwt`（domain=`.costrict.local`，+20 行）
  - **binding 状态校验**（eager 模式）：查 `user_gitea_binding.sync_status`，非 `synced` 返回 503 + `Retry-After: 5`；用户创建由 sync worker 在 `user.created` 时调 `POST /admin/users` 完成（中间件不持 admin token、不调 internal `models.CreateUser`）
- [ ] 修改 `routers/common/auth.go` + `routers/web/routes.go`（~50 行）：中间件链注册
- [ ] 失败降级：JWKS 拉不到 → 用 5min 前旧 key；JWT 无效 → 401；binding 非 synced → 503
- [ ] 单测：JWT 解析 / JWKS cache 命中率 / binding 状态校验 + 503 fallback / cookie fallback

#### 1.3 全局 pre-receive hook（~150 行，1 周）

- [ ] 修改 `modules/gitrepo/hooks.go` `CreateDelegateHooks`（~20 行）：加入系统级 pre-receive hook 路径 fallback
- [ ] 新增 `modules/git/preceive_global.go`（~80 行）：
  - 单文件大小限制（**统一默认值，不按 capability_type 差异化**，Phase 0.3.1 简化决策）
  - repo 总大小配额（owner 默认 + per-repo 覆盖）
  - commit message 不检查
- [ ] 新增 `modules/setting/quota.go`（~20 行）：加载 app.ini 中的 quota 默认配置
- [ ] 新增 `routers/internal/quota.go`（~30 行）：
  - `POST /api/internal/quota-cache-invalidate` endpoint
  - `GET /api/internal/healthz` endpoint
- [ ] PostgreSQL connection pool 直连共享 DB 的 `gitea_ext.quota_rules` 表（5min TTL cache）

#### 1.4 集成测试与灰度部署（3-5 天）

- [ ] 集成测试：
  - username 变更后 Gitea username 同步生效
  - push 大文件被拒（错误消息双语 + 标准化错误码）
  - 配额超额被拒
  - JWKS 失效降级
  - registry-seed yaml 变更触发 batch sync（与 Phase 2.4 GiteaConfigSyncWorker 联调）
- [ ] 灰度环境部署：fork Gitea 镜像替换官方镜像

#### 1.5 维护计划文档（持续）

- [ ] 每季度 Gitea upstream release 跟进 rebase 演练
- [ ] fork 改动清单维护（限定在认证 + hook 模块，~400 行）

### Phase 1 验收标准

- [ ] fork Gitea 镜像通过所有 CI 测试
- [ ] 集成测试全绿
- [ ] 灰度环境运行 1 周无 P0 故障
- [ ] fork 维护文档完备（rebase 流程演练过一次）

### Phase 1 风险点

| 风险 | 缓解 |
|---|---|
| Gitea upstream 大版本升级冲突（如 1.24 重构 auth 链） | Phase 0.2.1 锁定 LTS；fork 改动限定在认证 + hook |
| JWKS endpoint 不可达导致用户登录失败 | 5min JWKS cache 降级 + costrict-web 多副本 HA |
| pre-receive hook 性能瓶颈（大 push 阻塞） | DB query 加 cache；hook 同步超时 1s 兜底放行 + 异步告警 |

---

## Phase 2：costrict-web 用户中心（2-3 周）

> **目标**：costrict-web 自签 JWT，Casdoor 退化为多登录源 UI 提供者，建立用户生命周期 webhook 广播体系。
>
> **启动条件**：Phase 0 完成；与 Phase 1 部分并行。
>
> **依赖**：Phase 1.2 JWT 中间件（联调需要）。

### 任务清单

#### 2.1 JWT 自签（3 天）

- [ ] 实现 RS256 私钥加载（Phase 0.1.1 决策的存储方式）
- [ ] 实现 `SignJWT(user)` 函数（claims 含 user_id / preferred_username / email / groups）
- [ ] 暴露 `/.well-known/jwks.json` endpoint
- [ ] TTL 与 refresh 实现（Phase 0.1.3）

#### 2.2 数据库 schema（2 天）

按 §10.4 创建表：

- [ ] `user_gitea_binding(user_id, gitea_uid, gitea_username, sync_status, ...)`
- [ ] `user_profile(user_id, business_line_id, dept_id, role, preferences, quota, ...)`
- [ ] `webhook_subscriptions(id, subscriber_name, target_url, event_types, secret, ...)`
- [ ] `webhook_deliveries(id, event_id, event_type, subscription_id, payload, status, ...)`
- [ ] `gitea_admin_audit_log(id, actor_user_id, endpoint, payload_hash, request_id, ...)`
- [ ] `gitea_ext.quota_rules(owner, repo, max_file_size_mb, repo_quota_mb, ...)`
- [ ] `gitea_ext.config_versions(config_type, version, updated_at)`
- [ ] 索引按 §10.3 创建

#### 2.3 Casdoor 退化（1 天）

- [ ] 保留现有 `UserAuthIdentity` 表扩展支持 Casdoor 各 provider（github/sms/ldap，§9.3 typo 已修）
- [ ] costrict-web 登录链路改为：Casdoor 处理多源登录 UI → 回调 costrict-web → **costrict-web 自签 JWT**
- [ ] 不再依赖 Casdoor JWT

#### 2.4 通用 webhook 广播 worker（3 天）

- [ ] 监听 `user.updated` / `user.disabled` / `user.deleted` 事件
- [ ] 6 次指数退避（1s / 5s / 30s / 2min / 10min / 1h）
- [ ] 死信队列 + admin UI
- [ ] HMAC-SHA256 签名 + `event_id` 幂等
- [ ] 日 cron 全量校对兜底

#### 2.5 用户生命周期 sync worker（3 天）

- [ ] 监听 `user.updated` → 调 Gitea admin API `PATCH /admin/users/{old}` 改 username
- [ ] 监听 `user.disabled` / `user.deleted` → 调 Gitea admin API 移除 collaborator + 禁用账号
- [ ] 用户注销时 repo ownership 转移给 `costrict-system`（§9.7.4）
- [ ] 监听 `user.organization_changed`（来自 §3.5 dept-sync webhook，**不**依赖 `user_gitea_team_binding` 表）→ `OrgGiteaSyncWorker`：
  - 加入 org X → 调 Gitea admin team API 把用户加入 X 对应的 team（fire-and-forget）
  - 离开 org X → 调 Gitea admin team API 移除 team 成员
  - 失败重试**复用 §2.4 通用 webhook 广播 worker 的 6 次指数退避队列**（1s/5s/30s/2min/10min/1h）+ 死信队列
  - 每次调用写 `gitea_admin_audit_log`（actor=系统，endpoint=`/admin/teams/{id}/members`）
  - **不持久化 sync_status**：fork 中间件不校验 team 成员关系（Gitea 自己查 `team_user` 表），失败由重试 + 审计兜底

#### 2.6 GiteaConfigSyncWorker（3 天）

- [ ] 监听 `costrict-config/platform-config` push webhook
- [ ] diff `.gitea/*.yaml` 变更
- [ ] 分别调 Gitea API 应用（branch protection / teams / webhooks / labels）
- [ ] 写 `gitea_ext.quota_rules`
- [ ] 调 fork Gitea 内部 endpoint `POST /api/internal/quota-cache-invalidate` 失效 cache

#### 2.7 BotTokenRotationWorker（2 天）

- [ ] admin PAT 90 天自动轮换
- [ ] grace period（Phase 0.1.4）期间新旧 token 并存
- [ ] 写 secret store → webhook 通知下游 reload → 老 PAT 自然过期
- [ ] admin UI 一键 revoke（应急）

### Phase 2 验收标准

- [ ] 用户登录链路：Casdoor → costrict-web JWT + `user.created` webhook → sync worker 调 `POST /admin/users` 创建 Gitea → `user_gitea_binding.sync_status='synced'` 后 fork 中间件放行（完整跑通）
- [ ] 存量用户一次性 migration：扫描无 binding 的活跃用户分批补建（限速 10 QPS）
- [ ] username 改名 → Gitea username 同步（webhook 6 次重试覆盖）
- [ ] 用户注销 → repo ownership 转移给 `costrict-system`
- [ ] GiteaConfigSyncWorker：改 `quota.yaml` 后 1min 内 fork hook cache 失效
- [ ] admin PAT 轮换演练通过

### Phase 2 风险点

| 风险 | 缓解 |
|---|---|
| webhook 投递不可达（订阅方宕机） | 6 次退避 + 死信队列 + 日 cron 校对 |
| GiteaConfigSyncWorker 与 fork hook cache 不同步 | quota-cache-invalidate endpoint 失败时 sync worker 重试 + 5min 自然过期兜底 |
| Casdoor 退化期间双 JWT（旧 Casdoor + 新 costrict-web）冲突 | 灰度切换：先双 JWT 共存期 1 周，再下线 Casdoor JWT |

---

## Phase 3：平台配置中心 + 4 org 初始化（1 周）

> **目标**：建立 V3 平台的 4 个固定 org + 平台配置中心 repo + system webhook。
>
> **启动条件**：Phase 1 + Phase 2 完成。

### 任务清单

#### 3.1 4 个固定 org 创建（1 天）

- [ ] `costrict-config/`（配置中心 org）
- [ ] `costrict/`（官方能力项 org）
- [ ] `costrict-plugins/`（pack org）
- [ ] `costrict-mirror/`（mirror org）
- [ ] 用户 namespace `u-<username>/` 由 sync worker 在 `user.created` 时调 `POST /admin/users` 创建（eager 模式）；存量用户由启动时一次性 migration 补建

#### 3.2 `costrict-config/platform-config` 初始化（1 天）

按 §9.9 + Phase 0.3 决策创建：

- [ ] `.gitea/branch-protection.yaml`（默认 + overrides，§9.9.4）
- [ ] `.gitea/quota.yaml`（owner 默认 + per-repo 覆盖，§9.9.3）
- [ ] `.gitea/teams.yaml` / `.gitea/webhooks.yaml` / `.gitea/labels.yaml`
- [ ] `ISSUE_TEMPLATE/*.md` / `PULL_REQUEST_TEMPLATE.md`
- [ ] `README.md`（每个 yaml 的 schema + 编辑流程）

#### 3.3 `costrict/curated-seed` 初始化（半天）

- [ ] 留空 README + 目录骨架（skills/ commands/ subagents/）
- [ ] 后续填充精选能力项（不阻塞 Phase 3 完成）

#### 3.4 system webhook 注册（半天）

- [ ] 在 Gitea admin 面板配置 system-level webhook
- [ ] 事件类型：`push` + `pull_request`
- [ ] 目标：`POST /api/internal/git-sync` 和 `POST /api/internal/capability-check`（含 HMAC 签名校验）

#### 3.5 dept-sync 集成（E4 简化方案，2 天）

- [ ] dept-sync 服务加 webhook 推送接口（`dept.updated` / `dept.member_changed`）
- [ ] costrict-web `DeptSyncCacheWorker`：接收 webhook → 写 Redis（5min TTL）
- [ ] `/admin/departments/tree` API 改为读 Redis cache（dept-sync 故障时降级）
- [ ] private repo admin 邀请 UI（`/admin/private-repos` 页面）
- [ ] 离职清理 worker（监听 `user.disabled/deleted` → 移除 collaborator + 禁用 Gitea 账号）

### Phase 3 验收标准

- [ ] 4 org 创建完成，权限配置正确
- [ ] platform-config 内容 PR merge 后 1min 内 GiteaConfigSyncWorker 应用变更
- [ ] system webhook 投递成功率 > 99%
- [ ] dept-sync webhook → Redis cache 链路通

---

## Phase 4：capability-portal 实施（7-10.5 天）

> **目标**：实施 portal scheme A（monorepo 子包 + Vue 3 + iframe + 直连 Gitea API）。
>
> **启动条件**：Phase 0.4 决策完成；与 Phase 5 可并行。
>
> **详见**：`CAPABILITY_PORTAL_DECISION.md`。

### 任务清单

#### 4.1 脚手架（1-1.5 天）

- [ ] `packages/capability-portal/` 目录
- [ ] Vue 3 + Vite + TS + Tailwind 配置
- [ ] 复用 opencode monorepo 的 tsconfig / eslint / prettier
- [ ] nginx 反代配置：`/portal/*` + `/gitea/api/v1/*`

#### 4.2 marketplace 页（2-3 天）

- [ ] 卡片网格 + 分类筛选 + 搜索
- [ ] 调 costrict-web REST：`GET /api/capabilities`（仅业务字段）
- [ ] JWT cookie 自动带

#### 4.3 详情页（1.5-2 天）

- [ ] 调 `GET /api/capabilities/{slug}`（业务字段）
- [ ] 直连 `GET /gitea/api/v1/repos/.../contents/...`（content）
- [ ] markdown 渲染（markdown-it + highlight.js）

#### 4.4 编辑器页（1.5-2 天）

- [ ] CodeMirror 6 集成
- [ ] 直连 `POST /gitea/api/v1/repos/.../contents/...` 直推 main（branch protection 已放开）
- [ ] commit message 模板

#### 4.5 iframe 容器（0.5-1 天，app-ai-native 侧）

- [ ] SolidJS shell 写 iframe 容器组件（参考 `multica-page.tsx:1-163`）
- [ ] sandbox 策略（`allow-scripts allow-same-origin allow-forms`）
- [ ] ref 通信 + loading 状态

#### 4.6 5 层安全防护（0.5-1 天）

- [ ] 同域（nginx 反代到 `.costrict.local`）
- [ ] iframe sandbox（细粒度 allow 列表）
- [ ] JWT cookie（HttpOnly + SameSite=Lax）
- [ ] Gitea API 路径白名单（fork 中间件只放行 `/gitea/api/v1/*`）
- [ ] URL 防护（portal router 全部走 hash/history，无 redirect 跳 Gitea 原生页）

### Phase 4 验收标准

- [ ] marketplace / 详情 / 编辑器页跑通
- [ ] 直推 main 流程通（cookie 自动带，无 PAT 申请）
- [ ] iframe 嵌入无 X-Frame-Options 报错
- [ ] 5 层安全防护审计通过

---

## Phase 5：数据迁移 + 双通道运行（3-5 周）

> **目标**：把现有 `capability_items` 数据迁移到 V3 Gitea 仓库，启动 sync worker + capability-check worker；V2 通道冻结。
>
> **启动条件**：Phase 1-4 完成。

### 任务清单

#### 5.1 现有 capability_items 分类（2-3 天）

- [ ] 上游 GitHub 来源 → 标记 `mirror` kind，建立 mirror repo（Gitea 配置 mirror pull）
- [ ] plugin pack 来源 → 标记 `pack` kind，建立 pack repo（`costrict-plugins/<pack>`）
- [ ] 用户独立创建 → 标记 `standalone` kind，建立 standalone repo（`costrict/<slug>` 或 `u-<username>/<slug>`）
- [ ] 官方精选 → 标记 `seed` kind（可选，建立 `costrict/curated-seed/<type>/<slug>/`）

#### 5.2 迁移脚本（1 周）

按 kind 分别写脚本：

- [ ] **mirror**：建立空 repo → 配置 Gitea mirror pull → 等首次同步
- [ ] **pack**：从 DB 导出 → 生成 `plugins/<id>/.plugin.json` → 一次性 push
- [ ] **standalone**：从 DB 导出每个 item → 创建独立 repo → push
- [ ] **seed（可选）**：从 DB 导出 → 生成 mono-repo 目录树 → push

#### 5.3 DB 字段扩展（1 天）

- [ ] `capability_items` 加新字段（`source_repo_url` / `source_repo_path` / `source_repo_ref` / `source_repo_kind` / `capability_type` / `git_sha` / `git_last_synced_at` / `git_author_email` / `mirror_of` / `identification_status` / `health_issues` / `last_checked_at` / `last_clean_at`）
- [ ] `capability_registries` 加新字段（`kind` / `git_remote_url` / `git_default_branch` / `last_synced_commit` / `last_synced_at` / `mirror_of` / `gitea_visibility`）
- [ ] 写回 baseline 值（已迁移的 repo URL 等）
- [ ] 索引按 §10.3 创建

#### 5.4 sync worker（1 周）

- [ ] 实现新 endpoint：`POST /api/internal/git-sync`（接受 Gitea push webhook）
- [ ] sync worker：webhook → compare → raw → 解析 frontmatter → upsert `capability_items`
- [ ] 按 kind 分发（standalone / pack / mirror / seed 不同 handler）
- [ ] 幂等：Redis SETNX + 24h TTL
- [ ] webhook 风暴防护：Redis debounce 1-2 秒

#### 5.5 capability-check worker（1 周）

- [ ] 实现新 endpoint：`POST /api/internal/capability-check`（接受 Gitea PR webhook）
- [ ] worker：
  - 触发事件：`pull_request`（opened/synchronize/reopened）+ `push`（直推 main post-merge）+ mirror pull push
  - 拉 PR head / commit 文件树 → 启发式识别（§4.5 规则）+ schema 校验
  - 写 `capability_items.health_issues` + `identification_status`
  - PR 评论 health summary（不阻断 merge）
- [ ] 健康度状态机（§8.4）

#### 5.6 安全扫描迁移（1 周）

按 §8.5：

- [ ] `security_scans` 表加 `git_sha` + `trigger_type` + `pr_number` 字段（双写兼容期）
- [ ] `scan_job_service.Enqueue` 改造：短路键 `CurrentRevision` → `git_sha`；签名扩展加 `git_sha` + `trigger_type` + `pr_number` 入参
- [ ] scan 调用点迁移：`catalog_ingest_service.go:1024,1130` + `sync_service.go:421,491` → 新 sync worker
- [ ] PR 触发分支：`trigger_type=pr-check`，不覆盖主表
- [ ] post-merge 触发：`trigger_type=git-push`，覆盖 `capability_items.security_status`
- [ ] Plugin 跳过逻辑保留（`scan_service.go:252-254` 不动）
- [ ] manifest 透传：`health.identification` + `health.security` 双字段

#### 5.7 V2 通道冻结（半天）

- [ ] **冻结** `migrate ingest-upstream` 命令（代码不删，admin 不主动跑）
- [ ] **冻结** `services.CatalogIngestService`（同上）
- [ ] 通知所有 admin：V3 sync worker 上线后，V2 仅作应急 fallback
- [ ] 监控：V3 sync worker P99 延迟 / 失败率 / mirror pull 成功率

### Phase 5 验收标准

- [ ] 所有现有 capability_items 都有对应的 Gitea repo + 正确 kind 标记
- [ ] sync worker 处理 push webhook P99 < 30s
- [ ] capability-check worker 健康度状态机正确触发
- [ ] 安全扫描 PR 扫描结果正确写入 `security_scans` 表
- [ ] V2 通道冻结期间未触发过（V3 自身足够可靠）

### Phase 5 灰度策略

- 先开放**只读**：所有 repo 变更 → DB sync（V2 应急 fallback 保留但冻结）
- 再开放**写入**：用户/AI 默认直推 main（§9.8.2）；可选走 PR（§8.1.1）；REST API `/api/items POST/PUT/DELETE` 逐步下线
- 业务字段（favorite / install / security）API 保留

---

## Phase 6：V3 稳定运行观察期（≥ 2 周）

> **目标**：观察 V3 在生产环境的可靠性，确认无 P0 故障，收集用户反馈。
>
> **启动条件**：Phase 5 完成。
>
> **无代码改动**（除紧急 hotfix）。

### 任务清单

#### 6.1 监控仪表盘（1 天）

- [ ] sync 延迟 P99 / P95
- [ ] webhook 失败率
- [ ] PR 待审时长
- [ ] DB 行数趋势
- [ ] mirror pull 成功率
- [ ] fork Gitea pre-receive hook 拒绝率
- [ ] GiteaConfigSyncWorker 应用延迟

#### 6.2 告警配置（半天）

- [ ] sync P99 > 30s 持续 5min → 告警
- [ ] webhook 失败率 > 1% 持续 5min → 告警
- [ ] mirror pull 失败率 > 5% 持续 1h → 告警
- [ ] 告警通道（Phase 0.2.5 决策）

#### 6.3 用户反馈收集（持续 2 周）

- [ ] portal UX 反馈渠道（Slack 频道 / issue tracker）
- [ ] sync 延迟感受反馈
- [ ] AI agent 操作体验反馈

#### 6.4 V3 可靠性验收（持续 2 周）

- [ ] 无 P0 故障持续 2 周
- [ ] V2 通道未被触发过（V3 自身足够可靠）
- [ ] sync worker 覆盖率 = 100%（无 item 遗漏）

### Phase 6 验收标准

- [ ] 监控仪表盘 + 告警配置完成
- [ ] 持续 2 周无 P0 故障
- [ ] 用户反馈 NPS ≥ 7/10

### Phase 6 回滚条件

如果出现以下任一情况，回滚到 Phase 5 灰度策略（重新冻结 V2 通道）：

- V3 sync worker 大规模故障（>10% item 同步失败）
- fork Gitea 出现 P0 故障（用户无法 push / clone）
- 数据丢失（V3 sync 漏处理某 item）

---

## Phase 7：下线 V2 通道与清理（1 周）

> **目标**：完全下线 V2 catalog ingest 链路，删除冗余字段，文档更新。
>
> **启动条件**：Phase 6 验收通过。

### 任务清单

#### 7.1 下线 V2 命令与服务（1 天）

- [ ] 下线 `migrate ingest-upstream` 命令（删除 `cmd/migrate/main.go` 中的子命令）
- [ ] 下线 `services.CatalogIngestService`（删除整个 service 文件）
- [ ] 删除 `catalog_ingest_service.go:1024,1130` + `sync_service.go:421,491` 内的 scan Enqueue 调用
- [ ] 删除 `scripts/run-ingest.sh`（运维脚本）
- [ ] 删除 admin import bundle UI（`ADMIN_BUNDLE_IMPORT_DESIGN.md` 描述的链路）

#### 7.2 字段清理（半天）

- [ ] 删除 `security_scans.item_revision` 字段（迁移完成，统一用 `git_sha`）
- [ ] 删除 `CapabilityItem.SourceSHA` / `CatalogEntryDir` / `SourceType` / `CurrentRevision` 等冗余字段
- [ ] DB migration 脚本 + 回滚脚本

#### 7.3 文档更新（半天）

- [ ] `CATALOG_INGEST.md` → `CAPABILITY_GIT_SYNC.md`（重写）
- [ ] `ADMIN_BUNDLE_IMPORT_DESIGN.md` 标记 deprecated（保留历史归档）
- [ ] `AGENTS.md` 更新（删除 V2 ingest 相关描述，加 V3 链路说明）
- [ ] `SCAN_SKILL.md` 更新触发源章节（catalog_ingest → git-sync worker）

#### 7.4 监控仪表盘扩展（半天）

- [ ] 加 V3 专属指标：sync 延迟 / webhook 失败率 / PR 待审时长 / DB 行数趋势
- [ ] 删除 V2 专属指标（catalog ingest 频率 / bundle 大小 等）

### Phase 7 验收标准

- [ ] `migrate ingest-upstream` 命令完全删除
- [ ] `services.CatalogIngestService` 文件完全删除
- [ ] 冗余字段删除后所有查询不报错
- [ ] 文档全部更新到 V3 口径

---

## Phase 8：后续优化（持续，无终点）

> **目标**：V3 baseline 稳定后的演进方向。**不阻塞** Phase 7 验收。

### 8.1 已规划（详见 §15 后续工作）

- 跨企业能力项联邦（多 Gitea / server 实例间的 federation）
- 能力项依赖图谱（基于 Git submodule 或 manifest）
- AI agent 自动 PR（agent 主动起草能力项）
- Gitea Actions 替代部分 server worker
- 跨 repo 批量操作工具（如"一次性给所有 skill 加 frontmatter 字段"）

### 8.2 触发条件

| 优化项 | 触发条件 |
|---|---|
| 联邦 | 多实例部署需求出现（如客户企业内部独立部署 + 与官方同步） |
| 依赖图谱 | 用户反馈"想知道这个 skill 依赖哪些 command / mcp" |
| AI 自动 PR | AI agent 成熟度足够（自主起草 + 自主审核） |
| Gitea Actions | 当前 sync worker / capability-check worker 性能不足 |

### 8.3 Plugin marketplace 完全收敛

V3 baseline 稳定后，`costrict-plugin-marketplace` 项目（V2 时代的 770+ bare repo + build pipeline + GitHub release + import.sh）逐步收敛：

- build pipeline 保留（生产侧）
- 产出的 pack 推到 `costrict-plugins/<pack>`
- 客户端 `import.sh` 下线（改为 csc `git clone` Gitea pack）
- GitHub release 渠道下线（改为 Gitea 内部消费）

具体路线图待 V3 稳定后单独立项。

---

## 全局风险与对策

| 风险 | 影响范围 | 缓解 |
|---|---|---|
| Gitea fork 升级冲突 | Phase 1 / 8 | Phase 0.2.1 锁定 LTS；fork 改动限定 ~400 行；季度 rebase 演练 |
| JWT 中间件性能瓶颈 | Phase 1 / 2 | JWKS cache hit < 5ms；JWKS 失败降级 5min 旧 key |
| 数据迁移期间 item 遗漏 | Phase 5 | V2 应急 fallback 保留；Phase 6 观察期 catch up |
| 用户改 username 后 commit 历史割裂 | Phase 2 | git immutable（commit author 历史 username 不改）；文档说明 |
| mirror pull 频率跟不上上游 hot repo | Phase 0.3.3 / 5 | hot mirror 白名单 1h 频率；触发式 pull（webhook 上游 push）待 Phase 8 |
| portal iframe 跨域问题 | Phase 4 | 同域部署（nginx 反代）；cookie domain 正确配置 |
| plugin marketplace 收敛期间 dual source | Phase 8 | pack 平级原则：marketplace pack 与官方 pack 同等对待 |

---

## 关键里程碑

| 里程碑 | 完成时间 | 标志 |
|---|---|---|
| M0：决策闭环 | Phase 0 末（T+1 周） | 17 项决策全部记录，跨文档同步完成 |
| M1：Fork baseline ready | Phase 1 末（T+5 周） | fork Gitea 镜像灰度部署 + JWT 中间件 + 全局 hook |
| M2：用户中心切换 | Phase 2 末（T+7 周） | costrict-web 自签 JWT + Casdoor 退化 |
| M3：V3 平台 ready | Phase 3 末（T+8 周） | 4 org + 配置中心 + system webhook |
| M4：Portal 上线 | Phase 4 末（T+10 周） | marketplace / 详情 / 编辑器页跑通 |
| M5：数据迁移完成 | Phase 5 末（T+13 周） | 全部 capability_items 走 V3 链路 |
| M6：V3 稳定 | Phase 6 末（T+15 周） | 持续 2 周无 P0 故障 |
| M7：V2 下线 | Phase 7 末（T+16 周） | CatalogIngestService 删除 |

T = Phase 0 启动时间。关键路径：M0 → M1 → M2 → M5 → M6 → M7。

---

## 附录：Phase 之间的依赖关系

```
Phase 0 (决策闭环)
    │
    ▼
Phase 1 (Fork Gitea)  ←─并行─→  Phase 2 (costrict-web 用户中心)
    │                                 │
    └────────────┬────────────────────┘
                 ▼
            Phase 3 (配置中心 + 4 org)
                 │
                 ▼
            Phase 4 (Portal)  ←─并行─→  Phase 5 (数据迁移)
                 │                          │
                 └──────────┬───────────────┘
                            ▼
                       Phase 6 (观察期)
                            │
                            ▼
                       Phase 7 (V2 下线)
                            │
                            ▼
                       Phase 8 (后续优化，持续)
```

**关键路径**：Phase 0 → 1 → 2 → 3 → 5 → 6 → 7（约 15-16 周）
**并行机会**：Phase 1+2 / Phase 4+5（约节省 2-3 周）
**实际工时**：12-14 周（小团队 2-3 人）
