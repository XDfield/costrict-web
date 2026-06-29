package services

import (
	"errors"
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
		`CREATE TABLE device_migrations (
			id TEXT PRIMARY KEY,
			old_device_id TEXT NOT NULL,
			new_device_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			created_at DATETIME
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

func TestRegisterDevice_LegacyMigration_SameUser(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "legacy-id-001",
		DisplayName: "Old Device",
		Platform:    "windows",
		Version:     "1.0.0",
	})

	device, token, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "new-id-001",
		LegacyDeviceID: "legacy-id-001",
		DisplayName:    "Migrated Device",
		Platform:       "windows",
		Version:        "1.1.0",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if device.DeviceID != "new-id-001" {
		t.Fatalf("expected device_id new-id-001, got %q", device.DeviceID)
	}
	if token == "" {
		t.Fatal("expected non-empty token after migration")
	}

	var count int64
	db.Where("device_id = ?", "legacy-id-001").Model(&models.Device{}).Count(&count)
	if count != 0 {
		t.Fatal("legacy device_id should no longer exist")
	}
	db.Where("device_id = ?", "new-id-001").Model(&models.Device{}).Count(&count)
	if count != 1 {
		t.Fatal("new device_id should exist")
	}
}

func TestRegisterDevice_LegacyMigration_OrphanedDevice(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u1", "old-user", "Old User", time.Now(), time.Now())

	svc.RegisterDevice("old-user", RegisterDeviceRequest{
		DeviceID:    "legacy-id-002",
		DisplayName: "Orphaned",
		Platform:    "linux",
		Version:     "1.0.0",
	})

	db.Exec("DELETE FROM users WHERE subject_id = ?", "old-user")

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u2", "new-user", "New User", time.Now(), time.Now())

	device, token, err := svc.RegisterDevice("new-user", RegisterDeviceRequest{
		DeviceID:       "new-id-002",
		LegacyDeviceID: "legacy-id-002",
		DisplayName:    "Reclaimed",
		Platform:       "linux",
		Version:        "2.0.0",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if device.DeviceID != "new-id-002" {
		t.Fatalf("expected device_id new-id-002, got %q", device.DeviceID)
	}
	if device.UserID != "new-user" {
		t.Fatalf("expected user_id new-user, got %q", device.UserID)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
}

func TestRegisterDevice_LegacyMigration_DifferentActiveUser(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u1", "user-1", "User One", time.Now(), time.Now())
	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u2", "user-2", "User Two", time.Now(), time.Now())

	svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "legacy-id-003",
		DisplayName: "Owned",
		Platform:    "windows",
		Version:     "1.0.0",
	})

	device, token, err := svc.RegisterDevice("user-2", RegisterDeviceRequest{
		DeviceID:       "new-id-003",
		LegacyDeviceID: "legacy-id-003",
		DisplayName:    "Cloned Machine",
		Platform:       "linux",
		Version:        "1.1.0",
	})
	if err != nil {
		t.Fatalf("unexpected error for legacy conflict with different user: %v", err)
	}
	if device.DeviceID != "new-id-003" {
		t.Fatalf("expected device_id new-id-003, got %q", device.DeviceID)
	}
	if device.UserID != "user-2" {
		t.Fatalf("expected user_id user-2, got %q", device.UserID)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	var count int64
	db.Where("device_id = ?", "legacy-id-003").Model(&models.Device{}).Count(&count)
	if count != 1 {
		t.Fatal("original device (legacy-id-003) should still exist for user-1")
	}
	db.Where("device_id = ?", "new-id-003").Model(&models.Device{}).Count(&count)
	if count != 1 {
		t.Fatal("new device (new-id-003) should exist for user-2")
	}
}

func TestRegisterDevice_LegacyMigration_NoMatch(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	device, token, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "new-id-004",
		LegacyDeviceID: "nonexistent-legacy-id",
		DisplayName:    "New Device",
		Platform:       "macos",
		Version:        "1.0.0",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if device.DeviceID != "new-id-004" {
		t.Fatalf("expected device_id new-id-004, got %q", device.DeviceID)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
}

func TestRegisterDevice_LegacyMigration_SameIDSkipsLookup(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	device, token, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "same-id",
		LegacyDeviceID: "same-id",
		DisplayName:    "Same ID",
		Platform:       "windows",
		Version:        "1.0.0",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if device.DeviceID != "same-id" {
		t.Fatalf("expected device_id same-id, got %q", device.DeviceID)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
}

func TestRegisterDevice_NonLegacyConflict_StillRejected(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u1", "user-1", "User One", time.Now(), time.Now())
	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u2", "user-2", "User Two", time.Now(), time.Now())

	svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "dev-unique",
		DisplayName: "Owned",
		Platform:    "windows",
		Version:     "1.0.0",
	})

	_, _, err := svc.RegisterDevice("user-2", RegisterDeviceRequest{
		DeviceID:    "dev-unique",
		DisplayName: "Hijack",
		Platform:    "linux",
		Version:     "1.1.0",
	})
	if err != ErrDeviceAlreadyRegistered {
		t.Fatalf("non-legacy conflict should still return ErrDeviceAlreadyRegistered, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Clone-specific scenarios (V019 / V026 / V034)
// ---------------------------------------------------------------------------

// V019: Clone B registers with the exact same device_id as Clone A, same user.
// Server returns the existing device with ErrDeviceOwnedByCaller (idempotent re-register).
func TestRegisterDevice_CloneSameDeviceID_SameUser(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	_, originalToken, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "shared-device-id",
		DisplayName: "Clone A",
		Platform:    "windows",
		Version:     "1.0.0",
	})
	if err != nil {
		t.Fatalf("first register: %v", err)
	}

	device, token, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "shared-device-id",
		DisplayName: "Clone B",
		Platform:    "windows",
		Version:     "1.0.0",
	})
	if err != ErrDeviceOwnedByCaller {
		t.Fatalf("expected ErrDeviceOwnedByCaller for same-user clone, got %v", err)
	}
	if device == nil {
		t.Fatal("should return existing device")
	}
	if device.DeviceID != "shared-device-id" {
		t.Fatalf("device_id = %q, want shared-device-id", device.DeviceID)
	}
	if token != originalToken {
		t.Fatal("token should be the original token (no rotation on idempotent re-register)")
	}
}

// V019: Clone B registers with the exact same device_id, different active user → rejected.
func TestRegisterDevice_CloneSameDeviceID_DifferentUser(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u1", "user-1", "User One", time.Now(), time.Now())
	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u2", "user-2", "User Two", time.Now(), time.Now())

	svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "shared-device-id",
		DisplayName: "Clone A",
		Platform:    "windows",
		Version:     "1.0.0",
	})

	device, _, err := svc.RegisterDevice("user-2", RegisterDeviceRequest{
		DeviceID:    "shared-device-id",
		DisplayName: "Clone B",
		Platform:    "linux",
		Version:     "1.0.0",
	})
	if err != ErrDeviceAlreadyRegistered {
		t.Fatalf("expected ErrDeviceAlreadyRegistered for different-user clone, got %v", err)
	}
	if device != nil {
		t.Fatalf("should not return device for rejected clone, got %+v", device)
	}
}

// V026: Sequential migration with shared legacy ID (same user).
// Clone A migrates first → migration recorded. Clone B then arrives with the same legacy ID.
// Without ConfirmRecovery, the server returns RecoveryError (user must decide).
// With ConfirmRecovery: true, clone B recovers A's identity (same DB row).
func TestRegisterDevice_SequentialMigration_SameLegacyID_Recovers(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u1", "user-1", "User One", time.Now(), time.Now())

	_, _, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "hash_shared",
		DisplayName: "Original",
		Platform:    "windows",
		Version:     "1.0.0",
	})
	if err != nil {
		t.Fatalf("create original device: %v", err)
	}

	devA, tokenA, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "randomA",
		LegacyDeviceID: "hash_shared",
		DisplayName:    "Clone A (migrated)",
		Platform:       "windows",
		Version:        "2.0.0",
	})
	if err != nil {
		t.Fatalf("clone A migration: %v", err)
	}
	if devA.DeviceID != "randomA" {
		t.Fatalf("clone A device_id = %q, want randomA", devA.DeviceID)
	}

	// Clone B: without ConfirmRecovery → RecoveryError
	_, _, err = svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "randomB",
		LegacyDeviceID: "hash_shared",
		DisplayName:    "Clone B",
		Platform:       "windows",
		Version:        "2.0.0",
	})
	var recoveryErr *RecoveryError
	if !errors.As(err, &recoveryErr) {
		t.Fatalf("expected RecoveryError without ConfirmRecovery, got: %v", err)
	}
	if recoveryErr.RecoverableDeviceID != "randomA" {
		t.Fatalf("recovery device ID = %q, want randomA", recoveryErr.RecoverableDeviceID)
	}

	// Clone B: with ConfirmRecovery → recovers A's identity
	devB, tokenB, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:        "randomB",
		LegacyDeviceID:  "hash_shared",
		DisplayName:     "Clone B",
		Platform:        "windows",
		Version:         "2.0.0",
		ConfirmRecovery: true,
	})
	if err != nil {
		t.Fatalf("clone B recovery: %v", err)
	}
	if devB.DeviceID != "randomB" {
		t.Fatalf("clone B device_id = %q, want randomB", devB.DeviceID)
	}

	if devA.ID != devB.ID {
		t.Fatalf("clones sharing same legacy ID + user should reuse the same DB row (UUID); A=%q B=%q", devA.ID, devB.ID)
	}

	var count int64
	db.Model(&models.Device{}).Count(&count)
	if count != 1 {
		t.Fatalf("expected 1 device (recovered, not duplicated), got %d", count)
	}

	_, err = svc.VerifyDeviceToken(tokenA)
	if err == nil {
		t.Fatal("clone A's old token should be invalid (rotated on B's recovery)")
	}
	_, err = svc.VerifyDeviceToken(tokenB)
	if err != nil {
		t.Fatalf("clone B's new token should be valid: %v", err)
	}
}

// V034: MAC collision migration.
// Two DIFFERENT physical machines generate the same hash-based device_id (same MAC + username).
// Without ConfirmRecovery, the second machine gets a RecoveryError. With ForceNew, it creates
// a separate device. The user decides based on whether it's the same machine (v2 deleted) or
// a different machine (MAC collision).
func TestRegisterDevice_MACCollisionMigration_ForceNewCreatesSeparate(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u1", "user-1", "Colliding User", time.Now(), time.Now())

	svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "hash_collision",
		DisplayName: "Machine Template",
		Platform:    "linux",
		Version:     "1.0.0",
	})

	devA, _, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "random_mac_a",
		LegacyDeviceID: "hash_collision",
		DisplayName:    "Machine A",
		Platform:       "linux",
		Version:        "2.0.0",
	})
	if err != nil {
		t.Fatalf("machine A migration: %v", err)
	}
	if devA.DeviceID != "random_mac_a" {
		t.Fatalf("machine A device_id = %q, want random_mac_a", devA.DeviceID)
	}

	// Machine B: without flags → RecoveryError
	_, _, err = svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "random_mac_b",
		LegacyDeviceID: "hash_collision",
		DisplayName:    "Machine B",
		Platform:       "linux",
		Version:        "2.0.0",
	})
	var recoveryErr *RecoveryError
	if !errors.As(err, &recoveryErr) {
		t.Fatalf("expected RecoveryError, got: %v", err)
	}
	if recoveryErr.RecoverableDeviceID != "random_mac_a" {
		t.Fatalf("recovery device = %q, want random_mac_a", recoveryErr.RecoverableDeviceID)
	}

	// Machine B: with ForceNew → creates separate device
	devB, _, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "random_mac_b",
		LegacyDeviceID: "hash_collision",
		DisplayName:    "Machine B",
		Platform:       "linux",
		Version:        "2.0.0",
		ForceNew:       true,
	})
	if err != nil {
		t.Fatalf("machine B force-new: %v", err)
	}
	if devB.DeviceID != "random_mac_b" {
		t.Fatalf("machine B device_id = %q, want random_mac_b", devB.DeviceID)
	}

	if devA.DeviceID == devB.DeviceID {
		t.Fatal("MAC collision + ForceNew: machines should have SEPARATE device IDs")
	}

	var count int64
	db.Model(&models.Device{}).Count(&count)
	if count != 2 {
		t.Fatalf("expected 2 devices (separate), got %d", count)
	}
}

// V032: After migration, a clone still using the old device_id/token can't authenticate.
// The old device_id has been UPDATEd away, so VerifyDeviceToken with the old token fails.
func TestRegisterDevice_MigratedClone_OldTokenInvalid(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u1", "user-1", "User One", time.Now(), time.Now())

	_, oldToken, _ := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "old_hash_id",
		DisplayName: "Pre-migration",
		Platform:    "windows",
		Version:     "1.0.0",
	})

	_, newToken, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "new_random_id",
		LegacyDeviceID: "old_hash_id",
		DisplayName:    "Post-migration",
		Platform:       "windows",
		Version:        "2.0.0",
	})
	if err != nil {
		t.Fatalf("migration: %v", err)
	}

	_, err = svc.VerifyDeviceToken(oldToken)
	if err == nil {
		t.Fatal("old token should be invalid after migration (device_id was UPDATEd, token rotated)")
	}

	device, err := svc.VerifyDeviceToken(newToken)
	if err != nil {
		t.Fatalf("new token should be valid: %v", err)
	}
	if device.DeviceID != "new_random_id" {
		t.Fatalf("new token should resolve to new_random_id, got %q", device.DeviceID)
	}
}

// device_v2.json deleted after migration: legacy ID triggers recovery prompt.
// Without ConfirmRecovery, server returns RecoveryError. With ConfirmRecovery, the previous
// device is recovered (same DB row, workspace associations preserved).
//
// Timeline:
//  1. Device registered with hash "sha256_old" (legacy ID)
//  2. Migrated: DB UPDATEs device_id "sha256_old" → "randomA", migration recorded
//  3. device_v2.json accidentally deleted (no device files left)
//  4. Device re-registers: GenerateOldMachineID() produces "sha256_old" again (deterministic)
//  5. enroll(deviceId="randomB", legacyDeviceId="sha256_old") → RecoveryError (no flag)
//  6. User confirms → enroll(deviceId="randomB", legacyDeviceId="sha256_old", confirmRecovery=true)
//  7. Server recovers: migrate "randomA" → "randomB", returns recovered device
func TestRegisterDevice_V2Deleted_LegacyIDRecovers(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u1", "user-1", "User One", time.Now(), time.Now())

	_, _, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "sha256_old",
		DisplayName: "Pre-migration",
		Platform:    "windows",
		Version:     "1.0.0",
	})
	if err != nil {
		t.Fatalf("create original: %v", err)
	}

	migrated, _, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "randomA",
		LegacyDeviceID: "sha256_old",
		DisplayName:    "Post-migration",
		Platform:       "windows",
		Version:        "2.0.0",
	})
	if err != nil {
		t.Fatalf("migration: %v", err)
	}
	migratedUUID := migrated.ID

	var count int64
	db.Where("device_id = ?", "sha256_old").Model(&models.Device{}).Count(&count)
	if count != 0 {
		t.Fatal("sha256_old should be gone from devices table after migration")
	}
	db.Model(&models.DeviceMigration{}).Where("old_device_id = ?", "sha256_old").Count(&count)
	if count != 1 {
		t.Fatal("migration record should exist for sha256_old")
	}

	// Step 5: without ConfirmRecovery → RecoveryError
	_, _, err = svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "randomB",
		LegacyDeviceID: "sha256_old",
		DisplayName:    "After V2 deletion",
		Platform:       "windows",
		Version:        "2.0.0",
	})
	var recoveryErr *RecoveryError
	if !errors.As(err, &recoveryErr) {
		t.Fatalf("expected RecoveryError, got: %v", err)
	}
	if recoveryErr.RecoverableDeviceID != "randomA" {
		t.Fatalf("recovery device = %q, want randomA", recoveryErr.RecoverableDeviceID)
	}
	if recoveryErr.DisplayName != "Post-migration" {
		t.Fatalf("recovery display name = %q, want Post-migration", recoveryErr.DisplayName)
	}

	// Step 6-7: with ConfirmRecovery → recovers previous device
	reRegistered, reToken, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:        "randomB",
		LegacyDeviceID:  "sha256_old",
		DisplayName:     "After V2 deletion",
		Platform:        "windows",
		Version:         "2.0.0",
		ConfirmRecovery: true,
	})
	if err != nil {
		t.Fatalf("re-registration after v2 deletion: %v", err)
	}
	if reRegistered.DeviceID != "randomB" {
		t.Fatalf("re-registered device_id = %q, want randomB", reRegistered.DeviceID)
	}
	if reRegistered.ID != migratedUUID {
		t.Fatalf("should reuse same DB row (UUID %q), got %q — workspace associations lost!", migratedUUID, reRegistered.ID)
	}
	if reToken == "" {
		t.Fatal("should get a new token")
	}

	db.Model(&models.Device{}).Count(&count)
	if count != 1 {
		t.Fatalf("expected 1 device (recovered, not duplicated), got %d", count)
	}

	db.Where("device_id = ?", "randomA").Model(&models.Device{}).Count(&count)
	if count != 0 {
		t.Fatal("randomA should no longer exist (updated to randomB)")
	}
	db.Where("device_id = ?", "randomB").Model(&models.Device{}).Count(&count)
	if count != 1 {
		t.Fatal("randomB should exist")
	}

	_, err = svc.VerifyDeviceToken(reToken)
	if err != nil {
		t.Fatalf("new token should be valid: %v", err)
	}
}

// Clone pollution guard: a different user with the same legacy hash cannot recover
// another user's migrated device. The migration lookup is scoped by user_id.
func TestRegisterDevice_MigrationLookup_UserScoped(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u1", "user-1", "User One", time.Now(), time.Now())
	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u2", "user-2", "User Two", time.Now(), time.Now())

	_, _, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "sha256_shared",
		DisplayName: "User-1 Device",
		Platform:    "windows",
		Version:     "1.0.0",
	})
	if err != nil {
		t.Fatalf("create user-1 device: %v", err)
	}

	_, _, err = svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "randomA",
		LegacyDeviceID: "sha256_shared",
		DisplayName:    "Migrated",
		Platform:       "windows",
		Version:        "2.0.0",
	})
	if err != nil {
		t.Fatalf("user-1 migration: %v", err)
	}

	// user-2 (clone) tries to recover using the same hash
	cloneDevice, _, err := svc.RegisterDevice("user-2", RegisterDeviceRequest{
		DeviceID:       "randomC",
		LegacyDeviceID: "sha256_shared",
		DisplayName:    "Clone",
		Platform:       "linux",
		Version:        "2.0.0",
	})
	if err != nil {
		t.Fatalf("user-2 registration: %v", err)
	}
	if cloneDevice.DeviceID != "randomC" {
		t.Fatalf("clone should get its own device_id randomC, got %q", cloneDevice.DeviceID)
	}
	if cloneDevice.UserID != "user-2" {
		t.Fatalf("clone should belong to user-2, got %q", cloneDevice.UserID)
	}

	var count int64
	db.Model(&models.Device{}).Count(&count)
	if count != 2 {
		t.Fatalf("expected 2 separate devices, got %d (clone may have hijacked user-1's device)", count)
	}

	db.Where("device_id = ? AND user_id = ?", "randomA", "user-1").Model(&models.Device{}).Count(&count)
	if count != 1 {
		t.Fatal("user-1's migrated device (randomA) should be untouched by clone")
	}
	db.Where("device_id = ? AND user_id = ?", "randomC", "user-2").Model(&models.Device{}).Count(&count)
	if count != 1 {
		t.Fatal("user-2's clone should have its own device (randomC)")
	}
}

// V043: ForceNew skips migration table lookup entirely and creates a fresh device.
// Even when a migration record exists for the legacy ID, ForceNew bypasses recovery.
func TestRegisterDevice_ForceNew_SkipsMigrationLookup(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u1", "user-1", "User One", time.Now(), time.Now())

	_, _, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:    "hash_orig",
		DisplayName: "Original",
		Platform:    "windows",
		Version:     "1.0.0",
	})
	if err != nil {
		t.Fatalf("create original: %v", err)
	}

	_, _, err = svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "migratedA",
		LegacyDeviceID: "hash_orig",
		DisplayName:    "Migrated",
		Platform:       "windows",
		Version:        "2.0.0",
	})
	if err != nil {
		t.Fatalf("migration: %v", err)
	}

	// ForceNew should skip migration lookup and create a brand new device
	newDev, _, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "freshB",
		LegacyDeviceID: "hash_orig",
		DisplayName:    "Fresh Device",
		Platform:       "windows",
		Version:        "2.0.0",
		ForceNew:       true,
	})
	if err != nil {
		t.Fatalf("force-new registration: %v", err)
	}
	if newDev.DeviceID != "freshB" {
		t.Fatalf("device_id = %q, want freshB", newDev.DeviceID)
	}

	var count int64
	db.Model(&models.Device{}).Count(&count)
	if count != 2 {
		t.Fatalf("expected 2 devices (migratedA + freshB), got %d", count)
	}

	db.Where("device_id = ?", "migratedA").Model(&models.Device{}).Count(&count)
	if count != 1 {
		t.Fatal("migratedA should still exist (untouched by ForceNew)")
	}
}

// V044: First-time v2 registration records legacy→random migration.
// When device_v2.json is later deleted, the legacy fingerprint can recover the device.
//
// Timeline:
//  1. Fresh registration: deviceId="randomA", legacyDeviceId="hash_fp"
//  2. Server creates device AND records migration(hash_fp → randomA)
//  3. device_v2.json deleted → re-register: deviceId="randomB", legacyDeviceId="hash_fp"
//  4. Server: hash_fp not in devices → check migrations → found randomA → RecoveryError
//  5. User confirms → device recovered (same DB row as randomA, updated to randomB)
func TestRegisterDevice_FirstV2Registration_RecordsMigrationForRecovery(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u1", "user-1", "User One", time.Now(), time.Now())

	// Step 1-2: Fresh v2 registration with legacy fingerprint
	dev, _, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "randomA",
		LegacyDeviceID: "hash_fp",
		DisplayName:    "My Device",
		Platform:       "windows",
		Version:        "2.0.0",
	})
	if err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if dev.DeviceID != "randomA" {
		t.Fatalf("device_id = %q, want randomA", dev.DeviceID)
	}

	// Migration record must exist
	var migCount int64
	db.Model(&models.DeviceMigration{}).Where("old_device_id = ? AND new_device_id = ? AND user_id = ?",
		"hash_fp", "randomA", "user-1").Count(&migCount)
	if migCount != 1 {
		t.Fatalf("expected 1 migration record (hash_fp → randomA), got %d", migCount)
	}

	// Step 3-4: device_v2.json deleted → re-register → should get RecoveryError
	_, _, err = svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "randomB",
		LegacyDeviceID: "hash_fp",
		DisplayName:    "My Device",
		Platform:       "windows",
		Version:        "2.0.0",
	})
	var recoveryErr *RecoveryError
	if !errors.As(err, &recoveryErr) {
		t.Fatalf("expected RecoveryError after v2 deletion, got: %v", err)
	}
	if recoveryErr.RecoverableDeviceID != "randomA" {
		t.Fatalf("recoverable device = %q, want randomA", recoveryErr.RecoverableDeviceID)
	}

	// Step 5: user confirms → device recovered
	recovered, reToken, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:        "randomB",
		LegacyDeviceID:  "hash_fp",
		DisplayName:     "My Device",
		Platform:        "windows",
		Version:         "2.0.0",
		ConfirmRecovery: true,
	})
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if recovered.DeviceID != "randomB" {
		t.Fatalf("recovered device_id = %q, want randomB", recovered.DeviceID)
	}
	if recovered.ID != dev.ID {
		t.Fatalf("should reuse same DB row (UUID %q), got %q", dev.ID, recovered.ID)
	}

	var devCount int64
	db.Model(&models.Device{}).Count(&devCount)
	if devCount != 1 {
		t.Fatalf("expected 1 device (recovered), got %d", devCount)
	}

	_, err = svc.VerifyDeviceToken(reToken)
	if err != nil {
		t.Fatalf("new token should be valid: %v", err)
	}
}

// V045: Fingerprint change (MAC swap) — device reports new legacy fingerprint.
// Server records it so future recovery works with the new fingerprint.
func TestUpdateLegacyFingerprint_RecordsAndIsIdempotent(t *testing.T) {
	db := setupDeviceServiceDB(t)
	svc := &DeviceService{DB: db}

	db.Exec("INSERT INTO users (id, subject_id, display_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"u1", "user-1", "User One", time.Now(), time.Now())

	_, _, err := svc.RegisterDevice("user-1", RegisterDeviceRequest{
		DeviceID:       "randomA",
		LegacyDeviceID: "mac_hash_1",
		DisplayName:    "My Device",
		Platform:       "windows",
		Version:        "2.0.0",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	err = svc.UpdateLegacyFingerprint("randomA", "user-1", "mac_hash_2")
	if err != nil {
		t.Fatalf("update fingerprint: %v", err)
	}

	var count int64
	db.Model(&models.DeviceMigration{}).Where("old_device_id = ? AND new_device_id = ? AND user_id = ?",
		"mac_hash_2", "randomA", "user-1").Count(&count)
	if count != 1 {
		t.Fatalf("expected migration record for mac_hash_2 → randomA, got %d", count)
	}

	// Idempotent: calling again should not create a duplicate
	err = svc.UpdateLegacyFingerprint("randomA", "user-1", "mac_hash_2")
	if err != nil {
		t.Fatalf("idempotent update: %v", err)
	}
	db.Model(&models.DeviceMigration{}).Where("old_device_id = ? AND new_device_id = ? AND user_id = ?",
		"mac_hash_2", "randomA", "user-1").Count(&count)
	if count != 1 {
		t.Fatalf("should still be 1 record (idempotent), got %d", count)
	}

	// Both fingerprints should be able to find the device
	if id := svc.lookupMigratedDeviceID("mac_hash_1", "user-1"); id != "randomA" {
		t.Fatalf("mac_hash_1 should resolve to randomA, got %q", id)
	}
	if id := svc.lookupMigratedDeviceID("mac_hash_2", "user-1"); id != "randomA" {
		t.Fatalf("mac_hash_2 should resolve to randomA, got %q", id)
	}
}
