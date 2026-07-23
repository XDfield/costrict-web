// Package gitserver bootstrap helper (Git Ownership Refactor Phase 1).
//
// Bootstrap is the operator-controlled seed: when server starts with
// GIT_SERVER_TEMPLATE_ENDPOINT + GIT_SERVER_TEMPLATE_ADMIN_TOKEN set and no
// template row exists yet, we materialize one into git_servers. Subsequent
// tenant binds clone from the template (or pick its own).
//
// Idempotent: safe to call on every boot. No-op when env vars are unset or
// the template already exists.
//
// Compared to cs-user's version (Phase E3b.1.1), this variant does NOT
// backfill tenants (server has no tenants main table). Operators bind
// tenants explicitly via PUT /api/internal/tenants/:id/git-server.

package gitserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TemplateInput is the env-derived shape main.go passes in.
type TemplateInput struct {
	Endpoint      string
	AdminToken    string
	DisplayName   string
	AdminUser     string
	AdminPassword string
}

// ErrNoTemplateInput signals insufficient env data to materialize a row.
// main.go treats this as "feature disabled" (no template written).
var ErrNoTemplateInput = errors.New("gitserver: template input incomplete (endpoint or admin_token empty)")

// BootstrapTemplate ensures a git_servers row marked is_template=true exists
// matching the supplied input. Returns the template server_id.
//
//   - If a template row already exists, return its server_id (no mutation).
//   - Otherwise create a new row from input + UUID-generated server_id.
//
// Empty input (endpoint or admin_token blank) → ErrNoTemplateInput.
func BootstrapTemplate(ctx context.Context, db *gorm.DB, in TemplateInput) (string, error) {
	if in.Endpoint == "" || in.AdminToken == "" {
		return "", ErrNoTemplateInput
	}

	var existing models.GitServer
	err := db.WithContext(ctx).Where("is_template = ?", true).First(&existing).Error
	if err == nil {
		return existing.ServerID, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", fmt.Errorf("gitserver: query template row: %w", err)
	}

	displayName := in.DisplayName
	if displayName == "" {
		displayName = in.Endpoint
	}
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
	if err := db.WithContext(ctx).Select("*").Create(gs).Error; err != nil {
		return "", fmt.Errorf("gitserver: create template: %w", err)
	}
	return gs.ServerID, nil
}
