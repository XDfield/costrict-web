package systemrole

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
)

func newSystemRoleTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	module := New(setupSystemRoleTestDB(t))
	api := r.Group("/api")
	api.Use(func(c *gin.Context) {
		if userID := c.GetHeader("X-User-ID"); userID != "" {
			c.Set(appmiddleware.UserIDKey, userID)
		}
		c.Next()
	})
	if err := module.Service.GrantRole("u1", SystemRolePlatformAdmin, "u1"); err != nil {
		t.Fatalf("seed platform admin: %v", err)
	}
	module.RegisterRoutes(api)
	return r
}

func performSystemRoleJSON(r *gin.Engine, method, path, userID string, body any) *httptest.ResponseRecorder {
	var reqBody []byte
	if body != nil {
		reqBody, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("X-User-ID", userID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestGetMySystemRolesHandler(t *testing.T) {
	r := newSystemRoleTestRouter(t)
	w := performSystemRoleJSON(r, http.MethodGet, "/api/auth/system-roles/me", "u1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
}

func TestPlatformAdminEndpointsRequireRole(t *testing.T) {
	r := newSystemRoleTestRouter(t)
	w := performSystemRoleJSON(r, http.MethodGet, "/api/admin/system-roles?role=business_admin", "u2", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d, body=%s", w.Code, w.Body.String())
	}
}

func TestGrantAndRevokeSystemRoleHandlers(t *testing.T) {
	r := newSystemRoleTestRouter(t)
	w := performSystemRoleJSON(r, http.MethodPost, "/api/admin/system-roles/users/u2", "u1", map[string]any{"role": SystemRoleBusinessAdmin})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	w = performSystemRoleJSON(r, http.MethodGet, "/api/admin/system-roles/users/u2", "u1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	w = performSystemRoleJSON(r, http.MethodDelete, "/api/admin/system-roles/users/u2/business_admin", "u1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
}
