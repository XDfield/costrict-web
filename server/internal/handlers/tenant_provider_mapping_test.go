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

// stubProviderMappingRPC lets handler tests pin RPC responses.
type stubProviderMappingRPC struct {
	getResp    *userpkg.ProviderMapping
	getErr     error
	updateResp *userpkg.ProviderMapping
	updateErr  error

	gotCtx       context.Context
	gotMapping   *userpkg.ProviderMapping
	gotActor     string
	updateCalled bool
}

func (s *stubProviderMappingRPC) GetProviderMapping(ctx context.Context) (*userpkg.ProviderMapping, error) {
	s.gotCtx = ctx
	return s.getResp, s.getErr
}

func (s *stubProviderMappingRPC) UpdateProviderMapping(ctx context.Context, mapping *userpkg.ProviderMapping, actorSubjectID string) (*userpkg.ProviderMapping, error) {
	s.gotCtx = ctx
	s.gotMapping = mapping
	s.gotActor = actorSubjectID
	s.updateCalled = true
	return s.updateResp, s.updateErr
}

func newProviderMappingAPI(svc TenantProviderMappingService, claims middleware.AuthClaims) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.AuthClaimsKey, claims)
		c.Next()
	})
	api := &TenantProviderMappingAPI{Svc: svc}
	g := r.Group("/api/tenant/provider-mapping")
	g.GET("", api.GetProviderMapping)
	g.PUT("", api.UpdateProviderMapping)
	return r
}

func doProviderMappingReq(t *testing.T, r *gin.Engine, method, target string, body any) *httptest.ResponseRecorder {
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

func pmBool(b bool) *bool { return &b }
func pmInt(n int) *int    { return &n }

// ---------------- GetProviderMapping ----------------

func TestProviderMapping_Get_HappyPath(t *testing.T) {
	stub := &stubProviderMappingRPC{
		getResp: &userpkg.ProviderMapping{Providers: map[string]userpkg.Provider{
			"ldap": {Enabled: pmBool(true), Rank: pmInt(200)},
		}},
	}
	r := newProviderMappingAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doProviderMappingReq(t, r, http.MethodGet, "/api/tenant/provider-mapping", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	var got userpkg.ProviderMapping
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Providers) != 1 {
		t.Errorf("providers: got %d want 1", len(got.Providers))
	}
}

func TestProviderMapping_Get_FallsBackToTenantID(t *testing.T) {
	stub := &stubProviderMappingRPC{
		getResp: &userpkg.ProviderMapping{Providers: map[string]userpkg.Provider{}},
	}
	r := newProviderMappingAPI(stub, claimsWithTenant("", "t-acme", "admin"))

	w := doProviderMappingReq(t, r, http.MethodGet, "/api/tenant/provider-mapping", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestProviderMapping_Get_RPCUnavailable_502(t *testing.T) {
	stub := &stubProviderMappingRPC{getErr: userpkg.ErrRPCUnavailable}
	r := newProviderMappingAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	w := doProviderMappingReq(t, r, http.MethodGet, "/api/tenant/provider-mapping", nil)
	if w.Code != http.StatusBadGateway {
		t.Errorf("want 502, got %d", w.Code)
	}
}

func TestProviderMapping_Get_NilService_502(t *testing.T) {
	r := newProviderMappingAPI(nil, claimsWithTenant("acme", "t-acme", "admin"))
	w := doProviderMappingReq(t, r, http.MethodGet, "/api/tenant/provider-mapping", nil)
	if w.Code != http.StatusBadGateway {
		t.Errorf("nil svc: want 502, got %d", w.Code)
	}
}

func TestProviderMapping_Get_NoClaims_401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := &TenantProviderMappingAPI{Svc: &stubProviderMappingRPC{}}
	r.GET("/api/tenant/provider-mapping", api.GetProviderMapping)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/tenant/provider-mapping", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no claims: want 401, got %d", w.Code)
	}
}

func TestProviderMapping_Get_NoTenantBinding_403(t *testing.T) {
	stub := &stubProviderMappingRPC{}
	r := newProviderMappingAPI(stub, middleware.AuthClaims{Sub: "u-1", TenantRoles: []string{"admin"}})

	w := doProviderMappingReq(t, r, http.MethodGet, "/api/tenant/provider-mapping", nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("no tenant binding: want 403, got %d", w.Code)
	}
}

// ---------------- UpdateProviderMapping ----------------

func TestProviderMapping_Update_HappyPath(t *testing.T) {
	stub := &stubProviderMappingRPC{
		updateResp: &userpkg.ProviderMapping{Providers: map[string]userpkg.Provider{
			"ldap": {Enabled: pmBool(true), Rank: pmInt(200)},
		}},
	}
	claims := claimsWithTenant("acme", "t-acme", "admin")
	claims.Sub = "u-1"
	r := newProviderMappingAPI(stub, claims)

	body := userpkg.ProviderMapping{Providers: map[string]userpkg.Provider{
		"ldap": {Rank: pmInt(200)},
	}}
	w := doProviderMappingReq(t, r, http.MethodPut, "/api/tenant/provider-mapping", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	if stub.gotActor != "u-1" {
		t.Errorf("actor: got %q want u-1", stub.gotActor)
	}
	if stub.gotMapping == nil || len(stub.gotMapping.Providers) != 1 {
		t.Errorf("mapping: %+v", stub.gotMapping)
	}
}

func TestProviderMapping_Update_EmptySub_OmitsActor(t *testing.T) {
	stub := &stubProviderMappingRPC{
		updateResp: &userpkg.ProviderMapping{Providers: map[string]userpkg.Provider{}},
	}
	claims := claimsWithTenant("acme", "t-acme", "admin")
	claims.Sub = ""
	r := newProviderMappingAPI(stub, claims)

	body := userpkg.ProviderMapping{Providers: map[string]userpkg.Provider{}}
	w := doProviderMappingReq(t, r, http.MethodPut, "/api/tenant/provider-mapping", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	if stub.gotActor != "" {
		t.Errorf("actor: got %q want empty", stub.gotActor)
	}
}

func TestProviderMapping_Update_MalformedBody_400(t *testing.T) {
	stub := &stubProviderMappingRPC{}
	r := newProviderMappingAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	// providers as a string instead of map → ShouldBindJSON fails.
	w := doProviderMappingReq(t, r, http.MethodPut, "/api/tenant/provider-mapping",
		map[string]any{"providers": "not-a-map"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
	if stub.updateCalled {
		t.Error("svc should not have been called on malformed body")
	}
}

func TestProviderMapping_Update_InvalidProviderName_400(t *testing.T) {
	stub := &stubProviderMappingRPC{updateErr: userpkg.ErrProviderNameInvalid}
	r := newProviderMappingAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	body := userpkg.ProviderMapping{Providers: map[string]userpkg.Provider{"LDAP": {}}}
	w := doProviderMappingReq(t, r, http.MethodPut, "/api/tenant/provider-mapping", body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestProviderMapping_Update_InvalidInterval_400(t *testing.T) {
	stub := &stubProviderMappingRPC{updateErr: userpkg.ErrIntervalInvalid}
	r := newProviderMappingAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	body := userpkg.ProviderMapping{Providers: map[string]userpkg.Provider{
		"ldap": {EnterpriseSync: &userpkg.EnterpriseSync{Interval: "abc"}},
	}}
	w := doProviderMappingReq(t, r, http.MethodPut, "/api/tenant/provider-mapping", body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestProviderMapping_Update_NegativeRank_400(t *testing.T) {
	stub := &stubProviderMappingRPC{updateErr: userpkg.ErrRankNegative}
	r := newProviderMappingAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	body := userpkg.ProviderMapping{Providers: map[string]userpkg.Provider{
		"ldap": {Rank: pmInt(-1)},
	}}
	w := doProviderMappingReq(t, r, http.MethodPut, "/api/tenant/provider-mapping", body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestProviderMapping_Update_RPCUnavailable_502(t *testing.T) {
	stub := &stubProviderMappingRPC{updateErr: userpkg.ErrRPCUnavailable}
	r := newProviderMappingAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	body := userpkg.ProviderMapping{Providers: map[string]userpkg.Provider{}}
	w := doProviderMappingReq(t, r, http.MethodPut, "/api/tenant/provider-mapping", body)
	if w.Code != http.StatusBadGateway {
		t.Errorf("want 502, got %d", w.Code)
	}
}

func TestProviderMapping_Update_UnknownError_500(t *testing.T) {
	stub := &stubProviderMappingRPC{updateErr: errUnknown("boom")}
	r := newProviderMappingAPI(stub, claimsWithTenant("acme", "t-acme", "admin"))

	body := userpkg.ProviderMapping{Providers: map[string]userpkg.Provider{}}
	w := doProviderMappingReq(t, r, http.MethodPut, "/api/tenant/provider-mapping", body)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", w.Code)
	}
}
