# 本地 Casdoor 开发环境

把 costrict-web 整套链路（Casdoor 认证 + Postgres + Redis + server + portal）跑在本机的步骤。
适用场景：内网 Casdoor 临时无法访问、要离线复现 OAuth 流程、CI 之外的端到端调试。

## 前置

- Docker / Docker Compose
- Go 1.22+
- Bun（用于 portal）

## 1. 起 Postgres + Casdoor + Redis

仓库根目录执行：

```bash
docker compose up -d postgres casdoor redis
```

首次启动 Casdoor 会用 `init-db.sql` 在 Postgres 里建好的 `casdoor` 库自动建表 +
在 `built-in` 组织下创建默认 `admin` 用户（密码 `123`）和 `app-built-in` application。

## 2. 预置 costrict-local application

Casdoor 起来后跑一次 bootstrap 脚本，通过 REST API 创建一个 `costrict-local`
application（client_id / client_secret 写死在脚本里，仅本机用）：

```bash
./casdoor/bootstrap-local-app.sh
```

脚本是幂等的——重复执行只会在第一次创建，之后 skip。

> 我们没用 Casdoor 官方的 `init_data.json` 机制，因为 `casbin/casdoor:latest` 实测
> 不会读取这个文件（即便设了 `initDataFile` 配置）。bootstrap 脚本路径更稳、可调试。

验证：浏览器打开 <http://localhost:8000>，用 `admin / 123` 登录。
在 Applications 列表里应能看到 `costrict-local`，client_id = `costrict-local-client-id`。

> **数据持久化**：Casdoor 的数据存在 `postgres_data` volume 里。`docker compose down -v`
> 会清空；重新 up 后再跑一次 `bootstrap-local-app.sh` 就行。

## 3. 配置 server

```bash
cd server
cp .env.example .env            # 第一次才需要
cp .env.local.example .env.local
# 看一眼 .env.local，确认 CASDOOR_* 值与 casdoor/init_data.json 一致
```

`.env.local` 已经写好和本地 Casdoor 匹配的 `CASDOOR_CLIENT_ID` / `CASDOOR_CLIENT_SECRET` / `CASDOOR_ORGANIZATION`。

启动：

```bash
go run ./cmd/migrate          # 一次性建表
go run ./cmd/api               # 起 API，监听 :8080
```

## 4. 配置 portal

```bash
cd portal/opencode/packages/app-ai-native
cp .env.local.example .env.local
bun install                    # 第一次才需要
bun dev                        # 起在 :3000
```

`.env.local` 会覆盖 `.env.development` 里指向内网的 `VITE_CASDOOR_*`，切到 `http://localhost:8000`。

## 5. 验证 OAuth 链路

1. 浏览器打开 <http://localhost:3000>
2. 点击登录，应跳转到 `http://localhost:8000/login/oauth/authorize?client_id=costrict-local-client-id&...`
3. 用 `admin / 123` 登录
4. Casdoor 回跳到 `http://localhost:8080/api/auth/callback?code=...`
5. server 拿 code 换 token，写 Cookie，跳回 portal 首页

回跳成功 = 整条链路打通。

## Portal 默认走 auth mock，**看不到** Casdoor 用户

Portal 的 `vite.config.ts` 检测到 `VITE_CLOUD_SERVER_HOST` 是 `localhost` / `127.0.0.1` /
`0.0.0.0` / `[::1]` 时，会自动挂一个 `local-auth-mock` 中间件，硬编码返回
`{ id: "local-dev-user", name: "local-dev" }` 给所有 `/api/auth/me` 调用，并把
`/api/auth/permissions` 返回为 `["*"]`。

效果：即便 Casdoor 链路完全通，浏览器里的 User menu 永远显示 `local-dev`，
不会看到通过 Casdoor 登入的真实用户。这是 portal 开发者刻意做的本地友好设计，
让开发者不走完整 OAuth 也能调 UI。

**怎么测真的 OAuth 链路？** 两条路：

1. **手动撕掉 mock**：临时把 `portal/opencode/packages/app-ai-native/vite.config.ts`
   里的 `localAuthMock` 设成 `null`（或加个 `VITE_DISABLE_AUTH_MOCK` 开关）后重启
   `bun dev`。然后页面右上角 Sign In，会跳到 Casdoor、登录、回到 server `/api/auth/callback`，
   server 写 cookie 后 redirect 到 store。
2. **绕过 portal 直接打 server 端点**：`curl -c jar.txt http://localhost:8080/api/auth/login?...`
   走完整 token exchange，看 server 日志的 `casdoor token exchange` / `user upsert` 行。

不打算长期跑真 OAuth 的话，保留 mock 就行——data 层、items 接口、skill store 都不受影响。

## 已知坑位

- **`casdoor/app.conf` 必须是文件**：如果不存在就跑 `docker compose up`，Docker 会把 bind-mount source 自动创建成空目录，启动后 Casdoor 报 `conf file not found`。删除目录 + 把 `app.conf.example` 填好真值复制成 `app.conf` 即可。
- **client_secret 是明文写在 bootstrap-local-app.sh 里的**：仅用于 localhost。生产 / staging 请通过 Casdoor UI 单独建 application 并把 secret 注入 K8s Secret。
- **`portal/.env.development:25` 行的 zgsmAdminToken 是内网 JWT**：不要 commit 你自己的版本，本地切到 `.env.local` 即可绕开它。
- **重置 Casdoor**：`docker compose down && docker volume rm costrict-web_postgres_data && docker compose up -d postgres casdoor redis && ./casdoor/bootstrap-local-app.sh` —— 会重新跑 `init-db.sql` 并预置 application。注意这会同时清空 `costrict_db`（capability_items 等也会丢）。
