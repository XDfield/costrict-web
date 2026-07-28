package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// newChecker builds a tiny gin engine that runs RequireInternalToken, then
// returns 200 with body "ok" — so tests can assert pass-through vs abort.
func newChecker(token string) *gin.Engine {
	r := gin.New()
	r.Use(RequireInternalToken(token))
	r.GET("/", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return r
}

func TestRequireInternalToken_MissingHeader(t *testing.T) {
	r := newChecker("expected")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRequireInternalToken_EmptyHeader(t *testing.T) {
	r := newChecker("expected")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(InternalTokenHeader, "")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (empty header should be rejected)", w.Code)
	}
}

func TestRequireInternalToken_WrongToken(t *testing.T) {
	r := newChecker("expected")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(InternalTokenHeader, "wrong")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRequireInternalToken_CorrectToken(t *testing.T) {
	r := newChecker("expected")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(InternalTokenHeader, "expected")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Errorf("body = %q, want ok", w.Body.String())
	}
}

// TestRequireInternalToken_PrefixRejected guards against a subtle timing-attack
// failure mode where subtle.ConstantTimeCompare might appear to "match" a
// prefix — it must not.
func TestRequireInternalToken_PrefixRejected(t *testing.T) {
	r := newChecker("expected-secret-12345")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(InternalTokenHeader, "expected")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (prefix must not be accepted)", w.Code)
	}
}

func TestRequireInternalToken_EmptyExpectedBlocksAll(t *testing.T) {
	// Defense: if the operator accidentally constructs the middleware with an
	// empty token, every request must be rejected — ConstantTimeCompare("","")
	// returns 1, which would otherwise auth-bypass the whole internal API.
	// config.Load() prevents this at startup; the middleware guards direct use.
	r := newChecker("")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(InternalTokenHeader, "")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (empty configured token must block all requests)", w.Code)
	}
}
