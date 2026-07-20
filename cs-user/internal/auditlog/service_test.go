//go:build cgo

package auditlog

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// fakeLogger captures Printf calls so tests can assert WARN output.
type fakeLogger struct {
	mu    sync.Mutex
	lines []string
}

func (f *fakeLogger) Printf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// We don't evaluate format — just capture it raw so tests can match
	// substrings. Simplifies assertions and avoids fmt dependency here.
	f.lines = append(f.lines, format)
}

func (f *fakeLogger) HasLineContaining(needle string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.lines {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}

func newAuditTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.AuditLog{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// TestRecord_HappyPath verifies a fully-populated RecordParams round-trips
// through Create→First with all fields preserved.
func TestRecord_HappyPath(t *testing.T) {
	t.Parallel()
	db := newAuditTestDB(t)
	svc := NewService(db, nil)

	err := svc.Record(context.Background(), RecordParams{
		TenantID:           "t-acme",
		ActorSubjectID:     "u-admin",
		ActorTenantRole:    "owner",
		ActorPlatformScope: "full",
		Action:             models.ActionTenantSuspend,
		TargetType:         models.TargetTypeTenant,
		TargetID:           "tenant:t-acme",
		Payload:            map[string]any{"reason": "billing_overdue"},
		IP:                 "10.0.0.1",
		UserAgent:          "curl/8.0",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	var got models.AuditLog
	if err := db.First(&got, "action = ?", models.ActionTenantSuspend).Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.TenantID == nil || *got.TenantID != "t-acme" {
		t.Errorf("tenant_id: got %v, want t-acme", got.TenantID)
	}
	if got.ActorSubjectID == nil || *got.ActorSubjectID != "u-admin" {
		t.Errorf("actor_subject_id: got %v, want u-admin", got.ActorSubjectID)
	}
	if got.ActorTenantRole == nil || *got.ActorTenantRole != "owner" {
		t.Errorf("actor_tenant_role: got %v, want owner", got.ActorTenantRole)
	}
	if got.IP == nil || *got.IP != "10.0.0.1" {
		t.Errorf("ip: got %v, want 10.0.0.1", got.IP)
	}
}

// TestRecord_NullableTenantID verifies platform-level events write NULL
// tenant_id (no panic, no NOT NULL violation).
func TestRecord_NullableTenantID(t *testing.T) {
	t.Parallel()
	db := newAuditTestDB(t)
	svc := NewService(db, nil)

	err := svc.Record(context.Background(), RecordParams{
		ActorSubjectID:     "u-platform",
		ActorPlatformScope: "full",
		Action:             models.ActionTenantCreate,
		TargetType:         models.TargetTypeTenant,
		TargetID:           "tenant:t-new",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	var got models.AuditLog
	if err := db.First(&got, "action = ?", models.ActionTenantCreate).Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.TenantID != nil {
		t.Errorf("tenant_id: got %v, want nil", *got.TenantID)
	}
}

// TestRecord_NullableActor verifies system-initiated actions (no actor)
// round-trip — future hard-delete cron will use this shape.
func TestRecord_NullableActor(t *testing.T) {
	t.Parallel()
	db := newAuditTestDB(t)
	svc := NewService(db, nil)

	err := svc.Record(context.Background(), RecordParams{
		TenantID:   "t-cron",
		Action:     "tenant.hard_delete",
		TargetType: models.TargetTypeTenant,
		TargetID:   "tenant:t-cron",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	var got models.AuditLog
	if err := db.First(&got, "action = ?", "tenant.hard_delete").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.ActorSubjectID != nil {
		t.Errorf("actor_subject_id: got %v, want nil", *got.ActorSubjectID)
	}
}

// TestRecord_EmptyAction verifies the required-field guard short-circuits
// before touching the DB. No row should be written.
func TestRecord_EmptyAction(t *testing.T) {
	t.Parallel()
	db := newAuditTestDB(t)
	svc := NewService(db, nil)

	err := svc.Record(context.Background(), RecordParams{
		TenantID: "t-acme",
	})
	if err == nil {
		t.Fatal("expected ErrEmptyAction, got nil")
	}
	if !errorsIs(err, ErrEmptyAction) {
		t.Fatalf("err: got %v, want ErrEmptyAction", err)
	}
	var count int64
	db.Model(&models.AuditLog{}).Count(&count)
	if count != 0 {
		t.Errorf("no row should be written; got count=%d", count)
	}
}

// errorsIs is a local alias so the test file does not need to import errors
// (keeps the import list tight).
func errorsIs(err, target error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), target.Error())
}

// TestRecord_NilServiceDoesNotPanic verifies the nil-safe contract — callers
// may ignore the returned error and the call must not panic.
func TestRecord_NilServiceDoesNotPanic(t *testing.T) {
	t.Parallel()
	var svc *Service
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil service panicked: %v", r)
		}
	}()
	err := svc.Record(context.Background(), RecordParams{Action: models.ActionTenantCreate})
	if err == nil {
		t.Fatal("expected ErrNilDB, got nil")
	}
}

// TestRecord_DBClosedReturnsError verifies a DB write failure surfaces as a
// wrapped error AND triggers the WARN logger. Caller still ignores the error.
func TestRecord_DBClosedReturnsError(t *testing.T) {
	t.Parallel()
	db := newAuditTestDB(t)
	lg := &fakeLogger{}
	svc := NewService(db, lg)

	// Close the underlying connection so the next Create fails.
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}

	err := svc.Record(context.Background(), RecordParams{
		Action:     models.ActionTenantSuspend,
		TargetID:   "tenant:t-x",
		TargetType: models.TargetTypeTenant,
	})
	if err == nil {
		t.Fatal("expected write error, got nil")
	}
	if !lg.HasLineContaining("auditlog.Record: write failed") {
		t.Errorf("expected WARN log line, got %d lines: %v", len(lg.lines), lg.lines)
	}
}

// seedAuditRows writes count rows spread across distinct tenant/actor/action
// dimensions so list-filter tests have predictable fixtures. Returns the
// inserted rows in insertion order (oldest first).
func seedAuditRows(t *testing.T, svc *Service, count int) []*models.AuditLog {
	t.Helper()
	rows := make([]*models.AuditLog, 0, count)
	tenants := []string{"t-acme", "t-globex", "t-initech"}
	actors := []string{"u-admin", "u-support", "u-auditor"}
	actions := []string{
		models.ActionTenantCreate,
		models.ActionUserStatusChanged,
		models.ActionTenantConfigUpdate,
	}
	for i := 0; i < count; i++ {
		tenant := tenants[i%len(tenants)]
		actor := actors[i%len(actors)]
		action := actions[i%len(actions)]
		ts := time.Date(2026, 7, 1, 12, 0, i, 0, time.UTC)
		row := &models.AuditLog{
			TenantID:       &tenant,
			ActorSubjectID: &actor,
			Action:         action,
			TargetType:     ptrString(models.TargetTypeTenant),
			TargetID:       ptrString("tenant:" + tenant),
			CreatedAt:      ts,
		}
		if err := svc.db.Create(row).Error; err != nil {
			t.Fatalf("seed[%d]: %v", i, err)
		}
		rows = append(rows, row)
	}
	return rows
}

func ptrString(s string) *string { return &s }

// TestList_EmptyReturnsZeroTotal verifies a fresh DB reports Total=0 + nil
// Logs without error. The empty case must NOT be confused with an error.
func TestList_EmptyReturnsZeroTotal(t *testing.T) {
	t.Parallel()
	db := newAuditTestDB(t)
	svc := NewService(db, nil)

	res, err := svc.List(context.Background(), ListParams{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.Total != 0 {
		t.Errorf("Total = %d, want 0", res.Total)
	}
	if len(res.Logs) != 0 {
		t.Errorf("Logs len = %d, want 0", len(res.Logs))
	}
	if res.Limit != 100 {
		t.Errorf("Limit default = %d, want 100", res.Limit)
	}
	if res.Offset != 0 {
		t.Errorf("Offset default = %d, want 0", res.Offset)
	}
}

// TestList_DefaultsAndCaps asserts Limit normalization: non-positive -> 100,
// over-500 -> 500, in-range preserved. Negative Offset -> 0.
func TestList_DefaultsAndCaps(t *testing.T) {
	t.Parallel()
	db := newAuditTestDB(t)
	svc := NewService(db, nil)
	seedAuditRows(t, svc, 1)

	cases := []struct {
		name        string
		limit, offs int
		wantLimit   int
		wantOffset  int
	}{
		{"zero-limit-defaults-100", 0, 0, 100, 0},
		{"negative-limit-defaults-100", -5, 0, 100, 0},
		{"over-cap-clamped-500", 1000, 0, 500, 0},
		{"in-range-preserved", 25, 10, 25, 10},
		{"negative-offset-normalized-0", 50, -3, 50, 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Serial: the parent's sqlite :memory: DB is shared across
			// subtests; parallel goroutines trip gorm's connection pool
			// (each conn sees a fresh :memory: instance).
			res, err := svc.List(context.Background(), ListParams{Limit: tc.limit, Offset: tc.offs})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if res.Limit != tc.wantLimit {
				t.Errorf("Limit = %d, want %d", res.Limit, tc.wantLimit)
			}
			if res.Offset != tc.wantOffset {
				t.Errorf("Offset = %d, want %d", res.Offset, tc.wantOffset)
			}
		})
	}
}

// TestList_FilterByTenant verifies the tenant_id predicate narrows the result
// set and Total reflects only matching rows.
func TestList_FilterByTenant(t *testing.T) {
	t.Parallel()
	db := newAuditTestDB(t)
	svc := NewService(db, nil)
	seedAuditRows(t, svc, 9) // 3 tenants x 3 rows each

	res, err := svc.List(context.Background(), ListParams{TenantID: "t-acme"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.Total != 3 {
		t.Errorf("Total = %d, want 3", res.Total)
	}
	for _, r := range res.Logs {
		if r.TenantID == nil || *r.TenantID != "t-acme" {
			t.Errorf("row tenant = %v, want t-acme", r.TenantID)
		}
	}
}

// TestList_FilterByActionActorTarget verifies all three exact-match predicates
// compose with AND semantics.
func TestList_FilterByActionActorTarget(t *testing.T) {
	t.Parallel()
	db := newAuditTestDB(t)
	svc := NewService(db, nil)
	seedAuditRows(t, svc, 9)

	res, err := svc.List(context.Background(), ListParams{
		Action:         models.ActionTenantCreate,
		ActorSubjectID: "u-admin",
		TargetType:     models.TargetTypeTenant,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// seedAuditRows cycles actions [create, status_changed, config_update]
	// and actors [admin, support, auditor] in lockstep -- so action=create
	// coincides with actor=admin only at i=0,3,6 (3 rows). All three have
	// target_type=tenant.
	if res.Total != 3 {
		t.Errorf("Total = %d, want 3", res.Total)
	}
	for _, r := range res.Logs {
		if r.Action != models.ActionTenantCreate {
			t.Errorf("action = %q, want %q", r.Action, models.ActionTenantCreate)
		}
		if *r.ActorSubjectID != "u-admin" {
			t.Errorf("actor = %q, want u-admin", *r.ActorSubjectID)
		}
	}
}

// TestList_FilterByTimeRange verifies From/To bounds on created_at.
func TestList_FilterByTimeRange(t *testing.T) {
	t.Parallel()
	db := newAuditTestDB(t)
	svc := NewService(db, nil)
	seedAuditRows(t, svc, 9) // created_at = 2026-07-01 12:00:{0..8} UTC

	from := time.Date(2026, 7, 1, 12, 0, 3, 0, time.UTC)
	to := time.Date(2026, 7, 1, 12, 0, 6, 0, time.UTC)
	res, err := svc.List(context.Background(), ListParams{From: from, To: to})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Inclusive bounds -> seconds {3,4,5,6} = 4 rows.
	if res.Total != 4 {
		t.Errorf("Total = %d, want 4", res.Total)
	}
	for _, r := range res.Logs {
		if r.CreatedAt.Before(from) || r.CreatedAt.After(to) {
			t.Errorf("created_at = %v outside [%v,%v]", r.CreatedAt, from, to)
		}
	}
}

// TestList_OrderIsNewestFirst verifies the ORDER BY created_at DESC, id DESC
// tiebreaker. Seed inserts in chronological order; list must return reverse.
func TestList_OrderIsNewestFirst(t *testing.T) {
	t.Parallel()
	db := newAuditTestDB(t)
	svc := NewService(db, nil)
	rows := seedAuditRows(t, svc, 5)

	res, err := svc.List(context.Background(), ListParams{Limit: 5})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Logs) != 5 {
		t.Fatalf("Logs len = %d, want 5", len(res.Logs))
	}
	want := []*models.AuditLog{rows[4], rows[3], rows[2], rows[1], rows[0]}
	for i, w := range want {
		if res.Logs[i].ID != w.ID {
			t.Errorf("Logs[%d].ID = %d, want %d", i, res.Logs[i].ID, w.ID)
		}
	}
}

// TestList_Pagination verifies Limit+Offset return the expected slice and
// Total stays the unfiltered count.
func TestList_Pagination(t *testing.T) {
	t.Parallel()
	db := newAuditTestDB(t)
	svc := NewService(db, nil)
	seedAuditRows(t, svc, 9)

	res, err := svc.List(context.Background(), ListParams{Limit: 3, Offset: 3})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.Total != 9 {
		t.Errorf("Total = %d, want 9 (unfiltered)", res.Total)
	}
	if len(res.Logs) != 3 {
		t.Errorf("Logs len = %d, want 3", len(res.Logs))
	}
	if res.Limit != 3 || res.Offset != 3 {
		t.Errorf("Limit/Offset = %d/%d, want 3/3", res.Limit, res.Offset)
	}
}

// TestList_NilServiceReturnsErrNilDB mirrors Record's nil-safe contract so
// List can be exposed via handlers whose Deps graph may have a nil AuditLog
// (503 fallback).
func TestList_NilServiceReturnsErrNilDB(t *testing.T) {
	t.Parallel()
	var svc *Service
	_, err := svc.List(context.Background(), ListParams{})
	if err != ErrNilDB {
		t.Errorf("err = %v, want ErrNilDB", err)
	}
}
