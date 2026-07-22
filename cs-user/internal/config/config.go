// Package config loads cs-user runtime configuration from environment variables.
//
// All config is env-driven (12-factor). Phase 1 P0-1 scope: HTTP listen
// address, Postgres DSN, internal shared secret. If K8s ConfigMap file support
// is needed later, add a minimal yaml loader — for now env-only keeps the
// dependency surface small and avoids viper's Unmarshal+AutomaticEnv gotchas.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Config holds all cs-user runtime configuration.
type Config struct {
	HTTP     HTTPConfig
	Postgres PostgresConfig
	Internal InternalConfig
	JWT      JWTConfig
	Tenant   TenantConfig
	Gitea    GiteaConfig
	IDP      IDPConfig
}

type HTTPConfig struct {
	Port string
	Mode string // gin mode: debug / release / test
}

type PostgresConfig struct {
	Host     string
	Port     string
	Database string
	User     string
	Password string
	SSLMode  string
}

// DSN renders a lib/pq / pgx compatible connection string.
func (p PostgresConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s TimeZone=UTC",
		p.Host, p.Port, p.User, p.Password, p.Database, p.SSLMode,
	)
}

// InternalConfig holds the shared secret used to authenticate service-to-service
// calls from costrict-web (ADR D8: X-Internal-Token header).
type InternalConfig struct {
	// Token is the shared secret. Required — cs-user refuses to start if empty.
	Token string
}

// TenantConfig holds the B3b tenant-resolution inputs.
//
// ApexDomains is the list of bare domains the deployment serves, used by the
// subdomain fallback (Try 1) to extract the slug from a request Host header.
// Examples:
//
//   - prod: ["cs-user.example.com"]
//   - dev:  ["localhost:8080"]
//   - multi-region: ["cs-user.example.com","cs-user.example.cn"]
//
// Empty list (the default) disables subdomain resolution — the middleware
// falls through to cookie / X-Tenant-Id / default-tenant. Useful for local
// dev where the host is bare localhost without subdomain.
type TenantConfig struct {
	ApexDomains []string
}

// GiteaConfig holds the Phase E3a.1 Gitea auto-provisioning settings.
//
// Both fields OPTIONAL: when either is empty, cs-user treats Gitea
// auto-provisioning as disabled — the giteasync hook is a no-op and the
// /api/internal/users/:subject_id/gitea-binding endpoint still responds
// (always 404 in that case). Local dev + tests therefore need no real
// Gitea instance to run.
//
// BaseURL is the Gitea root (e.g. https://gitea.example.com); the
// trailing slash is stripped so tests/curl don't have to care.
//
// AdminToken is a Gitea PAT with admin scope (used for the
// "Authorization: token <PAT>" header on POST /admin/users). Stored in
// env / k8s secret — never logged.
type GiteaConfig struct {
	BaseURL    string
	AdminToken string
	// AdminUser / AdminPassword are OPTIONAL credentials propagated into
	// git_servers.config as admin_user / admin_password. Required by the
	// token-mint endpoints (POST /users/{name}/tokens) which sit behind
	// Gitea's reqBasicOrRevProxyAuth middleware and reject admin PAT auth.
	// When unset, bot provisioning against this Gitea cannot mint PATs.
	AdminUser     string
	AdminPassword string
}

// Enabled returns true when both BaseURL + AdminToken are populated. cs-user
// uses this to decide whether to construct *giteasync.Service at boot.
func (g GiteaConfig) Enabled() bool {
	return g.BaseURL != "" && g.AdminToken != ""
}

// IDPConfig holds IdP source validation knobs.
//
// AllowInsecure permits http:// (not just https://) URLs in IdP OAuth
// endpoints. Required for local dev where Casdoor / mock IdPs run on plain
// HTTP (e.g. http://127.0.0.1:8010). Production deployments must leave this
// false — checked at the validator layer, never bypassed elsewhere.
type IDPConfig struct {
	AllowInsecure bool
}

// JWTConfig holds the RS256 signing-key path + the A7 issuance parameters.
//
// SigningKeyPath is OPTIONAL at startup (Phase A3): when empty, the JWKS
// endpoint returns 503 (signing not configured) and SignJWT is never called
// by any wired path. A7 reissue-token endpoint returns 503 if the signer is
// missing.
//
// Issuer / TTL / DefaultAudience drive the A7 reissue-token flow. All three
// have safe defaults so a fresh deployment doesn't need extra config to
// issue tokens; operators override when integrating with relying parties
// that enforce specific iss / aud values.
type JWTConfig struct {
	// SigningKeyPath is the on-disk PEM file path. PKCS#1 ("RSA PRIVATE KEY")
	// or PKCS#8 ("PRIVATE KEY"). Mounted via k8s secret / docker secret.
	SigningKeyPath string

	// Issuer is the iss claim on cs-user-issued tokens. Defaults to
	// "cs-user" — operators set this to cs-user's public base URL when
	// relying parties need to verify iss against a known origin.
	Issuer string

	// TTL is the time from issuance to expiry. Defaults to 1h. Parsed from
	// the env var as a Go duration string ("1h", "30m", "3600s").
	TTL time.Duration

	// DefaultAudience is the aud claim applied when the caller doesn't
	// override. Empty slice means "no aud claim" — relying parties that
	// require aud will reject such tokens, so populate this in production.
	DefaultAudience []string
}

// Defaults applied when the corresponding env var is unset. Kept as package
// vars (not const) so tests in other packages can override via Load+env
// rather than reaching into config internals.
const (
	defaultJWTIssuer = "cs-user"
	defaultJWTTTL    = 1 * time.Hour
)

// Load reads configuration from environment variables (prefixed CS_USER_).
// Returns an error if any required field is missing or empty.
func Load() (*Config, error) {
	jwtCfg, err := loadJWTConfig()
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		HTTP: HTTPConfig{
			Port: envDefault("CS_USER_HTTP_PORT", "8081"),
			Mode: envDefault("CS_USER_HTTP_MODE", "debug"),
		},
		Postgres: PostgresConfig{
			Host:     envDefault("CS_USER_POSTGRES_HOST", "localhost"),
			Port:     envDefault("CS_USER_POSTGRES_PORT", "5432"),
			Database: envDefault("CS_USER_POSTGRES_DATABASE", "cs_user"),
			User:     os.Getenv("CS_USER_POSTGRES_USER"),
			Password: os.Getenv("CS_USER_POSTGRES_PASSWORD"),
			SSLMode:  envDefault("CS_USER_POSTGRES_SSLMODE", "disable"),
		},
		Internal: InternalConfig{
			Token: os.Getenv("CS_USER_INTERNAL_TOKEN"),
		},
		JWT: jwtCfg,
		Tenant: TenantConfig{
			ApexDomains: loadApexDomains(os.Getenv("CS_USER_APEX_DOMAINS")),
		},
		Gitea: GiteaConfig{
			BaseURL:       strings.TrimRight(os.Getenv("CS_USER_GITEA_BASE_URL"), "/"),
			AdminToken:    os.Getenv("CS_USER_GITEA_ADMIN_TOKEN"),
			AdminUser:     strings.TrimSpace(os.Getenv("CS_USER_GITEA_ADMIN_USER")),
			AdminPassword: os.Getenv("CS_USER_GITEA_ADMIN_PASSWORD"),
		},
		IDP: IDPConfig{
			AllowInsecure: envBool("CS_USER_IDP_ALLOW_INSECURE", false),
		},
	}

	if err := requireNonEmpty("CS_USER_INTERNAL_TOKEN", cfg.Internal.Token); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("CS_USER_POSTGRES_USER", cfg.Postgres.User); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("CS_USER_POSTGRES_PASSWORD", cfg.Postgres.Password); err != nil {
		return nil, err
	}

	return cfg, nil
}

// loadJWTConfig reads the JWT-related env vars. Split out so Load stays
// readable and so the parsing path (esp. the TTL duration + audience CSV)
// can be unit-tested without spinning the full Config.
func loadJWTConfig() (JWTConfig, error) {
	cfg := JWTConfig{
		SigningKeyPath: os.Getenv("CS_USER_JWT_SIGNING_KEY_PATH"),
		Issuer:         envDefault("CS_USER_JWT_ISSUER", defaultJWTIssuer),
	}

	ttlRaw := envDefault("CS_USER_JWT_TTL", defaultJWTTTL.String())
	ttl, err := time.ParseDuration(ttlRaw)
	if err != nil {
		return JWTConfig{}, fmt.Errorf("CS_USER_JWT_TTL %q: %w", ttlRaw, err)
	}
	if ttl <= 0 {
		return JWTConfig{}, fmt.Errorf("CS_USER_JWT_TTL must be positive, got %s", ttl)
	}
	cfg.TTL = ttl

	if audRaw := strings.TrimSpace(os.Getenv("CS_USER_JWT_AUDIENCE")); audRaw != "" {
		// Comma-separated per OIDC/RFC 7519 §4.1.3 conventions — short
		// strings, single-digit entry count typical. Whitespace around each
		// entry is trimmed so "a, b" → ["a","b"].
		for _, aud := range strings.Split(audRaw, ",") {
			if v := strings.TrimSpace(aud); v != "" {
				cfg.DefaultAudience = append(cfg.DefaultAudience, v)
			}
		}
	}

	return cfg, nil
}

// envDefault returns os.Getenv(key) or fallback if the env var is empty.
func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envBool parses a truthy env var. Accepts "1", "true", "yes" (case-
// insensitive). Empty or anything else → fallback. Used for CS_USER_IDP_
// ALLOW_INSECURE-style flags where the absence of the var means "off".
func envBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}

// requireNonEmpty returns a descriptive error if value is empty.
func requireNonEmpty(key, value string) error {
	if value == "" {
		return fmt.Errorf("%s must be set (non-empty)", key)
	}
	return nil
}

// loadApexDomains parses the CS_USER_APEX_DOMAINS env var. Comma-separated,
// whitespace-trimmed, empty entries dropped. Empty raw input → nil slice
// (subdomain resolution disabled). Mirrors the JWT audience CSV pattern.
func loadApexDomains(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	for _, d := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(d); v != "" {
			out = append(out, v)
		}
	}
	return out
}
