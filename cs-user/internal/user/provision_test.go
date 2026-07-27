//go:build cgo

package user

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"gorm.io/gorm"
)

// --- ProvisionByEnterprise ---

// TestProvisionByEnterprise_FirstCallCreatesUser verifies the brand-new path:
// a single call with a fresh enterprise_uid creates a user row + an
// employment_identities row carrying enterprise_uid as the reverse-lookup key.
func TestProvisionByEnterprise_FirstCallCreatesUser(t *testing.T) {
	svc := newTestService(t)
	ctx := tenant.WithTenant(context.Background(), &models.Tenant{TenantID: "acme"})

	u, isNew, err := svc.ProvisionByEnterprise(ctx, ProvisionByEnterpriseParams{
		EnterpriseProvider: "idtrust",
		EnterpriseUID:      "EXT-1",
		DisplayName:        "张三",
		Email:              "zhangsan@example.com",
	})
	if err != nil {
		t.Fatalf("ProvisionByEnterprise: %v", err)
	}
	if !isNew {
		t.Errorf("first call: isNew=false, want true")
	}
	if !strings.HasPrefix(u.SubjectID, "usr_") {
		t.Errorf("SubjectID: got %q, want usr_ prefix", u.SubjectID)
	}
	if u.TenantID != "acme" {
		t.Errorf("TenantID: got %q, want acme", u.TenantID)
	}
	// username fallback should fire ("ext_<uid>")
	if u.Username != "ext_EXT-1" {
		t.Errorf("Username: got %q, want ext_EXT-1", u.Username)
	}

	// employment_identities row must exist with the enterprise_uid we passed.
	var ei models.EmploymentIdentity
	if err := svc.db.Where("user_subject_id = ?", u.SubjectID).Take(&ei).Error; err != nil {
		t.Fatalf("query employment_identity: %v", err)
	}
	if ei.EnterpriseUID == nil || *ei.EnterpriseUID != "EXT-1" {
		t.Errorf("ei.EnterpriseUID: got %v, want EXT-1", ei.EnterpriseUID)
	}
	if ei.Provider != "idtrust" {
		t.Errorf("ei.Provider: got %q, want idtrust", ei.Provider)
	}
	if ei.TenantID != "acme" {
		t.Errorf("ei.TenantID: got %q, want acme", ei.TenantID)
	}
}

// TestProvisionByEnterprise_IdempotentRetry verifies a second call with the
// same (tenant, enterprise_uid) returns the existing user with isNew=false
// and does NOT create a second row.
func TestProvisionByEnterprise_IdempotentRetry(t *testing.T) {
	svc := newTestService(t)
	ctx := tenant.WithTenant(context.Background(), &models.Tenant{TenantID: "acme"})

	first, _, err := svc.ProvisionByEnterprise(ctx, ProvisionByEnterpriseParams{
		EnterpriseProvider: "idtrust",
		EnterpriseUID:      "EXT-2",
	})
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}

	second, isNew, err := svc.ProvisionByEnterprise(ctx, ProvisionByEnterpriseParams{
		EnterpriseProvider: "idtrust",
		EnterpriseUID:      "EXT-2",
	})
	if err != nil {
		t.Fatalf("second provision: %v", err)
	}
	if isNew {
		t.Errorf("second call: isNew=true, want false")
	}
	if first.SubjectID != second.SubjectID {
		t.Errorf("subject_id changed: first=%s second=%s", first.SubjectID, second.SubjectID)
	}

	// Exactly one employment_identities row.
	var count int64
	svc.db.Model(&models.EmploymentIdentity{}).Where("tenant_id = ? AND enterprise_uid = ?", "acme", "EXT-2").Count(&count)
	if count != 1 {
		t.Errorf("employment_identities rows: got %d, want 1", count)
	}
}

// TestProvisionByEnterprise_ProviderIsMetadataNotIsolationKey verifies the
// data model's contract: employment_identities.(tenant_id, enterprise_uid)
// is the unique key (per the partial unique index), so the same uid under
// different providers in the same tenant resolves to the SAME user. Provider
// is metadata on the row, not an isolation boundary.
//
// This matches the integration contract: cs-user does not validate provider
// names, and provider is not part of the identity primary key. EB that wants
// provider-scoped isolation must encode it in the enterprise_uid itself.
func TestProvisionByEnterprise_ProviderIsMetadataNotIsolationKey(t *testing.T) {
	svc := newTestService(t)
	ctx := tenant.WithTenant(context.Background(), &models.Tenant{TenantID: "acme"})

	a, _, err := svc.ProvisionByEnterprise(ctx, ProvisionByEnterpriseParams{
		EnterpriseProvider: "idtrust",
		EnterpriseUID:      "EXT-3",
	})
	if err != nil {
		t.Fatalf("provision idtrust: %v", err)
	}
	b, isNew, err := svc.ProvisionByEnterprise(ctx, ProvisionByEnterpriseParams{
		EnterpriseProvider: "wxwork",
		EnterpriseUID:      "EXT-3",
	})
	if err != nil {
		t.Fatalf("provision wxwork: %v", err)
	}
	if a.SubjectID != b.SubjectID {
		t.Errorf("subject_id drift across providers in same tenant: a=%s b=%s (data model says they should be the same user)", a.SubjectID, b.SubjectID)
	}
	if isNew {
		t.Errorf("second call isNew=true, want false (idempotent)")
	}
}

// TestProvisionByEnterprise_TenantIsolation verifies tenant isolation at the
// data layer: distinct (tenant, enterprise_uid) tuples produce distinct users.
//
// NOTE: cs-user's users.external_key is GLOBALLY unique (not per-tenant), so
// two tenants that send the same (provider, enterprise_uid) would collide on
// external_key. The realistic multi-tenant deployment has tenant-distinct
// IdPs (different providers per tenant) OR tenant-distinct enterprise_uid
// namespaces — both avoid the collision. This test uses tenant-distinct uids
// to exercise the isolation path without bumping into the external_key
// global uniqueness, which is a separate data-model concern tracked elsewhere.
//
// The race-recovery cross-tenant leak that compounded this scenario has been
// fixed (race-recovery WHERE is now grouped under tenant_id), so a real
// collision now surfaces as an error rather than silently returning the
// wrong tenant's user.
func TestProvisionByEnterprise_TenantIsolation(t *testing.T) {
	svc := newTestService(t)
	ctxA := tenant.WithTenant(context.Background(), &models.Tenant{TenantID: "tenant-a"})
	ctxB := tenant.WithTenant(context.Background(), &models.Tenant{TenantID: "tenant-b"})

	a, _, err := svc.ProvisionByEnterprise(ctxA, ProvisionByEnterpriseParams{
		EnterpriseProvider: "idtrust",
		EnterpriseUID:      "EXT-4-A",
	})
	if err != nil {
		t.Fatalf("provision tenant-a: %v", err)
	}
	b, _, err := svc.ProvisionByEnterprise(ctxB, ProvisionByEnterpriseParams{
		EnterpriseProvider: "idtrust",
		EnterpriseUID:      "EXT-4-B",
	})
	if err != nil {
		t.Fatalf("provision tenant-b: %v", err)
	}
	if a.SubjectID == b.SubjectID {
		t.Errorf("same subject_id across tenants: %s", a.SubjectID)
	}
	if a.TenantID != "tenant-a" || b.TenantID != "tenant-b" {
		t.Errorf("tenant_id mismatch: a=%s b=%s", a.TenantID, b.TenantID)
	}
}

// TestProvisionByEnterprise_ThenOAuthCallbackReattaches is the core closure
// test: provision a user via EB, then simulate a real OAuth callback carrying
// a DIFFERENT universal_id but the same enterprise_uid in ExternalClaims.
// GetOrCreateUser must reattach to the pre-provisioned subject_id (path 6).
func TestProvisionByEnterprise_ThenOAuthCallbackReattaches(t *testing.T) {
	svc := newTestService(t)
	ctx := tenant.WithTenant(context.Background(), &models.Tenant{TenantID: "acme"})

	pre, _, err := svc.ProvisionByEnterprise(ctx, ProvisionByEnterpriseParams{
		EnterpriseProvider: "idtrust",
		EnterpriseUID:      "EXT-5",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Simulate Casdoor's OAuth callback for the same human. universal_id is
	// whatever Casdoor minted (different from EB's EXT-5). ExternalClaims
	// carries enterprise_uid — the bridge between pre-provision and OAuth.
	oauthClaims := &models.JWTClaims{
		UniversalID: "casdoor-mint-xyz",
		Provider:    "idtrust",
		Name:        "zhangsan",
		ExternalClaims: map[string]any{
			"enterprise_uid": "EXT-5",
		},
	}
	got, isNew, err := svc.GetOrCreateUser(ctx, oauthClaims)
	if err != nil {
		t.Fatalf("GetOrCreateUser: %v", err)
	}
	if isNew {
		t.Errorf("isNew=true, want false (should reattach to pre-provisioned user)")
	}
	if got.SubjectID != pre.SubjectID {
		t.Errorf("subject_id drift: pre=%s oauth=%s", pre.SubjectID, got.SubjectID)
	}

	// No second user should have been created.
	var userCount int64
	svc.db.Model(&models.User{}).Where("tenant_id = ?", "acme").Count(&userCount)
	if userCount != 1 {
		t.Errorf("users in acme: got %d, want 1", userCount)
	}
}

// TestProvisionByEnterprise_MissingFieldsRejected verifies the 400 paths.
func TestProvisionByEnterprise_MissingFieldsRejected(t *testing.T) {
	svc := newTestService(t)
	ctx := tenant.WithTenant(context.Background(), &models.Tenant{TenantID: "acme"})

	if _, _, err := svc.ProvisionByEnterprise(ctx, ProvisionByEnterpriseParams{
		EnterpriseUID: "EXT-6",
	}); err == nil {
		t.Errorf("empty provider: want error, got nil")
	}
	if _, _, err := svc.ProvisionByEnterprise(ctx, ProvisionByEnterpriseParams{
		EnterpriseProvider: "idtrust",
	}); err == nil {
		t.Errorf("empty uid: want error, got nil")
	}
}

// TestProvisionByEnterprise_ExplicitUsernameHonored verifies the caller's
// username is used when supplied (no fallback to ext_<uid>).
func TestProvisionByEnterprise_ExplicitUsernameHonored(t *testing.T) {
	svc := newTestService(t)
	ctx := tenant.WithTenant(context.Background(), &models.Tenant{TenantID: "acme"})

	u, _, err := svc.ProvisionByEnterprise(ctx, ProvisionByEnterpriseParams{
		EnterpriseProvider: "idtrust",
		EnterpriseUID:      "EXT-7",
		Username:           "alice",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if u.Username != "alice" {
		t.Errorf("Username: got %q, want alice", u.Username)
	}
}

// TestProvisionByEnterprise_BypassesTenantProviderGate verifies the
// employment_identities row gets written even when the tenant explicitly
// does NOT list the provider in employment_providers.enabled (which would
// make ApplyEnterpriseMapping return ErrEnterpriseMappingDisabled). This
// proves the direct-write path bypasses the gate as designed.
func TestProvisionByEnterprise_BypassesTenantProviderGate(t *testing.T) {
	// Use newEmploymentMappingService so TenantConfig table exists and we
	// can seed an explicit "enabled: []" config.
	svc := newEmploymentMappingService(t)
	ctx := tenant.WithTenant(context.Background(), &models.Tenant{TenantID: "acme"})

	// Seed tenant_configs with an empty enabled list — provider "idtrust"
	// is NOT registered. ApplyEnterpriseMapping would refuse with
	// ErrEnterpriseMappingDisabled.
	seedTenantConfig(t, svc, "acme", "employment_providers:\n  enabled: []\n")

	// ProvisionByEnterprise must still write the row.
	if _, _, err := svc.ProvisionByEnterprise(ctx, ProvisionByEnterpriseParams{
		EnterpriseProvider: "idtrust",
		EnterpriseUID:      "EXT-8",
		EmployeeNumber:     "EMP-8",
	}); err != nil {
		t.Fatalf("provision: %v", err)
	}

	var ei models.EmploymentIdentity
	if err := svc.db.Where("tenant_id = ? AND enterprise_uid = ?", "acme", "EXT-8").Take(&ei).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("employment_identity row missing (gate not bypassed)")
		}
		t.Fatalf("query: %v", err)
	}
	if ei.EmployeeNumber == nil || *ei.EmployeeNumber != "EMP-8" {
		t.Errorf("EmployeeNumber: got %v, want EMP-8", ei.EmployeeNumber)
	}
}

// TestGetOrCreateUser_NoEnterpriseUIDSkipsPath6 is a regression guard: a
// real OAuth login whose ExternalClaims has no enterprise_uid must not
// trigger an extra query or accidental cross-match. Verifies the path-6
// branch is correctly gated on uid != "".
func TestGetOrCreateUser_NoEnterpriseUIDSkipsPath6(t *testing.T) {
	svc := newTestService(t)
	ctx := tenant.WithTenant(context.Background(), &models.Tenant{TenantID: "acme"})

	// Seed a user with an enterprise_uid via provision.
	if _, _, err := svc.ProvisionByEnterprise(ctx, ProvisionByEnterpriseParams{
		EnterpriseProvider: "idtrust",
		EnterpriseUID:      "EXT-9",
	}); err != nil {
		t.Fatalf("provision: %v", err)
	}

	// OAuth login with no enterprise_uid and a fresh universal_id — should
	// NOT match the pre-provisioned user.
	oauthClaims := &models.JWTClaims{
		UniversalID: "completely-different-id",
		Provider:    "idtrust",
		Name:        "newuser",
	}
	got, isNew, err := svc.GetOrCreateUser(ctx, oauthClaims)
	if err != nil {
		t.Fatalf("GetOrCreateUser: %v", err)
	}
	if !isNew {
		t.Errorf("isNew=false, want true (path 6 must not fire without enterprise_uid)")
	}
	if got.Username != "newuser" {
		t.Errorf("Username: got %q, want newuser", got.Username)
	}
}
