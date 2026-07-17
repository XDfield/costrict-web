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

// stubTenantConfigRPC lets handler tests pin RPC responses without a
// live cs-user. Captures the args (ctx / yaml / actor) so tests can
// assert the handler injected the right X-Tenant-Id source + actor.
type stubTenantConfigRPC struct {
	getResp    *userpkg.TenantConfig
	getErr     error
	updateResp *userpkg.TenantConfig
	updateErr  error

	gotCtx       context.Context
	gotYAML      string
	gotActor     string
	updateCalled bool
}

func (s *stubTenantConfigRPC) GetTenantConfig(ctx context.Context) (*userpkg.TenantConfig, error) {
	s.gotCtx = ctx
	return s.getResp, s.getErr
}

func (s *stubTenantConfigRPC) UpdateTenantConfig(ctx context.Context, yamlStr, actorSubjectID string) (*userpkg.TenantConfig, error) {
	s.gotCtx = ctx
	s.gotYAML = yamlStr
	s.gotActor = actorSubjectID
	s.updateCalled = true
	return s.updateResp, s.updateErr
}

// newTenantConfigAPI builds a gin engine with both /api/tenant/config
// routes. Auth middleware is bypassed — tests inject AuthClaims directly
// via gin context (mimicking what middleware.Auth does in production).
func newTenantConfigAPI(svc TenantConfigService, claims middleware.AuthClaims) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.AuthClaimsKey, claims)
		c.Next()
	})
	api := &TenantConfigAPI{Svc: svc}
	g := r.Group("/api/tenant/config")
	g.GET("", api.GetTenantConfig)
	g.PUT("", api.UpdateTenantConfig)
	return r
}

func doTenantConfigReq(t *testing.T, r *gin.Engine, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		buf, _ := json.Marshal(body)
		req = httptest.NewRequest(method, target, bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---------------- GetTenantConfig ----------------

func TestTenantConfig_Get_HappyPath(t *testing.T) {
	stub := &stubTenantConfigRPC{
		getResp: &userpkg.TenantConfig{TenantID: "t-acme", ConfigYAML: "key: value"},
	}
	r := newTenantConfigAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantConfigReq(t, r, http.MethodGet, "/api/tenant/config", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	var got userpkg.TenantConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TenantID != "t-acme" || got.ConfigYAML != "key: value" {
		t.Errorf("body: %+v", got)
	}
	if stub.gotCtx == nil {
		t.Error("svc not called")
	}
}

func TestTenantConfig_Get_FallsBackToTenantID(t *testing.T) {
	stub := &stubTenantConfigRPC{
		getResp: &userpkg.TenantConfig{TenantID: "t-acme", ConfigYAML: "{}"},
	}
	// Legacy token: no TenantSlug, but TenantID populated.
	r := newTenantConfigAPI(stub, claimsWithTenant("", "t-acme", "admin"))

	w := doTenantConfigReq(t, r, http.MethodGet, "/api/tenant/config", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestTenantConfig_Get_RPCUnavailable_502(t *testing.T) {
	stub := &stubTenantConfigRPC{getErr: userpkg.ErrRPCUnavailable}
	r := newTenantConfigAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantConfigReq(t, r, http.MethodGet, "/api/tenant/config", nil)
	if w.Code != http.StatusBadGateway {
		t.Errorf("want 502, got %d", w.Code)
	}
}

func TestTenantConfig_Get_TenantConfigUnavailable_502(t *testing.T) {
	stub := &stubTenantConfigRPC{getErr: userpkg.ErrTenantConfigUnavailable}
	r := newTenantConfigAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantConfigReq(t, r, http.MethodGet, "/api/tenant/config", nil)
	if w.Code != http.StatusBadGateway {
		t.Errorf("want 502, got %d", w.Code)
	}
}

func TestTenantConfig_Get_NilService_502(t *testing.T) {
	r := newTenantConfigAPI(nil, claimsWithTenant("acme", "t-acme", "admin"))
	w := doTenantConfigReq(t, r, http.MethodGet, "/api/tenant/config", nil)
	if w.Code != http.StatusBadGateway {
		t.Errorf("nil svc: want 502, got %d", w.Code)
	}
}

func TestTenantConfig_Get_NoClaims_401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := &TenantConfigAPI{Svc: &stubTenantConfigRPC{}}
	r.GET("/api/tenant/config", api.GetTenantConfig)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/tenant/config", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no claims: want 401, got %d", w.Code)
	}
}

func TestTenantConfig_Get_NoTenantBinding_403(t *testing.T) {
	stub := &stubTenantConfigRPC{}
	// Token has neither TenantSlug nor TenantID.
	r := newTenantConfigAPI(stub, middleware.AuthClaims{Sub: "u-1", TenantRoles: []string{"admin"}})

	w := doTenantConfigReq(t, r, http.MethodGet, "/api/tenant/config", nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("no tenant binding: want 403, got %d", w.Code)
	}
}

// ---------------- UpdateTenantConfig ----------------

func TestTenantConfig_Update_HappyPath(t *testing.T) {
	stub := &stubTenantConfigRPC{
		updateResp: &userpkg.TenantConfig{TenantID: "t-acme", ConfigYAML: "key: value", UpdatedBy: strPtrHelper("u-1")},
	}
	claims := claimsWithTenant("acme", "t-acme", "admin")
	claims.Sub = "u-1"
	r := newTenantConfigAPI(stub, claims)

	w := doTenantConfigReq(t, r, http.MethodPut, "/api/tenant/config",
		tenantConfigUpdateRequest{ConfigYAML: "key: value"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	if stub.gotYAML != "key: value" {
		t.Errorf("yaml: got %q want %q", stub.gotYAML, "key: value")
	}
	if stub.gotActor != "u-1" {
		t.Errorf("actor: got %q want u-1", stub.gotActor)
	}
}

func TestTenantConfig_Update_EmptySub_OmitsActor(t *testing.T) {
	stub := &stubTenantConfigRPC{
		updateResp: &userpkg.TenantConfig{TenantID: "t-acme", ConfigYAML: "{}"},
	}
	// Sub empty — handler should forward "" as the actor.
	claims := claimsWithTenant("acme", "t-acme", "admin")
	claims.Sub = ""
	r := newTenantConfigAPI(stub, claims)

	w := doTenantConfigReq(t, r, http.MethodPut, "/api/tenant/config",
		tenantConfigUpdateRequest{ConfigYAML: "{}"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	if stub.gotActor != "" {
		t.Errorf("actor: got %q want empty", stub.gotActor)
	}
}

func TestTenantConfig_Update_MissingBody_400(t *testing.T) {
	stub := &stubTenantConfigRPC{}
	r := newTenantConfigAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantConfigReq(t, r, http.MethodPut, "/api/tenant/config", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
	if stub.updateCalled {
		t.Error("svc should not have been called on missing body")
	}
}

func TestTenantConfig_Update_InvalidYAML_400(t *testing.T) {
	stub := &stubTenantConfigRPC{updateErr: userpkg.ErrInvalidYAML}
	r := newTenantConfigAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantConfigReq(t, r, http.MethodPut, "/api/tenant/config",
		tenantConfigUpdateRequest{ConfigYAML: "x"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestTenantConfig_Update_TooLarge_413(t *testing.T) {
	stub := &stubTenantConfigRPC{updateErr: userpkg.ErrYAMLTooLarge}
	r := newTenantConfigAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantConfigReq(t, r, http.MethodPut, "/api/tenant/config",
		tenantConfigUpdateRequest{ConfigYAML: "x"})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("want 413, got %d", w.Code)
	}
}

func TestTenantConfig_Update_RPCUnavailable_502(t *testing.T) {
	stub := &stubTenantConfigRPC{updateErr: userpkg.ErrRPCUnavailable}
	r := newTenantConfigAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantConfigReq(t, r, http.MethodPut, "/api/tenant/config",
		tenantConfigUpdateRequest{ConfigYAML: "x"})
	if w.Code != http.StatusBadGateway {
		t.Errorf("want 502, got %d", w.Code)
	}
}

func TestTenantConfig_Update_UnknownError_500(t *testing.T) {
	stub := &stubTenantConfigRPC{updateErr: errUnknown("boom")}
	r := newTenantConfigAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doTenantConfigReq(t, r, http.MethodPut, "/api/tenant/config",
		tenantConfigUpdateRequest{ConfigYAML: "x"})
	if w.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", w.Code)
	}
}

// strPtrHelper is the local equivalent of the strPtr in
// tenant_user_test.go's package — keeping the test file self-contained
// avoids a cross-file symbol dependency.
func strPtrHelper(s string) *string { return &s }
