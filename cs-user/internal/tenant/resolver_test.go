//go:build cgo

package tenant

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newResolverDB mirrors the cs-user models-package test fixture pattern:
// sqlite in-memory + AutoMigrate, with a couple of tenants pre-seeded so
// tests can resolve against known data. cgo-gated because sqlite needs CGO.
func newResolverDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.Tenant{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	// Seed: default + acme (with two email domains) + globex (overlaps on
	// globex.com with a fourth tenant, globex-cn, to exercise the ambiguous
	// path). inactive-co is suspended and must never resolve.
	seed := []models.Tenant{
		{TenantID: "default", Slug: "default", DisplayName: "Default Tenant", Status: "active"},
		{TenantID: "t-acme", Slug: "acme", DisplayName: "Acme Inc.", Status: "active", EmailDomains: `["acme.com","acme.cn"]`},
		{TenantID: "t-globex", Slug: "globex", DisplayName: "Globex Corp", Status: "active", EmailDomains: `["globex.com"]`},
		{TenantID: "t-globex-cn", Slug: "globex-cn", DisplayName: "Globex CN Sub", Status: "active", EmailDomains: `["globex.com"]`},
		{TenantID: "t-inactive", Slug: "inactive-co", DisplayName: "Inactive Co", Status: "suspended", EmailDomains: `["inactive.com"]`},
	}
	for i := range seed {
		if err := db.Create(&seed[i]).Error; err != nil {
			t.Fatalf("seed %s: %v", seed[i].TenantID, err)
		}
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// ------------------------------------------------------------------
// ResolveBySlug
// ------------------------------------------------------------------

func TestResolver_ResolveBySlug_Hit(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	got, err := r.ResolveBySlug(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ResolveBySlug: %v", err)
	}
	if got.TenantID != "t-acme" {
		t.Errorf("TenantID: got %q, want t-acme", got.TenantID)
	}
	if got.Slug != "acme" {
		t.Errorf("Slug: got %q, want acme", got.Slug)
	}
}

func TestResolver_ResolveBySlug_AcceptsTenantIDToo(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	// Some callers (B3b cookie / X-Tenant-Id header) carry the tenant_id
	// rather than the slug — accept both for symmetry.
	got, err := r.ResolveBySlug(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("ResolveBySlug by tenant_id: %v", err)
	}
	if got.TenantID != "t-acme" {
		t.Errorf("TenantID: got %q, want t-acme", got.TenantID)
	}
}

func TestResolver_ResolveBySlug_Miss(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	_, err := r.ResolveBySlug(context.Background(), "nonexistent")
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("expected ErrTenantNotFound, got %v", err)
	}
}

func TestResolver_ResolveBySlug_SuspendedExcluded(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	_, err := r.ResolveBySlug(context.Background(), "inactive-co")
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("suspended tenant must not resolve; got err=%v", err)
	}
}

func TestResolver_ResolveBySlug_EmptyInput(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	for _, in := range []string{"", "   ", "\t"} {
		_, err := r.ResolveBySlug(context.Background(), in)
		if !errors.Is(err, ErrTenantNotFound) {
			t.Errorf("input %q: expected ErrTenantNotFound, got %v", in, err)
		}
	}
}

// ------------------------------------------------------------------
// ResolveByEmailDomain
// ------------------------------------------------------------------

func TestResolver_ResolveByEmailDomain_UniqueHit(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	got, err := r.ResolveByEmailDomain(context.Background(), "acme.com")
	if err != nil {
		t.Fatalf("ResolveByEmailDomain: %v", err)
	}
	if got.TenantID != "t-acme" {
		t.Errorf("TenantID: got %q, want t-acme", got.TenantID)
	}
}

func TestResolver_ResolveByEmailDomain_CaseInsensitive(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	got, err := r.ResolveByEmailDomain(context.Background(), "ACME.COM")
	if err != nil {
		t.Fatalf("ResolveByEmailDomain uppercase: %v", err)
	}
	if got.TenantID != "t-acme" {
		t.Errorf("TenantID: got %q, want t-acme", got.TenantID)
	}
}

func TestResolver_ResolveByEmailDomain_Ambiguous(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	// globex.com is on BOTH t-globex and t-globex-cn → ErrAmbiguousTenant.
	_, err := r.ResolveByEmailDomain(context.Background(), "globex.com")
	if !errors.Is(err, ErrAmbiguousTenant) {
		t.Fatalf("expected ErrAmbiguousTenant, got %v", err)
	}
}

func TestResolver_ResolveByEmailDomain_Miss(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	_, err := r.ResolveByEmailDomain(context.Background(), "nowhere.example")
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("expected ErrTenantNotFound, got %v", err)
	}
}

func TestResolver_ResolveByEmailDomain_SuspendedExcluded(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	// inactive.com is on t-inactive (status='suspended'); the row exists
	// but the active filter excludes it, so the lookup misses.
	_, err := r.ResolveByEmailDomain(context.Background(), "inactive.com")
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("suspended tenant's email domain must not resolve; got err=%v", err)
	}
}

func TestResolver_ResolveByEmailDomain_MultiDomainPerTenant(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	// acme has BOTH acme.com and acme.cn; both must resolve to t-acme.
	for _, d := range []string{"acme.com", "acme.cn"} {
		got, err := r.ResolveByEmailDomain(context.Background(), d)
		if err != nil {
			t.Errorf("domain %q: %v", d, err)
			continue
		}
		if got.TenantID != "t-acme" {
			t.Errorf("domain %q: TenantID got %q, want t-acme", d, got.TenantID)
		}
	}
}

// ------------------------------------------------------------------
// ResolveByEmail
// ------------------------------------------------------------------

func TestResolver_ResolveByEmail_HappyPath(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	got, err := r.ResolveByEmail(context.Background(), "alice@acme.com")
	if err != nil {
		t.Fatalf("ResolveByEmail: %v", err)
	}
	if got.TenantID != "t-acme" {
		t.Errorf("TenantID: got %q, want t-acme", got.TenantID)
	}
}

func TestResolver_ResolveByEmail_Malformed(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	for _, in := range []string{"", "no-at-sign", "@acme.com", "alice@", "  "} {
		_, err := r.ResolveByEmail(context.Background(), in)
		if !errors.Is(err, ErrTenantNotFound) {
			t.Errorf("input %q: expected ErrTenantNotFound, got %v", in, err)
		}
	}
}

// ------------------------------------------------------------------
// ResolveFromHost
// ------------------------------------------------------------------

func TestResolver_ResolveFromHost_SubdomainHit(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	apex := []string{"cs-user.example.com"}
	got, err := r.ResolveFromHost(context.Background(), "acme.cs-user.example.com", apex)
	if err != nil {
		t.Fatalf("ResolveFromHost: %v", err)
	}
	if got.TenantID != "t-acme" {
		t.Errorf("TenantID: got %q, want t-acme", got.TenantID)
	}
}

func TestResolver_ResolveFromHost_WithPort(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	apex := []string{"cs-user.example.com"}
	got, err := r.ResolveFromHost(context.Background(), "acme.cs-user.example.com:8443", apex)
	if err != nil {
		t.Fatalf("ResolveFromHost with port: %v", err)
	}
	if got.TenantID != "t-acme" {
		t.Errorf("TenantID: got %q, want t-acme", got.TenantID)
	}
}

func TestResolver_ResolveFromHost_BareApex(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	apex := []string{"cs-user.example.com"}
	_, err := r.ResolveFromHost(context.Background(), "cs-user.example.com", apex)
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("bare apex must return ErrTenantNotFound, got %v", err)
	}
}

func TestResolver_ResolveFromHost_NoApexMatch(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	apex := []string{"cs-user.example.com"}
	// Unknown apex entirely → no signal.
	_, err := r.ResolveFromHost(context.Background(), "acme.other-site.example", apex)
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("unknown apex must return ErrTenantNotFound, got %v", err)
	}
}

func TestResolver_ResolveFromHost_NestedSubdomain(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	apex := []string{"cs-user.example.com"}
	// login.acme.cs-user.example.com → slug is the last segment of head
	// before the apex → "acme". B1 slug convention is single-label.
	got, err := r.ResolveFromHost(context.Background(), "login.acme.cs-user.example.com", apex)
	if err != nil {
		t.Fatalf("ResolveFromHost nested: %v", err)
	}
	if got.TenantID != "t-acme" {
		t.Errorf("TenantID: got %q, want t-acme", got.TenantID)
	}
}

func TestResolver_ResolveFromHost_FQDNTrailingDot(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	apex := []string{"cs-user.example.com"}
	got, err := r.ResolveFromHost(context.Background(), "acme.cs-user.example.com.", apex)
	if err != nil {
		t.Fatalf("ResolveFromHost FQDN: %v", err)
	}
	if got.TenantID != "t-acme" {
		t.Errorf("TenantID: got %q, want t-acme", got.TenantID)
	}
}

func TestResolver_ResolveFromHost_LocalhostApex(t *testing.T) {
	t.Parallel()
	r := NewResolver(newResolverDB(t))
	// Dev-mode setup: apex is "localhost:8080", host is "acme.localhost:8080".
	// Confirms apex with port strips correctly.
	apex := []string{"localhost:8080"}
	slug := slugFromHost("acme.localhost:8080", apex)
	if slug != "acme" {
		t.Fatalf("slugFromHost: got %q, want acme", slug)
	}
	got, err := r.ResolveFromHost(context.Background(), "acme.localhost:8080", apex)
	if err != nil {
		t.Fatalf("ResolveFromHost localhost apex: %v", err)
	}
	if got.TenantID != "t-acme" {
		t.Errorf("TenantID: got %q, want t-acme", got.TenantID)
	}
}

// ------------------------------------------------------------------
// ParseEmailDomains
// ------------------------------------------------------------------

func TestParseEmailDomains_HappyPath(t *testing.T) {
	t.Parallel()
	tn := &models.Tenant{EmailDomains: `["acme.com","acme.cn"]`}
	got := ParseEmailDomains(tn)
	if len(got) != 2 {
		t.Fatalf("len: got %d, want 2", len(got))
	}
	if got[0] != "acme.com" || got[1] != "acme.cn" {
		t.Errorf("got %v, want [acme.com acme.cn]", got)
	}
}

func TestParseEmailDomains_EmptyArray(t *testing.T) {
	t.Parallel()
	tn := &models.Tenant{EmailDomains: `[]`}
	if got := ParseEmailDomains(tn); len(got) != 0 {
		t.Errorf("empty array: got %v, want []", got)
	}
}

func TestParseEmailDomains_EmptyString(t *testing.T) {
	t.Parallel()
	tn := &models.Tenant{EmailDomains: ""}
	if got := ParseEmailDomains(tn); len(got) != 0 {
		t.Errorf("empty string: got %v, want []", got)
	}
}

func TestParseEmailDomains_Malformed(t *testing.T) {
	t.Parallel()
	// Malformed JSON must degrade to empty slice — same convention as
	// EmploymentIdentity.Attributes: the column is opaque at the DB layer,
	// app layer tolerates any string.
	tn := &models.Tenant{EmailDomains: "not-json"}
	if got := ParseEmailDomains(tn); len(got) != 0 {
		t.Errorf("malformed: got %v, want []", got)
	}
}

func TestParseEmailDomains_LowercasedOutput(t *testing.T) {
	t.Parallel()
	tn := &models.Tenant{EmailDomains: `["ACME.COM"," Acme.CN "]`}
	got := ParseEmailDomains(tn)
	for _, d := range got {
		if d != strings.ToLower(strings.TrimSpace(d)) {
			t.Errorf("entry not normalized: %q", d)
		}
	}
}

func TestParseEmailDomains_NilTenant(t *testing.T) {
	t.Parallel()
	if got := ParseEmailDomains(nil); got != nil {
		t.Errorf("nil tenant: got %v, want nil", got)
	}
}

// ------------------------------------------------------------------
// domainFromEmail (free helper, covered via ResolveByEmail above + edge
// cases here for clarity)
// ------------------------------------------------------------------

func TestDomainFromEmail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"alice@acme.com", "acme.com"},
		{"Alice@ACME.COM", "acme.com"},
		{"  alice@acme.com  ", "acme.com"},
		{"bob@sub.acme.cn", "sub.acme.cn"},
		{"plainword", ""},
		{"", ""},
		{"@acme.com", ""},
		{"alice@", ""},
	}
	for _, c := range cases {
		if got := domainFromEmail(c.in); got != c.want {
			t.Errorf("domainFromEmail(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

// ------------------------------------------------------------------
// Nil-receiver guards
// ------------------------------------------------------------------

func TestResolver_NilGuards(t *testing.T) {
	t.Parallel()
	var r *Resolver
	ctx := context.Background()
	for _, fn := range []struct {
		name string
		call func() error
	}{
		{"ResolveBySlug", func() error { _, e := r.ResolveBySlug(ctx, "acme"); return e }},
		{"ResolveByEmailDomain", func() error { _, e := r.ResolveByEmailDomain(ctx, "acme.com"); return e }},
		{"ResolveByEmail", func() error { _, e := r.ResolveByEmail(ctx, "alice@acme.com"); return e }},
		{"ResolveFromHost", func() error {
			_, e := r.ResolveFromHost(ctx, "acme.cs-user.example.com", []string{"cs-user.example.com"})
			return e
		}},
	} {
		if err := fn.call(); !errors.Is(err, ErrTenantNotFound) {
			t.Errorf("%s on nil receiver: expected ErrTenantNotFound, got %v", fn.name, err)
		}
	}
}
