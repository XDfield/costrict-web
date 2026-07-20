//go:build cgo

package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newAuditLogDB mirrors newPlatformAdminDB for in-memory sqlite + AutoMigrate.
// cgo-gated because sqlite needs CGO. The Postgres-specific JSONB column type
// is handled by GORM's translation layer for sqlite (stored as BLOB).
func newAuditLogDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&AuditLog{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func strPtr(s string) *string { return &s }

// TestAuditLog_TableName verifies the singular table name override — the
// migration ships user_center_audit_log (singular), not gorm's default
// audit_logs pluralization.
func TestAuditLog_TableName(t *testing.T) {
	t.Parallel()
	a := AuditLog{}
	if got := a.TableName(); got != "user_center_audit_log" {
		t.Errorf("TableName: got %q, want %q", got, "user_center_audit_log")
	}
}

// TestAuditLog_NullableFieldsDefault verifies tenant_id / actor_subject_id /
// role / scope / target_* / payload / ip / user_agent all accept NULL — the
// design explicitly wants platform-level events (NULL tenant_id) and system
// actions (NULL actor) to be first-class.
func TestAuditLog_NullableFieldsDefault(t *testing.T) {
	t.Parallel()
	db := newAuditLogDB(t)

	a := &AuditLog{
		Action:    ActionTenantCreate,
		CreatedAt: time.Now(),
	}
	if err := db.Create(a).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	var got AuditLog
	if err := db.First(&got, "id = ?", a.ID).Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.TenantID != nil {
		t.Errorf("tenant_id: got %v, want nil", *got.TenantID)
	}
	if got.ActorSubjectID != nil {
		t.Errorf("actor_subject_id: got %v, want nil", *got.ActorSubjectID)
	}
	if got.ActorTenantRole != nil {
		t.Errorf("actor_tenant_role: got %v, want nil", *got.ActorTenantRole)
	}
	if got.Payload != nil {
		t.Errorf("payload: got %v, want nil", got.Payload)
	}
}

// TestAuditLog_PayloadJSONRoundTrip verifies the JSONB column round-trips a
// arbitrary map through Marshal/Unmarshal. The Postgres JSONB type accepts
// arbitrary valid JSON; sqlite stores it as a BLOB.
func TestAuditLog_PayloadJSONRoundTrip(t *testing.T) {
	t.Parallel()
	db := newAuditLogDB(t)

	payload := map[string]any{
		"before": map[string]any{"status": "active"},
		"after":  map[string]any{"status": "suspended"},
		"count":  42,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	a := &AuditLog{
		Action:    ActionTenantSuspend,
		TargetID:  strPtr("tenant:t-acme"),
		Payload:   raw,
		CreatedAt: time.Now(),
	}
	if err := db.Create(a).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	var got AuditLog
	if err := db.First(&got, "id = ?", a.ID).Error; err != nil {
		t.Fatalf("First: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(got.Payload, &decoded); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	after, _ := decoded["after"].(map[string]any)
	if after["status"] != "suspended" {
		t.Errorf("payload.after.status: got %v, want suspended", after["status"])
	}
}

// TestAuditLog_ActionVocabConstants verifies the action and target_type
// constants compile + have stable string values. This is a regression guard
// against accidental vocabulary renames — downstream consumers (list endpoint,
// dashboards) key off these strings.
func TestAuditLog_ActionVocabConstants(t *testing.T) {
	t.Parallel()
	actions := []string{
		ActionTenantCreate,
		ActionTenantSuspend,
		ActionTenantRestore,
		ActionTenantDeletionRequested,
		ActionTenantConfigUpdate,
		ActionProviderMappingUpdate,
		ActionUserGiteaProvisioned,
		ActionUserStatusChanged,
	}
	seen := make(map[string]bool, len(actions))
	for i, a := range actions {
		if !strings.Contains(a, ".") {
			t.Errorf("action %d (%q) must contain a dot separator", i, a)
		}
		if a == "" {
			t.Errorf("action %d must not be empty", i)
		}
		if seen[a] {
			t.Errorf("action %q is duplicated in the test list (collision risk)", a)
		}
		seen[a] = true
	}
	if ActionUserStatusChanged != "user.status_changed" {
		t.Errorf("ActionUserStatusChanged must be %q, got %q", "user.status_changed", ActionUserStatusChanged)
	}
	targets := []string{
		TargetTypeTenant,
		TargetTypeTenantConfig,
		TargetTypeProviderMapping,
		TargetTypeUserGiteaBinding,
		TargetTypeUser,
	}
	for i, tt := range targets {
		if tt == "" {
			t.Errorf("target_type %d must not be empty", i)
		}
	}
	if TargetTypeUser != "user" {
		t.Errorf("TargetTypeUser must be %q, got %q", "user", TargetTypeUser)
	}
}
