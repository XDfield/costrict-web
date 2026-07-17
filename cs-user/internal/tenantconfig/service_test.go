//go:build cgo

package tenantconfig

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newDB builds a sqlite :memory: DB with the tenant_configs schema. cgo-gated
// because sqlite needs CGO; same pattern as tenant/admin_test.go.
func newDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.TenantConfig{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	return db
}

func strPtr(s string) *string { return &s }

// ---------------- Get ----------------

func TestGet_MissingRow_ReturnsDefault(t *testing.T) {
	s := New(newDB(t))
	got, err := s.Get(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TenantID != "t-acme" {
		t.Errorf("TenantID: got %q want t-acme", got.TenantID)
	}
	if got.ConfigYAML != "{}" {
		t.Errorf("ConfigYAML: got %q want {}", got.ConfigYAML)
	}
	if !got.CreatedAt.IsZero() {
		t.Errorf("CreatedAt should be zero on synthetic row, got %v", got.CreatedAt)
	}
}

func TestGet_ExistingRow_ReturnsStored(t *testing.T) {
	db := newDB(t)
	// Seed directly (not via Service.Update, which would trim) so we
	// control the exact stored bytes.
	seed := models.TenantConfig{
		TenantID:   "t-acme",
		ConfigYAML: "employment_providers:\n  enabled: [wxwork]",
	}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := New(db)
	got, err := s.Get(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ConfigYAML != seed.ConfigYAML {
		t.Errorf("ConfigYAML: got %q want %q", got.ConfigYAML, seed.ConfigYAML)
	}
}

func TestGet_EmptyTenantID_ErrEmptyTenantID(t *testing.T) {
	s := New(newDB(t))
	_, err := s.Get(context.Background(), "")
	if !errors.Is(err, ErrEmptyTenantID) {
		t.Errorf("want ErrEmptyTenantID, got %v", err)
	}
}

// ---------------- Update — happy paths ----------------

func TestUpdate_FirstWrite_Inserts(t *testing.T) {
	db := newDB(t)
	s := New(db)
	yaml := "employment_providers:\n  enabled: [wxwork]"

	got, err := s.Update(context.Background(), UpdateParams{
		TenantID:   "t-acme",
		ConfigYAML: yaml,
		UpdatedBy:  strPtr("subj-1"),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.ConfigYAML != yaml {
		t.Errorf("ConfigYAML echo: got %q want %q", got.ConfigYAML, yaml)
	}
	if got.UpdatedBy == nil || *got.UpdatedBy != "subj-1" {
		t.Errorf("UpdatedBy: got %v want subj-1", got.UpdatedBy)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set after insert")
	}

	// Re-read to confirm persistence.
	var row models.TenantConfig
	if err := db.Where("tenant_id = ?", "t-acme").Take(&row).Error; err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if row.ConfigYAML != yaml {
		t.Errorf("persisted YAML mismatch: got %q want %q", row.ConfigYAML, yaml)
	}
	if row.UpdatedBy == nil || *row.UpdatedBy != "subj-1" {
		t.Errorf("persisted UpdatedBy mismatch: got %v", row.UpdatedBy)
	}
}

func TestUpdate_SecondWrite_UpdatesInPlace(t *testing.T) {
	db := newDB(t)
	s := New(db)

	// First write.
	v1 := "employment_providers:\n  enabled: [wxwork]"
	if _, err := s.Update(context.Background(), UpdateParams{
		TenantID:   "t-acme",
		ConfigYAML: v1,
		UpdatedBy:  strPtr("subj-1"),
	}); err != nil {
		t.Fatalf("first Update: %v", err)
	}

	// Capture the original created_at to verify it's preserved.
	var before models.TenantConfig
	if err := db.Where("tenant_id = ?", "t-acme").Take(&before).Error; err != nil {
		t.Fatalf("read before: %v", err)
	}
	origCreated := before.CreatedAt

	// Second write with different content + different actor.
	v2 := "provider_mapping:\n  providers:\n    wxwork: {}"
	got, err := s.Update(context.Background(), UpdateParams{
		TenantID:   "t-acme",
		ConfigYAML: v2,
		UpdatedBy:  strPtr("subj-2"),
	})
	if err != nil {
		t.Fatalf("second Update: %v", err)
	}
	if got.ConfigYAML != v2 {
		t.Errorf("echo YAML: got %q want %q", got.ConfigYAML, v2)
	}
	if got.UpdatedBy == nil || *got.UpdatedBy != "subj-2" {
		t.Errorf("echo UpdatedBy: got %v want subj-2", got.UpdatedBy)
	}
	if !got.CreatedAt.Equal(origCreated) {
		t.Errorf("CreatedAt changed: was %v now %v (should be immutable)", origCreated, got.CreatedAt)
	}

	// Exactly one row in the table (insert+update, not insert+insert).
	var count int64
	db.Model(&models.TenantConfig{}).Count(&count)
	if count != 1 {
		t.Errorf("row count: got %d want 1", count)
	}
}

func TestUpdate_EmptyYAML_NormalizesToBraces(t *testing.T) {
	s := New(newDB(t))
	got, err := s.Update(context.Background(), UpdateParams{
		TenantID:   "t-acme",
		ConfigYAML: "   \n  ",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.ConfigYAML != "{}" {
		t.Errorf("normalized YAML: got %q want {}", got.ConfigYAML)
	}
}

func TestUpdate_UpdatedByNil_PreservesNull(t *testing.T) {
	s := New(newDB(t))
	got, err := s.Update(context.Background(), UpdateParams{
		TenantID:   "t-acme",
		ConfigYAML: "{}",
		// UpdatedBy intentionally nil.
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.UpdatedBy != nil {
		t.Errorf("UpdatedBy: got %v want nil", got.UpdatedBy)
	}
}

// ---------------- Update — error paths ----------------

func TestUpdate_InvalidYAML_ErrInvalidYAML(t *testing.T) {
	s := New(newDB(t))
	// Tabs as indentation — yaml.v3 rejects this.
	bad := "employment_providers:\n\tenabled: [wxwork]"
	_, err := s.Update(context.Background(), UpdateParams{
		TenantID:   "t-acme",
		ConfigYAML: bad,
	})
	if !errors.Is(err, ErrInvalidYAML) {
		t.Errorf("want ErrInvalidYAML, got %v", err)
	}

	// Row should NOT exist (validation runs before any DB write).
	var count int64
	s.db.Model(&models.TenantConfig{}).Count(&count)
	if count != 0 {
		t.Errorf("no row should be written on invalid YAML; got %d", count)
	}
}

func TestUpdate_YAMLTooLarge_ErrYAMLTooLarge(t *testing.T) {
	s := New(newDB(t))
	huge := strings.Repeat("a", MaxYAMLLength+1)
	_, err := s.Update(context.Background(), UpdateParams{
		TenantID:   "t-acme",
		ConfigYAML: huge,
	})
	if !errors.Is(err, ErrYAMLTooLarge) {
		t.Errorf("want ErrYAMLTooLarge, got %v", err)
	}
}

func TestUpdate_AtMaxYAML_Accepted(t *testing.T) {
	// Exactly MaxYAMLLength bytes — must be accepted (boundary is inclusive).
	s := New(newDB(t))
	// A long but valid YAML scalar (a single space-indented string).
	huge := "key: " + strings.Repeat("a", MaxYAMLLength-5)
	if len(huge) != MaxYAMLLength {
		t.Fatalf("fixture length: got %d want %d", len(huge), MaxYAMLLength)
	}
	got, err := s.Update(context.Background(), UpdateParams{
		TenantID:   "t-acme",
		ConfigYAML: huge,
	})
	if err != nil {
		t.Fatalf("Update at cap: %v", err)
	}
	if len(got.ConfigYAML) != MaxYAMLLength {
		t.Errorf("stored length: got %d want %d", len(got.ConfigYAML), MaxYAMLLength)
	}
}

func TestUpdate_EmptyTenantID_ErrEmptyTenantID(t *testing.T) {
	s := New(newDB(t))
	_, err := s.Update(context.Background(), UpdateParams{
		TenantID:   "",
		ConfigYAML: "{}",
	})
	if !errors.Is(err, ErrEmptyTenantID) {
		t.Errorf("want ErrEmptyTenantID, got %v", err)
	}
}

// ---------------- Update — YAML tolerance ----------------

func TestUpdate_YAMLUnknownKeysAccepted(t *testing.T) {
	// C3.2 deliberately accepts any parseable YAML — yaml.v3 ignores
	// unknown keys, so a future-section blob (e.g. provider_mapping)
	// stores cleanly even before C3.3 reads it.
	s := New(newDB(t))
	yaml := "employment_providers:\n  enabled: [wxwork]\nprovider_mapping:\n  providers:\n    wxwork:\n      corp_id: wxyz\nunknown_future_section:\n  whatever: true"
	got, err := s.Update(context.Background(), UpdateParams{
		TenantID:   "t-acme",
		ConfigYAML: yaml,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.ConfigYAML != yaml {
		t.Errorf("YAML mutated: got %q want %q", got.ConfigYAML, yaml)
	}
}
