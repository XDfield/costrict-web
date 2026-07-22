package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/userteams"
	"github.com/gin-gonic/gin"
)

type stubUserTeamsSvc struct {
	teams []userteams.TeamSummary
	err   error
}

func (s *stubUserTeamsSvc) ListUserTeams(ctx context.Context, subjectID string) ([]userteams.TeamSummary, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.teams, nil
}

func newTestUserTeamsRouter(svc UserTeamsService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := &UserTeamsAPI{Svc: svc}
	r.GET("/api/internal/users/:subject_id/teams", api.ListUserTeams)
	return r
}

func doGet(t *testing.T, r *gin.Engine, subjectID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/api/internal/users/"+subjectID+"/teams", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestListUserTeams_NotIntegrated_Returns503(t *testing.T) {
	r := newTestUserTeamsRouter(&stubUserTeamsSvc{err: userteams.ErrOrgTeamServiceNotIntegrated})
	w := doGet(t, r, "user-1")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503; body=%s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["error_code"] != "ORG_TEAM_SERVICE_UNAVAILABLE" {
		t.Errorf("error_code: got %q, want ORG_TEAM_SERVICE_UNAVAILABLE", body["error_code"])
	}
}

func TestListUserTeams_HappyPath(t *testing.T) {
	// Once org-team-service lands, this is the success shape callers expect.
	teams := []userteams.TeamSummary{
		{TeamID: "tid-1", DisplayName: "Platform", Role: "owner"},
		{TeamID: "tid-2", DisplayName: "Mobile", Role: "member"},
	}
	r := newTestUserTeamsRouter(&stubUserTeamsSvc{teams: teams})
	w := doGet(t, r, "user-1")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Teams []userteams.TeamSummary `json:"teams"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Teams) != 2 {
		t.Fatalf("teams len: got %d, want 2", len(body.Teams))
	}
	if body.Teams[0].TeamID != "tid-1" {
		t.Errorf("team[0].team_id: got %q, want tid-1", body.Teams[0].TeamID)
	}
}

func TestListUserTeams_EmptySubjectID_Returns400(t *testing.T) {
	// subject_id="" can't be routed via /:subject_id/teams; the router
	// would 404. But the empty-subjectID path is exercised by the service
	// when subject_id comes in as a query/body elsewhere — keep the test
	// for the handler's defensive guard via a direct call.
	api := &UserTeamsAPI{Svc: &stubUserTeamsSvc{}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Params = gin.Params{}
	api.ListUserTeams(c)
	if w := c.Writer.Status(); w != http.StatusBadRequest {
		t.Errorf("got %d, want 400", w)
	}
}

func TestListUserTeams_EmptyList_Returns200(t *testing.T) {
	// Empty list (not error) means "user belongs to no team" — @server
	// maps this to 403 NO_TEAM_MEMBERSHIP.
	r := newTestUserTeamsRouter(&stubUserTeamsSvc{teams: []userteams.TeamSummary{}})
	w := doGet(t, r, "user-1")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Teams []userteams.TeamSummary `json:"teams"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Teams == nil || len(body.Teams) != 0 {
		t.Errorf("expected non-nil empty slice, got %+v", body.Teams)
	}
}

func TestListUserTeams_ServiceGenericError_Returns500(t *testing.T) {
	r := newTestUserTeamsRouter(&stubUserTeamsSvc{err: errors.New("boom")})
	w := doGet(t, r, "user-1")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("got %d, want 500", w.Code)
	}
}
