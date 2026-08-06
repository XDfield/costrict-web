// Tests for the Gitea fork binding pre-check endpoint (FI-2).
//
// Uses sqlite :memory: + an in-process gin engine (setupHandlersDB /
// rawInsertServer live in git_servers_test.go). No HTTP server started.
//
// The assertions that matter are not "did it return 200" but:
//
//   - the body carries EXACTLY one key. The fork decodes into a one-field
//     struct and the endpoint is unauthenticated, so any extra field is both a
//     contract change and a potential disclosure. Asserting on the parsed key
//     set is the only check that still fails once someone adds a field.
//   - an unknown user is indistinguishable from an unprovisioned one (200 +
//     pending, never 404), so the endpoint is not a user-existence oracle.
//   - a database fault is a 500, never a synthesised "pending" and absolutely
//     never a synthesised "synced".

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	bindingTestServerID = "test-gitea"
	bindingTestTenant   = "default"
	bindingTestSubject  = "3b0dac64-bc70-4a51-9ac3-2d2473bd65f4"
)

func newBindingStatusRouter(db *gorm.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := NewGitBindingStatusAPI(db)
	r.GET("/api/internal/git-binding/status/:git_server_id/:user_subject_id", api.GetBindingStatus)
	return r
}

// seedBindingServer inserts an enabled gitea server plus its tenant binding.
func seedBindingServer(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now()
	rawInsertServer(t, db, &models.GitServer{
		ServerID:    bindingTestServerID,
		Kind:        models.GitServerKindGitea,
		Endpoint:    "http://127.0.0.1:3001",
		DisplayName: "Test Gitea",
		Config:      `{"admin_token":"tok"}`,
		Enabled:     true,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err := db.Exec(
		`INSERT INTO tenant_git_server_binding (tenant_id, git_server_id, bound_at, updated_at) VALUES (?, ?, ?, ?)`,
		bindingTestTenant, bindingTestServerID, now, now,
	).Error; err != nil {
		t.Fatalf("insert tenant binding: %v", err)
	}
}

func seedUserBinding(t *testing.T, db *gorm.DB, subject, tenant, providerKind, status string) {
	t.Helper()
	now := time.Now()
	if err := db.Exec(
		`INSERT INTO user_git_binding (user_subject_id, tenant_id, git_uid, git_username, provider_kind, sync_status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		subject, tenant, 4, "u-e2es7", providerKind, status, now, now,
	).Error; err != nil {
		t.Fatalf("insert user binding: %v", err)
	}
}

func getBindingStatus(t *testing.T, r *gin.Engine, serverID, subject string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/internal/git-binding/status/"+serverID+"/"+subject, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// assertOnlySyncStatus is the frozen-contract gate: the body must decode to a
// map with the single key "sync_status", carrying the expected value.
func assertOnlySyncStatus(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	if len(body) != 1 {
		t.Fatalf("body must carry exactly one key, got %d: %s", len(body), w.Body.String())
	}
	got, ok := body["sync_status"]
	if !ok {
		t.Fatalf("body missing sync_status: %s", w.Body.String())
	}
	if got != want {
		t.Fatalf("sync_status = %v, want %q", got, want)
	}
}

// Branch 1: a provisioned user. "synced" must be byte-exact — the fork compares
// with == and would reject any other casing.
func TestGetBindingStatus_Synced(t *testing.T) {
	db := setupHandlersDB(t)
	seedBindingServer(t, db)
	seedUserBinding(t, db, bindingTestSubject, bindingTestTenant, models.GitServerKindGitea, models.GitSyncStatusSynced)

	w := getBindingStatus(t, newBindingStatusRouter(db), bindingTestServerID, bindingTestSubject)
	assertOnlySyncStatus(t, w, "synced")

	if raw := w.Body.String(); raw != `{"sync_status":"synced"}` {
		t.Fatalf("wire body = %s, want {\"sync_status\":\"synced\"}", raw)
	}
}

// A row that exists but has not finished provisioning is reported verbatim.
func TestGetBindingStatus_PendingRow(t *testing.T) {
	db := setupHandlersDB(t)
	seedBindingServer(t, db)
	seedUserBinding(t, db, bindingTestSubject, bindingTestTenant, models.GitServerKindGitea, models.GitSyncStatusPending)

	w := getBindingStatus(t, newBindingStatusRouter(db), bindingTestServerID, bindingTestSubject)
	assertOnlySyncStatus(t, w, "pending")
}

func TestGetBindingStatus_ErrorRow(t *testing.T) {
	db := setupHandlersDB(t)
	seedBindingServer(t, db)
	seedUserBinding(t, db, bindingTestSubject, bindingTestTenant, models.GitServerKindGitea, models.GitSyncStatusError)

	w := getBindingStatus(t, newBindingStatusRouter(db), bindingTestServerID, bindingTestSubject)
	assertOnlySyncStatus(t, w, "error")
}

// Branch 2: no binding row at all. Must be 200 + pending, NOT 404 — otherwise
// an unauthenticated caller can enumerate which subject ids exist.
func TestGetBindingStatus_NoBindingIsPendingNotFound(t *testing.T) {
	db := setupHandlersDB(t)
	seedBindingServer(t, db)

	w := getBindingStatus(t, newBindingStatusRouter(db), bindingTestServerID, "00000000-0000-0000-0000-000000000000")
	assertOnlySyncStatus(t, w, "pending")
}

// A binding belonging to a different provider is not this server's business.
func TestGetBindingStatus_ProviderKindMismatchIsPending(t *testing.T) {
	db := setupHandlersDB(t)
	seedBindingServer(t, db)
	seedUserBinding(t, db, bindingTestSubject, bindingTestTenant, "gitlab", models.GitSyncStatusSynced)

	w := getBindingStatus(t, newBindingStatusRouter(db), bindingTestServerID, bindingTestSubject)
	assertOnlySyncStatus(t, w, "pending")
}

// A binding in a tenant that is not bound to this server must not leak across.
func TestGetBindingStatus_OtherTenantIsPending(t *testing.T) {
	db := setupHandlersDB(t)
	seedBindingServer(t, db)
	seedUserBinding(t, db, bindingTestSubject, "some-other-tenant", models.GitServerKindGitea, models.GitSyncStatusSynced)

	w := getBindingStatus(t, newBindingStatusRouter(db), bindingTestServerID, bindingTestSubject)
	assertOnlySyncStatus(t, w, "pending")
}

// Server row exists but nothing is bound to it: no user binding can belong to
// it, so pending — the caller is not at fault, the configuration is.
func TestGetBindingStatus_ServerWithoutTenantBindingIsPending(t *testing.T) {
	db := setupHandlersDB(t)
	now := time.Now()
	rawInsertServer(t, db, &models.GitServer{
		ServerID: bindingTestServerID, Kind: models.GitServerKindGitea,
		Endpoint: "http://127.0.0.1:3001", DisplayName: "Test Gitea",
		Config: `{"admin_token":"tok"}`, Enabled: true, CreatedAt: now, UpdatedAt: now,
	})

	w := getBindingStatus(t, newBindingStatusRouter(db), bindingTestServerID, bindingTestSubject)
	assertOnlySyncStatus(t, w, "pending")
}

// Branch 3: unknown git_server_id is configuration, not user data, so it is
// loud. The body must still disclose nothing about the user.
func TestGetBindingStatus_UnknownServerIs404(t *testing.T) {
	db := setupHandlersDB(t)
	seedBindingServer(t, db)
	seedUserBinding(t, db, bindingTestSubject, bindingTestTenant, models.GitServerKindGitea, models.GitSyncStatusSynced)

	w := getBindingStatus(t, newBindingStatusRouter(db), "no-such-server", bindingTestSubject)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", w.Code, w.Body.String())
	}
	if body := w.Body.String(); bodyHasTopLevelKey(t, body, "sync_status") {
		t.Fatalf("404 body must not carry a status: %s", body)
	}
}

// A disabled server is out of service; vouching for its users would be the
// fail-open reading.
func TestGetBindingStatus_DisabledServerIs404(t *testing.T) {
	db := setupHandlersDB(t)
	now := time.Now()
	rawInsertServer(t, db, &models.GitServer{
		ServerID: bindingTestServerID, Kind: models.GitServerKindGitea,
		Endpoint: "http://127.0.0.1:3001", DisplayName: "Test Gitea",
		Config: `{"admin_token":"tok"}`, Enabled: false, CreatedAt: now, UpdatedAt: now,
	})
	if err := db.Exec(
		`INSERT INTO tenant_git_server_binding (tenant_id, git_server_id, bound_at, updated_at) VALUES (?, ?, ?, ?)`,
		bindingTestTenant, bindingTestServerID, now, now,
	).Error; err != nil {
		t.Fatalf("insert tenant binding: %v", err)
	}
	seedUserBinding(t, db, bindingTestSubject, bindingTestTenant, models.GitServerKindGitea, models.GitSyncStatusSynced)

	w := getBindingStatus(t, newBindingStatusRouter(db), bindingTestServerID, bindingTestSubject)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", w.Code, w.Body.String())
	}
}

// Branch 4: a database fault must fail closed with a 500. The fork rejects on
// any non-200, so the user is denied — which is the point. Synthesising
// "pending" here would hide an outage; synthesising "synced" would admit
// unprovisioned users during one.
func TestGetBindingStatus_BindingQueryErrorIs500(t *testing.T) {
	db := setupHandlersDB(t)
	seedBindingServer(t, db)
	if err := db.Exec(`DROP TABLE user_git_binding`).Error; err != nil {
		t.Fatalf("drop table: %v", err)
	}

	w := getBindingStatus(t, newBindingStatusRouter(db), bindingTestServerID, bindingTestSubject)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
	if body := w.Body.String(); bodyHasTopLevelKey(t, body, "sync_status") {
		t.Fatalf("500 body must not carry a status: %s", body)
	}
}

// The same fail-closed rule applies when the server lookup itself fails: a
// missing row is 404, but a broken table is 500, and the two must not collapse.
func TestGetBindingStatus_ServerQueryErrorIs500(t *testing.T) {
	db := setupHandlersDB(t)
	seedBindingServer(t, db)
	if err := db.Exec(`DROP TABLE git_servers`).Error; err != nil {
		t.Fatalf("drop table: %v", err)
	}

	w := getBindingStatus(t, newBindingStatusRouter(db), bindingTestServerID, bindingTestSubject)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
}

func TestGetBindingStatus_TenantQueryErrorIs500(t *testing.T) {
	db := setupHandlersDB(t)
	seedBindingServer(t, db)
	if err := db.Exec(`DROP TABLE tenant_git_server_binding`).Error; err != nil {
		t.Fatalf("drop table: %v", err)
	}

	w := getBindingStatus(t, newBindingStatusRouter(db), bindingTestServerID, bindingTestSubject)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
}

// A nil pool must not panic into the fork's 3s window; it fails closed.
func TestGetBindingStatus_NilDBIs500(t *testing.T) {
	w := getBindingStatus(t, newBindingStatusRouter(nil), bindingTestServerID, bindingTestSubject)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
}

// Cache-Control keeps an intermediary from pinning a "pending" answer for
// longer than the fork's own cache already does.
func TestGetBindingStatus_SetsNoStore(t *testing.T) {
	db := setupHandlersDB(t)
	seedBindingServer(t, db)
	seedUserBinding(t, db, bindingTestSubject, bindingTestTenant, models.GitServerKindGitea, models.GitSyncStatusSynced)

	w := getBindingStatus(t, newBindingStatusRouter(db), bindingTestServerID, bindingTestSubject)
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestResolveGitBindingStatus(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"no rows", nil, "pending"},
		{"single synced", []string{"synced"}, "synced"},
		{"single pending", []string{"pending"}, "pending"},
		{"single error", []string{"error"}, "error"},
		{"synced wins over pending", []string{"pending", "synced"}, "synced"},
		{"synced wins over error", []string{"error", "synced"}, "synced"},
		{"first reported when none synced", []string{"error", "pending"}, "error"},
		{"empty stored value is pending", []string{""}, "pending"},
		{"unknown value mirrored verbatim", []string{"draining"}, "draining"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveGitBindingStatus(tc.in); got != tc.want {
				t.Fatalf("resolveGitBindingStatus(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// bodyHasTopLevelKey reports whether a JSON object body has the given top-level
// key. Deliberately structural rather than a substring match: the point is that
// no error path ever emits a sync_status field, and a substring check would be
// satisfied by the word appearing anywhere.
func bodyHasTopLevelKey(t *testing.T, body, key string) bool {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
