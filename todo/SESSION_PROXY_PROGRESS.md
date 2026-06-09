# Session Proxy 实施进度

基于 `docs/proposals/SESSION_PROXY_DESIGN.md`，按依赖关系排序的开发任务列表。

---

## 一、项目脚手架（无依赖）

- [x] `proxy/go.mod` — `module github.com/costrict/costrict-web/proxy`，依赖 `gin-gonic/gin`、`gorm.io/driver/postgres`、`gorm.io/gorm`、`go.uber.org/zap`、`gopkg.in/natefinch/lumberjack.v2`
- [x] `proxy/cmd/main.go` — 入口：解析 env 配置、启动校验、初始化组件、优雅关闭（SIGTERM drain）
  - 启动校验：`DATABASE_URL` 非空 + 格式合法 + `db.Ping()` + AutoMigrate + `SERVER_URL` 可达（可选）
  - 优雅关闭：停止 listener → 等 in-flight 完成 → 刷审计 channel → 关 DB → 退出
- [x] `proxy/internal/config.go` — 环境变量加载（`LISTEN_ADDR`、`SERVER_URL`、`DATABASE_URL`、`LOG_DIR`、`MAX_INTERCEPT_BODY_SIZE`、`FILTER_FAILURE_MODE` 等）
- [x] `proxy/internal/logger/logger.go` — 结构化文件日志（复用 gateway 方案）
- [x] `proxy/internal/middleware/jwt.go` — JWT 解码中间件
- [x] `proxy/internal/middleware/jwt_test.go` — JWT 中间件单测
- [x] `go.work` — 添加 `use ./proxy`
- [x] `proxy/Dockerfile` — 多阶段构建（参照 `gateway/Dockerfile`）
- [x] `proxy/filter_rules.yaml` — 可选规则配置文件（不存在时使用代码内默认值）

---

## 二、反向代理核心（依赖：一）

- [x] `proxy/internal/proxy/writer.go` — `interceptWriter`（实现 gin.ResponseWriter 接口）
- [x] `proxy/internal/proxy/writer_test.go` — interceptWriter 单测
- [x] `proxy/internal/router.go` — 路由分发（intercept / audit-only / disabled / pass-through）
  - 反向代理核心集成在 router.go 中（基于 `httputil.ReverseProxy`）
- [x] 健康检查（`/health` + `/health/ready`）

---

## 三、过滤策略引擎（依赖：一）

- [x] `proxy/internal/filter/strategy.go` — 过滤策略实现（redact + strip + mask）
- [x] `proxy/internal/filter/strategy_test.go` — 策略单测
- [x] `proxy/internal/filter/rules.go` — 规则加载（YAML 解析 + 默认值）
- [x] `proxy/internal/filter/rules_test.go` — 规则加载单测

---

## 四、内容过滤器（依赖：三）

- [x] `proxy/internal/filter/markdown.go` — Markdown code block 解析 + 过滤（含未闭合 block 检测）
- [x] `proxy/internal/filter/markdown_test.go` — Markdown 过滤单测
- [x] `proxy/internal/filter/code.go` — 纯代码文本过滤（整体 + 按行）
- [x] `proxy/internal/filter/code_test.go` — 代码过滤单测
- [x] `proxy/internal/filter/diff.go` — Unified diff 解析 + 过滤
- [x] `proxy/internal/filter/diff_test.go` — diff 过滤单测
- [x] `proxy/internal/filter/shell.go` — Shell 输出阈值过滤
- [x] `proxy/internal/filter/shell_test.go` — Shell 过滤单测
- [x] `proxy/internal/filter/engine.go` — Content Filter 入口（Content Type Router）
- [x] `proxy/internal/filter/engine_test.go` — 引入口集成测试

---

## 五、Part-aware 路由 + Tool 过滤（依赖：四）

- [x] `proxy/internal/filter/part.go` — Part 类型路由（text/tool/tool-result/reasoning/snapshot）
- [x] `proxy/internal/filter/part_test.go` — Part 路由单测
- [x] `proxy/internal/filter/tool.go` — Tool output 过滤（按工具名分发策略）
- [x] `proxy/internal/filter/tool_test.go` — Tool 过滤单测

---

## 六、JSON 响应改写（依赖：五）

- [x] `proxy/internal/filter/rewrite.go` — JSON 响应改写
  - 深度遍历 JSON 树 + Part 类型分发
  - Runtime 接口响应改写（files/content + diff/content）
  - 过滤标记注入（filtered- 前缀 + _filtered metadata）

---

## 七、SSE 流式过滤（依赖：五）

- [x] `proxy/internal/filter/sse.go` — SSE 流式过滤
  - 逐 event 解析 + Part-aware 过滤
  - Event Bus SSE（message.part.updated）过滤
  - 逐 event flush
- [x] `proxy/internal/filter/sse_test.go` — SSE 过滤单测

---

## 八、审计日志（依赖：二，可与四-七并行）

- [x] `proxy/internal/audit/model.go` — AuditLog + AuditFile + AuditTool 模型
- [x] `proxy/internal/audit/store.go` — GORM PostgreSQL 初始化 + 批量写入 + 过期清理
- [x] `proxy/internal/audit/worker.go` — 异步写入 worker（buffered channel + 批量 flush + 重试）
- [x] `proxy/internal/audit/worker_test.go` — Worker 单测（mock AuditStore）
- [x] 审计触发点（集成在 router.go 中：intercept 路径 + audit-only 路径）

---

## 九、集成部署（依赖：二 ~ 八全部完成）

- [ ] `docker-compose.yml` — 添加 proxy service
  - 依赖 server，暴露独立端口（8090）
  - 挂载 `filter_rules.yaml`
  - 环境变量 `DATABASE_URL` 指向独立 dbname（如 `costrict_audit`）
- [ ] UI 接入 — `app-ai-native` 环境变量
  - `API_BASE_URL` 指向 proxy 地址
  - Terminal 禁用：检测 403 `TERMINAL_DISABLED` 渲染禁用提示
  - Markdown 渲染器：识别 `streaming*` / `filtered*` 前缀，渲染骨架屏/过滤卡片
- [ ] 端到端测试
  - TextPart code block 过滤
  - ToolPart output 过滤（read_file、bash 阈值）
  - SSE 流式 TextPart 过滤（含未闭合 → 闭合过渡）
  - SSE Prompt 流过滤
  - Runtime 文件/diff 过滤
  - Terminal 403 禁用
  - 审计日志写入验证
  - 响应体超限降级透传
  - WebSocket terminal 403 + 其他透传

---

## 依赖关系图

```
一（脚手架）
├── 二（反向代理核心）──┐
├── 三（策略引擎）──┐   │
│                  │   │
│                  ▼   ▼
│              八（审计日志，可并行）
│                  
├── 三 → 四（内容过滤器）→ 五（Part-aware + Tool）→ 六（JSON 改写）
│                                              └→ 七（SSE 流式过滤）
│
└── 六、七、八 全部完成 → 九（集成部署）
```
