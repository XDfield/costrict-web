# Capability Portal 部署形态选型

| 字段 | 内容 |
|---|---|
| 状态 | Accepted（v3）· 2026-07-07 锁定 |
| 作者 | CoStrict 团队 |
| 创建日期 | 2026-07-06 |
| 决策日期 | 2026-07-07 |
| 评审范围 | app-ai-native / gitea fork / costrict-web / 部署运维 |
| 关联文档 | `CAPABILITY_GIT_REGISTRY_PROPOSAL.md`（V3 架构基线）、`MULTICA_COBUILD_PROPOSAL.md`（iframe 嵌入先例） |
| 选用方案 | **A — monorepo 子包 `packages/capability-portal/`** |

---

## TL;DR

V3 架构已定：`app-ai-native`（SolidJS 生产前端，iframe 宿主）+ `Gitea`（无头 Git 服务，fork 加 JWT 中间件）+ `costrict-web`（用户中心 + 隧道）。能力项编辑/审核 UI 走 iframe 嵌入路线。**portal 部署形态已锁定方案 A**：在 opencode monorepo 内新增 `packages/capability-portal/`（Vue 3 + Vite + TS + Tailwind），iframe 同域部署，**直连 Gitea API**（不走 costrict-web REST 转发）。

> 本文档**只**讨论 portal 的部署形态与代码组织，不讨论具体页面（marketplace / 详情 / 编辑 / PR 审核）的实现细节——那些在所有方案里都基本相同。

### v3 决策要点（覆盖 v1 假设）

| # | 决策项 | v3 取值 | 备注 |
|---|---|---|---|
| 1 | 鉴权机制 | HttpOnly cookie `costrict_jwt`（domain=`.costrict.local`） | fork JWT 中间件读 cookie fallback（+20 行）；不再依赖 OAuth2 Source |
| 2 | Gitea API 调用方式 | **portal 直连** Gitea REST API（不经过 costrict-web 转发） | 业务字段（favorite / distribute / scan）才走 costrict-web |
| 3 | costrict-web REST 角色 | 仅 4 个业务 endpoint（capabilities list/detail/favorite/create） | 不做 Gitea API proxy |
| 4 | UI 组件复用策略 | **不复用 Gitea Vue 组件**；用第三方库（markdown-it / CodeMirror 6 / diff2html） | Gitea 是 Go template + Vue 撒粉，非前后端分离，组件抽取代价高于直接换栈 |
| 5 | Branch Protection | 已简化（放开直推 main，仅防 force push + delete） | portal 编辑器直接 commit 到 main，不再强制 PR 流程 |
| 6 | iframe 沙箱 | 同域 + sandbox 白名单 + JWT cookie 自动带 | 5 层安全防护不变 |

---

## 1. 背景：为什么需要 portal，又为什么不直接 iframe Gitea

### 1.1 V3 架构下的角色划分

```
┌──────────────────────────────────────────────────┐
│  app-ai-native  (SolidJS)                        │
│  ├─ /workspace  /kanban  /admin  …（保留）        │
│  └─ /marketplace /capabilities/* /prs/*          │
│       ↓ iframe (同域, 共享 Casdoor cookie)        │
│  ┌────────────────────────────────────────────┐  │
│  │  capability-portal  (Vue 3, 业务页面)       │  │
│  │  ├─ 卡片网格 / 详情 / 编辑器 / diff 审核    │  │
│  │  └─ 通过 server PAT 代理调用 Gitea API     │  │
│  └────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────┘
            ↓ (PAT + IP 白名单)
       Gitea (无头, 仅 API)
```

### 1.2 两个硬约束（来自先前决策）

1. **iframe 不能直接渲染 Gitea 原生页面**——必须仿照 app-ai-native 的业务页面按需提供
2. **防止 URL 越权 / 直接调 Gitea API**——5 层防护：同域、PAT 代理、API 白名单、iframe sandbox、Casdoor cookie 共享

### 1.3 portal 必须解决的问题

- 业务页面（卡片网格 / 编辑器 / diff 审核）写在哪儿
- 怎么打包构建、怎么部署到 iframe 同域
- 怎么和 Casdoor SSO 复用（cookie 共享是 iframe 嵌入能否走通的前提）
- 长期维护边界：portal 与 SolidJS shell 的代码隔离度

---

## 2. 候选方案

### 方案 A：monorepo 子包 `packages/capability-portal/`（**已选定**）

**描述**：在 opencode monorepo 内新增 `packages/capability-portal/`，独立 Vue 3 + Vite + Tailwind 应用。构建产物 `dist/` 由 nginx 反向代理挂到与 app-ai-native / Gitea fork **同域**（例如 `/portal/*` → portal 静态资源；`/gitea/api/v1/*` → Gitea fork）。app-ai-native 通过 `<iframe src="/portal/marketplace">` 嵌入，portal 内 JS 直接 `fetch('/gitea/api/v1/repos/.../contents/...')`，HttpOnly cookie `costrict_jwt` 自动带。

**构建/部署**：
- `bun dev --filter capability-portal` 本地开发（端口独立，proxy 到 app-ai-native / Gitea fork 同域）
- `bun build --filter capability-portal` 产出静态资源
- nginx 配置：
  - `location /portal/ { root /var/www/capability-portal; try_files $uri /portal/index.html; }`
  - `location /gitea/ { proxy_pass http://gitea-fork:3000/; }`（含 `/gitea/api/v1/*`）

**Gitea API 调用方式（v3 决策点 #2）**：
- portal 直连 `/gitea/api/v1/*`，**不**经过 costrict-web 转发
- fork JWT 中间件读 cookie `costrict_jwt` fallback（§9.6），无需 portal 单独申请 PAT
- 仅以下业务字段走 costrict-web REST：
  - `GET /api/capabilities?q=&visibility=`（发现层索引）
  - `GET /api/capabilities/{slug}`（详情页右侧业务字段：favorite 数 / scan 状态 / distribute 列表）
  - `POST /api/items/{id}/favorite`
  - `POST /api/items/{id}/distribute`
- git 操作（raw / contents / commits / branches / pulls）全部直连 Gitea

**UI 组件复用策略（v3 决策点 #4）**：
- **不**从 Gitea fork 抽 Vue 组件（Gitea 是 Go template + Vue 撒粉，组件与后端模板耦合，抽取成本高于换栈）
- 选用成熟第三方库：
  - markdown 渲染：`markdown-it` + `highlight.js`
  - 代码编辑器：`CodeMirror 6`（capability.toml / 文档编辑）
  - diff 展示：`diff2html`（PR 审核 / commit 历史对比）
- 与 Gitea Vue 技术栈对齐仅是 coincidental，不作为强约束

**优点**：
- 代码隔离最干净——Vue/TS 完全独立，SolidJS shell 零侵入
- 与 Gitea fork 技术栈同源（Vue 3 + Vite + TS），未来若要深度协作（共享类型、复用 API client）成本可控
- 可以独立发版、独立回滚，不影响 app-ai-native 主链路
- iframe 边界天然清晰，URL 越权防护逻辑全部收敛在 portal 内
- monorepo 内可见性高，PR review、CI、版本对齐都在一处
- portal 直连 Gitea API，costrict-web 不背 Gitea 流量；JWT cookie 自动带，无需 server-side token 代理

**缺点**：
- 引入第二个前端框架（Vue）到 opencode monorepo——团队需要同时维护 SolidJS 和 Vue 两套
- 需要在 app-ai-native 写一个 iframe 容器组件（参考 `multica-page.tsx:1-163`）
- 增量构建时间略增（多一个 Vite 入口）
- 第三方库（markdown-it / CodeMirror / diff2html）样式与 Gitea 原生 UI 不完全一致（v3 已接受，因为不复用 Gitea 组件）

**工作量估算（v3 修正）**：
- 脚手架（Vue 3 + Vite + TS + Tailwind + monorepo 集成）：1-1.5 天
- 第一页（marketplace 卡片网格 + 搜索 + 分类筛选）：2-3 天
- 详情页（markdown 渲染 + commit 历史 + raw 拉取直连 Gitea）：1.5-2 天
- 编辑器页（CodeMirror + contents POST 直连 Gitea + 直推 main UX）：1.5-2 天
- iframe 容器（SolidJS shell 侧）：0.5-1 天
- JWT cookie 调试（cookie domain / sandbox / SameSite）：0.5 天
- 5 层安全防护（路径白名单 / sandbox / cookie / URL 防护 / 加签校验）：0.5-1 天（已与 §13 对齐）
- 合计：**7-10.5 天**（v1 估算 5-7 天偏低，因 v3 加直推 UX + cookie 调试 + 第三方库集成）

**长期维护**：高内聚低耦合，迭代节奏完全由 capability-portal 团队掌控；与 Gitea fork 升级无强耦合

---

### 方案 B：合并到 Gitea fork，添加自定义 Vue 路由

**描述**：在 `D:/DEV/gitea/` 的 fork 内，通过 `templates/custom/` 注入点 + 新增 Vue 路由，把 marketplace / 详情 / 编辑器页面直接写到 Gitea 自身前端里。iframe 的 src 指向 Gitea 的自定义路由（例如 `/marketplace`），但页面 UI 完全自写、不渲染 Gitea 原生 chrome。

**构建/部署**：
- 修改 `web_src/css/index.css` 与 `web_src/js/router.js`，新增 portal 路由
- `make build` 重新构建 Gitea 二进制
- iframe src 直接指向 `/gitea/marketplace`（同域反代）

**优点**：
- 复用 Gitea 已有的 Casdoor OAuth2 Source 集成，SSO 零额外工作
- 复用 Gitea 已有的 PAT、API、Git 后端——portal 调 API 是进程内调用
- 不引入第二个前端框架——Gitea 本来就是 Vue 3 + Vite + TS
- iframe src 与 Gitea 同 origin，cookie/session 天然共享

**缺点**：
- **强耦合 Gitea 升级**——每次跟 upstream 同步都要处理冲突，自定义路由是 fork 的典型痛点
- Gitea fork 已经被定位为"无头后端"，再在前端塞业务页面违背 V3 架构原则
- portal 页面与 Gitea 原生页面 URL 同域，**URL 越权防护难度上升**（需更细的中间件区分"被允许的业务页面"与"被屏蔽的 Gitea 原生页面"）
- 编辑器/diff 审核 UI 与 Gitea 已有的 editor/PR UI 视觉重叠，长期会产生"为什么不直接用 Gitea 原生"的反复讨论
- Gitea 前端代码组织与 opencode 团队熟悉的 monorepo 风格不同，贡献门槛高

**工作量估算**：脚手架 1 天（已有 Vue 栈）+ 第一页 2-3 天 + 5 层安全 3-4 天（防护更复杂）+ fork 维护成本摊销 ≈ **6-8 天 + 长期维护负担**

**长期维护**：与 Gitea upstream 绑定，每次升级需重新 review 自定义路由

---

### 方案 C：在 app-ai-native 内用 Vue web components 重写 `/store/*`

**描述**：不新增独立应用，直接在 `app-ai-native/src/pages/store/`（40 个 TSX，约 11.5K LOC）原地用 Vue 写成 web components（`defineCustomElement`），SolidJS shell 直接 import 这些元素当本地组件用。**取消 iframe**，回到原生嵌入。

**构建/部署**：
- `bun add vue` 到 app-ai-native
- 渐进式：每个 store 子页改写成 `.ce.vue`，Vite 配置 `defineCustomElement`
- SolidJS 直接 `<cap-marketplace />` 渲染

**优点**：
- **零 iframe**——没有跨边界 postMessage、没有 sandbox 隔离、没有 cookie 共享问题
- 用户视角是单一应用，路由/状态/UI 主题完全一致
- 渐进式重构 SolidJS → Vue 的真实落地（团队长期目标）
- 不需要 nginx 配置 iframe 同域

**缺点**：
- **违背"iframe 路线"先前决策**——5 层安全防护、URL 越权防护、PAT 代理机制都白做了
- Vue web component 与 SolidJS 的事件/props 边界处理繁琐，TypeScript 类型联动差
- 没有真正的运行时隔离——一个 Vue 组件 crash 可能拖垮整个 SolidJS shell
- 渐进式迁移过程混乱：两套框架 + 两套组件库（Vue 自有 + SolidJS）共存，store 页面里两种风格交织
- 仍然在 app-ai-native 内引入 Vue 框架成本，但失去了 iframe 的隔离红利

**工作量估算**：脚手架 1 天 + 每个 store 子页 1-2 天 × 40 个 ≈ **40-80 天**（即便只先迁 5-10 个核心页，也要 10-20 天）

**长期维护**：渐进迁移过程中代码风格混乱期最长，对团队心智负担最大

---

### 方案 D：独立仓库 `costrict/capability-portal`

**描述**：完全独立 repo，单独发版、单独 CI、单独部署。与 opencode / gitea / costrict-web 都不共享代码，通过 npm 包或 git submodule 共享类型定义。

**构建/部署**：
- 新建 repo
- 独立 Vite + Vue 3 + TS
- 通过 npm 发布 `@costrict/capability-portal-types`，app-ai-native 按需引用
- 部署时与 app-ai-native 同域反代

**优点**：
- 完全独立的代码生命周期——portal 团队与 opencode 团队互不干扰
- 可独立开源 / 私有化，license 灵活
- 与外部贡献者协作时边界最清晰

**缺点**：
- **跨仓库共享代码成本最高**——类型、工具函数、API client 都得发包或 submodule
- Casdoor SSO 配置、cookie 域名、CORS 都要重新对齐
- 与 opencode monorepo 已有的 `app-ai-native` / `app` / `opencode` 包无法直接 import
- monorepo 的好处（统一 tsconfig / 统一 lint / 统一 CI / 统一版本）全部失去
- 对小团队（目前看 ≤5 人）来说，多仓库管理成本不必要

**工作量估算**：脚手架 2-3 天（含 CI/CD/lint/license）+ 第一页 2-3 天 + 5 层安全 2 天 + 跨仓库类型共享 1-2 天 ≈ **7-10 天**

**长期维护**：仓库间版本对齐、breaking change 同步、跨 repo PR review 成本最高

---

## 3. 对比矩阵

| 维度 | A: monorepo 包 | B: Gitea fork 内 | C: Vue CE in SolidJS | D: 独立 repo |
|---|---|---|---|---|
| 与 V3 架构契合度 | ★★★★★ | ★★（违背无头定位） | ★★（放弃 iframe） | ★★★★ |
| 与 Gitea 技术栈对齐 | ★★★★（同 Vue 3） | ★★★★★（就是 Gitea） | ★★★ | ★★★★ |
| 与 SolidJS shell 隔离 | ★★★★★ | ★★★★ | ★（同进程） | ★★★★★ |
| URL 越权防护清晰度 | ★★★★★ | ★★（同 origin 难分） | ★★★★★ | ★★★★★ |
| Casdoor SSO 复用 | ★★★★ | ★★★★★（原生集成） | ★★★★★ | ★★★ |
| 长期升级负担 | ★★★★（轻） | ★（Gitea upstream 冲突） | ★★★ | ★★★★ |
| 跨仓库代码共享 | ★★★★★（同 monorepo） | ★★★ | ★★★★★ | ★（需发包） |
| 团队认知成本（短期） | ★★★（学 Vue） | ★★★★（已知栈） | ★★（双框架并存混乱） | ★★★ |
| 渐进迁移路径支持 | ★★★★ | ★★ | ★★★★★（最贴合长期目标） | ★★★★ |
| 工作量（首版）| 5-7 天 | 6-8 天 | 10-20 天（仅核心页） | 7-10 天 |

> ★ 越多越优。

---

## 4. 推荐与理由

### ✅ 已选定：**方案 A（monorepo 子包 `packages/capability-portal/`）**

**决策日期**：2026-07-07

**理由**：

1. **与 V3 架构契合**——iframe 路线 + 同域部署 + 5 层安全防护全部按既定方案落地，无需推翻重做
2. **JWT cookie 路径最短**——同域部署 + HttpOnly cookie 自动带，fork JWT 中间件只需 +20 行 cookie fallback；portal 无需 PAT 申请/续期/吊销流程
3. **直连 Gitea API 减负**——portal 直连 `/gitea/api/v1/*`，costrict-web 不背 Gitea 流量、不写 API proxy；REST endpoint 收敛到 4 个业务字段
4. **与 Gitea 技术栈对齐**——Vue 3 + Vite + TS 是团队未来要与 Gitea fork 协作的必经之路，提前在 monorepo 内沉淀经验
5. **代码隔离度最高**——portal 是 portal、shell 是 shell，PR review、CI、回滚都互不干扰
6. **URL 越权防护清晰**——portal 在 `/portal/*` 路径下，nginx 白名单只放行 `/portal/*` + `/gitea/api/v1/*`，Gitea 原生页面完全屏蔽
7. **monorepo 红利最大化**——类型、API client、工具函数、CI 配置直接复用，零跨仓库成本
8. **工作量适中**——7-10.5 天首版 vs 方案 B 的 fork 维护长期摊销、方案 C 的渐进混乱期、方案 D 的跨仓成本

### 不选其他方案的核心原因（v3 复核后 reaffirm）

- **方案 B**：违背 Gitea "无头后端" 定位；Gitea 是 Go template + Vue 撒粉**非前后端分离**，在 fork 内插业务页面要重构模板渲染链路，fork 升级冲突是长期慢性病
- **方案 C**：放弃 iframe 等于推翻 V3 决策（含 5 层安全、JWT cookie、fork 中间件全部白做）；Vue/SolidJS 双框架在同一 shell 内的混乱期不可接受
- **方案 D**：对小团队（≤5 人）而言多仓库管理是过度设计，monorepo 已能提供所有隔离需求
- **GitHub 生态**：截至 2026-07-07 未发现成熟的 Gitea 前端分离 SPA 项目可直接复用，自建成本不亚于方案 A

### 何时应重新评估

- 团队规模 > 10 人且 portal 团队独立 → 重新考虑方案 D
- SolidJS → Vue 全量迁移启动 → 重新考虑方案 C
- Gitea fork 升级频率降到 < 1 次/季度 + Gitea 完成 API-first 改造 → 重新考虑方案 B

---

## 5. 方案 A 的实施前置条件

> 决策已锁定（2026-07-07），以下为实施清单。

1. **monorepo 内脚手架**
   - `packages/capability-portal/` 目录
   - `package.json` + Vue 3 + Vite + TS + Tailwind
   - 复用 opencode monorepo 的 tsconfig / eslint / prettier

2. **构建与部署**
   - `bun dev --filter capability-portal` 启动 dev server（端口 4445 或类似）
   - `bun build --filter capability-portal` 产出 `dist/`
   - nginx 反代配置：
     - `location /portal/ { root /var/www/capability-portal; try_files $uri /portal/index.html; }`
     - `location /gitea/ { proxy_pass http://gitea-fork:3000/; }`（覆盖 `/gitea/api/v1/*`）

3. **JWT cookie 复用**（v3 鉴权机制）
   - portal 内调 `/api/auth/me`（与 app-ai-native 同 cookie）
   - portal 直连 `/gitea/api/v1/*` 时 HttpOnly cookie `costrict_jwt` 自动带
   - fork JWT 中间件加 cookie fallback（§9.6 已记 +20 行实现要点）
   - cookie 域：`.costrict.local`，SameSite=Lax，HttpOnly

4. **第一页：`/marketplace`**
   - 卡片网格 + 分类筛选 + 搜索
   - 调 costrict-web REST：`GET /api/capabilities`（仅业务字段）
   - 不直连 Gitea（marketplace 列表数据来自 server DB `capability_items`）

5. **详情页 + 编辑器页**
   - 详情页：调 `GET /api/capabilities/{slug}`（业务字段）+ 直连 `GET /gitea/api/v1/repos/.../contents/...`（content）
   - 编辑器：CodeMirror 6 + 直连 `POST /gitea/api/v1/repos/.../contents/...` 直推 main（branch protection 已放开）

6. **app-ai-native 内 iframe 容器**
   - 参考 `multica-page.tsx:1-163`
   - sandbox 策略（`allow-scripts allow-same-origin allow-forms`）+ ref 通信 + loading 状态

7. **5 层安全防护**（按 `CAPABILITY_GIT_REGISTRY_PROPOSAL.md` §13 落地）
   - 同域（nginx 反代到 `.costrict.local`）
   - iframe sandbox（细粒度 allow 列表）
   - JWT cookie（HttpOnly + SameSite=Lax）
   - Gitea API 路径白名单（fork 中间件只放行 `/gitea/api/v1/*`，屏蔽 Gitea 原生 HTML 路由）
   - URL 防护（portal 内 router 全部走 hash/history，无 redirect 跳 Gitea 原生页）

8. **第三方库集成**（v3 决策点 #4）
   - markdown 渲染：`markdown-it` + `highlight.js`
   - 代码编辑器：`CodeMirror 6`
   - diff 展示：`diff2html`
   - 不从 Gitea fork 抽 Vue 组件

---

## 6. 决策记录

| 决策项 | 内容 |
|---|---|
| 决策状态 | ✅ Accepted（v3） |
| 决策人 | CoStrict 团队 |
| 决策日期 | 2026-07-07 |
| 选择方案 | **A — monorepo 子包 `packages/capability-portal/`** |
| 决策理由 | (1) 与 V3 架构契合（iframe + 同域 + 5 层安全）；(2) JWT cookie 路径最短；(3) 直连 Gitea API 减负；(4) 代码隔离度最高；(5) 工作量适中（7-10.5 天首版） |
| v3 关键变化 | 鉴权 OAuth2 Source → JWT cookie；Gitea API 调用 server proxy → 直连；UI 组件 复用 Gitea → 第三方库；Branch Protection 严格 → 放开直推 main |
| 重新评估触发条件 | 见 §4「何时应重新评估」 |

---

## 7. v3 决策相对 v1 的变化记录

| 维度 | v1（Draft）假设 | v3（Accepted）落地 |
|---|---|---|
| 鉴权 | Casdoor 唯一 IdP + OAuth2 Source | costrict-web 签 JWT + HttpOnly cookie + fork JWT 中间件 cookie fallback |
| Gitea API 调用方 | portal → costrict-web REST → Gitea（server 转发） | portal → 直连 Gitea API（cookie 自动带） |
| costrict-web REST 角色 | 7+ endpoint（proxy Gitea） | 4 endpoint（仅业务字段：list / detail / favorite / distribute） |
| UI 组件来源 | 期望复用 Gitea Vue 组件 | 不复用；用 markdown-it / CodeMirror 6 / diff2html |
| Branch Protection | 必走 PR 流程 | 直推 main 允许（仅防 force push + delete） |
| 工时估算 | 5-7 天 | 7-10.5 天（加直推 UX + cookie 调试 + 第三方库集成） |

---

> 本文档不替代 `CAPABILITY_GIT_REGISTRY_PROPOSAL.md`，仅补充 portal 部署形态的决策依据。V3 架构、iframe 路线、5 层安全防护等既定方案在所有候选方案中保持一致（除方案 C 主动放弃 iframe）。
