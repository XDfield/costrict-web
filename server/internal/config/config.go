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
	FrontendURLs              []string // Allowed frontend origins for OAuth redirects; first entry is the default
	InternalSecret            string
	CookieSecure              bool     // Set auth cookie with Secure flag (HTTPS only); default true
	CORSAllowedOrigins        []string // Allowed CORS origins; empty means allow all (insecure, dev only)
	Casdoor                   CasdoorConfig
	LLM                       LLMConfig
	Embedding                 EmbeddingConfig
	Search                    SearchConfig
	UserSyncIntervalMinutes   int // User sync interval in minutes, default 15
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
		UserSyncIntervalMinutes: getEnvInt("USER_SYNC_INTERVAL_MINUTES", 15),
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
	if value := viper.GetString(key); value != "" {
		if b, err := strconv.ParseBool(value); err == nil {
			return b
		}
	}
	if value := os.Getenv(key); value != "" {
		if b, err := strconv.ParseBool(value); err == nil {
			return b
		}
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
