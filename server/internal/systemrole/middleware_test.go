package systemrole

import (
	"net/http"
	"net/http/httptest"
	"testing"

	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
)

func TestRequirePlatformAdminMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupSystemRoleTestDB(t)
	svc := NewSystemRoleService(db)
	if err := svc.GrantRole("u1", SystemRolePlatformAdmin, "u1"); err != nil {
		t.Fatalf("grant platform admin: %v", err)
	}

	r := gin.New()
	r.GET("/protected", func(c *gin.Context) {
		if userID := c.GetHeader("X-User-ID"); userID != "" {
			c.Set(appmiddleware.UserIDKey, userID)
		}
		c.Next()
	}, RequirePlatformAdmin(db), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d, body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("X-User-ID", "u2")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d, body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("X-User-ID", "u1")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
}

func TestRequireBusinessAdminOrAboveMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupSystemRoleTestDB(t)
	svc := NewSystemRoleService(db)
	if err := svc.GrantRole("u1", SystemRoleBusinessAdmin, "u1"); err != nil {
		t.Fatalf("grant business admin: %v", err)
	}
	if err := svc.GrantRole("u2", SystemRolePlatformAdmin, "u2"); err != nil {
		t.Fatalf("grant platform admin: %v", err)
	}

	r := gin.New()
	r.GET("/protected", func(c *gin.Context) {
		if userID := c.GetHeader("X-User-ID"); userID != "" {
			c.Set(appmiddleware.UserIDKey, userID)
		}
		c.Next()
	}, RequireBusinessAdminOrAbove(db), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("X-User-ID", "u3")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d, body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("X-User-ID", "u1")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for business admin, got %d, body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("X-User-ID", "u2")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for platform admin, got %d, body=%s", w.Code, w.Body.String())
	}
}
