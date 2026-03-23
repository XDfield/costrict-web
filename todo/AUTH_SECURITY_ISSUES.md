# 鉴权机制安全问题清单

基于对 `server/` 和 `gateway/` 的代码审查，记录当前鉴权机制的缺陷与缺失项。

---

## P0 — 高危，需立即修复

- [ ] **JWT 未做签名验证**
  - 位置：`server/internal/middleware/auth.go:100`
  - 问题：使用 `jwt.ParseUnverified` 跳过签名校验，任何人伪造 JWT payload 均可通过本地解析；仅在 Casdoor 服务不可用时才被 fallback 拦截
  - 修复：使用 Casdoor 公钥对 JWT 做签名验证，或始终强制走 Casdoor API 验证

- [ ] **设备隧道连接无任何认证**
  - 位置：`gateway/internal/router.go:17`、`gateway/internal/tunnel_handler.go:18`
  - 问题：`/device/:deviceID/tunnel` WebSocket 端点不验证连接方身份，任何人可冒充任意 deviceID 接管隧道
  - 修复：连接时校验设备 token（如 Bearer token 或 query 参数），验证通过后再建立 yamux session

- [x] **Gateway → Server 内部通信无认证**
  - 位置：`gateway/internal/registration.go`、`server/internal/gateway/handlers.go:149`
  - 问题：网关向 server 发送注册、心跳、设备上下线通知时无任何凭证，`/internal/gateway/*` 路由也无鉴权，外部可伪造网关注册或篡改设备状态
  - 修复：
    1. Server: 新增 `InternalAuth` 中间件，校验 `X-Internal-Secret` 请求头，密钥为空时拒绝所有请求
    2. Server: `/internal/gateway/*` 路由挂载 `InternalAuth` 中间件
    3. Gateway: 所有请求 server `/internal/*` 的 HTTP 调用统一通过 `internalPost`/`internalRequest` 辅助函数携带 `X-Internal-Secret`
    4. 双端通过 `INTERNAL_SECRET` 环境变量管理密钥

---

## P1 — 重要，近期修复

- [x] **大量业务写接口未强制鉴权**
  - 位置：`server/cmd/api/main.go:165-310`
  - 问题：`/api/repositories`、`/api/registries`、`/api/items`、`/api/artifacts`、`/api/marketplace` 等路由仅依赖全局 `OptionalAuth`，未登录用户可调用所有写操作（CreateRepository、DeleteRepository、UploadArtifact 等）
  - 修复：
    1. 将所有写操作及用户专属资源路由收拢到 `authed` group 并挂载 `RequireAuth`
    2. Items/Registries 只读接口（`GET /items`、`GET /items/:id`、`GET /registries/:id`、`GET /registries/:id/items`、versions、artifacts、download、scan-results 等）保留在 `OptionalAuth` 下，匿名用户可预览公开内容（`ListAllItems` 已通过 `buildVisibleRegistryIDs` 控制匿名仅见 public registry）
    3. Marketplace 浏览、webhook 等保持公开

- [ ] **`/cloud/device/:deviceID/proxy/*path` 无鉴权（TODO 未完成）**
  - 位置：`server/cmd/api/main.go:343`
  - 问题：代码注释 `// TODO: 打通链路后加认证`，当前任何人知道 deviceID 即可代理访问该设备内部服务
  - 修复：补充鉴权，验证调用方是该设备的归属用户

- [ ] **`/cloud/device/gateway-assign` 无鉴权**
  - 位置：`server/cmd/api/main.go:329`
  - 问题：设备分配网关接口完全公开，可被外部滥用枚举或耗尽网关资源
  - 修复：要求设备 token 认证后才可调用

- [ ] **Gateway 代理端点无鉴权**
  - 位置：`gateway/internal/router.go:18`、`gateway/internal/proxy_handler.go:21`
  - 问题：`/device/:deviceID/proxy/*path` 无任何认证，知道 deviceID 即可代理访问设备
  - 修复：验证请求方持有合法 token（用户 token 或内部密钥）

---

## P2 — 中等，计划修复

- [x] **`GetMyRepositories` 接受前端传入的 userId（越权）**
  - 位置：`server/internal/handlers/handlers.go:937`
  - 问题：`userId` 由 query 参数传入而非从 token 中提取，已认证用户可查询任意其他用户的仓库列表
  - 修复：从 `c.Get("userId")` 获取当前登录用户 ID，忽略前端传参

- [x] **`AddRepoRegistry` 中 ownerID 回退逻辑不安全**
  - 位置：`server/internal/handlers/handlers.go:766-769`
  - 问题：当 context 中取不到 `userID` 时，回退使用 `repo.OwnerID`，导致未认证调用者可以以仓库 owner 身份创建 registry
  - 修复：取不到 userID 时直接返回 401，不做回退

- [x] **Cookie 未设置 Secure 标志**
  - 位置：`server/internal/handlers/handlers.go:66`
  - 问题：`SetCookie` 的 `secure` 参数为 `false`，在 HTTPS 环境下 token 仍可能通过明文 HTTP 传输
  - 修复：新增 `COOKIE_SECURE` 配置项（默认 `true`），生产环境自动启用 Secure 标志，开发环境可设为 `false`

---

## P3 — 低优先级，优化项

- [x] **CORS 配置 `Allow-Origin: *` 与 `Allow-Credentials: true` 同时设置**
  - 位置：`server/internal/middleware/middleware.go:14-15`
  - 问题：浏览器规范禁止在 `Allow-Origin: *` 时携带凭证，两者同时设置无效且表明配置未经安全审查
  - 修复：`CORS()` 改为接受 `CORSConfig`，通过 `CORS_ALLOWED_ORIGINS` 环境变量配置允许的域名列表；未配置时回显请求 Origin（兼容开发模式），配置后严格校验 Origin 白名单
