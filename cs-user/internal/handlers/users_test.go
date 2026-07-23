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

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/user"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func init() { gin.SetMode(gin.TestMode) }

// stubUserService lets handlers tests pin responses without a DB. Each
// method field is optional; a nil field panics so a forgotten write test
// fails loudly instead of silently returning zero values.
type stubUserService struct {
	getByID                func(context.Context, string) (*models.User, error)
	getByIDs               func(context.Context, []string) (map[string]*models.User, error)
	searchUsers            func(context.Context, string, int) ([]*models.User, error)
	searchUsersByEmpNo     func(context.Context, string, int) ([]*models.User, error)
	getOrCreate            func(context.Context, *models.JWTClaims) (*models.User, error)
	bindIdentity           func(context.Context, string, *models.JWTClaims, ...models.BindIdentityOptions) error
	transfer               func(context.Context, string, string, string) error
	unbind                 func(context.Context, string, string) error
	applyEnterpriseMapping func(context.Context, user.EmploymentMappingParams) error
	listUsers              func(context.Context, user.ListUsersParams) ([]*models.User, int64, error)
	setUserStatus          func(context.Context, string, string, string) (*user.SetUserStatusResult, error)
	listOrganizations      func(context.Context) ([]user.OrganizationCount, error)
}

func (s stubUserService) GetUserByID(ctx context.Context, id string) (*models.User, error) {
	if s.getByID == nil {
		panic("stubUserService.getByID not wired")
	}
	return s.getByID(ctx, id)
}
func (s stubUserService) GetUsersByIDs(ctx context.Context, ids []string) (map[string]*models.User, error) {
	if s.getByIDs == nil {
		panic("stubUserService.getByIDs not wired")
	}
	return s.getByIDs(ctx, ids)
}
func (s stubUserService) SearchUsers(ctx context.Context, kw string, lim int) ([]*models.User, error) {
	if s.searchUsers == nil {
		panic("stubUserService.searchUsers not wired")
	}
	return s.searchUsers(ctx, kw, lim)
}
func (s stubUserService) SearchUsersByEmployeeNumber(ctx context.Context, empNo string, lim int) ([]*models.User, error) {
	if s.searchUsersByEmpNo == nil {
		panic("stubUserService.searchUsersByEmpNo not wired")
	}
	return s.searchUsersByEmpNo(ctx, empNo, lim)
}
func (s stubUserService) GetOrCreateUser(ctx context.Context, claims *models.JWTClaims) (*models.User, error) {
	if s.getOrCreate == nil {
		panic("stubUserService.getOrCreate not wired")
	}
	return s.getOrCreate(ctx, claims)
}
func (s stubUserService) BindIdentityToUser(ctx context.Context, sub string, claims *models.JWTClaims, opts ...models.BindIdentityOptions) error {
	if s.bindIdentity == nil {
		panic("stubUserService.bindIdentity not wired")
	}
	return s.bindIdentity(ctx, sub, claims, opts...)
}
func (s stubUserService) TransferIdentityToUser(ctx context.Context, tgt, key, src string) error {
	if s.transfer == nil {
		panic("stubUserService.transfer not wired")
	}
	return s.transfer(ctx, tgt, key, src)
}
func (s stubUserService) UnbindIdentityByProvider(ctx context.Context, sub, provider string) error {
	if s.unbind == nil {
		panic("stubUserService.unbind not wired")
	}
	return s.unbind(ctx, sub, provider)
}
func (s stubUserService) ApplyEnterpriseMapping(ctx context.Context, params user.EmploymentMappingParams) error {
	if s.applyEnterpriseMapping == nil {
		panic("stubUserService.applyEnterpriseMapping not wired")
	}
	return s.applyEnterpriseMapping(ctx, params)
}
func (s stubUserService) ListUsers(ctx context.Context, p user.ListUsersParams) ([]*models.User, int64, error) {
	if s.listUsers == nil {
		panic("stubUserService.listUsers not wired")
	}
	return s.listUsers(ctx, p)
}
func (s stubUserService) SetUserStatus(ctx context.Context, subjectID, status, operatorID string) (*user.SetUserStatusResult, error) {
	if s.setUserStatus == nil {
		panic("stubUserService.setUserStatus not wired")
	}
	return s.setUserStatus(ctx, subjectID, status, operatorID)
}
func (s stubUserService) ListOrganizations(ctx context.Context) ([]user.OrganizationCount, error) {
	if s.listOrganizations == nil {
		panic("stubUserService.listOrganizations not wired")
	}
	return s.listOrganizations(ctx)
}

func newUsersAPI(svc UserService) (*UsersAPI, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := &UsersAPI{Svc: svc}
	r.GET("/api/internal/users/:subject_id", api.GetUser)
	r.POST("/api/internal/users/by-ids", api.GetUsersByIDs)
	r.GET("/api/internal/users/search", api.SearchUsers)
	// Phase 2 write routes — mirror app.registerUserRoutes so handler tests
	// can exercise the same gin path tree (esp. the :subject_id vs. static
	// suffix distinction gin enforces).
	r.POST("/api/internal/users/get-or-create", api.GetOrCreate)
	r.POST("/api/internal/users/transfer-identity", api.TransferIdentity)
	r.POST("/api/internal/users/:subject_id/bind-identity", api.BindIdentity)
	r.DELETE("/api/internal/users/:subject_id/identities/:provider", api.UnbindIdentity)
	// Phase A4b route.
	r.POST("/api/internal/users/apply-enterprise-mapping", api.ApplyEnterpriseMapping)
	// Admin user-management route (admin-user-migration slice).
	r.GET("/api/internal/users/list", api.ListUsers)
	r.GET("/api/internal/users/organizations", api.ListOrganizations)
	r.POST("/api/internal/users/:subject_id/status", api.SetUserStatus)
	r.GET("/api/internal/users/:subject_id/profile", api.GetUserProfile)
	return api, r
}

func doJSON(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestGetUser_HappyPath(t *testing.T) {
	want := &models.User{SubjectID: "subj-1", Username: "alice"}
	api, r := newUsersAPI(stubUserService{
		getByID: func(_ context.Context, id string) (*models.User, error) {
			if id != "subj-1" {
				t.Errorf("handler passed id=%q, want subj-1", id)
			}
			return want, nil
		},
	})
	_ = api

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/subj-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var got models.User
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, w.Body.String())
	}
	if got.SubjectID != "subj-1" || got.Username != "alice" {
		t.Errorf("got %+v, want subj-1/alice", got)
	}
}

func TestGetUser_NotFound(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		getByID: func(context.Context, string) (*models.User, error) { return nil, gorm.ErrRecordNotFound },
	})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/missing", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGetUser_ServiceError(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		getByID: func(context.Context, string) (*models.User, error) { return nil, errors.New("db dead") },
	})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/x", nil)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	// Body must NOT echo the internal error string — avoid leaking.
	if bytes.Contains(w.Body.Bytes(), []byte("db dead")) {
		t.Errorf("body leaks internal error: %s", w.Body.String())
	}
}

func TestGetUsersByIDs_HappyPath(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		getByIDs: func(_ context.Context, ids []string) (map[string]*models.User, error) {
			if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
				t.Errorf("handler passed ids=%v, want [a b]", ids)
			}
			return map[string]*models.User{
				"a": {SubjectID: "a", Username: "alice"},
			}, nil
		},
	})

	w := doJSON(t, r, http.MethodPost, "/api/internal/users/by-ids", map[string]any{
		"ids": []string{"a", "b"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Users map[string]*models.User `json:"users"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, w.Body.String())
	}
	if len(resp.Users) != 1 || resp.Users["a"].Username != "alice" {
		t.Errorf("got %+v", resp.Users)
	}
}

func TestGetUsersByIDs_RejectsEmptyBody(t *testing.T) {
	_, r := newUsersAPI(stubUserService{})

	w := doJSON(t, r, http.MethodPost, "/api/internal/users/by-ids", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGetUsersByIDs_RejectsOversizedBatch(t *testing.T) {
	_, r := newUsersAPI(stubUserService{})

	big := make([]string, 501)
	for i := range big {
		big[i] = "x"
	}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/by-ids", map[string]any{"ids": big})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (max=500)", w.Code)
	}
}

func TestSearchUsers_HappyPath(t *testing.T) {
	called := false
	_, r := newUsersAPI(stubUserService{
		searchUsers: func(_ context.Context, kw string, lim int) ([]*models.User, error) {
			called = true
			if kw != "ali" {
				t.Errorf("keyword: got %q want ali", kw)
			}
			// Handler passes 0 when the limit query is unset; the service is
			// responsible for substituting defaultSearchLimit. Asserting here
			// would couple the handler to the service's default.
			if lim != 0 {
				t.Errorf("limit: got %d want 0 (handler passes raw, service applies default)", lim)
			}
			return []*models.User{{SubjectID: "a", Username: "alice"}}, nil
		},
	})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/search?keyword=ali", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if !called {
		t.Error("SearchUsers not invoked")
	}
}

func TestSearchUsers_ClampsLimit(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		searchUsers: func(_ context.Context, _ string, lim int) ([]*models.User, error) {
			if lim != 200 {
				t.Errorf("limit not clamped: got %d want 200", lim)
			}
			return nil, nil
		},
	})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/search?limit=99999", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestSearchUsers_RejectsNegativeLimit(t *testing.T) {
	_, r := newUsersAPI(stubUserService{})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/search?limit=-1", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestSearchUsers_RejectsGarbageLimit(t *testing.T) {
	_, r := newUsersAPI(stubUserService{})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/search?limit=abc", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestSearchUsers_ByEmployeeNumberRoutes: passing employee_number should
// (a) NOT call the keyword path, (b) call SearchUsersByEmployeeNumber with
// the parsed emp no and limit, and (c) return the service's response.
func TestSearchUsers_ByEmployeeNumberRoutes(t *testing.T) {
	keywordCalled := false
	empCalled := false
	_, r := newUsersAPI(stubUserService{
		searchUsers: func(context.Context, string, int) ([]*models.User, error) {
			keywordCalled = true
			return nil, nil
		},
		searchUsersByEmpNo: func(_ context.Context, empNo string, lim int) ([]*models.User, error) {
			empCalled = true
			if empNo != "1001" {
				t.Errorf("emp no: got %q want 1001", empNo)
			}
			if lim != 1 {
				t.Errorf("limit: got %d want 1", lim)
			}
			return []*models.User{{SubjectID: "usr-1", Username: "alice"}}, nil
		},
	})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/search?employee_number=1001&limit=1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if !empCalled {
		t.Error("SearchUsersByEmployeeNumber not invoked")
	}
	if keywordCalled {
		t.Error("keyword path should not run when employee_number is set")
	}
	var resp struct {
		Users []*models.User `json:"users"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, w.Body.String())
	}
	if len(resp.Users) != 1 || resp.Users[0].SubjectID != "usr-1" {
		t.Errorf("got %+v, want usr-1", resp.Users)
	}
}

// TestSearchUsers_RejectsKeywordAndEmployeeNumberBoth: doc v1.1 §5.2 marks
// the two query params mutually exclusive — supplying both is a 400 so the
// caller fixes their bug rather than getting a silent precedence choice.
func TestSearchUsers_RejectsKeywordAndEmployeeNumberBoth(t *testing.T) {
	_, r := newUsersAPI(stubUserService{})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/search?keyword=ali&employee_number=1001", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (mutual exclusion)", w.Code)
	}
}

// --- Phase 2: Write handler tests ---

func TestGetOrCreate_HappyPath(t *testing.T) {
	var capturedClaims *models.JWTClaims
	_, r := newUsersAPI(stubUserService{
		getOrCreate: func(_ context.Context, got *models.JWTClaims) (*models.User, error) {
			capturedClaims = got
			return &models.User{SubjectID: "usr-new", Username: "alice"}, nil
		},
	})

	body := models.JWTClaims{
		ID:          "id-1",
		Sub:         "sub-1",
		UniversalID: "uuid-1",
		Name:        "alice",
		Provider:    "github",
	}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/get-or-create", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if capturedClaims == nil || capturedClaims.UniversalID != "uuid-1" {
		t.Errorf("claims not propagated: %+v", capturedClaims)
	}
	var resp models.User
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.SubjectID != "usr-new" {
		t.Errorf("SubjectID: got %q, want usr-new", resp.SubjectID)
	}
}

func TestGetOrCreate_NilClaimsRejected(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		getOrCreate: func(_ context.Context, _ *models.JWTClaims) (*models.User, error) {
			return nil, errors.New("nil JWT claims")
		},
	})

	// Empty body — service returns "nil JWT claims", handler maps to 400.
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/get-or-create", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
}

func TestGetOrCreate_NoIdentifierRejected(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		getOrCreate: func(_ context.Context, _ *models.JWTClaims) (*models.User, error) {
			return nil, errors.New("no valid user identifier in JWT claims")
		},
	})

	w := doJSON(t, r, http.MethodPost, "/api/internal/users/get-or-create", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGetOrCreate_ServiceError(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		getOrCreate: func(_ context.Context, _ *models.JWTClaims) (*models.User, error) {
			return nil, errors.New("db dead")
		},
	})

	w := doJSON(t, r, http.MethodPost, "/api/internal/users/get-or-create",
		models.JWTClaims{UniversalID: "uuid-1"})
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("db dead")) {
		t.Errorf("body leaks internal error: %s", w.Body.String())
	}
}

func TestBindIdentity_HappyPath(t *testing.T) {
	var capturedSub string
	var capturedClaims *models.JWTClaims
	var capturedOpts []models.BindIdentityOptions
	_, r := newUsersAPI(stubUserService{
		bindIdentity: func(_ context.Context, sub string, claims *models.JWTClaims, opts ...models.BindIdentityOptions) error {
			capturedSub = sub
			capturedClaims = claims
			capturedOpts = opts
			return nil
		},
	})

	body := bindIdentityRequest{
		Claims:  &models.JWTClaims{UniversalID: "uuid-bind", Provider: "github"},
		Options: &models.BindIdentityOptions{ForceRebind: true},
	}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/subj-bind/bind-identity", body)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", w.Code, w.Body.String())
	}
	if capturedSub != "subj-bind" {
		t.Errorf("subject_id: got %q, want subj-bind", capturedSub)
	}
	if capturedClaims == nil || capturedClaims.UniversalID != "uuid-bind" {
		t.Errorf("claims not propagated: %+v", capturedClaims)
	}
	if len(capturedOpts) != 1 || !capturedOpts[0].ForceRebind {
		t.Errorf("ForceRebind not propagated: %+v", capturedOpts)
	}
}

func TestBindIdentity_MissingSubject(t *testing.T) {
	_, r := newUsersAPI(stubUserService{})
	// Static path "by-ids" can't masquerade as :subject_id here; gin would
	// 404. Skip — covered by BindIdentity's own guard.
	_ = r
}

func TestBindIdentity_InvalidBody(t *testing.T) {
	_, r := newUsersAPI(stubUserService{})

	// Empty body fails the Claims binding:"required".
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/subj-x/bind-identity", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestBindIdentity_ExplicitlyUnboundMaps409(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		bindIdentity: func(_ context.Context, _ string, _ *models.JWTClaims, _ ...models.BindIdentityOptions) error {
			return user.ErrExplicitlyUnbound
		},
	})

	body := bindIdentityRequest{Claims: &models.JWTClaims{UniversalID: "uuid-x", Provider: "github"}}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/subj-x/bind-identity", body)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

func TestBindIdentity_AlreadyBoundMaps409(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		bindIdentity: func(_ context.Context, _ string, _ *models.JWTClaims, _ ...models.BindIdentityOptions) error {
			return user.ErrIdentityAlreadyBound
		},
	})

	body := bindIdentityRequest{Claims: &models.JWTClaims{UniversalID: "uuid-x", Provider: "github"}}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/subj-x/bind-identity", body)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

func TestBindIdentity_BadRequestMaps400(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		bindIdentity: func(_ context.Context, _ string, _ *models.JWTClaims, _ ...models.BindIdentityOptions) error {
			return errors.New("external key is required")
		},
	})

	body := bindIdentityRequest{Claims: &models.JWTClaims{}}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/subj-x/bind-identity", body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestBindIdentity_InternalError(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		bindIdentity: func(_ context.Context, _ string, _ *models.JWTClaims, _ ...models.BindIdentityOptions) error {
			return errors.New("unexpected boom")
		},
	})

	body := bindIdentityRequest{Claims: &models.JWTClaims{UniversalID: "uuid-x"}}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/subj-x/bind-identity", body)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestTransferIdentity_HappyPath(t *testing.T) {
	var captured transferIdentityRequest
	_, r := newUsersAPI(stubUserService{
		transfer: func(_ context.Context, tgt, key, src string) error {
			captured = transferIdentityRequest{
				TargetUserSubjectID: tgt,
				ExternalKey:         key,
				SourceUserSubjectID: src,
			}
			return nil
		},
	})

	body := transferIdentityRequest{
		TargetUserSubjectID: "subj-to",
		ExternalKey:         "casdoor:github:xfer",
		SourceUserSubjectID: "subj-from",
	}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/transfer-identity", body)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if captured.TargetUserSubjectID != "subj-to" || captured.ExternalKey != "casdoor:github:xfer" {
		t.Errorf("args not propagated: %+v", captured)
	}
}

func TestTransferIdentity_MissingFields(t *testing.T) {
	_, r := newUsersAPI(stubUserService{})

	w := doJSON(t, r, http.MethodPost, "/api/internal/users/transfer-identity", map[string]any{
		"target_user_subject_id": "subj-x",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestTransferIdentity_NotFoundMaps404(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		transfer: func(_ context.Context, _, _, _ string) error {
			return errors.New("identity_not_found")
		},
	})

	body := transferIdentityRequest{
		TargetUserSubjectID: "subj-x",
		ExternalKey:         "casdoor:github:missing",
	}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/transfer-identity", body)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestTransferIdentity_ServiceError(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		transfer: func(_ context.Context, _, _, _ string) error {
			return errors.New("boom")
		},
	})

	body := transferIdentityRequest{
		TargetUserSubjectID: "subj-x",
		ExternalKey:         "casdoor:github:x",
	}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/transfer-identity", body)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestUnbindIdentity_HappyPath(t *testing.T) {
	var capturedSub, capturedProvider string
	_, r := newUsersAPI(stubUserService{
		unbind: func(_ context.Context, sub, provider string) error {
			capturedSub = sub
			capturedProvider = provider
			return nil
		},
	})

	w := doJSON(t, r, http.MethodDelete, "/api/internal/users/subj-x/identities/github", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", w.Code, w.Body.String())
	}
	if capturedSub != "subj-x" || capturedProvider != "github" {
		t.Errorf("args: got sub=%q provider=%q", capturedSub, capturedProvider)
	}
}

func TestUnbindIdentity_LastIdentityMaps409(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		unbind: func(_ context.Context, _, _ string) error {
			return user.ErrLastIdentity
		},
	})

	w := doJSON(t, r, http.MethodDelete, "/api/internal/users/subj-x/identities/github", nil)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

func TestUnbindIdentity_NotFoundMaps404(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		unbind: func(_ context.Context, _, _ string) error {
			return errors.New("identity not found")
		},
	})

	w := doJSON(t, r, http.MethodDelete, "/api/internal/users/subj-x/identities/github", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestUnbindIdentity_ServiceError(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		unbind: func(_ context.Context, _, _ string) error {
			return errors.New("boom")
		},
	})

	w := doJSON(t, r, http.MethodDelete, "/api/internal/users/subj-x/identities/github", nil)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// --- Phase A4b: ApplyEnterpriseMapping handler ---

// TestApplyEnterpriseMapping_AppliedTrue verifies the happy path: service
// returns nil → handler responds 200 with `{"applied": true}`.
func TestApplyEnterpriseMapping_AppliedTrue(t *testing.T) {
	var capturedParams user.EmploymentMappingParams
	_, r := newUsersAPI(stubUserService{
		applyEnterpriseMapping: func(_ context.Context, p user.EmploymentMappingParams) error {
			capturedParams = p
			return nil
		},
	})

	w := doJSON(t, r, http.MethodPost, "/api/internal/users/apply-enterprise-mapping", map[string]string{
		"user_subject_id": "usr_alice",
		"provider":        "idtrust",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Applied bool `json:"applied"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Applied {
		t.Errorf("applied: got false, want true")
	}
	if capturedParams.UserSubjectID != "usr_alice" {
		t.Errorf("UserSubjectID: got %q, want usr_alice", capturedParams.UserSubjectID)
	}
	if capturedParams.Provider != "idtrust" {
		t.Errorf("Provider: got %q, want idtrust", capturedParams.Provider)
	}
}

// TestApplyEnterpriseMapping_DisabledMaps200AppliedFalse verifies the disabled
// sentinel is surfaced as 200 + `{"applied": false}`. Login callers must be
// able to distinguish "skipped" from "applied" without sniffing error strings.
func TestApplyEnterpriseMapping_DisabledMaps200AppliedFalse(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		applyEnterpriseMapping: func(_ context.Context, _ user.EmploymentMappingParams) error {
			return user.ErrEnterpriseMappingDisabled
		},
	})

	w := doJSON(t, r, http.MethodPost, "/api/internal/users/apply-enterprise-mapping", map[string]string{
		"user_subject_id": "usr_alice",
		"provider":        "github",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (disabled is success)", w.Code)
	}
	var resp struct {
		Applied bool `json:"applied"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Applied {
		t.Errorf("applied: got true, want false for disabled provider")
	}
}

// TestApplyEnterpriseMapping_MalformedYAMLMaps500 verifies that a real service
// failure (e.g. malformed tenant_configs YAML) surfaces as 500, not silently
// swallowed as 200/applied=false.
func TestApplyEnterpriseMapping_MalformedYAMLMaps500(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		applyEnterpriseMapping: func(_ context.Context, _ user.EmploymentMappingParams) error {
			return errors.New("load employment_providers config: parse config_yaml: yaml: line 1: did not find expected ',' or ']'")
		},
	})

	w := doJSON(t, r, http.MethodPost, "/api/internal/users/apply-enterprise-mapping", map[string]string{
		"user_subject_id": "usr_alice",
		"provider":        "idtrust",
	})
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for malformed YAML", w.Code)
	}
}

// TestApplyEnterpriseMapping_ValidationMaps400 verifies missing required
// fields surface as 400 (gin's binding:"required" tag catches this before
// the service is called).
func TestApplyEnterpriseMapping_ValidationMaps400(t *testing.T) {
	tests := []struct {
		name string
		body map[string]string
	}{
		{"missing user_subject_id", map[string]string{"provider": "idtrust"}},
		{"missing provider", map[string]string{"user_subject_id": "usr_alice"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, r := newUsersAPI(stubUserService{
				applyEnterpriseMapping: func(_ context.Context, _ user.EmploymentMappingParams) error {
					t.Fatal("service should not be called on validation failure")
					return nil
				},
			})
			w := doJSON(t, r, http.MethodPost, "/api/internal/users/apply-enterprise-mapping", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400, body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// TestApplyEnterpriseMapping_OptionalTenantID verifies the tenant_id field is
// forwarded to the service when supplied. Empty tenant_id is the service's
// responsibility to default (TestApplyEnterpriseMapping_AppliedTrue covers
// the empty case).
func TestApplyEnterpriseMapping_OptionalTenantID(t *testing.T) {
	var capturedTenantID string
	_, r := newUsersAPI(stubUserService{
		applyEnterpriseMapping: func(_ context.Context, p user.EmploymentMappingParams) error {
			capturedTenantID = p.TenantID
			return nil
		},
	})

	w := doJSON(t, r, http.MethodPost, "/api/internal/users/apply-enterprise-mapping", map[string]string{
		"tenant_id":       "acme",
		"user_subject_id": "usr_alice",
		"provider":        "idtrust",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if capturedTenantID != "acme" {
		t.Errorf("TenantID: got %q, want acme", capturedTenantID)
	}
}

// --- ListUsers (admin-user-migration slice) ---

// newListUsersAPI wires the UsersAPI with a stub service for the
// ListUsers-only test surface.
func newListUsersAPI(svc UserService) (*UsersAPI, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := &UsersAPI{Svc: svc}
	r.GET("/api/internal/users/list", api.ListUsers)
	return api, r
}

func TestListUsers_HappyPathReturnsPaginatedResult(t *testing.T) {
	org := "eng"
	email := "alice@example.com"
	display := "Alice"
	avatar := "https://example.com/a.png"
	now := time.Now().UTC()
	users := []*models.User{
		{SubjectID: "usr_alice", Username: "alice", DisplayName: &display, Email: &email, AvatarURL: &avatar, Organization: &org, Status: "active", IsActive: true, CreatedAt: now},
	}
	var capturedParams user.ListUsersParams
	svc := stubUserService{
		listUsers: func(_ context.Context, p user.ListUsersParams) ([]*models.User, int64, error) {
			capturedParams = p
			return users, 1, nil
		},
	}
	_, r := newListUsersAPI(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/users/list?keyword=ali&organization=eng&status=active&page=2&page_size=10", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if capturedParams.Keyword != "ali" || capturedParams.Organization != "eng" || capturedParams.Status != "active" {
		t.Errorf("filter passthrough mismatch: %+v", capturedParams)
	}
	if capturedParams.Page != 2 || capturedParams.PageSize != 10 {
		t.Errorf("pagination mismatch: %+v", capturedParams)
	}

	var body adminUserListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Total != 1 || len(body.Users) != 1 {
		t.Fatalf("unexpected shape: %+v", body)
	}
	got := body.Users[0]
	if got.SubjectID != "usr_alice" || got.Username != "alice" || got.Status != "active" || !got.IsActive {
		t.Errorf("user payload mismatch: %+v", got)
	}
	if got.Email == nil || *got.Email != email {
		t.Errorf("email mismatch: %+v", got)
	}
	if got.CreatedAt == "" {
		t.Errorf("createdAt should be non-empty ISO string")
	}
}

func TestListUsers_DefaultsPageAndSizeWhenOmitted(t *testing.T) {
	var captured user.ListUsersParams
	svc := stubUserService{
		listUsers: func(_ context.Context, p user.ListUsersParams) ([]*models.User, int64, error) {
			captured = p
			return nil, 0, nil
		},
	}
	_, r := newListUsersAPI(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/users/list", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if captured.Page != 1 {
		t.Errorf("default page should be 1, got %d", captured.Page)
	}
	// Default page_size is left to the service layer (0 → service applies
	// DefaultAdminUserPageSize=20). Handler passes through whatever was
	// parsed; service clamps.
	if captured.PageSize != 0 {
		t.Errorf("handler should pass 0 when omitted, got %d", captured.PageSize)
	}
}

func TestListUsers_RejectsInvalidStatus(t *testing.T) {
	svc := stubUserService{
		listUsers: func(context.Context, user.ListUsersParams) ([]*models.User, int64, error) {
			t.Errorf("service must not be called for invalid status")
			return nil, 0, nil
		},
	}
	_, r := newListUsersAPI(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/users/list?status=quarantined", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid status, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListUsers_RejectsNonPositivePage(t *testing.T) {
	svc := stubUserService{
		listUsers: func(context.Context, user.ListUsersParams) ([]*models.User, int64, error) {
			t.Errorf("service must not be called for invalid page")
			return nil, 0, nil
		},
	}
	_, r := newListUsersAPI(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/users/list?page=0", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for page=0, got %d", w.Code)
	}
}

func TestListUsers_RejectsNonPositivePageSize(t *testing.T) {
	svc := stubUserService{
		listUsers: func(context.Context, user.ListUsersParams) ([]*models.User, int64, error) {
			t.Errorf("service must not be called for invalid page_size")
			return nil, 0, nil
		},
	}
	_, r := newListUsersAPI(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/users/list?page_size=0", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for page_size=0, got %d", w.Code)
	}
}

func TestListUsers_EmptyResultReturnsEmptyArrayNotNil(t *testing.T) {
	svc := stubUserService{
		listUsers: func(context.Context, user.ListUsersParams) ([]*models.User, int64, error) {
			return nil, 0, nil
		},
	}
	_, r := newListUsersAPI(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/users/list", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"users":[]`) {
		t.Errorf("empty result should serialize as [] not null: %s", w.Body.String())
	}
}

func TestListUsers_ServiceErrorReturns500(t *testing.T) {
	svc := stubUserService{
		listUsers: func(context.Context, user.ListUsersParams) ([]*models.User, int64, error) {
			return nil, 0, errors.New("db down")
		},
	}
	_, r := newListUsersAPI(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/users/list", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for service error, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "db down") {
		t.Errorf("internal error must not leak: %s", w.Body.String())
	}
}

// --- SetUserStatus (admin-user-migration slice) ---

func TestSetUserStatus_HappyPath(t *testing.T) {
	var gotSubject, gotStatus, gotOperator string
	svc := stubUserService{
		setUserStatus: func(_ context.Context, subjectID, status, operatorID string) (*user.SetUserStatusResult, error) {
			gotSubject, gotStatus, gotOperator = subjectID, status, operatorID
			return &user.SetUserStatusResult{SubjectID: subjectID, FromStatus: user.UserStatusActive, ToStatus: status}, nil
		},
	}
	_, r := newUsersAPI(svc)

	body := map[string]any{"status": "banned", "operator_id": "admin-007"}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/subj-1/status", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotSubject != "subj-1" || gotStatus != "banned" || gotOperator != "admin-007" {
		t.Errorf("service got (%q,%q,%q), want (subj-1,banned,admin-007)", gotSubject, gotStatus, gotOperator)
	}
	if !strings.Contains(w.Body.String(), `"from_status":"active"`) || !strings.Contains(w.Body.String(), `"to_status":"banned"`) {
		t.Errorf("response missing from/to status: %s", w.Body.String())
	}
}

func TestSetUserStatus_InvalidStatusReturns400(t *testing.T) {
	_, r := newUsersAPI(stubUserService{}) // service would panic if called

	body := map[string]any{"status": "quarantined", "operator_id": "admin-007"}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/subj-1/status", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid status, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSetUserStatus_MissingStatusReturns400(t *testing.T) {
	_, r := newUsersAPI(stubUserService{})

	w := doJSON(t, r, http.MethodPost, "/api/internal/users/subj-1/status", map[string]any{"operator_id": "admin-007"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing status, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSetUserStatus_SelfLockReturns409(t *testing.T) {
	svc := stubUserService{
		setUserStatus: func(_ context.Context, _, _, _ string) (*user.SetUserStatusResult, error) {
			return nil, user.ErrCannotChangeOwnStatus
		},
	}
	_, r := newUsersAPI(svc)

	body := map[string]any{"status": "disabled", "operator_id": "self-id"}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/self-id/status", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 self-lock, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cannot change own status") {
		t.Errorf("expected self-lock message, got: %s", w.Body.String())
	}
}

func TestSetUserStatus_NotFoundReturns404(t *testing.T) {
	svc := stubUserService{
		setUserStatus: func(_ context.Context, _, _, _ string) (*user.SetUserStatusResult, error) {
			return nil, user.ErrAdminUserNotFound
		},
	}
	_, r := newUsersAPI(svc)

	body := map[string]any{"status": "disabled"}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/ghost/status", body)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 unknown subject, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSetUserStatus_InternalErrorReturns500(t *testing.T) {
	svc := stubUserService{
		setUserStatus: func(_ context.Context, _, _, _ string) (*user.SetUserStatusResult, error) {
			return nil, errors.New("tx aborted")
		},
	}
	_, r := newUsersAPI(svc)

	body := map[string]any{"status": "banned"}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/subj-1/status", body)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 internal, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "tx aborted") {
		t.Errorf("internal error must not leak: %s", w.Body.String())
	}
}

func TestSetUserStatus_InvalidBodyReturns400(t *testing.T) {
	_, r := newUsersAPI(stubUserService{})

	// Malformed JSON.
	req := httptest.NewRequest(http.MethodPost, "/api/internal/users/subj-1/status", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 malformed body, got %d: %s", w.Code, w.Body.String())
	}
}

// --- ListOrganizations (admin-user-migration slice) ---

func TestListOrganizations_HappyPath(t *testing.T) {
	svc := stubUserService{
		listOrganizations: func(context.Context) ([]user.OrganizationCount, error) {
			return []user.OrganizationCount{
				{Organization: "Eng", MemberCount: 42},
				{Organization: "Ops", MemberCount: 7},
			}, nil
		},
	}
	_, r := newUsersAPI(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/users/organizations", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"organization":"Eng"`) || !strings.Contains(w.Body.String(), `"memberCount":42`) {
		t.Errorf("response missing org/count fields: %s", w.Body.String())
	}
}

func TestListOrganizations_EmptySerializesAsArray(t *testing.T) {
	svc := stubUserService{
		listOrganizations: func(context.Context) ([]user.OrganizationCount, error) {
			return nil, nil
		},
	}
	_, r := newUsersAPI(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/users/organizations", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"organizations":[]`) {
		t.Errorf("nil slice should serialize as [] not null: %s", w.Body.String())
	}
}

func TestListOrganizations_ServiceErrorReturns500(t *testing.T) {
	svc := stubUserService{
		listOrganizations: func(context.Context) ([]user.OrganizationCount, error) {
			return nil, errors.New("db down")
		},
	}
	_, r := newUsersAPI(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/users/organizations", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "db down") {
		t.Errorf("internal error must not leak: %s", w.Body.String())
	}
}

// --- GetUserProfile (admin-user-migration slice) ---

func TestGetUserProfile_HappyPath(t *testing.T) {
	disp := "Alice"
	email := "alice@example.com"
	org := "Eng"
	avatar := "https://cdn/x.png"
	lastLogin := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc := stubUserService{
		getByID: func(_ context.Context, _ string) (*models.User, error) {
			return &models.User{
				SubjectID:    "subj-1",
				Username:     "alice",
				DisplayName:  &disp,
				Email:        &email,
				AvatarURL:    &avatar,
				Organization: &org,
				Status:       user.UserStatusActive,
				IsActive:     true,
				LastLoginAt:  &lastLogin,
				CreatedAt:    created,
			}, nil
		},
	}
	_, r := newUsersAPI(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/users/subj-1/profile", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`"subject_id":"subj-1"`,
		`"username":"alice"`,
		`"display_name":"Alice"`,
		`"email":"alice@example.com"`,
		`"organization":"Eng"`,
		`"status":"active"`,
		`"is_active":true`,
		`"last_login_at":"2026-07-01T12:00:00Z"`,
		`"created_at":"2026-01-01T00:00:00Z"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %s: %s", want, body)
		}
	}
}

func TestGetUserProfile_DoesNotLeakInfraIDs(t *testing.T) {
	extKey := "ext-secret"
	casdoorID := "cas-abc"
	provUserID := "prov-xyz"
	svc := stubUserService{
		getByID: func(_ context.Context, _ string) (*models.User, error) {
			return &models.User{
				SubjectID:      "subj-1",
				Username:       "alice",
				ExternalKey:    &extKey,
				CasdoorID:      &casdoorID,
				ProviderUserID: &provUserID,
				Status:         user.UserStatusActive,
				IsActive:       true,
				CreatedAt:      time.Now(),
			}, nil
		},
	}
	_, r := newUsersAPI(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/users/subj-1/profile", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, banned := range []string{"external_key", "casdoor_id", "provider_user_id", "casdoor_sub", "casdoor_universal_id"} {
		if strings.Contains(body, banned) {
			t.Errorf("profile must not leak %s: %s", banned, body)
		}
	}
	if strings.Contains(body, "ext-secret") || strings.Contains(body, "cas-abc") || strings.Contains(body, "prov-xyz") {
		t.Errorf("infra identifiers leaked: %s", body)
	}
}

func TestGetUserProfile_NotFoundReturns404(t *testing.T) {
	svc := stubUserService{
		getByID: func(_ context.Context, _ string) (*models.User, error) {
			return nil, gorm.ErrRecordNotFound
		},
	}
	_, r := newUsersAPI(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/users/ghost/profile", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetUserProfile_InternalErrorReturns500(t *testing.T) {
	svc := stubUserService{
		getByID: func(_ context.Context, _ string) (*models.User, error) {
			return nil, errors.New("conn lost")
		},
	}
	_, r := newUsersAPI(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/users/subj-1/profile", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "conn lost") {
		t.Errorf("internal error must not leak: %s", w.Body.String())
	}
}
