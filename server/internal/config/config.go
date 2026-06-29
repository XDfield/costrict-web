package config

import (
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Port                      string
	DatabaseURL               string
	UsageSQLitePath           string
	UsageProvider             string
	UsageESReportBaseURL      string
	UsageESQueryBaseURL       string
	UsageESReportPath         string
	UsageESQueryPath          string
	UsageESTimeoutSec         int
	UsageESBasicUser          string
	UsageESBasicPass          string
	UsageESInsecureSkipVerify bool
	RedisURL                  string
	CloudBaseURL              string
	WebhookBaseURL            string // Public URL for WeCom/WeChat callback; defaults to CloudBaseURL
	AppURL                    string // Public URL for frontend links in notifications; defaults to CloudBaseURL
	ReleaseDownloadBaseURL    string
	SystemToken               string
	FrontendURLs              []string // Allowed frontend origins for OAuth redirects; first entry is the default
	InternalSecret            string
	CookieSecure              bool     // Set auth cookie with Secure flag (HTTPS only); default true
	CORSAllowedOrigins        []string // Allowed CORS origins; empty means allow all (insecure, dev only)
	Casdoor                   CasdoorConfig
	Channels                  ChannelSystemConfig
	LLM                       LLMConfig
	Embedding                 EmbeddingConfig
	Search                    SearchConfig
	DeptSync                  DeptSyncConfig
	UserSyncIntervalMinutes   int // User sync interval in minutes, default 15
	// BootstrapPlatformAdmins lists Casdoor universal_id values (case-sensitive,
	// NOT lowercased) that are automatically granted the platform_admin role when
	// they log in. universal_id is the stable global identity anchor Casdoor issues
	// for every identity (email can be empty for GitHub/phone logins, so it is not
	// reliable). This bootstraps the first administrator without manual SQL: a
	// deployment only needs to set BOOTSTRAP_PLATFORM_ADMIN_UNIVERSAL_IDS. Empty
	// means no bootstrap (zero behaviour change).
	BootstrapPlatformAdmins []string
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
	// Individual channel enable flags (system-level availability)
	WeComEnabled        bool `mapstructure:"CHANNEL_WECOM_ENABLED"`
	WeComWebhookEnabled bool `mapstructure:"CHANNEL_WECOM_WEBHOOK_ENABLED"`
	WeChatEnabled       bool `mapstructure:"CHANNEL_WECHAT_ENABLED"`
}

type WeComSystemConfig struct {
	CorpID         string
	AgentID        int
	Secret         string
	Token          string
	EncodingAESKey string
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
		Port:                      getEnv("PORT", "8080"),
		DatabaseURL:               getEnv("DATABASE_URL", "postgres://costrict:costrict_password@localhost:5432/costrict_db?sslmode=disable"),
		UsageSQLitePath:           getEnv("USAGE_SQLITE_PATH", "./data/usage/usage.db"),
		UsageProvider:             getEnv("USAGE_PROVIDER", "sqlite"),
		UsageESReportBaseURL:      getEnv("USAGE_ES_REPORT_BASE_URL", getEnv("USAGE_ES_BASE_URL", "")),
		UsageESQueryBaseURL:       getEnv("USAGE_ES_QUERY_BASE_URL", getEnv("USAGE_ES_BASE_URL", "")),
		UsageESReportPath:         getEnv("USAGE_ES_REPORT_PATH", "/internal/indicator/api/v1/session_turn_metrics"),
		UsageESQueryPath:          getEnv("USAGE_ES_QUERY_PATH", "/costrict_session_turn_metrics/_search"),
		UsageESTimeoutSec:         getEnvInt("USAGE_ES_TIMEOUT_SECONDS", 15),
		UsageESBasicUser:          getEnv("USAGE_ES_BASIC_USER", ""),
		UsageESBasicPass:          getEnv("USAGE_ES_BASIC_PASS", ""),
		UsageESInsecureSkipVerify: getEnvBool("USAGE_ES_INSECURE_SKIP_VERIFY", false),
		RedisURL:                  getEnv("REDIS_URL", ""),
		CloudBaseURL:              cloudBaseURL,
		WebhookBaseURL:            getEnv("WEBHOOK_BASE_URL", cloudBaseURL),
		AppURL:                    getEnv("APP_URL", cloudBaseURL),
		ReleaseDownloadBaseURL:    getEnv("RELEASE_DOWNLOAD_BASE_URL", ""),
		SystemToken:               getEnv("SYSTEM_TOKEN", ""),
		FrontendURLs:              frontendURLs,
		InternalSecret:            getEnv("INTERNAL_SECRET", ""),
		CookieSecure:              getEnvBool("COOKIE_SECURE", true),
		CORSAllowedOrigins:        getEnvSlice("CORS_ALLOWED_ORIGINS", nil),
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
		// universal_id is case-sensitive, so use getEnvSlice (NOT getEnvSliceLower).
		BootstrapPlatformAdmins: getEnvSlice("BOOTSTRAP_PLATFORM_ADMIN_UNIVERSAL_IDS", nil),
		Channels: ChannelSystemConfig{
			EnabledTypes:        getEnvSlice("CHANNEL_ENABLED_TYPES", nil),
			WeComEnabled:        getEnvBool("CHANNEL_WECOM_ENABLED", true),
			WeComWebhookEnabled: getEnvBool("CHANNEL_WECOM_WEBHOOK_ENABLED", true),
			WeChatEnabled:       getEnvBool("CHANNEL_WECHAT_ENABLED", true),
			WeCom: WeComSystemConfig{
				CorpID:         getEnv("WECOM_CORP_ID", ""),
				AgentID:        getEnvInt("WECOM_AGENT_ID", 0),
				Secret:         getEnv("WECOM_SECRET", ""),
				Token:          getEnv("WECOM_TOKEN", ""),
				EncodingAESKey: getEnv("WECOM_ENCODING_AES_KEY", ""),
			},
		},
	}
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
