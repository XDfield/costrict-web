package services

import (
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupDeviceServiceDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	stmts := []string{
		`CREATE TABLE devices (
			id TEXT PRIMARY KEY,
			device_id TEXT NOT NULL,
			display_name TEXT,
			platform TEXT,
			version TEXT,
			user_id TEXT,
			status TEXT DEFAULT 'offline',
			label TEXT,
			description TEXT,
			token TEXT,
			token_rotated_at DATETIME,
			last_connected_at DATETIME,
			last_seen_at DATETIME,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME
		)`,
		`CREATE TABLE users (
			id TEXT PRIMARY KEY,
			subject_id TEXT NOT NULL UNIQUE,
			display_name TEXT,
			email TEXT,
			phone TEXT,
			avatar_url TEXT,
			organization TEXT,
			auth_provider TEXT,
			is_active BOOLEAN DEFAULT TRUE,
			created_at DATETIME,
			updated_at DATETIME
		)`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

func TestRegisterDevice_NewDevice(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	device, token, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "dev-001",
		DisplayName: "My Device",
		Platform:    "windows",
		Version:     "1.0.0",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if device.DeviceID != "dev-001" {
		t.Fatalf("expected device_id dev-001, got %q", device.DeviceID)
	}
	if device.UserID != "user-1" {
		t.Fatalf("expected user_id user-1, got %q", device.UserID)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
}

func TestRegisterDevice_SameUserReRegisters(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "dev-001",
		DisplayName: "My Device",
		Platform:    "windows",
		Version:     "1.0.0",
	})

	device, token, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "dev-001",
		DisplayName: "Updated Name",
		Platform:    "linux",
		Version:     "1.1.0",
	})
	if err != ErrDeviceOwnedByCaller {
		t.Fatalf("expected ErrDeviceOwnedByCaller, got %v", err)
	}
	if device.DeviceID != "dev-001" {
		t.Fatalf("expected device_id dev-001, got %q", device.DeviceID)
	}
	if token == "" {
		t.Fatal("expected non-empty token on re-register")
	}
}

func TestRegisterDevice_DifferentUserOwnsDevice(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u1", "user-1", "User One", time.Now(), time.Now())
	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u2", "user-2", "User Two", time.Now(), time.Now())

	svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "dev-001",
		DisplayName: "My Device",
		Platform:    "windows",
		Version:     "1.0.0",
	})

	_, _, err := svc.RegisterDevice("user-2", RegisterDeviceRequest{
		DeviceID:    "dev-001",
		DisplayName: "Hijack",
		Platform:    "linux",
		Version:     "1.1.0",
	})
	if err != ErrDeviceAlreadyRegistered {
		t.Fatalf("expected ErrDeviceAlreadyRegistered, got %v", err)
	}
}

func TestRegisterDevice_OrphanedDeviceRebind(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u1", "user-1", "User One", time.Now(), time.Now())

	svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "dev-001",
		DisplayName: "Orphaned Device",
		Platform:    "windows",
		Version:     "1.0.0",
	})

	db.Exec("DELETE FROM users WHERE subject_id = ?", "user-1")

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u2", "user-2", "User Two", time.Now(), time.Now())

	device, token, err := svc.RegisterDevice("user-2", RegisterDeviceRequest{
		DeviceID:    "dev-001",
		DisplayName: "Reclaimed Device",
		Platform:    "linux",
		Version:     "2.0.0",
	})
	if err != nil {
		t.Fatalf("unexpected error for orphaned device rebind: %v", err)
	}
	if device.UserID != "user-2" {
		t.Fatalf("expected rebound user_id user-2, got %q", device.UserID)
	}
	if token == "" {
		t.Fatal("expected non-empty token after rebind")
	}

	var updated models.Device
	db.Where("device_id = ?", "dev-001").First(&updated)
	if updated.UserID != "user-2" {
		t.Fatalf("DB not updated: expected user_id user-2, got %q", updated.UserID)
	}
	if updated.DisplayName != "Reclaimed Device" {
		t.Fatalf("expected display_name 'Reclaimed Device', got %q", updated.DisplayName)
	}
	if updated.Platform != "linux" {
		t.Fatalf("expected platform 'linux', got %q", updated.Platform)
	}
	if updated.Version != "2.0.0" {
		t.Fatalf("expected version '2.0.0', got %q", updated.Version)
	}
}

func TestRegisterDevice_OrphanedDeviceDifferentNewUser(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	svc.RegisterDevice("ghost-user", RegisterDeviceRequest{
		DeviceID:    "dev-002",
		DisplayName: "Ghost Device",
		Platform:    "macos",
		Version:     "1.0.0",
	})

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u3", "user-3", "User Three", time.Now(), time.Now())

	device, token, err := svc.RegisterDevice("user-3", RegisterDeviceRequest{
		DeviceID:    "dev-002",
		DisplayName: "New Owner",
		Platform:    "windows",
		Version:     "2.0.0",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if device.UserID != "user-3" {
		t.Fatalf("expected user_id user-3, got %q", device.UserID)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	var count int64
	db.Table("users").Where("subject_id = ?", "ghost-user").Count(&count)
	if count != 0 {
		t.Fatal("ghost-user should not exist in users table")
	}
}
