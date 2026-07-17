//go:build cgo

package tenant

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newAdminDB mirrors the resolver test fixture pattern: sqlite :memory: +
// AutoMigrate(&Tenant{}). No pre-seed — admin tests start from an empty
// table so they exercise the create path from a known state. cgo-gated
// because sqlite needs CGO.
func newAdminDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.Tenant{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	return db
}

// seedTenant inserts a tenant row directly (bypassing Admin.CreateTenant) so
// tests of Update / Suspend / Restore / RequestDeletion can start from a
// known lifecycle state without exercising the create path.
func seedTenant(t *testing.T, db *gorm.DB, tn models.Tenant) *models.Tenant {
	t.Helper()
	if tn.TenantID == "" {
		tn.TenantID = "seed-" + tn.Slug
	}
	if tn.Status == "" {
		tn.Status = StatusActive
	}
	if err := db.Create(&tn).Error; err != nil {
		t.Fatalf("seed tenant %s: %v", tn.Slug, err)
	}
	return &tn
}

// ---------------- CreateTenant ----------------

func TestCreateTenant_HappyPath(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)

	tn, err := a.CreateTenant(context.Background(), CreateParams{
		Slug:         "acme",
		DisplayName:  "Acme Inc.",
		EmailDomains: []string{"acme.com", "ACME.CN"},
	})
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if tn.TenantID == "" {
		t.Error("TenantID should be populated")
	}
	if tn.Slug != "acme" {
		t.Errorf("Slug: got %q want %q", tn.Slug, "acme")
	}
	if tn.Status != StatusActive {
		t.Errorf("Status default: got %q want %q", tn.Status, StatusActive)
	}
	if tn.Edition != EditionTeam {
		t.Errorf("Edition default: got %q want %q", tn.Edition, EditionTeam)
	}
	// EmailDomains should be normalized (lowercased) + deduped + JSON-encoded.
	if tn.EmailDomains != `["acme.com","acme.cn"]` {
		t.Errorf("EmailDomains: got %q", tn.EmailDomains)
	}
	if tn.Features != "{}" || tn.Limits != "{}" || tn.Settings != "{}" {
		t.Errorf("JSON defaults wrong: features=%q limits=%q settings=%q", tn.Features, tn.Limits, tn.Settings)
	}
}

func TestCreateTenant_InvalidSlug(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)

	cases := []string{
		"",            // empty
		"ab",          // too short (< 3)
		"Acme",        // uppercase
		"acme corp",   // space
		"acme_special", // underscore (not URL-safe)
		strings_repeat("a", 33), // too long (> 32)
	}
	for _, slug := range cases {
		_, err := a.CreateTenant(context.Background(), CreateParams{
			Slug:        slug,
			DisplayName: "Acme",
		})
		if !errors.Is(err, ErrInvalidSlug) {
			t.Errorf("slug %q: want ErrInvalidSlug, got %v", slug, err)
		}
	}
}

func TestCreateTenant_InvalidDisplayName(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	_, err := a.CreateTenant(context.Background(), CreateParams{
		Slug:        "acme",
		DisplayName: "   ",
	})
	if !errors.Is(err, ErrInvalidDisplayName) {
		t.Errorf("want ErrInvalidDisplayName, got %v", err)
	}
}

func TestCreateTenant_InvalidEdition(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	_, err := a.CreateTenant(context.Background(), CreateParams{
		Slug:        "acme",
		DisplayName: "Acme",
		Edition:     "premium",
	})
	if !errors.Is(err, ErrInvalidEdition) {
		t.Errorf("want ErrInvalidEdition, got %v", err)
	}
}

func TestCreateTenant_SlugTaken(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	seedTenant(t, db, models.Tenant{Slug: "acme", DisplayName: "First"})

	_, err := a.CreateTenant(context.Background(), CreateParams{
		Slug:        "acme",
		DisplayName: "Second",
	})
	if !errors.Is(err, ErrSlugTaken) {
		t.Errorf("want ErrSlugTaken, got %v", err)
	}
}

func TestCreateTenant_EmailDomainConflict(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	seedTenant(t, db, models.Tenant{
		Slug:         "first",
		DisplayName:  "First",
		EmailDomains: `["acme.com"]`,
	})

	_, err := a.CreateTenant(context.Background(), CreateParams{
		Slug:         "second",
		DisplayName:  "Second",
		EmailDomains: []string{"ACME.com"},
	})
	if !errors.Is(err, ErrEmailDomainConflict) {
		t.Errorf("want ErrEmailDomainConflict, got %v", err)
	}
}

// ---------------- ListTenants ----------------

func TestListTenants_Pagination(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	for i := 0; i < 5; i++ {
		seedTenant(t, db, models.Tenant{
			TenantID:    tidI(i),
			Slug:        slugI(i),
			DisplayName: "T",
			Status:      StatusActive,
		})
	}
	// Add one suspended + one deleted to exercise the status filter.
	seedTenant(t, db, models.Tenant{TenantID: "id-s", Slug: "slug-s", DisplayName: "S", Status: StatusSuspended})
	seedTenant(t, db, models.Tenant{TenantID: "id-d", Slug: "slug-d", DisplayName: "D", Status: StatusDeleted})

	// No filter: total = 7, return first 3.
	res, err := a.ListTenants(context.Background(), ListParams{Limit: 3, Offset: 0})
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	if res.Total != 7 {
		t.Errorf("Total: got %d want 7", res.Total)
	}
	if len(res.Tenants) != 3 {
		t.Errorf("page size: got %d want 3", len(res.Tenants))
	}
	if res.Limit != 3 || res.Offset != 0 {
		t.Errorf("Limit/Offset echo: got %d/%d", res.Limit, res.Offset)
	}

	// Status filter: only suspended.
	res, err = a.ListTenants(context.Background(), ListParams{Limit: 100, Status: StatusSuspended})
	if err != nil {
		t.Fatalf("ListTenants filtered: %v", err)
	}
	if res.Total != 1 {
		t.Errorf("suspended count: got %d want 1", res.Total)
	}
	if len(res.Tenants) != 1 || res.Tenants[0].Slug != "slug-s" {
		t.Errorf("suspended row wrong: %+v", res.Tenants)
	}
}

func TestListTenants_DefaultLimitAndCap(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	seedTenant(t, db, models.Tenant{Slug: "t1", DisplayName: "T"})
	seedTenant(t, db, models.Tenant{Slug: "t2", DisplayName: "T"})

	// Limit <= 0 → default 100.
	res, err := a.ListTenants(context.Background(), ListParams{})
	if err != nil {
		t.Fatalf("ListTenants default: %v", err)
	}
	if res.Limit != 100 {
		t.Errorf("default limit: got %d want 100", res.Limit)
	}

	// Limit > 500 → capped at 100 (graceful — doesn't crash).
	res, err = a.ListTenants(context.Background(), ListParams{Limit: 9999})
	if err != nil {
		t.Fatalf("ListTenants cap: %v", err)
	}
	if res.Limit != 100 {
		t.Errorf("cap limit: got %d want 100", res.Limit)
	}
}

// ---------------- GetTenant ----------------

func TestGetTenant_ByIdOrSlug(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	seedTenant(t, db, models.Tenant{TenantID: "t-acme", Slug: "acme", DisplayName: "Acme"})

	for _, q := range []string{"t-acme", "acme"} {
		tn, err := a.GetTenant(context.Background(), q)
		if err != nil {
			t.Errorf("GetTenant(%q): %v", q, err)
		}
		if tn.TenantID != "t-acme" {
			t.Errorf("GetTenant(%q) id: got %q", q, tn.TenantID)
		}
	}
}

func TestGetTenant_NotFound(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	_, err := a.GetTenant(context.Background(), "nope")
	if !errors.Is(err, ErrTenantNotFound) {
		t.Errorf("want ErrTenantNotFound, got %v", err)
	}
}

// ---------------- UpdateTenant ----------------

func TestUpdateTenant_PartialFields(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	seedTenant(t, db, models.Tenant{
		TenantID:    "t-acme",
		Slug:        "acme",
		DisplayName: "Old",
		Edition:     EditionTeam,
		EmailDomains: `["acme.com"]`,
		Features:    "{}",
	})

	newName := "Acme Renamed"
	newDomains := []string{"acme.io"}
	newFeatures := `{"ai":true}`
	tn, err := a.UpdateTenant(context.Background(), "t-acme", UpdateParams{
		DisplayName:  &newName,
		EmailDomains: &newDomains,
		Features:     &newFeatures,
	})
	if err != nil {
		t.Fatalf("UpdateTenant: %v", err)
	}
	if tn.DisplayName != "Acme Renamed" {
		t.Errorf("DisplayName: got %q", tn.DisplayName)
	}
	if tn.EmailDomains != `["acme.io"]` {
		t.Errorf("EmailDomains: got %q", tn.EmailDomains)
	}
	if tn.Features != `{"ai":true}` {
		t.Errorf("Features: got %q", tn.Features)
	}
	// Untouched fields stay.
	if tn.Edition != EditionTeam {
		t.Errorf("Edition (untouched): got %q", tn.Edition)
	}
	if tn.Slug != "acme" {
		t.Errorf("Slug (immutable): got %q", tn.Slug)
	}
}

func TestUpdateTenant_NoOpPatchReturnsCurrent(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	seedTenant(t, db, models.Tenant{TenantID: "t-acme", Slug: "acme", DisplayName: "Same"})

	tn, err := a.UpdateTenant(context.Background(), "t-acme", UpdateParams{})
	if err != nil {
		t.Fatalf("empty UpdateTenant: %v", err)
	}
	if tn.DisplayName != "Same" {
		t.Errorf("DisplayName: got %q want %q", tn.DisplayName, "Same")
	}
}

func TestUpdateTenant_EmptyDisplayNameRejected(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	seedTenant(t, db, models.Tenant{TenantID: "t-acme", Slug: "acme", DisplayName: "Old"})

	empty := "   "
	_, err := a.UpdateTenant(context.Background(), "t-acme", UpdateParams{DisplayName: &empty})
	if !errors.Is(err, ErrInvalidDisplayName) {
		t.Errorf("want ErrInvalidDisplayName, got %v", err)
	}
}

func TestUpdateTenant_EmailDomainOverlapRejected(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	seedTenant(t, db, models.Tenant{TenantID: "t-a", Slug: "alpha", DisplayName: "A", EmailDomains: `["alpha.com"]`})
	seedTenant(t, db, models.Tenant{TenantID: "t-b", Slug: "bravo", DisplayName: "B"})

	newDomains := []string{"alpha.com"}
	_, err := a.UpdateTenant(context.Background(), "bravo", UpdateParams{EmailDomains: &newDomains})
	if !errors.Is(err, ErrEmailDomainConflict) {
		t.Errorf("want ErrEmailDomainConflict, got %v", err)
	}
}

func TestUpdateTenant_NotFound(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	newName := "x"
	_, err := a.UpdateTenant(context.Background(), "nope", UpdateParams{DisplayName: &newName})
	if !errors.Is(err, ErrTenantNotFound) {
		t.Errorf("want ErrTenantNotFound, got %v", err)
	}
}

// ---------------- Suspend / Restore / Delete ----------------

func TestSuspendTenant_HappyPath(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	seedTenant(t, db, models.Tenant{TenantID: "t-acme", Slug: "acme", DisplayName: "Acme", Status: StatusActive})

	tn, err := a.SuspendTenant(context.Background(), "acme")
	if err != nil {
		t.Fatalf("SuspendTenant: %v", err)
	}
	if tn.Status != StatusSuspended {
		t.Errorf("status: got %q want %q", tn.Status, StatusSuspended)
	}
}

func TestSuspendTenant_AlreadySuspended(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	seedTenant(t, db, models.Tenant{Slug: "acme", DisplayName: "Acme", Status: StatusSuspended})

	_, err := a.SuspendTenant(context.Background(), "acme")
	if !errors.Is(err, ErrInvalidStateTransition) {
		t.Errorf("want ErrInvalidStateTransition, got %v", err)
	}
}

func TestSuspendTenant_DeletedRejected(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	seedTenant(t, db, models.Tenant{Slug: "acme", DisplayName: "Acme", Status: StatusDeleted})

	_, err := a.SuspendTenant(context.Background(), "acme")
	if !errors.Is(err, ErrInvalidStateTransition) {
		t.Errorf("want ErrInvalidStateTransition, got %v", err)
	}
}

func TestRestoreTenant_HappyPath(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	seedTenant(t, db, models.Tenant{Slug: "acme", DisplayName: "Acme", Status: StatusSuspended})

	tn, err := a.RestoreTenant(context.Background(), "acme")
	if err != nil {
		t.Fatalf("RestoreTenant: %v", err)
	}
	if tn.Status != StatusActive {
		t.Errorf("status: got %q want %q", tn.Status, StatusActive)
	}
}

func TestRestoreTenant_ActiveRejected(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	seedTenant(t, db, models.Tenant{Slug: "acme", DisplayName: "Acme", Status: StatusActive})

	_, err := a.RestoreTenant(context.Background(), "acme")
	if !errors.Is(err, ErrInvalidStateTransition) {
		t.Errorf("want ErrInvalidStateTransition, got %v", err)
	}
}

func TestRequestDeletion_FromActive(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	seedTenant(t, db, models.Tenant{Slug: "acme", DisplayName: "Acme", Status: StatusActive})

	before := time.Now().UTC()
	tn, err := a.RequestDeletion(context.Background(), "acme")
	if err != nil {
		t.Fatalf("RequestDeletion from active: %v", err)
	}
	if tn.Status != StatusDeleted {
		t.Errorf("status: got %q want %q", tn.Status, StatusDeleted)
	}
	if tn.DeletionRequestedAt == nil {
		t.Fatal("deletion_requested_at not set")
	}
	if tn.DeletionRequestedAt.Before(before) {
		t.Errorf("deletion_requested_at %v before request time %v", *tn.DeletionRequestedAt, before)
	}
}

func TestRequestDeletion_FromSuspended(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	seedTenant(t, db, models.Tenant{Slug: "acme", DisplayName: "Acme", Status: StatusSuspended})

	tn, err := a.RequestDeletion(context.Background(), "acme")
	if err != nil {
		t.Fatalf("RequestDeletion from suspended: %v", err)
	}
	if tn.Status != StatusDeleted {
		t.Errorf("status: got %q want %q", tn.Status, StatusDeleted)
	}
}

func TestRequestDeletion_AlreadyDeleted(t *testing.T) {
	db := newAdminDB(t)
	a := NewAdmin(db)
	seedTenant(t, db, models.Tenant{Slug: "acme", DisplayName: "Acme", Status: StatusDeleted})

	_, err := a.RequestDeletion(context.Background(), "acme")
	if !errors.Is(err, ErrInvalidStateTransition) {
		t.Errorf("want ErrInvalidStateTransition, got %v", err)
	}
}

// ---------------- Nil-receiver safety ----------------

func TestAdmin_NilReceiverReturnsErr(t *testing.T) {
	var a *Admin // nil
	ctx := context.Background()
	cases := []struct {
		name string
		fn   func() error
	}{
		{"create", func() error { _, err := a.CreateTenant(ctx, CreateParams{Slug: "x", DisplayName: "X"}); return err }},
		{"list", func() error { _, err := a.ListTenants(ctx, ListParams{}); return err }},
		{"get", func() error { _, err := a.GetTenant(ctx, "x"); return err }},
		{"update", func() error {
			s := "x"
			_, err := a.UpdateTenant(ctx, "x", UpdateParams{DisplayName: &s})
			return err
		}},
		{"suspend", func() error { _, err := a.SuspendTenant(ctx, "x"); return err }},
		{"restore", func() error { _, err := a.RestoreTenant(ctx, "x"); return err }},
		{"delete", func() error { _, err := a.RequestDeletion(ctx, "x"); return err }},
	}
	for _, c := range cases {
		if err := c.fn(); !errors.Is(err, errAdminNotConfigured) {
			t.Errorf("%s: want errAdminNotConfigured, got %v", c.name, err)
		}
	}
}

// ---------------- helpers ----------------

// strings_repeat is a local helper (no strings pkg import needed for a single
// repeat call) — keeps imports minimal.
func strings_repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func tidI(i int) string  { return "t-id-" + string(rune('0'+i)) }
func slugI(i int) string { return "slug-" + string(rune('0'+i)) }
