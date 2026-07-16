//go:build cgo

package user

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newEmploymentMappingService opens an in-memory sqlite DB and AutoMigrates
// the four models cs-user owns. Local to this file so we don't churn
// newTestService in service_test.go (which the existing read/write tests
// depend on). If the package grows more cross-model test files, this can
// fold back into newTestService later.
func newEmploymentMappingService(t *testing.T) *Service {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.User{},
		&models.UserAuthIdentity{},
		&models.EmploymentIdentity{},
		&models.TenantConfig{},
	); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return NewService(db)
}

// seedTenantConfig inserts a tenant_configs row with the supplied YAML.
func seedTenantConfig(t *testing.T, svc *Service, tenantID, configYAML string) {
	t.Helper()
	if err := svc.db.Create(&models.TenantConfig{
		TenantID:   tenantID,
		ConfigYAML: configYAML,
	}).Error; err != nil {
		t.Fatalf("seed tenant_config: %v", err)
	}
}

// TestApplyEnterpriseMapping_DisabledProvider verifies that when the auth
// provider is NOT in the tenant's employment_providers.enabled list, the
// method returns ErrEnterpriseMappingDisabled and writes no row. Login is
// not blocked — the caller treats this sentinel as success.
func TestApplyEnterpriseMapping_DisabledProvider(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default",
		"employment_providers:\n  enabled: [idtrust]\n")

	err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID: "usr_alice",
		Provider:      "github", // not in enabled
	})
	if !errors.Is(err, ErrEnterpriseMappingDisabled) {
		t.Fatalf("got err=%v, want ErrEnterpriseMappingDisabled", err)
	}

	// Confirm no row was written.
	var count int64
	svc.db.Model(&models.EmploymentIdentity{}).Count(&count)
	if count != 0 {
		t.Errorf("expected 0 employment_identities rows, got %d", count)
	}
}

// TestApplyEnterpriseMapping_EnabledProvider_NewUser verifies the happy
// path: provider is enabled, no existing row → create with correct fields.
func TestApplyEnterpriseMapping_EnabledProvider_NewUser(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default",
		"employment_providers:\n  enabled: [idtrust, azure_ad]\n")

	before := time.Now()
	err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID: "usr_alice",
		Provider:      "idtrust",
	})
	if err != nil {
		t.Fatalf("ApplyEnterpriseMapping: %v", err)
	}

	var got models.EmploymentIdentity
	if err := svc.db.Where("user_subject_id = ?", "usr_alice").Take(&got).Error; err != nil {
		t.Fatalf("employment_identity missing: %v", err)
	}
	if got.Provider != "idtrust" {
		t.Errorf("Provider: got %q, want idtrust", got.Provider)
	}
	if got.SyncStatus != "fresh" {
		t.Errorf("SyncStatus: got %q, want fresh", got.SyncStatus)
	}
	if got.LastSyncedAt.Before(before) {
		t.Errorf("LastSyncedAt %v before apply start %v", got.LastSyncedAt, before)
	}
	wantNext := got.LastSyncedAt.Add(employmentSyncInterval)
	if !got.NextSyncDueAt.Equal(wantNext) {
		t.Errorf("NextSyncDueAt: got %v, want %v (= LastSyncedAt + 24h)", got.NextSyncDueAt, wantNext)
	}
}

// TestApplyEnterpriseMapping_EnabledProvider_ExistingUser verifies the
// update-in-place path: provider is enabled, row already exists → fields
// are refreshed, ID stays stable.
func TestApplyEnterpriseMapping_EnabledProvider_ExistingUser(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default",
		"employment_providers:\n  enabled: [idtrust]\n")

	// Seed a stale row from "yesterday".
	staleSyncedAt := time.Now().Add(-25 * time.Hour)
	staleNext := staleSyncedAt.Add(employmentSyncInterval)
	row := &models.EmploymentIdentity{
		UserSubjectID: "usr_alice",
		Provider:      "legacy_provider",
		SyncStatus:    "stale",
		LastSyncedAt:  staleSyncedAt,
		NextSyncDueAt: staleNext,
	}
	if err := svc.db.Create(row).Error; err != nil {
		t.Fatalf("seed stale row: %v", err)
	}
	originalID := row.ID

	before := time.Now()
	err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID: "usr_alice",
		Provider:      "idtrust",
	})
	if err != nil {
		t.Fatalf("ApplyEnterpriseMapping: %v", err)
	}

	var got models.EmploymentIdentity
	if err := svc.db.Where("user_subject_id = ?", "usr_alice").Take(&got).Error; err != nil {
		t.Fatalf("employment_identity missing: %v", err)
	}
	if got.ID != originalID {
		t.Errorf("ID changed: got %d, want %d (update-in-place contract)", got.ID, originalID)
	}
	if got.Provider != "idtrust" {
		t.Errorf("Provider: got %q, want idtrust (refreshed from legacy_provider)", got.Provider)
	}
	if got.SyncStatus != "fresh" {
		t.Errorf("SyncStatus: got %q, want fresh", got.SyncStatus)
	}
	if !got.LastSyncedAt.After(staleSyncedAt) {
		t.Errorf("LastSyncedAt should advance: got %v, stale was %v", got.LastSyncedAt, staleSyncedAt)
	}
	if !got.LastSyncedAt.After(before) {
		t.Errorf("LastSyncedAt %v should be after apply start %v", got.LastSyncedAt, before)
	}
}

// TestApplyEnterpriseMapping_EmptyConfigYAML verifies that the bootstrap
// row shipped by A6 (config_yaml="{}") behaves as "no providers enabled"
// rather than erroring — login must not break on a freshly-bootstrapped
// tenant_configs table.
func TestApplyEnterpriseMapping_EmptyConfigYAML(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default", "{}")

	err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID: "usr_alice",
		Provider:      "idtrust",
	})
	if !errors.Is(err, ErrEnterpriseMappingDisabled) {
		t.Fatalf("got err=%v, want ErrEnterpriseMappingDisabled for empty config_yaml", err)
	}
}

// TestApplyEnterpriseMapping_MissingTenantConfig verifies that a missing
// tenant_configs row is treated as disabled rather than erroring. Operators
// who want the feature off simply omit the row.
func TestApplyEnterpriseMapping_MissingTenantConfig(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	// No seedTenantConfig call — table is empty.

	err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID: "usr_alice",
		Provider:      "idtrust",
	})
	if !errors.Is(err, ErrEnterpriseMappingDisabled) {
		t.Fatalf("got err=%v, want ErrEnterpriseMappingDisabled when tenant_configs row missing", err)
	}
}

// TestApplyEnterpriseMapping_MalformedYAML verifies that a malformed
// config_yaml surfaces as a wrapped parse error rather than being silently
// treated as disabled. Operator config bugs deserve to be visible.
func TestApplyEnterpriseMapping_MalformedYAML(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default", "employment_providers:\n  enabled: [unclosed")

	err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID: "usr_alice",
		Provider:      "idtrust",
	})
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
	if errors.Is(err, ErrEnterpriseMappingDisabled) {
		t.Fatalf("malformed YAML must surface as parse error, not silent disable: %v", err)
	}
	// Substring match is the most stable check across yaml.v3 versions —
	// the error type isn't easily unwrap-able across releases.
	if !strings.Contains(err.Error(), "parse config_yaml") {
		t.Fatalf("error should mention parse step, got: %v", err)
	}
}

// TestApplyEnterpriseMapping_DefaultTenantID verifies that an empty
// TenantID falls back to "default" rather than erroring. Phase A callers
// don't need to know the tenant routing.
func TestApplyEnterpriseMapping_DefaultTenantID(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default",
		"employment_providers:\n  enabled: [idtrust]\n")

	err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		TenantID:      "",
		UserSubjectID: "usr_alice",
		Provider:      "idtrust",
	})
	if err != nil {
		t.Fatalf("ApplyEnterpriseMapping with empty TenantID: %v", err)
	}

	var got models.EmploymentIdentity
	if err := svc.db.Where("user_subject_id = ?", "usr_alice").Take(&got).Error; err != nil {
		t.Fatalf("row missing: %v", err)
	}
	if got.Provider != "idtrust" {
		t.Errorf("Provider: got %q, want idtrust", got.Provider)
	}
}

// TestApplyEnterpriseMapping_ValidationErrors verifies the input validation
// guards: empty UserSubjectID and empty Provider return errors before any
// DB access.
func TestApplyEnterpriseMapping_ValidationErrors(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default",
		"employment_providers:\n  enabled: [idtrust]\n")

	tests := []struct {
		name   string
		params EmploymentMappingParams
	}{
		{"empty UserSubjectID", EmploymentMappingParams{Provider: "idtrust"}},
		{"empty Provider", EmploymentMappingParams{UserSubjectID: "usr_alice"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.ApplyEnterpriseMapping(t.Context(), tc.params)
			if err == nil {
				t.Fatalf("expected validation error, got nil")
			}
			if errors.Is(err, ErrEnterpriseMappingDisabled) {
				t.Fatalf("validation error must not be masked as disabled: %v", err)
			}
		})
	}

	// Confirm no row was written.
	var count int64
	svc.db.Model(&models.EmploymentIdentity{}).Count(&count)
	if count != 0 {
		t.Errorf("validation failures should not write rows, got %d", count)
	}
}

// TestApplyEnterpriseMapping_PerProviderConfigIgnored verifies yaml.v3's
// default tolerance for unmapped fields. Phase A reads only `enabled`;
// the canonical per-provider config shape (provider_mapping.providers.<name>
// in MULTI_TENANCY §9.3) is being finalized and will be modeled in the
// follow-up PR that introduces real provider clients. This test pins the
// tolerance contract so follow-ups can swap the YAML freely without
// breaking Phase A's parse path.
func TestApplyEnterpriseMapping_PerProviderConfigIgnored(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	// Synthetic richer-than-minimal YAML blob. Production shape may differ;
	// what matters here is that the unmapped fields don't break parsing.
	seedTenantConfig(t, svc, "default", `employment_providers:
  enabled: [idtrust, azure_ad]
  priority_providers: [idtrust, azure_ad]
  idtrust:
    enabled: true
    interval: "24h"
    on_login: refresh_if_stale
    field_map:
      employee_number: "employeeNumber"
      cost_center: "departmentNumber"
  azure_ad:
    enabled: true
    interval: "12h"
`)

	err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID: "usr_alice",
		Provider:      "idtrust",
	})
	if err != nil {
		t.Fatalf("ApplyEnterpriseMapping with full design YAML: %v", err)
	}

	var got models.EmploymentIdentity
	if err := svc.db.Where("user_subject_id = ?", "usr_alice").Take(&got).Error; err != nil {
		t.Fatalf("row missing: %v", err)
	}
	if got.Provider != "idtrust" {
		t.Errorf("Provider: got %q, want idtrust", got.Provider)
	}
}
