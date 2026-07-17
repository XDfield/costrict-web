package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenantconfig"
	"github.com/gin-gonic/gin"
)

// stubTenantConfigService lets handler tests pin service responses.
type stubTenantConfigService struct {
	get    func(context.Context, string) (*models.TenantConfig, error)
	update func(context.Context, tenantconfig.UpdateParams) (*models.TenantConfig, error)

	gotTenantID string
	gotParams   *tenantconfig.UpdateParams
}

func (s *stubTenantConfigService) Get(ctx context.Context, tenantID string) (*models.TenantConfig, error) {
	s.gotTenantID = tenantID
	if s.get == nil {
		panic("stubTenantConfigService.get not wired")
	}
	return s.get(ctx, tenantID)
}

func (s *stubTenantConfigService) Update(ctx context.Context, p tenantconfig.UpdateParams) (*models.TenantConfig, error) {
	s.gotParams = &p
	s.gotTenantID = p.TenantID
	if s.update == nil {
		panic("stubTenantConfigService.update not wired")
	}
	return s.update(ctx, p)
}

// newTenantConfigAPI builds a gin engine with both /api/internal/tenant/config
// routes. A gin middleware shim injects the resolved tenant via c.Set("tenant",
// ...) — mirroring what middleware.ResolveTenant does in production.
func newTenantConfigAPI(svc TenantConfigService, resolvedTenant *models.Tenant) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if resolvedTenant != nil {
			c.Set("tenant", resolvedTenant)
		}
		c.Next()
	})
	api := &TenantConfigAPI{Svc: svc}
	g := r.Group("/api/internal/tenant/config")
	g.GET("", api.GetTenantConfig)
	g.PUT("", api.UpdateTenantConfig)
	return r
}

const tenantConfigBase = "/api/internal/tenant/config"

func tenantPtr(tn models.Tenant) *models.Tenant { return &tn }

// ---------------- GetTenantConfig ----------------

func TestGetTenantConfig_HappyPath(t *testing.T) {
	stub := &stubTenantConfigService{
		get: func(_ context.Context, id string) (*models.TenantConfig, error) {
			if id != "t-acme" {
				t.Errorf("tenant id: got %q want t-acme", id)
			}
			return &models.TenantConfig{
				TenantID:   "t-acme",
				ConfigYAML: "employment_providers:\n  enabled: [wxwork]",
			}, nil
		},
	}
	r := newTenantConfigAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})

	w := doJSON(t, r, http.MethodGet, tenantConfigBase, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	var got models.TenantConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TenantID != "t-acme" || got.ConfigYAML == "" {
		t.Errorf("body: %+v", got)
	}
}

func TestGetTenantConfig_NoResolvedTenant_400(t *testing.T) {
	stub := &stubTenantConfigService{
		get: func(context.Context, string) (*models.TenantConfig, error) {
			t.Fatal("service should not be called when tenant unresolved")
			return nil, nil
		},
	}
	r := newTenantConfigAPI(stub, nil) // no tenant in ctx

	w := doJSON(t, r, http.MethodGet, tenantConfigBase, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestGetTenantConfig_NilService_503(t *testing.T) {
	r := newTenantConfigAPI(nil, &models.Tenant{TenantID: "t-acme", Slug: "acme"})
	w := doJSON(t, r, http.MethodGet, tenantConfigBase, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("nil svc: want 503, got %d", w.Code)
	}
}

func TestGetTenantConfig_ServiceError_500(t *testing.T) {
	stub := &stubTenantConfigService{
		get: func(context.Context, string) (*models.TenantConfig, error) {
			return nil, errors.New("db down")
		},
	}
	r := newTenantConfigAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})
	w := doJSON(t, r, http.MethodGet, tenantConfigBase, nil)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", w.Code)
	}
}

// ---------------- UpdateTenantConfig ----------------

func TestUpdateTenantConfig_HappyPath(t *testing.T) {
	stub := &stubTenantConfigService{
		update: func(_ context.Context, p tenantconfig.UpdateParams) (*models.TenantConfig, error) {
			if p.TenantID != "t-acme" {
				t.Errorf("tenant id: got %q want t-acme", p.TenantID)
			}
			if p.ConfigYAML != "key: value" {
				t.Errorf("yaml: got %q want %q", p.ConfigYAML, "key: value")
			}
			if p.UpdatedBy == nil || *p.UpdatedBy != "subj-1" {
				t.Errorf("actor: got %v want subj-1", p.UpdatedBy)
			}
			return &models.TenantConfig{
				TenantID:   p.TenantID,
				ConfigYAML: p.ConfigYAML,
				UpdatedBy:  p.UpdatedBy,
			}, nil
		},
	}
	r := newTenantConfigAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})

	// Set actor header to verify forwarding.
	req := newRequestWithActor(http.MethodPut, tenantConfigBase, tenantConfigRequest{ConfigYAML: "key: value"}, "subj-1")
	w := httptestRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestUpdateTenantConfig_NoActorHeader_NilActor(t *testing.T) {
	stub := &stubTenantConfigService{
		update: func(_ context.Context, p tenantconfig.UpdateParams) (*models.TenantConfig, error) {
			if p.UpdatedBy != nil {
				t.Errorf("actor should be nil, got %v", *p.UpdatedBy)
			}
			return &models.TenantConfig{TenantID: p.TenantID, ConfigYAML: p.ConfigYAML}, nil
		},
	}
	r := newTenantConfigAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})

	w := doJSON(t, r, http.MethodPut, tenantConfigBase, tenantConfigRequest{ConfigYAML: "{}"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestUpdateTenantConfig_MissingBody_400(t *testing.T) {
	stub := &stubTenantConfigService{
		update: func(context.Context, tenantconfig.UpdateParams) (*models.TenantConfig, error) {
			t.Fatal("service should not be called on missing body")
			return nil, nil
		},
	}
	r := newTenantConfigAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})

	w := doJSON(t, r, http.MethodPut, tenantConfigBase, map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestUpdateTenantConfig_InvalidYAML_400(t *testing.T) {
	stub := &stubTenantConfigService{
		update: func(_ context.Context, _ tenantconfig.UpdateParams) (*models.TenantConfig, error) {
			return nil, tenantconfig.ErrInvalidYAML
		},
	}
	r := newTenantConfigAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})

	w := doJSON(t, r, http.MethodPut, tenantConfigBase, tenantConfigRequest{ConfigYAML: "x"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestUpdateTenantConfig_YAMLTooLarge_413(t *testing.T) {
	stub := &stubTenantConfigService{
		update: func(_ context.Context, _ tenantconfig.UpdateParams) (*models.TenantConfig, error) {
			return nil, tenantconfig.ErrYAMLTooLarge
		},
	}
	r := newTenantConfigAPI(stub, &models.Tenant{TenantID: "t-acme", Slug: "acme"})

	w := doJSON(t, r, http.MethodPut, tenantConfigBase, tenantConfigRequest{ConfigYAML: "x"})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("want 413, got %d", w.Code)
	}
}

func TestUpdateTenantConfig_NoResolvedTenant_400(t *testing.T) {
	stub := &stubTenantConfigService{
		update: func(context.Context, tenantconfig.UpdateParams) (*models.TenantConfig, error) {
			t.Fatal("service should not be called when tenant unresolved")
			return nil, nil
		},
	}
	r := newTenantConfigAPI(stub, nil)

	w := doJSON(t, r, http.MethodPut, tenantConfigBase, tenantConfigRequest{ConfigYAML: "x"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

// newRequestWithActor + httptestRecorder: small helpers so the actor-
// header test can override what doJSON sets. Kept local to this file so
// the rest of the suite stays on doJSON.
func newRequestWithActor(method, path string, body any, actor string) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			panic(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if actor != "" {
		req.Header.Set(actorSubjectIDHeader, actor)
	}
	return req
}

func httptestRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }
