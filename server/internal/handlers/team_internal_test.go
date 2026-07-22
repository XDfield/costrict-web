package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/crypto"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/teamns"
	"github.com/costrict/costrict-web/server/internal/tenant"
	"github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupTeamnsDB mirrors teamns.setupDB but lives here so handler tests
// don't depend on the teamns package's test files.
func setupTeamnsDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := db.Exec(`CREATE TABLE team_ns (
		team_id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL,
		team_display_name TEXT NOT NULL,
		team_ns_org TEXT NOT NULL UNIQUE,
		team_short TEXT NOT NULL,
		git_server_id TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'active',
		dissolved_at DATETIME,
		dissolution_reason TEXT,
		retention_until DATETIME,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create team_ns: %v", err)
	}
	if err := db.Exec(`CREATE TABLE team_bot_credentials (
		team_id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL,
		git_server_id TEXT NOT NULL,
		gitea_username TEXT NOT NULL,
		gitea_user_id INTEGER NOT NULL,
		gitea_token_id INTEGER NOT NULL,
		token_encrypted TEXT NOT NULL,
		token_sha256 TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		rotated_at DATETIME,
		revoked_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create team_bot_credentials: %v", err)
	}
	return db
}

func mustAESHandler(t *testing.T) *crypto.AESGCM {
	t.Helper()
	key, err := crypto.DecodeBase64Key("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	a, err := crypto.NewAESGCM(key)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	return a
}

// newTeamInternalRouter wires all 7 routes to the supplied teamns.Service.
// A nil svc emulates the feature-disabled state. The package-global
// teamnsService is reset to nil after each test via t.Cleanup so it can't
// leak into unrelated tests in the same package.
func newTeamInternalRouter(t *testing.T, svc *teamns.Service) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	teamnsService = svc
	t.Cleanup(func() { teamnsService = nil })
	r := gin.New()
	r.POST("/api/internal/teams", CreateTeam)
	r.GET("/api/internal/teams/:team_id", GetTeam)
	r.GET("/api/internal/teams", ListTeams)
	r.PATCH("/api/internal/teams/:team_id", PatchTeam)
	r.POST("/api/internal/teams/:team_id/members:sync", SyncTeamMembers)
	r.POST("/api/internal/teams/:team_id/dissolve", DissolveTeam)
	r.POST("/api/internal/teams/:team_id/bot-token:rotate", RotateBotToken)
	return r
}

// withTenant middleware injects the tenant_id into the request ctx so
// teamns.Service picks it up.
func withTenantMiddleware(tenantID string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := tenant.WithTenantID(c.Request.Context(), tenantID)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func doJSONWithTenant(t *testing.T, r *gin.Engine, method, path, tenantID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	// Wrap router with tenant middleware for this request.
	r2 := gin.New()
	r2.Use(withTenantMiddleware(tenantID))
	r2.NoRoute(func(c *gin.Context) {
		// Re-dispatch to r via ServeHTTP on the wrapped writer — the test
		// router doesn't have the tenant middleware, but we need ctx propagation.
		r.ServeHTTP(c.Writer, c.Request)
	})
	// Simpler: just call r directly with a ctx that already has tenant.
	ctx := tenant.WithTenantID(context.Background(), tenantID)
	req = req.WithContext(ctx)
	r.ServeHTTP(w, req)
	return w
}

// ---- 503 disabled tests ----

func TestHandlers_DisabledReturns503(t *testing.T) {
	r := newTeamInternalRouter(t, nil) // nil service
	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/internal/teams", teamns.CreateTeamRequest{}},
		{http.MethodGet, "/api/internal/teams/abc", nil},
		{http.MethodGet, "/api/internal/teams", nil},
		{http.MethodPatch, "/api/internal/teams/abc", teamns.PatchTeamRequest{}},
		{http.MethodPost, "/api/internal/teams/abc/members:sync", teamns.SyncMembersRequest{}},
		{http.MethodPost, "/api/internal/teams/abc/dissolve", teamns.DissolveTeamRequest{}},
		{http.MethodPost, "/api/internal/teams/abc/bot-token:rotate", teamns.RotateBotTokenRequest{}},
	}
	for i, tc := range cases {
		w := doJSONWithTenant(t, r, tc.method, tc.path, tenant.DefaultTenantID, tc.body)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("case %d %s %s: got %d, want 503; body=%s", i, tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

// ---- mapTeamnsError coverage ----

func TestMapTeamnsError_Coverage(t *testing.T) {
	cases := []struct {
		err      error
		wantCode int
		wantTag  string
	}{
		{teamns.ErrInvalidRequest, http.StatusBadRequest, "INVALID_REQUEST"},
		{teamns.ErrTeamNotFound, http.StatusNotFound, "TEAM_NOT_FOUND"},
		{teamns.ErrTeamIDTaken, http.StatusConflict, "TEAM_ID_TAKEN"},
		{teamns.ErrTeamArchived, http.StatusGone, "TEAM_ARCHIVED"},
		{teamns.ErrMemberUnresolved, http.StatusNotFound, "MEMBER_USER_NOT_FOUND"},
		{teamns.ErrBotUsernameTaken, http.StatusConflict, "BOT_USERNAME_TAKEN"},
		{teamns.ErrTenantGitServerUnresolved, http.StatusPreconditionFailed, "TENANT_GIT_SERVER_UNRESOLVED"},
		{errors.New("random"), http.StatusInternalServerError, ""},
	}
	for i, c := range cases {
		status, body := mapTeamnsError(c.err)
		if status != c.wantCode {
			t.Errorf("case %d: status got %d want %d", i, status, c.wantCode)
		}
		if c.wantTag != "" {
			code, _ := body["error_code"].(string)
			if code != c.wantTag {
				t.Errorf("case %d: error_code got %q want %q", i, code, c.wantTag)
			}
		}
	}
}

// ---- GetTeam happy path ----

func TestGetTeam_HappyReturns200(t *testing.T) {
	db := setupTeamnsDB(t)
	now := time.Now().UTC()
	teamID := padUUIDHandler(1)
	ns := models.TeamNamespace{
		TeamID: teamID, TenantID: tenant.DefaultTenantID,
		TeamDisplayName: "Platform", TeamNSOrg: "t-" + padUUIDHandler(1)[:8],
		TeamShort: padUUIDHandler(1)[:8], GitServerID: "gs-1",
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&ns).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newTeamInternalRouter(t, svc)

	w := doJSONWithTenant(t, r, http.MethodGet, "/api/internal/teams/"+teamID, tenant.DefaultTenantID, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d body=%s", w.Code, w.Body.String())
	}
	var got teamns.GetTeamResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TeamID != teamID {
		t.Errorf("team_id: got %q", got.TeamID)
	}
}

// ---- GetTeam not found → 404 ----

func TestGetTeam_NotFoundReturns404(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newTeamInternalRouter(t, svc)
	w := doJSONWithTenant(t, r, http.MethodGet, "/api/internal/teams/"+padUUIDHandler(2), tenant.DefaultTenantID, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// ---- CreateTeam bad body → 400 ----

func TestCreateTeam_BadJSONReturns400(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newTeamInternalRouter(t, svc)
	req := httptest.NewRequest(http.MethodPost, "/api/internal/teams", strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(tenant.WithTenantID(context.Background(), tenant.DefaultTenantID))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// ---- CreateTeam validation failure → 400 ----

func TestCreateTeam_MissingFieldsReturns400(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newTeamInternalRouter(t, svc)
	body := teamns.CreateTeamRequest{TeamID: "not-a-uuid", TeamDisplayName: "X"}
	w := doJSONWithTenant(t, r, http.MethodPost, "/api/internal/teams", tenant.DefaultTenantID, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "INVALID_REQUEST") {
		t.Errorf("expected error_code=INVALID_REQUEST in body: %s", w.Body.String())
	}
}

// ---- ListTeams rejects tenant_id query → 400 ----

func TestListTeams_TenantIDQueryRejected(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newTeamInternalRouter(t, svc)
	w := doJSONWithTenant(t, r, http.MethodGet, "/api/internal/teams?tenant_id=default", tenant.DefaultTenantID, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// ---- ListTeams happy path ----

func TestListTeams_Pagination(t *testing.T) {
	db := setupTeamnsDB(t)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		teamID := padUUIDHandler(i)
		ns := models.TeamNamespace{
			TeamID: teamID, TenantID: tenant.DefaultTenantID,
			TeamDisplayName: "T" + string(rune('a'+i)),
			TeamNSOrg:       "t-" + string(rune('a'+i)), TeamShort: string(rune('a' + i)),
			GitServerID: "gs", Status: "active", CreatedAt: now, UpdatedAt: now,
		}
		if err := db.Create(&ns).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newTeamInternalRouter(t, svc)
	w := doJSONWithTenant(t, r, http.MethodGet, "/api/internal/teams?page=1&page_size=2", tenant.DefaultTenantID, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", w.Code, w.Body.String())
	}
	var got teamns.ListResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Total != 3 || len(got.Teams) != 2 {
		t.Errorf("got total=%d teams=%d; want total=3 teams=2", got.Total, len(got.Teams))
	}
}

// ---- PatchTeam bad body → 400 ----

func TestPatchTeam_EmptyBodyReturns400(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newTeamInternalRouter(t, svc)
	w := doJSONWithTenant(t, r, http.MethodPatch, "/api/internal/teams/"+padUUIDHandler(3), tenant.DefaultTenantID, teamns.PatchTeamRequest{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// ---- DissolveTeam missing reason → 400 ----

func TestDissolveTeam_MissingReasonReturns400(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newTeamInternalRouter(t, svc)
	w := doJSONWithTenant(t, r, http.MethodPost, "/api/internal/teams/"+padUUIDHandler(4)+"/dissolve", tenant.DefaultTenantID, teamns.DissolveTeamRequest{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// ---- RotateBotToken missing reason → 400 ----

func TestRotateBotToken_MissingReasonReturns400(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newTeamInternalRouter(t, svc)
	w := doJSONWithTenant(t, r, http.MethodPost, "/api/internal/teams/"+padUUIDHandler(5)+"/bot-token:rotate", tenant.DefaultTenantID, teamns.RotateBotTokenRequest{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// ---- SyncTeamMembers bad mode → 400 ----

func TestSyncTeamMembers_BadModeReturns400(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newTeamInternalRouter(t, svc)
	body := teamns.SyncMembersRequest{Mode: "garbage"}
	w := doJSONWithTenant(t, r, http.MethodPost, "/api/internal/teams/"+padUUIDHandler(6)+"/members:sync", tenant.DefaultTenantID, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// ---- DissolveTeam idempotent on never-created team ----

func TestDissolveTeam_NeverCreatedReturns200(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newTeamInternalRouter(t, svc)
	body := teamns.DissolveTeamRequest{Reason: "test", Actor: user.UserRef{EmployeeNumber: "E-1"}}
	w := doJSONWithTenant(t, r, http.MethodPost, "/api/internal/teams/"+padUUIDHandler(7)+"/dissolve", tenant.DefaultTenantID, body)
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// padUUIDHandler produces a deterministic UUID-shaped string for tests.
func padUUIDHandler(n int) string {
	hex := "0123456789abcdef"
	d := string(hex[n%len(hex)])
	return d + d + d + d + d + d + d + d + "-" +
		d + d + d + d + "-" +
		"4" + d + d + d + "-" +
		"8" + d + d + d + "-" +
		d + d + d + d + d + d + d + d + d + d + d + d
}
