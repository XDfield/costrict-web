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
)

// Config holds all cs-user runtime configuration.
type Config struct {
	HTTP     HTTPConfig
	Postgres PostgresConfig
	Internal InternalConfig
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

// Load reads configuration from environment variables (prefixed CS_USER_).
// Returns an error if any required field is missing or empty.
func Load() (*Config, error) {
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

// envDefault returns os.Getenv(key) or fallback if the env var is empty.
func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
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
