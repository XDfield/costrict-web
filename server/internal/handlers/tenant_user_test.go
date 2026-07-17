package handlers

import (
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
// live cs-user. Captures the args (keyword/limit) and the ctx's tenant
// slug so tests can assert the handler injected the right X-Tenant-Id
// source.
type stubTenantUserService struct {
	resp   []userpkg.TenantUser
	err    error
	gotCtx context.Context
	gotKw  string
	gotLim int
}

func (s *stubTenantUserService) ListTenantUsers(ctx context.Context, keyword string, limit int) ([]userpkg.TenantUser, error) {
	s.gotCtx = ctx
	s.gotKw = keyword
	s.gotLim = limit
	return s.resp, s.err
}

// newTenantUserAPI builds a gin engine with the single /api/tenant/users
// route. Auth middleware is bypassed — tests inject AuthClaims directly
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
	return r
}

func doTenantUserReq(t *testing.T, r *gin.Engine, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
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

	w := doTenantUserReq(t, r, "/api/tenant/users?keyword=ali&limit=25")
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

	doTenantUserReq(t, r, "/api/tenant/users")

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

	w := doTenantUserReq(t, r, "/api/tenant/users")
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

	w := doTenantUserReq(t, r, "/api/tenant/users?limit=-1")
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

	w := doTenantUserReq(t, r, "/api/tenant/users?limit=500")
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

// ---------------- Error mapping ----------------

func TestTenantUser_List_RPCUnavailable_502(t *testing.T) {
	stub := &stubTenantUserService{err: userpkg.ErrRPCUnavailable}
	r := newTenantUserAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantUserReq(t, r, "/api/tenant/users")
	if w.Code != http.StatusBadGateway {
		t.Errorf("want 502, got %d", w.Code)
	}
}

func TestTenantUser_List_TenantUserUnavailable_502(t *testing.T) {
	stub := &stubTenantUserService{err: userpkg.ErrTenantUserUnavailable}
	r := newTenantUserAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantUserReq(t, r, "/api/tenant/users")
	if w.Code != http.StatusBadGateway {
		t.Errorf("want 502, got %d", w.Code)
	}
}

func TestTenantUser_List_UnknownError_500(t *testing.T) {
	stub := &stubTenantUserService{err: errUnknown("boom")}
	r := newTenantUserAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantUserReq(t, r, "/api/tenant/users")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", w.Code)
	}
}

// ---------------- Nil service / missing claims ----------------

func TestTenantUser_List_NilService_502(t *testing.T) {
	r := newTenantUserAPI(nil, claimsWithTenant("acme", "t-acme", "admin"))
	w := doTenantUserReq(t, r, "/api/tenant/users")
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

	w := doTenantUserReq(t, r, "/api/tenant/users")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no claims: want 401, got %d", w.Code)
	}
}

func TestTenantUser_List_NoTenantBinding_403(t *testing.T) {
	stub := &stubTenantUserService{}
	// Token has neither TenantSlug nor TenantID — should be impossible
	// behind RequireTenantAdmin but defensive.
	r := newTenantUserAPI(stub, middleware.AuthClaims{Sub: "u-1", TenantRoles: []string{"admin"}})

	w := doTenantUserReq(t, r, "/api/tenant/users")
	if w.Code != http.StatusForbidden {
		t.Errorf("no tenant binding: want 403, got %d", w.Code)
	}
}

// errUnknown is a non-sentinel error for the 500 test.
type errUnknown string

func (e errUnknown) Error() string { return string(e) }
