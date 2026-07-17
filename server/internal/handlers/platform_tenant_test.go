package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// stubPlatformTenantRPC lets handler tests pin RPC responses without a
// live cs-user. Each method field is optional; nil fields panic if hit so
// a forgotten write-test fails loudly.
type stubPlatformTenantRPC struct {
	list    func(context.Context, int, int, string) (*userpkg.PlatformTenantListResult, error)
	get     func(context.Context, string) (*userpkg.PlatformTenant, error)
	create  func(context.Context, userpkg.PlatformTenantCreateParams) (*userpkg.PlatformTenant, error)
	update  func(context.Context, string, userpkg.PlatformTenantUpdateParams) (*userpkg.PlatformTenant, error)
	suspend func(context.Context, string) (*userpkg.PlatformTenant, error)
	restore func(context.Context, string) (*userpkg.PlatformTenant, error)
	delete  func(context.Context, string) (*userpkg.PlatformTenant, error)
}

func (s stubPlatformTenantRPC) ListTenants(ctx context.Context, l, o int, st string) (*userpkg.PlatformTenantListResult, error) {
	if s.list == nil {
		panic("stub.list not wired")
	}
	return s.list(ctx, l, o, st)
}
func (s stubPlatformTenantRPC) GetTenant(ctx context.Context, id string) (*userpkg.PlatformTenant, error) {
	if s.get == nil {
		panic("stub.get not wired")
	}
	return s.get(ctx, id)
}
func (s stubPlatformTenantRPC) CreateTenant(ctx context.Context, p userpkg.PlatformTenantCreateParams) (*userpkg.PlatformTenant, error) {
	if s.create == nil {
		panic("stub.create not wired")
	}
	return s.create(ctx, p)
}
func (s stubPlatformTenantRPC) UpdateTenant(ctx context.Context, id string, p userpkg.PlatformTenantUpdateParams) (*userpkg.PlatformTenant, error) {
	if s.update == nil {
		panic("stub.update not wired")
	}
	return s.update(ctx, id, p)
}
func (s stubPlatformTenantRPC) SuspendTenant(ctx context.Context, id string) (*userpkg.PlatformTenant, error) {
	if s.suspend == nil {
		panic("stub.suspend not wired")
	}
	return s.suspend(ctx, id)
}
func (s stubPlatformTenantRPC) RestoreTenant(ctx context.Context, id string) (*userpkg.PlatformTenant, error) {
	if s.restore == nil {
		panic("stub.restore not wired")
	}
	return s.restore(ctx, id)
}
func (s stubPlatformTenantRPC) DeleteTenant(ctx context.Context, id string) (*userpkg.PlatformTenant, error) {
	if s.delete == nil {
		panic("stub.delete not wired")
	}
	return s.delete(ctx, id)
}

// newPlatformTenantAPI builds a gin engine wired with all 7 routes (no
// auth middleware — that's exercised by the C1 middleware tests in
// permission_test.go). Returns the engine so each test can drive it via
// httptest.
func newPlatformTenantAPI(svc PlatformTenantService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := &PlatformTenantAPI{Svc: svc}
	g := r.Group("/api/platform/tenants")
	g.GET("", api.PlatformListTenants)
	g.POST("", api.PlatformCreateTenant)
	g.GET("/:id", api.PlatformGetTenant)
	g.PATCH("/:id", api.PlatformUpdateTenant)
	g.POST("/:id/suspend", api.PlatformSuspendTenant)
	g.POST("/:id/restore", api.PlatformRestoreTenant)
	g.POST("/:id/delete", api.PlatformDeleteTenant)
	return r
}

// doPlatformReq fires a request at the engine, JSON-encoding body when
// supplied. Returns the recorder for assertion.
func doPlatformReq(t *testing.T, r *gin.Engine, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, target, nil)
	} else {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		req = httptest.NewRequest(method, target, bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
	}
	r.ServeHTTP(w, req)
	return w
}

const platformTenantsPathRoot = "/api/platform/tenants"

// ---------------- List ----------------

func TestPlatformListTenants_HappyPath(t *testing.T) {
	r := newPlatformTenantAPI(stubPlatformTenantRPC{
		list: func(_ context.Context, limit, offset int, status string) (*userpkg.PlatformTenantListResult, error) {
			if limit != 10 || offset != 5 || status != "active" {
				t.Errorf("list args: limit=%d offset=%d status=%q", limit, offset, status)
			}
			return &userpkg.PlatformTenantListResult{
				Tenants: []userpkg.PlatformTenant{{TenantID: "t-1"}},
				Total:   1, Limit: 10, Offset: 5,
			}, nil
		},
	})
	w := doPlatformReq(t, r, http.MethodGet, platformTenantsPathRoot+"?limit=10&offset=5&status=active", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestPlatformListTenants_RPCUnavailable_502(t *testing.T) {
	r := newPlatformTenantAPI(stubPlatformTenantRPC{
		list: func(context.Context, int, int, string) (*userpkg.PlatformTenantListResult, error) {
			return nil, userpkg.ErrRPCUnavailable
		},
	})
	w := doPlatformReq(t, r, http.MethodGet, platformTenantsPathRoot, nil)
	if w.Code != http.StatusBadGateway {
		t.Errorf("want 502, got %d", w.Code)
	}
}

// ---------------- Get ----------------

func TestPlatformGetTenant_HappyPath(t *testing.T) {
	r := newPlatformTenantAPI(stubPlatformTenantRPC{
		get: func(_ context.Context, id string) (*userpkg.PlatformTenant, error) {
			if id != "acme" {
				t.Errorf("id: got %q", id)
			}
			return &userpkg.PlatformTenant{TenantID: "t-1", Slug: "acme"}, nil
		},
	})
	w := doPlatformReq(t, r, http.MethodGet, platformTenantsPathRoot+"/acme", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestPlatformGetTenant_NotFound_404(t *testing.T) {
	r := newPlatformTenantAPI(stubPlatformTenantRPC{
		get: func(context.Context, string) (*userpkg.PlatformTenant, error) {
			return nil, userpkg.ErrTenantNotFound
		},
	})
	w := doPlatformReq(t, r, http.MethodGet, platformTenantsPathRoot+"/nope", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

// ---------------- Create ----------------

func TestPlatformCreateTenant_HappyPath(t *testing.T) {
	r := newPlatformTenantAPI(stubPlatformTenantRPC{
		create: func(_ context.Context, p userpkg.PlatformTenantCreateParams) (*userpkg.PlatformTenant, error) {
			if p.Slug != "acme" || p.DisplayName != "Acme" {
				t.Errorf("create params: %+v", p)
			}
			return &userpkg.PlatformTenant{TenantID: "t-1", Slug: "acme"}, nil
		},
	})
	w := doPlatformReq(t, r, http.MethodPost, platformTenantsPathRoot, platformCreateRequest{
		Slug: "acme", DisplayName: "Acme",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestPlatformCreateTenant_MissingFields_400(t *testing.T) {
	r := newPlatformTenantAPI(stubPlatformTenantRPC{})
	w := doPlatformReq(t, r, http.MethodPost, platformTenantsPathRoot, map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestPlatformCreateTenant_SlugTaken_409(t *testing.T) {
	r := newPlatformTenantAPI(stubPlatformTenantRPC{
		create: func(context.Context, userpkg.PlatformTenantCreateParams) (*userpkg.PlatformTenant, error) {
			return nil, userpkg.ErrSlugTaken
		},
	})
	w := doPlatformReq(t, r, http.MethodPost, platformTenantsPathRoot, platformCreateRequest{
		Slug: "acme", DisplayName: "Acme",
	})
	if w.Code != http.StatusConflict {
		t.Errorf("want 409, got %d", w.Code)
	}
}

// ---------------- Update ----------------

func TestPlatformUpdateTenant_HappyPath(t *testing.T) {
	r := newPlatformTenantAPI(stubPlatformTenantRPC{
		update: func(_ context.Context, id string, p userpkg.PlatformTenantUpdateParams) (*userpkg.PlatformTenant, error) {
			if id != "acme" {
				t.Errorf("id: got %q", id)
			}
			if p.DisplayName == nil || *p.DisplayName != "New" {
				t.Errorf("DisplayName: %+v", p.DisplayName)
			}
			return &userpkg.PlatformTenant{Slug: "acme", DisplayName: "New"}, nil
		},
	})
	newName := "New"
	w := doPlatformReq(t, r, http.MethodPatch, platformTenantsPathRoot+"/acme", platformUpdateRequest{
		DisplayName: &newName,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestPlatformUpdateTenant_DomainConflict_409(t *testing.T) {
	r := newPlatformTenantAPI(stubPlatformTenantRPC{
		update: func(context.Context, string, userpkg.PlatformTenantUpdateParams) (*userpkg.PlatformTenant, error) {
			return nil, userpkg.ErrEmailDomainConflict
		},
	})
	domains := []string{"taken.com"}
	w := doPlatformReq(t, r, http.MethodPatch, platformTenantsPathRoot+"/acme", platformUpdateRequest{
		EmailDomains: &domains,
	})
	if w.Code != http.StatusConflict {
		t.Errorf("want 409, got %d", w.Code)
	}
}

// ---------------- Suspend / Restore / Delete ----------------

func TestPlatformSuspendTenant_HappyPath(t *testing.T) {
	r := newPlatformTenantAPI(stubPlatformTenantRPC{
		suspend: func(context.Context, string) (*userpkg.PlatformTenant, error) {
			return &userpkg.PlatformTenant{Status: "suspended"}, nil
		},
	})
	w := doPlatformReq(t, r, http.MethodPost, platformTenantsPathRoot+"/acme/suspend", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestPlatformSuspendTenant_InvalidTransition_409(t *testing.T) {
	r := newPlatformTenantAPI(stubPlatformTenantRPC{
		suspend: func(context.Context, string) (*userpkg.PlatformTenant, error) {
			return nil, userpkg.ErrInvalidStateTransition
		},
	})
	w := doPlatformReq(t, r, http.MethodPost, platformTenantsPathRoot+"/acme/suspend", nil)
	if w.Code != http.StatusConflict {
		t.Errorf("want 409, got %d", w.Code)
	}
}

func TestPlatformRestoreTenant_HappyPath(t *testing.T) {
	r := newPlatformTenantAPI(stubPlatformTenantRPC{
		restore: func(context.Context, string) (*userpkg.PlatformTenant, error) {
			return &userpkg.PlatformTenant{Status: "active"}, nil
		},
	})
	w := doPlatformReq(t, r, http.MethodPost, platformTenantsPathRoot+"/acme/restore", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

func TestPlatformDeleteTenant_HappyPath(t *testing.T) {
	r := newPlatformTenantAPI(stubPlatformTenantRPC{
		delete: func(context.Context, string) (*userpkg.PlatformTenant, error) {
			return &userpkg.PlatformTenant{Status: "deleted"}, nil
		},
	})
	w := doPlatformReq(t, r, http.MethodPost, platformTenantsPathRoot+"/acme/delete", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
}

// ---------------- Nil service → 502 ----------------

func TestPlatformHandlers_NilService_502(t *testing.T) {
	r := newPlatformTenantAPI(nil)
	w := doPlatformReq(t, r, http.MethodGet, platformTenantsPathRoot, nil)
	if w.Code != http.StatusBadGateway {
		t.Errorf("nil svc: want 502, got %d", w.Code)
	}
}
