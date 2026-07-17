//go:build cgo

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newTenantMiddlewareDB mirrors the tenant/resolver_test.go fixture. Kept
// local (not shared with the tenant package) so the middleware package has
// no test-time dependency on the tenant package's internal fixtures.
func newTenantMiddlewareDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.Tenant{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	seed := []models.Tenant{
		{TenantID: "default", Slug: "default", DisplayName: "Default Tenant", Status: "active"},
		{TenantID: "t-acme", Slug: "acme", DisplayName: "Acme Inc.", Status: "active"},
	}
	for i := range seed {
		if err := db.Create(&seed[i]).Error; err != nil {
			t.Fatalf("seed %s: %v", seed[i].TenantID, err)
		}
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// newTenantEngine wires ResolveTenant into a gin engine with a probe handler
// that returns 200 + the resolved tenant_id (or "none" when no tenant).
func newTenantEngine(t *testing.T, resolver *tenant.Resolver, apex []string) *gin.Engine {
	t.Helper()
	r := gin.New()
	r.Use(ResolveTenant(resolver, apex))
	r.GET("/", func(c *gin.Context) {
		t2, _ := TenantFromGin(c)
		if t2 == nil {
			c.String(http.StatusOK, "none")
			return
		}
		c.String(http.StatusOK, t2.TenantID)
	})
	return r
}

func TestResolveTenant_XTenantIDHeaderWins(t *testing.T) {
	r := newTenantEngine(t, tenant.NewResolver(newTenantMiddlewareDB(t)), nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(TenantIDHeader, "t-acme")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "t-acme" {
		t.Errorf("body = %q, want t-acme", w.Body.String())
	}
}

func TestResolveTenant_SlugCookieUsedWhenHeaderAbsent(t *testing.T) {
	r := newTenantEngine(t, tenant.NewResolver(newTenantMiddlewareDB(t)), nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: TenantSlugCookie, Value: "acme"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "t-acme" {
		t.Errorf("body = %q, want t-acme from cookie", w.Body.String())
	}
}

func TestResolveTenant_HeaderPrecedenceOverCookie(t *testing.T) {
	r := newTenantEngine(t, tenant.NewResolver(newTenantMiddlewareDB(t)), nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(TenantIDHeader, "default")
	req.AddCookie(&http.Cookie{Name: TenantSlugCookie, Value: "acme"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "default" {
		t.Errorf("body = %q, want default (header wins)", w.Body.String())
	}
}

func TestResolveTenant_SubdomainLayer(t *testing.T) {
	apex := []string{"cs-user.example.com"}
	r := newTenantEngine(t, tenant.NewResolver(newTenantMiddlewareDB(t)), apex)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "acme.cs-user.example.com"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "t-acme" {
		t.Errorf("body = %q, want t-acme from subdomain", w.Body.String())
	}
}

func TestResolveTenant_FallbackToDefaultOnNoSignal(t *testing.T) {
	r := newTenantEngine(t, tenant.NewResolver(newTenantMiddlewareDB(t)), nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "default" {
		t.Errorf("body = %q, want default (no signal → fallback)", w.Body.String())
	}
}

func TestResolveTenant_BogusHeaderFallsThroughToDefault(t *testing.T) {
	r := newTenantEngine(t, tenant.NewResolver(newTenantMiddlewareDB(t)), nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(TenantIDHeader, "does-not-exist")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "default" {
		t.Errorf("body = %q, want default after miss", w.Body.String())
	}
}

func TestResolveTenant_NilResolverNoOps(t *testing.T) {
	// When no resolver wired, middleware must not crash and must leave the
	// tenant unset (Phase A backwards-compat).
	r := newTenantEngine(t, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "none" {
		t.Errorf("body = %q, want none (nil resolver)", w.Body.String())
	}
}

func TestResolveTenant_DefaultMissingReturnsNoTenant(t *testing.T) {
	// Construct a DB where the default tenant row has been deleted (simulates
	// bootstrap failure). Middleware should not abort; handler should see nil.
	db := newTenantMiddlewareDB(t)
	if err := db.Where("tenant_id = ?", "default").Delete(&models.Tenant{}).Error; err != nil {
		t.Fatalf("delete default: %v", err)
	}
	r := newTenantEngine(t, tenant.NewResolver(db), nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "none" {
		t.Errorf("body = %q, want none (default missing)", w.Body.String())
	}
}

// TestTenantFromGin_ClearsStaleErrorOnResolve guards against the soft edge
// case where a layer 1 hard-DB-error sets tenant_resolve_error, then layer 2
// succeeds and sets tenant. TenantFromGin must NOT surface a stale error
// alongside the resolved tenant.
func TestTenantFromGin_ClearsStaleErrorOnResolve(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	// Simulate the post-resolve state: middleware cleared the error after
	// resolving a tenant.
	c.Set("tenant", &models.Tenant{TenantID: "t-acme"})
	c.Set("tenant_resolve_error", nil)

	got, err := TenantFromGin(c)
	if err != nil {
		t.Errorf("err = %v, want nil (stale error must be cleared)", err)
	}
	if got == nil || got.TenantID != "t-acme" {
		t.Errorf("tenant = %+v, want t-acme", got)
	}
}
