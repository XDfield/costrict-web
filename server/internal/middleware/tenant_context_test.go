package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/tenant"
	"github.com/gin-gonic/gin"
)

// newTenantContextEngine builds a pipeline that pre-seeds AuthClaims then
// runs TenantContext and serves the resolved tenant_id back as plain text.
func newTenantContextEngine(authClaims AuthClaims, seed bool) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if seed {
			c.Set(AuthClaimsKey, authClaims)
		}
		c.Next()
	})
	r.Use(TenantContext())
	r.GET("/", func(c *gin.Context) {
		c.String(http.StatusOK, tenant.TenantIDFromContext(c.Request.Context()))
	})
	return r
}

func TestTenantContext_JWTTenantIDWins(t *testing.T) {
	r := newTenantContextEngine(AuthClaims{TenantID: "acme-corp", Sub: "u1"}, true)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "acme-corp" {
		t.Errorf("body = %q, want acme-corp", w.Body.String())
	}
}

func TestTenantContext_EmptyClaimsFallsBackToDefault(t *testing.T) {
	// Casdoor-issued pre-cutover token: AuthClaims exists but TenantID is "".
	r := newTenantContextEngine(AuthClaims{Sub: "u1"}, true)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != tenant.DefaultTenantID {
		t.Errorf("body = %q, want %s", w.Body.String(), tenant.DefaultTenantID)
	}
}

func TestTenantContext_NoAuthClaimsFallsBackToDefault(t *testing.T) {
	// Unauthenticated request — AuthClaimsKey never set.
	r := newTenantContextEngine(AuthClaims{}, false)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != tenant.DefaultTenantID {
		t.Errorf("body = %q, want %s (unauth fallback)", w.Body.String(), tenant.DefaultTenantID)
	}
}

func TestTenantContext_WrongTypeClaimsFallsBackToDefault(t *testing.T) {
	// Defensive: if someone shoved a non-AuthClaims value, no panic.
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(AuthClaimsKey, "not-AuthClaims")
		c.Next()
	})
	r.Use(TenantContext())
	r.GET("/", func(c *gin.Context) {
		c.String(http.StatusOK, tenant.TenantIDFromContext(c.Request.Context()))
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no panic)", w.Code)
	}
	if w.Body.String() != tenant.DefaultTenantID {
		t.Errorf("body = %q, want default", w.Body.String())
	}
}
