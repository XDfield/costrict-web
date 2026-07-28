package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenantconfig"
	"github.com/gin-gonic/gin"
)

// stubProviderMappingService pins service responses for handler tests.
type stubProviderMappingService struct {
	get     func(context.Context, string) (*tenantconfig.ProviderMapping, error)
	update  func(context.Context, tenantconfig.UpdateProviderMappingParams) (*tenantconfig.ProviderMapping, error)
	gotArg  *tenantconfig.UpdateProviderMappingParams
	gotArgT string
}

func (s *stubProviderMappingService) GetProviderMapping(ctx context.Context, tenantID string) (*tenantconfig.ProviderMapping, error) {
	s.gotArgT = tenantID
	if s.get == nil {
		panic("stubProviderMappingService.get not wired")
	}
	return s.get(ctx, tenantID)
}

func (s *stubProviderMappingService) UpdateProviderMapping(ctx context.Context, p tenantconfig.UpdateProviderMappingParams) (*tenantconfig.ProviderMapping, error) {
	s.gotArg = &p
	if s.update == nil {
		panic("stubProviderMappingService.update not wired")
	}
	return s.update(ctx, p)
}

// newProviderMappingAPI builds a gin engine with both
// /api/internal/tenant/provider-mapping routes. The shim mirrors
// middleware.ResolveTenant by setting the "tenant" context key.
func newProviderMappingAPI(svc TenantProviderMappingService, resolvedTenant *models.Tenant) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if resolvedTenant != nil {
			c.Set("tenant", resolvedTenant)
		}
		c.Next()
	})
	api := &TenantProviderMappingAPI{Svc: svc}
	g := r.Group("/api/internal/tenant/provider-mapping")
	g.GET("", api.GetProviderMapping)
	g.PUT("", api.UpdateProviderMapping)
	return r
}

const providerMappingBase = "/api/internal/tenant/provider-mapping"

// ptr helpers local to this test file (handlers package has no stdlib
// equivalent). intPtr signature mirrors the one in tenantconfig tests.
func hpBoolPtr(b bool) *bool { return &b }
func hpIntPtr(n int) *int    { return &n }

// ---------------- GetProviderMapping ----------------

func TestGetProviderMapping_HappyPath(t *testing.T) {
	stub := &stubProviderMappingService{
		get: func(_ context.Context, id string) (*tenantconfig.ProviderMapping, error) {
			if id != "t-acme" {
				t.Errorf("tenant id: got %q want t-acme", id)
			}
			return &tenantconfig.ProviderMapping{
				Providers: map[string]tenantconfig.Provider{
					"ldap": {Enabled: hpBoolPtr(true), Rank: hpIntPtr(200)},
				},
			}, nil
		},
	}
	r := newProviderMappingAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})

	w := doJSON(t, r, http.MethodGet, providerMappingBase, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	var got tenantconfig.ProviderMapping
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Providers) != 1 {
		t.Errorf("providers: got %d want 1", len(got.Providers))
	}
}

func TestGetProviderMapping_NoResolvedTenant_400(t *testing.T) {
	stub := &stubProviderMappingService{
		get: func(context.Context, string) (*tenantconfig.ProviderMapping, error) {
			t.Fatal("service should not be called when tenant unresolved")
			return nil, nil
		},
	}
	r := newProviderMappingAPI(stub, nil)

	w := doJSON(t, r, http.MethodGet, providerMappingBase, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestGetProviderMapping_NilService_503(t *testing.T) {
	r := newProviderMappingAPI(nil, &models.Tenant{TenantID: "t-acme", Slug: "acme"})
	w := doJSON(t, r, http.MethodGet, providerMappingBase, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("nil svc: want 503, got %d", w.Code)
	}
}

func TestGetProviderMapping_ServiceError_500(t *testing.T) {
	stub := &stubProviderMappingService{
		get: func(context.Context, string) (*tenantconfig.ProviderMapping, error) {
			return nil, errors.New("db down")
		},
	}
	r := newProviderMappingAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})

	w := doJSON(t, r, http.MethodGet, providerMappingBase, nil)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", w.Code)
	}
}

func TestGetProviderMapping_InvalidYAMLInStored_400(t *testing.T) {
	// Rare: stored blob somehow malformed. Sentinel propagates as 400.
	stub := &stubProviderMappingService{
		get: func(context.Context, string) (*tenantconfig.ProviderMapping, error) {
			return nil, tenantconfig.ErrInvalidYAML
		},
	}
	r := newProviderMappingAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})

	w := doJSON(t, r, http.MethodGet, providerMappingBase, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

// ---------------- UpdateProviderMapping ----------------

func TestUpdateProviderMapping_HappyPath(t *testing.T) {
	stub := &stubProviderMappingService{
		update: func(_ context.Context, p tenantconfig.UpdateProviderMappingParams) (*tenantconfig.ProviderMapping, error) {
			if p.TenantID != "t-acme" {
				t.Errorf("tenant id: got %q want t-acme", p.TenantID)
			}
			if p.UpdatedBy == nil || *p.UpdatedBy != "subj-1" {
				t.Errorf("actor: got %v want subj-1", p.UpdatedBy)
			}
			if _, ok := p.Mapping.Providers["ldap"]; !ok {
				t.Errorf("mapping: ldap missing")
			}
			return p.Mapping, nil
		},
	}
	r := newProviderMappingAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})

	body := tenantconfig.ProviderMapping{
		Providers: map[string]tenantconfig.Provider{
			"ldap": {Rank: hpIntPtr(200)},
		},
	}
	req := newRequestWithActor(http.MethodPut, providerMappingBase, body, "subj-1")
	w := httptestRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestUpdateProviderMapping_NoActorHeader_NilActor(t *testing.T) {
	stub := &stubProviderMappingService{
		update: func(_ context.Context, p tenantconfig.UpdateProviderMappingParams) (*tenantconfig.ProviderMapping, error) {
			if p.UpdatedBy != nil {
				t.Errorf("actor should be nil, got %v", *p.UpdatedBy)
			}
			return p.Mapping, nil
		},
	}
	r := newProviderMappingAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})

	body := tenantconfig.ProviderMapping{Providers: map[string]tenantconfig.Provider{}}
	w := doJSON(t, r, http.MethodPut, providerMappingBase, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestUpdateProviderMapping_MalformedBody_400(t *testing.T) {
	stub := &stubProviderMappingService{
		update: func(context.Context, tenantconfig.UpdateProviderMappingParams) (*tenantconfig.ProviderMapping, error) {
			t.Fatal("service should not be called on malformed body")
			return nil, nil
		},
	}
	r := newProviderMappingAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})

	// providers as a string instead of a map → JSON decode fails ShouldBindJSON.
	w := doJSON(t, r, http.MethodPut, providerMappingBase, map[string]any{"providers": "not-a-map"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestUpdateProviderMapping_InvalidProviderName_400(t *testing.T) {
	stub := &stubProviderMappingService{
		update: func(_ context.Context, _ tenantconfig.UpdateProviderMappingParams) (*tenantconfig.ProviderMapping, error) {
			return nil, tenantconfig.ErrProviderNameInvalid
		},
	}
	r := newProviderMappingAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})

	body := tenantconfig.ProviderMapping{Providers: map[string]tenantconfig.Provider{"LDAP": {}}}
	w := doJSON(t, r, http.MethodPut, providerMappingBase, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestUpdateProviderMapping_InvalidInterval_400(t *testing.T) {
	stub := &stubProviderMappingService{
		update: func(_ context.Context, _ tenantconfig.UpdateProviderMappingParams) (*tenantconfig.ProviderMapping, error) {
			return nil, tenantconfig.ErrIntervalInvalid
		},
	}
	r := newProviderMappingAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})

	body := tenantconfig.ProviderMapping{Providers: map[string]tenantconfig.Provider{
		"ldap": {EnterpriseSync: &tenantconfig.EnterpriseSync{Interval: "abc"}},
	}}
	w := doJSON(t, r, http.MethodPut, providerMappingBase, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestUpdateProviderMapping_NegativeRank_400(t *testing.T) {
	stub := &stubProviderMappingService{
		update: func(_ context.Context, _ tenantconfig.UpdateProviderMappingParams) (*tenantconfig.ProviderMapping, error) {
			return nil, tenantconfig.ErrRankNegative
		},
	}
	r := newProviderMappingAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})

	body := tenantconfig.ProviderMapping{Providers: map[string]tenantconfig.Provider{
		"ldap": {Rank: hpIntPtr(-1)},
	}}
	w := doJSON(t, r, http.MethodPut, providerMappingBase, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestUpdateProviderMapping_NoResolvedTenant_400(t *testing.T) {
	stub := &stubProviderMappingService{
		update: func(context.Context, tenantconfig.UpdateProviderMappingParams) (*tenantconfig.ProviderMapping, error) {
			t.Fatal("service should not be called when tenant unresolved")
			return nil, nil
		},
	}
	r := newProviderMappingAPI(stub, nil)

	body := tenantconfig.ProviderMapping{Providers: map[string]tenantconfig.Provider{}}
	w := doJSON(t, r, http.MethodPut, providerMappingBase, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestUpdateProviderMapping_NilService_503(t *testing.T) {
	r := newProviderMappingAPI(nil, &models.Tenant{TenantID: "t-acme", Slug: "acme"})
	body := tenantconfig.ProviderMapping{Providers: map[string]tenantconfig.Provider{}}
	w := doJSON(t, r, http.MethodPut, providerMappingBase, body)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("nil svc: want 503, got %d", w.Code)
	}
}

func TestUpdateProviderMapping_UnknownError_500(t *testing.T) {
	stub := &stubProviderMappingService{
		update: func(context.Context, tenantconfig.UpdateProviderMappingParams) (*tenantconfig.ProviderMapping, error) {
			return nil, errors.New("boom")
		},
	}
	r := newProviderMappingAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})

	body := tenantconfig.ProviderMapping{Providers: map[string]tenantconfig.Provider{}}
	w := doJSON(t, r, http.MethodPut, providerMappingBase, body)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", w.Code)
	}
}
