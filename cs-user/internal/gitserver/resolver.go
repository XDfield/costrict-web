// Package gitserver implements per-tenant Git server resolution (Phase E3b.1.1).
//
// The Resolver answers "which Git endpoint + admin token does this tenant
// use?" — replacing the broken global env-var singleton that E3a.1 (cs-user
// giteasync) and E3b.1 (@server gitsync) both shipped with.
//
// Flow:
//
//  1. Load tenants.git_server_id for the supplied tenant.
//  2. Load the bound git_servers row.
//  3. Parse the JSONB config blob to extract admin_token.
//  4. Return GitServerConfig{Endpoint, AdminToken, ServerID, Kind}.
//
// The Resolver is read-only and stateless; safe to share across goroutines.
// Caching lives in the consumer (gitsync.RPCResolver on @server has a 5-min
// TTL cache; cs-user constructs a fresh GiteaClient per Provision call, so
// there's nothing to cache here).
package gitserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/gorm"
)

// Sentinel errors. Callers (giteasync.Service, the RPC handler) translate:
//
//   - ErrTenantNotFound          → 404 (no such tenant)
//   - ErrTenantMissingGitServer  → 500 (operator mis-config; bootstrap missed it)
//   - ErrGitServerNotFound       → 500 (FK violation — should be impossible)
//   - ErrGitServerDisabled       → 503 (drained / decommission in progress)
//   - ErrConfigMalformed         → 500 (operator bug; admin_token unreadable)
var (
	ErrTenantNotFound         = errors.New("gitserver: tenant not found")
	ErrTenantMissingGitServer = errors.New("gitserver: tenant has no git_server_id (bootstrap incomplete)")
	ErrGitServerNotFound      = errors.New("gitserver: git_server row not found (FK violation)")
	ErrGitServerDisabled      = errors.New("gitserver: git server is disabled")
	ErrConfigMalformed        = errors.New("gitserver: config JSON malformed or missing admin_token")
)

// Config is the minimum the calling Git client needs. It's intentionally a
// value type — copies are cheap and there's no internal state worth hiding.
//
// AdminUser / AdminPassword are optional credentials required by Gitea
// endpoints that reject admin PAT auth (POST /users/{name}/tokens sits
// behind reqBasicOrRevProxyAuth in upstream Gitea). Empty = not configured;
// callers that need them should fall back to a clear error.
type Config struct {
	ServerID      string
	Kind          string
	Endpoint      string
	AdminToken    string
	AdminUser     string
	AdminPassword string
}

// Resolver interface allows the giteasync package to depend on a tiny surface
// (and lets tests inject a stub instead of standing up sqlite).
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

// Resolve walks tenants.git_server_id → git_servers.config.admin_token and
// returns the resolved Config. See package doc for the error vocabulary.
func (r *DBResolver) Resolve(ctx context.Context, tenantID string) (*Config, error) {
	if r == nil || r.db == nil {
		return nil, ErrTenantNotFound
	}
	if tenantID == "" {
		return nil, ErrTenantNotFound
	}

	// Load tenant to discover its git_server_id.
	var tn models.Tenant
	err := r.db.WithContext(ctx).
		Select("tenant_id", "git_server_id").
		First(&tn, "tenant_id = ?", tenantID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("gitserver: query tenant %q: %w", tenantID, err)
	}
	if tn.GitServerID == nil || *tn.GitServerID == "" {
		return nil, ErrTenantMissingGitServer
	}

	// Load the git_servers row.
	var gs models.GitServer
	err = r.db.WithContext(ctx).
		First(&gs, "server_id = ?", *tn.GitServerID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrGitServerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("gitserver: query git_server %q: %w", *tn.GitServerID, err)
	}
	if !gs.Enabled {
		return nil, ErrGitServerDisabled
	}

	// Parse config JSON for admin_token (required) + admin_user/admin_password
	// (optional, only needed for endpoints behind reqBasicOrRevProxyAuth).
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
	}, nil
}

// gitServerConfigJSON is the JSON shape of git_servers.config.
// admin_token is required; admin_user/admin_password are optional (only
// needed for endpoints behind reqBasicOrRevProxyAuth, e.g. token-mint).
// Future fields (webhook_secret, rate_limit, ...) land here as a
// single-source change.
type gitServerConfigJSON struct {
	AdminToken    string `json:"admin_token"`
	AdminUser     string `json:"admin_user,omitempty"`
	AdminPassword string `json:"admin_password,omitempty"`
}

// parseConfig decodes the config JSONB blob. Empty / "{}" → zero-value
// struct (caller treats empty AdminToken as ErrConfigMalformed). Malformed
// JSON → returned error (caller wraps with server_id).
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

// parseAdminToken decodes the config JSONB blob and extracts admin_token.
// Empty / malformed JSON → ErrConfigMalformed (caller wraps with server_id).
//
// Deprecated: kept for backward-compat with existing call sites; new code
// should call parseConfig to also pick up admin_user/admin_password.
func parseAdminToken(raw string) (string, error) {
	cfg, err := parseConfig(raw)
	if err != nil {
		return "", err
	}
	return cfg.AdminToken, nil
}
