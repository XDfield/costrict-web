package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/tenant"
	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

func newSlugEngine(apex []string) *gin.Engine {
	r := gin.New()
	r.Use(ResolveTenantSlug(apex))
	r.GET("/", func(c *gin.Context) {
		slug := tenant.SlugFromContext(c.Request.Context())
		if slug == "" {
			c.String(http.StatusOK, "none")
			return
		}
		c.String(http.StatusOK, slug)
	})
	return r
}

func TestResolveTenantSlug_HeaderWins(t *testing.T) {
	r := newSlugEngine(nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(TenantIDHeader, "acme")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "acme" {
		t.Errorf("body = %q, want acme", w.Body.String())
	}
}

func TestResolveTenantSlug_CookieFallback(t *testing.T) {
	r := newSlugEngine(nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: TenantSlugCookie, Value: "globex"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "globex" {
		t.Errorf("body = %q, want globex", w.Body.String())
	}
}

func TestResolveTenantSlug_HeaderPrecedenceOverCookie(t *testing.T) {
	r := newSlugEngine(nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(TenantIDHeader, "acme")
	req.AddCookie(&http.Cookie{Name: TenantSlugCookie, Value: "globex"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "acme" {
		t.Errorf("body = %q, want acme (header wins)", w.Body.String())
	}
}

func TestResolveTenantSlug_SubdomainLayer(t *testing.T) {
	r := newSlugEngine([]string{"example.com"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "acme.example.com"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "acme" {
		t.Errorf("body = %q, want acme from subdomain", w.Body.String())
	}
}

func TestResolveTenantSlug_SubdomainWithPort(t *testing.T) {
	r := newSlugEngine([]string{"localhost:8080"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "acme.localhost:8080"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "acme" {
		t.Errorf("body = %q, want acme (port-tolerant)", w.Body.String())
	}
}

func TestResolveTenantSlug_NestedSubdomainReturnsLabelBelowApex(t *testing.T) {
	// foo.acme.example.com with apex example.com → "acme" (label immediately
	// below apex, NOT "foo" — matches cs-user LastIndex extraction).
	r := newSlugEngine([]string{"example.com"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "foo.acme.example.com"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "acme" {
		t.Errorf("body = %q, want acme (label below apex)", w.Body.String())
	}
}

func TestResolveTenantSlug_NoSignalReturnsNone(t *testing.T) {
	r := newSlugEngine(nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "none" {
		t.Errorf("body = %q, want none (no signal)", w.Body.String())
	}
}

func TestResolveTenantSlug_HostIsApexReturnsNone(t *testing.T) {
	// Host = "example.com" with apex "example.com" → no subdomain → no signal.
	r := newSlugEngine([]string{"example.com"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "example.com"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "none" {
		t.Errorf("body = %q, want none (host IS apex)", w.Body.String())
	}
}

func TestResolveTenantSlug_EmptyHeaderIgnored(t *testing.T) {
	r := newSlugEngine(nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(TenantIDHeader, "   ")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "none" {
		t.Errorf("body = %q, want none (whitespace-only header ignored)", w.Body.String())
	}
}
