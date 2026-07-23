package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func resetProfileGate() {
	SetProfileGateEnabled(false)
	SetProfileChecker(nil)
}

// newGateRouter mounts RequireProfileComplete behind a UserIDKey-injecting
// middleware so we can vary "who the caller is" per test.
func newGateRouter(subjectID string) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if subjectID != "" {
			c.Set(UserIDKey, subjectID)
		}
		c.Next()
	})
	r.Use(RequireProfileComplete)
	r.GET("/api/items", func(c *gin.Context) { c.String(200, "ok") })
	r.GET("/api/users/me/complete-registration", func(c *gin.Context) { c.String(200, "reg") })
	return r
}

// TestProfileGate_DisabledByDefault verifies the gate is inert unless
// PROFILE_GATE_ENABLED=true. Pre-existing routes must keep working.
func TestProfileGate_DisabledByDefault(t *testing.T) {
	defer resetProfileGate()
	SetProfileGateEnabled(false)
	SetProfileChecker(func(string) (bool, error) {
		t.Fatal("checker must not be called when gate disabled")
		return false, nil
	})

	r := newGateRouter("usr_a")
	req := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with gate disabled, got %d", w.Code)
	}
}

// TestProfileGate_NoCheckerIsNoOp verifies the defensive contract: even
// with the flag on, no checker installed ⇒ no-op (avoids 403 storm on
// misconfigured rollout).
func TestProfileGate_NoCheckerIsNoOp(t *testing.T) {
	defer resetProfileGate()
	SetProfileGateEnabled(true)
	// Do not install a checker.

	r := newGateRouter("usr_a")
	req := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with no checker, got %d", w.Code)
	}
}

// TestProfileGate_IncompleteBlocks verifies the core 403 path.
func TestProfileGate_IncompleteBlocks(t *testing.T) {
	defer resetProfileGate()
	SetProfileGateEnabled(true)
	SetProfileChecker(func(string) (bool, error) { return false, nil })

	r := newGateRouter("usr_a")
	req := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if !bytesContains(w.Body.Bytes(), "profile_incomplete") {
		t.Errorf("expected profile_incomplete token, got %s", w.Body.String())
	}
}

// TestProfileGate_CompleteAllows verifies the happy path.
func TestProfileGate_CompleteAllows(t *testing.T) {
	defer resetProfileGate()
	SetProfileGateEnabled(true)
	SetProfileChecker(func(string) (bool, error) { return true, nil })

	r := newGateRouter("usr_a")
	req := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// TestProfileGate_LookupErrorFailsOpen verifies the safety contract.
func TestProfileGate_LookupErrorFailsOpen(t *testing.T) {
	defer resetProfileGate()
	SetProfileGateEnabled(true)
	SetProfileChecker(func(string) (bool, error) { return false, errors.New("db down") })

	r := newGateRouter("usr_a")
	req := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (fail-open), got %d", w.Code)
	}
}

// TestProfileGate_WhitelistedRoutesBypass verifies the registration routes
// themselves remain reachable when the user is gated.
func TestProfileGate_WhitelistedRoutesBypass(t *testing.T) {
	defer resetProfileGate()
	SetProfileGateEnabled(true)
	SetProfileChecker(func(string) (bool, error) { return false, nil })

	r := newGateRouter("usr_a")
	req := httptest.NewRequest(http.MethodGet, "/api/users/me/complete-registration", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on whitelisted route, got %d: %s", w.Code, w.Body.String())
	}
}

// TestProfileGate_NoSubjectAllows verifies the contract: routes without
// a resolved subject (intentionally public routes) are not gated.
func TestProfileGate_NoSubjectAllows(t *testing.T) {
	defer resetProfileGate()
	SetProfileGateEnabled(true)
	called := false
	SetProfileChecker(func(string) (bool, error) { called = true; return false, nil })

	// subjectID empty → no UserIDKey set, gate skips.
	r := newGateRouter("")
	req := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (no subject), got %d", w.Code)
	}
	if called {
		t.Errorf("checker must not be called when subject is empty")
	}
}

// TestProfileGate_InvalidateCache verifies cache invalidation pokes the
// entry so the next lookup hits the underlying checker again.
func TestProfileGate_InvalidateCache(t *testing.T) {
	defer resetProfileGate()
	SetProfileGateEnabled(true)
	hits := 0
	SetProfileChecker(func(string) (bool, error) {
		hits++
		return true, nil
	})

	// First call hits the underlying checker; second serves from cache.
	_, _ = profileChecker("usr_a")
	_, _ = profileChecker("usr_a")
	if hits != 1 {
		t.Fatalf("expected 1 hit, got %d", hits)
	}
	InvalidateProfileCache("usr_a")
	_, _ = profileChecker("usr_a")
	if hits != 2 {
		t.Fatalf("expected 2 hits after invalidate, got %d", hits)
	}
}

// bytesContains is a tiny local helper so the test file has no extra import.
func bytesContains(b []byte, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == sub {
			return true
		}
	}
	return false
}
