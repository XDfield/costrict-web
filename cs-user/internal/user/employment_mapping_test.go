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
	// `!Before` (>=) rather than `After` (>) because sqlite serializes the
	// timestamptz to a string and parses it back, dropping the monotonic
	// clock. On Windows (15ms granularity) the wall clock can land on the
	// same nanosecond, making strict `After(before)` flaky.
	if got.LastSyncedAt.Before(before) {
		t.Errorf("LastSyncedAt %v should be >= apply start %v", got.LastSyncedAt, before)
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

// TestApplyEnterpriseMapping_FieldMapAccepted verifies that the top-level
// field_map key parses cleanly. When ExternalClaims is NOT supplied the
// write path stays in stub mode — every enterprise column remains NULL even
// though field_map is configured. Tests in the "Slice 1.5 runtime" block
// below cover the populated-from-claims contract.
func TestApplyEnterpriseMapping_FieldMapAccepted(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default", `employment_providers:
  enabled: [wxwork]
  field_map:
    wxwork:
      enterprise_uid: "UserId"
      employee_number: "JobNumber"
      cost_center: "Department"
      org_path: "FullPath"
      hire_date: "JoinTime"
`)

	err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID: "usr_alice",
		Provider:      "wxwork",
		// ExternalClaims intentionally omitted — stub write path.
	})
	if err != nil {
		t.Fatalf("ApplyEnterpriseMapping with field_map: %v", err)
	}

	var got models.EmploymentIdentity
	if err := svc.db.Where("user_subject_id = ?", "usr_alice").Take(&got).Error; err != nil {
		t.Fatalf("row missing: %v", err)
	}
	// Stub-path invariant: no ExternalClaims → every mapped column stays NULL
	// (applyFieldMap returns empty when claims is empty). Populated-from-claims
	// behavior is pinned by TestApplyEnterpriseMapping_PopulatesFromClaims below.
	if got.EnterpriseUID != nil {
		t.Errorf("EnterpriseUID: got %v, want nil without ExternalClaims", *got.EnterpriseUID)
	}
}

// TestApplyEnterpriseMapping_FieldMapRejectsUnknownColumn verifies that a
// typo in field_map (internal column not in allowedEmploymentColumns)
// surfaces as a parse-time error rather than silently no-op'ing at runtime.
func TestApplyEnterpriseMapping_FieldMapRejectsUnknownColumn(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default", `employment_providers:
  enabled: [wxwork]
  field_map:
    wxwork:
      enterprise_uid: "UserId"
      manager_email: "leader@email"   # not in allowedEmploymentColumns
`)

	err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID: "usr_alice",
		Provider:      "wxwork",
	})
	if err == nil {
		t.Fatal("expected error for unknown internal column, got nil")
	}
	if errors.Is(err, ErrEnterpriseMappingDisabled) {
		t.Fatalf("field_map typo must surface as config error, not silent disable: %v", err)
	}
	if !strings.Contains(err.Error(), "unknown internal column") {
		t.Fatalf("error should mention unknown internal column, got: %v", err)
	}
	if !strings.Contains(err.Error(), "manager_email") {
		t.Fatalf("error should name the offending column, got: %v", err)
	}

	// No row should be written when config parse fails.
	var count int64
	svc.db.Model(&models.EmploymentIdentity{}).Count(&count)
	if count != 0 {
		t.Errorf("expected 0 rows on config error, got %d", count)
	}
}

// TestApplyEnterpriseMapping_FieldMapValidatesUnenabledProviders verifies
// that field_map validation is fail-fast: every entry in field_map is
// validated against allowedEmploymentColumns regardless of whether the
// provider is in the enabled list. This catches operator typos in pre-staged
// config before a future "enable this provider" flip silently no-ops.
//
// Consequence: if you want to stage a draft mapping with intentionally
// non-canonical field names, comment the block out or omit it entirely —
// do NOT leave it under field_map expecting it to be ignored.
func TestApplyEnterpriseMapping_FieldMapValidatesUnenabledProviders(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default", `employment_providers:
  enabled: [wxwork]
  field_map:
    wxwork:
      enterprise_uid: "UserId"
    azure_ad:                          # not in enabled, but VALID columns
      enterprise_uid: "oid"
      employee_number: "empId"
`)

	// Valid columns on an unenabled provider → parse OK (apply proceeds for wxwork).
	err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID: "usr_alice",
		Provider:      "wxwork",
	})
	if err != nil {
		t.Fatalf("ApplyEnterpriseMapping: %v", err)
	}
}

// TestApplyEnterpriseMapping_FieldMapRejectsBadColumnOnUnenabledProvider
// pins the fail-fast contract: a typo'd internal column on a provider NOT in
// enabled still surfaces as a parse error. Operators get the error at the
// first ApplyEnterpriseMapping call, not at a future "enable" flip.
func TestApplyEnterpriseMapping_FieldMapRejectsBadColumnOnUnenabledProvider(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default", `employment_providers:
  enabled: [wxwork]
  field_map:
    wxwork:
      enterprise_uid: "UserId"
    azure_ad:                          # not in enabled, but typo'd column
      enterprise_uid: "oid"
      manager_email: "leader@email"    # not in allowedEmploymentColumns
`)

	err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID: "usr_alice",
		Provider:      "wxwork",         // enabled provider with VALID mapping
	})
	if err == nil {
		t.Fatal("expected parse error from bad column on unenabled provider, got nil")
	}
	if !strings.Contains(err.Error(), "unknown internal column") {
		t.Fatalf("error should mention unknown internal column, got: %v", err)
	}
	if !strings.Contains(err.Error(), "azure_ad") {
		t.Fatalf("error should name the offending provider, got: %v", err)
	}
}

// TestAllowedEmploymentColumns pins the whitelist so a model field rename or
// removal doesn't silently shrink the operator's mapping vocabulary.
func TestAllowedEmploymentColumns(t *testing.T) {
	t.Parallel()
	want := []string{
		"enterprise_uid",
		"employee_number",
		"cost_center",
		"org_path",
		"direct_manager_subject_id",
		"direct_manager_external_ref",
		"job_title",
		"job_level",
		"employment_type",
		"hire_date",
		"regular_date",
		"work_location",
	}
	if len(allowedEmploymentColumns) != len(want) {
		t.Errorf("allowedEmploymentColumns size: got %d, want %d", len(allowedEmploymentColumns), len(want))
	}
	for _, col := range want {
		if _, ok := allowedEmploymentColumns[col]; !ok {
			t.Errorf("allowedEmploymentColumns missing %q", col)
		}
	}
}

// --- Slice 1.5: runtime field_map consumption ---

// TestApplyFieldMap_HappyPath unit-tests the mapping function directly with a
// mix of string + date columns. Exercises:
//   - string coercion via fmt.Sprint (numeric claim → string)
//   - date coercion from RFC 3339 string
//   - missing external field key → column skipped (not zero-valued)
//   - field_map column not in claims → silently absent from output
func TestApplyFieldMap_HappyPath(t *testing.T) {
	t.Parallel()
	fm := FieldMapConfig{
		"enterprise_uid":  "UserId",
		"employee_number": "JobNumber",
		"cost_center":     "Department",
		"hire_date":       "JoinTime",
		"job_level":       "Rank", // not present in claims → skipped
	}
	claims := map[string]any{
		"UserId":     "wx_alice_001",
		"JobNumber":  10042, // int → stringified
		"Department": "R&D",
		"JoinTime":   "2020-03-15T08:00:00Z",
	}

	got := applyFieldMap(fm, claims)
	if got["enterprise_uid"] != "wx_alice_001" {
		t.Errorf("enterprise_uid: got %v, want wx_alice_001", got["enterprise_uid"])
	}
	if got["employee_number"] != "10042" {
		t.Errorf("employee_number: got %v, want 10042 (int → stringified)", got["employee_number"])
	}
	if got["cost_center"] != "R&D" {
		t.Errorf("cost_center: got %v, want R&D", got["cost_center"])
	}
	hire, ok := got["hire_date"].(time.Time)
	if !ok {
		t.Fatalf("hire_date: expected time.Time, got %T", got["hire_date"])
	}
	wantHire := time.Date(2020, 3, 15, 8, 0, 0, 0, time.UTC)
	if !hire.Equal(wantHire) {
		t.Errorf("hire_date: got %v, want %v", hire, wantHire)
	}
	if _, present := got["job_level"]; present {
		t.Errorf("job_level should be absent (Rank missing from claims), got %v", got["job_level"])
	}
}

// TestApplyFieldMap_DottedPath verifies that external field references with
// "." walk nested claim maps. This is what lets a Casdoor-brokered IdP
// (idtrust, custom OAuth apps) be configured via field_map without server
// hard-coding the per-provider property namespace:
//
//	employment_providers.field_map.idtrust.enterprise_uid: "properties.oauth_Custom.id"
//
// Mirror of server's legacy authidentity strategy, but driven entirely by
// tenant config.
func TestApplyFieldMap_DottedPath(t *testing.T) {
	t.Parallel()

	// Simulates the shape of a Casdoor JWT for an idtrust login: per-provider
	// fields nested under properties.<oauth_prefix>.<field>.
	claims := map[string]any{
		"signupApplication": "idtrust",
		"properties": map[string]any{
			"oauth_Custom": map[string]any{
				"id":       "sangfor-001",
				"username": "alice",
				"jobTitle": "Staff Engineer",
			},
			"oauth_GitHub": map[string]any{
				"id": "should-not-leak-through-this-prefix",
			},
		},
	}

	fm := FieldMapConfig{
		"enterprise_uid": "properties.oauth_Custom.id",
		"job_title":      "properties.oauth_Custom.jobTitle",
		// Path that genuinely doesn't exist — must soft-skip, not panic.
		"cost_center": "properties.oauth_Nonexistent.id",
		// Path walks past a non-map leaf — must soft-skip, not panic.
		"org_path": "properties.oauth_Custom.id.bogus",
		// Top-level still works alongside dotted paths.
		"employment_type": "signupApplication",
	}

	got := applyFieldMap(fm, claims)
	if got["enterprise_uid"] != "sangfor-001" {
		t.Errorf("enterprise_uid: got %v, want sangfor-001", got["enterprise_uid"])
	}
	if got["job_title"] != "Staff Engineer" {
		t.Errorf("job_title: got %v, want 'Staff Engineer'", got["job_title"])
	}
	if _, present := got["cost_center"]; present {
		t.Errorf("cost_center should be absent (no such path), got %v", got["cost_center"])
	}
	if _, present := got["org_path"]; present {
		t.Errorf("org_path should be absent (path walked past a string leaf), got %v", got["org_path"])
	}
	if got["employment_type"] != "idtrust" {
		t.Errorf("employment_type: got %v, want idtrust", got["employment_type"])
	}
}

// TestApplyFieldMap_EmptyInputs confirms the stub-write-path invariant: with
// no field_map or no claims, the output is an empty map (no columns mapped),
// so callers that haven't wired ExternalClaims yet behave identically to the
// Slice 1 stub.
func TestApplyFieldMap_EmptyInputs(t *testing.T) {
	t.Parallel()
	if out := applyFieldMap(FieldMapConfig{}, map[string]any{"x": "y"}); len(out) != 0 {
		t.Errorf("empty field_map should yield empty map, got %v", out)
	}
	if out := applyFieldMap(FieldMapConfig{"enterprise_uid": "x"}, nil); len(out) != 0 {
		t.Errorf("nil claims should yield empty map, got %v", out)
	}
	if out := applyFieldMap(FieldMapConfig{"enterprise_uid": "x"}, map[string]any{}); len(out) != 0 {
		t.Errorf("empty claims should yield empty map, got %v", out)
	}
}

// TestApplyFieldMap_DateCoercion covers each numeric shape an IdP might emit
// for a date claim: RFC 3339 string, int64 Unix seconds, float64 (JWT
// NumericDate through encoding/json), and time.Time (already-parsed).
// Unparseable values silently skip the column rather than failing the login.
func TestApplyFieldMap_DateCoercion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  any
		want time.Time
	}{
		{"rfc3339", "2021-01-31T00:00:00Z", time.Date(2021, 1, 31, 0, 0, 0, 0, time.UTC)},
		{"int64_seconds", int64(1612051200), time.Date(2021, 1, 31, 0, 0, 0, 0, time.UTC)},
		{"float64_seconds", float64(1612051200), time.Date(2021, 1, 31, 0, 0, 0, 0, time.UTC)},
		{"time_Time", time.Date(2021, 1, 31, 0, 0, 0, 0, time.UTC), time.Date(2021, 1, 31, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fm := FieldMapConfig{"hire_date": "JoinTime"}
			claims := map[string]any{"JoinTime": tc.raw}
			got := applyFieldMap(fm, claims)
			hire, ok := got["hire_date"].(time.Time)
			if !ok {
				t.Fatalf("hire_date not coerced to time.Time: got %T (%v)", got["hire_date"], got["hire_date"])
			}
			if !hire.Equal(tc.want) {
				t.Errorf("hire_date: got %v, want %v", hire, tc.want)
			}
		})
	}

	// Unparseable string silently skips the column.
	fm := FieldMapConfig{"hire_date": "JoinTime"}
	got := applyFieldMap(fm, map[string]any{"JoinTime": "not-a-date"})
	if _, present := got["hire_date"]; present {
		t.Errorf("unparseable date should be skipped, got %v", got["hire_date"])
	}
}

// TestApplyEnterpriseMapping_PopulatesFromClaims end-to-end: with field_map
// configured AND ExternalClaims populated, the freshly-created row should
// carry the mapped values on both string and date columns. This is the
// canonical Slice 1.5 contract that field_map actually drives writes.
func TestApplyEnterpriseMapping_PopulatesFromClaims(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default", `employment_providers:
  enabled: [wxwork]
  field_map:
    wxwork:
      enterprise_uid: "UserId"
      employee_number: "JobNumber"
      cost_center: "Department"
      org_path: "FullPath"
      hire_date: "JoinTime"
      job_title: "Title"
`)
	claims := map[string]any{
		"UserId":     "wx_alice_001",
		"JobNumber":  "E-10042",
		"Department": "R&D",
		"FullPath":   "/Corp/RD/Platform",
		"JoinTime":   "2020-03-15T08:00:00Z",
		"Title":      "Staff Engineer",
	}

	err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID:  "usr_alice",
		Provider:       "wxwork",
		ExternalClaims: claims,
	})
	if err != nil {
		t.Fatalf("ApplyEnterpriseMapping: %v", err)
	}

	var got models.EmploymentIdentity
	if err := svc.db.Where("user_subject_id = ?", "usr_alice").Take(&got).Error; err != nil {
		t.Fatalf("row missing: %v", err)
	}
	wantStr := func(field string, got *string, want string) {
		t.Helper()
		if got == nil || *got != want {
			t.Errorf("%s: got %v, want %q", field, got, want)
		}
	}
	wantStr("EnterpriseUID", got.EnterpriseUID, "wx_alice_001")
	wantStr("EmployeeNumber", got.EmployeeNumber, "E-10042")
	wantStr("CostCenter", got.CostCenter, "R&D")
	wantStr("OrgPath", got.OrgPath, "/Corp/RD/Platform")
	wantStr("JobTitle", got.JobTitle, "Staff Engineer")
	if got.HireDate == nil {
		t.Errorf("HireDate: got nil, want 2020-03-15")
	} else {
		wantDay := time.Date(2020, 3, 15, 0, 0, 0, 0, time.UTC)
		// gorm serializes date columns as date-only (no time), so compare Y/M/D.
		gotY, gotM, gotD := got.HireDate.Date()
		wantY, wantM, wantD := wantDay.Date()
		if gotY != wantY || gotM != wantM || gotD != wantD {
			t.Errorf("HireDate: got %v, want Y/M/D=%v/%v/%v", got.HireDate, wantY, wantM, wantD)
		}
	}
}

// TestApplyEnterpriseMapping_MissingExternalClaimLeavesNull verifies the
// soft-fail contract: a field_map entry whose external field is absent from
// claims leaves the corresponding column NULL rather than failing the login.
// This is what makes a partial / draft field_map safe to ship.
func TestApplyEnterpriseMapping_MissingExternalClaimLeavesNull(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default", `employment_providers:
  enabled: [wxwork]
  field_map:
    wxwork:
      enterprise_uid: "UserId"
      employee_number: "JobNumber"
`)
	// Only UserId present; JobNumber missing entirely, also include a nil val.
	claims := map[string]any{
		"UserId":    "wx_alice_001",
		"JobNumber": nil,
	}

	err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID:  "usr_alice",
		Provider:       "wxwork",
		ExternalClaims: claims,
	})
	if err != nil {
		t.Fatalf("ApplyEnterpriseMapping: %v", err)
	}

	var got models.EmploymentIdentity
	if err := svc.db.Where("user_subject_id = ?", "usr_alice").Take(&got).Error; err != nil {
		t.Fatalf("row missing: %v", err)
	}
	if got.EnterpriseUID == nil || *got.EnterpriseUID != "wx_alice_001" {
		t.Errorf("EnterpriseUID: got %v, want wx_alice_001", got.EnterpriseUID)
	}
	if got.EmployeeNumber != nil {
		t.Errorf("EmployeeNumber: got %v, want nil (missing/nil claim → NULL)", *got.EmployeeNumber)
	}
}

// TestApplyEnterpriseMapping_FieldMapRefreshesExistingRow exercises the
// update-in-place path with ExternalClaims: a pre-existing row should pick
// up the mapped values on re-apply (not just the SyncStatus/LastSyncedAt
// fields). Guards against a regression where applyFieldMap output is wired
// into the create path but forgotten on the update path.
func TestApplyEnterpriseMapping_FieldMapRefreshesExistingRow(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default", `employment_providers:
  enabled: [wxwork]
  field_map:
    wxwork:
      enterprise_uid: "UserId"
      employee_number: "JobNumber"
`)
	// Seed an empty row from a previous login that had no claims.
	stale := &models.EmploymentIdentity{
		UserSubjectID: "usr_alice",
		Provider:      "wxwork",
		SyncStatus:    "stale",
	}
	if err := svc.db.Create(stale).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID: "usr_alice",
		Provider:      "wxwork",
		ExternalClaims: map[string]any{
			"UserId":    "wx_alice_001",
			"JobNumber": "E-10042",
		},
	})
	if err != nil {
		t.Fatalf("ApplyEnterpriseMapping: %v", err)
	}

	var got models.EmploymentIdentity
	if err := svc.db.Where("user_subject_id = ?", "usr_alice").Take(&got).Error; err != nil {
		t.Fatalf("row missing: %v", err)
	}
	if got.ID != stale.ID {
		t.Errorf("ID changed: got %d, want %d (update-in-place)", got.ID, stale.ID)
	}
	if got.EnterpriseUID == nil || *got.EnterpriseUID != "wx_alice_001" {
		t.Errorf("EnterpriseUID: got %v, want wx_alice_001 (refreshed via field_map)", got.EnterpriseUID)
	}
	if got.EmployeeNumber == nil || *got.EmployeeNumber != "E-10042" {
		t.Errorf("EmployeeNumber: got %v, want E-10042 (refreshed via field_map)", got.EmployeeNumber)
	}
}

// --- Slice 2: GetOrCreateUser auto-triggers enterprise mapping ---

// TestGetOrCreateUser_TriggersEnterpriseMapping verifies the end-to-end
// Slice 2 contract: a normal OAuth login (GetOrCreateUser) with an enabled
// provider + populated ExternalClaims + configured field_map writes an
// employment_identities row whose enterprise fields come from claims.
//
// This is the canonical "login flows enterprise mapping" path — no explicit
// ApplyEnterpriseMapping call from the handler; the service does it as a
// post-login best-effort hook.
func TestGetOrCreateUser_TriggersEnterpriseMapping(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default", `employment_providers:
  enabled: [wxwork]
  field_map:
    wxwork:
      enterprise_uid: "UserId"
      employee_number: "JobNumber"
      cost_center: "Department"
      hire_date: "JoinTime"
`)

	claims := &models.JWTClaims{
		Sub:               "wx_alice_sub",
		UniversalID:       "wx_alice_uID",
		Name:              "alice",
		PreferredUsername: "alice",
		Email:             "alice@example.com",
		Provider:          "wxwork",
		ProviderUserID:    "wx_alice_sub",
		ExternalClaims: map[string]any{
			"UserId":    "wx_alice_001",
			"JobNumber": "E-10042",
			"Department": "R&D",
			"JoinTime":  "2020-03-15T08:00:00Z",
		},
	}
	user, err := svc.GetOrCreateUser(t.Context(), claims)
	if err != nil {
		t.Fatalf("GetOrCreateUser: %v", err)
	}

	var got models.EmploymentIdentity
	if err := svc.db.Where("user_subject_id = ?", user.SubjectID).Take(&got).Error; err != nil {
		t.Fatalf("employment_identity missing: %v", err)
	}
	if got.Provider != "wxwork" {
		t.Errorf("Provider: got %q, want wxwork", got.Provider)
	}
	wantStr := func(field string, got *string, want string) {
		t.Helper()
		if got == nil || *got != want {
			t.Errorf("%s: got %v, want %q", field, got, want)
		}
	}
	wantStr("EnterpriseUID", got.EnterpriseUID, "wx_alice_001")
	wantStr("EmployeeNumber", got.EmployeeNumber, "E-10042")
	wantStr("CostCenter", got.CostCenter, "R&D")
	if got.HireDate == nil {
		t.Errorf("HireDate: got nil, want 2020-03-15")
	}
}

// TestGetOrCreateUser_EnterpriseMappingBestEffortDoesNotBlockLogin verifies
// that even when ApplyEnterpriseMapping WOULD fail (no tenant_configs row →
// ErrEnterpriseMappingDisabled), GetOrCreateUser itself succeeds and the
// user row is written. Enterprise mapping is a bonus feature; login flow
// must never depend on it.
func TestGetOrCreateUser_EnterpriseMappingBestEffortDoesNotBlockLogin(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	// No seedTenantConfig — feature disabled.

	claims := &models.JWTClaims{
		Sub:               "wx_alice_sub",
		UniversalID:       "wx_alice_uID",
		Name:              "alice",
		PreferredUsername: "alice",
		Email:             "alice@example.com",
		Provider:          "wxwork",
		ProviderUserID:    "wx_alice_sub",
		ExternalClaims:    map[string]any{"UserId": "wx_alice_001"},
	}
	user, err := svc.GetOrCreateUser(t.Context(), claims)
	if err != nil {
		t.Fatalf("GetOrCreateUser should succeed with mapping disabled, got: %v", err)
	}
	if user == nil || user.SubjectID == "" {
		t.Fatalf("user row missing: %+v", user)
	}

	// No employment_identities row should exist.
	var count int64
	svc.db.Model(&models.EmploymentIdentity{}).Count(&count)
	if count != 0 {
		t.Errorf("expected 0 employment_identities rows when feature disabled, got %d", count)
	}
}

// TestGetOrCreateUser_NoExternalClaimsStillWritesStubRow verifies the
// downgrade contract: provider is enabled but ExternalClaims is empty (e.g.
// legacy server path that hasn't been upgraded). The hook still fires and
// writes a stub employment_identities row (enterprise fields NULL).
func TestGetOrCreateUser_NoExternalClaimsStillWritesStubRow(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default",
		"employment_providers:\n  enabled: [wxwork]\n")

	claims := &models.JWTClaims{
		Sub:               "wx_alice_sub",
		UniversalID:       "wx_alice_uID",
		Name:              "alice",
		PreferredUsername: "alice",
		Email:             "alice@example.com",
		Provider:          "wxwork",
		ProviderUserID:    "wx_alice_sub",
		// ExternalClaims intentionally nil.
	}
	user, err := svc.GetOrCreateUser(t.Context(), claims)
	if err != nil {
		t.Fatalf("GetOrCreateUser: %v", err)
	}

	var got models.EmploymentIdentity
	if err := svc.db.Where("user_subject_id = ?", user.SubjectID).Take(&got).Error; err != nil {
		t.Fatalf("stub employment_identity missing: %v", err)
	}
	if got.Provider != "wxwork" {
		t.Errorf("Provider: got %q, want wxwork", got.Provider)
	}
	if got.EnterpriseUID != nil {
		t.Errorf("EnterpriseUID: got %v, want nil without ExternalClaims", *got.EnterpriseUID)
	}
}

// TestGetOrCreateUser_LegacyCasdoorProviderSkipsMapping verifies that a
// claims.Provider="" (legacy Casdoor path without provider routing) does NOT
// trigger ApplyEnterpriseMapping. The hook's nil/empty-provider guard short-
// circuits before the DB lookup, so no row is written and no tenant_configs
// query fires.
func TestGetOrCreateUser_LegacyCasdoorProviderSkipsMapping(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default",
		"employment_providers:\n  enabled: [wxwork]\n")

	claims := &models.JWTClaims{
		Sub:               "casdoor_sub",
		UniversalID:       "casdoor_uID",
		Name:              "alice",
		PreferredUsername: "alice",
		Email:             "alice@example.com",
		Provider:          "", // legacy Casdoor path
		ExternalClaims:    map[string]any{"UserId": "wx_alice_001"},
	}
	if _, err := svc.GetOrCreateUser(t.Context(), claims); err != nil {
		t.Fatalf("GetOrCreateUser: %v", err)
	}

	var count int64
	svc.db.Model(&models.EmploymentIdentity{}).Count(&count)
	if count != 0 {
		t.Errorf("expected 0 rows when Provider empty, got %d", count)
	}
}

// TestGetOrCreateUser_EnterpriseMapping_MalformedConfigDoesNotBlockLogin
// verifies the swallow-all-errors invariant: a malformed tenant_configs YAML
// would normally surface as a parse error from ApplyEnterpriseMapping, but
// the post-login hook must swallow it so login still succeeds. Operator
// config bugs surface in monitoring, not in user-facing login failures.
func TestGetOrCreateUser_EnterpriseMapping_MalformedConfigDoesNotBlockLogin(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default", "employment_providers:\n  enabled: [unclosed")

	claims := &models.JWTClaims{
		Sub:               "wx_alice_sub",
		UniversalID:       "wx_alice_uID",
		Name:              "alice",
		PreferredUsername: "alice",
		Email:             "alice@example.com",
		Provider:          "wxwork",
		ExternalClaims:    map[string]any{"UserId": "wx_alice_001"},
	}
	user, err := svc.GetOrCreateUser(t.Context(), claims)
	if err != nil {
		t.Fatalf("malformed YAML must not block login, got: %v", err)
	}
	if user == nil || user.SubjectID == "" {
		t.Fatalf("user row missing despite config error: %+v", user)
	}

	// No employment_identities row written (parsing failed before write).
	var count int64
	svc.db.Model(&models.EmploymentIdentity{}).Count(&count)
	if count != 0 {
		t.Errorf("expected 0 rows on parse failure, got %d", count)
	}
}

// --- A7: GetEmploymentIdentity reader ---

// TestGetEmploymentIdentity_HappyPath verifies a known row is returned with
// all enterprise fields intact. The reissue-token flow (A7) consumes this
// output and forwards it into auth.NewEnterpriseClaims.
func TestGetEmploymentIdentity_HappyPath(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)

	empNum := "E-1001"
	jobTitle := "Staff Engineer"
	row := &models.EmploymentIdentity{
		UserSubjectID:  "usr_alice",
		Provider:       "idtrust",
		EmployeeNumber: &empNum,
		JobTitle:       &jobTitle,
		SyncStatus:     "fresh",
		LastSyncedAt:   time.Now(),
		NextSyncDueAt:  time.Now().Add(employmentSyncInterval),
	}
	if err := svc.db.Create(row).Error; err != nil {
		t.Fatalf("seed row: %v", err)
	}

	got, err := svc.GetEmploymentIdentity(t.Context(), "usr_alice")
	if err != nil {
		t.Fatalf("GetEmploymentIdentity: %v", err)
	}
	if got == nil {
		t.Fatal("got nil row, want non-nil")
	}
	if got.UserSubjectID != "usr_alice" {
		t.Errorf("UserSubjectID: got %q", got.UserSubjectID)
	}
	if got.EmployeeNumber == nil || *got.EmployeeNumber != empNum {
		t.Errorf("EmployeeNumber: got %v, want %q", got.EmployeeNumber, empNum)
	}
	if got.JobTitle == nil || *got.JobTitle != jobTitle {
		t.Errorf("JobTitle: got %v, want %q", got.JobTitle, jobTitle)
	}
}

// TestGetEmploymentIdentity_MissingRowReturnsNilNotFound pins the
// graceful-degradation contract: a user without an employment_identities
// row (provider not enabled, never synced) yields (nil, nil) — the A7
// reissue flow treats this as "no enterprise context" rather than failing.
func TestGetEmploymentIdentity_MissingRowReturnsNilNotFound(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)

	got, err := svc.GetEmploymentIdentity(t.Context(), "usr_ghost")
	if err != nil {
		t.Fatalf("expected nil error for missing row, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil row for missing user, got %+v", got)
	}
}

// TestGetEmploymentIdentity_SoftDeletedExcluded verifies gorm's DeletedAt
// handling hides soft-deleted rows. The unique index in the migration allows
// re-create after soft-delete, so this test guards against accidentally
// surfacing tombstoned rows to the JWT issuance path.
func TestGetEmploymentIdentity_SoftDeletedExcluded(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)

	row := &models.EmploymentIdentity{
		UserSubjectID: "usr_alice",
		Provider:      "idtrust",
		SyncStatus:    "fresh",
	}
	if err := svc.db.Create(row).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := svc.db.Delete(row).Error; err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	got, err := svc.GetEmploymentIdentity(t.Context(), "usr_alice")
	if err != nil {
		t.Fatalf("soft-deleted query: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for soft-deleted row, got %+v", got)
	}
}

// TestGetEmploymentIdentity_EmptySubjectErrors verifies the input guard —
// empty userSubjectID is a programming error and surfaces as
// ErrEmptySubjectID (not a generic 500).
func TestGetEmploymentIdentity_EmptySubjectErrors(t *testing.T) {
	t.Parallel()
	svc := newEmploymentMappingService(t)

	_, err := svc.GetEmploymentIdentity(t.Context(), "")
	if !errors.Is(err, ErrEmptySubjectID) {
		t.Errorf("got err=%v, want ErrEmptySubjectID", err)
	}
}

// TestGetEmploymentIdentity_NilDBGuard verifies the defensive nil-receiver
// path. The reissue-token handler may construct a request before this
// service is wired; panicking would 500 the whole request, but returning
// an error lets the caller map it cleanly.
func TestGetEmploymentIdentity_NilDBGuard(t *testing.T) {
	var svc *Service // nil receiver
	_, err := svc.GetEmploymentIdentity(t.Context(), "usr_alice")
	if err == nil {
		t.Fatal("expected error from nil-receiver Service, got nil")
	}
}
