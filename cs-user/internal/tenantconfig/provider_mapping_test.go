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

// intPtr is a small helper because Go doesn't have one in stdlib.
func intPtr(n int) *int { return &n }
