//go:build cgo

package auditlog

import (
	"context"
	"strings"
	"sync"
	"testing"

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
