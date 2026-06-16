package settings

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupTestDB opens an in-memory sqlite DB and hand-creates the system_settings
// table. We deliberately do NOT use AutoMigrate: the postgres-specific column
// types (jsonb) would break sqlite. The hand-rolled schema mirrors the postgres
// migration closely enough for the service/handler logic under test.
func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}

	// Pin the pool to a single connection so every query hits the same :memory:
	// database (otherwise a fresh connection opens an empty db and the table
	// vanishes — "no such table").
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to get underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	if err := db.Exec(`CREATE TABLE system_settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL DEFAULT '{}',
		updated_by TEXT,
		updated_at DATETIME,
		created_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("failed to create system_settings table: %v", err)
	}

	return db
}

// ---------------------------------------------------------------------------
// Service-layer tests
// ---------------------------------------------------------------------------

func TestService_Set_GetAllRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	if _, err := svc.Set("maintenance_mode", json.RawMessage(`true`), "operator_1"); err != nil {
		t.Fatalf("Set returned error: %v", err)
	}
	if _, err := svc.Set("announcement_enabled", json.RawMessage(`{"text":"hi"}`), "operator_1"); err != nil {
		t.Fatalf("Set returned error: %v", err)
	}

	all, err := svc.GetAll()
	if err != nil {
		t.Fatalf("GetAll returned error: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 settings, got %d", len(all))
	}
	if string(all["maintenance_mode"]) != "true" {
		t.Errorf("maintenance_mode = %q, want %q", string(all["maintenance_mode"]), "true")
	}
	if string(all["announcement_enabled"]) != `{"text":"hi"}` {
		t.Errorf("announcement_enabled = %q, want %q", string(all["announcement_enabled"]), `{"text":"hi"}`)
	}
}

func TestService_Set_Upsert(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	if _, err := svc.Set("maintenance_mode", json.RawMessage(`false`), "operator_1"); err != nil {
		t.Fatalf("first Set returned error: %v", err)
	}
	if _, err := svc.Set("maintenance_mode", json.RawMessage(`true`), "operator_2"); err != nil {
		t.Fatalf("second Set returned error: %v", err)
	}

	all, err := svc.GetAll()
	if err != nil {
		t.Fatalf("GetAll returned error: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 setting after upsert, got %d", len(all))
	}
	if string(all["maintenance_mode"]) != "true" {
		t.Errorf("maintenance_mode = %q, want %q (upsert should overwrite)", string(all["maintenance_mode"]), "true")
	}
}

func TestService_Set_Validation(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	cases := []struct {
		name    string
		key     string
		value   json.RawMessage
		wantErr error
	}{
		{"empty key", "", json.RawMessage(`true`), ErrInvalidKey},
		{"key too long", strings.Repeat("k", MaxKeyBytes+1), json.RawMessage(`true`), ErrInvalidKey},
		{"invalid json value", "flag", json.RawMessage(`{not json`), ErrInvalidValue},
		{"value too large", "flag", json.RawMessage(`"` + strings.Repeat("a", MaxValueBytes) + `"`), ErrInvalidValue},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Set(tc.key, tc.value, "operator_1")
			if err != tc.wantErr {
				t.Fatalf("Set error = %v, want %v", err, tc.wantErr)
			}
		})
	}

	all, err := svc.GetAll()
	if err != nil {
		t.Fatalf("GetAll returned error: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("expected 0 settings after rejected sets, got %d", len(all))
	}
}

func TestService_Set_EmptyValueDefaultsToNull(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	if _, err := svc.Set("flag", nil, "operator_1"); err != nil {
		t.Fatalf("Set with nil value returned error: %v", err)
	}
	all, err := svc.GetAll()
	if err != nil {
		t.Fatalf("GetAll returned error: %v", err)
	}
	if string(all["flag"]) != "null" {
		t.Errorf("flag = %q, want %q", string(all["flag"]), "null")
	}
}

// ---------------------------------------------------------------------------
// Handler-layer tests (bypass RequirePlatformAdmin; set UserIDKey manually to
// simulate an authenticated platform admin).
// ---------------------------------------------------------------------------

func newAuthedContext(t *testing.T, method, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set(appmiddleware.UserIDKey, "operator_1")
	return c, rec
}

func TestHandler_List(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	if _, err := svc.Set("maintenance_mode", json.RawMessage(`true`), "operator_1"); err != nil {
		t.Fatalf("seed Set returned error: %v", err)
	}

	c, rec := newAuthedContext(t, http.MethodGet, "")
	ListSettingsHandler(svc)(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Settings map[string]json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v; body=%s", err, rec.Body.String())
	}
	if string(resp.Settings["maintenance_mode"]) != "true" {
		t.Errorf("response maintenance_mode = %q, want %q", string(resp.Settings["maintenance_mode"]), "true")
	}
}

func TestHandler_Update_Valid(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	c, rec := newAuthedContext(t, http.MethodPut, `{"value":true}`)
	c.Params = gin.Params{{Key: "key", Value: "maintenance_mode"}}
	UpdateSettingHandler(svc)(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	all, err := svc.GetAll()
	if err != nil {
		t.Fatalf("GetAll returned error: %v", err)
	}
	if string(all["maintenance_mode"]) != "true" {
		t.Errorf("persisted maintenance_mode = %q, want %q", string(all["maintenance_mode"]), "true")
	}
}

func TestHandler_Update_InvalidValue(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	// A value that exceeds MaxValueBytes is rejected with 400.
	body := `{"value":"` + strings.Repeat("a", MaxValueBytes) + `"}`
	c, rec := newAuthedContext(t, http.MethodPut, body)
	c.Params = gin.Params{{Key: "key", Value: "flag"}}
	UpdateSettingHandler(svc)(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	all, err := svc.GetAll()
	if err != nil {
		t.Fatalf("GetAll returned error: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("expected 0 settings after invalid update, got %d", len(all))
	}
}
