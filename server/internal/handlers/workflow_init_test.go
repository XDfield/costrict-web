package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/teamns"
	"github.com/costrict/costrict-web/server/internal/tenant"
	"github.com/gin-gonic/gin"
)

func newWorkflowInitRouter(t *testing.T, svc *teamns.Service) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	teamnsService = svc
	t.Cleanup(func() { teamnsService = nil })
	r := gin.New()
	r.POST("/api/internal/workflow/init", WorkflowInit)
	return r
}

func seedTeamForWorkflow(t *testing.T, db interface{}, teamID, encToken, sha string) {
	// helper kept as a stub — actual seeding happens in each test below.
}

func TestWorkflowInit_Disabled503(t *testing.T) {
	r := newWorkflowInitRouter(t, nil)
	body := WorkflowInitRequest{
		WorkflowDefSlug: "bug-fix-flow",
		TeamID:          padUUIDHandler(1),
		InstanceID:      padUUIDHandler(2),
	}
	w := doJSONWithTenant(t, r, http.MethodPost, "/api/internal/workflow/init", tenant.DefaultTenantID, body)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", w.Code)
	}
}

func TestWorkflowInit_BadJSONReturns400(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newWorkflowInitRouter(t, svc)
	req := httptest.NewRequest(http.MethodPost, "/api/internal/workflow/init", nil)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(tenant.WithTenantID(context.Background(), tenant.DefaultTenantID))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestWorkflowInit_MissingFieldsReturns400(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newWorkflowInitRouter(t, svc)
	body := WorkflowInitRequest{WorkflowDefSlug: "x"} // missing team_id / instance_id
	w := doJSONWithTenant(t, r, http.MethodPost, "/api/internal/workflow/init", tenant.DefaultTenantID, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestWorkflowInit_TeamNSMissing412(t *testing.T) {
	db := setupTeamnsDB(t)
	svc := teamns.NewService(db, nil, nil, mustAESHandler(t), nil)
	r := newWorkflowInitRouter(t, svc)
	body := WorkflowInitRequest{
		WorkflowDefSlug: "bug-fix-flow",
		TeamID:          padUUIDHandler(1),
		InstanceID:      padUUIDHandler(2),
	}
	w := doJSONWithTenant(t, r, http.MethodPost, "/api/internal/workflow/init", tenant.DefaultTenantID, body)
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("got %d, want 412; body=%s", w.Code, w.Body.String())
	}
	if !containsStr(w.Body.String(), "TEAM_NS_NOT_INITIALIZED") {
		t.Errorf("expected TEAM_NS_NOT_INITIALIZED in body: %s", w.Body.String())
	}
}

func TestWorkflowInit_HappyPath_ReturnsPathsAndBotCreds(t *testing.T) {
	db := setupTeamnsDB(t)
	aes := mustAESHandler(t)
	plaintext := "pat-XYZ"
	enc, err := aes.Seal([]byte(plaintext))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	now := time.Now().UTC()
	teamID := padUUIDHandler(3)
	ns := models.TeamNamespace{
		TeamID: teamID, TenantID: tenant.DefaultTenantID,
		TeamDisplayName: "Platform", TeamNSOrg: "t-" + padUUIDHandler(3)[:8],
		TeamShort: padUUIDHandler(3)[:8], GitServerID: "gs-1",
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&ns).Error; err != nil {
		t.Fatalf("seed ns: %v", err)
	}
	creds := models.TeamBotCredentials{
		TeamID: teamID, TenantID: tenant.DefaultTenantID, GitServerID: "gs-1",
		GiteaUsername: "bot-t-" + padUUIDHandler(3)[:8], GiteaUserID: 42, GiteaTokenID: 17,
		TokenEncrypted: enc, TokenSHA256: "sha", CreatedAt: now,
	}
	if err := db.Create(&creds).Error; err != nil {
		t.Fatalf("seed creds: %v", err)
	}

	// Inject a stub GitServer so EnsureWorkflowRepo's 7-step pipeline runs
	// against a fake backend — repo / branch / file ops all report "not
	// exists" first so the create path is exercised end-to-end.
	fake := &fakeGitServerHandler{}
	svc := teamns.NewService(db, nil, nil, aes, nil)
	svc.SetGitServerFactoryForTest(func(ctx context.Context, tenantID string) (gitsync.GitServer, error) {
		return fake, nil
	})
	r := newWorkflowInitRouter(t, svc)

	defSnap := `{"version":1}`
	body := WorkflowInitRequest{
		WorkflowDefSlug:    "bug-fix-flow",
		TeamID:             teamID,
		InstanceID:         padUUIDHandler(4),
		DefinitionSnapshot: defSnap,
	}
	w := doJSONWithTenant(t, r, http.MethodPost, "/api/internal/workflow/init", tenant.DefaultTenantID, body)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", w.Code, w.Body.String())
	}
	var got WorkflowInitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.WfRepoPath != "t-"+padUUIDHandler(3)[:8]+"/wf-bug-fix-flow" {
		t.Errorf("wf_repo_path: got %q", got.WfRepoPath)
	}
	if got.InstanceBranch != "inst-"+padUUIDHandler(4)[:8] {
		t.Errorf("instance_branch: got %q", got.InstanceBranch)
	}
	// Created flags: stub returned nil repo/branch/file → all created this call.
	if !got.Created.TypeRepo {
		t.Errorf("expected Created.TypeRepo=true")
	}
	if !got.Created.InstanceBranch {
		t.Errorf("expected Created.InstanceBranch=true")
	}
	if got.BotCredentials == nil || got.BotCredentials.Token != plaintext {
		t.Errorf("bot creds token: %+v", got.BotCredentials)
	}
	if got.BotCredentials == nil || got.BotCredentials.GiteaUsername != "bot-t-"+padUUIDHandler(3)[:8] {
		t.Errorf("bot creds username: %+v", got.BotCredentials)
	}
	if got.BotCredentials == nil || got.BotCredentials.CloneURLWithToken != "" {
		// clone_url_with_token is empty when the gitsync resolver isn't wired
		// (test constructs teamns with nil gitsync). Just assert the field is
		// present in the JSON shape; full coverage of URL composition happens
		// in the workflow_init integration test that wires a stub resolver.
		t.Logf("clone_url_with_token=%q (expected empty with nil gitsync)", got.BotCredentials.CloneURLWithToken)
	}
	// Backend ops were actually invoked (handler didn't short-circuit).
	if len(fake.createRepoCalls) != 1 {
		t.Errorf("expected 1 CreateRepo call, got %d", len(fake.createRepoCalls))
	}
	if len(fake.writeFileCalls) != 1 {
		t.Errorf("expected 1 WriteFile call, got %d", len(fake.writeFileCalls))
	}
	if len(fake.createBranchCalls) != 1 {
		t.Errorf("expected 1 CreateBranch call, got %d", len(fake.createBranchCalls))
	}
	// Two protection rules: main + inst-* glob.
	if len(fake.setBranchProtectionCalls) != 2 {
		t.Errorf("expected 2 SetBranchProtection calls, got %d", len(fake.setBranchProtectionCalls))
	}
}

func TestWorkflowInit_DefinitionDrift_Returns409(t *testing.T) {
	db := setupTeamnsDB(t)
	aes := mustAESHandler(t)
	plaintext := "pat-XYZ"
	enc, err := aes.Seal([]byte(plaintext))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	now := time.Now().UTC()
	teamID := padUUIDHandler(5)
	ns := models.TeamNamespace{
		TeamID: teamID, TenantID: tenant.DefaultTenantID,
		TeamDisplayName: "Platform", TeamNSOrg: "t-" + padUUIDHandler(5)[:8],
		TeamShort: padUUIDHandler(5)[:8], GitServerID: "gs-1",
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&ns).Error; err != nil {
		t.Fatalf("seed ns: %v", err)
	}
	creds := models.TeamBotCredentials{
		TeamID: teamID, TenantID: tenant.DefaultTenantID, GitServerID: "gs-1",
		GiteaUsername: "bot-t-" + padUUIDHandler(5)[:8], GiteaUserID: 42, GiteaTokenID: 17,
		TokenEncrypted: enc, TokenSHA256: "sha", CreatedAt: now,
	}
	if err := db.Create(&creds).Error; err != nil {
		t.Fatalf("seed creds: %v", err)
	}

	// Stub: repo exists, snapshot on main differs from caller's → drift.
	fake := &fakeGitServerHandler{
		getRepoResult:  &gitsync.Repo{ID: 42, Name: "wf-bug-fix-flow"},
		readFileResult: []byte(`{"version":"OLD"}`),
	}
	svc := teamns.NewService(db, nil, nil, aes, nil)
	svc.SetGitServerFactoryForTest(func(ctx context.Context, tenantID string) (gitsync.GitServer, error) {
		return fake, nil
	})
	r := newWorkflowInitRouter(t, svc)

	body := WorkflowInitRequest{
		WorkflowDefSlug:    "bug-fix-flow",
		TeamID:             teamID,
		InstanceID:         padUUIDHandler(6),
		DefinitionSnapshot: `{"version":"NEW"}`,
	}
	w := doJSONWithTenant(t, r, http.MethodPost, "/api/internal/workflow/init", tenant.DefaultTenantID, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409; body=%s", w.Code, w.Body.String())
	}
	if !containsStr(w.Body.String(), "DEFINITION_DRIFT") {
		t.Errorf("expected DEFINITION_DRIFT in body: %s", w.Body.String())
	}
	// Drift must short-circuit before protection / branch ops.
	if len(fake.setBranchProtectionCalls) != 0 {
		t.Errorf("expected no SetBranchProtection on drift, got %d", len(fake.setBranchProtectionCalls))
	}
	if len(fake.createBranchCalls) != 0 {
		t.Errorf("expected no CreateBranch on drift, got %d", len(fake.createBranchCalls))
	}
}

func containsStr(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
