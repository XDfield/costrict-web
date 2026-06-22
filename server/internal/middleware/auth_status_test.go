package middleware

import (
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
)

// statusGateRouter builds a RequireAuth-protected router that signs a valid JWT
// for subjectResolver-less auth (so c.GetString(UserIDKey) == claims.sub) and
// returns 200 on the protected route. Caller installs SetStatusChecker.
func statusGateRouter(t *testing.T) (router *gin.Engine, token string) {
	t.Helper()
	SetSubjectResolver(nil)
	key := generateTestRSAKey(t)
	kid := "status-kid"
	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{kid: &key.PublicKey})
	token = signTestJWT(t, key, kid, jwt.MapClaims{
		"sub":                "usr_status_subject",
		"name":               "Status User",
		"preferred_username": "statususer",
		"exp":                time.Now().Add(1 * time.Hour).Unix(),
	})

	router = gin.New()
	router.Use(RequireAuth("http://localhost:0", jwks))
	router.GET("/protected", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return router, token
}

// TestRequireAuth_NilStatusCheckerPassesThrough verifies the conservative
// default: with no checker installed, a fully authenticated request behaves
// exactly as before (no status lookup, 200).
func TestRequireAuth_NilStatusCheckerPassesThrough(t *testing.T) {
	SetStatusChecker(nil)
	defer SetStatusChecker(nil)

	router, token := statusGateRouter(t)

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := performRequest(router, req)

	if w.Code != http.StatusOK {
		t.Fatalf("nil checker should pass through, got %d; body=%s", w.Code, w.Body.String())
	}
}

// TestRequireAuth_ActiveStatusPassesThrough verifies an "active" account is let
// through.
func TestRequireAuth_ActiveStatusPassesThrough(t *testing.T) {
	defer SetStatusChecker(nil)
	var checkedSubject string
	SetStatusChecker(func(subjectID string) (string, error) {
		checkedSubject = subjectID
		return "active", nil
	})

	router, token := statusGateRouter(t)

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := performRequest(router, req)

	if w.Code != http.StatusOK {
		t.Fatalf("active account should pass, got %d", w.Code)
	}
	if checkedSubject != "usr_status_subject" {
		t.Errorf("checker received subject %q, want usr_status_subject", checkedSubject)
	}
}

// TestRequireAuth_BannedStatusRejected verifies a banned account is rejected
// with 403 (request never reaches the handler).
func TestRequireAuth_BannedStatusRejected(t *testing.T) {
	defer SetStatusChecker(nil)
	SetStatusChecker(func(subjectID string) (string, error) {
		return "banned", nil
	})

	router, token := statusGateRouter(t)

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := performRequest(router, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("banned account should be 403, got %d; body=%s", w.Code, w.Body.String())
	}
}

// TestRequireAuth_DisabledStatusRejected verifies a disabled account is rejected
// with 403.
func TestRequireAuth_DisabledStatusRejected(t *testing.T) {
	defer SetStatusChecker(nil)
	SetStatusChecker(func(subjectID string) (string, error) {
		return "disabled", nil
	})

	router, token := statusGateRouter(t)

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := performRequest(router, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("disabled account should be 403, got %d", w.Code)
	}
}

// TestRequireAuth_StatusCheckerErrorFailsOpen verifies the fail-open guarantee:
// a checker error must NOT lock out the user (request still succeeds). This is
// the critical safety property — a DB/audit wobble can never deny all access.
func TestRequireAuth_StatusCheckerErrorFailsOpen(t *testing.T) {
	defer SetStatusChecker(nil)
	SetStatusChecker(func(subjectID string) (string, error) {
		return "", errors.New("db unavailable")
	})

	router, token := statusGateRouter(t)

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := performRequest(router, req)

	if w.Code != http.StatusOK {
		t.Fatalf("checker error must fail open (200), got %d; body=%s", w.Code, w.Body.String())
	}
}

// TestRequireAuth_UnknownStatusPassesThrough verifies an unrecognized status
// value (e.g. empty / a future status the gate doesn't know) is treated as
// allowed rather than blocking — only explicit banned/disabled deny.
func TestRequireAuth_UnknownStatusPassesThrough(t *testing.T) {
	defer SetStatusChecker(nil)
	SetStatusChecker(func(subjectID string) (string, error) {
		return "", nil
	})

	router, token := statusGateRouter(t)

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := performRequest(router, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unknown status should pass through, got %d", w.Code)
	}
}

// TestStatusChecker_CachesSuccessfulLookups verifies the wrapped checker reuses
// a cached status within the TTL (the underlying lookup runs once across two
// requests).
func TestStatusChecker_CachesSuccessfulLookups(t *testing.T) {
	defer SetStatusChecker(nil)
	var calls int
	SetStatusChecker(func(subjectID string) (string, error) {
		calls++
		return "active", nil
	})

	router, token := statusGateRouter(t)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/protected", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := performRequest(router, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, w.Code)
		}
	}
	if calls != 1 {
		t.Fatalf("expected underlying checker to run once (cached), ran %d times", calls)
	}
}

// TestStatusChecker_InvalidateForcesRefresh verifies InvalidateStatusCache drops
// the cached value so a subsequent request hits the underlying checker again and
// the new (banned) status takes effect immediately.
func TestStatusChecker_InvalidateForcesRefresh(t *testing.T) {
	defer SetStatusChecker(nil)
	status := "active"
	SetStatusChecker(func(subjectID string) (string, error) {
		return status, nil
	})

	router, token := statusGateRouter(t)

	first := httptest.NewRequest("GET", "/protected", nil)
	first.Header.Set("Authorization", "Bearer "+token)
	if w := performRequest(router, first); w.Code != http.StatusOK {
		t.Fatalf("active should pass, got %d", w.Code)
	}

	// Flip to banned but DON'T invalidate: cached active still wins.
	status = "banned"
	cached := httptest.NewRequest("GET", "/protected", nil)
	cached.Header.Set("Authorization", "Bearer "+token)
	if w := performRequest(router, cached); w.Code != http.StatusOK {
		t.Fatalf("cached active should still pass before invalidate, got %d", w.Code)
	}

	// Invalidate -> next request must observe banned.
	InvalidateStatusCache("usr_status_subject")
	after := httptest.NewRequest("GET", "/protected", nil)
	after.Header.Set("Authorization", "Bearer "+token)
	if w := performRequest(router, after); w.Code != http.StatusForbidden {
		t.Fatalf("after invalidate banned should be 403, got %d", w.Code)
	}
}

// TestStatusChecker_ErrorsNotCached verifies an error result is not cached and
// fails open each time (so a later success is observed immediately).
func TestStatusChecker_ErrorsNotCached(t *testing.T) {
	defer SetStatusChecker(nil)
	fail := true
	var calls int
	SetStatusChecker(func(subjectID string) (string, error) {
		calls++
		if fail {
			return "", errors.New("db unavailable")
		}
		return "banned", nil
	})

	router, token := statusGateRouter(t)

	errReq := httptest.NewRequest("GET", "/protected", nil)
	errReq.Header.Set("Authorization", "Bearer "+token)
	if w := performRequest(router, errReq); w.Code != http.StatusOK {
		t.Fatalf("error must fail open (200), got %d", w.Code)
	}

	// Error wasn't cached: now the checker succeeds with banned -> 403.
	fail = false
	okReq := httptest.NewRequest("GET", "/protected", nil)
	okReq.Header.Set("Authorization", "Bearer "+token)
	if w := performRequest(router, okReq); w.Code != http.StatusForbidden {
		t.Fatalf("non-cached error should re-query and reject banned, got %d", w.Code)
	}
	if calls != 2 {
		t.Fatalf("expected 2 underlying calls (error not cached), got %d", calls)
	}
}
