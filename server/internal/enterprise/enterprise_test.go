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

	// Pin the pool to a single connection so every query in a test hits the same
	// :memory: database. With the default pool a second connection opens a fresh,
	// empty :memory: db and the hand-created table vanishes ("no such table").
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to get underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	if err := db.Exec(`CREATE TABLE enterprise_customers (
		id TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(16)))),
		name TEXT NOT NULL,
		logo TEXT NOT NULL,
		account_ids TEXT NOT NULL DEFAULT '[]',
		created_by TEXT,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("failed to create enterprise_customers table: %v", err)
	}

	// Minimal users table so ResolveMembers / ResolveSubjectIDs can join
	// casdoor_universal_id -> subject_id/username/display_name/avatar_url.
	if err := db.Exec(`CREATE TABLE users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		subject_id TEXT,
		username TEXT,
		display_name TEXT,
		avatar_url TEXT,
		casdoor_universal_id TEXT,
		deleted_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("failed to create users table: %v", err)
	}

	return db
}

// seedUser inserts a local user row keyed by its Casdoor universal_id so resolve
// helpers can map it back to subject_id / display fields.
func seedUser(t *testing.T, db *gorm.DB, subjectID, username, displayName, avatarURL, universalID string) {
	t.Helper()
	if err := db.Exec(
		`INSERT INTO users (subject_id, username, display_name, avatar_url, casdoor_universal_id) VALUES (?, ?, ?, ?, ?)`,
		subjectID, username, displayName, avatarURL, universalID,
	).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
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
		{"logo bad base64 payload", "Acme", "data:image/png;base64,!!!notbase64!!!", ErrLogoInvalid},
		{"logo missing base64 marker", "Acme", "data:image/png,rawdata", ErrLogoInvalid},
		{"logo unsupported svg mime", "Acme", "data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=", ErrLogoInvalid},
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

	newLogo := "data:image/jpeg;base64,/9j/4AAQ"
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

func TestService_ResolveMembers(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	seedUser(t, db, "usr_1", "alice", "Alice", "http://x/a.png", "uid_1")
	seedUser(t, db, "usr_2", "bob", "", "", "uid_2")

	// Order preserved; every input id yields a Member; unresolved id kept (empty
	// SubjectID).
	members := svc.ResolveMembers([]string{"uid_2", "uid_missing", "uid_1"})
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(members))
	}
	if members[0].UniversalID != "uid_2" || members[0].SubjectID != "usr_2" || members[0].Username != "bob" {
		t.Errorf("member[0] = %+v", members[0])
	}
	if members[0].DisplayName != "" {
		t.Errorf("member[0].DisplayName = %q, want empty", members[0].DisplayName)
	}
	if members[1].UniversalID != "uid_missing" || members[1].SubjectID != "" {
		t.Errorf("member[1] (unresolved) = %+v", members[1])
	}
	if members[2].UniversalID != "uid_1" || members[2].SubjectID != "usr_1" ||
		members[2].DisplayName != "Alice" || members[2].AvatarURL != "http://x/a.png" {
		t.Errorf("member[2] = %+v", members[2])
	}
}

func TestService_ResolveMembers_Empty(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	if got := svc.ResolveMembers(nil); len(got) != 0 {
		t.Fatalf("ResolveMembers(nil) = %v, want empty", got)
	}
	if got := svc.ResolveMembers([]string{}); len(got) != 0 {
		t.Fatalf("ResolveMembers([]) = %v, want empty", got)
	}
}

func TestService_ResolveSubjectIDs(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	seedUser(t, db, "usr_1", "alice", "Alice", "", "uid_1")
	seedUser(t, db, "usr_2", "bob", "Bob", "", "uid_2")

	// Only resolvable ids return subject_ids; unresolved dropped; order preserved.
	got := svc.ResolveSubjectIDs([]string{"uid_2", "uid_missing", "uid_1"})
	want := []string{"usr_2", "usr_1"}
	if !equalStrSlice(got, want) {
		t.Errorf("ResolveSubjectIDs = %v, want %v", got, want)
	}
}

// TestService_ResolveMembersBatch covers the shared batch core: it returns a map
// keyed by universal_id, dedupes inputs, and only contains resolvable ids.
func TestService_ResolveMembersBatch(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	seedUser(t, db, "usr_1", "alice", "Alice", "http://x/a.png", "uid_1")
	seedUser(t, db, "usr_2", "bob", "Bob", "", "uid_2")

	// Duplicate uid_1 + an unresolvable uid_missing: dedup tolerated, missing absent.
	m := svc.ResolveMembersBatch([]string{"uid_1", "uid_2", "uid_1", "uid_missing", ""})
	if len(m) != 2 {
		t.Fatalf("expected 2 resolved entries, got %d: %+v", len(m), m)
	}
	if got, ok := m["uid_1"]; !ok || got.SubjectID != "usr_1" || got.Username != "alice" ||
		got.DisplayName != "Alice" || got.AvatarURL != "http://x/a.png" {
		t.Errorf("m[uid_1] = %+v, ok=%v", got, ok)
	}
	if got, ok := m["uid_2"]; !ok || got.SubjectID != "usr_2" {
		t.Errorf("m[uid_2] = %+v, ok=%v", got, ok)
	}
	if _, ok := m["uid_missing"]; ok {
		t.Errorf("unresolvable uid_missing must be absent from the map")
	}

	// Empty input yields an empty (non-nil) map.
	if got := svc.ResolveMembersBatch(nil); got == nil || len(got) != 0 {
		t.Errorf("ResolveMembersBatch(nil) = %v, want empty non-nil map", got)
	}
}

// TestService_ResolveMembers_DeterministicTiebreak pins the determinism contract:
// when two user rows share the same casdoor_universal_id (a non-unique index), the
// lowest-id row wins and the result is stable across repeated calls.
func TestService_ResolveMembers_DeterministicTiebreak(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	// Same universal_id on two rows; insert the LATER subject first so the lowest
	// id (inserted first → smaller AUTOINCREMENT) is the "winner" we assert on.
	seedUser(t, db, "usr_low", "low", "Low", "", "uid_dup")    // id=1 (lowest)
	seedUser(t, db, "usr_high", "high", "High", "", "uid_dup") // id=2

	for i := 0; i < 5; i++ {
		members := svc.ResolveMembers([]string{"uid_dup"})
		if len(members) != 1 {
			t.Fatalf("iter %d: expected 1 member, got %d", i, len(members))
		}
		if members[0].SubjectID != "usr_low" || members[0].Username != "low" {
			t.Fatalf("iter %d: tiebreak winner = %+v, want lowest-id row usr_low", i, members[0])
		}
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
	// account_ids store universal_id; the public list resolves them to subject_id.
	// Seed two resolvable users + one universal_id with no local user (must drop).
	seedUser(t, db, "usr_1", "alice", "Alice", "", "uid_1")
	seedUser(t, db, "usr_2", "bob", "Bob", "", "uid_2")
	if _, err := svc.Create("Acme", validLogo, []string{"uid_1", "uid_2", "uid_missing"}, "operator_1"); err != nil {
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
	// Public endpoint exposes RESOLVED subject_ids only; unresolved uid_missing drops.
	wantIDs := []string{"usr_1", "usr_2"}
	if !equalStrSlice(got.IDs, wantIDs) {
		t.Errorf("response ids = %v, want %v (must be resolved subject_ids, no universal_id leak)", got.IDs, wantIDs)
	}
}

func TestHandler_AdminList(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	seedUser(t, db, "usr_1", "alice", "Alice", "http://x/a.png", "uid_1")
	// uid_missing has no local user → Member with empty SubjectID, still returned.
	if _, err := svc.Create("Acme", validLogo, []string{"uid_1", "uid_missing"}, "operator_1"); err != nil {
		t.Fatalf("seed Create returned error: %v", err)
	}

	c, rec := newAuthedContext(t, http.MethodGet, "")
	ListEnterpriseCustomersAdminHandler(svc)(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Customers []adminCustomerResponse `json:"customers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v; body=%s", err, rec.Body.String())
	}
	if len(resp.Customers) != 1 {
		t.Fatalf("expected 1 customer, got %d", len(resp.Customers))
	}

	got := resp.Customers[0]
	// Admin endpoint exposes the RAW universal_id list (identity anchor).
	wantUniversal := []string{"uid_1", "uid_missing"}
	if !equalStrSlice(got.UniversalIDs, wantUniversal) {
		t.Errorf("universalIds = %v, want %v", got.UniversalIDs, wantUniversal)
	}
	if len(got.Members) != 2 {
		t.Fatalf("expected 2 members (resolved + unresolved), got %d", len(got.Members))
	}
	if got.Members[0].UniversalID != "uid_1" || got.Members[0].SubjectID != "usr_1" ||
		got.Members[0].Username != "alice" || got.Members[0].DisplayName != "Alice" ||
		got.Members[0].AvatarURL != "http://x/a.png" {
		t.Errorf("resolved member[0] = %+v", got.Members[0])
	}
	// Unresolved universal_id is kept with an empty SubjectID.
	if got.Members[1].UniversalID != "uid_missing" || got.Members[1].SubjectID != "" {
		t.Errorf("unresolved member[1] = %+v, want {UniversalID:uid_missing SubjectID:\"\"}", got.Members[1])
	}
}

func TestHandler_Create_Valid(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	seedUser(t, db, "usr_1", "alice", "Alice", "", "uid_1")

	// ids are now Casdoor universal_id; create returns the ADMIN shape
	// (universalIds + resolved members).
	body := `{"name":"Acme","logo":"` + validLogo + `","ids":["uid_1","uid_2"]}`
	c, rec := newAuthedContext(t, http.MethodPost, body)
	CreateEnterpriseCustomerHandler(svc)(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Customer adminCustomerResponse `json:"customer"`
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
	wantUniversal := []string{"uid_1", "uid_2"}
	if !equalStrSlice(resp.Customer.UniversalIDs, wantUniversal) {
		t.Errorf("created universalIds = %v, want %v", resp.Customer.UniversalIDs, wantUniversal)
	}
	if len(resp.Customer.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(resp.Customer.Members))
	}
	if resp.Customer.Members[0].SubjectID != "usr_1" || resp.Customer.Members[1].SubjectID != "" {
		t.Errorf("members = %+v; want member[0] resolved to usr_1, member[1] unresolved", resp.Customer.Members)
	}

	// Confirm it actually persisted (stored value = the universal_id list).
	customers, err := svc.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(customers) != 1 {
		t.Fatalf("expected 1 persisted customer, got %d", len(customers))
	}
	if !equalStrSlice(decodeIDs(customers[0].AccountIDs), wantUniversal) {
		t.Errorf("persisted account_ids = %v, want %v", decodeIDs(customers[0].AccountIDs), wantUniversal)
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
