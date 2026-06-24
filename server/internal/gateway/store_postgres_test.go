package gateway

import (
	"os"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
)

func setupPostgresStoreTestDB(t *testing.T) *PostgresStore {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL store test")
	}
	db, err := database.Initialize(dsn)
	if err != nil {
		t.Fatalf("initialize database: %v", err)
	}
	// Clean up leftover test rows.
	_ = db.Where("id LIKE ?", "gw-%").Delete(&models.GatewayRegistry{})
	_ = db.Where("device_id LIKE ?", "dev-%").Delete(&models.GatewayDeviceBinding{})
	return NewPostgresStore(db).(*PostgresStore)
}

func TestPostgresStore_RegisterAndListGateways(t *testing.T) {
	s := setupPostgresStoreTestDB(t)

	info := &GatewayInfo{
		ID:            "gw-1",
		Endpoint:      "wss://gw1.example.com",
		InternalURL:   "http://10.0.0.1:8081",
		Region:        "default",
		Capacity:      100,
		CurrentConns:  5,
		LastHeartbeat: time.Now().UnixMilli(),
	}
	if err := s.RegisterGateway(info); err != nil {
		t.Fatalf("RegisterGateway: %v", err)
	}

	gateways, err := s.ListGateways()
	if err != nil {
		t.Fatalf("ListGateways: %v", err)
	}
	if len(gateways) != 1 {
		t.Fatalf("expected 1 gateway, got %d", len(gateways))
	}
	if gateways[0].ID != "gw-1" {
		t.Fatalf("unexpected gateway id: %s", gateways[0].ID)
	}

	// Re-register updates the record.
	info.CurrentConns = 10
	if err := s.RegisterGateway(info); err != nil {
		t.Fatalf("RegisterGateway update: %v", err)
	}

	gw, err := s.db.Where("id = ?", "gw-1").First(&models.GatewayRegistry{}).Rows()
	if err != nil {
		t.Fatalf("query gateway: %v", err)
	}
	defer gw.Close()
	var row models.GatewayRegistry
	if !gw.Next() {
		t.Fatal("gateway row not found")
	}
	_ = s.db.ScanRows(gw, &row)
	if row.CurrentConns != 10 {
		t.Fatalf("expected current_conns 10, got %d", row.CurrentConns)
	}
}

func TestPostgresStore_HeartbeatGateway(t *testing.T) {
	s := setupPostgresStoreTestDB(t)

	info := &GatewayInfo{ID: "gw-heartbeat", Endpoint: "wss://gw.example.com", InternalURL: "http://10.0.0.1:8081", Region: "default"}
	if err := s.RegisterGateway(info); err != nil {
		t.Fatalf("RegisterGateway: %v", err)
	}

	if err := s.HeartbeatGateway("gw-heartbeat", 42); err != nil {
		t.Fatalf("HeartbeatGateway: %v", err)
	}

	var row models.GatewayRegistry
	if err := s.db.Where("id = ?", "gw-heartbeat").First(&row).Error; err != nil {
		t.Fatalf("query gateway: %v", err)
	}
	if row.CurrentConns != 42 {
		t.Fatalf("expected current_conns 42, got %d", row.CurrentConns)
	}

	if err := s.HeartbeatGateway("missing", 1); err == nil {
		t.Fatal("expected error for missing gateway")
	}
}

func TestPostgresStore_DeviceBinding(t *testing.T) {
	s := setupPostgresStoreTestDB(t)

	gw := &GatewayInfo{ID: "gw-bind", Endpoint: "wss://gw.example.com", InternalURL: "http://10.0.0.1:8081", Region: "default"}
	if err := s.RegisterGateway(gw); err != nil {
		t.Fatalf("RegisterGateway: %v", err)
	}

	if err := s.BindDevice("dev-1", "gw-bind"); err != nil {
		t.Fatalf("BindDevice: %v", err)
	}

	gwID, err := s.GetDeviceGateway("dev-1")
	if err != nil {
		t.Fatalf("GetDeviceGateway: %v", err)
	}
	if gwID != "gw-bind" {
		t.Fatalf("unexpected gateway id: %s", gwID)
	}

	devices, err := s.ListDevicesByGateway("gw-bind")
	if err != nil {
		t.Fatalf("ListDevicesByGateway: %v", err)
	}
	if len(devices) != 1 || devices[0] != "dev-1" {
		t.Fatalf("unexpected devices: %v", devices)
	}

	if err := s.UnbindDevice("dev-1"); err != nil {
		t.Fatalf("UnbindDevice: %v", err)
	}
	_, err = s.GetDeviceGateway("dev-1")
	if err == nil {
		t.Fatal("expected error after unbind")
	}
}

func TestPostgresStore_RemoveGatewayWithDevices(t *testing.T) {
	s := setupPostgresStoreTestDB(t)

	gw := &GatewayInfo{ID: "gw-rm", Endpoint: "wss://gw.example.com", InternalURL: "http://10.0.0.1:8081", Region: "default"}
	if err := s.RegisterGateway(gw); err != nil {
		t.Fatalf("RegisterGateway: %v", err)
	}
	if err := s.BindDevice("dev-rm-1", "gw-rm"); err != nil {
		t.Fatalf("BindDevice: %v", err)
	}
	if err := s.BindDevice("dev-rm-2", "gw-rm"); err != nil {
		t.Fatalf("BindDevice: %v", err)
	}

	removed, err := s.RemoveGatewayWithDevices("gw-rm")
	if err != nil {
		t.Fatalf("RemoveGatewayWithDevices: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("expected 2 removed devices, got %d", len(removed))
	}

	var count int64
	s.db.Model(&models.GatewayRegistry{}).Count(&count)
	if count != 0 {
		t.Fatalf("expected 0 gateways, got %d", count)
	}
}

func TestPostgresStore_GetOrInitEpoch(t *testing.T) {
	s := setupPostgresStoreTestDB(t)

	first, err := s.GetOrInitEpoch(123)
	if err != nil {
		t.Fatalf("GetOrInitEpoch: %v", err)
	}
	if first != 123 {
		t.Fatalf("expected epoch 123, got %d", first)
	}

	second, err := s.GetOrInitEpoch(456)
	if err != nil {
		t.Fatalf("GetOrInitEpoch second: %v", err)
	}
	if second != 123 {
		t.Fatalf("expected epoch to remain 123, got %d", second)
	}
}

func TestPostgresStore_TryLock(t *testing.T) {
	s := setupPostgresStoreTestDB(t)

	acquired, err := s.TryLock("test-lock", 5*time.Second)
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	if !acquired {
		t.Fatal("expected to acquire lock")
	}

	// Same session should re-acquire the same lock immediately.
	acquired2, err := s.TryLock("test-lock", 5*time.Second)
	if err != nil {
		t.Fatalf("TryLock again: %v", err)
	}
	if !acquired2 {
		t.Fatal("expected same session to re-acquire lock")
	}
}
