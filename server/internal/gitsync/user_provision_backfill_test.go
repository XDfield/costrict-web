// Tests for UserProvisionService.BackfillMissingBindings (admin-triggered
// 存量用户 reconciliation path).

package gitsync

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

func TestBackfill_AlreadySyncedShortCircuits(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{}
	svc, db := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	// Seed a synced binding for usr-1 in tenant t1.
	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-1", TenantID: "t1", ShortID: "u-alice01", Username: "alice",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := countRows(t, db)
	createCallsBefore := len(stub.createCalls)

	res := svc.BackfillMissingBindings(context.Background(), "t1", []BackfillUser{
		{SubjectID: "usr-1", ShortID: "u-alice01", Username: "alice"},
	}, BackfillOptions{})

	if res.Total != 1 || res.AlreadyBound != 1 || res.Provisioned != 0 {
		t.Errorf("result = %+v, want Total=1 AlreadyBound=1 Provisioned=0", res)
	}
	if len(stub.createCalls) != createCallsBefore {
		t.Errorf("expected 0 new CreateUser calls for already-synced, got %d", len(stub.createCalls)-createCallsBefore)
	}
	if got := countRows(t, db); got != before {
		t.Errorf("row count changed: before=%d after=%d (already-synced should not insert)", before, got)
	}
}

func TestBackfill_ProvisionsMissing(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{created: &GiteaUser{ID: 42}}
	svc, _ := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	res := svc.BackfillMissingBindings(context.Background(), "t1", []BackfillUser{
		{SubjectID: "usr-2", ShortID: "u-bob02", Username: "bob"},
	}, BackfillOptions{})

	if res.Total != 1 || res.Provisioned != 1 || res.Failed != 0 {
		t.Errorf("result = %+v, want Total=1 Provisioned=1 Failed=0", res)
	}
	if len(stub.createCalls) != 1 {
		t.Errorf("expected 1 CreateUser call, got %d", len(stub.createCalls))
	}
	if got := stub.createCalls[0].Login; got != "u-bob02" {
		t.Errorf("Login = %q, want u-bob02", got)
	}
}

func TestBackfill_SkipsEmptyShortID(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{}
	svc, _ := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	res := svc.BackfillMissingBindings(context.Background(), "t1", []BackfillUser{
		{SubjectID: "usr-3", ShortID: "", Username: "carol"}, // no ShortID
		{SubjectID: "", ShortID: "u-x", Username: "x"},       // no SubjectID
	}, BackfillOptions{})

	if res.Total != 2 || res.Skipped != 2 {
		t.Errorf("result = %+v, want Total=2 Skipped=2", res)
	}
	if len(stub.createCalls) != 0 {
		t.Errorf("expected 0 CreateUser calls for skipped entries, got %d", len(stub.createCalls))
	}
}

func TestBackfill_FailedProvisionRecordsFailure(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{createErr: errors.New("500 internal")}
	svc, _ := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	res := svc.BackfillMissingBindings(context.Background(), "t1", []BackfillUser{
		{SubjectID: "usr-4", ShortID: "u-dave04", Username: "dave"},
	}, BackfillOptions{})

	if res.Total != 1 || res.Failed != 1 || res.Provisioned != 0 {
		t.Errorf("result = %+v, want Total=1 Failed=1 Provisioned=0", res)
	}
	if len(res.Failures) != 1 || res.Failures[0].SubjectID != "usr-4" {
		t.Errorf("Failures = %+v, want one entry for usr-4", res.Failures)
	}
	if !strings.Contains(res.Failures[0].Error, "500 internal") {
		t.Errorf("Failure.Error = %q, want it to contain '500 internal'", res.Failures[0].Error)
	}
}

func TestBackfill_MixedBatch(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{created: &GiteaUser{ID: 7}}
	svc, _ := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	// Seed usr-synced as already-synced.
	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-synced", TenantID: "t1", ShortID: "u-synced", Username: "synced",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	createCallsBefore := len(stub.createCalls)

	res := svc.BackfillMissingBindings(context.Background(), "t1", []BackfillUser{
		{SubjectID: "usr-synced", ShortID: "u-synced", Username: "synced"}, // already bound
		{SubjectID: "usr-new", ShortID: "u-new01", Username: "new"},         // fresh provision
		{SubjectID: "usr-stale", ShortID: "", Username: "stale"},            // skipped (no ShortID)
	}, BackfillOptions{})

	if res.Total != 3 {
		t.Errorf("Total = %d, want 3", res.Total)
	}
	if res.AlreadyBound != 1 {
		t.Errorf("AlreadyBound = %d, want 1", res.AlreadyBound)
	}
	if res.Provisioned != 1 {
		t.Errorf("Provisioned = %d, want 1", res.Provisioned)
	}
	if res.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", res.Skipped)
	}
	if res.Failed != 0 {
		t.Errorf("Failed = %d, want 0", res.Failed)
	}
	// Only the fresh user should have triggered a NEW CreateUser call
	// beyond the seed.
	newCalls := stub.createCalls[createCallsBefore:]
	if len(newCalls) != 1 || newCalls[0].Login != "u-new01" {
		t.Errorf("new createCalls = %+v, want one call for u-new01", newCalls)
	}
}

func TestBackfill_DefaultsTenantID(t *testing.T) {
	// Empty tenantID should fall back to "default" (mirrors ProvisionUser).
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{created: &GiteaUser{ID: 1}}
	svc, db := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	res := svc.BackfillMissingBindings(context.Background(), "", []BackfillUser{
		{SubjectID: "usr-1", ShortID: "u-alice01", Username: "alice"},
	}, BackfillOptions{})
	if res.Provisioned != 1 {
		t.Errorf("Provisioned = %d, want 1", res.Provisioned)
	}

	var b models.UserGitBinding
	if err := db.First(&b, "user_subject_id = ?", "usr-1").Error; err != nil {
		t.Fatalf("query binding: %v", err)
	}
	if b.TenantID != "default" {
		t.Errorf("TenantID = %q, want 'default'", b.TenantID)
	}
}

func TestBackfill_TenantScopedLookup(t *testing.T) {
	// A synced binding for usr-1 in tenant OTHER should not short-circuit
	// a backfill for tenant t1, because the boundStatus lookup query is
	// scoped to the target tenant.
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{created: &GiteaUser{ID: 5}}
	svc, db := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	// Seed a synced binding in tenant "other" with one ShortID.
	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-1", TenantID: "other", ShortID: "u-alice-other", Username: "alice",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Backfill the same subject_id in tenant t1 with a different ShortID
	// (different git_username — the table has a unique index on it).
	res := svc.BackfillMissingBindings(context.Background(), "t1", []BackfillUser{
		{SubjectID: "usr-1", ShortID: "u-alice-t1", Username: "alice"},
	}, BackfillOptions{})
	if res.Provisioned != 1 || res.AlreadyBound != 0 {
		t.Errorf("result = %+v, want Provisioned=1 AlreadyBound=0 (tenant-scoped)", res)
	}

	// Both (usr-1, other) and (usr-1, t1) bindings should exist now.
	var count int64
	db.Model(&models.UserGitBinding{}).Where("user_subject_id = ?", "usr-1").Count(&count)
	if count != 2 {
		t.Errorf("binding rows for usr-1 = %d, want 2 (one per tenant)", count)
	}
}

// countRows is a helper that returns total user_git_binding rows.
func countRows(t *testing.T, db *gorm.DB) int {
	t.Helper()
	var n int64
	if err := db.Model(&models.UserGitBinding{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return int(n)
}

func TestBackfill_UpdateDisplayNamePushesToGitea(t *testing.T) {
	// With opts.UpdateDisplayName=true, already-synced users get a PATCH
	// /admin/users/{username} call pushing cs-user's display_name → Gitea
	// full_name. Without it, no EditUser call is made (idempotent short-circuit).
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{}
	svc, _ := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	// Seed synced binding for usr-1.
	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-1", TenantID: "t1", ShortID: "u-alice01", Username: "alice",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	createCallsBefore := len(stub.createCalls)

	newName := "Alice Wonderland"
	res := svc.BackfillMissingBindings(context.Background(), "t1", []BackfillUser{
		{SubjectID: "usr-1", ShortID: "u-alice01", Username: "alice", DisplayName: &newName},
	}, BackfillOptions{UpdateDisplayName: true})

	if res.Total != 1 || res.AlreadyBound != 1 || res.DisplayNameUpdated != 1 || res.Failed != 0 {
		t.Errorf("result = %+v, want AlreadyBound=1 DisplayNameUpdated=1 Failed=0", res)
	}
	// Provisioned (fresh) path should not have fired.
	if len(stub.createCalls) != createCallsBefore {
		t.Errorf("CreateUser calls grew by %d, want 0", len(stub.createCalls)-createCallsBefore)
	}
	// EditUser should have been called once with the new full_name.
	if len(stub.editCalls) != 1 {
		t.Fatalf("editCalls = %+v, want exactly 1", stub.editCalls)
	}
	if stub.editCalls[0].Username != "u-alice01" || stub.editCalls[0].FullName != newName {
		t.Errorf("editCall = %+v, want username=u-alice01 full_name=%q", stub.editCalls[0], newName)
	}
}

// TestBackfill_SyncedBindingEmailNeverTouched locks the contract that
// backfill never re-touches the email of an already-provisioned Gitea
// account. Even with UpdateDisplayName=true (which is the only path that
// issues EditUser on synced bindings), the PATCH body must omit email —
// accounts opened before the {short_id}@costrict.com template switch
// keep their original email verbatim. Without this guarantee, an operator
// running backfill -update-display-name would silently rewrite every
// legacy Gitea account's email.
func TestBackfill_SyncedBindingEmailNeverTouched(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{}
	svc, _ := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	// Seed a synced binding — simulates a legacy Gitea account opened with
	// the pre-template email (whatever cs-user happened to return).
	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-legacy", TenantID: "t1", ShortID: "u-leg01", Username: "legacy",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	createCallsBefore := len(stub.createCalls)

	newName := "Legacy User"
	res := svc.BackfillMissingBindings(context.Background(), "t1", []BackfillUser{
		{SubjectID: "usr-legacy", ShortID: "u-leg01", Username: "legacy", DisplayName: &newName},
	}, BackfillOptions{UpdateDisplayName: true})

	if res.AlreadyBound != 1 || res.DisplayNameUpdated != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v, want AlreadyBound=1 DisplayNameUpdated=1 Failed=0", res)
	}
	if len(stub.createCalls) != createCallsBefore {
		t.Errorf("CreateUser grew by %d, want 0 (synced binding must not be re-provisioned)", len(stub.createCalls)-createCallsBefore)
	}
	if len(stub.editCalls) != 1 {
		t.Fatalf("editCalls = %+v, want exactly 1", stub.editCalls)
	}
	if stub.editCalls[0].EmailIsSet {
		t.Errorf("EditUser was called with Email set — backfill must NEVER rewrite email on already-provisioned accounts; got %+v", stub.editCalls[0])
	}
}

// TestBackfill_EditUserAlwaysSendsLoginName locks the workaround for the
// costrict Gitea fork's PATCH /admin/users/{username} requiring login_name.
// Without it Gitea returns HTTP 422 `[LoginName]: Required` and every
// already-bound user fails display_name sync (root cause of the
// 2026-07-31 backfill run that 100%-failed on 2/2 synced users). For
// locally-provisioned accounts (source_id=0) the canonical value is the
// username itself, mirroring Gitea's web-UI convention.
func TestBackfill_EditUserAlwaysSendsLoginName(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{}
	svc, _ := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-1", TenantID: "t1", ShortID: "u-alice01", Username: "alice",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	newName := "Alice Updated"
	res := svc.BackfillMissingBindings(context.Background(), "t1", []BackfillUser{
		{SubjectID: "usr-1", ShortID: "u-alice01", Username: "alice", DisplayName: &newName},
	}, BackfillOptions{UpdateDisplayName: true})

	if res.DisplayNameUpdated != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v, want DisplayNameUpdated=1 Failed=0", res)
	}
	if len(stub.editCalls) != 1 {
		t.Fatalf("editCalls = %+v, want exactly 1", stub.editCalls)
	}
	got := stub.editCalls[0]
	if !got.LoginNameIsSet {
		t.Errorf("EditUser was called without LoginName — Gitea fork returns 422 [LoginName]: Required; got %+v", got)
	}
	if got.LoginName != "u-alice01" {
		t.Errorf("EditUser LoginName = %q, want %q (gitUsername, the local-account convention)", got.LoginName, "u-alice01")
	}
}

func TestBackfill_UpdateDisplayNameOffByDefault(t *testing.T) {
	// opts.UpdateDisplayName=false: synced users short-circuit with no
	// EditUser call. This is the default-safe behaviour.
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{}
	svc, _ := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-1", TenantID: "t1", ShortID: "u-alice01", Username: "alice",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	name := "Alice"
	res := svc.BackfillMissingBindings(context.Background(), "t1", []BackfillUser{
		{SubjectID: "usr-1", ShortID: "u-alice01", Username: "alice", DisplayName: &name},
	}, BackfillOptions{}) // UpdateDisplayName defaults to false

	if res.DisplayNameUpdated != 0 {
		t.Errorf("DisplayNameUpdated = %d, want 0 (off by default)", res.DisplayNameUpdated)
	}
	if len(stub.editCalls) != 0 {
		t.Errorf("editCalls = %+v, want 0 when UpdateDisplayName=false", stub.editCalls)
	}
}

func TestBackfill_UpdateDisplayNameNilOrEmptyIsNoOp(t *testing.T) {
	// nil / blank display_name should not fire EditUser — we don't force-clear
	// existing Gitea full_name during reconciliation.
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{}
	svc, _ := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub})

	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-1", TenantID: "t1", ShortID: "u-alice01", Username: "alice",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	emptyName := ""
	res := svc.BackfillMissingBindings(context.Background(), "t1", []BackfillUser{
		{SubjectID: "usr-1", ShortID: "u-alice01", Username: "alice", DisplayName: nil},
		{SubjectID: "usr-x", ShortID: "u-empty", Username: "empty", DisplayName: &emptyName},
		{SubjectID: "usr-2", ShortID: "u-bob02", Username: "bob"}, // first provision (no EditUser)
	}, BackfillOptions{UpdateDisplayName: true})

	if res.DisplayNameUpdated != 0 {
		t.Errorf("DisplayNameUpdated = %d, want 0 (nil/empty display_name is no-op)", res.DisplayNameUpdated)
	}
	if len(stub.editCalls) != 0 {
		t.Errorf("editCalls = %+v, want 0", stub.editCalls)
	}
}

func TestBackfill_UpdateDisplayNameEditFailureRecordsFailure(t *testing.T) {
	// If EditUser errors (e.g. 500 from Gitea), record it as a per-user
	// failure but keep processing the rest of the batch.
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{editErr: errors.New("gitea 500")}
	svc, _ := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub})

	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-1", TenantID: "t1", ShortID: "u-alice01", Username: "alice",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	name := "Alice"
	res := svc.BackfillMissingBindings(context.Background(), "t1", []BackfillUser{
		{SubjectID: "usr-1", ShortID: "u-alice01", Username: "alice", DisplayName: &name},
	}, BackfillOptions{UpdateDisplayName: true})

	if res.DisplayNameUpdated != 0 || res.Failed != 1 {
		t.Errorf("result = %+v, want DisplayNameUpdated=0 Failed=1", res)
	}
	if len(res.Failures) != 1 || res.Failures[0].SubjectID != "usr-1" {
		t.Errorf("Failures = %+v, want one entry for usr-1", res.Failures)
	}
	if !strings.Contains(res.Failures[0].Error, "display_name sync") {
		t.Errorf("Failure.Error = %q, want prefix 'display_name sync'", res.Failures[0].Error)
	}
}
