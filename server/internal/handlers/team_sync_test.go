package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/gin-gonic/gin"
)

// fakeTeamSyncService is a test double for TeamSyncService. Returns
// canned result/err per call; records the tenant_id + team_id it was
// called with.
type fakeTeamSyncService struct {
	result       *gitsync.SyncResult
	err          error
	called       bool
	calledTenant string
	calledTeam   string
}

func (f *fakeTeamSyncService) SyncTeam(ctx context.Context, tenantID, teamID string) (*gitsync.SyncResult, error) {
	f.called = true
	f.calledTenant = tenantID
	f.calledTeam = teamID
	return f.result, f.err
}

func newTeamSyncTestRouter(svc TeamSyncService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	teamSyncService = svc

	r := gin.New()
	r.POST("/api/admin/tenants/:tenant_id/teams/:team_id/sync", SyncTeam)
	return r
}

func doTeamSync(t *testing.T, r *gin.Engine, tenantID, teamID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/tenants/"+tenantID+"/teams/"+teamID+"/sync", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestSyncTeam_HappyReturns200WithResult(t *testing.T) {
	svc := &fakeTeamSyncService{result: &gitsync.SyncResult{
		TeamID:      "team-a",
		GiteaTeamID: 42,
		Added:       []string{"alice"},
		Removed:     []string{"bob"},
		Skipped:     []string{"carol"},
	}}
	r := newTeamSyncTestRouter(svc)

	w := doTeamSync(t, r, "t-acme", "team-a")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !svc.called || svc.calledTeam != "team-a" || svc.calledTenant != "t-acme" {
		t.Errorf("expected service called with (t-acme, team-a), got called=%v tenant=%q team=%q",
			svc.called, svc.calledTenant, svc.calledTeam)
	}
	if !contains(w.Body.String(), "alice") || !contains(w.Body.String(), "bob") {
		t.Errorf("response body missing expected members: %s", w.Body.String())
	}
}

func TestSyncTeam_ErrTeamNotFoundReturns404(t *testing.T) {
	svc := &fakeTeamSyncService{err: gitsync.ErrTeamNotFound}
	r := newTeamSyncTestRouter(svc)

	w := doTeamSync(t, r, "t-acme", "nonexistent")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestSyncTeam_ErrGiteaUnreachableReturns502(t *testing.T) {
	svc := &fakeTeamSyncService{err: gitsync.ErrGiteaUnreachable}
	r := newTeamSyncTestRouter(svc)

	w := doTeamSync(t, r, "t-acme", "team-a")
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", w.Code)
	}
}

func TestSyncTeam_ErrGiteaUnauthorizedReturns502(t *testing.T) {
	// 502 not 401 — config error (wrong admin token), not caller's JWT.
	svc := &fakeTeamSyncService{err: gitsync.ErrGiteaUnauthorized}
	r := newTeamSyncTestRouter(svc)

	w := doTeamSync(t, r, "t-acme", "team-a")
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 for config error, got %d", w.Code)
	}
}

func TestSyncTeam_NilServiceReturns503(t *testing.T) {
	r := newTeamSyncTestRouter(nil)

	w := doTeamSync(t, r, "t-acme", "team-a")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when service is nil, got %d", w.Code)
	}
}

func TestSyncTeam_GiteaTeamNotFoundReturns404(t *testing.T) {
	svc := &fakeTeamSyncService{err: gitsync.ErrGiteaTeamNotFound}
	r := newTeamSyncTestRouter(svc)

	w := doTeamSync(t, r, "t-acme", "team-a")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for ErrGiteaTeamNotFound, got %d", w.Code)
	}
}

func TestSyncTeam_OtherErrorReturns500(t *testing.T) {
	svc := &fakeTeamSyncService{err: errors.New("unexpected")}
	r := newTeamSyncTestRouter(svc)

	w := doTeamSync(t, r, "t-acme", "team-a")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for unknown error, got %d", w.Code)
	}
}

// TestSyncTeam_EmptyTenantIDReturns400 covers the new input guard
// added in Phase E3b.1.1 — :tenant_id is required in the path.
func TestSyncTeam_EmptyTenantIDReturns400(t *testing.T) {
	svc := &fakeTeamSyncService{}
	r := newTeamSyncTestRouter(svc)

	// Use a literal URL since httptest.NewRequest won't substitute empty
	// path segments cleanly — we POST to a path with no tenant_id.
	req := httptest.NewRequest(http.MethodPost, "/api/admin/tenants//teams/team-a/sync", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty tenant_id, got %d body=%s", w.Code, w.Body.String())
	}
	if svc.called {
		t.Errorf("expected service NOT called when tenant_id empty")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
