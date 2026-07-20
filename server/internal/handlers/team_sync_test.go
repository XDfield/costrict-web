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
// canned result/err per call; records the team_id it was called with.
type fakeTeamSyncService struct {
	result   *gitsync.SyncResult
	err      error
	calledID string
	called   bool
}

func (f *fakeTeamSyncService) SyncTeam(ctx context.Context, teamID string) (*gitsync.SyncResult, error) {
	f.called = true
	f.calledID = teamID
	return f.result, f.err
}

func newTeamSyncTestRouter(svc TeamSyncService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	teamSyncService = svc

	r := gin.New()
	r.POST("/api/admin/teams/:team_id/sync", SyncTeam)
	return r
}

func doTeamSync(t *testing.T, r *gin.Engine, teamID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/teams/"+teamID+"/sync", nil)
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

	w := doTeamSync(t, r, "team-a")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !svc.called || svc.calledID != "team-a" {
		t.Errorf("expected service called with team-a, got called=%v id=%q", svc.called, svc.calledID)
	}
	if !contains(w.Body.String(), "alice") || !contains(w.Body.String(), "bob") {
		t.Errorf("response body missing expected members: %s", w.Body.String())
	}
}

func TestSyncTeam_ErrTeamNotFoundReturns404(t *testing.T) {
	svc := &fakeTeamSyncService{err: gitsync.ErrTeamNotFound}
	r := newTeamSyncTestRouter(svc)

	w := doTeamSync(t, r, "nonexistent")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestSyncTeam_ErrGiteaUnreachableReturns502(t *testing.T) {
	svc := &fakeTeamSyncService{err: gitsync.ErrGiteaUnreachable}
	r := newTeamSyncTestRouter(svc)

	w := doTeamSync(t, r, "team-a")
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", w.Code)
	}
}

func TestSyncTeam_ErrGiteaUnauthorizedReturns502(t *testing.T) {
	// 502 not 401 — config error (wrong admin token), not caller's JWT.
	svc := &fakeTeamSyncService{err: gitsync.ErrGiteaUnauthorized}
	r := newTeamSyncTestRouter(svc)

	w := doTeamSync(t, r, "team-a")
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 for config error, got %d", w.Code)
	}
}

func TestSyncTeam_NilServiceReturns503(t *testing.T) {
	r := newTeamSyncTestRouter(nil)

	w := doTeamSync(t, r, "team-a")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when service is nil, got %d", w.Code)
	}
}

func TestSyncTeam_GiteaTeamNotFoundReturns404(t *testing.T) {
	svc := &fakeTeamSyncService{err: gitsync.ErrGiteaTeamNotFound}
	r := newTeamSyncTestRouter(svc)

	w := doTeamSync(t, r, "team-a")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for ErrGiteaTeamNotFound, got %d", w.Code)
	}
}

func TestSyncTeam_OtherErrorReturns500(t *testing.T) {
	svc := &fakeTeamSyncService{err: errors.New("unexpected")}
	r := newTeamSyncTestRouter(svc)

	w := doTeamSync(t, r, "team-a")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for unknown error, got %d", w.Code)
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
