package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS devices (
		id TEXT PRIMARY KEY,
		device_id TEXT NOT NULL,
		display_name TEXT,
		platform TEXT,
		version TEXT,
		user_id TEXT,
		status TEXT DEFAULT 'offline',
		token TEXT,
		token_rotated_at DATETIME,
		last_connected_at DATETIME,
		last_seen_at DATETIME,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func setupTestRegistry(t *testing.T) *GatewayRegistry {
	t.Helper()
	store := NewMemoryStore()
	return &GatewayRegistry{store: store, onDevicesOffline: func(deviceIDs []string) {}}
}

func setupTestDeviceSvc(t *testing.T) *services.DeviceService {
	t.Helper()
	return &services.DeviceService{DB: setupTestDB(t)}
}

func setupOnlineHandlerRouter(t *testing.T, registry *GatewayRegistry) (*gin.Engine, *Client) {
	t.Helper()
	client := NewClient("test-secret")
	deviceSvc := setupTestDeviceSvc(t)
	r := gin.New()
	r.POST("/internal/gateway/device/online", DeviceOnlineHandler(registry, client, deviceSvc))
	return r, client
}

func doOnlineRequest(t *testing.T, r *gin.Engine, deviceID, gatewayID, connID string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]string{"deviceID": deviceID, "gatewayID": gatewayID, "connID": connID}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/internal/gateway/device/online", strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// V001: Cross-gateway takeover — DeviceOnlineHandler triggers close on old gateway
func TestDeviceOnlineHandler_CrossGatewayTakeover(t *testing.T) {
	registry := setupTestRegistry(t)
	registry.Register(&GatewayInfo{ID: "gwA", Endpoint: "e", InternalURL: "http://gwA:8081"})
	registry.Register(&GatewayInfo{ID: "gwB", Endpoint: "e", InternalURL: "http://gwB:8081"})

	var closeMu sync.Mutex
	closeCalled := false
	var closeConnID string

	fakeOldGw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/device/devX/close" {
			var body struct{ ConnID string `json:"connID"` }
			_ = json.NewDecoder(r.Body).Decode(&body)
			closeMu.Lock()
			closeCalled = true
			closeConnID = body.ConnID
			closeMu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	defer fakeOldGw.Close()

	registry.store.RegisterGateway(&GatewayInfo{
		ID:          "gwA",
		Endpoint:    "e",
		InternalURL: fakeOldGw.URL,
	})

	r, _ := setupOnlineHandlerRouter(t, registry)

	doOnlineRequest(t, r, "devX", "gwA", "conn1")
	w := doOnlineRequest(t, r, "devX", "gwB", "conn2")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Wait for async close request
	deadline := time.After(2 * time.Second)
	for {
		closeMu.Lock()
		done := closeCalled
		closeMu.Unlock()
		if done {
			break
		}
		select {
		case <-deadline:
			t.Fatal("close request not received by old gateway within 2s")
		case <-time.After(10 * time.Millisecond):
		}
	}

	closeMu.Lock()
	defer closeMu.Unlock()
	if !closeCalled {
		t.Fatal("old gateway should have received close request")
	}
	if closeConnID != "conn1" {
		t.Fatalf("close request should carry old connID conn1, got %q", closeConnID)
	}
}

// V016: Duplicate NotifyOnline (same gateway) — idempotent, no close triggered
func TestDeviceOnlineHandler_IdempotentSameGateway(t *testing.T) {
	registry := setupTestRegistry(t)
	r, _ := setupOnlineHandlerRouter(t, registry)

	w1 := doOnlineRequest(t, r, "devX", "gwA", "conn1")
	if w1.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", w1.Code)
	}

	w2 := doOnlineRequest(t, r, "devX", "gwA", "conn2")
	if w2.Code != http.StatusOK {
		t.Fatalf("second request: expected 200, got %d", w2.Code)
	}

	gw := registry.GetDeviceGatewayID("devX")
	if gw != "gwA" {
		t.Fatalf("device should be bound to gwA, got %q", gw)
	}
}

// V006: Close request to dead gateway — 5s timeout, new device not affected
func TestDeviceOnlineHandler_CloseRequestTimeout(t *testing.T) {
	registry := setupTestRegistry(t)

	slowGw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
	}))
	defer slowGw.Close()

	registry.store.RegisterGateway(&GatewayInfo{
		ID:          "gwA",
		Endpoint:    "e",
		InternalURL: slowGw.URL,
	})

	r, _ := setupOnlineHandlerRouter(t, registry)
	doOnlineRequest(t, r, "devX", "gwA", "conn1")

	start := time.Now()
	w := doOnlineRequest(t, r, "devX", "gwB", "conn2")
	elapsed := time.Since(start)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if elapsed > 6*time.Second {
		t.Fatalf("handler blocked for %v on close request; should be async", elapsed)
	}

	gw := registry.GetDeviceGatewayID("devX")
	if gw != "gwB" {
		t.Fatalf("device should be bound to gwB despite slow old gateway, got %q", gw)
	}
}

// V007: Close request returns non-200 — new device not affected
func TestDeviceOnlineHandler_CloseRequestNon200(t *testing.T) {
	registry := setupTestRegistry(t)

	errorGw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer errorGw.Close()

	registry.store.RegisterGateway(&GatewayInfo{
		ID:          "gwA",
		Endpoint:    "e",
		InternalURL: errorGw.URL,
	})

	r, _ := setupOnlineHandlerRouter(t, registry)
	doOnlineRequest(t, r, "devX", "gwA", "conn1")
	w := doOnlineRequest(t, r, "devX", "gwB", "conn2")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 despite old gateway 500, got %d", w.Code)
	}
	gw := registry.GetDeviceGatewayID("devX")
	if gw != "gwB" {
		t.Fatalf("device should be bound to gwB, got %q", gw)
	}
}

// V008: Close request to unreachable gateway (network partition)
func TestDeviceOnlineHandler_CloseRequestUnreachable(t *testing.T) {
	registry := setupTestRegistry(t)

	registry.store.RegisterGateway(&GatewayInfo{
		ID:          "gwA",
		Endpoint:    "e",
		InternalURL: "http://127.0.0.1:1",
	})

	r, _ := setupOnlineHandlerRouter(t, registry)
	doOnlineRequest(t, r, "devX", "gwA", "conn1")
	w := doOnlineRequest(t, r, "devX", "gwB", "conn2")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 despite unreachable old gateway, got %d", w.Code)
	}
	gw := registry.GetDeviceGatewayID("devX")
	if gw != "gwB" {
		t.Fatalf("device should be bound to gwB, got %q", gw)
	}
}

// V017: Duplicate NotifyOffline (idempotent)
func TestDeviceOfflineHandler_IdempotentWhenAlreadyOffline(t *testing.T) {
	registry := setupTestRegistry(t)
	deviceSvc := setupTestDeviceSvc(t)
	r := gin.New()
	r.POST("/internal/gateway/device/offline", DeviceOfflineHandler(registry, deviceSvc))

	body := `{"deviceID":"devX","gatewayID":"gwA"}`
	req := httptest.NewRequest(http.MethodPost, "/internal/gateway/device/offline", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for offline on unbound device, got %d", w.Code)
	}
}

// V025: NotifyOffline with wrong gateway — does not unbind
func TestDeviceOfflineHandler_WrongGatewayDoesNotUnbind(t *testing.T) {
	registry := setupTestRegistry(t)
	deviceSvc := setupTestDeviceSvc(t)

	registry.store.BindDevice("devX", "gwB", "conn1")

	r := gin.New()
	r.POST("/internal/gateway/device/offline", DeviceOfflineHandler(registry, deviceSvc))

	body := `{"deviceID":"devX","gatewayID":"gwA"}`
	req := httptest.NewRequest(http.MethodPost, "/internal/gateway/device/offline", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	gw := registry.GetDeviceGatewayID("devX")
	if gw != "gwB" {
		t.Fatalf("device should still be bound to gwB after wrong-gateway offline, got %q", gw)
	}
}

// V013: Device reconnects to old gateway after being taken over
func TestDeviceOnlineHandler_ReconnectToOldGateway(t *testing.T) {
	registry := setupTestRegistry(t)
	registry.Register(&GatewayInfo{ID: "gwA", Endpoint: "e", InternalURL: "http://gwA:8081"})
	registry.Register(&GatewayInfo{ID: "gwB", Endpoint: "e", InternalURL: "http://gwB:8081"})

	r, _ := setupOnlineHandlerRouter(t, registry)

	doOnlineRequest(t, r, "devX", "gwA", "conn1")
	doOnlineRequest(t, r, "devX", "gwB", "conn2")

	gw := registry.GetDeviceGatewayID("devX")
	if gw != "gwB" {
		t.Fatalf("after B connects, device should be on gwB, got %q", gw)
	}

	// A reconnects to gwA
	w := doOnlineRequest(t, r, "devX", "gwA", "conn3")
	if w.Code != http.StatusOK {
		t.Fatalf("reconnect request: expected 200, got %d", w.Code)
	}

	gw = registry.GetDeviceGatewayID("devX")
	if gw != "gwA" {
		t.Fatalf("after A reconnects, device should be on gwA, got %q", gw)
	}
}

// V009: Concurrent DeviceOnlineHandler calls — last one wins
func TestDeviceOnlineHandler_ConcurrentTakeover(t *testing.T) {
	registry := setupTestRegistry(t)
	r, _ := setupOnlineHandlerRouter(t, registry)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)

	for i := 0; i < n; i++ {
		idx := i
		go func() {
			defer wg.Done()
			gwID := "gwA"
			if idx%2 == 1 {
				gwID = "gwB"
			}
			connID := "conn" + string(rune('A'+idx))
			doOnlineRequest(t, r, "devX", gwID, connID)
		}()
	}
	wg.Wait()

	gw := registry.GetDeviceGatewayID("devX")
	if gw != "gwA" && gw != "gwB" {
		t.Fatalf("device should be bound to gwA or gwB, got %q", gw)
	}
}
