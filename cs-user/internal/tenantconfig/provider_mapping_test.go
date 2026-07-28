//go:build cgo

package tenantconfig

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
)

// ---------------- ParseProviderMapping ----------------

func TestParseProviderMapping_EmptyBlob(t *testing.T) {
	got, err := ParseProviderMapping("")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Providers) != 0 {
		t.Errorf("want empty map, got %v", got.Providers)
	}
}

func TestParseProviderMapping_NoSection(t *testing.T) {
	// Blob exists but has no provider_mapping key — should return empty.
	got, err := ParseProviderMapping("employment_providers:\n  enabled: [wxwork]")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Providers) != 0 {
		t.Errorf("want empty providers, got %v", got.Providers)
	}
}

func TestParseProviderMapping_WithSection(t *testing.T) {
	blob := "provider_mapping:\n  providers:\n    ldap:\n      rank: 200\n      field_map:\n        employee_number: emp_id"
	got, err := ParseProviderMapping(blob)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p, ok := got.Providers["ldap"]
	if !ok {
		t.Fatalf("ldap not in %v", got.Providers)
	}
	if p.Rank == nil || *p.Rank != 200 {
		t.Errorf("rank: got %v want 200", p.Rank)
	}
	if p.FieldMap["employee_number"] != "emp_id" {
		t.Errorf("field_map: got %q", p.FieldMap["employee_number"])
	}
}

func TestParseProviderMapping_MalformedYAML_ErrInvalidYAML(t *testing.T) {
	_, err := ParseProviderMapping("providers: [unterminated\n")
	if !errors.Is(err, ErrInvalidYAML) {
		t.Errorf("want ErrInvalidYAML, got %v", err)
	}
}

// ---------------- Validate ----------------

func TestValidate_DefaultsEnabledTrue(t *testing.T) {
	m := &ProviderMapping{Providers: map[string]Provider{
		"ldap": {Rank: intPtr(200)},
	}}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.Providers["ldap"].Enabled == nil || *m.Providers["ldap"].Enabled != true {
		t.Errorf("Enabled should default to true, got %v", m.Providers["ldap"].Enabled)
	}
}

func TestValidate_ExplicitEnabledFalsePreserved(t *testing.T) {
	f := false
	m := &ProviderMapping{Providers: map[string]Provider{
		"ldap": {Enabled: &f},
	}}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.Providers["ldap"].Enabled == nil || *m.Providers["ldap"].Enabled != false {
		t.Errorf("Enabled should be false, got %v", m.Providers["ldap"].Enabled)
	}
}

func TestValidate_InvalidProviderName(t *testing.T) {
	cases := []string{
		"Ldap",                  // uppercase
		"ldap-prod",             // hyphen not allowed
		"",                      // empty
		"ldap.prod",             // dot
		strings.Repeat("a", 65), // too long
	}
	for _, name := range cases {
		m := &ProviderMapping{Providers: map[string]Provider{name: {}}}
		err := m.Validate()
		if !errors.Is(err, ErrProviderNameInvalid) {
			t.Errorf("name %q: want ErrProviderNameInvalid, got %v", name, err)
		}
	}
}

func TestValidate_ValidProviderNames(t *testing.T) {
	cases := []string{"ldap", "wxwork", "azure_ad", "dingtalk", "a", strings.Repeat("a", 64)}
	for _, name := range cases {
		m := &ProviderMapping{Providers: map[string]Provider{name: {}}}
		if err := m.Validate(); err != nil {
			t.Errorf("name %q: want nil, got %v", name, err)
		}
	}
}

func TestValidate_NegativeRank(t *testing.T) {
	m := &ProviderMapping{Providers: map[string]Provider{
		"ldap": {Rank: intPtr(-1)},
	}}
	if err := m.Validate(); err == nil || !errors.Is(err, ErrRankNegative) {
		t.Errorf("want ErrRankNegative, got %v", err)
	}
}

func TestValidate_ZeroRank_OK(t *testing.T) {
	m := &ProviderMapping{Providers: map[string]Provider{
		"ldap": {Rank: intPtr(0)},
	}}
	if err := m.Validate(); err != nil {
		t.Errorf("rank=0 should be valid, got %v", err)
	}
}

func TestValidate_IntervalValid(t *testing.T) {
	cases := []string{"", "30m", "6h", "1h30m", "24h"}
	for _, s := range cases {
		m := &ProviderMapping{Providers: map[string]Provider{
			"ldap": {EnterpriseSync: &EnterpriseSync{Interval: s}},
		}}
		if err := m.Validate(); err != nil {
			t.Errorf("interval %q: want nil, got %v", s, err)
		}
	}
}

func TestValidate_IntervalInvalid(t *testing.T) {
	cases := []string{"abc", "10", "0s", "-1h", "9999h"}
	for _, s := range cases {
		m := &ProviderMapping{Providers: map[string]Provider{
			"ldap": {EnterpriseSync: &EnterpriseSync{Interval: s}},
		}}
		err := m.Validate()
		if !errors.Is(err, ErrIntervalInvalid) {
			t.Errorf("interval %q: want ErrIntervalInvalid, got %v", s, err)
		}
	}
}

func TestValidate_NilMapping_OK(t *testing.T) {
	var m *ProviderMapping
	if err := m.Validate(); err != nil {
		t.Errorf("nil mapping: want nil, got %v", err)
	}
}

// ---------------- Service: GetProviderMapping ----------------

func TestService_GetProviderMapping_MissingRow_EmptyMapping(t *testing.T) {
	s := New(newDB(t))
	got, err := s.GetProviderMapping(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("GetProviderMapping: %v", err)
	}
	if got == nil || got.Providers == nil || len(got.Providers) != 0 {
		t.Errorf("want non-nil empty mapping, got %+v", got)
	}
}

func TestService_GetProviderMapping_WithSection(t *testing.T) {
	db := newDB(t)
	// Seed raw YAML containing both employment_providers and provider_mapping.
	seed := models.TenantConfig{
		TenantID:   "t-acme",
		ConfigYAML: "employment_providers:\n  enabled: [wxwork]\nprovider_mapping:\n  providers:\n    ldap:\n      rank: 200",
	}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := New(db)
	got, err := s.GetProviderMapping(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("GetProviderMapping: %v", err)
	}
	if got.Providers["ldap"].Rank == nil || *got.Providers["ldap"].Rank != 200 {
		t.Errorf("ldap rank: got %v want 200", got.Providers["ldap"].Rank)
	}
}

// ---------------- Service: UpdateProviderMapping ----------------

func TestService_UpdateProviderMapping_FirstWrite(t *testing.T) {
	s := New(newDB(t))
	m := &ProviderMapping{Providers: map[string]Provider{
		"ldap": {Rank: intPtr(200), FieldMap: map[string]string{"employee_number": "emp_id"}},
	}}

	got, err := s.UpdateProviderMapping(context.Background(), UpdateProviderMappingParams{
		TenantID: "t-acme", Mapping: m, UpdatedBy: strPtr("subj-1"),
	})
	if err != nil {
		t.Fatalf("UpdateProviderMapping: %v", err)
	}
	if got.Providers["ldap"].Rank == nil || *got.Providers["ldap"].Rank != 200 {
		t.Errorf("echo rank: got %v", got.Providers["ldap"].Rank)
	}
	if got.Providers["ldap"].Enabled == nil || !*got.Providers["ldap"].Enabled {
		t.Errorf("Enabled should default to true, got %v", got.Providers["ldap"].Enabled)
	}

	// Verify the section landed in the stored blob.
	tc, _ := s.Get(context.Background(), "t-acme")
	if !strings.Contains(tc.ConfigYAML, "provider_mapping:") {
		t.Errorf("blob missing provider_mapping key: %q", tc.ConfigYAML)
	}
	if !strings.Contains(tc.ConfigYAML, "ldap:") {
		t.Errorf("blob missing ldap provider: %q", tc.ConfigYAML)
	}
}

func TestService_UpdateProviderMapping_PreservesSiblingSections(t *testing.T) {
	db := newDB(t)
	// Seed a blob with employment_providers — UpdateProviderMapping must not
	// disturb it.
	seed := models.TenantConfig{
		TenantID:   "t-acme",
		ConfigYAML: "employment_providers:\n  enabled: [wxwork, dingtalk]",
	}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := New(db)

	m := &ProviderMapping{Providers: map[string]Provider{
		"ldap": {Rank: intPtr(150)},
	}}
	_, err := s.UpdateProviderMapping(context.Background(), UpdateProviderMappingParams{
		TenantID: "t-acme", Mapping: m,
	})
	if err != nil {
		t.Fatalf("UpdateProviderMapping: %v", err)
	}

	tc, _ := s.Get(context.Background(), "t-acme")
	if !strings.Contains(tc.ConfigYAML, "employment_providers:") {
		t.Errorf("sibling lost: %q", tc.ConfigYAML)
	}
	if !strings.Contains(tc.ConfigYAML, "wxwork") || !strings.Contains(tc.ConfigYAML, "dingtalk") {
		t.Errorf("sibling content lost: %q", tc.ConfigYAML)
	}
	if !strings.Contains(tc.ConfigYAML, "provider_mapping:") {
		t.Errorf("provider_mapping missing: %q", tc.ConfigYAML)
	}
}

func TestService_UpdateProviderMapping_ReplacesSubtreeOnly(t *testing.T) {
	db := newDB(t)
	seed := models.TenantConfig{
		TenantID:   "t-acme",
		ConfigYAML: "provider_mapping:\n  providers:\n    ldap:\n      rank: 100\n    wxwork:\n      rank: 50",
	}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := New(db)

	// PUT with ONLY ldap — wxwork must be dropped (PUT = full replace).
	m := &ProviderMapping{Providers: map[string]Provider{
		"ldap": {Rank: intPtr(999)},
	}}
	got, err := s.UpdateProviderMapping(context.Background(), UpdateProviderMappingParams{
		TenantID: "t-acme", Mapping: m,
	})
	if err != nil {
		t.Fatalf("UpdateProviderMapping: %v", err)
	}
	if len(got.Providers) != 1 {
		t.Errorf("want 1 provider after replace, got %d (%v)", len(got.Providers), got.Providers)
	}
	if _, stillThere := got.Providers["wxwork"]; stillThere {
		t.Errorf("wxwork should be dropped on full replace")
	}
}

func TestService_UpdateProviderMapping_InvalidMapping_Errors(t *testing.T) {
	s := New(newDB(t))
	m := &ProviderMapping{Providers: map[string]Provider{
		"LDAP": {}, // uppercase rejected
	}}
	_, err := s.UpdateProviderMapping(context.Background(), UpdateProviderMappingParams{
		TenantID: "t-acme", Mapping: m,
	})
	if !errors.Is(err, ErrProviderNameInvalid) {
		t.Errorf("want ErrProviderNameInvalid, got %v", err)
	}

	// Blob should NOT have been written.
	tc, _ := s.Get(context.Background(), "t-acme")
	if tc.ConfigYAML != "{}" {
		t.Errorf("blob mutated on validation failure: %q", tc.ConfigYAML)
	}
}

func TestService_UpdateProviderMapping_EmptyTenantID(t *testing.T) {
	s := New(newDB(t))
	_, err := s.UpdateProviderMapping(context.Background(), UpdateProviderMappingParams{
		TenantID: "", Mapping: &ProviderMapping{},
	})
	if !errors.Is(err, ErrEmptyTenantID) {
		t.Errorf("want ErrEmptyTenantID, got %v", err)
	}
}

// ---------------- LoadProviderMapping (E1.2) ----------------

func TestLoadProviderMapping_ReturnsGlobalPlusTenantMerge(t *testing.T) {
	db := newDB(t)
	s := New(db)

	// No tenant-specific override — should return global default only
	mapping, err := s.LoadProviderMapping(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("LoadProviderMapping: %v", err)
	}

	// Check global defaults exist
	if mapping.Version != CurrentSupportedVersion {
		t.Errorf("Version: got %q want %q", mapping.Version, CurrentSupportedVersion)
	}

	// Check a known global provider exists
	gh, ok := mapping.Providers["github"]
	if !ok {
		t.Fatal("github provider missing from global defaults")
	}
	if gh.Enabled == nil || !*gh.Enabled {
		t.Error("github provider should be enabled by default")
	}
	if gh.Rank == nil || *gh.Rank != 100 {
		t.Errorf("github rank: got %v want 100", gh.Rank)
	}
}

func TestLoadProviderMapping_TenantOverride_ReplacesGlobal(t *testing.T) {
	db := newDB(t)
	s := New(db)

	// Seed tenant-specific config that overrides github
	tenantYAML := `
version: "1.0"
provider_mapping:
  providers:
    github:
      enabled: false
      rank: 50
`
	_, err := s.Update(context.Background(), UpdateParams{
		TenantID:   "t-acme",
		ConfigYAML: tenantYAML,
		UpdatedBy:  strPtr("test"),
	})
	if err != nil {
		t.Fatalf("seed tenant config: %v", err)
	}

	// Load effective mapping
	mapping, err := s.LoadProviderMapping(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("LoadProviderMapping: %v", err)
	}

	// Check tenant override is applied
	gh, ok := mapping.Providers["github"]
	if !ok {
		t.Fatal("github provider missing")
	}
	if gh.Enabled == nil || *gh.Enabled {
		t.Error("github provider should be disabled per tenant override")
	}
	if gh.Rank == nil || *gh.Rank != 50 {
		t.Errorf("github rank: got %v want 50 (tenant override)", gh.Rank)
	}

	// Check other global providers still exist
	google, ok := mapping.Providers["google"]
	if !ok {
		t.Error("google provider should still be present from global defaults")
	}
	if google.Enabled == nil || !*google.Enabled {
		t.Error("google provider should be enabled (no tenant override)")
	}
}

func TestLoadProviderMapping_MalformedTenantConfig_ReturnsGlobalOnly(t *testing.T) {
	db := newDB(t)
	s := New(db)

	// Seed invalid YAML
	tenantYAML := `
provider_mapping:
  providers:
    github:
      enabled: not-a-boolean
`
	_, err := s.Update(context.Background(), UpdateParams{
		TenantID:   "t-acme",
		ConfigYAML: tenantYAML,
		UpdatedBy:  strPtr("test"),
	})
	if err != nil {
		t.Fatalf("seed tenant config: %v", err)
	}

	// Should return global defaults without error
	mapping, err := s.LoadProviderMapping(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("LoadProviderMapping should not fail on malformed tenant config: %v", err)
	}

	// Should have global defaults
	if mapping.Version != CurrentSupportedVersion {
		t.Errorf("Version: got %q want %q (global default)", mapping.Version, CurrentSupportedVersion)
	}
	_, ok := mapping.Providers["github"]
	if !ok {
		t.Error("github provider should be present from global defaults")
	}
}

func TestLoadProviderMapping_EmptyTenantID_ReturnsError(t *testing.T) {
	db := newDB(t)
	s := New(db)

	_, err := s.LoadProviderMapping(context.Background(), "")
	if err == nil {
		t.Fatal("LoadProviderMapping should return error for empty tenantID")
	}
}

// ---------------- GetEnabledProviders (E1.6) ----------------

func TestGetEnabledProviders_ReturnsEnabledOnly(t *testing.T) {
	db := newDB(t)
	s := New(db)

	providers, err := s.GetEnabledProviders(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("GetEnabledProviders: %v", err)
	}

	// Should contain github (enabled by default)
	hasGitHub := false
	for _, p := range providers {
		if p == "github" {
			hasGitHub = true
		}
	}
	if !hasGitHub {
		t.Error("github should be in enabled providers")
	}

	// Should contain google (enabled by default)
	hasGoogle := false
	for _, p := range providers {
		if p == "google" {
			hasGoogle = true
		}
	}
	if !hasGoogle {
		t.Error("google should be in enabled providers")
	}
}

func TestGetEnabledProviders_TenantOverride_MergesCorrectly(t *testing.T) {
	db := newDB(t)
	s := New(db)

	// Tenant enables a disabled provider
	tenantYAML := `
version: "1.0"
provider_mapping:
  providers:
    ldap:
      enabled: true
      rank: 400
`
	_, err := s.Update(context.Background(), UpdateParams{
		TenantID:   "t-acme",
		ConfigYAML: tenantYAML,
		UpdatedBy:  strPtr("test"),
	})
	if err != nil {
		t.Fatalf("seed tenant config: %v", err)
	}

	providers, err := s.GetEnabledProviders(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("GetEnabledProviders: %v", err)
	}

	// Should contain ldap (enabled by tenant override)
	hasLDAP := false
	for _, p := range providers {
		if p == "ldap" {
			hasLDAP = true
		}
	}
	if !hasLDAP {
		t.Error("ldap should be in enabled providers (tenant override)")
	}

	// Should still contain github (global default)
	hasGitHub := false
	for _, p := range providers {
		if p == "github" {
			hasGitHub = true
		}
	}
	if !hasGitHub {
		t.Error("github should still be in enabled providers (global default)")
	}
}

// ---------------- Cache (E1.4 + E1.5) ----------------

func TestCache_HitMiss_WorksCorrectly(t *testing.T) {
	db := newDB(t)
	inner := New(db)
	cacher := NewMemoryCache()
	s := NewCachedService(inner, cacher)

	ctx := context.Background()
	tenantID := "t-acme"

	// First call — cache miss, loads from DB
	mapping1, err := s.LoadProviderMapping(ctx, tenantID)
	if err != nil {
		t.Fatalf("LoadProviderMapping (first call): %v", err)
	}

	// Verify it's cached
	_, found := cacher.Get(tenantID)
	if !found {
		t.Error("mapping should be cached after first load")
	}

	// Second call — cache hit (should return same instance)
	mapping2, err := s.LoadProviderMapping(ctx, tenantID)
	if err != nil {
		t.Fatalf("LoadProviderMapping (second call): %v", err)
	}

	// Should be the same instance (from cache)
	if mapping1 != mapping2 {
		t.Error("cache hit should return the same instance")
	}
}

func TestCache_Invalidation_WorksCorrectly(t *testing.T) {
	db := newDB(t)
	inner := New(db)
	cacher := NewMemoryCache()
	s := NewCachedService(inner, cacher)

	ctx := context.Background()
	tenantID := "t-acme"

	// Load and cache
	_, err := s.LoadProviderMapping(ctx, tenantID)
	if err != nil {
		t.Fatalf("LoadProviderMapping (before update): %v", err)
	}

	// Update provider mapping via service (should invalidate cache)
	tenantYAML := `
version: "1.0"
provider_mapping:
  providers:
    github:
      enabled: false
`
	_, err = s.inner.Update(ctx, UpdateParams{
		TenantID:   tenantID,
		ConfigYAML: tenantYAML,
		UpdatedBy:  strPtr("test"),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Cache should be invalidated
	_, found := cacher.Get(tenantID)
	if found {
		t.Error("cache should be invalidated after update")
	}

	// Next load should get fresh data
	mapping, err := s.LoadProviderMapping(ctx, tenantID)
	if err != nil {
		t.Fatalf("LoadProviderMapping (after update): %v", err)
	}

	gh, ok := mapping.Providers["github"]
	if !ok {
		t.Fatal("github provider missing")
	}
	if gh.Enabled == nil || *gh.Enabled {
		t.Error("github should be disabled after tenant update")
	}
}

func TestCache_InvalidateAll_WorksCorrectly(t *testing.T) {
	db := newDB(t)
	inner := New(db)
	cacher := NewMemoryCache()
	s := NewCachedService(inner, cacher)

	ctx := context.Background()

	// Load mappings for multiple tenants
	_, err := s.LoadProviderMapping(ctx, "t-acme")
	if err != nil {
		t.Fatalf("LoadProviderMapping (t-acme): %v", err)
	}
	_, err = s.LoadProviderMapping(ctx, "t-globex")
	if err != nil {
		t.Fatalf("LoadProviderMapping (t-globex): %v", err)
	}

	// Invalidate all
	s.InvalidateAllCaches()

	// Both should be gone
	_, found := cacher.Get("t-acme")
	if found {
		t.Error("t-acme should be invalidated")
	}
	_, found = cacher.Get("t-globex")
	if found {
		t.Error("t-globex should be invalidated")
	}
}

// ---------------- Version Validation (E1.3) ----------------

func TestVersion_DefaultsTo10WhenEmpty(t *testing.T) {
	mapping := &ProviderMapping{
		Providers: map[string]Provider{
			"test": {Enabled: boolPtr(true)},
		},
	}

	err := mapping.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if mapping.Version != CurrentSupportedVersion {
		t.Errorf("Version should default to %q, got %q", CurrentSupportedVersion, mapping.Version)
	}
}

func TestVersion_InvalidVersion_ReturnsError(t *testing.T) {
	mapping := &ProviderMapping{
		Version: "2.0",
		Providers: map[string]Provider{
			"test": {Enabled: boolPtr(true)},
		},
	}

	err := mapping.Validate()
	if err == nil {
		t.Fatal("Validate should return error for unsupported version")
	}
	if !errors.Is(err, ErrVersionInvalid) {
		t.Errorf("want ErrVersionInvalid, got %v", err)
	}
}

func TestVersion_NilMapping_Valid(t *testing.T) {
	var mapping *ProviderMapping
	err := mapping.Validate()
	if err != nil {
		t.Fatalf("Validate on nil mapping should succeed: %v", err)
	}
}
