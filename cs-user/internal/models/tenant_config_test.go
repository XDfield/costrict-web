//go:build cgo

package models

import (
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newTenantConfigDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&TenantConfig{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// TestTenantConfig_YAMLColumnRoundTrip writes a representative config_yaml
// blob and asserts it survives Create → First byte-for-byte. Confirms the
// gorm layer doesn't escape / normalize / trim the YAML.
func TestTenantConfig_YAMLColumnRoundTrip(t *testing.T) {
	t.Parallel()
	db := newTenantConfigDB(t)

	const yaml = "employment_providers:\n  enabled: [idtrust]\n  idtrust:\n    ttl: 86400\n"
	tc := &TenantConfig{
		TenantID:   "default",
		ConfigYAML: yaml,
	}
	if err := db.Create(tc).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	var got TenantConfig
	if err := db.First(&got, "tenant_id = ?", "default").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.ConfigYAML != yaml {
		t.Fatalf("config_yaml round-trip mismatch:\n got: %q\nwant: %q", got.ConfigYAML, yaml)
	}
}

// TestTenantConfig_DefaultRowInsert verifies that a row inserted with only
// tenant_id populated picks up the documented defaults (config_yaml='{}',
// timestamps non-zero, updated_by NULL).
func TestTenantConfig_DefaultRowInsert(t *testing.T) {
	t.Parallel()
	db := newTenantConfigDB(t)

	tc := &TenantConfig{TenantID: "default"}
	if err := db.Create(tc).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	var got TenantConfig
	if err := db.First(&got, "tenant_id = ?", "default").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.ConfigYAML != "{}" {
		t.Errorf("config_yaml default: got %q, want {}", got.ConfigYAML)
	}
	if got.UpdatedBy != nil {
		t.Errorf("updated_by default: got %+v, want nil", got.UpdatedBy)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at should default to a non-zero timestamp")
	}
	if got.UpdatedAt.IsZero() {
		t.Error("updated_at should default to a non-zero timestamp")
	}
}

// TestTenantConfig_TenantIDUniquePK asserts the PK on tenant_id rejects a
// second insert with the same value. Soft-delete semantics are NOT present
// on this table (no DeletedAt) — re-onboarding a tenant requires an UPSERT.
func TestTenantConfig_TenantIDUniquePK(t *testing.T) {
	t.Parallel()
	db := newTenantConfigDB(t)

	if err := db.Create(&TenantConfig{TenantID: "default"}).Error; err != nil {
		t.Fatalf("create first: %v", err)
	}
	err := db.Create(&TenantConfig{TenantID: "default"}).Error
	if err == nil {
		t.Fatal("expected PK constraint failure on duplicate tenant_id, got nil")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("expected UNIQUE constraint failure, got: %v", err)
	}
}
