// Package gitserver bootstrap helper (Phase E3b.1.1).
//
// Bootstrap is the env-var → DB migration bridge: when cs-user starts with
// CS_USER_GITEA_BASE_URL + CS_USER_GITEA_ADMIN_TOKEN set and no template row
// exists yet, we materialize one into git_servers and bind any unbound
// tenants to it. This preserves the existing operator workflow (env-var
// driven deploy) while moving the source of truth into the DB so subsequent
// tenant creates can clone from the template (§20.4).
//
// Idempotent: safe to call on every boot. No-op when env vars are unset or
// the template already exists.

package gitserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TemplateInput is the env-var-derived shape main.go passes in. Endpoint +
// AdminToken are the only required fields; DisplayName defaults to endpoint
// host if empty. AdminUser / AdminPassword are optional — when set they're
// written into git_servers.config so the @server gitsync client can use
// Basic auth for token-mint endpoints (Gitea's reqBasicOrRevProxyAuth
// middleware rejects admin PAT on those paths).
type TemplateInput struct {
	Endpoint      string
	AdminToken    string
	DisplayName   string
	AdminUser     string
	AdminPassword string
}

// BootstrapTemplate ensures a git_servers row marked is_template=true exists
// matching the supplied input. Behavior:
//
//   - If a template row already exists, return its server_id (no mutation).
//     The existing row is authoritative — operators edit it via the future
//     /api/internal/git-servers API, not by restarting cs-user with new env
//     vars.
//   - Otherwise create a new row from input + UUID-generated server_id, then
//     run BackfillUnboundTenants against it.
//
// Returns the template server_id so the caller (main.go) can log it.
//
// Empty input (endpoint or admin_token blank) → ErrNoTemplateInput; the
// caller should fall back to "Gitea auto-provisioning disabled" semantics
// (no template row written; giteasync.Service stays nil).
func BootstrapTemplate(ctx context.Context, db *gorm.DB, in TemplateInput) (string, error) {
	if in.Endpoint == "" || in.AdminToken == "" {
		return "", ErrNoTemplateInput
	}

	// Look for an existing template row first.
	var existing models.GitServer
	err := db.WithContext(ctx).
		Where("is_template = ?", true).
		First(&existing).Error
	if err == nil {
		// Template already exists — backfill any tenants that still lack a
		// binding (e.g. tenants created before bootstrap ran). Safe + cheap.
		if err := backfillUnboundTenants(ctx, db, existing.ServerID); err != nil {
			return existing.ServerID, fmt.Errorf("gitserver: backfill against existing template: %w", err)
		}
		return existing.ServerID, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", fmt.Errorf("gitserver: query template row: %w", err)
	}

	// No template row yet — materialize one from env values.
	displayName := in.DisplayName
	if displayName == "" {
		displayName = in.Endpoint
	}
	// Build the config blob. admin_token is always present; admin_user /
	// admin_password only when the operator supplied them (needed for
	// token-mint endpoints; other Gitea setups can omit).
	configMap := map[string]string{"admin_token": in.AdminToken}
	if in.AdminUser != "" && in.AdminPassword != "" {
		configMap["admin_user"] = in.AdminUser
		configMap["admin_password"] = in.AdminPassword
	}
	configBytes, err := json.Marshal(configMap)
	if err != nil {
		return "", fmt.Errorf("gitserver: marshal template config: %w", err)
	}

	gs := &models.GitServer{
		ServerID:    "gs-template-" + uuid.NewString(),
		Kind:        models.GitServerKindGitea,
		Endpoint:    in.Endpoint,
		DisplayName: displayName,
		Config:      string(configBytes),
		IsTemplate:  true,
		Enabled:     true,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	// Wrap create + backfill in a tx so we never end up with a template row
	// but unbackfilled tenants (which would trigger ErrTenantMissingGitServer
	// on the next request).
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(gs).Error; err != nil {
			return fmt.Errorf("create template: %w", err)
		}
		return backfillUnboundTenants(ctx, tx, gs.ServerID)
	})
	if err != nil {
		return "", err
	}
	return gs.ServerID, nil
}

// ErrNoTemplateInput signals that the caller didn't supply enough env data
// to materialize a template row. main.go treats this as "feature disabled".
var ErrNoTemplateInput = errors.New("gitserver: template input incomplete (endpoint or admin_token empty)")

// backfillUnboundTenants binds every tenant where git_server_id IS NULL to
// the supplied template server_id. This is the migration-window repair: the
// ALTER TABLE in 20260721160000 added the column nullable, and existing
// tenants need a value before the resolver returns success for them.
//
// New tenants created via the tenant CRUD API will clone from the template
// (future Phase C work); this helper only repairs legacy rows.
func backfillUnboundTenants(ctx context.Context, db *gorm.DB, templateServerID string) error {
	res := db.WithContext(ctx).
		Model(&models.Tenant{}).
		Where("git_server_id IS NULL").
		Update("git_server_id", templateServerID)
	if res.Error != nil {
		return fmt.Errorf("backfill tenants: %w", res.Error)
	}
	return nil
}
