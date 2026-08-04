// Package gitserver implements per-tenant Git server resolution (Git
// Ownership Refactor Phase 1).
//
// Server-side counterpart of cs-user's gitserver package. Queries
// tenant_git_server_binding + git_servers directly (no RPC back to cs-user).
//
// Flow:
//
//  1. Load tenant_git_server_binding for the supplied tenant.
//  2. Load the bound git_servers row.
//  3. Parse the JSONB config blob to extract admin_token (+ optional
//     admin_user / admin_password for token-mint endpoints).
//  4. Return Config.
//
// Read-only and stateless; safe to share across goroutines. No cache —
// callers construct a fresh Client per call (cold signup path).
package gitserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// Sentinel errors. Callers (gitsync.UserProvisioner, handlers) translate:
//
//   - ErrTenantMissingGitServer → soft-skip (no binding row)
//   - ErrGitServerNotFound      → 500 (FK violation — should be impossible)
//   - ErrGitServerDisabled      → 503 (drained / decommission in progress)
//   - ErrConfigMalformed        → 500 (operator bug; admin_token unreadable)
var (
	ErrTenantMissingGitServer = errors.New("gitserver: tenant has no git_server binding")
	ErrGitServerNotFound      = errors.New("gitserver: git_server row not found (FK violation)")
	ErrGitServerDisabled      = errors.New("gitserver: git server is disabled")
	ErrConfigMalformed        = errors.New("gitserver: config JSON malformed or missing admin_token")
)

// Config is the minimum the calling Git client needs. Value type — copies
// are cheap and there's no internal state worth hiding.
type Config struct {
	ServerID      string
	Kind          string
	Endpoint      string
	AdminToken    string
	AdminUser     string
	AdminPassword string
	// WebhookSecret verifies inbound Gitea webhook delivery signatures. It
	// is stored alongside the other per-server secrets in git_servers.config
	// as webhook_secret, never in a dedicated plaintext database column.
	WebhookSecret string
	// WebURL is the browser-facing base URL of the git server, used to build
	// links a user clicks (repo pages) and addresses their device clones from.
	// Endpoint is the API address, which on split internal/external deployments
	// is a cluster-internal host the browser cannot reach — and Gitea's own
	// html_url can't be trusted either when its ROOT_URL is misconfigured.
	// Optional: empty means "same as Endpoint".
	WebURL string
}

// Resolver is the minimal surface callers depend on.
type Resolver interface {
	Resolve(ctx context.Context, tenantID string) (*Config, error)
}

// DBResolver is the production Resolver: bound to a *gorm.DB, looks up the
// tenant's git_server row and parses config.admin_token.
type DBResolver struct {
	db *gorm.DB
}

// NewDBResolver binds a DBResolver to the supplied pool. Caller owns the
// pool's lifecycle.
func NewDBResolver(db *gorm.DB) *DBResolver {
	return &DBResolver{db: db}
}

// Resolve walks tenant_git_server_binding → git_servers.config.admin_token
// and returns the resolved Config.
func (r *DBResolver) Resolve(ctx context.Context, tenantID string) (*Config, error) {
	if r == nil || r.db == nil {
		return nil, ErrTenantMissingGitServer
	}
	if tenantID == "" {
		return nil, ErrTenantMissingGitServer
	}

	// Load binding row to discover the tenant's git_server_id.
	var binding models.TenantGitServerBinding
	err := r.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTenantMissingGitServer
	}
	if err != nil {
		return nil, fmt.Errorf("gitserver: query binding for tenant %q: %w", tenantID, err)
	}

	return r.ResolveByServerID(ctx, binding.GitServerID)
}

// ResolveByServerID loads a Git server directly by its stable server_id.
//
// It is used by inbound Git webhooks, whose route carries git_server_id and
// therefore has no tenant context. The returned Config intentionally has the
// same shape and validation as Resolve so consumers can safely use its API
// endpoint and admin token after the webhook has been authenticated.
func (r *DBResolver) ResolveByServerID(ctx context.Context, serverID string) (*Config, error) {
	if r == nil || r.db == nil || strings.TrimSpace(serverID) == "" {
		return nil, ErrGitServerNotFound
	}

	// Load the git_servers row.
	var gs models.GitServer
	err := r.db.WithContext(ctx).
		First(&gs, "server_id = ?", serverID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrGitServerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("gitserver: query git_server %q: %w", serverID, err)
	}
	if !gs.Enabled {
		return nil, ErrGitServerDisabled
	}

	// Parse config JSON.
	parsed, err := parseConfig(gs.Config)
	if err != nil {
		return nil, fmt.Errorf("%w: server=%s", ErrConfigMalformed, gs.ServerID)
	}
	if parsed.AdminToken == "" {
		return nil, fmt.Errorf("%w: admin_token empty: server=%s", ErrConfigMalformed, gs.ServerID)
	}

	return &Config{
		ServerID:      gs.ServerID,
		Kind:          gs.Kind,
		Endpoint:      gs.Endpoint,
		AdminToken:    parsed.AdminToken,
		AdminUser:     parsed.AdminUser,
		AdminPassword: parsed.AdminPassword,
		WebhookSecret: parsed.WebhookSecret,
		WebURL:        parsed.WebURL,
	}, nil
}

// gitServerConfigJSON is the JSON shape of git_servers.config.
type gitServerConfigJSON struct {
	AdminToken    string `json:"admin_token"`
	AdminUser     string `json:"admin_user,omitempty"`
	AdminPassword string `json:"admin_password,omitempty"`
	WebhookSecret string `json:"webhook_secret,omitempty"`
	WebURL        string `json:"web_url,omitempty"`
}

// parseConfig decodes the config JSONB blob. Empty / "{}" → zero-value
// struct (caller treats empty AdminToken as ErrConfigMalformed).
func parseConfig(raw string) (gitServerConfigJSON, error) {
	if raw == "" || raw == "{}" {
		return gitServerConfigJSON{}, nil
	}
	var cfg gitServerConfigJSON
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return gitServerConfigJSON{}, err
	}
	return cfg, nil
}
