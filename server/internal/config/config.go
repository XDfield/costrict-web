package config

import (
	"log"
	"os"
	"strconv"

	"github.com/spf13/viper"
)

type Config struct {
	Port        string
	DatabaseURL string
	RedisURL    string
	Casdoor     CasdoorConfig
	LLM         LLMConfig
	Embedding   EmbeddingConfig
	Search      SearchConfig
}

type CasdoorConfig struct {
	Endpoint    string
	ClientID    string
	Secret      string
	CallbackURL string
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
	Provider    string // openai
	APIKey      string
	Model       string // embedding-3
	BaseURL     string
	Dimensions  int // 1024 for GLM embedding
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
	viper.AddConfigPath("..")
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		log.Printf("Warning: .env file not found, using environment variables")
	}

	return &Config{
		Port:        getEnv("PORT", "8080"),
		DatabaseURL: getEnv("DATABASE_URL", "postgres://costrict:costrict_password@localhost:5432/costrict_db?sslmode=disable"),
		RedisURL:    getEnv("REDIS_URL", ""),
		Casdoor: CasdoorConfig{
			Endpoint:    getEnv("CASDOOR_ENDPOINT", "http://localhost:8000"),
			ClientID:    getEnv("CASDOOR_CLIENT_ID", ""),
			Secret:      getEnv("CASDOOR_CLIENT_SECRET", ""),
			CallbackURL: getEnv("CASDOOR_CALLBACK_URL", "http://localhost:8080/api/auth/callback"),
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
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.ParseFloat(value, 64); err == nil {
			return n
		}
	}
	return defaultValue
}
