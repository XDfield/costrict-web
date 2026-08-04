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
	"strconv"
	"strings"
	"time"
)

// parseInt wraps strconv.Atoi so the envInt call site stays tidy. Kept
// unexported — envInt is the public surface.
func parseInt(s string) (int, error) { return strconv.Atoi(s) }

// Config holds all cs-user runtime configuration.
type Config struct {
	HTTP     HTTPConfig
	Postgres PostgresConfig
	Internal InternalConfig
	JWT      JWTConfig
	Casdoor  CasdoorConfig
	Tenant   TenantConfig
	EventBus EventBusConfig
}

// CasdoorConfig backs the reissue-token handler's Casdoor JWT verification.
// cs-user no longer trusts server-supplied parsed claims unconditionally:
// when JWKSURL is configured, the handler pulls Casdoor's JWKS itself and
// re-validates any raw Casdoor JWT forwarded by server (signed, exp, nbf,
// iss, aud). Empty JWKSURL = verifier disabled (dev / pre-rollout); the
// handler logs and falls through to the legacy trust path so a misconfig
// never deadlocks login.
//
// FIXME(多租户 Casdoor): 当前 CasdoorConfig 是进程级全局——一台 cs-user 只能
// 验证一个 Casdoor 实例签发的 JWT。多租户场景下每个租户对接自家 Casdoor 时，
// 这套配置必须改成租户级别（来源是 tenant_configs.config_yaml 里的 Casdoor
// 子段，参考 MULTI_TENANCY_DESIGN §9.3 / §12.1）。届时验签流程需要从"先验后
// 查租户"调整为"peek JWT iss → 选 verifier → 验签 → 再查租户"，破掉顺序环。
// 改造完成前此结构保持进程级单实例。详见历史讨论与 MULTI_TENANCY_DESIGN。
type CasdoorConfig struct {
	// Endpoint is Casdoor's base URL (origin only, no path), e.g.
	// "https://casdoor.example.com". When JWKSURL is empty but Endpoint is
	// set, the JWKS URL is derived as "<Endpoint>/.well-known/jwks" at load
	// time (see loadCasdoorConfig). This matches the convention used by
	// server/internal/middleware/jwks.go (NewJWKSProvider appends the same
	// suffix to a base URL) so the two services stay consistent. Kept as a
	// distinct field (not folded into JWKSURL) so future per-tenant
	// refactoring can resolve it from tenant_configs without losing the
	// discovery-style derivation. See the FIXME on this struct.
	Endpoint string
	// JWKSURL is Casdoor's /.well-known/jwks endpoint. When set explicitly
	// via CS_USER_CASDOOR_JWKS_URL it overrides any derivation from Endpoint
	// (operators point this at a proxy / non-default path when Casdoor is
	// behind a custom route). Required for verification to engage — when
	// neither JWKSURL nor Endpoint is set, the verifier is disabled.
	JWKSURL string
	// Issuer is the expected `iss` claim. When empty, the iss check is
	// skipped (not recommended for production — set this to Casdoor's
	// origin so a token minted by a misconfigured peer IdP can't pass).
	Issuer string
	// Audience is the list of allowed `aud` values. When empty, the aud
	// check is skipped. Casdoor typically does not set aud on access
	// tokens, so the default is empty.
	Audience []string
	// JWKSHTTPTimeout bounds the JWKS fetch. Default 5s — long enough for
	// Casdoor round-trip on a slow link, short enough that a wedged
	// Casdoor doesn't stall login past the gateway timeout.
	JWKSHTTPTimeout time.Duration
	// JWKSRefreshTTL is how long a fetched key set is reused before a
	// background refresh. Default 15m. Unknown `kid` triggers an
	// immediate refresh regardless of TTL (handled in the verifier).
	JWKSRefreshTTL time.Duration
}

// EventBusConfig drives the Git Ownership Refactor Phase 2 outbox worker.
// Empty TargetURL = feature disabled (no events published; rows still land
// in user_events but the worker marks them as "target URL not configured"
// with backoff). Set both fields to enable delivery.
type EventBusConfig struct {
	TargetURL    string
	TargetToken  string
	PollInterval time.Duration
	BatchSize    int
	MaxAttempts  int
	BackoffBase  time.Duration
	BackoffMax   time.Duration
	HTTPTimeout  time.Duration
}

type HTTPConfig struct {
	Port string
	Mode string // gin mode: debug / release / test
	// SwaggerEnabled gates the public Swagger UI route (/swagger/*any).
	// Default false — the route is NOT mounted, so the cluster-internal
	// contract (33 endpoints + X-Internal-Token header name) is not
	// exposed to anyone who can reach the pod. Dev / CI set
	// CS_USER_SWAGGER_ENABLED=true to mount the UI for local iteration.
	// The generated spec (via `make swagger`) is unaffected — only the
	// HTTP route is gated.
	SwaggerEnabled bool
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

// IDPConfig removed (Phase E2 multi-IdP bypass deprecated). OAuth is
// brokered exclusively via Casdoor; cs-user no longer holds per-provider
// IdP configs.

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

	// TTL is the time from issuance to expiry. Defaults to 7d (168h) — long
	// enough that relying parties (Gitea fork auth, app-ai-native) don't
	// see constant re-issue churn in normal use, short enough to bound the
	// blast radius of a leaked cookie. Parsed from the env var as a Go
	// duration string ("168h", "7d" via ParseDuration-friendly forms, "30m").
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
	defaultJWTTTL    = 7 * 24 * time.Hour

	defaultCasdoorJWKSHTTPTimeout = 5 * time.Second
	defaultCasdoorJWKSRefreshTTL  = 15 * time.Minute
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
			Port: envDefault("CS_USER_HTTP_PORT", "8082"),
			Mode: envDefault("CS_USER_HTTP_MODE", "debug"),
			// SwaggerEnabled: default off — UI not mounted in production
			// (cluster-internal service, attack surface stays hidden).
			// See field doc above.
			SwaggerEnabled: envBool("CS_USER_SWAGGER_ENABLED", false),
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
		JWT:     jwtCfg,
		Casdoor: loadCasdoorConfig(),
		Tenant: TenantConfig{
			ApexDomains: loadApexDomains(os.Getenv("CS_USER_APEX_DOMAINS")),
		},
		EventBus: EventBusConfig{
			TargetURL:    strings.TrimSpace(os.Getenv("CS_USER_EVENT_TARGET_URL")),
			TargetToken:  os.Getenv("CS_USER_EVENT_TARGET_TOKEN"),
			PollInterval: envDuration("CS_USER_EVENT_POLL_INTERVAL", time.Second),
			BatchSize:    envInt("CS_USER_EVENT_BATCH_SIZE", 50),
			MaxAttempts:  envInt("CS_USER_EVENT_MAX_ATTEMPTS", 0),
			BackoffBase:  envDuration("CS_USER_EVENT_BACKOFF_BASE", 2*time.Second),
			BackoffMax:   envDuration("CS_USER_EVENT_BACKOFF_MAX", 5*time.Minute),
			HTTPTimeout:  envDuration("CS_USER_EVENT_HTTP_TIMEOUT", 5*time.Second),
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

// envDuration parses a time.Duration string ("5s", "2m"). Empty or invalid
// → fallback. Used for EventBusConfig timing knobs.
func envDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

// envInt parses an int env var. Empty or invalid → fallback.
func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := parseInt(v)
	if err != nil {
		return fallback
	}
	return n
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

// loadCasdoorConfig reads the Casdoor JWKS verification env vars. Empty
// JWKSURL → verifier disabled (load returns a zero-value config; the
// verifier struct treats empty JWKSURL as "off"). Optional knobs fall back
// to safe defaults.
//
// JWKS URL resolution: explicit CS_USER_CASDOOR_JWKS_URL wins; when unset,
// derive from CS_USER_CASDOOR_ENDPOINT as "<Endpoint>/.well-known/jwks"
// (matches server/internal/middleware/jwks.go's base-URL convention so the
// two services agree on the suffix). When both are empty the verifier stays
// disabled — operators in dev / pre-rollout don't need to fill anything.
func loadCasdoorConfig() CasdoorConfig {
	endpoint := strings.TrimSpace(os.Getenv("CS_USER_CASDOOR_ENDPOINT"))
	jwksURL := strings.TrimSpace(os.Getenv("CS_USER_CASDOOR_JWKS_URL"))
	if jwksURL == "" && endpoint != "" {
		jwksURL = strings.TrimRight(endpoint, "/") + "/.well-known/jwks"
	}
	cfg := CasdoorConfig{
		Endpoint:        endpoint,
		JWKSURL:         jwksURL,
		Issuer:          strings.TrimSpace(os.Getenv("CS_USER_CASDOOR_ISSUER")),
		JWKSHTTPTimeout: envDuration("CS_USER_CASDOOR_JWKS_HTTP_TIMEOUT", defaultCasdoorJWKSHTTPTimeout),
		JWKSRefreshTTL:  envDuration("CS_USER_CASDOOR_JWKS_REFRESH_TTL", defaultCasdoorJWKSRefreshTTL),
	}
	if audRaw := strings.TrimSpace(os.Getenv("CS_USER_CASDOOR_AUDIENCE")); audRaw != "" {
		for _, aud := range strings.Split(audRaw, ",") {
			if v := strings.TrimSpace(aud); v != "" {
				cfg.Audience = append(cfg.Audience, v)
			}
		}
	}
	return cfg
}
