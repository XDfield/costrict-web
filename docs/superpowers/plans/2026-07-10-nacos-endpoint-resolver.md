# Nacos Endpoint Resolver Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the costrict-web Gateway's `endpoint` configuration support Nacos as a dynamic source: if Nacos is configured, resolve `endpoint` from Nacos first; otherwise fall back to the `GATEWAY_ENDPOINT` environment variable.

**Architecture:** Introduce a small `EndpointResolver` in the Gateway that fetches a configured data ID from a Nacos server on startup via the Nacos Open API (HTTP), then uses that value as the Gateway's public endpoint when registering with the API server. The resolver is configured through new environment variables and is completely optional: existing deployments without Nacos continue to work unchanged. We use the Nacos HTTP Open API directly to avoid the heavy/vulnerable `nacos-sdk-go` dependency.

**Tech Stack:** Go 1.23 standard library (`net/http`), existing Gateway config loader in `gateway/internal/config.go`.

---

## File Structure

| File | Responsibility |
|------|----------------|
| `gateway/internal/config.go` | Add Nacos config fields and load them from environment; keep `Endpoint` fallback to env var. |
| `gateway/internal/endpoint_resolver.go` | New file: resolve endpoint from Nacos HTTP API, with env-var fallback and caching. |
| `gateway/internal/endpoint_resolver_test.go` | New file: unit tests for the resolver (mock Nacos HTTP server + fallback behavior). |
| `gateway/cmd/main.go` | Call the resolver before registration; use resolved endpoint when calling `gw.Register`. |
| `deploy/charts/gateway/values.yaml` | Add optional Nacos configuration values. |
| `deploy/charts/gateway/templates/deployment.yaml` | Pass Nacos env vars to container when configured. |
| `docs/superpowers/plans/2026-07-10-nacos-endpoint-resolver.md` | This plan. |

---

## Task 1: Verify clean baseline (no new dependency needed)

We will use the Nacos Open API via Go's standard `net/http`, so no external SDK dependency is required. This task simply confirms the workspace is clean after reverting the SDK attempt.

**Files:**
- None to modify.

- [ ] **Step 1: Verify go.mod/go.sum are clean**

Run:
```bash
cd /Users/linkai/code/costrict-web/.claude/worktrees/nacos-endpoint-resolver/gateway
git diff go.mod go.sum
```

Expected: no Nacos SDK entries.

- [ ] **Step 2: Verify build still works**

Run:
```bash
cd /Users/linkai/code/costrict-web/.claude/worktrees/nacos-endpoint-resolver/gateway
go build ./...
```

Expected: success, no errors.

- [ ] **Step 3: Mark task complete**

No commit needed for this task; it is a verification checkpoint.

---

## Task 2: Add Nacos config fields and env-var loading

**Files:**
- Modify: `gateway/internal/config.go`

- [ ] **Step 1: Add NacosConfig struct and resolver fields**

Modify `gateway/internal/config.go`:

```go
type Config struct {
	GatewayID      string // 网关唯一标识符，用于在 API 服务中注册和识别
	Port           string // 网关服务监听端口
	Endpoint       string // 网关外部访问地址，客户端通过此地址建立 WebSocket 隧道连接
	InternalURL    string // 网关内部访问地址，API 服务通过此地址代理请求到设备
	Region         string // 网关所属区域，用于就近分配和区域隔离
	Capacity       int    // 网关最大连接容量，超过容量后不再分配新设备
	ServerURL      string // costrict-web-api 服务地址，用于注册和心跳
	InternalSecret string // 与 API 服务通信的共享密钥，用于内部接口认证
	Nacos          NacosConfig
}

// NacosConfig configures dynamic endpoint resolution via Nacos.
// When ServerAddr and DataID are non-empty, the gateway will fetch the
// endpoint from Nacos and prefer it over the GATEWAY_ENDPOINT env var.
type NacosConfig struct {
	ServerAddr  string // e.g. "nacos-headless.nacos.svc.cluster.local:8848"
	NamespaceID string // empty for public namespace
	Group       string // defaults to "DEFAULT_GROUP"
	DataID      string // required to enable Nacos lookup
	TimeoutMs   uint64 // request timeout, defaults to 5000
}
```

- [ ] **Step 2: Load Nacos env vars in LoadConfig**

Update `LoadConfig` to populate `Nacos` fields:

```go
func LoadConfig() *Config {
	loadEnvFile(".env")

	return &Config{
		GatewayID:      getEnv("GATEWAY_ID", "gw-01"),
		Port:           getEnv("GATEWAY_PORT", "8081"),
		Endpoint:       getEnv("GATEWAY_ENDPOINT", "http://localhost:8081"),
		InternalURL:    getEnv("GATEWAY_INTERNAL_URL", "http://localhost:8081"),
		Region:         getEnv("GATEWAY_REGION", "default"),
		Capacity:       getEnvInt("GATEWAY_CAPACITY", 1000),
		ServerURL:      getEnv("SERVER_URL", "http://localhost:8080"),
		InternalSecret: getEnv("INTERNAL_SECRET", ""),
		Nacos: NacosConfig{
			ServerAddr:  getEnv("GATEWAY_NACOS_SERVER_ADDR", ""),
			NamespaceID: getEnv("GATEWAY_NACOS_NAMESPACE_ID", ""),
			Group:       getEnv("GATEWAY_NACOS_GROUP", "DEFAULT_GROUP"),
			DataID:      getEnv("GATEWAY_NACOS_DATA_ID", ""),
			TimeoutMs:   getEnvUint64("GATEWAY_NACOS_TIMEOUT_MS", 5000),
		},
	}
}
```

Add helper `getEnvUint64`:

```go
func getEnvUint64(key string, defaultValue uint64) uint64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return defaultValue
}
```

- [ ] **Step 3: Verify build**

Run:
```bash
cd /Users/linkai/code/costrict-web/gateway
go build ./...
```

Expected: success.

- [ ] **Step 4: Commit**

```bash
git add gateway/internal/config.go
git commit -m "feat(gateway): add Nacos config fields"
```

---

## Task 3: Implement EndpointResolver with Nacos fallback

**Files:**
- Create: `gateway/internal/endpoint_resolver.go`
- Create: `gateway/internal/endpoint_resolver_test.go`

- [ ] **Step 1: Write the resolver interface and implementation**

Create `gateway/internal/endpoint_resolver.go`:

```go
package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// EndpointResolver resolves the public endpoint the gateway should register.
// If Nacos is configured, it fetches the configured data ID; otherwise it
// returns the statically configured endpoint.
type EndpointResolver interface {
	Resolve(cfg *Config) (string, error)
}

// NewEndpointResolver returns the default resolver.
func NewEndpointResolver() EndpointResolver {
	return &defaultEndpointResolver{client: http.DefaultClient}
}

type defaultEndpointResolver struct {
	client *http.Client
}

func (r *defaultEndpointResolver) Resolve(cfg *Config) (string, error) {
	if cfg == nil {
		return "", errors.New("config is nil")
	}

	if !nacosEnabled(cfg.Nacos) {
		return cfg.Endpoint, nil
	}

	endpoint, err := resolveFromNacos(r.client, cfg.Nacos)
	if err != nil {
		return "", fmt.Errorf("resolve endpoint from Nacos failed: %w", err)
	}

	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", errors.New("Nacos returned empty endpoint")
	}

	return endpoint, nil
}

func nacosEnabled(n NacosConfig) bool {
	return n.ServerAddr != "" && n.DataID != ""
}

func resolveFromNacos(client *http.Client, n NacosConfig) (string, error) {
	serverAddr := n.ServerAddr
	if !strings.Contains(serverAddr, "://") {
		serverAddr = "http://" + serverAddr
	}
	if !strings.Contains(serverAddr, ":") {
		serverAddr = serverAddr + ":8848"
	}

	base, err := url.Parse(serverAddr)
	if err != nil {
		return "", fmt.Errorf("invalid nacos server addr %q: %w", n.ServerAddr, err)
	}

	q := base.Query()
	q.Set("dataId", n.DataID)
	q.Set("group", n.Group)
	if n.NamespaceID != "" {
		q.Set("tenant", n.NamespaceID)
	}
	base.Path = "/nacos/v1/cs/configs"
	base.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(n.TimeoutMs)*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return "", fmt.Errorf("build nacos request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request nacos config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("nacos returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read nacos response: %w", err)
	}

	return string(body), nil
}
```

- [ ] **Step 2: Write tests for fallback and Nacos HTTP behavior**

Create `gateway/internal/endpoint_resolver_test.go`:

```go
package internal

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDefaultEndpointResolver_NoNacos(t *testing.T) {
	r := NewEndpointResolver()
	cfg := &Config{Endpoint: "wss://cluster-a.example.com/device"}

	got, err := r.Resolve(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != cfg.Endpoint {
		t.Fatalf("expected %q, got %q", cfg.Endpoint, got)
	}
}

func TestDefaultEndpointResolver_NilConfig(t *testing.T) {
	r := NewEndpointResolver()
	if _, err := r.Resolve(nil); err == nil {
		t.Fatal("expected error for nil config")
	}
}

func TestDefaultEndpointResolver_FromNacos(t *testing.T) {
	wantEndpoint := "wss://cluster-b.example.com/device"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/nacos/v1/cs/configs" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("dataId"); got != "gateway-endpoint" {
			t.Fatalf("unexpected dataId: %s", got)
		}
		if got := r.URL.Query().Get("group"); got != "DEFAULT_GROUP" {
			t.Fatalf("unexpected group: %s", got)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(wantEndpoint))
	}))
	defer server.Close()

	r := NewEndpointResolver()
	cfg := &Config{
		Endpoint: "wss://cluster-a.example.com/device",
		Nacos: NacosConfig{
			ServerAddr: server.URL,
			DataID:     "gateway-endpoint",
			Group:      "DEFAULT_GROUP",
			TimeoutMs:  5000,
		},
	}

	got, err := r.Resolve(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantEndpoint {
		t.Fatalf("expected %q, got %q", wantEndpoint, got)
	}
}

func TestDefaultEndpointResolver_NacosError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("config not found"))
	}))
	defer server.Close()

	r := NewEndpointResolver()
	cfg := &Config{
		Endpoint: "wss://cluster-a.example.com/device",
		Nacos: NacosConfig{
			ServerAddr: server.URL,
			DataID:     "missing",
			Group:      "DEFAULT_GROUP",
			TimeoutMs:  5000,
		},
	}

	if _, err := r.Resolve(cfg); err == nil {
		t.Fatal("expected error for Nacos 404")
	}
}
```

- [ ] **Step 3: Run tests**

Run:
```bash
cd /Users/linkai/code/costrict-web/.claude/worktrees/nacos-endpoint-resolver/gateway
go test ./internal/ -v
```

Expected: all tests pass.

- [ ] **Step 4: Commit**

```bash
git add gateway/internal/endpoint_resolver.go gateway/internal/endpoint_resolver_test.go
git commit -m "feat(gateway): add endpoint resolver with Nacos fallback"
```

---

## Task 4: Integrate resolver into gateway startup

**Files:**
- Modify: `gateway/cmd/main.go`

- [ ] **Step 1: Resolve endpoint before registration**

Update `main()` in `gateway/cmd/main.go`:

```go
func main() {
	logger.Init(logger.Config{...})
	gin.DefaultWriter = logger.GinWriter()
	gin.DefaultErrorWriter = logger.GinErrorWriter()

	cfg := gw.LoadConfig()
	manager := gw.NewTunnelManager()

	endpoint, err := gw.NewEndpointResolver().Resolve(cfg)
	if err != nil {
		log.Fatalf("[Gateway] failed to resolve endpoint: %v", err)
	}
	log.Printf("[Gateway] resolved endpoint: %s", endpoint)

	r := gw.SetupRouter(manager, cfg)
	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
	}

	go func() {
		log.Printf("[Gateway] starting on port %s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[Gateway] failed to start: %v", err)
		}
	}()

	registerWithRetry(cfg, endpoint)
	...
}
```

Update `registerWithRetry` signature and call:

```go
func registerWithRetry(cfg *gw.Config, endpoint string) {
	for {
		if err := gw.Register(cfg.ServerURL, cfg.GatewayID, endpoint, cfg.InternalURL, cfg.Region, cfg.InternalSecret, cfg.Capacity); err != nil {
			log.Printf("[Gateway] register failed, retrying in 5s: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		log.Printf("[Gateway] registered with server %s as %s (endpoint=%s)", cfg.ServerURL, cfg.GatewayID, endpoint)
		return
	}
}
```

And update the re-register call inside `heartbeatLoop`:

```go
if regErr := gw.Register(cfg.ServerURL, cfg.GatewayID, endpoint, cfg.InternalURL, cfg.Region, cfg.InternalSecret, cfg.Capacity); regErr != nil {
```

- [ ] **Step 2: Verify build**

Run:
```bash
cd /Users/linkai/code/costrict-web/gateway
go build ./...
```

Expected: success.

- [ ] **Step 3: Commit**

```bash
git add gateway/cmd/main.go
git commit -m "feat(gateway): resolve endpoint before registration"
```

---

## Task 5: Add Helm values and env vars

**Files:**
- Modify: `deploy/charts/gateway/values.yaml`
- Modify: `deploy/charts/gateway/templates/deployment.yaml`

- [ ] **Step 1: Add Nacos values**

Update `deploy/charts/gateway/values.yaml` under `config`:

```yaml
config:
  port: "8081"
  serverUrl: "http://costrict-web-api:8080"
  gatewayId: ""
  gatewayIdPrefix: ""
  endpoint: ""
  internalUrl: ""
  region: "default"
  capacity: 100

  # Nacos dynamic endpoint configuration.
  # When serverAddr and dataID are set, the gateway reads endpoint from Nacos
  # instead of the static `endpoint` value above. Each cluster can publish a
  # different endpoint value in its own Nacos to steer clients to that cluster.
  nacos:
    enabled: false
    serverAddr: ""          # e.g. nacos-headless.nacos.svc.cluster.local:8848
    namespaceId: ""         # optional namespace ID
    group: "DEFAULT_GROUP"  # config group
    dataID: ""              # config data ID
    timeoutMs: 5000
```

- [ ] **Step 2: Pass env vars to container**

Update `deploy/charts/gateway/templates/deployment.yaml` (and similarly `daemonset.yaml`) to inject Nacos env vars:

```yaml
            - name: GATEWAY_ENDPOINT
              value: {{ .Values.config.endpoint | quote }}
            {{- if .Values.config.nacos.enabled }}
            - name: GATEWAY_NACOS_SERVER_ADDR
              value: {{ .Values.config.nacos.serverAddr | quote }}
            - name: GATEWAY_NACOS_NAMESPACE_ID
              value: {{ .Values.config.nacos.namespaceId | quote }}
            - name: GATEWAY_NACOS_GROUP
              value: {{ .Values.config.nacos.group | quote }}
            - name: GATEWAY_NACOS_DATA_ID
              value: {{ .Values.config.nacos.dataID | quote }}
            - name: GATEWAY_NACOS_TIMEOUT_MS
              value: {{ .Values.config.nacos.timeoutMs | quote }}
            {{- end }}
```

- [ ] **Step 3: Verify Helm template**

Run:
```bash
cd /Users/linkai/code/costrict-web
helm template costrict-web-gateway ./deploy/charts/gateway \
  --set config.nacos.enabled=true \
  --set config.nacos.serverAddr="nacos:8848" \
  --set config.nacos.dataID="gateway-endpoint" \
  --show-only templates/deployment.yaml | grep -A 16 "GATEWAY_ENDPOINT"
```

Expected: Nacos env vars rendered.

- [ ] **Step 4: Commit**

```bash
git add deploy/charts/gateway/values.yaml deploy/charts/gateway/templates/deployment.yaml
git commit -m "feat(gateway): expose Nacos endpoint config in Helm chart"
```

---

## Task 6: Add integration/smoke verification

**Files:**
- None (manual verification)

- [ ] **Step 1: Run gateway tests**

Run:
```bash
cd /Users/linkai/code/costrict-web/gateway
go test ./...
```

Expected: all tests pass.

- [ ] **Step 2: Build both server and gateway**

Run:
```bash
cd /Users/linkai/code/costrict-web/server && go build ./...
cd /Users/linkai/code/costrict-web/gateway && go build ./...
```

Expected: both build successfully.

- [ ] **Step 3: Commit (if any test fixes)**

If no changes, skip.

---

## Self-Review

**Spec coverage:**
- Nacos priority over env var: Task 3 resolver via Nacos HTTP Open API.
- Env var fallback: Task 3 resolver.
- No heavy/vulnerable SDK dependency: using stdlib HTTP.
- Two clusters with different domains: Task 5 Helm values per cluster.
- Shared database: unchanged; Gateway registry still works.

**Placeholder scan:**
- No TBD/TODO placeholders.
- All code blocks are concrete.

**Type consistency:**
- `EndpointResolver.Resolve(*Config) (string, error)` used consistently.
- `Config.Nacos` fields match env-var loader.

---

## Execution Handoff

**Plan complete and saved to `docs/superpowers/plans/2026-07-10-nacos-endpoint-resolver.md`. Two execution options:**

**1. Subagent-Driven (recommended)** - I dispatch a fresh subagent per task, review between tasks, fast iteration.

**2. Inline Execution** - Execute tasks in this session using executing-plans, batch execution with checkpoints.

**Which approach?**
