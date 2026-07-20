package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/tenant"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// actorMetaCapturingRPC wraps stubPlatformTenantRPC to capture the ctx
// handed to the underlying call so we can introspect the ActorMeta the
// handler injected.
type actorMetaCapturingRPC struct {
	stubPlatformTenantRPC
	capturedCtx context.Context
}

func (a *actorMetaCapturingRPC) CreateTenant(ctx context.Context, p userpkg.PlatformTenantCreateParams) (*userpkg.PlatformTenant, error) {
	a.capturedCtx = ctx
	return a.stubPlatformTenantRPC.CreateTenant(ctx, p)
}

// TestPlatformCreateTenant_InjectsActorMeta verifies the Phase C4.1
// handler-side plumbing: AuthClaims.TenantRoles[0] + AuthClaims.PlatformScope
// are propagated into the ctx as ActorMeta so the RPC client forwards them
// as X-Actor-Tenant-Role / X-Actor-Platform-Scope.
func TestPlatformCreateTenant_InjectsActorMeta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	stub := &actorMetaCapturingRPC{
		stubPlatformTenantRPC: stubPlatformTenantRPC{
			create: func(_ context.Context, _ userpkg.PlatformTenantCreateParams) (*userpkg.PlatformTenant, error) {
				return &userpkg.PlatformTenant{TenantID: "t-new", Slug: "acme"}, nil
			},
		},
	}
	api := &PlatformTenantAPI{Svc: stub}

	// Mount a tiny middleware that injects AuthClaims so platformActorCtx can
	// read them.
	r.Use(func(c *gin.Context) {
		c.Set(middleware.AuthClaimsKey, middleware.AuthClaims{
			Sub:           "u-platform",
			PlatformScope: "full",
			TenantRoles:   []string{"owner"},
		})
		c.Next()
	})
	r.POST("/api/platform/tenants", api.PlatformCreateTenant)

	body := `{"slug":"acme","display_name":"Acme"}`
	req := httptest.NewRequest(http.MethodPost, "/api/platform/tenants", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	if stub.capturedCtx == nil {
		t.Fatal("captured ctx is nil — CreateTenant not called")
	}
	m := tenant.ActorMetaFromContext(stub.capturedCtx)
	if m.Role != "owner" {
		t.Errorf("ActorMeta.Role: got %q want owner", m.Role)
	}
	if m.Scope != "full" {
		t.Errorf("ActorMeta.Scope: got %q want full", m.Scope)
	}
}

// TestActorMetaFromClaims_PicksFirstRole verifies the "first role wins"
// rule — a multi-role claims object still produces a single Role string
// (full role-list audit deferred per C4.1 known limitations).
func TestActorMetaFromClaims_PicksFirstRole(t *testing.T) {
	m := actorMetaFromClaims(middleware.AuthClaims{
		TenantRoles:   []string{"owner", "tenant_admin"},
		PlatformScope: "support",
	})
	if m.Role != "owner" {
		t.Errorf("Role: got %q want owner", m.Role)
	}
	if m.Scope != "support" {
		t.Errorf("Scope: got %q want support", m.Scope)
	}
}

// TestActorMetaFromClaims_EmptyClaims verifies the no-claims path returns
// zero-value ActorMeta (Role="" / Scope=""), which the RPC client will
// translate into omitted headers (NULL columns on the cs-user side).
func TestActorMetaFromClaims_EmptyClaims(t *testing.T) {
	m := actorMetaFromClaims(middleware.AuthClaims{})
	if m.Role != "" || m.Scope != "" {
		t.Errorf("empty claims: got %+v, want zero-value", m)
	}
}
