package internal

import (
	"os"
	"strconv"
)

type Config struct {
	ListenAddr            string
	ServerURL             string
	DatabaseURL           string
	DBMaxOpenConns        int
	DBMaxIdleConns        int
	DBConnMaxLifetimeSec  int
	AuditRetentionDays    int
	FilterRulesPath       string
	MaxInterceptBodySize  int64
	AuditChannelSize      int
	AuditBatchSize        int
	AuditFlushIntervalMs  int
	AuditSendTimeoutSec   int
	LogLevel              string
	ShutdownTimeout       int
	FilterFailureMode     string
}

func LoadConfig() *Config {
	return &Config{
		ListenAddr:           getEnv("LISTEN_ADDR", ":8090"),
		ServerURL:            getEnv("SERVER_URL", "http://server:8080"),
		DatabaseURL:          os.Getenv("DATABASE_URL"),
		DBMaxOpenConns:       getEnvInt("DB_MAX_OPEN_CONNS", 10),
		DBMaxIdleConns:       getEnvInt("DB_MAX_IDLE_CONNS", 5),
		DBConnMaxLifetimeSec: getEnvInt("DB_CONN_MAX_LIFETIME", 300),
		AuditRetentionDays:   getEnvInt("AUDIT_RETENTION_DAYS", 90),
		FilterRulesPath:      getEnv("FILTER_RULES_PATH", "./filter_rules.yaml"),
		MaxInterceptBodySize: getEnvInt64("MAX_INTERCEPT_BODY_SIZE", 52428800),
		AuditChannelSize:     getEnvInt("AUDIT_CHANNEL_SIZE", 4096),
		AuditBatchSize:       getEnvInt("AUDIT_BATCH_SIZE", 100),
		AuditFlushIntervalMs: getEnvInt("AUDIT_FLUSH_INTERVAL_MS", 5000),
		AuditSendTimeoutSec:  getEnvInt("AUDIT_SEND_TIMEOUT", 5),
		LogLevel:             getEnv("LOG_LEVEL", "info"),
		ShutdownTimeout:      getEnvInt("SHUTDOWN_TIMEOUT", 30),
		FilterFailureMode:    getEnv("FILTER_FAILURE_MODE", "block"),
	}
}

func getEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultValue
}

func getEnvInt64(key string, defaultValue int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return defaultValue
}
