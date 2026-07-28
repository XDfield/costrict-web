//go:build cgo

package user

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newTestService opens an in-memory sqlite DB and AutoMigrates the cs-user
// schema. We use sqlite (not Postgres) only at the test boundary — gorm
// renders the same SQL shape for both drivers for the read paths we test.
// CGo is required by the sqlite driver, hence the build tag.
func newTestService(t *testing.T) *Service {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.UserAuthIdentity{}, &models.EmploymentIdentity{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return NewService(db)
}

// seedUser inserts a user with sensible defaults; only override the fields
// the caller cares about via the opts closure. SubjectID defaults to a unique
// per-call value so multiple seedUser invocations in the same test don't
// collide on the unique index.
//
// Note on IsActive=false: gorm omits zero-value bools on Create, so a caller
// that sets IsActive=false via opts wouldn't get the value persisted. We work
// around this by re-applying bool fields via Updates after Create when they
// diverge from the column default. Other zero-value-prone fields (Status)
// carry non-zero defaults in the test fixture so they're fine.
var seedCounter int

func seedUser(t *testing.T, svc *Service, opts func(*models.User)) *models.User {
	t.Helper()
	seedCounter++
	u := &models.User{
		SubjectID: fmt.Sprintf("subj-%d", seedCounter),
		Username:  fmt.Sprintf("user-%d", seedCounter),
		IsActive:  true,
		Status:    "active",
	}
	if opts != nil {
		opts(u)
	}
	// Capture before Create — gorm reads back column defaults and would
	// otherwise mask a caller-supplied IsActive=false.
	desiredActive := u.IsActive
	if err := svc.db.Create(u).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if !desiredActive {
		if err := svc.db.Model(u).Update("is_active", false).Error; err != nil {
			t.Fatalf("seed user (is_active update): %v", err)
		}
		u.IsActive = false
	}
	return u
}

func seedIdentity(t *testing.T, svc *Service, opts func(*models.UserAuthIdentity)) *models.UserAuthIdentity {
	t.Helper()
	i := &models.UserAuthIdentity{
		UserSubjectID: "subj-1",
		Provider:      "casdoor",
		ExternalKey:   "casdoor:alice",
	}
	if opts != nil {
		opts(i)
	}
	if err := svc.db.Create(i).Error; err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	return i
}

func TestGetUserByID_Found(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID, u.Username = "subj-found", "bob"
	})

	got, err := svc.GetUserByID(context.Background(), "subj-found")
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.Username != "bob" {
		t.Errorf("username: got %q want bob", got.Username)
	}
}

func TestGetUserByID_NotFoundReturnsErrRecordNotFound(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.GetUserByID(context.Background(), "does-not-exist")
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("got %v, want gorm.ErrRecordNotFound", err)
	}
}

func TestGetUserByID_EmptySubjectErrors(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.GetUserByID(context.Background(), ""); !errors.Is(err, ErrEmptySubjectID) {
		t.Errorf("got %v, want ErrEmptySubjectID", err)
	}
}

func TestGetUserByID_DeletedUserNotVisible(t *testing.T) {
	svc := newTestService(t)
	u := seedUser(t, svc, func(u *models.User) { u.SubjectID = "soft-deleted" })
	if err := svc.db.Delete(u).Error; err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	if _, err := svc.GetUserByID(context.Background(), "soft-deleted"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("got %v, want ErrRecordNotFound for soft-deleted row", err)
	}
}

func TestGetUsersByIDs_ReturnsMap(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) { u.SubjectID, u.Username = "a", "alice" })
	seedUser(t, svc, func(u *models.User) { u.SubjectID, u.Username = "b", "bob" })

	got, err := svc.GetUsersByIDs(context.Background(), []string{"a", "b", "missing"})
	if err != nil {
		t.Fatalf("GetUsersByIDs: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d users, want 2 (missing silently omitted)", len(got))
	}
	if got["a"].Username != "alice" {
		t.Errorf("a.username = %q", got["a"].Username)
	}
	if _, ok := got["missing"]; ok {
		t.Error("missing ID should not appear in result")
	}
}

func TestGetUsersByIDs_EmptyInputSkipsDB(t *testing.T) {
	svc := newTestService(t)
	got, err := svc.GetUsersByIDs(context.Background(), nil)
	if err != nil {
		t.Fatalf("nil input: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("got %v, want non-nil empty map", got)
	}
}

func TestSearchUsers_KeywordMatches(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) { u.Username = "alice"; u.Email = strPtr("alice@example.com") })
	seedUser(t, svc, func(u *models.User) { u.Username = "bob"; u.Email = strPtr("bob@elsewhere.com") })
	seedUser(t, svc, func(u *models.User) { u.Username = "malice"; u.Email = strPtr("mal@x.com") })

	got, err := svc.SearchUsers(context.Background(), "ali", 10)
	if err != nil {
		t.Fatalf("SearchUsers: %v", err)
	}
	// "alice" matches username; "malice" matches username suffix.
	if len(got) != 2 {
		t.Errorf("got %d results, want 2 (username contains 'ali')", len(got))
	}
}

func TestSearchUsers_InactiveExcluded(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) { u.Username = "alice-active"; u.IsActive = true })
	seedUser(t, svc, func(u *models.User) { u.Username = "alice-inactive"; u.IsActive = false })

	got, err := svc.SearchUsers(context.Background(), "alice", 10)
	if err != nil {
		t.Fatalf("SearchUsers: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d results, want 1 (inactive must be excluded)", len(got))
	}
	if got[0].Username != "alice-active" {
		t.Errorf("got %q, want alice-active", got[0].Username)
	}
}

func TestSearchUsers_DefaultLimitApplies(t *testing.T) {
	svc := newTestService(t)
	// Seed more than defaultSearchLimit users; expect the cap to clip the
	// result. We don't assert exact count — that would couple to the constant.
	for i := 0; i < defaultSearchLimit+5; i++ {
		seedUser(t, svc, func(u *models.User) {
			u.SubjectID = "subj-" + string(rune('a'+i))
			u.Username = "user-" + string(rune('a'+i))
		})
	}

	got, err := svc.SearchUsers(context.Background(), "", 0) // limit=0 → default
	if err != nil {
		t.Fatalf("SearchUsers: %v", err)
	}
	if len(got) != defaultSearchLimit {
		t.Errorf("got %d results, want %d (default limit)", len(got), defaultSearchLimit)
	}
}

func TestListIdentities_OrdersPrimaryFirst(t *testing.T) {
	svc := newTestService(t)
	// Seed in reverse-primary order so we can verify ordering.
	seedIdentity(t, svc, func(i *models.UserAuthIdentity) {
		i.UserSubjectID, i.Provider, i.ExternalKey = "subj-1", "github", "github:1"
	})
	seedIdentity(t, svc, func(i *models.UserAuthIdentity) {
		i.UserSubjectID, i.Provider, i.ExternalKey, i.IsPrimary = "subj-1", "casdoor", "casdoor:1", true
	})

	got, err := svc.ListIdentities(context.Background(), "subj-1")
	if err != nil {
		t.Fatalf("ListIdentities: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 identities", len(got))
	}
	if !got[0].IsPrimary {
		t.Errorf("first identity should be primary; got provider=%q isPrimary=%v", got[0].Provider, got[0].IsPrimary)
	}
}

func TestListIdentities_EmptySubjectErrors(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.ListIdentities(context.Background(), ""); !errors.Is(err, ErrEmptySubjectID) {
		t.Errorf("got %v, want ErrEmptySubjectID", err)
	}
}

func TestListIdentities_NoRowsReturnsEmpty(t *testing.T) {
	svc := newTestService(t)
	got, err := svc.ListIdentities(context.Background(), "no-such-user")
	if err != nil {
		t.Fatalf("ListIdentities: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d identities, want 0", len(got))
	}
}

// seedEmployment inserts an employment_identities row tied to userSubjectID.
// The closure overrides defaults; tenant_id defaults to "default" via column
// default so the B5 tenant.Scope lookup (default tenant fallback) matches.
func seedEmployment(t *testing.T, svc *Service, opts func(*models.EmploymentIdentity)) *models.EmploymentIdentity {
	t.Helper()
	e := &models.EmploymentIdentity{
		TenantID:      tenant.DefaultTenantID,
		UserSubjectID: "subj-1",
		Provider:      "idtrust",
		LastSyncedAt:  time.Now().UTC(),
	}
	if opts != nil {
		opts(e)
	}
	if err := svc.db.Create(e).Error; err != nil {
		t.Fatalf("seed employment: %v", err)
	}
	return e
}

func TestSearchUsersByEmployeeNumber_Hit(t *testing.T) {
	svc := newTestService(t)
	u := seedUser(t, svc, func(u *models.User) { u.SubjectID, u.Username = "emp-1", "alice" })
	seedEmployment(t, svc, func(e *models.EmploymentIdentity) {
		e.UserSubjectID = u.SubjectID
		empNo := "1001"
		e.EmployeeNumber = &empNo
	})

	got, err := svc.SearchUsersByEmployeeNumber(context.Background(), "1001", 1)
	if err != nil {
		t.Fatalf("SearchUsersByEmployeeNumber: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d users, want 1", len(got))
	}
	if got[0].SubjectID != "emp-1" {
		t.Errorf("subject_id: got %q want emp-1", got[0].SubjectID)
	}
}

func TestSearchUsersByEmployeeNumber_NoMatchReturnsNil(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) { u.SubjectID = "u-no-emp" })

	got, err := svc.SearchUsersByEmployeeNumber(context.Background(), "9999", 1)
	if err != nil {
		t.Fatalf("SearchUsersByEmployeeNumber: %v", err)
	}
	if got != nil && len(got) != 0 {
		t.Errorf("got %v, want empty slice for no match", got)
	}
}

func TestSearchUsersByEmployeeNumber_MultipleRowsPicksLatestSync(t *testing.T) {
	// Same employee_number on two users (pre-Phase-B non-uniqueness allowed).
	// Service should pick the most recently synced one.
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) { u.SubjectID = "older" })
	seedUser(t, svc, func(u *models.User) { u.SubjectID = "newer" })

	empNo := "2002"
	older := time.Now().Add(-2 * time.Hour).UTC()
	newer := time.Now().UTC()

	seedEmployment(t, svc, func(e *models.EmploymentIdentity) {
		e.UserSubjectID = "older"
		e.EmployeeNumber = &empNo
		e.LastSyncedAt = older
	})
	seedEmployment(t, svc, func(e *models.EmploymentIdentity) {
		e.UserSubjectID = "newer"
		e.EmployeeNumber = &empNo
		e.LastSyncedAt = newer
	})

	got, err := svc.SearchUsersByEmployeeNumber(context.Background(), empNo, 1)
	if err != nil {
		t.Fatalf("SearchUsersByEmployeeNumber: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d users, want 1 (latest sync wins)", len(got))
	}
	if got[0].SubjectID != "newer" {
		t.Errorf("got %q, want newer (latest last_synced_at)", got[0].SubjectID)
	}
}

func TestSearchUsersByEmployeeNumber_InactiveUserExcluded(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID, u.IsActive = "inactive-emp", false
	})
	empNo := "3003"
	seedEmployment(t, svc, func(e *models.EmploymentIdentity) {
		e.UserSubjectID = "inactive-emp"
		e.EmployeeNumber = &empNo
	})

	got, err := svc.SearchUsersByEmployeeNumber(context.Background(), empNo, 1)
	if err != nil {
		t.Fatalf("SearchUsersByEmployeeNumber: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d users, want 0 (inactive user excluded)", len(got))
	}
}

func TestSearchUsersByEmployeeNumber_TenantScoped(t *testing.T) {
	// Two tenants, same employee_number, different physical users. The ctx
	// tenant signal decides which one is visible.
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "tenant-a-user"
		u.TenantID = "tenant-a"
	})
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "tenant-b-user"
		u.TenantID = "tenant-b"
	})
	empNo := "4004"
	seedEmployment(t, svc, func(e *models.EmploymentIdentity) {
		e.UserSubjectID = "tenant-a-user"
		e.TenantID = "tenant-a"
		e.EmployeeNumber = &empNo
	})
	seedEmployment(t, svc, func(e *models.EmploymentIdentity) {
		e.UserSubjectID = "tenant-b-user"
		e.TenantID = "tenant-b"
		e.EmployeeNumber = &empNo
	})

	tenantA := &models.Tenant{TenantID: "tenant-a"}
	ctxA := tenant.WithTenant(context.Background(), tenantA)
	got, err := svc.SearchUsersByEmployeeNumber(ctxA, empNo, 1)
	if err != nil {
		t.Fatalf("SearchUsersByEmployeeNumber: %v", err)
	}
	if len(got) != 1 || got[0].SubjectID != "tenant-a-user" {
		t.Errorf("tenant-a scope: got %v, want [tenant-a-user]", got)
	}

	tenantB := &models.Tenant{TenantID: "tenant-b"}
	ctxB := tenant.WithTenant(context.Background(), tenantB)
	got, err = svc.SearchUsersByEmployeeNumber(ctxB, empNo, 1)
	if err != nil {
		t.Fatalf("SearchUsersByEmployeeNumber: %v", err)
	}
	if len(got) != 1 || got[0].SubjectID != "tenant-b-user" {
		t.Errorf("tenant-b scope: got %v, want [tenant-b-user]", got)
	}
}

func TestSearchUsersByEmployeeNumber_EmptyArgErrors(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.SearchUsersByEmployeeNumber(context.Background(), "", 1); !errors.Is(err, ErrEmptyEmployeeNumber) {
		t.Errorf("got %v, want ErrEmptyEmployeeNumber", err)
	}
}

// TestService_NilDBGuards asserts every method short-circuits cleanly when
// the service is constructed without a DB (defensive against future callers
// that forget to inject one).
func TestService_NilDBGuards(t *testing.T) {
	svc := &Service{}
	if _, err := svc.GetUserByID(context.Background(), "x"); err == nil {
		t.Error("GetUserByID on nil db should error")
	}
	if _, err := svc.GetUsersByIDs(context.Background(), []string{"x"}); err == nil {
		t.Error("GetUsersByIDs on nil db should error")
	}
	if _, err := svc.SearchUsers(context.Background(), "x", 1); err == nil {
		t.Error("SearchUsers on nil db should error")
	}
	if _, err := svc.SearchUsersByEmployeeNumber(context.Background(), "x", 1); err == nil {
		t.Error("SearchUsersByEmployeeNumber on nil db should error")
	}
	if _, err := svc.ListIdentities(context.Background(), "x"); err == nil {
		t.Error("ListIdentities on nil db should error")
	}
}

func strPtr(s string) *string { return &s }
