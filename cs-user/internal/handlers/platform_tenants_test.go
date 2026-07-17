package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"github.com/gin-gonic/gin"
)

// stubPlatformTenantService lets handler tests pin responses without a DB.
// Each method field is optional; a nil field panics so a forgotten write
// test fails loudly instead of silently returning zero values.
type stubPlatformTenantService struct {
	create  func(context.Context, tenant.CreateParams) (*models.Tenant, error)
	list    func(context.Context, tenant.ListParams) (*tenant.ListResult, error)
	get     func(context.Context, string) (*models.Tenant, error)
	update  func(context.Context, string, tenant.UpdateParams) (*models.Tenant, error)
	suspend func(context.Context, string) (*models.Tenant, error)
	restore func(context.Context, string) (*models.Tenant, error)
	delete  func(context.Context, string) (*models.Tenant, error)
}

func (s stubPlatformTenantService) CreateTenant(ctx context.Context, p tenant.CreateParams) (*models.Tenant, error) {
	if s.create == nil {
		panic("stubPlatformTenantService.create not wired")
	}
	return s.create(ctx, p)
}
func (s stubPlatformTenantService) ListTenants(ctx context.Context, p tenant.ListParams) (*tenant.ListResult, error) {
	if s.list == nil {
		panic("stubPlatformTenantService.list not wired")
	}
	return s.list(ctx, p)
}
func (s stubPlatformTenantService) GetTenant(ctx context.Context, id string) (*models.Tenant, error) {
	if s.get == nil {
		panic("stubPlatformTenantService.get not wired")
	}
	return s.get(ctx, id)
}
func (s stubPlatformTenantService) UpdateTenant(ctx context.Context, id string, p tenant.UpdateParams) (*models.Tenant, error) {
	if s.update == nil {
		panic("stubPlatformTenantService.update not wired")
	}
	return s.update(ctx, id, p)
}
func (s stubPlatformTenantService) SuspendTenant(ctx context.Context, id string) (*models.Tenant, error) {
	if s.suspend == nil {
		panic("stubPlatformTenantService.suspend not wired")
	}
	return s.suspend(ctx, id)
}
func (s stubPlatformTenantService) RestoreTenant(ctx context.Context, id string) (*models.Tenant, error) {
	if s.restore == nil {
		panic("stubPlatformTenantService.restore not wired")
	}
	return s.restore(ctx, id)
}
func (s stubPlatformTenantService) RequestDeletion(ctx context.Context, id string) (*models.Tenant, error) {
	if s.delete == nil {
		panic("stubPlatformTenantService.delete not wired")
	}
	return s.delete(ctx, id)
}

// newPlatformTenantsAPI mirrors newUsersAPI: builds a gin engine wired with
// all 7 routes so handler tests exercise the same path tree production uses.
func newPlatformTenantsAPI(svc PlatformTenantService) (*PlatformTenantsAPI, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := &PlatformTenantsAPI{Svc: svc}
	g := r.Group("/api/internal/platform/tenants")
	g.GET("", api.ListTenants)
	g.POST("", api.CreateTenant)
	g.GET("/:id", api.GetTenant)
	g.PATCH("/:id", api.UpdateTenant)
	g.POST("/:id/suspend", api.SuspendTenant)
	g.POST("/:id/restore", api.RestoreTenant)
	g.POST("/:id/delete", api.DeleteTenant)
	return api, r
}

const platformTenantsBase = "/api/internal/platform/tenants"

// ---------------- CreateTenant ----------------

func TestPlatformCreateTenant_HappyPath(t *testing.T) {
	_, r := newPlatformTenantsAPI(stubPlatformTenantService{
		create: func(_ context.Context, p tenant.CreateParams) (*models.Tenant, error) {
			if p.Slug != "acme" || p.DisplayName != "Acme" {
				t.Errorf("create params: slug=%q name=%q", p.Slug, p.DisplayName)
			}
			return &models.Tenant{TenantID: "t-1", Slug: p.Slug, DisplayName: p.DisplayName, Status: tenant.StatusActive}, nil
		},
	})
	w := doJSON(t, r, http.MethodPost, platformTenantsBase, platformCreateTenantRequest{
		Slug: "acme", DisplayName: "Acme",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	var got models.Tenant
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TenantID != "t-1" || got.Slug != "acme" {
		t.Errorf("body: %+v", got)
	}
}

func TestPlatformCreateTenant_MissingFields(t *testing.T) {
	_, r := newPlatformTenantsAPI(stubPlatformTenantService{})
	// Empty body — binding "required" on slug + display_name should reject.
	w := doJSON(t, r, http.MethodPost, platformTenantsBase, map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestPlatformCreateTenant_SlugTaken(t *testing.T) {
	_, r := newPlatformTenantsAPI(stubPlatformTenantService{
		create: func(_ context.Context, _ tenant.CreateParams) (*models.Tenant, error) {
			return nil, tenant.ErrSlugTaken
		},
	})
	w := doJSON(t, r, http.MethodPost, platformTenantsBase, platformCreateTenantRequest{
		Slug: "acme", DisplayName: "Acme",
	})
	if w.Code != http.StatusConflict {
		t.Errorf("want 409, got %d", w.Code)
	}
}

func TestPlatformCreateTenant_InvalidSlug(t *testing.T) {
	_, r := newPlatformTenantsAPI(stubPlatformTenantService{
		create: func(_ context.Context, _ tenant.CreateParams) (*models.Tenant, error) {
			return nil, tenant.ErrInvalidSlug
		},
	})
	w := doJSON(t, r, http.MethodPost, platformTenantsBase, platformCreateTenantRequest{
		Slug: "AB", DisplayName: "Acme",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestPlatformCreateTenant_InternalError(t *testing.T) {
	_, r := newPlatformTenantsAPI(stubPlatformTenantService{
		create: func(_ context.Context, _ tenant.CreateParams) (*models.Tenant, error) {
			return nil, errors.New("boom")
		},
	})
	w := doJSON(t, r, http.MethodPost, platformTenantsBase, platformCreateTenantRequest{
		Slug: "acme", DisplayName: "Acme",
	})
	if w.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", w.Code)
	}
}

// ---------------- ListTenants ----------------

func TestPlatformListTenants_HappyPath(t *testing.T) {
	_, r := newPlatformTenantsAPI(stubPlatformTenantService{
		list: func(_ context.Context, p tenant.ListParams) (*tenant.ListResult, error) {
			if p.Limit != 10 || p.Offset != 20 || p.Status != "suspended" {
				t.Errorf("list params: %+v", p)
			}
			return &tenant.ListResult{
				Tenants: []*models.Tenant{{TenantID: "t-1", Slug: "a"}},
				Total:   1, Limit: 10, Offset: 20,
			}, nil
		},
	})
	w := doJSON(t, r, http.MethodGet, platformTenantsBase+"?limit=10&offset=20&status=suspended", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	var got tenant.ListResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Total != 1 || len(got.Tenants) != 1 {
		t.Errorf("body: %+v", got)
	}
}

// ---------------- GetTenant ----------------

func TestPlatformGetTenant_HappyPath(t *testing.T) {
	_, r := newPlatformTenantsAPI(stubPlatformTenantService{
		get: func(_ context.Context, id string) (*models.Tenant, error) {
			if id != "acme" {
				t.Errorf("id: got %q", id)
			}
			return &models.Tenant{TenantID: "t-1", Slug: "acme"}, nil
		},
	})
	w := doJSON(t, r, http.MethodGet, platformTenantsBase+"/acme", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestPlatformGetTenant_NotFound(t *testing.T) {
	_, r := newPlatformTenantsAPI(stubPlatformTenantService{
		get: func(_ context.Context, _ string) (*models.Tenant, error) {
			return nil, tenant.ErrTenantNotFound
		},
	})
	w := doJSON(t, r, http.MethodGet, platformTenantsBase+"/nope", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

// ---------------- UpdateTenant ----------------

func TestPlatformUpdateTenant_HappyPath(t *testing.T) {
	_, r := newPlatformTenantsAPI(stubPlatformTenantService{
		update: func(_ context.Context, id string, p tenant.UpdateParams) (*models.Tenant, error) {
			if id != "acme" {
				t.Errorf("id: got %q", id)
			}
			if p.DisplayName == nil || *p.DisplayName != "New" {
				t.Errorf("DisplayName: %+v", p.DisplayName)
			}
			return &models.Tenant{TenantID: "t-1", Slug: "acme", DisplayName: "New"}, nil
		},
	})
	newName := "New"
	w := doJSON(t, r, http.MethodPatch, platformTenantsBase+"/acme", platformUpdateTenantRequest{
		DisplayName: &newName,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestPlatformUpdateTenant_DomainConflict(t *testing.T) {
	_, r := newPlatformTenantsAPI(stubPlatformTenantService{
		update: func(_ context.Context, _ string, _ tenant.UpdateParams) (*models.Tenant, error) {
			return nil, tenant.ErrEmailDomainConflict
		},
	})
	domains := []string{"taken.com"}
	w := doJSON(t, r, http.MethodPatch, platformTenantsBase+"/acme", platformUpdateTenantRequest{
		EmailDomains: &domains,
	})
	if w.Code != http.StatusConflict {
		t.Errorf("want 409, got %d", w.Code)
	}
}

func TestPlatformUpdateTenant_NotFound(t *testing.T) {
	_, r := newPlatformTenantsAPI(stubPlatformTenantService{
		update: func(_ context.Context, _ string, _ tenant.UpdateParams) (*models.Tenant, error) {
			return nil, tenant.ErrTenantNotFound
		},
	})
	newName := "x"
	w := doJSON(t, r, http.MethodPatch, platformTenantsBase+"/nope", platformUpdateTenantRequest{
		DisplayName: &newName,
	})
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

// ---------------- Suspend / Restore / Delete ----------------

func TestPlatformSuspendTenant_HappyPath(t *testing.T) {
	_, r := newPlatformTenantsAPI(stubPlatformTenantService{
		suspend: func(_ context.Context, id string) (*models.Tenant, error) {
			if id != "acme" {
				t.Errorf("id: got %q", id)
			}
			return &models.Tenant{TenantID: "t-1", Slug: "acme", Status: tenant.StatusSuspended}, nil
		},
	})
	w := doJSON(t, r, http.MethodPost, platformTenantsBase+"/acme/suspend", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestPlatformSuspendTenant_InvalidTransition(t *testing.T) {
	_, r := newPlatformTenantsAPI(stubPlatformTenantService{
		suspend: func(_ context.Context, _ string) (*models.Tenant, error) {
			return nil, tenant.ErrInvalidStateTransition
		},
	})
	w := doJSON(t, r, http.MethodPost, platformTenantsBase+"/acme/suspend", nil)
	if w.Code != http.StatusConflict {
		t.Errorf("want 409, got %d", w.Code)
	}
}

func TestPlatformRestoreTenant_HappyPath(t *testing.T) {
	_, r := newPlatformTenantsAPI(stubPlatformTenantService{
		restore: func(_ context.Context, _ string) (*models.Tenant, error) {
			return &models.Tenant{Status: tenant.StatusActive}, nil
		},
	})
	w := doJSON(t, r, http.MethodPost, platformTenantsBase+"/acme/restore", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestPlatformDeleteTenant_HappyPath(t *testing.T) {
	_, r := newPlatformTenantsAPI(stubPlatformTenantService{
		delete: func(_ context.Context, id string) (*models.Tenant, error) {
			if id != "acme" {
				t.Errorf("id: got %q", id)
			}
			return &models.Tenant{Status: tenant.StatusDeleted}, nil
		},
	})
	w := doJSON(t, r, http.MethodPost, platformTenantsBase+"/acme/delete", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestPlatformDeleteTenant_AlreadyDeleted(t *testing.T) {
	_, r := newPlatformTenantsAPI(stubPlatformTenantService{
		delete: func(_ context.Context, _ string) (*models.Tenant, error) {
			return nil, tenant.ErrInvalidStateTransition
		},
	})
	w := doJSON(t, r, http.MethodPost, platformTenantsBase+"/acme/delete", nil)
	if w.Code != http.StatusConflict {
		t.Errorf("want 409, got %d", w.Code)
	}
}
