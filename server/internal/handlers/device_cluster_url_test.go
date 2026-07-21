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
		ID:          "gw-a-0",
		Endpoint:    "https://device-a.example.com",
		InternalURL: "http://10.0.0.1:8081",
		APIBaseURL:  apiBaseURL,
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

func TestClusterAPIURLFor_BoundToDifferentGatewayWithoutAPIBaseURL(t *testing.T) {
	registry := gateway.NewGatewayRegistry(gateway.NewMemoryStore(), nil)
	now := time.Now().UnixMilli()
	if err := registry.Register(&gateway.GatewayInfo{
		ID:            "gw-a-0",
		Endpoint:      "https://device-a.example.com",
		InternalURL:   "http://10.0.0.1:8081",
		APIBaseURL:    "https://api-a.example.com",
		LastHeartbeat: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&gateway.GatewayInfo{
		ID:            "gw-b-0",
		Endpoint:      "https://device-b.example.com",
		InternalURL:   "http://10.0.0.2:8081",
		APIBaseURL:    "",
		LastHeartbeat: now,
	}); err != nil {
		t.Fatal(err)
	}
	registry.BindDevice("dev-2", "gw-b-0", "conn-2")
	if got := clusterAPIURLFor(registry, liveGatewayAPIMap(registry), "dev-2"); got != "" {
		t.Fatalf("expected empty for device bound to gateway without apiBaseURL, got %q", got)
	}
}
