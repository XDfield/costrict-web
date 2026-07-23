package config

import (
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Port                    string
	DatabaseURL             string
	RedisURL                string
	CloudBaseURL            string
	WebhookBaseURL          string // Public URL for WeCom/WeChat callback; defaults to CloudBaseURL
	AppURL                  string // Public URL for frontend links in notifications; defaults to CloudBaseURL
	MulticaAPIURL           string // Multica server API base URL (e.g. http://multica-server:8080), used for session permission checks
	ReleaseDownloadBaseURL  string
	SystemToken             string
	FrontendURLs            []string // Allowed frontend origins for OAuth redirects; first entry is the default
	InternalSecret          string
	CookieSecure            bool     // Set auth cookie with Secure flag (HTTPS only); default true
	CORSAllowedOrigins      []string // Allowed CORS origins; empty means allow all (insecure, dev only)
	Casdoor                 CasdoorConfig
	Channels                ChannelSystemConfig
	LLM                     LLMConfig
	Embedding               EmbeddingConfig
	Search                  SearchConfig
	DeptSync                DeptSyncConfig
	UserSyncIntervalMinutes int // User sync interval in minutes, default 15
	UserService             UserServiceConfig
	// BootstrapPlatformAdmins lists Casdoor universal_id values (case-sensitive,
	// NOT lowercased) that are automatically granted the platform_admin role when
	// they log in. universal_id is the stable global identity anchor Casdoor issues
	// for every identity (email can be empty for GitHub/phone logins, so it is not
	// reliable). This bootstraps the first administrator without manual SQL: a
	// deployment only needs to set BOOTSTRAP_PLATFORM_ADMIN_UNIVERSAL_IDS. Empty
	// means no bootstrap (zero behaviour change).
	BootstrapPlatformAdmins []string
	ClawAgent               ClawAgentConfig // ClawAgent personal AI assistant config
	// JWTSignMode controls the A8 灰度 (gradual rollout) state for JWT
	// self-signing. Three states (off → dual → single) per ROADMAP §9.4:
	//
	//   - JWTSignModeOff:    Casdoor JWT authoritative; OAuth callback
	//                        does NOT call ReissueToken. Default. Matches
	//                        pre-A8 behavior exactly.
	//   - JWTSignModeDual:   cs-user-signed JWT becomes the cookie value
	//                        (OAuth callback calls ReissueToken), but
	//                        the verifier still accepts BOTH cs-user and
	//                        Casdoor JWKS. Use during the 30-day
	//                        crossover window so existing sessions with
	//                        Casdoor tokens keep working.
	//   - JWTSignModeSingle: cs-user JWT only. Casdoor JWKS dropped from
	//                        the verifier chain. End-state after the 30-day
	//                        灰度 closes.
	//
	// When mode != off, requires USER_SERVICE_BACKEND=rpc (RPCWriter) so
	// the OAuth callback can reach cs-user's reissue-token endpoint, AND
	// USER_SERVICE_URL must be set so the JWKS provider can fetch
	// cs-user's /.well-known/jwks.
	//
	// Migration: JWT_SELF_SIGN_ENABLED=true (A7b vocabulary) maps to
	// "dual"; false/unset maps to "off". JWT_SIGN_MODE wins when both
	// are set.
	JWTSignMode string
	// ProfileGateEnabled (R3 of REGISTRATION_PROFILE_DESIGN): when true,
	// first-time users without profile_completed_at get 403 profile_incomplete
	// on all non-whitelisted routes until they finish /register/complete.
	// Default false for staged rollout (dev → canary → prod). When false,
	// middleware.RequireProfileComplete is a no-op.
	ProfileGateEnabled bool
}

// JWTSignMode values for Config.JWTSignMode.
const (
	JWTSignModeOff    = "off"
	JWTSignModeDual   = "dual"
	JWTSignModeSingle = "single"
)

// UserServiceConfig selects and configures the read backend for user data.
// Phase 0/P0-7: default is local (read from server's own DB). Setting
// Backend to "rpc" routes point reads through cs-user via HTTP. Writes always
// stay on the local UserService — Phase 1 cs-user has no write API.
type UserServiceConfig struct {
	Backend       string // "local" (default) or "rpc"
	BaseURL       string // cs-user base URL when Backend == "rpc", e.g. http://cs-user:8080
	InternalToken string // X-Internal-Token value sent to cs-user
	TimeoutSec    int    // per-request HTTP timeout in seconds, default 10
	WriteMode     string // "local" (default, writes go through UserService) or "readonly" (writes return ErrWriteBlocked)
	// ApexDomains enables Host-subdomain tenant-slug resolution (Phase B3b.2a).
	// Empty (default) disables the subdomain layer — local dev mode. Prod
	// sets e.g. USER_SERVICE_APEX_DOMAINS=example.com,example.cn.
	ApexDomains []string
}

// Backend values for UserServiceConfig.Backend.
const (
	UserServiceBackendLocal = "local"
	UserServiceBackendRPC   = "rpc"
)

// AuthMultiIdPConfig removed — Phase E2.6 multi-IdP bypass deprecated.
// OAuth is brokered exclusively via Casdoor; per-provider credentials live
// inside Casdoor only. The legacy /api/auth/login + /api/auth/callback
// routes talk directly to Casdoor.

// WriteMode values for UserServiceConfig.WriteMode.
const (
	UserServiceWriteModeLocal    = "local"
	UserServiceWriteModeReadonly = "readonly"
)

// ClawAgentConfig holds configuration for the ClawAgent personal AI assistant.
type ClawAgentConfig struct {
	EncryptionKey string // AES-256-GCM encryption key for API keys
	Session       ClawAgentSessionConfig
}

type ClawAgentSessionConfig struct {
	DailyResetHour               int // Daily reset hour for direct sessions (default 4)
	GroupIdleMinutes             int // Idle timeout for group sessions in minutes (default 30)
	EventIdleMinutes             int // Idle timeout for event sessions in minutes (default 60)
	TaskIdleMinutes              int // Idle timeout for task sessions in minutes (default 120)
	PruneAfterDays               int // Delete archived sessions after N days (default 30)
	MaxSessionsPerUser           int // Max archived sessions per user (default 200)
	MaxSessionTokens             int // Token threshold for session compaction (default 8000)
	CompactionKeepRecentMessages int // Number of recent messages to keep during compaction (default 10)
	NotificationDelaySeconds     int // Delay before sending AI notification to user (default 30)
}

// DeptSyncConfig holds connection settings for the external dept-sync service
// (real department/user tree, X-Query-Key authenticated). BaseURL and APIKey are
// optional: when either is empty the dept-sync client is considered unconfigured
// and degrades gracefully (admin endpoints return 503, frontend shows a notice).
// PathPrefix/AuthHeader default to the current dept-sync contract and only need
// overriding if the service changes its route prefix or auth header.
type DeptSyncConfig struct {
	BaseURL     string // e.g. http://dept-sync:8080 (bare address, no route prefix)
	APIKey      string // query_key value issued by dept-sync; sent in AuthHeader
	PathPrefix  string // data-API route prefix, default /costrict-dept-info/api/v1
	AuthHeader  string // auth header name, default X-Query-Key
	TimeoutSec  int    // per-request HTTP timeout, default 10s
	CacheTTLSec int    // in-memory cache TTL for tree/users responses, default 60s
}

type ChannelSystemConfig struct {
	EnabledTypes []string // Deprecated; use individual enabled flags below
	WeCom        WeComSystemConfig
	WeComBot     WeComBotSystemConfig
	// Individual channel enable flags (system-level availability)
	WebhookEnabled      bool `mapstructure:"CHANNEL_WEBHOOK_ENABLED"`
	WeComEnabled        bool `mapstructure:"CHANNEL_WECOM_ENABLED"`
	WeComWebhookEnabled bool `mapstructure:"CHANNEL_WECOM_WEBHOOK_ENABLED"`
	WeComBotEnabled     bool `mapstructure:"CHANNEL_WECOM_BOT_ENABLED"`
	WeChatEnabled       bool `mapstructure:"CHANNEL_WECHAT_ENABLED"`
}

type WeComSystemConfig struct {
	CorpID         string
	AgentID        int
	Secret         string
	Token          string
	EncodingAESKey string
}

type WeComBotSystemConfig struct {
	ProxyURL  string
	AuthToken string
	BotQRCode string
}

type CasdoorConfig struct {
	Endpoint         string // Public URL for browser redirects (login page)
	InternalEndpoint string // Internal URL for server-to-server calls (token exchange, userinfo); falls back to Endpoint
	ClientID         string
	Secret           string
	CallbackURL      string
	Organization     string // Casdoor organization name for user queries (e.g. "user-group")
}

// LLMConfig holds configuration for the LLM service (GLM with OpenAI protocol)
type LLMConfig struct {
	Provider    string // openai (for GLM compatibility)
	APIKey      string
	Model       string // glm-4-plus
	BaseURL     string // https://open.bigmodel.cn/api/paas/v4
	MaxTokens   int
	Temperature float64
}

// EmbeddingConfig holds configuration for the embedding service
type EmbeddingConfig struct {
	Provider   string // openai
	APIKey     string
	Model      string // embedding-3
	BaseURL    string
	Dimensions int // 1024 for GLM embedding
}

// SearchConfig holds configuration for search functionality
type SearchConfig struct {
	DefaultLimit        int
	SimilarityThreshold float64
}

func Load() *Config {
	viper.SetConfigName(".env")
	viper.SetConfigType("env")
	viper.AddConfigPath(".")
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		log.Printf("Warning: .env file not found, using environment variables")
	}

	cloudBaseURL := getEnv("COSTRICT_CLOUD_BASE_URL", "https://zgsm.sangfor.com/cloud")

	// FRONTEND_URLS defaults to COSTRICT_CLOUD_BASE_URL when not explicitly set.
	frontendURLs := getEnvSlice("FRONTEND_URLS", []string{cloudBaseURL})

	return &Config{
		Port:                   getEnv("PORT", "8080"),
		DatabaseURL:            getEnv("DATABASE_URL", "postgres://costrict:costrict_password@localhost:5432/costrict_db?sslmode=disable"),
		RedisURL:               getEnv("REDIS_URL", ""),
		CloudBaseURL:           cloudBaseURL,
		WebhookBaseURL:         getEnv("WEBHOOK_BASE_URL", cloudBaseURL),
		AppURL:                 getEnv("APP_URL", cloudBaseURL),
		MulticaAPIURL:          getEnv("MULTICA_API_URL", ""),
		ReleaseDownloadBaseURL: getEnv("RELEASE_DOWNLOAD_BASE_URL", ""),
		SystemToken:            getEnv("SYSTEM_TOKEN", ""),
		FrontendURLs:           frontendURLs,
		InternalSecret:         getEnv("INTERNAL_SECRET", ""),
		CookieSecure:           getEnvBool("COOKIE_SECURE", true),
		CORSAllowedOrigins:     getEnvSlice("CORS_ALLOWED_ORIGINS", nil),
		Casdoor: CasdoorConfig{
			Endpoint:         getEnv("CASDOOR_ENDPOINT", "http://localhost:8000"),
			InternalEndpoint: getEnv("CASDOOR_INTERNAL_ENDPOINT", ""),
			ClientID:         getEnv("CASDOOR_CLIENT_ID", ""),
			Secret:           getEnv("CASDOOR_CLIENT_SECRET", ""),
			CallbackURL:      getEnv("CASDOOR_CALLBACK_URL", "http://localhost:8080/api/auth/callback"),
			Organization:     getEnv("CASDOOR_ORGANIZATION", "user-group"),
		},
		LLM: LLMConfig{
			Provider:    getEnv("LLM_PROVIDER", "openai"),
			APIKey:      getEnv("LLM_API_KEY", ""),
			Model:       getEnv("LLM_MODEL", "glm-4-plus"),
			BaseURL:     getEnv("LLM_BASE_URL", "https://open.bigmodel.cn/api/paas/v4"),
			MaxTokens:   getEnvInt("LLM_MAX_TOKENS", 4096),
			Temperature: getEnvFloat("LLM_TEMPERATURE", 0.7),
		},
		Embedding: EmbeddingConfig{
			Provider:   getEnv("EMBEDDING_PROVIDER", "openai"),
			APIKey:     getEnv("EMBEDDING_API_KEY", ""),
			Model:      getEnv("EMBEDDING_MODEL", "embedding-3"),
			BaseURL:    getEnv("EMBEDDING_BASE_URL", "https://open.bigmodel.cn/api/paas/v4"),
			Dimensions: getEnvInt("EMBEDDING_DIMENSIONS", 1024),
		},
		Search: SearchConfig{
			DefaultLimit:        getEnvInt("SEARCH_DEFAULT_LIMIT", 20),
			SimilarityThreshold: getEnvFloat("SEARCH_SIMILARITY_THRESHOLD", 0.7),
		},
		DeptSync: DeptSyncConfig{
			BaseURL:     getEnv("DEPT_SYNC_URL", ""),
			APIKey:      getEnv("DEPT_SYNC_API_KEY", ""),
			PathPrefix:  getEnv("DEPT_SYNC_PATH_PREFIX", "/costrict-dept-info/api/v1"),
			AuthHeader:  getEnv("DEPT_SYNC_AUTH_HEADER", "X-Query-Key"),
			TimeoutSec:  getEnvInt("DEPT_SYNC_TIMEOUT_SECONDS", 10),
			CacheTTLSec: getEnvInt("DEPT_SYNC_CACHE_TTL_SECONDS", 60),
		},
		UserSyncIntervalMinutes: getEnvInt("USER_SYNC_INTERVAL_MINUTES", 15),
		UserService: UserServiceConfig{
			Backend:       getEnv("USER_SERVICE_BACKEND", UserServiceBackendLocal),
			BaseURL:       getEnv("USER_SERVICE_URL", ""),
			InternalToken: getEnv("USER_SERVICE_INTERNAL_TOKEN", ""),
			TimeoutSec:    getEnvInt("USER_SERVICE_TIMEOUT_SECONDS", 10),
			WriteMode:     getEnv("USER_SERVICE_WRITE_MODE", UserServiceWriteModeLocal),
			ApexDomains:   getEnvSlice("USER_SERVICE_APEX_DOMAINS", nil),
		},
		// universal_id is case-sensitive, so use getEnvSlice (NOT getEnvSliceLower).
		BootstrapPlatformAdmins: getEnvSlice("BOOTSTRAP_PLATFORM_ADMIN_UNIVERSAL_IDS", nil),
		Channels: ChannelSystemConfig{
			EnabledTypes:        getEnvSlice("CHANNEL_ENABLED_TYPES", nil),
			WeComEnabled:        getEnvBool("CHANNEL_WECOM_ENABLED", false),
			WebhookEnabled:      getEnvBool("CHANNEL_WEBHOOK_ENABLED", false),
			WeComWebhookEnabled: getEnvBool("CHANNEL_WECOM_WEBHOOK_ENABLED", false),
			WeChatEnabled:       getEnvBool("CHANNEL_WECHAT_ENABLED", false),
			WeComBotEnabled:     getEnvBool("CHANNEL_WECOM_BOT_ENABLED", false),
			WeCom: WeComSystemConfig{
				CorpID:         getEnv("WECOM_CORP_ID", ""),
				AgentID:        getEnvInt("WECOM_AGENT_ID", 0),
				Secret:         getEnv("WECOM_SECRET", ""),
				Token:          getEnv("WECOM_TOKEN", ""),
				EncodingAESKey: getEnv("WECOM_ENCODING_AES_KEY", ""),
			},
			WeComBot: WeComBotSystemConfig{
				ProxyURL:  getEnv("WECOM_BOT_PROXY_URL", ""),
				AuthToken: getEnv("WECOM_BOT_PROXY_AUTH_TOKEN", ""),
				BotQRCode: getEnv("WECOM_BOT_QR_CODE_URL", ""),
			},
		},
		ClawAgent: ClawAgentConfig{
			EncryptionKey: getEnv("CLAWAGENT_ENCRYPTION_KEY", ""),
			Session: ClawAgentSessionConfig{
				DailyResetHour:               getEnvInt("CLAWAGENT_SESSION_DAILY_RESET_HOUR", 4),
				GroupIdleMinutes:             getEnvInt("CLAWAGENT_SESSION_GROUP_IDLE_MINUTES", 30),
				EventIdleMinutes:             getEnvInt("CLAWAGENT_SESSION_EVENT_IDLE_MINUTES", 60),
				TaskIdleMinutes:              getEnvInt("CLAWAGENT_SESSION_TASK_IDLE_MINUTES", 120),
				PruneAfterDays:               getEnvInt("CLAWAGENT_SESSION_PRUNE_AFTER_DAYS", 30),
				MaxSessionsPerUser:           getEnvInt("CLAWAGENT_SESSION_MAX_PER_USER", 200),
				MaxSessionTokens:             getEnvInt("CLAWAGENT_SESSION_MAX_TOKENS", 8000),
				CompactionKeepRecentMessages: getEnvInt("CLAWAGENT_SESSION_COMPACTION_KEEP_RECENT", 10),
				NotificationDelaySeconds:     getEnvInt("AI_NOTIFICATION_DELAY_SECONDS", 30),
			},
		},
		// Phase A8: three-state JWT self-sign mode. Loader prefers
		// JWT_SIGN_MODE (off|dual|single) and falls back to the A7b
		// bool vocabulary (JWT_SELF_SIGN_ENABLED=true → dual). Default
		// OFF — Casdoor JWT stays authoritative until operator flips.
		JWTSignMode: loadJWTSignMode(),
		ProfileGateEnabled: getEnvBool("PROFILE_GATE_ENABLED", false),
	}
}

// splitCommaList splits a comma-separated env value. Reuses the same
// semantics as getEnvSlice but without the default-value plumbing.
func splitCommaList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// loadJWTSignMode resolves the JWT self-sign mode from environment.
//
// Resolution order:
//  1. JWT_SIGN_MODE (preferred, A8 vocabulary): must be off|dual|single,
//     case-insensitive. Invalid values are fatal — silent fallback would
//     mask a typo as "off" and accidentally disable 灰度 mid-cutover.
//  2. JWT_SELF_SIGN_ENABLED (A7b vocabulary, retained for migration):
//     strconv.ParseBool true → dual, anything else → off.
//  3. Default: off.
func loadJWTSignMode() string {
	if raw := strings.ToLower(strings.TrimSpace(getEnv("JWT_SIGN_MODE", ""))); raw != "" {
		switch raw {
		case JWTSignModeOff, JWTSignModeDual, JWTSignModeSingle:
			return raw
		default:
			log.Fatalf("invalid JWT_SIGN_MODE %q: must be one of off|dual|single", raw)
		}
	}
	if getEnvBool("JWT_SELF_SIGN_ENABLED", false) {
		return JWTSignModeDual
	}
	return JWTSignModeOff
}

func getEnv(key, defaultValue string) string {
	// First check viper (reads from .env file)
	if value := viper.GetString(key); value != "" {
		return value
	}
	// Then check system environment variable
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	// First check viper
	if value := viper.GetString(key); value != "" {
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
	}
	// Then check system environment variable
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	// First check viper
	if value := viper.GetString(key); value != "" {
		if n, err := strconv.ParseFloat(value, 64); err == nil {
			return n
		}
	}
	// Then check system environment variable
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.ParseFloat(value, 64); err == nil {
			return n
		}
	}
	return defaultValue
}

// getEnvBool reads a boolean env var. Used by R3's PROFILE_GATE_ENABLED.
func getEnvBool(key string, defaultValue bool) bool {
	// Check if environment variable is explicitly set (even if empty)
	viperValue := viper.GetString(key)
	if viper.IsSet(key) {
		// Variable is set, check its value
		if viperValue != "" {
			if b, err := strconv.ParseBool(viperValue); err == nil {
				return b
			}
		}
		// Variable is set but empty or invalid, return false
		return false
	}
	// Check system environment variable
	if envValue := os.Getenv(key); envValue != "" {
		if b, err := strconv.ParseBool(envValue); err == nil {
			return b
		}
		return false
	}
	return defaultValue
}

// getEnvSlice reads a comma-separated environment variable into a string slice.
// Returns defaultValue if the variable is not set or empty.
func getEnvSlice(key string, defaultValue []string) []string {
	raw := getEnv(key, "")
	if raw == "" {
		return defaultValue
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	if len(result) == 0 {
		return defaultValue
	}
	return result
}

// getEnvSliceLower is like getEnvSlice but lowercases each entry. Used for
// case-insensitive matching (e.g. email allowlists). Returns defaultValue when
// the variable is unset/empty or contains only blanks.
func getEnvSliceLower(key string, defaultValue []string) []string {
	parts := getEnvSlice(key, nil)
	if parts == nil {
		return defaultValue
	}
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		result = append(result, strings.ToLower(p))
	}
	return result
}
