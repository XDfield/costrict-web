package enterprise

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

// setupTestDB opens an in-memory sqlite DB and hand-creates the
// enterprise_customers table. We deliberately do NOT use AutoMigrate: the model
// declares ID `default:gen_random_uuid()`, which is postgres-only and would
// break sqlite. Here the id default uses sqlite's randomblob so id-less INSERTs
// still get a primary key, mirroring the postgres gen_random_uuid() behaviour.
func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}

	db.Exec(`CREATE TABLE enterprise_customers (
		id TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(16)))),
		name TEXT NOT NULL,
		logo TEXT NOT NULL,
		account_ids TEXT NOT NULL DEFAULT '[]',
		created_by TEXT,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME
	)`)

	return db
}

const (
	validLogo = "data:image/png;base64,iVBORw0KGgo="
)

// ---------------------------------------------------------------------------
// Service-layer tests
// ---------------------------------------------------------------------------

func TestService_Create_ListRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	created, err := svc.Create("Acme", validLogo, []string{"usr_uuid_1", "usr_uuid_2"}, "operator_1")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("expected non-empty id after Create, got empty (sqlite did not backfill default)")
	}

	customers, err := svc.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(customers) != 1 {
		t.Fatalf("expected 1 customer, got %d", len(customers))
	}

	row := customers[0]
	if row.Name != "Acme" {
		t.Errorf("name = %q, want %q", row.Name, "Acme")
	}
	if row.Logo != validLogo {
		t.Errorf("logo = %q, want %q", row.Logo, validLogo)
	}
	if row.CreatedBy == nil || *row.CreatedBy != "operator_1" {
		t.Errorf("createdBy = %v, want %q", row.CreatedBy, "operator_1")
	}

	gotIDs := decodeIDs(row.AccountIDs)
	wantIDs := []string{"usr_uuid_1", "usr_uuid_2"}
	if !equalStrSlice(gotIDs, wantIDs) {
		t.Errorf("decoded account_ids = %v, want %v", gotIDs, wantIDs)
	}
	if row.ID == "" {
		t.Errorf("expected non-empty id from List, got empty")
	}
}

func TestService_Create_Validation(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	cases := []struct {
		name    string
		cName   string
		cLogo   string
		wantErr error
	}{
		{"empty name", "", validLogo, ErrInvalidEnterpriseCustomer},
		{"empty logo", "Acme", "", ErrInvalidEnterpriseCustomer},
		{"logo not data:image prefix", "Acme", "http://x/a.png", ErrLogoTooLarge},
		{"logo too large", "Acme", logoDataURIPrefix + strings.Repeat("A", MaxLogoBytes), ErrLogoTooLarge},
		{"name too long", strings.Repeat("n", MaxNameBytes+1), validLogo, ErrNameTooLong},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Create(tc.cName, tc.cLogo, nil, "operator_1")
			if err != tc.wantErr {
				t.Fatalf("Create error = %v, want %v", err, tc.wantErr)
			}
		})
	}

	// None of the rejected creates should have persisted.
	customers, err := svc.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(customers) != 0 {
		t.Fatalf("expected 0 customers after rejected creates, got %d", len(customers))
	}
}

func TestService_Update(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	created, err := svc.Create("Acme", validLogo, []string{"usr_uuid_1"}, "operator_1")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	newLogo := "data:image/jpeg;base64,/9j/4AAQ="
	updated, err := svc.Update(created.ID, "Acme Renamed", newLogo, []string{"usr_uuid_2", "usr_uuid_3"})
	if err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if updated.Name != "Acme Renamed" {
		t.Errorf("updated name = %q, want %q", updated.Name, "Acme Renamed")
	}

	customers, err := svc.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(customers) != 1 {
		t.Fatalf("expected 1 customer, got %d", len(customers))
	}

	row := customers[0]
	if row.Name != "Acme Renamed" {
		t.Errorf("name after update = %q, want %q", row.Name, "Acme Renamed")
	}
	if row.Logo != newLogo {
		t.Errorf("logo after update = %q, want %q", row.Logo, newLogo)
	}
	gotIDs := decodeIDs(row.AccountIDs)
	wantIDs := []string{"usr_uuid_2", "usr_uuid_3"}
	if !equalStrSlice(gotIDs, wantIDs) {
		t.Errorf("account_ids after update = %v, want %v", gotIDs, wantIDs)
	}
}

func TestService_Update_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	_, err := svc.Update("does-not-exist", "Acme", validLogo, nil)
	if err != ErrEnterpriseCustomerNotFound {
		t.Fatalf("Update of missing id error = %v, want %v", err, ErrEnterpriseCustomerNotFound)
	}
}

func TestService_Delete(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	created, err := svc.Create("Acme", validLogo, []string{"usr_uuid_1"}, "operator_1")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	if err := svc.Delete(created.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	customers, err := svc.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(customers) != 0 {
		t.Fatalf("expected 0 customers after delete, got %d", len(customers))
	}
}

func TestService_Delete_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	err := svc.Delete("does-not-exist")
	if err != ErrEnterpriseCustomerNotFound {
		t.Fatalf("Delete of missing id error = %v, want %v", err, ErrEnterpriseCustomerNotFound)
	}
}

// ---------------------------------------------------------------------------
// Handler-layer tests (bypass casdoor/RequirePlatformAdmin; set UserIDKey
// manually to simulate an authenticated platform admin).
// ---------------------------------------------------------------------------

func newAuthedContext(t *testing.T, method, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	var reqBody *strings.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	} else {
		reqBody = strings.NewReader("")
	}
	req := httptest.NewRequest(method, "/", reqBody)
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set(appmiddleware.UserIDKey, "operator_1")
	return c, rec
}

func TestHandler_List(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	if _, err := svc.Create("Acme", validLogo, []string{"usr_uuid_1", "usr_uuid_2"}, "operator_1"); err != nil {
		t.Fatalf("seed Create returned error: %v", err)
	}

	c, rec := newAuthedContext(t, http.MethodGet, "")
	ListEnterpriseCustomersHandler(svc)(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Customers []customerResponse `json:"customers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v; body=%s", err, rec.Body.String())
	}
	if len(resp.Customers) != 1 {
		t.Fatalf("expected 1 customer in response, got %d", len(resp.Customers))
	}

	got := resp.Customers[0]
	if got.ID == "" {
		t.Errorf("response id is empty")
	}
	if got.Name != "Acme" {
		t.Errorf("response name = %q, want %q", got.Name, "Acme")
	}
	if got.Logo != validLogo {
		t.Errorf("response logo = %q, want %q", got.Logo, validLogo)
	}
	wantIDs := []string{"usr_uuid_1", "usr_uuid_2"}
	if !equalStrSlice(got.IDs, wantIDs) {
		t.Errorf("response ids = %v, want %v", got.IDs, wantIDs)
	}
}

func TestHandler_Create_Valid(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	body := `{"name":"Acme","logo":"` + validLogo + `","ids":["usr_uuid_1","usr_uuid_2"]}`
	c, rec := newAuthedContext(t, http.MethodPost, body)
	CreateEnterpriseCustomerHandler(svc)(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Customer customerResponse `json:"customer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v; body=%s", err, rec.Body.String())
	}
	if resp.Customer.ID == "" {
		t.Errorf("created customer id is empty")
	}
	if resp.Customer.Name != "Acme" {
		t.Errorf("created name = %q, want %q", resp.Customer.Name, "Acme")
	}
	wantIDs := []string{"usr_uuid_1", "usr_uuid_2"}
	if !equalStrSlice(resp.Customer.IDs, wantIDs) {
		t.Errorf("created ids = %v, want %v", resp.Customer.IDs, wantIDs)
	}

	// Confirm it actually persisted.
	customers, err := svc.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(customers) != 1 {
		t.Fatalf("expected 1 persisted customer, got %d", len(customers))
	}
}

func TestHandler_Create_InvalidBody(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	// Empty name fails the `binding:"required"` tag → 400 before service runs.
	body := `{"name":"","logo":"` + validLogo + `","ids":[]}`
	c, rec := newAuthedContext(t, http.MethodPost, body)
	CreateEnterpriseCustomerHandler(svc)(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	customers, err := svc.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(customers) != 0 {
		t.Fatalf("expected 0 customers after invalid create, got %d", len(customers))
	}
}

// equalStrSlice reports whether two string slices have identical elements in
// order.
func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
