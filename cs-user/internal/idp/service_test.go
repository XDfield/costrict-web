//go:build cgo

package idp

import (
	"context"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newDB creates an in-memory SQLite database for testing.
func newDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	// Auto-migrate the models
	if err := db.AutoMigrate(&models.IdPSource{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	return db
}

// ---------------- Create (E2.2) ----------------

func TestCreate_ValidOAuthConfig_Success(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	params := CreateParams{
		TenantID:  "t-acme",
		Provider:  "github",
		Config: map[string]interface{}{
			"client_id":         "test-client-id",
			"client_secret":     "test-client-secret",
			"authorization_url": "https://github.com/login/oauth/authorize",
			"token_url":         "https://github.com/login/oauth/access_token",
			"userinfo_url":      "https://api.github.com/user",
			"scopes":            []string{"read:user", "user:email"},
		},
		CreatedBy: "test-admin",
	}

	view, err := svc.Create(context.Background(), params)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if view.TenantID != "t-acme" {
		t.Errorf("TenantID: got %q want t-acme", view.TenantID)
	}
	if view.Provider != "github" {
		t.Errorf("Provider: got %q want github", view.Provider)
	}
	if !view.Enabled {
		t.Error("Enabled should default to true")
	}
	if view.Priority != 0 {
		t.Errorf("Priority: got %d want 0 (default)", view.Priority)
	}
	if view.CreatedBy != "test-admin" {
		t.Errorf("CreatedBy: got %q want test-admin", view.CreatedBy)
	}

	// Verify config was serialized correctly
	if view.Config["client_id"] != "test-client-id" {
		t.Errorf("client_id not preserved in config")
	}
	// Verify secret was redacted
	if view.Config["client_secret"] != "******" {
		t.Errorf("client_secret should be redacted, got %v", view.Config["client_secret"])
	}
}

func TestCreate_InvalidOAuthConfig_ReturnsError(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	// Missing required fields
	params := CreateParams{
		TenantID: "t-acme",
		Provider: "github",
		Config: map[string]interface{}{
			"client_id": "test-client-id",
			// Missing client_secret, authorization_url, etc.
		},
	}

	_, err := svc.Create(context.Background(), params)
	if err == nil {
		t.Fatal("Create should return error for invalid OAuth config")
	}
}

func TestCreate_InvalidURL_ReturnsError(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	params := CreateParams{
		TenantID: "t-acme",
		Provider: "github",
		Config: map[string]interface{}{
			"client_id":         "test-client-id",
			"client_secret":     "test-client-secret",
			"authorization_url": "not-a-url",
			"token_url":         "https://github.com/login/oauth/access_token",
			"userinfo_url":      "https://api.github.com/user",
		},
	}

	_, err := svc.Create(context.Background(), params)
	if err == nil {
		t.Fatal("Create should return error for invalid URL")
	}
}

func TestCreate_HTTPURL_ReturnsError(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	params := CreateParams{
		TenantID: "t-acme",
		Provider: "github",
		Config: map[string]interface{}{
			"client_id":         "test-client-id",
			"client_secret":     "test-client-secret",
			"authorization_url": "http://github.com/login/oauth/authorize",
			"token_url":         "https://github.com/login/oauth/access_token",
			"userinfo_url":      "https://api.github.com/user",
		},
	}

	_, err := svc.Create(context.Background(), params)
	if err == nil {
		t.Fatal("Create should return error for HTTP URL (HTTPS required)")
	}
}

func TestCreate_EmptyTenantID_ReturnsError(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	params := CreateParams{
		TenantID: "",
		Provider: "github",
		Config: map[string]interface{}{
			"client_id": "test",
		},
	}

	_, err := svc.Create(context.Background(), params)
	if err == nil {
		t.Fatal("Create should return error for empty tenant_id")
	}
}

func TestCreate_EmptyProvider_ReturnsError(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	params := CreateParams{
		TenantID: "t-acme",
		Provider: "",
		Config:   map[string]interface{}{},
	}

	_, err := svc.Create(context.Background(), params)
	if err == nil {
		t.Fatal("Create should return error for empty provider")
	}
}

// ---------------- Get (E2.2) ----------------

func TestGet_Existing_ReturnsView(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	// Seed a record
	_, err := svc.Create(context.Background(), CreateParams{
		TenantID: "t-acme",
		Provider: "github",
		Config: map[string]interface{}{
			"client_id":         "test-client-id",
			"client_secret":     "test-client-secret",
			"authorization_url": "https://github.com/login/oauth/authorize",
			"token_url":         "https://github.com/login/oauth/access_token",
			"userinfo_url":      "https://api.github.com/user",
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Get it
	view, err := svc.Get(context.Background(), "t-acme", "github")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view == nil {
		t.Fatal("Get should return non-nil view")
	}

	if view.TenantID != "t-acme" {
		t.Errorf("TenantID: got %q want t-acme", view.TenantID)
	}
	if view.Provider != "github" {
		t.Errorf("Provider: got %q want github", view.Provider)
	}
}

func TestGet_NotFound_ReturnsNil(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	view, err := svc.Get(context.Background(), "t-acme", "github")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view != nil {
		t.Error("Get should return nil for non-existent record")
	}
}

func TestGet_EmptyTenantID_ReturnsError(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	_, err := svc.Get(context.Background(), "", "github")
	if err == nil {
		t.Fatal("Get should return error for empty tenant_id")
	}
}

// ---------------- List (E2.2) ----------------

func TestList_MultipleSources_ReturnsAll(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	// Seed multiple records
	sources := []CreateParams{
		{
			TenantID: "t-acme",
			Provider: "github",
			Config: map[string]interface{}{
				"client_id":         "github-client",
				"client_secret":     "github-secret",
				"authorization_url": "https://github.com/login/oauth/authorize",
				"token_url":         "https://github.com/login/oauth/access_token",
				"userinfo_url":      "https://api.github.com/user",
			},
			Priority: intPtr(100),
		},
		{
			TenantID: "t-acme",
			Provider: "google",
			Config: map[string]interface{}{
				"client_id":         "google-client",
				"client_secret":     "google-secret",
				"authorization_url": "https://accounts.google.com/o/oauth2/v2/auth",
				"token_url":         "https://oauth2.googleapis.com/token",
				"userinfo_url":      "https://www.googleapis.com/oauth2/v2/userinfo",
			},
			Priority: intPtr(200),
		},
	}

	for _, src := range sources {
		if _, err := svc.Create(context.Background(), src); err != nil {
			t.Fatalf("seed %s: %v", src.Provider, err)
		}
	}

	// List them
	views, err := svc.List(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(views) != 2 {
		t.Errorf("List returned %d items, want 2", len(views))
	}

	// Verify priority ordering (google should come first due to higher priority)
	if views[0].Provider != "google" {
		t.Errorf("First item should be google (priority 200), got %s", views[0].Provider)
	}
	if views[1].Provider != "github" {
		t.Errorf("Second item should be github (priority 100), got %s", views[1].Provider)
	}
}

func TestList_EmptyTenantID_ReturnsError(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	_, err := svc.List(context.Background(), "")
	if err == nil {
		t.Fatal("List should return error for empty tenant_id")
	}
}

func TestList_NoSources_ReturnsEmpty(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	views, err := svc.List(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(views) != 0 {
		t.Errorf("List should return empty slice, got %d items", len(views))
	}
}

// ---------------- Update (E2.2) ----------------

func TestUpdate_Existing_Success(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	// Seed a record
	_, err := svc.Create(context.Background(), CreateParams{
		TenantID: "t-acme",
		Provider: "github",
		Config: map[string]interface{}{
			"client_id":         "old-client-id",
			"client_secret":     "old-secret",
			"authorization_url": "https://github.com/login/oauth/authorize",
			"token_url":         "https://github.com/login/oauth/access_token",
			"userinfo_url":      "https://api.github.com/user",
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Update it
	updated, err := svc.Update(context.Background(), UpdateParams{
		TenantID: "t-acme",
		Provider: "github",
		Config: map[string]interface{}{
			"client_id":         "new-client-id",
			"client_secret":     "new-secret",
			"authorization_url": "https://github.com/login/oauth/authorize",
			"token_url":         "https://github.com/login/oauth/access_token",
			"userinfo_url":      "https://api.github.com/user",
		},
		Enabled:   boolPtr(false),
		Priority:  intPtr(50),
		UpdatedBy: "test-admin",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if updated.Config["client_id"] != "new-client-id" {
		t.Errorf("client_id not updated")
	}
	if updated.Config["client_secret"] != "******" {
		t.Errorf("client_secret should be redacted, got %v", updated.Config["client_secret"])
	}
	if updated.Enabled {
		t.Error("Enabled should be false")
	}
	if updated.Priority != 50 {
		t.Errorf("Priority: got %d want 50", updated.Priority)
	}
	if updated.UpdatedBy != "test-admin" {
		t.Errorf("UpdatedBy: got %q want test-admin", updated.UpdatedBy)
	}
}

func TestUpdate_NotFound_ReturnsError(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	_, err := svc.Update(context.Background(), UpdateParams{
		TenantID: "t-acme",
		Provider: "github",
		Config: map[string]interface{}{
			"client_id": "test",
		},
	})
	if err == nil {
		t.Fatal("Update should return error for non-existent record")
	}
}

func TestUpdate_InvalidConfig_ReturnsError(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	// Seed a valid record
	_, err := svc.Create(context.Background(), CreateParams{
		TenantID: "t-acme",
		Provider: "github",
		Config: map[string]interface{}{
			"client_id":         "test-client-id",
			"client_secret":     "test-secret",
			"authorization_url": "https://github.com/login/oauth/authorize",
			"token_url":         "https://github.com/login/oauth/access_token",
			"userinfo_url":      "https://api.github.com/user",
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Try to update with invalid config
	_, err = svc.Update(context.Background(), UpdateParams{
		TenantID: "t-acme",
		Provider: "github",
		Config: map[string]interface{}{
			"client_id": "test-client-id",
			// Missing other required fields
		},
	})
	if err == nil {
		t.Fatal("Update should return error for invalid config")
	}
}

// ---------------- Delete (E2.2) ----------------

func TestDelete_Existing_Success(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	// Seed a record
	_, err := svc.Create(context.Background(), CreateParams{
		TenantID: "t-acme",
		Provider: "github",
		Config: map[string]interface{}{
			"client_id":         "test-client-id",
			"client_secret":     "test-secret",
			"authorization_url": "https://github.com/login/oauth/authorize",
			"token_url":         "https://github.com/login/oauth/access_token",
			"userinfo_url":      "https://api.github.com/user",
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Delete it
	err = svc.Delete(context.Background(), "t-acme", "github")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify it's gone
	view, err := svc.Get(context.Background(), "t-acme", "github")
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if view != nil {
		t.Error("Record should be deleted")
	}
}

func TestDelete_NotFound_ReturnsError(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	err := svc.Delete(context.Background(), "t-acme", "github")
	if err == nil {
		t.Fatal("Delete should return error for non-existent record")
	}
}

// ---------------- GetTenantIdPs (E2.3) ----------------

func TestGetTenantIdPs_EnabledOnly_FiltersCorrectly(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	// Seed multiple IdP sources with different enabled states
	sources := []CreateParams{
		{
			TenantID: "t-acme",
			Provider: "github",
			Config: map[string]interface{}{
				"client_id":         "github-client",
				"client_secret":     "github-secret",
				"authorization_url": "https://github.com/login/oauth/authorize",
				"token_url":         "https://github.com/login/oauth/access_token",
				"userinfo_url":      "https://api.github.com/user",
			},
			Enabled:  boolPtr(true),
			Priority: intPtr(100),
		},
		{
			TenantID: "t-acme",
			Provider: "google",
			Config: map[string]interface{}{
				"client_id":         "google-client",
				"client_secret":     "google-secret",
				"authorization_url": "https://accounts.google.com/o/oauth2/v2/auth",
				"token_url":         "https://oauth2.googleapis.com/token",
				"userinfo_url":      "https://www.googleapis.com/oauth2/v2/userinfo",
			},
			Enabled:  func() *bool { b := false; return &b }(), // disabled
			Priority: intPtr(200),
		},
	}

	for _, src := range sources {
		if _, err := svc.Create(context.Background(), src); err != nil {
			t.Fatalf("seed %s: %v", src.Provider, err)
		}
	}

	// Get enabled IdPs (no provider_mapping filter)
	views, err := svc.GetTenantIdPs(context.Background(), "t-acme", nil)
	if err != nil {
		t.Fatalf("GetTenantIdPs: %v", err)
	}

	if len(views) != 1 {
		t.Errorf("GetTenantIdPs returned %d items, want 1 (only github enabled)", len(views))
	}
	if len(views) > 0 && views[0].Provider != "github" {
		t.Errorf("Expected github, got %s", views[0].Provider)
	}
}

func TestGetTenantIdPs_EmptyTenantID_ReturnsError(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	_, err := svc.GetTenantIdPs(context.Background(), "", nil)
	if err == nil {
		t.Fatal("GetTenantIdPs should return error for empty tenant_id")
	}
}

func TestGetTenantIdPs_NoSources_ReturnsEmpty(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	views, err := svc.GetTenantIdPs(context.Background(), "t-acme", nil)
	if err != nil {
		t.Fatalf("GetTenantIdPs: %v", err)
	}

	if len(views) != 0 {
		t.Errorf("GetTenantIdPs should return empty slice, got %d items", len(views))
	}
}

// ---------------- GetTenantIdPsInternal (E2.6) ----------------

func TestGetTenantIdPsInternal_ReturnsSecrets_NotRedacted(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	_, err := svc.Create(context.Background(), CreateParams{
		TenantID: "t-acme",
		Provider: "github",
		Config: map[string]interface{}{
			"client_id":         "github-client",
			"client_secret":     "super-secret-value",
			"authorization_url": "https://github.com/login/oauth/authorize",
			"token_url":         "https://github.com/login/oauth/access_token",
			"userinfo_url":      "https://api.github.com/user",
			"scopes":            []string{"read:user"},
		},
		Enabled: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	views, err := svc.GetTenantIdPsInternal(context.Background(), "t-acme", nil)
	if err != nil {
		t.Fatalf("GetTenantIdPsInternal: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(views))
	}
	// CRITICAL: client_secret must NOT be redacted for internal callers
	if views[0].Config["client_secret"] != "super-secret-value" {
		t.Errorf("client_secret must be raw for internal view, got %v", views[0].Config["client_secret"])
	}
	if views[0].Config["client_id"] != "github-client" {
		t.Errorf("client_id should be preserved, got %v", views[0].Config["client_id"])
	}
}

func TestGetTenantIdPsInternal_FiltersDisabled(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	// github enabled, google disabled
	if _, err := svc.Create(context.Background(), CreateParams{
		TenantID: "t-acme", Provider: "github",
		Config: map[string]interface{}{
			"client_id": "gh", "client_secret": "s",
			"authorization_url": "https://github.com/login/oauth/authorize",
			"token_url":         "https://github.com/login/oauth/access_token",
			"userinfo_url":      "https://api.github.com/user",
		},
		Enabled: boolPtr(true),
	}); err != nil {
		t.Fatalf("seed github: %v", err)
	}
	if _, err := svc.Create(context.Background(), CreateParams{
		TenantID: "t-acme", Provider: "google",
		Config: map[string]interface{}{
			"client_id": "g", "client_secret": "s",
			"authorization_url": "https://accounts.google.com/o/oauth2/v2/auth",
			"token_url":         "https://oauth2.googleapis.com/token",
			"userinfo_url":      "https://www.googleapis.com/oauth2/v2/userinfo",
		},
		Enabled: func() *bool { b := false; return &b }(),
	}); err != nil {
		t.Fatalf("seed google: %v", err)
	}

	views, err := svc.GetTenantIdPsInternal(context.Background(), "t-acme", nil)
	if err != nil {
		t.Fatalf("GetTenantIdPsInternal: %v", err)
	}
	if len(views) != 1 || views[0].Provider != "github" {
		t.Errorf("expected only github enabled, got %+v", views)
	}
}

func TestGetInternal_ReturnsRawSecret(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	_, err := svc.Create(context.Background(), CreateParams{
		TenantID: "t-acme", Provider: "github",
		Config: map[string]interface{}{
			"client_id":         "gh-id",
			"client_secret":     "raw-secret",
			"authorization_url": "https://github.com/login/oauth/authorize",
			"token_url":         "https://github.com/login/oauth/access_token",
			"userinfo_url":      "https://api.github.com/user",
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	view, err := svc.GetInternal(context.Background(), "t-acme", "github")
	if err != nil {
		t.Fatalf("GetInternal: %v", err)
	}
	if view == nil {
		t.Fatal("expected non-nil view")
	}
	if view.Config["client_secret"] != "raw-secret" {
		t.Errorf("GetInternal must return raw secret, got %v", view.Config["client_secret"])
	}
}

func TestGetInternal_NotFound_ReturnsNil(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	view, err := svc.GetInternal(context.Background(), "t-acme", "github")
	if err != nil {
		t.Fatalf("GetInternal: %v", err)
	}
	if view != nil {
		t.Error("expected nil for missing provider")
	}
}

// ---------------- Secret Redaction Tests ----------------

func TestSecretRedaction_OAuthCredentials_Redacted(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	view, err := svc.Create(context.Background(), CreateParams{
		TenantID: "t-acme",
		Provider: "github",
		Config: map[string]interface{}{
			"client_id":         "test-client-id",
			"client_secret":     "super-secret-value",
			"authorization_url": "https://github.com/login/oauth/authorize",
			"token_url":         "https://github.com/login/oauth/access_token",
			"userinfo_url":      "https://api.github.com/user",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify non-sensitive fields are preserved
	if view.Config["client_id"] != "test-client-id" {
		t.Errorf("client_id should be preserved")
	}
	// Verify sensitive fields are redacted
	if view.Config["client_secret"] != "******" {
		t.Errorf("client_secret should be redacted to ******, got %v", view.Config["client_secret"])
	}
}

func TestSecretRedaction_LDAPCredentials_Redacted(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	view, err := svc.Create(context.Background(), CreateParams{
		TenantID: "t-acme",
		Provider: "ldap",
		Config: map[string]interface{}{
			"host":         "ldap.example.com",
			"base_dn":      "dc=example,dc=com",
			"user_filter":  "(uid={username})",
			"bind_dn":      "cn=admin,dc=example,dc=com",
			"bind_password": "super-secret-password",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify non-sensitive fields are preserved
	if view.Config["host"] != "ldap.example.com" {
		t.Errorf("host should be preserved")
	}
	// Verify bind_password is redacted
	if view.Config["bind_password"] != "******" {
		t.Errorf("bind_password should be redacted to ******, got %v", view.Config["bind_password"])
	}
}

func TestSecretRedaction_MultipleSecrets_AllRedacted(t *testing.T) {
	db := newDB(t)
	svc := New(db)

	view, err := svc.Create(context.Background(), CreateParams{
		TenantID: "t-acme",
		Provider: "custom",
		Config: map[string]interface{}{
			"api_key":          "secret-api-key",
			"webhook_secret":   "webhook-secret",
			"client_secret":    "oauth-secret",
			"public_field":     "public-value",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify all sensitive fields are redacted
	sensitiveFields := []string{"api_key", "webhook_secret", "client_secret"}
	for _, field := range sensitiveFields {
		if view.Config[field] != "******" {
			t.Errorf("%s should be redacted to ******, got %v", field, view.Config[field])
		}
	}
	// Verify non-sensitive field is preserved
	if view.Config["public_field"] != "public-value" {
		t.Errorf("public_field should be preserved, got %v", view.Config["public_field"])
	}
}

// ---------------- Helpers ----------------

func boolPtr(b bool) *bool     { return &b }
func intPtr(i int) *int        { return &i }
