package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/teamns"
	"github.com/costrict/costrict-web/server/internal/tenant"
	"github.com/gin-gonic/gin"
)

// stubTeamResolver is a test double for the TeamResolver interface.
type stubTeamResolver struct {
	teams []TeamSummary
	err   error
}

func (s *stubTeamResolver) ResolveCurrentUserTeams(c *gin.Context, subjectID string) ([]TeamSummary, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.teams, nil
}

func newKBEnsureRouter(t *testing.T, svc *teamns.Service, resolver TeamResolver) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	teamnsService = svc
	t.Cleanup(func() { teamnsService = nil })
	teamResolver = resolver
	t.Cleanup(func() { teamResolver = nil })
	r := gin.New()
	// Mimic RequireAuth: tests set UserIDKey explicitly via a small middleware.
	r.Use(func(c *gin.Context) {
		if uid := c.GetHeader("X-Test-Subject"); uid != "" {
			c.Set(middleware.UserIDKey, uid)
		}
		c.Next()
	})
	r.POST("/api/kb/ensure", KBEnsure)
	return r
}

func doKBEnsure(t *testing.T, r *gin.Engine, subject string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/kb/ensure", nil)
	req.Header.Set("Content-Type", "application/json")
	if subject != "" {
		req.Header.Set("X-Test-Subject", subject)
	}
	req = req.WithContext(tenant.WithTenantID(context.Background(), tenant.DefaultTenantID))
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		req.Body = nil
		req.Body = newBytesReader(t, buf)
		req.ContentLength = int64(len(buf))
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func newBytesReader(t *testing.T, b []byte) *readerOfBytes {
	t.Helper()
	return &readerOfBytes{b: b}
}

type readerOfBytes struct {
	b   []byte
	pos int
}

func (r *readerOfBytes) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, errEOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

func (r *readerOfBytes) Close() error { return nil }

var errEOF = errEOFReal{}

type errEOFReal struct{}

func (errEOFReal) Error() string { return "EOF" }

func seedTeamForKB(t *testing.T, db interface{}, teamID, encToken, sha, giteaUsername string) {
	// helper no-op — actual seeding done inline below per test
}

func TestKBEnsure_ResolverNil_Returns503(t *testing.T) {
	// teamResolver nil → 503 ORG_TEAM_SERVICE_UNAVAILABLE
	// (teamnsService must be non-nil to avoid the feature-disabled 503 path
	// which returns a different error_code).
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	gin.SetMode(gin.TestMode)
	teamnsService = svc
	t.Cleanup(func() { teamnsService = nil })
	teamResolver = nil
	t.Cleanup(func() { teamResolver = nil })
	r := gin.New()
	r.POST("/api/kb/ensure", KBEnsure)

	body := KBEnsureRequest{CodeRepoURL: "https://github.com/o/p.git"}
	w := doKBEnsure(t, r, "user-1", body)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503; body=%s", w.Code, w.Body.String())
	}
	if !containsStr(w.Body.String(), "ORG_TEAM_SERVICE_UNAVAILABLE") {
		t.Errorf("expected error_code ORG_TEAM_SERVICE_UNAVAILABLE: %s", w.Body.String())
	}
}

func TestKBEnsure_BadJSONReturns400(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newKBEnsureRouter(t, svc, &stubTeamResolver{})

	req := httptest.NewRequest(http.MethodPost, "/api/kb/ensure", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Subject", "user-1")
	req = req.WithContext(tenant.WithTenantID(context.Background(), tenant.DefaultTenantID))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestKBEnsure_MissingCodeRepoURL400(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newKBEnsureRouter(t, svc, &stubTeamResolver{})

	body := KBEnsureRequest{CodeRepoURL: ""}
	w := doKBEnsure(t, r, "user-1", body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestKBEnsure_ZeroTeams_Returns403(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newKBEnsureRouter(t, svc, &stubTeamResolver{teams: []TeamSummary{}})

	body := KBEnsureRequest{CodeRepoURL: "https://github.com/o/p.git"}
	w := doKBEnsure(t, r, "user-1", body)
	if w.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !containsStr(w.Body.String(), "NO_TEAM_MEMBERSHIP") {
		t.Errorf("expected NO_TEAM_MEMBERSHIP: %s", w.Body.String())
	}
}

func TestKBEnsure_MultiTeams_Returns409(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	teams := []TeamSummary{
		{TeamID: padUUIDHandler(7), DisplayName: "Platform", Role: "owner"},
		{TeamID: padUUIDHandler(8), DisplayName: "Mobile", Role: "member"},
	}
	r := newKBEnsureRouter(t, svc, &stubTeamResolver{teams: teams})

	body := KBEnsureRequest{CodeRepoURL: "https://github.com/o/p.git"}
	w := doKBEnsure(t, r, "user-1", body)
	if w.Code != http.StatusConflict {
		t.Errorf("got %d, want 409; body=%s", w.Code, w.Body.String())
	}
	if !containsStr(w.Body.String(), "TEAM_DISAMBIGUATION_REQUIRED") {
		t.Errorf("expected TEAM_DISAMBIGUATION_REQUIRED: %s", w.Body.String())
	}
	// Body should include both teams.
	if !containsStr(w.Body.String(), padUUIDHandler(7)) || !containsStr(w.Body.String(), padUUIDHandler(8)) {
		t.Errorf("expected both team_ids in body: %s", w.Body.String())
	}
}

func TestKBEnsure_ExplicitTeamID_NotMember_Returns403(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	teams := []TeamSummary{
		{TeamID: padUUIDHandler(7), DisplayName: "Platform", Role: "owner"},
	}
	r := newKBEnsureRouter(t, svc, &stubTeamResolver{teams: teams})

	body := KBEnsureRequest{
		CodeRepoURL: "https://github.com/o/p.git",
		TeamID:      padUUIDHandler(99), // not in user's team list
	}
	w := doKBEnsure(t, r, "user-1", body)
	if w.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !containsStr(w.Body.String(), "TEAM_MEMBERSHIP_REQUIRED") {
		t.Errorf("expected TEAM_MEMBERSHIP_REQUIRED: %s", w.Body.String())
	}
}

func TestKBEnsure_SingleTeam_HappyPath(t *testing.T) {
	db := setupTeamnsDB(t)
	aes := mustAESHandler(t)
	plaintext := "pat-XYZ"
	enc, err := aes.Seal([]byte(plaintext))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	now := time.Now().UTC()
	teamID := padUUIDHandler(3)
	short := teamID[:8]
	if err := db.Create(&models.TeamNamespace{
		TeamID: teamID, TenantID: tenant.DefaultTenantID,
		TeamDisplayName: "Platform", TeamNSOrg: "t-" + short,
		TeamShort: short, GitServerID: "gs-1",
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed ns: %v", err)
	}
	if err := db.Create(&models.TeamBotCredentials{
		TeamID: teamID, TenantID: tenant.DefaultTenantID, GitServerID: "gs-1",
		GiteaUsername: "bot-t-" + short, GiteaUserID: 42, GiteaTokenID: 17,
		TokenEncrypted: enc, TokenSHA256: "sha", CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed creds: %v", err)
	}

	fake := &fakeGitServerHandler{}
	svc := teamns.NewService(db, nil, nil, aes, nil)
	svc.SetGitServerFactoryForTest(func(ctx context.Context, tenantID string) (gitsync.GitServer, error) {
		return fake, nil
	})
	teams := []TeamSummary{{TeamID: teamID, DisplayName: "Platform", Role: "owner"}}
	r := newKBEnsureRouter(t, svc, &stubTeamResolver{teams: teams})

	body := KBEnsureRequest{CodeRepoURL: "https://github.com/ownerA/proj.git"}
	w := doKBEnsure(t, r, "user-1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", w.Code, w.Body.String())
	}
	var got KBEnsureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantPath := "t-" + short + "/kb-github.com__ownera__proj"
	if got.KbRepoPath != wantPath {
		t.Errorf("kb_repo_path: got %q, want %q", got.KbRepoPath, wantPath)
	}
	if !got.Created.KbRepo {
		t.Errorf("expected Created.KbRepo=true")
	}
	if got.TeamID != teamID {
		t.Errorf("team_id: got %q, want %q", got.TeamID, teamID)
	}
	if got.TeamResolution != "implicit_single" {
		t.Errorf("team_resolution: got %q, want implicit_single", got.TeamResolution)
	}
	if got.BotCredentials == nil || got.BotCredentials.Token != plaintext {
		t.Errorf("bot creds: %+v", got.BotCredentials)
	}
	// Only main protection (no inst-* glob for kb). Workflow uses 2 calls;
	// kb uses 1.
	if len(fake.setBranchProtectionCalls) != 1 {
		t.Errorf("setBranchProtectionCalls: got %d, want 1", len(fake.setBranchProtectionCalls))
	} else if fake.setBranchProtectionCalls[0].Opts.RuleName != "main" {
		t.Errorf("rule name: got %q, want main", fake.setBranchProtectionCalls[0].Opts.RuleName)
	}
	// KB must not touch snapshot file or instance branch.
	if len(fake.writeFileCalls) != 0 || len(fake.createBranchCalls) != 0 {
		t.Errorf("kb should not write snapshot / instance branch: write=%d createBranch=%d",
			len(fake.writeFileCalls), len(fake.createBranchCalls))
	}
}

func TestKBEnsure_ExplicitTeamID_Member_HappyPath(t *testing.T) {
	db := setupTeamnsDB(t)
	aes := mustAESHandler(t)
	plaintext := "pat-XYZ"
	enc, err := aes.Seal([]byte(plaintext))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	now := time.Now().UTC()
	teamID := padUUIDHandler(10)
	short := teamID[:8]
	if err := db.Create(&models.TeamNamespace{
		TeamID: teamID, TenantID: tenant.DefaultTenantID,
		TeamDisplayName: "Mobile", TeamNSOrg: "t-" + short,
		TeamShort: short, GitServerID: "gs-1",
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed ns: %v", err)
	}
	if err := db.Create(&models.TeamBotCredentials{
		TeamID: teamID, TenantID: tenant.DefaultTenantID, GitServerID: "gs-1",
		GiteaUsername: "bot-t-" + short, GiteaUserID: 43, GiteaTokenID: 18,
		TokenEncrypted: enc, TokenSHA256: "sha", CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed creds: %v", err)
	}

	fake := &fakeGitServerHandler{}
	svc := teamns.NewService(db, nil, nil, aes, nil)
	svc.SetGitServerFactoryForTest(func(ctx context.Context, tenantID string) (gitsync.GitServer, error) {
		return fake, nil
	})
	// Multi-team user explicitly chooses teamID.
	teams := []TeamSummary{
		{TeamID: teamID, DisplayName: "Mobile", Role: "owner"},
		{TeamID: padUUIDHandler(11), DisplayName: "Other", Role: "member"},
	}
	r := newKBEnsureRouter(t, svc, &stubTeamResolver{teams: teams})

	body := KBEnsureRequest{
		CodeRepoURL: "https://github.com/o/p.git",
		TeamID:      teamID,
	}
	w := doKBEnsure(t, r, "user-1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", w.Code, w.Body.String())
	}
	var got KBEnsureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TeamResolution != "explicit" {
		t.Errorf("team_resolution: got %q, want explicit", got.TeamResolution)
	}
	if got.TeamID != teamID {
		t.Errorf("team_id: got %q, want %q", got.TeamID, teamID)
	}
}

func TestKBEnsure_ResolverError_Returns503(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newKBEnsureRouter(t, svc, &stubTeamResolver{err: errOrgTeamServiceUnavailableTest("rpc down")})

	body := KBEnsureRequest{CodeRepoURL: "https://github.com/o/p.git"}
	w := doKBEnsure(t, r, "user-1", body)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503; body=%s", w.Code, w.Body.String())
	}
	if !containsStr(w.Body.String(), "ORG_TEAM_SERVICE_UNAVAILABLE") {
		t.Errorf("expected ORG_TEAM_SERVICE_UNAVAILABLE: %s", w.Body.String())
	}
}

// errOrgTeamServiceUnavailableTest is a sentinel used to simulate the
// resolver blowing up; tests check the handler maps it to 503.
type errOrgTeamServiceUnavailableTest string

func (e errOrgTeamServiceUnavailableTest) Error() string { return string(e) }

func TestKBEnsure_TeamNSMissing_Returns412(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	// No team_ns seeded. Single-team resolver returns a team_id, but lookup
	// in DB will fail → 412.
	teamID := padUUIDHandler(12)
	teams := []TeamSummary{{TeamID: teamID, DisplayName: "Ghost", Role: "owner"}}
	r := newKBEnsureRouter(t, svc, &stubTeamResolver{teams: teams})

	body := KBEnsureRequest{CodeRepoURL: "https://github.com/o/p.git"}
	w := doKBEnsure(t, r, "user-1", body)
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("got %d, want 412; body=%s", w.Code, w.Body.String())
	}
	if !containsStr(w.Body.String(), "TEAM_NS_NOT_INITIALIZED") {
		t.Errorf("expected TEAM_NS_NOT_INITIALIZED: %s", w.Body.String())
	}
}
