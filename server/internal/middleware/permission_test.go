package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// newPermissionEngine builds a gin pipeline that pre-seeds AuthClaimsKey and
// then mounts one of the permission middlewares. Mirrors newTenantMatchEngine.
func newPermissionEngine(mw gin.HandlerFunc, claims AuthClaims, setClaims bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if setClaims {
			c.Set(AuthClaimsKey, claims)
		}
		c.Next()
	})
	r.GET("/x", mw, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func doPerm(t *testing.T, r *gin.Engine) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rw := httptest.NewRecorder()
	r.ServeHTTP(rw, req)
	return rw.Code
}

// --- RequirePlatformAdmin ---

func TestRequirePlatformAdmin_HappyPath(t *testing.T) {
	r := newPermissionEngine(
		RequirePlatformAdmin(),
		AuthClaims{PlatformAdmin: true, PlatformScope: "full"},
		true,
	)
	if code := doPerm(t, r); code != http.StatusOK {
		t.Errorf("got %d, want 200", code)
	}
}

func TestRequirePlatformAdmin_RegularUser403(t *testing.T) {
	r := newPermissionEngine(
		RequirePlatformAdmin(),
		AuthClaims{Sub: "u-regular"},
		true,
	)
	if code := doPerm(t, r); code != http.StatusForbidden {
		t.Errorf("got %d, want 403", code)
	}
}

func TestRequirePlatformAdmin_Unauthenticated401(t *testing.T) {
	r := newPermissionEngine(
		RequirePlatformAdmin(),
		AuthClaims{},
		false, // no AuthClaimsKey set
	)
	if code := doPerm(t, r); code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", code)
	}
}

func TestRequirePlatformAdmin_ScopeFilterAccepts(t *testing.T) {
	r := newPermissionEngine(
		RequirePlatformAdmin("full", "support"),
		AuthClaims{PlatformAdmin: true, PlatformScope: "support"},
		true,
	)
	if code := doPerm(t, r); code != http.StatusOK {
		t.Errorf("got %d, want 200 (support in allowlist)", code)
	}
}

func TestRequirePlatformAdmin_ScopeFilterRejects(t *testing.T) {
	r := newPermissionEngine(
		RequirePlatformAdmin("full", "support"),
		AuthClaims{PlatformAdmin: true, PlatformScope: "read_only"},
		true,
	)
	if code := doPerm(t, r); code != http.StatusForbidden {
		t.Errorf("got %d, want 403 (read_only not in allowlist)", code)
	}
}

// --- RequireTenantAdmin ---

func TestRequireTenantAdmin_RoleMatch(t *testing.T) {
	r := newPermissionEngine(
		RequireTenantAdmin("owner", "admin"),
		AuthClaims{TenantID: "t-acme", TenantRoles: []string{"admin"}},
		true,
	)
	if code := doPerm(t, r); code != http.StatusOK {
		t.Errorf("got %d, want 200", code)
	}
}

func TestRequireTenantAdmin_RoleMismatch(t *testing.T) {
	r := newPermissionEngine(
		RequireTenantAdmin("owner", "admin"),
		AuthClaims{TenantID: "t-acme", TenantRoles: []string{"billing"}},
		true,
	)
	if code := doPerm(t, r); code != http.StatusForbidden {
		t.Errorf("got %d, want 403 (billing not in [owner,admin])", code)
	}
}

func TestRequireTenantAdmin_RegularMember403(t *testing.T) {
	r := newPermissionEngine(
		RequireTenantAdmin("owner", "admin"),
		AuthClaims{TenantID: "t-acme"}, // no TenantRoles
		true,
	)
	if code := doPerm(t, r); code != http.StatusForbidden {
		t.Errorf("got %d, want 403 (no tenant_admin role)", code)
	}
}

func TestRequireTenantAdmin_PlatformAdminBypass(t *testing.T) {
	// Platform admins are super-tenant (§14.3) — bypass the tenant-admin
	// check even when their TenantRoles is empty.
	r := newPermissionEngine(
		RequireTenantAdmin("owner"),
		AuthClaims{PlatformAdmin: true, PlatformScope: "full", TenantID: "t-acme"},
		true,
	)
	if code := doPerm(t, r); code != http.StatusOK {
		t.Errorf("got %d, want 200 (platform-admin bypass)", code)
	}
}

func TestRequireTenantAdmin_AnyRoleAcceptedWhenNoArgs(t *testing.T) {
	r := newPermissionEngine(
		RequireTenantAdmin(), // no role args = any tenant_admin role is fine
		AuthClaims{TenantID: "t-acme", TenantRoles: []string{"billing"}},
		true,
	)
	if code := doPerm(t, r); code != http.StatusOK {
		t.Errorf("got %d, want 200 (any role accepted)", code)
	}
}

func TestRequireTenantAdmin_Unauthenticated401(t *testing.T) {
	r := newPermissionEngine(
		RequireTenantAdmin("owner"),
		AuthClaims{},
		false,
	)
	if code := doPerm(t, r); code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", code)
	}
}

// --- RequireTenantMember ---

func TestRequireTenantMember_HappyPath(t *testing.T) {
	r := newPermissionEngine(
		RequireTenantMember(),
		AuthClaims{TenantID: "t-acme"},
		true,
	)
	if code := doPerm(t, r); code != http.StatusOK {
		t.Errorf("got %d, want 200", code)
	}
}

func TestRequireTenantMember_NoTenantID403(t *testing.T) {
	// Defensive — shouldn't happen in practice (TenantContext backfills
	// "default"), but the middleware must not panic and must deny cleanly.
	r := newPermissionEngine(
		RequireTenantMember(),
		AuthClaims{Sub: "u-1"}, // TenantID empty
		true,
	)
	if code := doPerm(t, r); code != http.StatusForbidden {
		t.Errorf("got %d, want 403", code)
	}
}

func TestRequireTenantMember_Unauthenticated401(t *testing.T) {
	r := newPermissionEngine(
		RequireTenantMember(),
		AuthClaims{},
		false,
	)
	if code := doPerm(t, r); code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", code)
	}
}

// TestRequirePermission_NonAuthClaimsValueDoesntPanic verifies the type-
// assertion guard — a non-AuthClaims value at AuthClaimsKey must surface as
// 401, not crash the request.
func TestRequirePermission_NonAuthClaimsValueDoesntPanic(t *testing.T) {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(AuthClaimsKey, "not-AuthClaims")
		c.Next()
	})
	r.GET("/x", RequirePlatformAdmin(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rw := httptest.NewRecorder()
	r.ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401 (non-AuthClaims value treated as unauth)", rw.Code)
	}
}
