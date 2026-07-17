package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/tenant"
	"github.com/gin-gonic/gin"
)

// newTenantMatchEngine builds a minimal pipeline that mirrors production: a
// stub handler that pre-seeds AuthClaims + ctx slug, then TenantMatch, then a
// sentinel handler returning 200. Tests vary the seeded values.
func newTenantMatchEngine(authClaims AuthClaims, slug string) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if authClaims.Sub != "" || authClaims.TenantSlug != "" {
			c.Set(AuthClaimsKey, authClaims)
		}
		ctx := tenant.WithSlug(c.Request.Context(), slug)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	r.Use(TenantMatch())
	r.GET("/", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	return r
}

func TestTenantMatch_BothEmpty_PassThrough(t *testing.T) {
	r := newTenantMatchEngine(AuthClaims{}, "")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestTenantMatch_JWTSlugOnly_PassThrough(t *testing.T) {
	// No runtime signal (apexDomains unset, no cookie/header) → skip.
	r := newTenantMatchEngine(AuthClaims{TenantSlug: "acme", Sub: "u1"}, "")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no runtime slug → skip)", w.Code)
	}
}

func TestTenantMatch_RuntimeSlugOnly_PassThrough(t *testing.T) {
	// Casdoor-issued token (no tenant_slug claim) → skip.
	r := newTenantMatchEngine(AuthClaims{Sub: "u1"}, "acme")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no JWT slug → skip)", w.Code)
	}
}

func TestTenantMatch_SlugsMatch_PassThrough(t *testing.T) {
	r := newTenantMatchEngine(AuthClaims{TenantSlug: "acme", Sub: "u1"}, "acme")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (slugs match)", w.Code)
	}
}

func TestTenantMatch_SlugsDiverge_Aborts401(t *testing.T) {
	r := newTenantMatchEngine(AuthClaims{TenantSlug: "acme", Sub: "u1"}, "globex")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (cross-tenant)", w.Code)
	}
}

func TestTenantMatch_NoAuthClaimsEntry_PassThrough(t *testing.T) {
	// OptionalAuth path with no token — AuthClaimsKey never set.
	r := newTenantMatchEngine(AuthClaims{}, "acme")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no auth claims at all)", w.Code)
	}
}
