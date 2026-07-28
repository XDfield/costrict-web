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
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"github.com/gin-gonic/gin"
)

// stubTenantResolver is the in-memory TenantResolverService for handler tests.
// Each field is optional — nil means "return zero value" so a test only
// declares the path it cares about.
type stubTenantResolver struct {
	byEmail      func(ctx context.Context, email string) (*models.Tenant, error)
	listByDomain func(ctx context.Context, email string) ([]*models.Tenant, error)
}

func (s stubTenantResolver) ResolveByEmail(ctx context.Context, email string) (*models.Tenant, error) {
	if s.byEmail == nil {
		return nil, tenant.ErrTenantNotFound
	}
	return s.byEmail(ctx, email)
}

func (s stubTenantResolver) ListByEmailDomain(ctx context.Context, email string) ([]*models.Tenant, error) {
	if s.listByDomain == nil {
		return nil, nil
	}
	return s.listByDomain(ctx, email)
}

func newTenantsAPI(r TenantResolverService) (*TenantsAPI, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	api := &TenantsAPI{Resolver: r}
	engine := gin.New()
	engine.POST("/api/internal/tenants/resolve-by-email", api.ResolveByEmail)
	return api, engine
}

func doResolveByEmail(t *testing.T, r http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/internal/tenants/resolve-by-email", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestResolveByEmail_UniqueHit returns slug+tenant_id with status=ok.
func TestResolveByEmail_UniqueHit(t *testing.T) {
	r := stubTenantResolver{
		byEmail: func(_ context.Context, email string) (*models.Tenant, error) {
			if email != "alice@acme.example.com" {
				t.Fatalf("unexpected email: %q", email)
			}
			return &models.Tenant{TenantID: "t-acme", Slug: "acme", DisplayName: "Acme Co"}, nil
		},
	}
	_, engine := newTenantsAPI(r)

	w := doResolveByEmail(t, engine, `{"email":"alice@acme.example.com"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status field: got %v, want ok", resp["status"])
	}
	if resp["slug"] != "acme" {
		t.Errorf("slug: got %v, want acme", resp["slug"])
	}
	if resp["tenant_id"] != "t-acme" {
		t.Errorf("tenant_id: got %v, want t-acme", resp["tenant_id"])
	}
	if _, hasCandidates := resp["candidates"]; hasCandidates {
		t.Errorf("candidates field should be absent on ok, got %v", resp["candidates"])
	}
}

// TestResolveByEmail_Ambiguous returns status=ambiguous + candidates from
// ListByEmailDomain.
func TestResolveByEmail_Ambiguous(t *testing.T) {
	r := stubTenantResolver{
		byEmail: func(_ context.Context, _ string) (*models.Tenant, error) {
			return nil, tenant.ErrAmbiguousTenant
		},
		listByDomain: func(_ context.Context, _ string) ([]*models.Tenant, error) {
			return []*models.Tenant{
				{TenantID: "t-acme", Slug: "acme", DisplayName: "Acme Co"},
				{TenantID: "t-acme-emea", Slug: "acme-emea", DisplayName: "Acme EMEA"},
			}, nil
		},
	}
	_, engine := newTenantsAPI(r)

	w := doResolveByEmail(t, engine, `{"email":"alice@acme.example.com"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "ambiguous" {
		t.Fatalf("status: got %v, want ambiguous", resp["status"])
	}
	cands, ok := resp["candidates"].([]any)
	if !ok {
		t.Fatalf("candidates missing or wrong type: %T", resp["candidates"])
	}
	if len(cands) != 2 {
		t.Fatalf("candidates count: got %d, want 2", len(cands))
	}
	first := cands[0].(map[string]any)
	if first["slug"] != "acme" {
		t.Errorf("first candidate slug: got %v, want acme", first["slug"])
	}
	if first["name"] != "Acme Co" {
		t.Errorf("first candidate name: got %v, want 'Acme Co'", first["name"])
	}
}

// TestResolveByEmail_NotFound returns status=not_found, NOT 4xx.
func TestResolveByEmail_NotFound(t *testing.T) {
	r := stubTenantResolver{
		byEmail: func(_ context.Context, _ string) (*models.Tenant, error) {
			return nil, tenant.ErrTenantNotFound
		},
	}
	_, engine := newTenantsAPI(r)

	w := doResolveByEmail(t, engine, `{"email":"alice@nowhere.example.com"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "not_found" {
		t.Errorf("status: got %v, want not_found", resp["status"])
	}
}

// TestResolveByEmail_AmbiguousWithEmptyCandidateList guards against a
// regression where ListByEmailDomain returns 0 rows despite the byEmail
// call signaling ambiguity (race between the two scans).
func TestResolveByEmail_AmbiguousWithEmptyCandidateList(t *testing.T) {
	r := stubTenantResolver{
		byEmail: func(_ context.Context, _ string) (*models.Tenant, error) {
			return nil, tenant.ErrAmbiguousTenant
		},
		listByDomain: func(_ context.Context, _ string) ([]*models.Tenant, error) {
			return nil, nil
		},
	}
	_, engine := newTenantsAPI(r)

	w := doResolveByEmail(t, engine, `{"email":"alice@acme.example.com"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "ambiguous" {
		t.Errorf("status: got %v, want ambiguous", resp["status"])
	}
	cands, _ := resp["candidates"].([]any)
	if len(cands) != 0 {
		t.Errorf("candidates: got %d, want 0", len(cands))
	}
}

// TestResolveByEmail_MalformedBody → 400.
func TestResolveByEmail_MalformedBody(t *testing.T) {
	_, engine := newTenantsAPI(stubTenantResolver{})
	w := doResolveByEmail(t, engine, `not-json`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
}

// TestResolveByEmail_EmptyEmail → 400.
func TestResolveByEmail_EmptyEmail(t *testing.T) {
	_, engine := newTenantsAPI(stubTenantResolver{})
	w := doResolveByEmail(t, engine, `{"email":"  "}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
}

// TestResolveByEmail_ResolverError other than the two sentinels should
// still map to not_found (we treat unexpected errors as "Try 2 miss" so
// the OAuth callback falls through to default tenant rather than 500ing).
func TestResolveByEmail_ResolverError(t *testing.T) {
	r := stubTenantResolver{
		byEmail: func(_ context.Context, _ string) (*models.Tenant, error) {
			return nil, errors.New("db connection lost")
		},
	}
	_, engine := newTenantsAPI(r)

	w := doResolveByEmail(t, engine, `{"email":"alice@acme.example.com"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "not_found" {
		t.Errorf("status: got %v, want not_found (unexpected resolver errors fall through)", resp["status"])
	}
}
