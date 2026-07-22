package gateway

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// V021/V023: BindDevice returns old gateway and connID atomically
func TestMemoryStore_BindDevice_ReturnsOldBinding(t *testing.T) {
	s := NewMemoryStore()

	oldGw, oldConn, err := s.BindDevice("dev1", "gwA", "conn1")
	if err != nil {
		t.Fatalf("first BindDevice: %v", err)
	}
	if oldGw != "" || oldConn != "" {
		t.Fatalf("first bind should return empty old values, got gw=%q conn=%q", oldGw, oldConn)
	}

	oldGw, oldConn, err = s.BindDevice("dev1", "gwB", "conn2")
	if err != nil {
		t.Fatalf("second BindDevice: %v", err)
	}
	if oldGw != "gwA" {
		t.Fatalf("expected old gateway gwA, got %q", oldGw)
	}
	if oldConn != "conn1" {
		t.Fatalf("expected old connID conn1, got %q", oldConn)
	}

	gw, err := s.GetDeviceGateway("dev1")
	if err != nil {
		t.Fatalf("GetDeviceGateway: %v", err)
	}
	if gw != "gwB" {
		t.Fatalf("expected current gateway gwB, got %q", gw)
	}
}

// V016: BindDevice idempotent — same gateway returns same binding
func TestMemoryStore_BindDevice_IdempotentSameGateway(t *testing.T) {
	s := NewMemoryStore()
	s.BindDevice("dev1", "gwA", "conn1")

	oldGw, oldConn, _ := s.BindDevice("dev1", "gwA", "conn1b")
	if oldGw != "gwA" {
		t.Fatalf("idempotent bind should return same gateway, got %q", oldGw)
	}
	if oldConn != "conn1" {
		t.Fatalf("idempotent bind should return old connID, got %q", oldConn)
	}
}

// V017: UnbindDevice then GetDeviceGateway returns error
func TestMemoryStore_UnbindDevice(t *testing.T) {
	s := NewMemoryStore()
	s.BindDevice("dev1", "gwA", "conn1")

	if err := s.UnbindDevice("dev1"); err != nil {
		t.Fatalf("UnbindDevice: %v", err)
	}

	_, err := s.GetDeviceGateway("dev1")
	if err == nil {
		t.Fatal("expected error after unbind")
	}
}

// V009/V023: Concurrent BindDevice — last writer wins, all see consistent state
func TestMemoryStore_BindDevice_Concurrent(t *testing.T) {
	s := NewMemoryStore()
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)

	results := make([][2]string, n)
	for i := 0; i < n; i++ {
		idx := i
		go func() {
			defer wg.Done()
			gwID := fmt.Sprintf("gw%d", idx%3)
			connID := fmt.Sprintf("conn%d", idx)
			oldGw, oldConn, _ := s.BindDevice("dev1", gwID, connID)
			results[idx] = [2]string{oldGw, oldConn}
		}()
	}
	wg.Wait()

	gw, err := s.GetDeviceGateway("dev1")
	if err != nil {
		t.Fatalf("GetDeviceGateway after concurrent binds: %v", err)
	}
	if gw == "" {
		t.Fatal("device should be bound to a gateway after concurrent binds")
	}
}

// V011: RemoveGatewayWithDevices cleans up all device bindings
func TestMemoryStore_RemoveGatewayWithDevices(t *testing.T) {
	s := NewMemoryStore()
	s.BindDevice("dev1", "gwA", "conn1")
	s.BindDevice("dev2", "gwA", "conn2")
	s.BindDevice("dev3", "gwB", "conn3")

	deviceIDs, err := s.RemoveGatewayWithDevices("gwA")
	if err != nil {
		t.Fatalf("RemoveGatewayWithDevices: %v", err)
	}
	if len(deviceIDs) != 2 {
		t.Fatalf("expected 2 removed devices, got %d", len(deviceIDs))
	}

	_, err = s.GetDeviceGateway("dev1")
	if err == nil {
		t.Fatal("dev1 should be unbound after gateway removal")
	}
	_, err = s.GetDeviceGateway("dev2")
	if err == nil {
		t.Fatal("dev2 should be unbound after gateway removal")
	}

	gw, err := s.GetDeviceGateway("dev3")
	if err != nil {
		t.Fatalf("dev3 should still be bound: %v", err)
	}
	if gw != "gwB" {
		t.Fatalf("dev3 should be bound to gwB, got %q", gw)
	}
}

func TestMemoryStore_GetGateway_NotFound(t *testing.T) {
	s := NewMemoryStore()
	_, err := s.GetGateway("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent gateway")
	}
}

func TestMemoryStore_GetDeviceGateway_NotFound(t *testing.T) {
	s := NewMemoryStore()
	_, err := s.GetDeviceGateway("nonexistent")
	if err == nil {
		t.Fatal("expected error for unbound device")
	}
}

func TestMemoryStore_ListDevicesByGateway(t *testing.T) {
	s := NewMemoryStore()
	s.BindDevice("dev1", "gwA", "conn1")
	s.BindDevice("dev2", "gwA", "conn2")
	s.BindDevice("dev3", "gwB", "conn3")

	devices, err := s.ListDevicesByGateway("gwA")
	if err != nil {
		t.Fatalf("ListDevicesByGateway: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices for gwA, got %d", len(devices))
	}
}

func TestMemoryStore_RegisterAndGetGateway(t *testing.T) {
	s := NewMemoryStore()
	info := &GatewayInfo{
		ID:          "gwA",
		Endpoint:    "http://gwA:8080",
		InternalURL: "http://gwA:8081",
		Region:      "us-east",
		Capacity:    100,
	}
	if err := s.RegisterGateway(info); err != nil {
		t.Fatalf("RegisterGateway: %v", err)
	}

	got, err := s.GetGateway("gwA")
	if err != nil {
		t.Fatalf("GetGateway: %v", err)
	}
	if got.ID != "gwA" || got.InternalURL != "http://gwA:8081" {
		t.Fatalf("GetGateway returned wrong data: %+v", got)
	}
}

func TestMemoryStore_HeartbeatUpdates(t *testing.T) {
	s := NewMemoryStore()
	s.RegisterGateway(&GatewayInfo{ID: "gwA", Endpoint: "e", InternalURL: "i"})
	time.Sleep(5 * time.Millisecond)

	if err := s.HeartbeatGateway("gwA", 42); err != nil {
		t.Fatalf("HeartbeatGateway: %v", err)
	}

	got, _ := s.GetGateway("gwA")
	if got.CurrentConns != 42 {
		t.Fatalf("expected CurrentConns=42, got %d", got.CurrentConns)
	}
}

// V040: MemoryStore has no persistence — state lost when recreated
func TestMemoryStore_NoPersistence(t *testing.T) {
	s1 := NewMemoryStore()
	s1.BindDevice("dev1", "gwA", "conn1")
	s1.RegisterGateway(&GatewayInfo{ID: "gwA", Endpoint: "e", InternalURL: "i"})

	// Simulate restart with a fresh store
	s2 := NewMemoryStore()
	_, err := s2.GetDeviceGateway("dev1")
	if err == nil {
		t.Fatal("new MemoryStore should not have data from previous instance")
	}
}

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
