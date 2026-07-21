# 多集群客户端侧集群路由（Client-Side Cluster Routing）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 设备信息接口携带归属集群的 API 地址（clusterAPIURL），前端按它把设备请求路由到归属集群域名，Server 不做跨集群转发。

**Architecture:** Gateway 从 Nacos（独立 dataId，失败回退 env）解析本集群 Server 公网 API 地址 apiBaseURL 并在注册时上报；Server 将其存入 gateway_registry 并在设备列表/详情接口附带 clusterAPIURL；前端在 getProxyUrl 中按 deviceId 查询缓存选择 API origin，缓存由设备接口响应填充，路由失效时刷新重试一次。

**Tech Stack:** Go (gateway + server, Gin, GORM/redis/memory store)、SolidJS/TS (portal, bun test)、Helm chart。

**Spec:** `docs/superpowers/specs/2026-07-21-multi-cluster-client-routing-design.md`
**Branch:** `feat/multi-cluster-client-routing`（基于 origin/main，工作目录 `/Users/linkai/code/costrict-web`）

---

## 背景知识（实现者需要知道的现状）

- Gateway 已有 Nacos endpoint 解析器 `gateway/internal/endpoint_resolver.go`：`Resolve(cfg)` 从 Nacos 读 endpoint，`ErrNacosConfigNotFound` 表示 404 可回退；`resolveFromNacos(client, n)` 是实际 HTTP 逻辑；`NacosEnabled(n)` 判断配置齐全。测试模式见 `endpoint_resolver_test.go`（httptest 假 Nacos）。
- Gateway 注册客户端 `gateway/internal/registration.go:40` `Register(serverURL, gatewayID, endpoint, internalURL, region, secret string, capacity int)`；`gateway/cmd/main.go` 有两处调用（`registerWithRetry` 和 `heartbeatLoop` 里的重注册）。
- Server 注册 handler `server/internal/gateway/handlers.go:63-92`；`GatewayInfo` 在 `server/internal/gateway/types.go:3-11`（无 json tag，redis store 直接 JSON 序列化整个结构体，加字段自动兼容）。
- Postgres store `server/internal/gateway/store_postgres.go`：RegisterGateway 用 `clause.OnConflict.DoUpdates` upsert；model 在 `server/internal/models/models.go:121-133`（`GatewayRegistry`，AutoMigrate 加列：`server/cmd/migrate/main.go:236` 与 `server/cmd/worker/main.go:77` 都已包含该 model，改 model 即迁移）。
- 设备接口 `server/internal/handlers/device.go`：`ListDevicesHandler(svc, updateSvc)`（:97）、`GetDeviceHandler(svc)`（:172）、`deviceToMap`（:135）。路由在 `server/cmd/api/main.go:467-468` 注册，但 `gatewayRegistry` 在 :600 才构造——**需要把 store/registry 构造块前移**（Task 4 详述）。
- 前端 proxy URL 唯一构造点 `portal/packages/app-ai-native/src/pages/workspace/lib/url.ts` `getProxyUrl(deviceId)`，约 10 个调用点（layout.tsx、mobile、multica、console、cloud-device-api、store/lib/api.ts）全部只传 deviceId——**方案：getProxyUrl 内部读缓存，调用点零改动**。
- 前端有两个 `normalizeDevice`：`pages/workspace/lib/api.ts:67` 和 `pages/store/lib/api.ts:432`（各自独立），设备列表/详情响应都会经过它们——缓存在这两处填充。
- 前端测试：`cd portal/packages/app-ai-native && bun test <file>`（bun:test，happydom preload）。
- Server "设备未连接"错误：proxy 失败时返回 JSON `{"error": "device not connected"}`（`registry.go:151`），前端 apiFetch 会把它作为 Error.message 抛出——失效重试按此消息匹配。

---

### Task 1: Gateway — Config 与解析器支持 apiBaseURL

**Files:**
- Modify: `gateway/internal/config.go`
- Modify: `gateway/internal/endpoint_resolver.go`
- Test: `gateway/internal/endpoint_resolver_test.go`

- [ ] **Step 1: 写失败测试**

在 `gateway/internal/endpoint_resolver_test.go` 追加：

```go
func TestResolveAPIBaseURL_NoNacos(t *testing.T) {
	resolver := NewEndpointResolver()
	cfg := &Config{APIBaseURL: "https://api-a.example.com"}

	got, err := resolver.ResolveAPIBaseURL(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != cfg.APIBaseURL {
		t.Fatalf("expected %q, got %q", cfg.APIBaseURL, got)
	}
}

func TestResolveAPIBaseURL_NacosSuccess(t *testing.T) {
	expected := "https://api-a.example.com"
	var capturedDataID string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedDataID = r.URL.Query().Get("dataId")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, expected)
	}))
	defer server.Close()

	resolver := NewEndpointResolverWithClient(server.Client())
	cfg := &Config{
		APIBaseURL: "https://static.example.com",
		Nacos: NacosConfig{
			ServerAddr:        server.URL,
			DataID:            "endpoint-data-id",
			APIBaseURLDataID:  "api-base-url-data-id",
			Group:             "DEFAULT_GROUP",
		},
	}

	got, err := resolver.ResolveAPIBaseURL(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}
	if capturedDataID != "api-base-url-data-id" {
		t.Fatalf("expected dataId %q, got %q", "api-base-url-data-id", capturedDataID)
	}
}

func TestResolveAPIBaseURL_NacosNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	resolver := NewEndpointResolverWithClient(server.Client())
	cfg := &Config{
		APIBaseURL: "https://static.example.com",
		Nacos: NacosConfig{
			ServerAddr:       server.URL,
			APIBaseURLDataID: "missing",
		},
	}

	_, err := resolver.ResolveAPIBaseURL(cfg)
	if !errors.Is(err, ErrNacosConfigNotFound) {
		t.Fatalf("expected ErrNacosConfigNotFound, got %v", err)
	}
}

func TestResolveAPIBaseURL_NacosInvalidURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "not-a-url")
	}))
	defer server.Close()

	resolver := NewEndpointResolverWithClient(server.Client())
	cfg := &Config{
		Nacos: NacosConfig{
			ServerAddr:       server.URL,
			APIBaseURLDataID: "bad",
		},
	}

	_, err := resolver.ResolveAPIBaseURL(cfg)
	if err == nil {
		t.Fatal("expected error for invalid URL from Nacos")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd /Users/linkai/code/costrict-web/gateway && go test ./internal/ -run TestResolveAPIBaseURL -v`
Expected: 编译失败，`ResolveAPIBaseURL` 未定义、`APIBaseURLDataID` 未定义。

- [ ] **Step 3: 改 Config**

`gateway/internal/config.go`：

`Config` struct（第 10-20 行）在 `Endpoint` 字段后加：

```go
	APIBaseURL     string      // 本集群 Server 公网 API 地址，多集群部署时供 Web 前端路由设备请求；单集群可留空
```

`NacosConfig` struct 在 `DataID` 后加：

```go
	APIBaseURLDataID string // optional: data ID for resolving APIBaseURL from Nacos
```

`LoadConfig()` 的返回值里，`Endpoint:` 行后加：

```go
		APIBaseURL:     getEnv("GATEWAY_API_BASE_URL", ""),
```

Nacos 初始化块里 `DataID:` 行后加：

```go
			APIBaseURLDataID: getEnv("GATEWAY_NACOS_API_BASE_URL_DATA_ID", ""),
```

- [ ] **Step 4: 改 resolver**

`gateway/internal/endpoint_resolver.go`：

1. `EndpointResolver` 接口加方法：

```go
type EndpointResolver interface {
	Resolve(cfg *Config) (string, error)
	// ResolveAPIBaseURL resolves the cluster's public Server API base URL.
	// Uses Nacos when APIBaseURLDataID is configured; otherwise returns the
	// statically configured value. May be empty in single-cluster setups.
	ResolveAPIBaseURL(cfg *Config) (string, error)
}
```

2. 在 `Resolve` 之后加实现：

```go
func (r *defaultEndpointResolver) ResolveAPIBaseURL(cfg *Config) (string, error) {
	if cfg == nil {
		return "", errors.New("config is nil")
	}

	if !NacosAPIBaseURLEnabled(cfg.Nacos) {
		return cfg.APIBaseURL, nil
	}

	apiBaseURL, err := resolveFromNacos(r.client, cfg.Nacos, cfg.Nacos.APIBaseURLDataID)
	if err != nil {
		return "", fmt.Errorf("resolve apiBaseURL from Nacos failed: %w", err)
	}

	apiBaseURL = strings.TrimSpace(apiBaseURL)
	if apiBaseURL == "" {
		return "", errors.New("Nacos returned empty apiBaseURL")
	}

	if err := validateEndpoint(apiBaseURL); err != nil {
		return "", err
	}

	return apiBaseURL, nil
}

// NacosAPIBaseURLEnabled reports whether Nacos apiBaseURL resolution is configured.
func NacosAPIBaseURLEnabled(n NacosConfig) bool {
	return n.ServerAddr != "" && n.APIBaseURLDataID != ""
}
```

3. 把 `resolveFromNacos` 的签名改为接收 dataID 参数（消除对 `n.DataID` 的硬编码）：

```go
func resolveFromNacos(client *http.Client, n NacosConfig, dataID string) (string, error) {
```

函数体内 `q.Set("dataId", n.DataID)` 改为 `q.Set("dataId", dataID)`。

4. `Resolve` 中的调用同步改为：

```go
	endpoint, err := resolveFromNacos(r.client, cfg.Nacos, cfg.Nacos.DataID)
```

- [ ] **Step 5: 跑测试确认通过**

Run: `cd /Users/linkai/code/costrict-web/gateway && go test ./internal/ -run 'TestResolve' -v`
Expected: 全部 PASS（含原有 TestResolve_* 不回归）。

- [ ] **Step 6: 全量构建与测试**

Run: `cd /Users/linkai/code/costrict-web/gateway && go build ./... && go vet ./... && go test ./...`
Expected: 通过。

- [ ] **Step 7: Commit**

```bash
cd /Users/linkai/code/costrict-web
git add gateway/internal/config.go gateway/internal/endpoint_resolver.go gateway/internal/endpoint_resolver_test.go
git commit -m "feat(gateway): resolve cluster apiBaseURL from Nacos with env fallback

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 2: Gateway — 注册时上报 apiBaseURL

**Files:**
- Modify: `gateway/internal/registration.go:40-58`
- Modify: `gateway/cmd/main.go`

- [ ] **Step 1: 改 Register 客户端**

`gateway/internal/registration.go` 的 `Register` 函数签名加 `apiBaseURL` 参数，body 加字段：

```go
func Register(serverURL, gatewayID, endpoint, internalURL, region, secret string, capacity int, apiBaseURL string) error {
	body := map[string]any{
		"gatewayID":   gatewayID,
		"endpoint":    endpoint,
		"internalURL": internalURL,
		"region":      region,
		"capacity":    capacity,
		"apiBaseURL":  apiBaseURL,
	}
	// ...其余不变
```

- [ ] **Step 2: 改 main.go 解析与传递**

`gateway/cmd/main.go`：

1. 在 endpoint 解析块（第 33-49 行）之后加 apiBaseURL 解析：

```go
	apiBaseURL, err := gw.NewEndpointResolver().ResolveAPIBaseURL(cfg)
	apiSource := "env"
	if gw.NacosAPIBaseURLEnabled(cfg.Nacos) && err == nil {
		apiSource = "nacos"
	}
	if err != nil {
		if errors.Is(err, gw.ErrNacosConfigNotFound) {
			log.Printf("[Gateway] Nacos apiBaseURL config not found (dataId=%s, group=%s), falling back to env", cfg.Nacos.APIBaseURLDataID, cfg.Nacos.Group)
		} else {
			log.Printf("[Gateway] failed to resolve apiBaseURL from Nacos, falling back to env: %v", err)
		}
		apiBaseURL = cfg.APIBaseURL
		apiSource = "env"
	}
	log.Printf("[Gateway] apiBaseURL resolved: source=%s value=%q", apiSource, apiBaseURL)
```

2. `registerWithRetry` 和 `heartbeatLoop` 签名加 `apiBaseURL string`，内部两处 `gw.Register(...)` 调用末尾加 `apiBaseURL` 实参：

```go
func registerWithRetry(cfg *gw.Config, endpoint, apiBaseURL string) {
	for {
		if err := gw.Register(cfg.ServerURL, cfg.GatewayID, endpoint, cfg.InternalURL, cfg.Region, cfg.InternalSecret, cfg.Capacity, apiBaseURL); err != nil {
```

```go
func heartbeatLoop(cfg *gw.Config, manager *gw.TunnelManager, endpoint, apiBaseURL string, stop <-chan struct{}) {
	// ...重注册处同样加 apiBaseURL 实参
```

3. main() 中的调用点同步：

```go
	registerWithRetry(cfg, endpoint, apiBaseURL)

	stopHeartbeat := make(chan struct{})
	go heartbeatLoop(cfg, manager, endpoint, apiBaseURL, stopHeartbeat)
```

- [ ] **Step 3: 构建与测试**

Run: `cd /Users/linkai/code/costrict-web/gateway && go build ./... && go vet ./... && go test ./...`
Expected: 通过。

- [ ] **Step 4: Commit**

```bash
cd /Users/linkai/code/costrict-web
git add gateway/internal/registration.go gateway/cmd/main.go
git commit -m "feat(gateway): report apiBaseURL on registration

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 3: Server — GatewayInfo/注册 handler/store/model 携带 APIBaseURL

**Files:**
- Modify: `server/internal/gateway/types.go:3-11`
- Modify: `server/internal/gateway/handlers.go:51-92`
- Modify: `server/internal/gateway/store_postgres.go`
- Modify: `server/internal/models/models.go:121-133`
- Test: `server/internal/gateway/handlers_test.go`、`server/internal/gateway/store_test.go`

- [ ] **Step 1: 写失败测试**

`server/internal/gateway/handlers_test.go` 追加（参考文件内现有测试的 gin 构造方式）：

```go
func TestGatewayRegisterHandler_WithAPIBaseURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := NewGatewayRegistry(NewMemoryStore(), nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/internal/gateway/register",
		strings.NewReader(`{"gatewayID":"gw-a-0","endpoint":"https://device-a.example.com","internalURL":"http://10.0.0.1:8081","region":"a","capacity":100,"apiBaseURL":"https://api-a.example.com"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	GatewayRegisterHandler(registry)(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	gw := registry.GetGatewayInfo("gw-a-0")
	if gw == nil {
		t.Fatal("gateway not registered")
	}
	if gw.APIBaseURL != "https://api-a.example.com" {
		t.Fatalf("expected APIBaseURL persisted, got %q", gw.APIBaseURL)
	}
}

func TestGatewayRegisterHandler_WithoutAPIBaseURL_BackwardCompatible(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := NewGatewayRegistry(NewMemoryStore(), nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/internal/gateway/register",
		strings.NewReader(`{"gatewayID":"gw-old","endpoint":"https://device.example.com","internalURL":"http://10.0.0.2:8081"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	GatewayRegisterHandler(registry)(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gw := registry.GetGatewayInfo("gw-old"); gw == nil || gw.APIBaseURL != "" {
		t.Fatalf("expected empty APIBaseURL for legacy gateway, got %+v", gw)
	}
}
```

（如文件缺少 `net/http/httptest`、`strings` import 则补上。）

`server/internal/gateway/store_test.go` 追加 MemoryStore 往返测试：

```go
func TestMemoryStore_APIBaseURLRoundTrip(t *testing.T) {
	s := NewMemoryStore()
	info := &GatewayInfo{ID: "gw-1", Endpoint: "https://d.example.com", InternalURL: "http://10.0.0.1:8081", APIBaseURL: "https://api-a.example.com"}
	if err := s.RegisterGateway(info); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetGateway("gw-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.APIBaseURL != info.APIBaseURL {
		t.Fatalf("expected %q, got %q", info.APIBaseURL, got.APIBaseURL)
	}
	list, err := s.ListGateways()
	if err != nil || len(list) != 1 || list[0].APIBaseURL != info.APIBaseURL {
		t.Fatalf("ListGateways lost APIBaseURL: %+v, err=%v", list, err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd /Users/linkai/code/costrict-web/server && go test ./internal/gateway/ -run 'TestGatewayRegisterHandler_WithAPIBaseURL|TestGatewayRegisterHandler_WithoutAPIBaseURL|TestMemoryStore_APIBaseURLRoundTrip' -v`
Expected: 编译失败（`APIBaseURL` 未定义）。

- [ ] **Step 3: 加字段**

1. `server/internal/gateway/types.go` `GatewayInfo` 在 `InternalURL` 后加：

```go
	APIBaseURL    string
```

2. `server/internal/models/models.go` `GatewayRegistry` 在 `InternalURL` 字段后加（显式列名避免依赖 GORM 命名推断）：

```go
	APIBaseURL    string    `gorm:"column:api_base_url;type:text;not null;default:''" json:"apiBaseUrl"`
```

3. `server/internal/gateway/handlers.go` `GatewayRegisterHandler`：body struct 加字段并传递，swagger 注释同步：

```go
		var body struct {
			GatewayID   string `json:"gatewayID" binding:"required"`
			Endpoint    string `json:"endpoint" binding:"required"`
			InternalURL string `json:"internalURL" binding:"required"`
			Region      string `json:"region"`
			Capacity    int    `json:"capacity"`
			APIBaseURL  string `json:"apiBaseURL"`
		}
```

```go
		info := &GatewayInfo{
			ID:            body.GatewayID,
			Endpoint:      body.Endpoint,
			InternalURL:   body.InternalURL,
			Region:        body.Region,
			Capacity:      body.Capacity,
			APIBaseURL:    body.APIBaseURL,
			LastHeartbeat: time.Now().UnixMilli(),
		}
```

swagger 注释第 58 行 `@Param body body object{...}` 改为：

```
// @Param        body  body  object{gatewayID=string,endpoint=string,internalURL=string,region=string,capacity=integer,apiBaseURL=string}  true  "Gateway registration data"
```

4. `server/internal/gateway/store_postgres.go`：
   - `RegisterGateway` 的 `DoUpdates` 列表加 `"api_base_url"`，`Create` 的 struct 字面量加 `APIBaseURL: info.APIBaseURL,`
   - `ListGateways` 与 `GetGateway` 的 `GatewayInfo{...}` 构造加 `APIBaseURL: r.APIBaseURL,`（GetGateway 构造字段名是 `row`，对应 `APIBaseURL: row.APIBaseURL,`）

redis store 与 memory store 无需改动（整结构体 JSON 序列化/指针存储）。

- [ ] **Step 4: 跑测试确认通过**

Run: `cd /Users/linkai/code/costrict-web/server && go test ./internal/gateway/ -v 2>&1 | tail -20`
Expected: 全部 PASS。若 `handlers_test.go` 有 pre-existing gofmt 问题与本改动无关，跳过该文件的 gofmt 检查。

- [ ] **Step 5: 全量构建**

Run: `cd /Users/linkai/code/costrict-web/server && go build ./... && go vet ./internal/gateway/ ./internal/models/`
Expected: 通过。

- [ ] **Step 6: Commit**

```bash
cd /Users/linkai/code/costrict-web
git add server/internal/gateway/types.go server/internal/gateway/handlers.go server/internal/gateway/store_postgres.go server/internal/models/models.go server/internal/gateway/handlers_test.go server/internal/gateway/store_test.go
git commit -m "feat(server): carry gateway apiBaseURL through registry and stores

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 4: Server — 设备信息接口附带 clusterAPIURL

**Files:**
- Modify: `server/internal/handlers/device.go:97-193`
- Modify: `server/cmd/api/main.go`（接线 + 构造顺序）
- Test: `server/internal/handlers/device_cluster_url_test.go`（新建）

**背景接线问题**：`main.go:467-468` 注册设备路由时 `gatewayRegistry`（:600）还不存在。解法：把 store 选择块（:577-593）与 `gatewayRegistry := gateway.NewGatewayRegistry(...)`（:600-608）**移动到 `authed` 路由组注册之前**（deviceSvc 创建之后即可，这些代码只依赖 cfg/db/deviceSvc/log）。leader 选举 goroutine、gatewayClient 等其余代码留在原位置不动。

- [ ] **Step 1: 写失败测试**

新建 `server/internal/handlers/device_cluster_url_test.go`：

```go
package handlers

import (
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/gateway"
)

func setupRegistryWithBinding(t *testing.T, apiBaseURL string) *gateway.GatewayRegistry {
	t.Helper()
	registry := gateway.NewGatewayRegistry(gateway.NewMemoryStore(), nil)
	info := &gateway.GatewayInfo{
		ID:            "gw-a-0",
		Endpoint:      "https://device-a.example.com",
		InternalURL:   "http://10.0.0.1:8081",
		APIBaseURL:    apiBaseURL,
		// ListLiveGateways 按 LastHeartbeat 过滤，必须设置为当前时间否则 gateway 不在存活列表中
		LastHeartbeat: time.Now().UnixMilli(),
	}
	if err := registry.Register(info); err != nil {
		t.Fatal(err)
	}
	registry.BindDevice("dev-1", "gw-a-0", "conn-1")
	return registry
}

func TestClusterAPIURLFor_BoundDevice(t *testing.T) {
	registry := setupRegistryWithBinding(t, "https://api-a.example.com")
	got := clusterAPIURLFor(registry, liveGatewayAPIMap(registry), "dev-1")
	if got != "https://api-a.example.com" {
		t.Fatalf("expected cluster URL, got %q", got)
	}
}

func TestClusterAPIURLFor_UnboundDevice(t *testing.T) {
	registry := setupRegistryWithBinding(t, "https://api-a.example.com")
	if got := clusterAPIURLFor(registry, liveGatewayAPIMap(registry), "dev-unknown"); got != "" {
		t.Fatalf("expected empty for unbound device, got %q", got)
	}
}

func TestClusterAPIURLFor_GatewayWithoutAPIBaseURL(t *testing.T) {
	registry := setupRegistryWithBinding(t, "")
	if got := clusterAPIURLFor(registry, liveGatewayAPIMap(registry), "dev-1"); got != "" {
		t.Fatalf("expected empty when gateway has no apiBaseURL, got %q", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd /Users/linkai/code/costrict-web/server && go test ./internal/handlers/ -run TestClusterAPIURLFor -v`
Expected: 编译失败（`clusterAPIURLFor`、`liveGatewayAPIMap` 未定义）。

- [ ] **Step 3: 实现辅助函数与接口增强**

`server/internal/handlers/device.go`：

1. import 加（注意与既有 import 合并）：

```go
	"github.com/costrict/costrict-web/server/internal/gateway"
```

2. 在 `deviceToMap` 后加：

```go
// liveGatewayAPIMap 返回 gatewayID -> apiBaseURL 映射（仅含存活且上报了 apiBaseURL 的网关）。
func liveGatewayAPIMap(registry *gateway.GatewayRegistry) map[string]string {
	m := map[string]string{}
	if registry == nil {
		return m
	}
	gateways, err := registry.ListLiveGateways()
	if err != nil {
		return m
	}
	for _, gw := range gateways {
		if gw.APIBaseURL != "" {
			m[gw.ID] = gw.APIBaseURL
		}
	}
	return m
}

// clusterAPIURLFor 返回设备归属集群的 Server 公网 API 地址；未绑定或未上报时返回空串。
func clusterAPIURLFor(registry *gateway.GatewayRegistry, gwAPI map[string]string, deviceID string) string {
	if registry == nil {
		return ""
	}
	gwID := registry.GetDeviceGatewayID(deviceID)
	if gwID == "" {
		return ""
	}
	return gwAPI[gwID]
}
```

3. `ListDevicesHandler` 签名改为 `func ListDevicesHandler(svc *services.DeviceService, updateSvc *services.UpdateService, registry *gateway.GatewayRegistry) gin.HandlerFunc`，函数体在 `releasesMap` 行后加 map 构建、循环里加字段：

```go
		releasesMap, _ := updateSvc.GetLatestReleasesMap()
		gwAPI := liveGatewayAPIMap(registry)

		for _, d := range devices {
			item := deviceToMap(d)

			if url := clusterAPIURLFor(registry, gwAPI, d.DeviceID); url != "" {
				item["clusterAPIURL"] = url
			} else {
				item["clusterAPIURL"] = nil
			}
			// ...canUpdate 逻辑不变
```

4. `GetDeviceHandler` 签名改为 `func GetDeviceHandler(svc *services.DeviceService, registry *gateway.GatewayRegistry) gin.HandlerFunc`，返回处由直接返回 `device` 改为带 clusterAPIURL 的 map：

```go
		item := deviceToMap(*device)
		gwAPI := liveGatewayAPIMap(registry)
		if url := clusterAPIURLFor(registry, gwAPI, device.DeviceID); url != "" {
			item["clusterAPIURL"] = url
		} else {
			item["clusterAPIURL"] = nil
		}
		c.JSON(http.StatusOK, gin.H{"device": item})
```

（`svc.GetDevice` 返回值若不是指针则去掉 `*`——以实际签名为准调整 `deviceToMap(*device)`/`deviceToMap(device)`。）

- [ ] **Step 4: 改 main.go 接线**

1. 把 `server/cmd/api/main.go` 中 store 选择块（约 :577-593，`var redisClient *redis.Client` 到 `}` 的整个 if/else）与 `gatewayRegistry := gateway.NewGatewayRegistry(store, func(deviceIDs []string) {...})`（约 :600-608）**剪切**到设备路由注册之前（`deviceSvc` 创建之后、`api` 路由组注册开始之前）。注意 `recommendHandler.SetBehaviorRateLimiter(redisClient, 30)` 一行**留在原位置**（它不属于 store 选择块，但依赖 redisClient 变量——移动后仍在作用域内，无需改动）。

2. 路由注册改签名：

```go
			devices.GET("", handlers.ListDevicesHandler(deviceSvc, updateSvc, gatewayRegistry))
			devices.GET("/:deviceID", handlers.GetDeviceHandler(deviceSvc, gatewayRegistry))
```

3. 检查 main.go 内其他 `ListDevicesHandler`/`GetDeviceHandler` 调用点（应只有这两处）并同步。

- [ ] **Step 5: 跑测试确认通过**

Run: `cd /Users/linkai/code/costrict-web/server && go build ./... && go test ./internal/handlers/ -run TestClusterAPIURLFor -v`
Expected: 3 个测试 PASS。

- [ ] **Step 6: 全量验证**

Run: `cd /Users/linkai/code/costrict-web/server && go vet ./internal/handlers/ ./cmd/api/ && go test ./internal/handlers/ 2>&1 | tail -5`
Expected: 通过（若该包存在 pre-existing 测试失败，确认与本次改动无关并在报告中说明）。

- [ ] **Step 7: Commit**

```bash
cd /Users/linkai/code/costrict-web
git add server/internal/handlers/device.go server/internal/handlers/device_cluster_url_test.go server/cmd/api/main.go
git commit -m "feat(server): expose device clusterAPIURL in device list/detail APIs

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 5: Chart — values 与模板注入 GATEWAY_API_BASE_URL

**Files:**
- Modify: `deploy/charts/gateway/values.yaml:66-96`
- Modify: `deploy/charts/gateway/templates/deployment.yaml`、`daemonset.yaml`、`statefulset.yaml`

- [ ] **Step 1: values.yaml**

`config` 块 `endpoint: ""` 后加：

```yaml
  # 本集群 Server 的公网 API 地址（如 https://api-a.example.com）。
  # 多集群（共享数据库、域名不同）部署时必填：Gateway 注册时上报，
  # Web 前端据此把设备请求路由到设备归属集群。单集群部署留空即可。
  apiBaseURL: ""
```

`config.nacos` 块 `dataID:` 行后加：

```yaml
    apiBaseURLDataID: ""  # optional: data ID holding this cluster's Server public API URL
```

- [ ] **Step 2: 三个模板加 env**

在 `deployment.yaml`、`daemonset.yaml`、`statefulset.yaml` 中，`- name: GATEWAY_ENDPOINT` 的 value 行之后统一加：

```yaml
            - name: GATEWAY_API_BASE_URL
              value: {{ .Values.config.apiBaseURL | quote }}
```

并在 `{{- if .Values.config.nacos.enabled }}` 块内 `- name: GATEWAY_NACOS_DATA_ID` 的 value 行之后加：

```yaml
            - name: GATEWAY_NACOS_API_BASE_URL_DATA_ID
              value: {{ .Values.config.nacos.apiBaseURLDataID | quote }}
```

- [ ] **Step 3: 渲染验证**

Run:

```bash
cd /Users/linkai/code/costrict-web
helm lint deploy/charts/gateway
helm template test deploy/charts/gateway --set config.apiBaseURL=https://api-a.example.com --set config.nacos.enabled=true --set config.nacos.serverAddr=nacos:8848 --set config.nacos.dataID=gw-endpoint --set config.nacos.apiBaseURLDataID=gw-api-base | grep -A1 "GATEWAY_API_BASE_URL\|GATEWAY_NACOS_API_BASE_URL_DATA_ID"
```

Expected: lint 通过；渲染结果中 `GATEWAY_API_BASE_URL` value 为 `"https://api-a.example.com"`，`GATEWAY_NACOS_API_BASE_URL_DATA_ID` value 为 `"gw-api-base"`。

- [ ] **Step 4: Commit**

```bash
cd /Users/linkai/code/costrict-web
git add deploy/charts/gateway/values.yaml deploy/charts/gateway/templates/deployment.yaml deploy/charts/gateway/templates/daemonset.yaml deploy/charts/gateway/templates/statefulset.yaml
git commit -m "feat(gateway-chart): inject GATEWAY_API_BASE_URL and Nacos data ID env

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 6: Frontend — cluster 缓存 + getProxyUrl + normalizeDevice 透传

**Files:**
- Modify: `portal/packages/app-ai-native/src/pages/workspace/lib/url.ts`
- Modify: `portal/packages/app-ai-native/src/pages/workspace/types.ts:5-24`
- Modify: `portal/packages/app-ai-native/src/pages/workspace/lib/api.ts:47-87`
- Modify: `portal/packages/app-ai-native/src/pages/store/lib/api.ts:375,432`（DeviceResponse/normalizeDevice）
- Test: `portal/packages/app-ai-native/src/pages/workspace/lib/url.test.ts`（新建）

- [ ] **Step 1: 写失败测试**

新建 `portal/packages/app-ai-native/src/pages/workspace/lib/url.test.ts`：

```ts
import { describe, expect, test } from "bun:test"
import { getProxyUrl, setDeviceClusterAPIURL } from "./url"

describe("getProxyUrl", () => {
  test("defaults to APP_URL when no cluster override", () => {
    setDeviceClusterAPIURL("dev-none", null)
    expect(getProxyUrl("dev-none")).toBe("http://127.0.0.1:3000/cloud/device/dev-none/proxy")
  })

  test("uses cluster API URL when set", () => {
    setDeviceClusterAPIURL("dev-a", "https://api-a.example.com")
    expect(getProxyUrl("dev-a")).toBe("https://api-a.example.com/cloud/device/dev-a/proxy")
    setDeviceClusterAPIURL("dev-a", null)
  })

  test("clearing override falls back to APP_URL", () => {
    setDeviceClusterAPIURL("dev-b", "https://api-b.example.com")
    setDeviceClusterAPIURL("dev-b", null)
    expect(getProxyUrl("dev-b")).toBe("http://127.0.0.1:3000/cloud/device/dev-b/proxy")
  })
})
```

Run: `cd /Users/linkai/code/costrict-web/portal/packages/app-ai-native && bun test src/pages/workspace/lib/url.test.ts`
Expected: FAIL（`setDeviceClusterAPIURL` 未导出）。

- [ ] **Step 2: 改 url.ts（加缓存）**

整个文件改为：

```ts
import { env } from "@/lib/env"

// deviceId -> 设备归属集群的 Server 公网 API 地址。
// 由设备列表/详情接口响应（normalizeDevice）填充；无记录时视为本集群设备。
const clusterCache = new Map<string, string>()

/** 记录/清除设备归属集群的 API 地址（url 为空时清除记录）。 */
export function setDeviceClusterAPIURL(deviceId: string, url: string | null | undefined) {
    if (url) clusterCache.set(deviceId, url)
    else clusterCache.delete(deviceId)
}

export function getProxyUrl(deviceId: string) {
    const base = clusterCache.get(deviceId) || env.APP_URL
    return `${base}/cloud/device/${deviceId}/proxy`
}
```

Run 同上测试。Expected: 3 个 PASS。

- [ ] **Step 3: types.ts 加字段**

`src/pages/workspace/types.ts` `Device` interface `latestVersion?: string` 行后加：

```ts
  clusterAPIURL?: string | null
```

- [ ] **Step 4: workspace/lib/api.ts 透传并填充缓存**

1. 文件头部 import 加：

```ts
import { setDeviceClusterAPIURL } from "./url"
```

2. `DeviceResponse`（:47-65）`latestVersion?: string | null` 行后加：

```ts
  clusterAPIURL?: string | null
```

3. `normalizeDevice`（:67-87）函数体开头加缓存填充，返回值加字段：

```ts
function normalizeDevice(device: DeviceResponse): Device {
  setDeviceClusterAPIURL(device.deviceId, device.clusterAPIURL)
  return {
    // ...既有字段不变
    latestVersion: device.latestVersion ?? undefined,
    clusterAPIURL: device.clusterAPIURL ?? undefined,
    // createdAt/updatedAt 不变
```

- [ ] **Step 5: store/lib/api.ts 透传并填充缓存**

1. 文件头部 import 加（该文件已有大量 import，注意别名冲突）：

```ts
import { setDeviceClusterAPIURL } from "@/pages/workspace/lib/url"
```

2. 其 `DeviceResponse`（:375）与 `normalizeDevice`（:432）做与 Step 4 相同的字段透传 + `setDeviceClusterAPIURL(device.deviceId, device.clusterAPIURL)` 调用。

- [ ] **Step 6: 前端测试与类型检查**

Run:

```bash
cd /Users/linkai/code/costrict-web/portal/packages/app-ai-native
bun test src/pages/workspace/lib/url.test.ts
bun run typecheck 2>&1 | tail -5 || true
```

Expected: url 测试 PASS；typecheck 无新增错误（若无 typecheck script 则用 `bunx tsc --noEmit`，pre-existing 错误记录说明即可）。

- [ ] **Step 7: Commit**

```bash
cd /Users/linkai/code/costrict-web
git add portal/packages/app-ai-native/src/pages/workspace/lib/url.ts portal/packages/app-ai-native/src/pages/workspace/lib/url.test.ts portal/packages/app-ai-native/src/pages/workspace/types.ts portal/packages/app-ai-native/src/pages/workspace/lib/api.ts portal/packages/app-ai-native/src/pages/store/lib/api.ts
git commit -m "feat(portal): route device proxy URLs via cluster-aware cache

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 7: Frontend — 失效重试 + store 命令调用走 getProxyUrl

**Files:**
- Create: `portal/packages/app-ai-native/src/pages/workspace/lib/cluster-retry.ts`
- Modify: `portal/packages/app-ai-native/src/pages/workspace/lib/cloud-device-api.ts`
- Modify: `portal/packages/app-ai-native/src/pages/store/lib/api.ts:1765-1780`（sendCommand/getCommandStatus）
- Test: `portal/packages/app-ai-native/src/pages/workspace/lib/cluster-retry.test.ts`（新建）

- [ ] **Step 1: 写失败测试**

新建 `cluster-retry.test.ts`：

```ts
import { describe, expect, test } from "bun:test"
import { withClusterRetry } from "./cluster-retry"

describe("withClusterRetry", () => {
  test("retries once after refreshing route on 'device not connected'", async () => {
    let calls = 0
    let refreshed = ""
    const fn = async () => {
      calls++
      if (calls === 1) throw new Error("device not connected")
      return "ok"
    }
    const result = await withClusterRetry("dev-1", fn, async (id) => { refreshed = id })
    expect(result).toBe("ok")
    expect(calls).toBe(2)
    expect(refreshed).toBe("dev-1")
  })

  test("does not retry on unrelated errors", async () => {
    let calls = 0
    const fn = async () => { calls++; throw new Error("permission denied") }
    await expect(withClusterRetry("dev-1", fn, async () => {})).rejects.toThrow("permission denied")
    expect(calls).toBe(1)
  })

  test("propagates error if retry also fails", async () => {
    const fn = async () => { throw new Error("device not connected") }
    await expect(withClusterRetry("dev-1", fn, async () => {})).rejects.toThrow("device not connected")
  })
})
```

Run: `cd /Users/linkai/code/costrict-web/portal/packages/app-ai-native && bun test src/pages/workspace/lib/cluster-retry.test.ts`
Expected: FAIL（模块不存在）。

- [ ] **Step 2: 新建 cluster-retry.ts**

```ts
import { deviceApi } from "./api"

const STALE_ROUTE_RE = /device not connected/i

/** 重新拉取设备详情，normalizeDevice 会顺带刷新 cluster 缓存。 */
export async function refreshClusterAPIURL(deviceId: string): Promise<void> {
  await deviceApi.get(deviceId)
}

/**
 * 设备路由失效重试：fn 因"device not connected"失败时，
 * 刷新该设备的 clusterAPIURL 缓存后重试一次（覆盖设备换集群场景）。
 */
export async function withClusterRetry<T>(
  deviceId: string,
  fn: () => Promise<T>,
  refresh: (deviceId: string) => Promise<void> = refreshClusterAPIURL,
): Promise<T> {
  try {
    return await fn()
  } catch (err) {
    if (!(err instanceof Error) || !STALE_ROUTE_RE.test(err.message)) throw err
    await refresh(deviceId)
    return await fn()
  }
}
```

Run 同上测试。Expected: 3 个 PASS。

注意：`workspace/lib/api.ts` 的 `deviceApi.get` 必须确实经过 `normalizeDevice`（现状 :165-168 是的，Task 6 已加缓存填充）。若发现 get 未走 normalizeDevice，先修正再走下一步。

- [ ] **Step 3: cloud-device-api.ts 包装导出函数**

文件头部 import 加：

```ts
import { withClusterRetry } from "./cluster-retry"
```

把文件中**所有以 deviceId 为首参的导出 async 函数**的函数体用 `withClusterRetry(deviceId, () => ...)` 包一层。模式（以 `checkRuntimeConfig` 为例，其余函数照此逐一包装）：

```ts
export async function checkRuntimeConfig(deviceId: string): Promise<{ supported: boolean; ... }> {
  return withClusterRetry(deviceId, async () => {
    // ...原函数体不变
  })
}
```

（函数较多的文件，逐一包装，保持原逻辑一字不动，只加包装。）

- [ ] **Step 4: store/lib/api.ts 命令调用走 getProxyUrl + 重试**

1. import 加：

```ts
import { getProxyUrl } from "@/pages/workspace/lib/url"
import { withClusterRetry } from "@/pages/workspace/lib/cluster-retry"
```

2. `updateApi.sendCommand`（约 :1765）改为：

```ts
  sendCommand: async (deviceId: string, cmd: DeviceCommandRequest) =>
    withClusterRetry(deviceId, async () => {
      const res = await fetch(`${getProxyUrl(deviceId)}/api/v1/commands`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(cmd),
      })
      if (!res.ok) {
        if (res.status === 401) onUnauthorized(`/cloud/device/${deviceId}/proxy/api/v1/commands`)
        const err = await res.json().catch(() => ({ error: res.statusText }))
        throw new Error(err.error || `Request failed: ${res.status}`)
      }
      return (await res.json()) as DeviceCommandAck
    }),
```

（`onUnauthorized` 若该文件未 import 则从 `@/lib/session-expired` 引入；若原 apiFetch 行为有差异以保持原错误处理为准。）

3. `getCommandStatus`（约 :1771）中的 URL 由 `` `${API_BASE}/cloud/device/${deviceId}/proxy/api/v1/commands/status?...` `` 改为 `` `${getProxyUrl(deviceId)}/api/v1/commands/status?...` ``，并同样用 `withClusterRetry(deviceId, ...)` 包装（保留 404 返回 null 的既有语义——404 不抛错所以不会触发重试，正确）。

- [ ] **Step 5: 测试与类型检查**

Run:

```bash
cd /Users/linkai/code/costrict-web/portal/packages/app-ai-native
bun test src/pages/workspace/lib/cluster-retry.test.ts src/pages/workspace/lib/url.test.ts
bunx tsc --noEmit 2>&1 | tail -10 || true
```

Expected: 测试 PASS；无新增类型错误。

- [ ] **Step 6: Commit**

```bash
cd /Users/linkai/code/costrict-web
git add portal/packages/app-ai-native/src/pages/workspace/lib/cluster-retry.ts portal/packages/app-ai-native/src/pages/workspace/lib/cluster-retry.test.ts portal/packages/app-ai-native/src/pages/workspace/lib/cloud-device-api.ts portal/packages/app-ai-native/src/pages/store/lib/api.ts
git commit -m "feat(portal): retry once with refreshed cluster route on stale device binding

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## 收尾验证（全部 Task 完成后）

- [ ] `cd /Users/linkai/code/costrict-web/gateway && go build ./... && go test ./...`
- [ ] `cd /Users/linkai/code/costrict-web/server && go build ./... && go test ./internal/gateway/ ./internal/handlers/`
- [ ] `cd /Users/linkai/code/costrict-web/portal/packages/app-ai-native && bun test src/pages/workspace/lib/`
- [ ] `helm lint deploy/charts/gateway`
- [ ] 单集群兼容人工确认：不配 `apiBaseURL` 时，注册请求 `apiBaseURL` 为空串 → 设备接口 `clusterAPIURL: null` → 前端缓存无记录 → `getProxyUrl` 行为与改动前完全一致。

## 部署侧待办（不在本计划代码内，交付时执行）

1. 每集群 Nacos 发布 apiBaseURL dataId（纯文本 URL），或 chart value `config.apiBaseURL` 填值。
2. 两集群 Server 的 `CORS_ALLOWED_ORIGINS` 互加对方 Web 域名。
3. 数据库 migration 由 AutoMigrate 完成（`cmd/migrate` / `cmd/worker` 启动时自动加 `api_base_url` 列），无需手工 DDL。
4. 验证点（实施时注意）：`cloud-terminal-api.ts` 的 `credentials: "include"` 是否依赖 cookie 鉴权——若依赖需改 Authorization 头，否则跨域终端会 401。Task 6/7 实现者检查该文件鉴权方式并在报告中说明结论；如需改造，作为 Task 7 的一部分顺带完成（把 token 放入 Authorization 头）。
