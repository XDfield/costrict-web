//go:build cgo

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/auditlog"
	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"github.com/costrict/costrict-web/cs-user/internal/tenantconfig"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newAuditDB opens an in-memory sqlite + AutoMigrates AuditLog so handler
// tests can assert on rows written via the real auditlog.Service path.
func newAuditDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.AuditLog{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// firstAuditRow returns the first audit row matching the action; fails the
// test if zero or more than one match exists for that action.
func firstAuditRow(t *testing.T, db *gorm.DB, action string) models.AuditLog {
	t.Helper()
	var rows []models.AuditLog
	if err := db.Where("action = ?", action).Find(&rows).Error; err != nil {
		t.Fatalf("query audit: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row for action=%q, got %d", action, len(rows))
	}
	return rows[0]
}

// newPlatformTenantsAPIWithAudit mirrors newPlatformTenantsAPI but injects
// a real *auditlog.Service so audit assertions can run.
func newPlatformTenantsAPIWithAudit(svc PlatformTenantService, audit *auditlog.Service) (*PlatformTenantsAPI, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := &PlatformTenantsAPI{Svc: svc, Audit: audit}
	g := r.Group("/api/internal/platform/tenants")
	g.POST("", api.CreateTenant)
	g.POST("/:id/suspend", api.SuspendTenant)
	g.POST("/:id/restore", api.RestoreTenant)
	g.POST("/:id/delete", api.DeleteTenant)
	return api, r
}

// stubTenantContextMW injects a fake resolved tenant into the gin context
// using the same key the ResolveTenant middleware uses ("tenant"), so
// captureAuditMeta sees a non-empty tenant_id without standing up the full
// resolver chain.
func stubTenantContextMW(tenantID string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("tenant", &models.Tenant{TenantID: tenantID})
		c.Next()
	}
}

// newTenantConfigAPIWithAudit mirrors newTenantConfigAPI but injects audit +
// stubs the resolved-tenant middleware so requireTenantID sees a tenant.
func newTenantConfigAPIWithAudit(svc TenantConfigService, audit *auditlog.Service, tenantID string) (*TenantConfigAPI, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(stubTenantContextMW(tenantID))
	api := &TenantConfigAPI{Svc: svc, Audit: audit}
	g := r.Group("/api/internal/tenant")
	g.GET("/config", api.GetTenantConfig)
	g.PUT("/config", api.UpdateTenantConfig)
	return api, r
}

// newTenantProviderMappingAPIWithAudit mirrors the provider_mapping variant.
func newTenantProviderMappingAPIWithAudit(svc TenantProviderMappingService, audit *auditlog.Service, tenantID string) (*TenantProviderMappingAPI, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(stubTenantContextMW(tenantID))
	api := &TenantProviderMappingAPI{Svc: svc, Audit: audit}
	g := r.Group("/api/internal/tenant")
	g.GET("/provider-mapping", api.GetProviderMapping)
	g.PUT("/provider-mapping", api.UpdateProviderMapping)
	return api, r
}

// doAuditReq builds a JSON request with optional extra headers and dispatches
// it through the engine. Local variant of doJSON that allows header override
// (doJSON sets no headers and we need X-Actor-Subject-Id / X-Actor-Platform-
// Scope / X-Actor-Tenant-Role).
func doAuditReq(t *testing.T, r *gin.Engine, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyJSON []byte
	if body != nil {
		var err error
		bodyJSON, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestAudit_CreateTenantWritesRow verifies the C4.1 happy path: a successful
// POST /platform/tenants creates an audit row with action=tenant.create and
// the new tenant_id as target.
func TestAudit_CreateTenantWritesRow(t *testing.T) {
	db := newAuditDB(t)
	audit := auditlog.NewService(db, nil)
	_, r := newPlatformTenantsAPIWithAudit(stubPlatformTenantService{
		create: func(_ context.Context, _ tenant.CreateParams) (*models.Tenant, error) {
			return &models.Tenant{TenantID: "t-new", Slug: "acme", Status: tenant.StatusActive}, nil
		},
	}, audit)

	w := doAuditReq(t, r, http.MethodPost, "/api/internal/platform/tenants",
		platformCreateTenantRequest{Slug: "acme", DisplayName: "Acme"},
		map[string]string{
			"X-Actor-Subject-Id":     "u-platform",
			"X-Actor-Platform-Scope": "full",
		})
	if w.Code != http.StatusCreated {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	row := firstAuditRow(t, db, models.ActionTenantCreate)
	if row.TargetID == nil || *row.TargetID != "tenant:t-new" {
		t.Errorf("target_id: got %v, want tenant:t-new", row.TargetID)
	}
	if row.ActorSubjectID == nil || *row.ActorSubjectID != "u-platform" {
		t.Errorf("actor_subject_id: got %v, want u-platform", row.ActorSubjectID)
	}
	if row.ActorPlatformScope == nil || *row.ActorPlatformScope != "full" {
		t.Errorf("actor_platform_scope: got %v, want full", row.ActorPlatformScope)
	}
}

// TestAudit_SuspendTenantWritesRow verifies suspend path emits a distinct
// action from create.
func TestAudit_SuspendTenantWritesRow(t *testing.T) {
	db := newAuditDB(t)
	audit := auditlog.NewService(db, nil)
	_, r := newPlatformTenantsAPIWithAudit(stubPlatformTenantService{
		suspend: func(_ context.Context, _ string) (*models.Tenant, error) {
			return &models.Tenant{TenantID: "t-1", Slug: "acme", Status: tenant.StatusSuspended}, nil
		},
	}, audit)

	w := doAuditReq(t, r, http.MethodPost, "/api/internal/platform/tenants/t-1/suspend", nil,
		map[string]string{"X-Actor-Subject-Id": "u-admin"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	row := firstAuditRow(t, db, models.ActionTenantSuspend)
	if row.TargetID == nil || *row.TargetID != "tenant:t-1" {
		t.Errorf("target_id: got %v, want tenant:t-1", row.TargetID)
	}
}

// TestAudit_TenantConfigUpdateWritesRow verifies the tenant_config.update path
// captures tenant_id (from middleware tenant context) + actor role (from
// header).
func TestAudit_TenantConfigUpdateWritesRow(t *testing.T) {
	db := newAuditDB(t)
	audit := auditlog.NewService(db, nil)
	_, r := newTenantConfigAPIWithAudit(&stubTenantConfigService{
		update: func(_ context.Context, p tenantconfig.UpdateParams) (*models.TenantConfig, error) {
			return &models.TenantConfig{TenantID: p.TenantID, ConfigYAML: p.ConfigYAML}, nil
		},
	}, audit, "t-acme")

	w := doAuditReq(t, r, http.MethodPut, "/api/internal/tenant/config",
		tenantConfigRequest{ConfigYAML: "features: {x: true}"},
		map[string]string{
			"X-Actor-Subject-Id":  "u-admin",
			"X-Actor-Tenant-Role": "owner",
		})
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	row := firstAuditRow(t, db, models.ActionTenantConfigUpdate)
	if row.TenantID == nil || *row.TenantID != "t-acme" {
		t.Errorf("tenant_id: got %v, want t-acme", row.TenantID)
	}
	if row.ActorTenantRole == nil || *row.ActorTenantRole != "owner" {
		t.Errorf("actor_tenant_role: got %v, want owner", row.ActorTenantRole)
	}
	if row.TargetID == nil || *row.TargetID != "tenant_config:t-acme" {
		t.Errorf("target_id: got %v, want tenant_config:t-acme", row.TargetID)
	}
}

// TestAudit_ProviderMappingUpdateWritesRow verifies the typed provider_mapping
// path emits its own action and captures the provider count payload.
func TestAudit_ProviderMappingUpdateWritesRow(t *testing.T) {
	db := newAuditDB(t)
	audit := auditlog.NewService(db, nil)
	_, r := newTenantProviderMappingAPIWithAudit(&stubProviderMappingService{
		update: func(_ context.Context, _ tenantconfig.UpdateProviderMappingParams) (*tenantconfig.ProviderMapping, error) {
			return &tenantconfig.ProviderMapping{Providers: map[string]tenantconfig.Provider{
				"foo": {},
				"bar": {},
			}}, nil
		},
	}, audit, "t-acme")

	body := tenantconfig.ProviderMapping{Providers: map[string]tenantconfig.Provider{
		"foo": {Enabled: hpBoolPtr(true), Rank: hpIntPtr(1)},
		"bar": {Enabled: hpBoolPtr(false), Rank: hpIntPtr(2)},
	}}
	w := doAuditReq(t, r, http.MethodPut, "/api/internal/tenant/provider-mapping", body,
		map[string]string{"X-Actor-Subject-Id": "u-admin"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	row := firstAuditRow(t, db, models.ActionProviderMappingUpdate)
	if row.TargetID == nil || *row.TargetID != "provider_mapping:t-acme" {
		t.Errorf("target_id: got %v, want provider_mapping:t-acme", row.TargetID)
	}
	var payload map[string]any
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if count, _ := payload["provider_count"].(float64); count != 2 {
		t.Errorf("payload.provider_count: got %v, want 2", payload["provider_count"])
	}
}

// TestAudit_NilServiceSkipsQuietly verifies the nil-safe contract: a handler
// with Audit=nil (test path / 503 fallback) succeeds without panic and writes
// no audit row.
func TestAudit_NilServiceSkipsQuietly(t *testing.T) {
	db := newAuditDB(t)
	_, r := newPlatformTenantsAPIWithAudit(stubPlatformTenantService{
		create: func(_ context.Context, _ tenant.CreateParams) (*models.Tenant, error) {
			return &models.Tenant{TenantID: "t-x", Slug: "x", Status: tenant.StatusActive}, nil
		},
	}, nil) // nil audit

	w := doJSON(t, r, http.MethodPost, "/api/internal/platform/tenants", platformCreateTenantRequest{
		Slug: "x", DisplayName: "X",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	var count int64
	db.Model(&models.AuditLog{}).Count(&count)
	if count != 0 {
		t.Errorf("nil audit should write no row; got count=%d", count)
	}
}
