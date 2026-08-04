package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func newCORSRouter(cfg CORSConfig) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CORS(cfg))
	r.GET("/ping", func(c *gin.Context) { c.String(200, "pong") })
	r.OPTIONS("/ping", func(c *gin.Context) { c.String(200, "pong") })
	return r
}

// TestCORS_EmptyConfig_DefaultDenies verifies the security-critical default:
// when no allowed origins are configured AND DevMode is off, the middleware
// must NOT echo the request Origin back. Pre-fix behavior reflected any
// Origin verbatim with credentials=true, allowing any malicious site to make
// credentialed cross-origin requests. secreport CVSS 2.3 (CORS reflection).
func TestCORS_EmptyConfig_DefaultDenies(t *testing.T) {
	r := newCORSRouter(CORSConfig{}) // DevMode=false, empty AllowedOrigins

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	r.ServeHTTP(w, req)

	aco := w.Header().Get("Access-Control-Allow-Origin")
	if aco != "" {
		t.Fatalf("expected NO Access-Control-Allow-Origin header in default-deny mode, got %q", aco)
	}
	if w.Header().Get("Access-Control-Allow-Credentials") == "true" {
		t.Fatal("expected NO Access-Control-Allow-Credentials=true in default-deny mode")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (request still allowed, just no CORS headers), got %d", w.Code)
	}
}

// TestCORS_EmptyConfig_DevModeReflects confirms the legacy dev behavior is
// reachable only when DevMode is explicitly enabled.
func TestCORS_EmptyConfig_DevModeReflects(t *testing.T) {
	r := newCORSRouter(CORSConfig{DevMode: true})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("Origin", "https://dev.local:5173")
	r.ServeHTTP(w, req)

	if got, want := w.Header().Get("Access-Control-Allow-Origin"), "https://dev.local:5173"; got != want {
		t.Fatalf("expected dev-mode reflected origin %q, got %q", want, got)
	}
	if w.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatal("expected Access-Control-Allow-Credentials=true in dev mode")
	}
}

// TestCORS_AllowlistReflectsOnlyAllowed confirms the allowlist path is
// unchanged: only origins on the configured list are reflected.
func TestCORS_AllowlistReflectsOnlyAllowed(t *testing.T) {
	r := newCORSRouter(CORSConfig{AllowedOrigins: []string{"https://app.costrict.ai"}})

	// Allowed origin → reflected.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("Origin", "https://app.costrict.ai")
	r.ServeHTTP(w, req)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://app.costrict.ai" {
		t.Fatalf("expected allowlisted origin reflected, got %q", got)
	}

	// Disallowed origin → no CORS header, request still served (no Origin match
	// is a silent pass-through for non-OPTIONS; the browser will block the
	// response, which is the point).
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req2.Header.Set("Origin", "https://evil.example.com")
	r.ServeHTTP(w2, req2)
	if got := w2.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("expected NO header for disallowed origin, got %q", got)
	}
}

// TestCORS_AllowlistBlockedOrigin_Options403 confirms OPTIONS preflight from
// a disallowed origin is rejected with 403 (browser will not fall through to
// the actual request).
func TestCORS_AllowlistBlockedOrigin_Options403(t *testing.T) {
	r := newCORSRouter(CORSConfig{AllowedOrigins: []string{"https://app.costrict.ai"}})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/ping", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for OPTIONS preflight from disallowed origin, got %d", w.Code)
	}
}

// TestCORS_DefaultDeny_OPTIONSNoHeader confirms OPTIONS preflight in
// default-deny mode (empty config, DevMode off) doesn't return CORS headers
// either — the browser must reject the preflight.
func TestCORS_DefaultDeny_OPTIONSNoHeader(t *testing.T) {
	r := newCORSRouter(CORSConfig{})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/ping", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("expected NO header in default-deny OPTIONS, got %q", got)
	}
	if w.Header().Get("Access-Control-Allow-Methods") != "" {
		t.Fatal("expected NO Allow-Methods in default-deny OPTIONS")
	}
}
