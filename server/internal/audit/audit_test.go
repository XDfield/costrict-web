package audit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupTestDB opens an in-memory sqlite DB and hand-creates the admin_audit_logs
// table. We avoid AutoMigrate because the postgres-specific jsonb / uuid column
// types do not map cleanly onto sqlite; the hand-rolled schema mirrors the
// postgres migration closely enough for the service/handler logic under test.
func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}

	// Pin the pool to a single connection so every query hits the same :memory:
	// database (a fresh connection would open an empty db — "no such table").
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to get underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	if err := db.Exec(`CREATE TABLE admin_audit_logs (
		id TEXT PRIMARY KEY,
		actor_id TEXT NOT NULL,
		action TEXT NOT NULL,
		target_type TEXT,
		target_id TEXT,
		payload TEXT NOT NULL DEFAULT '{}',
		created_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("failed to create admin_audit_logs table: %v", err)
	}

	return db
}

// insert is a test helper that writes a row synchronously (the production Record
// path is async; tests want deterministic ordering and timestamps).
func insert(t *testing.T, db *gorm.DB, actor, action, targetType, targetID string, createdAt time.Time, payload string) {
	t.Helper()
	row := models.AdminAuditLog{
		ID:         action + "-" + targetID + "-" + createdAt.Format(time.RFC3339Nano),
		ActorID:    actor,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Payload:    []byte(payload),
		CreatedAt:  createdAt,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("insert audit row failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Write-path test (package Logger.Record). Record is async, so we poll briefly.
// ---------------------------------------------------------------------------

func TestLogger_Record_WritesRow(t *testing.T) {
	db := setupTestDB(t)
	// id has a sqlite default? No — our hand-rolled table has no default, so set
	// it via a BeforeCreate-less path: Record relies on the DB default in prod
	// (gen_random_uuid). For sqlite we patch the row to carry an id by using a
	// gorm callback-free Create with an explicit id through a small wrapper.
	lg := New(db)

	// Give the row an id by registering a create hook on the in-memory db.
	db.Callback().Create().Before("gorm:create").Register("test_audit_id", func(tx *gorm.DB) {
		if entry, ok := tx.Statement.Dest.(*models.AdminAuditLog); ok && entry.ID == "" {
			entry.ID = entry.Action + "-" + entry.TargetID + "-" + time.Now().Format(time.RFC3339Nano)
		}
	})

	lg.Record("actor_1", ActionEnterpriseCreate, TargetEnterpriseCustomer, "ent_1", map[string]any{"name": "Acme"})

	// Poll up to ~1s for the async insert.
	var count int64
	for i := 0; i < 50; i++ {
		db.Model(&models.AdminAuditLog{}).Count(&count)
		if count > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if count != 1 {
		t.Fatalf("expected 1 audit row after Record, got %d", count)
	}

	var row models.AdminAuditLog
	if err := db.First(&row).Error; err != nil {
		t.Fatalf("failed to read back audit row: %v", err)
	}
	if row.ActorID != "actor_1" || row.Action != ActionEnterpriseCreate || row.TargetID != "ent_1" {
		t.Errorf("unexpected row: %+v", row)
	}
	if !strings.Contains(string(row.Payload), "Acme") {
		t.Errorf("payload missing data: %s", string(row.Payload))
	}
}

func TestRecord_NoopWhenUninitialized(t *testing.T) {
	// pkgLogger is nil by default in a fresh test binary; package-level Record
	// must not panic when uninitialized.
	pkgLogger = nil
	Record("actor", "x.y", "t", "id", nil) // must not panic
}

// ---------------------------------------------------------------------------
// Query-path tests (Service.List filter + pagination + ordering).
// ---------------------------------------------------------------------------

func seedLogs(t *testing.T, db *gorm.DB) {
	t.Helper()
	base := time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)
	insert(t, db, "actor_a", ActionEnterpriseCreate, TargetEnterpriseCustomer, "ent_1", base, `{}`)
	insert(t, db, "actor_b", ActionSystemRoleGrant, TargetUser, "usr_1", base.Add(1*time.Hour), `{}`)
	insert(t, db, "actor_a", ActionSettingUpdate, TargetSetting, "maintenance_mode", base.Add(2*time.Hour), `{}`)
	insert(t, db, "actor_a", ActionEnterpriseCreate, TargetEnterpriseCustomer, "ent_2", base.Add(3*time.Hour), `{}`)
}

func TestService_List_OrderingAndPagination(t *testing.T) {
	db := setupTestDB(t)
	seedLogs(t, db)
	svc := NewService(db)

	logs, total, err := svc.List(Filter{}, 1, 2)
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if total != 4 {
		t.Fatalf("total = %d, want 4", total)
	}
	if len(logs) != 2 {
		t.Fatalf("page size = %d, want 2", len(logs))
	}
	// Newest first: ent_2 (base+3h) then maintenance_mode (base+2h).
	if logs[0].TargetID != "ent_2" || logs[1].TargetID != "maintenance_mode" {
		t.Errorf("ordering wrong: got [%s, %s]", logs[0].TargetID, logs[1].TargetID)
	}

	page2, _, err := svc.List(Filter{}, 2, 2)
	if err != nil {
		t.Fatalf("List page 2 error: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page 2 size = %d, want 2", len(page2))
	}
	if page2[0].TargetID != "usr_1" || page2[1].TargetID != "ent_1" {
		t.Errorf("page 2 ordering wrong: got [%s, %s]", page2[0].TargetID, page2[1].TargetID)
	}
}

func TestService_List_FilterByAction(t *testing.T) {
	db := setupTestDB(t)
	seedLogs(t, db)
	svc := NewService(db)

	logs, total, err := svc.List(Filter{Action: ActionEnterpriseCreate}, 1, 20)
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if total != 2 || len(logs) != 2 {
		t.Fatalf("action filter: total=%d len=%d, want 2/2", total, len(logs))
	}
	for _, l := range logs {
		if l.Action != ActionEnterpriseCreate {
			t.Errorf("unexpected action %q in filtered result", l.Action)
		}
	}
}

func TestService_List_FilterByActorAndTargetType(t *testing.T) {
	db := setupTestDB(t)
	seedLogs(t, db)
	svc := NewService(db)

	logs, total, err := svc.List(Filter{ActorID: "actor_a", TargetType: TargetEnterpriseCustomer}, 1, 20)
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if total != 2 || len(logs) != 2 {
		t.Fatalf("actor+type filter: total=%d len=%d, want 2/2", total, len(logs))
	}
}

func TestService_List_FilterByTimeRange(t *testing.T) {
	db := setupTestDB(t)
	seedLogs(t, db)
	svc := NewService(db)

	from := time.Date(2026, 6, 16, 11, 30, 0, 0, time.UTC) // after base+1h
	to := time.Date(2026, 6, 16, 12, 30, 0, 0, time.UTC)   // before base+3h
	logs, total, err := svc.List(Filter{From: &from, To: &to}, 1, 20)
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	// Only base+2h (maintenance_mode) falls in the window.
	if total != 1 || len(logs) != 1 || logs[0].TargetID != "maintenance_mode" {
		t.Fatalf("time range filter wrong: total=%d logs=%v", total, logs)
	}
}

// ---------------------------------------------------------------------------
// Handler-layer test (filter + pagination through the HTTP boundary).
// ---------------------------------------------------------------------------

func TestHandler_List(t *testing.T) {
	db := setupTestDB(t)
	seedLogs(t, db)
	svc := NewService(db)

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/?action="+ActionEnterpriseCreate+"&page=1&pageSize=20", nil)

	ListAuditLogsHandler(svc)(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Logs     []models.AdminAuditLog `json:"logs"`
		Total    int64                  `json:"total"`
		Page     int                    `json:"page"`
		PageSize int                    `json:"pageSize"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if resp.Total != 2 || len(resp.Logs) != 2 {
		t.Fatalf("handler filter: total=%d len=%d, want 2/2", resp.Total, len(resp.Logs))
	}
	if resp.Page != 1 || resp.PageSize != 20 {
		t.Errorf("pagination echo wrong: page=%d pageSize=%d", resp.Page, resp.PageSize)
	}
}
