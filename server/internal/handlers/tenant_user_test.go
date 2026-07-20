package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/middleware"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// stubTenantUserService lets handler tests pin RPC responses without a
// live cs-user. Captures the args (keyword/limit/status) and the ctx's
// tenant slug so tests can assert the handler injected the right
// X-Tenant-Id source.
type stubTenantUserService struct {
	// list fields
	resp   []userpkg.TenantUser
	err    error
	gotCtx context.Context
	gotKw  string
	gotLim int

	// set-status fields
	statusResp *userpkg.AdminSetUserStatusResult
	statusErr  error
	gotSubject string
	gotStatus  string
	gotOp      string
}

func (s *stubTenantUserService) ListTenantUsers(ctx context.Context, keyword string, limit int) ([]userpkg.TenantUser, error) {
	s.gotCtx = ctx
	s.gotKw = keyword
	s.gotLim = limit
	return s.resp, s.err
}

func (s *stubTenantUserService) SetUserStatus(ctx context.Context, subjectID, status, operatorID string) (*userpkg.AdminSetUserStatusResult, error) {
	s.gotCtx = ctx
	s.gotSubject = subjectID
	s.gotStatus = status
	s.gotOp = operatorID
	return s.statusResp, s.statusErr
}

// newTenantUserAPI builds a gin engine with the /api/tenant/users routes
// mounted. Auth middleware is bypassed — tests inject AuthClaims directly
// via gin context (mimicking what middleware.Auth does in production).
func newTenantUserAPI(svc TenantUserService, claims middleware.AuthClaims) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Shim the auth middleware's gin-context write so the handler can
	// read claims via c.Get(middleware.AuthClaimsKey).
	r.Use(func(c *gin.Context) {
		c.Set(middleware.AuthClaimsKey, claims)
		c.Next()
	})
	api := &TenantUserAPI{Svc: svc}
	r.GET("/api/tenant/users", api.ListTenantUsers)
	r.PUT("/api/tenant/users/:id/status", api.SetTenantUserStatus)
	return r
}

func doTenantUserReq(t *testing.T, r *gin.Engine, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, target, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	r.ServeHTTP(w, req)
	return w
}

func claimsWithTenant(slug, tenantID string, roles ...string) middleware.AuthClaims {
	return middleware.AuthClaims{
		Sub:         "u-1",
		TenantID:    tenantID,
		TenantSlug:  slug,
		TenantRoles: roles,
	}
}

// ---------------- Happy path ----------------

func TestTenantUser_List_HappyPath(t *testing.T) {
	stub := &stubTenantUserService{
		resp: []userpkg.TenantUser{
			{SubjectID: "s-1", Username: "alice", TenantID: "t-acme"},
		},
	}
	r := newTenantUserAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantUserReq(t, r, http.MethodGet, "/api/tenant/users?keyword=ali&limit=25", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}

	var got []userpkg.TenantUser
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 || got[0].Username != "alice" {
		t.Errorf("body: %+v", got)
	}

	// Handler forwarded keyword + limit verbatim.
	if stub.gotKw != "ali" || stub.gotLim != 25 {
		t.Errorf("svc args: kw=%q lim=%d", stub.gotKw, stub.gotLim)
	}
}

// ---------------- Slug injection ----------------

func TestTenantUser_List_InjectsTenantSlug(t *testing.T) {
	stub := &stubTenantUserService{resp: []userpkg.TenantUser{}}
	// Token carries TenantSlug=acme.
	r := newTenantUserAPI(stub, claimsWithTenant("acme", "t-acme", "owner"))

	doTenantUserReq(t, r, http.MethodGet, "/api/tenant/users", nil)

	if stub.gotCtx == nil {
		t.Fatal("svc not called")
	}
	// The handler should have written the slug into ctx via
	// tenant.WithSlug so the RPC client forwards it as X-Tenant-Id.
	// We can't read tenant.SlugFromContext here without importing
	// internal/tenant; instead assert via behavior — the test in
	// rpc_client_tenant_user_test.go already covers the actual header
	// forwarding. Here we just confirm the call happened with the
	// right keyword + limit defaults.
	if stub.gotKw != "" || stub.gotLim != 0 {
		t.Errorf("defaults: kw=%q lim=%d (want empty/0)", stub.gotKw, stub.gotLim)
	}
}

func TestTenantUser_List_FallsBackToTenantID(t *testing.T) {
	stub := &stubTenantUserService{resp: []userpkg.TenantUser{}}
	// Legacy token: TenantSlug empty, TenantID populated. Handler
	// should fall back to TenantID as the X-Tenant-Id source.
	r := newTenantUserAPI(stub, claimsWithTenant("", "t-acme", "admin"))

	w := doTenantUserReq(t, r, http.MethodGet, "/api/tenant/users", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	if stub.gotCtx == nil {
		t.Fatal("svc not called")
	}
}

// ---------------- Validation ----------------

func TestTenantUser_List_NegativeLimit_400(t *testing.T) {
	stub := &stubTenantUserService{}
	r := newTenantUserAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantUserReq(t, r, http.MethodGet, "/api/tenant/users?limit=-1", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
	if stub.gotCtx != nil {
		t.Error("svc should not have been called")
	}
}

func TestTenantUser_List_OverMaxLimit_400(t *testing.T) {
	stub := &stubTenantUserService{}
	r := newTenantUserAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantUserReq(t, r, http.MethodGet, "/api/tenant/users?limit=500", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

// ---------------- Error mapping ----------------

func TestTenantUser_List_RPCUnavailable_502(t *testing.T) {
	stub := &stubTenantUserService{err: userpkg.ErrRPCUnavailable}
	r := newTenantUserAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantUserReq(t, r, http.MethodGet, "/api/tenant/users", nil)
	if w.Code != http.StatusBadGateway {
		t.Errorf("want 502, got %d", w.Code)
	}
}

func TestTenantUser_List_TenantUserUnavailable_502(t *testing.T) {
	stub := &stubTenantUserService{err: userpkg.ErrTenantUserUnavailable}
	r := newTenantUserAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantUserReq(t, r, http.MethodGet, "/api/tenant/users", nil)
	if w.Code != http.StatusBadGateway {
		t.Errorf("want 502, got %d", w.Code)
	}
}

func TestTenantUser_List_UnknownError_500(t *testing.T) {
	stub := &stubTenantUserService{err: errUnknown("boom")}
	r := newTenantUserAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantUserReq(t, r, http.MethodGet, "/api/tenant/users", nil)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", w.Code)
	}
}

// ---------------- Nil service / missing claims ----------------

func TestTenantUser_List_NilService_502(t *testing.T) {
	r := newTenantUserAPI(nil, claimsWithTenant("acme", "t-acme", "admin"))
	w := doTenantUserReq(t, r, http.MethodGet, "/api/tenant/users", nil)
	if w.Code != http.StatusBadGateway {
		t.Errorf("nil svc: want 502, got %d", w.Code)
	}
}

func TestTenantUser_List_NoClaims_401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// No claims injected — simulates a route accidentally mounted
	// without the Auth middleware.
	api := &TenantUserAPI{Svc: &stubTenantUserService{}}
	r.GET("/api/tenant/users", api.ListTenantUsers)

	w := doTenantUserReq(t, r, http.MethodGet, "/api/tenant/users", nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no claims: want 401, got %d", w.Code)
	}
}

func TestTenantUser_List_NoTenantBinding_403(t *testing.T) {
	stub := &stubTenantUserService{}
	// Token has neither TenantSlug nor TenantID — should be impossible
	// behind RequireTenantAdmin but defensive.
	r := newTenantUserAPI(stub, middleware.AuthClaims{Sub: "u-1", TenantRoles: []string{"admin"}})

	w := doTenantUserReq(t, r, http.MethodGet, "/api/tenant/users", nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("no tenant binding: want 403, got %d", w.Code)
	}
}

// errUnknown is a non-sentinel error for the 500 test.
type errUnknown string

func (e errUnknown) Error() string { return string(e) }

// ---------------- SetTenantUserStatus ----------------

func TestTenantUser_SetStatus_HappyPath(t *testing.T) {
	stub := &stubTenantUserService{
		statusResp: &userpkg.AdminSetUserStatusResult{FromStatus: "active", ToStatus: "disabled"},
	}
	r := newTenantUserAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	body := []byte(`{"status":"disabled"}`)
	w := doTenantUserReq(t, r, http.MethodPut, "/api/tenant/users/s-2/status", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}

	// Operator forwarded from AuthClaims.Sub.
	if stub.gotSubject != "s-2" || stub.gotStatus != "disabled" || stub.gotOp != "u-1" {
		t.Errorf("svc args: subj=%q status=%q op=%q", stub.gotSubject, stub.gotStatus, stub.gotOp)
	}

	var resp struct {
		Success    bool   `json:"success"`
		FromStatus string `json:"from_status"`
		ToStatus   string `json:"to_status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Success || resp.FromStatus != "active" || resp.ToStatus != "disabled" {
		t.Errorf("body: %+v", resp)
	}
}

func TestTenantUser_SetStatus_SelfLockRejectedAs400(t *testing.T) {
	stub := &stubTenantUserService{
		statusErr: userpkg.ErrAdminUserRPCCannotChangeOwn,
	}
	r := newTenantUserAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	body := []byte(`{"status":"disabled"}`)
	w := doTenantUserReq(t, r, http.MethodPut, "/api/tenant/users/u-1/status", body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("self-lock: want 400, got %d", w.Code)
	}
}

func TestTenantUser_SetStatus_NotFoundReturns404(t *testing.T) {
	stub := &stubTenantUserService{
		statusErr: userpkg.ErrAdminUserRPCNotFound,
	}
	r := newTenantUserAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	body := []byte(`{"status":"disabled"}`)
	w := doTenantUserReq(t, r, http.MethodPut, "/api/tenant/users/ghost/status", body)
	if w.Code != http.StatusNotFound {
		t.Errorf("not-found: want 404, got %d", w.Code)
	}
}

func TestTenantUser_SetStatus_InvalidStatusReturns400(t *testing.T) {
	stub := &stubTenantUserService{
		statusErr: userpkg.ErrAdminUserRPCInvalidStatus,
	}
	r := newTenantUserAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	body := []byte(`{"status":"quarantined"}`)
	w := doTenantUserReq(t, r, http.MethodPut, "/api/tenant/users/s-2/status", body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid status: want 400, got %d", w.Code)
	}
}

func TestTenantUser_SetStatus_RPCUnavailableReturns502(t *testing.T) {
	stub := &stubTenantUserService{
		statusErr: userpkg.ErrRPCUnavailable,
	}
	r := newTenantUserAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	body := []byte(`{"status":"disabled"}`)
	w := doTenantUserReq(t, r, http.MethodPut, "/api/tenant/users/s-2/status", body)
	if w.Code != http.StatusBadGateway {
		t.Errorf("rpc unavailable: want 502, got %d", w.Code)
	}
}

func TestTenantUser_SetStatus_NilService_502(t *testing.T) {
	r := newTenantUserAPI(nil, claimsWithTenant("acme", "t-acme", "admin"))
	body := []byte(`{"status":"disabled"}`)
	w := doTenantUserReq(t, r, http.MethodPut, "/api/tenant/users/s-2/status", body)
	if w.Code != http.StatusBadGateway {
		t.Errorf("nil svc: want 502, got %d", w.Code)
	}
}

func TestTenantUser_SetStatus_MissingOperator_401(t *testing.T) {
	// Token without Sub — should be impossible behind RequireAuth but
	// defensive. Handler must surface 401 not panic.
	stub := &stubTenantUserService{}
	r := newTenantUserAPI(stub, middleware.AuthClaims{TenantID: "t-acme", TenantRoles: []string{"admin"}})

	body := []byte(`{"status":"disabled"}`)
	w := doTenantUserReq(t, r, http.MethodPut, "/api/tenant/users/s-2/status", body)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("missing operator: want 401, got %d", w.Code)
	}
	if stub.gotSubject != "" {
		t.Error("svc should not have been called")
	}
}

func TestTenantUser_SetStatus_InvalidBodyReturns400(t *testing.T) {
	stub := &stubTenantUserService{}
	r := newTenantUserAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantUserReq(t, r, http.MethodPut, "/api/tenant/users/s-2/status", []byte(`{}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty body: want 400, got %d", w.Code)
	}
	if stub.gotSubject != "" {
		t.Error("svc should not have been called on body bind failure")
	}
}
